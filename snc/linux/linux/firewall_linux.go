// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"fmt"
	"os/exec"
	"strings"

	"tunnel_cat/snc/core"
)

const iptablesChain = "SHORTNERDCAT"

// FirewallManager manages an iptables chain that blocks DNS on the physical
// NIC to prevent leaks if the split-tunnel routes fail.
// All other enforcement is done by the routes; iptables is a safety net.
type FirewallManager struct {
	physIface string
	applied   bool
}

// Apply creates the SHORTNERDCAT chain and blocks UDP/TCP port 53 on physIface.
// Non-fatal: caller should not abort connect on failure.
func (f *FirewallManager) Apply(physIface string) error {
	if f.applied {
		return nil
	}
	if physIface == "" {
		return fmt.Errorf("firewall: physical interface unknown â€” skipping")
	}
	f.physIface = physIface

	// Create chain (idempotent â€” iptables returns 1 if it already exists).
	exec.Command("iptables", "-N", iptablesChain).Run() //nolint:errcheck

	// Wire it into OUTPUT if not already there.
	checkOut := exec.Command("iptables", "-C", "OUTPUT", "-j", iptablesChain).Run()
	if checkOut != nil {
		if out, err := exec.Command("iptables", "-I", "OUTPUT", "-j", iptablesChain).CombinedOutput(); err != nil {
			return fmt.Errorf("firewall: iptables -I OUTPUT: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	// Block DNS out on the physical NIC so all DNS queries flow through the TUN.
	for _, proto := range []string{"udp", "tcp"} {
		rule := []string{"-A", iptablesChain, "-o", physIface, "-p", proto, "--dport", "53", "-j", "DROP"}
		if out, err := exec.Command("iptables", rule...).CombinedOutput(); err != nil {
			return fmt.Errorf("firewall: iptables block DNS %s: %w (%s)", proto, err, strings.TrimSpace(string(out)))
		}
	}

	f.applied = true
	core.Log.Printf("firewall: iptables chain %s applied (iface=%s)", iptablesChain, physIface)
	return nil
}

// Remove flushes all rules from the chain and removes its OUTPUT reference.
// Safe to call even if Apply was never called or failed.
func (f *FirewallManager) Remove() {
	if !f.applied {
		return
	}
	exec.Command("iptables", "-D", "OUTPUT", "-j", iptablesChain).Run() //nolint:errcheck
	exec.Command("iptables", "-F", iptablesChain).Run()                 //nolint:errcheck
	exec.Command("iptables", "-X", iptablesChain).Run()                 //nolint:errcheck
	f.applied = false
	core.Log.Printf("firewall: iptables chain %s removed", iptablesChain)
}
