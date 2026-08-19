// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

// Notification toast popup â€” pure WinAPI, no dependencies.
//
// Architecture: WS_POPUP | WS_EX_TOPMOST | WS_EX_TOOLWINDOW window with GDI
// painting.  Rounded corners via SetWindowRgn.  Positioned bottom-right via
// SPI_GETWORKAREA.  Auto-closes after 8 s or on âœ• click.
// Multiple messages are stacked vertically in one window.

package windows

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"tunnel_cat/snc/core"
)

// â”€â”€ Colour palette (SNC, VisualStyle.md Â§4.5) â”€â”€ COLORREF = 0x00BBGGRR â”€â”€â”€â”€â”€â”€
const (
	notifColBg     uintptr = 0x0028201B // #1B2028 snc-surface
	notifColHeader uintptr = 0x00342B25 // #252B34 snc-fur-highlight
	notifColTitle  uintptr = 0x00FFC859 // #59C8FF snc-screen-cyan
	notifColText   uintptr = 0x00E8F1F4 // #F4F1E8 snc-text-primary
	notifColClose  uintptr = 0x00B0A299 // #99A2B0 snc-text-muted (idle)
	notifColCloseH uintptr = 0x0047B5FF // #FFB547 snc-warm-amber (hover)
	notifColSep    uintptr = 0x00342B25 // #252B34 snc-fur-highlight
)

// â”€â”€ Layout constants â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
const (
	notifWinWidth       = 380
	notifHeaderH  int32 = 40
	notifPadX     int32 = 14
	notifPadY     int32 = 12
	notifMsgH     int32 = 80   // height allocated per message
	notifSepH     int32 = 8    // gap between stacked messages
	notifAutoMs         = 8000 // auto-dismiss after 8 s
)

// â”€â”€ State â€” accessed only from the OS-locked message-loop goroutine â”€â”€â”€â”€â”€â”€â”€â”€â”€
var notifState struct {
	msgs       []string
	closeRect  [4]int32 // L, T, R, B in client coords
	hoverClose bool
}

// â”€â”€ DLL handles â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
var (
	notifUser32   = syscall.NewLazyDLL("user32.dll")
	notifGdi32    = syscall.NewLazyDLL("gdi32.dll")
	notifKernel32 = syscall.NewLazyDLL("kernel32.dll")
)

var (
	notifClassOnce sync.Once
	notifWndProcCB uintptr
)

// â”€â”€ Win32 structures â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type notifRECT struct{ left, top, right, bottom int32 }

type notifWNDCLASSEX struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// TRACKMOUSEEVENT â€” matches Win64 layout (24 bytes).
type notifTME struct {
	cbSize      uint32
	dwFlags     uint32
	hwndTrack   uintptr
	dwHoverTime uint32
	_pad        uint32
}

// ShowNotification displays a styled topmost toast window with msgs stacked
// vertically.  Blocks until dismissed (Ã— click or 8-second timer).
// Safe to call from any goroutine.
func ShowNotification(msgs []string) {
	if len(msgs) == 0 {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	notifState.msgs = msgs
	notifState.hoverClose = false

	notifClassOnce.Do(func() {
		notifWndProcCB = syscall.NewCallback(notifWndProc)
	})

	height := notifCalcHeight(len(msgs))

	className, _ := syscall.UTF16PtrFromString("SNCnotify")
	wc := notifWNDCLASSEX{
		cbSize:        uint32(unsafe.Sizeof(notifWNDCLASSEX{})),
		lpfnWndProc:   notifWndProcCB,
		hInstance:     notifModuleHandle(),
		lpszClassName: className,
	}
	notifUser32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc)))

	// Position: bottom-right of the work area (excludes taskbar).
	var wa notifRECT
	notifUser32.NewProc("SystemParametersInfoW").Call(
		0x0030, 0, uintptr(unsafe.Pointer(&wa)), 0) // SPI_GETWORKAREA
	x := wa.right - notifWinWidth - 12
	y := wa.bottom - int32(height) - 12

	windowName, _ := syscall.UTF16PtrFromString("Tunnel Cat")
	hwnd, _, _ := notifUser32.NewProc("CreateWindowExW").Call(
		0x00000008|0x00000080, // WS_EX_TOPMOST | WS_EX_TOOLWINDOW
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		0x80000000, // WS_POPUP
		uintptr(x), uintptr(y),
		notifWinWidth, uintptr(height),
		0, 0, uintptr(notifModuleHandle()), 0)
	if hwnd == 0 {
		core.Log.Printf("notify: CreateWindowExW failed")
		return
	}
	defer notifUser32.NewProc("DestroyWindow").Call(hwnd)

	// Rounded corners (12-px radius ellipse).
	hrgn, _, _ := notifGdi32.NewProc("CreateRoundRectRgn").Call(
		0, 0, uintptr(notifWinWidth+1), uintptr(height+1), 12, 12)
	notifUser32.NewProc("SetWindowRgn").Call(hwnd, hrgn, 1)

	notifUser32.NewProc("ShowWindow").Call(hwnd, 4) // SW_SHOWNOACTIVATE
	notifUser32.NewProc("SetTimer").Call(hwnd, 1, notifAutoMs, 0)

	var m [7]uintptr
	for {
		r, _, _ := notifUser32.NewProc("GetMessageW").Call(
			uintptr(unsafe.Pointer(&m[0])), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		notifUser32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&m[0])))
		notifUser32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&m[0])))
	}
	// Drain stray WM_QUIT so the caller's message loop is not poisoned.
	var peek [7]uintptr
	notifUser32.NewProc("PeekMessageW").Call(
		uintptr(unsafe.Pointer(&peek[0])), 0, 0x0012, 0x0012, 1) // WM_QUIT, PM_REMOVE
}

func notifCalcHeight(n int) int {
	h := int(notifHeaderH+notifPadY) + n*int(notifMsgH) + (n-1)*int(notifSepH) + int(notifPadY)
	if h > 440 {
		h = 440
	}
	return h
}

func notifWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	const (
		wmDestroy     = 0x0002
		wmPaint       = 0x000F
		wmTimer       = 0x0113
		wmLButtonDown = 0x0201
		wmMouseMove   = 0x0200
		wmMouseLeave  = 0x02A3
		wmNcHitTest   = 0x0084
		htCaption     = 2
	)
	switch msg {
	case wmPaint:
		notifDoPaint(hwnd)
		return 0

	case wmTimer:
		if wParam == 1 {
			notifUser32.NewProc("PostQuitMessage").Call(0)
		}
		return 0

	case wmLButtonDown:
		mx := int32(lParam & 0xFFFF)
		my := int32((lParam >> 16) & 0xFFFF)
		cr := notifState.closeRect
		if mx >= cr[0] && mx < cr[2] && my >= cr[1] && my < cr[3] {
			notifUser32.NewProc("PostQuitMessage").Call(0)
		}
		return 0

	case wmMouseMove:
		mx := int32(lParam & 0xFFFF)
		my := int32((lParam >> 16) & 0xFFFF)
		cr := notifState.closeRect
		hover := mx >= cr[0] && mx < cr[2] && my >= cr[1] && my < cr[3]
		if hover != notifState.hoverClose {
			notifState.hoverClose = hover
			inv := notifRECT{cr[0], cr[1], cr[2], cr[3]}
			notifUser32.NewProc("InvalidateRect").Call(hwnd, uintptr(unsafe.Pointer(&inv)), 1)
		}
		tme := notifTME{
			cbSize:    uint32(unsafe.Sizeof(notifTME{})),
			dwFlags:   0x00000002, // TME_LEAVE
			hwndTrack: hwnd,
		}
		notifUser32.NewProc("TrackMouseEvent").Call(uintptr(unsafe.Pointer(&tme)))
		return 0

	case wmMouseLeave:
		if notifState.hoverClose {
			notifState.hoverClose = false
			cr := notifState.closeRect
			inv := notifRECT{cr[0], cr[1], cr[2], cr[3]}
			notifUser32.NewProc("InvalidateRect").Call(hwnd, uintptr(unsafe.Pointer(&inv)), 1)
		}
		return 0

	case wmNcHitTest:
		// Treat client area as caption so the user can drag the window.
		r, _, _ := notifUser32.NewProc("DefWindowProcW").Call(hwnd, msg, wParam, lParam)
		if r == 1 { // HTCLIENT
			return htCaption
		}
		return r

	case wmDestroy:
		notifUser32.NewProc("PostQuitMessage").Call(0)
		return 0
	}
	r, _, _ := notifUser32.NewProc("DefWindowProcW").Call(hwnd, msg, wParam, lParam)
	return r
}

func notifDoPaint(hwnd uintptr) {
	var ps [80]byte // PAINTSTRUCT (72 bytes on Win64; 80 for safety)
	hdc, _, _ := notifUser32.NewProc("BeginPaint").Call(hwnd, uintptr(unsafe.Pointer(&ps[0])))
	defer notifUser32.NewProc("EndPaint").Call(hwnd, uintptr(unsafe.Pointer(&ps[0])))

	var cr notifRECT
	notifUser32.NewProc("GetClientRect").Call(hwnd, uintptr(unsafe.Pointer(&cr)))

	// â”€â”€ Fill background â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	bgBrush, _, _ := notifGdi32.NewProc("CreateSolidBrush").Call(notifColBg)
	notifUser32.NewProc("FillRect").Call(hdc, uintptr(unsafe.Pointer(&cr)), bgBrush)
	notifGdi32.NewProc("DeleteObject").Call(bgBrush)

	// â”€â”€ Header bar â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	hdrBrush, _, _ := notifGdi32.NewProc("CreateSolidBrush").Call(notifColHeader)
	hdr := notifRECT{0, 0, cr.right, notifHeaderH}
	notifUser32.NewProc("FillRect").Call(hdc, uintptr(unsafe.Pointer(&hdr)), hdrBrush)
	notifGdi32.NewProc("DeleteObject").Call(hdrBrush)

	notifGdi32.NewProc("SetBkMode").Call(hdc, 1) // TRANSPARENT

	segoeUI, _ := syscall.UTF16PtrFromString(ThemeFontUI)

	// â”€â”€ Title + close button (share one font select) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	titleFont, _, _ := notifGdi32.NewProc("CreateFontW").Call(
		18, 0, 0, 0,
		700 /* FW_BOLD */, 0, 0, 0,
		1 /* DEFAULT_CHARSET */, 0, 0,
		5 /* CLEARTYPE_QUALITY */, 0,
		uintptr(unsafe.Pointer(segoeUI)))
	origFont, _, _ := notifGdi32.NewProc("SelectObject").Call(hdc, titleFont)

	notifGdi32.NewProc("SetTextColor").Call(hdc, notifColTitle)
	titleStr, _ := syscall.UTF16PtrFromString("Tunnel Cat")
	titleR := notifRECT{notifPadX, 0, cr.right - 38, notifHeaderH}
	notifUser32.NewProc("DrawTextW").Call(
		hdc, uintptr(unsafe.Pointer(titleStr)), ^uintptr(0),
		uintptr(unsafe.Pointer(&titleR)),
		0x0824) // DT_SINGLELINE|DT_VCENTER|DT_NOPREFIX

	// Close button hitbox: right 34 px of header, 4 px inset from edges.
	notifState.closeRect = [4]int32{cr.right - 34, 4, cr.right - 4, notifHeaderH - 4}
	closeColor := notifColClose
	if notifState.hoverClose {
		closeColor = notifColCloseH
	}
	notifGdi32.NewProc("SetTextColor").Call(hdc, closeColor)
	xStr, _ := syscall.UTF16PtrFromString("âœ•")
	closeR := notifRECT{cr.right - 38, 0, cr.right - 2, notifHeaderH}
	notifUser32.NewProc("DrawTextW").Call(
		hdc, uintptr(unsafe.Pointer(xStr)), ^uintptr(0),
		uintptr(unsafe.Pointer(&closeR)),
		0x0825) // DT_SINGLELINE|DT_VCENTER|DT_CENTER|DT_NOPREFIX

	notifGdi32.NewProc("SelectObject").Call(hdc, origFont)
	notifGdi32.NewProc("DeleteObject").Call(titleFont)

	// â”€â”€ Message body â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	msgFont, _, _ := notifGdi32.NewProc("CreateFontW").Call(
		15, 0, 0, 0,
		400, 0, 0, 0,
		1, 0, 0,
		5, 0,
		uintptr(unsafe.Pointer(segoeUI)))
	notifGdi32.NewProc("SelectObject").Call(hdc, msgFont)
	notifGdi32.NewProc("SetTextColor").Call(hdc, notifColText)

	y := notifHeaderH + notifPadY
	for i, m := range notifState.msgs {
		if i > 0 {
			// Thin separator between stacked messages.
			sepBrush, _, _ := notifGdi32.NewProc("CreateSolidBrush").Call(notifColSep)
			sepR := notifRECT{notifPadX, y - 5, cr.right - notifPadX, y - 4}
			notifUser32.NewProc("FillRect").Call(hdc, uintptr(unsafe.Pointer(&sepR)), sepBrush)
			notifGdi32.NewProc("DeleteObject").Call(sepBrush)
		}
		txt, _ := syscall.UTF16PtrFromString(m)
		msgR := notifRECT{notifPadX, y, cr.right - notifPadX, y + notifMsgH}
		notifUser32.NewProc("DrawTextW").Call(
			hdc, uintptr(unsafe.Pointer(txt)), ^uintptr(0),
			uintptr(unsafe.Pointer(&msgR)),
			0x0810) // DT_WORDBREAK|DT_NOPREFIX
		y += notifMsgH + notifSepH
	}

	notifGdi32.NewProc("SelectObject").Call(hdc, origFont)
	notifGdi32.NewProc("DeleteObject").Call(msgFont)
}

func notifModuleHandle() uintptr {
	h, _, _ := notifKernel32.NewProc("GetModuleHandleW").Call(0)
	return h
}
