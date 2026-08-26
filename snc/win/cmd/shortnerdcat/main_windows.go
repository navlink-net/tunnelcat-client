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

	"github.com/anacrolix/torrent/metainfo"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
	"shortnerdcat/snc/shared/keymigrate"
	"shortnerdcat/snc/shared/navlinkauth"
	snwin "shortnerdcat/snc/win/windows"
	"tunnel_cat/binlog"
	"tunnel_cat/dht"
	"tunnel_cat/logevent"
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

	// Clean up leftover artifacts from any earlier interrupted update attempt
	// FIRST, unconditionally, before either applying a new pending update or
	// checking single-instance below. Previously this ran only after the
	// single-instance check succeeded (see the 0c block) -- but the relaunch
	// in applyClientSelfReplace starts its child and calls os.Exit(0) in the
	// parent essentially concurrently, so the child's single-instance check
	// can race the OS actually releasing the parent's mutex. A child that
	// loses that race returns at the single-instance guard below and NEVER
	// reached UpdateCleanup, permanently stranding that update's .old file --
	// this is what let it survive 12 days in the original 2026-08-10
	// incident, and recurred live on 2026-08-22. Running cleanup here means
	// every process that even starts up removes stale artifacts, regardless
	// of which one ends up winning the single-instance race.
	core.UpdateCleanup()

	// 0b. Apply pending update (before mutex so the new process is not blocked).
	core.ApplyPendingUpdate()
	if len(forcedControls) > 0 {
		logevent.Emit(binlog.TagSystem, logevent.EventWinStartupDiag,
			logevent.Str(logevent.AttrStage, logevent.WinStartupDiagStageControlsOverride),
			logevent.Str(logevent.AttrDetail, fmt.Sprintf("%v", forcedControls)))
	}

	// Ã¢"â‚¬Ã¢"â‚¬ 0c. Single-instance guard Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	// CreateMutexW returns ERROR_ALREADY_EXISTS if another instance holds the
	// mutex.  We keep the handle open for the lifetime of the process so the
	// OS releases it automatically on exit.
	if !ensureSingleInstance() {
		logevent.Emit(binlog.TagSystem, logevent.EventWinStartupDiag,
			logevent.Str(logevent.AttrStage, logevent.WinStartupDiagStageSingleInstanceBlocked))
		return
	}

	// Remove any DoH config left by a session that ended without clean disconnect.
	// Must run before auto-connect so DNS is not broken during the connect sequence.
	snwin.CleanupDoH()

	// Register navlink:// URL scheme so the OS can route activation links to this exe.
	snwin.RegisterURLScheme()

	// Ã¢"â‚¬Ã¢"â‚¬ 1b. Watchdog: persist our PID and start the watchdog if not running Ã¢"â‚¬Ã¢"â‚¬
	// Write PID so the watchdog can attach to us if it starts after we do.
	if err := core.WriteWatchdogState(core.WatchdogState{MainPID: os.Getpid()}); err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinWatchdogLifecycle,
			logevent.Str(logevent.AttrStage, logevent.WinWatchdogLifecycleStageStateWriteFailed),
			logevent.Str(logevent.AttrErr, err.Error()))
	}
	// Start the watchdog process if it is not already running.  The watchdog
	// advertises its presence via a named mutex; no-op if already up.
	var watchdogProc *os.Process
	if !snwin.WatchdogRunning() {
		if wp, err := snwin.StartWatchdog(); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinWatchdogLifecycle,
				logevent.Str(logevent.AttrStage, logevent.WinWatchdogLifecycleStageStartFailed),
				logevent.Str(logevent.AttrErr, err.Error()))
		} else {
			watchdogProc = wp
			logevent.Emit(binlog.TagSystem, logevent.EventWinWatchdogLifecycle,
				logevent.Str(logevent.AttrStage, logevent.WinWatchdogLifecycleStageStarted),
				logevent.Int(logevent.AttrPid, int64(wp.Pid)))
		}
	} else {
		logevent.Emit(binlog.TagSystem, logevent.EventWinWatchdogLifecycle,
			logevent.Str(logevent.AttrStage, logevent.WinWatchdogLifecycleStageAlreadyRunning))
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinWatchdogLifecycle,
						logevent.Str(logevent.AttrStage, logevent.WinWatchdogLifecycleStageExitedRestarting))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinWatchdogLifecycle,
					logevent.Str(logevent.AttrStage, logevent.WinWatchdogLifecycleStageRestartFailed),
					logevent.Str(logevent.AttrErr, err.Error()))
				watchdogProc = nil
			} else {
				watchdogProc = wp
				logevent.Emit(binlog.TagSystem, logevent.EventWinWatchdogLifecycle,
					logevent.Str(logevent.AttrStage, logevent.WinWatchdogLifecycleStageRestarted),
					logevent.Int(logevent.AttrPid, int64(wp.Pid)))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinStartupDiag,
				logevent.Str(logevent.AttrStage, logevent.WinStartupDiagStagePanic),
				logevent.Str(logevent.AttrDetail, fmt.Sprintf("%v\n%s", r, debug.Stack())))
		}
	}()

	// Ã¢"â‚¬Ã¢"â‚¬ 2. Splash screen (after UAC, after logging) Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	// appWindow is declared here so the window can be shown immediately after
	// the splash, before the auth sequence completes.
	var appWindow *snwin.AppWindow

	if !watchdogRestart {
		logevent.Emit(binlog.TagSystem, logevent.EventWinStartupDiag,
			logevent.Str(logevent.AttrStage, logevent.WinStartupDiagStageSplashShowing))
		snwin.ShowSplash(core.Version, 5*time.Second)
		logevent.Emit(binlog.TagSystem, logevent.EventWinStartupDiag,
			logevent.Str(logevent.AttrStage, logevent.WinStartupDiagStageSplashDone))
	}

	// Start the app window immediately after the splash so it appears while
	// auth runs in the background.  Callbacks referencing trayApp are wired
	// below after trayApp is created; all callback fields are nil-guarded.
	appWindow = snwin.NewAppWindow()
	appWindow.Start()

	// Ã¢"â‚¬Ã¢"â‚¬ 3. Try auto-login with saved key Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	logevent.Emit(binlog.TagSystem, logevent.EventWinStartupDiag,
		logevent.Str(logevent.AttrStage, logevent.WinStartupDiagStageKeyLoadStart))

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

		// torrentEngine joins this machine into the same swarm the
		// torrent-seed fleet (opentracker/transmission-daemon) already
		// seeds client software + manifest torrents through -- see
		// tunnel_cat/snc/core/torrent.go. Not user-facing, no settings
		// toggle: gated purely by the arbiter's manifest torrent_enabled
		// flag (core.TorrentManifestAllowed()), independent of whether the
		// VPN tunnel itself is connected. Created once at startup;
		// torrentSlotMagnet/torrentSlotHash track the CURRENT magnet/infohash
		// per slot (a software package slug, "manifest", or "versions") --
		// not just an ever-growing set of every magnet ever seen. When a
		// slot's magnet changes (a new client version was published), the
		// old infohash is looked up here and removed (stop seeding + delete
		// its data) before the new one is added, so superseded versions
		// don't sit around seeded forever.
		torrentEngine     *core.TorrentEngine
		torrentOnce       sync.Once
		torrentSlotMagnet = make(map[string]string)
		torrentSlotHash   = make(map[string]metainfo.Hash)
		torrentMu         sync.Mutex
		torrentUpdateOnce sync.Once
		// upd is assigned much later (near the tray/window setup) but
		// referenced from torrentCheckUpdate above -- forward-declared here
		// for the same reason wireTorrent is, so torrentCheckUpdate can fire
		// upd.OnReady without duplicating its update-available UX.
		upd *core.Updater
		// wireTorrent is assigned below (after globalDisc/appDataDir are in
		// scope); declared here as a forward reference so wireDHT's fetch
		// callback (defined earlier in this function) can call it on every
		// manifest refresh, not just once at startup.
		wireTorrent func()

		// topupClient drives the on-demand control-list supplement (see
		// tunnel_cat/snc/core/manifest_topup.go). Recreated whenever savedKey
		// changes (new/refreshed key = new AuthSig to send); read/written
		// under topupMu since both the periodic ticker and onConnect can
		// touch it concurrently.
		topupMu     sync.Mutex
		topupClient *core.TopupClient
	)

	// pickServerURL returns the best server URL to connect to.
	// When --controls is active, always returns the first forced control.
	// Otherwise prefers in-region controls, then falls back to key nodes.
	pickServerURL := func(kd *core.KeyData) string {
		if len(forcedControls) > 0 {
			logevent.Emit(binlog.TagSystem, logevent.EventWinPickServerUrl,
				logevent.Str(logevent.AttrStage, "forced"),
				logevent.Str(logevent.AttrAddr, forcedControls[0]))
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinPickServerUrl,
						logevent.Str(logevent.AttrStage, "in_region"),
						logevent.Str(logevent.AttrAddr, n),
						logevent.Str(logevent.AttrCc, country))
					return ensureHTTPS(n)
				}
			}
			logevent.Emit(binlog.TagSystem, logevent.EventWinPickServerUrl,
				logevent.Str(logevent.AttrStage, "no_in_region"),
				logevent.Str(logevent.AttrAddr, nodes[0]),
				logevent.Str(logevent.AttrCc, country))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinDiscovery,
					logevent.Str(logevent.AttrStage, "control_list_updated"),
					logevent.Str(logevent.AttrControls, fmt.Sprintf("%v", controls)))
			})
			if err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinDiscovery,
					logevent.Str(logevent.AttrStage, "init_failed"),
					logevent.Str(logevent.AttrErr, err.Error()))
				return
			}
			if err := globalDisc.LoadCached(); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinDiscovery,
					logevent.Str(logevent.AttrStage, "no_cached_manifest"),
					logevent.Str(logevent.AttrErr, err.Error()))
			}
			globalDisc.UseAsSNIProvider()
			globalDisc.UseAsFingerprintProvider() // pin control-node certs to the signed manifest (2026-08-07 security fix)
			// Show admin broadcast notifications  -  deduplicated by ID, 24 h TTL.
			globalDisc.SetNotificationCallback(func(notifs []core.Notification) {
				logevent.Emit(binlog.TagSystem, logevent.EventWinNotification,
					logevent.Str(logevent.AttrStage, "received"),
					logevent.Int(logevent.AttrCount, int64(len(notifs))))
				now := time.Now().Unix()
				seen := loadNotifSeen()
				var newMsgs []string
				for _, n := range notifs {
					logevent.Emit(binlog.TagSystem, logevent.EventWinNotification,
						logevent.Str(logevent.AttrStage, "item"),
						logevent.Str(logevent.AttrNotifId, n.ID),
						logevent.Int(logevent.AttrCreatedAt, n.CreatedAt),
						logevent.Bool(logevent.AttrSeen, seen[n.ID]),
						logevent.Str(logevent.AttrMsg, n.Message))
					if seen[n.ID] {
						continue
					}
					if now-n.CreatedAt > 24*3600 {
						logevent.Emit(binlog.TagSystem, logevent.EventWinNotification,
							logevent.Str(logevent.AttrStage, "item_expired"),
							logevent.Str(logevent.AttrNotifId, n.ID),
							logevent.Int(logevent.AttrAgeSec, now-n.CreatedAt))
						continue
					}
					newMsgs = append(newMsgs, n.Message)
					seen[n.ID] = true
				}
				if len(newMsgs) == 0 {
					logevent.Emit(binlog.TagSystem, logevent.EventWinNotification,
						logevent.Str(logevent.AttrStage, "none_new"))
					return
				}
				logevent.Emit(binlog.TagSystem, logevent.EventWinNotification,
					logevent.Str(logevent.AttrStage, "showing"),
					logevent.Int(logevent.AttrCount, int64(len(newMsgs))))
				saveNotifSeen(seen)
				go snwin.ShowNotification(newMsgs)
			})
			globalDisc.Start(10 * time.Minute)
			// wireTorrent must be reachable from every initDiscovery call
			// site, not only the ones that also call wireDHT/startDiscovery
			// -- the auto-login/bootstrap path calls initDiscovery directly
			// (see its other call site below) and would otherwise never
			// start the torrent engine or feed it any magnets at all.
			if wireTorrent != nil {
				wireTorrent()
			}
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinClubDiscovery,
					logevent.Str(logevent.AttrStage, "control_list_updated"),
					logevent.Str(logevent.AttrSlug, slug),
					logevent.Str(logevent.AttrControls, fmt.Sprintf("%v", controls)))
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
						logevent.Emit(binlog.TagSystem, logevent.EventWinClubRecommend,
							logevent.Str(logevent.AttrResult, "error"),
							logevent.Str(logevent.AttrUsername, username),
							logevent.Str(logevent.AttrErr, err.Error()))
					} else {
						logevent.Emit(binlog.TagSystem, logevent.EventWinClubRecommend,
							logevent.Str(logevent.AttrResult, "ok"),
							logevent.Str(logevent.AttrUsername, username))
					}
				}
			}

			for _, slug := range []string{"cat_club", "elite_cat_club"} {
				slug := slug
				cd, err := core.NewClubDiscoverer(slug, kd.ArbiterPubkey, tokenFn, func(controls []string) {
					merge(slug, controls)
				})
				if err != nil {
					logevent.Emit(binlog.TagSystem, logevent.EventWinClubDiscovery,
						logevent.Str(logevent.AttrStage, "init_failed"),
						logevent.Str(logevent.AttrSlug, slug),
						logevent.Str(logevent.AttrErr, err.Error()))
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
			if wireTorrent != nil {
				wireTorrent()
			}
		})
		dhtNode.SetManifestHandler(func(raw []byte) {
			if err := globalDisc.InjectRaw(raw); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinDhtGossipRejected,
					logevent.Str(logevent.AttrErr, err.Error()))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinMirror,
					logevent.Str(logevent.AttrStage, "init_failed"),
					logevent.Str(logevent.AttrErr, err.Error()))
				return
			}
			mirrorMgr.LoadCached()
			dhtNode.SetContentHandler(mirrorMgr.OnContentManifest)
			// Serve side: someone else punched to us wanting a chunk we have.
			dhtNode.SetMirrorPunchHandler(func(peerAddr string) {
				conn, err := core.Punch("", peerAddr)
				if err != nil {
					logevent.Emit(binlog.TagSystem, logevent.EventWinMirror,
						logevent.Str(logevent.AttrStage, "server_punch_failed"),
						logevent.Str(logevent.AttrPeer, peerAddr),
						logevent.Str(logevent.AttrErr, err.Error()))
					return
				}
				core.NewMirrorConn(conn, mirrorMgr.ServeChunk)
				logevent.Emit(binlog.TagSystem, logevent.EventWinMirror,
					logevent.Str(logevent.AttrStage, "server_serving"),
					logevent.Str(logevent.AttrPeer, peerAddr))
			})
			logevent.Emit(binlog.TagSystem, logevent.EventWinMirror,
				logevent.Str(logevent.AttrStage, "initialized"),
				logevent.Str(logevent.AttrDataDir, filepath.Join(appDataDir, "mirror")))
		})
	}

	// torrentSyncSlot brings one named slot (a software package slug,
	// "manifest", or "versions") to the given magnet. A no-op if the slot
	// is already on that exact magnet. When the slot WAS on a different
	// magnet (a new version got published), the old torrent is stopped and
	// its data deleted before the new one is added -- superseded versions
	// must not sit around seeded forever (see the "убрать устаревшие
	// раздачи" requirement this implements).
	torrentSyncSlot := func(slot, magnet, label string) {
		if magnet == "" || torrentEngine == nil {
			return
		}
		torrentMu.Lock()
		prevMagnet, hadPrev := torrentSlotMagnet[slot]
		prevHash, hadHash := torrentSlotHash[slot]
		torrentMu.Unlock()
		if hadPrev && prevMagnet == magnet {
			return // unchanged
		}
		newHash, err := torrentEngine.AddMagnet(magnet)
		if err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
				logevent.Str(logevent.AttrSetting, "torrent_add"),
				logevent.Str(logevent.AttrDetail, label),
				logevent.Str(logevent.AttrErr, err.Error()))
			return
		}
		if hadHash {
			if err := torrentEngine.Remove(prevHash, true); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
					logevent.Str(logevent.AttrSetting, "torrent_remove_stale"),
					logevent.Str(logevent.AttrDetail, label),
					logevent.Str(logevent.AttrErr, err.Error()))
			} else {
				logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
					logevent.Str(logevent.AttrSetting, "torrent_remove_stale"),
					logevent.Str(logevent.AttrDetail, label),
					logevent.Str(logevent.AttrStage, "removed"))
			}
		}
		torrentMu.Lock()
		torrentSlotMagnet[slot] = magnet
		torrentSlotHash[slot] = newHash
		torrentMu.Unlock()
		logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
			logevent.Str(logevent.AttrSetting, "torrent_add"),
			logevent.Str(logevent.AttrDetail, label),
			logevent.Str(logevent.AttrStage, "added"))
	}
	// torrentUpdateSlug picks which OTA distributable this client should
	// watch for over torrent, mirroring core.Updater.checkAndDownload's own
	// two-strategy logic exactly (see updater.go's doc comment) so the
	// torrent path and the existing HTTP path always agree on which slug is
	// authoritative for this particular install.
	torrentUpdateSlug := func() string {
		if snwin.IsInstalledByInstaller() {
			return "windows"
		}
		return "windows-installer"
	}
	// torrentCheckUpdate looks at the "versions" torrent (once downloaded)
	// for the update slug's version; if newer than the running build, waits
	// for that slug's own software torrent to finish downloading, extracts
	// it via the same path core.Updater's HTTP flow uses, and fires the
	// exact same upd.OnReady callback -- so the update-available UX (tray
	// notification, dialog, ApplyPendingUpdate) is identical regardless of
	// which channel actually delivered the bytes.
	torrentCheckUpdate := func() {
		if torrentEngine == nil {
			return
		}
		dataDir := filepath.Join(appDataDir, "torrents")
		var versionsDone bool
		for _, it := range torrentEngine.List() {
			if it.HaveInfo && it.Name == "versions.json" && it.Done {
				versionsDone = true
				break
			}
		}
		if !versionsDone {
			return
		}
		raw, err := os.ReadFile(filepath.Join(dataDir, "versions.json"))
		if err != nil {
			return
		}
		var versions map[string]struct {
			Available bool   `json:"available"`
			Version   string `json:"version"`
		}
		if err := json.Unmarshal(raw, &versions); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
				logevent.Str(logevent.AttrSetting, "torrent_update"),
				logevent.Str(logevent.AttrStage, "versions_parse_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
			return
		}
		slug := torrentUpdateSlug()
		entry, ok := versions[slug]
		// Same numeric-YYYYMMDDHHMM comparison convention as
		// core.Updater.checkAndDownload -- version strings sort correctly
		// as plain strings since they're fixed-width zero-padded.
		if !ok || !entry.Available || entry.Version == "" || entry.Version <= core.Version {
			return
		}
		var softwareDone bool
		var softwareName string
		// Match by tracked slot hash directly rather than re-parsing the
		// magnet's btih out of the URI a second time.
		torrentMu.Lock()
		wantHash, haveWant := torrentSlotHash[slug]
		torrentMu.Unlock()
		if !haveWant {
			return
		}
		for _, it := range torrentEngine.List() {
			if it.InfoHash == wantHash.HexString() && it.HaveInfo && it.Done {
				softwareDone = true
				softwareName = it.Name
				break
			}
		}
		if !softwareDone {
			return
		}
		torrentUpdateOnce.Do(func() {
			zipPath := filepath.Join(dataDir, softwareName)
			if err := core.ApplyTorrentDownloadedZip(zipPath, snwin.IsInstalledByInstaller()); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
					logevent.Str(logevent.AttrSetting, "torrent_update"),
					logevent.Str(logevent.AttrStage, "apply_failed"),
					logevent.Str(logevent.AttrErr, err.Error()))
				return
			}
			logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
				logevent.Str(logevent.AttrSetting, "torrent_update"),
				logevent.Str(logevent.AttrStage, "ready"),
				logevent.Str(logevent.AttrDetail, entry.Version))
			if upd != nil && upd.OnReady != nil {
				upd.OnReady(entry.Version)
			}
		})
	}
	torrentCheckMagnets := func() {
		if globalDisc == nil || torrentEngine == nil {
			return
		}
		magnets := globalDisc.TorrentMagnets()
		for slug, m := range magnets {
			torrentSyncSlot(slug, m, "software:"+slug)
		}
		torrentSyncSlot("manifest", globalDisc.ManifestTorrentMagnet(), "manifest")
		torrentCheckUpdate()
	}
	wireTorrent = func() {
		if globalDisc == nil {
			return
		}
		torrentOnce.Do(func() {
			torrentEngine = core.NewTorrentEngine(filepath.Join(appDataDir, "torrents"))
			if err := torrentEngine.Start(); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
					logevent.Str(logevent.AttrSetting, "torrent_engine"),
					logevent.Str(logevent.AttrErr, err.Error()))
				torrentEngine = nil
				return
			}
			logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
				logevent.Str(logevent.AttrSetting, "torrent_engine"),
				logevent.Str(logevent.AttrStage, "started"))
			// globalDisc.TorrentMagnets()/ManifestTorrentMagnet() are only
			// populated after the discoverer's first successful fetch,
			// which may not have completed yet at this exact call site (it
			// races Start()'s own async fetch, and some call paths --
			// e.g. the auto-login/bootstrap path -- never trigger a second
			// call to wireTorrent at all). Poll independently of any
			// specific fetch-completion hook so magnets are picked up
			// whenever they actually arrive, on every code path -- this
			// same loop also re-checks the "versions" torrent for updates
			// once it and the relevant software torrent finish downloading.
			go func() {
				t := time.NewTicker(2 * time.Minute)
				defer t.Stop()
				torrentCheckMagnets()
				for range t.C {
					torrentCheckMagnets()
				}
			}()
		})
		torrentCheckMagnets()
	}

	// startDiscovery is kept for call-site compatibility; it now delegates to
	// initDiscovery (idempotent) + wireDHT + wireMirror + wireTorrent.
	startDiscovery := func(srvURL string, kd *core.KeyData, dhtNode *core.DHTNode) {
		initDiscovery(srvURL, kd)
		wireDHT(dhtNode)
		wireMirror(dhtNode, kd)
		wireTorrent()
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinDeepLink,
						logevent.Str(logevent.AttrResult, "ok"))
				} else {
					logevent.Emit(binlog.TagSystem, logevent.EventWinDeepLink,
						logevent.Str(logevent.AttrResult, "error"),
						logevent.Str(logevent.AttrErr, saveErr.Error()))
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
		kd, err := core.ParseKeyString(keyStr)
		if err == nil && kd.IsLegacy() {
			// Legacy (V1, unsigned) key found on disk: its ControlNodes/
			// Servers list is not verifiable (see snc/shared/keymigrate's
			// doc comment), so it must not be dialed as-is. Migrate first;
			// on failure, treat as if no usable key were on disk at all --
			// do NOT fall through to auto-connecting with the unverified
			// list below.
			core.Log.Printf("startup: legacy V1 key on disk for %s, migrating to V2", kd.Username)
			migCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			newKeyStr, newKD, migErr := keymigrate.Migrate(migCtx, kd)
			cancel()
			if migErr != nil {
				core.Log.Printf("startup: legacy key migration failed, skipping auto-connect: %v", migErr)
				kd, err = nil, fmt.Errorf("legacy key migration failed: %w", migErr)
			} else {
				core.Log.Printf("startup: legacy key migrated OK, key_id=%s", newKD.KeyID)
				if saveErr := snwin.SaveKey(newKeyStr); saveErr != nil {
					core.Log.Printf("startup: could not persist migrated key: %v", saveErr)
				}
				keyStr, kd = newKeyStr, newKD
			}
		}
		if err == nil && len(kd.Nodes()) > 0 {
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
					logevent.Str(logevent.AttrFlow, "auto"),
					logevent.Str(logevent.AttrStage, "trying"),
					logevent.Str(logevent.AttrUser, kd.Username),
					logevent.Str(logevent.AttrUrl, url))
				a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
				a.SetKeyAuth(kd)
				if err := a.Login(); err != nil {
					logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
						logevent.Str(logevent.AttrFlow, "auto"),
						logevent.Str(logevent.AttrStage, "failed"),
						logevent.Str(logevent.AttrUrl, url),
						logevent.Str(logevent.AttrErr, err.Error()))
					return nil
				}
				logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
					logevent.Str(logevent.AttrFlow, "auto"),
					logevent.Str(logevent.AttrStage, "ok"),
					logevent.Str(logevent.AttrUrl, url))
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
						logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
							logevent.Str(logevent.AttrFlow, "auto"),
							logevent.Str(logevent.AttrStage, "relay_trying"),
							logevent.Str(logevent.AttrAddr, relay.Addr),
							logevent.Str(logevent.AttrRelayCc, relay.CountryCode))
						if a := loginURL(url); a != nil {
							logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
								logevent.Str(logevent.AttrFlow, "auto"),
								logevent.Str(logevent.AttrStage, "relay_ok"),
								logevent.Str(logevent.AttrAddr, relay.Addr))
							authOK = true
							applyAuth(a, url)
							break
						}
					}
				}
			}
			if authOK {
			} else {
				logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
					logevent.Str(logevent.AttrFlow, "auto"),
					logevent.Str(logevent.AttrStage, "all_unreachable"))
			}
		} else {
			logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
				logevent.Str(logevent.AttrFlow, "auto"),
				logevent.Str(logevent.AttrStage, "saved_key_invalid"),
				logevent.Str(logevent.AttrErr, err.Error()))
		}
	} else {
		logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
			logevent.Str(logevent.AttrFlow, "auto"),
			logevent.Str(logevent.AttrStage, "no_saved_key"),
			logevent.Str(logevent.AttrErr, err.Error()))
	}
	// Show Connect whenever a key exists  -  even if auto-auth failed.
	// onConnect handles re-auth with saved credentials before dialling.
	initialLogin := savedKey != nil

	// Ã¢"â‚¬Ã¢"â‚¬ 4. System tray (blocks until Quit) Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬

	// Ã¢"â‚¬Ã¢"â‚¬ 4a. NodeID (needed by relay auto-start inside onConnect) Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬Ã¢"â‚¬
	// (appDataDir declared above, before auto-auth, so relay cache is accessible at startup)

	// Load or generate stable device UUID for key-binding enforcement (M4+).
	deviceID = loadOrCreateDeviceID(appDataDir)
	logevent.Emit(binlog.TagSystem, logevent.EventWinDeviceId, logevent.Str(logevent.AttrValue, deviceID))

	nodeID, err := core.LoadOrGenNodeID(appDataDir)
	if err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinNodeId,
			logevent.Str(logevent.AttrStage, "load_failed"),
			logevent.Str(logevent.AttrErr, err.Error()))
		nodeID = "unknown"
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinNodeId,
		logevent.Str(logevent.AttrStage, "loaded"),
		logevent.Str(logevent.AttrValue, nodeID))

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
			logevent.Emit(binlog.TagSystem, logevent.EventWinDht,
				logevent.Str(logevent.AttrStage, "started"),
				logevent.Str(logevent.AttrNodeId, nodeID),
				logevent.Str(logevent.AttrAddr, udpConn.LocalAddr().String()))

			// Relay server: when another client wants to use us as relay, it sends
			// MsgHolePunch with its external addr and the control it wants to reach.
			// We punch back and create a UDPRelayConn that forwards to that control.
			dhtNode.SetHolePunchHandler(func(peerAddr, controlURL string) {
				conn, err := core.Punch("", peerAddr)
				if err != nil {
					logevent.Emit(binlog.TagSystem, logevent.EventWinDht,
						logevent.Str(logevent.AttrStage, "relay_server_punch_failed"),
						logevent.Str(logevent.AttrPeer, peerAddr),
						logevent.Str(logevent.AttrErr, err.Error()))
					return
				}
				relay := core.NewUDPRelayConn(conn, controlURL)
				logevent.Emit(binlog.TagSystem, logevent.EventWinDht,
					logevent.Str(logevent.AttrStage, "relay_server_serving"),
					logevent.Str(logevent.AttrPeer, peerAddr),
					logevent.Str(logevent.AttrControlUrl, controlURL))
				// relay.readLoop runs in the background; when the peer marks it
				// Failed we just close â€” no keepalive from our side needed (client
				// sends pings, we send pongs).
				go func() {
					<-relay.StopCh()
					relay.Close()
				}()
			})
		} else {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDht,
				logevent.Str(logevent.AttrStage, "listen_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
		}
	} else {
		logevent.Emit(binlog.TagSystem, logevent.EventWinDht,
			logevent.Str(logevent.AttrStage, "bad_node_id"),
			logevent.Str(logevent.AttrNodeId, nodeID))
	}

	// Load persisted country so regional routing works correctly from the
	// very first connect after a restart.
	if cc := loadCountry(appDataDir); cc != "" {
		lastKnownCountry = cc
		logevent.Emit(binlog.TagSystem, logevent.EventWinGeo,
			logevent.Str(logevent.AttrStage, "persisted_country_loaded"),
			logevent.Str(logevent.AttrCc, cc))
	}

	// Load user settings early so all goroutines (GPS, country checker) can read
	// PreferredRegion without referencing a not-yet-declared variable.
	settings := loadClientSettings(appDataDir)

	// Explicit region selection overrides persisted/detected country immediately.
	if settings.PreferredRegion != "" {
		lastKnownCountry = settings.PreferredRegion
		router.SetMyCountry(settings.PreferredRegion)
		logevent.Emit(binlog.TagSystem, logevent.EventWinGeo,
			logevent.Str(logevent.AttrStage, "explicit_region_applied"),
			logevent.Str(logevent.AttrCc, settings.PreferredRegion))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinGeo,
					logevent.Str(logevent.AttrStage, "device_country_applied"),
					logevent.Str(logevent.AttrCc, cc))
			} else {
				logevent.Emit(binlog.TagSystem, logevent.EventWinGeo,
					logevent.Str(logevent.AttrStage, "device_country_stored"),
					logevent.Str(logevent.AttrCc, cc),
					logevent.Str(logevent.AttrRegion, settings.PreferredRegion))
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

	// runTopup drives one manifest-topup round (see
	// tunnel_cat/snc/core/manifest_topup.go): while router has fewer than its
	// target live control count, ask navlink.net directly (bypassing TUN,
	// same as the discoverer's navlink fallback) for one more, e2e-probe it,
	// and fold it into router if it's actually reachable. Safe to call
	// whether the tunnel is currently connected or disconnected, and safe to
	// call concurrently from both the periodic ticker and a fresh connect --
	// see TopupClient.Run's doc comment for why overlap is harmless.
	runTopup := func() {
		if savedKey == nil || router == nil {
			return
		}
		topupMu.Lock()
		if topupClient == nil {
			topupClient = core.NewTopupClientFromKey("https://navlink.net", savedKey,
				func() string {
					if routes == nil {
						return ""
					}
					return routes.LocalAddr()
				}, nil)
		}
		tc := topupClient
		topupMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		tc.Run(ctx,
			router.AllControlAddrs,
			func() int { return len(router.QualifyingControlAddrs()) },
			func(addr string) bool { return core.QuickHealthCheck(addr, 5*time.Second) },
			func(addr string) {
				router.AddControl(addr)
				router.BuildPaths()
				core.Log.Printf("manifest-topup: added control %s to router", addr)
			},
		)
	}

	// Manifest topup runs for the whole process lifetime, independent of
	// connect/disconnect (see runTopup's doc comment above) -- on its own
	// 5-minute tick, and also triggered once right after each successful
	// connect (see the onConnect call further down). No stop channel: the
	// process exiting is what stops it, same as dhtNode/connStatsCollector.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runTopup()
		}
	}()

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

		// Legacy (V1, unsigned) key: its ControlNodes/Servers list is not
		// verifiable (see snc/shared/keymigrate's doc comment) -- silently
		// exchange it for a fresh, arbiter-signed V2 key using the
		// credentials it carries, authenticated against navlink.net
		// directly rather than anything derived from the key itself. If
		// this fails, the key is treated as unauthenticated: we do NOT
		// fall back to dialing its own (unverifiable) node list.
		if kd.IsLegacy() {
			core.Log.Printf("login: legacy V1 key detected for %s, migrating to V2", kd.Username)
			migCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			newKeyStr, newKD, migErr := keymigrate.Migrate(migCtx, kd)
			cancel()
			if migErr != nil {
				core.Log.Printf("login: legacy key migration failed: %v", migErr)
				return fmt.Errorf("could not renew your key (please try again or contact support): %w", migErr)
			}
			core.Log.Printf("login: legacy key migrated OK, key_id=%s", newKD.KeyID)
			keyStr = newKeyStr
			kd = newKD
		}

		tryLoginAuth := func(url string) bool {
			logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
				logevent.Str(logevent.AttrFlow, "manual"),
				logevent.Str(logevent.AttrStage, "trying"),
				logevent.Str(logevent.AttrUser, kd.Username),
				logevent.Str(logevent.AttrUrl, url))
			a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
			a.SetKeyAuth(kd)
			a.SetDeviceInfo(kd.KeyID, deviceID, "Windows PC")
			if err := a.Login(); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
					logevent.Str(logevent.AttrFlow, "manual"),
					logevent.Str(logevent.AttrStage, "failed"),
					logevent.Str(logevent.AttrUrl, url),
					logevent.Str(logevent.AttrErr, err.Error()))
				return false
			}
			logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
				logevent.Str(logevent.AttrFlow, "manual"),
				logevent.Str(logevent.AttrStage, "ok"),
				logevent.Str(logevent.AttrUrl, url))
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
						logevent.Str(logevent.AttrFlow, "manual"),
						logevent.Str(logevent.AttrStage, "relay_trying"),
						logevent.Str(logevent.AttrAddr, relay.Addr),
						logevent.Str(logevent.AttrRelayCc, relay.CountryCode))
					if tryLoginAuth(url) {
						logevent.Emit(binlog.TagSystem, logevent.EventWinAuth,
							logevent.Str(logevent.AttrFlow, "manual"),
							logevent.Str(logevent.AttrStage, "relay_ok"),
							logevent.Str(logevent.AttrAddr, relay.Addr))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinKeySaveFailed, logevent.Str(logevent.AttrErr, saveErr.Error()))
		}
		if !snwin.IsCurrentExeAutostarted() {
			if err := snwin.RegisterAutostart(); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinAutostart,
					logevent.Str(logevent.AttrResult, "error"),
					logevent.Str(logevent.AttrErr, err.Error()))
			} else {
				logevent.Emit(binlog.TagSystem, logevent.EventWinAutostart,
					logevent.Str(logevent.AttrResult, "ok"))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinFirewall,
				logevent.Str(logevent.AttrStage, "exe_path_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinFirewall,
				logevent.Str(logevent.AttrStage, "add_failed"),
				logevent.Str(logevent.AttrErr, err.Error()),
				logevent.Str(logevent.AttrDetail, strings.TrimSpace(string(out))))
			return
		}
		fwRuleInstalled = true
		logevent.Emit(binlog.TagSystem, logevent.EventWinFirewall,
			logevent.Str(logevent.AttrStage, "added"),
			logevent.Str(logevent.AttrExe, filepath.Base(exe)))
	}

	removeOutboundFirewallRule := func() {
		if !fwRuleInstalled {
			return // not installed  -  nothing to remove
		}
		out, err := fwHiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+fwRuleName,
		).CombinedOutput()
		if err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinFirewall,
				logevent.Str(logevent.AttrStage, "delete_failed"),
				logevent.Str(logevent.AttrErr, err.Error()),
				logevent.Str(logevent.AttrDetail, strings.TrimSpace(string(out))))
			return
		}
		fwRuleInstalled = false
		logevent.Emit(binlog.TagSystem, logevent.EventWinFirewall, logevent.Str(logevent.AttrStage, "removed"))
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
		logevent.Emit(binlog.TagSystem, logevent.EventWinConnectStart,
			logevent.Bool(logevent.AttrDialerPresent, dialer != nil),
			logevent.Bool(logevent.AttrTrayPresent, trayApp != nil))
		if dialer != nil {
			wireDialer(dialer)
		}

		// If auto-auth failed at startup (or token expired), re-authenticate
		// silently with saved credentials before attempting to connect.
		if dialer == nil && savedKey != nil {
			srvURL := pickServerURL(savedKey)
			logevent.Emit(binlog.TagSystem, logevent.EventWinConnectReauth,
				logevent.Str(logevent.AttrStage, "trying"),
				logevent.Str(logevent.AttrUser, savedKey.Username),
				logevent.Str(logevent.AttrUrl, srvURL))
			a := core.NewAuthenticator(srvURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetKeyAuth(savedKey)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
			if err := a.Login(); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinConnectReauth,
					logevent.Str(logevent.AttrStage, "direct_failed"),
					logevent.Str(logevent.AttrUrl, srvURL),
					logevent.Str(logevent.AttrErr, err.Error()))
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
									logevent.Emit(binlog.TagSystem, logevent.EventWinConnectUdpRelay,
										logevent.Str(logevent.AttrStage, "punch_failed"),
										logevent.Str(logevent.AttrRelayAddr, entry.Addr),
										logevent.Str(logevent.AttrErr, perr.Error()))
									continue
								}
								rc := core.NewUDPRelayConn(conn, "")
								ra := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
								ra.SetKeyAuth(savedKey)
								ra.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
								if rerr := ra.LoginViaUDP(rc); rerr != nil {
									logevent.Emit(binlog.TagSystem, logevent.EventWinConnectUdpRelay,
										logevent.Str(logevent.AttrStage, "login_failed"),
										logevent.Str(logevent.AttrRelayAddr, entry.Addr),
										logevent.Str(logevent.AttrCtrl, ctrlNode),
										logevent.Str(logevent.AttrErr, rerr.Error()))
									rc.Close()
									continue
								}
								logevent.Emit(binlog.TagSystem, logevent.EventWinConnectUdpRelay,
									logevent.Str(logevent.AttrStage, "ok"),
									logevent.Str(logevent.AttrRelayAddr, entry.Addr),
									logevent.Str(logevent.AttrCtrl, ctrlNode))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinConnectReauth,
					logevent.Str(logevent.AttrStage, "ok"),
					logevent.Str(logevent.AttrUrl, srvURL))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinConnectControls,
					logevent.Str(logevent.AttrList, fmt.Sprintf("%v", allCtrlAddrs)))
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

		logevent.Emit(binlog.TagSystem, logevent.EventWinConnectRelays, logevent.Str(logevent.AttrStage, "fetching"))
		var fetchedRelays []core.RelayEntry
		if relays, err := core.FetchRelayList(relayAPIURL); err == nil {
			fetchedRelays = relays
			router.UpdateRelays(relays)
			if err := core.SaveRelayList(filepath.Join(appDataDir, "relays.json"), relays); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinConnectRelays,
					logevent.Str(logevent.AttrStage, "save_failed"),
					logevent.Str(logevent.AttrErr, err.Error()))
			}
		} else {
			logevent.Emit(binlog.TagSystem, logevent.EventWinConnectRelays,
				logevent.Str(logevent.AttrStage, "fetch_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
			router.UpdateRelays(nil)
		}
		logevent.Emit(binlog.TagSystem, logevent.EventWinConnectRelays, logevent.Str(logevent.AttrStage, "probing_data_plane"))
		router.ProbeDataPlane(3 * time.Second)
		logevent.Emit(binlog.TagSystem, logevent.EventWinConnectRelays, logevent.Str(logevent.AttrStage, "building_paths"))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinConnectSwitch,
				logevent.Str(logevent.AttrFrom, serverURL),
				logevent.Str(logevent.AttrTo, p.ControlAddr))
			a := core.NewAuthenticator(preferred, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "Windows PC")
			a.SetKeyAuth(savedKey)
			a.AdoptToken(dialer.Token())
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinDhtBootstrap,
				logevent.Int(logevent.AttrCtrlCount, int64(len(ctrlAddrs))),
				logevent.Int(logevent.AttrRelayCount, int64(len(fetchedRelays))))

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
							logevent.Emit(binlog.TagSystem, logevent.EventWinDhtAnnounce,
								logevent.Str(logevent.AttrStage, "probe_failed"),
								logevent.Str(logevent.AttrCtrl, ctrlAddr),
								logevent.Str(logevent.AttrErr, err.Error()))
							continue
						}
						cc := lastKnownCountry
						entry, err := core.BuildSignedRelayEntry(nodeID, ep.String(), cc)
						if err != nil {
							logevent.Emit(binlog.TagSystem, logevent.EventWinDhtAnnounce,
								logevent.Str(logevent.AttrStage, "sign_failed"),
								logevent.Str(logevent.AttrErr, err.Error()))
							return
						}
						dhtNode.SetOwnEntry(entry)
						logevent.Emit(binlog.TagSystem, logevent.EventWinDhtAnnounce,
							logevent.Str(logevent.AttrStage, "entry_set"),
							logevent.Str(logevent.AttrAddr, ep.String()),
							logevent.Str(logevent.AttrCc, cc))
						// Register a refresher so each subsequent announce uses a fresh TS+sig.
						// Captures ep and cc from the probe; re-probing on every tick is not
						// needed because the NAT mapping is kept alive by the announce packets.
						epStr := ep.String()
						dhtNode.SetEntryRefresher(func() (*dht.RelayEntry, error) {
							return core.BuildSignedRelayEntry(nodeID, epStr, cc)
						})
						return
					}
					logevent.Emit(binlog.TagSystem, logevent.EventWinDhtAnnounce, logevent.Str(logevent.AttrStage, "all_probes_failed"))
					select {
					case <-announceStop:
						return
					case <-time.After(30 * time.Second):
					}
				}
			}()

			// Per-relay-addr exponential backoff: prevents goroutine storms when a
			// relay is unreachable (no UDP path). Starts at 30s (one ticker cycle),
			// doubles each failure, caps at 5 min. State is reset on success.
			// Ports the fix already shipped on Android/iOS (main_linux.go,
			// lib_ios.go) and mac (main_darwin.go) -- windows never had it either
			// (same gap mac had, just hidden behind logevent.Emit instead of a
			// grep-able Log.Printf string). Confirmed live 2026-08-20/21 on mac: a
			// client with ~120 known relay peers re-punched the entire set every
			// 30s tick forever with no memory of recent failures, generating
			// 800K+ hole-punch log lines in an hour and correlating with real
			// tunnel throughput crashing to near-zero for several-minute
			// stretches while the storm contended with real traffic for local
			// CPU/network.
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
							if !relayBackoffExpired(entry.Addr) {
								continue // recently failed; wait for backoff to expire
							}
							for _, ctrl := range blockedCtrls {
								logevent.Emit(binlog.TagSystem, logevent.EventWinRelayClient,
									logevent.Str(logevent.AttrStage, "punching"),
									logevent.Str(logevent.AttrRelayAddr, entry.Addr),
									logevent.Str(logevent.AttrCtrl, ctrl))
								go func(relayAddr, ctrlURL, ownAddr string) {
									// Send HolePunch invitation to relay via DHT socket.
									dhtNode.SendHolePunch(relayAddr, ownAddr, ctrlURL)
									// Punch from our side simultaneously.
									conn, err := core.Punch("", relayAddr)
									if err != nil {
										logevent.Emit(binlog.TagSystem, logevent.EventWinRelayClient,
											logevent.Str(logevent.AttrStage, "punch_failed"),
											logevent.Str(logevent.AttrRelayAddr, relayAddr),
											logevent.Str(logevent.AttrErr, err.Error()))
										relayPunchFailed(relayAddr)
										return
									}
									rc := core.NewUDPRelayConn(conn, "")
									router.RegisterUDPPeer(entry.NodeID, rc)
									relayPunchSucceeded(relayAddr)
									logevent.Emit(binlog.TagSystem, logevent.EventWinRelayClient,
										logevent.Str(logevent.AttrStage, "established"),
										logevent.Str(logevent.AttrRelayAddr, relayAddr),
										logevent.Str(logevent.AttrCtrl, ctrlURL))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinMyip,
				logevent.Str(logevent.AttrResult, "error"),
				logevent.Str(logevent.AttrErr, myIPErr.Error()))
		} else {
			logevent.Emit(binlog.TagSystem, logevent.EventWinMyip,
				logevent.Str(logevent.AttrResult, "ok"),
				logevent.Str(logevent.AttrValue, publicIP))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinBypassInit, logevent.Str(logevent.AttrErr, err.Error()))
			}
			if bypassMgr != nil {
				// Skip CIDR-based country detection when GPS already answered or the
				// user has explicitly selected a region â€” those take precedence.
				discoveredMu.RLock()
				skipCIDR := gpsDetected || settings.PreferredRegion != ""
				discoveredMu.RUnlock()
				if skipCIDR {
					logevent.Emit(binlog.TagSystem, logevent.EventWinCountryPredetect,
						logevent.Str(logevent.AttrStage, "skipped"),
						logevent.Bool(logevent.AttrGpsDetected, gpsDetected),
						logevent.Str(logevent.AttrRegion, settings.PreferredRegion))
				} else {
					logevent.Emit(binlog.TagSystem, logevent.EventWinCountryPredetect, logevent.Str(logevent.AttrStage, "waiting"))
					cc := waitForCountry(bypassMgr, 5*time.Second)
					if cc != "" {
						discoveredMu.Lock()
						lastKnownCountry = cc
						discoveredMu.Unlock()
						saveCountry(appDataDir, cc)
						logevent.Emit(binlog.TagSystem, logevent.EventWinCountryPredetect,
							logevent.Str(logevent.AttrStage, "detected"),
							logevent.Str(logevent.AttrCc, cc))
						if best := pickServerURL(savedKey); best != "" && best != serverURL {
							logevent.Emit(binlog.TagSystem, logevent.EventWinCountryPredetect,
								logevent.Str(logevent.AttrStage, "switching"),
								logevent.Str(logevent.AttrFrom, serverURL),
								logevent.Str(logevent.AttrTo, best),
								logevent.Str(logevent.AttrCc, cc))
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
						logevent.Emit(binlog.TagSystem, logevent.EventWinCountryPredetect,
							logevent.Str(logevent.AttrStage, "timed_out"),
							logevent.Str(logevent.AttrTo, serverURL))
					}
				}
			}
		}

		effectiveURL := serverURL
		routingViaRelay := false
		path := router.Primary()
		if path != nil && !path.IsDirect() {
			relay := path.Relays[0]
			if path.UDPRelay != nil {
				// UDP hole-punched relay: bypass HTTP, route via peer UDP socket.
				logevent.Emit(binlog.TagSystem, logevent.EventWinPathPick,
					logevent.Str(logevent.AttrStage, "udp_relay"),
					logevent.Str(logevent.AttrScore, fmt.Sprintf("%.0f", path.Score)),
					logevent.Str(logevent.AttrAddr, relay.Addr))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinPathPick,
					logevent.Str(logevent.AttrStage, "tcp_relay"),
					logevent.Str(logevent.AttrScore, fmt.Sprintf("%.0f", path.Score)),
					logevent.Str(logevent.AttrAddr, relay.Addr))
			}
		} else {
			logevent.Emit(binlog.TagSystem, logevent.EventWinPathPick, logevent.Str(logevent.AttrStage, "none"))
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinUdpUpgrade,
						logevent.Str(logevent.AttrStage, "skipped_tcp_available"),
						logevent.Str(logevent.AttrCtrl, ctrlAddr))
				} else if td, tdErr := router.NewControlDialer(ctrlAddr, dialer.Auth()); tdErr == nil {
					logevent.Emit(binlog.TagSystem, logevent.EventWinUdpUpgrade,
						logevent.Str(logevent.AttrStage, "upgraded"),
						logevent.Str(logevent.AttrCtrl, ctrlAddr))
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
						rtt, ok = core.ProbeControlQUIC(addr, 4*time.Second)
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinPoolProbeFailed, logevent.Str(logevent.AttrAddr, res.addr))
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
			// Cap at 12, guaranteeing real TCP/QUIC diversity in the pool
			// instead of a plain RTT-sort cap -- see BalanceByTransport's doc
			// comment for why a pure RTT sort can silently fill the whole
			// pool with one transport and leave no redundancy when it
			// degrades.
			viable = router.BalanceByTransport(viable, 12)
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
				logevent.Str(logevent.AttrStage, "topup_needed"),
				logevent.Int(logevent.AttrNeed, int64(need)),
				logevent.Int(logevent.AttrCount, int64(len(fallback))))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
					logevent.Str(logevent.AttrStage, "topup_added"),
					logevent.Int(logevent.AttrCount, int64(len(extra))),
					logevent.Int(logevent.AttrTotal, int64(len(viable)+len(extra))))
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
						logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
							logevent.Str(logevent.AttrStage, "auth_failed"),
							logevent.Str(logevent.AttrAddr, addr),
							logevent.Str(logevent.AttrErr, err.Error()))
						continue
					}
					var err error
					td, err = router.NewControlDialer(addr, a)
					if err != nil {
						logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
							logevent.Str(logevent.AttrStage, "dialer_failed"),
							logevent.Str(logevent.AttrAddr, addr),
							logevent.Str(logevent.AttrErr, err.Error()))
						continue
					}
					wireDialer(td)
					logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
						logevent.Str(logevent.AttrStage, "control_added"),
						logevent.Str(logevent.AttrAddr, addr))
				}
				poolDialers = append(poolDialers, td)
			}
			if len(poolDialers) == 0 {
				poolDialers = []*core.TunnelDialer{dialer} // fallback: primary only
			}
			logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
				logevent.Str(logevent.AttrStage, "pool_size"),
				logevent.Int(logevent.AttrCount, int64(len(poolDialers))))
			return poolDialers
		}
		logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
			logevent.Str(logevent.AttrStage, "building"),
			logevent.Int(logevent.AttrCount, int64(len(initialViable))))
		initialPoolDialers := buildDialerSlice(initialViable)
		dialerPool = core.NewDialerPool(initialPoolDialers)
		logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild,
			logevent.Str(logevent.AttrStage, "pool_ready"),
			logevent.Int(logevent.AttrCount, int64(dialerPool.Size())))

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
					func() bool {
						return trayApp != nil && trayApp.IsWildcatEnabled()
					},
				)
			}
		})

		// Warning: first re-auth failure  ->  orange icon so user sees something is wrong.
		dialer.SetReAuthWarningHook(func() {
			logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelReauth, logevent.Str(logevent.AttrStage, "warning"))
			if trayApp != nil {
				trayApp.SetAuthWarning("auth server unavailable, retrying...")
			}
		})
		// Recovery: re-auth succeeded after a warning  ->  restore green icon.
		dialer.SetReAuthRecoveredHook(func() {
			logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelReauth, logevent.Str(logevent.AttrStage, "recovered"))
			if trayApp != nil {
				trayApp.ClearAuthWarning()
			}
		})
		// Fatal: exhausted retry window.
		// Auth rejection (server refuses credentials)  ->  login error state, user must re-enter key.
		// Server unavailable (network/arbiter down)  ->  auto-reconnect and keep trying.
		dialer.SetFatalErrorHook(func(err error) {
			logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelReauth,
				logevent.Str(logevent.AttrStage, "fatal"),
				logevent.Str(logevent.AttrErr, err.Error()))
			if trayApp == nil {
				return
			}
			if strings.Contains(err.Error(), "server unavailable") {
				logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelReauth, logevent.Str(logevent.AttrStage, "server_unavailable_reconnect"))
				trayApp.TriggerReconnect()
			} else {
				logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelReauth, logevent.Str(logevent.AttrStage, "credentials_rejected"))
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelEvict,
						logevent.Str(logevent.AttrReason, "first_fail"),
						logevent.Str(logevent.AttrCtrl, ctrlURL))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelEvict,
					logevent.Str(logevent.AttrReason, "data_fail"),
					logevent.Str(logevent.AttrCtrl, ctrlURL))
				core.SetTunnelHealthy(false)
				router.MarkControlDataDead(addr, time.Now().Add(1*time.Minute))
				pool.Evict(td)
				if pool.Size() == 0 {
					logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelEvict,
						logevent.Str(logevent.AttrReason, "pool_empty"),
						logevent.Str(logevent.AttrCtrl, ctrlURL))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelEvict,
					logevent.Str(logevent.AttrReason, "udp_fail"),
					logevent.Str(logevent.AttrCtrl, ctrlURL))
				core.SetTunnelHealthy(false)
				router.MarkUDPDataFailed(addr)
				if pool.Size() > 1 {
					pool.Evict(td)
				} else {
					logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelEvict,
						logevent.Str(logevent.AttrReason, "last_dialer"),
						logevent.Str(logevent.AttrCtrl, ctrlURL))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelEvict,
					logevent.Str(logevent.AttrReason, "auth_fail"),
					logevent.Str(logevent.AttrCtrl, ctrlURL))
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

				logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelSilentRefresh, logevent.Str(logevent.AttrStage, "started"))

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
								logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelSilentRefresh,
									logevent.Str(logevent.AttrStage, "dialer_failed"),
									logevent.Str(logevent.AttrAddr, addr),
									logevent.Str(logevent.AttrErr, err.Error()))
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
						logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelSilentRefresh,
							logevent.Str(logevent.AttrStage, "succeeded"),
							logevent.Int(logevent.AttrPoolSize, int64(len(freshDialers))))
						core.SetTunnelHealthy(true)
						return
					}

					logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelSilentRefresh,
						logevent.Str(logevent.AttrStage, "attempt_failed"),
						logevent.Int(logevent.AttrRetryDelayMs, retryDelay.Milliseconds()))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelSilentRefresh, logevent.Str(logevent.AttrStage, "timed_out"))
				if trayApp != nil {
					trayApp.TriggerReconnect()
				}
			}()
		}

		logevent.Emit(binlog.TagSystem, logevent.EventWinPoolBuild, logevent.Str(logevent.AttrStage, "hooks_attached"))
		// Attach fail hooks to every pool dialer.
		for _, td := range initialPoolDialers {
			attachDataFailHook(td)
			attachUDPFailedHook(td)
			attachAuthFailHook(td)
		}

		logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup, logevent.Str(logevent.AttrStage, "socks5_starting"))
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("SOCKS5 listen: %w", err)
		}
		socksLn = ln
		logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup,
			logevent.Str(logevent.AttrStage, "socks5_listening"),
			logevent.Str(logevent.AttrAddr, ln.Addr().String()))

		logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup, logevent.Str(logevent.AttrStage, "tun_starting"))
		tun = core.NewTUNBridge(ln.Addr().String())
		if err := tun.Start(); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup,
				logevent.Str(logevent.AttrStage, "tun_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
			socksLn.Close()
			socksLn = nil
			return fmt.Errorf("TUN: %w", err)
		}

		logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup,
			logevent.Str(logevent.AttrStage, "routes_applying"),
			logevent.Str(logevent.AttrAddr, effectiveURL))
		routes = snwin.NewRouteManager()
		if err := routes.Apply(hostOf(effectiveURL), core.TUNAddr); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup,
				logevent.Str(logevent.AttrStage, "routes_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
			tun.Stop()
			tun = nil
			socksLn.Close()
			socksLn = nil
			return fmt.Errorf("routing: %w", err)
		}
		logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup, logevent.Str(logevent.AttrStage, "connected"))

		// Point the TUN adapter's own DNS at 1.1.1.1 unconditionally, before
		// the DoH branch below. Without this the adapter has no DNS server of
		// its own and Windows silently keeps resolving via the physical NIC,
		// bypassing the tunnel for every query regardless of DoH or routing
		// (see EnsureTunnelDNS's doc comment). ConfigureDoH/ConfigureDoHFallback
		// build on top of this when DoH is enabled.
		if err := snwin.EnsureTunnelDNS(); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup,
				logevent.Str(logevent.AttrStage, "dns_pin_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinSocks5TunSetup,
				logevent.Str(logevent.AttrStage, "watchdog_write_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinDoh,
					logevent.Str(logevent.AttrTrigger, "connect"),
					logevent.Str(logevent.AttrStage, "configuring"))
				if err := snwin.ConfigureDoH(); err != nil {
					logevent.Emit(binlog.TagSystem, logevent.EventWinDoh,
						logevent.Str(logevent.AttrTrigger, "connect"),
						logevent.Str(logevent.AttrStage, "netsh_unavailable"),
						logevent.Str(logevent.AttrErr, err.Error()))
					if p, perr := snwin.ConfigureDoHFallback(); perr != nil {
						logevent.Emit(binlog.TagSystem, logevent.EventWinDoh,
							logevent.Str(logevent.AttrTrigger, "connect"),
							logevent.Str(logevent.AttrStage, "proxy_failed"),
							logevent.Str(logevent.AttrErr, perr.Error()))
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
		logevent.Emit(binlog.TagSystem, logevent.EventWinConnectFinalize, logevent.Str(logevent.AttrStage, "starting_server"))
		socks5 = core.NewSOCKS5ServerWithPool("", dialerPool, bypassMgr)
		// Use the user's explicit toggle; fall back to CC-based default if no choice saved yet.
		if trayApp != nil {
			socks5.BlockQUIC = trayApp.IsBlockQUICEnabled()
		} else if bypassMgr != nil {
			cc := bypassMgr.Country()
			socks5.BlockQUIC = cc == "RU" || cc == "CN"
		}
		if socks5.BlockQUIC {
			logevent.Emit(binlog.TagSystem, logevent.EventWinConnectFinalize, logevent.Str(logevent.AttrStage, "quic_blocked"))
		}
		// Trial (2026-08-13, rolled out to all desktop clients 2026-08-12):
		// dedicated direct-to-control dialer for general (non-DNS) UDP
		// ASSOCIATE traffic -- voice/video call media, games, anything not
		// otherwise DNS or bypassed. Backed by QUIC (see NewQUICRelayDialer,
		// snc/core/quic_relay.go) rather than the old raw-SNCU native UDP
		// path. See the full rationale in socks5.RealtimeUDPDialer's doc
		// comment (snc/core/socks5.go) and dialerFor's use of it
		// (snc/core/udp_assoc.go). Best-effort -- normal pool-based UDP relay
		// (today's behavior) is exactly what happens if this fails, nothing
		// blocks on it.
		socks5.RealtimeUDPDialer = core.NewQUICRelayDialer(strings.TrimPrefix(effectiveURL, "https://"), dialer.Auth())
		logevent.Emit(binlog.TagSystem, logevent.EventWinConnectFinalize,
			logevent.Str(logevent.AttrStage, "realtime_udp_ready"),
			logevent.Str(logevent.AttrAddr, effectiveURL))
		go socks5.Serve(core.WrapWithTorrentFilter(socksLn)) //nolint:errcheck
		logevent.Emit(binlog.TagSystem, logevent.EventWinConnectFinalize, logevent.Str(logevent.AttrStage, "server_started"))

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
				logevent.Emit(binlog.TagSystem, logevent.EventWinPoolRefill,
					logevent.Str(logevent.AttrStage, "below_target"),
					logevent.Int(logevent.AttrSize, int64(dialerPool.Size())),
					logevent.Int(logevent.AttrTarget, int64(target)))
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
						logevent.Emit(binlog.TagSystem, logevent.EventWinPoolRefill,
							logevent.Str(logevent.AttrStage, "auth_failed"),
							logevent.Str(logevent.AttrAddr, addr),
							logevent.Str(logevent.AttrErr, err.Error()))
						continue
					}
					td, err := router.NewControlDialer(addr, a)
					if err != nil {
						logevent.Emit(binlog.TagSystem, logevent.EventWinPoolRefill,
							logevent.Str(logevent.AttrStage, "dialer_failed"),
							logevent.Str(logevent.AttrAddr, addr),
							logevent.Str(logevent.AttrErr, err.Error()))
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
						logevent.Emit(binlog.TagSystem, logevent.EventWinTunnelWatchdogStale,
							logevent.Int(logevent.AttrIdleMs, time.Since(last).Milliseconds()))
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
			func() bool {
				return trayApp != nil && trayApp.IsWildcatEnabled()
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
			func() bool {
				return trayApp != nil && trayApp.IsWildcatEnabled()
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinCountryChange,
						logevent.Str(logevent.AttrStage, "changed"),
						logevent.Str(logevent.AttrFrom, prev),
						logevent.Str(logevent.AttrTo, cc))
					if trayApp != nil {
						trayApp.TriggerReconnect()
					}
				} else {
					// First detection: rebuild paths now so in-country controls are
					// selected immediately, without waiting for the 60 s pool refresh.
					logevent.Emit(binlog.TagSystem, logevent.EventWinCountryChange,
						logevent.Str(logevent.AttrStage, "region_set"),
						logevent.Str(logevent.AttrTo, cc))
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
										logevent.Emit(binlog.TagSystem, logevent.EventWinCountryChange, logevent.Str(logevent.AttrStage, "pool_complete_after_retry"))
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
				logevent.Emit(binlog.TagSystem, logevent.EventWinCountryChange,
					logevent.Str(logevent.AttrStage, "cidr_init_skipped"),
					logevent.Bool(logevent.AttrGpsDetected, gpsDetected),
					logevent.Str(logevent.AttrRegion, settings.PreferredRegion))
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

		// This is the non-WildCat connect path -- the WildCat branch further up
		// this function returns before ever reaching here (see its own
		// IncConnect/StartWildcatSession calls).
		connStatsCollector.IncConnect(!autoReconnect)
		go runTopup()
		return nil
	}

	onDisconnect := func(autoReconnect bool) {
		// No-op if no WildCat session is active (regular connect, or already
		// closed out above).
		connStatsCollector.IncDisconnect(!autoReconnect)
		if autoReconnect {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDisconnect, logevent.Str(logevent.AttrStage, "auto_reconnect"))
		} else {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDisconnect, logevent.Str(logevent.AttrStage, "user_initiated"))
		}
		// Clear the connected flag so the watchdog knows no cleanup is needed
		// if main exits cleanly after this point.
		if err := core.WriteWatchdogState(core.WatchdogState{MainPID: os.Getpid()}); err != nil {
			logevent.Emit(binlog.TagSystem, logevent.EventWinDisconnect,
				logevent.Str(logevent.AttrStage, "watchdog_write_failed"),
				logevent.Str(logevent.AttrErr, err.Error()))
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
		logevent.Emit(binlog.TagSystem, logevent.EventWinDisconnect, logevent.Str(logevent.AttrStage, "disconnected"))
	}

	autoConnect := savedKey != nil // connect on startup whenever a valid key is present

	// Background update checker  -  runs for the lifetime of the app, independent
	// of connect/disconnect cycles.  Uses discovered controls when available,
	// falls back to key nodes.  Notifies the tray when a binary is ready.
	upd = core.NewUpdater(func() []string {
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

	logevent.Emit(binlog.TagSystem, logevent.EventWinTrayStart)
	trayApp = snwin.NewTrayApp(core.Version, initialLogin, autoConnect, settings.DOHEnabled, initBlockQUIC, settings.WildcatEnabled, settings.PreferredRegion,
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinDoh,
						logevent.Str(logevent.AttrTrigger, "toggle"),
						logevent.Str(logevent.AttrStage, "configuring"))
					if err := snwin.ConfigureDoH(); err != nil {
						logevent.Emit(binlog.TagSystem, logevent.EventWinDoh,
							logevent.Str(logevent.AttrTrigger, "toggle"),
							logevent.Str(logevent.AttrStage, "netsh_unavailable"),
							logevent.Str(logevent.AttrErr, err.Error()))
						if p, perr := snwin.ConfigureDoHFallback(); perr != nil {
							logevent.Emit(binlog.TagSystem, logevent.EventWinDoh,
								logevent.Str(logevent.AttrTrigger, "toggle"),
								logevent.Str(logevent.AttrStage, "proxy_failed"),
								logevent.Str(logevent.AttrErr, perr.Error()))
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
					logevent.Emit(binlog.TagSystem, logevent.EventWinDoh,
						logevent.Str(logevent.AttrTrigger, "toggle"),
						logevent.Str(logevent.AttrStage, "disabled"))
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
			logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
				logevent.Str(logevent.AttrSetting, "block_quic"),
				logevent.Str(logevent.AttrValue, fmt.Sprintf("%v", enabled)))
			// Apply immediately to the running SOCKS5 server â€” no reconnect needed.
			if socks5 != nil {
				socks5.BlockQUIC = enabled
			}
		},
		func(enabled bool) {
			if enabled {
				// Unconditional informational warning -- always shown when the
				// user turns WildCat on, every time, no "don't show again".
				snwin.ShowWildcatWarning()
			}
			settings.WildcatEnabled = enabled
			saveClientSettings(appDataDir, settings)
			logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
				logevent.Str(logevent.AttrSetting, "wildcat"),
				logevent.Str(logevent.AttrValue, fmt.Sprintf("%v", enabled)))
			// WildCat mode switches the underlying transport; always requires reconnect.
		},
		func(region string) {
			settings.PreferredRegion = region
			saveClientSettings(appDataDir, settings)
			logevent.Emit(binlog.TagSystem, logevent.EventWinSettingsChange,
				logevent.Str(logevent.AttrSetting, "region"),
				logevent.Str(logevent.AttrValue, region))
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
	trayApp.SetSettingsChangeCallback(func() { appWindow.RefreshSettingsState(trayApp.GetAppSettings()) })
	trayApp.SetBytesTickCallback(func() {
		sent, recv := core.TotalBytes()
		appWindow.UpdateBytes(sent, recv)
	})
	appWindow.Start()

	trayApp.Run()

	appWindow.Destroy()

	// Tray has exited (user clicked Quit).  onDisconnect was called by the tray
	// before returning, so routes and DoH are already restored.
	// Stop the watchdog monitor goroutine so it does not restart the watchdog.
	close(stopWatchdogMonitor)
	// Signal the watchdog that this is a clean exit  -  do not restart main.
	snwin.SignalCleanShutdown()
	logevent.Emit(binlog.TagSystem, logevent.EventWinShutdownSignaled)
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
	WildcatEnabled  bool   `json:"wildcat_enabled,omitempty"`  // route via the WildCat covert-relay transport
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

	// The "do you have a key?" Yes/No prompt is no longer shown automatically
	// -- go straight to the screen that matches reachability. The user can
	// still switch manually: ShowKeyDialogWithLogin's "Log In Instead" button
	// and ShowLoginDialog's "I Have a Key" button (below, via navlinkLoginFlow)
	// remain fully wired.
	if !reachable {
		// Without navlink.net reachable, login can never succeed, so go
		// straight to manual key entry (no Login button — it wouldn't work).
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
		logevent.Emit(binlog.TagSystem, logevent.EventWinPersistFailed,
			logevent.Str(logevent.AttrWhat, "device_id"),
			logevent.Str(logevent.AttrErr, err.Error()))
	}
	return id
}

// saveCountry persists the client's ISO country code to appDataDir/country.txt.
func saveCountry(dir, cc string) {
	if err := os.WriteFile(filepath.Join(dir, "country.txt"), []byte(cc), 0600); err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinPersistFailed,
			logevent.Str(logevent.AttrWhat, "country"),
			logevent.Str(logevent.AttrErr, err.Error()))
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

	logevent.Emit(binlog.TagSystem, logevent.EventWinSingleInstance, logevent.Str(logevent.AttrStage, "terminating_other"))

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
		logevent.Emit(binlog.TagSystem, logevent.EventWinSingleInstance, logevent.Str(logevent.AttrStage, "kill_failed"))
		return false
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinSingleInstance, logevent.Str(logevent.AttrStage, "old_terminated"))
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
