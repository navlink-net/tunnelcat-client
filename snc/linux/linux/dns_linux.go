// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"tunnel_cat/snc/core"
)

const (
	dnsServerDefault = "1.1.1.1"
	dnsCmdTimeout    = 8 * time.Second
)

// DNSManager configures the system DNS via resolvectl (systemd-resolved) to
// prevent DNS leaks during an active tunnel session.
//
// In normal mode, dnsAddr is "1.1.1.1" which is bypassed via the original
// gateway.  In DoH mode, dnsAddr is "127.0.0.1" (the local DoHProxy), and
// the DoHProxy forwards queries as HTTPS through the TUN to Cloudflare.
type DNSManager struct {
	iface string // physical interface name, e.g. "eth0"
}

// Apply configures DNS for physIface using dnsAddr as the upstream resolver.
// physIface must be the value returned by RouteManager.PhysIface().
// Pass "" for dnsAddr to use the default (1.1.1.1).
func (d *DNSManager) Apply(physIface, dnsAddr string) error {
	if physIface == "" {
		return fmt.Errorf("dns: physical interface unknown â€” skipping")
	}
	if dnsAddr == "" {
		dnsAddr = dnsServerDefault
	}
	d.iface = physIface

	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()

	// Set DNS server for the physical interface.
	if out, err := exec.CommandContext(ctx, "resolvectl", "dns", physIface, dnsAddr).CombinedOutput(); err != nil {
		return fmt.Errorf("dns: resolvectl dns %s %s: %w (out: %s)", physIface, dnsAddr, err, strings.TrimSpace(string(out)))
	}

	// Make it the default resolver for all domains (~. is the catch-all route).
	ctx2, cancel2 := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel2()
	if out, err := exec.CommandContext(ctx2, "resolvectl", "domain", physIface, "~.").CombinedOutput(); err != nil {
		core.Log.Printf("dns: resolvectl domain %s ~.: %v (out: %s)", physIface, err, strings.TrimSpace(string(out)))
	}

	core.Log.Printf("dns: set %q DNS â†’ %s (default route ~.)", physIface, dnsAddr)
	return nil
}

// Restore reverts DNS configuration for the physical interface to DHCP defaults.
func (d *DNSManager) Restore() {
	if d.iface == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "resolvectl", "revert", d.iface).CombinedOutput(); err != nil {
		core.Log.Printf("dns: restore %q: %v (out: %s)", d.iface, err, strings.TrimSpace(string(out)))
	} else {
		core.Log.Printf("dns: restored %q DNS to DHCP", d.iface)
	}
}

// CleanupDNS reverts DNS for all interfaces with non-default configuration.
// Called by the watchdog on unclean exit when the interface name is unknown.
func CleanupDNS() {
	ifaces, err := listResolvectlIfaces()
	if err != nil {
		core.Log.Printf("dns: cleanup: list interfaces: %v", err)
		return
	}
	for _, iface := range ifaces {
		ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
		out, err := exec.CommandContext(ctx, "resolvectl", "revert", iface).CombinedOutput()
		cancel()
		if err != nil {
			core.Log.Printf("dns: cleanup %q: %v (%s)", iface, err, strings.TrimSpace(string(out)))
		}
	}
	core.Log.Printf("dns: cleanup: reverted all interfaces")
}

// listResolvectlIfaces returns interface names from `resolvectl status`.
func listResolvectlIfaces() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "resolvectl", "status").Output()
	if err != nil {
		return nil, err
	}
	var ifaces []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// Lines like "Link 2 (eth0)" or "Link 3 (wlan0)"
		if strings.HasPrefix(line, "Link ") {
			start := strings.Index(line, "(")
			end := strings.Index(line, ")")
			if start >= 0 && end > start {
				iface := line[start+1 : end]
				if iface != "" {
					ifaces = append(ifaces, iface)
				}
			}
		}
	}
	return ifaces, nil
}
