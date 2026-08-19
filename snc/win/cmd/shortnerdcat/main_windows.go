// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
	"shortnerdcat/snc/shared/navlinkauth"
	snwin "shortnerdcat/snc/win/windows"
	"tunnel_cat/dht"
	"tunnel_cat/snc/core"
)

// bananameterProberOnce ensures the BananaMeter tunnel-diagnostics probe
// (see TODO.md "BananaMeter-based tunnel diagnostics") is started exactly
// once per process, even though the connect flow that starts it can run
// multiple times over the app's life (reconnects).
var bananameterProberOnce sync.Once

// fwHiddenCmd wraps exec.Command with CREATE_NO_WINDOW so that netsh
// advfirewall processes do not flash a console window in GUI mode.
func fwHiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	return cmd
}

func init() {
	// Wire the OTA signal so ApplyPendingUpdate notifies the watchdog before
	// calling os.Exit.  Set in init() so it is in place before main() runs.
	core.UpdateSignalFunc = snwin.SignalUpdateRestart
}

func main() {
	// Ã¢"â‚¬Ã¢"â‚¬ --watchdog mode: run as the watchdog process Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	if len(os.Args) > 1 && os.Args[1] == "--watchdog" {
		runAsWatchdog()
		return
	}

	// --restarted: watchdog restart after a crash  -  skip splash screen.
	watchdogRestart := len(os.Args) > 1 && os.Args[1] == "--restarted"

	// --controls addr1,addr2,...  â€” debug flag to restrict which control nodes are used.
	// Overrides discovery, key nodes, and pool selection. Useful for isolating a single
	// control when testing transport behavior.
	var forcedControls []string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--controls" && i+1 < len(os.Args) {
			for _, addr := range strings.Split(os.Args[i+1], ",") {
				if addr = strings.TrimSpace(addr); addr != "" {
					forcedControls = append(forcedControls, addr)
				}
			}
			i++
		}
	}

	// Ã¢"â‚¬Ã¢"â‚¬ 0a. Elevate to Administrator if needed Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	if !isAdmin() {
		relaunchAsAdmin(os.Args[1:]...)
		return
	}

	// Ã¢"â‚¬Ã¢"â‚¬ 0b. Apply pending update (before mutex so the new process is not blocked).
	core.ApplyPendingUpdate()

	// Ã¢"â‚¬Ã¢"â‚¬ 1. Logging  -  initialised early so early-exit paths are visible in logs Ã¢"â‚¬
	if err := core.InitLogging(`C:\.shortnerdcat\logs`); err != nil {
		fmt.Fprintf(os.Stderr, "warn: logging init: %v\n", err)
	}
	// Redirect stderr to a crash file so Go runtime fatal errors (concurrent map
	// writes, stack overflows, unrecovered panics in other goroutines) are captured.
	// For a GUI app the default stderr handle is INVALID_HANDLE_VALUE â€” fatal errors
	// vanish without a trace otherwise.
	if f, err := os.OpenFile(filepath.Join(`C:\.shortnerdcat\logs`, "crash.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
		if err2 := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd())); err2 == nil {
			os.Stderr = f
		}
	}
	if len(forcedControls) > 0 {
		core.Log.Printf("DEBUG: --controls override active: %v", forcedControls)
	}

	// Ã¢"â‚¬Ã¢"â‚¬ 0c. Single-instance guard Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	// CreateMutexW returns ERROR_ALREADY_EXISTS if another instance holds the
	// mutex.  We keep the handle open for the lifetime of the process so the
	// OS releases it automatically on exit.
	if !ensureSingleInstance() {
		core.Log.Printf("another instance is running and could not be terminated â€” exiting")
		return
	}

	// Clean up leftover artifacts from a previous update attempt.
	core.UpdateCleanup()

	// Remove any DoH config left by a session that ended without clean disconnect.
	// Must run before auto-connect so DNS is not broken during the connect sequence.
	snwin.CleanupDoH()

	// Register navlink:// URL scheme so the OS can route activation links to this exe.
	snwin.RegisterURLScheme()

	// Ã¢"â‚¬Ã¢"â‚¬ 1b. Watchdog: persist our PID and start the watchdog if not running Ã¢"â‚¬Ã¢"â‚¬
	// Write PID so the watchdog can attach to us if it starts after we do.
	if err := core.WriteWatchdogState(core.WatchdogState{MainPID: os.Getpid()}); err != nil {
		core.Log.Printf("warn: write watchdog state: %v", err)
	}
	// Start the watchdog process if it is not already running.  The watchdog
	// advertises its presence via a named mutex; no-op if already up.
	var watchdogProc *os.Process
	if !snwin.WatchdogRunning() {
		if wp, err := snwin.StartWatchdog(); err != nil {
			core.Log.Printf("warn: start watchdog: %v", err)
		} else {
			watchdogProc = wp
			core.Log.Printf("watchdog: started pid=%d", wp.Pid)
		}
	} else {
		core.Log.Printf("watchdog: already running")
	}
	// Monitor the watchdog in the background; restart it if it dies.
	// stopWatchdogMonitor is closed when main is quitting to stop restarts.
	stopWatchdogMonitor := make(chan struct{})
	go func() {
		for {
			if watchdogProc != nil {
				watchdogDone := make(chan struct{})
				go func(p *os.Process) {
					p.Wait() //nolint:errcheck
					close(watchdogDone)
				}(watchdogProc)
				select {
				case <-stopWatchdogMonitor:
					return
				case <-watchdogDone:
					core.Log.Printf("watchdog: process exited  -  restarting")
				}
			}
			// Brief pause before (re)checking / restarting.
			select {
			case <-stopWatchdogMonitor:
				return
			case <-time.After(3 * time.Second):
			}
			if snwin.WatchdogRunning() {
				// Another instance already running; just wait and re-check.
				select {
				case <-stopWatchdogMonitor:
					return
				case <-time.After(30 * time.Second):
				}
				watchdogProc = nil
				continue
			}
			wp, err := snwin.StartWatchdog()
			if err != nil {
				core.Log.Printf("warn: restart watchdog: %v", err)
				watchdogProc = nil
			} else {
				watchdogProc = wp
				core.Log.Printf("watchdog: restarted pid=%d", wp.Pid)
			}
		}
	}()

	// Liveness heartbeat: update LastAlive every 60 s so the watchdog can
	// detect a frozen-but-alive main process and restart it.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			core.TouchAlive()
		}
	}()

	// Catch any panic and write it to the log before the process dies.
	defer func() {
		if r := recover(); r != nil {
			core.Log.Printf("PANIC: %v\n%s", r, debug.Stack())
		}
	}()

	// Ã¢"â‚¬Ã¢"â‚¬ 2. Splash screen (after UAC, after logging) Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	// appWindow is declared here so the window can be shown immediately after
	// the splash, before the auth sequence completes.
	var appWindow *snwin.AppWindow

	if !watchdogRestart {
		core.Log.Println("showing splash")
		snwin.ShowSplash(core.Version, 5*time.Second)
		core.Log.Println("splash done")
	}

	// Start the app window immediately after the splash so it appears while
	// auth runs in the background.  Callbacks referencing trayApp are wired
	// below after trayApp is created; all callback fields are nil-guarded.
	appWindow = snwin.NewAppWindow()
	appWindow.Start()

	// Ã¢"â‚¬Ã¢"â‚¬ 3. Try auto-login with saved key Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	core.Log.Println("loading key")

	// appDataDir is declared here (before auto-auth) so the relay cache is
	// accessible during the startup auth loop as well as in onConnect/onLogin.
	appDataDir := filepath.Join(os.Getenv("APPDATA"), "ShortNerdCat")

	// discoveredControls holds the control-node list refreshed by the discoverer.
	// It is updated from a background goroutine; protect with discoveredMu.
	var (
		discoveredMu          sync.RWMutex
		discoveredControls    []string
		discoveredRegions     map[string]string  // addr  ->  ISO region code (advisory)
		discoveredLoadFactors map[string]float64 // addr -> bonus/malus coefficient (advisory, see Router.SetLoadFactors)
		lastKnownCountry      string             // client's own region; set after first connect
		gpsCountry            string             // country from GPS/locale; kept even when explicit region is active
		gpsDetected           bool               // set when GPS/locale gave a country; CIDR detection is then skipped
		dhtNode               *core.DHTNode      // declared here so startDiscovery/onLogin closures can capture it

		globalDisc *core.Discoverer // single Discoverer instance, created once at app startup
		discOnce   sync.Once

		// Club discoverers -- parallel to globalDisc, one per known club slug
		// (see tunnel_cat/docs/club-membership.md). Their extra control
		// addresses are merged into discoveredClubControls and appended
		// alongside discoveredControls in pickServerURL below -- additive,
		// never replacing the general-population control list. Unlike
		// globalDisc, these need a live session token (X-Session), so they
		// can only start once a dialer exists post-login, not at app startup.
		clubDiscoverers        []*core.ClubDiscoverer
		clubDiscOnce           sync.Once
		discoveredClubControls []string

		// adminPoller re-checks admin status live (see AdminStatusPoller's
		// doc comment) instead of trusting savedKey.IsAdmin -- a static
		// field baked into the key string at issuance time -- forever.
		adminPoller *core.AdminStatusPoller

		mirrorMgr  *core.MirrorManager // torrent-like content mirroring; created once dhtNode+kd are both ready
		mirrorOnce sync.Once
	)

	// pickServerURL returns the best server URL to connect to.
	// When --controls is active, always returns the first forced control.
	// Otherwise prefers in-region controls, then falls back to key nodes.
	pickServerURL := func(kd *core.KeyData) string {
		if len(forcedControls) > 0 {
			core.Log.Printf("pickServerURL: forced control %s", forcedControls[0])
			return ensureHTTPS(forcedControls[0])
		}

		discoveredMu.RLock()
		dc := append([]string{}, discoveredControls...)
		dc = append(dc, discoveredClubControls...) // additive: club-dedicated controls are extra candidates, never a replacement for the general list
		regions := discoveredRegions
		country := lastKnownCountry
		discoveredMu.RUnlock()

		nodes := dc
		if len(nodes) == 0 && kd != nil {
			nodes = kd.Nodes()
		}
		if len(nodes) == 0 {
			return ""
		}

		// Prefer an in-region control when we know the client's region.
		if country != "" && len(regions) > 0 {
			for _, n := range nodes {
				if regions[n] == country {
					core.Log.Printf("pickServerURL: in-region control selected addr=%s cc=%s", n, country)
					return ensureHTTPS(n)
				}
			}
			core.Log.Printf("pickServerURL: no in-region control found (cc=%s)  -  using %s", country, nodes[0])
		}
		return ensureHTTPS(nodes[0])
	}

	// initDiscovery creates the Discoverer once at app startup and begins background
	// manifest refresh. Safe to call multiple times â€” runs only on the first call.
	// dhtNode may be nil here; call wireDHT separately once the DHT node is ready.
	initDiscovery := func(srvURL string, kd *core.KeyData) {
		discOnce.Do(func() {
			cacheFile := manifestCacheFile()
			var err error
			globalDisc, err = core.NewDiscoverer(srvURL, kd.ArbiterPubkey, core.NewDiscoveryClient(), cacheFile, func(controls []string) {
				discoveredMu.Lock()
				discoveredControls = controls
				if globalDisc != nil {
					discoveredRegions = globalDisc.Regions()
					discoveredLoadFactors = globalDisc.LoadFactors()
				}
				discoveredMu.Unlock()
				core.Log.Printf("discovery: control list updated: %v", controls)
			})
			if err != nil {
				core.Log.Printf("discovery: init failed: %v", err)
				return
			}
			if err := globalDisc.LoadCached(); err != nil {
				core.Log.Printf("discovery: no cached manifest: %v", err)
			}
			globalDisc.UseAsSNIProvider()
			globalDisc.UseAsFingerprintProvider() // pin control-node certs to the signed manifest (2026-08-07 security fix)
			// Show admin broadcast notifications  -  deduplicated by ID, 24 h TTL.
			globalDisc.SetNotificationCallback(func(notifs []core.Notification) {
				core.Log.Printf("notify: received %d notification(s) from manifest", len(notifs))
				now := time.Now().Unix()
				seen := loadNotifSeen()
				var newMsgs []string
				for _, n := range notifs {
					core.Log.Printf("notify: id=%s created_at=%d seen=%v msg=%q", n.ID, n.CreatedAt, seen[n.ID], n.Message)
					if seen[n.ID] {
						continue
					}
					if now-n.CreatedAt > 24*3600 {
						core.Log.Printf("notify: id=%s expired (age=%ds), skipping", n.ID, now-n.CreatedAt)
						continue
					}
					newMsgs = append(newMsgs, n.Message)
					seen[n.ID] = true
				}
				if len(newMsgs) == 0 {
					core.Log.Printf("notify: nothing new to show")
					return
				}
				core.Log.Printf("notify: showing %d new message(s)", len(newMsgs))
				saveNotifSeen(seen)
				go snwin.ShowNotification(newMsgs)
			})
			globalDisc.Start(10 * time.Minute)
		})
	}

	// initClubDiscovery creates one ClubDiscoverer per known club slug and
	// starts polling. Unlike initDiscovery, this needs a live session token
	// (tokenFn), so it can only run after login -- call it once a dialer
	// exists (see dialer.Token() call sites), not at app startup. Safe to
	// call multiple times; runs only on the first call.
	initClubDiscovery := func(srvURL string, kd *core.KeyData, tokenFn func() string) {
		clubDiscOnce.Do(func() {
			// slugTheme maps a club slug to its illustration theme name (see
			// AppWindow.ClubTheme). Elite Cat Club wins if the user is
			// confirmed a member of both -- it subsumes Cat Club everywhere
			// else in this feature, so the UI should reflect that too.
			slugTheme := map[string]string{"cat_club": "catclub", "elite_cat_club": "elite"}
			slugBadgeLabel := map[string]string{"cat_club": "Cat Club", "elite_cat_club": "Elite Cat Club"}
			themePriority := map[string]int{"": 0, "catclub": 1, "elite": 2}

			var mu sync.Mutex
			bySlug := make(map[string][]string)
			memberSlugs := make(map[string]bool)
			bySlugDisc := make(map[string]*core.ClubDiscoverer)

			merge := func(slug string, controls []string) {
				mu.Lock()
				bySlug[slug] = controls
				var flat []string
				for _, cs := range bySlug {
					flat = append(flat, cs...)
				}
				mu.Unlock()
				discoveredMu.Lock()
				discoveredClubControls = flat
				discoveredMu.Unlock()
				core.Log.Printf("club-discovery %s: control list updated: %v", slug, controls)
			}
			applyTheme := func() {
				mu.Lock()
				best := ""
				bestSlug := ""
				for slug := range memberSlugs {
					if t := slugTheme[slug]; themePriority[t] > themePriority[best] {
						best, bestSlug = t, slug
					}
				}
				var cd *core.ClubDiscoverer
				if bestSlug != "" {
					cd = bySlugDisc[bestSlug]
				}
				mu.Unlock()

				badgeText := ""
				if cd != nil {
					if grantingClub, num, ok := cd.MembershipInfo(); ok {
						label := slugBadgeLabel[grantingClub]
						if label == "" {
							label = slugBadgeLabel[bestSlug] // fall back to the discoverer's own slug if the arbiter didn't echo membership_club
						}
						badgeText = fmt.Sprintf("%s Member #%d", label, num)
					}
				}
				if appWindow != nil {
					appWindow.ReloadClubTheme(best, badgeText)
					// Cat Club (or subsuming) access is exactly what gates
					// recommending -- any confirmed membership is enough,
					// regardless of which one ended up "best" for theming.
					mu.Lock()
					canRecommend := memberSlugs["cat_club"] || memberSlugs["elite_cat_club"]
					mu.Unlock()
					appWindow.SetCanRecommend(canRecommend)
				}
			}

			if appWindow != nil {
				appWindow.RecommendFn = func(username string) {
					if err := core.RecommendCatClubMember(srvURL, tokenFn, username); err != nil {
						core.Log.Printf("club-recommend: %s: %v", username, err)
					} else {
						core.Log.Printf("club-recommend: recommended %s for Cat Club", username)
					}
				}
			}

			for _, slug := range []string{"cat_club", "elite_cat_club"} {
				slug := slug
				cd, err := core.NewClubDiscoverer(slug, kd.ArbiterPubkey, tokenFn, func(controls []string) {
					merge(slug, controls)
				})
				if err != nil {
					core.Log.Printf("club-discovery %s: init failed: %v", slug, err)
					continue
				}
				mu.Lock()
				bySlugDisc[slug] = cd
				mu.Unlock()
				cd.SetMembershipCallback(func(ok bool) {
					mu.Lock()
					memberSlugs[slug] = ok
					mu.Unlock()
					applyTheme()
				})
				cd.SetServerURL(srvURL)
				cd.Start(10 * time.Minute)
				clubDiscoverers = append(clubDiscoverers, cd)
			}

			adminPoller = core.NewAdminStatusPoller(tokenFn, kd.IsAdmin, func(isAdmin bool) {
				if appWindow != nil {
					appWindow.SetAdminAccount(isAdmin)
				}
			})
			adminPoller.Start(10 * time.Minute)
		})
	}

	// wireDHT connects the global Discoverer to a DHT node for manifest gossip.
	// Must be called after initDiscovery; safe to call multiple times.
	wireDHT := func(dhtNode *core.DHTNode) {
		if globalDisc == nil || dhtNode == nil {
			return
		}
		globalDisc.SetFetchCallback(func(raw []byte, ts int64) {
			dhtNode.SetManifest(raw, ts)
		})
		dhtNode.SetManifestHandler(func(raw []byte) {
			if err := globalDisc.InjectRaw(raw); err != nil {
				core.Log.Printf("discovery: DHT gossip manifest rejected: %v", err)
			}
		})
	}

	// wireMirror sets up torrent-like content mirroring once dhtNode and kd
	// (for the arbiter pubkey) are both available. Idempotent, same contract
	// as wireDHT â€” safe to call every time startDiscovery runs.
	wireMirror := func(dhtNode *core.DHTNode, kd *core.KeyData) {
		if dhtNode == nil || kd == nil {
			return
		}
		mirrorOnce.Do(func() {
			var err error
			mirrorMgr, err = core.NewMirrorManager(
				kd.ArbiterPubkey,
				filepath.Join(appDataDir, "mirror"),
				// Fetch-side punch: invite the peer via DHT, then punch back â€”
				// same two-step handshake the existing relay-client role uses
				// (core.Punch("", relayAddr) after SendHolePunch), just on the
				// parallel mirror-punch channel so it never collides with
				// tunnel-relay's own hole-punch traffic.
				func(peerAddr string) (*net.UDPConn, error) {
					ownEntry := dhtNode.OwnEntry()
					if ownEntry == nil {
						return nil, fmt.Errorf("mirror: own external address not yet known")
					}
					dhtNode.SendMirrorPunch(peerAddr, ownEntry.Addr)
					return core.Punch("", peerAddr)
				},
				dhtNode.CompletePeers,
				dhtNode.SetContent,
				dhtNode.SetContentComplete,
				core.DefaultRelayChunkFetcher(globalDisc),
			)
			if err != nil {
				core.Log.Printf("mirror: init failed: %v", err)
				return
			}
			mirrorMgr.LoadCached()
			dhtNode.SetContentHandler(mirrorMgr.OnContentManifest)
			// Serve side: someone else punched to us wanting a chunk we have.
			dhtNode.SetMirrorPunchHandler(func(peerAddr string) {
				conn, err := core.Punch("", peerAddr)
				if err != nil {
					core.Log.Printf("mirror-server: punch to %s failed: %v", peerAddr, err)
					return
				}
				core.NewMirrorConn(conn, mirrorMgr.ServeChunk)
				core.Log.Printf("mirror-server: serving chunk requests from %s", peerAddr)
			})
			core.Log.Printf("mirror: initialized dataDir=%s", filepath.Join(appDataDir, "mirror"))
		})
	}

	// startDiscovery is kept for call-site compatibility; it now delegates to
	// initDiscovery (idempotent) + wireDHT + wireMirror.
	startDiscovery := func(srvURL string, kd *core.KeyData, dhtNode *core.DHTNode) {
		initDiscovery(srvURL, kd)
		wireDHT(dhtNode)
		wireMirror(dhtNode, kd)
	}

	// deviceID is loaded below (after appDataDir), but declared here so the
	// auto-auth block and onConnect closures can all capture the same variable.
	var deviceID string

	// If launched via navlink://activate?key=... URL scheme, save the key to
	// disk so the LoadKey call below picks it up immediately.
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "navlink://") {
			continue
		}
		if idx := strings.Index(arg, "key="); idx >= 0 {
			keyStr := arg[idx+4:]
			if amp := strings.IndexByte(keyStr, '&'); amp >= 0 {
				keyStr = keyStr[:amp]
			}
			if keyStr != "" {
				if saveErr := snwin.SaveKey(keyStr); saveErr == nil {
					core.Log.Printf("deep-link: key saved from navlink:// URL")
				} else {
					core.Log.Printf("deep-link: could not save key: %v", saveErr)
				}
			}
		}
		break
	}

	var (
		dialer    *core.TunnelDialer
		serverURL string
		savedKey  *core.KeyData // non-nil when a valid key is on disk
	)
	if keyStr, err := snwin.LoadKey(); err == nil {
		if kd, err := core.ParseKeyString(keyStr); err == nil && len(kd.Nodes()) > 0 {
			savedKey = kd
			// A key's embedded ControlNodes is bootstrap-only: once a manifest
			// has ever been cached, it is authoritative and the key's node
			// list must not be raced again (see core.BootstrapControlList).
			cachedManifestControls := core.ReadManifestCacheControls(manifestCacheFile(), kd.ArbiterPubkey)
			bootstrapSeed := kd.Nodes()[0]
			if len(cachedManifestControls) > 0 {
				bootstrapSeed = cachedManifestControls[0]
			}
			// Start background manifest fetch immediately so the control list
			// is ready before the tunnel connects, even if auth never succeeds.
			go initDiscovery(ensureHTTPS(bootstrapSeed), kd)
			// deviceID not yet loaded here; SetDeviceInfo is called again in onConnect.
			// The initial auto-login proceeds without binding info  -  the arbiter allows
			// it on first activation.
			// loginURL authenticates to one control URL and returns the authenticator
			// on success, nil on failure. No side effects â€” safe to call concurrently.
			loginURL := func(url string) *core.Authenticator {
				core.Log.Printf("auto-auth user %s @ %s", kd.Username, url)
				a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
				a.SetKeyAuth(kd)
				if err := a.Login(); err != nil {
					core.Log.Printf("auto-auth failed %s: %v", url, err)
					return nil
				}
				core.Log.Printf("auto-auth OK %s token=%s...", url, a.Token()[:8])
				return a
			}
			// applyAuth wires a successful authenticator into the global dialer state.
			// Must be called from the main goroutine (not concurrently).
			applyAuth := func(a *core.Authenticator, url string) {
				if userNotifs := a.DrainNotifications(); len(userNotifs) > 0 {
					go showUserNotifications(userNotifs)
				}
				dialer = core.NewTunnelDialer(a)
				serverURL = url
				go startDiscovery(url, kd, dhtNode)
			}

			authOK := false
			authNodes := core.BootstrapControlList(cachedManifestControls, kd.Nodes())
			if len(forcedControls) > 0 {
				authNodes = forcedControls
			}

			// Race all direct controls in parallel: first successful auth wins.
			// Remaining goroutines finish in the background and their results are discarded.
			type authResult struct {
				a   *core.Authenticator
				url string
			}
			resultCh := make(chan authResult, 1)
			var authWg sync.WaitGroup
			for _, node := range authNodes {
				authWg.Add(1)
				go func(u string) {
					defer authWg.Done()
					if a := loginURL(u); a != nil {
						select {
						case resultCh <- authResult{a, u}:
						default:
						}
					}
				}(ensureHTTPS(node))
			}
			go func() { authWg.Wait(); close(resultCh) }()

			if res, ok := <-resultCh; ok {
				authOK = true
				applyAuth(res.a, res.url)
			}

			// If all direct controls are unreachable, try cached relay peers as
			// transparent TCP proxies  -  same country first, then others.
			if !authOK && len(forcedControls) == 0 {
				cachedRelays, _ := core.LoadRelayList(filepath.Join(appDataDir, "relays.json"))
				if len(cachedRelays) > 0 {
					cc := loadCountry(appDataDir)
					for _, relay := range core.RelaysByCountry(cachedRelays, cc) {
						url := ensureHTTPS(relay.Addr)
						core.Log.Printf("auto-auth: trying relay=%s cc=%s", relay.Addr, relay.CountryCode)
						if a := loginURL(url); a != nil {
							core.Log.Printf("auto-auth: via relay OK relay=%s", relay.Addr)
							authOK = true
							applyAuth(a, url)
							break
						}
					}
				}
			}
			if authOK {
			} else {
				core.Log.Printf("auto-auth: all nodes unreachable")
			}
		} else {
			core.Log.Printf("saved key invalid: %v", err)
		}
	} else {
		core.Log.Printf("no saved key: %v", err)
	}
	// Show Connect whenever a key exists  -  even if auto-auth failed.
	// onConnect handles re-auth with saved credentials before dialling.
	initialLogin := savedKey != nil

	// Ã¢"â‚¬Ã¢"â‚¬ 4. System tray (blocks until Quit) Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬

	// Ã¢"â‚¬Ã¢"â‚¬ 4a. NodeID (needed by relay auto-start inside onConnect) Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	// (appDataDir declared above, before auto-auth, so relay cache is accessible at startup)

	// Load or generate stable device UUID for key-binding enforcement (M4+).
	deviceID = loadOrCreateDeviceID(appDataDir)
	core.Log.Printf("device ID: %.8s...", deviceID)

	nodeID, err := core.LoadOrGenNodeID(appDataDir)
	if err != nil {
		core.Log.Printf("warn: could not load/gen node ID: %v", err)
		nodeID = "unknown"
	}
	core.Log.Printf("node ID: %.8s...", nodeID)

	router := core.NewRouter()
	router.SetSelfNodeID(nodeID)

	// connStatsCollector lives for the whole process (created once, like
	// router/nodeID above), not per-connect -- its event counters must
	// accumulate across reconnects and only get drained by the uploader's
	// own tick (see ConnStatsCollector.Snapshot), same reasoning as why
	// dialerPool/router are declared once here instead of per-onConnect.
	connStatsCollector := core.NewConnStatsCollector(filepath.Join(appDataDir, "connstats.json"))

	// Ã¢"â‚¬Ã¢"â‚¬ 4b. DHT node  -  runs for the lifetime of the app Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	// Kept alive across connect/disconnect so the relay registry stays warm.
	// Tunnel traffic routing is independent; disconnected state just means the
	// router has no path to offer, not that DHT stops learning peers.
	if dhtID, err := core.ParseDHTID(nodeID); err == nil {
		if udpConn, err := net.ListenUDP("udp", &net.UDPAddr{}); err == nil {
			peersPath := filepath.Join(appDataDir, "peers.json")
			dhtNode = core.NewDHTNode(dhtID, udpConn, peersPath)
			dhtNode.Bootstrap(nil)                                           // load peers.json cache; no network seeds yet
			dhtNode.LoadRelays(filepath.Join(appDataDir, "dht_relays.json")) //nolint:errcheck
			dhtNode.Start()
			core.Log.Printf("dht: node started id=%.8s... addr=%s", nodeID, udpConn.LocalAddr())

			// Relay server: when another client wants to use us as relay, it sends
			// MsgHolePunch with its external addr and the control it wants to reach.
			// We punch back and create a UDPRelayConn that forwards to that control.
			dhtNode.SetHolePunchHandler(func(peerAddr, controlURL string) {
				conn, err := core.Punch("", peerAddr)
				if err != nil {
					core.Log.Printf("relay-server: punch to %s failed: %v", peerAddr, err)
					return
				}
				relay := core.NewUDPRelayConn(conn, controlURL)
				core.Log.Printf("relay-server: serving %s â†’ %s", peerAddr, controlURL)
				// relay.readLoop runs in the background; when the peer marks it
				// Failed we just close â€” no keepalive from our side needed (client
				// sends pings, we send pongs).
				go func() {
					<-relay.StopCh()
					relay.Close()
				}()
			})
		} else {
			core.Log.Printf("dht: UDP listen failed: %v  -  DHT disabled", err)
		}
	} else {
		core.Log.Printf("dht: bad node ID %q  -  DHT disabled", nodeID)
	}

	// Load persisted country so regional routing works correctly from the
	// very first connect after a restart.
	if cc := loadCountry(appDataDir); cc != "" {
		lastKnownCountry = cc
		core.Log.Printf("router: loaded persisted country %q", cc)
	}

	// Load user settings early so all goroutines (GPS, country checker) can read
	// PreferredRegion without referencing a not-yet-declared variable.
	settings := loadClientSettings(appDataDir)

	// Explicit region selection overrides persisted/detected country immediately.
	if settings.PreferredRegion != "" {
		lastKnownCountry = settings.PreferredRegion
		router.SetMyCountry(settings.PreferredRegion)
		core.Log.Printf("geo: explicit region %q applied from settings", settings.PreferredRegion)
	}

	// Detect device country from GPS/locale/timezone in background.
	// GPS may show a system consent dialog; completes within 5 s.
	// Result overrides the persisted value when it arrives.
	go func() {
		if cc := detectDeviceCC(); cc != "" {
			discoveredMu.Lock()
			// Always store GPS result for restoration when user switches back to Auto.
			gpsCountry = cc
			gpsDetected = true
			// Explicit region overrides GPS; GPS overrides CIDR.
			if settings.PreferredRegion == "" {
				lastKnownCountry = cc
			}
			discoveredMu.Unlock()
			if settings.PreferredRegion == "" {
				router.SetMyCountry(cc)
				saveCountry(appDataDir, cc)
				core.Log.Printf("geo: device country=%q applied", cc)
			} else {
				core.Log.Printf("geo: device country=%q stored (explicit region %q active)", cc, settings.PreferredRegion)
			}
		}
	}()

	// dhtMergerStop is closed on disconnect to stop merging DHT relays into
	// the router.  Re-created on each connect.
	var dhtMergerStop chan struct{}

	// relayAnnounceStop is closed on disconnect to stop the relay-entry
	// announce loop (endpoint probe + sign + publish, retried every 30s while
	// it hasn't succeeded).  Re-created on each connect.  Before 2026-08-10
	// this loop had no stop channel at all and dhtMergerStop was only closed
	// from onDisconnect -- a connect attempt that failed/timed out (tray.go's
	// doConnect, which retries via scheduleRetry WITHOUT calling onDisconnect
	// first) leaked a fresh, never-cancelled copy of both goroutines on every
	// retry. Over a long run of retries (e.g. a flaky network for an hour)
	// the leaked copies compound and their combined DHT probing/holepunch
	// traffic can saturate the network enough to disrupt unrelated traffic
	// (observed: a concurrent Teams call degraded badly while this was
	// happening). Fixed by stopping any previous instance of both loops at
	// the top of onConnect, not only in onDisconnect.
	var relayAnnounceStop chan struct{}

	// countryCheckerStop is closed on disconnect to stop the periodic
	// country-detection loop.  Re-created on each connect.
	var countryCheckerStop chan struct{}

	var trayApp *snwin.TrayApp

	var (
		socksLn           net.Listener
		socks5            *core.SOCKS5Server
		tun               *core.TUNBridge
		routes            *snwin.RouteManager
		bypassMgr         *core.BypassManager
		decoyMgr          *core.DecoyManager
		logUploader       *core.LogUploader
		connStatsUploader *core.ConnStatsUploader
		origGW            string          // saved for DoH restoration on disconnect
		dohProxy          *snwin.DoHProxy // non-nil when fallback DoH proxy is running
		dohNetshActive    bool            // true when netsh DoH was configured
		dohMu             sync.Mutex      // guards dohProxy and dohNetshActive
		dialerPool        *core.DialerPool
		poolRefreshStop   chan struct{} // closed to stop the pool refresh goroutine
	)

	// onLogin shows the key dialog (or, for users with no key yet, the
	// "do you have a key?" branch that can end in a direct navlink.net
	// login + automatic key issuance), authenticates, and wires up the dialer.
	onLogin := func() error {
		keyStr, err := obtainActivationKey()
		if err != nil {
			return err
		}
		kd, err := core.ParseKeyString(keyStr)
		if err != nil {
			return fmt.Errorf("invalid key: %w", err)
		}
		if len(kd.Nodes()) == 0 {
			return fmt.Errorf("key contains no server addresses")
		}

		tryLoginAuth := func(url string) bool {
			core.Log.Printf("login: auth user %s @ %s", kd.Username, url)
			a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
			a.SetKeyAuth(kd)
			a.SetDeviceInfo(kd.KeyID, deviceID, "Windows PC")
			if err := a.Login(); err != nil {
				core.Log.Printf("login: auth failed %s: %v", url, err)
				return false
			}
			core.Log.Printf("login: auth OK %s token=%s...", url, a.Token()[:8])
			dialer = core.NewTunnelDialer(a)
			serverURL = url
			go startDiscovery(url, kd, dhtNode)
			return true
		}

		// Try all nodes from the key, then fall back to cached relay peers.
		// A cached manifest (from a prior session, any account) is network-
		// wide and authoritative once it exists — see core.BootstrapControlList.
		authOK := false
		loginNodes := core.BootstrapControlList(
			core.ReadManifestCacheControls(manifestCacheFile(), kd.ArbiterPubkey), kd.Nodes())
		if len(forcedControls) > 0 {
			loginNodes = forcedControls
		}
		for _, node := range loginNodes {
			if tryLoginAuth(ensureHTTPS(node)) {
				authOK = true
				break
			}
		}
		if !authOK && len(forcedControls) == 0 {
			cachedRelays, _ := core.LoadRelayList(filepath.Join(appDataDir, "relays.json"))
			if len(cachedRelays) > 0 {
				cc := loadCountry(appDataDir)
				for _, relay := range core.RelaysByCountry(cachedRelays, cc) {
					url := ensureHTTPS(relay.Addr)
					core.Log.Printf("login: trying relay=%s cc=%s", relay.Addr, relay.CountryCode)
					if tryLoginAuth(url) {
						core.Log.Printf("login: via relay OK relay=%s", relay.Addr)
						authOK = true
						break
					}
				}
			}
		}
		if !authOK {
			return fmt.Errorf("authentication failed: all nodes unreachable")
		}
		savedKey = kd
		if saveErr := snwin.SaveKey(keyStr); saveErr != nil {
			core.Log.Printf("warn: could not save key: %v", saveErr)
		}
		if !snwin.IsCurrentExeAutostarted() {
			if err := snwin.RegisterAutostart(); err != nil {
				core.Log.Printf("warn: autostart register: %v", err)
			} else {
				core.Log.Println("autostart: registered")
			}
		}
		return nil
	}

	onLogout := func() {
		savedKey = nil
		dialer = nil
		serverURL = ""
	}

	// fwRuleName is the Windows Firewall outbound rule we install on connect and
	// remove on disconnect.  The rule allows our process to make new outbound TCP
	// connections after the TUN adapter is up  -  without it, Windows Firewall may
	// block new connections with WSAEACCES even when bypass routes are in place.
	const fwRuleName = "ShortNerdCat"

	// fwRuleInstalled tracks whether our firewall rule is currently active.
	// The rule is added once on first connect and removed only on user-initiated
	// disconnect (Quit, Disconnect button, Logout).  Automatic reconnect cycles
	// leave it in place so no netsh windows appear during silent recovery.
	var fwRuleInstalled bool

	addOutboundFirewallRule := func() {
		if fwRuleInstalled {
			return // already installed  -  skip during auto-reconnect
		}
		exe, err := os.Executable()
		if err != nil {
			core.Log.Printf("firewall: cannot get executable path: %v", err)
			return
		}
		// Silently remove any stale rule left by a previous crashed session.
		fwHiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule", //nolint:errcheck
			"name="+fwRuleName).Run()
		out, err := fwHiddenCmd("netsh", "advfirewall", "firewall", "add", "rule",
			"name="+fwRuleName, "dir=out", "action=allow",
			"program="+exe, "enable=yes",
		).CombinedOutput()
		if err != nil {
			core.Log.Printf("firewall: add outbound rule failed: %v (%s)", err, strings.TrimSpace(string(out)))
			return
		}
		fwRuleInstalled = true
		core.Log.Printf("firewall: outbound allow rule added for %s", filepath.Base(exe))
	}

	removeOutboundFirewallRule := func() {
		if !fwRuleInstalled {
			return // not installed  -  nothing to remove
		}
		out, err := fwHiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+fwRuleName,
		).CombinedOutput()
		if err != nil {
			core.Log.Printf("firewall: delete outbound rule failed: %v (%s)", err, strings.TrimSpace(string(out)))
			return
		}
		fwRuleInstalled = false
		core.Log.Printf("firewall: outbound allow rule removed")
	}

	onConnect := func(autoReconnect bool) error {
		if !autoReconnect {
			addOutboundFirewallRule()
		}

		// publicIP is the client's external IP, fetched below via FetchMyIP.
		// Declared here so wireDialer can capture it by reference and inject it
		// into every TunnelDialer via SetClientIP (for accurate country stats on exit).
		var publicIP string

		// wireDialer attaches the traffic-RTT hook to a freshly created dialer so
		// passive POST round-trip observations feed back into the router's scoring.
		// It also stamps the dialer with the client's public IP for X-Client-IP.
		wireDialer := func(td *core.TunnelDialer) *core.TunnelDialer {
			td.SetRTTUpdateHook(func(surl string, rtt time.Duration) {
				router.UpdateTrafficRTT(surl, rtt)
			})
			if publicIP != "" {
				td.SetClientIP(publicIP)
			}
			discoveredMu.RLock()
			cc := lastKnownCountry
			discoveredMu.RUnlock()
			if cc != "" {
				td.SetClientCC(cc)
			}
			return td
		}
		// Wire the hook on the existing dialer if present (may be from auto-login
		// or onLogin, which run before this closure is defined).  Re-wiring on
		// reconnect is harmless  -  SetRTTUpdateHook just replaces the callback.
		core.Log.Printf("connect: dialer=%v trayApp=%v", dialer != nil, trayApp != nil)
		if dialer != nil {
			wireDialer(dialer)
		}

		// If auto-auth failed at startup (or token expired), re-authenticate
		// silently with saved credentials before attempting to connect.
		if dialer == nil && savedKey != nil {
			srvURL := pickServerURL(savedKey)
			core.Log.Printf("connect: re-auth user %s @ %s", savedKey.Username, srvURL)
			a := core.NewAuthenticator(srvURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetKeyAuth(savedKey)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
			if err := a.Login(); err != nil {
				core.Log.Printf("connect: re-auth direct failed: %v â€” trying UDP relay bootstrap", err)
				// Fallback: bootstrap via DHT UDP relay when TCP is blocked.
				udpOK := false
				if dhtNode != nil {
					var ownUDPAddr string
					for _, n := range savedKey.Nodes() {
						ep, perr := core.ProbeExternalEndpoint(n, "")
						if perr == nil {
							ownUDPAddr = ep.String()
							break
						}
					}
					if ownUDPAddr != "" {
					relayLoop:
						for _, entry := range dhtNode.Relays() {
							for _, ctrlNode := range savedKey.Nodes() {
								ctrlURL := ensureHTTPS(ctrlNode)
								dhtNode.SendHolePunch(entry.Addr, ownUDPAddr, ctrlURL)
								time.Sleep(150 * time.Millisecond)
								conn, perr := core.Punch("", entry.Addr)
								if perr != nil {
									core.Log.Printf("connect: udp-relay punch %s: %v", entry.Addr, perr)
									continue
								}
								rc := core.NewUDPRelayConn(conn, "")
								ra := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
								ra.SetKeyAuth(savedKey)
								ra.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
								if rerr := ra.LoginViaUDP(rc); rerr != nil {
									core.Log.Printf("connect: udp-relay %sâ†’%s: %v", entry.Addr, ctrlNode, rerr)
									rc.Close()
									continue
								}
								core.Log.Printf("connect: re-auth via UDP relay OK relay=%s ctrl=%s", entry.Addr, ctrlNode)
								dialer = wireDialer(core.NewUDPRelayDialer(rc, ra))
								serverURL = ctrlURL
								udpOK = true
								break relayLoop
							}
						}
					}
				}
				if !udpOK {
					return fmt.Errorf("authentication failed: %w", err)
				}
			} else {
				core.Log.Printf("connect: re-auth OK, token=%s...", a.Token()[:8])
				dialer = wireDialer(core.NewTunnelDialer(a))
				serverURL = srvURL
			}
		}
		if dialer == nil {
			return fmt.Errorf("not logged in")
		}

		// Route selection (M2.4): measure RTT to all relays, score paths, pick best.
		// Re-runs on every connect so a stale/dead relay is excluded automatically.
		relayAPIURL := serverURL

		// Register ALL control nodes from the key so the router can score and
		// failover across them.  Discovered controls are appended if available.
		// When --controls is active, only those controls are used.
		// allCtrlAddrs is declared in the outer scope so it can be used later
		// to install bypass routes for every control IP after routes.Apply.
		var allCtrlAddrs []string
		{
			if len(forcedControls) > 0 {
				for _, n := range forcedControls {
					allCtrlAddrs = append(allCtrlAddrs, hostNameOf(n)+":443")
				}
				core.Log.Printf("connect: using forced controls: %v", allCtrlAddrs)
				router.SetControlsWithRegions(allCtrlAddrs, nil)
			} else {
				if savedKey != nil {
					for _, n := range savedKey.Nodes() {
						allCtrlAddrs = append(allCtrlAddrs, hostNameOf(n)+":443")
					}
				}
				discoveredMu.RLock()
				regions := discoveredRegions
				loadFactors := discoveredLoadFactors
				for _, dc := range discoveredControls {
					h := hostNameOf(dc) + ":443"
					found := false
					for _, a := range allCtrlAddrs {
						if a == h {
							found = true
							break
						}
					}
					if !found {
						allCtrlAddrs = append(allCtrlAddrs, h)
					}
				}
				discoveredMu.RUnlock()
				if len(allCtrlAddrs) == 0 {
					allCtrlAddrs = []string{hostNameOf(serverURL) + ":443"}
				}
				router.SetControlsWithRegions(allCtrlAddrs, regions)
				router.SetLoadFactors(loadFactors)
			}
		}

		core.Log.Printf("connect: fetching relay list")
		var fetchedRelays []core.RelayEntry
		if relays, err := core.FetchRelayList(relayAPIURL); err == nil {
			fetchedRelays = relays
			router.UpdateRelays(relays)
			if err := core.SaveRelayList(filepath.Join(appDataDir, "relays.json"), relays); err != nil {
				core.Log.Printf("connect: save relay list: %v", err)
			}
		} else {
			core.Log.Printf("connect: relay list fetch failed (%v)  -  will route direct", err)
			router.UpdateRelays(nil)
		}
		core.Log.Printf("connect: probing data plane")
		router.ProbeDataPlane(3 * time.Second)
		core.Log.Printf("connect: building paths")
		// Restore my-country before BuildPaths: disconnect clears it, and the
		// country-checker goroutine may be skipped (GPS/explicit region).
		discoveredMu.RLock()
		cc := lastKnownCountry
		discoveredMu.RUnlock()
		if cc != "" {
			router.SetMyCountry(cc)
		}
		router.BuildPaths()

		// Switch to a different control only when the primary path is significantly
		// better than the current one (â‰¥50% lower RTT score) or the current
		// control has dropped out of all paths entirely.  Switching on marginal
		// differences would destabilise an already-connected session.
		if better, p := router.PrimaryIsBetter(serverURL, 1.5); better && p != nil {
			preferred := ensureHTTPS(p.ControlAddr)
			core.Log.Printf("connect: router prefers control %s over %s (â‰¥50%% better) â€” switching", p.ControlAddr, serverURL)
			a := core.NewAuthenticator(preferred, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
			a.SetKeyAuth(savedKey)
			a.AdoptToken(dialer.Token())
			core.Log.Printf("connect: switched to %s (token migrated)", p.ControlAddr)
			if td, tdErr := router.NewControlDialer(p.ControlAddr, a); tdErr == nil {
				dialer = wireDialer(td)
			} else {
				dialer = wireDialer(core.NewTunnelDialer(a))
			}
			serverURL = preferred
			relayAPIURL = preferred
		}

		// DHT (M4.2): seed from control node IPs (they run DHT on the same UDP port)
		// plus any relay addresses from the API.  Bootstrap is additive â€” already-known
		// peers are skipped.  Controls are the reliable bootstrap seeds; user relays
		// are discovered via DHT after bootstrap.
		if dhtNode != nil {
			// Stop any previous connect attempt's announce/merger loops before
			// starting new ones. Normally onDisconnect already did this, but a
			// connect attempt that failed or timed out retries via
			// tray.go's scheduleRetry WITHOUT going through onDisconnect first
			// -- without this, every such retry leaked another permanently-running
			// copy of both loops (see relayAnnounceStop's doc comment above).
			if relayAnnounceStop != nil {
				close(relayAnnounceStop)
				relayAnnounceStop = nil
			}
			if dhtMergerStop != nil {
				close(dhtMergerStop)
				dhtMergerStop = nil
			}

			ctrlAddrs := savedKey.Nodes()
			seeds := make([]string, 0, len(ctrlAddrs)+len(fetchedRelays))
			for _, addr := range ctrlAddrs {
				seeds = append(seeds, addr)
			}
			for _, r := range fetchedRelays {
				seeds = append(seeds, r.Addr)
			}
			dhtNode.Bootstrap(seeds)
			core.Log.Printf("dht: bootstrapped with %d control(s) + %d relay(s)", len(ctrlAddrs), len(fetchedRelays))

			// Announce this node as a relay so other clients can discover it.
			// Probe the external UDP endpoint via the reflector, then sign and publish.
			announceStop := make(chan struct{})
			relayAnnounceStop = announceStop
			go func() {
				for {
					for _, ctrlAddr := range ctrlAddrs {
						select {
						case <-announceStop:
							return
						default:
						}
						ep, err := core.ProbeExternalEndpoint(ctrlAddr, "")
						if err != nil {
							core.Log.Printf("dht: probe external endpoint via %s: %v", ctrlAddr, err)
							continue
						}
						cc := lastKnownCountry
						entry, err := core.BuildSignedRelayEntry(nodeID, ep.String(), cc)
						if err != nil {
							core.Log.Printf("dht: sign relay entry: %v", err)
							return
						}
						dhtNode.SetOwnEntry(entry)
						core.Log.Printf("dht: own relay entry set addr=%s cc=%s", ep, cc)
						// Register a refresher so each subsequent announce uses a fresh TS+sig.
						// Captures ep and cc from the probe; re-probing on every tick is not
						// needed because the NAT mapping is kept alive by the announce packets.
						epStr := ep.String()
						dhtNode.SetEntryRefresher(func() (*dht.RelayEntry, error) {
							return core.BuildSignedRelayEntry(nodeID, epStr, cc)
						})
						return
					}
					core.Log.Printf("dht: all endpoint probes failed, retrying in 30s")
					select {
					case <-announceStop:
						return
					case <-time.After(30 * time.Second):
					}
				}
			}()

			// Merge DHT-discovered relays into router and initiate hole punches
			// for blocked controls.  Runs every 30 s â€” fast enough to react to
			// new relay entries (RelayTTL = 2 min) without hammering the network.
			stop := make(chan struct{})
			dhtMergerStop = stop
			go func() {
				t := time.NewTicker(30 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						entries := dhtNode.Relays()
						if len(entries) == 0 {
							continue
						}
						dhtNode.SaveRelays(filepath.Join(appDataDir, "dht_relays.json")) //nolint:errcheck
						router.MergeDHTRelays(entries)
						router.ProbeDataPlane(3 * time.Second)
						router.BuildPaths()

						// Relay client: for each relay that has no live UDP peer yet,
						// send a HolePunch invitation and punch simultaneously.
						// Only do this for controls that are currently unreachable directly.
						blockedCtrls := router.UnreachableControls()
						if len(blockedCtrls) == 0 {
							continue
						}
						ownEntry := dhtNode.OwnEntry()
						if ownEntry == nil {
							continue // own external addr not yet probed
						}
						for _, entry := range entries {
							if router.HasUDPPeer(entry.NodeID) {
								continue // already connected
							}
							if entry.NodeID == nodeID || entry.Addr == ownEntry.Addr {
								continue // don't relay through ourselves
							}
							for _, ctrl := range blockedCtrls {
								core.Log.Printf("relay-client: punching %s for ctrl=%s", entry.Addr, ctrl)
								go func(relayAddr, ctrlURL, ownAddr string) {
									// Send HolePunch invitation to relay via DHT socket.
									dhtNode.SendHolePunch(relayAddr, ownAddr, ctrlURL)
									// Punch from our side simultaneously.
									conn, err := core.Punch("", relayAddr)
									if err != nil {
										core.Log.Printf("relay-client: punch %s failed: %v", relayAddr, err)
										return
									}
									rc := core.NewUDPRelayConn(conn, "")
									router.RegisterUDPPeer(entry.NodeID, rc)
									core.Log.Printf("relay-client: relay established %s â†’ %s", relayAddr, ctrlURL)
									router.BuildPaths()
								}(entry.Addr, ctrl, ownEntry.Addr)
								break // one control per relay is enough to establish the path
							}
						}
					}
				}
			}()
		}

		// Ã¢"â‚¬Ã¢"â‚¬ Country pre-detection Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
		// Detect the client's country via bypass CIDR BEFORE the TUN starts.
		// This blocks user traffic from flowing until we know which regional
		// control is correct (or 5 s timeout).  If a better control exists,
		// re-auth and rebuild paths before the tunnel opens.
		//
		// FetchMyIP runs here (before TUN/routes) so:
		//   (a) the request leaves on the physical NIC, and
		//   (b) the bypass manager can pass myIP to /api/bypass/cidr for
		//       country detection (refresh() returns early without it).
		var myIPErr error
		publicIP, myIPErr = core.FetchMyIP(relayAPIURL)
		if myIPErr != nil {
			core.Log.Printf("myip: fetch failed: %v", myIPErr)
		} else {
			core.Log.Printf("myip: %s", publicIP)
			dialer.SetClientIP(publicIP) // primary dialer created before FetchMyIP; stamp it now
		}
		{
			bPubkey := ""
			if savedKey != nil {
				bPubkey = savedKey.ArbiterPubkey
			}
			bCacheFile := filepath.Join(appDataDir, "cidr.json")
			if bm, err := core.NewBypassManager([]string{serverURL}, bPubkey, bCacheFile); err == nil {
				bm.SetToken(dialer.Token())
				if publicIP != "" {
					bm.SetMyIP(publicIP)
				}
				bm.Start()
				bypassMgr = bm
				if savedKey != nil {
					// SetAdminAccount is seeded here from the key's static
					// IsAdmin field (best answer available before the first
					// live poll completes); initClubDiscovery's adminPoller
					// takes over from there and keeps it live afterward.
					initClubDiscovery(serverURL, savedKey, dialer.Token)
					if appWindow != nil {
						appWindow.SetAdminAccount(savedKey.IsAdmin)
					}
				}
			} else {
				core.Log.Printf("connect: bypass init failed (%v)  -  skipping country pre-detection", err)
			}
			if bypassMgr != nil {
				// Skip CIDR-based country detection when GPS already answered or the
				// user has explicitly selected a region â€” those take precedence.
				discoveredMu.RLock()
				skipCIDR := gpsDetected || settings.PreferredRegion != ""
				discoveredMu.RUnlock()
				if skipCIDR {
					core.Log.Printf("connect: skipping CIDR country pre-detection (gpsDetected=%v preferredRegion=%q)", gpsDetected, settings.PreferredRegion)
				} else {
					core.Log.Printf("connect: waiting for country detection (up to 5 s)...")
					cc := waitForCountry(bypassMgr, 5*time.Second)
					if cc != "" {
						discoveredMu.Lock()
						lastKnownCountry = cc
						discoveredMu.Unlock()
						saveCountry(appDataDir, cc)
						core.Log.Printf("connect: pre-detected country=%q", cc)
						if best := pickServerURL(savedKey); best != "" && best != serverURL {
							core.Log.Printf("connect: switching control %s  ->  %s (country=%s)", serverURL, best, cc)
							a := core.NewAuthenticator(best, savedKey.APIKey, savedKey.Username, savedKey.Password)
							a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
							a.SetKeyAuth(savedKey)
							a.AdoptToken(dialer.Token())
							ctrlAddr := hostNameOf(best) + ":443"
							if td, tdErr := router.NewControlDialer(ctrlAddr, a); tdErr == nil {
								dialer = wireDialer(td)
							} else {
								dialer = wireDialer(core.NewTunnelDialer(a))
							}
							serverURL = best
							relayAPIURL = best
							router.ProbeDataPlane(3 * time.Second)
							router.BuildPaths()
							// Restart bypass manager against new control server.
							bypassMgr.Stop()
							if bm2, err := core.NewBypassManager([]string{best}, bPubkey, bCacheFile); err == nil {
								bm2.SetToken(a.Token())
								if publicIP != "" {
									bm2.SetMyIP(publicIP)
								}
								bm2.Start()
								bypassMgr = bm2
							} else {
								bypassMgr = nil
							}
						}
					} else {
						core.Log.Printf("connect: country detection timed out  -  proceeding with %s", serverURL)
					}
				}
			}
		}

		core.Log.Printf("connect: picking path")
		effectiveURL := serverURL
		routingViaRelay := false
		path := router.Primary()
		if path != nil && !path.IsDirect() {
			relay := path.Relays[0]
			if path.UDPRelay != nil {
				// UDP hole-punched relay: bypass HTTP, route via peer UDP socket.
				core.Log.Printf("connect: routing via UDP relay score=%.0f peer=%s", path.Score, relay.Addr)
				dialer = core.NewUDPRelayDialer(path.UDPRelay, dialer.Auth())
			} else {
				// TCP relay: keep serverURL as the control URL so the pool tracks
				// this dialer correctly (Has/NeedsRefill use ServerURL). Redirect
				// TCP connections through the relay via dial override instead.
				// The relay is a pure TCP forwarder; TLS terminates at the control.
				relayAddr := relay.Addr
				relayDialFn := func(ctx context.Context, _ string) (net.Conn, error) {
					return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).
						DialContext(ctx, "tcp", relayAddr)
				}
				dialer.Auth().SetDialFunc(relayDialFn)
				dialer.SetDialFunc(relayDialFn)
				effectiveURL = ensureHTTPS(relay.Addr) // bypass route must cover relay IP
				routingViaRelay = true
				core.Log.Printf("connect: routing via relay score=%.0f addr=%s", path.Score, relay.Addr)
			}
		} else {
			core.Log.Printf("connect: no relay path selected  -  routing direct to control")
		}

		// If routing direct (no relay) and the router detected TCP is blocked on the
		// primary control, upgrade the dialer to UDP â€” but ONLY when no other TCP-capable
		// control exists.  Preferring a TCP-capable standby as primary is safer than UDP,
		// which is less reliable and whose first-fail hook can cascade into a 404 storm.
		if !routingViaRelay {
			ctrlAddr := hostNameOf(serverURL) + ":443"
			if router.ControlTransportName(ctrlAddr) == "udp" {
				hasTCPControl := false
				discoveredMu.RLock()
				for _, dc := range discoveredControls {
					if hostNameOf(dc)+":443" != ctrlAddr && router.ControlTransportName(hostNameOf(dc)+":443") != "udp" {
						hasTCPControl = true
						break
					}
				}
				discoveredMu.RUnlock()
				if hasTCPControl {
					core.Log.Printf("connect: primary control %s TCP blocked but TCP-capable controls exist â€” skipping UDP upgrade", ctrlAddr)
				} else if td, tdErr := router.NewControlDialer(ctrlAddr, dialer.Auth()); tdErr == nil {
					core.Log.Printf("connect: primary control %s TCP blocked (no TCP alternatives) â€” upgrading to UDP", ctrlAddr)
					dialer = wireDialer(td)
				}
			}
		}

		// buildViableAddrs returns qualifying controls sorted by e2e RTT (best first).
		// Controls whose exits are unreachable are excluded and marked data-dead.
		// A control is considered alive only if data can flow through it to an exit;
		// TCP reachability alone is not sufficient.
		buildViableAddrs := func(qual []string) []string {
			type pr struct {
				addr string
				rtt  time.Duration
				ok   bool
			}
			ch := make(chan pr, len(qual))
			for _, addr := range qual {
				go func(addr string) {
					// Use UDP probe for controls whose TCP path is blocked so that
					// UDP-mode controls are not wrongly excluded from the pool.
					var rtt time.Duration
					var ok bool
					if router.ControlTransportName(addr) == "udp" {
						rtt, ok = core.ProbeControlUDP(addr, 4*time.Second)
					} else {
						rtt, ok = core.ProbeControlE2E(addr, 4*time.Second)
					}
					ch <- pr{addr, rtt, ok}
				}(addr)
			}
			var viable []string
			for range qual {
				res := <-ch
				if res.ok {
					router.UpdateControlRTT(res.addr, res.rtt)
					viable = append(viable, res.addr)
				} else {
					core.Log.Printf("connect: pool: e2e probe failed %s  -  exits unreachable", res.addr)
				}
			}
			sort.Slice(viable, func(i, j int) bool {
				// TCP-capable controls always sort before UDP-only so that slot 0
				// (primary) is never UDP when a TCP alternative is available.
				iUDP := router.ControlTransportName(viable[i]) == "udp"
				jUDP := router.ControlTransportName(viable[j]) == "udp"
				if iUDP != jUDP {
					return !iUDP // TCP (false) < UDP (true)
				}
				ri, rj := router.ControlRTT(viable[i]), router.ControlRTT(viable[j])
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
			if len(viable) > 12 {
				viable = viable[:12]
			}
			return viable
		}

		// Clear any stale dial-func overrides from the previous session.
		if dialer != nil {
			dialer.ClearDialFunc()
			dialer.Auth().ClearDialFunc()
		}

		// buildViableAddrsTopup probes qualifying (in-country) controls and tops up
		// to 5 with out-of-country controls when in-country viable count < 5.
		// In-country controls are listed first (lower RTT priority preserved).
		// When filling out-of-country slots, RU and CN controls are ranked last
		// regardless of RTT â€” they route traffic through adversarial jurisdictions.
		buildViableAddrsTopup := func(qual []string) []string {
			viable := buildViableAddrs(qual)
			if len(viable) >= 5 {
				return viable
			}
			qualSet := make(map[string]bool, len(qual))
			for _, a := range qual {
				qualSet[a] = true
			}
			discoveredMu.RLock()
			allControls := append([]string{}, discoveredControls...)
			regions := discoveredRegions
			discoveredMu.RUnlock()
			var fallback []string
			for _, addr := range allControls {
				if !qualSet[addr] {
					fallback = append(fallback, addr)
				}
			}
			if len(fallback) == 0 {
				return viable
			}
			need := 5 - len(viable)
			core.Log.Printf("connect: pool: topping up  -  need %d more, probing %d out-of-country candidate(s)", need, len(fallback))
			extra := buildViableAddrs(fallback)
			// Deprioritize RU/CN: sort them after all other regions, preserving RTT
			// order within each group. buildViableAddrs already sorted by RTT, so
			// SliceStable keeps that order intact within the two groups.
			sort.SliceStable(extra, func(i, j int) bool {
				isRuCn := func(addr string) bool {
					cc := regions[addr]
					return cc == "RU" || cc == "CN"
				}
				di, dj := isRuCn(extra[i]), isRuCn(extra[j])
				if di != dj {
					return !di // non-RU/CN first
				}
				return false // preserve existing RTT order within group
			})
			if len(extra) > need {
				extra = extra[:need]
			}
			if len(extra) > 0 {
				core.Log.Printf("connect: pool: added %d out-of-country control(s), total viable=%d", len(extra), len(viable)+len(extra))
			}
			return append(viable, extra...)
		}

		// Build the initial set of viable controls.
		// Run e2e probes and top up to 5 with out-of-country controls.
		initialViable := buildViableAddrsTopup(router.QualifyingControlAddrs())

		// Build DialerPool from all qualifying controls (post BuildPaths + country filter).
		// dialers[0] is primary (lowest e2e RTT); standbys follow in RTT order.
		buildDialerSlice := func(viable []string) []*core.TunnelDialer {
			var poolDialers []*core.TunnelDialer
			for _, addr := range viable {
				ctrlURL := ensureHTTPS(addr)
				var td *core.TunnelDialer
				if ctrlURL == serverURL {
					td = dialer // reuse the already-authenticated primary dialer
				} else {
					a := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
					a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
					a.SetKeyAuth(savedKey)
					if err := a.Login(); err != nil {
						core.Log.Printf("connect: pool: auth to %s failed (%v)  -  skipping", addr, err)
						continue
					}
					var err error
					td, err = router.NewControlDialer(addr, a)
					if err != nil {
						core.Log.Printf("connect: pool: dialer for %s failed (%v)  -  skipping", addr, err)
						continue
					}
					wireDialer(td)
					core.Log.Printf("connect: pool: added control %s", addr)
				}
				poolDialers = append(poolDialers, td)
			}
			if len(poolDialers) == 0 {
				poolDialers = []*core.TunnelDialer{dialer} // fallback: primary only
			}
			core.Log.Printf("connect: dialer pool size=%d", len(poolDialers))
			return poolDialers
		}
		core.Log.Printf("connect: building dialer pool viable=%d", len(initialViable))
		initialPoolDialers := buildDialerSlice(initialViable)
		dialerPool = core.NewDialerPool(initialPoolDialers)
		core.Log.Printf("connect: dialer pool ready size=%d", dialerPool.Size())

		// Widen manifest-fetch candidates beyond the last-cached (≤12-node)
		// manifest with whatever's in the active dialer pool right now, and
		// fall back to fetching directly from navlink.net (bypassing TUN via
		// the physical NIC, like decoy traffic) when every known control
		// fails outright. See discovery.go's doc comments on both.
		if globalDisc != nil {
			globalDisc.SetExtraURLsProvider(func() []string {
				if dialerPool == nil {
					return nil
				}
				return dialerPool.URLs()
			})
			globalDisc.SetNavlinkFallback(func() string {
				if routes == nil {
					return ""
				}
				return routes.LocalAddr()
			}, nil, core.DefaultClientTelemetryKey)
		}

		// BananaMeter tunnel-diagnostics probe (see TODO.md "BananaMeter-based
		// tunnel diagnostics"): started once per process, not per connect --
		// it reads dialerPool/trayApp fresh on every tick, so it survives
		// reconnects and pool swaps on its own.
		bananameterProberOnce.Do(func() {
			if savedKey != nil {
				prober := core.NewBananameterProber(core.DefaultBananameterCreds(), savedKey.ClientID, deviceID, savedKey.Username)
				prober.Start(
					func() *core.TunnelDialer {
						if dialerPool == nil {
							return nil
						}
						return dialerPool.Pick()
					},
				)
			}
		})

		// Warning: first re-auth failure  ->  orange icon so user sees something is wrong.
		dialer.SetReAuthWarningHook(func() {
			core.Log.Printf("tunnel: re-auth failing  -  showing warning")
			if trayApp != nil {
				trayApp.SetAuthWarning("auth server unavailable, retrying...")
			}
		})
		// Recovery: re-auth succeeded after a warning  ->  restore green icon.
		dialer.SetReAuthRecoveredHook(func() {
			core.Log.Printf("tunnel: re-auth recovered  -  clearing warning")
			if trayApp != nil {
				trayApp.ClearAuthWarning()
			}
		})
		// Fatal: exhausted retry window.
		// Auth rejection (server refuses credentials)  ->  login error state, user must re-enter key.
		// Server unavailable (network/arbiter down)  ->  auto-reconnect and keep trying.
		dialer.SetFatalErrorHook(func(err error) {
			core.Log.Printf("tunnel: fatal re-auth failure (%v)", err)
			if trayApp == nil {
				return
			}
			if strings.Contains(err.Error(), "server unavailable") {
				core.Log.Printf("tunnel: server unavailable  -  triggering reconnect")
				trayApp.TriggerReconnect()
			} else {
				core.Log.Printf("tunnel: credentials rejected  -  entering login error state")
				trayApp.ShowLoginError()
			}
		})
		// startSilentRefresh is declared here (forward reference) so that
		// attachDataFailHook can call it; it is assigned right below.
		var startSilentRefresh func()
		var silentRefreshActive int32

		// Data-plane failure: evict dead dialer, then attempt a silent path
		// switch  -  rebuild the dialer pool without restarting TUN or routes.
		// TUN stays up so no traffic leaks to the physical NIC during the switch.
		// Escalates to a full TriggerReconnect only after 2 minutes of failure.
		attachDataFailHook := func(td *core.TunnelDialer) {
			ctrlURL := td.ServerURL()
			// Soft eviction: fires when â‰¥50% of dial/stream outcomes in the 30s window fail.
			// Applies a flap penalty and routes new connections to a standby.
			td.SetFirstFailHook(func() {
				// dialerPool is nulled out by disconnect() without cancelling in-flight
				// dials on the dialers it owned -- a dial already underway can still
				// fail and fire this hook afterward. Snapshot into a local and bail if
				// torn down; a crashed process is a total outage (relaunched by the
				// watchdog, dropping every live connection), while a no-op here is
				// correct since there's no pool left to evict from anyway.
				pool := dialerPool
				if pool == nil {
					return
				}
				if pool.Size() > 1 {
					core.Log.Printf("tunnel: first stream failure on %s  -  instant eviction, switching to standby", ctrlURL)
					pool.Evict(td)
					router.RecordControlFlap(hostNameOf(ctrlURL) + ":443")
					startSilentRefresh()
				}
			})
			td.SetDataFailHook(3, func() {
				pool := dialerPool
				if pool == nil {
					return
				}
				addr := hostNameOf(ctrlURL) + ":443"
				core.Log.Printf("tunnel: data-plane failure on %s  -  marking data-dead, evicting", ctrlURL)
				core.SetTunnelHealthy(false)
				router.MarkControlDataDead(addr, time.Now().Add(1*time.Minute))
				pool.Evict(td)
				if pool.Size() == 0 {
					core.Log.Printf("tunnel: pool empty after data-fail  -  triggering full reconnect")
					if trayApp != nil {
						trayApp.TriggerReconnect()
					}
					return
				}
				startSilentRefresh()
			})
		}

		// UDP relay failure: downgrade the control to TCP-only for 5 min, evict
		// the dialer with the broken UDP channel, and trigger a silent refresh.
		attachUDPFailedHook := func(td *core.TunnelDialer) {
			ctrlURL := td.ServerURL()
			td.SetUDPFailedHook(func() {
				pool := dialerPool
				if pool == nil {
					return
				}
				addr := hostNameOf(ctrlURL) + ":443"
				core.Log.Printf("tunnel: UDP relay failed on %s  -  downgrading to TCP, starting silent refresh", ctrlURL)
				core.SetTunnelHealthy(false)
				router.MarkUDPDataFailed(addr)
				if pool.Size() > 1 {
					pool.Evict(td)
				} else {
					core.Log.Printf("tunnel: last dialer  -  not evicting, waiting for silent refresh")
				}
				startSilentRefresh()
			})
		}

		// attachAuthFailHook reacts to repeated *login* failures against this
		// dialer's control node -- distinct from attachDataFailHook's
		// data-plane failures, which fire on Dial/stream outcomes and never
		// trigger for a dialer whose Authenticator can't complete a login at
		// all. Without this, such a dialer just retries the same fixed node
		// forever (refreshToken's own 3-hour patience window) even though the
		// router's RTT/liveness probe -- a different, unauthenticated
		// transport -- may still report it "alive" (e.g. TCP blocked but UDP
		// still answering /p/v1/ping; see the 2026-08-11 control incident).
		// Reuses the same eviction + flap + silent-refresh path as
		// attachDataFailHook, so node ranking itself is untouched.
		attachAuthFailHook := func(td *core.TunnelDialer) {
			ctrlURL := td.ServerURL()
			td.SetAuthFailHook(func() {
				pool := dialerPool
				if pool == nil {
					return
				}
				core.Log.Printf("tunnel: repeated login failures on %s  -  evicting, switching to standby", ctrlURL)
				router.RecordControlFlap(hostNameOf(ctrlURL) + ":443")
				if pool.Size() > 1 {
					pool.Evict(td)
				}
				startSilentRefresh()
			})
		}

		// startSilentRefresh tries to rebuild the dialer pool without touching
		// TUN, routes, DoH, or the UI indicator.  Only one instance runs at a
		// time.  After 2 minutes of failure it escalates to TriggerReconnect.
		startSilentRefresh = func() {
			if !atomic.CompareAndSwapInt32(&silentRefreshActive, 0, 1) {
				return // already refreshing
			}
			capturedStop := poolRefreshStop // capture channel value, not variable
			go func() {
				defer atomic.StoreInt32(&silentRefreshActive, 0)

				deadline := time.Now().Add(2 * time.Minute)
				retryDelay := 5 * time.Second

				core.Log.Printf("tunnel: silent path refresh started (2 min deadline)")

				for time.Now().Before(deadline) {
					// Probe controls and rebuild.
					// Probe immediately on first attempt; sleep between retries.
					// Skipping the initial wait cuts ~5 s off the outage window.
					router.ProbeDataPlane(5 * time.Second)
					router.BuildPaths()

					viable := buildViableAddrsTopup(router.QualifyingControlAddrs())
					var freshDialers []*core.TunnelDialer
					for _, addr := range viable {
						ctrlURL := ensureHTTPS(addr)
						a := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
						a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
						a.SetKeyAuth(savedKey)
						if err := a.Login(); err == nil {
							td, err := router.NewControlDialer(addr, a)
							if err != nil {
								core.Log.Printf("connect: silent refresh: dialer for %s failed (%v)", addr, err)
								continue
							}
							wired := wireDialer(td)
							attachDataFailHook(wired)
							attachUDPFailedHook(wired)
							attachAuthFailHook(wired)
							freshDialers = append(freshDialers, td)
						}
					}

					if len(freshDialers) > 0 {
						dialerPool.Swap(freshDialers)
						core.Log.Printf("tunnel: silent path refresh succeeded  -  pool=%d", len(freshDialers))
						core.SetTunnelHealthy(true)
						return
					}

					core.Log.Printf("tunnel: silent refresh attempt failed  -  retry in %v", retryDelay)
					select {
					case <-capturedStop:
						return // disconnect was requested  -  stop quietly
					case <-time.After(retryDelay):
					}
					if retryDelay < 30*time.Second {
						retryDelay *= 2
					}
				}

				// 2 minutes exhausted  -  fall back to full reconnect.
				core.Log.Printf("tunnel: silent refresh timed out  -  triggering full reconnect")
				if trayApp != nil {
					trayApp.TriggerReconnect()
				}
			}()
		}

		core.Log.Printf("connect: attaching fail hooks")
		// Attach fail hooks to every pool dialer.
		for _, td := range initialPoolDialers {
			attachDataFailHook(td)
			attachUDPFailedHook(td)
			attachAuthFailHook(td)
		}

		core.Log.Printf("connect: starting SOCKS5")
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("SOCKS5 listen: %w", err)
		}
		socksLn = ln
		core.Log.Printf("SOCKS5 listening on %s", ln.Addr())

		core.Log.Printf("connect: starting TUN")
		tun = core.NewTUNBridge(ln.Addr().String())
		if err := tun.Start(); err != nil {
			core.Log.Printf("ERROR: TUN start: %v", err)
			socksLn.Close()
			socksLn = nil
			return fmt.Errorf("TUN: %w", err)
		}

		core.Log.Printf("connect: applying routes effectiveURL=%s", effectiveURL)
		routes = snwin.NewRouteManager()
		if err := routes.Apply(hostOf(effectiveURL), core.TUNAddr); err != nil {
			core.Log.Printf("ERROR: routes: %v", err)
			tun.Stop()
			tun = nil
			socksLn.Close()
			socksLn = nil
			return fmt.Errorf("routing: %w", err)
		}
		core.Log.Println("connected: TUN up, routes applied")

		// Point the TUN adapter's own DNS at 1.1.1.1 unconditionally, before
		// the DoH branch below. Without this the adapter has no DNS server of
		// its own and Windows silently keeps resolving via the physical NIC,
		// bypassing the tunnel for every query regardless of DoH or routing
		// (see EnsureTunnelDNS's doc comment). ConfigureDoH/ConfigureDoHFallback
		// build on top of this when DoH is enabled.
		if err := snwin.EnsureTunnelDNS(); err != nil {
			core.Log.Printf("warn: point TUN DNS at 1.1.1.1: %v", err)
		}

		// Add bypass routes for all known control IPs beyond effectiveURL.
		// TunnelDialers must reach any control directly (no TUN) to avoid
		// routing loops and to allow re-auth during silent path refresh.
		for _, ctrlAddr := range allCtrlAddrs {
			if ips, err := net.LookupHost(hostNameOf(ctrlAddr)); err == nil {
				for _, ip := range ips {
					routes.AddBypass(ip)
				}
			}
		}

		// Persist connection state so the watchdog can clean up after a crash.
		origGW = routes.OrigGW()
		if err := core.WriteWatchdogState(core.WatchdogState{
			Connected:     true,
			OrigGW:        origGW,
			MainPID:       os.Getpid(),
			TunnelHealthy: true,
		}); err != nil {
			core.Log.Printf("warn: write watchdog state (connected): %v", err)
		}

		// Decoy traffic: fire background HTTPS GETs that bypass the TUN
		// (bound to the physical NIC via routes.LocalAddr()) so they never
		// consume tunnel bandwidth.
		decoyMgr = core.NewDecoyManager(routes.LocalAddr())
		dialer.SetActivityHook(decoyMgr.MarkActivity)
		decoyMgr.Start()

		// DoH: configure when the user has it enabled (default: on).
		// Routes DNS through the tunnel via HTTPS so Russian ISPs can't inject
		// domestic CDN IPs (e.g. Meta  ->  Selectel/MTS) in DNS responses.
		if trayApp == nil || trayApp.IsDNSOverHTTPSEnabled() {
			go func() {
				core.Log.Printf("connect: configuring DoH")
				if err := snwin.ConfigureDoH(); err != nil {
					core.Log.Printf("warn: netsh DoH unavailable (%v)  -  starting local DoH proxy", err)
					if p, perr := snwin.ConfigureDoHFallback(); perr != nil {
						core.Log.Printf("warn: DoH proxy failed: %v  -  DNS stays plain-UDP, still tunneled", perr)
					} else {
						dohMu.Lock()
						dohProxy = p
						dohMu.Unlock()
					}
				} else {
					dohMu.Lock()
					dohNetshActive = true
					dohMu.Unlock()
				}
			}()
		}

		// Configure bypass manager (started during country pre-detection above).
		// Now that routes are up, set localIP so bypass dials bind to the right NIC.
		// Re-set token in case relay selection changed the active dialer.
		if bypassMgr != nil {
			bypassMgr.SetLocalIP(routes.LocalAddr())
			bypassMgr.SetToken(dialer.Token())
		}
		core.Log.Printf("connect: starting SOCKS5 server")
		socks5 = core.NewSOCKS5ServerWithPool("", dialerPool, bypassMgr)
		// Use the user's explicit toggle; fall back to CC-based default if no choice saved yet.
		if trayApp != nil {
			socks5.BlockQUIC = trayApp.IsBlockQUICEnabled()
		} else if bypassMgr != nil {
			cc := bypassMgr.Country()
			socks5.BlockQUIC = cc == "RU" || cc == "CN"
		}
		if socks5.BlockQUIC {
			core.Log.Printf("connect: QUIC (UDP:443) blocked (blockQUIC=%v)", socks5.BlockQUIC)
		}
		// Trial (2026-08-13, rolled out to all desktop clients 2026-08-12):
		// dedicated native-UDP dialer for general (non-DNS) UDP ASSOCIATE
		// traffic -- voice/video call media, games, anything not otherwise
		// DNS or bypassed. See the full rationale in socks5.RealtimeUDPDialer's
		// doc comment (snc/core/socks5.go) and dialerFor's use of it
		// (snc/core/udp_assoc.go). Best-effort -- normal pool-based UDP relay
		// (today's behavior) is exactly what happens if this fails, nothing
		// blocks on it.
		if udpConn, uerr := core.NewUDPControlConn(strings.TrimPrefix(effectiveURL, "https://")); uerr == nil {
			socks5.RealtimeUDPDialer = core.NewUDPRelayDialer(udpConn, dialer.Auth())
			core.Log.Printf("connect: realtime UDP trial dialer ready via %s", effectiveURL)
		} else {
			core.Log.Printf("connect: realtime UDP trial dialer unavailable (%v) -- falling back to pool", uerr)
		}
		go socks5.Serve(core.WrapWithTorrentFilter(socksLn)) //nolint:errcheck
		core.Log.Printf("connect: SOCKS5 server started")

		// Pool management: RTT-based promotion + drain completion every 10 s.
		// Replaces the old disruptive 60 s wholesale rebuild  -  working dialers
		// are never forcibly replaced; the pool only grows (refill) or shrinks
		// (eviction via data-fail hook).
		if poolRefreshStop != nil {
			close(poolRefreshStop)
		}
		poolRefreshStop = make(chan struct{})
		dialerPool.StartManagement(10*time.Second, poolRefreshStop)

		// Refill: when the pool drops below the number of qualifying controls,
		// probe and authenticate missing controls and Add() them as standbys.
		// Only adds; never replaces or disrupts working dialers.
		capturedRefillStop := poolRefreshStop
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-capturedRefillStop:
					return
				case <-t.C:
				}
				target := len(router.QualifyingControlAddrs())
				if !dialerPool.NeedsRefill(target) {
					continue
				}
				core.Log.Printf("connect: pool below target (%d/%d)  -  refilling",
					dialerPool.Size(), target)
				// Re-probe to get a fresh picture before adding new dialers.
				router.ProbeDataPlane(5 * time.Second)
				router.BuildPaths()
				for _, addr := range buildViableAddrs(router.QualifyingControlAddrs()) {
					ctrlURL := ensureHTTPS(addr)
					if dialerPool.Has(ctrlURL) {
						continue
					}
					a := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
					a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
					a.SetKeyAuth(savedKey)
					if err := a.Login(); err != nil {
						core.Log.Printf("connect: refill: auth to %s failed: %v", addr, err)
						continue
					}
					td, err := router.NewControlDialer(addr, a)
					if err != nil {
						core.Log.Printf("connect: refill: dialer for %s failed: %v", addr, err)
						continue
					}
					wired := wireDialer(td)
					attachDataFailHook(wired)
					attachUDPFailedHook(wired)
					attachAuthFailHook(wired)
					td.SetLoadFactor(router.LoadFactorFor(addr))
					dialerPool.Add(td)
				}
			}
		}()

		// Refresh load-balancing bonus/malus coefficients every 5 minutes --
		// matches the arbiter's own recompute cadence (see
		// snc-arbiter/load_factor.go), so polling faster would just re-read
		// the same value. Reads globalDisc directly rather than the
		// discoveredLoadFactors mirror above: that mirror only refreshes
		// inside the discovery onChange callback, which fires on control-LIST
		// changes, not on every manifest fetch -- load factors need the
		// latter. Pushes into both the router (nodeScore/scorePath, used when
		// the pool is next rebuilt) and every dialer already in the live pool
		// (DialerPool.pickWeight, used for every in-flight connection right
		// now) so an already-connected session's traffic share adapts
		// without needing a reconnect.
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-capturedRefillStop:
					return
				case <-t.C:
				}
				if globalDisc == nil {
					continue
				}
				lf := globalDisc.LoadFactors()
				if len(lf) == 0 {
					continue
				}
				router.SetLoadFactors(lf)
				for _, addr := range router.QualifyingControlAddrs() {
					if td := dialerPool.Get(ensureHTTPS(addr)); td != nil {
						td.SetLoadFactor(router.LoadFactorFor(addr))
					}
				}
			}
		}()

		// Data-plane watchdog: check every 5 s whether data has flowed through the
		// pool recently.  On Windows, idle periods with no user traffic are normal
		// (no keep-alive POSTs), so the stale threshold is set to 30 s  -  long
		// enough to survive normal browsing pauses but short enough to catch a
		// genuinely dead tunnel (DPI block, exit failure) well before the 60 s
		// periodic rebuild.
		const watchdogStale = 30 * time.Second
		capturedWatchdogStop := poolRefreshStop
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-capturedWatchdogStop:
					return
				case <-ticker.C:
					last := dialerPool.LastDataTime()
					if !last.IsZero() && time.Since(last) > watchdogStale {
						core.Log.Printf("tunnel: watchdog: no data for %s  -  forcing silent refresh",
							time.Since(last).Round(time.Second))
						startSilentRefresh()
					}
				}
			}
		}()

		// Auto-start relay after connect.
		// Public-IP relay only when the control-visible IP matches the local
		// physical NIC â€” i.e. no NAT.  All other cases use the yamux NAT relay.
		capturedBypassMgr := bypassMgr // capture for goroutines below

		// Log upload: ship recent logs straight to the arbiter (navlink.net)
		// over the live tunnel every 5 minutes -- see log_upload.go for why
		// (removed the control/exit relay hop, which saw plaintext content).
		if logUploader != nil {
			logUploader.Stop()
		}
		logUploader = core.NewLogUploader(nodeID, "windows")
		logUploader.Start(
			func() *core.TunnelDialer {
				if dialerPool == nil {
					return nil
				}
				return dialerPool.Pick()
			},
		)

		// Connection-stats upload: same channel/cadence as log upload above,
		// separate endpoint -- see core.ConnStatsUploader. connStatsCollector
		// itself lives for the whole process (declared once near router at
		// the top of main), only the uploader is recreated per connect.
		if connStatsUploader != nil {
			connStatsUploader.Stop()
		}
		connStatsUploader = core.NewConnStatsUploader(connStatsCollector, dialerPool, router, nodeID, "windows", savedKey.Username)
		connStatsUploader.Start(
			func() *core.TunnelDialer {
				if dialerPool == nil {
					return nil
				}
				return dialerPool.Pick()
			},
		)

		// Propagate client country to router once bypass CIDR data is loaded,
		// then re-check every 5 minutes.  If the country changes mid-session
		// (unlikely but possible), trigger a reconnect so the next connection
		// picks the correct regional control.
		stop := make(chan struct{})
		countryCheckerStop = stop
		capturedAppDataDir := appDataDir
		go func() {
			if capturedBypassMgr == nil {
				return
			}
			applyCountry := func(cc string) {
				router.SetMyCountry(cc)
				discoveredMu.Lock()
				prev := lastKnownCountry
				lastKnownCountry = cc
				discoveredMu.Unlock()
				saveCountry(capturedAppDataDir, cc)
				if dialer != nil {
					dialer.SetClientCC(cc)
				}
				if prev != "" && prev != cc {
					core.Log.Printf("router: country changed %q  ->  %q  -  triggering reconnect", prev, cc)
					if trayApp != nil {
						trayApp.TriggerReconnect()
					}
				} else {
					// First detection: rebuild paths now so in-country controls are
					// selected immediately, without waiting for the 60 s pool refresh.
					core.Log.Printf("router: my region set to %q  -  rebuilding paths", cc)
					router.BuildPaths()
					if dialerPool != nil {
						dialerPool.Swap(buildDialerSlice(buildViableAddrs(router.QualifyingControlAddrs())))
						// If some secondary controls failed auth (e.g. firewall settling
						// after TUN came up), retry every 15 s until the pool is full.
						if dialerPool.Size() < len(router.QualifyingControlAddrs()) {
							go func() {
								t := time.NewTicker(15 * time.Second)
								defer t.Stop()
								for {
									select {
									case <-stop:
										return
									case <-t.C:
									}
									if dialerPool == nil {
										return
									}
									dialerPool.Swap(buildDialerSlice(buildViableAddrs(router.QualifyingControlAddrs())))
									if dialerPool.Size() >= len(router.QualifyingControlAddrs()) {
										core.Log.Printf("connect: pool complete after retry")
										return
									}
								}
							}()
						}
					}
				}
			}
			// Initial detection: poll until bypass data is ready (up to ~30 s).
			// GPS-detected or user-selected region takes precedence over CIDR.
			discoveredMu.RLock()
			skipCIDRInit := gpsDetected || settings.PreferredRegion != ""
			discoveredMu.RUnlock()
			if skipCIDRInit {
				core.Log.Printf("router: skipping CIDR initial detection (gpsDetected=%v preferredRegion=%q)", gpsDetected, settings.PreferredRegion)
			} else {
				for i := 0; i < 15; i++ {
					select {
					case <-stop:
						return
					case <-time.After(2 * time.Second):
					}
					if cc := capturedBypassMgr.Country(); cc != "" {
						applyCountry(cc)
						break
					}
				}
			}
			// Periodic recheck every 5 minutes while connected.
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					discoveredMu.RLock()
					skipCIDR := gpsDetected || settings.PreferredRegion != ""
					discoveredMu.RUnlock()
					if skipCIDR {
						continue
					}
					if cc := capturedBypassMgr.Country(); cc != "" {
						discoveredMu.RLock()
						current := lastKnownCountry
						discoveredMu.RUnlock()
						if cc != current {
							applyCountry(cc)
						}
					}
				}
			}
		}()

		connStatsCollector.IncConnect(!autoReconnect)
		return nil
	}

	onDisconnect := func(autoReconnect bool) {
		connStatsCollector.IncDisconnect(!autoReconnect)
		if autoReconnect {
			core.Log.Println("disconnecting... (auto-reconnect by code)")
		} else {
			core.Log.Println("disconnecting... (user-initiated)")
		}
		// Clear the connected flag so the watchdog knows no cleanup is needed
		// if main exits cleanly after this point.
		if err := core.WriteWatchdogState(core.WatchdogState{MainPID: os.Getpid()}); err != nil {
			core.Log.Printf("warn: write watchdog state (disconnect): %v", err)
		}
		router.CloseAllUDPPeers()
		router.SetMyCountry("") // clear region filter so next connect re-evaluates
		// Stop the country checker (keeps lastKnownCountry and discoveredRegions
		// intact so the next connect can route correctly immediately).
		if countryCheckerStop != nil {
			close(countryCheckerStop)
			countryCheckerStop = nil
		}
		// Stop the DHT -> router merger and relay-announce loop (DHT itself
		// keeps running to maintain the registry).
		if dhtMergerStop != nil {
			close(dhtMergerStop)
			dhtMergerStop = nil
		}
		if relayAnnounceStop != nil {
			close(relayAnnounceStop)
			relayAnnounceStop = nil
		}
		if poolRefreshStop != nil {
			close(poolRefreshStop)
			poolRefreshStop = nil
		}
		if decoyMgr != nil {
			decoyMgr.Stop()
			decoyMgr = nil
		}
		dohMu.Lock()
		proxy := dohProxy
		netshWasActive := dohNetshActive
		dohProxy = nil
		dohNetshActive = false
		dohMu.Unlock()
		if proxy != nil {
			snwin.StopDoHFallback(proxy)
		} else if netshWasActive {
			snwin.RestoreDoH()
		}
		origGW = ""
		if bypassMgr != nil {
			bypassMgr.Stop()
			bypassMgr = nil
		}
		if logUploader != nil {
			logUploader.Stop()
			logUploader = nil
		}
		if routes != nil {
			routes.Restore()
			routes = nil
		}
		if tun != nil {
			tun.Stop()
			tun = nil
		}
		if socksLn != nil {
			socksLn.Close()
			socksLn = nil
			socks5 = nil
		}
		if !autoReconnect {
			removeOutboundFirewallRule()
		}
		core.Log.Println("disconnected")
	}

	autoConnect := savedKey != nil // connect on startup whenever a valid key is present

	// Background update checker  -  runs for the lifetime of the app, independent
	// of connect/disconnect cycles.  Uses discovered controls when available,
	// falls back to key nodes.  Notifies the tray when a binary is ready.
	upd := core.NewUpdater(func() []string {
		discoveredMu.RLock()
		dc := append([]string{}, discoveredControls...)
		discoveredMu.RUnlock()
		if len(dc) > 0 {
			urls := make([]string, len(dc))
			for i, n := range dc {
				urls[i] = ensureHTTPS(n)
			}
			return urls
		}
		if savedKey != nil {
			nodes := savedKey.Nodes()
			urls := make([]string, len(nodes))
			for i, n := range nodes {
				urls[i] = ensureHTTPS(n)
			}
			return urls
		}
		return nil
	})
	upd.IsInstallerManaged = snwin.IsInstalledByInstaller

	var updatePromptOnce sync.Once
	upd.OnReady = func(v string) {
		if trayApp != nil {
			trayApp.NotifyUpdateReady(v)
		}
		// checkAndDownload() re-runs every 30 min and calls OnReady again each time
		// it still sees remote > core.Version (which stays true on the running
		// process until it actually restarts) -- guard so the messagebox itself
		// only ever appears once per launch; the tray "Update to X" item above
		// stays available for the user to act on later regardless.
		updatePromptOnce.Do(func() {
			go func() {
				if snwin.ShowUpdateAvailableDialog(v) {
					core.ApplyPendingUpdate()
				}
			}()
		})
	}
	upd.Start()

	// Determine initial blockQUIC value: persisted user choice overrides CC-based default.
	initBlockQUIC := lastKnownCountry == "RU" || lastKnownCountry == "CN"
	if settings.BlockQUIC != nil {
		initBlockQUIC = *settings.BlockQUIC
	}

	core.Log.Println("starting tray")
	trayApp = snwin.NewTrayApp(core.Version, initialLogin, autoConnect, settings.DOHEnabled, initBlockQUIC, settings.PreferredRegion,
		onLogin, onLogout, onConnect, onDisconnect,
		func(enabled bool) {
			settings.DOHEnabled = enabled
			saveClientSettings(appDataDir, settings)
			// Toggle DoH at runtime if currently connected.
			if origGW == "" {
				return // not connected; setting takes effect at next connect
			}
			if enabled {
				dohMu.Lock()
				alreadyActive := dohProxy != nil || dohNetshActive
				dohMu.Unlock()
				if alreadyActive {
					return
				}
				go func() {
					core.Log.Printf("DoH: enabled by user  -  configuring")
					if err := snwin.ConfigureDoH(); err != nil {
						core.Log.Printf("DoH toggle: netsh unavailable (%v)  -  starting local proxy", err)
						if p, perr := snwin.ConfigureDoHFallback(); perr != nil {
							core.Log.Printf("DoH toggle: proxy failed: %v", perr)
						} else {
							dohMu.Lock()
							dohProxy = p
							dohMu.Unlock()
						}
					} else {
						dohMu.Lock()
						dohNetshActive = true
						dohMu.Unlock()
					}
				}()
			} else {
				dohMu.Lock()
				p := dohProxy
				n := dohNetshActive
				dohProxy = nil
				dohNetshActive = false
				dohMu.Unlock()
				go func() {
					// DoH is only ever an encryption choice for DNS -- disabling it
					// must not touch whether DNS uses the tunnel at all. Both
					// StopDoHFallback and RestoreDoH point DNS back at plain UDP
					// while leaving it captured by TUN, never re-adding a bypass
					// route (see the RouteManager doc comment in routes.go for the
					// 2026-08-12 incident this fixes: Google/YouTube/WhatsApp broke
					// for a user after they toggled DoH off, because DNS leaked
					// straight to the ISP instead of staying in the tunnel).
					core.Log.Printf("DoH: disabled by user  -  DNS stays plain-UDP, still tunneled")
					if p != nil {
						snwin.StopDoHFallback(p)
					} else if n {
						snwin.RestoreDoH()
					}
				}()
			}
		},
		func(enabled bool) {
			v := enabled
			settings.BlockQUIC = &v
			saveClientSettings(appDataDir, settings)
			core.Log.Printf("Disable QUIC: %v - applying live", enabled)
			// Apply immediately to the running SOCKS5 server â€” no reconnect needed.
			if socks5 != nil {
				socks5.BlockQUIC = enabled
			}
		},
		func(region string) {
			settings.PreferredRegion = region
			saveClientSettings(appDataDir, settings)
			core.Log.Printf("region: user selected %q", region)
			if region != "" {
				// Explicit region: apply immediately; CIDR detection is now skipped.
				discoveredMu.Lock()
				lastKnownCountry = region
				discoveredMu.Unlock()
				router.SetMyCountry(region)
			}
			// For Auto (region == ""): restore GPS-detected country if available,
			// otherwise keep whatever was persisted; re-applied at next reconnect.
			if region == "" {
				discoveredMu.Lock()
				if gpsCountry != "" {
					lastKnownCountry = gpsCountry
				}
				discoveredMu.Unlock()
			}
			if trayApp != nil {
				trayApp.TriggerReconnect()
			}
		},
	)
	// Consulted by onPowerWake before doing a full reconnect on wake from
	// sleep: if the currently active control still answers, the wake event
	// is treated as spurious and no reconnect happens. Unknown current path
	// (router.Primary() == nil) is treated as unhealthy, since we can't
	// verify anything -- falls back to the previous always-reconnect behavior.
	trayApp.SetHealthCheck(func() bool {
		p := router.Primary()
		if p == nil || p.ControlAddr == "" {
			return false
		}
		return core.QuickHealthCheck(p.ControlAddr, 3*time.Second)
	})
	// Wire trayApp callbacks into the already-running app window.
	appWindow.ConnectFn = func() { trayApp.TriggerConnect() }
	appWindow.DisconnectFn = func() { trayApp.TriggerDisconnect() }
	appWindow.StatusFn = func() snwin.AppStatus { return trayApp.GetAppStatus() }
	appWindow.GetSettingsFn = func() snwin.AppSettings { return trayApp.GetAppSettings() }
	appWindow.SetSettingsFn = func(s snwin.AppSettings) { trayApp.ApplyWindowSettings(s) }
	appWindow.LoginFn = func() { trayApp.TriggerLogin() }
	appWindow.LogoutFn = func() { trayApp.TriggerLogout() }
	appWindow.AboutFn = func() { trayApp.ShowAbout() }
	appWindow.UpdateFn = func() { trayApp.TriggerUpdateInstall() }
	appWindow.QuitFn = func() { trayApp.TriggerQuit() }
	appWindow.UpdateReadyFn = func() bool { return trayApp.IsUpdateReady() }
	trayApp.OnUpdateReadyChanged = appWindow.RefreshUpdateState

	trayApp.SetWindowCallback(func() { appWindow.Show() })
	trayApp.SetStatusCallback(func() { appWindow.UpdateStatus(trayApp.GetAppStatus()) })
	appWindow.Start()

	trayApp.Run()

	appWindow.Destroy()

	// Tray has exited (user clicked Quit).  onDisconnect was called by the tray
	// before returning, so routes and DoH are already restored.
	// Stop the watchdog monitor goroutine so it does not restart the watchdog.
	close(stopWatchdogMonitor)
	// Signal the watchdog that this is a clean exit  -  do not restart main.
	snwin.SignalCleanShutdown()
	core.Log.Println("clean shutdown signaled")
}

// hostOf strips scheme and path from a URL, returning "host" or "host:port".
func hostOf(serverURL string) string {
	s := strings.TrimPrefix(serverURL, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// hostNameOf is like hostOf but also strips the port, returning only the
// hostname.  Use when appending ":443" explicitly to avoid "host:443:443".
func hostNameOf(serverURL string) string {
	h := hostOf(serverURL)
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// clientSettings holds user preferences persisted across sessions.
type clientSettings struct {
	DOHEnabled      bool   `json:"doh_enabled"`
	BlockQUIC       *bool  `json:"block_quic,omitempty"`       // nil = use CC-based default (RU/CN); explicit = user override
	PreferredRegion string `json:"preferred_region,omitempty"` // "" = Auto; "RU"/"EU"/"US"/"CN"/"XX"
}

func loadClientSettings(dir string) clientSettings {
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return clientSettings{DOHEnabled: true} // default: DoH on for new installs
	}
	var s clientSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return clientSettings{DOHEnabled: true}
	}
	return s
}

func saveClientSettings(dir string, s clientSettings) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	os.MkdirAll(dir, 0700)                                        //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "settings.json"), data, 0600) //nolint:errcheck
}

// obtainActivationKey runs the "do you have a key?" flow: if the user has
// one, it's the existing key-entry dialog (with a "Log In Instead" button
// when navlink.net is reachable); if not, and navlink.net is reachable, it's
// a direct (non-tunneled) email/password login against navlink.net that
// automatically issues a key. Returns the raw activation-key string exactly
// as if it had been typed into the key dialog — callers should feed it into
// core.ParseKeyString unchanged.
func obtainActivationKey() (string, error) {
	nc := navlinkauth.New()
	ctx := context.Background()
	reachable := nc.Probe(ctx)

	hasKey, answered := snwin.ShowHaveKeyPrompt()
	if !answered {
		return "", fmt.Errorf("cancelled")
	}

	if hasKey {
		if !reachable {
			keyStr, ok := snwin.ShowKeyDialog()
			if !ok || keyStr == "" {
				return "", fmt.Errorf("cancelled")
			}
			return keyStr, nil
		}
		keyStr, ok, wantsLogin := snwin.ShowKeyDialogWithLogin()
		if wantsLogin {
			return navlinkLoginFlow(nc, ctx)
		}
		if !ok || keyStr == "" {
			return "", fmt.Errorf("cancelled")
		}
		return keyStr, nil
	}

	// No key: without navlink.net reachable, login can never succeed, so go
	// straight to manual key entry (no Login button — it wouldn't work).
	if !reachable {
		keyStr, ok := snwin.ShowKeyDialog()
		if !ok || keyStr == "" {
			return "", fmt.Errorf("cancelled")
		}
		return keyStr, nil
	}
	return navlinkLoginFlow(nc, ctx)
}

// navlinkLoginFlow prompts for navlink.net credentials, retrying on
// authentication failure, until the user either succeeds, cancels, or
// switches to manual key entry via "I Have a Key".
func navlinkLoginFlow(nc *navlinkauth.Client, ctx context.Context) (string, error) {
	for {
		email, password, ok, wantsKeyMode := snwin.ShowLoginDialog()
		if wantsKeyMode {
			keyStr, ok := snwin.ShowKeyDialog()
			if !ok || keyStr == "" {
				return "", fmt.Errorf("cancelled")
			}
			return keyStr, nil
		}
		if !ok {
			return "", fmt.Errorf("cancelled")
		}
		if err := nc.Login(ctx, email, password); err != nil {
			snwin.ShowError("Could not log in:\n\n" + err.Error())
			continue
		}
		keyStr, _, _, err := nc.FreeKey(ctx)
		if err != nil {
			snwin.ShowError("Logged in, but could not get a key:\n\n" + err.Error())
			continue
		}
		return keyStr, nil
	}
}

func ensureHTTPS(u string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "https://" + u
	}
	return u
}

// waitForCountry polls bm.Country() until it returns a non-empty value or
// timeout elapses.  Returns the country code, or "" on timeout.
func waitForCountry(bm *core.BypassManager, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cc := bm.Country(); cc != "" {
			return cc
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}

// loadOrCreateDeviceID reads the device UUID from appDataDir/device_id, or
// generates and persists a new one.  The device_id is a stable per-device
// identifier sent to the arbiter to enforce single-device key binding.
func loadOrCreateDeviceID(appDataDir string) string {
	path := filepath.Join(appDataDir, "device_id")
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id
		}
	}
	id := uuid.New().String()
	if err := os.WriteFile(path, []byte(id), 0600); err != nil {
		core.Log.Printf("warn: save device_id: %v", err)
	}
	return id
}

// saveCountry persists the client's ISO country code to appDataDir/country.txt.
func saveCountry(dir, cc string) {
	if err := os.WriteFile(filepath.Join(dir, "country.txt"), []byte(cc), 0600); err != nil {
		core.Log.Printf("warn: save country: %v", err)
	}
}

// loadCountry reads the persisted country code from appDataDir/country.txt.
// Returns "" if the file does not exist or cannot be read.
func loadCountry(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "country.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// manifestCacheFile returns the path where the control-node manifest is cached.
func manifestCacheFile() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "SNC", "manifest.json")
}

func notifSeenFile() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "SNC", "notifications_seen.json")
}

func loadNotifSeen() map[string]bool {
	data, err := os.ReadFile(notifSeenFile())
	if err != nil {
		return map[string]bool{}
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return map[string]bool{}
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func saveNotifSeen(seen map[string]bool) {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	data, _ := json.Marshal(ids)
	_ = os.MkdirAll(filepath.Dir(notifSeenFile()), 0700)
	_ = os.WriteFile(notifSeenFile(), data, 0600)
}

// showUserNotifications displays per-user notifications received from the arbiter,
// deduplicating by ID using the same seen-file as broadcast notifications.
func showUserNotifications(notifs []core.Notification) {
	seen := loadNotifSeen()
	now := time.Now().Unix()
	var msgs []string
	for _, n := range notifs {
		if seen[n.ID] {
			continue
		}
		if now-n.CreatedAt > 24*3600 {
			continue
		}
		msgs = append(msgs, n.Message)
		seen[n.ID] = true
	}
	if len(msgs) == 0 {
		return
	}
	saveNotifSeen(seen)
	snwin.ShowNotification(msgs)
}

// ensureSingleInstance guarantees this process is the only running main instance.
// If another instance holds the named mutex, it is terminated so the new binary
// can take over.  Before killing the old process the watchdog is signaled via
// SNCUpdateRestart so it treats the handoff as an orderly upgrade rather than a
// crash â€” skipping network cleanup and attaching to the new PID instead of
// launching yet another copy.
// Returns true when this process has successfully acquired the mutex.
func ensureSingleInstance() bool {
	name, _ := syscall.UTF16PtrFromString("Global\\ShortnerdcatSingleInstance")
	kernel32 := syscall.MustLoadDLL("kernel32.dll")
	createMutex := kernel32.MustFindProc("CreateMutexW")
	closeHandle := kernel32.MustFindProc("CloseHandle")
	const errAlreadyExists = syscall.Errno(183)

	h, _, err := createMutex.Call(0, 1, uintptr(unsafe.Pointer(name)))
	if err != errAlreadyExists {
		// Mutex created fresh â€” we are the sole instance; handle intentionally leaked.
		return true
	}
	// Another instance holds the mutex.  Close our duplicate reference so we can
	// retry CreateMutex cleanly after the old process releases it.
	closeHandle.Call(h)

	core.Log.Printf("another instance is running â€” terminating it")

	// Signal the watchdog: this is an orderly upgrade, not a crash.
	// The watchdog will skip networking cleanup and attach to our PID instead of
	// launching another copy of the binary.
	snwin.SignalUpdateRestart()

	// Terminate the old instance using the PID from the watchdog state file.
	st := core.ReadWatchdogState()
	if st.MainPID != 0 && st.MainPID != os.Getpid() {
		openProcess := kernel32.MustFindProc("OpenProcess")
		waitForSingle := kernel32.MustFindProc("WaitForSingleObject")
		terminate := kernel32.MustFindProc("TerminateProcess")
		const procRights = uintptr(0x0001 | 0x00100000) // PROCESS_TERMINATE | SYNCHRONIZE
		ph, _, _ := openProcess.Call(procRights, 0, uintptr(st.MainPID))
		if ph != 0 {
			terminate.Call(ph, 1)
			waitForSingle.Call(ph, 5000) // wait up to 5 s for clean exit
			closeHandle.Call(ph)
		}
	}

	// Retry â€” old instance has released the mutex.
	time.Sleep(200 * time.Millisecond)
	h2, _, err2 := createMutex.Call(0, 1, uintptr(unsafe.Pointer(name)))
	_ = h2 // intentionally leaked; OS releases on exit
	if err2 == errAlreadyExists {
		core.Log.Printf("ensureSingleInstance: could not acquire mutex after kill")
		return false
	}
	core.Log.Printf("ensureSingleInstance: old instance terminated, mutex acquired")
	return true
}

// isAdmin returns true when the current process has administrator privileges.
func isAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	return err == nil
}

// relaunchAsAdmin triggers a UAC prompt to re-run the binary elevated.
// Optional args are passed as command-line parameters to the new process.
func relaunchAsAdmin(args ...string) {
	exe, _ := os.Executable()
	shell32 := syscall.MustLoadDLL("shell32.dll")
	shellEx := shell32.MustFindProc("ShellExecuteW")
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	var params uintptr
	if len(args) > 0 {
		p, _ := syscall.UTF16PtrFromString(strings.Join(args, " "))
		params = uintptr(unsafe.Pointer(p))
	}
	shellEx.Call(0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		params, 0, 1) // SW_SHOWNORMAL
}
