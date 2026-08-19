// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"fmt"
	"os/exec"
	"strings"

	"tunnel_cat/snc/core"
)

const pfAnchor = "shortnerdcat"

// FirewallManager loads and removes pf anchor rules for the SNC tunnel.
// The rules block DNS (port 53) on the physical NIC to prevent leak if routing
// table changes fail â€” DNS must always go through the TUN.
// All other enforcement is done by split-tunnel routes; pf is a safety net.
type FirewallManager struct {
	applied bool
}

// Apply loads DNS-leak prevention rules via pf.
// physIface is the outbound physical interface name (e.g. "en0"), obtained
// from RouteManager.PhysIface() after Apply().
// Non-fatal: logs errors and returns them; caller should not abort connect.
//
// Implementation note: pf anchors are only evaluated if the anchor is
// referenced in the main ruleset. We prepend the anchor declaration to the
// existing in-kernel rules so existing policy is preserved, then load our
// anchor rules into the named sub-table.
func (f *FirewallManager) Apply(physIface string) error {
	if f.applied {
		return nil
	}
	if physIface == "" {
		return fmt.Errorf("firewall: physical interface unknown â€” skipping")
	}

	// Read the current in-kernel main ruleset so we can preserve it.
	existingRules := ""
	if out, err := exec.Command("pfctl", "-s", "rules").Output(); err == nil {
		existingRules = strings.TrimSpace(string(out))
	}

	// Prepend our anchor declaration to the main ruleset so pf evaluates it.
	// existing rules follow, preserving any prior policy.
	mainRuleset := fmt.Sprintf("anchor \"%s\"\n%s\n", pfAnchor, existingRules)

	cmd := exec.Command("pfctl", "-f", "-")
	cmd.Stdin = strings.NewReader(mainRuleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("firewall: pfctl load main rules: %w (out: %s)", err, strings.TrimSpace(string(out)))
	}

	// Load our DNS-block rules into the anchor.
	// Block DNS on the physical NIC so queries always flow through the TUN.
	anchorRules := fmt.Sprintf(
		"block out quick on %s proto { tcp udp } to any port 53\n",
		physIface,
	)
	cmd2 := exec.Command("pfctl", "-a", pfAnchor, "-f", "-")
	cmd2.Stdin = strings.NewReader(anchorRules)
	if out, err := cmd2.CombinedOutput(); err != nil {
		return fmt.Errorf("firewall: pfctl load anchor: %w (out: %s)", err, strings.TrimSpace(string(out)))
	}

	// Ensure pf itself is enabled (no-op if already running).
	if out, err := exec.Command("pfctl", "-e").CombinedOutput(); err != nil {
		// "pfctl: pf already enabled" exits non-zero â€” ignore.
		msg := strings.TrimSpace(string(out))
		if !strings.Contains(msg, "already enabled") {
			core.Log.Printf("firewall: pfctl -e: %v (%s)", err, msg)
		}
	}

	f.applied = true
	core.Log.Printf("firewall: pf anchor loaded (iface=%s)", physIface)
	return nil
}

// Remove flushes all rules from the pf anchor.
// Safe to call even if Apply was never called or failed.
func (f *FirewallManager) Remove() {
	if !f.applied {
		return
	}
	if out, err := exec.Command("pfctl", "-a", pfAnchor, "-F", "all").CombinedOutput(); err != nil {
		core.Log.Printf("firewall: pfctl flush anchor: %v (%s)", err, strings.TrimSpace(string(out)))
	} else {
		core.Log.Printf("firewall: pf anchor flushed")
	}
	f.applied = false
}
