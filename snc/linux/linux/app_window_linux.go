// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

// app_window_linux.go â€” GTK/WebKit2GTK app window (Tunnel Cat UI).
//
// Architecture mirrors the macOS WKWebView window:
//   - Same HTML/CSS/JS served via webkit_web_view_load_html.
//   - PNG assets are embedded as base64 data URIs (no custom URI scheme needed).
//   - JSâ†’Go calls use WebKit script message handlers (same API as WKWebView:
//     window.webkit.messageHandlers.X.postMessage(â€¦)).
//   - Goâ†’JS calls use schedule_app_win_run_js (defined in app_window_linux.c)
//     which schedules execution on the GTK main thread via g_idle_add.
//
// All C implementation lives in app_window_linux.c to avoid CGO multiple-
// definition link errors (CGO inlines Go-file C preambles into several
// generated .c files, so any non-trivial definition there gets duplicated).

/*
#include <webkit2/webkit2.h>
#include <stdlib.h>

// Functions defined in app_window_linux.c.
int  app_win_is_created(void);
void schedule_app_win_create(gpointer data);
void schedule_app_win_show(void);
void schedule_app_win_run_js(gpointer data);
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"unsafe"

	"shortnerdcat/snc/shared/theme"
	"tunnel_cat/snc/core"
)

// AppWindowCallbacks holds the Go handlers for app-window events.
type AppWindowCallbacks struct {
	OnConnect    func()
	OnDisconnect func()
	OnSettings   func(AppSettings)
	OnPageReady  func()
	OnKey        func(string) // user submitted an activation key from the login panel

	// OnHaveKeyAnswer fires when the user answers the initial "do you have a
	// key?" prompt. true -> key-entry panel; false -> credential-login panel
	// (if navlink.net was reachable) or key-entry panel otherwise.
	OnHaveKeyAnswer func(hasKey bool)
	// OnCredentialLogin fires when the user submits email+password on the
	// credential-login panel. The callback is responsible for calling
	// navlinkauth and, on success, feeding the resulting key into the same
	// path as OnKey (e.g. by invoking OnKey itself).
	OnCredentialLogin func(email, password string)
	// OnKeyModeSwitch fires when the user clicks "I Have a Key" on the
	// credential-login panel, requesting a switch back to key entry.
	OnKeyModeSwitch func()
	// OnRecommend fires when the user submits the "Recommend new Cat Club
	// members" form in the Settings panel -- see
	// tunnel_cat/docs/club-membership.md. The daemon holds the session
	// token needed to actually call the arbiter, so this typically just
	// forwards to the daemon via IPC (mirrors the macOS client).
	OnRecommend func(username string)
	// OnClubThemePreview fires when an admin picks a theme from the
	// Settings panel's Club Theme selector (theme="regular"/"catclub"/
	// "elite") -- purely a client-side UI convenience (see keyenc.go's
	// IsAdmin doc comment: "never a real permission"), no tunnel/session
	// round trip needed.
	OnClubThemePreview func(theme string)
}

// AppWindow wraps the GTK/WebKit2GTK app window.
type AppWindow struct {
	mu      sync.Mutex
	created bool
	cbs     AppWindowCallbacks
}

// globalAppWin is referenced by the CGo export callbacks below.
var globalAppWin *AppWindow

// NewAppWindow registers callbacks and returns the window handle.
// Call Show() to create and display the window on the GTK main thread.
func NewAppWindow(cbs AppWindowCallbacks) *AppWindow {
	w := &AppWindow{cbs: cbs}
	globalAppWin = w
	return w
}

// Show creates the window on first call; focuses it on subsequent calls.
// Safe to call from any goroutine â€” GTK work is scheduled via g_idle_add.
func (w *AppWindow) Show() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.created {
		w.created = true
		html := buildAppWindowHTML()
		cs := C.CString(html)
		C.schedule_app_win_create(C.gpointer(unsafe.Pointer(cs)))
	} else {
		C.schedule_app_win_show()
	}
}

// RunJS evaluates js in the WebView on the GTK main thread.
func (w *AppWindow) RunJS(js string) {
	if C.app_win_is_created() == 0 {
		return
	}
	cs := C.CString(js)
	C.schedule_app_win_run_js(C.gpointer(unsafe.Pointer(cs)))
}

// PushStatus sends the current tunnel state to the window's JS.
func (w *AppWindow) PushStatus(s AppStatus) {
	b, _ := json.Marshal(s)
	w.RunJS("if(window.onStatusUpdate)window.onStatusUpdate(" + string(b) + ")")
}

// PushSettings sends the current settings to the window's JS.
func (w *AppWindow) PushSettings(s AppSettings) {
	b, _ := json.Marshal(s)
	w.RunJS("if(window.onSettingsUpdate)window.onSettingsUpdate(" + string(b) + ")")
}

// PushClubTheme sends the current club theme + header badge text + live
// admin status + Cat Club recommend eligibility to the window's JS -- see
// tunnel_cat/docs/club-membership.md. theme is "" (regular), "catclub", or
// "elite"; badgeText is "" for the regular tier. isAdmin gates the Club
// Theme preview selector; canRecommend gates the Recommend section.
func (w *AppWindow) PushClubTheme(theme, badgeText string, isAdmin, canRecommend bool) {
	b, _ := json.Marshal(struct {
		Theme        string `json:"theme"`
		Badge        string `json:"badge"`
		IsAdmin      bool   `json:"is_admin"`
		CanRecommend bool   `json:"can_recommend"`
	}{Theme: theme, Badge: badgeText, IsAdmin: isAdmin, CanRecommend: canRecommend})
	w.RunJS("if(window.onClubThemeUpdate)window.onClubThemeUpdate(" + string(b) + ")")
}

// RunLoginError shows an error message in the login panel's key-entry overlay.
func (w *AppWindow) RunLoginError(msg string) {
	b, _ := json.Marshal(msg)
	w.RunJS("if(window.onLoginError)window.onLoginError(" + string(b) + ")")
}

// RunCredentialError shows an error message on the credential-login panel
// (e.g. "Invalid email or password") without dismissing it, so the user can
// retry.
func (w *AppWindow) RunCredentialError(msg string) {
	b, _ := json.Marshal(msg)
	w.RunJS("if(window.onCredentialError)window.onCredentialError(" + string(b) + ")")
}

// PushNavlinkReachable tells the window's JS whether navlink.net is reachable
// directly right now, so it can decide whether to offer the credential-login
// path at all. Call once, as soon as the probe (see navlinkauth.Probe)
// resolves — before that, the UI conservatively behaves as unreachable.
func (w *AppWindow) PushNavlinkReachable(reachable bool) {
	v := "false"
	if reachable {
		v = "true"
	}
	w.RunJS("if(window.onNavlinkReachable)window.onNavlinkReachable(" + v + ")")
}

// â”€â”€ CGo export callbacks (called from C signal handlers in app_window_linux.c) â”€â”€

//export goAppWinConnect
func goAppWinConnect() {
	if globalAppWin != nil && globalAppWin.cbs.OnConnect != nil {
		go globalAppWin.cbs.OnConnect()
	}
}

//export goAppWinDisconnect
func goAppWinDisconnect() {
	if globalAppWin != nil && globalAppWin.cbs.OnDisconnect != nil {
		go globalAppWin.cbs.OnDisconnect()
	}
}

//export goAppWinSettings
func goAppWinSettings(jsonStr *C.char) {
	if globalAppWin == nil || globalAppWin.cbs.OnSettings == nil {
		return
	}
	var s AppSettings
	if err := json.Unmarshal([]byte(C.GoString(jsonStr)), &s); err != nil {
		core.Log.Printf("app-window: parse settings: %v", err)
		return
	}
	go globalAppWin.cbs.OnSettings(s)
}

//export goAppWinPageReady
func goAppWinPageReady() {
	if globalAppWin != nil && globalAppWin.cbs.OnPageReady != nil {
		go globalAppWin.cbs.OnPageReady()
	}
}

//export goAppWinKey
func goAppWinKey(keyStr *C.char) {
	if globalAppWin == nil || globalAppWin.cbs.OnKey == nil {
		return
	}
	go globalAppWin.cbs.OnKey(C.GoString(keyStr))
}

//export goAppWinRecommend
func goAppWinRecommend(usernameStr *C.char) {
	if globalAppWin == nil || globalAppWin.cbs.OnRecommend == nil {
		return
	}
	go globalAppWin.cbs.OnRecommend(C.GoString(usernameStr))
}

//export goAppWinClubThemePreview
func goAppWinClubThemePreview(themeStr *C.char) {
	if globalAppWin == nil || globalAppWin.cbs.OnClubThemePreview == nil {
		return
	}
	go globalAppWin.cbs.OnClubThemePreview(C.GoString(themeStr))
}

//export goAppWinHaveKeyAnswer
func goAppWinHaveKeyAnswer(hasKey C.int) {
	if globalAppWin == nil || globalAppWin.cbs.OnHaveKeyAnswer == nil {
		return
	}
	go globalAppWin.cbs.OnHaveKeyAnswer(hasKey != 0)
}

//export goAppWinCredentialLogin
func goAppWinCredentialLogin(jsonStr *C.char) {
	if globalAppWin == nil || globalAppWin.cbs.OnCredentialLogin == nil {
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(C.GoString(jsonStr)), &req); err != nil {
		core.Log.Printf("app-window: parse credential login: %v", err)
		return
	}
	go globalAppWin.cbs.OnCredentialLogin(req.Email, req.Password)
}

//export goAppWinKeyModeSwitch
func goAppWinKeyModeSwitch() {
	if globalAppWin == nil || globalAppWin.cbs.OnKeyModeSwitch == nil {
		return
	}
	go globalAppWin.cbs.OnKeyModeSwitch()
}

// goAppWinLoadFailed and goAppWinProcessTerminated exist so a real
// page-load or WebKit-renderer-process failure is visible in the client's
// own log (already collected and periodically uploaded, see
// core.InitLogging/core.LogUploader in main_linux.go) instead of being
// completely silent -- see app_window_linux.c's on_wv_load_failed doc
// comment for the gap this closes.

//export goAppWinLoadFailed
func goAppWinLoadFailed(message *C.char) {
	core.Log.Printf("app-window: WebKit load-failed: %s", C.GoString(message))
}

//export goAppWinProcessTerminated
func goAppWinProcessTerminated(reason C.int) {
	core.Log.Printf("app-window: WebKit web process terminated, reason=%d", int(reason))
}

// â”€â”€ HTML generation â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// buildAppWindowHTML returns the complete window HTML with PNG assets embedded
// as base64 data URIs so no custom URI scheme is needed.
func buildAppWindowHTML() string {
	html := appWindowHTMLTemplate
	// Deliberately separate files from snc_idle.png etc., which tray_linux.go
	// embeds for the systray status icon -- those used to be the same files;
	// redesigning one silently broke the other (a tiny tray icon rendered
	// from a full illustration looks nothing like the intended icon). Keep
	// them independent.
	for _, name := range []string{
		"illustration_idle.png", "illustration_connecting.png", "illustration_connected.png",
		"illustration_error.png",
		// Club-theme variants (see tunnel_cat/docs/club-membership.md) --
		// same five states, palette-only variants. Their sncasset:// tokens
		// appear in the JS catAssets map below, not as <img> src literals,
		// so this ReplaceAll pass is the only place they get baked in.
		"illustration_idle_catclub.png", "illustration_connecting_catclub.png",
		"illustration_connected_catclub.png",
		"illustration_error_catclub.png",
		"illustration_idle_elite.png", "illustration_connecting_elite.png",
		"illustration_connected_elite.png",
		"illustration_error_elite.png",
	} {
		data := readAsset(name)
		if len(data) == 0 {
			continue
		}
		uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
		html = strings.ReplaceAll(html, "sncasset://"+name, uri)
	}
	return html
}

// appWindowHTMLTemplate is the UI rendered inside the WebKit2GTK view.
// Asset URLs use the sncasset:// placeholder replaced at runtime with base64.
// JS message handler API is identical to the macOS WKWebView implementation.
const appWindowHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
` + theme.CSS + `
*{box-sizing:border-box;margin:0;padding:0}
body{
  font-family:var(--font-ui);
  background:var(--bg-primary);color:var(--text-primary);height:100vh;
  display:flex;flex-direction:column;overflow:hidden;
  user-select:none;-webkit-user-select:none;
}
.app{display:flex;flex-direction:column;height:100vh;position:relative}
.tabs{
  display:flex;background:rgba(27,32,40,0.88);
  border-bottom:1px solid rgba(89,200,255,0.25);flex-shrink:0;
}
.tab{
  padding:11px 26px;cursor:pointer;font-size:12px;font-weight:600;
  letter-spacing:1.2px;color:rgba(153,162,176,0.8);
  border-bottom:2px solid transparent;transition:all .2s;
}
.tab:hover{color:rgba(89,200,255,0.85)}
.tab.active{color:var(--screen-cyan);border-bottom-color:var(--screen-cyan);
  text-shadow:0 0 10px rgba(89,200,255,0.5)}
/* â”€â”€ Club membership badge â”€â”€ not brand tokens, palette specified directly
   for this feature (see tunnel_cat/docs/club-membership.md): Cat Club is
   light blue/white, Elite Cat Club matches beautysqrl.com's dark-brown/gold.
   Full-width bar pinned to the very top of the illustration (mirrors the
   bottom status bar's shape/position) -- moved out of the tab bar 2026-08-15
   per feedback ("должен быть поверх картинки, в самом её верху"). */
#club-badge{
  display:none;position:absolute;top:0;left:0;right:0;z-index:1;
  text-align:center;padding:9px 12px;font-size:12px;font-weight:700;
  letter-spacing:.4px;white-space:nowrap;
}
#club-badge.badge-catclub{background:#59C8FF;color:#FFFFFF}
#club-badge.badge-elite{background:#3B2412;color:#FFD700}
.content{flex:1;overflow:hidden;position:relative}
.panel{display:none;height:100%;flex-direction:column;
  align-items:center;padding:22px 26px}
.panel.active{display:flex}
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
  display:flex;align-items:center;justify-content:center;
  padding-bottom:44px; /* keep the button clear of the status bar */
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
.btn-pill-disconnect{
  background:linear-gradient(180deg,#ff6a5a 0%,#e0331f 50%,#b8250f 100%);
  box-shadow:0 3px 0 #7c1808,0 7px 16px rgba(0,0,0,0.45),
             inset 0 1px 0 rgba(255,255,255,0.35);
}
.btn-pill-disconnect:active{
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
.btn{
  padding:11px 30px;border:none;border-radius:8px;font-size:13px;
  font-weight:700;letter-spacing:1.2px;cursor:pointer;transition:all .2s;
  text-transform:uppercase;
}
.btn-connect{
  background:linear-gradient(130deg,var(--screen-cyan),var(--nerd-violet));color:#fff;
  box-shadow:0 0 22px rgba(89,200,255,0.35);
}
.btn-connect:hover{background:linear-gradient(130deg,var(--screen-cyan),var(--nerd-violet))}
.btn-disconnect{
  background:rgba(180,30,30,0.18);border:1px solid rgba(255,50,50,0.35);color:#ff6060;
}
.btn-disconnect:hover{background:rgba(200,40,40,0.38)}
#panel-settings{background:rgba(37,43,52,0.5);padding:22px 26px;align-items:stretch}
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
  outline:none;cursor:pointer;
}
.srow input[type=checkbox]{width:16px;height:16px;cursor:pointer;accent-color:var(--screen-cyan)}
/* Login overlay â€” shown when not logged in */
#login-overlay{
  display:none;position:absolute;inset:0;z-index:10;
  background:var(--bg-primary);flex-direction:column;align-items:center;
  justify-content:center;padding:28px 26px;gap:14px;
}
#login-overlay.visible{display:flex}
#login-overlay img{width:90px;height:90px;object-fit:contain;
  filter:drop-shadow(0 0 12px rgba(89,200,255,0.3))}
#login-overlay h2{font-size:15px;font-weight:700;letter-spacing:0.5px;
  color:var(--text-primary);text-align:center}
#login-overlay p{font-size:12px;color:rgba(153,162,176,0.9);text-align:center}
#key-input{
  width:100%;min-height:80px;
  background:var(--kitten-black);border:1px solid rgba(89,200,255,0.4);
  border-radius:8px;color:var(--text-primary);padding:10px;font-size:12px;
  font-family:var(--font-mono);resize:none;outline:none;
  user-select:text;-webkit-user-select:text;
}
#key-input:focus{border-color:rgba(89,200,255,0.7)}
#login-err,#credential-err{color:#ff7070;font-size:12px;text-align:center;display:none}
#btn-activate,#btn-login-switch,#btn-do-login,#btn-key-mode-switch{width:100%}
#have-key-overlay{
  display:none;position:absolute;inset:0;z-index:10;
  background:var(--bg-primary);flex-direction:column;align-items:center;
  justify-content:center;padding:28px 26px;gap:14px;
}
#have-key-overlay.visible{display:flex}
#have-key-overlay h2{font-size:15px;font-weight:700;letter-spacing:0.5px;
  color:var(--text-primary);text-align:center}
#have-key-row{display:flex;gap:12px;width:100%}
#have-key-row .btn{flex:1;text-transform:none;letter-spacing:0.2px}
#credential-login-overlay{
  display:none;position:absolute;inset:0;z-index:10;
  background:var(--bg-primary);flex-direction:column;align-items:center;
  justify-content:center;padding:28px 26px;gap:10px;
}
#credential-login-overlay.visible{display:flex}
#credential-login-overlay h2{font-size:15px;font-weight:700;letter-spacing:0.5px;
  color:var(--text-primary);text-align:center;margin-bottom:4px}
.cred-input{
  width:100%;background:var(--kitten-black);border:1px solid rgba(89,200,255,0.4);
  border-radius:8px;color:var(--text-primary);padding:10px;font-size:13px;
  outline:none;
}
.cred-input:focus{border-color:rgba(89,200,255,0.7)}
.cred-pass-row{position:relative;width:100%}
.cred-pass-row .cred-input{padding-right:44px}
.cred-eye{
  position:absolute;right:0;top:0;bottom:0;width:40px;
  background:transparent;border:none;cursor:pointer;
  display:flex;align-items:center;justify-content:center;
  color:rgba(153,162,176,0.9);font-size:16px;
}
.cred-eye:hover{color:var(--text-primary)}
</style>
</head>
<body>
<div class="app">
  <!-- Have-key prompt: the very first thing shown when not logged in -->
  <div id="have-key-overlay">
    <img src="sncasset://illustration_idle.png" alt="">
    <h2>Do you have a ShortNerdCat activation key?</h2>
    <div id="have-key-row">
      <button class="btn btn-connect" onclick="handleHaveKeyAnswer(true)">Yes, I have a key</button>
      <button class="btn btn-disconnect" onclick="handleHaveKeyAnswer(false)">No, I don't have one</button>
    </div>
  </div>

  <!-- Credential login: email/password against navlink.net, shown when the
       user has no key but navlink.net is reachable directly. -->
  <div id="credential-login-overlay">
    <img src="sncasset://illustration_idle.png" alt="">
    <h2>Log In</h2>
    <input class="cred-input" type="email" id="cred-email" placeholder="Email" autocomplete="email">
    <div class="cred-pass-row">
      <input class="cred-input" type="password" id="cred-password" placeholder="Password" autocomplete="current-password">
      <button type="button" class="cred-eye" id="cred-password-eye" onclick="toggleCredPassword()">&#128065;</button>
    </div>
    <div id="credential-err"></div>
    <button id="btn-do-login" class="btn btn-connect" onclick="handleCredentialLogin()">LOGIN</button>
    <button id="btn-key-mode-switch" class="btn btn-disconnect" onclick="handleKeyModeSwitch()">I Have a Key</button>
  </div>

  <!-- Login overlay: key-entry panel -->
  <div id="login-overlay">
    <img src="sncasset://illustration_idle.png" alt="">
    <h2>Tunnel Cat</h2>
    <p>Paste your activation key to get started.</p>
    <textarea id="key-input" placeholder="Paste activation key here..."></textarea>
    <div id="login-err"></div>
    <button id="btn-activate" class="btn btn-connect" onclick="handleActivate()">ACTIVATE</button>
    <button id="btn-login-switch" class="btn btn-disconnect" style="display:none" onclick="handleShowCredentialLogin()">Log In Instead</button>
  </div>

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
          <button id="btn-disconnect" class="btn-pill btn-pill-disconnect"
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
          <div class="srow-label">DNS over HTTPS</div>
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
function switchTab(name,el){
  document.querySelectorAll('.tab').forEach(t=>t.classList.remove('active'));
  document.querySelectorAll('.panel').forEach(p=>p.classList.remove('active'));
  el.classList.add('active');
  document.getElementById('panel-'+name).classList.add('active');
}
function handleConnect()    { window.webkit.messageHandlers.sncConnect.postMessage({}) }
function handleDisconnect() { window.webkit.messageHandlers.sncDisconnect.postMessage({}) }
function handleActivate() {
  var key = document.getElementById('key-input').value.trim();
  if (!key) return;
  document.getElementById('login-err').style.display = 'none';
  document.getElementById('btn-activate').disabled = true;
  window.webkit.messageHandlers.sncKey.postMessage(key);
}

// â”€â”€ "Do you have a key?" / credential login state machine â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
// window.__navlinkReachable starts false (conservative) and is set once Go's
// direct (non-tunneled) probe of navlink.net resolves -- see PushNavlinkReachable.
window.__navlinkReachable = false;

function showOverlay(id) {
  ['have-key-overlay', 'credential-login-overlay', 'login-overlay'].forEach(function(o) {
    document.getElementById(o).classList.toggle('visible', o === id);
  });
}

function showKeyEntry() {
  document.getElementById('btn-login-switch').style.display = window.__navlinkReachable ? '' : 'none';
  showOverlay('login-overlay');
}

function handleHaveKeyAnswer(hasKey) {
  window.webkit.messageHandlers.sncHaveKeyAnswer.postMessage(hasKey);
  if (hasKey) {
    showKeyEntry();
  } else if (window.__navlinkReachable) {
    showOverlay('credential-login-overlay');
  } else {
    showKeyEntry();
  }
}

function handleShowCredentialLogin() { showOverlay('credential-login-overlay'); }

function handleKeyModeSwitch() {
  window.webkit.messageHandlers.sncKeyModeSwitch.postMessage({});
  showKeyEntry();
}

function toggleCredPassword() {
  var input = document.getElementById('cred-password');
  var shown = input.type === 'text';
  input.type = shown ? 'password' : 'text';
  document.getElementById('cred-password-eye').textContent = shown ? '\u{1F441}' : '\u{1F576}';
}

function handleCredentialLogin() {
  var email = document.getElementById('cred-email').value.trim();
  var password = document.getElementById('cred-password').value;
  if (!email || !password) return;
  document.getElementById('credential-err').style.display = 'none';
  document.getElementById('btn-do-login').disabled = true;
  window.webkit.messageHandlers.sncCredentialLogin.postMessage({email: email, password: password});
}

window.onNavlinkReachable = function(reachable) {
  window.__navlinkReachable = !!reachable;
  var loginOverlayVisible = document.getElementById('login-overlay').classList.contains('visible');
  if (loginOverlayVisible) {
    document.getElementById('btn-login-switch').style.display = window.__navlinkReachable ? '' : 'none';
  }
};

window.onCredentialError = function(msg) {
  var err = document.getElementById('credential-err');
  err.textContent = msg;
  err.style.display = 'block';
  document.getElementById('btn-do-login').disabled = false;
};
function saveSettings() {
  window.webkit.messageHandlers.sncSetSettings.postMessage({
    doh:       document.getElementById('s-doh').checked,
    blockQUIC: document.getElementById('s-block-quic').checked,
    region:    document.getElementById('s-region').value,
  });
}
// catAssets maps state[+"_catclub"|"_elite"] to its baked-in data URI --
// see buildAppWindowHTML's ReplaceAll pass, which is the only place these
// sncasset:// tokens get resolved (unlike macOS, this window has no custom
// URI scheme handler; everything is a data: URI decided at HTML-build time,
// so JS can only pick among URLs already present in the page, not construct
// new sncasset:// paths at runtime).
window.catAssets = {
  idle: 'sncasset://illustration_idle.png',
  connecting: 'sncasset://illustration_connecting.png',
  connected: 'sncasset://illustration_connected.png',
  error: 'sncasset://illustration_error.png',
  idle_catclub: 'sncasset://illustration_idle_catclub.png',
  connecting_catclub: 'sncasset://illustration_connecting_catclub.png',
  connected_catclub: 'sncasset://illustration_connected_catclub.png',
  error_catclub: 'sncasset://illustration_error_catclub.png',
  idle_elite: 'sncasset://illustration_idle_elite.png',
  connecting_elite: 'sncasset://illustration_connecting_elite.png',
  connected_elite: 'sncasset://illustration_connected_elite.png',
  error_elite: 'sncasset://illustration_error_elite.png',
};
window.clubTheme = '';
function catAssetURL(state) {
  var suffix = window.clubTheme === 'catclub' ? '_catclub'
             : window.clubTheme === 'elite'   ? '_elite' : '';
  return window.catAssets[state + suffix] || window.catAssets[state];
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

function submitRecommend() {
  var input  = document.getElementById('recommend-username');
  var status = document.getElementById('recommend-status');
  var username = input.value.trim();
  if (!username) return;
  window.webkit.messageHandlers.sncRecommend.postMessage(username);
  status.textContent = 'Recommendation sent for ' + username + '.';
  input.value = '';
}

function submitClubThemePreview() {
  var theme = document.getElementById('s-club-theme').value;
  window.webkit.messageHandlers.sncClubThemePreview.postMessage(theme);
}

window.onStatusUpdate = function(s) {
  window.lastStatus = s;
  if (!s.loggedIn) {
    // Only default to the "do you have a key?" prompt the first time we see
    // a logged-out status -- repeated status pushes (polling) must not reset
    // the user back to square one if they already moved on to the
    // credential-login or key-entry screen.
    var anyVisible = ['have-key-overlay', 'credential-login-overlay', 'login-overlay']
      .some(function(o) { return document.getElementById(o).classList.contains('visible'); });
    if (!anyVisible) {
      showOverlay('have-key-overlay');
    }
    document.getElementById('btn-activate').disabled = false;
    return;
  }
  showOverlay(null);
  document.getElementById('login-err').style.display = 'none';

  var img  = document.getElementById('cat-img');
  var bar  = document.getElementById('status-bar');
  var bcon = document.getElementById('btn-connect');
  var bdis = document.getElementById('btn-disconnect');
  bar.className = '';
  if(s.error){
    bar.classList.add('state-error');bar.textContent=s.errorMsg||'Error';
    bcon.style.display='';bdis.style.display='none';
    img.src=catAssetURL('error');return;
  }
  if(s.disconnecting){
    bar.classList.add('state-connecting');bar.textContent='Disconnecting...';
    bcon.style.display='none';bdis.style.display='none';
    img.src=catAssetURL('connecting');return;
  }
  if(s.connecting){
    bar.classList.add('state-connecting');bar.textContent='Connecting...';
    bcon.style.display='none';bdis.style.display='';
    img.src=catAssetURL('connecting');return;
  }
  if(s.connected){
    bar.classList.add('state-connected');
    bar.textContent='Connected';
    bcon.style.display='none';bdis.style.display='';
    img.src=catAssetURL('connected');return;
  }
  bar.classList.add('state-disconnected');bar.textContent='Disconnected';
  bcon.style.display='';bdis.style.display='none';
  img.src=catAssetURL('idle');
};
window.onLoginError = function(msg) {
  var err = document.getElementById('login-err');
  err.textContent = msg;
  err.style.display = 'block';
  document.getElementById('btn-activate').disabled = false;
};
window.onSettingsUpdate = function(s) {
  document.getElementById('s-doh').checked        = !!s.doh;
  document.getElementById('s-block-quic').checked = !!s.blockQUIC;
  document.getElementById('s-region').value       = s.region||'';
};
window.webkit.messageHandlers.sncPageReady.postMessage({});
</script>
</body>
</html>`
