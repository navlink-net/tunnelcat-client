// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	_ "embed"
	"runtime/debug"
	"unsafe"

	"golang.org/x/sys/windows"
	"tunnel_cat/snc/core"
)

//go:embed assets/bg.png
var uiBgPNG []byte

// â”€â”€ Win32 helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

var (
	winUser32 = windows.NewLazySystemDLL("user32.dll")

	winSetWindowLongPtrW        = winUser32.NewProc("SetWindowLongPtrW")
	winCallWindowProcW          = winUser32.NewProc("CallWindowProcW")
	winDefWindowProcW           = winUser32.NewProc("DefWindowProcW")
	winShowWindowProc           = winUser32.NewProc("ShowWindow")
	winSetForegroundWindow      = winUser32.NewProc("SetForegroundWindow")
	winIsWindowVisible          = winUser32.NewProc("IsWindowVisible")
	winFindWindowW              = winUser32.NewProc("FindWindowW")
	winSendMessageW             = winUser32.NewProc("SendMessageW")
	winSetWindowPos             = winUser32.NewProc("SetWindowPos")
	winCreateIconFromResourceEx = winUser32.NewProc("CreateIconFromResourceEx")
	winDestroyIconFn            = winUser32.NewProc("DestroyIcon")
	winGetWindowRect            = winUser32.NewProc("GetWindowRect")
)

const (
	wvSWHide    uintptr = 0
	wvSWRestore uintptr = 9
	wvWMClose   uintptr = 0x0010
	wvWMSetIcon uintptr = 0x0080
	wvIconSmall uintptr = 0
	wvIconBig   uintptr = 1
	// GWLP_WNDPROC = -4 as uintptr on 64-bit Windows
	wvGWLPWndProc = ^uintptr(3)
)

// â”€â”€ AppStatus / AppSettings â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// AppStatus is the tunnel state, used by both the native panel and tray.
type AppStatus struct {
	Connected     bool   `json:"connected"`
	Connecting    bool   `json:"connecting"`
	Disconnecting bool   `json:"disconnecting"`
	Error         bool   `json:"error"`    // last connect/login attempt failed (see TrayApp.setTrayIcon)
	ErrorMsg      string `json:"errorMsg"` // human-readable reason, e.g. "Connect failed: dial tcp ...: timeout"
	Mode          string `json:"mode"`     // "direct"
	Elapsed       string `json:"elapsed"`  // "01:23:45" or ""
}

// AppSettings mirrors the persistent settings.
type AppSettings struct {
	DoH       bool   `json:"doh"`
	BlockQUIC bool   `json:"blockQUIC"`
	Region    string `json:"region"` // "" = Auto; "RU"/"EU"/"US"/"CN"/"XX"
}

// â”€â”€ setWindowIconFromICO â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// setWindowIconFromICO sets the window icon from raw ICO bytes (as produced by
// pngToICO). Skips the 22-byte ICONDIR + ICONDIRENTRY header.
// Returns the newly created HICON (caller must eventually call DestroyIcon on it).
func setWindowIconFromICO(hwnd uintptr, icoData []byte) uintptr {
	core.Log.Printf("window: setWindowIconFromICO: icoData len=%d", len(icoData))
	if len(icoData) <= 22 {
		return 0
	}
	imgData := icoData[22:]
	hIcon, _, lastErr := winCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&imgData[0])),
		uintptr(len(imgData)),
		1, 0x00030000, 32, 32, 0,
	)
	core.Log.Printf("window: CreateIconFromResourceEx hIcon=0x%x err=%v", hIcon, lastErr)
	if hIcon == 0 {
		return 0
	}
	winSendMessageW.Call(hwnd, wvWMSetIcon, wvIconSmall, hIcon)
	winSendMessageW.Call(hwnd, wvWMSetIcon, wvIconBig, hIcon)
	// SWP_FRAMECHANGED forces the shell to repaint the taskbar button with the
	// new icon; WM_SETICON alone does not always trigger a taskbar refresh.
	const swpNoSize, swpNoMove, swpNoZOrder, swpFrameChanged = 0x0001, 0x0002, 0x0004, 0x0020
	winSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoSize|swpNoMove|swpNoZOrder|swpFrameChanged)
	return hIcon
}

// â”€â”€ InstallTrayLClick â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// InstallTrayLClick subclasses the hidden SystrayClass window so that a
// left-click on the tray icon calls fn instead of showing the menu.
func InstallTrayLClick(fn func()) {
	if fn == nil {
		return
	}
	const (
		wmUser       = 0x0400
		wmSystrayMsg = wmUser + 1
		wmLButtonUp  = 0x0202
		wmLButtonDbl = 0x0203
	)
	core.Log.Printf("tray: InstallTrayLClick: searching for SystrayClass window...")
	classPtr, _ := windows.UTF16PtrFromString("SystrayClass")
	hwnd, _, _ := winFindWindowW.Call(uintptr(unsafe.Pointer(classPtr)), 0)
	if hwnd == 0 {
		core.Log.Printf("tray: SystrayClass window not found â€” left-click handler not installed")
		return
	}
	core.Log.Printf("tray: SystrayClass hwnd=0x%x", hwnd)

	cb := windows.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		defer func() {
			if r := recover(); r != nil {
				core.Log.Printf("tray: WndProc PANIC: msg=0x%x panic=%v\n%s", msg, r, debug.Stack())
			}
		}()
		if msg == wmSystrayMsg {
			core.Log.Printf("tray: WndProc: systray msg lp=0x%x", lp)
			if lp == wmLButtonUp || lp == wmLButtonDbl {
				core.Log.Printf("tray: left-click detected â€” calling fn")
				go fn()
				return 0
			}
		}
		if trayLClickOrigProc == 0 {
			r, _, _ := winDefWindowProcW.Call(h, msg, wp, lp)
			return r
		}
		r, _, _ := winCallWindowProcW.Call(trayLClickOrigProc, h, msg, wp, lp)
		return r
	})
	orig, _, lastErr := winSetWindowLongPtrW.Call(hwnd, wvGWLPWndProc, cb)
	core.Log.Printf("tray: SetWindowLongPtrW: origProc=0x%x err=%v", orig, lastErr)
	trayLClickOrigProc = orig
	trayLClickCB = cb
}

var (
	trayLClickOrigProc uintptr
	trayLClickCB       uintptr //nolint:unused
)
