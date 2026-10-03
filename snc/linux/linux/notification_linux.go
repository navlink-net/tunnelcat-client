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

// ShowWildcatInfo shows a blocking, two-button informational dialog
// explaining that whitelist-bypass mode is now a separate app, WildCat --
// mirrors the equivalent info panel on other platforms, shown every time
// the tray's WildCat menu item is clicked (no state, no toggle). "Download"
// opens the WildCat app's page; "Close" dismisses. Same two-binary fallback
// shape as ShowKeyDialog (zenity, then kdialog). Best-effort: if neither
// binary is available, this silently does nothing.
//
// runWildcatInfoDialog distinguishes "the binary ran and the user answered"
// (ran=true) from "the binary isn't installed / failed to start" (ran=false)
// via the error type -- exec.Command.Run() returns *exec.ExitError for a
// real nonzero exit (e.g. the user clicked Close) and a different error type
// when the binary itself can't be found.
func runWildcatInfoDialog(name string, args ...string) (ok, ran bool) {
	err := exec.Command(name, args...).Run()
	if err == nil {
		return true, true
	}
	if _, isExitErr := err.(*exec.ExitError); isExitErr {
		return false, true
	}
	return false, false
}

func ShowWildcatInfo() {
	title := T("wildcat_info_title")
	text := T("wildcat_info_message")
	downloadLabel := T("wildcat_info_download")
	closeLabel := T("wildcat_info_close")

	if ok, ran := runWildcatInfoDialog("zenity", "--question",
		"--title="+title, "--text="+text,
		"--ok-label="+downloadLabel, "--cancel-label="+closeLabel); ran {
		if ok {
			exec.Command("xdg-open", "https://apps.navlink.net").Run() //nolint:errcheck
		}
		return
	}
	if ok, ran := runWildcatInfoDialog("kdialog",
		"--title", title, "--yesno", text,
		"--yes-label", downloadLabel, "--no-label", closeLabel); ran {
		if ok {
			exec.Command("xdg-open", "https://apps.navlink.net").Run() //nolint:errcheck
		}
		return
	}
	core.Log.Printf("wildcat info: neither zenity nor kdialog available")
}
