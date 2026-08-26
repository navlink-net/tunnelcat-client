// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/google/uuid"

	snmac "shortnerdcat/snc/mac/macos"
	"shortnerdcat/snc/shared/keymigrate"
	"tunnel_cat/dht"
	"tunnel_cat/snc/core"
)

// socksListenAddr is the local SOCKS5 proxy address that TUN traffic is forwarded to.
const socksListenAddr = "127.0.0.1:1080"

// homeWasFallback is true when init() couldn't determine a real user HOME
// (LaunchDaemon boot case, see below) and fell back to /var/root. Checked
// later once a console user is detected, so logging can be redirected out
// of /var/root -- see the console-user poll loop in main().
var homeWasFallback bool

// init overrides APPDATA so core.WatchdogStatePath() uses ~/.shortnerdcat.
// When elevated via osascript, HOME may be /var/root; --user-home corrects it.
// When launched by LaunchDaemon, HOME may be absent entirely; fall back to
// /var/root (root's home on macOS) so all data files land in a writable place.
//
// /var/root is a dead end for anything the user is meant to see, though:
// it's 0700 root:wheel, so no chmod applied to files or subdirectories
// underneath it (see openLogsToUser) can make it traversable by a regular
// user -- "Show Logs in Finder" and manual Finder/Terminal access both hit
// permission denied on /var/root itself regardless. This is exactly what
// happens on every reboot once the LaunchDaemon auto-start plist takes over
// (no --user-home, no HOME in a system daemon's environment): logs keep
// being written, just somewhere the user can never reach. See homeWasFallback
// and its use in main() for the fix -- redirect to the real console user's
// home as soon as one is detected.
func init() {
	home := os.Getenv("HOME")
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--user-home" && i+1 < len(os.Args) {
			home = os.Args[i+1]
			os.Setenv("HOME", home)
			break
		}
	}
	if home == "" {
		home = "/var/root"
		homeWasFallback = true
	}
	os.Setenv("APPDATA", filepath.Join(home, ".shortnerdcat"))
}

// homeDirForUID resolves the home directory of a local user by UID, without
// shelling out (os/user.LookupId uses getpwuid(3) directly on darwin).
// Returns "" if the UID doesn't resolve to a real account or has no home.
func homeDirForUID(uid string) string {
	u, err := user.LookupId(uid)
	if err != nil || u.HomeDir == "" {
		return ""
	}
	return u.HomeDir
}

// appDataDir returns ~/.shortnerdcat.
func appDataDir() string {
	return os.Getenv("APPDATA")
}

// ipc is the IPC server; the tray process connects to it after launch.
var ipc *ipcServer

// bananameterProberOnce ensures the prober is started exactly once per process lifetime.
var bananameterProberOnce sync.Once

func debugLog(msg string) {
	f, err := os.OpenFile("/tmp/snc_debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s: [main] uid=%d pid=%d %s\n", time.Now().Format("2006-01-02 15:04:05"), os.Getuid(), os.Getpid(), msg)
	f.Close()
}

func main() {
	debugLog(fmt.Sprintf("started args=%v", os.Args))

	// â”€â”€ --watchdog mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if len(os.Args) > 1 && os.Args[1] == "--watchdog" {
		debugLog("entering watchdog mode")
		runAsWatchdog()
		return
	}

	// --restarted: watchdog restart after a crash â€” skip the splash screen.
	watchdogRestart := len(os.Args) > 1 && os.Args[1] == "--restarted"

	// --controls addr1,addr2,... â€” restrict which control nodes are used (debug).
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

	// â”€â”€ --tray mode: UI process running as the logged-in user â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	var traySocket string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--tray" && i+1 < len(os.Args) {
			traySocket = os.Args[i+1]
			break
		}
	}
	if traySocket != "" {
		debugLog(fmt.Sprintf("entering tray mode socket=%s", traySocket))
		runTrayProcess(traySocket, watchdogRestart)
		return
	}

	// â”€â”€ 0a. Privilege check â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	debugLog(fmt.Sprintf("privilege check uid=%d", os.Getuid()))
	if os.Getuid() != 0 {
		socketPath := snmac.IPCSocketPath(fmt.Sprintf("%d", os.Getuid()))
		debugLog(fmt.Sprintf("not root, checking daemon %s", socketPath))
		if daemonRespondsToPing(socketPath) {
			debugLog("daemon responds to ping, starting tray directly")
			runTrayProcess(socketPath, watchdogRestart)
			return
		}
		// A raw socket connect (the old check here, IsDaemonSocketLive) only
		// proves something is listening, not that it's actually processing
		// requests -- a hung daemon whose accept loop is still alive but
		// stuck elsewhere passed that check forever, so a user relaunch just
		// reattached a fresh tray to the same dead backend instead of ever
		// killing+restarting it (see killStaleProcesses/ensureSingleInstance
		// below, on the root path relaunchAsRoot leads to). Confirmed live
		// 2026-08-17 on the Linux client's identical pattern -- its watchdog
		// log showed it re-attach to an old main pid on startup and then
		// went completely silent. A real ping/pong round-trip catches that;
		// a bare connect does not.
		debugLog("daemon not responding to ping â€” relaunching as root for a clean restart")
		relaunchAsRoot()
		return
	}

	// â”€â”€ 0b. Apply pending OTA update before single-instance guard â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	debugLog("root: applying pending update")
	core.ApplyPendingUpdate()

	// â”€â”€ 1. Logging â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	logDir := filepath.Join(appDataDir(), "logs")
	debugLog(fmt.Sprintf("logDir=%s", logDir))
	if err := core.InitLogging(logDir); err != nil {
		fmt.Fprintf(os.Stderr, "warn: logging init: %v\n", err)
	}
	debugLog("logging initialized")
	// Make log files and directory readable by the regular (non-root) user.
	openLogsToUser(logDir)
	// Redirect stderr to a crash file so fatal Go runtime errors are captured.
	if f, err := os.OpenFile(filepath.Join(logDir, "crash.txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())) //nolint:errcheck
		f.Close()
	}
	debugLog("stderr redirected to crash.txt")
	if len(forcedControls) > 0 {
		core.Log.Printf("DEBUG: --controls override: %v", forcedControls)
	}

	// â”€â”€ 0c. Kill stale processes and clean routing table â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	killStaleProcesses()
	snmac.CleanupSplitRoutes("")

	// â”€â”€ 0d. Single-instance guard â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if !ensureSingleInstance() {
		core.Log.Printf("another instance is running â€” exiting")
		return
	}
	core.UpdateCleanup()
	core.UpdateSignalFunc = snmac.SignalUpdateRestart

	// Install the LaunchDaemon plist on first run so launchd starts the
	// privileged process automatically after every subsequent reboot.
	// The plist is written but not loaded immediately (loading would spawn a
	// second root instance conflicting with this one).  It takes effect on
	// the next reboot â€” after which no osascript password is ever needed.
	if exe, err := os.Executable(); err == nil {
		wasInstalled := snmac.IsLaunchDaemonInstalled()
		if err := snmac.EnsureLaunchDaemon(exe); err != nil {
			core.Log.Printf("warn: LaunchDaemon: %v", err)
		} else if !wasInstalled {
			core.Log.Printf("LaunchDaemon: installed at %s (active after next reboot)", snmac.LaunchDaemonPlistPath)
		}
	}

	// â”€â”€ 1b. Watchdog â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if err := core.WriteWatchdogState(core.WatchdogState{MainPID: os.Getpid()}); err != nil {
		core.Log.Printf("warn: write watchdog state: %v", err)
	}
	snmac.WritePIDFile(os.Getpid())

	var watchdogProc *os.Process
	if !snmac.WatchdogRunning() {
		if wp, err := snmac.StartWatchdog(); err != nil {
			core.Log.Printf("warn: start watchdog: %v", err)
		} else {
			watchdogProc = wp
			core.Log.Printf("watchdog: started pid=%d", wp.Pid)
		}
	} else {
		core.Log.Printf("watchdog: already running")
	}

	stopWatchdogMonitor := make(chan struct{})
	go func() {
		for {
			if watchdogProc != nil {
				done := make(chan struct{})
				go func(p *os.Process) { p.Wait(); close(done) }(watchdogProc) //nolint:errcheck
				select {
				case <-stopWatchdogMonitor:
					return
				case <-done:
					core.Log.Printf("watchdog: process exited â€” restarting")
				}
			}
			select {
			case <-stopWatchdogMonitor:
				return
			case <-time.After(3 * time.Second):
			}
			if snmac.WatchdogRunning() {
				select {
				case <-stopWatchdogMonitor:
					return
				case <-time.After(30 * time.Second):
				}
				watchdogProc = nil
				continue
			}
			wp, err := snmac.StartWatchdog()
			if err != nil {
				core.Log.Printf("warn: restart watchdog: %v", err)
				watchdogProc = nil
			} else {
				watchdogProc = wp
				core.Log.Printf("watchdog: restarted pid=%d", wp.Pid)
			}
		}
	}()

	// Liveness heartbeat so the watchdog can detect a frozen main process.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			core.TouchAlive()
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			core.Log.Printf("PANIC: %v\n%s", r, debug.Stack())
		}
	}()

	// â”€â”€ 2b. IPC: start tray process as logged-in user â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	ipc = newIPCServer()

	// startTrayProcess launches a new tray process as the logged-in user.
	// restarted=true suppresses the splash screen (used for crash/sleep relaunches).
	startTrayProcess := func(uid, socketPath string, restarted bool) {
		exe, _ := os.Executable()
		home := os.Getenv("HOME")
		args := []string{"--tray", socketPath, "--user-home", home}
		if restarted {
			args = append(args, "--restarted")
		}
		trayLogPath := filepath.Join(logDir, "snc_tray.log")
		trayShell := shellEscape(exe)
		for _, a := range args {
			trayShell += " " + shellEscape(a)
		}
		trayShell += " >> " + shellEscape(trayLogPath) + " 2>&1"
		if err := exec.Command("/bin/launchctl", "asuser", uid, "/bin/sh", "-c", trayShell).Start(); err != nil {
			core.Log.Printf("warn: start tray process: %v", err)
		} else {
			core.Log.Printf("tray: started for uid=%s socket=%s restarted=%v", uid, socketPath, restarted)
		}
	}

	// The privileged daemon is normally started by launchd (RunAtLoad) at boot
	// time, well before the GUI login session exists. A one-shot UID check here
	// would frequently see uid==0 (nobody logged in yet) and give up forever,
	// leaving the daemon running with no tray/UI for the rest of its lifetime.
	// Poll until a console user appears (covers slow logins, FileVault unlock,
	// fast user switching at boot), then set up IPC and launch the tray exactly once.
	var trayMu sync.Mutex
	var ipcUID, ipcSocket string
	setTrayIPC := func(uid, sock string) {
		trayMu.Lock()
		ipcUID, ipcSocket = uid, sock
		trayMu.Unlock()
	}
	getTrayIPC := func() (string, string) {
		trayMu.Lock()
		defer trayMu.Unlock()
		return ipcUID, ipcSocket
	}
	go func() {
		for {
			if uid := loggedInUID(); uid != "" {
				// If startup fell back to /var/root (no HOME, no --user-home --
				// the LaunchDaemon boot case), logs and app data have been
				// landing somewhere the user can never reach (/var/root is
				// 0700, not fixable by chmod'ing anything underneath it -- see
				// homeWasFallback's doc comment). Now that a real console user
				// exists, redirect logging there before doing anything else,
				// so this is the last daemon lifetime a user has to ask "where
				// are my logs" about.
				if homeWasFallback {
					if home := homeDirForUID(uid); home != "" {
						os.Setenv("HOME", home)
						os.Setenv("APPDATA", filepath.Join(home, ".shortnerdcat"))
						homeWasFallback = false
						newLogDir := filepath.Join(appDataDir(), "logs")
						if err := core.InitLogging(newLogDir); err != nil {
							core.Log.Printf("warn: re-init logging for uid=%s at %s: %v", uid, newLogDir, err)
						} else {
							openLogsToUser(newLogDir)
							// Also repoint the outer logDir var: startTrayProcess's
							// trayLogPath and the later ipc.SendInit(..., logDir, ...)
							// both close over this same variable, and without this
							// reassignment they'd keep using the pre-redirect
							// /var/root path -- which stays 0700 and unreachable by
							// the user no matter what openLogsToUser chmods
							// underneath it (see homeWasFallback's doc comment).
							// That's exactly why "Show Logs in Finder" kept failing
							// even after logs themselves started landing correctly.
							logDir = newLogDir
							core.Log.Printf("startup: redirected logging from /var/root fallback to %s (uid=%s)", newLogDir, uid)
						}
					}
				}
				socketPath := snmac.IPCSocketPath(uid)
				ln, err := snmac.IPCListen(socketPath, uid)
				if err != nil {
					core.Log.Printf("warn: IPC listen: %v â€” retrying", err)
				} else {
					setTrayIPC(uid, socketPath)
					go ipc.acceptLoop(ln)
					startTrayProcess(uid, socketPath, watchdogRestart)
					core.Log.Printf("tray: console user detected uid=%s â€” launched", uid)
					return
				}
			}
			time.Sleep(2 * time.Second)
		}
	}()

	// â”€â”€ 3. Discovery & auth setup â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	adir := appDataDir()

	var (
		discoveredMu           sync.RWMutex
		discoveredControls     []string
		discoveredClubControls []string // additive extra controls from ClubDiscoverer, see initClubDiscovery
		discoveredRegions      map[string]string
		lastKnownCountry       string
		gpsCountry             string
		gpsDetected            bool
		globalDisc             *core.Discoverer
		discOnce               sync.Once
		clubDiscoverers        []*core.ClubDiscoverer
		clubDiscOnce           sync.Once
		adminPoller            *core.AdminStatusPoller
		setClubThemePreview    func(theme string) // wired by initClubDiscovery; nil until first login
		mirrorMgr              *core.MirrorManager
		mirrorOnce             sync.Once

		// torrent distribution: engine + per-slot state (mirrors Windows wiring).
		torrentEngine     *core.TorrentEngine
		torrentOnce       sync.Once
		torrentUpdateOnce sync.Once
		torrentSlotMagnet = make(map[string]string)
		torrentSlotHash   = make(map[string]metainfo.Hash)
		torrentMu         sync.Mutex
		// wireTorrent is forward-declared so initDiscovery and wireDHT can
		// reference it before the full implementation is assigned below.
		wireTorrent func()
	)

	pickServerURL := func(kd *core.KeyData) string {
		if len(forcedControls) > 0 {
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
		if country != "" && len(regions) > 0 {
			for _, n := range nodes {
				if regions[n] == country {
					return ensureHTTPS(n)
				}
			}
		}
		return ensureHTTPS(nodes[0])
	}

	initDiscovery := func(srvURL string, kd *core.KeyData) {
		discOnce.Do(func() {
			var err error
			globalDisc, err = core.NewDiscoverer(srvURL, kd.ArbiterPubkey,
				core.NewDiscoveryClient(), manifestCacheFile(),
				func(controls []string) {
					discoveredMu.Lock()
					discoveredControls = controls
					if globalDisc != nil {
						discoveredRegions = globalDisc.Regions()
					}
					discoveredMu.Unlock()
					core.Log.Printf("discovery: controls updated: %v", controls)
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
			globalDisc.SetNotificationCallback(func(notifs []core.Notification) {
				now := time.Now().Unix()
				seen := loadNotifSeen()
				var newMsgs []string
				for _, n := range notifs {
					if seen[n.ID] || now-n.CreatedAt > 24*3600 {
						continue
					}
					newMsgs = append(newMsgs, n.Message)
					seen[n.ID] = true
				}
				if len(newMsgs) > 0 {
					saveNotifSeen(seen)
					go snmac.ShowNotifications(newMsgs)
				}
			})
			globalDisc.Start(10 * time.Minute)
			if wireTorrent != nil {
				wireTorrent()
			}
		})
	}

	// initClubDiscovery creates one ClubDiscoverer per known club slug and
	// starts polling -- mirrors the Windows client's function of the same
	// name (see tunnel_cat/docs/club-membership.md). Unlike initDiscovery,
	// this needs a live session token (tokenFn), so it can only run after
	// login, not at daemon startup. Safe to call multiple times; runs only
	// on the first call. Pushes theme/badge updates to the tray process via
	// ipc.PushClubTheme, which forwards them to the window over the daemon
	// <-> tray IPC socket (there is no in-process window handle here, unlike
	// Windows -- this daemon and the UI are separate processes).
	initClubDiscovery := func(srvURL string, kd *core.KeyData, tokenFn func() string) {
		clubDiscOnce.Do(func() {
			slugTheme := map[string]string{"cat_club": "catclub", "elite_cat_club": "elite"}
			slugBadgeLabel := map[string]string{"cat_club": "Cat Club", "elite_cat_club": "Elite Cat Club"}
			themePriority := map[string]int{"": 0, "catclub": 1, "elite": 2}

			var mu sync.Mutex
			bySlug := make(map[string][]string)
			memberSlugs := make(map[string]bool)
			bySlugDisc := make(map[string]*core.ClubDiscoverer)
			isAdmin := kd.IsAdmin
			previewTheme := ""

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
			// applyTheme recomputes the effective theme/badge/canRecommend and
			// pushes it to the tray -- called after any membership change,
			// admin-status change, or preview-override change. The admin's
			// preview override only ever applies while isAdmin is true; it is
			// otherwise ignored (never falls back to it for a non-admin), same
			// gate as every other client.
			applyTheme := func() {
				mu.Lock()
				best, bestSlug := "", ""
				for slug := range memberSlugs {
					if !memberSlugs[slug] {
						continue
					}
					if t := slugTheme[slug]; themePriority[t] > themePriority[best] {
						best, bestSlug = t, slug
					}
				}
				var cd *core.ClubDiscoverer
				if bestSlug != "" {
					cd = bySlugDisc[bestSlug]
				}
				canRecommend := memberSlugs["cat_club"] || memberSlugs["elite_cat_club"]
				admin := isAdmin
				effTheme := best
				if admin && previewTheme != "" {
					effTheme = previewTheme
				}
				mu.Unlock()

				badgeText := ""
				if cd != nil {
					if grantingClub, num, ok := cd.MembershipInfo(); ok {
						label := slugBadgeLabel[grantingClub]
						if label == "" {
							label = slugBadgeLabel[bestSlug]
						}
						badgeText = fmt.Sprintf("%s Member #%d", label, num)
					}
				}
				if admin && effTheme != best {
					// Admin is previewing a theme that differs from their
					// real membership -- show exactly the same badge text a
					// real member of that tier would see, no distinguishing
					// marker and no fabricated number (see 2026-08-14:
					// "(Preview)" was added unilaterally and rejected --
					// preview must look identical to the real thing).
					switch effTheme {
					case "catclub":
						badgeText = "Cat Club Member"
					case "elite":
						badgeText = "Elite Cat Club Member"
					default:
						badgeText = ""
					}
				}
				if ipc != nil {
					ipc.PushClubTheme(effTheme, badgeText, admin, canRecommend)
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

			// setClubThemePreview is called from the "club_theme_preview" IPC
			// command handler when the tray's admin-only Club Theme submenu
			// changes selection.
			setClubThemePreview = func(theme string) {
				mu.Lock()
				if !isAdmin {
					mu.Unlock()
					return
				}
				previewTheme = theme
				mu.Unlock()
				applyTheme()
			}

			adminPoller = core.NewAdminStatusPoller(tokenFn, kd.IsAdmin, func(newIsAdmin bool) {
				mu.Lock()
				isAdmin = newIsAdmin
				if !isAdmin {
					previewTheme = ""
				}
				mu.Unlock()
				applyTheme()
			})
			adminPoller.Start(10 * time.Minute)
		})
	}

	// wireDHT connects the global Discoverer to the DHT node for manifest gossip.
	// Must be called after initDiscovery; safe to call multiple times.
	wireDHT := func(node *core.DHTNode) {
		if globalDisc == nil || node == nil {
			return
		}
		globalDisc.SetFetchCallback(func(raw []byte, ts int64) {
			node.SetManifest(raw, ts)
			if wireTorrent != nil {
				wireTorrent()
			}
		})
		node.SetManifestHandler(func(raw []byte) {
			if err := globalDisc.InjectRaw(raw); err != nil {
				core.Log.Printf("discovery: DHT gossip manifest rejected: %v", err)
			}
		})
	}

	// wireMirror sets up torrent-like content mirroring once dhtNode and kd
	// (for the arbiter pubkey) are both available. Idempotent, same contract
	// as wireDHT.
	wireMirror := func(node *core.DHTNode, kd *core.KeyData) {
		if node == nil || kd == nil {
			return
		}
		mirrorOnce.Do(func() {
			var err error
			mirrorMgr, err = core.NewMirrorManager(
				kd.ArbiterPubkey,
				filepath.Join(adir, "mirror"),
				func(peerAddr string) (*net.UDPConn, error) {
					ownEntry := node.OwnEntry()
					if ownEntry == nil {
						return nil, fmt.Errorf("mirror: own external address not yet known")
					}
					node.SendMirrorPunch(peerAddr, ownEntry.Addr)
					return core.Punch("", peerAddr)
				},
				node.CompletePeers,
				node.SetContent,
				node.SetContentComplete,
				core.DefaultRelayChunkFetcher(globalDisc),
			)
			if err != nil {
				core.Log.Printf("mirror: init failed: %v", err)
				return
			}
			mirrorMgr.LoadCached()
			node.SetContentHandler(mirrorMgr.OnContentManifest)
			node.SetMirrorPunchHandler(func(peerAddr string) {
				conn, err := core.Punch("", peerAddr)
				if err != nil {
					core.Log.Printf("mirror-server: punch to %s failed: %v", peerAddr, err)
					return
				}
				core.NewMirrorConn(conn, mirrorMgr.ServeChunk)
				core.Log.Printf("mirror-server: serving chunk requests from %s", peerAddr)
			})
			core.Log.Printf("mirror: initialized dataDir=%s", filepath.Join(adir, "mirror"))
		})
	}

	// torrentSyncSlot brings one named slot (a software slug or "manifest") to
	// the given magnet. No-op if the slot is already on that magnet. When the
	// magnet changes (new version published) the old torrent is removed and its
	// data deleted before the new one is added — superseded versions must not
	// sit around seeded forever.
	torrentSyncSlot := func(slot, magnet, label string) {
		if magnet == "" || torrentEngine == nil {
			return
		}
		torrentMu.Lock()
		prevMagnet, hadPrev := torrentSlotMagnet[slot]
		prevHash, hadHash := torrentSlotHash[slot]
		torrentMu.Unlock()
		if hadPrev && prevMagnet == magnet {
			return
		}
		newHash, err := torrentEngine.AddMagnet(magnet)
		if err != nil {
			core.Log.Printf("torrent: add %s (%s): %v", label, slot, err)
			return
		}
		if hadHash {
			if err := torrentEngine.Remove(prevHash, true); err != nil {
				core.Log.Printf("torrent: remove stale %s: %v", label, err)
			} else {
				core.Log.Printf("torrent: removed stale %s", label)
			}
		}
		torrentMu.Lock()
		torrentSlotMagnet[slot] = magnet
		torrentSlotHash[slot] = newHash
		torrentMu.Unlock()
		core.Log.Printf("torrent: added %s (%s)", label, slot)
	}

	// torrentCheckUpdate reads the "versions" torrent once it's downloaded; if
	// a newer version is available and the "macos" software torrent is also
	// done, stages it via ApplyTorrentDownloadedDMG and fires the same
	// ipc.PushUpdate callback the HTTP updater uses.
	torrentCheckUpdate := func() {
		if torrentEngine == nil {
			return
		}
		dataDir := filepath.Join(adir, "torrents")
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
			core.Log.Printf("torrent: versions.json parse failed: %v", err)
			return
		}
		entry, ok := versions["macos"]
		if !ok || !entry.Available || entry.Version == "" || entry.Version <= core.Version {
			return
		}
		torrentMu.Lock()
		wantHash, haveWant := torrentSlotHash["macos"]
		torrentMu.Unlock()
		if !haveWant {
			return
		}
		var softwareDone bool
		var softwareName string
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
			dmgPath := filepath.Join(dataDir, softwareName)
			if err := core.ApplyTorrentDownloadedDMG(dmgPath); err != nil {
				core.Log.Printf("torrent: stage DMG failed: %v", err)
				return
			}
			core.Log.Printf("torrent: update %s staged via torrent, notifying", entry.Version)
			ipc.PushUpdate(entry.Version)
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
			torrentEngine = core.NewTorrentEngine(filepath.Join(adir, "torrents"))
			if err := torrentEngine.Start(); err != nil {
				core.Log.Printf("torrent: engine start failed: %v", err)
				torrentEngine = nil
				return
			}
			core.Log.Printf("torrent: engine started")
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

	// startDiscovery is the canonical entry point: idempotent init + DHT wiring.
	startDiscovery := func(srvURL string, kd *core.KeyData, node *core.DHTNode) {
		initDiscovery(srvURL, kd)
		wireDHT(node)
		wireMirror(node, kd)
		if wireTorrent != nil {
			wireTorrent()
		}
	}

	getControls := func() []string {
		if len(forcedControls) > 0 {
			return forcedControls
		}
		discoveredMu.RLock()
		defer discoveredMu.RUnlock()
		return append([]string{}, discoveredControls...)
	}

	// â”€â”€ Device & node identity â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	deviceID := loadOrCreateDeviceID(adir)
	core.Log.Printf("device ID: %.8s...", deviceID)

	nodeID, err := core.LoadOrGenNodeID(adir)
	if err != nil {
		core.Log.Printf("warn: node ID: %v", err)
		nodeID = "unknown"
	}
	core.Log.Printf("node ID: %.8s...", nodeID)

	// DHT node â€” started once at app startup and kept alive across
	// connect/disconnect so the relay registry stays warm between sessions.
	var dhtNode *core.DHTNode
	if dhtID, err := core.ParseDHTID(nodeID); err == nil {
		if udpConn, err := net.ListenUDP("udp", &net.UDPAddr{}); err == nil {
			peersPath := filepath.Join(adir, "peers.json")
			dhtNode = core.NewDHTNode(dhtID, udpConn, peersPath)
			dhtNode.Bootstrap(nil)                                     // load peers.json cache; no network seeds yet
			dhtNode.LoadRelays(filepath.Join(adir, "dht_relays.json")) //nolint:errcheck
			dhtNode.Start()
			core.Log.Printf("dht: node started id=%.8s... addr=%s", nodeID, udpConn.LocalAddr())
		} else {
			core.Log.Printf("dht: UDP listen failed: %v â€” DHT disabled", err)
		}
	} else {
		core.Log.Printf("dht: bad node ID %q â€” DHT disabled", nodeID)
	}

	router := core.NewRouter()

	// connStatsCollector lives for the whole process (created once, like
	// router above), not per-connect -- its event counters must accumulate
	// across reconnects and only get drained by the uploader's own tick
	// (see ConnStatsCollector.Snapshot).
	connStatsCollector := core.NewConnStatsCollector(filepath.Join(adir, "connstats.json"))

	// Load persisted country so regional routing works from the first connect.
	if cc := loadCountry(adir); cc != "" {
		lastKnownCountry = cc
		core.Log.Printf("geo: loaded persisted country %q", cc)
	}

	settings := loadClientSettings(adir)

	if settings.PreferredRegion != "" {
		lastKnownCountry = settings.PreferredRegion
		router.SetMyCountry(settings.PreferredRegion)
	}

	// Detect device country in background (GPS/locale/timezone).
	go func() {
		if cc := detectDeviceCC(); cc != "" {
			discoveredMu.Lock()
			gpsCountry = cc
			gpsDetected = true
			if settings.PreferredRegion == "" {
				lastKnownCountry = cc
			}
			discoveredMu.Unlock()
			if settings.PreferredRegion == "" {
				router.SetMyCountry(cc)
				saveCountry(adir, cc)
				core.Log.Printf("geo: device country=%q applied", cc)
			}
		}
	}()

	var dhtMergerStop chan struct{}
	var countryCheckerStop chan struct{}

	// byteTickerStop stops the once-a-second uplink/downlink counter push to
	// the tray (see onConnect/onDisconnect below and ipcServer.PushBytes).
	// nil whenever not connected.
	var byteTickerStop chan struct{}

	var pfwMgr snmac.FirewallManager // pf DNS-leak prevention rules; zero value is ready

	var (
		socksLn           net.Listener
		socks5            *core.SOCKS5Server
		tunBridge         *core.TUNBridge
		routes            *snmac.RouteManager
		dns               *snmac.DNSManager
		logUploader       *core.LogUploader
		connStatsUploader *core.ConnStatsUploader
		bypassMgr         *core.BypassManager
		dialerPool        *core.DialerPool
		poolRefreshStop   chan struct{}
		dialer            *core.TunnelDialer
		serverURL         string
		savedKey          *core.KeyData
		decoyMgr          *core.DecoyManager
	)

	// â”€â”€ Auto-login with saved key â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if keyStr, err := snmac.LoadKey(adir); err == nil {
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
				if saveErr := snmac.SaveKey(adir, newKeyStr); saveErr != nil {
					core.Log.Printf("startup: could not persist migrated key: %v", saveErr)
				}
				keyStr, kd = newKeyStr, newKD
			}
		}
		if err == nil && len(kd.Nodes()) > 0 {
			savedKey = kd
			// Once a manifest has ever been cached, it is authoritative — the
			// key's embedded ControlNodes is bootstrap-only and must not be
			// raced alongside (or merged with) manifest data forever, or a
			// control retired from the live manifest stays in every future
			// auth attempt for as long as the user never re-downloads a key.
			cachedManifestControls := core.ReadManifestCacheControls(manifestCacheFile(), kd.ArbiterPubkey)
			bootstrapSeed := kd.Nodes()[0]
			if len(cachedManifestControls) > 0 {
				bootstrapSeed = cachedManifestControls[0]
			}
			go startDiscovery(ensureHTTPS(bootstrapSeed), kd, dhtNode)
			// loginURL authenticates to one control URL and returns the authenticator
			// on success, nil on failure. No side effects â€” safe to call concurrently.
			autoLoginURL := func(url string) *core.Authenticator {
				core.Log.Printf("auto-auth: user %s @ %s", kd.Username, url)
				a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
				a.SetKeyAuth(kd)
				a.SetDeviceInfo(kd.KeyID, deviceID, "macOS")
				if err := a.Login(); err != nil {
					core.Log.Printf("auto-auth: failed %s: %v", url, err)
					return nil
				}
				core.Log.Printf("auto-auth: OK %s token=%s...", url, a.Token()[:8])
				return a
			}
			// applyAutoAuth wires a successful authenticator into global dialer state.
			// Must be called from the main goroutine (not concurrently).
			applyAutoAuth := func(a *core.Authenticator, url string) {
				if userNotifs := a.DrainNotifications(); len(userNotifs) > 0 {
					go showUserNotifications(userNotifs)
				}
				dialer = core.NewTunnelDialer(a)
				serverURL = url
				go startDiscovery(url, kd, dhtNode)
			}
			authNodes := core.BootstrapControlList(cachedManifestControls, kd.Nodes())
			if len(forcedControls) > 0 {
				authNodes = forcedControls
			}
			// Race all direct controls in parallel: first successful auth wins.
			// Remaining goroutines finish in the background and their results are discarded.
			type autoAuthResult struct {
				a   *core.Authenticator
				url string
			}
			autoResultCh := make(chan autoAuthResult, 1)
			var autoAuthWg sync.WaitGroup
			for _, node := range authNodes {
				autoAuthWg.Add(1)
				go func(u string) {
					defer autoAuthWg.Done()
					if a := autoLoginURL(u); a != nil {
						select {
						case autoResultCh <- autoAuthResult{a, u}:
						default:
						}
					}
				}(ensureHTTPS(node))
			}
			go func() { autoAuthWg.Wait(); close(autoResultCh) }()
			authOK := false
			if res, ok := <-autoResultCh; ok {
				authOK = true
				applyAutoAuth(res.a, res.url)
			}
			if !authOK {
				// Fallback: try cached relays sequentially (relays are less reliable).
				if cachedRelays, _ := core.LoadRelayList(filepath.Join(adir, "relays.json")); len(cachedRelays) > 0 {
					cc := loadCountry(adir)
					for _, relay := range core.RelaysByCountry(cachedRelays, cc) {
						if a := autoLoginURL(ensureHTTPS(relay.Addr)); a != nil {
							applyAutoAuth(a, ensureHTTPS(relay.Addr))
							authOK = true
							break
						}
					}
				}
			}
			if !authOK {
				core.Log.Printf("auto-auth: all nodes unreachable")
			}
		} else {
			core.Log.Printf("saved key invalid: %v", err)
		}
	} else {
		core.Log.Printf("no saved key: %v", err)
	}

	initialLogin := savedKey != nil

	// â”€â”€ onLoginWithKey â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// Called when the tray process submits a subscription key via IPC.
	onLoginWithKey := func(keyStr string) error {
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

		// loginURL authenticates to one control URL; no side effects, safe to call concurrently.
		loginURL := func(url string) *core.Authenticator {
			core.Log.Printf("login: auth user %s @ %s", kd.Username, url)
			a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
			a.SetKeyAuth(kd)
			a.SetDeviceInfo(kd.KeyID, deviceID, "macOS")
			if err := a.Login(); err != nil {
				core.Log.Printf("login: auth failed %s: %v", url, err)
				return nil
			}
			core.Log.Printf("login: auth OK %s token=%s...", url, a.Token()[:8])
			return a
		}
		// applyLoginAuth wires authenticator into global dialer state (main goroutine only).
		applyLoginAuth := func(a *core.Authenticator, url string) {
			dialer = core.NewTunnelDialer(a)
			serverURL = url
			go startDiscovery(url, kd, dhtNode)
		}

		// A cached manifest (from a prior session, any account) is network-
		// wide and authoritative once it exists — see core.BootstrapControlList.
		loginNodes := core.BootstrapControlList(
			core.ReadManifestCacheControls(manifestCacheFile(), kd.ArbiterPubkey), kd.Nodes())
		if len(forcedControls) > 0 {
			loginNodes = forcedControls
		}
		// Race all direct controls in parallel: first successful auth wins.
		type loginResult struct {
			a   *core.Authenticator
			url string
		}
		loginResultCh := make(chan loginResult, 1)
		var loginWg sync.WaitGroup
		for _, node := range loginNodes {
			loginWg.Add(1)
			go func(u string) {
				defer loginWg.Done()
				if a := loginURL(u); a != nil {
					select {
					case loginResultCh <- loginResult{a, u}:
					default:
					}
				}
			}(ensureHTTPS(node))
		}
		go func() { loginWg.Wait(); close(loginResultCh) }()
		authOK := false
		if res, ok := <-loginResultCh; ok {
			authOK = true
			applyLoginAuth(res.a, res.url)
		}
		if !authOK {
			if cachedRelays, _ := core.LoadRelayList(filepath.Join(adir, "relays.json")); len(cachedRelays) > 0 {
				cc := loadCountry(adir)
				for _, relay := range core.RelaysByCountry(cachedRelays, cc) {
					if a := loginURL(ensureHTTPS(relay.Addr)); a != nil {
						applyLoginAuth(a, ensureHTTPS(relay.Addr))
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
		if err := snmac.SaveKey(adir, keyStr); err != nil {
			core.Log.Printf("warn: save key: %v", err)
		}
		if !snmac.IsAutostartRegistered() {
			if err := snmac.RegisterAutostart(); err != nil {
				core.Log.Printf("autostart: register: %v", err)
			}
		}
		return nil
	}

	onLogout := func() {
		savedKey = nil
		dialer = nil
		serverURL = ""
		os.Remove(filepath.Join(adir, "key.dat")) //nolint:errcheck
	}

	// â”€â”€ onConnect â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	onConnect := func(autoReconnect bool) error {
		// Clear the explicit-disconnect flag so a watchdog restart after this
		// connect will auto-reconnect correctly.
		os.Remove(filepath.Join(adir, "user_disconnected")) //nolint:errcheck

		var publicIP string

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
		if dialer != nil {
			wireDialer(dialer)
		}

		// Re-auth if token has expired or auto-auth was skipped at startup.
		if dialer == nil && savedKey != nil {
			srvURL := pickServerURL(savedKey)
			core.Log.Printf("connect: re-auth user %s @ %s", savedKey.Username, srvURL)
			a := core.NewAuthenticator(srvURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetKeyAuth(savedKey)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
			if err := a.Login(); err != nil {
				core.Log.Printf("connect: re-auth direct failed: %v â€” trying UDP relay bootstrap", err)
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
								ra.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
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
				core.Log.Printf("connect: re-auth OK token=%s...", a.Token()[:8])
				dialer = wireDialer(core.NewTunnelDialer(a))
				serverURL = srvURL
			}
		}
		if dialer == nil {
			return fmt.Errorf("not logged in")
		}

		relayAPIURL := serverURL

		// Register all known control addresses in the router.
		var allCtrlAddrs []string
		if len(forcedControls) > 0 {
			for _, n := range forcedControls {
				allCtrlAddrs = append(allCtrlAddrs, hostNameOf(n)+":443")
			}
			router.SetControlsWithRegions(allCtrlAddrs, nil)
		} else {
			if savedKey != nil {
				for _, n := range savedKey.Nodes() {
					allCtrlAddrs = append(allCtrlAddrs, hostNameOf(n)+":443")
				}
			}
			discoveredMu.RLock()
			regions := discoveredRegions
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
			if globalDisc != nil {
				router.SetLoadFactors(globalDisc.LoadFactors())
			}
		}

		// Fetch relay list and probe data plane.
		var fetchedRelays []core.RelayEntry
		if relays, err := core.FetchRelayList(relayAPIURL); err == nil {
			fetchedRelays = relays
			router.UpdateRelays(relays)
			core.SaveRelayList(filepath.Join(adir, "relays.json"), relays) //nolint:errcheck
		} else {
			core.Log.Printf("connect: relay list fetch failed (%v) â€” routing direct", err)
			router.UpdateRelays(nil)
		}

		// DHT: seed from both control node IPs and relay list, then start the merger.
		// Controls are the reliable bootstrap seeds; relays are discovered via DHT after bootstrap.
		if dhtNode != nil {
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
			go func() {
				for {
					for _, ctrlAddr := range ctrlAddrs {
						ep, err := core.ProbeExternalEndpoint(ctrlAddr, "")
						if err != nil {
							core.Log.Printf("dht: probe external endpoint via %s: %v", ctrlAddr, err)
							continue
						}
						discoveredMu.RLock()
						cc := lastKnownCountry
						discoveredMu.RUnlock()
						entry, err := core.BuildSignedRelayEntry(nodeID, ep.String(), cc)
						if err != nil {
							core.Log.Printf("dht: sign relay entry: %v", err)
							return
						}
						dhtNode.SetOwnEntry(entry)
						core.Log.Printf("dht: own relay entry set addr=%s cc=%s", ep, cc)
						epStr := ep.String()
						dhtNode.SetEntryRefresher(func() (*dht.RelayEntry, error) {
							discoveredMu.RLock()
							cc2 := lastKnownCountry
							discoveredMu.RUnlock()
							return core.BuildSignedRelayEntry(nodeID, epStr, cc2)
						})
						return
					}
					core.Log.Printf("dht: all endpoint probes failed, retrying in 30s")
					time.Sleep(30 * time.Second)
				}
			}()

			// Per-relay-addr exponential backoff: prevents goroutine storms when a
			// relay is unreachable (no UDP path). Starts at 30s (one ticker cycle),
			// doubles each failure, caps at 5 min. State is reset on success.
			// Ports the fix already shipped on Android/iOS (main_linux.go,
			// lib_ios.go) -- mac never had it, and confirmed live 2026-08-20/21:
			// a client with several out-of-country "blocked" controls and ~120
			// known relay peers re-punched the entire set every 30s tick forever
			// (no backoff at all), generating 800K+ hole-punch log lines in an
			// hour and correlating with real tunnel throughput crashing to
			// near-zero for several-minute stretches (contending with real
			// traffic for local CPU/network) before recovering once the storm
			// let up.
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

			// Merge DHT-discovered relays into router and initiate hole punches for
			// blocked controls. Runs every 30s â€” fast enough to react to new relay
			// entries (RelayTTL = 2 min) without hammering the network.
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
						dhtNode.SaveRelays(filepath.Join(adir, "dht_relays.json")) //nolint:errcheck
						router.MergeDHTRelays(entries)
						router.ProbeDataPlane(3 * time.Second)
						router.BuildPaths()

						// Relay client: for each relay with no live UDP peer yet,
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
								core.Log.Printf("relay-client: punching %s for ctrl=%s", entry.Addr, ctrl)
								go func(relayAddr, ctrlURL, ownAddr string) {
									dhtNode.SendHolePunch(relayAddr, ownAddr, ctrlURL)
									conn, err := core.Punch("", relayAddr)
									if err != nil {
										core.Log.Printf("relay-client: punch %s failed: %v", relayAddr, err)
										relayPunchFailed(relayAddr)
										return
									}
									rc := core.NewUDPRelayConn(conn, "")
									router.RegisterUDPPeer(entry.NodeID, rc)
									relayPunchSucceeded(relayAddr)
									core.Log.Printf("relay-client: relay established %s â†’ %s", relayAddr, ctrlURL)
									router.BuildPaths()
								}(entry.Addr, ctrl, ownEntry.Addr)
								break
							}
						}
					}
				}
			}()
		}

		router.ProbeDataPlane(3 * time.Second)

		discoveredMu.RLock()
		cc := lastKnownCountry
		discoveredMu.RUnlock()
		if cc != "" {
			router.SetMyCountry(cc)
		}
		router.BuildPaths()

		// Pick best primary path.
		if better, p := router.PrimaryIsBetter(serverURL, 1.5); better && p != nil {
			preferred := ensureHTTPS(p.ControlAddr)
			a := core.NewAuthenticator(preferred, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
			a.SetKeyAuth(savedKey)
			a.AdoptToken(dialer.Token())
			if td, err := router.NewControlDialer(p.ControlAddr, a); err == nil {
				dialer = wireDialer(td)
			} else {
				dialer = wireDialer(core.NewTunnelDialer(a))
			}
			serverURL = preferred
			relayAPIURL = preferred
			core.Log.Printf("connect: switched to control %s (better RTT)", p.ControlAddr)
		}

		// Resolve public IP for country detection and relay eligibility.
		var myIPErr error
		publicIP, myIPErr = core.FetchMyIP(relayAPIURL)
		if myIPErr != nil {
			core.Log.Printf("connect: myIP fetch failed: %v", myIPErr)
		} else {
			core.Log.Printf("connect: myIP=%s", publicIP)
			dialer.SetClientIP(publicIP)
		}

		// Bypass manager for CIDR-based country detection.
		{
			bPubkey := ""
			if savedKey != nil {
				bPubkey = savedKey.ArbiterPubkey
			}
			bCacheFile := filepath.Join(adir, "cidr.json")
			if bm, err := core.NewBypassManager([]string{serverURL}, bPubkey, bCacheFile); err == nil {
				bm.SetToken(dialer.Token())
				if publicIP != "" {
					bm.SetMyIP(publicIP)
				}
				bm.Start()
				bypassMgr = bm
				if savedKey != nil {
					initClubDiscovery(serverURL, savedKey, dialer.Token)
				}
			}
			if bypassMgr != nil {
				discoveredMu.RLock()
				skipCIDR := gpsDetected || settings.PreferredRegion != ""
				discoveredMu.RUnlock()
				if !skipCIDR {
					core.Log.Printf("connect: waiting for CIDR country detection (up to 5 s)...")
					if detectedCC := waitForCountry(bypassMgr, 5*time.Second); detectedCC != "" {
						discoveredMu.Lock()
						lastKnownCountry = detectedCC
						discoveredMu.Unlock()
						saveCountry(adir, detectedCC)
						core.Log.Printf("connect: pre-detected country=%q", detectedCC)
						if best := pickServerURL(savedKey); best != "" && best != serverURL {
							a := core.NewAuthenticator(best, savedKey.APIKey, savedKey.Username, savedKey.Password)
							a.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
							a.SetKeyAuth(savedKey)
							a.AdoptToken(dialer.Token())
							if td, err := router.NewControlDialer(hostNameOf(best)+":443", a); err == nil {
								dialer = wireDialer(td)
							} else {
								dialer = wireDialer(core.NewTunnelDialer(a))
							}
							serverURL = best
							relayAPIURL = best
							router.ProbeDataPlane(3 * time.Second)
							router.BuildPaths()
						}
					}
				}
			}
		}

		// Select relay if available.
		effectiveURL := serverURL
		if path := router.Primary(); path != nil && !path.IsDirect() {
			relay := path.Relays[0]
			if path.UDPRelay != nil {
				core.Log.Printf("connect: routing via UDP relay peer=%s", relay.Addr)
				dialer = core.NewUDPRelayDialer(path.UDPRelay, dialer.Auth())
			} else {
				relayURL := ensureHTTPS(relay.Addr)
				ra := core.NewAuthenticator(relayURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
				ra.SetKeyAuth(savedKey)
				ra.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
				// Reuse existing session token â€” one session per VPN activation.
				if dialer != nil {
					ra.AdoptToken(dialer.Token())
				}
				if ra.Token() != "" {
					dialer = wireDialer(core.NewTunnelDialer(ra))
					effectiveURL = relayURL
					core.Log.Printf("connect: routing via relay addr=%s (token adopted)", relay.Addr)
				} else if err := ra.Login(); err == nil {
					dialer = wireDialer(core.NewTunnelDialer(ra))
					effectiveURL = relayURL
					core.Log.Printf("connect: routing via relay addr=%s", relay.Addr)
				} else {
					core.Log.Printf("connect: relay auth failed (%v) â€” routing direct", err)
				}
			}
		}

		// Detach stale transport overrides from the previous session.
		if dialer != nil {
			dialer.ClearDialFunc()
			dialer.Auth().ClearDialFunc()
		}

		// buildViableAddrs probes qualifying controls and returns RTT-sorted list.
		buildViableAddrs := func(qual []string) []string {
			type pr struct {
				addr string
				rtt  time.Duration
				ok   bool
			}
			ch := make(chan pr, len(qual))
			for _, addr := range qual {
				go func(addr string) {
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
				}
			}
			sort.Slice(viable, func(i, j int) bool {
				iUDP := router.ControlTransportName(viable[i]) == "udp"
				jUDP := router.ControlTransportName(viable[j]) == "udp"
				if iUDP != jUDP {
					return !iUDP
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
			core.Log.Printf("connect: pool: topping up â€” need %d more, probing %d out-of-country candidate(s)", need, len(fallback))
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

		initialViable := buildViableAddrsTopup(router.QualifyingControlAddrs())

		buildDialerSlice := func(viable []string) []*core.TunnelDialer {
			var poolDialers []*core.TunnelDialer
			for _, addr := range viable {
				ctrlURL := ensureHTTPS(addr)
				var td *core.TunnelDialer
				if ctrlURL == serverURL || ctrlURL == effectiveURL {
					td = dialer
				} else {
					a := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
					a.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
					a.SetKeyAuth(savedKey)
					if err := a.Login(); err != nil {
						core.Log.Printf("connect: pool: auth to %s failed (%v) â€” skipping", addr, err)
						continue
					}
					var tdErr error
					td, tdErr = router.NewControlDialer(addr, a)
					if tdErr != nil {
						core.Log.Printf("connect: pool: dialer for %s failed (%v) â€” skipping", addr, tdErr)
						continue
					}
					wireDialer(td)
					core.Log.Printf("connect: pool: added control %s", addr)
				}
				poolDialers = append(poolDialers, td)
			}
			// UDP relay pass: add controls reachable only via hole-punched relay peers.
			// Cap-at-5 above applies only to direct controls; relay-backed ones are
			// additive. bootstrapAuth (dialer.Auth()) is reused â€” tokens are
			// cross-control; re-auth hits a reachable control; data routes via
			// UDPRelayConn.controlURL to the blocked control.
			{
				directSet := make(map[string]bool, len(viable))
				for _, a := range viable {
					directSet[ensureHTTPS(a)] = true
				}
				for _, path := range router.Paths() {
					if path.UDPRelay == nil {
						continue
					}
					ctrlURL := ensureHTTPS(path.ControlAddr)
					if directSet[ctrlURL] {
						continue
					}
					directSet[ctrlURL] = true
					rd := core.NewUDPRelayDialer(path.UDPRelay, dialer.Auth())
					wireDialer(rd)
					poolDialers = append(poolDialers, rd)
					core.Log.Printf("connect: pool: UDP relay ctrl=%s via peer=%s", path.ControlAddr, path.Relays[0].Addr)
				}
			}
			if len(poolDialers) == 0 {
				poolDialers = []*core.TunnelDialer{dialer}
			}
			return poolDialers
		}

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

		// BananaMeter tunnel-diagnostics probe: started once per process lifetime,
		// reads dialerPool/ipc fresh each tick so it survives reconnects and pool swaps.
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
						return ipc.IsWildcatEnabled()
					},
				)
			}
		})

		// Auth warning / recovery / fatal hooks on the primary dialer.
		dialer.SetReAuthWarningHook(func() {
			ipc.PushAuthWarn("auth server unavailable, retrying...")
		})
		dialer.SetReAuthRecoveredHook(func() {
			ipc.ClearAuthWarn()
		})
		dialer.SetFatalErrorHook(func(err error) {
			core.Log.Printf("tunnel: fatal re-auth failure (%v)", err)
			if strings.Contains(err.Error(), "server unavailable") {
				ipc.TriggerReconnect()
			} else {
				ipc.PushStatus("login_error", "")
			}
		})

		var startSilentRefresh func()
		var silentRefreshActive int32

		attachDataFailHook := func(td *core.TunnelDialer) {
			ctrlURL := td.ServerURL()
			td.SetFirstFailHook(func() {
				// dialerPool is nulled out by disconnect() (see below) without cancelling
				// in-flight dials on the dialers it owned -- a dial that was already
				// underway can still fail and fire this hook afterward. Snapshot into a
				// local and bail if torn down; a crashed process is a total outage
				// (the watchdog just relaunches it, dropping every live connection),
				// while a no-op here just means this stale hook does nothing, which is
				// correct since there's no pool left to evict from anyway.
				pool := dialerPool
				if pool == nil {
					return
				}
				if pool.Size() > 1 {
					core.Log.Printf("tunnel: first stream failure on %s â€” instant eviction, switching to standby", ctrlURL)
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
				core.Log.Printf("tunnel: data-plane failure on %s â€” marking data-dead, evicting", ctrlURL)
				core.SetTunnelHealthy(false)
				router.MarkControlDataDead(addr, time.Now().Add(1*time.Minute))
				pool.Evict(td)
				if pool.Size() == 0 {
					core.Log.Printf("tunnel: pool empty after data-fail â€” triggering full reconnect")
					ipc.TriggerReconnect()
					return
				}
				startSilentRefresh()
			})
		}

		attachUDPFailedHook := func(td *core.TunnelDialer) {
			ctrlURL := td.ServerURL()
			td.SetUDPFailedHook(func() {
				pool := dialerPool
				if pool == nil {
					return
				}
				addr := hostNameOf(ctrlURL) + ":443"
				core.Log.Printf("tunnel: UDP relay failed on %s â€” downgrading to TCP", ctrlURL)
				core.SetTunnelHealthy(false)
				router.MarkUDPDataFailed(addr)
				if pool.Size() > 1 {
					pool.Evict(td)
				}
				startSilentRefresh()
			})
		}

		// attachAuthFailHook reacts to repeated *login* failures against this
		// dialer's control node (e.g. TLS interception/blocking, or the node
		// simply being unreachable at the auth layer) — distinct from
		// attachDataFailHook's data-plane failures. Without this, a dialer
		// whose underlying Authenticator can no longer log in just keeps
		// retrying the same fixed node forever (refreshToken's own 3-hour
		// patience window) while looking "alive" to the router's RTT/liveness
		// probe, which checks a different, unauthenticated transport (see the
		// TCP-blocked-but-UDP-alive control incident, 2026-08-11) — so this
		// node was never re-evaluated against the others despite the login
		// itself never succeeding. Reuses the exact same eviction + flap +
		// silent-refresh path as data-plane failures, so node *ranking* is
		// untouched: this only removes a currently-unusable candidate from
		// consideration, same as an evicted data-dead node, with the same
		// short-TTL flap penalty rather than a permanent ban.
		attachAuthFailHook := func(td *core.TunnelDialer) {
			ctrlURL := td.ServerURL()
			td.SetAuthFailHook(func() {
				pool := dialerPool
				if pool == nil {
					return
				}
				core.Log.Printf("tunnel: repeated login failures on %s â€” evicting, switching to standby", ctrlURL)
				router.RecordControlFlap(hostNameOf(ctrlURL) + ":443")
				if pool.Size() > 1 {
					pool.Evict(td)
				}
				startSilentRefresh()
			})
		}

		startSilentRefresh = func() {
			if !atomic.CompareAndSwapInt32(&silentRefreshActive, 0, 1) {
				return
			}
			capturedStop := poolRefreshStop
			capturedPool := dialerPool
			go func() {
				defer atomic.StoreInt32(&silentRefreshActive, 0)
				defer func() {
					if r := recover(); r != nil {
						stack := debug.Stack()
						core.Log.Printf("tunnel: silentRefresh PANIC: %v\n%s", r, stack)
					}
				}()
				deadline := time.Now().Add(2 * time.Minute)
				retryDelay := 5 * time.Second
				core.Log.Printf("tunnel: silent path refresh started (2 min deadline)")

				for time.Now().Before(deadline) {
					router.ProbeDataPlane(5 * time.Second)
					router.BuildPaths()
					// Check if disconnect happened while we were probing.
					select {
					case <-capturedStop:
						return
					default:
					}
					if capturedPool == nil {
						return
					}
					viable := buildViableAddrsTopup(router.QualifyingControlAddrs())
					var freshDialers []*core.TunnelDialer
					for _, addr := range viable {
						ctrlURL := ensureHTTPS(addr)
						a := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
						a.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
						a.SetKeyAuth(savedKey)
						if err := a.Login(); err == nil {
							td, tdErr := router.NewControlDialer(addr, a)
							if tdErr != nil {
								continue
							}
							wired := wireDialer(td)
							attachDataFailHook(wired)
							attachUDPFailedHook(wired)
							attachAuthFailHook(wired)
							freshDialers = append(freshDialers, td)
						}
					}
					// UDP relay pass: add controls reachable via hole-punched peers.
					{
						directSet := make(map[string]bool, len(viable))
						for _, a := range viable {
							directSet[ensureHTTPS(a)] = true
						}
						for _, path := range router.Paths() {
							if path.UDPRelay == nil || directSet[ensureHTTPS(path.ControlAddr)] {
								continue
							}
							directSet[ensureHTTPS(path.ControlAddr)] = true
							rd := core.NewUDPRelayDialer(path.UDPRelay, dialer.Auth())
							wireDialer(rd)
							attachDataFailHook(rd)
							attachUDPFailedHook(rd)
							attachAuthFailHook(rd)
							freshDialers = append(freshDialers, rd)
							core.Log.Printf("tunnel: silent refresh: UDP relay ctrl=%s via peer=%s", path.ControlAddr, path.Relays[0].Addr)
						}
					}
					if len(freshDialers) > 0 {
						select {
						case <-capturedStop:
							return
						default:
						}
						if capturedPool == nil {
							return
						}
						capturedPool.Swap(freshDialers)
						core.Log.Printf("tunnel: silent refresh succeeded pool=%d", len(freshDialers))
						core.SetTunnelHealthy(true)
						return
					}
					core.Log.Printf("tunnel: silent refresh attempt failed â€” retry in %v", retryDelay)
					select {
					case <-capturedStop:
						return
					case <-time.After(retryDelay):
					}
					if retryDelay < 30*time.Second {
						retryDelay *= 2
					}
				}
				core.Log.Printf("tunnel: silent refresh timed out â€” triggering full reconnect")
				ipc.TriggerReconnect()
			}()
		}

		// Attach fail hooks to every pool dialer regardless of transport mode.
		for _, td := range initialPoolDialers {
			attachDataFailHook(td)
			attachUDPFailedHook(td)
			attachAuthFailHook(td)
		}

		// Start SOCKS5 listener.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("SOCKS5 listen: %w", err)
		}
		socksLn = ln
		core.Log.Printf("SOCKS5 listening on %s", ln.Addr())

		// Start TUN.
		tunBridge = core.NewTUNBridge(ln.Addr().String())
		if err := tunBridge.Start(); err != nil {
			socksLn.Close()
			socksLn = nil
			tunBridge = nil
			return fmt.Errorf("TUN: %w", err)
		}

		// Apply routes. No DNS-bypass suppression needed any more â€” routes.go
		// never adds one (see its doc comment): DNS is always captured by TUN
		// and handled by snc/core/udp_assoc.go, DoH or not.
		routes = &snmac.RouteManager{}
		routes.Prepare(serverHost(effectiveURL))
		if err := routes.Apply(); err != nil {
			tunBridge.Stop()
			tunBridge = nil
			socksLn.Close()
			socksLn = nil
			routes = nil
			return fmt.Errorf("routes: %w", err)
		}

		// Add bypass routes for all control IPs.
		for _, ctrlAddr := range allCtrlAddrs {
			if ips, err := net.LookupHost(hostNameOf(ctrlAddr)); err == nil {
				for _, ip := range ips {
					routes.AddBypass(ip)
				}
			}
		}

		// Apply DNS.
		dns = &snmac.DNSManager{}
		if err := dns.Apply(); err != nil {
			core.Log.Printf("warn: DNS: %v", err)
		}

		// pf firewall: block DNS on the physical NIC to prevent leaks if routes fail.
		if err := pfwMgr.Apply(routes.PhysIface()); err != nil {
			core.Log.Printf("warn: firewall: %v", err)
		}

		// Persist state for watchdog crash recovery.
		if err := core.WriteWatchdogState(core.WatchdogState{
			Connected:     true,
			OrigGW:        routes.OrigGW(),
			MainPID:       os.Getpid(),
			TunnelHealthy: true,
		}); err != nil {
			core.Log.Printf("warn: write watchdog state: %v", err)
		}

		// Decoy traffic: fire background HTTPS GETs bound to the physical NIC
		// so they never consume tunnel bandwidth.
		decoyMgr = core.NewDecoyManager(routes.LocalAddr())
		dialer.SetActivityHook(decoyMgr.MarkActivity)
		decoyMgr.Start()
		core.Log.Printf("connect: decoy manager started physIP=%s", routes.LocalAddr())

		// Start SOCKS5 server: spread connections across all qualifying controls via pool.
		capturedBypassMgr := bypassMgr
		socks5BypassMgr := capturedBypassMgr
		if ipc.IsWildcatEnabled() {
			socks5BypassMgr = nil // bypass disabled in WildCat mode: all traffic must use TURN
		}
		socks5 = core.NewSOCKS5ServerWithPool("", dialerPool, socks5BypassMgr)
		// Use the user's explicit toggle; fall back to CC-based default if no choice saved yet.
		if settings.BlockQUIC != nil {
			socks5.BlockQUIC = *settings.BlockQUIC
		} else {
			cc := lastKnownCountry
			socks5.BlockQUIC = cc == "RU" || cc == "CN"
		}
		if socks5.BlockQUIC {
			core.Log.Printf("connect: QUIC (UDP:443) blocked (blockQUIC=%v)", socks5.BlockQUIC)
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
		if !ipc.IsWildcatEnabled() {
			socks5.RealtimeUDPDialer = core.NewQUICRelayDialer(strings.TrimPrefix(effectiveURL, "https://"), dialer.Auth())
			core.Log.Printf("connect: realtime UDP trial dialer ready via %s", effectiveURL)
		}
		go socks5.Serve(socksLn) //nolint:errcheck

		// Pool management: RTT-based promotion + drain completion.
		if poolRefreshStop != nil {
			close(poolRefreshStop)
		}
		poolRefreshStop = make(chan struct{})
		dialerPool.StartManagement(10*time.Second, poolRefreshStop)

		// Pool refill: add missing qualifying controls every 15 s.
		capturedRefillStop := poolRefreshStop
		capturedRefillPool := dialerPool
		go func() {
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					core.Log.Printf("connect: poolRefill PANIC: %v\n%s", r, stack)
				}
			}()
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-capturedRefillStop:
					return
				case <-t.C:
				}
				if capturedRefillPool == nil {
					return
				}
				target := len(router.QualifyingControlAddrs())
				if !capturedRefillPool.NeedsRefill(target) {
					continue
				}
				core.Log.Printf("connect: pool below target (%d/%d) â€” refilling", capturedRefillPool.Size(), target)
				router.ProbeDataPlane(5 * time.Second)
				router.BuildPaths()
				// Re-check after blocking probe.
				select {
				case <-capturedRefillStop:
					return
				default:
				}
				if capturedRefillPool == nil {
					return
				}
				for _, addr := range buildViableAddrs(router.QualifyingControlAddrs()) {
					ctrlURL := ensureHTTPS(addr)
					if capturedRefillPool.Has(ctrlURL) {
						continue
					}
					a := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
					a.SetDeviceInfo(savedKey.KeyID, deviceID, "macOS")
					a.SetKeyAuth(savedKey)
					if err := a.Login(); err != nil {
						continue
					}
					td, err := router.NewControlDialer(addr, a)
					if err != nil {
						continue
					}
					wired := wireDialer(td)
					attachDataFailHook(wired)
					attachUDPFailedHook(wired)
					attachAuthFailHook(wired)
					capturedRefillPool.Add(td)
				}
			}
		}()

		// Data-plane watchdog: force silent refresh if pool has no data for 30 s.
		{
			capturedWdStop := poolRefreshStop
			capturedWdPool := dialerPool
			go func() {
				defer func() {
					if r := recover(); r != nil {
						stack := debug.Stack()
						core.Log.Printf("tunnel: dataWatchdog PANIC: %v\n%s", r, stack)
					}
				}()
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-capturedWdStop:
						return
					case <-ticker.C:
						if capturedWdPool == nil {
							return
						}
						last := capturedWdPool.LastDataTime()
						if !last.IsZero() && time.Since(last) > 30*time.Second {
							core.Log.Printf("tunnel: watchdog: no data for %s â€” silent refresh", time.Since(last).Round(time.Second))
							startSilentRefresh()
						}
					}
				}
			}()
		}

		// Bypass manager: now routes are up, bind to physical NIC.
		if capturedBypassMgr != nil {
			capturedBypassMgr.SetLocalIP(routes.LocalAddr())
			capturedBypassMgr.SetToken(dialer.Token())
		}

		// Log upload: ship recent logs straight to the arbiter (navlink.net)
		// over the live tunnel every 5 minutes -- see log_upload.go for why
		// (removed the control/exit relay hop, which saw plaintext content).
		if logUploader != nil {
			logUploader.Stop()
		}
		logUploader = core.NewLogUploader(nodeID, "macos")
		logUploader.Start(
			func() *core.TunnelDialer {
				if dialerPool == nil {
					return nil
				}
				return dialerPool.Pick()
			},
			func() bool {
				return ipc.IsWildcatEnabled()
			},
		)

		// Connection-stats upload: same channel/cadence as log upload above,
		// separate endpoint -- see core.ConnStatsUploader. connStatsCollector
		// itself lives for the whole process (declared once near router at
		// the top of main), only the uploader is recreated per connect.
		if connStatsUploader != nil {
			connStatsUploader.Stop()
		}
		connStatsUploader = core.NewConnStatsUploader(connStatsCollector, dialerPool, router, nodeID, "macos", savedKey.Username)
		connStatsUploader.Start(
			func() *core.TunnelDialer {
				if dialerPool == nil {
					return nil
				}
				return dialerPool.Pick()
			},
			func() bool {
				return ipc.IsWildcatEnabled()
			},
		)

		// Country checker: propagate bypass CIDR result to router and recheck every 5 min.
		stop := make(chan struct{})
		countryCheckerStop = stop
		capturedAdir := adir
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
				saveCountry(capturedAdir, cc)
				if dialer != nil {
					dialer.SetClientCC(cc)
				}
				if prev != "" && prev != cc {
					core.Log.Printf("router: country changed %q â†’ %q â€” triggering reconnect", prev, cc)
					ipc.TriggerReconnect()
				} else {
					router.BuildPaths()
				}
			}
			discoveredMu.RLock()
			skipCIDRInit := gpsDetected || settings.PreferredRegion != ""
			discoveredMu.RUnlock()
			if !skipCIDRInit {
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

		// Update checker.
		updater := core.NewUpdater(getControls)
		updater.OnReady = func(v string) {
			ipc.PushUpdate(v)
		}
		updater.Start()

		core.Log.Printf("connected: srvURL=%s region=%q", serverURL, settings.PreferredRegion)
		connStatsCollector.IncConnect(!autoReconnect)
		// WildCat mode is decided once, at connect time -- not something that
		// flips mid-session, so it's safe to check it here to start the
		// session-duration clock. onDisconnect below closes it out.
		if ipc.IsWildcatEnabled() {
			connStatsCollector.StartWildcatSession()
		}

		// Start the once-a-second uplink/downlink counter push to the tray
		// (core.TotalBytes() only exists meaningfully here in the daemon --
		// see BytesSent/BytesRecv's doc comment in macos/ipc.go). Stopped in
		// onDisconnect below; if a ticker is already running (e.g. a
		// reconnect that never tore down cleanly) stop it first so two
		// tickers never race pushes.
		if byteTickerStop != nil {
			close(byteTickerStop)
		}
		byteTickerStop = make(chan struct{})
		go func(stop chan struct{}) {
			t := time.NewTicker(1 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					sent, recv := core.TotalBytes()
					ipc.PushBytes(sent, recv)
				}
			}
		}(byteTickerStop)

		return nil
	}

	// â”€â”€ onDisconnect â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	onDisconnect := func(autoReconnect bool) {
		// Stop the uplink/downlink counter ticker started in onConnect, and
		// tell the tray to hide/zero the display -- core.TotalBytes() keeps
		// counting in the background (it's process-wide, not session-scoped),
		// but nothing should look "live" once there's no tunnel.
		if byteTickerStop != nil {
			close(byteTickerStop)
			byteTickerStop = nil
		}
		ipc.PushBytes(0, 0)

		// No-op if no WildCat session is active (regular connect).
		connStatsCollector.IncDisconnect(!autoReconnect)
		if autoReconnect {
			core.Log.Println("disconnecting... (auto-reconnect)")
		} else {
			core.Log.Println("disconnecting... (user-initiated)")
			// Persist intent so a watchdog restart after this disconnect does not
			// auto-reconnect â€” the user explicitly asked to be disconnected.
			os.WriteFile(filepath.Join(adir, "user_disconnected"), []byte{}, 0600) //nolint:errcheck
		}
		core.WriteWatchdogState(core.WatchdogState{MainPID: os.Getpid()}) //nolint:errcheck
		router.CloseAllUDPPeers()
		router.SetMyCountry("")

		if countryCheckerStop != nil {
			close(countryCheckerStop)
			countryCheckerStop = nil
		}
		if dhtMergerStop != nil {
			close(dhtMergerStop)
			dhtMergerStop = nil
		}
		if poolRefreshStop != nil {
			close(poolRefreshStop)
			poolRefreshStop = nil
		}
		if bypassMgr != nil {
			bypassMgr.Stop()
			bypassMgr = nil
		}
		if logUploader != nil {
			logUploader.Stop()
			logUploader = nil
		}
		if decoyMgr != nil {
			decoyMgr.Stop()
			decoyMgr = nil
		}
		if socks5 != nil {
			socks5.WildcatDNS = false
			socks5.Close() //nolint:errcheck
			socks5 = nil
		}
		if socksLn != nil {
			socksLn.Close()
			socksLn = nil
		}
		if tunBridge != nil {
			tunBridge.Stop()
			tunBridge = nil
		}
		if routes != nil {
			routes.Remove()
			routes = nil
		}
		if dns != nil {
			dns.Restore()
			dns = nil
		}
		pfwMgr.Remove()
		dialerPool = nil
		core.Log.Printf("disconnected")
	}

	onBlockQUICChange := func(v bool) {
		settings.BlockQUIC = &v
		saveClientSettings(adir, settings)
		core.Log.Printf("Disable QUIC: %v - applying live", v)
		if socks5 != nil {
			socks5.BlockQUIC = v
		}
	}
	onRegionChange := func(r string) {
		settings.PreferredRegion = r
		saveClientSettings(adir, settings)
		discoveredMu.Lock()
		if r == "" {
			// Restored to Auto: reapply GPS country if known.
			if gpsCountry != "" {
				lastKnownCountry = gpsCountry
			}
		} else {
			lastKnownCountry = r
		}
		discoveredMu.Unlock()
		ipc.TriggerReconnect()
	}

	// â”€â”€ Power events â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	snmac.WatchPowerEvents(func() {
		core.Log.Printf("power: wake â€” triggering reconnect")
		ipc.TriggerReconnect()
	})

	// â”€â”€ Tray init / relaunch monitor â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// Sends init to every tray process that connects (initial launch or relaunch
	// after the process is killed, e.g. during system sleep).  When the tray
	// disconnects unexpectedly, relaunches it after a short delay.
	initBlockQUIC := lastKnownCountry == "RU" || lastKnownCountry == "CN"
	if settings.BlockQUIC != nil {
		initBlockQUIC = *settings.BlockQUIC
	}
	go func() {
		var lastConn *snmac.IPCConn
		firstConnect := true
		for {
			ipc.mu.Lock()
			conn := ipc.conn
			ipc.mu.Unlock()

			if conn != lastConn {
				prev := lastConn
				lastConn = conn

				if conn != nil {
					// New tray connection â€” send init.
					autoConnect := settings.AutoConnect
					if firstConnect && watchdogRestart {
						// On watchdog restart, honour any explicit user disconnect.
						if _, err := os.Stat(filepath.Join(adir, "user_disconnected")); err == nil {
							autoConnect = false
							core.Log.Printf("watchdog restart: user_disconnected flag â€” suppressing auto-connect")
						}
					}
					firstConnect = false
					ipc.SendInit(core.Version, logDir,
						initialLogin, autoConnect,
						settings.DOHEnabled, initBlockQUIC,
						settings.PreferredRegion)
				} else if uid, sock := getTrayIPC(); prev != nil && uid != "" {
					// Tray process died â€” relaunch it after a short pause.
					go func() {
						time.Sleep(1 * time.Second)
						core.Log.Printf("tray: process exited â€” relaunching")
						startTrayProcess(uid, sock, true)
					}()
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// â”€â”€ IPC command loop (replaces trayApp.Run) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	for {
		select {
		case cmd := <-ipc.cmdCh:
			switch cmd.T {
			case "connect":
				core.Log.Printf("ipc: connect autoReconnect=%v", cmd.AutoReconnect)
				ipc.PushStatus("pending", "")
				go func(auto bool) {
					if err := onConnect(auto); err != nil {
						core.Log.Printf("connect: %v", err)
						ipc.PushStatus("error", err.Error())
					} else {
						core.Log.Printf("ipc: connect succeeded, pushing connected")
						ipc.PushStatus("connected", "")
					}
				}(cmd.AutoReconnect)

			case "disconnect":
				core.Log.Printf("ipc: disconnect autoReconnect=%v", cmd.AutoReconnect)
				ipc.PushStatus("pending", "Disconnectingâ€¦")
				go func(auto bool) {
					onDisconnect(auto)
					core.Log.Printf("ipc: disconnect done, pushing idle (autoReconnect=%v)", auto)
					ipc.PushStatus("idle", "")
				}(cmd.AutoReconnect)

			case "recommend":
				// Forwarded from the tray's Settings-panel form (see
				// tunnel_cat/docs/club-membership.md) -- only the daemon
				// holds the session token needed to call the arbiter.
				if dialer == nil || serverURL == "" {
					core.Log.Printf("club-recommend: no active session, dropping recommendation for %s", cmd.TargetUsername)
				} else {
					go func(target string) {
						if err := core.RecommendCatClubMember(serverURL, dialer.Token, target); err != nil {
							core.Log.Printf("club-recommend: %s: %v", target, err)
						} else {
							core.Log.Printf("club-recommend: recommended %s for Cat Club", target)
						}
					}(cmd.TargetUsername)
				}

			case "club_theme_preview":
				// Admin-only theme preview override, selected from the tray's
				// Club Theme submenu. setClubThemePreview itself re-checks
				// isAdmin server-side and no-ops otherwise.
				if setClubThemePreview != nil {
					setClubThemePreview(cmd.PreviewTheme)
				}

			case "key":
				// User submitted a key (from login menu or ask_key response).
				go func(keyStr string) {
					if err := onLoginWithKey(keyStr); err != nil {
						core.Log.Printf("login: %v", err)
						ipc.PushStatus("error", err.Error())
						return
					}
					ipc.PushStatus("logged_in", "")
					if settings.AutoConnect {
						ipc.PushStatus("pending", "")
						if err := onConnect(false); err != nil {
							ipc.PushStatus("error", err.Error())
						} else {
							ipc.PushStatus("connected", "")
						}
					}
				}(cmd.Key)

			case "key_cancel":
				// Put empty string on keyCh in case AskKey() is waiting.
				select {
				case ipc.keyCh <- "":
				default:
				}

			case "logout":
				go func() {
					onDisconnect(false)
					onLogout()
					ipc.PushStatus("logged_out", "")
				}()

			case "settings":
				core.Log.Printf("ipc: settings blockQUIC=%v region=%q doh=%v", cmd.BlockQUIC, cmd.Region, cmd.DOH)
				onBlockQUICChange(cmd.BlockQUIC)
				onRegionChange(cmd.Region)

			case "wildcat":
				// Tray toggled WildCat mode. Trigger a full reconnect so onConnect
				// picks up the change on the new call.
				core.Log.Printf("ipc: wildcat toggled enabled=%v", cmd.WildcatEnabled)
				go func(enable bool) {
					onDisconnect(true)
					if enable {
						ipc.PushStatus("pending", "")
						if err := onConnect(true); err != nil {
							core.Log.Printf("wildcat: connect: %v", err)
							ipc.PushStatus("error", err.Error())
						} else {
							ipc.PushStatus("connected", "")
						}
					} else {
						ipc.PushStatus("idle", "")
					}
				}(cmd.WildcatEnabled)

			case "quit":
				core.Log.Printf("ipc: quit received from tray")
				goto shutdown
			}

		case <-ipc.reconnectCh:
			go func() {
				onDisconnect(true)
				ipc.PushStatus("pending", "")
				if err := onConnect(true); err != nil {
					ipc.PushStatus("error", err.Error())
				} else {
					ipc.PushStatus("connected", "")
				}
			}()
		}
	}

shutdown:
	close(stopWatchdogMonitor)
	snmac.SignalCleanShutdown()
	core.Log.Println("clean shutdown signaled")
}

// â”€â”€ Helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// loggedInUID returns the UID string of the console (GUI) user, or "".
func loggedInUID() string {
	out, err := exec.Command("/usr/bin/stat", "-f", "%u", "/dev/console").Output()
	if err != nil {
		return ""
	}
	uid := strings.TrimSpace(string(out))
	if uid == "0" {
		return "" // nobody logged in graphically
	}
	return uid
}

// daemonRespondsToPing reports whether a daemon is not just listening on
// socketPath but actually answers a "ping" with "pong" within a short
// deadline -- the real health check that replaced a bare socket-connect
// test at the caller (see its doc comment for why the difference matters).
// The probe connection is fully closed before returning, so it never
// lingers to compete with the real tray connection accepted right after.
func daemonRespondsToPing(socketPath string) bool {
	c, err := net.DialTimeout("unix", socketPath, 300*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(1500 * time.Millisecond)) //nolint:errcheck
	conn := snmac.NewIPCConn(c)
	if err := conn.SendCmd(snmac.IPCCmd{T: "ping"}); err != nil {
		return false
	}
	msg, err := conn.ReadMsg()
	if err != nil {
		return false
	}
	return msg.T == "pong"
}

func relaunchAsRoot() {
	exe, err := os.Executable()
	if err != nil {
		debugLog(fmt.Sprintf("relaunchAsRoot: os.Executable error: %v", err))
		fmt.Fprintln(os.Stderr, "relaunchAsRoot:", err)
		return
	}
	home := os.Getenv("HOME")
	debugLog(fmt.Sprintf("relaunchAsRoot: exe=%s home=%s", exe, home))
	cmd := fmt.Sprintf(`%s --user-home %s`, shellEscape(exe), shellEscape(home))
	script := fmt.Sprintf(`do shell script "%s" with administrator privileges`, cmd)
	out, oerr := exec.Command("osascript", "-e", script).CombinedOutput()
	debugLog(fmt.Sprintf("relaunchAsRoot: osascript done err=%v output=%q", oerr, string(out)))
}

func ensureSingleInstance() bool {
	p := filepath.Join(appDataDir(), "snc_main.lock")
	os.MkdirAll(filepath.Dir(p), 0700) //nolint:errcheck
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return true
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		// Another instance holds the lock â€” kill it and retry.
		if pid := snmac.ReadPIDFile(); pid > 0 {
			core.Log.Printf("single-instance: killing old pid %d", pid)
			snmac.TerminateProcess(pid)
			snmac.WaitProcessTimeout(pid, 5*time.Second)
		}
		f2, err2 := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
		if err2 != nil {
			return true
		}
		if err2 := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err2 != nil {
			f2.Close()
			return false
		}
		// Leak f2 intentionally.
		return true
	}
	// File handle intentionally leaked â€” OS releases flock on process exit.
	return true
}

func killStaleProcesses() {
	myPID := os.Getpid()
	out, err := exec.Command("pgrep", "-x", "shortnerdcat").Output()
	if err != nil {
		return
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid == myPID {
			continue
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		core.Log.Printf("startup: terminating stale pid=%d", pid)
		syscall.Kill(pid, syscall.SIGTERM) //nolint:errcheck
	}
	time.Sleep(500 * time.Millisecond)
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
	}
	core.Log.Printf("startup: killed %d stale process(es)", len(pids))
}

func showKeyDialog() (string, error) {
	prompt := strings.ReplaceAll(snmac.T("key_dialog_prompt"), `"`, `\"`)
	cancelBtn := strings.ReplaceAll(snmac.T("key_dialog_cancel"), `"`, `\"`)
	okBtn := strings.ReplaceAll(snmac.T("key_dialog_ok"), `"`, `\"`)
	script := `text returned of (display dialog "` + prompt + `" ` +
		`default answer "" with hidden answer buttons {"` + cancelBtn + `", "` + okBtn + `"} default button "` + okBtn + `")`
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func manifestCacheFile() string {
	return filepath.Join(appDataDir(), "manifest-cache.json")
}

func ensureHTTPS(addr string) string {
	if strings.HasPrefix(addr, "https://") || strings.HasPrefix(addr, "http://") {
		return addr
	}
	return "https://" + addr
}

// serverHost extracts the bare hostname from a server URL.
func serverHost(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	if h, _, ok := strings.Cut(u, ":"); ok {
		return h
	}
	return u
}

// hostNameOf strips scheme, path, and port from a URL.
func hostNameOf(u string) string {
	h := strings.TrimPrefix(u, "https://")
	h = strings.TrimPrefix(h, "http://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func shellEscape(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

// â”€â”€ Settings â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type clientSettings struct {
	AutoConnect     bool   `json:"auto_connect"`
	DOHEnabled      bool   `json:"doh_enabled"`
	BlockQUIC       *bool  `json:"block_quic,omitempty"` // nil = CC-based default; explicit = user override
	PreferredRegion string `json:"preferred_region,omitempty"`
}

func loadClientSettings(dir string) clientSettings {
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return clientSettings{AutoConnect: true, DOHEnabled: true}
	}
	var s clientSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return clientSettings{AutoConnect: true, DOHEnabled: true}
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

// â”€â”€ Persistence helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func loadOrCreateDeviceID(dir string) string {
	path := filepath.Join(dir, "device_id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	id := uuid.New().String()
	os.WriteFile(path, []byte(id), 0600) //nolint:errcheck
	return id
}

func saveCountry(dir, cc string) {
	os.WriteFile(filepath.Join(dir, "country.txt"), []byte(cc), 0600) //nolint:errcheck
}

func loadCountry(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "country.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func notifSeenFile() string {
	return filepath.Join(appDataDir(), "notifications_seen.json")
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
	os.MkdirAll(filepath.Dir(notifSeenFile()), 0700) //nolint:errcheck
	os.WriteFile(notifSeenFile(), data, 0600)        //nolint:errcheck
}

func showUserNotifications(notifs []core.Notification) {
	seen := loadNotifSeen()
	now := time.Now().Unix()
	var msgs []string
	for _, n := range notifs {
		if seen[n.ID] || now-n.CreatedAt > 24*3600 {
			continue
		}
		msgs = append(msgs, n.Message)
		seen[n.ID] = true
	}
	if len(msgs) == 0 {
		return
	}
	saveNotifSeen(seen)
	snmac.ShowNotifications(msgs)
}

// waitForCountry polls bm.Country() until non-empty or timeout.
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

// openLogsToUser makes the log directory and all files inside readable by
// the regular (non-root) user. The process runs as root, so log files are
// created with root ownership; this corrects that so "tail -f" works without
// sudo.
// openLogsInFinder opens logDir in Finder as the logged-in (non-root) user.
// The app runs as root, so we use launchctl asuser <uid> open <path> to
// deliver the open request to the user's session.
func openLogsInFinder(logDir string) {
	out, err := exec.Command("/usr/bin/stat", "-f", "%u", "/dev/console").Output()
	if err != nil {
		core.Log.Printf("share logs: stat /dev/console: %v", err)
		return
	}
	uid := strings.TrimSpace(string(out))
	if err := exec.Command("/bin/launchctl", "asuser", uid, "/usr/bin/open", logDir).Run(); err != nil {
		core.Log.Printf("share logs: open %s: %v", logDir, err)
	}
}

func openLogsToUser(logDir string) {
	// Make the parent appdata dir traversable (x bits) so the user process
	// can reach the logs subdirectory even though root owns it.
	os.Chmod(filepath.Dir(logDir), 0711) //nolint:errcheck
	// 0755 dir: user can list and enter; files get 0644.
	os.Chmod(logDir, 0755) //nolint:errcheck
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			os.Chmod(filepath.Join(logDir, e.Name()), 0644) //nolint:errcheck
		}
	}
}
