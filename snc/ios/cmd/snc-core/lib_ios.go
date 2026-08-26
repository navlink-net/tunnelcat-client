// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build ios

// snc-core iOS library â€” compiled as a static c-archive and linked into the
// SNCTunnel Network Extension target.
//
// Exported C entry points (called from Swift via bridging header):
//
//	SNCStart(key, logDir, dataDir, tunFD, wildcatMode) â†’ int32 (0 = ok, -1 = err)
//	SNCStop()
//	SNCGetStatus() â†’ *char  (JSON; free with SNCFreeString)
//	SNCFreeString(*char)
//	SNCSetWildcat(int32)
//	SNCSetWildcatToken(*char)   â€” set the access token used by WildCat mode
//	SNCReconnect()
//
// The tunnel runs inside the NEPacketTunnelProvider process. iOS automatically
// routes all sockets created by the extension process through the physical
// interface (not the VPN tunnel), so no VpnService.protect() equivalent is
// needed.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"shortnerdcat/snc/shared/keymigrate"
	snc "tunnel_cat/snc/core"
)

// â”€â”€ Global tunnel state â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

const (
	stateIdle       int32 = 0
	stateConnecting int32 = 1
	stateConnected  int32 = 2
	stateError      int32 = 3
)

var (
	gState        atomic.Int32
	gErrorMsg     atomic.Value // string
	gStopCh       chan struct{}
	gMu           sync.Mutex
	gWildcat      atomic.Int32 // 1 = WildCat mode, toggled via the IPC surface
	gWildcatToken atomic.Value // string, pushed via SNCSetWildcatToken
	gReconnectCh  = make(chan struct{}, 1)

	gPool     *snc.DialerPool
	gDecoy    *snc.DecoyManager
	gBypass   *snc.BypassManager
	gSocks5   *snc.SOCKS5Server
	gSocksLn  net.Listener
	gRouter   *snc.Router
	gDiscOnce sync.Once
	gDisc     *snc.Discoverer
	gDHT      *snc.DHTNode

	gMirrorOnce sync.Once
	gMirror     *snc.MirrorManager

	gClubDiscOnce    sync.Once
	gClubDiscMu      sync.Mutex
	gClubDiscoverers map[string]*snc.ClubDiscoverer

	gKey      *snc.KeyData
	gDataDir  string
	gLogDir   string
	gTunFD    int
	gDeviceID string
	gNodeID   string

	// gConnStats lives for the whole extension process (created once, like
	// gDiscOnce/gMirrorOnce above), not per-SNCStart -- its event counters
	// must accumulate across reconnects within this process's lifetime and
	// only get drained by the uploader's own tick (see
	// ConnStatsCollector.Snapshot).
	gConnStatsOnce sync.Once
	gConnStats     *snc.ConnStatsCollector
	gManualConnect atomic.Int32 // set by SNCStart, read by runTunnel when the pool is built
)

// â”€â”€ Exported C entry points â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

//export SNCStart
func SNCStart(cKey, cLogDir, cDataDir *C.char, tunFD C.int, wildcatMode C.int, manual C.int) C.int {
	gMu.Lock()
	defer gMu.Unlock()

	if gState.Load() != stateIdle {
		return -1
	}

	keyStr := C.GoString(cKey)
	logDir := C.GoString(cLogDir)
	dataDir := C.GoString(cDataDir)

	kd, err := snc.ParseKeyString(keyStr)
	if err != nil || len(kd.Nodes()) == 0 {
		return -1
	}

	if err := os.MkdirAll(logDir, 0700); err != nil {
		return -1
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return -1
	}
	if err := snc.InitLogging(logDir); err != nil {
		fmt.Fprintf(os.Stderr, "snc-core-ios: logging: %v\n", err)
	}

	gKey = kd
	gLogDir = logDir
	gDataDir = dataDir
	gTunFD = int(tunFD)
	if wildcatMode != 0 {
		gWildcat.Store(1)
	}

	gStopCh = make(chan struct{})
	gState.Store(stateConnecting)
	gErrorMsg.Store("")
	gConnStatsOnce.Do(func() { gConnStats = snc.NewConnStatsCollector(filepath.Join(dataDir, "connstats.json")) })
	gManualConnect.Store(manual)

	go runTunnel(gStopCh)
	return 0
}

//export SNCStop
func SNCStop(manual C.int) {
	if gConnStats != nil {
		gConnStats.IncDisconnect(manual != 0)
	}
	gMu.Lock()
	ch := gStopCh
	gMu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

//export SNCGetStatus
func SNCGetStatus() *C.char {
	type statusJSON struct {
		State     string `json:"state"`
		Error     string `json:"error,omitempty"`
		BytesSent int64  `json:"bytesSent"`
		BytesRecv int64  `json:"bytesRecv"`
	}
	var s statusJSON
	switch gState.Load() {
	case stateIdle:
		s.State = "idle"
	case stateConnecting:
		s.State = "connecting"
	case stateConnected:
		s.State = "connected"
	case stateError:
		s.State = "error"
		if msg, ok := gErrorMsg.Load().(string); ok {
			s.Error = msg
		}
	}
	// Live uplink/downlink counter (per-process, resets on extension restart --
	// see core.TotalBytes's doc comment). Included unconditionally so the
	// Swift side always has a fresh reading regardless of state; it only
	// displays this while connected.
	s.BytesSent, s.BytesRecv = snc.TotalBytes()
	b, _ := json.Marshal(s)
	return C.CString(string(b)) // caller must free
}

//export SNCFreeString
func SNCFreeString(s *C.char) {
	C.free(unsafe.Pointer(s))
}

//export SNCSetWildcat
func SNCSetWildcat(enabled C.int) {
	if enabled != 0 {
		gWildcat.Store(1)
	} else {
		gWildcat.Store(0)
	}
	select {
	case gReconnectCh <- struct{}{}:
	default:
	}
}

//export SNCSetWildcatToken
func SNCSetWildcatToken(cToken *C.char) {
	if cToken != nil {
		gWildcatToken.Store(C.GoString(cToken))
	}
}

//export SNCReconnect
func SNCReconnect() {
	select {
	case gReconnectCh <- struct{}{}:
	default:
	}
}

func main() {} // required for c-archive build mode

// â”€â”€ Tunnel goroutine â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func runTunnel(stopCh <-chan struct{}) {
	defer func() {
		teardown()
		gState.Store(stateIdle)
		gMu.Lock()
		gStopCh = nil
		gMu.Unlock()
	}()

	kd := gKey
	dataDir := gDataDir

	// Legacy (V1, unsigned) key: its ControlNodes/Servers list is not
	// verifiable (see snc/shared/keymigrate's doc comment), so it must not
	// be dialed as-is. Migrate first; on failure, treat it like any other
	// unusable key -- do NOT fall back to using its own (unverifiable)
	// node list. Written to dataDir/snc.migrated_key so the extension's
	// Swift side (PacketTunnelProvider's status-poll timer) can persist it
	// into the shared UserDefaults key VPNManager reads, the same way
	// SNCVpnService.kt does for Android via snc.migrated_key.
	if kd.IsLegacy() {
		snc.Log.Printf("snc-core-ios: legacy V1 key detected for %s, migrating to V2", kd.Username)
		migCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		newKeyStr, newKD, migErr := keymigrate.Migrate(migCtx, kd)
		cancel()
		if migErr != nil {
			snc.Log.Printf("snc-core-ios: legacy key migration failed: %v", migErr)
			gState.Store(stateError)
			gErrorMsg.Store("could not renew your key — please try again")
			return
		}
		snc.Log.Printf("snc-core-ios: legacy key migrated OK, key_id=%s", newKD.KeyID)
		if err := os.WriteFile(filepath.Join(dataDir, "snc.migrated_key"), []byte(newKeyStr), 0600); err != nil {
			snc.Log.Printf("snc-core-ios: could not write migrated key: %v", err)
		}
		kd = newKD
	}

	deviceID := loadOrCreateDeviceID(dataDir)
	gDeviceID = deviceID

	nodeID, err := snc.LoadOrGenNodeID(dataDir)
	if err != nil {
		snc.Log.Printf("snc-core-ios: node id: %v", err)
		nodeID = "unknown"
	}
	gNodeID = nodeID

	// DHT node: relay mesh and hole-punch support.
	// On iOS, NEPacketTunnelProvider sockets automatically bypass the VPN.
	var dhtNode *snc.DHTNode
	if dhtID, err := snc.ParseDHTID(nodeID); err == nil {
		if udpConn, err := net.ListenUDP("udp", &net.UDPAddr{}); err == nil {
			dhtNode = snc.NewDHTNode(dhtID, udpConn, filepath.Join(dataDir, "peers.json"))
			dhtNode.Bootstrap(nil)
			dhtNode.LoadRelays(filepath.Join(dataDir, "dht_relays.json")) //nolint:errcheck
			dhtNode.Start()
			snc.Log.Printf("snc-core-ios: dht: node started id=%.8s... addr=%s", nodeID, udpConn.LocalAddr())
			// Semaphore limits concurrent handlers so a flood of incoming DHT punch
			// requests cannot spawn unbounded goroutines.
			incomingPunchSem := make(chan struct{}, 5)
			dhtNode.SetHolePunchHandler(func(peerAddr, controlURL string) {
				select {
				case incomingPunchSem <- struct{}{}:
					defer func() { <-incomingPunchSem }()
				default:
					snc.Log.Printf("snc-core-ios: relay-server: punch from %s dropped (semaphore full)", peerAddr)
					return
				}
				conn, err := snc.Punch("", peerAddr)
				if err != nil {
					snc.Log.Printf("snc-core-ios: relay-server: punch %s: %v", peerAddr, err)
					return
				}
				relay := snc.NewUDPRelayConn(conn, controlURL)
				snc.Log.Printf("snc-core-ios: relay-server: serving %s â†’ %s", peerAddr, controlURL)
				go func() { <-relay.StopCh(); relay.Close() }()
			})
			gDHT = dhtNode
		} else {
			snc.Log.Printf("snc-core-ios: dht: UDP listen failed: %v", err)
		}
	} else {
		snc.Log.Printf("snc-core-ios: dht: bad node ID: %v", err)
	}

	// DNS: TCP-only with multiple fallbacks.
	// Yandex DNS (77.88.8.8, 77.88.8.1) is primary â€” works for Russian users
	// on networks where Google/Cloudflare UDP:53 is intercepted.
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			for _, srv := range []string{
				"77.88.8.8:53", "77.88.8.1:53",
				"8.8.8.8:53", "8.8.4.4:53", "1.1.1.1:53", "9.9.9.9:53",
			} {
				if c, err := d.DialContext(ctx, "tcp", srv); err == nil {
					return c, nil
				}
			}
			return nil, fmt.Errorf("all DNS servers unreachable")
		},
	}

	// Country detection: timezone fallback (GPS APIs not available from NE extension).
	clientCC := loadCountry(dataDir)
	if tz := snc.TimezoneCC(); tz != "" && clientCC == "" {
		clientCC = tz
		saveCountry(dataDir, tz)
	}
	snc.Log.Printf("snc-core-ios: country=%q nodeID=%.8s...", clientCC, nodeID)

	// Load cached relays and manifest controls.
	cachedRelays, _ := snc.LoadRelayList(filepath.Join(dataDir, "relays.json"))
	cachedManifestControls, cachedRegions := snc.ReadManifestCacheControlsAndRegions(
		filepath.Join(dataDir, "manifest.json"), kd.ArbiterPubkey)
	dhtRelaysPath := filepath.Join(dataDir, "dht_relays.json")

	// isDenial distinguishes explicit server rejection from network errors.
	isDenial := func(err error) bool {
		if err == nil {
			return false
		}
		s := strings.ToLower(err.Error())
		return strings.Contains(s, "invalid key") ||
			strings.Contains(s, "key expired") ||
			strings.Contains(s, "login failed")
	}

	type authResult struct {
		url    string
		auth   *snc.Authenticator
		denied bool
	}

	raceAuth := func(candidates []string) authResult {
		ch := make(chan authResult, len(candidates))
		for i, url := range candidates {
			go func(i int, url string) {
				snc.Log.Printf("snc-core-ios: bootstrap auth %s (node %d/%d)", url, i+1, len(candidates))
				a := snc.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
				a.SetKeyAuth(kd)
				a.SetDeviceInfo(kd.KeyID, deviceID, "iOS")
				if err := a.Login(); err != nil {
					snc.Log.Printf("snc-core-ios: auth failed %s: %v", url, err)
					ch <- authResult{url: url, denied: isDenial(err)}
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

	// Bootstrap: try key nodes + cached manifest, then cached relays, then DHT.
	var bootstrapURL string
	var bootstrapAuth *snc.Authenticator

	denialStreak := 0
	const denialsBeforeReport = 3

	for attempt := 1; bootstrapAuth == nil; attempt++ {
		select {
		case <-stopCh:
			return
		default:
		}

		// Build candidates: cached manifest if one exists (authoritative once
		// present — see snc.BootstrapControlList), else the key's node list
		// as a cold-start fallback. RU/CN deprioritized below.
		var allURLs []string
		for _, n := range snc.BootstrapControlList(cachedManifestControls, kd.Nodes()) {
			allURLs = append(allURLs, "https://"+n)
		}
		var preferred, deprioritized []string
		for _, u := range allURLs {
			addr := strings.TrimPrefix(u, "https://")
			cc := cachedRegions[addr]
			if cc == "RU" || cc == "CN" {
				deprioritized = append(deprioritized, u)
			} else {
				preferred = append(preferred, u)
			}
		}

		var r authResult
		if len(preferred) > 0 {
			r = raceAuth(preferred)
		}
		if r.auth == nil && len(deprioritized) > 0 {
			r = raceAuth(deprioritized)
		}
		if r.auth == nil && len(cachedRelays) > 0 {
			sorted := snc.RelaysByCountry(cachedRelays, loadCountry(dataDir))
			relayURLs := make([]string, len(sorted))
			for i, relay := range sorted {
				relayURLs[i] = "https://" + relay.Addr
			}
			r = raceAuth(relayURLs)
		}

		// UDP relay bootstrap via DHT hole punch.
		if r.auth == nil && !r.denied && dhtNode != nil {
			var ownUDPAddr string
			for _, n := range kd.Nodes() {
				ep, err := snc.ProbeExternalEndpoint(n, "")
				if err == nil {
					ownUDPAddr = ep.String()
					break
				}
			}
			if ownUDPAddr != "" {
			relayLoop:
				for _, entry := range dhtNode.Relays() {
					for _, ctrlNode := range kd.Nodes() {
						ctrlURL := "https://" + ctrlNode
						dhtNode.SendHolePunch(entry.Addr, ownUDPAddr, ctrlURL)
						time.Sleep(150 * time.Millisecond)
						conn, err := snc.Punch("", entry.Addr)
						if err != nil {
							snc.Log.Printf("snc-core-ios: bootstrap udp-relay: punch %s: %v", entry.Addr, err)
							continue
						}
						rc := snc.NewUDPRelayConn(conn, "")
						a := snc.NewAuthenticator(ctrlURL, kd.APIKey, kd.Username, kd.Password)
						a.SetKeyAuth(kd)
						a.SetDeviceInfo(kd.KeyID, deviceID, "iOS")
						if err := a.LoginViaUDP(rc); err != nil {
							snc.Log.Printf("snc-core-ios: bootstrap udp-relay %sâ†’%s: %v", entry.Addr, ctrlNode, err)
							rc.Close()
							if errors.Is(err, snc.ErrAuthRejected) {
								r.denied = true
								break relayLoop
							}
							continue
						}
						r.url = ctrlURL
						r.auth = a
						snc.Log.Printf("snc-core-ios: bootstrap via UDP relay OK relay=%s ctrl=%s", entry.Addr, ctrlNode)
						break relayLoop
					}
				}
			}
		}

		if r.auth != nil {
			denialStreak = 0
			bootstrapURL = r.url
			bootstrapAuth = r.auth
			writeState(dataDir, "ok")
			writeUserNotifs(dataDir, r.auth)
		} else {
			if r.denied {
				denialStreak++
				if denialStreak >= denialsBeforeReport {
					writeState(dataDir, "key_denied")
				}
			} else {
				denialStreak = 0
			}
			delay := time.Duration(attempt) * 5 * time.Second
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
			snc.Log.Printf("snc-core-ios: auth failed, retry in %s", delay)
			select {
			case <-stopCh:
				return
			case <-time.After(delay):
			}
		}
	}
	snc.Log.Printf("snc-core-ios: auth OK server=%s", bootstrapURL)

	// Detect public IP for bypass country detection.
	var publicIP string
	if ip, err := snc.FetchMyIP(bootstrapURL); err == nil {
		publicIP = ip
		snc.Log.Printf("snc-core-ios: publicIP=%s", ip)
	}

	// Bypass manager.
	bm, err := snc.NewBypassManager(bootstrapURL, kd.ArbiterPubkey,
		filepath.Join(dataDir, "cidr.json"))
	if err == nil {
		bm.SetToken(bootstrapAuth.Token())
		if publicIP != "" {
			bm.SetMyIP(publicIP)
		}
		bm.Start()
		gBypass = bm
	} else {
		snc.Log.Printf("snc-core-ios: bypass manager: %v", err)
	}

	// Normalise control addresses to host:port.
	var ctrlAddrsMu sync.RWMutex
	ctrlAddrs := make([]string, 0, len(kd.Nodes()))
	for _, node := range kd.Nodes() {
		addr := node
		if _, _, err := net.SplitHostPort(node); err != nil {
			addr = node + ":443"
		}
		ctrlAddrs = append(ctrlAddrs, addr)
	}

	// classifyControls returns a regions map (addr â†’ cc) for controls whose IP
	// falls within the bypass CIDRs. Used by the router to prefer in-country controls.
	classifyControls := func(cc string) map[string]string {
		regions := make(map[string]string)
		if gBypass == nil || cc == "" {
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
					if ip := net.ParseIP(ipStr); ip != nil && gBypass.ContainsIP(ip) {
						regions[addr] = cc
						break
					}
				}
			}
		}
		return regions
	}

	// Router setup.
	router := snc.NewRouter()
	router.SetSelfNodeID(nodeID)
	if clientCC != "" {
		router.SetMyCountry(clientCC)
	}
	router.SetControlsWithRegions(ctrlAddrs, classifyControls(clientCC))
	if gDisc != nil {
		router.SetLoadFactors(gDisc.LoadFactors())
	}
	gRouter = router

	// Fetch relay list and seed DHT.
	var fetchedRelays []snc.RelayEntry
	if relays, err := snc.FetchRelayList(bootstrapURL); err == nil {
		fetchedRelays = relays
		router.UpdateRelays(relays)
		snc.Log.Printf("snc-core-ios: relay list: %d entries", len(relays))
		snc.SaveRelayList(filepath.Join(dataDir, "relays.json"), relays) //nolint:errcheck
	} else {
		snc.Log.Printf("snc-core-ios: relay list: %v â€” routing direct", err)
	}

	if dhtNode != nil {
		seeds := make([]string, 0, len(ctrlAddrs)+len(fetchedRelays))
		for _, addr := range ctrlAddrs {
			seeds = append(seeds, addr)
		}
		for _, r := range fetchedRelays {
			seeds = append(seeds, r.Addr)
		}
		dhtNode.Bootstrap(seeds)
		snc.Log.Printf("snc-core-ios: dht: bootstrapped with %d control(s) + %d relay(s)", len(ctrlAddrs), len(fetchedRelays))

		go func() {
			for {
				for _, ctrlAddr := range ctrlAddrs {
					ep, err := snc.ProbeExternalEndpoint(ctrlAddr, "")
					if err != nil {
						snc.Log.Printf("snc-core-ios: dht: probe via %s: %v", ctrlAddr, err)
						continue
					}
					entry, err := snc.BuildSignedRelayEntry(nodeID, ep.String(), clientCC)
					if err != nil {
						snc.Log.Printf("snc-core-ios: dht: sign relay entry: %v", err)
						return
					}
					dhtNode.SetOwnEntry(entry)
					snc.Log.Printf("snc-core-ios: dht: own relay entry set addr=%s cc=%s", ep, clientCC)
					epStr := ep.String()
					dhtNode.SetEntryRefresher(func() (*snc.DHTRelayEntry, error) {
						return snc.BuildSignedRelayEntry(nodeID, epStr, clientCC)
					})
					return
				}
				snc.Log.Printf("snc-core-ios: dht: all probes failed, retrying in 30s")
				time.Sleep(30 * time.Second)
			}
		}()
	}

	// In WildCat mode all direct TCP to control nodes is blocked â€” skip data-plane probe.
	if gWildcat.Load() == 0 {
		router.ProbeDataPlane(5 * time.Second)
	}
	router.BuildPaths()

	dataFailCh := make(chan struct{}, 1)
	var pool *snc.DialerPool

	// Per-address auth failure backoff tracking.
	authFails := make(map[string]int)
	authSkipUntil := make(map[string]time.Time)

	bootstrapDialer := snc.NewTunnelDialer(bootstrapAuth)
	if publicIP != "" {
		bootstrapDialer.SetClientIP(publicIP)
	}
	if clientCC != "" {
		bootstrapDialer.SetClientCC(clientCC)
	}
	attachFatalErrorHook(dataDir, bootstrapDialer)

	// buildPoolDialers authenticates to all qualifying controls and returns a
	// slice of TunnelDialers. Falls back to bootstrapDialer on total failure.
	// Returns nil when all e2e probes fail so the caller can keep the current pool.
	buildPoolDialers := func() []*snc.TunnelDialer {
		bootstrapDialer.Auth().ClearDialFunc()

		qualAddrs := router.QualifyingControlAddrs()

		// E2E probe: keep only controls whose exits are actually reachable.
		type probeResult struct {
			addr string
			rtt  time.Duration
		}
		probeCh := make(chan probeResult, len(qualAddrs))
		for _, addr := range qualAddrs {
			go func(addr string) {
				var rtt time.Duration
				var ok bool
				if router.ControlTransportName(addr) == "udp" {
					rtt, ok = snc.ProbeControlQUIC(addr, 4*time.Second)
				} else {
					rtt, ok = snc.ProbeControlE2E(addr, 4*time.Second)
				}
				if ok {
					probeCh <- probeResult{addr, rtt}
				} else {
					snc.Log.Printf("snc-core-ios: pool: e2e probe failed %s", addr)
					probeCh <- probeResult{}
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
		// Sort by E2E RTT so dialers[0] is the primary (lowest latency).
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
		// Guarantees real TCP/QUIC diversity in the pool instead of a plain
		// RTT-sort cap -- see BalanceByTransport's doc comment for why a
		// pure RTT sort can silently fill the whole pool with one transport
		// and leave no redundancy when it degrades.
		viableAddrs = router.BalanceByTransport(viableAddrs, 5)

		// Relay second-pass: build UDP relay dialers for blocked controls that have
		// a hole-punched peer path. Additive to the direct set (no cap applied).
		relayDialers := make(map[string]*snc.TunnelDialer)
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
				if publicIP != "" {
					rd.SetClientIP(publicIP)
				}
				if clientCC != "" {
					rd.SetClientCC(clientCC)
				}
				relayDialers[path.ControlAddr] = rd
				viableAddrs = append(viableAddrs, path.ControlAddr)
				snc.Log.Printf("snc-core-ios: pool: UDP relay ctrl=%s via peer=%s", path.ControlAddr, path.Relays[0].Addr)
			}
		}

		// Top-up: when in-country viable controls < 5, add out-of-country controls
		// as warm standbys to maintain redundancy.
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
				var activeFallbacks []string
				for _, a := range fallbackAddrs {
					if !time.Now().Before(authSkipUntil[a]) {
						activeFallbacks = append(activeFallbacks, a)
					}
				}
				snc.Log.Printf("snc-core-ios: pool: topping up â€” need %d more, probing %d out-of-country", need, len(activeFallbacks))
				var extras []string
				if len(activeFallbacks) > 0 {
					fbCh := make(chan probeResult, len(activeFallbacks))
					for _, addr := range activeFallbacks {
						go func(addr string) {
							var rtt time.Duration
							var ok bool
							if router.ControlTransportName(addr) == "udp" {
								rtt, ok = snc.ProbeControlQUIC(addr, 4*time.Second)
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
			}
		}

		if len(viableAddrs) == 0 {
			if pool == nil {
				snc.Log.Printf("snc-core-ios: all controls e2e-dead â€” using bootstrap dialer")
				return []*snc.TunnelDialer{bootstrapDialer}
			}
			snc.Log.Printf("snc-core-ios: all controls e2e-dead â€” keeping current pool")
			return nil
		}

		type authRes struct {
			d        *snc.TunnelDialer
			addr     string
			authOK   bool
			rejected bool
		}
		ch := make(chan authRes, len(viableAddrs))
		for _, addr := range viableAddrs {
			go func(addr string) {
				// Relay-backed controls: dialer is pre-built, skip auth round-trip.
				if rd := relayDialers[addr]; rd != nil {
					ctrlURL := "https://" + addr
					rd.SetFirstFailHook(func() {
						snc.Log.Printf("snc-core-ios: first failure on UDP relay to %s â€” evicting", ctrlURL)
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
						select {
						case dataFailCh <- struct{}{}:
						default:
						}
					})
					rd.SetAuthFailHook(func() {
						snc.Log.Printf("snc-core-ios: repeated login failures on UDP relay to %s â€” evicting", ctrlURL)
						if pool != nil {
							pool.Evict(rd)
						}
						router.RecordControlFlap(addr)
						select {
						case dataFailCh <- struct{}{}:
						default:
						}
					})
					ch <- authRes{rd, addr, true, false}
					return
				}

				if time.Now().Before(authSkipUntil[addr]) {
					ch <- authRes{addr: addr}
					return
				}
				host := addr
				if _, _, err := net.SplitHostPort(addr); err != nil {
					host = addr + ":443"
				}
				ctrlURL := "https://" + host
				a := snc.NewAuthenticator(ctrlURL, kd.APIKey, kd.Username, kd.Password)
				a.SetDeviceInfo(kd.KeyID, deviceID, "iOS")
				a.SetKeyAuth(kd)
				// Reuse the bootstrap session token â€” one session per VPN activation.
				a.AdoptToken(bootstrapAuth.Token())
				snc.Log.Printf("snc-core-ios: pool: auth OK %s (token adopted)", addr)
				d, err := router.NewControlDialer(addr, a)
				if err != nil {
					snc.Log.Printf("snc-core-ios: pool: dialer for %s failed: %v", addr, err)
					ch <- authRes{nil, addr, true, false}
					return
				}
				if publicIP != "" {
					d.SetClientIP(publicIP)
				}
				if clientCC != "" {
					d.SetClientCC(clientCC)
				}
				d.SetFirstFailHook(func() {
					snc.Log.Printf("snc-core-ios: first stream failure on %s â€” evicting", ctrlURL)
					if pool != nil {
						pool.Evict(d)
					}
					router.RecordControlFlap(addr)
					select {
					case dataFailCh <- struct{}{}:
					default:
					}
				})
				d.SetDataFailHook(3, func() {
					select {
					case dataFailCh <- struct{}{}:
					default:
					}
				})
				// Repeated *login* failures are a distinct signal from
				// stream/data-plane failures — see the equivalent comment in
				// snc/android/cmd/snc-core/main_linux.go.
				d.SetAuthFailHook(func() {
					snc.Log.Printf("snc-core-ios: repeated login failures on %s â€” evicting", ctrlURL)
					if pool != nil {
						pool.Evict(d)
					}
					router.RecordControlFlap(addr)
					select {
					case dataFailCh <- struct{}{}:
					default:
					}
				})
				ch <- authRes{d, addr, true, false}
			}(addr)
		}

		var dialers []*snc.TunnelDialer
		networkErrURLs := make(map[string]bool)
		for range viableAddrs {
			r := <-ch
			if r.d != nil {
				dialers = append(dialers, r.d)
			}
			if r.addr != "" {
				if r.authOK {
					authFails[r.addr] = 0
					delete(authSkipUntil, r.addr)
				} else if r.rejected {
					authFails[r.addr]++
					if authFails[r.addr] >= 3 {
						snc.Log.Printf("snc-core-ios: pool: backing off %s for 15s", r.addr)
						authSkipUntil[r.addr] = time.Now().Add(15 * time.Second)
						authFails[r.addr] = 0
					}
				} else {
					host := r.addr
					if _, _, err := net.SplitHostPort(r.addr); err != nil {
						host = r.addr + ":443"
					}
					networkErrURLs["https://"+host] = true
				}
			}
		}
		// For controls that failed with a transient network error, carry forward the
		// existing dialer â€” the session token is still valid.
		if pool != nil {
			for ctrlURL := range networkErrURLs {
				if old := pool.Get(ctrlURL); old != nil {
					dialers = append(dialers, old)
					snc.Log.Printf("snc-core-ios: pool: carried forward %s (network error)", ctrlURL)
				}
			}
		}
		if len(dialers) == 0 {
			snc.Log.Printf("snc-core-ios: pool: no dialers built â€” using bootstrap")
			return []*snc.TunnelDialer{bootstrapDialer}
		}
		snc.Log.Printf("snc-core-ios: pool size=%d", len(dialers))
		return dialers
	}

	// Discovery: manifest poller for control list updates.
	gDiscOnce.Do(func() {
		disc, err := snc.NewDiscoverer(bootstrapURL, kd.ArbiterPubkey,
			snc.NewDiscoveryClient(),
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
				snc.Log.Printf("snc-core-ios: discovery: %d control nodes", len(addrs))
				if router != nil && pool != nil {
					router.SetControlsWithRegions(addrs, classifyControls(clientCC))
					router.SetLoadFactors(disc.LoadFactors())
					router.BuildPaths()
					if nd := buildPoolDialers(); nd != nil {
						pool.Swap(nd)
					}
				}
			})
		if err == nil {
			disc.LoadCached() //nolint:errcheck
			disc.UseAsSNIProvider()
			disc.UseAsFingerprintProvider() // pin control-node certs to the signed manifest (2026-08-07 security fix)
			disc.SetNotificationCallback(func(notifs []snc.Notification) {
				writeNotifsSeen(dataDir, notifs)
			})
			disc.Start(10 * time.Minute)
			gDisc = disc

			// DHT manifest gossip: share manifests peer-to-peer so the network
			// continues if the arbiter is DDoS'd or its domain is blocked.
			if dhtNode != nil {
				disc.SetFetchCallback(func(raw []byte, ts int64) {
					dhtNode.SetManifest(raw, ts)
				})
				dhtNode.SetManifestHandler(func(raw []byte) {
					if err := disc.InjectRaw(raw); err != nil {
						snc.Log.Printf("snc-core-ios: dht: gossip manifest rejected: %v", err)
					}
				})

				// Torrent-like content mirroring, same DHT node, independent channel.
				gMirrorOnce.Do(func() {
					var merr error
					gMirror, merr = snc.NewMirrorManager(
						kd.ArbiterPubkey,
						filepath.Join(dataDir, "mirror"),
						func(peerAddr string) (*net.UDPConn, error) {
							ownEntry := dhtNode.OwnEntry()
							if ownEntry == nil {
								return nil, fmt.Errorf("mirror: own external address not yet known")
							}
							dhtNode.SendMirrorPunch(peerAddr, ownEntry.Addr)
							return snc.Punch("", peerAddr)
						},
						dhtNode.CompletePeers,
						dhtNode.SetContent,
						dhtNode.SetContentComplete,
						snc.DefaultRelayChunkFetcher(disc),
					)
					if merr != nil {
						snc.Log.Printf("snc-core-ios: mirror: init failed: %v", merr)
						return
					}
					gMirror.LoadCached()
					dhtNode.SetContentHandler(gMirror.OnContentManifest)
					dhtNode.SetMirrorPunchHandler(func(peerAddr string) {
						conn, err := snc.Punch("", peerAddr)
						if err != nil {
							snc.Log.Printf("snc-core-ios: mirror-server: punch to %s failed: %v", peerAddr, err)
							return
						}
						snc.NewMirrorConn(conn, gMirror.ServeChunk)
						snc.Log.Printf("snc-core-ios: mirror-server: serving chunk requests from %s", peerAddr)
					})
					snc.Log.Printf("snc-core-ios: mirror: initialized dataDir=%s", filepath.Join(dataDir, "mirror"))
				})
			}
		}
	})

	gClubDiscOnce.Do(func() {
		discs := make(map[string]*snc.ClubDiscoverer)
		for _, slug := range []string{"cat_club", "elite_cat_club"} {
			slug := slug
			cd, err := snc.NewClubDiscoverer(slug, kd.ArbiterPubkey, bootstrapAuth.Token, nil)
			if err != nil {
				snc.Log.Printf("snc-core-ios: club-discovery %s: init failed: %v", slug, err)
				continue
			}
			cd.SetServerURL(bootstrapURL)
			cd.SetMembershipCallback(func(ok bool) {
				snc.Log.Printf("snc-core-ios: club-discovery %s: membership=%v", slug, ok)
				if router != nil && pool != nil {
					if nd := buildPoolDialers(); nd != nil {
						pool.Swap(nd)
					}
				}
			})
			cd.Start(10 * time.Minute)
			discs[slug] = cd
		}
		gClubDiscMu.Lock()
		gClubDiscoverers = discs
		gClubDiscMu.Unlock()
	})

	pool = snc.NewDialerPool(buildPoolDialers())
	gPool = pool
	snc.Log.Printf("snc-core-ios: dialer pool ready size=%d", pool.Size())

	// Widen manifest-fetch candidates beyond the last-cached (≤12-node)
	// manifest with whatever's in the active dialer pool right now, and fall
	// back to fetching directly from navlink.net when every known control
	// fails outright. No TUN-bypass needed on iOS -- the extension process's
	// own sockets already route via the physical NIC automatically (see
	// this file's other doc comments on that property). See discovery.go's
	// doc comments on both.
	if gDisc != nil {
		gDisc.SetExtraURLsProvider(func() []string {
			if gPool == nil {
				return nil
			}
			return gPool.URLs()
		})
		gDisc.SetNavlinkFallback(nil, nil, snc.DefaultClientTelemetryKey)
	}
	if gConnStats != nil {
		gConnStats.IncConnect(gManualConnect.Load() != 0)
		// WildCat mode is decided once, at connect time (SNCStart's
		// wildcatMode param / SNCSetWildcat before connect) -- not something
		// that flips mid-session, so it's safe to read here to start the
		// session-duration clock.
		if gWildcat.Load() != 0 {
			gConnStats.StartWildcatSession()
		}
	}

	// SOCKS5 listener.
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		gState.Store(stateError)
		gErrorMsg.Store(fmt.Sprintf("SOCKS5 listen: %v", err))
		return
	}
	gSocksLn = socksLn
	snc.Log.Printf("snc-core-ios: SOCKS5 at %s", socksLn.Addr())

	bypassForSocks5 := gBypass
	if gWildcat.Load() != 0 {
		bypassForSocks5 = nil
	}
	socks5 := snc.NewSOCKS5ServerWithPool("", pool, bypassForSocks5)
	// DNS goes through the tunnel in WildCat mode too (2026-08-12: flipped
	// from the old "bypass to avoid relay latency" default -- see the same
	// change in the other platform clients for the full reasoning). No
	// WildcatDNS assignment needed here any more; false is the zero value.
	gSocks5 = socks5
	go socks5.Serve(socksLn) //nolint:errcheck

	// TUN.
	if err := startTUN(gTunFD, socksLn.Addr().String()); err != nil {
		socksLn.Close()
		gState.Store(stateError)
		gErrorMsg.Store(fmt.Sprintf("TUN: %v", err))
		return
	}
	snc.Log.Printf("snc-core-ios: TUN started fd=%d", gTunFD)

	// Decoy traffic.
	decoyMgr := snc.NewDecoyManager("")
	if gWildcat.Load() == 0 {
		pool.SetActivityHook(decoyMgr.MarkActivity)
		decoyMgr.Start()
	}
	gDecoy = decoyMgr

	// Log upload: ship recent logs straight to the arbiter (navlink.net)
	// over the live tunnel every 5 minutes -- see log_upload.go for why
	// (removed the control/exit relay hop, which saw plaintext content).
	logUploader := snc.NewLogUploader(nodeID, "ios")
	logUploader.Start(
		func() *snc.TunnelDialer {
			if gPool == nil {
				return nil
			}
			return gPool.Pick()
		},
		func() bool {
			return gWildcat.Load() != 0
		},
	)
	defer logUploader.Stop()

	// Connection-stats upload: same channel/cadence as log upload above,
	// separate endpoint -- see snc.ConnStatsUploader.
	connStatsUploader := snc.NewConnStatsUploader(gConnStats, pool, router, nodeID, "ios", kd.Username)
	connStatsUploader.Start(
		func() *snc.TunnelDialer {
			if gPool == nil {
				return nil
			}
			return gPool.Pick()
		},
		func() bool {
			return gWildcat.Load() != 0
		},
	)
	defer connStatsUploader.Stop()

	// Updater.
	updater := snc.NewUpdater(func() []string { return ctrlAddrs })
	updater.OnReady = func(v string) {
		writeState(dataDir, "update:"+v)
	}
	updater.Start()

	gState.Store(stateConnected)
	writeState(dataDir, "ok")
	snc.Log.Printf("snc-core-ios: connected")

	// applyCountry reclassifies controls and rebuilds the pool when country changes.
	applyCountry := func(cc string) {
		clientCC = cc
		router.SetMyCountry(cc)
		regions := classifyControls(cc)
		ctrlAddrsMu.RLock()
		addrs := ctrlAddrs
		ctrlAddrsMu.RUnlock()
		router.SetControlsWithRegions(addrs, regions)
		if gDisc != nil {
			router.SetLoadFactors(gDisc.LoadFactors())
		}
		saveCountry(dataDir, cc)
		snc.Log.Printf("snc-core-ios: country: %q detected â€” rebuilding paths", cc)
		router.BuildPaths()
		if nd := buildPoolDialers(); nd != nil {
			pool.Swap(nd)
		}
	}

	// WildCat stall watchdog: if the covert-relay session dies and pool.manage()
	// cannot recover within its backoff window, signal the NE to restart the
	// tunnel with a clean slate. 24 ticks Ã— 5s = 2 minutes of silence triggers stop.
	const wildcatStallTicks = 24
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		deadTicks := 0
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
			if gWildcat.Load() == 0 {
				deadTicks = 0
				continue
			}
			if deadTicks >= wildcatStallTicks {
				snc.Log.Printf("snc-core-ios: WildCat watchdog: no live TURN sessions for %s â€” stopping tunnel",
					time.Duration(deadTicks)*5*time.Second)
				gState.Store(stateError)
				gErrorMsg.Store("WildCat stall â€” restarting")
				SNCStop()
				return
			}
		}
	}()

	// Data-plane watchdog (non-WildCat mode; WildCat has its own stall watchdog
	// above). 2026-08-06: found and fixed on Android after a real incident where
	// the tunnel reported HTTP 200 to every POST but carried near-zero real
	// payload for minutes with nothing to detect or recover from it; the same
	// gap existed here (only the WildCat branch had a watchdog).
	//
	// Same day, second incident: the first version of this fix used
	// pool.LastDataTime() (only sees dialers currently in pool.slots) for the
	// hard-restart trigger, and it false-positived mid-call on Android -- logs
	// showed a real "POST ok ... payload=393B" the same second as "no access to
	// any control", right before the restart killed a working session. The
	// dialer actually carrying that traffic had been evicted from the pool (or
	// was bootstrapDialer, never a pool member at all), so pool.LastDataTime()
	// went stale while data was still flowing fine elsewhere in the process.
	// TunnelMonitor is fed unconditionally by every real send/recv anywhere in
	// the process (payload-aware -- ignores zero-length relay envelopes too),
	// not scoped to a pool snapshot, so the more drastic action (full restart)
	// now requires that whole-process signal; the narrower pool signal only
	// gets the soft nudge.
	const watchdogStale = 10 * time.Second
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
			if gWildcat.Load() != 0 {
				continue
			}
			if snc.TunnelMonitor.IsStuck() {
				snc.Log.Printf("snc-core-ios: watchdog: tunnel one-sided (sent but no real data received) â€” restarting tunnel from scratch")
				gState.Store(stateError)
				gErrorMsg.Store("no access to controls â€” restarting")
				SNCStop()
				return
			}
			if last := pool.LastDataTime(); !last.IsZero() && time.Since(last) > watchdogStale {
				snc.Log.Printf("snc-core-ios: watchdog: pool stale for %s â€” forcing re-probe (soft, not restarting)",
					time.Since(last).Round(time.Second))
				select {
				case dataFailCh <- struct{}{}:
				default:
				}
			}
		}
	}()

	// Adaptive pool refresh: fast after evictions, slow when healthy.
	const (
		refreshFast = 60 * time.Second
		refreshBase = 3 * time.Minute
		refreshMax  = 5 * time.Minute
	)
	poolRefreshStop := make(chan struct{})
	go func() {
		interval := refreshBase
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

			case <-gReconnectCh:
				snc.Log.Printf("snc-core-ios: reconnect triggered")
				pool.DrainEvictions()
				if gWildcat.Load() == 0 {
					router.ProbeDataPlane(5 * time.Second)
				}
				router.BuildPaths()
				if nd := buildPoolDialers(); nd != nil {
					pool.Swap(nd)
				}
				// DNS always goes through the tunnel now, WildCat or not — see
				// the comment where socks5 is constructed above.
				resetTimer(refreshFast)

			case <-dataFailCh:
				pool.DrainEvictions()
				if gWildcat.Load() == 0 {
					router.ProbeDataPlane(5 * time.Second)
				}
				router.BuildPaths()
				if nd := buildPoolDialers(); nd != nil {
					pool.Swap(nd)
				}
				resetTimer(refreshFast)

			case <-timer.C:
				// Country re-check.
				if gBypass != nil {
					if cc := gBypass.Country(); cc != "" && cc != clientCC {
						applyCountry(cc)
						pool.DrainEvictions()
						resetTimer(refreshBase)
						continue
					}
				}
				isWildcat := gWildcat.Load() != 0
				noAliveControls := !isWildcat && len(router.QualifyingControlAddrs()) == 0
				if pool.DrainEvictions() > 0 || noAliveControls {
					if !isWildcat {
						router.ProbeDataPlane(5 * time.Second)
					}
					resetTimer(refreshFast)
				} else {
					next := interval * 3 / 2
					if next > refreshMax {
						next = refreshMax
					}
					resetTimer(next)
				}
				router.BuildPaths()
				newDialers := buildPoolDialers()
				if newDialers == nil {
					snc.Log.Printf("snc-core-ios: pool: no viable controls this cycle â€” keeping current pool")
				} else {
					// Guard against single bad probe collapsing pool to 1 dialer.
					if !isWildcat && len(newDialers) <= 1 && pool.Size() > 1 {
						snc.Log.Printf("snc-core-ios: pool shrink %dâ†’%d, stabilising", pool.Size(), len(newDialers))
						select {
						case <-poolRefreshStop:
							return
						case <-time.After(20 * time.Second):
						}
						router.ProbeDataPlane(5 * time.Second)
						router.BuildPaths()
						newDialers = buildPoolDialers()
					}
					if newDialers != nil {
						pool.Swap(newDialers)
					}
				}
			}
		}
	}()

	// Country watcher: two-phase interval.
	// Phase 1 (5 s): rapid polling until bypass CIDRs load and country is known.
	// Phase 2 (60 s): slow polling once initial classification is done.
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
				if gBypass != nil {
					if cc := gBypass.Country(); cc != "" {
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

	// DHT merger: pull relay entries every 30 s and punch to blocked controls.
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
			if ps.failures < 8 {
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
					if gWildcat.Load() == 0 {
						router.ProbeDataPlane(3 * time.Second)
						router.BuildPaths()
					}
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
							continue
						}
						if !relayBackoffExpired(entry.Addr) {
							continue // recently failed; wait for backoff to expire
						}
						for _, ctrl := range blockedCtrls {
							snc.Log.Printf("snc-core-ios: relay-client: punching %s for ctrl=%s", entry.Addr, ctrl)
							go func(relayAddr, ctrlURL, ownAddr string) {
								dhtNode.SendHolePunch(relayAddr, ownAddr, ctrlURL)
								conn, err := snc.Punch("", relayAddr)
								if err != nil {
									snc.Log.Printf("snc-core-ios: relay-client: punch %s failed: %v", relayAddr, err)
									relayPunchFailed(relayAddr)
									return
								}
								rc := snc.NewUDPRelayConn(conn, "")
								router.RegisterUDPPeer(entry.NodeID, rc)
								relayPunchSucceeded(relayAddr)
								snc.Log.Printf("snc-core-ios: relay-client: relay established %s â†’ %s", relayAddr, ctrlURL)
								router.BuildPaths()
							}(entry.Addr, ctrl, ownEntry.Addr)
							break
						}
					}
				}
			}
		}()
	}

	// Main loop: wait for stop signal.
	select {
	case <-stopCh:
	}
	close(poolRefreshStop)
}

// â”€â”€ Teardown â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func teardown() {
	snc.Log.Printf("snc-core-ios: teardown")
	if gDecoy != nil {
		gDecoy.Stop()
		gDecoy = nil
	}
	if gSocks5 != nil {
		gSocks5.Close() //nolint:errcheck
		gSocks5 = nil
	}
	if gSocksLn != nil {
		gSocksLn.Close()
		gSocksLn = nil
	}
	stopTUN()
	if gBypass != nil {
		gBypass.Stop()
		gBypass = nil
	}
	gPool = nil
	gRouter = nil
	if gDHT != nil {
		gDHT.Stop()
		gDHT = nil
	}
	snc.Log.Printf("snc-core-ios: teardown complete")
}
