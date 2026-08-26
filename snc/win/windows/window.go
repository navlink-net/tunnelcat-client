// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	_ "embed"
	"fmt"
	"runtime/debug"
	"unsafe"

	"golang.org/x/sys/windows"
	"tunnel_cat/binlog"
	"tunnel_cat/logevent"
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
	Error         bool   `json:"error"`      // last connect/login attempt failed (see TrayApp.setTrayIcon)
	ErrorMsg      string `json:"errorMsg"`   // human-readable reason, e.g. "Connect failed: dial tcp ...: timeout"
	Mode          string `json:"mode"`       // "direct"
	Elapsed       string `json:"elapsed"`    // "01:23:45" or ""
	QUICLocked    bool   `json:"quicLocked"` // WildCat is forcing QUIC blocked -- see TrayApp.IsWildcatQUICLocked
}

// AppSettings mirrors the persistent settings.
type AppSettings struct {
	DoH       bool   `json:"doh"`
	BlockQUIC bool   `json:"blockQUIC"`
	Region    string `json:"region"` // "" = Auto; "RU"/"EU"/"US"/"CN"/"XX"
	WildCat   bool   `json:"wildcat"`
}

// formatByteCount renders a cumulative byte count as a short human-readable
// string (e.g. "1.2 MB"), binary (1024-based) units, one decimal place.
// Used by the main window's uplink/downlink counter (see
// AppWindow.UpdateBytes). No existing helper for this in the windows
// package as of this writing -- checked before adding.
func formatByteCount(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(1024), 0
	for v := n / 1024; v >= 1024 && exp < 3; v /= 1024 {
		div *= 1024
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// â”€â”€ setWindowIconFromICO â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// setWindowIconFromICO sets the window icon from raw ICO bytes (as produced by
// pngToICO). Skips the 22-byte ICONDIR + ICONDIRENTRY header.
// Returns the newly created HICON (caller must eventually call DestroyIcon on it).
func setWindowIconFromICO(hwnd uintptr, icoData []byte) uintptr {
	logevent.Emit(binlog.TagSystem, logevent.EventWinSetIcon,
		logevent.Str(logevent.AttrStage, "start"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("len=%d", len(icoData))))
	if len(icoData) <= 22 {
		return 0
	}
	imgData := icoData[22:]
	hIcon, _, lastErr := winCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&imgData[0])),
		uintptr(len(imgData)),
		1, 0x00030000, 32, 32, 0,
	)
	logevent.Emit(binlog.TagSystem, logevent.EventWinSetIcon,
		logevent.Str(logevent.AttrStage, "created"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("hIcon=0x%x err=%v", hIcon, lastErr)))
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
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayLclickInstall, logevent.Str(logevent.AttrStage, "searching"))
	classPtr, _ := windows.UTF16PtrFromString("SystrayClass")
	hwnd, _, _ := winFindWindowW.Call(uintptr(unsafe.Pointer(classPtr)), 0)
	if hwnd == 0 {
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayLclickInstall, logevent.Str(logevent.AttrStage, "not_found"))
		return
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayLclickInstall,
		logevent.Str(logevent.AttrStage, "found"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("hwnd=0x%x", hwnd)))

	cb := windows.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		defer func() {
			if r := recover(); r != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayLclickInstall,
					logevent.Str(logevent.AttrStage, "panic"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("msg=0x%x panic=%v\n%s", msg, r, debug.Stack())))
			}
		}()
		if msg == wmSystrayMsg {
			logevent.Emit(binlog.TagSystem, logevent.EventWinTrayLclickInstall,
				logevent.Str(logevent.AttrStage, "systray_msg"),
				logevent.Str(logevent.AttrDetail, fmt.Sprintf("lp=0x%x", lp)))
			if lp == wmLButtonUp || lp == wmLButtonDbl {
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayLclickInstall, logevent.Str(logevent.AttrStage, "click_detected"))
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
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayLclickInstall,
		logevent.Str(logevent.AttrStage, "subclassed"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("origProc=0x%x err=%v", orig, lastErr)))
	trayLClickOrigProc = orig
	trayLClickCB = cb
}

var (
	trayLClickOrigProc uintptr
	trayLClickCB       uintptr //nolint:unused
)
