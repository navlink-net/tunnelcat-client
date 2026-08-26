// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"tunnel_cat/binlog"
	"tunnel_cat/logevent"
	"tunnel_cat/snc/core"
)

//go:embed assets/snc_idle.png
var iconIdlePNG []byte

//go:embed assets/snc_connecting.png
var iconConnectingPNG []byte

//go:embed assets/snc_connected.png
var iconConnectedPNG []byte

//go:embed assets/snc_error.png
var iconErrorPNG []byte

//go:embed assets/snc_wildcat.png
var iconWildcatPNG []byte

// Cached ICO bytes for each state (built once at package init).
var (
	icoIdle       []byte
	icoConnecting []byte
	icoConnected  []byte
	icoWildcat    []byte
	icoError      []byte
	icoQuitDim    []byte // dark-gray â€” alternates with icoIdle to create a blinking quit indicator
)

func init() {
	icoIdle = pngToICO(iconIdlePNG)
	icoConnecting = pngToICO(iconConnectingPNG)
	icoConnected = pngToICO(iconConnectedPNG)
	icoWildcat = pngToICO(iconWildcatPNG)
	icoError = pngToICO(iconErrorPNG)
	icoQuitDim = coloredICO(48, 48, 48)
}

const (
	connectDeadline    = 90 * time.Second // max time for onConnect before error state
	disconnectDeadline = 30 * time.Second // max time for onDisconnect before forced UI reset
)

// TrayApp manages the ShortNerdCat system-tray icon and menu.
//
// Lifecycle:
//
//	tray := NewTrayApp(...)
//	tray.Run() // blocks on the main goroutine until Quit is clicked
type TrayApp struct {
	mu            sync.Mutex
	loggedIn      bool
	autoConnect   bool // connect automatically on startup
	connected     bool
	connecting    bool   // true while a connect attempt is in progress
	disconnecting bool   // true while onDisconnect callback is running
	errorMsg      string // non-empty only when setTrayIcon last set icoError; the actual failure reason
	connectedAt   time.Time
	stopTick      chan struct{}
	version       string

	onStatusChange func() // called on every status transition; may be nil

	// onSettingsChange is called whenever a settings toggle (DoH, BlockQUIC,
	// WildCat, region) is changed FROM the tray's own context menu. The
	// window's Settings tab and native menu bar only ever learn about a
	// settings change through the reverse direction (AppWindow.saveSettings/
	// toggleMenuSetting -> SetSettingsFn -> ApplyWindowSettings) -- a change
	// made here, directly on the tray icon's menu, had no path back to the
	// window at all before this, so its checkboxes/menu items could sit
	// showing stale state indefinitely. Confirmed via a real support report,
	// 2026-08-25: "DoH checked in the tray context menu, but the checkbox in
	// the app's own settings doesn't show it." See SetSettingsChangeCallback.
	onSettingsChange func() // may be nil

	// onBytesTick is called about once/second while connected, in step with
	// tickElapsed's own ticker -- see SetBytesTickCallback. Kept separate
	// from onStatusChange because that one drives a full window repaint
	// (UpdateStatus/InvalidateRect) on every call, which tickElapsed
	// deliberately avoids doing every second (see its own comment below).
	onBytesTick func() // may be nil

	// reconnectCh receives a signal when the watchdog or a power-resume event
	// wants the app to perform an automatic disconnect+reconnect cycle.
	reconnectCh chan struct{}

	// loginCh / logoutCh / quitCh are triggered by the app window's own native
	// menu bar so its items behave identically to the tray menu items.
	loginCh  chan struct{}
	logoutCh chan struct{}
	quitCh   chan struct{}

	// connectCh / disconnectCh are triggered by the app window's Connect and
	// Disconnect buttons so they behave identically to the tray menu items.
	connectCh    chan struct{}
	disconnectCh chan struct{}

	// updateReady mirrors mUpdate's visibility so the app window's menu can
	// enable/gray its own "Update available" item without reaching into
	// systray internals. Protected by mu.
	updateReady bool

	// OnUpdateReadyChanged is called (nil-safe) every time updateReady
	// changes -- both when a fresh download makes an update available and
	// when TriggerUpdateInstall discovers there was nothing to apply after
	// all and resets the flag. Without this, the app window's own "Update"
	// menu item only ever re-synced from applySettingsToControls (i.e.
	// whenever a setting happened to change) and could stay stuck greyed
	// out indefinitely after the tray's own item was already live -- found
	// 2026-08-16, the two surfaces drifted for days on end because nothing
	// tied them together. Set from outside (main_windows.go), since
	// TrayApp doesn't otherwise know about AppWindow.
	OnUpdateReadyChanged func()

	// quitting is set when the user clicks Quit.  doReconnect checks it so that
	// a background reconnect goroutine does not race with the shutdown path.
	quitting bool // protected by mu

	// userDisconnected is set when the user explicitly clicks Disconnect and
	// cleared when doConnect runs.  TriggerReconnect is a no-op while this flag
	// is set, preventing data-fail hooks that fire during TUN teardown from
	// re-connecting without the user's request.
	userDisconnected bool // protected by mu

	// retryMu guards retryStop. retryStop is non-nil when a background
	// connect-retry goroutine is running; closing it cancels the goroutine.
	retryMu   sync.Mutex
	retryStop chan struct{}

	// disconnectPending is set when the user clicks Disconnect while a connect
	// attempt is still in progress. doConnect checks it on completion and tears
	// the tunnel down immediately if it came up while the user was waiting.
	disconnectPending     bool // protected by mu
	reconnectAfterPending bool // set alongside disconnectPending when disconnect is part of an auto-reconnect

	dohEnabled       bool   // persisted setting â€” used to initialise the checkbox on first draw
	blockQUICEnabled bool   // persisted setting â€” block QUIC (UDP:443) to improve video on bad connections
	preferredRegion  string // "" = Auto; "RU"/"EU"/"US"/"CN"/"XX" for explicit region

	// lastWakeReconnect debounces onPowerWake: rapid/flaky OS sleep-wake
	// cycling can fire several PBT_APMRESUMEAUTOMATIC events within seconds of
	// each other, and each one used to trigger its own full reconnect.
	lastWakeReconnect time.Time // protected by mu

	// healthCheck, if set, is consulted by onPowerWake before doing a full
	// reconnect: if the tunnel still answers, the wake event is treated as
	// spurious and no reconnect happens. nil means always reconnect on wake
	// (previous behavior). Set via SetHealthCheck; not wired through the
	// constructor since it depends on state (the router) built after NewTrayApp.
	healthCheck func() bool // protected by mu

	// Callbacks â€” all may be nil-safe.
	onLogin              func() error
	onLogout             func()
	onConnect            func(autoReconnect bool) error
	onDisconnect         func(autoReconnect bool)
	onDNSOverHTTPSChange func(bool)   // called when user toggles DoH checkbox; may be nil
	onBlockQUICChange    func(bool)   // called when user toggles Disable QUIC checkbox; may be nil
	onRegionChange       func(string) // called when user picks a region; receives region code; may be nil

	mLogin         *systray.MenuItem
	mLogout        *systray.MenuItem
	mConnect       *systray.MenuItem
	mDisconnect    *systray.MenuItem
	mDNSOverHTTPS  *systray.MenuItem // DoH opt-in checkbox; shown only when logged in
	mBlockQUIC     *systray.MenuItem // Disable QUIC checkbox; shown only when logged in
	mRegion        *systray.MenuItem // Region submenu parent; shown only when logged in
	mRegionAuto    *systray.MenuItem // radio: Auto (detect automatically)
	mRegionRussia  *systray.MenuItem // radio: Russia
	mRegionEurope  *systray.MenuItem // radio: Europe
	mRegionUSA     *systray.MenuItem // radio: USA
	mRegionChina   *systray.MenuItem // radio: China
	mRegionOther   *systray.MenuItem // radio: Other
	mUpdate        *systray.MenuItem // shown only when a newer binary has been downloaded
	updateNotifyCh chan string       // receives new version string when update is ready

	wildcatEnabled  bool
	mWildcat        *systray.MenuItem
	onWildcatChange func(bool)

	onOpenWindow func() // opens/shows the main application window; may be nil
}

// NewTrayApp creates a TrayApp.
//
//   - initiallyLoggedIn: true if the app already has valid auth at startup.
//   - dohEnabled: initial state of the "DNS over HTTPS" checkbox (loaded from settings).
//   - blockQUICEnabled: initial state of the "Disable QUIC" checkbox (loaded from settings).
//   - onLogin: called when the user clicks Login; should show the key dialog and
//     authenticate; returns nil on success.
//   - onLogout: called when the user clicks Logout.
//   - onConnect: called when the user clicks Connect; returns nil on success.
//   - onDisconnect: called on Disconnect / Quit.
//   - onDNSOverHTTPSChange: called when user toggles the DoH checkbox; may be nil.
//   - onBlockQUICChange: called when user toggles the Disable QUIC checkbox; may be nil.
//   - preferredRegion: initial region selection ("" = Auto; "RU"/"EU"/"US"/"CN"/"XX").
//   - onRegionChange: called when user picks a region; receives the new region code; may be nil.
func NewTrayApp(
	version string,
	initiallyLoggedIn bool,
	autoConnect bool,
	dohEnabled bool,
	blockQUICEnabled bool,
	wildcatEnabled bool,
	preferredRegion string,
	onLogin func() error,
	onLogout func(),
	onConnect func(autoReconnect bool) error,
	onDisconnect func(autoReconnect bool),
	onDNSOverHTTPSChange func(bool),
	onBlockQUICChange func(bool),
	onWildcatChange func(bool),
	onRegionChange func(string),
) *TrayApp {
	return &TrayApp{
		version:              version,
		loggedIn:             initiallyLoggedIn,
		autoConnect:          autoConnect,
		dohEnabled:           dohEnabled,
		blockQUICEnabled:     blockQUICEnabled,
		wildcatEnabled:       wildcatEnabled,
		preferredRegion:      preferredRegion,
		onLogin:              onLogin,
		onLogout:             onLogout,
		onConnect:            onConnect,
		onDisconnect:         onDisconnect,
		onDNSOverHTTPSChange: onDNSOverHTTPSChange,
		onBlockQUICChange:    onBlockQUICChange,
		onWildcatChange:      onWildcatChange,
		onRegionChange:       onRegionChange,
		reconnectCh:          make(chan struct{}, 1),
		updateNotifyCh:       make(chan string, 1),
		connectCh:            make(chan struct{}, 1),
		disconnectCh:         make(chan struct{}, 1),
		loginCh:              make(chan struct{}, 1),
		logoutCh:             make(chan struct{}, 1),
		quitCh:               make(chan struct{}, 1),
	}
}

// NotifyUpdateReady shows the "Update to <version>" tray menu item.
// Safe to call from any goroutine; non-blocking.
func (a *TrayApp) NotifyUpdateReady(version string) {
	select {
	case a.updateNotifyCh <- version:
	default:
	}
}

// IsDNSOverHTTPSEnabled reports whether the user has enabled the DoH fallback option.
func (a *TrayApp) IsDNSOverHTTPSEnabled() bool {
	if a.mDNSOverHTTPS == nil {
		return false
	}
	return a.mDNSOverHTTPS.Checked()
}

func (a *TrayApp) connectedIcon() []byte {
	if a.IsWildcatEnabled() {
		return icoWildcat
	}
	return icoConnected
}

// setTrayIcon sets the systray icon and records the failure reason (if any),
// so GetAppStatus can expose the same information to the main window's
// status bar (see uiwindow.go's paintTunnel) instead of a bare "Error" with
// no detail -- without duplicating the failure-detection logic already
// embedded in each call site below. errMsg == "" means "not an error".
func (a *TrayApp) setTrayIcon(icon []byte, errMsg string) {
	a.mu.Lock()
	a.errorMsg = errMsg
	a.mu.Unlock()
	systray.SetIcon(icon)
}

// IsDisconnectPending reports whether a disconnect was requested while a connect
// was already in progress. Used by the connect goroutine to abort mid-flight
// work (e.g. the WildCat relay pool build) without waiting for the full connect to finish.
func (a *TrayApp) IsDisconnectPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disconnectPending
}

// IsBlockQUICEnabled reports whether the user has enabled the Disable QUIC option.
func (a *TrayApp) IsBlockQUICEnabled() bool {
	if a.mBlockQUIC == nil {
		return a.blockQUICEnabled
	}
	return a.mBlockQUIC.Checked()
}

// IsWildcatEnabled reports whether the user has enabled WildCat mode.
func (a *TrayApp) IsWildcatEnabled() bool {
	if a.mWildcat == nil {
		return a.wildcatEnabled
	}
	return a.mWildcat.Checked()
}

// IsWildcatQUICLocked reports whether WildCat mode is currently forcing QUIC
// blocked, for the active/pending session. This is true only while a
// WildCat connection is actually connecting or connected -- WildCat's
// effect on the tunnel is decided once, at connect time (see doConnect),
// so merely checking the WildCat checkbox while idle forces nothing yet.
// The underlying Disable QUIC preference is never touched by this -- callers
// hide/grey the checkbox instead, so un-hiding it later shows exactly the
// state it was in before WildCat took over.
func (a *TrayApp) IsWildcatQUICLocked() bool {
	a.mu.Lock()
	active := a.connected || a.connecting
	a.mu.Unlock()
	return active && a.IsWildcatEnabled()
}

// refreshBlockQUICVisibility hides the "Disable QUIC" checkbox while
// IsWildcatQUICLocked, and shows it again otherwise -- see that method's
// doc comment for why this is safe to call unconditionally on every status
// or WildCat-setting change.
func (a *TrayApp) refreshBlockQUICVisibility() {
	if a.mBlockQUIC == nil {
		return
	}
	a.mu.Lock()
	loggedIn := a.loggedIn
	a.mu.Unlock()
	if !loggedIn {
		return // login/logout flow already hides/shows it directly
	}
	if a.IsWildcatQUICLocked() {
		a.mBlockQUIC.Hide()
	} else {
		a.mBlockQUIC.Show()
	}
}

// GetPreferredRegion returns the user's current explicit region selection.
// Returns "" when set to Auto (automatic detection).
func (a *TrayApp) GetPreferredRegion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.preferredRegion
}

// setRegion selects a region radio button and notifies the callback.
func (a *TrayApp) setRegion(code string, selected *systray.MenuItem) {
	items := []*systray.MenuItem{a.mRegionAuto, a.mRegionRussia, a.mRegionEurope, a.mRegionUSA, a.mRegionChina, a.mRegionOther}
	for _, it := range items {
		if it == selected {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
	a.mu.Lock()
	a.preferredRegion = code
	a.mu.Unlock()
	a.mRegion.SetTitle(T("tray_region_prefix") + regionName(code))
	if a.onRegionChange != nil {
		a.onRegionChange(code)
	}
	a.notifySettingsChange()
}

// regionName maps an ISO region code to a display name.
func regionName(code string) string {
	switch code {
	case "RU":
		return T("region_russia")
	case "EU":
		return T("region_europe")
	case "US":
		return T("region_usa")
	case "CN":
		return T("region_china")
	case "XX":
		return T("region_other")
	default:
		return T("region_auto")
	}
}

// Run initialises the system tray and blocks until the user clicks Quit.
// Must be called from the main goroutine.
func (a *TrayApp) Run() {
	systray.Run(a.onReady, func() {})
}

func (a *TrayApp) onReady() {
	a.setTrayIcon(icoIdle, "")
	systray.SetTooltip(T("app_title"))

	mOpen := systray.AddMenuItem(T("tray_open"), T("tray_open_tip"))
	a.mLogin = systray.AddMenuItem(T("login_button"), T("tray_login_tip"))
	a.mLogout = systray.AddMenuItem(T("tray_logout"), T("tray_logout_tip"))
	systray.AddSeparator()
	a.mConnect = systray.AddMenuItem(T("tray_connect"), T("tray_connect_tip"))
	a.mDisconnect = systray.AddMenuItem(T("tray_disconnect"), T("tray_disconnect_tip"))
	systray.AddSeparator()
	a.mDNSOverHTTPS = systray.AddMenuItemCheckbox(T("tray_doh"), T("tray_doh_tip"), a.dohEnabled)
	a.mBlockQUIC = systray.AddMenuItemCheckbox(T("tray_block_quic"), T("tray_block_quic_tip"), a.blockQUICEnabled)
	a.mWildcat = systray.AddMenuItemCheckbox(T("tray_wildcat"), T("tray_wildcat_tip"), a.wildcatEnabled)
	a.mRegion = systray.AddMenuItem(T("tray_region_prefix")+regionName(a.preferredRegion), T("tray_region_tip"))
	a.mRegionAuto = a.mRegion.AddSubMenuItemCheckbox(T("region_auto"), T("tray_region_auto_tip"), a.preferredRegion == "")
	a.mRegionRussia = a.mRegion.AddSubMenuItemCheckbox(T("region_russia"), T("region_russia"), a.preferredRegion == "RU")
	a.mRegionEurope = a.mRegion.AddSubMenuItemCheckbox(T("region_europe"), T("region_europe"), a.preferredRegion == "EU")
	a.mRegionUSA = a.mRegion.AddSubMenuItemCheckbox(T("region_usa"), T("region_usa_full"), a.preferredRegion == "US")
	a.mRegionChina = a.mRegion.AddSubMenuItemCheckbox(T("region_china"), T("region_china"), a.preferredRegion == "CN")
	a.mRegionOther = a.mRegion.AddSubMenuItemCheckbox(T("region_other"), T("tray_region_other_tip"), a.preferredRegion == "XX")
	systray.AddSeparator()
	mAbout := systray.AddMenuItem(T("tray_about"), T("tray_about_tip"))
	a.mUpdate = systray.AddMenuItem(T("tray_update"), T("tray_update_tip"))
	a.mUpdate.Hide()
	mQuit := systray.AddMenuItem(T("tray_quit"), T("tray_quit_tip"))

	// Initial visibility depends on login state.
	if a.loggedIn {
		a.mLogin.Hide()
		a.mDisconnect.Hide()
		// DoH checkbox is visible only when logged in; start unchecked.
	} else {
		a.mLogout.Hide()
		a.mConnect.Hide()
		a.mDisconnect.Hide()
		a.mDNSOverHTTPS.Hide()
		a.mBlockQUIC.Hide()
		a.mWildcat.Hide()
		a.mRegion.Hide()
		// "Not logged in yet" is a normal startup state, not a failure -- keep
		// the tray's error-look icon (pre-existing behavior) but don't surface
		// it as a red "Error" bar in the main window (that's reserved for
		// actual login/connect failures below).
		a.setTrayIcon(icoError, "")
		systray.SetTooltip(T("tooltip_not_logged_in"))
	}

	// If not logged in at startup, auto-show the login dialog once the tray
	// has settled (small delay so the icon is visible first).
	// If already logged in with saved credentials, connect automatically.
	if !a.loggedIn {
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.doLogin()
		}()
	} else if a.autoConnect {
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.doConnect(false)
		}()
	}

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "open_window"))
				if a.onOpenWindow != nil {
					go a.onOpenWindow()
				}
			case <-a.mLogin.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "login"))
				a.doLogin()
			case <-a.mLogout.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "logout"))
				go a.doLogout()
			case <-a.mConnect.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "connect"))
				// Hide immediately to block double-clicks before the goroutine starts.
				a.mConnect.Hide()
				go a.doConnect(false)
			case <-a.mDisconnect.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "disconnect"))
				a.mDisconnect.Hide()
				go a.doDisconnect(false)
			case <-a.mDNSOverHTTPS.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction,
					logevent.Str(logevent.AttrAction, "doh_toggle"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("was_checked=%v", a.mDNSOverHTTPS.Checked())))
				if a.mDNSOverHTTPS.Checked() {
					a.mDNSOverHTTPS.Uncheck()
				} else {
					a.mDNSOverHTTPS.Check()
				}
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction,
					logevent.Str(logevent.AttrAction, "doh_now"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("%v", a.mDNSOverHTTPS.Checked())))
				if a.onDNSOverHTTPSChange != nil {
					a.onDNSOverHTTPSChange(a.mDNSOverHTTPS.Checked())
				}
				a.notifySettingsChange()
			case <-a.mBlockQUIC.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction,
					logevent.Str(logevent.AttrAction, "block_quic_toggle"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("was_checked=%v", a.mBlockQUIC.Checked())))
				if a.mBlockQUIC.Checked() {
					a.mBlockQUIC.Uncheck()
				} else {
					a.mBlockQUIC.Check()
				}
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction,
					logevent.Str(logevent.AttrAction, "block_quic_now"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("%v", a.mBlockQUIC.Checked())))
				if a.onBlockQUICChange != nil {
					a.onBlockQUICChange(a.mBlockQUIC.Checked())
				}
				a.notifySettingsChange()
			case <-a.mWildcat.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction,
					logevent.Str(logevent.AttrAction, "wildcat_toggle"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("was_checked=%v", a.mWildcat.Checked())))
				if a.mWildcat.Checked() {
					a.mWildcat.Uncheck()
				} else {
					a.mWildcat.Check()
				}
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction,
					logevent.Str(logevent.AttrAction, "wildcat_now"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("%v", a.mWildcat.Checked())))
				a.refreshBlockQUICVisibility()
				if a.onWildcatChange != nil {
					a.onWildcatChange(a.mWildcat.Checked())
				}
				a.notifySettingsChange()
			case <-a.mRegionAuto.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "region_auto"))
				a.setRegion("", a.mRegionAuto)
			case <-a.mRegionRussia.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "region_ru"))
				a.setRegion("RU", a.mRegionRussia)
			case <-a.mRegionEurope.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "region_eu"))
				a.setRegion("EU", a.mRegionEurope)
			case <-a.mRegionUSA.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "region_us"))
				a.setRegion("US", a.mRegionUSA)
			case <-a.mRegionChina.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "region_cn"))
				a.setRegion("CN", a.mRegionChina)
			case <-a.mRegionOther.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "region_other"))
				a.setRegion("XX", a.mRegionOther)
			case <-a.connectCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "connect_ch"))
				a.mConnect.Hide()
				go a.doConnect(false)
			case <-a.disconnectCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "disconnect_ch"))
				a.mDisconnect.Hide()
				go a.doDisconnect(false)
			case <-a.reconnectCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "reconnect_ch"))
				// Run disconnect+reconnect in a background goroutine so the event
				// loop is never blocked (onDisconnect may take tens of seconds on
				// a post-hibernate network stack, and Quit must remain responsive).
				go a.doReconnect()
			case v := <-a.updateNotifyCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction,
					logevent.Str(logevent.AttrAction, "update_notification"),
					logevent.Str(logevent.AttrDetail, v))
				a.mUpdate.SetTitle("Update to " + v)
				a.mUpdate.Show()
				a.mu.Lock()
				a.updateReady = true
				a.mu.Unlock()
				if a.OnUpdateReadyChanged != nil {
					a.OnUpdateReadyChanged()
				}
			case <-a.mUpdate.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "update_click"))
				a.TriggerUpdateInstall()
			case <-a.loginCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "login_ch"))
				a.doLogin()
			case <-a.logoutCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "logout_ch"))
				go a.doLogout()
			case <-a.quitCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "quit_ch"))
				a.doQuit()
				return
			case <-mAbout.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "about"))
				go ShowSplash(a.version, 5*time.Second)
			case <-mQuit.ClickedCh:
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayMenuAction, logevent.Str(logevent.AttrAction, "quit"))
				a.doQuit()
				return
			}
		}
	}()

	// Subscribe to Windows power events so that wake from sleep/hibernate
	// triggers an immediate reconnect instead of waiting for the 10-second
	// connectivity probe timeout.
	WatchPowerEvents(a.onPowerWake)

	// Install left-click handler: single left-click on the tray icon opens the
	// main window instead of showing the menu (right-click still shows the menu).
	InstallTrayLClick(a.onOpenWindow)
}

func (a *TrayApp) doLogin() {
	if a.onLogin == nil {
		return
	}
	a.setTrayIcon(icoConnecting, "")
	systray.SetTooltip(T("tooltip_logging_in"))
	if err := a.onLogin(); err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect,
			logevent.Str(logevent.AttrStage, "login_failed"),
			logevent.Str(logevent.AttrErr, err.Error()))
		msg := core.FriendlyConnectError(err)
		a.setTrayIcon(icoError, T("err_login_failed")+msg)
		systray.SetTooltip(T("tooltip_login_failed") + msg)
		return
	}
	a.mu.Lock()
	a.loggedIn = true
	a.mu.Unlock()
	a.setTrayIcon(icoIdle, "")
	systray.SetTooltip(T("app_title"))
	a.mLogin.Hide()
	a.mLogout.Show()
	a.mConnect.Show()
	a.mDNSOverHTTPS.Show()
	a.mBlockQUIC.Show()
	a.mWildcat.Show()
	a.mRegion.Show()
}

func (a *TrayApp) doLogout() {
	a.cancelRetry()
	a.doDisconnect(false)
	if a.onLogout != nil {
		a.onLogout()
	}
	a.mu.Lock()
	a.loggedIn = false
	a.mu.Unlock()
	a.setTrayIcon(icoIdle, "")
	systray.SetTooltip(T("tooltip_not_logged_in"))
	a.mConnect.Hide()
	a.mDisconnect.Hide()
	a.mLogout.Hide()
	a.mDNSOverHTTPS.Hide()
	a.mBlockQUIC.Hide()
	a.mRegion.Hide()
	a.mLogin.Show()
}

func (a *TrayApp) doConnect(autoReconnect bool) {
	a.cancelRetry() // cancel any pending retry before this attempt
	a.mu.Lock()
	if a.connecting {
		a.mu.Unlock()
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect,
			logevent.Str(logevent.AttrStage, "already_in_progress"),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("autoReconnect=%v", autoReconnect)))
		return
	}
	a.disconnectPending = false
	a.userDisconnected = false
	a.connecting = true
	a.mu.Unlock()
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect,
		logevent.Str(logevent.AttrStage, "starting"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("autoReconnect=%v", autoReconnect)))
	a.callStatusChange()
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect, logevent.Str(logevent.AttrStage, "ui_hiding_connect"))
	a.mConnect.Hide()
	a.mDisconnect.Show() // allow the user to cancel while connecting
	a.setTrayIcon(icoConnecting, "")
	systray.SetTooltip(T("tooltip_connecting"))

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect,
					logevent.Str(logevent.AttrStage, "panic"),
					logevent.Str(logevent.AttrDetail, fmt.Sprintf("%v\n%s", r, stack)))
				errCh <- fmt.Errorf("connect panicked: %v", r)
			}
		}()
		errCh <- a.onConnect(autoReconnect)
	}()
	var connectErr error
	select {
	case connectErr = <-errCh:
	case <-time.After(connectDeadline):
		connectErr = fmt.Errorf("connect sequence hung for %v", connectDeadline)
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect,
			logevent.Str(logevent.AttrStage, "error_entering"),
			logevent.Str(logevent.AttrErr, connectErr.Error()))
	}

	a.mu.Lock()
	pending := a.disconnectPending
	a.mu.Unlock()

	if connectErr != nil {
		a.mu.Lock()
		a.connecting = false
		shouldReconnect := a.reconnectAfterPending
		a.reconnectAfterPending = false
		a.mu.Unlock()
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect,
			logevent.Str(logevent.AttrStage, "failed"),
			logevent.Str(logevent.AttrErr, connectErr.Error()),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("pending=%v reconnect=%v", pending, shouldReconnect)))
		a.callStatusChange()
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect, logevent.Str(logevent.AttrStage, "ui_restore_connect"))
		a.mDisconnect.Hide()
		if pending && shouldReconnect {
			// Disconnect was part of an auto-reconnect cycle that fired while
			// warmup or another slow step was in progress. Re-queue the connect
			// so the cycle completes â€” same as the success+pending path below.
			a.setTrayIcon(icoIdle, "")
			systray.SetTooltip(T("app_title"))
			logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect, logevent.Str(logevent.AttrStage, "requeue_pending"))
			select {
			case a.reconnectCh <- struct{}{}:
			default:
			}
		} else if pending {
			a.setTrayIcon(icoIdle, "")
			systray.SetTooltip(T("app_title"))
			a.mConnect.Show()
		} else {
			msg := core.FriendlyConnectError(connectErr)
			a.setTrayIcon(icoError, T("err_connect_failed")+msg)
			systray.SetTooltip(T("tooltip_error_prefix") + msg)
			a.mConnect.Show()
			a.scheduleRetry() // keep trying in the background
		}
		return
	}

	a.mu.Lock()
	a.connected = true
	a.connecting = false
	a.connectedAt = time.Now()
	a.stopTick = make(chan struct{})
	stop := a.stopTick
	a.mu.Unlock()
	a.callStatusChange()

	if pending {
		// Tunnel came up but user already clicked Disconnect â€” tear it down immediately.
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect, logevent.Str(logevent.AttrStage, "disconnect_during_connect"))
		if a.onDisconnect != nil {
			a.onDisconnect(false)
		}
		a.mu.Lock()
		a.connected = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
		shouldReconnect := a.reconnectAfterPending
		a.reconnectAfterPending = false
		a.mu.Unlock()
		a.callStatusChange()
		if shouldReconnect {
			// The disconnect was part of an auto-reconnect cycle (e.g. mode toggle).
			// Queue a new connect so the cycle completes with updated settings.
			logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect, logevent.Str(logevent.AttrStage, "requeue_after_disconnect"))
			select {
			case a.reconnectCh <- struct{}{}:
			default:
			}
			return
		}
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect, logevent.Str(logevent.AttrStage, "ui_pending_restore"))
		a.mDisconnect.Hide()
		a.setTrayIcon(icoIdle, "")
		systray.SetTooltip(T("app_title"))
		a.mConnect.Show()
		return
	}

	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayConnect, logevent.Str(logevent.AttrStage, "success"))
	a.setTrayIcon(a.connectedIcon(), "")
	// mDisconnect is already visible
	//
	// The settings items (DoH/BlockQUIC/WildCat/Region), by contrast, are
	// NOT already visible here for an auto-reconnect: doDisconnect() hides
	// all of them unconditionally at the start of every disconnect, but only
	// re-Shows them in its own "restore idle" branch, which is explicitly
	// skipped for autoReconnect (see its own comment -- the icon is meant to
	// stay on "connecting" through the cycle instead of flashing idle). This
	// success path is the only other place that could have shown them back,
	// and never did -- confirmed via a real support report, 2026-08-25: DoH
	// and the region picker vanishing from the tray menu after the machine
	// unlocks (which fires an auto-reconnect), fixed only by restarting the
	// app. Login is guaranteed true to have reached this point at all, so
	// showing unconditionally here is safe.
	a.mDNSOverHTTPS.Show()
	a.mBlockQUIC.Show()
	a.mWildcat.Show()
	a.mRegion.Show()

	go a.tickElapsed(stop)
	core.TunnelMonitor.Reset()
	go a.runWatchdog(stop)
}

func (a *TrayApp) tickElapsed(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case t := <-ticker.C:
			a.mu.Lock()
			since := t.Sub(a.connectedAt)
			a.mu.Unlock()
			h := int(since.Hours())
			m := int(since.Minutes()) % 60
			s := int(since.Seconds()) % 60
			// Update tray tooltip only â€” the app window does not display elapsed
			// time, so triggering a full window repaint every second is unnecessary.
			systray.SetTooltip(fmt.Sprintf(T("tooltip_connected_fmt"), h, m, s))

			// Uplink/downlink byte counter -- cheap by design (see
			// SetBytesTickCallback), so unlike the elapsed-time display above
			// it's fine to refresh this every second.
			a.mu.Lock()
			tick := a.onBytesTick
			a.mu.Unlock()
			if tick != nil {
				tick()
			}
		}
	}
}

func (a *TrayApp) doDisconnect(autoReconnect bool) {
	a.cancelRetry() // user-initiated disconnect stops automatic retries
	if !autoReconnect {
		a.mu.Lock()
		a.userDisconnected = true
		a.mu.Unlock()
		select {
		case <-a.reconnectCh:
			logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect, logevent.Str(logevent.AttrStage, "drained_stale"))
		default:
		}
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect,
		logevent.Str(logevent.AttrStage, "starting"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("autoReconnect=%v", autoReconnect)))
	a.mu.Lock()
	if !a.connected {
		// A connect attempt may be in progress â€” flag it so doConnect tears
		// the tunnel down as soon as (and if) it comes up.
		a.disconnectPending = true
		if autoReconnect {
			a.reconnectAfterPending = true
		}
		a.mu.Unlock()
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect,
			logevent.Str(logevent.AttrStage, "not_connected_pending"),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("reconnect=%v", autoReconnect)))
		a.mDisconnect.Hide()
		systray.SetTooltip(T("tooltip_cancelling"))
		// Notify the window so it doesn't show a stale state.
		a.callStatusChange()
		return
	}
	a.connected = false
	a.connecting = false
	a.disconnecting = true
	loggedIn := a.loggedIn
	quitting := a.quitting
	if a.stopTick != nil {
		close(a.stopTick)
		a.stopTick = nil
	}
	a.mu.Unlock()
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect, logevent.Str(logevent.AttrStage, "connecting_to_disconnecting"))
	a.callStatusChange()

	if !quitting {
		// Disconnecting state: connecting icon, no action items available.
		// When quitting, the caller manages the icon (blinking gray).
		a.setTrayIcon(icoConnecting, "")
		systray.SetTooltip(T("tooltip_disconnecting"))
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect, logevent.Str(logevent.AttrStage, "hiding_actions"))
	a.mConnect.Hide()
	a.mDisconnect.Hide()
	a.mLogout.Hide()
	a.mDNSOverHTTPS.Hide()
	a.mBlockQUIC.Hide()
	a.mRegion.Hide()

	disconnDone := make(chan struct{})
	go func() {
		a.onDisconnect(autoReconnect)
		close(disconnDone)
	}()
	select {
	case <-disconnDone:
	case <-time.After(disconnectDeadline):
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect,
			logevent.Str(logevent.AttrStage, "hung_forcing_reset"),
			logevent.Str(logevent.AttrDetail, disconnectDeadline.String()))
	}

	a.mu.Lock()
	a.disconnecting = false
	a.mu.Unlock()
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect, logevent.Str(logevent.AttrStage, "done"))
	if !autoReconnect {
		// Only notify the UI on explicit user disconnect; auto-reconnect lets
		// doConnect fire the next callStatusChange so the window skips "Disconnected".
		a.callStatusChange()
	}

	if !quitting && !autoReconnect {
		// Restore idle (disconnected) state only for explicit user disconnects.
		// Auto-reconnects keep the connecting icon so the user never sees Disconnected.
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect,
			logevent.Str(logevent.AttrStage, "restoring_idle"),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("loggedIn=%v", loggedIn)))
		a.setTrayIcon(icoIdle, "")
		systray.SetTooltip(T("app_title"))
		if loggedIn {
			logevent.Emit(binlog.TagSystem, logevent.EventWinTrayDisconnect, logevent.Str(logevent.AttrStage, "showing_connect"))
			a.mConnect.Show()
			a.mLogout.Show()
			a.mDNSOverHTTPS.Show()
			a.mBlockQUIC.Show()
			a.mWildcat.Show()
			a.mRegion.Show()
		}
	}
}

// cancelRetry stops any pending background connect-retry goroutine.
func (a *TrayApp) cancelRetry() {
	a.retryMu.Lock()
	if a.retryStop != nil {
		close(a.retryStop)
		a.retryStop = nil
	}
	a.retryMu.Unlock()
}

// scheduleRetry starts a goroutine that sends to reconnectCh after delay,
// retrying with exponential backoff (30 s â†’ 60 s â†’ 120 s â€¦ cap 5 min).
func (a *TrayApp) scheduleRetry() {
	a.retryMu.Lock()
	stop := make(chan struct{})
	a.retryStop = stop
	a.retryMu.Unlock()

	go func() {
		const (
			minDelay = 30 * time.Second
			maxDelay = 5 * time.Minute
		)
		delay := minDelay
		for {
			select {
			case <-stop:
				return
			case <-time.After(delay):
			}
			// reconnectCh triggers doDisconnect (no-op if not connected) + doConnect.
			select {
			case a.reconnectCh <- struct{}{}:
			default:
			}
			if delay < maxDelay {
				delay *= 2
			}
		}
	}()
}

// ShowLoginError tears down the tunnel and shows a persistent red-icon
// "Login error" state.  The user must click Connect to retry.
// Called when the server explicitly and repeatedly rejects credentials
// (as opposed to a transient network failure, which triggers TriggerReconnect).
// ShowLoginError handles a definitive credential rejection (bad password,
// expired key, or -- since 2026-08-16 -- a bad AuthSig that gave the arbiter
// a chance to clear a possible key-issuance race and still failed, see
// isInvalidKeyRejection): the stored key/credentials are known-bad, so this
// shows a modal explaining why, then goes straight to re-login (doLogin --
// the exact same flow used at first launch when the tray finds no saved
// login, see NewTrayApp's startup block) instead of leaving a dead-end
// "Connect" button that would just retry the same broken key and loop back
// here every ~3-10 minutes. Explaining first matters: silently dropping the
// user into the login dialog with no context reads as the app forgetting
// their credentials, not as "your key stopped working."
func (a *TrayApp) ShowLoginError() {
	a.mu.Lock()
	wasConnected := a.connected
	if wasConnected {
		a.connected = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
	}
	a.mu.Unlock()

	if wasConnected {
		a.onDisconnect(false) // full user-visible disconnect â€” removes firewall rule
	}

	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState,
		logevent.Str(logevent.AttrStage, "login_error"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("wasConnected=%v", wasConnected)))
	a.setTrayIcon(icoError, T("err_login_rejected"))
	systray.SetTooltip(T("tooltip_login_error"))
	a.mDisconnect.Hide()
	a.mConnect.Hide()
	// Keep mLogin available in case the user cancels the dialog below --
	// doLogin() only re-hides it on a *successful* login, and its own
	// failure path doesn't touch menu visibility, so without this a
	// cancelled/failed re-login would leave no menu item able to retry.
	a.mLogin.Show()
	ShowError(T("err_key_rejected_msg"))
	a.mu.Lock()
	a.loggedIn = false
	a.mu.Unlock()
	// doLogin sets its own icon/tooltip states (connecting -> idle-with-
	// mConnect-shown on success, or error-with-message on failure/cancel) --
	// nothing further to set here.
	a.doLogin()
}

// TriggerReconnect queues an automatic disconnect+reconnect cycle.
// Safe to call from any goroutine; non-blocking.
// No-op when the user has explicitly disconnected to prevent data-fail hooks
// that fire during TUN teardown from reconnecting without user intent.
func (a *TrayApp) TriggerReconnect() {
	a.mu.Lock()
	ud := a.userDisconnected
	a.mu.Unlock()
	if ud {
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState, logevent.Str(logevent.AttrStage, "reconnect_suppressed"))
		return
	}
	select {
	case a.reconnectCh <- struct{}{}:
	default: // already queued
	}
}

// SetHealthCheck installs a short-timeout probe that onPowerWake consults
// before doing a full reconnect on wake from sleep. fn should return quickly
// (bounded timeout) and report whether the tunnel still looks usable. Not
// part of NewTrayApp's constructor because it depends on the Router, which is
// built by the caller after the TrayApp already exists.
func (a *TrayApp) SetHealthCheck(fn func() bool) {
	a.mu.Lock()
	a.healthCheck = fn
	a.mu.Unlock()
}

// wakeReconnectDebounce is the minimum gap between two wake-triggered
// reconnects. Flaky OS sleep/wake cycling can fire several power-resume
// events within seconds of each other; only the first of a burst should
// trigger any work.
const wakeReconnectDebounce = 10 * time.Second

// onPowerWake is wired to WatchPowerEvents. Unlike TriggerReconnect (used by
// every other auto-reconnect trigger), it debounces rapid repeat wake events
// and, if a health check is configured, skips the reconnect entirely when the
// tunnel still answers -- most wake events don't actually need a teardown.
func (a *TrayApp) onPowerWake() {
	a.mu.Lock()
	now := time.Now()
	since := now.Sub(a.lastWakeReconnect)
	if since < wakeReconnectDebounce {
		a.mu.Unlock()
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState,
			logevent.Str(logevent.AttrStage, "power_wake_debounced"),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("%.1fs", since.Seconds())))
		return
	}
	a.lastWakeReconnect = now
	hc := a.healthCheck
	a.mu.Unlock()

	if hc != nil && hc() {
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState, logevent.Str(logevent.AttrStage, "power_wake_healthy_skip"))
		return
	}

	a.TriggerReconnect()
}

// doReconnect is the body of the background reconnect goroutine.
// It checks quitting before and after doDisconnect to avoid racing with Quit.
func (a *TrayApp) doReconnect() {
	a.mu.Lock()
	q := a.quitting
	a.mu.Unlock()
	if q {
		return
	}
	a.doDisconnect(true)
	a.mu.Lock()
	q = a.quitting
	a.mu.Unlock()
	if !q {
		a.doConnect(true)
	}
}

// SetAuthWarning switches the tray to a warning state (connecting icon +
// tooltip) to signal that re-authentication is failing but not yet fatal.
// Cleared automatically when the next connect/disconnect cycle runs.
func (a *TrayApp) SetAuthWarning(msg string) {
	a.mu.Lock()
	connected := a.connected
	a.mu.Unlock()
	if !connected {
		return
	}
	a.setTrayIcon(icoConnecting, "")
	systray.SetTooltip(T("app_title_dash_prefix") + msg)
}

// ClearAuthWarning restores the tray to the normal connected state after a
// re-auth warning has been resolved.
func (a *TrayApp) ClearAuthWarning() {
	a.mu.Lock()
	connected := a.connected
	connectedAt := a.connectedAt
	a.mu.Unlock()
	if !connected {
		return
	}
	since := time.Since(connectedAt)
	h := int(since.Hours())
	m := int(since.Minutes()) % 60
	s := int(since.Seconds()) % 60
	a.setTrayIcon(a.connectedIcon(), "")
	systray.SetTooltip(fmt.Sprintf(T("tooltip_connected_fmt"), h, m, s))
}

// TriggerConnect queues a connect action from the app window.
// Safe to call from any goroutine; non-blocking.
func (a *TrayApp) TriggerConnect() {
	select {
	case a.connectCh <- struct{}{}:
	default:
	}
}

// TriggerDisconnect queues a disconnect action from the app window.
// Safe to call from any goroutine; non-blocking.
func (a *TrayApp) TriggerDisconnect() {
	select {
	case a.disconnectCh <- struct{}{}:
	default:
	}
}

// TriggerLogout queues a logout action from the app window's menu
// (mirrors the tray menu's Logout item — see mLogout.ClickedCh).
// Safe to call from any goroutine; non-blocking.
func (a *TrayApp) TriggerLogout() {
	select {
	case a.logoutCh <- struct{}{}:
	default:
	}
}

// TriggerLogin queues a login action from the app window's menu
// (mirrors the tray menu's Login item — see mLogin.ClickedCh).
// Safe to call from any goroutine; non-blocking.
func (a *TrayApp) TriggerLogin() {
	select {
	case a.loginCh <- struct{}{}:
	default:
	}
}

// TriggerQuit queues a quit action from the app window's menu (mirrors the
// tray menu's Quit item). Safe to call from any goroutine; non-blocking.
func (a *TrayApp) TriggerQuit() {
	select {
	case a.quitCh <- struct{}{}:
	default:
	}
}

// doQuit runs the shared quit/disconnect sequence for both the tray's own
// Quit menu item and the app window's menu Quit item (via quitCh). Must be
// followed by "return" from the caller's select loop — it does not itself
// exit onReady's goroutine, only tears down the tunnel and calls
// systray.Quit().
func (a *TrayApp) doQuit() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()

	// Blink gray icon for the entire quit/disconnect sequence so the
	// user sees progress even if the tray appears frozen.
	systray.SetTooltip(T("tooltip_quitting"))
	blinkStop := make(chan struct{})
	go func() {
		dim := true
		for {
			select {
			case <-blinkStop:
				return
			case <-time.After(400 * time.Millisecond):
			}
			if dim {
				a.setTrayIcon(icoQuitDim, "")
			} else {
				a.setTrayIcon(icoIdle, "")
			}
			dim = !dim
		}
	}()

	// Disconnect with a hard deadline. If the shutdown sequence
	// hangs (netsh/TUN/route commands stuck), force-exit after 30 s
	// so the process does not stay alive forever. Signal clean
	// shutdown first so the watchdog does not restart main.
	done := make(chan struct{})
	go func() {
		a.doDisconnect(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState, logevent.Str(logevent.AttrStage, "quit_disconnect_timeout"))
		SignalCleanShutdown()
		close(blinkStop)
		os.Exit(0)
	}
	close(blinkStop)
	systray.Quit()
}

// ShowAbout shows the About splash. Stateless — safe to call directly from
// any goroutine without going through a trigger channel.
func (a *TrayApp) ShowAbout() {
	go ShowSplash(a.version, 5*time.Second)
}

// TriggerUpdateInstall installs a downloaded update and restarts. Stateless —
// safe to call directly from any goroutine. Callers should check
// IsUpdateReady first (the app window greys its menu item accordingly), but
// this no longer trusts that check blindly: if core.ApplyPendingUpdate
// reports there was actually nothing on disk to apply (the ready state was
// stale -- e.g. an earlier attempt already consumed the pending file, or it
// was cleaned up some other way), the tray item is hidden and updateReady
// is reset instead of silently leaving a clickable dead end that does
// nothing forever. Found 2026-08-16: a click on "Update to X" with no
// pending file produced zero observable effect and zero log output --
// indistinguishable from the button simply being broken.
func (a *TrayApp) TriggerUpdateInstall() {
	go func() {
		if core.ApplyPendingUpdate() {
			return // install started (or handed off); this process is on its way out
		}
		logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState, logevent.Str(logevent.AttrStage, "update_reset"))
		a.mu.Lock()
		a.updateReady = false
		a.mu.Unlock()
		a.mUpdate.Hide()
		if a.OnUpdateReadyChanged != nil {
			a.OnUpdateReadyChanged()
		}
	}()
}

// IsUpdateReady reports whether a downloaded update is ready to install
// (mirrors mUpdate's visibility in the tray menu).
func (a *TrayApp) IsUpdateReady() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.updateReady
}

// SetRegionByCode selects a region by code ("" = Auto; "RU"/"EU"/"US"/"CN"/"XX")
// from the app window's menu, without needing a *systray.MenuItem reference.
// Safe to call from any goroutine.
func (a *TrayApp) SetRegionByCode(code string) {
	var selected *systray.MenuItem
	switch code {
	case "RU":
		selected = a.mRegionRussia
	case "EU":
		selected = a.mRegionEurope
	case "US":
		selected = a.mRegionUSA
	case "CN":
		selected = a.mRegionChina
	case "XX":
		selected = a.mRegionOther
	default:
		selected = a.mRegionAuto
	}
	a.setRegion(code, selected)
}

// SetStatusCallback registers a function called whenever the tunnel state changes.
// The callback fires on its own goroutine (non-blocking); may be called frequently.
func (a *TrayApp) SetStatusCallback(fn func()) {
	a.mu.Lock()
	a.onStatusChange = fn
	a.mu.Unlock()
}

// SetBytesTickCallback registers a function called about once per second
// while connected (see tickElapsed), for a lightweight uplink/downlink
// counter refresh that shouldn't pay for a full status repaint every tick.
func (a *TrayApp) SetBytesTickCallback(fn func()) {
	a.mu.Lock()
	a.onBytesTick = fn
	a.mu.Unlock()
}

// SetSettingsChangeCallback registers a function called whenever a settings
// toggle changes via the tray's own context menu (DoH, BlockQUIC, WildCat,
// region) -- the one direction of sync that had no path back to the window
// at all. See onSettingsChange's doc comment for the incident this fixes.
func (a *TrayApp) SetSettingsChangeCallback(fn func()) {
	a.mu.Lock()
	a.onSettingsChange = fn
	a.mu.Unlock()
}

// notifySettingsChange fires onSettingsChange in a goroutine if it is set.
// Call after every tray-menu settings mutation (DoH/BlockQUIC/WildCat/
// region), mirroring callStatusChange's fire-and-forget shape.
func (a *TrayApp) notifySettingsChange() {
	a.mu.Lock()
	fn := a.onSettingsChange
	a.mu.Unlock()
	if fn != nil {
		go fn()
	}
}

// callStatusChange fires onStatusChange in a goroutine if it is set.
func (a *TrayApp) callStatusChange() {
	a.mu.Lock()
	fn := a.onStatusChange
	connected := a.connected
	connecting := a.connecting
	disconnecting := a.disconnecting
	a.mu.Unlock()
	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState,
		logevent.Str(logevent.AttrStage, "status_change"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("connected=%v connecting=%v disconnecting=%v fn=%v", connected, connecting, disconnecting, fn != nil)))
	a.refreshBlockQUICVisibility()
	if fn != nil {
		go fn()
	}
}

// GetAppStatus returns the current tunnel state for the app window UI.
func (a *TrayApp) GetAppStatus() AppStatus {
	a.mu.Lock()
	connected := a.connected
	connecting := a.connecting
	disconnecting := a.disconnecting
	errorMsg := a.errorMsg
	at := a.connectedAt
	a.mu.Unlock()

	s := AppStatus{Connected: connected, Connecting: connecting, Disconnecting: disconnecting, Error: errorMsg != "", ErrorMsg: errorMsg, QUICLocked: a.IsWildcatQUICLocked()}
	if connected {
		since := time.Since(at)
		h := int(since.Hours())
		m := int(since.Minutes()) % 60
		sec := int(since.Seconds()) % 60
		s.Elapsed = fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
		if a.IsWildcatEnabled() {
			s.Mode = "wildcat"
		} else {
			s.Mode = "direct"
		}
	}
	return s
}

// GetAppSettings returns the current settings for the app window UI.
func (a *TrayApp) GetAppSettings() AppSettings {
	return AppSettings{
		DoH:       a.IsDNSOverHTTPSEnabled(),
		BlockQUIC: a.IsBlockQUICEnabled(),
		Region:    a.GetPreferredRegion(),
		WildCat:   a.IsWildcatEnabled(),
	}
}

// ApplyWindowSettings applies settings changed in the app window, mirroring
// the tray-menu checkbox handlers so callbacks fire and settings are saved.
// Guards against nil menu items (called before onReady completes).
func (a *TrayApp) ApplyWindowSettings(s AppSettings) {
	// DoH
	if s.DoH != a.IsDNSOverHTTPSEnabled() {
		if a.mDNSOverHTTPS != nil {
			if s.DoH {
				a.mDNSOverHTTPS.Check()
			} else {
				a.mDNSOverHTTPS.Uncheck()
			}
		}
		if a.onDNSOverHTTPSChange != nil {
			a.onDNSOverHTTPSChange(s.DoH)
		}
	}
	// Disable QUIC
	if s.BlockQUIC != a.IsBlockQUICEnabled() {
		if a.mBlockQUIC != nil {
			if s.BlockQUIC {
				a.mBlockQUIC.Check()
			} else {
				a.mBlockQUIC.Uncheck()
			}
		}
		if a.onBlockQUICChange != nil {
			a.onBlockQUICChange(s.BlockQUIC)
		}
	}
	// WildCat
	if s.WildCat != a.IsWildcatEnabled() {
		if a.mWildcat != nil {
			if s.WildCat {
				a.mWildcat.Check()
			} else {
				a.mWildcat.Uncheck()
			}
		}
		if a.onWildcatChange != nil {
			a.onWildcatChange(s.WildCat)
		}
		a.refreshBlockQUICVisibility()
	}
	// Region
	if s.Region != a.GetPreferredRegion() {
		if a.mRegionAuto != nil {
			switch s.Region {
			case "RU":
				a.setRegion("RU", a.mRegionRussia)
			case "EU":
				a.setRegion("EU", a.mRegionEurope)
			case "US":
				a.setRegion("US", a.mRegionUSA)
			case "CN":
				a.setRegion("CN", a.mRegionChina)
			case "XX":
				a.setRegion("XX", a.mRegionOther)
			default:
				a.setRegion("", a.mRegionAuto)
			}
		}
	}
}

// SetWindowCallback registers the function called when the user clicks
// "Open ShortNerdCat" in the tray menu.  Must be called before Run().
func (a *TrayApp) SetWindowCallback(fn func()) {
	a.onOpenWindow = fn
}

// runWatchdog passively monitors tunnel traffic every 2 s.
// It fires a reconnect when real outbound payload has been sent recently but
// no real inbound data has arrived within tunnelStuckTimeout (10 s).
// Idle sessions (no outbound traffic) are never flagged â€” only active sessions
// with a one-sided flow (sent but nothing back) trigger reconnect.
// Exits immediately when stop is closed (user-initiated disconnect).
func (a *TrayApp) runWatchdog(stop <-chan struct{}) {
	const checkInterval = 2 * time.Second
	timer := time.NewTimer(checkInterval)
	defer timer.Stop()

	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}

		if core.TunnelMonitor.IsStuck() {
			logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState, logevent.Str(logevent.AttrStage, "watchdog_stuck"))
			// Use TriggerReconnect (not a direct reconnectCh send) so the
			// userDisconnected guard is respected: if the user clicked Disconnect
			// at the same moment the timer fired, we must not override that.
			a.TriggerReconnect()
			return
		}

		timer.Reset(checkInterval)
	}
}

// enterErrorState tears down the tunnel and moves the tray to error state.
// The user must press Connect to try again.
func (a *TrayApp) enterErrorState() {
	a.mu.Lock()
	if !a.connected {
		a.mu.Unlock()
		return
	}
	a.connected = false
	stopCh := a.stopTick
	a.stopTick = nil
	a.mu.Unlock()

	if stopCh != nil {
		close(stopCh) // stops tickElapsed
	}
	a.onDisconnect(true) // automatic teardown â€” auto-reconnect follows via reconnectCh

	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayState, logevent.Str(logevent.AttrStage, "error_state_connection_lost"))
	a.setTrayIcon(icoError, T("err_connection_lost"))
	systray.SetTooltip(T("tooltip_connection_lost"))
	a.mDisconnect.Hide()
	a.mConnect.Show()
}

// pngToICO decodes a PNG and encodes it as a minimal 32Ã—32 32-bpp ICO.
// Scales the image with bilinear interpolation (uses bilinearScale from
// splash.go â€” same package).
func pngToICO(pngData []byte) []byte {
	const iconSize = 32

	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return coloredICO(128, 128, 128) // fallback: grey
	}

	var scaled *image.NRGBA
	sb := src.Bounds()
	if sb.Dx() == iconSize && sb.Dy() == iconSize {
		scaled = toNRGBAImage(src)
	} else {
		scaled = bilinearScale(src, iconSize, iconSize)
	}

	const (
		w, h     = iconSize, iconSize
		pixBytes = w * h * 4
		maskRows = h
		maskSize = maskRows * 4 // 4 bytes/row for 32-px AND mask (padded to DWORD)
		dataSize = 40 + pixBytes + maskSize
	)

	buf := make([]byte, 6+16+dataSize)

	// ICONDIR
	binary.LittleEndian.PutUint16(buf[2:], 1)
	binary.LittleEndian.PutUint16(buf[4:], 1)

	// ICONDIRENTRY
	buf[6], buf[7] = w, h
	binary.LittleEndian.PutUint16(buf[10:], 1)
	binary.LittleEndian.PutUint16(buf[12:], 32)
	binary.LittleEndian.PutUint32(buf[14:], uint32(dataSize))
	binary.LittleEndian.PutUint32(buf[18:], 22)

	// BITMAPINFOHEADER
	o := 22
	binary.LittleEndian.PutUint32(buf[o:], 40)
	binary.LittleEndian.PutUint32(buf[o+4:], w)
	binary.LittleEndian.PutUint32(buf[o+8:], h*2) // XOR+AND maps
	binary.LittleEndian.PutUint16(buf[o+12:], 1)
	binary.LittleEndian.PutUint16(buf[o+14:], 32)
	binary.LittleEndian.PutUint32(buf[o+20:], pixBytes)
	o += 40

	// Pixel data: BGRA, bottom-up rows.
	for row := h - 1; row >= 0; row-- {
		for col := 0; col < w; col++ {
			c := scaled.NRGBAAt(col, row)
			buf[o] = c.B
			buf[o+1] = c.G
			buf[o+2] = c.R
			buf[o+3] = c.A
			o += 4
		}
	}
	// AND-mask: all zeros = fully opaque (buf already zero).

	return buf
}

// toNRGBAImage converts an image.Image to *image.NRGBA without scaling.
func toNRGBAImage(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(x, y, color.NRGBAModel.Convert(src.At(x, y)))
		}
	}
	return dst
}

// coloredICO is the fallback: generates a minimal 16Ã—16 32-bpp ICO filled
// with a solid colour.  Used when PNG decoding fails.
func coloredICO(r, g, b byte) []byte {
	const (
		w, h     = 16, 16
		pixBytes = w * h * 4
		maskRows = h
		maskSize = maskRows * 4
		dataSize = 40 + pixBytes + maskSize
	)

	buf := make([]byte, 6+16+dataSize)

	binary.LittleEndian.PutUint16(buf[2:], 1)
	binary.LittleEndian.PutUint16(buf[4:], 1)

	buf[6], buf[7] = w, h
	binary.LittleEndian.PutUint16(buf[10:], 1)
	binary.LittleEndian.PutUint16(buf[12:], 32)
	binary.LittleEndian.PutUint32(buf[14:], uint32(dataSize))
	binary.LittleEndian.PutUint32(buf[18:], 22)

	o := 22
	binary.LittleEndian.PutUint32(buf[o:], 40)
	binary.LittleEndian.PutUint32(buf[o+4:], w)
	binary.LittleEndian.PutUint32(buf[o+8:], h*2)
	binary.LittleEndian.PutUint16(buf[o+12:], 1)
	binary.LittleEndian.PutUint16(buf[o+14:], 32)
	binary.LittleEndian.PutUint32(buf[o+20:], pixBytes)
	o += 40

	for i := 0; i < w*h; i++ {
		buf[o] = b
		buf[o+1] = g
		buf[o+2] = r
		buf[o+3] = 0xFF
		o += 4
	}
	return buf
}
