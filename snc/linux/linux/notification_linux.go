// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"os/exec"
	"strings"

	"tunnel_cat/snc/core"
)

// ShowNotification displays a desktop notification via notify-send (libnotify).
// Falls back to dbus-send if notify-send is unavailable.
// Non-blocking: the notification is fired and forgotten.
func ShowNotification(title, body string) {
	if err := exec.Command("notify-send", "--app-name=ShortNerdCat", title, body).Run(); err != nil {
		// Fallback: raw dbus call (works when libnotify is not installed).
		dbusArgs := []string{
			"--session",
			"--dest=org.freedesktop.Notifications",
			"--object-path=/org/freedesktop/Notifications",
			"--method=org.freedesktop.Notifications.Notify",
			"shortnerdcat",
			"uint32:0",
			"",
			title,
			body,
			"array:string:",
			"dict:string:variant:",
			"int32:-1",
		}
		if err2 := exec.Command("dbus-send", dbusArgs...).Run(); err2 != nil {
			core.Log.Printf("notification: notify-send: %v; dbus-send: %v", err, err2)
		}
	}
}

// ShowNotifications shows one notification per message with "ShortNerdCat" as title.
func ShowNotifications(msgs []string) {
	for _, msg := range msgs {
		ShowNotification("ShortNerdCat", msg)
	}
}

// ShowKeyDialog shows an interactive entry dialog using zenity or kdialog.
// Returns the entered string, or an error if the user cancelled.
func ShowKeyDialog() (string, error) {
	prompt := T("key_dialog_prompt")
	// Try zenity first (GNOME/GTK), then kdialog (KDE).
	if out, err := exec.Command("zenity", "--entry",
		"--title=ShortNerdCat",
		"--text="+prompt,
		"--width=420",
	).Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	if out, err := exec.Command("kdialog",
		"--title=ShortNerdCat",
		"--inputbox="+prompt,
	).Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return "", nil
}

// ShowWildcatWarning shows a blocking, single-button (OK) informational
// dialog explaining what WildCat mode does, mirroring the equivalent warning
// shown on other platforms before WildCat is enabled. Same two-binary
// fallback shape as ShowKeyDialog (zenity, then kdialog). Best-effort: if
// neither binary is available, this silently does nothing rather than
// blocking WildCat activation on a dialog helper being installed.
func ShowWildcatWarning() {
	title := T("wildcat_warning_title")
	text := T("wildcat_warning_text")
	if err := exec.Command("zenity", "--warning",
		"--title="+title,
		"--text="+text,
		"--ok-label=OK",
	).Run(); err == nil {
		return
	}
	if err := exec.Command("kdialog",
		"--title", title,
		"--msgbox", text,
	).Run(); err == nil {
		return
	}
	core.Log.Printf("wildcat warning: neither zenity nor kdialog available")
}
