// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
	"tunnel_cat/snc/core"
)

// â”€â”€ Key input dialog (pure Win32, no walk/comctl32 dependency) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

var (
	dlgUser32   = syscall.NewLazyDLL("user32.dll")
	dlgKernel32 = syscall.NewLazyDLL("kernel32.dll")

	keyDlgOnce     sync.Once
	keyDlgCallback uintptr
)

// keyDlgState is accessed only from the message-loop goroutine
// (WndProc is called synchronously from DispatchMessage).
var keyDlgState struct {
	editHwnd     uintptr
	text         string
	accepted     bool
	showLoginBtn bool // set before dialog creation; when true, wmCreate adds a "Log In Instead" button
	wantsLogin   bool // set if the user clicked that button
}

func keyDlgWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	const (
		wmCreate         = 0x0001
		wmDestroy        = 0x0002
		wmCommand        = 0x0111
		wmCtlColorStatic = 0x0138

		wsChild   = 0x40000000
		wsVisible = 0x10000000
		wsBorder  = 0x00800000
		wsTabStop = 0x00010000

		exClientEdge = 0x00000200

		esAutoHScroll   = 0x0080
		bsDefPushButton = 0x00000001

		idOK        = uintptr(1)
		idCancel    = uintptr(2)
		idScanImage = uintptr(3)
		idLogin     = uintptr(4)
		idEdit      = uintptr(100)
	)

	switch msg {
	case wmCreate:
		staticClass, _ := syscall.UTF16PtrFromString("STATIC")
		labelText, _ := syscall.UTF16PtrFromString("Enter your activation key:")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(staticClass)),
			uintptr(unsafe.Pointer(labelText)),
			wsChild|wsVisible,
			20, 18, 440, 20,
			hwnd, 0,
			dlgKernelHandle(), 0)

		editClass, _ := syscall.UTF16PtrFromString("EDIT")
		keyDlgState.editHwnd, _, _ = dlgUser32.NewProc("CreateWindowExW").Call(
			exClientEdge,
			uintptr(unsafe.Pointer(editClass)),
			0,
			wsChild|wsVisible|wsTabStop|esAutoHScroll,
			20, 46, 440, 24,
			hwnd, idEdit,
			dlgKernelHandle(), 0)

		btnClass, _ := syscall.UTF16PtrFromString("BUTTON")

		scanText, _ := syscall.UTF16PtrFromString("Scan from Image")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(btnClass)),
			uintptr(unsafe.Pointer(scanText)),
			wsChild|wsVisible|wsTabStop,
			20, 88, 170, 26,
			hwnd, idScanImage,
			dlgKernelHandle(), 0)

		okText, _ := syscall.UTF16PtrFromString("OK")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(btnClass)),
			uintptr(unsafe.Pointer(okText)),
			wsChild|wsVisible|wsTabStop|bsDefPushButton,
			215, 88, 100, 26,
			hwnd, idOK,
			dlgKernelHandle(), 0)

		cancelText, _ := syscall.UTF16PtrFromString("Cancel")
		dlgUser32.NewProc("CreateWindowExW").Call(
			0,
			uintptr(unsafe.Pointer(btnClass)),
			uintptr(unsafe.Pointer(cancelText)),
			wsChild|wsVisible|wsTabStop,
			330, 88, 100, 26,
			hwnd, idCancel,
			dlgKernelHandle(), 0)

		if keyDlgState.showLoginBtn {
			loginText, _ := syscall.UTF16PtrFromString("Log In Instead")
			dlgUser32.NewProc("CreateWindowExW").Call(
				0,
				uintptr(unsafe.Pointer(btnClass)),
				uintptr(unsafe.Pointer(loginText)),
				wsChild|wsVisible|wsTabStop,
				20, 124, 440, 26,
				hwnd, idLogin,
				dlgKernelHandle(), 0)
		}

		dlgUser32.NewProc("SetFocus").Call(keyDlgState.editHwnd)
		return 0

	case wmCommand:
		id := wParam & 0xFFFF
		switch id {
		case 3: // Scan from Image
			path, ok := openQRImageFileDialog(hwnd)
			if !ok {
				return 0
			}
			text, err := decodeQRFromImageFile(path)
			if err != nil {
				errMsg, _ := syscall.UTF16PtrFromString("Could not read QR code:\n\n" + err.Error())
				errTitle, _ := syscall.UTF16PtrFromString("QR Scan Error")
				dlgUser32.NewProc("MessageBoxW").Call(
					hwnd,
					uintptr(unsafe.Pointer(errMsg)),
					uintptr(unsafe.Pointer(errTitle)),
					0x30) // MB_ICONWARNING
				return 0
			}
			textW, _ := syscall.UTF16PtrFromString(text)
			dlgUser32.NewProc("SetWindowTextW").Call(
				keyDlgState.editHwnd,
				uintptr(unsafe.Pointer(textW)))
			return 0
		case 1: // OK
			var buf [4096]uint16
			dlgUser32.NewProc("GetWindowTextW").Call(
				keyDlgState.editHwnd,
				uintptr(unsafe.Pointer(&buf[0])),
				uintptr(len(buf)))
			text := syscall.UTF16ToString(buf[:])
			if _, err := core.ParseKeyString(text); err != nil {
				errMsg, _ := syscall.UTF16PtrFromString(
					"The activation key is not valid:\n\n" + err.Error())
				errTitle, _ := syscall.UTF16PtrFromString("Invalid Key")
				dlgUser32.NewProc("MessageBoxW").Call(
					hwnd,
					uintptr(unsafe.Pointer(errMsg)),
					uintptr(unsafe.Pointer(errTitle)),
					0x10) // MB_ICONERROR
				return 0
			}
			keyDlgState.text = text
			keyDlgState.accepted = true
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0

		case 2: // Cancel
			dlgUser32.NewProc("DestroyWindow").Call(hwnd)
			return 0

		case 4: // Log In Instead
			keyDlgState.wantsLogin = true
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

// ShowKeyDialog opens a native Win32 input dialog for the activation key.
// Returns (key, true) on OK, ("", false) on Cancel or error.
func ShowKeyDialog() (string, bool) {
	text, accepted, _ := showKeyDialogImpl(false)
	return text, accepted
}

// ShowKeyDialogWithLogin is ShowKeyDialog plus a "Log In Instead" button,
// shown when navlink.net has been probed reachable so the user can switch
// to credential login without an activation key. wantsLogin is true if that
// button was clicked (text/accepted are then meaningless).
func ShowKeyDialogWithLogin() (text string, accepted bool, wantsLogin bool) {
	return showKeyDialogImpl(true)
}

func showKeyDialogImpl(showLoginBtn bool) (string, bool, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	keyDlgOnce.Do(func() {
		keyDlgCallback = syscall.NewCallback(keyDlgWndProc)
	})
	keyDlgState = struct {
		editHwnd     uintptr
		text         string
		accepted     bool
		showLoginBtn bool
		wantsLogin   bool
	}{showLoginBtn: showLoginBtn}

	const (
		wsPopup    = 0x80000000
		wsCaption  = 0x00C00000
		wsSysMenu  = 0x00080000
		wsDlgFrame = 0x00400000
		wsTabStop  = 0x00010000

		exDlgModalFrame = 0x00000001
		exTopMost       = 0x00000008

		// Client-area dimensions.  Window dimensions are computed below via
		// AdjustWindowRectEx so we get the right size on any DPI/theme.
		clientW = int32(480)
	)
	// One extra row when the "Log In Instead" button is present.
	clientH := int32(130)
	if showLoginBtn {
		clientH = 166
	}

	className, _ := syscall.UTF16PtrFromString("SNCKeyDlg")
	titleText, _ := syscall.UTF16PtrFromString("ShortNerdCat â€” Activation Key")

	wc := keyDlgWNDCLASSEX{
		cbSize:        uint32(unsafe.Sizeof(keyDlgWNDCLASSEX{})),
		lpfnWndProc:   keyDlgCallback,
		hInstance:     dlgKernelHandle(),
		hbrBackground: 15, // COLOR_3DFACE (= 14) + 1 = 15
		lpszClassName: className,
	}
	// Ignore error â€” class may already be registered on 2nd call.
	dlgUser32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc)))

	// AdjustWindowRectEx: convert desired client rect â†’ total window rect.
	// rect = {left, top, right, bottom}; after the call right-left = total width,
	// bottom-top = total height including caption and borders.
	rect := [4]int32{0, 0, clientW, clientH}
	dlgUser32.NewProc("AdjustWindowRectEx").Call(
		uintptr(unsafe.Pointer(&rect[0])),
		wsPopup|wsCaption|wsSysMenu, // dwStyle
		0,                           // bMenu = FALSE
		exDlgModalFrame)             // dwExStyle
	dlgW := rect[2] - rect[0]
	dlgH := rect[3] - rect[1]

	// Centre on screen.
	sw, _, _ := dlgUser32.NewProc("GetSystemMetrics").Call(0)  // SM_CXSCREEN
	sh, _, _ := dlgUser32.NewProc("GetSystemMetrics").Call(1)  // SM_CYSCREEN
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
		core.Log.Printf("key dialog: CreateWindowExW failed")
		return "", false, false
	}

	dlgUser32.NewProc("ShowWindow").Call(hwnd, 5)   // SW_SHOW
	dlgUser32.NewProc("UpdateWindow").Call(hwnd)

	// Message loop â€” IsDialogMessageW handles Tab/Enter/Escape.
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

	// Drain any WM_QUIT left by DestroyWindow/PostQuitMessage so systray's
	// message loop isn't poisoned.
	var peek [7]uintptr
	dlgUser32.NewProc("PeekMessageW").Call(
		uintptr(unsafe.Pointer(&peek[0])), 0,
		0x0012, 0x0012, // WM_QUIT = 0x0012
		1)              // PM_REMOVE

	return keyDlgState.text, keyDlgState.accepted, keyDlgState.wantsLogin
}

func dlgKernelHandle() uintptr {
	h, _, _ := dlgKernel32.NewProc("GetModuleHandleW").Call(0)
	return h
}

// keyDlgWNDCLASSEX mirrors WNDCLASSEXW for RegisterClassExW.
type keyDlgWNDCLASSEX struct {
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

// ShowError displays a modal error message box using the Windows API directly.
// Safe to call before any message loop is running.
func ShowError(msg string) {
	title, _ := syscall.UTF16PtrFromString("ShortNerdCat")
	text, _ := syscall.UTF16PtrFromString(msg)
	dlgUser32.NewProc("MessageBoxW").Call(
		0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(title)),
		0x10) // MB_ICONERROR
}

// ShowUpdateAvailableDialog shows a modal Yes/No message box offering to apply
// an update that has already been downloaded and verified by core.Updater
// (see NotifyUpdateReady's caller). Returns true if the user picked "Yes".
func ShowUpdateAvailableDialog(newVersion string) bool {
	title, _ := syscall.UTF16PtrFromString("ShortNerdCat")
	msg := fmt.Sprintf(
		"A new version (%s) has been downloaded and is ready to install.\n\nRestart now to update?",
		newVersion)
	text, _ := syscall.UTF16PtrFromString(msg)
	ret, _, _ := dlgUser32.NewProc("MessageBoxW").Call(
		0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(title)),
		0x24) // MB_ICONQUESTION | MB_YESNO
	const idYes = 6
	return ret == idYes
}

// â”€â”€ DPAPI key storage â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	procProtect   = crypt32.NewProc("CryptProtectData")
	procUnprotect = crypt32.NewProc("CryptUnprotectData")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(data []byte) *dataBlob {
	if len(data) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
}

func (b *dataBlob) bytes() []byte {
	if b.cbData == 0 || b.pbData == nil {
		return nil
	}
	return unsafe.Slice(b.pbData, b.cbData)
}

func dpapiProtect(plain []byte) ([]byte, error) {
	in := newBlob(plain)
	var out dataBlob
	ret, _, err := procProtect.Call(
		uintptr(unsafe.Pointer(in)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	result := make([]byte, out.cbData)
	copy(result, out.bytes())
	return result, nil
}

func dpapiUnprotect(enc []byte) ([]byte, error) {
	in := newBlob(enc)
	var out dataBlob
	ret, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(in)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	result := make([]byte, out.cbData)
	copy(result, out.bytes())
	return result, nil
}

func keyFilePath() (string, error) {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return "", fmt.Errorf("APPDATA environment variable not set")
	}
	dir := filepath.Join(appdata, "ShortNerdCat")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "key.dat"), nil
}

// SaveKey encrypts keyStr with DPAPI and writes it to
// %APPDATA%\ShortNerdCat\key.dat.
func SaveKey(keyStr string) error {
	enc, err := dpapiProtect([]byte(keyStr))
	if err != nil {
		return err
	}
	path, err := keyFilePath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, enc, 0600)
}

// LoadKey reads and decrypts the stored activation key.
func LoadKey() (string, error) {
	path, err := keyFilePath()
	if err != nil {
		return "", err
	}
	enc, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	plain, err := dpapiUnprotect(enc)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RegisterURLScheme registers the navlink:// custom URL scheme under
// HKEY_CURRENT_USER so Windows routes activation links to this executable.
// Called on every startup; safe if the key already exists.
func RegisterURLScheme() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	base, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Classes\navlink`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer base.Close()
	_ = base.SetStringValue("", "URL:ShortNerdCat Protocol")
	_ = base.SetStringValue("URL Protocol", "")

	cmd, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Classes\navlink\shell\open\command`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer cmd.Close()
	_ = cmd.SetStringValue("", `"`+exe+`" "%1"`)
}
