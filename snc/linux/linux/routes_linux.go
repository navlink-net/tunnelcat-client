// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

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

// ipCmd runs /sbin/ip (or ip from PATH) with the given arguments.
func ipCmd(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), routeCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %v: %w (out: %s)", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteManager saves the original default gateway and installs split-tunnel
// routes so that:
//   - Traffic to the control server(s) keeps going via the original gateway.
//   - All other traffic goes through the TUN interface (0/1 + 128/1).
//   - DNS (port 53) is captured by TUN like everything else and handled by
//     snc/core/udp_assoc.go's own per-mode logic, which tunnels it by default.
//     No OS-level route exclusion for the DNS server IP exists here on
//     purpose: an earlier "DNS bypass" route (removed 2026-08-12, see the
//     incident where a user's Google/YouTube/WhatsApp broke on another
//     platform after DoH got toggled off) pre-empted that logic entirely.
//     Every call site in this client had already disabled it by hand before
//     this cleanup; this just makes that the only behavior instead of a
//     per-call-site opt-out.
type RouteManager struct {
	origGW      string
	origIface   string   // physical outbound interface name (e.g. "eth0", "wlan0")
	localAddr   string   // physical outbound IP before TUN routes are applied
	serverAddrs []string // resolved control server IPs
	ipv6Blocked bool     // true if Apply added the ip6tables IPv6 block; tells Remove whether to lift it
}

// Prepare resolves serverHost to IP addresses for bypass routes.
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

	gw, iface, err := defaultGatewayLinux()
	if err != nil {
		return fmt.Errorf("routes: detect default gateway: %w", err)
	}
	r.origGW = gw
	r.origIface = iface
	core.Log.Printf("routes: default gateway %s iface %s", gw, iface)

	// Bypass routes for server IPs â€” must precede the split routes.
	for _, ip := range r.serverAddrs {
		if err := ipCmd("route", "add", ip+"/32", "via", gw, "proto", "static"); err != nil {
			core.Log.Printf("routes: WARN bypass %s: %v", ip, err)
		} else {
			core.Log.Printf("routes: bypass %s â†’ %s", ip, gw)
		}
	}

	// No DNS bypass route added here on purpose â€” see the RouteManager doc
	// comment. Still delete any stale one left by an older binary version or
	// a crashed previous session: it would intercept DNS queries from
	// systemd-resolved before they reach the TUN, and the iptables DNS-leak
	// block on the physical NIC would then drop them silently.
	ipCmd("route", "del", "1.1.1.1/32") //nolint:errcheck â€” ignore if not present

	// Split-tunnel: two /1 routes cover 0.0.0.0/0 with lower metric than default.
	tunGW := core.TUNAddr
	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := ipCmd("route", "add", cidr, "via", tunGW, "proto", "static"); err != nil {
			return fmt.Errorf("routes: add %s: %w", cidr, err)
		}
		core.Log.Printf("routes: %s â†’ %s (TUN)", cidr, tunGW)
	}

	// IPv6: when the arbiter's manifest says no exit in the fleet can route
	// IPv6 (core.IPv6TunnelDisabled), block it system-wide via ip6tables
	// instead of just skipping the split route. Not routing IPv6 into the
	// tunnel is not the same as blocking it: with no IPv6 route on the TUN
	// interface, IPv6-capable apps just use the physical interface's own
	// IPv6 directly, completely bypassing the VPN -- a real IP leak, not
	// just "no IPv6 through the tunnel" (confirmed live 2026-08-16, mirrors
	// the same leak already fixed on Windows via a firewall rule,
	// 2026-08-15). See snc/core/discovery.go, admin_ipv6.go.
	if core.IPv6TunnelDisabled() {
		core.Log.Printf("routes: IPv6 disabled by arbiter â€” blocking IPv6 egress via ip6tables")
		if err := setIPv6TablesBlock(true); err != nil {
			core.Log.Printf("routes: WARN block IPv6 egress: %v", err)
		} else {
			r.ipv6Blocked = true
			core.Log.Printf("routes: IPv6 egress blocked via ip6tables chain %s", ip6tablesChain)
		}
	} else {
		for _, cidr6 := range []string{"::/1", "8000::/1"} {
			if err := ipCmd("-6", "route", "add", cidr6, "dev", core.TUNDeviceName); err != nil {
				core.Log.Printf("routes: WARN IPv6 %s: %v", cidr6, err)
			} else {
				core.Log.Printf("routes: IPv6 %s â†’ %s (TUN)", cidr6, core.TUNDeviceName)
			}
		}
	}

	return nil
}

// ip6tablesChain is a dedicated chain so this block is scoped to our own
// rules and never touches any other app's iptables state. Fixed name so add
// is idempotent and Remove reliably finds it even across process restarts
// (e.g. a previous run that crashed after Apply). Mirrors firewall_linux.go's
// iptablesChain pattern (a separate manager/concern -- DNS-leak safety net --
// so kept independent here rather than folded into FirewallManager).
const ip6tablesChain = "SHORTNERDCAT6"

// setIPv6TablesBlock adds or removes an ip6tables chain blocking all
// outbound IPv6 traffic system-wide. Deliberately not scoped to the TUN or
// physical adapter alone -- the goal is to stop IPv6 leaking out *any*
// interface while the arbiter's kill switch is active, mirroring Windows'
// setIPv6FirewallBlock (routes.go) and macOS's setIPv6PfBlock.
func setIPv6TablesBlock(enable bool) error {
	if !enable {
		exec.Command("ip6tables", "-D", "OUTPUT", "-j", ip6tablesChain).Run() //nolint:errcheck
		exec.Command("ip6tables", "-F", ip6tablesChain).Run()                 //nolint:errcheck
		exec.Command("ip6tables", "-X", ip6tablesChain).Run()                 //nolint:errcheck
		return nil
	}
	// Idempotent: -N returns nonzero (harmlessly) if the chain already exists.
	exec.Command("ip6tables", "-N", ip6tablesChain).Run() //nolint:errcheck
	if err := exec.Command("ip6tables", "-C", "OUTPUT", "-j", ip6tablesChain).Run(); err != nil {
		if out, err := exec.Command("ip6tables", "-I", "OUTPUT", "-j", ip6tablesChain).CombinedOutput(); err != nil {
			return fmt.Errorf("ip6tables -I OUTPUT: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command("ip6tables", "-A", ip6tablesChain, "-j", "DROP").CombinedOutput(); err != nil {
		return fmt.Errorf("ip6tables block: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Remove deletes the split-tunnel routes and restores connectivity.
func (r *RouteManager) Remove() {
	core.Log.Printf("routes: removing split-tunnel (origGW=%s)", r.origGW)

	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		ipCmd("route", "del", cidr) //nolint:errcheck
	}
	for _, cidr6 := range []string{"::/1", "8000::/1"} {
		ipCmd("-6", "route", "del", cidr6) //nolint:errcheck
	}
	for _, ip := range r.serverAddrs {
		ipCmd("route", "del", ip+"/32") //nolint:errcheck
	}

	if r.ipv6Blocked {
		if err := setIPv6TablesBlock(false); err != nil {
			core.Log.Printf("routes: WARN remove IPv6 egress block: %v", err)
		} else {
			core.Log.Printf("routes: IPv6 egress block removed")
		}
		r.ipv6Blocked = false
	}

	core.Log.Printf("routes: removed")
}

// AddBypass installs a host route for ip via the original gateway.
// Idempotent: skips IPs already tracked.
func (r *RouteManager) AddBypass(ip string) {
	if r.origGW == "" {
		return
	}
	for _, existing := range r.serverAddrs {
		if existing == ip {
			return
		}
	}
	if err := ipCmd("route", "add", ip+"/32", "via", r.origGW, "proto", "static"); err != nil {
		core.Log.Printf("routes: WARN add bypass %s: %v", ip, err)
		return
	}
	r.serverAddrs = append(r.serverAddrs, ip)
	core.Log.Printf("routes: added bypass %s â†’ %s", ip, r.origGW)
}

// OrigGW returns the original default gateway captured during Apply.
func (r *RouteManager) OrigGW() string { return r.origGW }

// PhysIface returns the outbound interface name captured during Apply.
func (r *RouteManager) PhysIface() string { return r.origIface }

// LocalAddr returns the physical outbound IP captured at the start of Apply.
func (r *RouteManager) LocalAddr() string { return r.localAddr }

// defaultGatewayLinux parses `ip route show default` to find the current
// default IPv4 gateway and outbound interface name.
func defaultGatewayLinux() (gw, iface string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), routeCmdTimeout)
	defer cancel()
	out, execErr := exec.CommandContext(ctx, "ip", "route", "show", "default").Output()
	if execErr != nil {
		return "", "", execErr
	}
	// Output looks like: "default via 192.168.1.1 dev eth0 proto dhcp ..."
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[0] != "default" {
			continue
		}
		for i, f := range fields {
			if f == "via" && i+1 < len(fields) {
				gw = fields[i+1]
			}
			if f == "dev" && i+1 < len(fields) {
				iface = fields[i+1]
			}
		}
		if gw != "" {
			return gw, iface, nil
		}
	}
	return "", "", fmt.Errorf("default gateway not found in ip route output")
}

// CleanupSplitRoutes removes split-tunnel routes without a live RouteManager.
// Called by the watchdog on unclean exit.
func CleanupSplitRoutes(origGW string) {
	core.Log.Printf("routes: watchdog cleanup (origGW=%q)", origGW)
	ipCmd("route", "del", "0.0.0.0/1")      //nolint:errcheck
	ipCmd("route", "del", "128.0.0.0/1")    //nolint:errcheck
	ipCmd("-6", "route", "del", "::/1")     //nolint:errcheck
	ipCmd("-6", "route", "del", "8000::/1") //nolint:errcheck
	// Remove any static host routes with metric 0 (our bypass routes).
	cleanupBypassHostRoutes()
}

// cleanupBypassHostRoutes deletes static host routes left by a previous
// RouteManager.Apply() call that was interrupted before Remove().
func cleanupBypassHostRoutes() {
	ctx, cancel := context.WithTimeout(context.Background(), routeCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", "route", "show", "table", "main").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Static host routes look like: "1.2.3.4 via 192.168.1.1 dev eth0 proto static"
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// Check for /32 host route with proto static (added by us since this fix) or
		// proto boot / no proto (added by older code without explicit proto flag).
		isHost := strings.HasSuffix(fields[0], "/32") || !strings.Contains(fields[0], "/")
		isOurs := false
		for _, f := range fields {
			if f == "static" || f == "boot" {
				isOurs = true
				break
			}
		}
		// Routes with no proto keyword at all are also ours (older sessions).
		hasProtoKeyword := false
		for _, f := range fields {
			if f == "proto" {
				hasProtoKeyword = true
				break
			}
		}
		if !hasProtoKeyword {
			isOurs = true
		}
		if isHost && isOurs {
			ip := strings.TrimSuffix(fields[0], "/32")
			if net.ParseIP(ip) != nil {
				core.Log.Printf("routes: cleanup stale bypass host route %s", ip)
				ipCmd("route", "del", fields[0]) //nolint:errcheck
			}
		}
	}
}
