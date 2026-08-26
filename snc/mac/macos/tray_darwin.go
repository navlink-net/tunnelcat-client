// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"embed"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"tunnel_cat/snc/core"
)

// Icons are copied to assets/ by build.sh before compilation.
// See assets/.gitkeep for the list of required files.
//
//go:embed assets
var assetsFS embed.FS

func readAsset(name string) []byte {
	b, _ := assetsFS.ReadFile("assets/" + name)
	return b
}

const (
	connectDeadline    = 90 * time.Second // max time for onConnect before error state
	disconnectDeadline = 30 * time.Second // max time for onDisconnect before forced UI reset
)

// TrayApp manages the ShortNerdCat menu-bar icon and menu.
//
// Lifecycle:
//
//	tray := NewTrayApp(...)
//	tray.Run() // blocks on the main goroutine (required by Cocoa)
type TrayApp struct {
	mu            sync.Mutex
	loggedIn      bool
	pendingLogin  bool // deferred login trigger: fire after window is ready
	autoConnect   bool
	connected     bool
	connecting    bool   // true while a connect attempt is in progress
	disconnecting bool   // true while onDisconnect is running
	errorMsg      string // non-empty only when setTrayIcon last set the error icon; the actual failure reason
	connectedAt   time.Time
	stopTick      chan struct{}
	version       string

	// reconnectCh receives a signal when the watchdog or a power-resume event
	// wants the app to perform an automatic disconnect+reconnect cycle.
	reconnectCh chan struct{}

	// connectCh / disconnectCh are triggered by the app window's buttons so they
	// behave identically to the tray menu items.
	connectCh    chan struct{}
	disconnectCh chan struct{}

	// quitting is set when the user clicks Quit.
	quitting bool // protected by mu

	// userDisconnected is set on manual disconnect, cleared in doConnect.
	// TriggerReconnect is suppressed while set.
	userDisconnected bool // protected by mu

	// retryMu guards retryStop. Closing retryStop cancels the retry goroutine.
	retryMu   sync.Mutex
	retryStop chan struct{}

	dohEnabled       bool
	blockQUICEnabled bool
	preferredRegion  string

	onLogin              func() error
	onLogout             func()
	onConnect            func(autoReconnect bool) error
	onDisconnect         func(autoReconnect bool)
	onDNSOverHTTPSChange func(bool)
	onBlockQUICChange    func(bool)
	onRegionChange       func(string)

	// onStatusChange is called (in a goroutine) on every tunnel state transition.
	onStatusChange func()
	// onOpenWindow is called when the user clicks the Open menu item.
	onOpenWindow func()
	// onBeforeQuit is called immediately when Quit is clicked, before disconnect.
	onBeforeQuit func()
	// readyCb is called once from onReady after the tray is fully initialised.
	readyCb func()

	mOpen          *systray.MenuItem
	mLogin         *systray.MenuItem
	mLogout        *systray.MenuItem
	mConnect       *systray.MenuItem
	mDisconnect    *systray.MenuItem
	mDNSOverHTTPS  *systray.MenuItem
	mBlockQUIC     *systray.MenuItem
	mRegion        *systray.MenuItem
	mRegionAuto    *systray.MenuItem
	mRegionRussia  *systray.MenuItem
	mRegionEurope  *systray.MenuItem
	mRegionUSA     *systray.MenuItem
	mRegionChina   *systray.MenuItem
	mRegionOther   *systray.MenuItem
	mUpdate        *systray.MenuItem
	mShareLogs     *systray.MenuItem
	mWildcat       *systray.MenuItem // WildCat mode toggle
	updateNotifyCh chan string

	wildcatEnabled bool
	// onWildcatChange is called when the user toggles the WildCat menu item.
	// enabled=true triggers whatever credential acquisition WildCat mode
	// needs in the tray process; the resulting token (or "" on cancel) is
	// passed as wildcatToken. enabled=false disables WildCat.
	onWildcatChange func(enabled bool, wildcatToken string)

	onShareLogs func()

	// menuXxxCh: trigger channels for the native NSApp menu bar (same pattern as
	// Windows TrayApp.loginCh/logoutCh/quitCh). Non-blocking sends from CGo
	// callbacks; consumed on the tray's onReady select loop.
	menuLoginCh   chan struct{}
	menuLogoutCh  chan struct{}
	menuQuitCh    chan struct{}
	menuDoHCh     chan struct{}
	menuQUICCh    chan struct{}
	menuWildcatCh chan struct{}
	menuRegionCh  chan string

	// updateReady mirrors mUpdate's visibility so the menu bar's Update item
	// can be grayed/enabled without reaching into systray internals. Protected by mu.
	updateReady bool
}

// NewTrayApp creates a TrayApp.
func NewTrayApp(
	version string,
	initiallyLoggedIn bool,
	autoConnect bool,
	dohEnabled bool,
	blockQUICEnabled bool,
	preferredRegion string,
	onLogin func() error,
	onLogout func(),
	onConnect func(autoReconnect bool) error,
	onDisconnect func(autoReconnect bool),
	onDNSOverHTTPSChange func(bool),
	onBlockQUICChange func(bool),
	onRegionChange func(string),
) *TrayApp {
	return &TrayApp{
		version:              version,
		loggedIn:             initiallyLoggedIn,
		autoConnect:          autoConnect,
		dohEnabled:           dohEnabled,
		blockQUICEnabled:     blockQUICEnabled,
		preferredRegion:      preferredRegion,
		onLogin:              onLogin,
		onLogout:             onLogout,
		onConnect:            onConnect,
		onDisconnect:         onDisconnect,
		onDNSOverHTTPSChange: onDNSOverHTTPSChange,
		onBlockQUICChange:    onBlockQUICChange,
		onRegionChange:       onRegionChange,
		reconnectCh:          make(chan struct{}, 1),
		connectCh:            make(chan struct{}, 1),
		disconnectCh:         make(chan struct{}, 1),
		updateNotifyCh:       make(chan string, 1),
		menuLoginCh:          make(chan struct{}, 1),
		menuLogoutCh:         make(chan struct{}, 1),
		menuQuitCh:           make(chan struct{}, 1),
		menuDoHCh:            make(chan struct{}, 1),
		menuQUICCh:           make(chan struct{}, 1),
		menuWildcatCh:        make(chan struct{}, 1),
		menuRegionCh:         make(chan string, 1),
	}
}

// NotifyUpdateReady shows the "Update to <version>" menu item. Safe to call from any goroutine.
func (a *TrayApp) NotifyUpdateReady(version string) {
	select {
	case a.updateNotifyCh <- version:
	default:
	}
}

// IsDNSOverHTTPSEnabled reports whether the user has enabled the DoH fallback option.
func (a *TrayApp) IsDNSOverHTTPSEnabled() bool {
	if a.mDNSOverHTTPS == nil {
		return a.dohEnabled
	}
	return a.mDNSOverHTTPS.Checked()
}

// IsBlockQUICEnabled reports whether the user has enabled the Disable QUIC option.
func (a *TrayApp) IsBlockQUICEnabled() bool {
	if a.mBlockQUIC == nil {
		return a.blockQUICEnabled
	}
	return a.mBlockQUIC.Checked()
}

// setTrayIcon sets the tray icon and records errMsg as the current error
// reason ("" for any non-error icon), mirroring the Windows implementation
// so the main window status bar can show the actual failure text.
func (a *TrayApp) setTrayIcon(icon []byte, errMsg string) {
	a.mu.Lock()
	a.errorMsg = errMsg
	a.mu.Unlock()
	systray.SetIcon(icon)
}

// connectedIcon returns the icon for the connected state.
func (a *TrayApp) connectedIcon() []byte {
	a.mu.Lock()
	wc := a.wildcatEnabled
	a.mu.Unlock()
	if wc {
		core.Log.Printf("connectedIcon: wildcatEnabled=true â†’ snc_wildcat.png")
		return readAsset("snc_wildcat.png")
	}
	core.Log.Printf("connectedIcon: wildcatEnabled=false â†’ snc_connected.png")
	return readAsset("snc_connected.png")
}

// PreferredRegion returns the current region preference ("" = Auto).
func (a *TrayApp) PreferredRegion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.preferredRegion
}

// ReconnectCh returns the channel that delivers reconnect signals.
func (a *TrayApp) ReconnectCh() <-chan struct{} { return a.reconnectCh }

// SetReadyCallback registers a function called once (in a goroutine) from
// onReady after the tray is fully initialised. Use this to create the app
// window after the Cocoa run loop has started.
func (a *TrayApp) SetReadyCallback(fn func()) {
	a.readyCb = fn
}

// TriggerLoginIfNeeded fires the login dialog if no key was saved at startup.
// Must be called after the app window is created so ShowKeyEntry can use it.
func (a *TrayApp) TriggerLoginIfNeeded() {
	if !a.pendingLogin {
		return
	}
	a.pendingLogin = false
	go a.doLogin()
}

// SetBeforeQuitCallback registers a function called immediately when Quit is
// clicked, before any disconnect logic. Use it to cancel blocking dialogs.
func (a *TrayApp) SetBeforeQuitCallback(fn func()) {
	a.mu.Lock()
	a.onBeforeQuit = fn
	a.mu.Unlock()
}

// SetShareLogsCallback registers a function called when the user clicks
// "Show Logs in Finder".
func (a *TrayApp) SetShareLogsCallback(fn func()) {
	a.mu.Lock()
	a.onShareLogs = fn
	a.mu.Unlock()
}

// SetWildcatCallback registers a function called when the WildCat menu item
// is toggled. enabled=true means the user wants WildCat on; the tray should
// acquire credentials, then call the callback with the resulting token.
// If acquisition fails or the user cancels, call with enabled=false, wildcatToken="".
func (a *TrayApp) SetWildcatCallback(fn func(enabled bool, wildcatToken string)) {
	a.mu.Lock()
	a.onWildcatChange = fn
	a.mu.Unlock()
}

// IsWildcatEnabled reports whether WildCat mode is currently enabled.
func (a *TrayApp) IsWildcatEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wildcatEnabled
}

// IsWildcatQUICLocked reports whether WildCat mode is currently forcing QUIC
// blocked, for the active/pending session. True only while a WildCat
// connection is actually connecting or connected -- unblocked UDP:443 QUIC
// could leak traffic straight past the WildCat relay. The underlying
// Disable QUIC preference is never touched by this -- callers hide the
// checkbox instead, so un-hiding it later shows exactly the state it was in
// before WildCat took over.
func (a *TrayApp) IsWildcatQUICLocked() bool {
	a.mu.Lock()
	active := a.connected || a.connecting
	wildcat := a.wildcatEnabled
	a.mu.Unlock()
	return active && wildcat
}

// refreshBlockQUICVisibility hides the "Disable QUIC" checkbox while
// IsWildcatQUICLocked, and shows it again otherwise.
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

// SetWildcatEnabled forces the WildCat menu item into the given state
// (used when the daemon reports connect failure and the tray must uncheck).
func (a *TrayApp) SetWildcatEnabled(enabled bool) {
	core.Log.Printf("SetWildcatEnabled: %v", enabled)
	a.mu.Lock()
	a.wildcatEnabled = enabled
	a.mu.Unlock()
	if a.mWildcat == nil {
		return
	}
	if enabled {
		a.mWildcat.Check()
	} else {
		a.mWildcat.Uncheck()
	}
	a.refreshBlockQUICVisibility()
}

// SetOpenWindowCallback registers a function called when the user clicks the
// "Open ShortNerdCat" tray menu item.
func (a *TrayApp) SetOpenWindowCallback(fn func()) {
	a.mu.Lock()
	a.onOpenWindow = fn
	a.mu.Unlock()
}

// SetStatusCallback registers a function called (in a goroutine) on every
// tunnel state transition (connect start/end, disconnect start/end, error).
func (a *TrayApp) SetStatusCallback(fn func()) {
	a.mu.Lock()
	a.onStatusChange = fn
	a.mu.Unlock()
}

// callStatusChange fires onStatusChange in a goroutine if registered.
func (a *TrayApp) callStatusChange() {
	a.mu.Lock()
	fn := a.onStatusChange
	a.mu.Unlock()
	a.refreshBlockQUICVisibility()
	if fn != nil {
		go fn()
	}
}

// GetAppStatus returns the current tunnel state for the window UI.
func (a *TrayApp) GetAppStatus() AppStatus {
	a.mu.Lock()
	connected := a.connected
	connecting := a.connecting
	disconnecting := a.disconnecting
	loggedIn := a.loggedIn
	at := a.connectedAt
	wildcat := a.wildcatEnabled
	errMsg := a.errorMsg
	a.mu.Unlock()

	s := AppStatus{
		Connected:     connected,
		Connecting:    connecting,
		Disconnecting: disconnecting,
		LoggedIn:      loggedIn,
		Error:         errMsg != "",
		ErrorMsg:      errMsg,
	}
	if connected {
		since := time.Since(at)
		h := int(since.Hours())
		m := int(since.Minutes()) % 60
		sec := int(since.Seconds()) % 60
		s.Elapsed = fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
		if wildcat {
			s.Mode = "wildcat"
		} else {
			s.Mode = "direct"
		}
	}
	return s
}

// GetAppSettings returns the current settings for the window UI.
func (a *TrayApp) GetAppSettings() AppSettings {
	return AppSettings{
		DoH:        a.IsDNSOverHTTPSEnabled(),
		BlockQUIC:  a.IsBlockQUICEnabled(),
		Region:     a.PreferredRegion(),
		Wildcat:    a.IsWildcatEnabled(),
		QUICLocked: a.IsWildcatQUICLocked(),
	}
}

// ApplyWindowSettings applies settings changed via the app window, mirroring
// the tray-menu checkbox handlers so all callbacks fire and settings are saved.
func (a *TrayApp) ApplyWindowSettings(s AppSettings) {
	if s.DoH != a.IsDNSOverHTTPSEnabled() {
		if a.mDNSOverHTTPS != nil {
			if s.DoH {
				a.mDNSOverHTTPS.Check()
			} else {
				a.mDNSOverHTTPS.Uncheck()
			}
		}
		a.mu.Lock()
		a.dohEnabled = s.DoH
		a.mu.Unlock()
		if a.onDNSOverHTTPSChange != nil {
			a.onDNSOverHTTPSChange(s.DoH)
		}
	}
	if s.BlockQUIC != a.IsBlockQUICEnabled() {
		if a.mBlockQUIC != nil {
			if s.BlockQUIC {
				a.mBlockQUIC.Check()
			} else {
				a.mBlockQUIC.Uncheck()
			}
		}
		a.mu.Lock()
		a.blockQUICEnabled = s.BlockQUIC
		a.mu.Unlock()
		if a.onBlockQUICChange != nil {
			a.onBlockQUICChange(s.BlockQUIC)
		}
	}
	if s.Region != a.PreferredRegion() {
		a.setRegionByCode(s.Region)
	}
	if s.Wildcat != a.IsWildcatEnabled() {
		go a.doWildcatToggle()
	}
}

// setRegionByCode selects the region radio button matching code and fires callbacks.
func (a *TrayApp) setRegionByCode(code string) {
	items := map[string]*systray.MenuItem{
		"":   a.mRegionAuto,
		"RU": a.mRegionRussia,
		"EU": a.mRegionEurope,
		"US": a.mRegionUSA,
		"CN": a.mRegionChina,
		"XX": a.mRegionOther,
	}
	if item, ok := items[code]; ok && item != nil {
		a.setRegion(code, item)
	}
}

// TriggerConnect queues a Connect action from the app window. Non-blocking.
func (a *TrayApp) TriggerConnect() {
	select {
	case a.connectCh <- struct{}{}:
	default:
	}
}

// TriggerDisconnect queues a Disconnect action from the app window. Non-blocking.
func (a *TrayApp) TriggerDisconnect() {
	select {
	case a.disconnectCh <- struct{}{}:
	default:
	}
}

// TriggerReconnect queues an automatic disconnect+reconnect cycle.
// Safe to call from any goroutine; non-blocking.
func (a *TrayApp) TriggerReconnect() {
	a.mu.Lock()
	ud := a.userDisconnected
	a.mu.Unlock()
	core.Log.Printf("TriggerReconnect: userDisconnected=%v", ud)
	if ud {
		core.Log.Printf("TriggerReconnect: suppressed (user manually disconnected)")
		return
	}
	select {
	case a.reconnectCh <- struct{}{}:
		core.Log.Printf("TriggerReconnect: queued")
	default:
		core.Log.Printf("TriggerReconnect: already queued, skipped")
	}
}

// TriggerLogin queues a login action from the native menu bar. Non-blocking.
func (a *TrayApp) TriggerLogin() {
	select {
	case a.menuLoginCh <- struct{}{}:
	default:
	}
}

// TriggerLogout queues a logout action from the native menu bar. Non-blocking.
func (a *TrayApp) TriggerLogout() {
	select {
	case a.menuLogoutCh <- struct{}{}:
	default:
	}
}

// TriggerQuit queues a quit action from the native menu bar. Non-blocking.
func (a *TrayApp) TriggerQuit() {
	select {
	case a.menuQuitCh <- struct{}{}:
	default:
	}
}

// ShowAbout shows the About splash. Safe to call from any goroutine.
func (a *TrayApp) ShowAbout() { go showAbout(a.version) }

// TriggerUpdateInstall installs a downloaded update and restarts.
func (a *TrayApp) TriggerUpdateInstall() { go core.ApplyPendingUpdate() }

// IsUpdateReady reports whether a downloaded update is waiting to install.
func (a *TrayApp) IsUpdateReady() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.updateReady
}

// SetRegionByCode selects a region from the native menu bar. Non-blocking.
func (a *TrayApp) SetRegionByCode(code string) {
	select {
	case a.menuRegionCh <- code:
	default:
	}
}

// doQuit performs the shared quit/disconnect sequence for both the tray menu
// and the native menu bar's Quit item. Caller must return immediately after —
// doQuit calls systray.Quit() which terminates the event loop.
func (a *TrayApp) doQuit() {
	a.mu.Lock()
	a.quitting = true
	fn := a.onBeforeQuit
	a.mu.Unlock()
	if fn != nil {
		fn()
	}

	systray.SetTooltip("ShortNerdCat — Quitting…")
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
				a.setTrayIcon(readAsset("snc_connecting.png"), "")
			} else {
				a.setTrayIcon(readAsset("snc_idle.png"), "")
			}
			dim = !dim
		}
	}()

	done := make(chan struct{})
	go func() {
		a.doDisconnect(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		core.Log.Printf("quit: disconnect timed out — forcing exit")
		SignalCleanShutdown()
		close(blinkStop)
		os.Exit(0)
	}
	close(blinkStop)
	systray.Quit()
}

// Run starts the menu-bar icon and blocks on the main goroutine until Quit.
// Must be called from the main goroutine.
func (a *TrayApp) Run() {
	systray.Run(a.onReady, func() {
		// onExit fires when NSApplication terminates — including Cmd+Q on the
		// main window, which bypasses the "Quit ShortNerdCat" menu-item path.
		// If the tray hasn't already initiated the quit (which would have sent
		// "quit" to the daemon), do it now so the daemon exits cleanly and the
		// watchdog doesn't relaunch the tray.
		a.mu.Lock()
		q := a.quitting
		fn := a.onBeforeQuit
		a.mu.Unlock()
		if !q && fn != nil {
			fn()
		}
	})
}

func (a *TrayApp) onReady() {
	fmt.Fprintf(os.Stderr, "tray: onReady start\n")
	a.setTrayIcon(readAsset("snc_idle.png"), "")
	fmt.Fprintf(os.Stderr, "tray: SetIcon done\n")
	systray.SetTooltip("ShortNerdCat")
	fmt.Fprintf(os.Stderr, "tray: SetTooltip done\n")

	a.mOpen = systray.AddMenuItem(T("tray_open"), "")
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mOpen done\n")
	systray.AddSeparator()
	a.mLogin = systray.AddMenuItem(T("tray_login"), T("tray_login_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mLogin done\n")
	a.mLogout = systray.AddMenuItem(T("tray_logout"), T("tray_logout_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mLogout done\n")
	systray.AddSeparator()
	a.mConnect = systray.AddMenuItem(T("tray_connect"), T("tray_connect_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mConnect done\n")
	a.mDisconnect = systray.AddMenuItem(T("tray_disconnect"), T("tray_disconnect_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mDisconnect done\n")
	systray.AddSeparator()
	a.mDNSOverHTTPS = systray.AddMenuItemCheckbox(T("tray_doh"), T("tray_doh_tip"), a.dohEnabled)
	fmt.Fprintf(os.Stderr, "tray: AddMenuItemCheckbox mDNSOverHTTPS done\n")
	a.mBlockQUIC = systray.AddMenuItemCheckbox(T("tray_quic"), T("tray_quic_tip"), a.blockQUICEnabled)
	fmt.Fprintf(os.Stderr, "tray: AddMenuItemCheckbox mBlockQUIC done\n")
	a.mRegion = systray.AddMenuItem(T("tray_region_prefix")+regionName(a.preferredRegion), T("tray_region_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mRegion done\n")
	a.mRegionAuto = a.mRegion.AddSubMenuItemCheckbox(T("tray_region_auto"), T("tray_region_auto_tip"), a.preferredRegion == "")
	fmt.Fprintf(os.Stderr, "tray: AddSubMenuItemCheckbox mRegionAuto done\n")
	a.mRegionRussia = a.mRegion.AddSubMenuItemCheckbox(T("tray_region_ru"), T("tray_region_ru_tip"), a.preferredRegion == "RU")
	fmt.Fprintf(os.Stderr, "tray: AddSubMenuItemCheckbox mRegionRussia done\n")
	a.mRegionEurope = a.mRegion.AddSubMenuItemCheckbox(T("tray_region_eu"), T("tray_region_eu_tip"), a.preferredRegion == "EU")
	fmt.Fprintf(os.Stderr, "tray: AddSubMenuItemCheckbox mRegionEurope done\n")
	a.mRegionUSA = a.mRegion.AddSubMenuItemCheckbox(T("tray_region_us"), T("tray_region_us_tip"), a.preferredRegion == "US")
	fmt.Fprintf(os.Stderr, "tray: AddSubMenuItemCheckbox mRegionUSA done\n")
	a.mRegionChina = a.mRegion.AddSubMenuItemCheckbox(T("tray_region_cn"), T("tray_region_cn_tip"), a.preferredRegion == "CN")
	fmt.Fprintf(os.Stderr, "tray: AddSubMenuItemCheckbox mRegionChina done\n")
	a.mRegionOther = a.mRegion.AddSubMenuItemCheckbox(T("tray_region_other"), T("tray_region_other_tip"), a.preferredRegion == "XX")
	fmt.Fprintf(os.Stderr, "tray: AddSubMenuItemCheckbox mRegionOther done\n")
	a.mWildcat = systray.AddMenuItemCheckbox(T("tray_wildcat"), T("tray_wildcat_tip"), false)
	fmt.Fprintf(os.Stderr, "tray: AddMenuItemCheckbox mWildcat done\n")
	systray.AddSeparator()
	mAbout := systray.AddMenuItem(T("tray_about"), T("tray_about_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mAbout done\n")
	a.mShareLogs = systray.AddMenuItem(T("tray_share_logs"), T("tray_share_logs_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mShareLogs done\n")
	a.mUpdate = systray.AddMenuItem(T("tray_update"), T("tray_update_tip"))
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mUpdate done\n")
	a.mUpdate.Hide()
	mQuit := systray.AddMenuItem(T("tray_quit"), "")
	fmt.Fprintf(os.Stderr, "tray: AddMenuItem mQuit done\n")

	// Initial visibility depends on login state.
	if a.loggedIn {
		a.mLogin.Hide()
		a.mDisconnect.Hide()
	} else {
		a.mLogout.Hide()
		a.mConnect.Hide()
		a.mDisconnect.Hide()
		a.mDNSOverHTTPS.Hide()
		a.mBlockQUIC.Hide()
		a.mRegion.Hide()
		a.setTrayIcon(readAsset("snc_error.png"), "")
		systray.SetTooltip("ShortNerdCat â€” not logged in")
	}

	// If not logged in at startup, mark pendingLogin so that TriggerLoginIfNeeded()
	// can show the dialog after the app window is ready (called from readyCb).
	// If already logged in with saved credentials, connect automatically.
	if !a.loggedIn {
		a.pendingLogin = true
		a.setTrayIcon(readAsset("snc_idle.png"), "")
		systray.SetTooltip("ShortNerdCat â€” not logged in")
	} else if a.autoConnect {
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.doConnect(false)
		}()
	}

	// Fire the ready callback after the tray is fully set up.
	fmt.Fprintf(os.Stderr, "tray: onReady setup complete, firing readyCb\n")
	if a.readyCb != nil {
		go a.readyCb()
	}
	fmt.Fprintf(os.Stderr, "tray: onReady returning\n")

	go func() {
		for {
			select {
			case <-a.mOpen.ClickedCh:
				a.mu.Lock()
				fn := a.onOpenWindow
				a.mu.Unlock()
				if fn != nil {
					go fn()
				}

			case <-a.connectCh:
				a.mu.Lock()
				loggedIn := a.loggedIn
				a.mu.Unlock()
				if !loggedIn {
					go a.doLogin()
				} else {
					a.mConnect.Hide()
					go a.doConnect(false)
				}

			case <-a.disconnectCh:
				a.mDisconnect.Hide()
				go a.doDisconnect(false)

			case <-a.mLogin.ClickedCh:
				a.doLogin()

			case <-a.mLogout.ClickedCh:
				go a.doLogout()

			case <-a.mConnect.ClickedCh:
				// Hide immediately to block double-clicks before the goroutine starts.
				a.mConnect.Hide()
				go a.doConnect(false)

			case <-a.mDisconnect.ClickedCh:
				a.mDisconnect.Hide()
				go a.doDisconnect(false)

			case <-a.mDNSOverHTTPS.ClickedCh:
				if a.mDNSOverHTTPS.Checked() {
					a.mDNSOverHTTPS.Uncheck()
				} else {
					a.mDNSOverHTTPS.Check()
				}
				if a.onDNSOverHTTPSChange != nil {
					a.onDNSOverHTTPSChange(a.mDNSOverHTTPS.Checked())
				}

			case <-a.mBlockQUIC.ClickedCh:
				if a.mBlockQUIC.Checked() {
					a.mBlockQUIC.Uncheck()
				} else {
					a.mBlockQUIC.Check()
				}
				if a.onBlockQUICChange != nil {
					a.onBlockQUICChange(a.mBlockQUIC.Checked())
				}

			case <-a.mRegionAuto.ClickedCh:
				a.setRegion("", a.mRegionAuto)
			case <-a.mRegionRussia.ClickedCh:
				a.setRegion("RU", a.mRegionRussia)
			case <-a.mRegionEurope.ClickedCh:
				a.setRegion("EU", a.mRegionEurope)
			case <-a.mRegionUSA.ClickedCh:
				a.setRegion("US", a.mRegionUSA)
			case <-a.mRegionChina.ClickedCh:
				a.setRegion("CN", a.mRegionChina)
			case <-a.mRegionOther.ClickedCh:
				a.setRegion("XX", a.mRegionOther)

			case <-a.reconnectCh:
				// Run in a background goroutine so the event loop is never blocked.
				go a.doReconnect()

			case v := <-a.updateNotifyCh:
				a.mUpdate.SetTitle("Update to " + v)
				a.mUpdate.Show()
				a.mu.Lock()
				a.updateReady = true
				a.mu.Unlock()

			case <-a.mUpdate.ClickedCh:
				go core.ApplyPendingUpdate()

			case <-mAbout.ClickedCh:
				go showAbout(a.version)

			case <-a.mShareLogs.ClickedCh:
				a.mu.Lock()
				fn := a.onShareLogs
				a.mu.Unlock()
				if fn != nil {
					go fn()
				}

			case <-a.mWildcat.ClickedCh:
				go a.doWildcatToggle()

			case <-mQuit.ClickedCh:
				a.doQuit()
				return

			// ── Native menu bar triggers ──────────────────────────────────────
			case <-a.menuLoginCh:
				a.doLogin()
			case <-a.menuLogoutCh:
				go a.doLogout()
			case <-a.menuQuitCh:
				a.doQuit()
				return
			case <-a.menuDoHCh:
				if a.mDNSOverHTTPS.Checked() {
					a.mDNSOverHTTPS.Uncheck()
				} else {
					a.mDNSOverHTTPS.Check()
				}
				if a.onDNSOverHTTPSChange != nil {
					a.onDNSOverHTTPSChange(a.mDNSOverHTTPS.Checked())
				}
				a.callStatusChange()
			case <-a.menuQUICCh:
				if a.mBlockQUIC.Checked() {
					a.mBlockQUIC.Uncheck()
				} else {
					a.mBlockQUIC.Check()
				}
				if a.onBlockQUICChange != nil {
					a.onBlockQUICChange(a.mBlockQUIC.Checked())
				}
				a.callStatusChange()
			case <-a.menuWildcatCh:
				go a.doWildcatToggle()
			case code := <-a.menuRegionCh:
				a.setRegionByCode(code)
				a.callStatusChange()
			}
		}
	}()
}

func (a *TrayApp) doWildcatToggle() {
	a.mu.Lock()
	wasEnabled := a.wildcatEnabled
	connected := a.connected
	fn := a.onWildcatChange
	a.mu.Unlock()

	newEnabled := !wasEnabled
	core.Log.Printf("doWildcatToggle: %v â†’ %v", wasEnabled, newEnabled)

	if newEnabled {
		// Unconditional one-button warning every time WildCat is turned on --
		// blocks this goroutine, but doWildcatToggle is always invoked via
		// `go a.doWildcatToggle()` (see the mWildcat.ClickedCh / menuWildcatCh
		// cases in onReady and ApplyWindowSettings), never on the systray
		// event loop itself, so blocking here is safe.
		ShowWildcatWarning()
	}

	a.mu.Lock()
	a.wildcatEnabled = newEnabled
	a.mu.Unlock()

	if newEnabled {
		a.mWildcat.Check()
	} else {
		a.mWildcat.Uncheck()
	}
	if fn != nil {
		fn(newEnabled, "") // credential acquisition happens at connect time, not here
	}
	a.callStatusChange() // push updated settings+status to window

	// If currently connected, disconnect+reconnect to switch relay mode.
	// Clear userDisconnected so TriggerReconnect isn't suppressed by a previous
	// manual disconnect (user is explicitly changing mode now).
	if connected {
		core.Log.Printf("doWildcatToggle: connected â€” clearing userDisconnected, triggering reconnect")
		a.mu.Lock()
		a.userDisconnected = false
		a.mu.Unlock()
		a.TriggerReconnect()
	}
}

func (a *TrayApp) doLogin() {
	if a.onLogin == nil {
		return
	}
	if err := a.onLogin(); err != nil {
		core.Log.Printf("login: %v", err)
		// "login cancelled" is user intent â€” keep idle icon, not error.
		if err.Error() != "login cancelled" {
			msg := core.FriendlyConnectError(err)
			a.setTrayIcon(readAsset("snc_error.png"), "Login failed: "+msg)
			systray.SetTooltip("ShortNerdCat â€” login failed: " + msg)
		}
		return
	}
	a.mu.Lock()
	a.loggedIn = true
	a.mu.Unlock()
	a.setTrayIcon(readAsset("snc_idle.png"), "")
	systray.SetTooltip("ShortNerdCat")
	a.mLogin.Hide()
	a.mLogout.Show()
	a.mConnect.Show()
	a.mDNSOverHTTPS.Show()
	a.mBlockQUIC.Show()
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
	a.setTrayIcon(readAsset("snc_idle.png"), "")
	systray.SetTooltip("ShortNerdCat â€” not logged in")
	a.mConnect.Hide()
	a.mDisconnect.Hide()
	a.mLogout.Hide()
	a.mDNSOverHTTPS.Hide()
	a.mBlockQUIC.Hide()
	a.mRegion.Hide()
	a.mLogin.Show()
}

func (a *TrayApp) doConnect(autoReconnect bool) {
	core.Log.Printf("doConnect: autoReconnect=%v", autoReconnect)
	a.cancelRetry()

	// Guard against concurrent doConnect calls (e.g. window button + watchdog race).
	a.mu.Lock()
	if a.connecting || a.connected {
		a.mu.Unlock()
		core.Log.Printf("doConnect: already active (connecting=%v connected=%v), skipping", a.connecting, a.connected)
		return
	}
	a.connecting = true
	a.userDisconnected = false // clear manual-disconnect flag on any new connect
	a.mu.Unlock()

	a.mConnect.Hide()
	a.callStatusChange()
	core.Log.Printf("doConnect: setting connecting icon")
	a.setTrayIcon(readAsset("snc_connecting.png"), "")
	systray.SetTooltip("ShortNerdCat â€” Connectingâ€¦")

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				core.Log.Printf("connect: PANIC: %v\n%s", r, stack)
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
		core.Log.Printf("connect: %v â€” entering error state", connectErr)
	}
	if connectErr != nil {
		a.mu.Lock()
		a.connecting = false
		a.mu.Unlock()
		a.callStatusChange()
		msg := core.FriendlyConnectError(connectErr)
		a.setTrayIcon(readAsset("snc_error.png"), "Connect failed: "+msg)
		systray.SetTooltip("ShortNerdCat â€” Error: " + msg)
		a.mConnect.Show()
		a.scheduleRetry()
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

	core.Log.Printf("doConnect: setting connected icon (wildcatEnabled=%v)", a.wildcatEnabled)
	a.setTrayIcon(a.connectedIcon(), "")
	a.mDisconnect.Show()

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
			systray.SetTooltip(fmt.Sprintf("ShortNerdCat â€” Connected  %02d:%02d:%02d", h, m, s))
		}
	}
}

func (a *TrayApp) doDisconnect(autoReconnect bool) {
	core.Log.Printf("doDisconnect: autoReconnect=%v", autoReconnect)
	a.cancelRetry()
	if !autoReconnect {
		a.mu.Lock()
		a.userDisconnected = true
		a.mu.Unlock()
		select {
		case <-a.reconnectCh:
			core.Log.Printf("disconnect: drained stale reconnectCh message")
		default:
		}
	}
	a.mu.Lock()
	if !a.connected && !a.connecting {
		a.mu.Unlock()
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
	a.callStatusChange()

	if !quitting {
		a.setTrayIcon(readAsset("snc_connecting.png"), "")
		systray.SetTooltip("ShortNerdCat â€” Disconnectingâ€¦")
	}
	a.mConnect.Hide()
	a.mDisconnect.Hide()
	a.mLogout.Hide()
	a.mDNSOverHTTPS.Hide()
	a.mBlockQUIC.Hide()
	a.mRegion.Hide()

	disconnDone := make(chan struct{})
	go func() {
		if a.onDisconnect != nil {
			a.onDisconnect(autoReconnect)
		}
		close(disconnDone)
	}()
	select {
	case <-disconnDone:
	case <-time.After(disconnectDeadline):
		core.Log.Printf("disconnect: onDisconnect hung for %v â€” forcing UI reset", disconnectDeadline)
	}

	a.mu.Lock()
	a.disconnecting = false
	a.mu.Unlock()
	a.callStatusChange()

	if !quitting {
		core.Log.Printf("doDisconnect: setting idle icon (autoReconnect=%v)", autoReconnect)
		a.setTrayIcon(readAsset("snc_idle.png"), "")
		systray.SetTooltip("ShortNerdCat")
		if loggedIn {
			a.mConnect.Show()
			a.mLogout.Show()
			a.mDNSOverHTTPS.Show()
			a.mBlockQUIC.Show()
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

// scheduleRetry starts a goroutine that retries the connect with exponential
// backoff (30 s â†’ 60 s â†’ 120 s â€¦ cap 5 min).
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

// doReconnect is the body of the background reconnect goroutine.
func (a *TrayApp) doReconnect() {
	a.mu.Lock()
	q := a.quitting
	a.mu.Unlock()
	if q {
		return
	}
	core.Log.Printf("doReconnect: starting disconnect")
	a.doDisconnect(true)

	// doDisconnect just finished, so connecting/connected MUST be false now --
	// but ApplyIPCStatus (an independent writer, fed by async "pending"/
	// "connected"/"idle" pushes from the daemon over IPC, racing with this
	// synchronous tray-side flow) can land a stale "pending" from a
	// not-yet-resolved earlier cycle in the gap between doDisconnect
	// returning and doConnect's own guard check below. That leaves
	// connecting=true for a connect attempt that never actually started,
	// so doConnect's "already active" guard silently skips forever --
	// real incident, 2026-08-10: a wake-triggered reconnect left the app
	// stuck in "idle" for 8 minutes with no error, no retry, until the user
	// noticed and clicked Connect manually. doReconnect owns this sequence
	// end to end and just confirmed disconnect completed, so it's safe (and
	// necessary) to force a clean slate right before calling doConnect.
	a.mu.Lock()
	a.connecting = false
	a.connected = false
	q = a.quitting
	a.mu.Unlock()

	core.Log.Printf("doReconnect: disconnect done, quitting=%v", q)
	if !q {
		core.Log.Printf("doReconnect: starting connect")
		a.doConnect(true)
		core.Log.Printf("doReconnect: connect done")
	}
}

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
	a.mRegion.SetTitle("Region: " + regionName(code))
	if a.onRegionChange != nil {
		a.onRegionChange(code)
	}
}

func regionName(code string) string {
	switch code {
	case "RU":
		return "Russia"
	case "EU":
		return "Europe"
	case "US":
		return "USA"
	case "CN":
		return "China"
	case "XX":
		return "Other"
	default:
		return "Auto"
	}
}

// runWatchdog passively monitors tunnel traffic every 2 s.
// It fires a reconnect when real outbound payload has been sent recently but
// no real inbound data has arrived within tunnelStuckTimeout (10 s).
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
			core.Log.Printf("watchdog: tunnel stuck â€” outbound payload with no inbound response â€” requesting reconnect")
			// Use TriggerReconnect (not a direct reconnectCh send) so the
			// userDisconnected guard is respected: if the user clicked Disconnect
			// at the same moment the timer fired, we must not override that.
			a.TriggerReconnect()
			return
		}

		timer.Reset(checkInterval)
	}
}

// SetAuthWarning switches the tray to a warning state (connecting icon +
// tooltip) to signal that re-authentication is failing but not yet fatal.
func (a *TrayApp) SetAuthWarning(msg string) {
	a.mu.Lock()
	connected := a.connected
	a.mu.Unlock()
	if !connected {
		return
	}
	a.setTrayIcon(readAsset("snc_connecting.png"), "")
	systray.SetTooltip("ShortNerdCat â€” " + msg)
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
	systray.SetTooltip(fmt.Sprintf("ShortNerdCat â€” Connected  %02d:%02d:%02d", h, m, s))
}

// ShowLoginError tears down the tunnel and shows a persistent red-icon
// "Login error" state. The user must click Connect to retry.
func (a *TrayApp) ShowLoginError() {
	a.mu.Lock()
	wasConnected := a.connected
	if wasConnected {
		a.connected = false
		a.connecting = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
	}
	a.mu.Unlock()
	if wasConnected {
		a.callStatusChange()
	}

	if wasConnected && a.onDisconnect != nil {
		a.onDisconnect(false)
	}

	a.setTrayIcon(readAsset("snc_error.png"), "Login error â€” your key was rejected")
	systray.SetTooltip("ShortNerdCat â€” Login error")
	a.mDisconnect.Hide()
	a.mConnect.Show()
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
	a.connecting = false
	stopCh := a.stopTick
	a.stopTick = nil
	a.mu.Unlock()
	a.callStatusChange()

	if stopCh != nil {
		close(stopCh)
	}
	if a.onDisconnect != nil {
		a.onDisconnect(true)
	}

	a.setTrayIcon(readAsset("snc_error.png"), "Connection lost")
	systray.SetTooltip("ShortNerdCat â€” Connection lost")
	a.mDisconnect.Hide()
	a.mConnect.Show()
}

// showAbout mirrors Windows: re-uses the splash screen, shown for 5 seconds.
func showAbout(version string) {
	ShowSplash(version, 5*time.Second)
}

// Quit asks the systray to exit. Safe to call from any goroutine.
func (a *TrayApp) Quit() {
	systray.Quit()
}

// ApplyIPCStatus updates tray icon/state from a status message received over IPC.
// Called by the tray process when the main (root) process pushes a state change.
// States: "idle", "pending", "connected", "error", "login_error", "logged_out".
func (a *TrayApp) ApplyIPCStatus(state, msg string) {
	core.Log.Printf("ApplyIPCStatus: state=%q msg=%q", state, msg)
	switch state {
	case "pending":
		a.setTrayIcon(readAsset("snc_connecting.png"), "")
		if msg != "" {
			systray.SetTooltip("ShortNerdCat â€” " + msg)
		} else {
			systray.SetTooltip("ShortNerdCat â€” Connectingâ€¦")
		}
		a.mu.Lock()
		a.connecting = true
		a.connected = false
		a.mu.Unlock()
		a.callStatusChange()

	case "connected":
		a.mu.Lock()
		alreadyConnected := a.connected
		a.connecting = false
		a.connected = true
		if !alreadyConnected {
			a.connectedAt = time.Now()
			stop := make(chan struct{})
			a.stopTick = stop
			a.mu.Unlock()
			go a.tickElapsed(stop)
			core.TunnelMonitor.Reset()
		} else {
			a.mu.Unlock()
		}
		a.callStatusChange()
		core.Log.Printf("ApplyIPCStatus connected: setting connected icon (wildcatEnabled=%v)", a.wildcatEnabled)
		a.setTrayIcon(a.connectedIcon(), "")
		a.mDisconnect.Show()
		a.mConnect.Hide()

	case "idle":
		a.mu.Lock()
		wasActive := a.connected || a.connecting || a.disconnecting
		a.connected = false
		a.connecting = false
		a.disconnecting = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
		loggedIn := a.loggedIn
		a.mu.Unlock()
		if wasActive {
			a.callStatusChange()
		}
		a.setTrayIcon(readAsset("snc_idle.png"), "")
		systray.SetTooltip("ShortNerdCat")
		a.mDisconnect.Hide()
		if loggedIn {
			a.mConnect.Show()
		}

	case "error":
		a.mu.Lock()
		a.connected = false
		a.connecting = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
		a.mu.Unlock()
		a.callStatusChange()
		errText := msg
		if errText == "" {
			errText = "connection error"
		}
		a.setTrayIcon(readAsset("snc_error.png"), errText)
		systray.SetTooltip("ShortNerdCat â€” " + errText)
		a.mDisconnect.Hide()
		a.mConnect.Show()
		a.scheduleRetry()

	case "login_error":
		// The stored key/credentials are known-bad (server explicitly
		// rejected them, see snc/core's isAuthRejection/isInvalidKeyRejection)
		// -- go straight to re-login (doLogin, the same flow used at first
		// launch and from the Login menu item) instead of leaving a
		// dead-end mConnect that would just retry the same broken key.
		// Explain first: silently dropping into the login dialog with no
		// context reads as the app forgetting the user's credentials, not
		// as "your key stopped working."
		a.mu.Lock()
		a.connected = false
		a.connecting = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
		a.mu.Unlock()
		a.callStatusChange()
		a.setTrayIcon(readAsset("snc_error.png"), "Login error â€” your key was rejected")
		systray.SetTooltip("ShortNerdCat â€” Login error")
		a.mDisconnect.Hide()
		a.mConnect.Hide()
		// Keep mLogin visible in case the user cancels the dialog below --
		// doLogin's own failure path doesn't touch menu visibility, so
		// without this a cancelled/failed retry would leave no menu item
		// able to get back in.
		a.mLogin.Show()
		ShowError("ShortNerdCat", "Your ShortNerdCat key was rejected by the server and can no longer be used.\n\nPlease log in again.")
		a.mu.Lock()
		a.loggedIn = false
		a.mu.Unlock()
		a.doLogin()

	case "logged_in":
		a.mu.Lock()
		a.loggedIn = true
		a.mu.Unlock()
		a.setTrayIcon(readAsset("snc_idle.png"), "")
		systray.SetTooltip("ShortNerdCat")
		a.mLogin.Hide()
		a.mLogout.Show()
		a.mConnect.Show()
		a.mDNSOverHTTPS.Show()
		a.mBlockQUIC.Show()
		a.mRegion.Show()

	case "logged_out":
		a.mu.Lock()
		a.loggedIn = false
		a.connected = false
		a.connecting = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
		a.mu.Unlock()
		a.callStatusChange()
		a.setTrayIcon(readAsset("snc_idle.png"), "")
		systray.SetTooltip("ShortNerdCat â€” not logged in")
		a.mConnect.Hide()
		a.mDisconnect.Hide()
		a.mLogout.Hide()
		a.mDNSOverHTTPS.Hide()
		a.mBlockQUIC.Hide()
		a.mRegion.Hide()
		a.mLogin.Show()
	}
}
