// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// ── "Do you have a key?" prompt ─────────────────────────────────────────────

var (
	haveKeyDlgOnce     sync.Once
	haveKeyDlgCallback uintptr
)

var haveKeyDlgState struct {
	hasKey   bool
	answered bool
}

func haveKeyDlgWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	const (
		wmCreate  = 0x0001
		wmDestroy = 0x0002
		wmCommand = 0x0111

		wsChild   = 0x40000000
		wsVisible = 0x10000000
		wsTabStop = 0x00010000

		bsDefPushButton = 0x00000001

		idYes = uintptr(1)
		idNo  = uintptr(2)
	)

	switch msg {
	case wmCreate:
		staticClass, _ := syscall.UTF16PtrFromString("STATIC")
		labelText, _ := syscall.UTF16PtrFromString("Do you have a ShortNerdCat activation key?")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(staticClass)),
			uintptr(unsafe.Pointer(labelText)),
			wsChild|wsVisible,
			20, 20, 440, 40,
			hwnd, 0,
			dlgKernelHandle(), 0)

		btnClass, _ := syscall.UTF16PtrFromString("BUTTON")

		yesText, _ := syscall.UTF16PtrFromString("Yes, I have a key")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(btnClass)),
			uintptr(unsafe.Pointer(yesText)),
			wsChild|wsVisible|wsTabStop|bsDefPushButton,
			20, 74, 210, 30,
			hwnd, idYes,
			dlgKernelHandle(), 0)

		noText, _ := syscall.UTF16PtrFromString("No, I don't have one")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(btnClass)),
			uintptr(unsafe.Pointer(noText)),
			wsChild|wsVisible|wsTabStop,
			250, 74, 210, 30,
			hwnd, idNo,
			dlgKernelHandle(), 0)
		return 0

	case wmCommand:
		switch wParam & 0xFFFF {
		case idYes:
			haveKeyDlgState.hasKey = true
			haveKeyDlgState.answered = true
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0
		case idNo:
			haveKeyDlgState.hasKey = false
			haveKeyDlgState.answered = true
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

// ShowHaveKeyPrompt asks the user whether they already have an activation
// key. ok is false if the dialog was dismissed without an answer (e.g.
// closed via Alt+F4) — callers should treat that like Cancel.
func ShowHaveKeyPrompt() (hasKey bool, ok bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	haveKeyDlgOnce.Do(func() {
		haveKeyDlgCallback = syscall.NewCallback(haveKeyDlgWndProc)
	})
	haveKeyDlgState = struct {
		hasKey   bool
		answered bool
	}{}

	hwnd := createSimpleDialog("SNCHaveKeyDlg", "ShortNerdCat — Get Started", haveKeyDlgCallback, 480, 130)
	if hwnd == 0 {
		return false, false
	}
	runDialogMessageLoop(hwnd)
	return haveKeyDlgState.hasKey, haveKeyDlgState.answered
}

// ── Credential login dialog ─────────────────────────────────────────────────

var (
	loginDlgOnce     sync.Once
	loginDlgCallback uintptr
)

var loginDlgState struct {
	emailHwnd     uintptr
	passwordHwnd  uintptr
	eyeHwnd       uintptr
	email         string
	password      string
	accepted      bool
	wantsKeyMode  bool
	passwordShown bool
}

func loginDlgWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	const (
		wmCreate  = 0x0001
		wmDestroy = 0x0002
		wmCommand = 0x0111

		wsChild   = 0x40000000
		wsVisible = 0x10000000
		wsTabStop = 0x00010000

		exClientEdge = 0x00000200

		esAutoHScroll   = 0x0080
		esPassword      = 0x0020
		bsDefPushButton = 0x00000001

		emSetPasswordChar = 0x00CC

		idOK      = uintptr(1)
		idCancel  = uintptr(2)
		idKeyMode = uintptr(3)
		idEmail   = uintptr(100)
		idPass    = uintptr(101)
		idEye     = uintptr(102)
	)

	switch msg {
	case wmCreate:
		staticClass, _ := syscall.UTF16PtrFromString("STATIC")
		editClass, _ := syscall.UTF16PtrFromString("EDIT")
		btnClass, _ := syscall.UTF16PtrFromString("BUTTON")

		emailLabel, _ := syscall.UTF16PtrFromString(T("email_label"))
		dlgUser32.NewProc("CreateWindowExW").Call(
			0, uintptr(unsafe.Pointer(staticClass)), uintptr(unsafe.Pointer(emailLabel)),
			wsChild|wsVisible, 20, 18, 440, 18, hwnd, 0, dlgKernelHandle(), 0)

		loginDlgState.emailHwnd, _, _ = dlgUser32.NewProc("CreateWindowExW").Call(
			exClientEdge, uintptr(unsafe.Pointer(editClass)), 0,
			wsChild|wsVisible|wsTabStop|esAutoHScroll,
			20, 38, 440, 24, hwnd, idEmail, dlgKernelHandle(), 0)

		passLabel, _ := syscall.UTF16PtrFromString(T("password_label"))
		dlgUser32.NewProc("CreateWindowExW").Call(
			0, uintptr(unsafe.Pointer(staticClass)), uintptr(unsafe.Pointer(passLabel)),
			wsChild|wsVisible, 20, 70, 440, 18, hwnd, 0, dlgKernelHandle(), 0)

		loginDlgState.passwordHwnd, _, _ = dlgUser32.NewProc("CreateWindowExW").Call(
			exClientEdge, uintptr(unsafe.Pointer(editClass)), 0,
			wsChild|wsVisible|wsTabStop|esAutoHScroll|esPassword,
			20, 90, 390, 24, hwnd, idPass, dlgKernelHandle(), 0)

		eyeText, _ := syscall.UTF16PtrFromString(T("show"))
		loginDlgState.eyeHwnd, _, _ = dlgUser32.NewProc("CreateWindowExW").Call(
			0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(eyeText)),
			wsChild|wsVisible|wsTabStop,
			420, 90, 40, 24, hwnd, idEye, dlgKernelHandle(), 0)

		loginText, _ := syscall.UTF16PtrFromString(T("login_button"))
		dlgUser32.NewProc("CreateWindowExW").Call(
			0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(loginText)),
			wsChild|wsVisible|wsTabStop|bsDefPushButton,
			20, 130, 140, 28, hwnd, idOK, dlgKernelHandle(), 0)

		cancelText, _ := syscall.UTF16PtrFromString(T("cancel"))
		dlgUser32.NewProc("CreateWindowExW").Call(
			0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(cancelText)),
			wsChild|wsVisible|wsTabStop,
			170, 130, 140, 28, hwnd, idCancel, dlgKernelHandle(), 0)

		keyModeText, _ := syscall.UTF16PtrFromString(T("i_have_a_key"))
		dlgUser32.NewProc("CreateWindowExW").Call(
			0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(keyModeText)),
			wsChild|wsVisible|wsTabStop,
			320, 130, 140, 28, hwnd, idKeyMode, dlgKernelHandle(), 0)

		dlgUser32.NewProc("SetFocus").Call(loginDlgState.emailHwnd)
		return 0

	case wmCommand:
		switch wParam & 0xFFFF {
		case idOK:
			loginDlgState.email = getWindowText(loginDlgState.emailHwnd)
			loginDlgState.password = getWindowText(loginDlgState.passwordHwnd)
			if loginDlgState.email == "" || loginDlgState.password == "" {
				return 0 // require both fields, keep dialog open
			}
			loginDlgState.accepted = true
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0
		case idCancel:
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0
		case idKeyMode:
			loginDlgState.wantsKeyMode = true
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0
		case idEye:
			loginDlgState.passwordShown = !loginDlgState.passwordShown
			if loginDlgState.passwordShown {
				dlgUser32.NewProc("SendMessageW").Call(loginDlgState.passwordHwnd, emSetPasswordChar, 0, 0)
				hideText, _ := syscall.UTF16PtrFromString(T("hide"))
				dlgUser32.NewProc("SetWindowTextW").Call(loginDlgState.eyeHwnd, uintptr(unsafe.Pointer(hideText)))
			} else {
				dlgUser32.NewProc("SendMessageW").Call(loginDlgState.passwordHwnd, emSetPasswordChar, uintptr('*'), 0)
				showText, _ := syscall.UTF16PtrFromString(T("show"))
				dlgUser32.NewProc("SetWindowTextW").Call(loginDlgState.eyeHwnd, uintptr(unsafe.Pointer(showText)))
			}
			dlgUser32.NewProc("InvalidateRect").Call(loginDlgState.passwordHwnd, 0, 1)
			return 0
		}

	case wmDestroy:
		dlgUser32.NewProc("PostQuitMessage").Call(0)
		return 0
	}

	r, _, _ := dlgUser32.NewProc("DefWindowProcW").Call(hwnd, msg, wParam, lParam)
	return r
}

// ShowLoginDialog prompts for navlink.net account credentials. wantsKeyMode
// is true if the user clicked "I Have a Key" instead (email/password/ok are
// then meaningless).
func ShowLoginDialog() (email, password string, ok bool, wantsKeyMode bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	loginDlgOnce.Do(func() {
		loginDlgCallback = syscall.NewCallback(loginDlgWndProc)
	})
	loginDlgState = struct {
		emailHwnd     uintptr
		passwordHwnd  uintptr
		eyeHwnd       uintptr
		email         string
		password      string
		accepted      bool
		wantsKeyMode  bool
		passwordShown bool
	}{}

	hwnd := createSimpleDialog("SNCLoginDlg", T("login_dialog_title"), loginDlgCallback, 480, 200)
	if hwnd == 0 {
		return "", "", false, false
	}
	runDialogMessageLoop(hwnd)
	return loginDlgState.email, loginDlgState.password, loginDlgState.accepted, loginDlgState.wantsKeyMode
}

// ── Shared dialog scaffolding (extracted from ShowKeyDialog's original body) ─

// createSimpleDialog registers (if needed) and creates a centered, modal-style
// popup window of the given client size, following the exact same
// WNDCLASSEX/AdjustWindowRectEx pattern as the key-entry dialog.
func createSimpleDialog(className, title string, wndProc uintptr, clientW, clientH int32) uintptr {
	const (
		wsPopup   = 0x80000000
		wsCaption = 0x00C00000
		wsSysMenu = 0x00080000

		exDlgModalFrame = 0x00000001
		exTopMost       = 0x00000008
	)

	classNameW, _ := syscall.UTF16PtrFromString(className)
	titleW, _ := syscall.UTF16PtrFromString(title)

	wc := keyDlgWNDCLASSEX{
		cbSize:        uint32(unsafe.Sizeof(keyDlgWNDCLASSEX{})),
		lpfnWndProc:   wndProc,
		hInstance:     dlgKernelHandle(),
		hbrBackground: 15, // COLOR_3DFACE + 1
		lpszClassName: classNameW,
	}
	dlgUser32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc))) // ignore error: may already be registered

	rect := [4]int32{0, 0, clientW, clientH}
	dlgUser32.NewProc("AdjustWindowRectEx").Call(
		uintptr(unsafe.Pointer(&rect[0])),
		wsPopup|wsCaption|wsSysMenu,
		0,
		exDlgModalFrame)
	dlgW := rect[2] - rect[0]
	dlgH := rect[3] - rect[1]

	sw, _, _ := dlgUser32.NewProc("GetSystemMetrics").Call(0)
	sh, _, _ := dlgUser32.NewProc("GetSystemMetrics").Call(1)
	x := (int32(sw) - dlgW) / 2
	y := (int32(sh) - dlgH) / 2

	hwnd, _, _ := dlgUser32.NewProc("CreateWindowExW").Call(
		exDlgModalFrame|exTopMost,
		uintptr(unsafe.Pointer(classNameW)),
		uintptr(unsafe.Pointer(titleW)),
		wsPopup|wsCaption|wsSysMenu,
		uintptr(x), uintptr(y), uintptr(dlgW), uintptr(dlgH),
		0, 0, uintptr(dlgKernelHandle()), 0)
	if hwnd == 0 {
		return 0
	}
	dlgUser32.NewProc("ShowWindow").Call(hwnd, 5) // SW_SHOW
	dlgUser32.NewProc("UpdateWindow").Call(hwnd)
	return hwnd
}

// runDialogMessageLoop pumps messages for hwnd until it's destroyed, then
// drains any leftover WM_QUIT so a subsequent systray message loop isn't
// poisoned — identical to ShowKeyDialog's original loop.
func runDialogMessageLoop(hwnd uintptr) {
	var msg [7]uintptr
	for {
		r, _, _ := dlgUser32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		isDialogMsg, _, _ := dlgUser32.NewProc("IsDialogMessageW").Call(hwnd, uintptr(unsafe.Pointer(&msg[0])))
		if isDialogMsg != 0 {
			continue
		}
		dlgUser32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg[0])))
		dlgUser32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg[0])))
	}
	var peek [7]uintptr
	dlgUser32.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&peek[0])), 0, 0x0012, 0x0012, 1)
}

// getWindowText reads the current text of an EDIT control.
func getWindowText(hwnd uintptr) string {
	var buf [4096]uint16
	dlgUser32.NewProc("GetWindowTextW").Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:])
}
