// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tunnel_cat/binlog"
	"tunnel_cat/logevent"
	"tunnel_cat/snc/core"
)

const cmdTimeout = 8 * time.Second // max wait for any netsh/route child process

// hiddenCmd wraps exec.Command and sets CREATE_NO_WINDOW so that the child
// process does not flash a console window in a -H windowsgui build.
// A timeout context is attached so a hung netsh/route process is killed after
// cmdTimeout rather than blocking the connect sequence indefinitely.
func hiddenCmd(name string, args ...string) *exec.Cmd {
	ctx, _ := context.WithTimeout(context.Background(), cmdTimeout) //nolint:govet
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd
}

// RouteManager saves the original default gateway and installs split-tunnel
// routes so that:
//   - Traffic to the control node(s) keeps going via the original gateway
//     (avoids a routing loop through the tunnel itself).
//   - All other traffic goes through the TUN interface (0.0.0.0/1 and
//     128.0.0.0/1 together cover every address and out-metric the original
//     0.0.0.0/0 default route).
//   - DNS (port 53) is captured by TUN like everything else and handled by
//     snc/core/udp_assoc.go's own per-mode logic, which already tunnels it
//     by default and only goes direct in WildCat mode (see that file's
//     forwardOutbound). No OS-level route exclusion for the DNS server IP
//     exists here on purpose: an earlier "DNS bypass" route (removed
//     2026-08-12, see the incident where a user's Google/YouTube/WhatsApp
//     broke after DoH got toggled off) pre-empted that logic entirely by
//     sending DNS packets out the physical NIC before they ever reached TUN,
//     leaking every query to the ISP in plain text whenever DoH wasn't
//     separately forcing them back through the tunnel.
type RouteManager struct {
	origGW      string
	serverAddrs []string // resolved control-node IPs
	localIP     string   // original NIC IP recorded during Apply
	ipv6Blocked bool     // true if Apply added the IPv6 firewall block; tells Restore whether to remove it
}

// LocalAddr returns the original NIC IP as recorded during Apply.
// Returns "" if Apply has not been called yet.
func (r *RouteManager) LocalAddr() string { return r.localIP }

// OrigGW returns the original default gateway as recorded during Apply.
// Returns "" if Apply has not been called yet.
func (r *RouteManager) OrigGW() string { return r.origGW }

// tunIfaceName must match core.TUNDeviceName.
const tunIfaceName = "ShortNerdCat"

// NewRouteManager returns a RouteManager ready to use.
func NewRouteManager() *RouteManager {
	return &RouteManager{}
}

// Apply installs the split-tunnel routes.
//
//   - serverHost: hostname or host:port of the control node.
//   - tunGW: TUN interface IP that will act as gateway (e.g. "198.18.0.1").
func (r *RouteManager) Apply(serverHost, tunGW string) error {
	// Strip port if present.
	host, _, err := net.SplitHostPort(serverHost)
	if err != nil {
		host = serverHost
	}

	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	r.serverAddrs = addrs
	logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
		logevent.Str(logevent.AttrStage, "server_resolved"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("%s -> %v", host, addrs)))

	gw, err := getDefaultGateway()
	if err != nil {
		return fmt.Errorf("get default gateway: %w", err)
	}
	r.origGW = gw
	logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
		logevent.Str(logevent.AttrStage, "gateway_found"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("gw=%s tunGW=%s", gw, tunGW)))

	// Record the original NIC IP before adding TUN routes.
	if lip, err := localIPToGateway(gw); err == nil {
		r.localIP = lip
		logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
			logevent.Str(logevent.AttrStage, "local_ip_found"),
			logevent.Str(logevent.AttrDetail, lip))
	} else {
		logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
			logevent.Str(logevent.AttrStage, "local_ip_failed"),
			logevent.Str(logevent.AttrErr, err.Error()))
	}

	// Host routes for control-node IPs bypass the tunnel entirely.
	for _, addr := range addrs {
		if err := routeCmd("add", addr, "mask", "255.255.255.255", gw); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
				logevent.Str(logevent.AttrStage, "server_bypass_failed"),
				logevent.Str(logevent.AttrDetail, addr),
				logevent.Str(logevent.AttrErr, err.Error()))
			r.Restore()
			return fmt.Errorf("add server route %s: %w", addr, err)
		}
		logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
			logevent.Str(logevent.AttrStage, "server_bypass_added"),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("%s via %s", addr, gw)))
	}

	// No DNS bypass route here on purpose â€” see the RouteManager doc comment.
	// DNS (port 53) is captured by TUN and handled by udp_assoc.go.

	// Find the TUN adapter's interface index.  Without it, Windows assigns the
	// split routes to the physical adapter (which has no path to 198.18.0.1),
	// so packets are dropped instead of going through WinTun.
	tunIdx, err := ifaceIndexByIP(tunGW)
	if err != nil {
		return fmt.Errorf("find TUN interface index for %s: %w", tunGW, err)
	}
	tunIdxStr := strconv.Itoa(tunIdx)
	logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
		logevent.Str(logevent.AttrStage, "tun_iface_found"),
		logevent.Str(logevent.AttrDetail, tunIdxStr))

	// Split-default routes redirect everything else through the TUN.
	// From this point on all traffic goes via TUN â€” keep post-route work minimal.
	if err := routeCmd("add", "0.0.0.0", "mask", "128.0.0.0", tunGW, "metric", "1", "if", tunIdxStr); err != nil {
		r.Restore()
		return fmt.Errorf("add split route 0/1: %w", err)
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
		logevent.Str(logevent.AttrStage, "split_route_added"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("0.0.0.0/1 via %s if %d metric 1", tunGW, tunIdx)))

	if err := routeCmd("add", "128.0.0.0", "mask", "128.0.0.0", tunGW, "metric", "1", "if", tunIdxStr); err != nil {
		r.Restore()
		return fmt.Errorf("add split route 128/1: %w", err)
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
		logevent.Str(logevent.AttrStage, "split_route_added"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("128.0.0.0/1 via %s if %d metric 1", tunGW, tunIdx)))

	// IPv6 split-default routes â€” mirror Android addRoute("::", 0).
	// ::/1 and 8000::/1 together cover all IPv6 addresses and out-metric any
	// existing ::/0 default route on the physical interface.
	// Skipped entirely when the arbiter's manifest says no exit in the fleet
	// can route IPv6 (core.IPv6TunnelDisabled) -- routing it into the tunnel
	// in that case just hands IPv6-preferred apps (Facebook, WhatsApp) a dead
	// address instead of letting them fall back to IPv4, which exits can
	// actually dial. See tunnel_cat/snc/core/discovery.go, admin_ipv6.go.
	//
	// Not routing IPv6 into the tunnel is not the same as blocking it: with
	// no IPv6 split route, IPv6-capable apps still reach the internet
	// directly over the physical adapter's own IPv6 connectivity, completely
	// bypassing the VPN -- a real IP leak, not just "no IPv6 through the
	// tunnel" (confirmed live 2026-08-15: the arbiter's ipv6_enabled=0 kill
	// switch was active but nothing was actually blocking egress). So when
	// the kill switch is on, also add an outbound Windows Firewall block for
	// all IPv6 traffic -- removed again in Restore().
	if core.IPv6TunnelDisabled() {
		logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes, logevent.Str(logevent.AttrStage, "ipv6_disabled_blocking"))
		if err := setIPv6FirewallBlock(true); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
				logevent.Str(logevent.AttrStage, "ipv6_block_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
		} else {
			r.ipv6Blocked = true
			logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes, logevent.Str(logevent.AttrStage, "ipv6_block_ok"))
		}
	} else {
		for _, pfx := range []string{"::/1", "8000::/1"} {
			if err := ip6RouteCmd("add", pfx); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
					logevent.Str(logevent.AttrStage, "ipv6_route_failed"),
					logevent.Str(logevent.AttrDetail, pfx),
					logevent.Str(logevent.AttrErr, err.Error()))
			} else {
				logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
					logevent.Str(logevent.AttrStage, "ipv6_route_added"),
					logevent.Str(logevent.AttrDetail, pfx))
			}
		}
	}

	return nil
}

// AddBypass installs a host-route for ip via the original gateway, identical
// to the per-server bypass routes set by Apply.  Call after Apply to give
// additional control servers a direct path so TunnelDialers can reach them
// without routing through TUN (prevents loops and enables silent path switch).
// Idempotent: silently skips IPs already in r.serverAddrs.
func (r *RouteManager) AddBypass(ip string) {
	if r.origGW == "" {
		return
	}
	for _, existing := range r.serverAddrs {
		if existing == ip {
			return
		}
	}
	if err := routeCmd("add", ip, "mask", "255.255.255.255", r.origGW); err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
			logevent.Str(logevent.AttrStage, "bypass_failed"),
			logevent.Str(logevent.AttrDetail, ip),
			logevent.Str(logevent.AttrErr, err.Error()))
		return
	}
	r.serverAddrs = append(r.serverAddrs, ip)
	logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
		logevent.Str(logevent.AttrStage, "bypass_added"),
		logevent.Str(logevent.AttrDetail, fmt.Sprintf("%s via %s", ip, r.origGW)))
}

// Restore removes the TUN routes and the per-server bypass routes.
// The original default route is untouched (it was never removed).
func (r *RouteManager) Restore() {
	// Ignore errors â€” best-effort cleanup.
	routeCmd("delete", "0.0.0.0", "mask", "128.0.0.0")
	routeCmd("delete", "128.0.0.0", "mask", "128.0.0.0")
	for _, addr := range r.serverAddrs {
		routeCmd("delete", addr, "mask", "255.255.255.255")
	}
	ip6RouteCmd("delete", "::/1")     //nolint:errcheck
	ip6RouteCmd("delete", "8000::/1") //nolint:errcheck
	if r.ipv6Blocked {
		if err := setIPv6FirewallBlock(false); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes,
				logevent.Str(logevent.AttrStage, "ipv6_unblock_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
		} else {
			logevent.Emit(binlog.TagSystem, logevent.EventWinRoutes, logevent.Str(logevent.AttrStage, "ipv6_unblocked"))
		}
		r.ipv6Blocked = false
	}
}

// ipv6FirewallRuleName is the Windows Firewall rule name used to block IPv6
// egress when the arbiter's fleet-wide kill switch is on (core.IPv6TunnelDisabled).
// Fixed name so add is idempotent (netsh just adds a duplicate-named rule
// harmlessly) and remove reliably finds it even across process restarts --
// e.g. if a previous run crashed after Apply but before Restore.
const ipv6FirewallRuleName = "ShortNerdCat-BlockIPv6Egress"

// setIPv6FirewallBlock adds or removes an outbound Windows Firewall rule
// blocking all IPv6 traffic (any protocol, any remote address) system-wide.
// This is deliberately not scoped to the TUN or physical adapter alone:
// the goal is to stop IPv6 leaking out *any* interface while the arbiter's
// kill switch is active, not just to keep it out of the tunnel.
// ipv6AnyRemoteIP matches every IPv6 address for netsh advfirewall's
// remoteip= parameter. The obvious CIDR form "::/0" is rejected outright --
// confirmed live 2026-08-15, netsh's own error is "One or more of the
// address prefixes is invalid." -- netsh advfirewall's address-prefix
// parser doesn't accept the compressed all-zeros form combined with a /0
// mask, even though it's valid CIDR everywhere else in this codebase (the
// exact same "::/0" + "8000::/1" split-route pair a few lines up in Apply
// works fine with "netsh interface ipv6 add route", a completely different
// netsh subsystem with its own parser). The documented, verified
// workaround is the explicit start-end range spanning the whole address
// space instead of CIDR shorthand.
const ipv6AnyRemoteIP = "::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"

func setIPv6FirewallBlock(enable bool) error {
	if enable {
		// Remove any stale rule first (e.g. left over from a crashed prior
		// run) so this is idempotent rather than accumulating duplicates.
		hiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+ipv6FirewallRuleName).Run() //nolint:errcheck
		out, err := hiddenCmd("netsh", "advfirewall", "firewall", "add", "rule",
			"name="+ipv6FirewallRuleName, "dir=out", "action=block",
			"protocol=any", "remoteip="+ipv6AnyRemoteIP, "enable=yes").CombinedOutput()
		if err != nil {
			return fmt.Errorf("netsh advfirewall add rule: %w (output: %s)", err, out)
		}
		return nil
	}
	out, err := hiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule",
		"name="+ipv6FirewallRuleName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh advfirewall delete rule: %w (output: %s)", err, out)
	}
	return nil
}

// ip6RouteCmd runs "netsh interface ipv6 add/delete route <prefix> <iface> [nexthop]".
func ip6RouteCmd(action, prefix string) error {
	var args []string
	if action == "add" {
		args = []string{"interface", "ipv6", "add", "route",
			prefix, tunIfaceName, core.TUNIPv6Addr,
			"metric=1", "store=active"}
	} else {
		args = []string{"interface", "ipv6", "delete", "route",
			prefix, tunIfaceName}
	}
	out, err := hiddenCmd("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh ipv6 %s route %s: %w (output: %s)", action, prefix, err, out)
	}
	return nil
}

// setTUNDNS sets (or clears) the DNS server on the named TUN adapter.
// dns="" resets the adapter to DHCP.
func setTUNDNS(iface, dns string) error {
	var args []string
	if dns == "" {
		args = []string{"interface", "ip", "set", "dns",
			"name=" + iface, "source=dhcp"}
	} else {
		args = []string{"interface", "ip", "set", "dns",
			"name=" + iface, "source=static", "address=" + dns, "validate=no"}
	}
	out, err := hiddenCmd("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh set dns: %w (output: %s)", err, out)
	}
	return nil
}

// getDefaultGateway parses "route print 0.0.0.0" to find the active default
// gateway. Returns the gateway IP as a string.
// Retries up to 5 times with 1-second delays: the routing table can be
// transiently unavailable while the WinTun adapter is being created or torn
// down, causing route.exe to exit with status 1.
func getDefaultGateway() (string, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		gw, err := tryGetDefaultGateway()
		if err == nil {
			return gw, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func tryGetDefaultGateway() (string, error) {
	out, err := hiddenCmd("route", "print", "0.0.0.0").Output()
	if err != nil {
		return "", fmt.Errorf("route print: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		// IPv4 route table lines: Network  Netmask  Gateway  Interface  Metric
		if len(fields) >= 3 && fields[0] == "0.0.0.0" && fields[1] == "0.0.0.0" {
			return fields[2], nil
		}
	}
	return "", fmt.Errorf("default gateway not found in route table")
}

// ifaceIndexByIP returns the index of the network interface that has the given
// IP address assigned.  Used to resolve the TUN adapter's index so we can
// pass it to "route add ... if <index>" and force the route onto WinTun.
func ifaceIndexByIP(ip string) (int, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ipStr string
			switch v := addr.(type) {
			case *net.IPNet:
				ipStr = v.IP.String()
			case *net.IPAddr:
				ipStr = v.IP.String()
			}
			if ipStr == ip {
				return iface.Index, nil
			}
		}
	}
	return 0, fmt.Errorf("no interface has IP %s", ip)
}

// localIPToGateway returns the local IP address the OS would use to reach
// the given gateway by opening a UDP "connection" (no packets sent).
func localIPToGateway(gateway string) (string, error) {
	conn, err := net.Dial("udp", gateway+":443")
	if err != nil {
		return "", err
	}
	conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	return host, err
}

func routeCmd(args ...string) error {
	out, err := hiddenCmd("route", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route %s: %w (output: %s)", strings.Join(args, " "), err, out)
	}
	return nil
}
