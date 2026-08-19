// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

// Package windows â€” splash screen.
//
// Architecture: pure-Go PNG decode/scale â†’ 32-bpp premultiplied-alpha DIB
// â†’ UpdateLayeredWindow.  The logo PNG is rendered on a fully transparent
// background so the logo floats on the desktop with no underlay.
//
// UpdateLayeredWindow (not WM_PAINT/BitBlt) is used because it is the only
// mechanism that supports per-pixel alpha compositing on the desktop.

package windows

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"tunnel_cat/snc/core"
)

//go:embed assets/logo.png
var logoPNG []byte

// ShowSplash shows a splash screen centred on the primary monitor and blocks
// for exactly duration (not counting decode/scale/render time).
// Safe to call from any goroutine â€” it locks its OS thread internally.
func ShowSplash(version string, duration time.Duration) {
	// Lock to a single OS thread so that:
	//   (a) the Win32 window and the GetMessageW loop are on the same thread, and
	//   (b) no other goroutine's windows share this thread's message queue
	//       (preventing spurious WM_TIMER / WM_QUIT messages).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	core.Log.Printf("splash: start version=%s", version)

	// â”€â”€ 1. Decode and scale the logo â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	src, err := png.Decode(bytes.NewReader(logoPNG))
	if err != nil {
		core.Log.Printf("splash: PNG decode failed: %v", err)
		return
	}
	core.Log.Printf("splash: logo decoded %dx%d", src.Bounds().Dx(), src.Bounds().Dy())

	const logoH = 360
	sb := src.Bounds()
	logoW := int32(float64(logoH) * float64(sb.Dx()) / float64(sb.Dy()))
	logo := bilinearScale(src, int(logoW), logoH)

	drawSplashText(logo, version)

	canvasW := logoW
	canvasH := int32(logoH)

	// â”€â”€ 2. Create 32-bpp top-down DIB (all zero = fully transparent) â”€â”€â”€â”€â”€â”€â”€â”€â”€
	screenDC, _, _ := splashUser32.NewProc("GetDC").Call(0)
	memDC, _, _ := splashGdi32.NewProc("CreateCompatibleDC").Call(screenDC)
	splashUser32.NewProc("ReleaseDC").Call(0, screenDC)
	defer splashGdi32.NewProc("DeleteDC").Call(memDC)

	bi := splashBMI{}
	bi.h.biSize = uint32(unsafe.Sizeof(bi.h))
	bi.h.biWidth = canvasW
	bi.h.biHeight = -canvasH // negative = top-down
	bi.h.biPlanes = 1
	bi.h.biBitCount = 32

	var dibBits unsafe.Pointer
	hbm, _, _ := splashGdi32.NewProc("CreateDIBSection").Call(
		memDC,
		uintptr(unsafe.Pointer(&bi)),
		0,
		uintptr(unsafe.Pointer(&dibBits)),
		0, 0)
	if hbm == 0 {
		core.Log.Printf("splash: CreateDIBSection failed (canvas %dx%d)", canvasW, canvasH)
		return
	}
	core.Log.Printf("splash: DIB created %dx%d", canvasW, canvasH)
	oldBM, _, _ := splashGdi32.NewProc("SelectObject").Call(memDC, hbm)
	defer func() {
		splashGdi32.NewProc("SelectObject").Call(memDC, oldBM)
		splashGdi32.NewProc("DeleteObject").Call(hbm)
	}()

	n := int(canvasW * canvasH)
	pixels := (*[1 << 26]uint32)(dibBits)[:n:n]
	// All zero = transparent (A=0, RGB=0) â€” background is the desktop.

	// â”€â”€ 3. Write logo pixels with premultiplied alpha â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	//
	// UpdateLayeredWindow with ULW_ALPHA requires premultiplied BGRA:
	//   each of R, G, B = channel * A / 255.
	// Where the logo is transparent (A=0), leave pixels at zero.
	lb := logo.Bounds()
	for y := 0; y < lb.Dy(); y++ {
		for x := 0; x < lb.Dx(); x++ {
			c := logo.NRGBAAt(x, y)
			a := uint32(c.A)
			if a == 0 {
				continue // fully transparent â€” leave as 0
			}
			r := uint32(c.R) * a / 255
			g := uint32(c.G) * a / 255
			b := uint32(c.B) * a / 255
			// DIB little-endian uint32: byte0=B, byte1=G, byte2=R, byte3=A
			pixels[y*int(canvasW)+x] = b | (g << 8) | (r << 16) | (a << 24)
		}
	}

	// â”€â”€ 4. Create WS_EX_LAYERED window (hidden) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	hwnd := splashCreateLayeredWindow(canvasW, canvasH)
	if hwnd == 0 {
		core.Log.Printf("splash: CreateWindowExW failed")
		return
	}
	defer splashUser32.NewProc("DestroyWindow").Call(hwnd)
	core.Log.Printf("splash: window created hwnd=%x", hwnd)

	// â”€â”€ 5. UpdateLayeredWindow: set content and position â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	sw, _, _ := splashUser32.NewProc("GetSystemMetrics").Call(0) // SM_CXSCREEN
	sh, _, _ := splashUser32.NewProc("GetSystemMetrics").Call(1) // SM_CYSCREEN
	x := (int32(sw) - canvasW) / 2
	y := (int32(sh) - canvasH) / 2

	hdcScreen, _, _ := splashUser32.NewProc("GetDC").Call(0)
	pptDst := [2]int32{x, y}     // destination position on screen
	psize := [2]int32{canvasW, canvasH}
	pptSrc := [2]int32{0, 0}     // source origin in memDC
	// BLENDFUNCTION: BlendOp=AC_SRC_OVER(0), BlendFlags=0,
	//   SourceConstantAlpha=255 (per-pixel alpha), AlphaFormat=AC_SRC_ALPHA(1)
	blend := [4]byte{0, 0, 255, 1}
	ret, _, errWin := splashUser32.NewProc("UpdateLayeredWindow").Call(
		hwnd,
		hdcScreen,
		uintptr(unsafe.Pointer(&pptDst[0])),
		uintptr(unsafe.Pointer(&psize[0])),
		memDC,
		uintptr(unsafe.Pointer(&pptSrc[0])),
		0,
		uintptr(unsafe.Pointer(&blend[0])),
		2) // ULW_ALPHA
	splashUser32.NewProc("ReleaseDC").Call(0, hdcScreen)
	core.Log.Printf("splash: UpdateLayeredWindow ret=%d err=%v pos=(%d,%d) size=%dx%d",
		ret, errWin, x, y, canvasW, canvasH)

	// Show the window â€” content is already set by UpdateLayeredWindow.
	splashUser32.NewProc("ShowWindow").Call(hwnd, 5) // SW_SHOW
	splashUser32.NewProc("SetForegroundWindow").Call(hwnd)
	core.Log.Printf("splash: shown, entering message loop")

	// â”€â”€ 6. Message loop â€” exit on timer or left-click â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	timerID, _, _ := splashUser32.NewProc("SetTimer").Call(hwnd, 1, uintptr(duration.Milliseconds()), 0)
	core.Log.Printf("splash: timer set id=%d ms=%d", timerID, duration.Milliseconds())

	var msg [7]uintptr
	for {
		r, _, _ := splashUser32.NewProc("GetMessageW").Call(
			uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			core.Log.Printf("splash: GetMessageW=%d (WM_QUIT or error)", r)
			break
		}
		msgID := uint32(msg[1])
		// Only dismiss on OUR timer (hwnd + timer ID 1), not any WM_TIMER on
		// this thread (other windows may share the message queue).
		if msgID == 0x0113 && msg[0] == hwnd && msg[2] == 1 { // WM_TIMER, ours
			core.Log.Printf("splash: dismissed by WM_TIMER")
			break
		}
		if msgID == 0x0201 { // WM_LBUTTONDOWN
			core.Log.Printf("splash: dismissed by click")
			break
		}
		splashUser32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg[0])))
		splashUser32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg[0])))
	}
	core.Log.Printf("splash: done")

	// Drain any stray WM_QUIT before returning so the next message loop
	// (tray, key dialog) is not poisoned.
	var peek [7]uintptr
	splashUser32.NewProc("PeekMessageW").Call(
		uintptr(unsafe.Pointer(&peek[0])), 0,
		0x0012, 0x0012, 1) // WM_QUIT, PM_REMOVE
}

// splashCreateLayeredWindow creates a WS_EX_LAYERED | WS_POPUP window.
// Content is supplied by UpdateLayeredWindow, not WM_PAINT.
func splashCreateLayeredWindow(w, h int32) uintptr {
	splashWndProcOnce.Do(func() {
		splashWndProcCB = syscall.NewCallback(splashDefWndProc)
	})

	className, _ := syscall.UTF16PtrFromString("SNCsplash")
	windowName, _ := syscall.UTF16PtrFromString("ShortNerdCat")

	wc := splashWNDCLASSEX{
		cbSize:        uint32(unsafe.Sizeof(splashWNDCLASSEX{})),
		lpfnWndProc:   splashWndProcCB,
		hInstance:     splashGetModuleHandle(),
		lpszClassName: className,
		// No hbrBackground â€” layered windows own their rendering entirely.
	}
	// Ignore error â€” class may already be registered on a second call (About).
	splashUser32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc)))

	const (
		wsExLayered    = 0x00080000
		wsExTopmost    = 0x00000008
		wsExToolWindow = 0x00000080
		wsPopup        = 0x80000000
	)
	hwnd, _, _ := splashUser32.NewProc("CreateWindowExW").Call(
		wsExLayered|wsExTopmost|wsExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		wsPopup,
		0, 0, uintptr(w), uintptr(h),
		0, 0, uintptr(splashGetModuleHandle()), 0)
	return hwnd
}

// splashDefWndProc is the minimal WndProc for the layered splash window.
// All rendering is handled by UpdateLayeredWindow; only DefWindowProcW is needed.
func splashDefWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	r, _, _ := splashUser32.NewProc("DefWindowProcW").Call(hwnd, msg, wp, lp)
	return r
}

func splashGetModuleHandle() uintptr {
	h, _, _ := splashKernel32.NewProc("GetModuleHandleW").Call(0)
	return h
}

// drawSplashText renders "Tunnel Cat" and the version string onto img.
// Text is drawn white with a dark shadow for readability over any background.
func drawSplashText(img *image.NRGBA, version string) {
	boldTTF, err := opentype.Parse(gobold.TTF)
	if err != nil {
		core.Log.Printf("splash: parse bold font: %v", err)
		return
	}
	regularTTF, err := opentype.Parse(goregular.TTF)
	if err != nil {
		core.Log.Printf("splash: parse regular font: %v", err)
		return
	}

	titleFace, err := opentype.NewFace(boldTTF, &opentype.FaceOptions{
		Size:    42,
		DPI:     96,
		Hinting: font.HintingFull,
	})
	if err != nil {
		core.Log.Printf("splash: title face: %v", err)
		return
	}
	defer titleFace.Close()

	verFace, err := opentype.NewFace(regularTTF, &opentype.FaceOptions{
		Size:    20,
		DPI:     96,
		Hinting: font.HintingFull,
	})
	if err != nil {
		core.Log.Printf("splash: version face: %v", err)
		return
	}
	defer verFace.Close()

	b := img.Bounds()
	w := b.Dx()
	h := b.Dy()

	const title = "Tunnel Cat"

	titleW := (&font.Drawer{Face: titleFace}).MeasureString(title).Ceil()
	verW := (&font.Drawer{Face: verFace}).MeasureString(version).Ceil()

	// Title near the top, version near the bottom.
	titleY := titleFace.Metrics().Ascent.Ceil() + 10
	verY := h - verFace.Metrics().Descent.Ceil() - 10

	shadow := image.NewUniform(color.NRGBA{0, 0, 0, 160})
	white := image.NewUniform(color.NRGBA{255, 255, 255, 255})

	for _, pass := range []struct {
		dx, dy int
		src    image.Image
	}{
		{2, 2, shadow},
		{0, 0, white},
	} {
		d := &font.Drawer{Dst: img, Src: pass.src, Face: titleFace,
			Dot: fixed.P((w-titleW)/2+pass.dx, titleY+pass.dy)}
		d.DrawString(title)
		d = &font.Drawer{Dst: img, Src: pass.src, Face: verFace,
			Dot: fixed.P((w-verW)/2+pass.dx, verY+pass.dy)}
		d.DrawString(version)
	}
}

// bilinearScale returns a new NRGBA image scaled to (dstW Ã— dstH).
func bilinearScale(src image.Image, dstW, dstH int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	sb := src.Bounds()
	srcW, srcH := sb.Dx(), sb.Dy()
	for dy := 0; dy < dstH; dy++ {
		sy := float64(dy) * float64(srcH-1) / float64(dstH-1)
		y0 := int(sy)
		y1 := y0 + 1
		if y1 >= srcH {
			y1 = srcH - 1
		}
		fy := sy - float64(y0)
		for dx := 0; dx < dstW; dx++ {
			sx := float64(dx) * float64(srcW-1) / float64(dstW-1)
			x0 := int(sx)
			x1 := x0 + 1
			if x1 >= srcW {
				x1 = srcW - 1
			}
			fx := sx - float64(x0)
			c00 := toNRGBA(src.At(sb.Min.X+x0, sb.Min.Y+y0))
			c10 := toNRGBA(src.At(sb.Min.X+x1, sb.Min.Y+y0))
			c01 := toNRGBA(src.At(sb.Min.X+x0, sb.Min.Y+y1))
			c11 := toNRGBA(src.At(sb.Min.X+x1, sb.Min.Y+y1))
			dst.SetNRGBA(dx, dy, color.NRGBA{
				R: lerp2(c00.R, c10.R, c01.R, c11.R, fx, fy),
				G: lerp2(c00.G, c10.G, c01.G, c11.G, fx, fy),
				B: lerp2(c00.B, c10.B, c01.B, c11.B, fx, fy),
				A: lerp2(c00.A, c10.A, c01.A, c11.A, fx, fy),
			})
		}
	}
	return dst
}

func toNRGBA(c color.Color) color.NRGBA {
	r, g, b, a := c.RGBA()
	if a == 0 {
		return color.NRGBA{}
	}
	return color.NRGBA{
		R: uint8(r * 255 / a),
		G: uint8(g * 255 / a),
		B: uint8(b * 255 / a),
		A: uint8(a >> 8),
	}
}

func lerp2(v00, v10, v01, v11 uint8, fx, fy float64) uint8 {
	top := float64(v00)*(1-fx) + float64(v10)*fx
	bot := float64(v01)*(1-fx) + float64(v11)*fx
	return uint8(top*(1-fy) + bot*fy)
}

// â”€â”€ package-level DLL handles â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

var (
	splashUser32   = syscall.NewLazyDLL("user32.dll")
	splashGdi32    = syscall.NewLazyDLL("gdi32.dll")
	splashKernel32 = syscall.NewLazyDLL("kernel32.dll")
)

var (
	splashWndProcOnce sync.Once
	splashWndProcCB   uintptr
)

// â”€â”€ Win32 structures â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type splashBMIHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type splashBMI struct {
	h    splashBMIHeader
	_rgb [1]uint32
}

type splashWNDCLASSEX struct {
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
