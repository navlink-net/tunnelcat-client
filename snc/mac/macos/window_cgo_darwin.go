// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

// CGo bridge between Go and the Objective-C Cocoa window implementation.
// All C/ObjC calls are isolated here so that window_darwin.go (public API)
// and tray_darwin.go can remain CGo-free and be fully visible to gopls.

/*
#cgo LDFLAGS: -framework Cocoa -framework WebKit -framework CoreImage
#include "window_cocoa.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"tunnel_cat/snc/core"
)

// windowRegisterAsset passes an in-memory asset to the ObjC scheme handler.
func windowRegisterAsset(name string, data []byte) {
	if len(data) == 0 {
		return
	}
	cname := C.CString(name)
	C.snc_window_register_asset(cname,
		(*C.uchar)(unsafe.Pointer(&data[0])),
		C.int(len(data)))
	C.free(unsafe.Pointer(cname))
}

// windowSetHTML delivers the HTML content to ObjC before window creation.
func windowSetHTML(html string) {
	cs := C.CString(html)
	C.snc_window_set_html(cs)
	C.free(unsafe.Pointer(cs))
}

func windowInit()           { C.snc_window_init() }
func windowShow()           { C.snc_window_show() }
func windowHide()           { C.snc_window_hide() }
func windowDestroy()        { C.snc_window_destroy() }
func windowShowKeyEntry()   { C.snc_window_show_key_entry() }
func windowCancelKeyEntry() { C.snc_window_cancel_key_entry() }
func registerURLHandler()   { C.snc_register_url_handler() }

func windowShowHaveKeyPrompt()   { C.snc_window_show_have_key_prompt() }
func windowShowCredentialLogin() { C.snc_window_show_credential_login() }

func windowPushStatus(b []byte) {
	cs := C.CString(string(b))
	C.snc_window_push_status(cs)
	C.free(unsafe.Pointer(cs))
}

func windowPushSettings(b []byte) {
	cs := C.CString(string(b))
	C.snc_window_push_settings(cs)
	C.free(unsafe.Pointer(cs))
}

func windowPushClubTheme(b []byte) {
	cs := C.CString(string(b))
	C.snc_window_push_club_theme(cs)
	C.free(unsafe.Pointer(cs))
}

func windowBuildAppMenu() { C.snc_window_build_app_menu() }

func windowSyncAppMenu(doh, quic bool, region string, updateReady bool) {
	boolC := func(v bool) C.int {
		if v {
			return 1
		}
		return 0
	}
	cr := C.CString(region)
	C.snc_window_sync_app_menu(boolC(doh), boolC(quic), cr, boolC(updateReady))
	C.free(unsafe.Pointer(cr))
}

// â”€â”€ Exported CGo callbacks invoked from Objective-C â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

//export go_snc_connect
func go_snc_connect() {
	if globalWindow != nil && globalWindow.onConnect != nil {
		go globalWindow.onConnect()
	}
}

//export go_snc_disconnect
func go_snc_disconnect() {
	if globalWindow != nil && globalWindow.onDisconnect != nil {
		go globalWindow.onDisconnect()
	}
}

//export go_snc_hide_window
func go_snc_hide_window() {
	C.snc_window_hide()
}

//export go_snc_set_settings
func go_snc_set_settings(jsonStr *C.char) {
	if globalWindow == nil || globalWindow.onSettings == nil {
		return
	}
	var s AppSettings
	if err := json.Unmarshal([]byte(C.GoString(jsonStr)), &s); err != nil {
		core.Log.Printf("window: parse settings: %v", err)
		return
	}
	go globalWindow.onSettings(s)
}

//export go_snc_recommend
func go_snc_recommend(usernameStr *C.char) {
	if globalWindow == nil || globalWindow.onRecommend == nil {
		return
	}
	username := C.GoString(usernameStr)
	go globalWindow.onRecommend(username)
}

//export go_snc_club_theme_preview
func go_snc_club_theme_preview(themeStr *C.char) {
	if globalWindow == nil || globalWindow.onClubThemePreview == nil {
		return
	}
	theme := C.GoString(themeStr)
	go globalWindow.onClubThemePreview(theme)
}

//export go_snc_submit_key
func go_snc_submit_key(keyStr *C.char) {
	if globalWindow == nil || globalWindow.keyCh == nil {
		return
	}
	select {
	case globalWindow.keyCh <- C.GoString(keyStr):
	default:
	}
}

//export go_snc_page_ready
func go_snc_page_ready() {
	if globalWindow != nil && globalWindow.onPageReady != nil {
		go globalWindow.onPageReady()
	}
}

//export go_snc_have_key_answer
func go_snc_have_key_answer(hasKey C.int) {
	if globalWindow == nil || globalWindow.haveKeyCh == nil {
		return
	}
	select {
	case globalWindow.haveKeyCh <- hasKey != 0:
	default:
	}
}

//export go_snc_credential_login
func go_snc_credential_login(email, password *C.char) {
	if globalWindow == nil || globalWindow.loginCh == nil {
		return
	}
	select {
	case globalWindow.loginCh <- credentialResult{email: C.GoString(email), password: C.GoString(password)}:
	default:
	}
}

//export go_snc_credential_login_key_mode
func go_snc_credential_login_key_mode() {
	if globalWindow == nil || globalWindow.loginCh == nil {
		return
	}
	select {
	case globalWindow.loginCh <- credentialResult{wantsKeyMode: true}:
	default:
	}
}

// â"€â"€ Native menu bar CGo callbacks â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€

//export go_snc_menu_login
func go_snc_menu_login() {
	if globalTray != nil {
		globalTray.TriggerLogin()
	}
}

//export go_snc_menu_logout
func go_snc_menu_logout() {
	if globalTray != nil {
		globalTray.TriggerLogout()
	}
}

//export go_snc_menu_connect
func go_snc_menu_connect() {
	if globalTray != nil {
		globalTray.TriggerConnect()
	}
}

//export go_snc_menu_disconnect
func go_snc_menu_disconnect() {
	if globalTray != nil {
		globalTray.TriggerDisconnect()
	}
}

//export go_snc_menu_toggle_doh
func go_snc_menu_toggle_doh() {
	if globalTray != nil {
		select {
		case globalTray.menuDoHCh <- struct{}{}:
		default:
		}
	}
}

//export go_snc_menu_toggle_quic
func go_snc_menu_toggle_quic() {
	if globalTray != nil {
		select {
		case globalTray.menuQUICCh <- struct{}{}:
		default:
		}
	}
}

//export go_snc_menu_region
func go_snc_menu_region(code *C.char) {
	if globalTray != nil {
		globalTray.SetRegionByCode(C.GoString(code))
	}
}

//export go_snc_menu_about
func go_snc_menu_about() {
	if globalTray != nil {
		globalTray.ShowAbout()
	}
}

//export go_snc_menu_update
func go_snc_menu_update() {
	if globalTray != nil {
		globalTray.TriggerUpdateInstall()
	}
}

//export go_snc_menu_quit
func go_snc_menu_quit() {
	if globalTray != nil {
		globalTray.TriggerQuit()
	}
}

// goNavlinkURL is called from ObjC when the OS delivers a navlink:// Apple Event.
//
//export goNavlinkURL
func goNavlinkURL(urlStr *C.char) {
	raw := C.GoString(urlStr)
	// Parse navlink://activate?key=...
	const prefix = "navlink://activate?key="
	const altPrefix = "navlink://activate/?key="
	var key string
	for _, p := range []string{prefix, altPrefix} {
		if idx := len(p); len(raw) > idx && raw[:idx] == p {
			key = raw[idx:]
			if amp := len(key); amp > 0 {
				if i := indexOf(key, '&'); i >= 0 {
					key = key[:i]
				}
			}
			break
		}
	}
	// Fall back to URL query parsing for URL-encoded keys.
	if key == "" {
		if comps := parseSimpleQuery(raw); comps != "" {
			key = comps
		}
	}
	if key == "" {
		return
	}
	select {
	case DeepLinkKeysCh <- key:
	default:
	}
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func parseSimpleQuery(raw string) string {
	q := raw
	if i := len("navlink://activate"); len(raw) > i {
		q = raw[i:]
	}
	if len(q) > 0 && (q[0] == '?' || q[0] == '/') {
		q = q[1:]
	}
	for _, part := range splitStr(q, '&') {
		if len(part) > 4 && part[:4] == "key=" {
			return part[4:]
		}
	}
	return ""
}

func splitStr(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
