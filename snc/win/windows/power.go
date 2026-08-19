// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"runtime"
	"syscall"
	"unsafe"

	"tunnel_cat/snc/core"
)

var (
	modUser32           = syscall.MustLoadDLL("user32.dll")
	procRegisterClassEx = modUser32.MustFindProc("RegisterClassExW")
	procCreateWindowEx  = modUser32.MustFindProc("CreateWindowExW")
	procDefWindowProc   = modUser32.MustFindProc("DefWindowProcW")
	procGetMessage      = modUser32.MustFindProc("GetMessageW")
	procDispatchMessage = modUser32.MustFindProc("DispatchMessageW")
	procTranslateMsg    = modUser32.MustFindProc("TranslateMessage")
	procGetModuleHandle = modKernel32.MustFindProc("GetModuleHandleW")
)

const (
	wmPowerBroadcast      uintptr = 0x0218
	pbsApmResumeAutomatic uintptr = 0x0012 // automatic wake (sleep or hibernate)
	pbsApmResumeSuspend   uintptr = 0x0007 // user-initiated wake
	hwndMessage           uintptr = ^uintptr(2) // HWND_MESSAGE = (HWND)-3
)

// wndClassExW mirrors the Win32 WNDCLASSEXW structure (x64 layout).
type wndClassExW struct {
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

// WatchPowerEvents starts a background goroutine that receives Windows
// sleep/hibernate/wake events via a hidden message-only Win32 window.
// onResume is called (in a new goroutine) each time the system wakes from
// sleep or hibernate (PBT_APMRESUMEAUTOMATIC or PBT_APMRESUMESUSPEND).
func WatchPowerEvents(onResume func()) {
	if onResume == nil {
		return
	}
	go runPowerWatcher(onResume)
}

func runPowerWatcher(onResume func()) {
	// Win32 message loops must stay on the same OS thread they were created on.
	runtime.LockOSThread()

	hInst, _, _ := procGetModuleHandle.Call(0)
	className, _ := syscall.UTF16PtrFromString("SNCPowerWatcher")

	wndProc := syscall.NewCallback(func(hwnd, uMsg, wParam, lParam uintptr) uintptr {
		if uMsg == wmPowerBroadcast &&
			(wParam == pbsApmResumeAutomatic || wParam == pbsApmResumeSuspend) {
			core.Log.Printf("power: wake event 0x%x â€” triggering reconnect", wParam)
			// Refresh LastAlive immediately so the watchdog does not mistake
			// a hibernate resume for a frozen process (the on-disk timestamp
			// is stale by the entire sleep duration).
			core.TouchAlive()
			go onResume()
		}
		r, _, _ := procDefWindowProc.Call(hwnd, uMsg, wParam, lParam)
		return r
	})

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   wndProc,
		hInstance:     hInst,
		lpszClassName: className,
	}
	if atom, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		core.Log.Printf("power: RegisterClassExW failed â€” power events unavailable")
		return
	}

	hwnd, _, _ := procCreateWindowEx.Call(
		0,                                  // dwExStyle
		uintptr(unsafe.Pointer(className)), // lpClassName
		uintptr(unsafe.Pointer(className)), // lpWindowName
		0,                                  // dwStyle (invisible)
		0, 0, 0, 0,                         // x, y, w, h
		hwndMessage,                        // hWndParent = HWND_MESSAGE (message-only window)
		0, hInst, 0,                        // hMenu, hInstance, lpParam
	)
	if hwnd == 0 {
		core.Log.Printf("power: CreateWindowExW failed â€” power events unavailable")
		return
	}
	core.Log.Println("power: watcher started")

	// Pump Win32 messages; WndProc handles WM_POWERBROADCAST.
	// Using a plain [64]byte buffer avoids importing x/sys/windows for MSG.
	var m [64]byte
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m[0])), 0, 0, 0)
		if int32(r) <= 0 {
			break // WM_QUIT (0) or error (-1)
		}
		procTranslateMsg.Call(uintptr(unsafe.Pointer(&m[0])))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m[0])))
	}
}
