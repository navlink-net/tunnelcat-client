// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"tunnel_cat/snc/core"
)

const routeCmdTimeout = 8 * time.Second

// routeCmd runs /sbin/route with the given arguments.
func routeCmd(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), routeCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/sbin/route", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route %v: %w (out: %s)", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteManager saves the original default gateway and installs split-tunnel
// routes so that:
//   - Traffic to the control node(s) keeps going via the original gateway
//     (avoids a routing loop through the tunnel).
//   - All other traffic goes through the TUN interface (0/1 and 128/1 together
//     cover every address and out-metric the original default route).
//   - DNS (port 53) is captured by TUN like everything else and handled by
//     snc/core/udp_assoc.go's own per-mode logic, which tunnels it by default.
//     No OS-level route exclusion for the DNS server IP exists here on
//     purpose: an earlier "DNS bypass" route (removed 2026-08-12, see the
//     incident where a user's Google/YouTube/WhatsApp broke after DoH got
//     toggled off) pre-empted that logic entirely by sending DNS packets out
//     the physical interface before they ever reached TUN.
type RouteManager struct {
	origGW      string
	localAddr   string   // physical outbound IP, captured before TUN routes are applied
	physIface   string   // physical outbound interface name (e.g. "en0")
	serverAddrs []string // resolved control-node IPs
	ipv6Blocked bool     // true if Apply added the pf IPv6 block; tells Remove whether to lift it
}

// Prepare resolves serverHost to IP addresses and records them for Apply.
// Call once before Apply so bypass routes can be added before the default route changes.
func (r *RouteManager) Prepare(serverHost string) {
	host, _, err := net.SplitHostPort(serverHost)
	if err != nil {
		host = serverHost
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		core.Log.Printf("routes: DNS resolve %q: %v", host, err)
		return
	}
	for _, a := range addrs {
		if net.ParseIP(a) != nil {
			r.serverAddrs = append(r.serverAddrs, a)
		}
	}
}

// Apply reads the current default gateway and installs split-tunnel routes.
func (r *RouteManager) Apply() error {
	// Capture physical outbound IP before TUN routes change the routing table.
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			r.localAddr = a.IP.String()
		}
		conn.Close()
	}
	if r.localAddr != "" {
		core.Log.Printf("routes: local addr %s", r.localAddr)
	}

	gw, iface, err := defaultGateway()
	if err != nil {
		return fmt.Errorf("routes: detect default gateway: %w", err)
	}
	r.origGW = gw
	r.physIface = iface
	core.Log.Printf("routes: default gateway %s iface %s", gw, iface)

	// Bypass routes for server IPs â€” must be added before the split routes so
	// control traffic does not go through the tunnel.
	for _, ip := range r.serverAddrs {
		if err := routeCmd("add", "-host", ip, gw); err != nil {
			core.Log.Printf("routes: WARN bypass %s: %v", ip, err)
		} else {
			core.Log.Printf("routes: bypass %s â†’ %s", ip, gw)
		}
	}

	// No DNS bypass route here on purpose â€” see the RouteManager doc comment.
	// DNS (port 53) is captured by TUN and handled by udp_assoc.go.

	// Split-tunnel: redirect all traffic through the TUN IP.
	// Two /1 routes together cover 0.0.0.0/0 and have a lower routing metric
	// than the original default route, so they take priority.
	tunGW := core.TUNAddr
	for _, cidr := range []string{"0/1", "128/1"} {
		if err := routeCmd("add", "-net", cidr, tunGW); err != nil {
			return fmt.Errorf("routes: add %s: %w", cidr, err)
		}
		core.Log.Printf("routes: %s â†’ %s (TUN)", cidr, tunGW)
	}

	// IPv6: when the arbiter's manifest says no exit in the fleet can route
	// IPv6 (core.IPv6TunnelDisabled), block it system-wide via pf instead of
	// just skipping the split route. Not routing IPv6 into the tunnel is not
	// the same as blocking it: with no IPv6 route on the TUN interface,
	// IPv6-capable apps just use the physical interface's own IPv6 directly,
	// completely bypassing the VPN -- a real IP leak, not just "no IPv6
	// through the tunnel" (confirmed live 2026-08-16, mirrors the same leak
	// already fixed on Windows via a firewall rule, 2026-08-15). See
	// snc/core/discovery.go, admin_ipv6.go.
	if core.IPv6TunnelDisabled() {
		core.Log.Printf("routes: IPv6 disabled by arbiter â€” blocking IPv6 egress via pf")
		if err := setIPv6PfBlock(true); err != nil {
			core.Log.Printf("routes: WARN block IPv6 egress: %v", err)
		} else {
			r.ipv6Blocked = true
			core.Log.Printf("routes: IPv6 egress blocked via pf anchor %s", pfIPv6AnchorName)
		}
	} else {
		for _, cidr6 := range []string{"::/1", "8000::/1"} {
			if err := routeCmd("add", "-inet6", "-net", cidr6, core.TUNIPv6Addr); err != nil {
				core.Log.Printf("routes: WARN IPv6 %s: %v", cidr6, err)
			} else {
				core.Log.Printf("routes: IPv6 %s â†’ %s (TUN)", cidr6, core.TUNIPv6Addr)
			}
		}
	}

	return nil
}

// pfIPv6AnchorName is a dedicated pf anchor so this block is scoped to our
// own rule and never touches the user's main pf.conf or any other app's
// rules. Fixed name so add is idempotent and Remove reliably finds it even
// across process restarts (e.g. a previous run that crashed after Apply).
const pfIPv6AnchorName = "com.shortnerdcat/blockipv6"

// setIPv6PfBlock adds or removes a pf anchor rule blocking all outbound IPv6
// traffic system-wide. Deliberately not scoped to the TUN or physical
// adapter alone -- the goal is to stop IPv6 leaking out *any* interface
// while the arbiter's kill switch is active, mirroring Windows'
// setIPv6FirewallBlock (routes.go).
func setIPv6PfBlock(enable bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), routeCmdTimeout)
	defer cancel()
	if enable {
		cmd := exec.CommandContext(ctx, "/sbin/pfctl", "-a", pfIPv6AnchorName, "-f", "-")
		cmd.Stdin = strings.NewReader("block out quick inet6 all\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("pfctl load anchor: %w (out: %s)", err, strings.TrimSpace(string(out)))
		}
		// Ensure pf itself is enabled -- harmless (and non-fatal) if already on.
		exec.CommandContext(ctx, "/sbin/pfctl", "-e").Run() //nolint:errcheck
		return nil
	}
	out, err := exec.CommandContext(ctx, "/sbin/pfctl", "-a", pfIPv6AnchorName, "-F", "rules").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl flush anchor: %w (out: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Remove deletes the split-tunnel routes and restores connectivity.
// Safe to call even if Apply was never called or partially failed.
func (r *RouteManager) Remove() {
	core.Log.Printf("routes: removing split-tunnel (origGW=%s)", r.origGW)

	for _, cidr := range []string{"0/1", "128/1"} {
		routeCmd("delete", "-net", cidr) //nolint:errcheck
	}
	for _, cidr6 := range []string{"::/1", "8000::/1"} {
		routeCmd("delete", "-inet6", "-net", cidr6) //nolint:errcheck
	}

	for _, ip := range r.serverAddrs {
		routeCmd("delete", "-host", ip) //nolint:errcheck
	}

	if r.ipv6Blocked {
		if err := setIPv6PfBlock(false); err != nil {
			core.Log.Printf("routes: WARN remove IPv6 egress block: %v", err)
		} else {
			core.Log.Printf("routes: IPv6 egress block removed")
		}
		r.ipv6Blocked = false
	}

	core.Log.Printf("routes: removed")
}

// AddBypass installs a host route for ip via the original gateway, identical
// to the per-server bypass routes set by Apply. Call after Apply to give
// additional hosts (controls) a direct path so they are never routed through
// the TUN. Idempotent: skips IPs already tracked.
func (r *RouteManager) AddBypass(ip string) {
	if r.origGW == "" {
		return
	}
	for _, existing := range r.serverAddrs {
		if existing == ip {
			return
		}
	}
	if err := routeCmd("add", "-host", ip, r.origGW); err != nil {
		core.Log.Printf("routes: WARN add bypass %s: %v", ip, err)
		return
	}
	r.serverAddrs = append(r.serverAddrs, ip)
	core.Log.Printf("routes: added bypass %s â†’ %s", ip, r.origGW)
}

// OrigGW returns the original default gateway captured during Apply.
func (r *RouteManager) OrigGW() string { return r.origGW }

// PhysIface returns the outbound interface name (e.g. "en0") captured during Apply.
func (r *RouteManager) PhysIface() string { return r.physIface }

// LocalAddr returns the physical outbound IP address captured at the start of Apply,
// before TUN routes redirect traffic. Used by decoy manager and relay clients.
func (r *RouteManager) LocalAddr() string { return r.localAddr }

// defaultGateway parses the output of "netstat -rn -f inet" to find the
// current default IPv4 gateway and the outbound interface name.
func defaultGateway() (gw, iface string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), routeCmdTimeout)
	defer cancel()
	out, execErr := exec.CommandContext(ctx, "netstat", "-rn", "-f", "inet").Output()
	if execErr != nil {
		return "", "", execErr
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		// Lines look like: "default   192.168.1.1   UGScg   en0"
		if !strings.HasPrefix(line, "default") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && net.ParseIP(fields[1]) != nil {
			gw = fields[1]
			if len(fields) >= 4 {
				iface = fields[3]
			}
			return gw, iface, nil
		}
	}
	return "", "", fmt.Errorf("default gateway not found in netstat output")
}

// CleanupSplitRoutes removes split-tunnel routes without a live RouteManager.
// Called by the watchdog on unclean exit.
func CleanupSplitRoutes(origGW string) {
	core.Log.Printf("routes: watchdog cleanup (origGW=%q)", origGW)
	routeCmd("delete", "-net", "0/1")                //nolint:errcheck
	routeCmd("delete", "-net", "128/1")              //nolint:errcheck
	routeCmd("delete", "-inet6", "-net", "::/1")     //nolint:errcheck
	routeCmd("delete", "-inet6", "-net", "8000::/1") //nolint:errcheck
	cleanupBypassHostRoutes()
}

// cleanupBypassHostRoutes deletes all static host routes (UGHS) left by a
// previous RouteManager.Apply() call that was interrupted before Remove().
func cleanupBypassHostRoutes() {
	ctx, cancel := context.WithTimeout(context.Background(), routeCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		flags := fields[2]
		// UGHS = Up Gateway Host Static â€” our bypass routes.
		if strings.Contains(flags, "H") && strings.Contains(flags, "S") {
			ip := fields[0]
			if net.ParseIP(ip) != nil {
				core.Log.Printf("routes: cleanup stale bypass host route %s", ip)
				routeCmd("delete", "-host", ip) //nolint:errcheck
			}
		}
	}
}
