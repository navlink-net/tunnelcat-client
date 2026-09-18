// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	_ "embed"

	"bytes"
	"fmt"
	"image/png"
	"runtime"
	"runtime/debug"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"tunnel_cat/binlog"
	"tunnel_cat/logevent"
)

// Main-screen illustration -- deliberately a SEPARATE file from
// assets/snc_idle.png etc., which tray.go embeds for the systray/window
// icon. They used to be the same file; a redesign of one silently broke the
// other (systray + title bar icon) since pngToICO shrinks whatever image is
// there. Keep them independent going forward.
//
//go:embed assets/illustration_idle.png
var catIdlePNG []byte

//go:embed assets/illustration_connecting.png
var catConnectingPNG []byte

//go:embed assets/illustration_connected.png
var catConnectedPNG []byte

// Club-theme variants of the same three illustrations -- same layout/content
// as the default set above, only the color palette and artwork differ (see
// AppWindow.ClubTheme). No "error" variants: this client has no error
// illustration slot at all yet (see catEntry usage in initGDI), so there's
// nothing to theme there either.
//
//go:embed assets/illustration_idle_catclub.png
var catIdlePNGCatClub []byte

//go:embed assets/illustration_connecting_catclub.png
var catConnectingPNGCatClub []byte

//go:embed assets/illustration_connected_catclub.png
var catConnectedPNGCatClub []byte

//go:embed assets/illustration_idle_elite.png
var catIdlePNGElite []byte

//go:embed assets/illustration_connecting_elite.png
var catConnectingPNGElite []byte

//go:embed assets/illustration_connected_elite.png
var catConnectedPNGElite []byte

// â”€â”€ Layout â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

const (
	uiW     = 480
	uiH     = 500
	uiHdrH  = 46              // custom title bar
	uiTabH  = 38              // tab strip
	uiContY = uiHdrH + uiTabH // content top = 84
)

// â”€â”€ Colors (COLORREF = R | G<<8 | B<<16) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
//
// Aliased onto the SNC palette (theme_windows.go) rather than renamed at
// every call site -- keeps this diff to the token values, not a rename
// across the whole file.

const (
	uiClrBg         uint32 = ThemeBgPrimary    // #11141A
	uiClrHdr        uint32 = ThemeKittenBlack  // #07090D
	uiClrTab        uint32 = ThemeSurface      // #1B2028 tab bar bg
	uiClrTabAct     uint32 = ThemeFurHighlight // active tab bg
	uiClrPanel      uint32 = ThemeSurface      // #1B2028 panel
	uiClrCyan       uint32 = ThemeScreenCyan   // #59C8FF
	uiClrText       uint32 = ThemeTextPrimary
	uiClrMuted      uint32 = ThemeTextMuted
	uiClrGreen      uint32 = ThemeSoftLime                // connected
	uiClrYellow     uint32 = ThemeWarmAmber               // connecting
	uiClrRed        uint32 = 200 | (50 << 8) | (50 << 16) // error -- not a brand token, kept recognizably red
	uiClrBtnBdr     uint32 = ThemeScreenCyan              // connect btn border
	uiClrDiscBdr    uint32 = 160 | (60 << 8) | (60 << 16) // disconnect btn border -- error-adjacent, not a brand token
	uiClrBtnBg      uint32 = ThemeSurface                 // button bg
	uiClrSettingsBg uint32 = ThemeFurHighlight            // Settings bg

	// Club membership header badge -- not brand tokens, palette specified
	// directly for this feature (see tunnel_cat/docs/club-membership.md):
	// Cat Club is light blue/white/silver, Elite Cat Club matches
	// beautysqrl.com's dark-brown/gold/orange.
	uiClrCatClubBadgeBg uint32 = 0x59 | (0xC8 << 8) | (0xFF << 16) // #59C8FF light blue
	uiClrCatClubBadgeFg uint32 = 0xFF | (0xFF << 8) | (0xFF << 16) // white
	uiClrEliteBadgeBg   uint32 = 0x3B | (0x24 << 8) | (0x12 << 16) // #3B2412 dark brown
	uiClrEliteBadgeFg   uint32 = 0xFF | (0xD7 << 8) | (0x00 << 16) // #FFD700 gold
)

// â”€â”€ Win32 constants â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

const (
	uiWS_POPUP        = 0x80000000
	uiWS_SYSMENU      = 0x00080000
	uiWS_VISIBLE      = 0x10000000
	uiWS_CHILD        = 0x40000000
	uiWS_TABSTOP      = 0x00010000
	uiWS_GROUP        = 0x00020000
	uiWS_EX_APPWINDOW = 0x00040000
	uiWS_EX_TOPMOST   = 0x00000008

	uiBSAUTOCHECKBOX  = 0x00000003
	uiBSOWNERDRAW     = 0x0000000B
	uiCBSDROPDOWNLIST = 0x00000003
	uiCBSHASSTRINGS   = 0x00000200
	uiSSRIGHT         = 0x00000002 // SS_RIGHT -- right-aligned STATIC text

	uiODS_SELECTED = 0x0001
	uiODS_FOCUS    = 0x0010

	uiBN_CLICKED    = 0
	uiCBN_SELCHANGE = 1

	uiNULL_BRUSH = 5
	uiNULL_PEN   = 8

	uiWS_EX_TRANSPARENT = 0x00000020

	// Cat image display size (px) â€” centered in tunnel tab
	catSize = 160
	catX    = (uiW - catSize) / 2 // 160
	catY    = uiContY + 24        // 108

	uiDT_CENTER     = 0x00000001
	uiDT_VCENTER    = 0x00000004
	uiDT_SINGLELINE = 0x00000020
	uiDT_LEFT       = 0x00000000
	uiDT_WORDBREAK  = 0x00000010

	uiTRANSPARENT = 1
	uiOPAQUE      = 2

	uiPS_SOLID          = 0
	uiFW_BOLD           = 700
	uiFW_NORMAL         = 400
	uiCLEARTYPE_QUALITY = 5
	uiDEFAULT_CHARSET   = 1

	uiCB_ADDSTRING    = 0x0143
	uiCB_SETCURSEL    = 0x014E
	uiCB_GETCURSEL    = 0x0147
	uiCB_RESETCONTENT = 0x014B

	uiBM_SETCHECK   = 0x00F1
	uiBM_GETCHECK   = 0x00F0
	uiBST_CHECKED   = 1
	uiBST_UNCHECKED = 0

	uiWM_CLOSE           = 0x0010
	uiWM_DESTROY         = 0x0002
	uiWM_PAINT           = 0x000F
	uiWM_ERASEBKG        = 0x0014
	uiWM_COMMAND         = 0x0111
	uiWM_DRAWITEM        = 0x002B
	uiWM_CTLCOLORBTN     = 0x0135
	uiWM_CTLCOLORSTATIC  = 0x0138
	uiWM_CTLCOLORLISTBOX = 0x0132
	uiWM_CTLCOLOREDIT    = 0x0133
	uiWM_LBUTTONDOWN     = 0x0201
	uiWM_NCHITTEST       = 0x0084
	uiWM_SETCURSOR       = 0x0020
	uiWM_APP             = 0x8000
	uiWM_UpdateStatus    = uiWM_APP + 1
	uiWM_ReloadClubTheme = uiWM_APP + 2
	uiWM_SetAdminAccount = uiWM_APP + 3
	uiWM_SetCanRecommend = uiWM_APP + 4
	uiWM_UpdateBytes     = uiWM_APP + 5
	uiWM_QUIT            = 0x0012

	uiHTCAPTION  = 2
	uiHTCLIENT   = 1
	uiSW_SHOW    = 5
	uiSW_HIDE    = 0
	uiSW_RESTORE = 9

	// DWMWA_USE_IMMERSIVE_DARK_MODE
	uiDWMWA_DARK = 20

	// Control IDs
	uiIDConnect     = 101
	uiIDDisconnect  = 102
	uiIDDoH         = 104
	uiIDRegion      = 107
	uiIDBlockQUIC   = 108
	uiIDByteCounter = 110
	uiIDLogUpload   = 111

	// Native menu-bar item IDs (separate range from control IDs above so
	// WM_COMMAND dispatch never collides between a button/checkbox and a
	// menu item).
	uiIDMenuLogin        = 300
	uiIDMenuLogout       = 301
	uiIDMenuConnect      = 302
	uiIDMenuDisconnect   = 303
	uiIDMenuDoH          = 304
	uiIDMenuBlockQUIC    = 305
	uiIDMenuRegionAuto   = 307
	uiIDMenuRegionRussia = 308
	uiIDMenuRegionEurope = 309
	uiIDMenuRegionUSA    = 310
	uiIDMenuRegionChina  = 311
	uiIDMenuRegionOther  = 312
	uiIDMenuAbout        = 313
	uiIDMenuUpdate       = 314
	uiIDMenuQuit         = 315

	// Admin-only theme preview submenu -- see SetAdminAccount. Grayed out
	// (not removed) until the logged-in key's IsAdmin flag is known, same
	// enable/disable mechanism already used for uiIDMenuUpdate.
	uiIDMenuPreviewRegular = 316
	uiIDMenuPreviewCatClub = 317
	uiIDMenuPreviewElite   = 318

	// Recommend-a-member -- see SetCanRecommend. Grayed out until Cat Club
	// (or subsuming) membership is confirmed, same mechanism as the preview
	// submenu above.
	uiIDMenuRecommend = 319

	// Menu API flags/constants.
	uiMF_STRING    = 0x00000000
	uiMF_POPUP     = 0x00000010
	uiMF_SEPARATOR = 0x00000800
	uiMF_CHECKED   = 0x00000008
	uiMF_UNCHECKED = 0x00000000
	uiMF_GRAYED    = 0x00000001
	uiMF_ENABLED   = 0x00000000
	uiMF_BYCOMMAND = 0x00000000
	uiSM_CYMENU    = 15
)

// â”€â”€ Win32 structs â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type uiRECT struct{ Left, Top, Right, Bottom int32 }
type uiPOINT struct{ X, Y int32 }

type uiPAINTSTRUCT struct {
	HDC     uintptr
	FErase  int32
	RcPaint uiRECT
	_       [36]byte
}

type uiDRAWITEMSTRUCT struct {
	CtlType, CtlID, ItemID, ItemAction, ItemState uint32
	HwndItem                                      uintptr
	HDC                                           uintptr
	RcItem                                        uiRECT
	ItemData                                      uintptr
}

type uiWNDCLASSEX struct {
	CbSize                                   uint32
	Style                                    uint32
	LpfnWndProc                              uintptr
	CbClsExtra, CbWndExtra                   int32
	HInstance, HIcon, HCursor, HbrBackground uintptr
	LpszMenuName, LpszClassName              *uint16
	HIconSm                                  uintptr
}

type uiBMIHEADER struct {
	BiSize                           uint32
	BiWidth                          int32
	BiHeight                         int32
	BiPlanes, BiBitCount             uint16
	BiCompression, BiSizeImage       uint32
	BiXPelsPerMeter, BiYPelsPerMeter int32
	BiClrUsed, BiClrImportant        uint32
}
type uiBMI struct {
	H   uiBMIHEADER
	RGB [1]uint32
}

// â”€â”€ DLL procs â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

var (
	uiUser32  = windows.NewLazySystemDLL("user32.dll")
	uiGdi32   = windows.NewLazySystemDLL("gdi32.dll")
	uiMsimg32 = windows.NewLazySystemDLL("msimg32.dll")
	uiDwmapi  = windows.NewLazySystemDLL("dwmapi.dll")

	uiRegisterClassExW   = uiUser32.NewProc("RegisterClassExW")
	uiCreateWindowExW    = uiUser32.NewProc("CreateWindowExW")
	uiDestroyWindowFn    = uiUser32.NewProc("DestroyWindow")
	uiGetClientRectFn    = uiUser32.NewProc("GetClientRect")
	uiBeginPaintFn       = uiUser32.NewProc("BeginPaint")
	uiEndPaintFn         = uiUser32.NewProc("EndPaint")
	uiInvalidateRectFn   = uiUser32.NewProc("InvalidateRect")
	uiPostMessageFn      = uiUser32.NewProc("PostMessageW")
	uiFillRectFn         = uiUser32.NewProc("FillRect")
	uiDrawTextFn         = uiUser32.NewProc("DrawTextW")
	uiGetMessageFn       = uiUser32.NewProc("GetMessageW")
	uiTranslateMessageFn = uiUser32.NewProc("TranslateMessage")
	uiDispatchMessageFn  = uiUser32.NewProc("DispatchMessageW")
	uiDefWindowProcFn    = uiUser32.NewProc("DefWindowProcW")
	uiShowWindowFn       = uiUser32.NewProc("ShowWindow")
	uiSetForegroundFn    = uiUser32.NewProc("SetForegroundWindow")
	uiIsWindowVisibleFn  = uiUser32.NewProc("IsWindowVisible")
	uiGetDCFn            = uiUser32.NewProc("GetDC")
	uiReleaseDCFn        = uiUser32.NewProc("ReleaseDC")
	uiGetSystemMetricsFn = uiUser32.NewProc("GetSystemMetrics")
	uiSendMessageFn      = uiUser32.NewProc("SendMessageW")
	uiPostQuitFn         = uiUser32.NewProc("PostQuitMessage")
	uiCreateMenuFn       = uiUser32.NewProc("CreateMenu")
	uiCreatePopupMenuFn  = uiUser32.NewProc("CreatePopupMenu")
	uiAppendMenuFn       = uiUser32.NewProc("AppendMenuW")
	uiSetMenuFn          = uiUser32.NewProc("SetMenu")
	uiCheckMenuItemFn    = uiUser32.NewProc("CheckMenuItem")
	uiEnableMenuItemFn   = uiUser32.NewProc("EnableMenuItem")
	uiEnableWindowFn     = uiUser32.NewProc("EnableWindow")
	uiGetModuleHandleFn  = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")

	uiCreateFontFn             = uiGdi32.NewProc("CreateFontW")
	uiCreateSolidBrushFn       = uiGdi32.NewProc("CreateSolidBrush")
	uiCreatePenFn              = uiGdi32.NewProc("CreatePen")
	uiSelectObjectFn           = uiGdi32.NewProc("SelectObject")
	uiDeleteObjectFn           = uiGdi32.NewProc("DeleteObject")
	uiDeleteDCFn               = uiGdi32.NewProc("DeleteDC")
	uiCreateCompDCFn           = uiGdi32.NewProc("CreateCompatibleDC")
	uiCreateDIBSectionFn       = uiGdi32.NewProc("CreateDIBSection")
	uiSetBkModeFn              = uiGdi32.NewProc("SetBkMode")
	uiSetTextColorFn           = uiGdi32.NewProc("SetTextColor")
	uiBitBltFn                 = uiGdi32.NewProc("BitBlt")
	uiCreateCompatibleBitmapFn = uiGdi32.NewProc("CreateCompatibleBitmap")
	uiEllipseFn                = uiGdi32.NewProc("Ellipse")
	uiRoundRectFn              = uiGdi32.NewProc("RoundRect")
	uiCreateRoundRectRgnFn     = uiGdi32.NewProc("CreateRoundRectRgn")
	uiSelectClipRgnFn          = uiGdi32.NewProc("SelectClipRgn")
	uiRectangleFn              = uiGdi32.NewProc("Rectangle")
	uiGetStockObjectFn         = uiGdi32.NewProc("GetStockObject")
	uiTextOutFn                = uiGdi32.NewProc("TextOutW")
	uiMoveToExFn               = uiGdi32.NewProc("MoveToEx")
	uiLineToFn                 = uiGdi32.NewProc("LineTo")

	uiAlphaBlendFn = uiMsimg32.NewProc("AlphaBlend")
	uiDwmSetAttrFn = uiDwmapi.NewProc("DwmSetWindowAttribute")

	uiSetBkColorFn     = uiGdi32.NewProc("SetBkColor")
	uiSetWindowThemeFn = windows.NewLazySystemDLL("uxtheme.dll").NewProc("SetWindowTheme")
)

// â”€â”€ AppWindow â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// AppWindow is the main ShortNerdCat UI panel â€” a fixed-size native Win32 window
// with custom dark painting.  It holds the Tunnel and Settings tabs.
// The Browse button opens a separate BrowserWindow.
type AppWindow struct {
	startOnce sync.Once
	readyCh   chan struct{}
	hwnd      uintptr
	activeTab int // 0 = Tunnel, 1 = Settings

	// ClubTheme selects which illustration set initGDI loads: "" (default),
	// "catclub", or "elite". Must be set before Start()/runLoop() build the
	// GDI bitmaps -- there is no live re-theming after the window is up,
	// same as every other GDI resource here (see initGDI/freeGDI).
	ClubTheme string

	// GDI resources (created in runLoop, freed on destroy)
	bgMemDC     uintptr
	bgBitmap    uintptr
	hfTitle     uintptr // large bold title font
	hfBody      uintptr // body text
	hfBadge     uintptr // small bold badge
	hbrBg       uintptr
	hbrHdr      uintptr
	hbrTab      uintptr
	hbrTabAct   uintptr
	hbrPanel    uintptr
	hbrSettings uintptr // solid blue bg for Settings tab

	// Cat image DIBs â€” one per tunnel state (created in initGDI)
	catIdleDC, catIdleBM       uintptr
	catConnectingDC, catConnBM uintptr
	catConnectedDC, catConBM   uintptr

	// Full-window-sized (uiWÃ—uiH) versions of the same illustrations, stretched
	// to fill the whole content area as the background instead of bg.png --
	// experiment requested 2026-08-08: "stretch the kitten pictures to the
	// full window size instead of the background, let's see if it's better."
	catIdleBgDC, catIdleBgBM             uintptr
	catConnectingBgDC, catConnectingBgBM uintptr
	catConnectedBgDC, catConnectedBgBM   uintptr

	// Window HICON â€” tracked so the previous handle is destroyed before replacement
	// (CreateIconFromResourceEx leaks one handle per call otherwise).
	hIcon uintptr

	// Child control HWNDs (created in createControls)
	hConnect    uintptr
	hDisconnect uintptr
	hRegion     uintptr // region combobox
	hDoH        uintptr // native checkboxes on Settings tab
	hBlockQUIC  uintptr
	hLogUpload  uintptr // account-level, not part of AppSettings -- see LogUploadToggleFn
	hBytesLabel uintptr // uplink/downlink counter, above the tunnel status bar; see UpdateBytes

	// State â€” updated from any goroutine, read in WndProc (always on runLoop thread)
	mu                   sync.Mutex
	status               AppStatus
	settings             AppSettings
	bytesSent, bytesRecv int64  // stashed by UpdateBytes, applied on the runLoop thread by uiWM_UpdateBytes
	pendingClubTheme     string // stashed by ReloadClubTheme, applied on the runLoop thread by uiWM_ReloadClubTheme
	pendingBadgeText     string // stashed alongside pendingClubTheme; "" = no badge (regular tier)
	clubBadgeText        string // currently-displayed badge text, set by applyClubTheme on the runLoop thread
	pendingIsAdmin       bool   // stashed by SetAdminAccount, applied on the runLoop thread by uiWM_SetAdminAccount
	pendingCanRecommend  bool   // stashed by SetCanRecommend, applied on the runLoop thread by uiWM_SetCanRecommend

	// Callbacks â€” set before Start()
	ConnectFn     func()
	DisconnectFn  func()
	StatusFn      func() AppStatus
	GetSettingsFn func() AppSettings
	SetSettingsFn func(AppSettings)
	// LogUploadToggleFn is called with the new checked state when the user
	// flips the Settings-tab log-upload checkbox. Deliberately separate from
	// GetSettingsFn/SetSettingsFn: this preference lives on the account
	// server-side (core.LogUploader.SetPref), not in the local settings
	// file, and setting it requires a live tunnel dialer -- see
	// docs/LOG_UPLOAD_PRIVACY.md. May be nil before login/first connect.
	LogUploadToggleFn func(enabled bool)
	LoginFn           func()
	LogoutFn      func()
	AboutFn       func()
	UpdateFn      func()
	QuitFn        func()
	UpdateReadyFn func() bool
	RecommendFn   func(username string) // called with the entered username after ShowRecommendDialog submits; may be nil

	// Native menu-bar handles (see createMenuBar).
	hMenuMain    uintptr
	hMenuRegion  uintptr
	hMenuPreview uintptr
	cyMenu       int32 // GetSystemMetrics(SM_CYMENU); see WM_NCHITTEST below for why this is needed
}

// â”€â”€ Global WndProc callback (single AppWindow instance) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

var (
	globalAW      *AppWindow
	uiWndProcCB   uintptr
	uiWndProcOnce sync.Once
)

func uiWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	defer func() {
		if r := recover(); r != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
				logevent.Str(logevent.AttrStage, "panic"),
				logevent.Str(logevent.AttrDetail, fmt.Sprintf("msg=0x%x: %v\n%s", msg, r, debug.Stack())))
		}
	}()
	aw := globalAW
	if aw == nil || aw.hwnd != hwnd {
		r, _, _ := uiDefWindowProcFn.Call(hwnd, msg, wp, lp)
		return r
	}
	return aw.handleMsg(msg, wp, lp)
}

func (aw *AppWindow) handleMsg(msg, wp, lp uintptr) uintptr {
	switch msg {

	case uiWM_ERASEBKG:
		// Suppress default erasure â€” WM_PAINT handles everything via double-buffer.
		return 1

	case uiWM_PAINT:
		var ps uiPAINTSTRUCT
		hdc, _, _ := uiBeginPaintFn.Call(aw.hwnd, uintptr(unsafe.Pointer(&ps)))
		// Double-buffer: compose background + content into an off-screen DC,
		// then blit to the screen in a single operation to eliminate flicker.
		memDC, _, _ := uiCreateCompDCFn.Call(hdc)
		memBM, _, _ := uiCreateCompatibleBitmapFn.Call(hdc, uiW, uiH)
		oldBM, _, _ := uiSelectObjectFn.Call(memDC, memBM)
		aw.paintBackground(memDC)
		aw.paintContent(memDC)
		uiBitBltFn.Call(hdc, 0, 0, uiW, uiH, memDC, 0, 0, 0x00CC0020 /*SRCCOPY*/)
		uiSelectObjectFn.Call(memDC, oldBM)
		uiDeleteObjectFn.Call(memBM)
		uiDeleteDCFn.Call(memDC)
		uiEndPaintFn.Call(aw.hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0

	case uiWM_DRAWITEM:
		aw.handleDrawItem(lp)
		return 1

	case uiWM_CTLCOLORBTN, uiWM_CTLCOLORSTATIC:
		if lp == aw.hBytesLabel {
			// Byte counter: no background box at all -- just white text
			// directly over the illustration/status-bar area, per explicit
			// "make the little plate transparent" request. NULL_BRUSH (stock
			// object 5) tells Windows not to paint anything behind the text.
			uiSetBkModeFn.Call(wp, uiTRANSPARENT)
			uiSetTextColorFn.Call(wp, 0x00FFFFFF)
			nullBrush, _, _ := uiGetStockObjectFn.Call(5) // NULL_BRUSH
			return nullBrush
		}
		if aw.activeTab == 1 {
			uiSetBkModeFn.Call(wp, uiTRANSPARENT)
			uiSetTextColorFn.Call(wp, 0x00FFFFFF)
			return aw.hbrSettings
		}
		uiSetBkModeFn.Call(wp, uiTRANSPARENT)
		uiSetTextColorFn.Call(wp, uintptr(uiClrText))
		uiSetBkColorFn.Call(wp, uintptr(uiClrBg))
		return aw.hbrBg

	case uiWM_CTLCOLORLISTBOX, uiWM_CTLCOLOREDIT:
		if aw.activeTab == 1 {
			uiSetBkModeFn.Call(wp, uiOPAQUE)
			uiSetTextColorFn.Call(wp, 0x00000000)
			uiSetBkColorFn.Call(wp, 0x00FFFFFF)
			wb, _, _ := uiGetStockObjectFn.Call(0) // WHITE_BRUSH
			return wb
		}
		uiSetBkModeFn.Call(wp, uiOPAQUE)
		uiSetTextColorFn.Call(wp, uintptr(uiClrText))
		uiSetBkColorFn.Call(wp, uintptr(uiClrPanel))
		return aw.hbrPanel

	case uiWM_COMMAND:
		aw.handleCommand(uint16(wp), uint16(wp>>16))
		return 0

	case uiWM_UpdateStatus:
		aw.mu.Lock()
		s := aw.status
		aw.mu.Unlock()
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
			logevent.Str(logevent.AttrStage, "status"),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("connected=%v connecting=%v disconnecting=%v mode=%q tab=%d", s.Connected, s.Connecting, s.Disconnecting, s.Mode, aw.activeTab)))
		aw.syncConnectButtons()
		aw.syncWindowIcon()
		aw.syncBlockQUICLock(s.QUICLocked)
		aw.syncByteCounterVisibility(s.Connected)
		uiInvalidateRectFn.Call(aw.hwnd, 0, 0) // bErase=FALSE â€” WM_PAINT double-buffers everything
		return 0

	case uiWM_UpdateBytes:
		// Cheap path: only re-sets one STATIC control's text via WM_SETTEXT,
		// no InvalidateRect/full repaint -- this fires about once/second
		// while connected (see TrayApp's tick in tray.go), same reasoning as
		// why tickElapsed only updates the tray tooltip instead of calling
		// UpdateStatus every second.
		aw.mu.Lock()
		sent, recv := aw.bytesSent, aw.bytesRecv
		aw.mu.Unlock()
		if aw.hBytesLabel != 0 {
			text := fmt.Sprintf("↑ %s   ↓ %s", formatByteCount(sent), formatByteCount(recv))
			p, _ := windows.UTF16PtrFromString(text)
			uiSendMessageFn.Call(aw.hBytesLabel, 0x000C /*WM_SETTEXT*/, 0, uintptr(unsafe.Pointer(p)))
		}
		return 0

	case uiWM_ReloadClubTheme:
		aw.mu.Lock()
		theme, badgeText := aw.pendingClubTheme, aw.pendingBadgeText
		aw.mu.Unlock()
		aw.applyClubTheme(theme, badgeText)
		return 0

	case uiWM_SetAdminAccount:
		aw.mu.Lock()
		isAdmin := aw.pendingIsAdmin
		aw.mu.Unlock()
		if aw.hMenuMain != 0 && aw.hMenuPreview != 0 {
			flag := uintptr(uiMF_BYCOMMAND | uiMF_GRAYED)
			if isAdmin {
				flag = uiMF_BYCOMMAND | uiMF_ENABLED
			}
			uiEnableMenuItemFn.Call(aw.hMenuMain, aw.hMenuPreview, flag)
		}
		return 0

	case uiWM_SetCanRecommend:
		aw.mu.Lock()
		canRecommend := aw.pendingCanRecommend
		aw.mu.Unlock()
		if aw.hMenuMain != 0 {
			flag := uintptr(uiMF_BYCOMMAND | uiMF_GRAYED)
			if canRecommend {
				flag = uiMF_BYCOMMAND | uiMF_ENABLED
			}
			uiEnableMenuItemFn.Call(aw.hMenuMain, uintptr(uiIDMenuRecommend), flag)
		}
		return 0

	case uiWM_LBUTTONDOWN:
		x := int32(lp & 0xFFFF)
		y := int32((lp >> 16) & 0xFFFF)
		aw.handleClick(x, y)
		return 0

	case uiWM_NCHITTEST:
		// lp contains cursor position in screen coordinates.
		var wr uiRECT
		winGetWindowRect.Call(aw.hwnd, uintptr(unsafe.Pointer(&wr)))
		winX := int32(lp&0xFFFF) - wr.Left
		winY := int32((lp>>16)&0xFFFF) - wr.Top

		// GetWindowRect (and therefore winY here) starts at the very top of the
		// window, which is the real native menu bar's own strip (cyMenu tall,
		// added on top of the client area in runLoop) -- NOT the top of our
		// owner-drawn pseudo-titlebar below it. Before the menu bar existed,
		// winY==0 WAS the top of that pseudo-titlebar, so this whole handler
		// unconditionally treated every click in the first uiHdrH pixels as
		// HTCAPTION/HTCLIENT. Once the menu bar was added, clicks landing on
		// its strip (0 <= winY < cyMenu) still fell into that same range and
		// got returned as HTCAPTION -- overriding Windows' own correct HTMENU
		// classification, so the menu bar drew but never opened on click, see
		// the same-day incident this fixes. Let DefWindowProc classify that
		// strip (real HTMENU/HTSYSMENU handling) and only apply the custom
		// logic below it, shifted down by cyMenu.
		if winY < aw.cyMenu {
			break
		}
		winY -= aw.cyMenu

		if winY >= 0 && winY < uiHdrH {
			// Close button (rightmost 40 px of header) must return HTCLIENT
			// so WM_LBUTTONDOWN reaches handleClick; the rest is HTCAPTION
			// to allow window dragging.
			if winX >= uiW-40 {
				return uiHTCLIENT
			}
			return uiHTCAPTION
		}
		return uiHTCLIENT

	case uiWM_CLOSE:
		// Hide rather than destroy so state is preserved.
		uiShowWindowFn.Call(aw.hwnd, uiSW_HIDE)
		return 0

	case uiWM_DESTROY:
		aw.freeGDI()
		uiPostQuitFn.Call(0)
		return 0
	}
	r, _, _ := uiDefWindowProcFn.Call(aw.hwnd, msg, wp, lp)
	return r
}

// â”€â”€ Painting â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func (aw *AppWindow) paintBackground(hdc uintptr) {
	var rc uiRECT
	uiGetClientRectFn.Call(aw.hwnd, uintptr(unsafe.Pointer(&rc)))
	// Fill with solid dark base.
	uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&rc)), aw.hbrBg)

	// Experiment (2026-08-08): stretch the current state's kitten
	// illustration to fill the whole window instead of the old bg.png
	// backdrop -- see catIdleBgDC's field comment. Falls back to bg.png if
	// for some reason the state illustration failed to load.
	aw.mu.Lock()
	s := aw.status
	aw.mu.Unlock()
	bgDC := aw.catIdleBgDC
	if s.Connected {
		bgDC = aw.catConnectedBgDC
	} else if s.Connecting || s.Disconnecting {
		bgDC = aw.catConnectingBgDC
	}
	if bgDC != 0 {
		blend := [4]byte{0, 0, 255, 1} // AC_SRC_OVER, alpha=255, AC_SRC_ALPHA
		uiAlphaBlendFn.Call(
			hdc, 0, 0, uiW, uiH,
			bgDC, 0, 0, uiW, uiH,
			uintptr(*(*uint32)(unsafe.Pointer(&blend[0]))),
		)
	} else if aw.bgMemDC != 0 {
		blend := [4]byte{0, 0, 165, 0} // AC_SRC_OVER, flags=0, alpha=165, AC_SRC_ALPHA=0
		uiAlphaBlendFn.Call(
			hdc, 0, 0, uiW, uiH,
			aw.bgMemDC, 0, 0, uiW, uiH,
			uintptr(*(*uint32)(unsafe.Pointer(&blend[0]))),
		)
	}
}

func (aw *AppWindow) paintContent(hdc uintptr) {
	uiSetBkModeFn.Call(hdc, uiTRANSPARENT)

	// â”€â”€ Header â”€â”€
	hdrRC := uiRECT{0, 0, uiW, uiHdrH}
	uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&hdrRC)), aw.hbrHdr)
	// Title text
	uiSelectObjectFn.Call(hdc, aw.hfTitle)
	uiSetTextColorFn.Call(hdc, uintptr(uiClrCyan))
	titleRC := uiRECT{16, 0, 300, uiHdrH}
	uiDrawTextFn.Call(hdc, uintptr(unsafe.Pointer(mustUTF16Ptr("ShortNerdCat"))),
		^uintptr(0), uintptr(unsafe.Pointer(&titleRC)),
		uiDT_LEFT|uiDT_VCENTER|uiDT_SINGLELINE)
	// X button â€” drawn as two diagonal GDI lines (font-independent).
	{
		const cx, cy uintptr = uiW - 20, uiHdrH / 2
		const arm uintptr = 7
		xPen, _, _ := uiCreatePenFn.Call(uiPS_SOLID, 1, uintptr(uiClrMuted))
		oldXPen, _, _ := uiSelectObjectFn.Call(hdc, xPen)
		uiMoveToExFn.Call(hdc, cx-arm, cy-arm, 0)
		uiLineToFn.Call(hdc, cx+arm, cy+arm)
		uiMoveToExFn.Call(hdc, cx+arm, cy-arm, 0)
		uiLineToFn.Call(hdc, cx-arm, cy+arm)
		uiSelectObjectFn.Call(hdc, oldXPen)
		uiDeleteObjectFn.Call(xPen)
	}

	// â”€â”€ Tab bar â”€â”€
	tabBarRC := uiRECT{0, uiHdrH, uiW, uiHdrH + uiTabH}
	uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&tabBarRC)), aw.hbrTab)
	tabs := []string{T("tab_tunnel"), T("tab_settings")}
	tabW := int32(uiW / len(tabs))
	uiSelectObjectFn.Call(hdc, aw.hfBadge)
	for i, label := range tabs {
		x0 := int32(i) * tabW
		tabRC := uiRECT{x0, uiHdrH, x0 + tabW, uiHdrH + uiTabH}
		if i == aw.activeTab {
			uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&tabRC)), aw.hbrTabAct)
			uiSetTextColorFn.Call(hdc, uintptr(uiClrCyan))
			// Active tab underline
			pen, _, _ := uiCreatePenFn.Call(uiPS_SOLID, 2, uintptr(uiClrCyan))
			old, _, _ := uiSelectObjectFn.Call(hdc, pen)
			uiMoveToExFn.Call(hdc, uintptr(x0+4), uintptr(uiHdrH+uiTabH-2), 0)
			uiLineToFn.Call(hdc, uintptr(x0+tabW-4), uintptr(uiHdrH+uiTabH-2))
			uiSelectObjectFn.Call(hdc, old)
			uiDeleteObjectFn.Call(pen)
		} else {
			uiSetTextColorFn.Call(hdc, uintptr(uiClrMuted))
		}
		uiDrawTextFn.Call(hdc, uintptr(unsafe.Pointer(mustUTF16Ptr(label))),
			^uintptr(0), uintptr(unsafe.Pointer(&tabRC)),
			uiDT_CENTER|uiDT_VCENTER|uiDT_SINGLELINE)
	}

	// â”€â”€ Content â”€â”€
	if aw.activeTab == 0 {
		aw.paintTunnel(hdc)
	} else {
		// Solid blue background â€” overrides the cosmic bg image in this area.
		contRC := uiRECT{0, uiContY, uiW, uiH}
		uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&contRC)), aw.hbrSettings)
		aw.paintSettings(hdc)
	}
}

func (aw *AppWindow) paintSettings(hdc uintptr) {
	uiSetBkModeFn.Call(hdc, uiTRANSPARENT)

	// â”€â”€ Section header helper â”€â”€
	paintSectionHdr := func(text string, y int32) {
		uiSelectObjectFn.Call(hdc, aw.hfBadge)
		uiSetTextColorFn.Call(hdc, uintptr(uint32(0x00aad8f0))) // light blue-grey on dark blue
		rc := uiRECT{30, y, uiW - 30, y + 16}
		uiDrawTextFn.Call(hdc, uintptr(unsafe.Pointer(mustUTF16Ptr(text))),
			^uintptr(0), uintptr(unsafe.Pointer(&rc)), uiDT_LEFT|uiDT_SINGLELINE)
		pen, _, _ := uiCreatePenFn.Call(uiPS_SOLID, 1, uintptr(uint32(0x005580b0)))
		old, _, _ := uiSelectObjectFn.Call(hdc, pen)
		uiMoveToExFn.Call(hdc, 30, uintptr(y+17), 0)
		uiLineToFn.Call(hdc, uiW-30, uintptr(y+17))
		uiSelectObjectFn.Call(hdc, old)
		uiDeleteObjectFn.Call(pen)
	}

	// â”€â”€ Sub-label helper â”€â”€
	paintSubLabel := func(text string, y int32) {
		uiSelectObjectFn.Call(hdc, aw.hfBody)
		uiSetTextColorFn.Call(hdc, 0x00FFFFFF)
		rc := uiRECT{30, y, uiW - 30, y + 20}
		uiDrawTextFn.Call(hdc, uintptr(unsafe.Pointer(mustUTF16Ptr(text))),
			^uintptr(0), uintptr(unsafe.Pointer(&rc)), uiDT_LEFT|uiDT_VCENTER|uiDT_SINGLELINE)
	}

	// â”€â”€ Layout â”€â”€
	const s0 = uiContY + 12 // "YOUR LOCATION" header
	const s1 = uiContY + 94 // "CONNECTION OPTIONS" header

	paintSectionHdr(T("section_your_location"), s0)
	paintSubLabel(T("label_your_location"), s0+20)
	// combobox is a native control at y = uiContY+58 â€” painted by Windows

	paintSectionHdr(T("section_connection_options"), s1)
	// Native checkbox (hDoH) is rendered by Windows.
}

// Status bar colors -- COLORREF = R | G<<8 | B<<16, see uiwindow.go:41.
// Requested 2026-08-08: white text on colored background, one color per state.
const (
	statusBarGreen  uint32 = 46 | (160 << 8) | (67 << 16)   // connected
	statusBarGray   uint32 = 120 | (120 << 8) | (120 << 16) // disconnected
	statusBarOrange uint32 = 230 | (140 << 8) | (20 << 16)  // connecting/disconnecting
	statusBarRed    uint32 = 200 | (40 << 8) | (40 << 16)   // error
)

func (aw *AppWindow) paintTunnel(hdc uintptr) {
	aw.mu.Lock()
	s := aw.status
	aw.mu.Unlock()

	// Club membership chevron: full-width bar pinned to the very top of the
	// illustration (uiContY, right below the tab strip) -- mirrors the
	// bottom status bar's shape/position instead of being squeezed into the
	// header next to the title (2026-08-15: moved out of the header per
	// feedback, "должен быть поверх картинки, в самом её верху"). Regular-
	// tier users never see this (aw.clubBadgeText stays "").
	if aw.clubBadgeText != "" {
		bg, fg := uiClrCatClubBadgeBg, uiClrCatClubBadgeFg
		if aw.ClubTheme == "elite" {
			bg, fg = uiClrEliteBadgeBg, uiClrEliteBadgeFg
		}
		const badgeH = 30
		badgeRC := uiRECT{0, uiContY, uiW, uiContY + badgeH}
		badgeBrush, _, _ := uiCreateSolidBrushFn.Call(uintptr(bg))
		uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&badgeRC)), badgeBrush)
		uiDeleteObjectFn.Call(badgeBrush)
		uiSelectObjectFn.Call(hdc, aw.hfBadge)
		uiSetTextColorFn.Call(hdc, uintptr(fg))
		uiDrawTextFn.Call(hdc, uintptr(unsafe.Pointer(mustUTF16Ptr(aw.clubBadgeText))),
			^uintptr(0), uintptr(unsafe.Pointer(&badgeRC)),
			uiDT_CENTER|uiDT_VCENTER|uiDT_SINGLELINE)
	}

	// The full-window background (paintBackground) already shows the
	// current state's kitten illustration -- the small boxed duplicate that
	// used to be drawn here was removed 2026-08-08 per feedback ("стало
	// гораздо лучше, только уменьшенный дубликат картинки теперь не нужен").
	// A full-width colored status bar at the very bottom of the window
	// replaces the old floating text strip (same feedback round, "оформим
	// покрасивее" -- shevron-style bar, white text, one color per state).
	statusText := T("status_disconnected")
	barColor := statusBarGray
	switch {
	case s.Error:
		statusText = T("status_error")
		if s.ErrorMsg != "" {
			statusText = s.ErrorMsg
		}
		barColor = statusBarRed
	case s.Connected:
		statusText = T("status_connected")
		barColor = statusBarGreen
	case s.Connecting:
		statusText = T("status_connecting")
		barColor = statusBarOrange
	case s.Disconnecting:
		statusText = T("status_disconnecting")
		barColor = statusBarOrange
	}

	// Error messages can be long (e.g. "Connect failed: dial tcp ...:
	// timeout") -- give the bar more room and let the text wrap instead of
	// truncating it back down to an uninformative "Error".
	barH := int32(36)
	textFlags := uintptr(uiDT_CENTER | uiDT_VCENTER | uiDT_SINGLELINE)
	if s.Error {
		barH = 54
		textFlags = uiDT_CENTER | uiDT_WORDBREAK
	}
	barRC := uiRECT{0, uiH - barH, uiW, uiH}
	barBr, _, _ := uiCreateSolidBrushFn.Call(uintptr(barColor))
	uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&barRC)), barBr)
	uiDeleteObjectFn.Call(barBr)

	uiSelectObjectFn.Call(hdc, aw.hfBody)
	uiSetBkModeFn.Call(hdc, uiTRANSPARENT)
	uiSetTextColorFn.Call(hdc, 0x00FFFFFF) // white, regardless of bar color
	textRC := barRC
	if s.Error {
		textRC = uiRECT{8, barRC.Top + 4, uiW - 8, barRC.Bottom - 4}
	}
	uiDrawTextFn.Call(hdc, uintptr(unsafe.Pointer(mustUTF16Ptr(statusText))),
		^uintptr(0), uintptr(unsafe.Pointer(&textRC)),
		textFlags)
}

func (aw *AppWindow) handleDrawItem(lp uintptr) {
	di := (*uiDRAWITEMSTRUCT)(unsafe.Pointer(lp))
	hdc := di.HDC
	rc := di.RcItem

	pressed := (di.ItemState & uiODS_SELECTED) != 0

	var label string
	switch di.CtlID {
	case uiIDConnect:
		label = T("button_connect")
	case uiIDDisconnect:
		label = T("button_disconnect")
	default:
		return
	}

	// Convex pill button: solid blue fill, rounded corners, a lighter
	// highlight band across the top half for a raised/glossy look, white
	// text. Requested 2026-08-08: "выпуклой, со скругленными углами, белый
	// текст на синем фоне." Pressed state uses a darker blue and no
	// highlight, reading as "pushed in" rather than "lit up."
	const (
		blueBase  uint32 = 30 | (110 << 8) | (220 << 16) // #1E6EDC
		bluePress uint32 = 20 | (75 << 8) | (160 << 16)  // darker, "pushed in"
		blueHi    uint32 = 90 | (160 << 8) | (240 << 16) // lighter, top-half glossy highlight
	)
	base := blueBase
	if pressed {
		base = bluePress
	}

	rgn, _, _ := uiCreateRoundRectRgnFn.Call(
		uintptr(rc.Left), uintptr(rc.Top),
		uintptr(rc.Right+1), uintptr(rc.Bottom+1),
		16, 16)

	baseBr, _, _ := uiCreateSolidBrushFn.Call(uintptr(base))
	uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&rc)), baseBr)
	uiDeleteObjectFn.Call(baseBr)

	if !pressed {
		// Clip to the pill shape, then paint a lighter band over the top
		// half only -- this is what reads as a glossy/convex highlight.
		uiSelectClipRgnFn.Call(hdc, rgn)
		hiRC := uiRECT{rc.Left, rc.Top, rc.Right, rc.Top + (rc.Bottom-rc.Top)/2}
		hiBr, _, _ := uiCreateSolidBrushFn.Call(uintptr(blueHi))
		uiFillRectFn.Call(hdc, uintptr(unsafe.Pointer(&hiRC)), hiBr)
		uiDeleteObjectFn.Call(hiBr)
		uiSelectClipRgnFn.Call(hdc, 0)
	}
	uiDeleteObjectFn.Call(rgn)

	// Outline, same pill shape.
	pen, _, _ := uiCreatePenFn.Call(uiPS_SOLID, 1, uintptr(base))
	nullBr, _, _ := uiGetStockObjectFn.Call(uiNULL_BRUSH)
	oldPen, _, _ := uiSelectObjectFn.Call(hdc, pen)
	oldBr, _, _ := uiSelectObjectFn.Call(hdc, nullBr)
	uiRoundRectFn.Call(hdc,
		uintptr(rc.Left), uintptr(rc.Top),
		uintptr(rc.Right), uintptr(rc.Bottom),
		16, 16)
	uiSelectObjectFn.Call(hdc, oldPen)
	uiSelectObjectFn.Call(hdc, oldBr)
	uiDeleteObjectFn.Call(pen)

	// Label -- always white.
	uiSetBkModeFn.Call(hdc, uiTRANSPARENT)
	uiSetTextColorFn.Call(hdc, 0x00FFFFFF)
	uiSelectObjectFn.Call(hdc, aw.hfBadge)
	uiDrawTextFn.Call(hdc, uintptr(unsafe.Pointer(mustUTF16Ptr(label))),
		^uintptr(0), uintptr(unsafe.Pointer(&rc)),
		uiDT_CENTER|uiDT_VCENTER|uiDT_SINGLELINE)
}

// â”€â”€ Interaction â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func (aw *AppWindow) handleClick(x, y int32) {
	// Header X button (top-right 40px)
	if y < uiHdrH && x >= uiW-40 {
		uiShowWindowFn.Call(aw.hwnd, uiSW_HIDE)
		return
	}
	// Tab strip
	if y >= uiHdrH && y < uiHdrH+uiTabH {
		tab := int(x) / (uiW / 2)
		if tab != aw.activeTab {
			aw.activeTab = tab
			aw.syncTabControls()
			uiInvalidateRectFn.Call(aw.hwnd, 0, 0) // bErase=FALSE â€” WM_PAINT double-buffers everything
		}
		return
	}
}

func (aw *AppWindow) handleCommand(id, notif uint16) {
	switch id {
	case uiIDConnect:
		if notif == uiBN_CLICKED && aw.ConnectFn != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "connect"))
			go aw.ConnectFn()
		}
	case uiIDDisconnect:
		if notif == uiBN_CLICKED && aw.DisconnectFn != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "disconnect"))
			go aw.DisconnectFn()
		}
	case uiIDDoH, uiIDBlockQUIC:
		if notif == uiBN_CLICKED {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu,
				logevent.Str(logevent.AttrAction, "checkbox_toggle"),
				logevent.Str(logevent.AttrDetail, fmt.Sprintf("id=%d", id)))
			go aw.saveSettings()
		}
	case uiIDLogUpload:
		if notif == uiBN_CLICKED {
			checked := checkboxChecked(aw.hLogUpload)
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu,
				logevent.Str(logevent.AttrAction, "log_upload_toggle"),
				logevent.Str(logevent.AttrDetail, fmt.Sprintf("enabled=%v", checked)))
			if aw.LogUploadToggleFn != nil {
				go aw.LogUploadToggleFn(checked)
			}
		}
	case uiIDRegion:
		if notif == uiCBN_SELCHANGE {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "region_combo"))
			aw.saveSettings()
		}

	// â”€â”€ Native menu bar â”€â”€ mirrors the tray's right-click menu item-for-item.
	case uiIDMenuLogin:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "login"))
		if aw.LoginFn != nil {
			go aw.LoginFn()
		}
	case uiIDMenuLogout:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "logout"))
		if aw.LogoutFn != nil {
			go aw.LogoutFn()
		}
	case uiIDMenuConnect:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "connect_menu"))
		if aw.ConnectFn != nil {
			go aw.ConnectFn()
		}
	case uiIDMenuDisconnect:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "disconnect_menu"))
		if aw.DisconnectFn != nil {
			go aw.DisconnectFn()
		}
	case uiIDMenuDoH:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "doh_toggle"))
		aw.toggleMenuSetting(func(s *AppSettings) { s.DoH = !s.DoH })
	case uiIDMenuBlockQUIC:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "quic_toggle"))
		aw.toggleMenuSetting(func(s *AppSettings) { s.BlockQUIC = !s.BlockQUIC })
	case uiIDMenuRegionAuto:
		aw.setMenuRegion("")
	case uiIDMenuRegionRussia:
		aw.setMenuRegion("RU")
	case uiIDMenuRegionEurope:
		aw.setMenuRegion("EU")
	case uiIDMenuRegionUSA:
		aw.setMenuRegion("US")
	case uiIDMenuRegionChina:
		aw.setMenuRegion("CN")
	case uiIDMenuRegionOther:
		aw.setMenuRegion("XX")
	case uiIDMenuPreviewRegular:
		aw.ReloadClubTheme("", "")
	case uiIDMenuPreviewCatClub:
		// No distinguishing marker text: the preview must look exactly like
		// what a real member sees, same as every other client.
		aw.ReloadClubTheme("catclub", T("badge_cat_club_member"))
	case uiIDMenuPreviewElite:
		aw.ReloadClubTheme("elite", T("badge_elite_cat_club_member"))
	case uiIDMenuRecommend:
		// ShowRecommendDialog is a nested modal message loop (same pattern
		// Win32's own MessageBox uses) -- safe to call synchronously from
		// here. What happens after is network I/O, so that part is
		// dispatched via go, same reasoning as the About/Update/Quit
		// callbacks below.
		if username, ok := ShowRecommendDialog(); ok && aw.RecommendFn != nil {
			go aw.RecommendFn(username)
		}
	case uiIDMenuAbout:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "about"))
		// go, not a direct call -- audit finding, 2026-08-10: every other
		// Fn callback invoked from WM_COMMAND (the UI thread) was already
		// either channel-dispatched (TriggerLogin/TriggerConnect/... are
		// non-blocking sends) or spawned via go; these three were the only
		// ones calling straight through, which is exactly the "GUI blocked
		// on logic" shape the user asked to eliminate. AboutFn itself just
		// shows a splash, but nothing here should ever depend on a
		// callback field staying synchronous just because it happens to
		// be cheap today.
		if aw.AboutFn != nil {
			go aw.AboutFn()
		}
	case uiIDMenuUpdate:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "update"))
		// UpdateFn -> core.ApplyPendingUpdate(), real file I/O (staging/
		// replacing the installed binary) -- must never run on the UI
		// thread, see the case above.
		if aw.UpdateFn != nil {
			go aw.UpdateFn()
		}
	case uiIDMenuQuit:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowMenu, logevent.Str(logevent.AttrAction, "quit"))
		// QuitFn -> TriggerQuit(), already a non-blocking channel send, but
		// go here too for the same reason as AboutFn: this call site should
		// never itself become a reason a future change to QuitFn blocks the
		// message pump.
		if aw.QuitFn != nil {
			go aw.QuitFn()
		}
	}
}

func (aw *AppWindow) saveSettings() {
	if aw.SetSettingsFn == nil {
		return
	}
	aw.SetSettingsFn(AppSettings{
		DoH:       checkboxChecked(aw.hDoH),
		BlockQUIC: checkboxChecked(aw.hBlockQUIC),
		Region:    regionFromIndex(comboGetSel(aw.hRegion)),
	})
	// Re-read canonical settings (tray may have applied mutual exclusion)
	// and mirror back to controls.
	if aw.GetSettingsFn != nil {
		aw.applySettingsToControls(aw.GetSettingsFn())
	}
}

func (aw *AppWindow) syncWindowIcon() {
	aw.mu.Lock()
	s := aw.status
	aw.mu.Unlock()
	var ico []byte
	var icoName string
	switch {
	case s.Connected:
		ico, icoName = icoConnected, "connected"
	case s.Disconnecting, s.Connecting:
		ico, icoName = icoConnecting, "connecting"
	default:
		ico, icoName = icoIdle, "idle"
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
		logevent.Str(logevent.AttrStage, "status"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("syncWindowIcon -> %s (connected=%v connecting=%v disconnecting=%v)", icoName, s.Connected, s.Connecting, s.Disconnecting)))
	newIcon := setWindowIconFromICO(aw.hwnd, ico)
	if aw.hIcon != 0 {
		winDestroyIconFn.Call(aw.hIcon)
	}
	aw.hIcon = newIcon
}

func (aw *AppWindow) syncConnectButtons() {
	if aw.activeTab != 0 {
		uiShowWindowFn.Call(aw.hConnect, uiSW_HIDE)
		uiShowWindowFn.Call(aw.hDisconnect, uiSW_HIDE)
		return
	}
	aw.mu.Lock()
	s := aw.status
	aw.mu.Unlock()
	sw := func(h uintptr, show bool) {
		v := uintptr(uiSW_HIDE)
		if show {
			v = uiSW_SHOW
		}
		uiShowWindowFn.Call(h, v)
	}
	sw(aw.hConnect, !s.Connected && !s.Connecting && !s.Disconnecting)
	sw(aw.hDisconnect, s.Connected || s.Connecting)
}

func (aw *AppWindow) syncTabControls() {
	// Show/hide controls based on active tab.
	tunnel := aw.activeTab == 0
	settings := aw.activeTab == 1
	showIf := func(h uintptr, show bool) {
		v := uintptr(uiSW_HIDE)
		if show {
			v = uintptr(uiSW_SHOW)
		}
		uiShowWindowFn.Call(h, v)
	}
	// Tunnel controls
	showIf(aw.hConnect, tunnel)
	showIf(aw.hDisconnect, tunnel)
	// Settings controls
	showIf(aw.hRegion, settings)
	showIf(aw.hDoH, settings)
	showIf(aw.hBlockQUIC, settings)
	showIf(aw.hLogUpload, settings)
	if tunnel {
		aw.syncConnectButtons()
	}
}

// â”€â”€ Lifecycle â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// NewAppWindow creates an AppWindow. Call Start() to spin up the Win32 window.
func NewAppWindow() *AppWindow {
	return &AppWindow{readyCh: make(chan struct{})}
}

// Start spins up the native Win32 window on a dedicated OS-locked goroutine.
func (aw *AppWindow) Start() {
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle, logevent.Str(logevent.AttrStage, "start"))
	aw.startOnce.Do(func() { go aw.runLoop() })
}

func (aw *AppWindow) runLoop() {
	defer func() {
		if r := recover(); r != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
				logevent.Str(logevent.AttrStage, "run_panic"),
				logevent.Str(logevent.AttrDetail, fmt.Sprintf("%v\n%s", r, debug.Stack())))
			close(aw.readyCh)
		}
	}()
	runtime.LockOSThread()

	globalAW = aw

	uiWndProcOnce.Do(func() {
		uiWndProcCB = windows.NewCallback(uiWndProc)
	})

	// Register window class.
	hInst, _, _ := uiGetModuleHandleFn.Call(0)
	className, _ := windows.UTF16PtrFromString("ShortNerdCatUI")

	// Build idle HICON for the window class so the taskbar button shows the
	// correct icon from the very first frame, before WM_SETICON is sent.
	var classHIcon uintptr
	if len(icoIdle) > 22 {
		imgData := icoIdle[22:]
		classHIcon, _, _ = winCreateIconFromResourceEx.Call(
			uintptr(unsafe.Pointer(&imgData[0])),
			uintptr(len(imgData)),
			1, 0x00030000, 32, 32, 0,
		)
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
		logevent.Str(logevent.AttrStage, "class_icon"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("hIcon=0x%x", classHIcon)))

	wc := uiWNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(uiWNDCLASSEX{})),
		LpfnWndProc:   uiWndProcCB,
		HInstance:     hInst,
		HIcon:         classHIcon,
		HIconSm:       classHIcon,
		LpszClassName: className,
	}
	uiRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	// Native menu bar ("ShortNerdCat" ▾) — built before the window so its
	// handle can be passed to CreateWindowExW directly. cyMenu is added to
	// the requested window height so the CLIENT area (everything painted
	// with the uiW/uiH constants) stays exactly uiW×uiH; Windows carves the
	// menu bar's own strip out of the extra height automatically.
	hMenuBar := aw.createMenuBar()
	cyMenu, _, _ := uiGetSystemMetricsFn.Call(uiSM_CYMENU)
	aw.cyMenu = int32(cyMenu)

	// Centre on screen.
	sw, _, _ := uiGetSystemMetricsFn.Call(0) // SM_CXSCREEN
	sh, _, _ := uiGetSystemMetricsFn.Call(1) // SM_CYSCREEN
	totalH := uiH + int32(cyMenu)
	x := (int32(sw) - uiW) / 2
	y := (int32(sh) - totalH) / 2

	winName, _ := windows.UTF16PtrFromString(T("app_title"))
	hwnd, _, _ := uiCreateWindowExW.Call(
		uiWS_EX_APPWINDOW,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(winName)),
		uiWS_POPUP|uiWS_SYSMENU|uiWS_VISIBLE,
		uintptr(x), uintptr(y), uiW, uintptr(totalH),
		0, hMenuBar, hInst, 0)
	if hwnd == 0 {
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle, logevent.Str(logevent.AttrStage, "window_failed"))
		close(aw.readyCh)
		return
	}
	aw.hwnd = hwnd
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
		logevent.Str(logevent.AttrStage, "hwnd_created"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("0x%x", hwnd)))

	// Dark title bar via DWM.
	darkMode := uint32(1)
	uiDwmSetAttrFn.Call(hwnd, uiDWMWA_DARK, uintptr(unsafe.Pointer(&darkMode)), 4)

	// Belt-and-suspenders: send WM_SETICON too (class icon covers the taskbar,
	// this covers the window caption icon and ALT+TAB).
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
		logevent.Str(logevent.AttrStage, "initial_icon"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("len=%d", len(icoIdle))))
	aw.hIcon = setWindowIconFromICO(hwnd, icoIdle)

	// Build GDI resources.
	aw.initGDI()

	// Create child controls.
	aw.createControls(hInst)

	// Load initial settings.
	if aw.GetSettingsFn != nil {
		s := aw.GetSettingsFn()
		aw.mu.Lock()
		aw.settings = s
		aw.mu.Unlock()
		aw.applySettingsToControls(s)
	}

	aw.syncTabControls()

	close(aw.readyCh)
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle, logevent.Str(logevent.AttrStage, "ready"))
	// Sync status immediately â€” callStatusChange may have fired before readyCh was closed.
	if aw.StatusFn != nil {
		aw.UpdateStatus(aw.StatusFn())
	}

	// Message loop.
	type msg struct {
		Hwnd    uintptr
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      uiPOINT
	}
	var m msg
	for {
		r, _, _ := uiGetMessageFn.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		uiTranslateMessageFn.Call(uintptr(unsafe.Pointer(&m)))
		uiDispatchMessageFn.Call(uintptr(unsafe.Pointer(&m)))
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle, logevent.Str(logevent.AttrStage, "message_loop_exited"))
}

// â”€â”€ GDI init / cleanup â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// themedCatPNGs picks the illustration byte-slice set matching aw.ClubTheme,
// falling back to the default set for an unrecognized value.
func (aw *AppWindow) themedCatPNGs() (idle, connecting, connected []byte) {
	switch aw.ClubTheme {
	case "catclub":
		return catIdlePNGCatClub, catConnectingPNGCatClub, catConnectedPNGCatClub
	case "elite":
		return catIdlePNGElite, catConnectingPNGElite, catConnectedPNGElite
	default:
		return catIdlePNG, catConnectingPNG, catConnectedPNG
	}
}

func (aw *AppWindow) initGDI() {
	mkFont := func(h int32, weight uint32, italic bool, face string) uintptr {
		it := uintptr(0)
		if italic {
			it = 1
		}
		fp, _ := windows.UTF16PtrFromString(face)
		f, _, _ := uiCreateFontFn.Call(
			uintptr(h), 0, 0, 0, uintptr(weight), it, 0, 0,
			uiDEFAULT_CHARSET, 0, 0, uiCLEARTYPE_QUALITY, 0,
			uintptr(unsafe.Pointer(fp)))
		return f
	}
	aw.hfTitle = mkFont(-18, uiFW_BOLD, false, ThemeFontUI)
	aw.hfBody = mkFont(-14, uiFW_NORMAL, false, ThemeFontUI)
	aw.hfBadge = mkFont(-12, uiFW_BOLD, false, ThemeFontUI)

	mkBrush := func(c uint32) uintptr {
		b, _, _ := uiCreateSolidBrushFn.Call(uintptr(c))
		return b
	}
	aw.hbrBg = mkBrush(uiClrBg)
	aw.hbrHdr = mkBrush(uiClrHdr)
	aw.hbrTab = mkBrush(uiClrTab)
	aw.hbrTabAct = mkBrush(uiClrTabAct)
	aw.hbrPanel = mkBrush(uiClrPanel)
	aw.hbrSettings = mkBrush(uiClrSettingsBg)

	// Build background bitmap from bg.png.
	src, err := png.Decode(bytes.NewReader(uiBgPNG))
	if err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
			logevent.Str(logevent.AttrStage, "bg_decode_failed"),
			logevent.Str(logevent.AttrDetail, err.Error()))
		return
	}
	scaled := bilinearScale(src, uiW, uiH)
	screenDC, _, _ := uiGetDCFn.Call(0)
	memDC, _, _ := uiCreateCompDCFn.Call(screenDC)
	uiReleaseDCFn.Call(0, screenDC)

	bi := uiBMI{}
	bi.H.BiSize = uint32(unsafe.Sizeof(bi.H))
	bi.H.BiWidth = int32(uiW)
	bi.H.BiHeight = -int32(uiH)
	bi.H.BiPlanes = 1
	bi.H.BiBitCount = 32

	var bits unsafe.Pointer
	hbm, _, _ := uiCreateDIBSectionFn.Call(memDC,
		uintptr(unsafe.Pointer(&bi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 {
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle, logevent.Str(logevent.AttrStage, "dib_failed"))
		uiDeleteDCFn.Call(memDC)
		return
	}
	uiSelectObjectFn.Call(memDC, hbm)

	// Write BGRA pixels.
	n := uiW * uiH
	pixels := (*[1 << 26]uint32)(bits)[:n:n]
	b := scaled.Bounds()
	for y := 0; y < uiH; y++ {
		for x := 0; x < uiW; x++ {
			c := scaled.NRGBAAt(b.Min.X+x, b.Min.Y+y)
			pixels[y*uiW+x] = uint32(c.B) | (uint32(c.G) << 8) | (uint32(c.R) << 16) | (uint32(c.A) << 24)
		}
	}
	aw.bgMemDC = memDC
	aw.bgBitmap = hbm
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
		logevent.Str(logevent.AttrStage, "bg_ready"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("%dx%d", uiW, uiH)))

	idlePNG, connectingPNG, connectedPNG := aw.themedCatPNGs()
	aw.loadCatBitmaps(idlePNG, connectingPNG, connectedPNG)
}

// catEntry pairs one illustration's source PNG bytes with the DC/bitmap
// handle fields it fills in -- used by loadCatBitmaps below.
type catEntry struct {
	data []byte
	dc   *uintptr
	bm   *uintptr
}

// loadCatBitmaps builds both the small (catSizeÃ—catSize) and full-window
// (uiWÃ—uiH) premultiplied-alpha DIBs for the four tunnel-state
// illustrations, writing into aw.catIdleDC/BM etc. Split out of initGDI so
// ReloadClubTheme can rebuild just these bitmaps from a different PNG set
// without re-running the rest of initGDI (fonts, brushes, background --
// none of which depend on club theme).
//
// Callers must free any bitmaps already present in the target fields
// before calling this (see freeGDI's freeDIB pattern) -- it always
// overwrites, never frees what it's replacing.
func (aw *AppWindow) loadCatBitmaps(idlePNG, connectingPNG, connectedPNG []byte) {
	// Load cat images (pre-multiplied alpha, catSizeÃ—catSize).
	cats := []catEntry{
		{idlePNG, &aw.catIdleDC, &aw.catIdleBM},
		{connectingPNG, &aw.catConnectingDC, &aw.catConnBM},
		{connectedPNG, &aw.catConnectedDC, &aw.catConBM},
	}
	for _, c := range cats {
		img, err := png.Decode(bytes.NewReader(c.data))
		if err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
				logevent.Str(logevent.AttrStage, "cat_decode_failed"),
				logevent.Str(logevent.AttrDetail, err.Error()))
			continue
		}
		scaled := bilinearScale(img, catSize, catSize)
		sDC, _, _ := uiGetDCFn.Call(0)
		mDC, _, _ := uiCreateCompDCFn.Call(sDC)
		uiReleaseDCFn.Call(0, sDC)

		cbi := uiBMI{}
		cbi.H.BiSize = uint32(unsafe.Sizeof(cbi.H))
		cbi.H.BiWidth = catSize
		cbi.H.BiHeight = -catSize
		cbi.H.BiPlanes = 1
		cbi.H.BiBitCount = 32
		var cbits unsafe.Pointer
		cbm, _, _ := uiCreateDIBSectionFn.Call(mDC,
			uintptr(unsafe.Pointer(&cbi)), 0,
			uintptr(unsafe.Pointer(&cbits)), 0, 0)
		if cbm == 0 {
			uiDeleteDCFn.Call(mDC)
			continue
		}
		uiSelectObjectFn.Call(mDC, cbm)

		n := catSize * catSize
		pix := (*[1 << 26]uint32)(cbits)[:n:n]
		b := scaled.Bounds()
		for y := 0; y < catSize; y++ {
			for x := 0; x < catSize; x++ {
				col := scaled.NRGBAAt(b.Min.X+x, b.Min.Y+y)
				// Pre-multiply alpha for AlphaBlend AC_SRC_ALPHA.
				a := uint32(col.A)
				r := uint32(col.R) * a / 255
				g := uint32(col.G) * a / 255
				bv := uint32(col.B) * a / 255
				pix[y*catSize+x] = bv | (g << 8) | (r << 16) | (a << 24)
			}
		}
		*c.dc = mDC
		*c.bm = cbm
	}

	// Full-window-sized versions of the same 4 illustrations (see the
	// catIdleBgDC field comment) -- same premultiplied-alpha DIB build, just
	// scaled to uiWÃ—uiH instead of catSizeÃ—catSize.
	catsBg := []catEntry{
		{idlePNG, &aw.catIdleBgDC, &aw.catIdleBgBM},
		{connectingPNG, &aw.catConnectingBgDC, &aw.catConnectingBgBM},
		{connectedPNG, &aw.catConnectedBgDC, &aw.catConnectedBgBM},
	}
	for _, c := range catsBg {
		img, err := png.Decode(bytes.NewReader(c.data))
		if err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
				logevent.Str(logevent.AttrStage, "cat_bg_decode_failed"),
				logevent.Str(logevent.AttrDetail, err.Error()))
			continue
		}
		scaled := bilinearScale(img, uiW, uiH)
		sDC, _, _ := uiGetDCFn.Call(0)
		mDC, _, _ := uiCreateCompDCFn.Call(sDC)
		uiReleaseDCFn.Call(0, sDC)

		cbi := uiBMI{}
		cbi.H.BiSize = uint32(unsafe.Sizeof(cbi.H))
		cbi.H.BiWidth = uiW
		cbi.H.BiHeight = -uiH
		cbi.H.BiPlanes = 1
		cbi.H.BiBitCount = 32
		var cbits unsafe.Pointer
		cbm, _, _ := uiCreateDIBSectionFn.Call(mDC,
			uintptr(unsafe.Pointer(&cbi)), 0,
			uintptr(unsafe.Pointer(&cbits)), 0, 0)
		if cbm == 0 {
			uiDeleteDCFn.Call(mDC)
			continue
		}
		uiSelectObjectFn.Call(mDC, cbm)

		n := int(uiW * uiH)
		pix := (*[1 << 28]uint32)(cbits)[:n:n]
		b := scaled.Bounds()
		for y := 0; y < int(uiH); y++ {
			for x := 0; x < int(uiW); x++ {
				col := scaled.NRGBAAt(b.Min.X+x, b.Min.Y+y)
				a := uint32(col.A)
				r := uint32(col.R) * a / 255
				g := uint32(col.G) * a / 255
				bv := uint32(col.B) * a / 255
				pix[y*int(uiW)+x] = bv | (g << 8) | (r << 16) | (a << 24)
			}
		}
		*c.dc = mDC
		*c.bm = cbm
	}
}

func (aw *AppWindow) freeGDI() {
	del := func(h uintptr) {
		if h != 0 {
			uiDeleteObjectFn.Call(h)
		}
	}
	freeDIB := func(dc, bm uintptr) {
		if bm != 0 {
			uiSelectObjectFn.Call(dc, bm)
			del(bm)
		}
		if dc != 0 {
			uiDeleteDCFn.Call(dc)
		}
	}
	if aw.hIcon != 0 {
		winDestroyIconFn.Call(aw.hIcon)
		aw.hIcon = 0
	}
	del(aw.hfTitle)
	del(aw.hfBody)
	del(aw.hfBadge)
	del(aw.hbrBg)
	del(aw.hbrHdr)
	del(aw.hbrTab)
	del(aw.hbrTabAct)
	del(aw.hbrPanel)
	del(aw.hbrSettings)
	freeDIB(aw.bgMemDC, aw.bgBitmap)
	freeDIB(aw.catIdleDC, aw.catIdleBM)
	freeDIB(aw.catConnectingDC, aw.catConnBM)
	freeDIB(aw.catConnectedDC, aw.catConBM)
	freeDIB(aw.catIdleBgDC, aw.catIdleBgBM)
	freeDIB(aw.catConnectingBgDC, aw.catConnectingBgBM)
	freeDIB(aw.catConnectedBgDC, aw.catConnectedBgBM)
}

// ReloadClubTheme requests switching the illustration set and header badge
// live -- unlike every other GDI resource in this file, club theme can
// become known *after* the window is already up (membership is confirmed
// post-login, well after initGDI's one-time build at Start()). badgeText
// is shown as a chevron in the header ("Cat Club Member #N" / "Elite Cat
// Club Member #N"); pass "" for the regular (no badge) tier.
//
// Safe to call from any goroutine (e.g. a ClubDiscoverer membership
// callback firing on its own poller goroutine): this only stashes the
// requested theme/badge and posts uiWM_ReloadClubTheme -- the actual GDI
// handle free/rebuild happens in applyClubTheme on the runLoop thread,
// same cross-thread-marshaling pattern as UpdateStatus/uiWM_UpdateStatus.
func (aw *AppWindow) ReloadClubTheme(theme, badgeText string) {
	aw.mu.Lock()
	aw.pendingClubTheme = theme
	aw.pendingBadgeText = badgeText
	aw.mu.Unlock()
	if aw.hwnd != 0 {
		uiPostMessageFn.Call(aw.hwnd, uiWM_ReloadClubTheme, 0, 0)
	}
}

// SetAdminAccount enables or disables the "Preview Theme" menu item --
// call once login completes with the parsed key's IsAdmin flag (see
// KeyData in snc/core/key.go). Safe to call from any goroutine; the actual
// EnableMenuItem call is marshaled onto the runLoop thread via
// uiWM_SetAdminAccount, same pattern as ReloadClubTheme.
func (aw *AppWindow) SetAdminAccount(isAdmin bool) {
	aw.mu.Lock()
	aw.pendingIsAdmin = isAdmin
	aw.mu.Unlock()
	if aw.hwnd != 0 {
		uiPostMessageFn.Call(aw.hwnd, uiWM_SetAdminAccount, 0, 0)
	}
}

// SetCanRecommend enables or disables the "Recommend new Cat Club members"
// menu item -- call whenever Cat Club (or subsuming) membership status
// changes, e.g. from a ClubDiscoverer.SetMembershipCallback. Safe to call
// from any goroutine; marshaled onto the runLoop thread via
// uiWM_SetCanRecommend, same pattern as SetAdminAccount.
func (aw *AppWindow) SetCanRecommend(can bool) {
	aw.mu.Lock()
	aw.pendingCanRecommend = can
	aw.mu.Unlock()
	if aw.hwnd != 0 {
		uiPostMessageFn.Call(aw.hwnd, uiWM_SetCanRecommend, 0, 0)
	}
}

// applyClubTheme does the actual GDI work for ReloadClubTheme -- must only
// run on the runLoop thread (called from handleMsg's uiWM_ReloadClubTheme
// case). Frees the 8 existing cat-illustration handles (small +
// full-window, matching freeGDI's freeDIB pattern exactly, since these are
// OS resources Go's GC does not know how to release), rebuilds them from
// the new theme's PNGs via loadCatBitmaps, updates the header badge text,
// and repaints.
func (aw *AppWindow) applyClubTheme(theme, badgeText string) {
	if aw.ClubTheme == theme && aw.clubBadgeText == badgeText {
		return
	}
	aw.clubBadgeText = badgeText
	del := func(h uintptr) {
		if h != 0 {
			uiDeleteObjectFn.Call(h)
		}
	}
	freeDIB := func(dc, bm uintptr) {
		if bm != 0 {
			uiSelectObjectFn.Call(dc, bm)
			del(bm)
		}
		if dc != 0 {
			uiDeleteDCFn.Call(dc)
		}
	}
	freeDIB(aw.catIdleDC, aw.catIdleBM)
	freeDIB(aw.catConnectingDC, aw.catConnBM)
	freeDIB(aw.catConnectedDC, aw.catConBM)
	freeDIB(aw.catIdleBgDC, aw.catIdleBgBM)
	freeDIB(aw.catConnectingBgDC, aw.catConnectingBgBM)
	freeDIB(aw.catConnectedBgDC, aw.catConnectedBgBM)

	aw.ClubTheme = theme
	idlePNG, connectingPNG, connectedPNG := aw.themedCatPNGs()
	aw.loadCatBitmaps(idlePNG, connectingPNG, connectedPNG)

	if aw.hwnd != 0 {
		uiInvalidateRectFn.Call(aw.hwnd, 0, 0) // bErase=FALSE -- WM_PAINT double-buffers everything
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
		logevent.Str(logevent.AttrStage, "club_theme"),
		logevent.Str(logevent.AttrDetail, theme))
}

// â”€â”€ Native menu bar â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
//
// A regular Win32 menu bar (not custom-painted, unlike the rest of this
// window) so it looks and behaves exactly like any other Windows app's menu
// -- mirrors the tray's right-click menu item-for-item, so every action is
// reachable from the window without hunting for the tray icon. Requested
// specifically after a support incident where the only way to log out and
// re-enter credentials was via the (non-obvious) tray icon.
//
// createMenuBar must be called before CreateWindowExW: its return value is
// passed as CreateWindowExW's hMenu parameter directly.
func (aw *AppWindow) createMenuBar() uintptr {
	hMenuBar, _, _ := uiCreateMenuFn.Call()
	hMain, _, _ := uiCreatePopupMenuFn.Call()

	appendStr := func(hMenu uintptr, id uintptr, text string) {
		p, _ := windows.UTF16PtrFromString(text)
		uiAppendMenuFn.Call(hMenu, uiMF_STRING, id, uintptr(unsafe.Pointer(p)))
	}
	appendSep := func(hMenu uintptr) {
		uiAppendMenuFn.Call(hMenu, uiMF_SEPARATOR, 0, 0)
	}
	appendPopup := func(hMenu, hSub uintptr, text string) {
		p, _ := windows.UTF16PtrFromString(text)
		uiAppendMenuFn.Call(hMenu, uiMF_POPUP, hSub, uintptr(unsafe.Pointer(p)))
	}

	appendStr(hMain, uiIDMenuLogin, T("login_button"))
	appendStr(hMain, uiIDMenuLogout, T("tray_logout"))
	appendSep(hMain)
	appendStr(hMain, uiIDMenuConnect, T("tray_connect"))
	appendStr(hMain, uiIDMenuDisconnect, T("tray_disconnect"))
	appendSep(hMain)
	appendStr(hMain, uiIDMenuDoH, T("tray_doh"))
	appendStr(hMain, uiIDMenuBlockQUIC, T("tray_block_quic"))

	hRegion, _, _ := uiCreatePopupMenuFn.Call()
	appendStr(hRegion, uiIDMenuRegionAuto, T("region_auto"))
	appendStr(hRegion, uiIDMenuRegionRussia, T("region_russia"))
	appendStr(hRegion, uiIDMenuRegionEurope, T("region_europe"))
	appendStr(hRegion, uiIDMenuRegionUSA, T("region_usa"))
	appendStr(hRegion, uiIDMenuRegionChina, T("region_china"))
	appendStr(hRegion, uiIDMenuRegionOther, T("region_other"))
	aw.hMenuRegion = hRegion
	appendPopup(hMain, hRegion, T("menu_region"))

	// Admin-only: preview any club theme regardless of actual membership.
	// Grayed out until SetAdminAccount(true) confirms the logged-in key is
	// an admin account -- see uiIDMenuPreviewRegular's doc comment.
	hPreview, _, _ := uiCreatePopupMenuFn.Call()
	appendStr(hPreview, uiIDMenuPreviewRegular, T("preview_regular"))
	appendStr(hPreview, uiIDMenuPreviewCatClub, T("preview_cat_club"))
	appendStr(hPreview, uiIDMenuPreviewElite, T("preview_elite_cat_club"))
	aw.hMenuPreview = hPreview
	appendPopup(hMain, hPreview, T("menu_preview_theme"))
	// AppendMenu(MF_POPUP, hSubMenu, ...) uses the submenu's own handle as
	// this item's "ID" -- EnableMenuItem+MF_BYCOMMAND can target it the
	// same way (the popup has no WM_COMMAND id of its own to select by).
	uiEnableMenuItemFn.Call(hMain, hPreview, uiMF_BYCOMMAND|uiMF_GRAYED)

	// Recommend-a-member -- grayed until SetCanRecommend(true) confirms Cat
	// Club (or subsuming) access. Regular string item, not a popup, so it's
	// enabled/disabled directly by its own command ID.
	appendStr(hMain, uiIDMenuRecommend, T("menu_recommend"))
	uiEnableMenuItemFn.Call(hMain, uintptr(uiIDMenuRecommend), uiMF_BYCOMMAND|uiMF_GRAYED)

	appendSep(hMain)
	appendStr(hMain, uiIDMenuAbout, T("tray_about"))
	appendStr(hMain, uiIDMenuUpdate, T("tray_update"))
	appendSep(hMain)
	appendStr(hMain, uiIDMenuQuit, T("tray_quit"))

	aw.hMenuMain = hMain
	appendPopup(hMenuBar, hMain, T("app_title"))
	return hMenuBar
}

// syncMenuState mirrors s onto the menu bar's checkmarks and the Update
// item's enabled state. Called from applySettingsToControls (its single
// caller-independent choke point) so the menu never drifts out of sync with
// the Settings tab, regardless of which one the user last touched.
func (aw *AppWindow) syncMenuState(s AppSettings) {
	if aw.hMenuMain == 0 {
		return
	}

	checkFlag := func(v bool) uintptr {
		if v {
			return uiMF_BYCOMMAND | uiMF_CHECKED
		}
		return uiMF_BYCOMMAND | uiMF_UNCHECKED
	}
	uiCheckMenuItemFn.Call(aw.hMenuMain, uiIDMenuDoH, checkFlag(s.DoH))
	uiCheckMenuItemFn.Call(aw.hMenuMain, uiIDMenuBlockQUIC, checkFlag(s.BlockQUIC))

	if aw.hMenuRegion != 0 {
		regionIDs := map[string]uintptr{
			"":   uiIDMenuRegionAuto,
			"RU": uiIDMenuRegionRussia,
			"EU": uiIDMenuRegionEurope,
			"US": uiIDMenuRegionUSA,
			"CN": uiIDMenuRegionChina,
			"XX": uiIDMenuRegionOther,
		}
		for code, id := range regionIDs {
			uiCheckMenuItemFn.Call(aw.hMenuRegion, id, checkFlag(code == s.Region))
		}
	}

	aw.syncUpdateMenuItem()
}

// syncUpdateMenuItem refreshes only the "Update" menu item's enabled/greyed
// state from UpdateReadyFn. Split out of syncMenuState so it can be called
// on its own -- see RefreshUpdateState -- without re-touching every other
// checkbox at a moment that has nothing to do with settings.
func (aw *AppWindow) syncUpdateMenuItem() {
	if aw.hMenuMain == 0 {
		return
	}
	enableFlag := uintptr(uiMF_BYCOMMAND | uiMF_GRAYED)
	if aw.UpdateReadyFn != nil && aw.UpdateReadyFn() {
		enableFlag = uiMF_BYCOMMAND | uiMF_ENABLED
	}
	uiEnableMenuItemFn.Call(aw.hMenuMain, uiIDMenuUpdate, enableFlag)
}

// RefreshUpdateState re-checks UpdateReadyFn and updates the "Update" menu
// item accordingly. Call this whenever update-readiness actually changes
// (see tray.go's TrayApp.OnUpdateReadyChanged) rather than relying on it
// only being caught incidentally the next time a settings change happens
// to call syncMenuState -- before 2026-08-16 that was the only trigger, so
// the app window's menu could sit stale (showing no update available) for
// the life of the process even after the tray's own item had already gone
// live. Safe to call from any goroutine, same as the menu-state calls
// syncMenuState already makes directly.
func (aw *AppWindow) RefreshUpdateState() {
	aw.syncUpdateMenuItem()
}

// toggleMenuSetting flips one boolean field of AppSettings (chosen by mutate)
// and pushes the result through the same SetSettingsFn path the Settings
// tab's native checkboxes use, so there is exactly one source of truth.
func (aw *AppWindow) toggleMenuSetting(mutate func(*AppSettings)) {
	if aw.GetSettingsFn == nil || aw.SetSettingsFn == nil {
		return
	}
	s := aw.GetSettingsFn()
	mutate(&s)
	aw.SetSettingsFn(s)
	s = aw.GetSettingsFn() // tray may apply mutual exclusion; re-read canonical state
	aw.mu.Lock()
	aw.settings = s
	aw.mu.Unlock()
	aw.applySettingsToControls(s)
}

// setMenuRegion is toggleMenuSetting's region-specific counterpart (region is
// a multi-value selection, not a boolean).
func (aw *AppWindow) setMenuRegion(code string) {
	aw.toggleMenuSetting(func(s *AppSettings) { s.Region = code })
}

// â”€â”€ Control creation â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func (aw *AppWindow) createControls(hInst uintptr) {
	mk := func(class string, style, x, y, w, h int, id uintptr) uintptr {
		cn, _ := windows.UTF16PtrFromString(class)
		hwnd, _, _ := uiCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(cn)),
			0,
			uiWS_CHILD|uiWS_VISIBLE|uintptr(style),
			uintptr(x), uintptr(y), uintptr(w), uintptr(h),
			aw.hwnd, id, hInst, 0)
		uiSendMessageFn.Call(hwnd, 0x0030 /*WM_SETFONT*/, aw.hfBody, 1)
		return hwnd
	}

	// â”€â”€ Tunnel tab â”€â”€
	// Buttons sit below the cat image (catY + catSize + ~168 px for status/elapsed row).
	aw.hConnect = mk("BUTTON", uiBSOWNERDRAW, 140, uiContY+256, 200, 40, uiIDConnect)
	aw.hDisconnect = mk("BUTTON", uiBSOWNERDRAW, 140, uiContY+256, 200, 40, uiIDDisconnect)

	// Uplink/downlink byte counter -- bottom-right, just above the full-width
	// tunnel status bar (which is barH=36px tall at the very bottom, see
	// paintTunnel). Right-aligned STATIC text so it hugs the corner. Starts
	// hidden -- there's nothing meaningful to show before a tunnel exists;
	// syncByteCounterVisibility shows/hides it in step with connect state.
	aw.hBytesLabel = mk("STATIC", uiSSRIGHT, uiW-260-12, uiH-36-40, 260, 18, uiIDByteCounter)
	uiShowWindowFn.Call(aw.hBytesLabel, uiSW_HIDE)

	// â”€â”€ Settings tab â”€â”€
	// All settings content (labels, checkboxes) is fully custom-painted in
	// paintSettings.  Only the region combobox is a native control.
	const sx = 30

	// Region combobox (sits below the painted "YOUR LOCATION" section header).
	aw.hRegion = mk("COMBOBOX",
		uiCBSDROPDOWNLIST|uiCBSHASSTRINGS|uiWS_TABSTOP,
		sx, uiContY+58, 200, 200, uiIDRegion)
	for _, entry := range []string{T("region_auto_detect"), T("region_russia"), T("region_europe"), T("region_usa"), T("region_china"), T("region_other")} {
		ep, _ := windows.UTF16PtrFromString(entry)
		uiSendMessageFn.Call(aw.hRegion, uiCB_ADDSTRING, 0, uintptr(unsafe.Pointer(ep)))
	}
	uiSendMessageFn.Call(aw.hRegion, uiCB_SETCURSEL, 0, 0) // default = Auto

	// Native checkboxes for connection options.
	setText := func(hwnd uintptr, s string) {
		p, _ := windows.UTF16PtrFromString(s)
		uiSendMessageFn.Call(hwnd, 0x000C /*WM_SETTEXT*/, 0, uintptr(unsafe.Pointer(p)))
	}
	const s1 = uiContY + 94
	aw.hDoH = mk("BUTTON", uiBSAUTOCHECKBOX|uiWS_TABSTOP, sx, s1+22, 220, 22, uiIDDoH)
	setText(aw.hDoH, T("checkbox_doh"))
	aw.hBlockQUIC = mk("BUTTON", uiBSAUTOCHECKBOX|uiWS_TABSTOP, sx, s1+52, 220, 22, uiIDBlockQUIC)
	setText(aw.hBlockQUIC, T("tray_block_quic"))
	// Account-level (server-side) preference, not part of AppSettings -- see
	// LogUploadToggleFn's doc comment. Starts unchecked; SetLogUploadCheck
	// is called once the real value is fetched after connecting.
	aw.hLogUpload = mk("BUTTON", uiBSAUTOCHECKBOX|uiWS_TABSTOP, sx, s1+82, 320, 22, uiIDLogUpload)
	setText(aw.hLogUpload, T("checkbox_log_upload"))
}

// SetLogUploadCheck sets the log-upload checkbox's displayed state without
// firing LogUploadToggleFn -- for pushing the real value fetched from the
// arbiter (core.LogUploader.GetPref) onto the control, or reverting an
// optimistic flip the arbiter never accepted, as opposed to the user
// actually clicking it. Safe to call before the window exists (no-op:
// hLogUpload is 0 until createControls runs) and from any goroutine: it
// issues a plain Win32 SendMessage (BM_SETCHECK), which the OS marshals to
// the control's owning (runLoop) thread when called from another one --
// unlike the pending*/PostMessage-based setters elsewhere on AppWindow,
// this doesn't need to stash state for the runLoop to apply later, since
// BM_SETCHECK is a self-contained message with no shared state to race on.
func (aw *AppWindow) SetLogUploadCheck(checked bool) {
	if aw.hLogUpload == 0 {
		return
	}
	val := uintptr(uiBST_UNCHECKED)
	if checked {
		val = uintptr(uiBST_CHECKED)
	}
	uiSendMessageFn.Call(aw.hLogUpload, uiBM_SETCHECK, val, 0)
}

func (aw *AppWindow) applySettingsToControls(s AppSettings) {
	setCheck := func(hwnd uintptr, v bool) {
		val := uintptr(uiBST_UNCHECKED)
		if v {
			val = uintptr(uiBST_CHECKED)
		}
		uiSendMessageFn.Call(hwnd, uiBM_SETCHECK, val, 0)
	}
	setCheck(aw.hDoH, s.DoH)
	setCheck(aw.hBlockQUIC, s.BlockQUIC)
	uiSendMessageFn.Call(aw.hRegion, uiCB_SETCURSEL, uintptr(regionToIndex(s.Region)), 0)
	aw.syncMenuState(s)
}

// RefreshSettingsState re-reads s onto the Settings tab's checkboxes and the
// window's own native menu bar. Exported so TrayApp can push a settings
// change made directly on the tray icon's context menu back to the window --
// see TrayApp.SetSettingsChangeCallback's doc comment for the gap this
// closes (that direction had no path back to the window at all before).
func (aw *AppWindow) RefreshSettingsState(s AppSettings) {
	aw.applySettingsToControls(s)
}

// syncBlockQUICLock greys the "Disable QUIC" checkbox (Settings tab) and
// menu item while WildCat is forcing QUIC blocked (see
// TrayApp.IsWildcatQUICLocked) -- the checkbox's own checked state (and the
// underlying preference behind it) is left untouched, so re-enabling it
// once WildCat releases the lock shows exactly the state from before.
func (aw *AppWindow) syncBlockQUICLock(locked bool) {
	if aw.hBlockQUIC != 0 {
		enable := uintptr(1)
		if locked {
			enable = 0
		}
		uiEnableWindowFn.Call(aw.hBlockQUIC, enable)
	}
	if aw.hMenuMain != 0 {
		flag := uintptr(uiMF_BYCOMMAND | uiMF_ENABLED)
		if locked {
			flag = uiMF_BYCOMMAND | uiMF_GRAYED
		}
		uiEnableMenuItemFn.Call(aw.hMenuMain, uiIDMenuBlockQUIC, flag)
	}
}

// syncByteCounterVisibility shows the uplink/downlink STATIC control while
// connected and hides it otherwise (per-session counter, nothing to display
// -- and nothing meaningful -- before a tunnel exists). Called from the
// uiWM_UpdateStatus handler, so it always runs on the runLoop thread.
func (aw *AppWindow) syncByteCounterVisibility(connected bool) {
	if aw.hBytesLabel == 0 {
		return
	}
	visible := uintptr(uiSW_HIDE)
	if connected {
		visible = uiSW_SHOW
	}
	uiShowWindowFn.Call(aw.hBytesLabel, visible)
}

// â”€â”€ Public API â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func (aw *AppWindow) waitReady() bool {
	if aw.readyCh == nil {
		return false
	}
	<-aw.readyCh
	return aw.hwnd != 0
}

// Show makes the window visible.
func (aw *AppWindow) Show() {
	if !aw.waitReady() {
		return
	}
	uiPostMessageFn.Call(aw.hwnd, uiWM_APP+2, 0, 0) // handled in WndProc
	// Direct show is safe here â€” PostMessage ensures thread safety.
	uiShowWindowFn.Call(aw.hwnd, uiSW_RESTORE)
	uiSetForegroundFn.Call(aw.hwnd)
}

// Hide hides the window.
func (aw *AppWindow) Hide() {
	if aw.hwnd != 0 {
		uiShowWindowFn.Call(aw.hwnd, uiSW_HIDE)
	}
}

// UpdateStatus pushes a status refresh request to the window thread.
func (aw *AppWindow) UpdateStatus(s AppStatus) {
	select {
	case <-aw.readyCh:
	default:
		logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
			logevent.Str(logevent.AttrStage, "status_dropped"),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("connected=%v connecting=%v disconnecting=%v", s.Connected, s.Connecting, s.Disconnecting)))
		return
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinUiwindowLifecycle,
		logevent.Str(logevent.AttrStage, "status"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("UpdateStatus connected=%v connecting=%v disconnecting=%v mode=%q hwnd=0x%x", s.Connected, s.Connecting, s.Disconnecting, s.Mode, aw.hwnd)))
	aw.mu.Lock()
	aw.status = s
	aw.mu.Unlock()
	if aw.hwnd != 0 {
		uiPostMessageFn.Call(aw.hwnd, uiWM_UpdateStatus, 0, 0)
	}
}

// UpdateBytes pushes an updated cumulative uplink/downlink byte count
// (core.TotalBytes()) to the window thread for display in the byte-counter
// STATIC control. Called about once per second while connected -- see
// TrayApp's tickElapsed in tray.go and its wiring in main_windows.go.
// Cheap by design: unlike UpdateStatus this does not invalidate/repaint the
// whole window, only re-sets one control's text (see uiWM_UpdateBytes).
func (aw *AppWindow) UpdateBytes(sent, recv int64) {
	select {
	case <-aw.readyCh:
	default:
		return
	}
	aw.mu.Lock()
	aw.bytesSent, aw.bytesRecv = sent, recv
	aw.mu.Unlock()
	if aw.hwnd != 0 {
		uiPostMessageFn.Call(aw.hwnd, uiWM_UpdateBytes, 0, 0)
	}
}

// Destroy shuts down the window.
func (aw *AppWindow) Destroy() {
	select {
	case <-aw.readyCh:
		if aw.hwnd != 0 {
			uiDestroyWindowFn.Call(aw.hwnd)
		}
	default:
	}
}

// â”€â”€ Helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func mustUTF16Ptr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

func checkboxChecked(hwnd uintptr) bool {
	r, _, _ := uiSendMessageFn.Call(hwnd, uiBM_GETCHECK, 0, 0)
	return r == uiBST_CHECKED
}

func comboGetSel(hwnd uintptr) int {
	r, _, _ := uiSendMessageFn.Call(hwnd, uiCB_GETCURSEL, 0, 0)
	return int(int32(r))
}

// regionToIndex maps a region code to the combobox index (0..5).
func regionToIndex(code string) int {
	switch code {
	case "RU":
		return 1
	case "EU":
		return 2
	case "US":
		return 3
	case "CN":
		return 4
	case "XX":
		return 5
	default:
		return 0 // Auto
	}
}

// regionFromIndex maps a combobox index back to a region code.
func regionFromIndex(i int) string {
	switch i {
	case 1:
		return "RU"
	case 2:
		return "EU"
	case 3:
		return "US"
	case 4:
		return "CN"
	case 5:
		return "XX"
	default:
		return ""
	}
}

// getWindowRectFn wraps GetWindowRect using the proc declared in window.go.
func getWindowRectOf(hwnd uintptr, rc *uiRECT) {
	winGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(rc)))
}
