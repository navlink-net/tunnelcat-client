// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"fmt"
	"net/url"
	"runtime"
	"sync"
	"time"

	webview2 "github.com/jchv/go-webview2"

	"tunnel_cat/binlog"
	"tunnel_cat/logevent"
)

// loginWebviewDataPath is WebView2's own profile directory for the login
// popup -- separate from every other webview's DataPath in this package
// (see signup_webview_windows.go's own comment for why) so a stale/corrupt
// profile in one never affects another.
const loginWebviewDataPath = `C:\.shortnerdcat\webview2-login`

// loginURLBase is the same-origin page loaded into the popup. Replaces the
// old raw Win32 credential-login dialog (dialog_auth.go's
// showLoginDialogImpl) with a themed page matching signup-app.html's look,
// per feedback_no_default_os_dialogs_unified_style -- see login-app.html's
// own doc comment for the full rationale and the division of labor (the
// page performs the actual login/free-key calls itself; this function only
// relays the outcome).
const loginURLBase = "https://navlink.net/login-app.html"

// ShowLoginWebView opens a full-screen WebView2 popup for navlink.net
// credential login. prefillEmail, if non-empty, pre-fills the email field
// (passed as a URL query param) and focuses the password field instead --
// used when returning here right after a successful sign-up, same as the
// old dialog's ShowLoginDialogWithEmail.
//
// Exactly one of these three outcomes holds on return:
//   - keyStr != "": logged in and a key was issued -- use it like a typed key.
//   - wantsKeyMode: the user clicked "I Have a Key".
//   - wantsSignup: the user clicked "Create New Account".
//
// None of the above (the window was simply closed) means cancelled, same
// convention as ShowCreateAccountWebView.
func ShowLoginWebView(prefillEmail string) (keyStr string, wantsKeyMode, wantsSignup bool) {
	type result struct {
		key     string
		keyMode bool
		signup  bool
	}
	resCh := make(chan result, 1)
	var once sync.Once
	send := func(res result) { once.Do(func() { resCh <- res }) }

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction, logevent.Str(logevent.AttrAction, "login_webview_open"))

		// Cache-busting query param -- see signup_webview_windows.go's
		// identical comment; same WebView2 persistent-cache behavior applies
		// here, in this page's own separate DataPath.
		q := url.Values{"_cb": {fmt.Sprintf("%d", time.Now().Unix())}}
		if prefillEmail != "" {
			q.Set("email", prefillEmail)
		}
		pageURL := loginURLBase + "?" + q.Encode()

		sw, _, _ := signupGetSystemMetricsFn.Call(signupSMCXScreen)
		sh, _, _ := signupGetSystemMetricsFn.Call(signupSMCYScreen)
		wv := webview2.NewWithOptions(webview2.WebViewOptions{
			Debug:     false,
			AutoFocus: true,
			DataPath:  loginWebviewDataPath,
			WindowOptions: webview2.WindowOptions{
				Title:  T("login_dialog_title"),
				Width:  uint(sw),
				Height: uint(sh),
				Center: true,
			},
		})
		if wv == nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinWindowCreateFailed, logevent.Str(logevent.AttrWindow, "login_webview"))
			ShowError(T("signup_load_failed_msg") + "WebView2 runtime not installed -- install Microsoft Edge or the standalone WebView2 Runtime")
			send(result{})
			return
		}
		defer wv.Destroy()

		hwnd := uintptr(wv.Window())
		setWindowIconFromICO(hwnd, icoIdle)
		signupShowWindowFn.Call(hwnd, signupSWMaximize)

		closeWith := func(res result) {
			send(res)
			wv.Dispatch(func() { wv.Destroy() })
		}

		// sncLoginDone(key) is called by login-app.html's JS once
		// /api/account/login + /api/key/free both succeed.
		if err := wv.Bind("sncLoginDone", func(key string) {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction, logevent.Str(logevent.AttrAction, "login_webview_done"))
			closeWith(result{key: key})
		}); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinWindowCreateFailed, logevent.Str(logevent.AttrWindow, "login_webview_bind_done"))
		}
		if err := wv.Bind("sncLoginKeyMode", func() {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction, logevent.Str(logevent.AttrAction, "login_webview_keymode"))
			closeWith(result{keyMode: true})
		}); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinWindowCreateFailed, logevent.Str(logevent.AttrWindow, "login_webview_bind_keymode"))
		}
		if err := wv.Bind("sncLoginSignup", func() {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction, logevent.Str(logevent.AttrAction, "login_webview_signup"))
			closeWith(result{signup: true})
		}); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinWindowCreateFailed, logevent.Str(logevent.AttrWindow, "login_webview_bind_signup"))
		}

		wv.Navigate(pageURL)
		wv.Run()

		// Run() ended without any bridge firing -- window was closed. Treat
		// exactly like the old dialog's Cancel button.
		logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction, logevent.Str(logevent.AttrAction, "login_webview_cancelled"))
		send(result{})
	}()

	res := <-resCh
	return res.key, res.keyMode, res.signup
}
