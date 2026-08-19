// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tunnel_cat/snc/core"
)

const (
	systemdServiceName = "shortnerdcat"
	systemdServiceFile = systemdServiceName + ".service"
	// Installed under /etc/systemd/system so it starts at boot as root.
	systemdServicePath = "/etc/systemd/system/" + systemdServiceFile
)

// RegisterAutostart writes the systemd service unit and enables it.
// The unit launches the daemon with --watchdog on system boot.
func RegisterAutostart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	unit := fmt.Sprintf(`[Unit]
Description=ShortNerdCat VPN daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, exe)

	if err := os.WriteFile(systemdServicePath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("autostart: write service unit: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()          //nolint:errcheck
	exec.CommandContext(ctx, "systemctl", "enable", systemdServiceName).Run() //nolint:errcheck

	core.Log.Printf("autostart: registered systemd unit â†’ %s", exe)
	return nil
}

// RemoveAutostart disables and removes the systemd service unit.
func RemoveAutostart() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "systemctl", "disable", systemdServiceName).Run() //nolint:errcheck
	exec.CommandContext(ctx, "systemctl", "stop", systemdServiceName).Run()    //nolint:errcheck

	if err := os.Remove(systemdServicePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("autostart: remove unit: %w", err)
	}
	exec.CommandContext(ctx, "systemctl", "daemon-reload").Run() //nolint:errcheck
	core.Log.Printf("autostart: removed systemd unit")
	return nil
}

// IsAutostartRegistered returns true if the service unit file exists.
func IsAutostartRegistered() bool {
	data, err := os.ReadFile(systemdServicePath)
	if err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(string(data), filepath.Base(exe))
}
