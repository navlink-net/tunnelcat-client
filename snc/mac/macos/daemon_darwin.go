// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"fmt"
	"net"
	"os"
	"time"
)

const (
	launchDaemonLabel = "net.navlink.snc"
	// LaunchDaemonPlistPath is the system-wide plist installed once so launchd
	// starts the privileged process automatically on every subsequent reboot —
	// eliminating the osascript password prompt after the first install.
	LaunchDaemonPlistPath = "/Library/LaunchDaemons/" + launchDaemonLabel + ".plist"
)

// IsDaemonSocketLive returns true if the privileged main process is already
// accepting connections on its IPC Unix socket.  The check uses a 300 ms
// dial timeout so it is invisible to the user.
func IsDaemonSocketLive(socketPath string) bool {
	c, err := net.DialTimeout("unix", socketPath, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// EnsureLaunchDaemon writes the LaunchDaemon plist for exe to
// /Library/LaunchDaemons/ when it is absent or when the exe path has
// changed (e.g. after an in-place update).  The plist is NOT loaded
// immediately — doing so would spawn a second root process that conflicts
// with the already-running one.  launchd picks it up automatically on the
// next reboot, after which no password is ever requested again.
// Must be called from a root-privileged context.
func EnsureLaunchDaemon(exe string) error {
	plist := buildDaemonPlist(exe)
	existing, err := os.ReadFile(LaunchDaemonPlistPath)
	if err == nil && string(existing) == plist {
		return nil // already up to date
	}
	if err := os.MkdirAll("/Library/LaunchDaemons", 0755); err != nil {
		return fmt.Errorf("LaunchDaemon install: %w", err)
	}
	if err := os.WriteFile(LaunchDaemonPlistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("LaunchDaemon install: %w", err)
	}
	return nil
}

// IsLaunchDaemonInstalled reports whether the plist file is present on disk.
func IsLaunchDaemonInstalled() bool {
	_, err := os.Stat(LaunchDaemonPlistPath)
	return err == nil
}

func buildDaemonPlist(exe string) string {
	// KeepAlive.SuccessfulExit=false: launchd restarts only on crash (non-zero
	// exit).  A clean user-initiated Quit exits with code 0 and is not restarted,
	// preserving the expected "Quit means quit" behaviour.
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardErrorPath</key>
	<string>/var/log/snc-daemon.log</string>
</dict>
</plist>
`, launchDaemonLabel, exe)
}
