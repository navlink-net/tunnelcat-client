// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux

// snc-core is the Android/Linux core binary for ShortNerdCat.
//
// Launched by Kotlinâ€™s SNCVpnService as a subprocess. Configuration is
// passed through environment variables:
//
//	SNC_KEY                 SNC key string (required)
//	SNC_TUN_FD              integer fd of the open /dev/tun interface (required)
//	SNC_PROTECT_SOCKET      path of the Kotlin protect Unix socket (optional)
//	SNC_UID_SOCKET          path of the Kotlin per-app UID lookup Unix socket (optional, TUN mode only)
//	SNC_SOCKET              path for the IPC control socket created by this process (optional)
//	SNC_LOG_DIR             directory for rotating log files (default /data/local/tmp/snc-logs)
//	SNC_DATA_DIR            directory for nodeid and cidr cache (default = SNC_LOG_DIR)
//	SNC_DATACHAN_DEPTH      dataChan buffer depth per TunnelConn (default 64; set proportional to RAM)
//
// IPC protocol (SNC_SOCKET): newline-delimited JSON, one command per connection.
//
//	{"cmd":"status"}                      â†’ StatusResponse
//	{"cmd":"stop"}                        â†’ {"ok":true}  then os.Exit(0)
//	{"cmd":"trim"}                        â†’ {"ok":true}  GC + FreeOSMemory (triggered by onTrimMemory)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	androidcore "tunnel_cat/android-core"
	snc "tunnel_cat/snc/core"
)

// bananameterProberOnce ensures the BananaMeter tunnel-diagnostics probe
// (see TODO.md "BananaMeter-based tunnel diagnostics") is started exactly
// once per process, even though the setup path that starts it can run
// multiple times over the process's life (reconnects).
var bananameterProberOnce sync.Once

// globalDisc is the single Discoverer instance for the lifetime of the process.
// Initialised once via discOnce so that early startup and the tunnel-connect
// path share the same object without racing.
var (
	globalDisc *snc.Discoverer
	discOnce   sync.Once

	mirrorMgr  *snc.MirrorManager
	mirrorOnce sync.Once
)

// lastKotlinHeartbeatUnix is updated by the "heartbeat" IPC command, sent
// every 60s by SNCVpnService's netHeartbeat thread. 0 means none received yet
// this process lifetime (e.g. Kotlin hasn't reached that point in startup).
// 2026-08-06 incident: that thread's own on-disk logging (snc_lifecycle.log)
// silently stopped for ~2h with nothing anywhere to show it. This gives the
// Go side (which already logs reliably) an independent, visible signal if
// the Kotlin-side thread goes quiet again, instead of it only showing up as
// an unexplained gap in a file nobody's watching in real time.
var lastKotlinHeartbeatUnix int64 // atomic unix seconds

// swapPoolGrowOnly applies nd to pool, unless doing so would shrink it.
// Every rebuild trigger (manifest update, dataFail, the periodic refresh
// timer) calls buildPoolDialers again, so blindly swapping each smaller
// candidate result in here regresses an already-healthy multi-dialer pool
// down to a single dialer repeatedly throughout the session -- confirmed in
// the field as session-wide slowdowns and total
// stalls on whichever site happened to be loading through that one dialer
// at the time. Confirmed-dead dialers are pruned via pool.Evict() directly
// (from FirstFailHook/DataFailHook), independent of this function, so
// refusing a shrinking refresh here never delays removing them -- it only
// skips swapping in a smaller candidate set that is still mid-resolution.
func swapPoolGrowOnly(pool *snc.DialerPool, nd []*snc.TunnelDialer) {
	if pool == nil || len(nd) == 0 {
		return
	}
	if len(nd) < pool.Size() {
		snc.Log.Printf("snc-core: pool: skip swap to %d dialer(s) — would shrink from %d (still-resolving partial result)", len(nd), pool.Size())
		return
	}
	pool.Swap(nd)
}

func main() {
	logDir := os.Getenv("SNC_LOG_DIR")
	if logDir == "" {
		logDir = "/data/local/tmp/snc-logs"
	}
	dataDir := os.Getenv("SNC_DATA_DIR")
	if dataDir == "" {
		dataDir = logDir
	}

	// Remove stale port files from a previous session so Kotlin waits in its
	// retry loops until fresh ports are written by the new process.
	// Without this, Kotlin reads the old port, connects immediately, and gets
	// "connection refused" because this new process hasn't bound the port yet.
	os.Remove(filepath.Join(dataDir, "snc.socks"))  //nolint:errcheck
	os.Remove(filepath.Join(dataDir, "snc.browse")) //nolint:errcheck

	if err := snc.InitLogging(logDir); err != nil {
		fmt.Fprintf(os.Stderr, "snc-core: logging: %v\n", err)
		os.Exit(1)
	}
	if err := snc.InitTunLogging(logDir); err != nil {
		snc.Log.Printf("snc-core: tun logging: %v (continuing without)", err)
	}
	// Tee: file (via InitLogging, which already feeds snc.LogRing -- the
	// periodic log-upload source, see androidcore.NewLogUploader below) +
	// stderr (logcat drain thread in Kotlin). A separate Android-only ring
	// buffer used to be teed in here too; removed 2026-08-11 once the
	// periodic uploader switched to snc.LogRing (same content, same 256KB
	// size) -- keeping both was pure redundant capture.
	snc.Log.SetOutput(io.MultiWriter(snc.Log.Writer(), os.Stderr))

	keyStr := os.Getenv("SNC_KEY")
	tunFDStr := os.Getenv("SNC_TUN_FD")
	protectSock := os.Getenv("SNC_PROTECT_SOCKET")
	uidSock := os.Getenv("SNC_UID_SOCKET")
	ipcSock := os.Getenv("SNC_SOCKET")

	// Apply dataChan depth from env â€” set before any TunnelConn is created.
	if depthStr := os.Getenv("SNC_DATACHAN_DEPTH"); depthStr != "" {
		if d, err := strconv.Atoi(depthStr); err == nil && d > 0 {
			snc.ChanDepth = d
			snc.Log.Printf("snc-core: dataChan depth=%d", d)
		}
	}

	// Leave 2 CPU cores free for Android system processes on devices with more than 4 cores.
	// Without this limit Go uses all cores, starving the kernel scheduler and modem threads
	// under heavy tunnel load, which can trigger thermal throttling or a watchdog reboot.
	if n := runtime.NumCPU(); n > 4 {
		runtime.GOMAXPROCS(n - 2)
		snc.Log.Printf("snc-core: GOMAXPROCS=%d (CPUs=%d, 2 reserved for system)", n-2, n)
	}

	// In proxy-only mode (SNC_PROXY_ONLY=1) there is no TUN â€” pure SOCKS5 proxy for embedding.
	isProxyOnly := os.Getenv("SNC_PROXY_ONLY") == "1"
	// Connection-stats admin-dashboard feature (see core.ConnStatsCollector):
	// set by SNCVpnService.kt/CoreProcess.kt when this process launch was NOT
	// a genuine user-initiated connect (internal reconnect, sticky restart).
	autoReconnect := os.Getenv("SNC_AUTO_RECONNECT") == "1"
	if keyStr == "" || (tunFDStr == "" && !isProxyOnly) {
		snc.Log.Printf("snc-core: SNC_KEY and SNC_TUN_FD are required")
		writeState(dataDir, "key_error")
		os.Exit(1)
	}
	if tunFDStr == "" {
		tunFDStr = "-1"
	}
	if isProxyOnly {
		snc.Log.Printf("snc-core: proxy-only mode â€” SOCKS5 proxy, no TUN")
	}

	tunFD, err := strconv.Atoi(tunFDStr)
	if err != nil {
		snc.Log.Printf("snc-core: invalid SNC_TUN_FD %q: %v", tunFDStr, err)
		os.Exit(1)
	}

	// router is declared here (before kd parsing) so it's available to
	// closures defined ahead of its assignment; assigned via snc.NewRouter()
	// at line ~636.
	var router *snc.Router
	// connStatsCollector lives for the whole process, not per-connect --
	// its event counters must accumulate across reconnects and only get
	// drained by the uploader's own tick (see ConnStatsCollector.Snapshot).
	// Unlike router it has no deferred-assignment dependency, so it's
	// created immediately rather than left nil until later.
	connStatsCollector := snc.NewConnStatsCollector(filepath.Join(dataDir, "connstats.json"))

	// Install protect hook before any socket is opened.
	protectWatchdogStop := make(chan struct{})
	uidWatchdogStop := make(chan struct{})
	var dotProxyAddr string
	if protectSock != "" {
		if err := connectProtectWithRetry(protectSock); err != nil {
			if tunFD >= 0 {
				// With a live TUN, every unprotected socket loops back through it
				// (routing loop) and dials never complete â€” the tunnel would be
				// permanently stuck with no way to recover within this process.
				// Exit so Kotlin tears down and restarts us with a fresh protect
				// socket rather than limping along unprotected forever.
				snc.Log.Printf("snc-core: protect socket: %v â€” exiting so Kotlin restarts us with a fresh socket", err)
				os.Exit(1)
			}
			snc.Log.Printf("snc-core: protect socket: %v (continuing without)", err)
		} else {
			snc.SetDialControl(androidcore.Protect.DialControl)
			androidcore.Protect.StartWatchdog(protectWatchdogStop)

			// Local DoT proxy: intercepts port-853 connections from the TUN so
			// Android Private DNS resolves correctly in networks where 8.8.8.8 is
			// blocked.  Must start after protect is wired so upstream DNS sockets
			// bypass the TUN (no routing loop).
			dotDialFn := func(addr string) (net.Conn, error) {
				d := net.Dialer{Timeout: 4 * time.Second, Control: androidcore.Protect.DialControl}
				return d.DialContext(context.Background(), "tcp", addr)
			}
			if dp, err := androidcore.NewDotProxy(dotDialFn); err != nil {
				snc.Log.Printf("snc-core: DoT proxy start failed: %v (continuing without)", err)
			} else {
				dotProxyAddr = dp.Addr()
				go dp.Serve()
			}
		}
	}

	// Per-app exit-IP stickiness (PickForUID, see dialer_pool.go) needs to
	// know which app owns each TUN-forwarded connection. Non-fatal if this
	// fails to connect -- androidcore.UID.Lookup returns -1 (unresolved)
	// when there's no client connection, and PickForUID treats an
	// unresolved UID as "no stickiness for this one connection" rather than
	// pinning it, so the tunnel still works, just without per-app grouping.
	if uidSock != "" {
		if err := androidcore.UID.Connect(uidSock); err != nil {
			snc.Log.Printf("snc-core: uid socket: %v (continuing without per-app exit stickiness)", err)
		} else {
			androidcore.UID.StartWatchdog(uidWatchdogStop)
		}
	}

	// Replace the default DNS resolver with one that uses protected sockets.
	// Two modes:
	//   Warmup (no VPN TUN active): TCP then UDP to public servers â€” Android's loopback
	//     DNS proxy at [::1]:53 is unreachable from a static Go binary (no JNI/libc).
	//     TCP:53 is tried first: many Russian ISPs block UDP:53 to external servers
	//     (transparent DNS proxy) while leaving TCP:53 open.
	//   Normal (VPN TUN active): TCP-only via protected sockets â€” Android blocks UDP
	//     socket creation (EPERM) in VPN subprocesses.
	if isProxyOnly {
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				servers := []string{"77.88.8.8:53", "77.88.8.1:53", "8.8.8.8:53", "1.1.1.1:53", "9.9.9.9:53"}
				var lastErr error
				for _, proto := range []string{"tcp", "udp"} {
					for _, srv := range servers {
						conn, err := d.DialContext(ctx, proto, srv)
						if err == nil {
							return conn, nil
						}
						lastErr = err
					}
				}
				return nil, lastErr
			},
		}
	} else {
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 3 * time.Second, Control: androidcore.Protect.DialControl}
				servers := []string{"77.88.8.8:53", "77.88.8.1:53", "8.8.8.8:53", "8.8.4.4:53", "1.1.1.1:53", "9.9.9.9:53"}
				var lastErr error
				for _, srv := range servers {
					conn, err := d.DialContext(ctx, "tcp", srv)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		}
	}

	// Persistent node identity.
	nodeID, err := snc.LoadOrGenNodeID(dataDir)
	if err != nil {
		snc.Log.Printf("snc-core: node id: %v", err)
		os.Exit(1)
	}
	snc.Log.Printf("snc-core: node id=%.16sÃ¢â‚¬Â¦", nodeID)

	// DHT gossip node â€” runs for the lifetime of the process, kept alive across
	// network changes so the relay registry stays warm.  UDP socket is protected
	// via dialControl so it bypasses the VPN TUN on Android.
	// Runs in proxy-only mode too: that's how the background keepalive
	// service (manifest + DHT, no TUN) stays on the gossip mesh.
	var dhtNode *snc.DHTNode
	if dhtID, err := snc.ParseDHTID(nodeID); err == nil {
		lc := net.ListenConfig{Control: androidcore.Protect.DialControl}
		if pc, err := lc.ListenPacket(context.Background(), "udp4", ":0"); err == nil {
			dhtNode = snc.NewDHTNode(dhtID, pc.(*net.UDPConn), filepath.Join(dataDir, "peers.json"))
			dhtNode.Bootstrap(nil) // load peers.json cache; no network seeds yet
			dhtNode.Start()
			snc.Log.Printf("dht: node started id=%.8sâ€¦ addr=%s", nodeID, pc.LocalAddr())

			// Relay server: punch back to whoever invites us and start forwarding.
			// Semaphore limits concurrent handlers so a flood of incoming DHT punch
			// requests cannot spawn unbounded goroutines.
			incomingPunchSem := make(chan struct{}, 5)
			dhtNode.SetHolePunchHandler(func(peerAddr, controlURL string) {
				select {
				case incomingPunchSem <- struct{}{}:
					defer func() { <-incomingPunchSem }()
				default:
					snc.Log.Printf("relay-server: punch from %s dropped (semaphore full)", peerAddr)
					return
				}
				conn, err := snc.PunchProtected(peerAddr, androidcore.Protect.DialControl)
				if err != nil {
					snc.Log.Printf("relay-server: punch to %s failed: %v", peerAddr, err)
					return
				}
				relay := snc.NewUDPRelayConn(conn, controlURL)
				snc.Log.Printf("relay-server: serving %s â†’ %s", peerAddr, controlURL)
				go func() {
					<-relay.StopCh()
					relay.Close()
				}()
			})
		} else {
			snc.Log.Printf("dht: UDP listen failed: %v â€” DHT disabled", err)
		}
	} else {
		snc.Log.Printf("dht: bad node ID: %v â€” DHT disabled", err)
	}

	// Parse key and authenticate.
	kd, err := snc.ParseKeyString(keyStr)
	if err != nil {
		snc.Log.Printf("snc-core: invalid key: %v", err)
		writeState(dataDir, "key_error")
		os.Exit(1)
	}
	if len(kd.Nodes()) == 0 {
		snc.Log.Printf("snc-core: key contains no server addresses")
		writeState(dataDir, "key_error")
		os.Exit(1)
	}
	// SNC_CC is set by the Kotlin layer before launching this process.
	// It carries the device's physical country code detected via GPS,
	// cell-network registration, or timezone â€” whichever was available.
	// Go cannot call Android location APIs, so Kotlin resolves country first
	// and passes it as an env var â€” the simplest cross-language handoff.
	deviceCC := strings.ToUpper(strings.TrimSpace(os.Getenv("SNC_CC")))
	if len(deviceCC) == 2 {
		snc.Log.Printf("snc-core: device country from Kotlin=%q", deviceCC)
	} else {
		// Fallback: derive from timezone (no permission needed, works offline).
		if tz := snc.TimezoneCC(); tz != "" {
			deviceCC = tz
			snc.Log.Printf("snc-core: device country from timezone=%q", deviceCC)
		}
	}
	androidcore.LogNetworkInterfaces()
	deviceID := loadOrCreateDeviceID(dataDir)

	// Load persisted relay list and client country for bootstrap relay fallback.
	// On first run both are empty; populated after the first successful session.
	cachedRelays, _ := snc.LoadRelayList(filepath.Join(dataDir, "relays.json"))
	bootstrapCC := loadCountry(dataDir)
	dhtRelaysPath := filepath.Join(dataDir, "dht_relays.json")
	// Pre-load DHT relay entries from previous session into the DHT node so they
	// are available immediately for hole-punch bootstrap before any GetRelays response.
	if dhtNode != nil {
		dhtNode.LoadRelays(dhtRelaysPath) //nolint:errcheck
	}

	// Load cached manifest to augment bootstrap candidates with controls discovered
	// in a previous session.  This lets the client reconnect even if the key's
	// static node list is stale (nodes replaced/removed since the key was issued).
	// Also load the addrâ†’country-code map so bootstrap deprioritizes RU/CN controls.
	cachedManifestControls, cachedRegions := snc.ReadManifestCacheControlsAndRegions(
		filepath.Join(dataDir, "manifest.json"), kd.ArbiterPubkey)

	// Write initial control list so Kotlin's UpdateChecker can query update endpoints
	// even before discovery augments the list with freshly found controls.
	writeControls(dataDir, snc.BootstrapControlList(cachedManifestControls, kd.Nodes()))

	// Bootstrap: try each node in order until one responds.
	// If all direct nodes are unreachable, fall back to cached relay peers â€”
	// sorted by country (same country first) so nearby relays are tried first.
	// If all relays also fail, wait and retry indefinitely â€” never exit, so the
	// VPN service stays alive.
	var bootstrapURL string
	var bootstrapAuth *snc.Authenticator
	// isDenial reports whether an auth error is an explicit server rejection
	// that requires user action (expired subscription, revoked key).
	// Positive filter: only specific arbiter response strings count as denial.
	// Network errors, TLS failures, EOF, and unexpected responses are NOT denials â€”
	// they are transient and should be retried silently.
	// This distinction matters because a misconfigured router or a temporarily
	// unreachable arbiter looks like an auth failure from the error type perspective;
	// treating those as "key denied" would show a false "subscription expired" UI.
	isDenial := func(err error) bool {
		if err == nil {
			return false
		}
		s := strings.ToLower(err.Error())
		return strings.Contains(s, "invalid key") ||
			strings.Contains(s, "key expired") ||
			strings.Contains(s, "login failed")
	}

	// writeVPNState writes a one-word state ("ok" or "key_denied") to
	// $dataDir/snc.state so Kotlin can update the notification icon.
	writeVPNState := func(state string) {
		path := filepath.Join(dataDir, "snc.state")
		_ = os.WriteFile(path, []byte(state), 0644)
		snc.Log.Printf("snc-core: vpn-state=%s", state)
	}

	// attachFatalErrorHook wires SetFatalErrorHook on an already-authenticated
	// dialer -- the bootstrap-time isDenial/writeVPNState("key_denied") pair
	// above only covers auth failures during the initial connect/pool-build
	// loop. Once a session is running, a token refresh can still fail and
	// give up permanently (TunnelDialer.refreshToken's fatal path,
	// tunnel_cat/snc/core/tunnel.go) -- until this, nothing on Android ever
	// wired that hook at all, so a live "your key/session was rejected"
	// event went completely unsurfaced: the tunnel just stopped working with
	// no state change and no UI reaction (found while auditing all 5
	// platforms for this, 2026-08-16 -- see the Windows/Mac/Linux commits
	// from the same investigation, which had a *visible but dead-end* UI
	// instead; Android had no visible reaction at all).
	// Reuses the same "key_denied" state string and Kotlin poll pipeline
	// (SNCVpnService.startStateWatch) already wired for the bootstrap case,
	// rather than inventing a second parallel signal.
	attachFatalErrorHook := func(d *snc.TunnelDialer) {
		d.SetFatalErrorHook(func(err error) {
			snc.Log.Printf("snc-core: fatal re-auth failure: %v", err)
			if strings.Contains(err.Error(), "server unavailable") {
				return // transient/arbiter-down exhaustion -- keep retrying silently
			}
			writeVPNState("key_denied")
		})
	}

	// writeUserNotifs drains per-user notifications from a just-authenticated
	// Authenticator and appends new ones to snc.notif for Kotlin to display.
	// Uses the same seen-file dedup as the broadcast notification callback.
	writeUserNotifs := func(a *snc.Authenticator) {
		notifs := a.DrainNotifications()
		if len(notifs) == 0 {
			return
		}
		seenPath := filepath.Join(dataDir, "notif_seen.json")
		now := time.Now().Unix()
		seen := loadNotifSeenFile(seenPath)
		var newMsgs []string
		for _, n := range notifs {
			if seen[n.ID] || now-n.CreatedAt > 24*3600 {
				continue
			}
			newMsgs = append(newMsgs, n.Message)
			seen[n.ID] = true
		}
		if len(newMsgs) == 0 {
			return
		}
		saveNotifSeenFile(seenPath, seen)
		data, _ := json.Marshal(newMsgs)
		notifPath := filepath.Join(dataDir, "snc.notif")
		if err := os.WriteFile(notifPath, data, 0o644); err != nil {
			snc.Log.Printf("snc-core: user-notify: write snc.notif: %v", err)
		} else {
			snc.Log.Printf("snc-core: user-notify: wrote %d message(s) to snc.notif", len(newMsgs))
		}
	}

	type authResult struct {
		url    string
		auth   *snc.Authenticator
		denied bool // true if at least one node explicitly rejected (not timeout)
	}
	// raceAuth launches goroutines for all candidate URLs simultaneously and
	// returns as soon as the first one authenticates successfully.
	// Remaining goroutines finish on their own timeout and are discarded.
	raceAuth := func(candidates []string, label string) authResult {
		ch := make(chan authResult, len(candidates))
		for i, url := range candidates {
			go func(i int, url string) {
				snc.Log.Printf("snc-core: bootstrap %s=%s (node %d/%d)", label, url, i+1, len(candidates))
				androidcore.ProbeTCP(androidcore.ProbeAddr(strings.TrimPrefix(url, "https://")))
				a := snc.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
				a.SetKeyAuth(kd)
				a.SetDeviceInfo(kd.KeyID, deviceID, "Android")
				if err := a.Login(); err != nil {
					snc.Log.Printf("snc-core: bootstrap auth failed %s: %v", url, err)
					ch <- authResult{url: url, auth: nil, denied: isDenial(err)}
				} else {
					ch <- authResult{url: url, auth: a}
				}
			}(i, url)
		}
		hadDenial := false
		for range candidates {
			r := <-ch
			if r.auth != nil {
				return r
			}
			if r.denied {
				hadDenial = true
			}
		}
		return authResult{denied: hadDenial}
	}

	// denialStreak counts consecutive bootstrap iterations where every node
	// returned an explicit server rejection. Key denied is only reported to
	// the UI after 3 consecutive denial iterations â€” this prevents a single
	// transient server-side error from incorrectly blocking the user.
	// In practice a genuine denial (expired sub, revoked key) returns the same
	// string on every retry; 3 attempts is enough to distinguish it from a
	// one-off server hiccup or a DPI-injected reset that mimics an HTTP error.
	denialStreak := 0
	const denialsBeforeReport = 3

	for attempt := 1; bootstrapAuth == nil; attempt++ {
		// The key's embedded ControlNodes is bootstrap-only: once a manifest
		// has ever been cached, it is authoritative and the key's node list
		// must not be raced alongside it (see snc.BootstrapControlList) — a
		// control retired from the live manifest must stop being tried.
		nodes := snc.BootstrapControlList(cachedManifestControls, kd.Nodes())
		urls := make([]string, 0, len(nodes))
		for _, n := range nodes {
			urls = append(urls, "https://"+n)
		}
		// Split controls: prefer non-RU/CN; only try adversarial jurisdictions as last resort.
		// A control node in Russia or China is subject to local law: it can be compelled
		// to log traffic or hand over keys. We only route through such nodes when no
		// other option is available, never as the primary path.
		// On first run cachedRegions is nil and all controls land in preferred â€” correct behaviour.
		var preferred, deprioritized []string
		for _, u := range urls {
			addr := strings.TrimPrefix(u, "https://")
			cc := cachedRegions[addr]
			if cc == "RU" || cc == "CN" {
				deprioritized = append(deprioritized, u)
			} else {
				preferred = append(preferred, u)
			}
		}
		nodeSource := "key (no cached manifest yet)"
		if len(cachedManifestControls) > 0 {
			nodeSource = "cached manifest"
		}
		snc.Log.Printf("snc-core: bootstrap auth user=%s attempt=%d nodes=%d (preferred=%d rucn=%d source=%s)",
			kd.Username, attempt, len(urls), len(preferred), len(deprioritized), nodeSource)
		iterDenied := false
		var r authResult
		if len(preferred) > 0 {
			r = raceAuth(preferred, "server")
		}
		if r.auth == nil && len(deprioritized) > 0 {
			snc.Log.Printf("snc-core: bootstrap: no preferred control responded â€” trying %d RU/CN control(s)", len(deprioritized))
			r = raceAuth(deprioritized, "server-rucn")
		}
		if r.auth != nil {
			denialStreak = 0
			bootstrapURL = r.url
			bootstrapAuth = r.auth
			writeVPNState("ok")
			writeUserNotifs(r.auth)
		} else if r.denied {
			iterDenied = true
		}

		// 2. If all direct nodes unreachable, try cached relays as transparent proxies.
		// Each relay is a raw-TCP passthrough to its own control â€” client does its own
		// TLS + auth through it.  Relays are typically operated by end-users whose
		// home ISPs are not censored; they serve as a diverse set of entry points that
		// are harder to block en masse than a fixed list of datacenter IPs.
		// Same country first: a relay in the user's country usually has lower latency
		// and is less likely to be blocked by the user's own ISP.
		if bootstrapAuth == nil && len(cachedRelays) > 0 {
			sorted := snc.RelaysByCountry(cachedRelays, bootstrapCC)
			relayURLs := make([]string, len(sorted))
			for i, relay := range sorted {
				relayURLs[i] = "https://" + relay.Addr
				snc.Log.Printf("snc-core: bootstrap relay candidate=%s cc=%s", relay.Addr, relay.CountryCode)
			}
			if r := raceAuth(relayURLs, "relay"); r.auth != nil {
				denialStreak = 0
				bootstrapURL = r.url
				bootstrapAuth = r.auth
				writeVPNState("ok")
				writeUserNotifs(r.auth)
				snc.Log.Printf("snc-core: bootstrap via relay OK relay=%s", bootstrapURL)
			} else if r.denied {
				iterDenied = true
			}
		}
		// 3. If still not connected, try UDP relay bootstrap via DHT hole punch.
		// This works when TCP to controls is fully blocked but UDP is not:
		//   Censored client --UDP hole punch--> Relay --TCP--> Control
		// The relay (another SNC user device) transparently forwards our auth
		// request to the control it is already connected to.
		if bootstrapAuth == nil && dhtNode != nil && !iterDenied {
			// Probe our external UDP endpoint â€” works via UDP even when TCP is blocked.
			var ownUDPAddr string
			for _, n := range kd.Nodes() {
				ep, err := snc.ProbeExternalEndpointProtected(n, androidcore.Protect.DialControl)
				if err == nil {
					ownUDPAddr = ep.String()
					break
				}
			}
			if ownUDPAddr != "" {
				relayEntries := dhtNode.Relays()
				snc.Log.Printf("snc-core: bootstrap: trying %d DHT relay(s) via UDP hole punch (own=%s)", len(relayEntries), ownUDPAddr)
			outer:
				for _, entry := range relayEntries {
					for _, ctrlNode := range kd.Nodes() {
						ctrlURL := "https://" + ctrlNode
						dhtNode.SendHolePunch(entry.Addr, ownUDPAddr, ctrlURL)
						time.Sleep(150 * time.Millisecond) // let relay receive and start punching
						conn, err := snc.PunchProtected(entry.Addr, androidcore.Protect.DialControl)
						if err != nil {
							snc.Log.Printf("snc-core: bootstrap udp-relay: punch %s: %v", entry.Addr, err)
							continue
						}
						rc := snc.NewUDPRelayConn(conn, "")
						a := snc.NewAuthenticator(ctrlURL, kd.APIKey, kd.Username, kd.Password)
						a.SetKeyAuth(kd)
						a.SetDeviceInfo(kd.KeyID, deviceID, "Android")
						if err := a.LoginViaUDP(rc); err != nil {
							snc.Log.Printf("snc-core: bootstrap udp-relay %sâ†’%s: %v", entry.Addr, ctrlNode, err)
							rc.Close()
							if errors.Is(err, snc.ErrAuthRejected) {
								iterDenied = true
								break outer
							}
							continue
						}
						bootstrapAuth = a
						bootstrapURL = ctrlURL
						writeVPNState("ok")
						writeUserNotifs(a)
						// Router is not created yet; the DHT merger goroutine will register
						// this relay peer and build paths once the pool is ready.
						snc.Log.Printf("snc-core: bootstrap via UDP relay OK relay=%s ctrl=%s", entry.Addr, ctrlNode)
						break outer
					}
				}
			}
		}

		if bootstrapAuth == nil {
			if iterDenied {
				denialStreak++
				if denialStreak >= denialsBeforeReport {
					writeVPNState("key_denied")
				} else {
					snc.Log.Printf("snc-core: key denial attempt %d/%d â€” retrying before reporting", denialStreak, denialsBeforeReport)
				}
			} else {
				denialStreak = 0
				if isProxyOnly {
					// Proxy-only: no retry loop â€” exit so SncProxyManager can switch strategy.
					snc.Log.Printf("snc-core: proxy-only: all controls unreachable â€” writing no_controls and exiting")
					writeVPNState("no_controls")
					os.Exit(0)
				}
			}
			delay := time.Duration(attempt) * 5 * time.Second
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
			snc.Log.Printf("snc-core: all nodes unreachable, retrying in %s", delay)
			time.Sleep(delay)
		}
	}
	snc.Log.Printf("snc-core: bootstrap auth OK server=%s token=%.8sÃ¢â‚¬Â¦", bootstrapURL, bootstrapAuth.Token())

	// Per-connection home-region bypass: home-country IPs are dialled directly
	// via androidcore.Protect.DialControl instead of through the tunnel.
	// Why bypass: routing domestic traffic (local banks, government sites, media)
	// through a foreign exit would break IP-gated content and add unnecessary
	// latency. The bypass list is a set of CIDRs fetched from the control that
	// correspond to the user's home country â€” anything outside those CIDRs goes
	// through the tunnel as usual.
	//
	// publicIP is the client's external IP, used both for bypass CIDR country
	// detection and for X-Client-IP stamping on tunnel requests (so exit nodes
	// can correctly attribute country in stats instead of seeing the control IP).
	// We detect it via checkip before calling bm.Start() so the first CIDR fetch
	// already sends the correct ?ip= and gets the right country's CIDR list.
	var publicIP string
	var bypass *snc.BypassManager
	if !isProxyOnly {
		// Build the initial bypass URL list — cached manifest if one exists
		// (authoritative, see snc.BootstrapControlList), else the key's node
		// list as a cold-start fallback. SetNodes keeps this list current as
		// the Discoverer receives updated manifests.
		bootstrapNodes := snc.BootstrapControlList(cachedManifestControls, kd.Nodes())
		bypassURLs := make([]string, 0, len(bootstrapNodes))
		for _, n := range bootstrapNodes {
			bypassURLs = append(bypassURLs, "https://"+n)
		}
		bm, berr := snc.NewBypassManager(bypassURLs, kd.ArbiterPubkey, filepath.Join(dataDir, "cidr.json"))
		if berr != nil {
			snc.Log.Printf("snc-core: bypass manager: %v (continuing without bypass)", berr)
		} else {
			bm.SetToken(bootstrapAuth.Token())
			// Detect public IP before Start() so refresh() sends ?ip= and the exit
			// returns the correct home-country CIDRs instead of guessing from source IP.
			if ip, err := androidcore.DetectPublicIP(androidcore.NewProtectedHTTPClient()); err != nil {
				snc.Log.Printf("snc-core: bypass: public IP detect failed (%v) â€” country detection may be wrong", err)
			} else {
				publicIP = ip
				bm.SetMyIP(ip)
				snc.Log.Printf("snc-core: bypass: public IP=%s", ip)
			}
			bm.Start()
			// Disable regional (CIDR/home-TLD) bypass entirely, via the "Disable
			// regional bypass" menu toggle -- SNC_DISABLE_BYPASS=1. LAN addresses
			// are unaffected: they are decided independently of
			// BypassManager.Enabled() at the SOCKS5/UDP call sites
			// (isLANAddress), never routed through ShouldBypass at all, so they
			// stay bypassed even with this on -- "system-necessary" bypass, per
			// the toggle's intent.
			if os.Getenv("SNC_DISABLE_BYPASS") == "1" {
				bm.SetEnabled(false)
				snc.Log.Printf("snc-core: regional bypass disabled by user (SNC_DISABLE_BYPASS=1); LAN exceptions still apply")
			}
			bypass = bm
			// Write cidr_status immediately so the Kotlin UI sees the correct dot color
			// right when isRunning=true fires, without waiting for the periodic goroutine.
			_ = os.WriteFile(filepath.Join(dataDir, "snc.cidr_status"), []byte(bm.CIDRStatus()), 0o644)
		}
	}

	// Normalise control addresses to host:port.
	// ctrlAddrsMu guards ctrlAddrs; updated by the Discoverer when a fresh
	// manifest arrives so all goroutines see a consistent snapshot.
	var ctrlAddrsMu sync.RWMutex

	ctrlAddrs := make([]string, len(kd.Nodes()))
	for i, node := range kd.Nodes() {
		addr := node
		if _, _, err := net.SplitHostPort(node); err != nil {
			addr = node + ":443"
		}
		ctrlAddrs[i] = addr
	}

	// clubCtrlAddrs holds club-dedicated control addresses discovered by the
	// per-slug ClubDiscoverers (see initClubDiscovery below). Additive only,
	// same guarantee as the Windows client: club controls are extra
	// candidates alongside the general-population ctrlAddrs, never a
	// replacement -- a non-member (or the general pool) is unaffected.
	var clubCtrlAddrs []string

	// clientCC holds the current best-known country code for this device.
	// Seeded from persisted storage so regional routing works from the very
	// first connection after a restart, before bypass CIDRs are fetched.
	clientCC := loadCountry(dataDir)
	if clientCC != "" {
		snc.Log.Printf("snc-core: loaded persisted country=%q", clientCC)
	}
	// Device CC (GPS/network/timezone) overrides persisted IP-based value.
	if deviceCC != "" {
		clientCC = deviceCC
		saveCountry(dataDir, deviceCC)
	}

	// Block QUIC (UDP:443): QUIC over a TCP-based SNC tunnel performs poorly due to
	// UDP-in-TCP head-of-line blocking. Enabled automatically for RU/CN, or explicitly
	// by the user via SNC_BLOCK_QUIC=1 (set by the Disable QUIC menu toggle).
	blockQUIC := clientCC == "RU" || clientCC == "CN"
	if os.Getenv("SNC_BLOCK_QUIC") == "1" {
		blockQUIC = true
	}
	if blockQUIC {
		snc.Log.Printf("snc-core: QUIC (UDP:443) blocked for region=%s explicit=%v", clientCC, os.Getenv("SNC_BLOCK_QUIC") == "1")
	}

	// User's manual "disable IPv6" preference (Disable IPv6 menu toggle),
	// snapshotted at connect time same as SNC_BLOCK_QUIC above. The arbiter's
	// own live kill switch (snc.IPv6TunnelDisabled) is checked separately,
	// per-dial, inside appStickyDialer.ipv6Blocked -- this env var only
	// carries the user's own static choice, which already requires a
	// reconnect to take effect (see MainActivity's "reconnect for changes"
	// toast), so a snapshot is fine here.
	disableIPv6 := os.Getenv("SNC_DISABLE_IPV6") == "1"
	if disableIPv6 {
		snc.Log.Printf("snc-core: IPv6 disabled by user preference")
	}

	// classifyControls returns a regions map (addr Ã¢â€ â€™ cc) for all ctrlAddrs
	// whose IP is covered by the current bypass CIDRs.
	classifyControls := func(cc string) map[string]string {
		regions := make(map[string]string)
		if bypass == nil || cc == "" {
			return regions
		}
		ctrlAddrsMu.RLock()
		addrs := ctrlAddrs
		ctrlAddrsMu.RUnlock()
		for _, addr := range addrs {
			host := addr
			if h, _, err := net.SplitHostPort(addr); err == nil {
				host = h
			}
			if ips, err := net.LookupHost(host); err == nil {
				for _, ipStr := range ips {
					if ip := net.ParseIP(ipStr); ip != nil && bypass.ContainsIP(ip) {
						regions[addr] = cc
						break
					}
				}
			}
		}
		return regions
	}

	// Set up router, probe data plane, build paths, pick qualifying controls.
	router = snc.NewRouter()
	router.SetSelfNodeID(nodeID)
	if clientCC != "" {
		router.SetMyCountry(clientCC)
	}
	router.SetControlsWithRegions(ctrlAddrs, classifyControls(clientCC))
	if globalDisc != nil {
		router.SetLoadFactors(globalDisc.LoadFactors())
	}
	var fetchedRelays []snc.RelayEntry
	if relays, err := snc.FetchRelayList(bootstrapURL); err == nil {
		fetchedRelays = relays
		router.UpdateRelays(relays)
		snc.Log.Printf("snc-core: relay list: %d entries", len(relays))
		if err := snc.SaveRelayList(filepath.Join(dataDir, "relays.json"), relays); err != nil {
			snc.Log.Printf("snc-core: save relay list: %v", err)
		}
	} else {
		snc.Log.Printf("snc-core: relay list fetch failed (%v) â€” routing direct", err)
	}
	if dhtNode != nil {
		ctrlAddrs := kd.Nodes()
		seeds := make([]string, 0, len(ctrlAddrs)+len(fetchedRelays))
		for _, addr := range ctrlAddrs {
			seeds = append(seeds, addr)
		}
		for _, r := range fetchedRelays {
			seeds = append(seeds, r.Addr)
		}
		dhtNode.Bootstrap(seeds)
		snc.Log.Printf("dht: bootstrapped with %d control(s) + %d relay(s)", len(ctrlAddrs), len(fetchedRelays))

		go func() {
			for {
				for _, ctrlAddr := range ctrlAddrs {
					ep, err := snc.ProbeExternalEndpointProtected(ctrlAddr, androidcore.Protect.DialControl)
					if err != nil {
						snc.Log.Printf("dht: probe external endpoint via %s: %v", ctrlAddr, err)
						continue
					}
					entry, err := snc.BuildSignedRelayEntry(nodeID, ep.String(), clientCC)
					if err != nil {
						snc.Log.Printf("dht: sign relay entry: %v", err)
						return
					}
					dhtNode.SetOwnEntry(entry)
					snc.Log.Printf("dht: own relay entry set addr=%s cc=%s", ep, clientCC)
					epStr := ep.String()
					dhtNode.SetEntryRefresher(func() (*snc.DHTRelayEntry, error) {
						return snc.BuildSignedRelayEntry(nodeID, epStr, clientCC)
					})
					return
				}
				snc.Log.Printf("dht: all endpoint probes failed, retrying in 30s")
				time.Sleep(30 * time.Second)
			}
		}()
	}
	router.ProbeDataPlane(5 * time.Second)
	router.BuildPaths()

	// dataFailCh is signalled by the data-plane failure hook (below) to trigger
	// an immediate pool refresh without waiting for the periodic timer.
	// Capacity 1: if multiple failures fire before the refresh goroutine wakes,
	// they coalesce into a single refresh rather than queueing N rebuilds.
	dataFailCh := make(chan struct{}, 1)

	// pool is declared as a var so that the data-fail hook closure (defined inside
	// buildPoolDialers) can capture it by reference before pool is assigned.
	var pool *snc.DialerPool

	// exitCodeStallRestart is caught by Kotlin as a signal to reconnect immediately
	// and silently (no error shown to the user).  Used for all soft failures:
	// stalls, unreachable controls, and any other recoverable condition.
	// Exit code 1 (the default on os.Exit(1)) would show a permanent error UI in
	// Kotlin; code 100 triggers a silent VpnService restart instead, giving the
	// next process a clean slate without alarming the user.
	const exitCodeStallRestart = 100

	// bootstrapDialer is the persistent fallback dialer used when all control
	// auth attempts fail. Reusing the same object across pool rebuilds means
	// existing udp-assoc sessions keep their reference without interruption.
	bootstrapDialer := snc.NewTunnelDialer(bootstrapAuth)
	bootstrapDialer.SetRTTUpdateHook(func(surl string, rtt time.Duration) {
		router.UpdateTrafficRTT(surl, rtt)
	})
	attachFatalErrorHook(bootstrapDialer)
	if publicIP != "" {
		bootstrapDialer.SetClientIP(publicIP)
	}

	// Per-address auth failure tracking: backs off nodes that consistently fail auth
	// so a dead out-of-country control does not waste a probe slot on every rebuild.
	authFails := make(map[string]int)
	authSkipUntil := make(map[string]time.Time)

	// Auth to all qualifying controls in parallel, return dialers slice.
	// Falls back to bootstrapAuth when no controls pass the e2e probe.

	buildPoolDialers := func() []*snc.TunnelDialer {
		qualAddrs := router.QualifyingControlAddrs()

		// E2E probe: filter controls by whether their exits are actually reachable.
		// A control that passes TCP/auth but has dead exits carries no useful traffic â€”
		// it would accept connections and then time out on every actual HTTP request.
		// By probing end-to-end before adding a control to the pool, we ensure that
		// only controls with live exit paths receive user traffic.
		// Probes run in parallel; each result feeds back into the router's RTT table
		// so path scoring uses real exit latency rather than the TCP probe RTT.
		type probeResult struct {
			addr string
			rtt  time.Duration
		}
		probeCh := make(chan probeResult, len(qualAddrs))
		for _, addr := range qualAddrs {
			go func(addr string) {
				// When TCP to the control is blocked but UDP is alive (router marks
				// transport=udp), ProbeControlE2E always fails even if exits are
				// healthy â€” it uses a TCP TLS connection.  Fall back to the UDP
				// probe path so UDP-mode controls are not incorrectly excluded.
				var rtt time.Duration
				var ok bool
				if router.ControlTransportName(addr) == "udp" {
					rtt, ok = snc.ProbeControlUDP(addr, 4*time.Second)
				} else {
					rtt, ok = snc.ProbeControlE2E(addr, 4*time.Second)
				}
				if ok {
					probeCh <- probeResult{addr, rtt}
				} else {
					snc.Log.Printf("snc-core: pool: e2e probe failed %s â€” exits unreachable", addr)
					probeCh <- probeResult{} // zero addr = failed
				}
			}(addr)
		}
		var viableAddrs []string
		for range qualAddrs {
			if pr := <-probeCh; pr.addr != "" {
				router.UpdateControlRTT(pr.addr, pr.rtt)
				viableAddrs = append(viableAddrs, pr.addr)
			}
		}
		// Sort by e2e RTT so dialers[0] is the primary (lowest latency).
		sort.Slice(viableAddrs, func(i, j int) bool {
			ri, rj := router.ControlRTT(viableAddrs[i]), router.ControlRTT(viableAddrs[j])
			if ri == 0 {
				return false
			}
			if rj == 0 {
				return true
			}
			return ri < rj
		})
		// Cap at 12: enough headroom to spread load across every qualifying
		// control instead of concentrating it on whichever 2-5 happen to
		// have the best RTT at rebuild time (see snc/core/router.go's
		// matching minPoolControls/maxPoolControls bounds).
		if len(viableAddrs) > 12 {
			viableAddrs = viableAddrs[:12]
		}

		// Relay second-pass: for controls unreachable directly but reachable via a
		// hole-punched relay peer (path.UDPRelay != nil), build a UDPRelayDialer now
		// so they enter the pool alongside the direct-TCP dialers.
		// Cap-at-5 above applies only to direct controls; relay-backed ones are
		// additive â€” they are the only path to blocked controls, so no cap.
		//
		// Auth is skipped for relay-backed controls: bootstrapAuth is reused because
		// tokens are valid cross-control. Re-auth (on token expiry) hits bootstrapAuth's
		// serverURL (a reachable control). The UDPRelayConn.controlURL routes POST
		// traffic to the blocked control â€” auth.serverURL is not consulted in UDP mode.
		relayDialers := make(map[string]*snc.TunnelDialer) // ctrlAddr -> pre-built dialer
		{
			directSet := make(map[string]bool, len(viableAddrs))
			for _, a := range viableAddrs {
				directSet[a] = true
			}
			for _, path := range router.Paths() {
				if path.UDPRelay == nil || directSet[path.ControlAddr] {
					continue
				}
				if _, exists := relayDialers[path.ControlAddr]; exists {
					continue
				}
				rd := snc.NewUDPRelayDialer(path.UDPRelay, bootstrapAuth)
				rd.SetRTTUpdateHook(func(surl string, rtt time.Duration) {
					router.UpdateTrafficRTT(surl, rtt)
				})
				if publicIP != "" {
					rd.SetClientIP(publicIP)
				}
				if clientCC != "" {
					rd.SetClientCC(clientCC)
				}
				relayDialers[path.ControlAddr] = rd
				viableAddrs = append(viableAddrs, path.ControlAddr)
				snc.Log.Printf("snc-core: pool: UDP relay ctrl=%s via peer=%s", path.ControlAddr, path.Relays[0].Addr)
			}
		}

		// Top-up: if in-country viable controls < 5, probe out-of-country controls to fill the gap.
		// Why top-up: the pool needs redundancy so that a single control going down
		// during a session doesn't kill connectivity. With only 1-2 in-country controls
		// available, losing one triggers an immediate rebuild that stalls traffic for
		// several seconds. Out-of-country controls are slightly higher latency but
		// fully functional; they serve as hot standbys rather than primaries.
		// This also handles the stall case (viable=0) without a separate code path.
		if len(viableAddrs) < 5 {
			ctrlAddrsMu.RLock()
			allAddrs := ctrlAddrs
			ctrlAddrsMu.RUnlock()
			qualSet := make(map[string]bool, len(qualAddrs))
			for _, a := range qualAddrs {
				qualSet[a] = true
			}
			var fallbackAddrs []string
			for _, a := range allAddrs {
				if !qualSet[a] {
					fallbackAddrs = append(fallbackAddrs, a)
				}
			}
			if len(fallbackAddrs) > 0 {
				need := 5 - len(viableAddrs)
				// Skip addresses that are in auth backoff (too many consecutive auth failures).
				var activeFallbacks []string
				for _, a := range fallbackAddrs {
					if time.Now().Before(authSkipUntil[a]) {
						snc.Log.Printf("snc-core: pool: skip %s (auth backoff)", a)
					} else {
						activeFallbacks = append(activeFallbacks, a)
					}
				}
				snc.Log.Printf("snc-core: pool: topping up â€” need %d more, probing %d out-of-country candidate(s)", need, len(activeFallbacks))
				var extras []string
				if len(activeFallbacks) > 0 {
					fbCh := make(chan probeResult, len(activeFallbacks))
					for _, addr := range activeFallbacks {
						go func(addr string) {
							var rtt time.Duration
							var ok bool
							if router.ControlTransportName(addr) == "udp" {
								rtt, ok = snc.ProbeControlUDP(addr, 4*time.Second)
							} else {
								rtt, ok = snc.ProbeControlE2E(addr, 4*time.Second)
							}
							if ok {
								fbCh <- probeResult{addr, rtt}
							} else {
								fbCh <- probeResult{}
							}
						}(addr)
					}
					for range activeFallbacks {
						if pr := <-fbCh; pr.addr != "" {
							router.UpdateControlRTT(pr.addr, pr.rtt)
							extras = append(extras, pr.addr)
						}
					}
				}
				sort.Slice(extras, func(i, j int) bool {
					ri, rj := router.ControlRTT(extras[i]), router.ControlRTT(extras[j])
					if ri == 0 {
						return false
					}
					if rj == 0 {
						return true
					}
					return ri < rj
				})
				if len(extras) > need {
					extras = extras[:need]
				}
				viableAddrs = append(viableAddrs, extras...)
				if len(extras) > 0 {
					snc.Log.Printf("snc-core: pool: added %d out-of-country control(s), total viable=%d", len(extras), len(viableAddrs))
				}
			}
		}

		// No viable controls found this cycle.
		if len(viableAddrs) == 0 {
			if pool == nil {
				if isProxyOnly {
					snc.Log.Printf("snc-core: proxy-only: all controls e2e-dead â€” writing no_controls and exiting")
					writeVPNState("no_controls")
					os.Exit(0)
				}
				// Initial build only â€” cannot start a tunnel with zero controls.
				snc.Log.Printf("snc-core: all controls e2e-dead â€” silent restart")
				os.Exit(exitCodeStallRestart)
			}
			if isProxyOnly {
				snc.Log.Printf("snc-core: proxy-only: all controls e2e-dead â€” writing no_controls and exiting")
				writeVPNState("no_controls")
				os.Exit(0)
			}
			// Rebuild: keep the current pool alive; the refresh goroutine will
			// retry on the next cycle (refreshFast = 60 s).
			snc.Log.Printf("snc-core: all controls e2e-dead â€” keeping current pool alive, will retry")
			return nil
		}

		// Switching to direct controls â€” clear stale transport overrides
		// so bootstrapDialer (used as fallback) uses direct TCP.
		bootstrapDialer.Auth().ClearDialFunc()

		type authResult struct {
			d        *snc.TunnelDialer
			addr     string
			authOK   bool
			rejected bool // true = server explicitly denied credentials (ErrAuthRejected)
		}
		ch := make(chan authResult, len(viableAddrs))
		for _, addr := range viableAddrs {
			go func(addr string) {
				// Relay-backed controls: dialer is pre-built with bootstrapAuth.
				// Skip the auth round-trip â€” the blocked control is unreachable
				// directly; tokens are valid cross-control so bootstrapAuth suffices.
				if rd := relayDialers[addr]; rd != nil {
					ctrlURL := "https://" + addr
					rd.SetFirstFailHook(func() {
						snc.Log.Printf("tunnel: first failure on UDP relay path to %s â€” evicting", ctrlURL)
						if pool != nil {
							pool.Evict(rd)
						}
						router.RecordControlFlap(addr)
						select {
						case dataFailCh <- struct{}{}:
						default:
						}
					})
					rd.SetDataFailHook(3, func() {
						snc.Log.Printf("tunnel: data-plane failure on UDP relay path to %s â€” fast refresh", ctrlURL)
						select {
						case dataFailCh <- struct{}{}:
						default:
						}
					})
					// See the direct-dialer SetAuthFailHook below for why this is
					// needed separately from SetDataFailHook/SetFirstFailHook.
					rd.SetAuthFailHook(func() {
						snc.Log.Printf("tunnel: repeated login failures on UDP relay path to %s â€” evicting, switching to standby", ctrlURL)
						if pool != nil {
							pool.Evict(rd)
						}
						router.RecordControlFlap(addr)
						select {
						case dataFailCh <- struct{}{}:
						default:
						}
					})
					ch <- authResult{rd, addr, true, false}
					return
				}

				host := addr
				if _, _, err := net.SplitHostPort(addr); err != nil {
					host = addr + ":443"
				}
				ctrlURL := "https://" + host
				a := snc.NewAuthenticator(ctrlURL, kd.APIKey, kd.Username, kd.Password)
				a.SetDeviceInfo(kd.KeyID, deviceID, "Android")
				a.SetKeyAuth(kd)
				// Reuse the bootstrap session token â€” one session per VPN activation.
				a.AdoptToken(bootstrapAuth.Token())
				snc.Log.Printf("snc-core: pool: auth OK %s (token adopted)", addr)
				d, err := router.NewControlDialer(addr, a)
				if err != nil {
					snc.Log.Printf("snc-core: pool: dialer for %s failed: %v", addr, err)
					ch <- authResult{nil, addr, true, false}
					return
				}
				d.SetRTTUpdateHook(func(surl string, rtt time.Duration) {
					router.UpdateTrafficRTT(surl, rtt)
				})
				if publicIP != "" {
					d.SetClientIP(publicIP)
				}
				if clientCC != "" {
					d.SetClientCC(clientCC)
				}
				// Instant pool eviction on first stream failure: immediately route new
				// connections to a standby dialer so the user sees at most one failed
				// request, not several retries against a dead control.
				// SetDataFailHook triggers a full rebuild after 3 consecutive failures,
				// which re-authenticates all controls and picks the best available set.
				d.SetFirstFailHook(func() {
					snc.Log.Printf("tunnel: first stream failure on %s â€” instant eviction, switching to standby", ctrlURL)
					if pool != nil {
						pool.Evict(d)
					}
					router.RecordControlFlap(addr)
					select {
					case dataFailCh <- struct{}{}:
					default:
					}
				})
				// Evict this dialer and trigger fast refresh when the data plane
				// keeps failing (e.g. relay returns 404, or POST times out) even
				// though auth succeeds â€” mirrors the win-client silent-refresh path.
				d.SetDataFailHook(3, func() {
					snc.Log.Printf("tunnel: data-plane failure on %s â€” fast refresh", ctrlURL)
					select {
					case dataFailCh <- struct{}{}:
					default:
					}
				})
				// Repeated *login* failures (refreshToken's transient-error
				// counter) are a distinct signal from data-plane failures: a
				// dialer whose Authenticator can't log in never gets far
				// enough to hit SetFirstFailHook/SetDataFailHook at all,
				// since those trigger on Dial/stream outcomes, not on
				// refreshToken's own internal retry loop. Without this, a
				// node that looks "alive" to the router's RTT/liveness probe
				// (which uses a different, unauthenticated transport — e.g.
				// TCP blocked but UDP still answering /p/v1/ping, see the
				// 2026-08-11 control incident) but whose auth layer is
				// actually blocked/intercepted just keeps retrying forever
				// against the same fixed node.
				d.SetAuthFailHook(func() {
					snc.Log.Printf("tunnel: repeated login failures on %s â€” evicting, switching to standby", ctrlURL)
					if pool != nil {
						pool.Evict(d)
					}
					router.RecordControlFlap(addr)
					select {
					case dataFailCh <- struct{}{}:
					default:
					}
				})
				ch <- authResult{d, addr, true, false}
			}(addr)
		}
		var dialers []*snc.TunnelDialer
		// networkErrURLs collects ctrlURLs that failed with a network error (not
		// an explicit credential rejection). After processing all results we carry
		// their old dialers forward from the current pool rather than dropping them:
		// a TCP timeout does not invalidate an existing session token.
		networkErrURLs := make(map[string]bool)
		for range viableAddrs {
			r := <-ch
			if r.d != nil {
				dialers = append(dialers, r.d)
			}
			// Update auth backoff state.
			// - authOK (including dialer-creation failure after auth): reset counter.
			// - rejected (server explicitly denied credentials): increment backoff counter.
			// - network error (!authOK && !rejected): don't touch counter â€” transient
			//   unreachability should not accumulate toward a backoff penalty.
			if r.addr != "" {
				if r.authOK {
					authFails[r.addr] = 0
					delete(authSkipUntil, r.addr)
				} else if r.rejected {
					authFails[r.addr]++
					if authFails[r.addr] >= 3 {
						snc.Log.Printf("snc-core: pool: backing off %s for 15s after 3 consecutive auth rejections", r.addr)
						authSkipUntil[r.addr] = time.Now().Add(15 * time.Second)
						authFails[r.addr] = 0
					}
				} else {
					// Network error: carry forward the old dialer below.
					host := r.addr
					if _, _, err := net.SplitHostPort(r.addr); err != nil {
						host = r.addr + ":443"
					}
					networkErrURLs["https://"+host] = true
				}
			}
		}
		// For controls that failed with a transient network error, reuse the
		// existing dialer from the current pool if one is present. The session
		// token is still valid; the server will reject it when it expires and
		// the dialer will be evicted then, triggering a fresh auth cycle.
		if pool != nil {
			for ctrlURL := range networkErrURLs {
				if old := pool.Get(ctrlURL); old != nil {
					dialers = append(dialers, old)
					snc.Log.Printf("snc-core: pool: carried forward %s (network error, session preserved)", ctrlURL)
				}
			}
		}
		if len(dialers) == 0 {
			snc.Log.Printf("snc-core: all qualifying controls unreachable â€” using bootstrap")
			dialers = []*snc.TunnelDialer{bootstrapDialer}
		}
		snc.Log.Printf("snc-core: dialer pool size=%d", len(dialers))
		return dialers
	}

	// initDiscovery starts the manifest Discoverer (once) with the given control URL.
	// Safe to call from any goroutine; subsequent calls are no-ops.
	// After init, globalDisc is non-nil and begins background fetching.
	//
	// Why once: the Discoverer fetches a signed manifest from the arbiter that contains
	// the authoritative list of all control nodes and config metadata.
	// It must be initialised exactly once per process; reinitialising would reset the
	// manifest cache and cause a brief gap where ctrlAddrs is empty.
	initDiscovery := func(srvURL string, kd *snc.KeyData) {
		discOnce.Do(func() {
			d, derr := snc.NewDiscoverer(srvURL, kd.ArbiterPubkey, androidcore.NewProtectedHTTPClient(),
				filepath.Join(dataDir, "manifest.json"),
				func(controls []string) {
					addrs := make([]string, 0, len(controls))
					for _, node := range controls {
						addr := node
						if _, _, err := net.SplitHostPort(node); err != nil {
							addr = node + ":443"
						}
						addrs = append(addrs, addr)
					}
					ctrlAddrsMu.Lock()
					ctrlAddrs = addrs
					ctrlAddrsMu.Unlock()
					snc.Log.Printf("snc-core: discovery: %d control nodes", len(addrs))
					writeControls(dataDir, addrs)
					if globalDisc != nil {
						writeIPv6State(dataDir, globalDisc.IPv6Enabled())
					}
					if bypass != nil {
						newURLs := make([]string, len(addrs))
						for i, a := range addrs {
							newURLs[i] = "https://" + a
						}
						bypass.SetNodes(newURLs)
					}
					if router != nil && pool != nil {
						router.SetControlsWithRegions(addrs, classifyControls(clientCC))
						if globalDisc != nil {
							router.SetLoadFactors(globalDisc.LoadFactors())
						}
						router.BuildPaths()
						if nd := buildPoolDialers(); nd != nil {
							swapPoolGrowOnly(pool, nd)
						}
					}
				})
			if derr != nil {
				snc.Log.Printf("snc-core: discovery init: %v", derr)
				return
			}
			if err := d.LoadCached(); err != nil {
				snc.Log.Printf("snc-core: discovery: no cached manifest: %v", err)
			}
			d.UseAsSNIProvider()
			d.UseAsFingerprintProvider() // pin control-node certs to the signed manifest (2026-08-07 security fix)
			// Wire admin broadcast notifications â€” write snc.notif for Kotlin to display.
			d.SetNotificationCallback(func(notifs []snc.Notification) {
				snc.Log.Printf("snc-core: notify: received %d notification(s)", len(notifs))
				seenPath := filepath.Join(dataDir, "notif_seen.json")
				now := time.Now().Unix()
				seen := loadNotifSeenFile(seenPath)
				var newMsgs []string
				for _, n := range notifs {
					if seen[n.ID] || now-n.CreatedAt > 24*3600 {
						continue
					}
					newMsgs = append(newMsgs, n.Message)
					seen[n.ID] = true
				}
				if len(newMsgs) == 0 {
					snc.Log.Printf("snc-core: notify: nothing new to show")
					return
				}
				saveNotifSeenFile(seenPath, seen)
				data, _ := json.Marshal(newMsgs)
				notifPath := filepath.Join(dataDir, "snc.notif")
				if err := os.WriteFile(notifPath, data, 0o644); err != nil {
					snc.Log.Printf("snc-core: notify: write snc.notif: %v", err)
				} else {
					snc.Log.Printf("snc-core: notify: wrote %d message(s) to snc.notif", len(newMsgs))
				}
			})
			d.Start(10 * time.Minute)
			globalDisc = d
		})
	}

	// wireDHT connects the Discoverer and DHT node for manifest gossip.
	// Must be called after both globalDisc and dhtNode are ready.
	// Why DHT gossip: the manifest is normally fetched from the arbiter over HTTPS.
	// If the arbiter is seized, DDoS'd, or its domain blocked, the manifest becomes
	// unreachable. DHT gossip lets clients share manifests they've already fetched
	// peer-to-peer, so the network continues to operate even when the central server
	// is completely unavailable â€” the last good manifest propagates across the mesh.
	wireDHT := func(dhtNode *snc.DHTNode) {
		if globalDisc == nil || dhtNode == nil {
			return
		}
		globalDisc.SetFetchCallback(func(raw []byte, ts int64) {
			dhtNode.SetManifest(raw, ts)
		})
		dhtNode.SetManifestHandler(func(raw []byte) {
			if err := globalDisc.InjectRaw(raw); err != nil {
				snc.Log.Printf("dht: gossip manifest rejected: %v", err)
			}
		})
	}

	// wireMirror sets up torrent-like content mirroring once dhtNode and kd
	// (for the arbiter pubkey) are both available. Idempotent.
	wireMirror := func(dhtNode *snc.DHTNode, kd *snc.KeyData) {
		if dhtNode == nil || kd == nil {
			return
		}
		mirrorOnce.Do(func() {
			var err error
			mirrorMgr, err = snc.NewMirrorManager(
				kd.ArbiterPubkey,
				filepath.Join(dataDir, "mirror"),
				// Android sockets must go through VpnService.protect() (same
				// requirement as the relay-client punch above) or the punch
				// traffic would loop back through the TUN it's meant to bypass.
				func(peerAddr string) (*net.UDPConn, error) {
					ownEntry := dhtNode.OwnEntry()
					if ownEntry == nil {
						return nil, fmt.Errorf("mirror: own external address not yet known")
					}
					dhtNode.SendMirrorPunch(peerAddr, ownEntry.Addr)
					return snc.PunchProtected(peerAddr, androidcore.Protect.DialControl)
				},
				dhtNode.CompletePeers,
				dhtNode.SetContent,
				dhtNode.SetContentComplete,
				snc.DefaultRelayChunkFetcher(globalDisc),
			)
			if err != nil {
				snc.Log.Printf("mirror: init failed: %v", err)
				return
			}
			mirrorMgr.LoadCached()
			dhtNode.SetContentHandler(mirrorMgr.OnContentManifest)
			dhtNode.SetMirrorPunchHandler(func(peerAddr string) {
				conn, err := snc.PunchProtected(peerAddr, androidcore.Protect.DialControl)
				if err != nil {
					snc.Log.Printf("mirror-server: punch to %s failed: %v", peerAddr, err)
					return
				}
				snc.NewMirrorConn(conn, mirrorMgr.ServeChunk)
				snc.Log.Printf("mirror-server: serving chunk requests from %s", peerAddr)
			})
			snc.Log.Printf("mirror: initialized dataDir=%s", filepath.Join(dataDir, "mirror"))
		})
	}

	// initClubDiscovery creates one ClubDiscoverer per known club slug and
	// starts polling -- Android equivalent of main_windows.go's
	// initClubDiscovery. Needs a live session token (tokenFn), so it can
	// only run after bootstrap auth succeeds, same as Windows. State
	// (theme/badge/canRecommend/isAdmin) is exposed to Kotlin via the
	// "club-status" IPC command instead of a direct GUI call, since Go has
	// no call path into the Kotlin UI on this platform.
	initClubDiscovery := func(srvURL string, kd *snc.KeyData, tokenFn func() string, cs *clubState) {
		applyClubControls := func() {
			ctrlAddrsMu.RLock()
			combined := append([]string{}, ctrlAddrs...)
			combined = append(combined, clubCtrlAddrs...)
			ctrlAddrsMu.RUnlock()
			if router != nil && pool != nil {
				router.SetControlsWithRegions(combined, classifyControls(clientCC))
				if globalDisc != nil {
					router.SetLoadFactors(globalDisc.LoadFactors())
				}
				router.BuildPaths()
				if nd := buildPoolDialers(); nd != nil {
					swapPoolGrowOnly(pool, nd)
				}
			}
		}

		cs.recommendFn = func(username string) error {
			return snc.RecommendCatClubMember(srvURL, tokenFn, username)
		}

		for _, slug := range []string{"cat_club", "elite_cat_club"} {
			slug := slug
			cd, err := snc.NewClubDiscoverer(slug, kd.ArbiterPubkey, tokenFn, func(controls []string) {
				flat := cs.mergeClubControls(slug, controls)
				ctrlAddrsMu.Lock()
				clubCtrlAddrs = flat
				ctrlAddrsMu.Unlock()
				snc.Log.Printf("club-discovery %s: control list updated: %v", slug, controls)
				applyClubControls()
			})
			if err != nil {
				snc.Log.Printf("club-discovery %s: init failed: %v", slug, err)
				continue
			}
			cs.mu.Lock()
			cs.bySlugDisc[slug] = cd
			cs.mu.Unlock()
			cd.SetMembershipCallback(func(ok bool) {
				cs.applyMembership(slug, ok)
			})
			cd.SetServerURL(srvURL)
			cd.Start(10 * time.Minute)
		}

		adminPoller := snc.NewAdminStatusPoller(tokenFn, kd.IsAdmin, cs.setAdmin)
		adminPoller.Start(10 * time.Minute)
	}

	initDiscovery(bootstrapURL, kd)
	// Write manifest_status immediately after initDiscovery so the Kotlin UI sees the
	// correct dot color right away, without waiting for the periodic goroutine.
	if globalDisc != nil {
		_ = os.WriteFile(filepath.Join(dataDir, "snc.manifest_status"), []byte(globalDisc.ManifestStatus()), 0o644)
	}
	pool = snc.NewDialerPool(buildPoolDialers())
	connStatsCollector.IncConnect(!autoReconnect)

	clubSt := newClubState(kd.IsAdmin)
	initClubDiscovery(bootstrapURL, kd, bootstrapAuth.Token, clubSt)

	// Widen manifest-fetch candidates beyond the last-cached (≤12-node)
	// manifest with whatever's in the active dialer pool right now, and fall
	// back to fetching directly from navlink.net (bypassing TUN via
	// VpnService.protect(), like the decoy manager does) when every known
	// control fails outright. See discovery.go's doc comments on both.
	if globalDisc != nil {
		globalDisc.SetExtraURLsProvider(func() []string {
			if pool == nil {
				return nil
			}
			return pool.URLs()
		})
		globalDisc.SetNavlinkFallback(func() string { return "" }, androidcore.Protect.DialControl, snc.DefaultClientTelemetryKey)
	}

	// BananaMeter tunnel-diagnostics probe (see TODO.md "BananaMeter-based
	// tunnel diagnostics"): started once per process; reads pool fresh on
	// every tick, so it survives reconnects and pool swaps on its own.
	bananameterProberOnce.Do(func() {
		if kd != nil {
			prober := snc.NewBananameterProber(snc.DefaultBananameterCreds(), kd.ClientID, deviceID, kd.Username)
			prober.Start(
				func() *snc.TunnelDialer {
					if pool == nil {
						return nil
					}
					return pool.Pick()
				},
				func() bool { return false },
			)
		}
	})

	// Wire DHT gossip after both pool and discoverer are ready.
	wireDHT(dhtNode)
	wireMirror(dhtNode, kd)

	// Decoy traffic: periodic HTTPS GETs to popular CDN endpoints blend the
	// client's TLS fingerprint pattern with normal browser activity.
	// Without decoy, a DPI system would see only tunnel handshakes to a fixed IP â€”
	// a distinctive pattern that a statistical classifier can identify even without
	// payload inspection. Mixing in traffic to Google, Cloudflare, etc. makes the
	// traffic profile indistinguishable from that of a regular browser.
	// Connections are protected via dialControl (bypass TUN), not LocalAddr-bound.
	var decoyMgrStop func()
	if !isProxyOnly {
		dm := snc.NewDecoyManagerWithTransport(snc.NewDecoyTransport("", androidcore.Protect.DialControl))
		pool.SetActivityHook(dm.MarkActivity)
		dm.Start()
		decoyMgrStop = dm.Stop
	}

	// srvURL: used for IPC status and log upload; bootstrapURL is always reachable.
	srvURL := bootstrapURL

	// Bind SOCKS5 server on a random local port.
	// A random port avoids conflicts with other apps and is written to snc.socks
	// for Kotlin to read; Kotlin configures the VpnBuilder to forward all TCP/UDP
	// traffic to this local proxy address.
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		snc.Log.Printf("snc-core: SOCKS5 listen: %v", err)
		os.Exit(1)
	}
	socks5 := snc.NewSOCKS5ServerWithPool("", pool, bypass)
	// Trial (2026-08-13, Android only): dedicated native-UDP dialer for
	// general (non-DNS) UDP ASSOCIATE traffic -- voice/video call media,
	// games, anything not otherwise DNS or bypassed. See the full
	// rationale in socks5.RealtimeUDPDialer's doc comment (snc/core/
	// socks5.go) and dialerFor's use of it (snc/core/udp_assoc.go).
	// Best-effort -- normal pool-based UDP relay (today's behavior) is
	// exactly what happens if this fails, nothing blocks on it.
	if udpConn, uerr := snc.NewUDPControlConn(strings.TrimPrefix(bootstrapURL, "https://")); uerr == nil {
		socks5.RealtimeUDPDialer = snc.NewUDPRelayDialer(udpConn, bootstrapAuth)
		snc.Log.Printf("snc-core: realtime UDP trial dialer ready via %s", bootstrapURL)
	} else {
		snc.Log.Printf("snc-core: realtime UDP trial dialer unavailable (%v) -- falling back to pool", uerr)
	}
	go socks5.Serve(socksLn) //nolint:errcheck
	snc.Log.Printf("snc-core: SOCKS5 on %s", socksLn.Addr())
	// Write SOCKS5 address to a file so the Android app can read the port.
	os.WriteFile(filepath.Join(dataDir, "snc.socks"), []byte(socksLn.Addr().String()), 0600) //nolint:errcheck

	// applyCountry reclassifies controls and rebuilds the dialer pool when the
	// client's country changes (first detection or moved countries).
	// Must be called after pool is created.
	applyCountry := func(cc string) {
		clientCC = cc
		router.SetMyCountry(cc)
		regions := classifyControls(cc)
		ctrlAddrsMu.RLock()
		addrs := ctrlAddrs
		ctrlAddrsMu.RUnlock()
		router.SetControlsWithRegions(addrs, regions)
		if globalDisc != nil {
			router.SetLoadFactors(globalDisc.LoadFactors())
		}
		saveCountry(dataDir, cc)
		snc.Log.Printf("snc-core: country: %q detected â€” %d/%d controls in-country â€” rebuilding paths",
			cc, len(regions), len(addrs))
		router.BuildPaths()
		if nd := buildPoolDialers(); nd != nil {
			swapPoolGrowOnly(pool, nd)
		}
	}

	// Adaptive pool refresh:
	//   - After evictions (dead dialers): full re-probe + rebuild at refreshFast.
	//   - Healthy (no evictions): skip ProbeDataPlane, back off to refreshMax.
	// ProbeDataPlane opens TCP connections to every control/relay node â€” skipping
	// it when the pool is healthy is the main battery saving here.
	const (
		refreshFast      = 60 * time.Second
		refreshBase      = 3 * time.Minute
		refreshMax       = 5 * time.Minute
		refreshScreenOff = 10 * time.Minute
	)
	reconnectCh := make(chan struct{}, 1)
	screenCh := make(chan bool, 1)
	poolRefreshStop := make(chan struct{})
	go func() {
		interval := refreshBase
		screenOff := false
		timer := time.NewTimer(interval)
		defer timer.Stop()

		resetTimer := func(d time.Duration) {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			interval = d
			timer.Reset(interval)
		}

		for {
			select {
			case <-poolRefreshStop:
				return

			case off := <-screenCh:
				screenOff = off
				if off {
					// Stretch the current interval up to the screen-off cap.
					if interval < refreshScreenOff {
						resetTimer(refreshScreenOff)
					}
				} else {
					// Screen on â€” probe immediately so connections are fresh.
					pool.DrainEvictions()
					router.ProbeDataPlane(5 * time.Second)
					router.BuildPaths()
					if nd := buildPoolDialers(); nd != nil {
						swapPoolGrowOnly(pool, nd)
					}
					resetTimer(refreshFast)
				}

			case <-reconnectCh:
				// Network change signalled from Kotlin â€” full probe immediately.
				pool.DrainEvictions()
				router.ProbeDataPlane(5 * time.Second)
				router.BuildPaths()
				if nd := buildPoolDialers(); nd != nil {
					swapPoolGrowOnly(pool, nd)
				}
				resetTimer(refreshFast)

			case <-dataFailCh:
				// Data-plane failure detected by SetDataFailHook â€” dialer already
				// evicted from pool; re-probe and rebuild without waiting for timer.
				pool.DrainEvictions()
				router.ProbeDataPlane(5 * time.Second)
				router.BuildPaths()
				if nd := buildPoolDialers(); nd != nil {
					swapPoolGrowOnly(pool, nd)
				}
				resetTimer(refreshFast)

			case <-timer.C:
				// Country re-check as fallback in case the watcher missed a change.
				if bypass != nil {
					if cc := bypass.Country(); cc != "" && cc != clientCC {
						applyCountry(cc)
						pool.DrainEvictions()
						resetTimer(refreshBase)
						continue
					}
				}
				maxInterval := refreshMax
				if screenOff {
					maxInterval = refreshScreenOff
				}
				// Re-probe when dialers were evicted OR when no qualifying controls
				// are alive after a failed probe.  Without the second condition, a
				// single failed ProbeDataPlane leaves all controls marked not-alive
				// indefinitely, since the timer skips the probe when there are no evictions.
				noAliveControls := len(router.QualifyingControlAddrs()) == 0
				if pool.DrainEvictions() > 0 || noAliveControls {
					// Lost dialers or no live controls â€” full probe to find healthy ones.
					router.ProbeDataPlane(5 * time.Second)
					resetTimer(refreshFast)
				} else {
					// All dialers alive â€” skip probe, only rebuild routing table.
					next := interval * 3 / 2
					if next > maxInterval {
						next = maxInterval
					}
					resetTimer(next)
				}
				router.BuildPaths()
				newDialers := buildPoolDialers()
				if newDialers == nil {
					// All e2e probes failed this cycle â€” keep current pool alive.
					// The refresh goroutine will retry on the next timer tick.
					snc.Log.Printf("snc-core: pool: no viable controls this cycle â€” keeping current pool")
				} else {
					// Guard against a single bad probe collapsing all traffic onto
					// one node: if the pool would shrink to <=1 from a larger set,
					// wait 20 s and re-probe once before accepting the degraded list.
					if len(newDialers) <= 1 && pool.Size() > 1 {
						snc.Log.Printf("snc-core: pool shrink %d->%d, stabilising", pool.Size(), len(newDialers))
						select {
						case <-poolRefreshStop:
							return
						case <-time.After(20 * time.Second):
						}
						router.ProbeDataPlane(5 * time.Second)
						router.BuildPaths()
						newDialers = buildPoolDialers()
						snc.Log.Printf("snc-core: pool after stabilise: %d dialer(s)", len(newDialers))
					}
					if newDialers != nil {
						swapPoolGrowOnly(pool, newDialers)
					}
				}
			}
		}
	}()

	// Country watcher: poll bypass.Country() every 5 s and act immediately
	// when the country becomes known or changes â€” without waiting for the 60 s
	// pool refresh cycle. This ensures regional control selection kicks in
	// within seconds of bypass CIDRs being loaded, even on the first connect.
	//
	// controlsClassified tracks whether controls have been classified with live
	// bypass data. At startup, classifyControls runs before bypass CIDRs load
	// (bypass.Start() is async), so CountryCode is empty even if clientCC was
	// restored from persisted storage. We must re-classify once bypass data
	// arrives, even if the country code itself has not changed.
	// Country watcher: two-phase interval.
	// Phase 1 (5 s): rapid polling until bypass CIDRs load and country is known.
	// Phase 2 (60 s): slow polling after initial classification â€” country rarely
	// changes mid-session, so waking every 5 s is wasteful once we know it.
	controlsClassified := false
	go func() {
		const fastInterval = 5 * time.Second
		const slowInterval = 60 * time.Second
		interval := fastInterval
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-poolRefreshStop:
				return
			case <-timer.C:
				if bypass != nil {
					if cc := bypass.Country(); cc != "" {
						if cc != clientCC || !controlsClassified {
							applyCountry(cc)
							controlsClassified = true
						}
					}
				}
				if controlsClassified {
					interval = slowInterval
				}
				timer.Reset(interval)
			}
		}
	}()

	// DHT merger: every 30 s pull relay entries and initiate hole punches for
	// blocked controls.  Interval matches RelayTTL/4 so entries are refreshed
	// well before they expire (RelayTTL = 2 min).
	if dhtNode != nil {
		// Per-relay-addr exponential backoff: prevents goroutine storms when a
		// relay is unreachable (no UDP path).  Starts at 30 s (one ticker cycle),
		// doubles each failure, caps at 5 min.  State is reset on success.
		type relayPunchState struct {
			failedAt time.Time
			failures int
		}
		var (
			relayPunchMu     sync.Mutex
			relayPunchStates = map[string]*relayPunchState{}
		)
		relayBackoffExpired := func(addr string) bool {
			relayPunchMu.Lock()
			defer relayPunchMu.Unlock()
			ps := relayPunchStates[addr]
			if ps == nil {
				return true
			}
			const maxBackoff = 5 * time.Minute
			backoff := 30 * time.Second
			for i := 0; i < ps.failures; i++ {
				backoff *= 2
				if backoff >= maxBackoff {
					backoff = maxBackoff
					break
				}
			}
			return time.Since(ps.failedAt) >= backoff
		}
		relayPunchFailed := func(addr string) {
			relayPunchMu.Lock()
			defer relayPunchMu.Unlock()
			ps := relayPunchStates[addr]
			if ps == nil {
				ps = &relayPunchState{}
				relayPunchStates[addr] = ps
			}
			ps.failedAt = time.Now()
			if ps.failures < 8 { // cap at 2^8 shifts; maxBackoff clamps the actual wait
				ps.failures++
			}
		}
		relayPunchSucceeded := func(addr string) {
			relayPunchMu.Lock()
			defer relayPunchMu.Unlock()
			delete(relayPunchStates, addr)
		}

		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-poolRefreshStop:
					return
				case <-t.C:
					entries := dhtNode.Relays()
					if len(entries) == 0 {
						continue
					}
					dhtNode.SaveRelays(dhtRelaysPath) //nolint:errcheck
					router.MergeDHTRelays(entries)
					router.ProbeDataPlane(3 * time.Second)
					router.BuildPaths()

					// Relay client: punch to new relays for blocked controls.
					blockedCtrls := router.UnreachableControls()
					if len(blockedCtrls) == 0 {
						continue
					}
					ownEntry := dhtNode.OwnEntry()
					if ownEntry == nil {
						continue
					}
					for _, entry := range entries {
						if router.HasUDPPeer(entry.NodeID) {
							continue
						}
						if entry.NodeID == nodeID || entry.Addr == ownEntry.Addr {
							continue // don't relay through ourselves
						}
						if !relayBackoffExpired(entry.Addr) {
							continue // recently failed; wait for backoff to expire
						}
						for _, ctrl := range blockedCtrls {
							snc.Log.Printf("relay-client: punching %s for ctrl=%s", entry.Addr, ctrl)
							go func(relayAddr, ctrlURL, ownAddr string) {
								dhtNode.SendHolePunch(relayAddr, ownAddr, ctrlURL)
								conn, err := snc.PunchProtected(relayAddr, androidcore.Protect.DialControl)
								if err != nil {
									snc.Log.Printf("relay-client: punch %s failed: %v", relayAddr, err)
									relayPunchFailed(relayAddr)
									return
								}
								rc := snc.NewUDPRelayConn(conn, "")
								router.RegisterUDPPeer(entry.NodeID, rc)
								relayPunchSucceeded(relayAddr)
								snc.Log.Printf("relay-client: relay established %s â†’ %s", relayAddr, ctrlURL)
								router.BuildPaths()
							}(entry.Addr, ctrl, ownEntry.Addr)
							break
						}
					}
				}
			}
		}()
	}

	// Data-plane watchdog: check every 5 s whether data has flowed through the
	// pool recently.  On Android, idle periods with no user traffic are normal
	// (no keep-alive POSTs), so the stale threshold is set to 30 s â€” long
	// enough to survive normal browsing pauses but short enough to catch a
	// genuinely dead tunnel (DPI block, exit failure) well before the periodic
	// refresh timer fires.  Firing too aggressively triggers ProbeDataPlane
	// (5 s, hits all 6 controls) which hammers the radio and can cause mobile
	// data instability on carriers like Verizon.
	//
	// watchdogStale: how long with zero successful POST to ANY control before
	// the whole connection-establishment process is restarted from scratch.
	// Explicit product requirement (2026-08-06): "no access to controls at
	// all" for 10s -> full restart, not a soft re-probe. A soft nudge
	// (dataFailCh into the existing pool/router) was tried first and wasn't
	// enough to self-heal the 2026-08-06 incident, so this exits hard via
	// os.Exit(exitCodeStallRestart), which Kotlin catches as a signal for a
	// clean, silent process restart.
	const watchdogStale = 10 * time.Second
	// kotlinHeartbeatStale is how long the "heartbeat" IPC command (sent every
	// 60s by SNCVpnService's netHeartbeat thread) can go missing before this is
	// logged loudly.  Set well above the 60s send interval to tolerate normal
	// jitter/backgrounding delay.  This does not attempt to fix anything on the
	// Kotlin side (Go has no way to resurrect a Kotlin thread) -- it exists
	// purely so a repeat of the 2026-08-06 incident (that thread going silent
	// for hours with zero trace anywhere) shows up immediately in the one log
	// stream (Go's) that has proven reliable, instead of only as a retroactive
	// gap noticed during a later investigation.
	const kotlinHeartbeatStale = 3 * time.Minute
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		kotlinHeartbeatWasStale := false
		startedAt := time.Now()
		for {
			select {
			case <-poolRefreshStop:
				return
			case <-ticker.C:
				if last := atomic.LoadInt64(&lastKotlinHeartbeatUnix); last != 0 || time.Since(startedAt) > kotlinHeartbeatStale {
					var age time.Duration
					if last != 0 {
						age = time.Since(time.Unix(last, 0))
					} else {
						age = time.Since(startedAt) // never received one at all this process lifetime
					}
					stale := age > kotlinHeartbeatStale
					if stale && !kotlinHeartbeatWasStale {
						snc.Log.Printf("snc-core: WARNING Kotlin heartbeat missing for %s â€” network-change reconnect signal may not fire (see 2026-08-06 incident)", age.Round(time.Second))
					} else if !stale && kotlinHeartbeatWasStale {
						snc.Log.Printf("snc-core: Kotlin heartbeat recovered")
					}
					kotlinHeartbeatWasStale = stale
				}

				// 2026-08-06 incident #2: pool.LastDataTime() only sees dialers
				// currently in pool.slots. A dialer already handling a live relayed
				// TCP session (e.g. a WhatsApp call) keeps posting real traffic even
				// after it's evicted from the pool, or if traffic is flowing through
				// bootstrapDialer (never a pool member at all) -- pool.LastDataTime()
				// goes stale regardless, and a hard restart here was confirmed (same
				// day) to kill an actively-working call mid-session on a false
				// positive: logs show a real "POST ok ... payload=393B" in the same
				// second as "no access to any control", right before the restart.
				// TunnelMonitor is fed unconditionally by every real send/recv
				// anywhere in the process (TCP relay and UDP relay alike), not
				// scoped to a pool snapshot, so it doesn't have this blind spot.
				// The more drastic action (full restart) now requires the more
				// reliable, whole-process signal; the narrower pool signal only
				// gets the soft nudge.
				if snc.TunnelMonitor.IsStuck() {
					snc.Log.Printf("snc-core: watchdog: tunnel one-sided (sent but no real data received) for >%s â€” restarting connection setup from scratch", watchdogStale)
					os.Exit(exitCodeStallRestart)
				}
				if last := pool.LastDataTime(); !last.IsZero() && time.Since(last) > watchdogStale {
					snc.Log.Printf("snc-core: watchdog: pool stale for %s â€” forcing re-probe (soft, not restarting)",
						time.Since(last).Round(time.Second))
					select {
					case dataFailCh <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	// Traffic activity writer: every 1 s write the unix timestamp of the last data
	// transfer to snc.traffic so Kotlin's state watcher can animate the icon.
	trafficPath := filepath.Join(dataDir, "snc.traffic")
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-poolRefreshStop:
				return
			case <-ticker.C:
				last := pool.LastDataTime()
				if !last.IsZero() {
					ts := strconv.FormatInt(last.Unix(), 10)
					_ = os.WriteFile(trafficPath, []byte(ts), 0o644)
				}
			}
		}
	}()

	// Wire TUN Ã¢â€ â€™ gVisor Ã¢â€ â€™ SOCKS5.
	if !isProxyOnly {
		if err := androidcore.StartTUN(tunFD, socksLn.Addr().String(), pool, bypass, dotProxyAddr, blockQUIC, disableIPv6); err != nil {
			snc.Log.Printf("snc-core: TUN start: %v", err)
			os.Exit(1)
		}
		snc.Log.Printf("snc-core: TUN running")
	}

	state := &runState{
		nodeID:             nodeID,
		socksAddr:          socksLn.Addr().String(),
		server:             srvURL,
		reconnectCh:        reconnectCh,
		screenCh:           screenCh,
		socks5:             socks5,
		dataDir:            dataDir,
		connStatsCollector: connStatsCollector,
		club:               clubSt,
	}

	if ipcSock != "" {
		go serveIPC(ipcSock, state)
	}

	// Log upload: ship recent logs straight to the arbiter (navlink.net)
	// over the live tunnel every 5 minutes -- see android-core/log_upload.go
	// for why (removed the control/exit relay hop, which saw plaintext
	// content). Same pickDialer closure as the BananaMeter prober above.
	androidcore.NewLogUploader(nodeID).Start(
		func() *snc.TunnelDialer {
			if pool == nil {
				return nil
			}
			return pool.Pick()
		},
		func() bool { return false },
	)

	// Connection-stats upload: same channel/cadence as log upload above,
	// separate endpoint -- see snc.ConnStatsUploader (called directly here,
	// unlike LogUploader, following the same direct-call convention already
	// used for snc.NewBananameterProber above -- no androidcore wrapper
	// needed, this is a brand new uploader with no historical reason to
	// carry Android-specific state the way the old log ring buffer did).
	snc.NewConnStatsUploader(connStatsCollector, pool, router, nodeID, "android", kd.Username).Start(
		func() *snc.TunnelDialer {
			if pool == nil {
				return nil
			}
			return pool.Pick()
		},
		func() bool { return false },
	)

	// Write snc.cidr_status, snc.manifest_status, and snc.club_status every
	// 5 s so the UI can show coloured dots (none/cached/fresh) for CIDR and
	// manifest health, and pick the club theme/badge/admin-preview state --
	// same poll-a-state-file convention as everything else here, not a
	// request/response IPC round-trip (Kotlin's sendIpc is fire-and-forget
	// only, see SNCVpnService.kt).
	go func() {
		cidrFile := filepath.Join(dataDir, "snc.cidr_status")
		manifestFile := filepath.Join(dataDir, "snc.manifest_status")
		clubFile := filepath.Join(dataDir, "snc.club_status")
		for {
			if bypass != nil {
				os.WriteFile(cidrFile, []byte(bypass.CIDRStatus()), 0o644) //nolint:errcheck
			}
			if globalDisc != nil {
				os.WriteFile(manifestFile, []byte(globalDisc.ManifestStatus()), 0o644) //nolint:errcheck
			}
			if clubSt != nil {
				// The admin theme-preview override is applied entirely
				// client-side in Kotlin now (see ClubStatus.kt) -- it's a
				// cosmetic UI switcher only (see keyenc.go's IsAdmin comment:
				// "never a real permission"), so there is no reason for Go to
				// know about it at all. This file always reports the *real*
				// discovered membership theme, unaffected by any preview.
				if b, err := json.Marshal(clubSt.status()); err == nil {
					os.WriteFile(clubFile, b, 0o644) //nolint:errcheck
				}
			}
			time.Sleep(5 * time.Second)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	snc.Log.Printf("snc-core: shutting down")
	close(protectWatchdogStop)
	close(uidWatchdogStop)
	close(poolRefreshStop)
	if decoyMgrStop != nil {
		decoyMgrStop()
	}
	if !isProxyOnly {
		androidcore.StopTUN()
	}
	socksLn.Close()
}

// runState holds read-only info about the running session.
type runState struct {
	nodeID             string
	socksAddr          string
	server             string
	reconnectCh        chan struct{} // send to trigger an immediate pool refresh (capacity 1)
	screenCh           chan bool     // send true=screen-off, false=screen-on (capacity 1)
	socks5             *snc.SOCKS5Server
	dataDir            string
	connStatsCollector *snc.ConnStatsCollector
	club               *clubState
}

// clubState mirrors the Cat Club / Elite Cat Club UI state that the Windows
// client keeps on AppWindow (ReloadClubTheme/SetAdminAccount/SetCanRecommend)
// -- Android has no direct Go->Kotlin call path, so this is polled by Kotlin
// via the "club-status" IPC command instead. slugTheme/slugBadgeLabel/
// themePriority mirror shortnerdcat/snc/win/cmd/shortnerdcat/main_windows.go's
// initClubDiscovery exactly, so the two clients pick the same theme under
// dual membership (Elite subsumes Cat Club).
type clubState struct {
	mu           sync.Mutex
	bySlug       map[string][]string // slug -> discovered club controls, merged additively into ctrlAddrs
	memberSlugs  map[string]bool
	bySlugDisc   map[string]*snc.ClubDiscoverer
	theme        string // discovered: "", "catclub", "elite"
	badgeText    string
	canRecommend bool
	isAdmin      bool
	previewTheme string // admin-only override; "" = no override
	recommendFn  func(username string) error
}

var (
	slugTheme      = map[string]string{"cat_club": "catclub", "elite_cat_club": "elite"}
	slugBadgeLabel = map[string]string{"cat_club": "Cat Club", "elite_cat_club": "Elite Cat Club"}
	themePriority  = map[string]int{"": 0, "catclub": 1, "elite": 2}
)

func newClubState(isAdmin bool) *clubState {
	return &clubState{
		bySlug:      make(map[string][]string),
		memberSlugs: make(map[string]bool),
		bySlugDisc:  make(map[string]*snc.ClubDiscoverer),
		isAdmin:     isAdmin,
	}
}

// setAdmin updates the live admin flag -- called by an snc.AdminStatusPoller
// (see its doc comment for why isAdmin must never be trusted as a one-shot
// value from the key string alone). effectiveTheme() already ignores
// previewTheme once isAdmin is false, so clearing it here too is just belt
// and braces, not load-bearing.
func (c *clubState) setAdmin(isAdmin bool) {
	c.mu.Lock()
	c.isAdmin = isAdmin
	if !isAdmin {
		c.previewTheme = ""
	}
	c.mu.Unlock()
}

// effectiveTheme returns the theme to actually render: the admin's preview
// override if one is set (admin-only, never applies to non-admins), else the
// account's real discovered membership theme.
func (c *clubState) effectiveTheme() (theme, badge string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isAdmin && c.previewTheme != "" {
		return c.previewTheme, c.badgeText
	}
	return c.theme, c.badgeText
}

func (c *clubState) status() androidcore.ClubStatusResponse {
	theme, badge := c.effectiveTheme()
	c.mu.Lock()
	canRec := c.canRecommend
	isAdmin := c.isAdmin
	c.mu.Unlock()
	return androidcore.ClubStatusResponse{
		Theme:        theme,
		BadgeText:    badge,
		IsAdmin:      isAdmin,
		CanRecommend: canRec,
	}
}

// setPreview sets or clears (theme="") the admin-only preview override.
// Returns an error if the account is not an admin.
func (c *clubState) setPreview(theme string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isAdmin {
		return fmt.Errorf("preview theme is admin-only")
	}
	c.previewTheme = theme
	return nil
}

// mergeClubControls folds one club slug's discovered controls into the flat
// set added to ctrlAddrs, and returns the flat union across every club --
// mirrors main_windows.go's initClubDiscovery merge closure.
func (c *clubState) mergeClubControls(slug string, controls []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bySlug[slug] = controls
	var flat []string
	for _, cs := range c.bySlug {
		flat = append(flat, cs...)
	}
	return flat
}

// applyMembership recomputes theme/badge/canRecommend after a membership
// callback fires for one slug -- mirrors main_windows.go's applyTheme.
func (c *clubState) applyMembership(slug string, isMember bool) {
	c.mu.Lock()
	c.memberSlugs[slug] = isMember

	best, bestSlug := "", ""
	for s := range c.memberSlugs {
		if !c.memberSlugs[s] {
			continue
		}
		if t := slugTheme[s]; themePriority[t] > themePriority[best] {
			best, bestSlug = t, s
		}
	}
	var cd *snc.ClubDiscoverer
	if bestSlug != "" {
		cd = c.bySlugDisc[bestSlug]
	}
	c.theme = best
	c.canRecommend = c.memberSlugs["cat_club"] || c.memberSlugs["elite_cat_club"]
	badgeText := ""
	c.mu.Unlock()

	if cd != nil {
		if grantingClub, num, ok := cd.MembershipInfo(); ok {
			label := slugBadgeLabel[grantingClub]
			if label == "" {
				label = slugBadgeLabel[bestSlug]
			}
			badgeText = fmt.Sprintf("%s Member #%d", label, num)
		}
	}
	c.mu.Lock()
	c.badgeText = badgeText
	c.mu.Unlock()
	snc.Log.Printf("club-discovery: theme=%q badge=%q canRecommend=%v", best, badgeText, c.memberSlugs["cat_club"] || c.memberSlugs["elite_cat_club"])
}

// connectProtectWithRetry tries to connect to the Kotlin protect socket with
// back-off retries. Without protect(), every socket this process opens loops
// back through its own TUN (routing loop) and times out forever â€” so it is
// worth waiting out a transient hiccup on the Kotlin side (e.g. stopVpn()
// tearing down and recreating the LocalServerSocket for a superseded
// generation) rather than giving up after ~2 seconds.
func connectProtectWithRetry(path string) error {
	const maxRetries = 10
	for i := range maxRetries {
		if err := androidcore.Protect.Connect(path); err == nil {
			snc.Log.Printf("snc-core: protect socket connected (attempt %d/%d)", i+1, maxRetries)
			return nil
		}
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
		}
	}
	return fmt.Errorf("could not connect to protect socket %s after %d attempts", path, maxRetries)
}

// serveIPC accepts connections on the Unix socket at ipcSock and dispatches
// commands.
func serveIPC(ipcSock string, st *runState) {
	// 2026-08-07: a real user's logs showed zero IPC commands (reconnect,
	// nettype, screen-on/off, heartbeat) ever reaching this listener across
	// multiple full VPN sessions with confirmed network switches, despite the
	// listener itself binding fine (no "ipc listen" error logged) -- meaning
	// Kotlin's LocalSocket connect() side was failing silently (only visible
	// in Logcat, not in the file logs users send). This socket was the only
	// piece using the Linux filesystem Unix-socket namespace; the *other*
	// Kotlin<->Go channel (protect, android-core/protect_linux.go) uses the
	// abstract namespace and has never shown a comparable failure. Switching
	// to the same abstract namespace here to match the one pattern already
	// proven reliable on real devices, since the exact failure mode couldn't
	// be root-caused without on-device logcat access.
	netAddr := ipcSock
	if strings.HasPrefix(ipcSock, "@") {
		netAddr = "\x00" + ipcSock[1:]
	} else {
		os.Remove(ipcSock) //nolint:errcheck
	}
	ln, err := net.Listen("unix", netAddr)
	if err != nil {
		snc.Log.Printf("snc-core: ipc listen: %v", err)
		return
	}
	defer ln.Close()
	snc.Log.Printf("snc-core: IPC at %s", ipcSock)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn, st)
	}
}

func handleConn(conn net.Conn, st *runState) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck

	var cmd androidcore.Command
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&cmd); err != nil {
		return
	}

	switch cmd.Cmd {
	case "status":
		androidcore.WriteJSON(conn, androidcore.StatusResponse{ //nolint:errcheck
			Running:   androidcore.TUNRunning(),
			SocksAddr: st.socksAddr,
			NodeID:    st.nodeID,
		})
	case "reconnect":
		// Triggered by Kotlin NetworkCallback on network change.
		select {
		case st.reconnectCh <- struct{}{}:
		default:
		}
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "disconnect":
		// Sent by Kotlin's stopVpn() right before it kills this process
		// (best-effort, may race the kill -- see stopVpn's comment in
		// SNCVpnService.kt). manual=true for a genuine user/explicit
		// disconnect; false for teardown-before-reconnect, where a new
		// "connect" follows immediately.
		var dargs struct {
			Manual bool `json:"manual"`
		}
		if len(cmd.Args) > 0 {
			json.Unmarshal(cmd.Args, &dargs) //nolint:errcheck
		}
		st.connStatsCollector.IncDisconnect(dargs.Manual)
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "notify":
		// Kotlin sends diagnostic notifications before taking actions that would
		// otherwise leave no trace in the Go log (e.g. onRevoke() â†’ SIGKILL).
		var args struct {
			Msg string `json:"msg"`
		}
		if len(cmd.Args) > 0 {
			json.Unmarshal(cmd.Args, &args) //nolint:errcheck
		}
		if args.Msg != "" {
			snc.Log.Printf("kotlin: %s", args.Msg)
		}
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "nettype":
		// Kotlin reports current network transport (wifi/mobile/other/none).
		// Logged for diagnostics; no action taken â€” reconnect is sent separately.
		var args struct {
			Type string `json:"type"` // "wifi", "mobile", "other", "none"
		}
		if len(cmd.Args) > 0 {
			json.Unmarshal(cmd.Args, &args) //nolint:errcheck
		}
		if args.Type != "" {
			snc.Log.Printf("network: transport=%s", args.Type)
		}
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "heartbeat":
		// Sent every 60s by SNCVpnService's netHeartbeat thread, independent of
		// its on-disk logging. See lastKotlinHeartbeatUnix doc comment.
		atomic.StoreInt64(&lastKotlinHeartbeatUnix, time.Now().Unix())
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "screen-off":
		select {
		case st.screenCh <- true:
		default:
		}
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "screen-on":
		select {
		case st.screenCh <- false:
		default:
		}
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "trim":
		// Android onTrimMemory sends this when the system is under memory pressure.
		runtime.GC()
		debug.FreeOSMemory()
		snc.Log.Printf("snc-core: trim: GC+FreeOSMemory done")
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "club-status":
		// Polled periodically by Kotlin to drive the theme (colors+images),
		// the permanent membership badge, the admin theme-preview submenu,
		// and the "Recommend new Cat Club members" menu item's visibility.
		if st.club == nil {
			androidcore.WriteJSON(conn, androidcore.ClubStatusResponse{}) //nolint:errcheck
			break
		}
		androidcore.WriteJSON(conn, st.club.status()) //nolint:errcheck
	case "club-preview":
		// Admin-only theme preview override; args.Theme="" clears it.
		var args androidcore.ClubPreviewArgs
		if len(cmd.Args) > 0 {
			json.Unmarshal(cmd.Args, &args) //nolint:errcheck
		}
		if st.club == nil {
			androidcore.WriteJSON(conn, androidcore.Response{OK: false, Error: "club discovery not running"}) //nolint:errcheck
			break
		}
		if err := st.club.setPreview(args.Theme); err != nil {
			androidcore.WriteJSON(conn, androidcore.Response{OK: false, Error: err.Error()}) //nolint:errcheck
			break
		}
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "club-recommend":
		var args androidcore.ClubRecommendArgs
		if len(cmd.Args) > 0 {
			json.Unmarshal(cmd.Args, &args) //nolint:errcheck
		}
		if st.club == nil || st.club.recommendFn == nil || args.Username == "" {
			androidcore.WriteJSON(conn, androidcore.Response{OK: false, Error: "club discovery not running"}) //nolint:errcheck
			break
		}
		if err := st.club.recommendFn(args.Username); err != nil {
			snc.Log.Printf("club-recommend: %s: %v", args.Username, err)
			androidcore.WriteJSON(conn, androidcore.Response{OK: false, Error: err.Error()}) //nolint:errcheck
			break
		}
		snc.Log.Printf("club-recommend: recommended %s for Cat Club", args.Username)
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
	case "stop":
		androidcore.WriteJSON(conn, androidcore.Response{OK: true}) //nolint:errcheck
		conn.Close()
		androidcore.StopTUN()
		os.Exit(0)
	default:
		androidcore.WriteJSON(conn, androidcore.Response{Error: "unknown command: " + cmd.Cmd}) //nolint:errcheck
	}
}

// loadOrCreateDeviceID reads the stable device UUID from dir/device_id, or
// generates and persists a new one for key-binding enforcement (M4+).
func loadOrCreateDeviceID(dir string) string {
	path := filepath.Join(dir, "device_id")
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id
		}
	}
	id := uuid.New().String()
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		snc.Log.Printf("warn: save device_id: %v", err)
	}
	return id
}

func saveCountry(dir, cc string) {
	_ = os.WriteFile(filepath.Join(dir, "country.txt"), []byte(cc), 0o600)
}

// writeState writes a one-word status to snc.state so the Kotlin layer can
// act on it without waiting for the state-watch 2 s poll interval.
// Used for non-retriable exits (key_error) where immediate detection matters.
func writeState(dir, state string) {
	_ = os.WriteFile(filepath.Join(dir, "snc.state"), []byte(state), 0o600)
}

func loadCountry(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "country.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func loadNotifSeenFile(path string) map[string]bool {
	seen := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return seen
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return seen
	}
	for _, id := range ids {
		seen[id] = true
	}
	return seen
}

func saveNotifSeenFile(path string, seen map[string]bool) {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	data, _ := json.Marshal(ids)
	_ = os.WriteFile(path, data, 0o600)
}

// writeControls writes the current list of control node addresses (host:port)
// to $dataDir/snc.controls as newline-separated text.  Kotlin's UpdateChecker
// reads this file to discover where to check for APK updates.
func writeControls(dataDir string, nodes []string) {
	seen := make(map[string]struct{}, len(nodes))
	var unique []string
	for _, n := range nodes {
		if _, ok := seen[n]; !ok {
			seen[n] = struct{}{}
			unique = append(unique, n)
		}
	}
	_ = os.WriteFile(filepath.Join(dataDir, "snc.controls"),
		[]byte(strings.Join(unique, "\n")+"\n"), 0o644)
}

// writeIPv6State writes the arbiter's advisory ipv6_enabled kill switch to
// $dataDir/snc.ipv6 as a single byte: "1" enabled, "0" disabled, file absent
// or empty if the arbiter hasn't said (keep whatever local default applies).
// Kotlin's MainActivity reads this to force "disable IPv6" on and hide the
// manual menu toggle when the fleet-wide answer is false -- see admin_ipv6.go
// on the arbiter for why this exists (2026-08-12: no exit anywhere has IPv6).
func writeIPv6State(dataDir string, enabled *bool) {
	path := filepath.Join(dataDir, "snc.ipv6")
	if enabled == nil {
		_ = os.Remove(path)
		return
	}
	val := "0"
	if *enabled {
		val = "1"
	}
	_ = os.WriteFile(path, []byte(val), 0o644)
}
