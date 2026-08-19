// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"fmt"

	"tunnel_cat/snc/core"
)

const (
	dohServer   = "1.1.1.1"
	dohTemplate = "https://cloudflare-dns.com/dns-query"
)

// EnsureTunnelDNS points the TUN adapter's DNS server at dohServer (1.1.1.1)
// in plain UDP. Call once, right after routes are applied, regardless of the
// DoH setting -- without an explicit DNS server on the TUN adapter itself,
// Windows silently keeps resolving via whatever DNS server the physical NIC
// already had (found live, 2026-08-12: a fresh connect with DoH off left
// "ShortNerdCat" with Get-DnsClientServerAddress = {}, so every query kept
// going out the physical adapter to the LAN router/ISP resolver -- neither
// snc/core/udp_assoc.go nor anything else in this file ever saw them, no
// matter what routes.go does). ConfigureDoH/ConfigureDoHFallback layer HTTPS
// on top of this afterward when DoH is enabled; when it's not, this call is
// what actually makes DNS reach the tunnel at all.
func EnsureTunnelDNS() error {
	return setTUNDNS(tunIfaceName, dohServer)
}

// ConfigureDoH enables DNS-over-HTTPS on this machine for 1.1.1.1.
//
// No route changes here: DNS (port 53) is always captured by TUN and handled
// by snc/core/udp_assoc.go's own per-mode logic regardless of DoH (see the
// RouteManager doc comment in routes.go) -- this only configures Windows to
// wrap those already-tunneled queries in HTTPS instead of leaving them plain
// UDP (requires Win 11 / Win 10 KB5033375+).
func ConfigureDoH() error {
	out, err := hiddenCmd("netsh", "dns", "add", "encryption",
		"server="+dohServer,
		"dohtemplate="+dohTemplate,
		"autoupgrade=yes",
		"udpfallback=no",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh dns add encryption: %w (out: %s)", err, out)
	}
	core.Log.Printf("dns: DoH enabled â€” %s â†’ %s (tunneled)", dohServer, dohTemplate)
	return nil
}

// CleanupDoH removes any lingering DoH configuration left by a previous session
// that ended without a clean disconnect (crash, hard reboot, etc.).
// Called once at startup before any connection attempt.
func CleanupDoH() {
	out, err := hiddenCmd("netsh", "dns", "delete", "encryption",
		"server="+dohServer,
	).CombinedOutput()
	if err != nil {
		// Not an error if the config was not set â€” ignore.
		core.Log.Printf("dns: startup cleanup: no DoH config found (%s)", out)
	} else {
		core.Log.Printf("dns: startup cleanup: removed stale DoH config for %s", dohServer)
	}
}

// ConfigureDoHFallback is the fallback path for Windows versions that do not
// support "netsh dns add encryption" (older Win 10 builds). It starts a local
// UDP-to-DoH proxy on 127.0.0.1:53 and points the TUN adapter's DNS at it, so
// every DNS query from applications is captured by TUN (as always) and then
// resolved via encrypted HTTPS through the tunnel instead of plain UDP.
//
// Returns the running proxy (pass to StopDoHFallback on disconnect) or an error.
func ConfigureDoHFallback() (*DoHProxy, error) {
	proxy := newDoHProxy()
	if err := proxy.start(); err != nil {
		return nil, fmt.Errorf("start DoH proxy: %w", err)
	}

	if err := setTUNDNS(tunIfaceName, "127.0.0.1"); err != nil {
		proxy.stop()
		return nil, fmt.Errorf("set TUN DNS to DoH proxy: %w", err)
	}
	core.Log.Printf("dns: DoH fallback active â€” TUN DNS=127.0.0.1 â†’ local proxy â†’ %s via tunnel", dohTemplate)
	return proxy, nil
}

// StopDoHFallback stops the DoH proxy started by ConfigureDoHFallback and
// points the TUN adapter's DNS directly at dohServer (1.1.1.1) instead of
// resetting to DHCP -- so plain UDP:53 queries keep being captured by TUN and
// carried through the tunnel (still handled by udp_assoc.go, just no longer
// HTTPS-wrapped). Safe to call for both a live DoH-off toggle and a full
// disconnect: routes.go never adds a DNS bypass route in either case, so
// there is nothing here that needs to special-case "the tunnel is going
// away" -- the TUN adapter itself gets torn down separately as part of
// disconnect, at which point this DNS setting stops mattering.
func StopDoHFallback(proxy *DoHProxy) {
	if proxy == nil {
		return
	}
	proxy.stop()
	if err := setTUNDNS(tunIfaceName, dohServer); err != nil {
		core.Log.Printf("dns: point TUN DNS at %s: %v", dohServer, err)
	} else {
		core.Log.Printf("dns: DoH proxy stopped, TUN DNS now %s directly (plain UDP, still tunneled)", dohServer)
	}
}

// RestoreDoH removes the DoH configuration added by ConfigureDoH. Once
// removed, Windows falls back to querying dohServer in plain UDP -- still
// captured by TUN and tunneled by udp_assoc.go, same reasoning as
// StopDoHFallback above.
func RestoreDoH() {
	out, err := hiddenCmd("netsh", "dns", "delete", "encryption",
		"server="+dohServer,
	).CombinedOutput()
	if err != nil {
		core.Log.Printf("dns: DoH remove failed: %v (out: %s)", err, out)
	} else {
		core.Log.Printf("dns: DoH removed for %s (queries stay plain-UDP, still tunneled)", dohServer)
	}
}
