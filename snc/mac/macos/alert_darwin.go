// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
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
	script := `button returned of (display dialog ` +
		quoteAS("ShortNerdCat "+version+" is ready.\nUpdate now to get the latest features and fixes.") +
		` with title ` + quoteAS("Update Available") +
		` buttons {"Later", "Update Now"} default button "Update Now")`
	out, err := exec.Command("osascript", "-e", script).Output()
	return err == nil && strings.TrimSpace(string(out)) == "Update Now"
}

// quoteAS quotes a string for embedding in an AppleScript string literal.
func quoteAS(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
