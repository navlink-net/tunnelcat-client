// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

// Recommend-a-member dialog: a minimal, standalone text-input prompt for
// "Recommend new Cat Club members" (see tunnel_cat/docs/club-membership.md).
// Deliberately a SEPARATE window class and separate state from the
// activation-key dialog in dialog.go -- that dialog validates input as a
// key string and always shows a QR-scan button, neither of which apply
// here, and generalizing it risked destabilizing the login flow for a
// nice-to-have social feature. This file mirrors its structure closely but
// touches none of its code.

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	recDlgOnce     sync.Once
	recDlgCallback uintptr
)

// recDlgState is accessed only from the message-loop goroutine (WndProc is
// called synchronously from DispatchMessage), same assumption as keyDlgState.
var recDlgState struct {
	editHwnd uintptr
	text     string
	accepted bool
}

func recDlgWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	const (
		wmCreate  = 0x0001
		wmDestroy = 0x0002
		wmCommand = 0x0111

		wsChild         = 0x40000000
		wsVisible       = 0x10000000
		wsTabStop       = 0x00010000
		esAutoHScroll   = 0x0080
		exClientEdge    = 0x00000200
		bsDefPushButton = 0x00000001

		idEdit   = 100
		idOK     = 1
		idCancel = 2
	)

	switch msg {
	case wmCreate:
		staticClass, _ := syscall.UTF16PtrFromString("STATIC")
		labelText, _ := syscall.UTF16PtrFromString("Username to recommend for Cat Club:")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(staticClass)),
			uintptr(unsafe.Pointer(labelText)),
			wsChild|wsVisible,
			20, 18, 440, 20,
			hwnd, 0,
			dlgKernelHandle(), 0)

		editClass, _ := syscall.UTF16PtrFromString("EDIT")
		recDlgState.editHwnd, _, _ = dlgUser32.NewProc("CreateWindowExW").Call(
			exClientEdge,
			uintptr(unsafe.Pointer(editClass)),
			0,
			wsChild|wsVisible|wsTabStop|esAutoHScroll,
			20, 46, 440, 24,
			hwnd, idEdit,
			dlgKernelHandle(), 0)

		btnClass, _ := syscall.UTF16PtrFromString("BUTTON")

		okText, _ := syscall.UTF16PtrFromString("Recommend")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(btnClass)),
			uintptr(unsafe.Pointer(okText)),
			wsChild|wsVisible|wsTabStop|bsDefPushButton,
			215, 88, 120, 26,
			hwnd, idOK,
			dlgKernelHandle(), 0)

		cancelText, _ := syscall.UTF16PtrFromString("Cancel")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(btnClass)),
			uintptr(unsafe.Pointer(cancelText)),
			wsChild|wsVisible|wsTabStop,
			345, 88, 100, 26,
			hwnd, idCancel,
			dlgKernelHandle(), 0)

		dlgUser32.NewProc("SetFocus").Call(recDlgState.editHwnd)
		return 0

	case wmCommand:
		id := wParam & 0xFFFF
		switch id {
		case idOK:
			var buf [256]uint16
			dlgUser32.NewProc("GetWindowTextW").Call(
				recDlgState.editHwnd,
				uintptr(unsafe.Pointer(&buf[0])),
				uintptr(len(buf)))
			text := syscall.UTF16ToString(buf[:])
			if text == "" {
				return 0 // empty submit is a no-op, not a cancel
			}
			recDlgState.text = text
			recDlgState.accepted = true
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0

		case idCancel:
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0
		}

	case wmDestroy:
		dlgUser32.NewProc("PostQuitMessage").Call(0)
		return 0
	}

	r, _, _ := dlgUser32.NewProc("DefWindowProcW").Call(hwnd, msg, wParam, lParam)
	return r
}

// ShowRecommendDialog opens a native Win32 input dialog asking for a
// username to recommend for Cat Club. Returns (username, true) on submit,
// ("", false) on Cancel or error.
func ShowRecommendDialog() (string, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	recDlgOnce.Do(func() {
		recDlgCallback = syscall.NewCallback(recDlgWndProc)
	})
	recDlgState = struct {
		editHwnd uintptr
		text     string
		accepted bool
	}{}

	const (
		wsPopup   = 0x80000000
		wsCaption = 0x00C00000
		wsSysMenu = 0x00080000

		exDlgModalFrame = 0x00000001
		exTopMost       = 0x00000008

		clientW = int32(480)
		clientH = int32(130)
	)

	className, _ := syscall.UTF16PtrFromString("SNCRecommendDlg")
	titleText, _ := syscall.UTF16PtrFromString("ShortNerdCat — Recommend a Member")

	wc := keyDlgWNDCLASSEX{
		cbSize:        uint32(unsafe.Sizeof(keyDlgWNDCLASSEX{})),
		lpfnWndProc:   recDlgCallback,
		hInstance:     dlgKernelHandle(),
		hbrBackground: 15, // COLOR_3DFACE (= 14) + 1 = 15
		lpszClassName: className,
	}
	dlgUser32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc))) // ignore error — class may already be registered

	rect := [4]int32{0, 0, clientW, clientH}
	dlgUser32.NewProc("AdjustWindowRectEx").Call(
		uintptr(unsafe.Pointer(&rect[0])),
		wsPopup|wsCaption|wsSysMenu,
		0,
		exDlgModalFrame)
	dlgW := rect[2] - rect[0]
	dlgH := rect[3] - rect[1]

	sw, _, _ := dlgUser32.NewProc("GetSystemMetrics").Call(0) // SM_CXSCREEN
	sh, _, _ := dlgUser32.NewProc("GetSystemMetrics").Call(1) // SM_CYSCREEN
	x := (int32(sw) - dlgW) / 2
	y := (int32(sh) - dlgH) / 2

	hwnd, _, _ := dlgUser32.NewProc("CreateWindowExW").Call(
		exDlgModalFrame|exTopMost,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(titleText)),
		wsPopup|wsCaption|wsSysMenu,
		uintptr(x), uintptr(y), uintptr(dlgW), uintptr(dlgH),
		0, 0, uintptr(dlgKernelHandle()), 0)
	if hwnd == 0 {
		return "", false
	}

	dlgUser32.NewProc("ShowWindow").Call(hwnd, 5) // SW_SHOW
	dlgUser32.NewProc("UpdateWindow").Call(hwnd)

	var msg [7]uintptr
	for {
		r, _, _ := dlgUser32.NewProc("GetMessageW").Call(
			uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		isDialogMsg, _, _ := dlgUser32.NewProc("IsDialogMessageW").Call(
			hwnd, uintptr(unsafe.Pointer(&msg[0])))
		if isDialogMsg != 0 {
			continue
		}
		dlgUser32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg[0])))
		dlgUser32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg[0])))
	}

	var peek [7]uintptr
	dlgUser32.NewProc("PeekMessageW").Call(
		uintptr(unsafe.Pointer(&peek[0])), 0,
		0x0012, 0x0012, // WM_QUIT
		1) // PM_REMOVE

	return recDlgState.text, recDlgState.accepted
}
