// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"fmt"
	"os/exec"
	"strings"
)

// ShowError displays a simple modal alert via osascript — deliberately not
// another native Cocoa panel (window_cocoa.m already carries substantial new
// UI for this feature; a plain AppleScript alert is enough for an
// error message and matches the existing osascript-based fallback dialog
// pattern used elsewhere in this package's cmd/ callers).
func ShowError(title, message string) {
	script := `display alert ` + quoteAS(title) + ` message ` + quoteAS(message)
	exec.Command("osascript", "-e", script).Run() //nolint:errcheck
}

// ShowUpdateAvailableAlert shows a native "Update available" prompt.
// Returns true if the user clicked "Update Now", false on "Later" or dismiss.
// Must be called from the tray (user-session) process, not the root daemon.
func ShowUpdateAvailableAlert(version string) bool {
	laterBtn := T("alert_update_later")
	nowBtn := T("alert_update_now")
	script := `button returned of (display dialog ` +
		quoteAS(fmt.Sprintf(T("alert_update_message_fmt"), version)) +
		` with title ` + quoteAS(T("alert_update_title")) +
		` buttons {` + quoteAS(laterBtn) + `, ` + quoteAS(nowBtn) + `} default button ` + quoteAS(nowBtn) + `)`
	out, err := exec.Command("osascript", "-e", script).Output()
	return err == nil && strings.TrimSpace(string(out)) == nowBtn
}

// ShowWildcatWarning shows an unconditional informational warning before
// WildCat mode is enabled -- see doWildcatToggle in tray_darwin.go, which
// calls this once every time the user turns WildCat on (no state check, no
// cancel, OK only).
func ShowWildcatWarning() {
	script := `display dialog ` + quoteAS(T("wildcat_warning_message")) +
		` with title ` + quoteAS(T("wildcat_warning_title")) +
		` buttons {"OK"} default button "OK"`
	exec.Command("osascript", "-e", script).Run() //nolint:errcheck
}

// quoteAS quotes a string for embedding in an AppleScript string literal.
func quoteAS(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
