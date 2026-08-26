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
	"strings"

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
		"illustration_wildcat.png", "illustration_error.png",
		// Club-theme variants (see tunnel_cat/docs/club-membership.md) --
		// same five states, palette-only variants selected client-side by
		// appending "_catclub"/"_elite" to the filename (see onClubThemeUpdate
		// in windowHTML's JS below). Registered unconditionally; a regular
		// (non-member) user's JS just never references these URLs.
		"illustration_idle_catclub.png", "illustration_connecting_catclub.png",
		"illustration_connected_catclub.png", "illustration_wildcat_catclub.png",
		"illustration_error_catclub.png",
		"illustration_idle_elite.png", "illustration_connecting_elite.png",
		"illustration_connected_elite.png", "illustration_wildcat_elite.png",
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

// PushBytes encodes the live cumulative uplink/downlink byte counters as JSON
// and calls window.onBytesUpdate(...) in the WebView. Called once a second
// while connected, and once with (0, 0) on disconnect -- see the byte-ticker
// in cmd/shortnerdcat/main_darwin.go's onConnect/onDisconnect.
func (w *SNCWindow) PushBytes(sent, recv int64) {
	b, _ := json.Marshal(struct {
		Sent int64 `json:"sent"`
		Recv int64 `json:"recv"`
	}{Sent: sent, Recv: recv})
	windowPushBytes(b)
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
	quicLocked := globalTray != nil && globalTray.IsWildcatQUICLocked()
	windowSyncAppMenu(s.DoH, s.BlockQUIC, s.Wildcat, s.Region, updateReady, quicLocked)
}

// â”€â”€ HTML / CSS / JS â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// resolveTTokens replaces every "{{T:key}}" occurrence in s with T(key)'s
// result (see i18n_darwin.go / strings_darwin.go). Used by windowHTML() to
// localize the embedded WebView UI without templating the whole string with
// text/template -- the HTML/CSS/JS below is large and mostly non-localized
// markup, so a minimal token scanner keeps the diff small and avoids
// escaping concerns text/template would introduce for the inline <script>.
func resolveTTokens(s string) string {
	const open = "{{T:"
	const close = "}}"
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		afterOpen := rest[i+len(open):]
		j := strings.Index(afterOpen, close)
		if j < 0 {
			// Malformed token (missing "}}") -- emit the rest verbatim rather
			// than looping forever or silently eating text.
			b.WriteString(rest[i:])
			break
		}
		key := afterOpen[:j]
		b.WriteString(T(key))
		rest = afterOpen[j+len(close):]
	}
	return b.String()
}

// windowHTML returns the complete UI HTML for the macOS app window.
// Images are served via the sncasset:// custom URL scheme registered at startup.
// User-facing text is written as "{{T:key}}" tokens resolved by
// resolveTTokens against strings_darwin.go's stringsEN/stringsRU tables.
func windowHTML() string {
	return resolveTTokens(`<!DOCTYPE html>
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
#status-bar.state-wildcat{background:#000}

/* â”€â”€ Live uplink/downlink counter â”€â”€ */
/* Sits just above #status-bar, bottom-right of the illustration. Hidden by
   default; shown only while connected (see window.onStatusUpdate /
   window.onBytesUpdate below) -- there is nothing meaningful to show while
   idle/connecting/error, and the daemon stops pushing updates in that state
   anyway (see the byte-ticker in cmd/shortnerdcat/main_darwin.go). */
#byte-counter{
  position:absolute;right:10px;bottom:94px;z-index:2;
  display:none;
  padding:3px 9px;border-radius:6px;
  background:rgba(0,0,0,0.45);
  color:#fff;font-size:11px;font-weight:600;letter-spacing:0.2px;
  font-variant-numeric:tabular-nums;
  pointer-events:none;
}

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
    <div class="tab active" onclick="switchTab('tunnel',this)">{{T:html_tab_tunnel}}</div>
    <div class="tab"        onclick="switchTab('settings',this)">{{T:html_tab_settings}}</div>
  </div>

  <div class="content">

    <div id="panel-tunnel" class="panel active">
      <div id="club-badge"></div>
      <img id="cat-img" src="sncasset://illustration_idle.png" alt="">
      <div id="tunnel-overlay">
        <div class="btn-row">
          <button id="btn-connect"    class="btn-pill"
                  onclick="handleConnect()">{{T:html_btn_connect}}</button>
          <button id="btn-disconnect" class="btn-pill btn-disconnect"
                  style="display:none" onclick="handleDisconnect()">{{T:html_btn_disconnect}}</button>
        </div>
      </div>
      <div id="byte-counter"></div>
      <div id="status-bar" class="state-disconnected">{{T:html_status_disconnected}}</div>
    </div>

    <div id="panel-settings" class="panel">
      <div class="settings-section">
        <h3>{{T:html_settings_location}}</h3>
        <div class="srow">
          <div class="srow-label">{{T:html_region_label}}</div>
          <select class="region-sel" id="s-region" onchange="saveSettings()">
            <option value="">{{T:html_region_auto}}</option>
            <option value="RU">{{T:html_region_ru}}</option>
            <option value="EU">{{T:html_region_eu}}</option>
            <option value="US">{{T:html_region_us}}</option>
            <option value="CN">{{T:html_region_cn}}</option>
            <option value="XX">{{T:html_region_other}}</option>
          </select>
        </div>
      </div>
      <div class="settings-section">
        <h3>{{T:html_settings_connection}}</h3>
        <div class="srow">
          <div class="srow-label">{{T:html_wildcat_label}}</div>
          <input type="checkbox" id="s-wildcat" onchange="saveSettings()">
        </div>
        <div class="srow">
          <div class="srow-label">{{T:html_doh_label}}</div>
          <input type="checkbox" id="s-doh" onchange="saveSettings()">
        </div>
        <div class="srow" id="s-block-quic-row">
          <div class="srow-label">{{T:html_block_quic_label}}</div>
          <input type="checkbox" id="s-block-quic" onchange="saveSettings()">
        </div>
      </div>
      <div class="settings-section" id="club-theme-section" style="display:none">
        <h3>{{T:html_club_theme_section}}</h3>
        <div class="srow">
          <div class="srow-label">{{T:html_club_theme_label}}</div>
          <select class="region-sel" id="s-club-theme" onchange="submitClubThemePreview()">
            <option value="">{{T:html_club_theme_regular}}</option>
            <option value="catclub">{{T:html_club_theme_catclub}}</option>
            <option value="elite">{{T:html_club_theme_elite}}</option>
          </select>
        </div>
      </div>
      <div class="settings-section" id="recommend-section" style="display:none">
        <h3>{{T:html_recommend_section}}</h3>
        <div class="srow" style="flex-direction:column;align-items:stretch;gap:8px">
          <input type="text" id="recommend-username" placeholder="{{T:html_recommend_placeholder}}"
                 style="background:rgba(7,9,13,0.8);border:1px solid rgba(89,200,255,0.3);
                        border-radius:6px;color:var(--text-primary);padding:7px 10px;font-size:13px;outline:none">
          <button onclick="submitRecommend()"
                  style="background:rgba(89,200,255,0.15);border:1px solid rgba(89,200,255,0.4);
                         border-radius:6px;color:var(--screen-cyan);padding:7px 10px;font-size:12px;
                         font-weight:600;cursor:pointer">{{T:html_recommend_button}}</button>
          <div id="recommend-status" style="font-size:11px;color:rgba(153,162,176,0.8)"></div>
        </div>
      </div>
    </div>

  </div>
</div>

<script>
// I18N holds Go-resolved translations for strings the JS builds dynamically
// (status-bar text, recommend confirmation) rather than embedding directly
// in markup -- see resolveTTokens() in window_darwin.go.
window.I18N = {
  error:            "{{T:html_js_error}}",
  disconnecting:    "{{T:html_js_disconnecting}}",
  connecting:       "{{T:html_js_connecting}}",
  connectedWildcat: "{{T:html_js_connected_wildcat}}",
  connected:        "{{T:html_js_connected}}",
  disconnected:     "{{T:html_js_disconnected}}",
  recommendSentPrefix: "{{T:html_recommend_status_prefix}}"
};

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
    wildcat:   document.getElementById('s-wildcat').checked,
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
  status.textContent = window.I18N.recommendSentPrefix + username + '.';
  input.value = '';
}

// formatBytes renders a byte count like "1.2 MB" / "45.3 MB" / "3 B".
// Binary (1024-based) units, one decimal place above 1 KB.
function formatBytes(n) {
  n = n || 0;
  var units = ['B', 'KB', 'MB', 'GB', 'TB'];
  var i = 0;
  var v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  var str = i === 0 ? String(Math.round(v)) : v.toFixed(1);
  return str + ' ' + units[i];
}

// onBytesUpdate updates the "sent / recv" counter -- pushed once a second
// while connected by the daemon (see BytesSent/BytesRecv in macos/ipc.go).
// The element's visibility itself is driven by onStatusUpdate below (hidden
// outside the connected state), not by this function, so a stray late push
// racing a disconnect can't leave it shown.
window.onBytesUpdate = function(b) {
  var el = document.getElementById('byte-counter');
  if (!el) return;
  el.textContent = '↑ ' + formatBytes(b && b.sent) + '   ↓ ' + formatBytes(b && b.recv);
};

window.onStatusUpdate = function(s) {
  window.lastStatus = s;
  var img  = document.getElementById('cat-img');
  var bar  = document.getElementById('status-bar');
  var bcon = document.getElementById('btn-connect');
  var bdis = document.getElementById('btn-disconnect');
  var byteEl = document.getElementById('byte-counter');

  bar.className = '';
  if (s.error) {
    bar.classList.add('state-error');
    bar.textContent = s.errorMsg || window.I18N.error;
    bcon.style.display = ''; bdis.style.display = 'none';
    if (byteEl) byteEl.style.display = 'none';
    img.src = catAssetURL('error'); return;
  }
  if (s.disconnecting) {
    bar.classList.add('state-connecting');
    bar.textContent = window.I18N.disconnecting;
    bcon.style.display = 'none'; bdis.style.display = 'none';
    if (byteEl) byteEl.style.display = 'none';
    img.src = catAssetURL('connecting'); return;
  }
  if (s.connecting) {
    bar.classList.add('state-connecting');
    bar.textContent = window.I18N.connecting;
    bcon.style.display = 'none'; bdis.style.display = '';
    if (byteEl) byteEl.style.display = 'none';
    img.src = catAssetURL('connecting'); return;
  }
  if (s.connected) {
    var isWildcat = s.mode === 'wildcat';
    bar.classList.add(isWildcat ? 'state-wildcat' : 'state-connected');
    bar.textContent = isWildcat ? window.I18N.connectedWildcat : window.I18N.connected;
    bcon.style.display = 'none'; bdis.style.display = '';
    if (byteEl) byteEl.style.display = '';
    img.src = catAssetURL(isWildcat ? 'wildcat' : 'connected');
    return;
  }
  bar.classList.add('state-disconnected');
  bar.textContent = window.I18N.disconnected;
  bcon.style.display = ''; bdis.style.display = 'none';
  if (byteEl) byteEl.style.display = 'none';
  img.src = catAssetURL('idle');
};

window.onSettingsUpdate = function(s) {
  document.getElementById('s-wildcat').checked    = !!s.wildcat;
  document.getElementById('s-doh').checked        = !!s.doh;
  document.getElementById('s-block-quic').checked = !!s.blockQUIC;
  document.getElementById('s-region').value       = s.region || '';
  // WildCat forces QUIC blocked for the session -- hide the row entirely
  // rather than just greying it (checkbox state above is left untouched,
  // so it reads correctly again the moment WildCat releases the lock).
  document.getElementById('s-block-quic-row').style.display = s.quicLocked ? 'none' : '';
};

// Notify Go that the page is ready so it can re-push the current status.
// This handles the race where status updates arrive before onStatusUpdate
// is defined (WebView still loading HTML).
window.webkit.messageHandlers.sncPageReady.postMessage({});
</script>
</body>
</html>`)
}
