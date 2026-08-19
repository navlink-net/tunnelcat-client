// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tunnel_cat/snc/core"
)

const (
	launchAgentLabel = "net.navlink.shortnerdcat"
	launchAgentFile  = "net.navlink.shortnerdcat.plist"
)

// launchAgentPath returns ~/Library/LaunchAgents/<file>.
func launchAgentPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/var/root"
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentFile)
}

// RegisterAutostart installs a LaunchAgent plist so launchd starts
// ShortNerdCat automatically at the user's next graphical login.
//
// This must never call "launchctl load" itself: RegisterAutostart runs inside
// the root-privileged daemon, so launchctl would load the job as root instead
// of into the user's gui/<uid> domain. With RunAtLoad=true that immediately
// spawns a second, environment-stripped instance (no HOME â€” falls back to
// /var/root), which kills the legitimate session via the single-instance
// guard's "kill the old holder" path and surfaces as a duplicate login
// prompt. Writing the plist is sufficient: launchd picks it up correctly on
// the next real login, the same pattern used for the system LaunchDaemon.
func RegisterAutostart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	// For autostart, launch via the .app bundle script so Gatekeeper quarantine
	// is handled by the bundle, not a bare executable path.
	// Heuristic: if exe is inside *.app/Contents/MacOS/, use the .app path.
	launchPath := exe
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
		appBundle := exe[:i+4] // up to and including ".app"
		sh := filepath.Join(appBundle, "Contents", "MacOS", "ShortNerdCat.sh")
		if _, err := os.Stat(sh); err == nil {
			launchPath = sh
		}
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>--watchdog</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
	<key>StandardOutPath</key>
	<string>/dev/null</string>
	<key>StandardErrorPath</key>
	<string>/dev/null</string>
</dict>
</plist>
`, launchAgentLabel, launchPath)

	p := launchAgentPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return fmt.Errorf("autostart: mkdir: %w", err)
	}
	if err := os.WriteFile(p, []byte(plist), 0644); err != nil {
		return fmt.Errorf("autostart: write plist: %w", err)
	}

	core.Log.Printf("autostart: registered %s â†’ %s (active at next login)", launchAgentLabel, launchPath)
	return nil
}

// RemoveAutostart removes the LaunchAgent plist so it no longer starts at
// the next login. Like RegisterAutostart, this must not shell out to
// launchctl from the root daemon â€” see RegisterAutostart for why.
func RemoveAutostart() error {
	p := launchAgentPath()

	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("autostart: remove plist: %w", err)
	}
	core.Log.Printf("autostart: removed %s", launchAgentLabel)
	return nil
}

// IsAutostartRegistered returns true if the LaunchAgent plist exists and
// points to the current executable.
func IsAutostartRegistered() bool {
	p := launchAgentPath()
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(string(data), filepath.Base(exe))
}
