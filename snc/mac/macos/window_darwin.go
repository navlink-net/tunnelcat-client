// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

// SNCWindow is the public API for the native macOS app window.
// CGo internals live in window_cgo_darwin.go; this file is CGo-free so that
// gopls and other tools can resolve SNCWindow and NewSNCWindow correctly.

import (
	"encoding/json"
	"fmt"
	"os"

	"shortnerdcat/snc/shared/theme"
	"tunnel_cat/snc/core"
)

// SNCWindow wraps the native macOS WKWebView window.
//
// Lifecycle:
//
//	win := NewSNCWindow(...)   // registers assets, schedules Cocoa init
//	win.Show()                 // called when user clicks Open in tray
//	win.PushStatus(s)          // called on every tunnel state change
//	win.PushSettings(s)        // called when window becomes visible
//	win.Destroy()              // called at app exit
type SNCWindow struct {
	onConnect          func()
	onDisconnect       func()
	onSettings         func(AppSettings)
	onPageReady        func()
	onRecommend        func(username string) // set via SetRecommendCallback; see tunnel_cat/docs/club-membership.md
	onClubThemePreview func(theme string)    // set via SetClubThemePreviewCallback
	keyCh              chan string
	haveKeyCh          chan bool
	loginCh            chan credentialResult
}

// SetRecommendCallback registers the function called when the user submits
// the "Recommend new Cat Club members" form in the Settings panel. Set
// once membership is confirmed (see initClubDiscovery in main_darwin.go) --
// safe to set at any time, just must be set before the user can click
// submit (the panel itself is JS-hidden until then anyway).
func (w *SNCWindow) SetRecommendCallback(fn func(username string)) { w.onRecommend = fn }

// SetClubThemePreviewCallback registers the function called when an admin
// picks a theme from the Settings panel's Club Theme selector
// (theme="regular"/"catclub"/"elite"). Purely a client-side UI convenience
// (see keyenc.go's IsAdmin doc comment -- "never a real permission"); the
// callback just needs to update local UI state, no tunnel/session round trip.
func (w *SNCWindow) SetClubThemePreviewCallback(fn func(theme string)) { w.onClubThemePreview = fn }

// credentialResult carries the outcome of the credential-login panel back
// from the ObjC delegate: either an email+password submission, or a
// wantsKeyMode switch to manual key entry (fields empty in that case).
type credentialResult struct {
	email        string
	password     string
	wantsKeyMode bool
}

// globalWindow is the singleton SNCWindow referenced by CGo callbacks in
// window_cgo_darwin.go. Both files are in the same package; the variable is
// shared at link time.
var globalWindow *SNCWindow

// globalTray is the singleton TrayApp referenced by the go_snc_menu_* CGo
// callbacks in window_cgo_darwin.go. Set via SetMenuTray before BuildAppMenu.
var globalTray *TrayApp

// SetMenuTray registers the TrayApp for native menu-bar callbacks. Must be
// called before BuildAppMenu.
func SetMenuTray(t *TrayApp) { globalTray = t }

// DeepLinkKeysCh receives keys delivered via navlink://activate?key=â€¦ Apple Events.
// Buffered so that an early-arrival event (before the window is created) is not lost.
// The tray process reads from this channel and forwards the key to the daemon via IPC.
var DeepLinkKeysCh = make(chan string, 4)

// RegisterURLHandler registers the NSAppleEventManager handler for the navlink://
// URL scheme. Must be called before the Cocoa run loop starts.
func RegisterURLHandler() { registerURLHandler() }

// NewSNCWindow registers assets from the embedded FS, injects HTML, and
// schedules NSWindow + WKWebView creation on the Cocoa main thread via
// dispatch_async. Returns immediately â€” the window is not yet visible.
//
// Must be called after the Cocoa run loop has started (i.e., from a goroutine
// that fires after tray.Run() begins) so that dispatch_async can deliver to
// the main queue.
func NewSNCWindow(
	onConnect func(),
	onDisconnect func(),
	onSettings func(AppSettings),
) *SNCWindow {
	fmt.Fprintf(os.Stderr, "tray: NewSNCWindow called\n")
	w := &SNCWindow{
		onConnect:    onConnect,
		onDisconnect: onDisconnect,
		onSettings:   onSettings,
	}
	globalWindow = w

	// Register all assets with the ObjC scheme handler (sncasset://<name>).
	// The main-screen illustration files are deliberately separate from
	// snc_idle.png etc., which tray_darwin.go embeds for the menu-bar status
	// icon -- those used to be the same files; redesigning one silently
	// broke the other (a tiny menu-bar icon rendered from a full illustration
	// looks nothing like the intended icon). Keep them independent.
	for _, name := range []string{
		"bg.png",
		"illustration_idle.png", "illustration_connecting.png", "illustration_connected.png",
		"illustration_error.png",
		// Club-theme variants (see tunnel_cat/docs/club-membership.md) --
		// same four states, palette-only variants selected client-side by
		// appending "_catclub"/"_elite" to the filename (see onClubThemeUpdate
		// in windowHTML's JS below). Registered unconditionally; a regular
		// (non-member) user's JS just never references these URLs.
		"illustration_idle_catclub.png", "illustration_connecting_catclub.png",
		"illustration_connected_catclub.png",
		"illustration_error_catclub.png",
		"illustration_idle_elite.png", "illustration_connecting_elite.png",
		"illustration_connected_elite.png",
		"illustration_error_elite.png",
	} {
		fmt.Fprintf(os.Stderr, "tray: registering asset %q\n", name)
		data := readAsset(name)
		if len(data) == 0 {
			core.Log.Printf("window: asset %q missing from embedded FS", name)
			continue
		}
		windowRegisterAsset(name, data)
		fmt.Fprintf(os.Stderr, "tray: registered asset %q ok\n", name)
	}

	fmt.Fprintf(os.Stderr, "tray: calling windowSetHTML\n")
	windowSetHTML(windowHTML())
	fmt.Fprintf(os.Stderr, "tray: calling windowInit\n")
	windowInit() // dispatch_async to main thread â€” returns immediately
	fmt.Fprintf(os.Stderr, "tray: windowInit returned\n")

	return w
}

// Show brings the window to the foreground.
func (w *SNCWindow) Show() { windowShow() }

// Hide hides the window without destroying it.
func (w *SNCWindow) Hide() { windowHide() }

// Destroy closes and releases the native window resources.
func (w *SNCWindow) Destroy() { windowDestroy() }

// ShowKeyEntry shows the native key-entry dialog and blocks until the user
// submits or cancels. Returns an error if cancelled.
func (w *SNCWindow) ShowKeyEntry() (string, error) {
	ch := make(chan string, 1)
	w.keyCh = ch
	windowShowKeyEntry()
	key := <-ch
	w.keyCh = nil
	if key == "" {
		return "", fmt.Errorf("cancelled")
	}
	return key, nil
}

// CancelKeyEntry dismisses the key-entry dialog without submitting a key.
func (w *SNCWindow) CancelKeyEntry() { windowCancelKeyEntry() }

// ShowHaveKeyPrompt asks the user whether they already have an activation
// key, blocking until they answer Yes/No or close the panel (treated as No).
func (w *SNCWindow) ShowHaveKeyPrompt() bool {
	ch := make(chan bool, 1)
	w.haveKeyCh = ch
	windowShowHaveKeyPrompt()
	hasKey := <-ch
	w.haveKeyCh = nil
	return hasKey
}

// ShowCredentialLogin shows the email/password login panel and blocks until
// the user submits, switches to key-entry mode, or cancels. err is non-nil
// only on outright cancel (window closed); wantsKeyMode distinguishes the
// "I Have a Key" switch from a real cancel.
func (w *SNCWindow) ShowCredentialLogin() (email, password string, wantsKeyMode bool, err error) {
	ch := make(chan credentialResult, 1)
	w.loginCh = ch
	windowShowCredentialLogin()
	res := <-ch
	w.loginCh = nil
	if res.wantsKeyMode {
		return "", "", true, nil
	}
	if res.email == "" || res.password == "" {
		return "", "", false, fmt.Errorf("cancelled")
	}
	return res.email, res.password, false, nil
}

// SetPageReadyCallback registers fn to be called once the WebView finishes
// loading its HTML â€” use it to re-push status/settings so no update is missed.
func (w *SNCWindow) SetPageReadyCallback(fn func()) { w.onPageReady = fn }

// PushStatus encodes s as JSON and calls window.onStatusUpdate(s) in the WebView.
func (w *SNCWindow) PushStatus(s AppStatus) {
	b, _ := json.Marshal(s)
	windowPushStatus(b)
}

// PushClubTheme encodes the current club theme + header badge text as JSON
// and calls window.onClubThemeUpdate(...) in the WebView -- see
// tunnel_cat/docs/club-membership.md. theme is "" (regular), "catclub", or
// "elite"; badgeText is "" for the regular tier.
func (w *SNCWindow) PushClubTheme(theme, badgeText string, isAdmin, canRecommend bool) {
	b, _ := json.Marshal(struct {
		Theme        string `json:"theme"`
		Badge        string `json:"badge"`
		IsAdmin      bool   `json:"is_admin,omitempty"`
		CanRecommend bool   `json:"can_recommend,omitempty"`
	}{Theme: theme, Badge: badgeText, IsAdmin: isAdmin, CanRecommend: canRecommend})
	windowPushClubTheme(b)
}

// BuildAppMenu builds the NSApp main menu bar. Must be called after
// NewSNCWindow and SetMenuTray. Dispatched asynchronously to the main thread,
// so it safely enqueues after windowInit without any extra synchronisation.
func (w *SNCWindow) BuildAppMenu() { windowBuildAppMenu() }

// PushSettings encodes s as JSON and calls window.onSettingsUpdate(s) in the WebView.
// Also syncs the native menu bar checkmarks so they stay in step with the
// Settings tab without any separate update path.
func (w *SNCWindow) PushSettings(s AppSettings) {
	b, _ := json.Marshal(s)
	windowPushSettings(b)
	updateReady := globalTray != nil && globalTray.IsUpdateReady()
	windowSyncAppMenu(s.DoH, s.BlockQUIC, s.Region, updateReady)
}

// â”€â”€ HTML / CSS / JS â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// windowHTML returns the complete UI HTML for the macOS app window.
// Images are served via the sncasset:// custom URL scheme registered at startup.
func windowHTML() string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
` + theme.CSS + `
*{box-sizing:border-box;margin:0;padding:0}

body{
  font-family:var(--font-ui);
  background:var(--bg-primary);
  background-image:url("sncasset://bg.png");
  background-size:cover;
  background-position:center;
  background-attachment:fixed;
  color:var(--text-primary);
  height:100vh;
  display:flex;
  flex-direction:column;
  overflow:hidden;
  user-select:none;
  -webkit-user-select:none;
}

body::before{
  content:'';
  position:fixed;
  inset:0;
  background:rgba(7,9,13,0.78);
  pointer-events:none;
  z-index:0;
}

.app{position:relative;z-index:1;display:flex;flex-direction:column;height:100vh}

/* â”€â”€ Tab bar â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€ */
.tabs{
  display:flex;
  background:rgba(27,32,40,0.88);
  border-bottom:1px solid rgba(89,200,255,0.25);
  flex-shrink:0;
}
.tab{
  padding:11px 26px;cursor:pointer;font-size:12px;font-weight:600;
  letter-spacing:1.2px;color:rgba(153,162,176,0.8);
  border-bottom:2px solid transparent;transition:all .2s;
}
.tab:hover{color:rgba(89,200,255,0.85)}
.tab.active{color:var(--screen-cyan);border-bottom-color:var(--screen-cyan);
  text-shadow:0 0 10px rgba(89,200,255,0.5)}

/* â”€â”€ Club membership badge â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€ */
/* Not brand tokens -- palette specified directly for this feature (see
   tunnel_cat/docs/club-membership.md): Cat Club is light blue/white,
   Elite Cat Club matches beautysqrl.com's dark-brown/gold. Full-width bar
   pinned to the very top of the illustration (mirrors #status-bar's shape/
   position at the bottom) -- moved out of the tab bar 2026-08-15 per
   feedback ("должен быть поверх картинки, в самом её верху"). Hidden
   (display:none) for the regular tier -- see onClubThemeUpdate below. */
#club-badge{
  display:none;position:absolute;top:0;left:0;right:0;z-index:1;
  text-align:center;padding:9px 12px;font-size:12px;font-weight:700;
  letter-spacing:.4px;white-space:nowrap;
}
#club-badge.badge-catclub{background:#59C8FF;color:#FFFFFF}
#club-badge.badge-elite{background:#3B2412;color:#FFD700}

/* â”€â”€ Content area â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€ */
.content{flex:1;overflow:hidden;position:relative}
.panel{display:none;height:100%;flex-direction:column;
  align-items:center;padding:22px 26px}
.panel.active{display:flex}

/* â”€â”€ Tunnel panel â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€ */
/* The tunnel panel is filled edge-to-edge by the current state's kitten
   illustration; there is no separate small icon anywhere else in the UI. */
#panel-tunnel{position:relative;padding:0;overflow:hidden}
#cat-img{
  position:absolute;inset:0;
  width:100%;height:100%;object-fit:cover;
  z-index:0;pointer-events:none;
}

#tunnel-overlay{
  position:absolute;inset:0;z-index:1;
  display:flex;align-items:flex-start;justify-content:center;
  padding-top:10%;
}

.btn-row{display:flex;gap:12px}

/* â”€â”€ Convex pill Connect/Disconnect button â”€â”€ */
.btn-pill{
  position:relative;
  padding:14px 44px;border:none;border-radius:999px;
  font-size:14px;font-weight:700;letter-spacing:1.1px;
  color:#fff;cursor:pointer;text-transform:uppercase;
  background:linear-gradient(180deg,#4aa3ff 0%,#1f78e0 50%,#1560c0 100%);
  box-shadow:0 3px 0 #0c4a92,0 7px 16px rgba(0,0,0,0.45),
             inset 0 1px 0 rgba(255,255,255,0.35);
  transition:box-shadow .1s,transform .1s;
}
.btn-pill::before{
  content:'';position:absolute;left:8%;right:8%;top:10%;height:36%;
  border-radius:999px/100%;
  background:linear-gradient(180deg,rgba(255,255,255,0.55),rgba(255,255,255,0));
  pointer-events:none;
}
.btn-pill:active{
  transform:translateY(2px);
  background:linear-gradient(180deg,#1f78e0 0%,#1560c0 50%,#0f4a94 100%);
  box-shadow:0 1px 0 #0c4a92,0 2px 6px rgba(0,0,0,0.4),
             inset 0 1px 0 rgba(255,255,255,0.2);
}
.btn-disconnect{
  background:linear-gradient(180deg,#ff6a5a 0%,#e0331f 50%,#b8250f 100%);
  box-shadow:0 3px 0 #7c1808,0 7px 16px rgba(0,0,0,0.45),
             inset 0 1px 0 rgba(255,255,255,0.35);
}
.btn-disconnect:active{
  background:linear-gradient(180deg,#e0331f 0%,#b8250f 50%,#8f1c0b 100%);
  box-shadow:0 1px 0 #7c1808,0 2px 6px rgba(0,0,0,0.4),
             inset 0 1px 0 rgba(255,255,255,0.2);
}

/* â”€â”€ Bottom status bar â”€â”€ */
#status-bar{
  position:absolute;left:0;right:0;bottom:0;z-index:2;
  padding:10px 16px;text-align:center;
  color:#fff;font-size:13px;font-weight:600;letter-spacing:0.3px;
  background:#5b6470;
  transition:background-color .2s;
}
#status-bar.state-connected{background:#2e8b3d}
#status-bar.state-disconnected{background:#5b6470}
#status-bar.state-connecting{background:#e08a2e}
#status-bar.state-error{background:#c0392b}

/* â”€â”€ Settings panel â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€ */
#panel-settings{
  background:rgba(37,43,52,0.5);
  padding:22px 26px;
  align-items:stretch;
}

.settings-section{margin-bottom:20px}
.settings-section h3{
  font-size:11px;font-weight:700;letter-spacing:1.8px;text-transform:uppercase;
  color:rgba(89,200,255,0.75);margin-bottom:12px;
}
.srow{
  display:flex;align-items:center;justify-content:space-between;
  padding:9px 0;border-bottom:1px solid rgba(89,200,255,0.18);
}
.srow:last-child{border-bottom:none}
.srow-label{font-size:13px;color:rgba(244,241,232,0.9)}

.region-sel{
  background:rgba(7,9,13,0.8);border:1px solid rgba(89,200,255,0.3);
  border-radius:6px;color:var(--text-primary);padding:5px 8px;font-size:12px;
  outline:none;cursor:pointer;-webkit-appearance:auto;
}
.region-sel:focus{border-color:rgba(89,200,255,0.55)}

.srow input[type=checkbox]{
  width:16px;height:16px;cursor:pointer;accent-color:var(--screen-cyan);
}
</style>
</head>
<body>
<div class="app">

  <div class="tabs">
    <div class="tab active" onclick="switchTab('tunnel',this)">TUNNEL</div>
    <div class="tab"        onclick="switchTab('settings',this)">SETTINGS</div>
  </div>

  <div class="content">

    <div id="panel-tunnel" class="panel active">
      <div id="club-badge"></div>
      <img id="cat-img" src="sncasset://illustration_idle.png" alt="">
      <div id="tunnel-overlay">
        <div class="btn-row">
          <button id="btn-connect"    class="btn-pill"
                  onclick="handleConnect()">Connect</button>
          <button id="btn-disconnect" class="btn-pill btn-disconnect"
                  style="display:none" onclick="handleDisconnect()">Disconnect</button>
        </div>
      </div>
      <div id="status-bar" class="state-disconnected">Disconnected</div>
    </div>

    <div id="panel-settings" class="panel">
      <div class="settings-section">
        <h3>Your Location</h3>
        <div class="srow">
          <div class="srow-label">Region</div>
          <select class="region-sel" id="s-region" onchange="saveSettings()">
            <option value="">Auto</option>
            <option value="RU">Russia</option>
            <option value="EU">Europe</option>
            <option value="US">USA</option>
            <option value="CN">China</option>
            <option value="XX">Other</option>
          </select>
        </div>
      </div>
      <div class="settings-section">
        <h3>Connection Options</h3>
        <div class="srow">
          <div class="srow-label">DoH &#8212; DNS over HTTPS</div>
          <input type="checkbox" id="s-doh" onchange="saveSettings()">
        </div>
        <div class="srow" id="s-block-quic-row">
          <div class="srow-label">Disable QUIC</div>
          <input type="checkbox" id="s-block-quic" onchange="saveSettings()">
        </div>
      </div>
      <div class="settings-section" id="club-theme-section" style="display:none">
        <h3>Club Theme (admin preview)</h3>
        <div class="srow">
          <div class="srow-label">Theme</div>
          <select class="region-sel" id="s-club-theme" onchange="submitClubThemePreview()">
            <option value="">Regular</option>
            <option value="catclub">Cat Club</option>
            <option value="elite">Elite Cat Club</option>
          </select>
        </div>
      </div>
      <div class="settings-section" id="recommend-section" style="display:none">
        <h3>Recommend new Cat Club members</h3>
        <div class="srow" style="flex-direction:column;align-items:stretch;gap:8px">
          <input type="text" id="recommend-username" placeholder="username to recommend"
                 style="background:rgba(7,9,13,0.8);border:1px solid rgba(89,200,255,0.3);
                        border-radius:6px;color:var(--text-primary);padding:7px 10px;font-size:13px;outline:none">
          <button onclick="submitRecommend()"
                  style="background:rgba(89,200,255,0.15);border:1px solid rgba(89,200,255,0.4);
                         border-radius:6px;color:var(--screen-cyan);padding:7px 10px;font-size:12px;
                         font-weight:600;cursor:pointer">Recommend</button>
          <div id="recommend-status" style="font-size:11px;color:rgba(153,162,176,0.8)"></div>
        </div>
      </div>
    </div>

  </div>
</div>

<script>
function switchTab(name, el) {
  document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
  document.querySelectorAll('.panel').forEach(p => p.classList.remove('active'));
  el.classList.add('active');
  document.getElementById('panel-' + name).classList.add('active');
}

function handleConnect()    { window.webkit.messageHandlers.sncConnect.postMessage({}) }
function handleDisconnect() { window.webkit.messageHandlers.sncDisconnect.postMessage({}) }

function saveSettings() {
  window.webkit.messageHandlers.sncSetSettings.postMessage({
    doh:       document.getElementById('s-doh').checked,
    blockQUIC: document.getElementById('s-block-quic').checked,
    region:    document.getElementById('s-region').value,
  });
}

// window.clubTheme is "" (regular), "catclub", or "elite" -- set by
// onClubThemeUpdate below, once membership is confirmed. catAssetURL
// appends the matching theme suffix to whichever illustration state
// onStatusUpdate wants to show, so the two concerns (connection state,
// club theme) stay independent of each other.
window.clubTheme = '';
function catAssetURL(state) {
  var suffix = window.clubTheme === 'catclub' ? '_catclub'
             : window.clubTheme === 'elite'   ? '_elite' : '';
  return 'sncasset://illustration_' + state + suffix + '.png';
}

window.onClubThemeUpdate = function(t) {
  window.clubTheme = t.theme || '';
  var badge = document.getElementById('club-badge');
  badge.classList.remove('badge-catclub', 'badge-elite');
  if (t.badge) {
    badge.textContent = t.badge;
    badge.classList.add(window.clubTheme === 'elite' ? 'badge-elite' : 'badge-catclub');
    badge.style.display = '';
  } else {
    badge.style.display = 'none';
  }
  // Re-apply the current connection state so the illustration picks up
  // the new theme immediately, not just on the next status change.
  if (window.lastStatus) window.onStatusUpdate(window.lastStatus);
  // can_recommend is its own signal, not window.clubTheme -- an admin
  // previewing a theme is not necessarily a real Cat Club/Elite member, so
  // gating Recommend visibility on the (possibly preview-overridden) theme
  // would wrongly show it during a non-member admin's preview.
  document.getElementById('recommend-section').style.display = t.can_recommend ? '' : 'none';
  var themeSection = document.getElementById('club-theme-section');
  themeSection.style.display = t.is_admin ? '' : 'none';
  if (t.is_admin) {
    document.getElementById('s-club-theme').value = window.clubTheme;
  }
};

function submitClubThemePreview() {
  var theme = document.getElementById('s-club-theme').value;
  window.webkit.messageHandlers.sncClubThemePreview.postMessage(theme);
}

function submitRecommend() {
  var input  = document.getElementById('recommend-username');
  var status = document.getElementById('recommend-status');
  var username = input.value.trim();
  if (!username) return;
  window.webkit.messageHandlers.sncRecommend.postMessage(username);
  status.textContent = 'Recommendation sent for ' + username + '.';
  input.value = '';
}

window.onStatusUpdate = function(s) {
  window.lastStatus = s;
  var img  = document.getElementById('cat-img');
  var bar  = document.getElementById('status-bar');
  var bcon = document.getElementById('btn-connect');
  var bdis = document.getElementById('btn-disconnect');

  bar.className = '';
  if (s.error) {
    bar.classList.add('state-error');
    bar.textContent = s.errorMsg || 'Error';
    bcon.style.display = ''; bdis.style.display = 'none';
    img.src = catAssetURL('error'); return;
  }
  if (s.disconnecting) {
    bar.classList.add('state-connecting');
    bar.textContent = 'Disconnecting...';
    bcon.style.display = 'none'; bdis.style.display = 'none';
    img.src = catAssetURL('connecting'); return;
  }
  if (s.connecting) {
    bar.classList.add('state-connecting');
    bar.textContent = 'Connecting...';
    bcon.style.display = 'none'; bdis.style.display = '';
    img.src = catAssetURL('connecting'); return;
  }
  if (s.connected) {
    bar.classList.add('state-connected');
    bar.textContent = 'Connected';
    bcon.style.display = 'none'; bdis.style.display = '';
    img.src = catAssetURL('connected');
    return;
  }
  bar.classList.add('state-disconnected');
  bar.textContent = 'Disconnected';
  bcon.style.display = ''; bdis.style.display = 'none';
  img.src = catAssetURL('idle');
};

window.onSettingsUpdate = function(s) {
  document.getElementById('s-doh').checked        = !!s.doh;
  document.getElementById('s-block-quic').checked = !!s.blockQUIC;
  document.getElementById('s-region').value       = s.region || '';
};

// Notify Go that the page is ready so it can re-push the current status.
// This handles the race where status updates arrive before onStatusUpdate
// is defined (WebView still loading HTML).
window.webkit.messageHandlers.sncPageReady.postMessage({});
</script>
</body>
</html>`
}
