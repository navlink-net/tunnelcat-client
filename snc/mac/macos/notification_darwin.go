// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"os/exec"
	"strings"

	"tunnel_cat/snc/core"
)

// ShowNotification displays a macOS user notification via osascript.
// Non-blocking: the notification is fired and forgotten.
// Silently ignored if osascript is unavailable (sandboxed/restricted environment).
func ShowNotification(title, body string) {
	title = strings.ReplaceAll(title, `"`, `\"`)
	body = strings.ReplaceAll(body, `"`, `\"`)
	script := `display notification "` + body + `" with title "` + title + `"`
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		core.Log.Printf("notification: osascript: %v", err)
	}
}

// ShowNotifications shows one notification per message with "ShortNerdCat" as title.
// Mirrors the Windows snwin.ShowNotification(msgs []string) call signature.
func ShowNotifications(msgs []string) {
	for _, msg := range msgs {
		ShowNotification("ShortNerdCat", msg)
	}
}
