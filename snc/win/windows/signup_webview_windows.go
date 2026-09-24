// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"

	webview2 "github.com/jchv/go-webview2"

	"tunnel_cat/binlog"
	"tunnel_cat/logevent"
)

var (
	signupUser32             = syscall.NewLazyDLL("user32.dll")
	signupGetSystemMetricsFn = signupUser32.NewProc("GetSystemMetrics")
	signupShowWindowFn       = signupUser32.NewProc("ShowWindow")
)

const (
	signupSMCXScreen = 0
	signupSMCYScreen = 1
	signupSWMaximize = 3
)

// signupWebviewDataPath is WebView2's own profile directory for the sign-up
// popup -- separate from browserwindow.go's C:\.shortnerdcat\webview2-browser
// and vkauth's caller-supplied path so a stale/corrupt profile in one never
// affects the others.
const signupWebviewDataPath = `C:\.shortnerdcat\webview2-signup`

// signupURL is the same-origin (https://navlink.net) page loaded into the
// popup. It is NOT the site's own sign-up form (that stays in
// auth-modals.js/index.html, untouched) -- a separate minimal page exists
// specifically so this webview's fetch() calls to /api/captcha and
// /api/account/start are same-origin and need no CORS wiring. See
// tunnel_cat/Web/navlink/signup-app.html's own doc comment.
const signupURL = "https://navlink.net/signup-app.html"

// ShowCreateAccountWebView opens a modal WebView2 popup for account sign-up
// (email + captcha, same widget the site itself uses) and blocks until the
// page reports success (email returned, non-empty) or the user closes the
// window (email returned empty). Registration ends here, with the
// confirmation email sent -- the caller's job afterward is just to return to
// the ordinary login screen with this email pre-filled, per the agreed
// design ("Регистрация заканчивается с отправкой письма. Остальное -
// обычная процедура логина").
func ShowCreateAccountWebView() (email string) {
	type result struct{ email string }
	resCh := make(chan result, 1)
	var once sync.Once
	send := func(email string) { once.Do(func() { resCh <- result{email} }) }

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction, logevent.Str(logevent.AttrAction, "signup_webview_open"))

		// Full screen, no fixed size: the page itself (signup-app.html) is
		// laid out to fit any viewport without scrolling, so there is no
		// benefit to a small fixed-size popup here (unlike browserwindow.go
		// /vkauth_windows.go, which show real external pages of unknown
		// length). Width/Height are still passed as a same-size fallback in
		// case SW_MAXIMIZE below is somehow unavailable.
		sw, _, _ := signupGetSystemMetricsFn.Call(signupSMCXScreen)
		sh, _, _ := signupGetSystemMetricsFn.Call(signupSMCYScreen)
		wv := webview2.NewWithOptions(webview2.WebViewOptions{
			Debug:     false,
			AutoFocus: true,
			DataPath:  signupWebviewDataPath,
			WindowOptions: webview2.WindowOptions{
				Title:  T("signup_dialog_title"),
				Width:  uint(sw),
				Height: uint(sh),
				Center: true,
			},
		})
		if wv == nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinWindowCreateFailed, logevent.Str(logevent.AttrWindow, "signup_webview"))
			ShowError(T("signup_load_failed_msg") + "WebView2 runtime not installed -- install Microsoft Edge or the standalone WebView2 Runtime")
			send("")
			return
		}
		defer wv.Destroy()

		hwnd := uintptr(wv.Window())
		setWindowIconFromICO(hwnd, icoIdle)
		signupShowWindowFn.Call(hwnd, signupSWMaximize)

		// sncSignupDone(email) is called by signup-app.html's JS once
		// /api/account/start succeeds -- see that page's submit handler.
		if err := wv.Bind("sncSignupDone", func(email string) {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction,
				logevent.Str(logevent.AttrAction, "signup_webview_done"))
			send(email)
			wv.Dispatch(func() { wv.Destroy() })
		}); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinWindowCreateFailed, logevent.Str(logevent.AttrWindow, "signup_webview_bind"))
		}

		// Cache-busting query param: WebView2 persists an HTTP cache in
		// DataPath across app runs (confirmed live -- a stale signup-app.html
		// kept loading here after a server-side deploy, in the SAME profile
		// dir, until this was added), so without a varying URL a user who
		// already opened this popup once could keep getting an outdated
		// page indefinitely, missing later fixes/deploys entirely.
		wv.Navigate(fmt.Sprintf("%s?_cb=%d", signupURL, time.Now().Unix()))
		wv.Run()

		// Run() ended without sncSignupDone firing -- window was closed
		// (Cancel-equivalent). Treat exactly like Cancel: empty result.
		logevent.Emit(binlog.TagSystem, logevent.EventWinDialogAction, logevent.Str(logevent.AttrAction, "signup_webview_cancelled"))
		send("")
	}()

	res := <-resCh
	return res.email
}
