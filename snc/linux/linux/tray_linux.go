// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"embed"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"tunnel_cat/snc/core"
)

// Icons are copied to assets/ by deploy/linux/build.sh before compilation.
// See assets/.gitkeep for the list of required files.
//
//go:embed assets
var assetsFS embed.FS

func readAsset(name string) []byte {
	b, _ := assetsFS.ReadFile("assets/" + name)
	return b
}

const (
	connectDeadline    = 90 * time.Second
	disconnectDeadline = 30 * time.Second
)

// TrayApp manages the ShortNerdCat system-tray icon and menu on Linux.
//
// Lifecycle:
//
//	tray := NewTrayApp(...)
//	tray.Run() // blocks on the main goroutine (required by GTK)
type TrayApp struct {
	mu            sync.Mutex
	loggedIn      bool
	pendingLogin  bool
	autoConnect   bool
	connected     bool
	connecting    bool
	disconnecting bool
	errorMsg      string // non-empty only when setTrayIcon last set the error icon; the actual failure reason
	stopTick      chan struct{}
	version       string

	reconnectCh  chan struct{}
	connectCh    chan struct{}
	disconnectCh chan struct{}
	quitting     bool

	userDisconnected bool

	retryMu   sync.Mutex
	retryStop chan struct{}

	// disconnectPending is set when the user clicks Disconnect while a connect
	// attempt is still in progress. doConnect checks it on completion and tears
	// the tunnel down immediately if it came up while the user was waiting.
	disconnectPending     bool // protected by mu
	reconnectAfterPending bool // set alongside disconnectPending during auto-reconnect

	dohEnabled       bool
	blockQUICEnabled bool
	wildcatEnabled   bool
	preferredRegion  string

	onLogin              func() error
	onLogout             func()
	onConnect            func(autoReconnect bool) error
	onDisconnect         func(autoReconnect bool)
	onDNSOverHTTPSChange func(bool)
	onBlockQUICChange    func(bool)
	onWildcatChange      func(bool)
	onRegionChange       func(string)
	onStatusChange       func()
	onBeforeQuit         func()
	readyCb              func()
	onShareLogs          func()

	mLogin         *systray.MenuItem
	mLogout        *systray.MenuItem
	mConnect       *systray.MenuItem
	mDisconnect    *systray.MenuItem
	mDNSOverHTTPS  *systray.MenuItem
	mBlockQUIC     *systray.MenuItem
	mWildcat       *systray.MenuItem
	mRegion        *systray.MenuItem
	mRegionAuto    *systray.MenuItem
	mRegionRussia  *systray.MenuItem
	mRegionEurope  *systray.MenuItem
	mRegionUSA     *systray.MenuItem
	mRegionChina   *systray.MenuItem
	mRegionOther   *systray.MenuItem
	mOpen          *systray.MenuItem
	mUpdate        *systray.MenuItem
	mShareLogs     *systray.MenuItem
	mAbout         *systray.MenuItem
	updateNotifyCh chan string

	onOpenWindow func()
}

// NewTrayApp creates a TrayApp.
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
		connectCh:            make(chan struct{}, 1),
		disconnectCh:         make(chan struct{}, 1),
		updateNotifyCh:       make(chan string, 1),
	}
}

// NotifyUpdateReady shows the "Update to <version>" menu item.
func (a *TrayApp) NotifyUpdateReady(version string) {
	select {
	case a.updateNotifyCh <- version:
	default:
	}
}

// IsDNSOverHTTPSEnabled reports the current DoH setting.
func (a *TrayApp) IsDNSOverHTTPSEnabled() bool {
	if a.mDNSOverHTTPS == nil {
		return a.dohEnabled
	}
	return a.mDNSOverHTTPS.Checked()
}

// IsBlockQUICEnabled reports the current Disable QUIC setting.
func (a *TrayApp) IsBlockQUICEnabled() bool {
	if a.mBlockQUIC == nil {
		return a.blockQUICEnabled
	}
	return a.mBlockQUIC.Checked()
}

// IsWildcatEnabled reports whether WildCat mode is currently enabled.
func (a *TrayApp) IsWildcatEnabled() bool {
	if a.mWildcat == nil {
		return a.wildcatEnabled
	}
	return a.mWildcat.Checked()
}

// SetWildcatChecked forces the WildCat checkbox to checked/unchecked without
// triggering the callback (used to revert on a failed connect attempt).
func (a *TrayApp) SetWildcatChecked(v bool) {
	a.mu.Lock()
	a.wildcatEnabled = v
	a.mu.Unlock()
	if a.mWildcat == nil {
		return
	}
	if v {
		a.mWildcat.Check()
	} else {
		a.mWildcat.Uncheck()
	}
	a.refreshBlockQUICVisibility()
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
	a.mu.Unlock()
	return active && a.IsWildcatEnabled()
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

// setTrayIcon sets the tray icon and records errMsg as the current error
// reason ("" for any non-error icon), mirroring the Windows implementation
// so the main window status bar can show the actual failure text.
func (a *TrayApp) setTrayIcon(icon []byte, errMsg string) {
	a.mu.Lock()
	a.errorMsg = errMsg
	a.mu.Unlock()
	systray.SetIcon(icon)
}

func (a *TrayApp) connectedIcon() []byte {
	if a.IsWildcatEnabled() {
		return readAsset("snc_wildcat.png")
	}
	return readAsset("snc_connected.png")
}

// PreferredRegion returns the current region preference.
func (a *TrayApp) PreferredRegion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.preferredRegion
}

// ReconnectCh returns the channel that delivers reconnect signals.
func (a *TrayApp) ReconnectCh() <-chan struct{} { return a.reconnectCh }

// SetReadyCallback registers a function called once from onReady.
func (a *TrayApp) SetReadyCallback(fn func()) { a.readyCb = fn }

// TriggerLoginIfNeeded fires the login dialog if no key was saved at startup.
func (a *TrayApp) TriggerLoginIfNeeded() {
	if !a.pendingLogin {
		return
	}
	a.pendingLogin = false
	go a.doLogin()
}

// SetBeforeQuitCallback registers a function called when Quit is clicked.
func (a *TrayApp) SetBeforeQuitCallback(fn func()) {
	a.mu.Lock()
	a.onBeforeQuit = fn
	a.mu.Unlock()
}

// SetShareLogsCallback registers a function for "Open Log Directory".
func (a *TrayApp) SetShareLogsCallback(fn func()) {
	a.mu.Lock()
	a.onShareLogs = fn
	a.mu.Unlock()
}

// SetOpenWindowCallback registers a function called when "Open Window" is clicked.
func (a *TrayApp) SetOpenWindowCallback(fn func()) {
	a.mu.Lock()
	a.onOpenWindow = fn
	a.mu.Unlock()
}

// SetStatusCallback registers a function called on every tunnel state transition.
func (a *TrayApp) SetStatusCallback(fn func()) {
	a.mu.Lock()
	a.onStatusChange = fn
	a.mu.Unlock()
}

func (a *TrayApp) callStatusChange() {
	a.mu.Lock()
	fn := a.onStatusChange
	a.mu.Unlock()
	a.refreshBlockQUICVisibility()
	if fn != nil {
		go fn()
	}
}

// GetAppStatus returns the current tunnel state.
func (a *TrayApp) GetAppStatus() AppStatus {
	a.mu.Lock()
	connected := a.connected
	connecting := a.connecting
	disconnecting := a.disconnecting
	loggedIn := a.loggedIn
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
		s.Mode = "direct"
	}
	return s
}

// GetAppSettings returns the current settings.
func (a *TrayApp) GetAppSettings() AppSettings {
	return AppSettings{
		DoH:        a.IsDNSOverHTTPSEnabled(),
		BlockQUIC:  a.IsBlockQUICEnabled(),
		Region:     a.PreferredRegion(),
		WildCat:    a.IsWildcatEnabled(),
		QUICLocked: a.IsWildcatQUICLocked(),
	}
}

// TriggerConnect queues a Connect action. Non-blocking.
func (a *TrayApp) TriggerConnect() {
	select {
	case a.connectCh <- struct{}{}:
	default:
	}
}

// TriggerDisconnect queues a Disconnect action. Non-blocking.
func (a *TrayApp) TriggerDisconnect() {
	select {
	case a.disconnectCh <- struct{}{}:
	default:
	}
}

// TriggerReconnect queues an automatic disconnect+reconnect cycle.
func (a *TrayApp) TriggerReconnect() {
	a.mu.Lock()
	ud := a.userDisconnected
	a.mu.Unlock()
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

// SetAuthWarning switches the tray icon to a warning state.
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

// ClearAuthWarning restores the tray to normal connected state.
func (a *TrayApp) ClearAuthWarning() {
	a.mu.Lock()
	connected := a.connected
	a.mu.Unlock()
	if !connected {
		return
	}
	a.setTrayIcon(a.connectedIcon(), "")
	systray.SetTooltip("ShortNerdCat â€” " + T("status_connected"))
}

// ApplyIPCStatus updates tray icon/state from a status message received over IPC.
func (a *TrayApp) ApplyIPCStatus(state, msg string) {
	core.Log.Printf("ApplyIPCStatus: state=%q msg=%q", state, msg)
	switch state {
	case "pending":
		a.setTrayIcon(readAsset("snc_connecting.png"), "")
		if msg != "" {
			systray.SetTooltip("ShortNerdCat â€” " + msg)
		} else {
			systray.SetTooltip("ShortNerdCat â€” " + T("status_connecting"))
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
			stop := make(chan struct{})
			a.stopTick = stop
			a.mu.Unlock()
			core.TunnelMonitor.Reset()
		} else {
			a.mu.Unlock()
		}
		a.callStatusChange()
		a.setTrayIcon(a.connectedIcon(), "")
		a.mDisconnect.Show()
		a.mConnect.Hide()

	case "idle":
		a.mu.Lock()
		wasConnected := a.connected
		a.connected = false
		a.connecting = false
		a.disconnecting = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
		loggedIn := a.loggedIn
		a.mu.Unlock()
		if wasConnected {
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
		// Auth errors (key invalid/expired, server rejected) â†’ prompt for new key.
		// Network errors â†’ schedule automatic retry.
		if strings.Contains(msg, "not logged in") ||
			strings.Contains(msg, "authentication failed") ||
			strings.Contains(msg, "invalid key") {
			go a.doLogin()
		} else {
			a.scheduleRetry()
		}

	case "login_error":
		// The stored key/credentials are known-bad (server explicitly
		// rejected them, see snc/core's isAuthRejection/isInvalidKeyRejection)
		// -- go straight to re-login (doLogin, same as the "error" case's
		// message-matched auto-login above and the Login menu item) instead
		// of leaving a dead-end mConnect that would just retry the same
		// broken key. Explain first via a desktop notification (no blocking
		// native message-box helper exists on this platform, see
		// notification_linux.go) so re-login doesn't read as the app
		// forgetting the user's credentials.
		a.mu.Lock()
		a.connected = false
		a.connecting = false
		if a.stopTick != nil {
			close(a.stopTick)
			a.stopTick = nil
		}
		a.mu.Unlock()
		a.callStatusChange()
		a.setTrayIcon(readAsset("snc_error.png"), T("login_error_key_rejected"))
		systray.SetTooltip("ShortNerdCat â€” " + T("status_login_error"))
		a.mDisconnect.Hide()
		a.mConnect.Hide()
		// Keep mLogin visible in case the user cancels the dialog below --
		// doLogin's own failure path doesn't touch menu visibility, so
		// without this a cancelled/failed retry would leave no menu item
		// able to get back in.
		a.mLogin.Show()
		ShowNotification("ShortNerdCat â€” "+T("notify_login_error_title"), T("notify_login_error_body"))
		a.mu.Lock()
		a.loggedIn = false
		a.mu.Unlock()
		go a.doLogin()

	case "logged_in":
		a.mu.Lock()
		a.loggedIn = true
		a.mu.Unlock()
		a.callStatusChange()
		a.setTrayIcon(readAsset("snc_idle.png"), "")
		systray.SetTooltip("ShortNerdCat")
		a.mLogin.Hide()
		a.mLogout.Show()
		a.mConnect.Show()
		a.mDNSOverHTTPS.Show()
		a.mBlockQUIC.Show()
		a.mWildcat.Show()
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
		systray.SetTooltip("ShortNerdCat â€” " + T("status_not_logged_in"))
		a.mConnect.Hide()
		a.mDisconnect.Hide()
		a.mLogout.Hide()
		a.mDNSOverHTTPS.Hide()
		a.mBlockQUIC.Hide()
		a.mWildcat.Hide()
		a.mRegion.Hide()
		a.mLogin.Show()
	}
}

// Run starts the tray icon and blocks on the main goroutine until Quit.
// Must be called from the main goroutine.
func (a *TrayApp) Run() {
	systray.Run(a.onReady, func() {})
}

// Quit asks the systray to exit.
func (a *TrayApp) Quit() {
	systray.Quit()
}

func (a *TrayApp) onReady() {
	a.setTrayIcon(readAsset("snc_idle.png"), "")
	systray.SetTooltip("ShortNerdCat")

	a.mLogin = systray.AddMenuItem(T("menu_login_title"), T("menu_login_tooltip"))
	a.mLogout = systray.AddMenuItem(T("menu_logout_title"), T("menu_logout_tooltip"))
	systray.AddSeparator()
	a.mConnect = systray.AddMenuItem(T("menu_connect_title"), T("menu_connect_tooltip"))
	a.mDisconnect = systray.AddMenuItem(T("menu_disconnect_title"), T("menu_disconnect_tooltip"))
	systray.AddSeparator()
	a.mDNSOverHTTPS = systray.AddMenuItemCheckbox(T("menu_doh_title"), T("menu_doh_tooltip"), a.dohEnabled)
	a.mBlockQUIC = systray.AddMenuItemCheckbox(T("menu_block_quic_title"), T("menu_block_quic_tooltip"), a.blockQUICEnabled)
	a.mWildcat = systray.AddMenuItemCheckbox(T("menu_wildcat_title"), T("menu_wildcat_tooltip"), a.wildcatEnabled)
	a.mRegion = systray.AddMenuItem(T("menu_region_prefix")+regionName(a.preferredRegion), T("menu_region_tooltip"))
	a.mRegionAuto = a.mRegion.AddSubMenuItemCheckbox(T("region_auto"), T("region_auto_tooltip"), a.preferredRegion == "")
	a.mRegionRussia = a.mRegion.AddSubMenuItemCheckbox(T("region_russia"), T("region_russia"), a.preferredRegion == "RU")
	a.mRegionEurope = a.mRegion.AddSubMenuItemCheckbox(T("region_europe"), T("region_europe"), a.preferredRegion == "EU")
	a.mRegionUSA = a.mRegion.AddSubMenuItemCheckbox(T("region_usa"), T("region_usa_full"), a.preferredRegion == "US")
	a.mRegionChina = a.mRegion.AddSubMenuItemCheckbox(T("region_china"), T("region_china"), a.preferredRegion == "CN")
	a.mRegionOther = a.mRegion.AddSubMenuItemCheckbox(T("region_other"), T("region_other_tooltip"), a.preferredRegion == "XX")
	systray.AddSeparator()
	a.mOpen = systray.AddMenuItem(T("menu_open_title"), T("menu_open_tooltip"))
	a.mShareLogs = systray.AddMenuItem(T("menu_sharelogs_title"), T("menu_sharelogs_tooltip"))
	a.mAbout = systray.AddMenuItem(T("menu_about_title"), T("menu_about_tooltip"))
	a.mUpdate = systray.AddMenuItem(T("menu_update_title"), T("menu_update_tooltip"))
	a.mUpdate.Hide()
	mQuit := systray.AddMenuItem(T("menu_quit_title"), "")

	if a.loggedIn {
		a.mLogin.Hide()
		a.mDisconnect.Hide()
	} else {
		a.mLogout.Hide()
		a.mConnect.Hide()
		a.mDisconnect.Hide()
		a.mDNSOverHTTPS.Hide()
		a.mBlockQUIC.Hide()
		a.mWildcat.Hide()
		a.mRegion.Hide()
		a.setTrayIcon(readAsset("snc_error.png"), "")
		systray.SetTooltip("ShortNerdCat â€” " + T("status_not_logged_in"))
	}

	if !a.loggedIn {
		a.pendingLogin = true
	} else if a.autoConnect {
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.doConnect(false)
		}()
	}

	if a.readyCb != nil {
		go a.readyCb()
	}

	go func() {
		for {
			select {
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

			case <-a.mWildcat.ClickedCh:
				core.Log.Printf("tray: user toggled WildCat (was checked=%v)", a.mWildcat.Checked())
				if a.mWildcat.Checked() {
					a.mWildcat.Uncheck()
				} else {
					a.mWildcat.Check()
				}
				core.Log.Printf("tray: WildCat now=%v", a.mWildcat.Checked())
				a.refreshBlockQUICVisibility()
				if a.onWildcatChange != nil {
					a.onWildcatChange(a.mWildcat.Checked())
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
				go a.doReconnect()

			case v := <-a.updateNotifyCh:
				a.mUpdate.SetTitle("Update to " + v)
				a.mUpdate.Show()

			case <-a.mUpdate.ClickedCh:
				go core.ApplyPendingUpdate()

			case <-a.mOpen.ClickedCh:
				a.mu.Lock()
				fn := a.onOpenWindow
				a.mu.Unlock()
				if fn != nil {
					go fn()
				}

			case <-a.mShareLogs.ClickedCh:
				a.mu.Lock()
				fn := a.onShareLogs
				a.mu.Unlock()
				if fn != nil {
					go fn()
				}

			case <-a.mAbout.ClickedCh:
				go ShowNotification("Tunnel Cat", "v"+a.version+"\nnavlink.net")

			case <-mQuit.ClickedCh:
				a.mu.Lock()
				a.quitting = true
				fn := a.onBeforeQuit
				a.mu.Unlock()
				if fn != nil {
					fn()
				}

				systray.SetTooltip("ShortNerdCat â€” " + T("status_quitting"))
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
					core.Log.Printf("quit: disconnect timed out â€” forcing exit")
					SignalCleanShutdown()
					close(blinkStop)
					os.Exit(0)
				}
				close(blinkStop)
				// onBeforeQuit already sent "quit" IPC to the daemon so it can
				// signal clean shutdown and exit â€” no need to write flag files here.
				systray.Quit()
				return
			}
		}
	}()
}

func (a *TrayApp) doLogin() {
	if a.onLogin == nil {
		return
	}
	if err := a.onLogin(); err != nil {
		core.Log.Printf("login: %v", err)
		// "login cancelled" = user dismissed dialog.
		// "key entry in progress" = window opened, waiting for async key submission.
		// Either way: no error icon â€” just wait.
		if err.Error() != "login cancelled" && err.Error() != "key entry in progress" {
			msg := core.FriendlyConnectError(err)
			a.setTrayIcon(readAsset("snc_error.png"), T("login_failed_prefix_cap")+msg)
			systray.SetTooltip("ShortNerdCat â€” " + T("login_failed_prefix_low") + msg)
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
	a.setTrayIcon(readAsset("snc_idle.png"), "")
	systray.SetTooltip("ShortNerdCat â€” " + T("status_not_logged_in"))
	a.mConnect.Hide()
	a.mDisconnect.Hide()
	a.mLogout.Hide()
	a.mDNSOverHTTPS.Hide()
	a.mBlockQUIC.Hide()
	a.mWildcat.Hide()
	a.mRegion.Hide()
	a.mLogin.Show()
}

func (a *TrayApp) doConnect(autoReconnect bool) {
	core.Log.Printf("doConnect: autoReconnect=%v", autoReconnect)
	a.cancelRetry()
	a.mConnect.Hide()
	a.mu.Lock()
	if a.connecting {
		a.mu.Unlock()
		core.Log.Printf("connect: already in progress â€” skipping duplicate (autoReconnect=%v)", autoReconnect)
		return
	}
	a.disconnectPending = false
	a.userDisconnected = false
	a.connecting = true
	a.mu.Unlock()
	a.callStatusChange()
	a.setTrayIcon(readAsset("snc_connecting.png"), "")
	systray.SetTooltip("ShortNerdCat â€” " + T("status_connecting"))

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
	}
	a.mu.Lock()
	pending := a.disconnectPending
	shouldReconnect := a.reconnectAfterPending
	a.mu.Unlock()

	if connectErr != nil {
		a.mu.Lock()
		a.connecting = false
		a.reconnectAfterPending = false
		a.mu.Unlock()
		a.callStatusChange()
		core.Log.Printf("connect: failed â€” %v (pending=%v reconnect=%v)", connectErr, pending, shouldReconnect)
		if shouldReconnect {
			go a.doReconnect()
			return
		}
		msg := core.FriendlyConnectError(connectErr)
		a.setTrayIcon(readAsset("snc_error.png"), T("connect_failed_prefix")+msg)
		systray.SetTooltip("ShortNerdCat â€” " + T("error_prefix") + msg)
		a.mConnect.Show()
		a.scheduleRetry()
		return
	}

	a.mu.Lock()
	a.connected = true
	a.connecting = false
	a.reconnectAfterPending = false
	a.stopTick = make(chan struct{})
	stop := a.stopTick
	a.mu.Unlock()
	a.callStatusChange()

	if pending {
		// User clicked Disconnect while connect was in progress â€” tear down immediately.
		core.Log.Printf("connect: disconnectPending set â€” tearing down immediately (reconnect=%v)", shouldReconnect)
		close(stop)
		go func() {
			a.doDisconnect(shouldReconnect)
		}()
		return
	}

	a.setTrayIcon(a.connectedIcon(), "")
	a.mDisconnect.Show()

	core.TunnelMonitor.Reset()
	go a.runWatchdog(stop)
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
		default:
		}
	}
	a.mu.Lock()
	if !a.connected {
		// A connect attempt may be in progress â€” flag it so doConnect tears
		// the tunnel down as soon as (and if) it comes up.
		if a.connecting {
			a.disconnectPending = true
			if autoReconnect {
				a.reconnectAfterPending = true
			}
			a.mu.Unlock()
			core.Log.Printf("disconnect: connect in progress â€” setting disconnectPending (reconnect=%v)", autoReconnect)
			a.mDisconnect.Hide()
			systray.SetTooltip("ShortNerdCat â€” " + T("status_cancelling"))
			a.callStatusChange()
			return
		}
		a.mu.Unlock()
		return
	}
	a.connected = false
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
		systray.SetTooltip("ShortNerdCat â€” " + T("status_disconnecting"))
	}
	a.mConnect.Hide()
	a.mDisconnect.Hide()
	a.mLogout.Hide()
	a.mDNSOverHTTPS.Hide()
	a.mBlockQUIC.Hide()
	a.mWildcat.Hide()
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
		a.setTrayIcon(readAsset("snc_idle.png"), "")
		systray.SetTooltip("ShortNerdCat")
		if loggedIn {
			a.mConnect.Show()
			a.mLogout.Show()
			a.mDNSOverHTTPS.Show()
			a.mBlockQUIC.Show()
			a.mWildcat.Show()
			a.mRegion.Show()
		}
	}
}

func (a *TrayApp) cancelRetry() {
	a.retryMu.Lock()
	if a.retryStop != nil {
		close(a.retryStop)
		a.retryStop = nil
	}
	a.retryMu.Unlock()
}

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

func (a *TrayApp) setRegion(code string, selected *systray.MenuItem) {
	items := []*systray.MenuItem{
		a.mRegionAuto, a.mRegionRussia, a.mRegionEurope,
		a.mRegionUSA, a.mRegionChina, a.mRegionOther,
	}
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
	a.mRegion.SetTitle(T("menu_region_prefix") + regionName(code))
	if a.onRegionChange != nil {
		a.onRegionChange(code)
	}
}

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
			core.Log.Printf("watchdog: tunnel stuck â€” requesting reconnect")
			// Use TriggerReconnect (not a direct reconnectCh send) so the
			// userDisconnected guard is respected: if the user clicked Disconnect
			// at the same moment the timer fired, we must not override that.
			a.TriggerReconnect()
			return
		}

		timer.Reset(checkInterval)
	}
}

// ShowLoginError tears down the tunnel and shows a persistent error state.
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
	a.setTrayIcon(readAsset("snc_error.png"), T("login_error_key_rejected"))
	systray.SetTooltip("ShortNerdCat â€” " + T("status_login_error"))
	a.mDisconnect.Hide()
	a.mConnect.Show()
}
