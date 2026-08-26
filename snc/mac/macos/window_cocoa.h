// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

#pragma once
#include <stdint.h>

// snc_window_register_asset stores an in-memory asset served to the WebView via
// the custom "sncasset://" URL scheme. Call once per asset before snc_window_init.
void snc_window_register_asset(const char *name, const unsigned char *data, int len);

// snc_window_set_html provides the HTML content loaded into the WebView.
// Must be called before snc_window_init.
void snc_window_set_html(const char *html);

// snc_window_init schedules NSWindow + WKWebView creation on the Cocoa main thread.
// Returns immediately; creation is asynchronous.
void snc_window_init(void);

// snc_window_show brings the window to the foreground (dispatch_async to main queue).
void snc_window_show(void);

// snc_window_hide hides the window without destroying it.
void snc_window_hide(void);

// snc_window_destroy closes and releases the window.
void snc_window_destroy(void);

// snc_window_push_status evaluates window.onStatusUpdate(<statusJSON>) in the WebView.
void snc_window_push_status(const char *statusJSON);

// snc_window_push_settings evaluates window.onSettingsUpdate(<settingsJSON>) in the WebView.
void snc_window_push_settings(const char *settingsJSON);

// snc_window_push_club_theme evaluates window.onClubThemeUpdate(<themeJSON>)
// in the WebView -- see tunnel_cat/docs/club-membership.md. themeJSON is
// {"theme":"","badge":""} for the regular tier (no badge), or
// {"theme":"catclub"|"elite","badge":"Cat Club Member #N"} once membership
// is confirmed.
void snc_window_push_club_theme(const char *themeJSON);

// snc_window_push_bytes evaluates window.onBytesUpdate(<bytesJSON>) in the
// WebView. bytesJSON is {"sent":<int64>,"recv":<int64>}, the live cumulative
// uplink/downlink byte counters (see core.TotalBytes's doc comment) -- pushed
// once a second while connected, and once with sent=recv=0 on disconnect.
void snc_window_push_bytes(const char *bytesJSON);

// snc_window_show_key_entry shows a non-modal key-entry window (NSWindow + NSTextField).
void snc_window_show_key_entry(void);

// snc_window_cancel_key_entry dismisses the key-entry dialog without submitting a key.
void snc_window_cancel_key_entry(void);

// snc_splash_open shows the startup splash (dark rounded card with logo + version).
// Dispatches to the main thread asynchronously; returns immediately.
void snc_splash_open(const unsigned char *pngData, int pngLen, const char *version);

// snc_splash_close hides and destroys the splash window asynchronously.
void snc_splash_close(void);

// snc_register_url_handler registers an NSAppleEventManager handler for the
// navlink:// URL scheme. When the OS delivers a navlink://activate?key=… URL,
// goNavlinkURL is called with the raw URL string. Must be called before the
// Cocoa run loop starts (i.e. before tray.Run()).
void snc_register_url_handler(void);

// snc_window_show_have_key_prompt shows a non-modal "Do you have a key?"
// panel (Yes/No). The answer is delivered via go_snc_have_key_answer.
void snc_window_show_have_key_prompt(void);

// snc_window_show_credential_login shows a non-modal email/password login
// panel, with a "Log In" button (delivers via go_snc_credential_login) and an
// "I Have a Key" button (delivers via go_snc_credential_login_key_mode).
// Closing the window without submitting is treated as cancel (empty email).
void snc_window_show_credential_login(void);

// snc_window_build_app_menu builds the NSApp main menu bar with all
// ShortNerdCat actions. Must be called after snc_window_init (dispatched to
// the main queue, so the order is preserved). Each item calls back into Go
// via the go_snc_menu_* exported functions.
void snc_window_build_app_menu(void);

// snc_window_sync_app_menu updates menu-item checkmarks and the Update item's
// enabled state to match the current settings. Safe to call from any thread —
// dispatches to the main queue internally.
void snc_window_sync_app_menu(int doh, int quic, int wildcat, const char *region, int updateReady, int quicLocked);
