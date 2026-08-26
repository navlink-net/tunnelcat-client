// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
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

	snlin "shortnerdcat/snc/linux/linux"
	snmac "shortnerdcat/snc/mac/macos"
	"shortnerdcat/snc/shared/keymigrate"
	"tunnel_cat/snc/core"
)

// init overrides APPDATA so all daemon data files land in /var/lib/shortnerdcat.
// This is the system-level equivalent of ~/.shortnerdcat on macOS.
// A --data-dir flag can override it for testing.
func init() {
	dir := "/var/lib/shortnerdcat"
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--data-dir" && i+1 < len(os.Args) {
			dir = os.Args[i+1]
			break
		}
	}
	os.Setenv("APPDATA", dir)
}

func appDataDir() string { return os.Getenv("APPDATA") }

var ipc *ipcServer

func debugLog(msg string) {
	f, err := os.OpenFile("/tmp/snc_debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s: [main] uid=%d pid=%d %s\n",
		time.Now().Format("2006-01-02 15:04:05"), os.Getuid(), os.Getpid(), msg)
	f.Close()
}

func main() {
	debugLog(fmt.Sprintf("started args=%v", os.Args))

	// â”€â”€ CLI subcommands â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "status", "connect", "disconnect", "key", "logs":
			runCLIClient(os.Args[1:])
			return
		}
	}

	// â”€â”€ Deep-link: navlink://activate?key=... â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// Invoked by xdg-open when the user clicks a navlink:// URI. Write the key
	// to a pending-key file in XDG_RUNTIME_DIR; the tray process polls for it.
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "navlink://") {
			handleDeepLink(arg)
			return
		}
	}

	// â”€â”€ --watchdog mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if len(os.Args) > 1 && os.Args[1] == "--watchdog" {
		debugLog("entering watchdog mode")
		runAsWatchdog()
		return
	}

	watchdogRestart := len(os.Args) > 1 && os.Args[1] == "--restarted"

	// --controls addr1,addr2,... â€” restrict which controls are used (debug).
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

	// â”€â”€ --tray mode: UI process running as the logged-in user â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

	// â”€â”€ 0a. Privilege check â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	debugLog(fmt.Sprintf("privilege check uid=%d", os.Getuid()))
	if os.Getuid() != 0 {
		socketPath := snmac.IPCSocketPath(fmt.Sprintf("%d", os.Getuid()))
		debugLog(fmt.Sprintf("not root, checking daemon %s", socketPath))
		if daemonRespondsToStatus() {
			debugLog("daemon responds to status, starting tray directly")
			runTrayProcess(socketPath, watchdogRestart)
			return
		}
		// A raw socket connect (the old check here) only proves something is
		// listening, not that it's actually processing requests -- a hung
		// daemon whose accept loop is still alive but stuck elsewhere passed
		// that check forever, so a user relaunch just reattached a fresh tray
		// to the same dead backend instead of ever killing+restarting it (see
		// killStaleProcesses/ensureSingleInstance below, on the root path
		// relaunchAsRoot leads to). Confirmed live 2026-08-17: a client's
		// watchdog log showed it re-attach to an old main pid on startup and
		// then went completely silent -- exactly this failure mode. A real
		// status round-trip catches that; a bare connect does not.
		debugLog("daemon not responding to status â€” relaunching as root for a clean restart")
		relaunchAsRoot()
		return
	}

	// â”€â”€ 0b. Apply pending OTA update â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	debugLog("root: applying pending update")
	core.ApplyPendingUpdate()

	// â”€â”€ 1. Logging â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	logDir := filepath.Join(appDataDir(), "logs")
	debugLog(fmt.Sprintf("logDir=%s", logDir))
	if err := core.InitLogging(logDir); err != nil {
		fmt.Fprintf(os.Stderr, "warn: logging init: %v\n", err)
	}
	openLogsToUser(logDir)
	if len(forcedControls) > 0 {
		core.Log.Printf("DEBUG: --controls override: %v", forcedControls)
	}

	// â”€â”€ 0c. Kill stale processes â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	killStaleProcesses()
	snlin.CleanupSplitRoutes("")

	// â”€â”€ 0d. Single-instance guard â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if !ensureSingleInstance() {
		core.Log.Printf("another instance is running â€” exiting")
		return
	}
	core.UpdateCleanup()
	core.UpdateSignalFunc = snlin.SignalUpdateRestart

	// Install systemd service on first run.
	if !snlin.IsAutostartRegistered() {
		if err := snlin.RegisterAutostart(); err != nil {
			core.Log.Printf("warn: autostart register: %v", err)
		}
	}

	// â”€â”€ 1b. Watchdog â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if err := core.WriteWatchdogState(core.WatchdogState{MainPID: os.Getpid()}); err != nil {
		core.Log.Printf("warn: write watchdog state: %v", err)
	}
	snlin.WritePIDFile(os.Getpid())

	var watchdogProc *os.Process
	if !snlin.WatchdogRunning() {
		if wp, err := snlin.StartWatchdog(); err != nil {
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
			if snlin.WatchdogRunning() {
				select {
				case <-stopWatchdogMonitor:
					return
				case <-time.After(30 * time.Second):
				}
				watchdogProc = nil
				continue
			}
			wp, err := snlin.StartWatchdog()
			if err != nil {
				core.Log.Printf("warn: restart watchdog: %v", err)
				watchdogProc = nil
			} else {
				watchdogProc = wp
				core.Log.Printf("watchdog: restarted pid=%d", wp.Pid)
			}
		}
	}()

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

	// â”€â”€ 2b. IPC: start tray process as logged-in user â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	ipc = newIPCServer()

	// startTrayProcess launches a new tray process as the logged-in user.
	startTrayProcess := func(sess *snlin.LoggedInSession, socketPath string, restarted bool) {
		if sess == nil {
			core.Log.Printf("warn: no session detected â€” tray icon will not appear")
			return
		}
		exe, _ := os.Executable()
		args := []string{"--tray", socketPath}
		if restarted {
			args = append(args, "--restarted")
		}
		uidNum, err := strconv.Atoi(sess.UID)
		if err != nil {
			core.Log.Printf("warn: invalid session UID %q: %v", sess.UID, err)
			return
		}
		trayLogPath := filepath.Join(logDir, "snc_tray.log")
		logFile, _ := os.OpenFile(trayLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)

		cmd := exec.Command(exe, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(uidNum),
				Gid: uint32(uidNum),
			},
		}
		env := []string{
			"HOME=" + sess.Home,
			"XDG_RUNTIME_DIR=/run/user/" + sess.UID,
		}
		if sess.Display != "" {
			env = append(env, "DISPLAY="+sess.Display)
		}
		if sess.DBus != "" {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS="+sess.DBus)
		}
		cmd.Env = env
		if logFile != nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
		if err := cmd.Start(); err != nil {
			core.Log.Printf("warn: start tray process: %v", err)
		} else {
			core.Log.Printf("tray: started for uid=%s socket=%s restarted=%v", sess.UID, socketPath, restarted)
		}
	}

	var ipcSession *snlin.LoggedInSession
	var ipcSocket string
	{
		sess := snlin.DetectLoggedInSession()
		if sess == nil {
			core.Log.Printf("warn: could not detect logged-in user â€” tray icon will not appear")
		} else {
			socketPath := snmac.IPCSocketPath(sess.UID)
			ln, err := snmac.IPCListen(socketPath, sess.UID)
			if err != nil {
				core.Log.Printf("warn: IPC listen: %v", err)
			} else {
				ipcSession = sess
				ipcSocket = socketPath
				go ipc.acceptLoop(ln)
				startTrayProcess(sess, socketPath, watchdogRestart)
			}
		}
	}

	// CLI socket â€” lets non-root users query status and send commands.
	go runCLIServer(cliSocketPath, logDir, ipc)

	// Live uplink/downlink byte counters for the app window (see
	// core.TotalBytes' doc comment: cumulative application-payload bytes
	// sent/received by this process since it started, updated in real time
	// by the tunnel data plane's RecordTunnelSent/RecordTunnelRecv calls in
	// tunnel_cat/snc/core/tunnel.go -- all of which run here in the daemon,
	// never in the tray process, since the daemon is the one holding the TUN
	// device, SOCKS5 server, and dialers). There is no existing periodic
	// status-push loop to piggyback on (PushStatus above is only ever called
	// at state-transition points), so this is a new daemon-lifetime ticker,
	// deliberately not scoped to onConnect/onDisconnect: ipc.PushBytes is a
	// no-op while no tray is attached or nothing has been sent yet, and the
	// window itself decides whether to display the counter based on the
	// connected status it already tracks (see app window's onStatusUpdate).
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for range t.C {
			sent, recv := core.TotalBytes()
			ipc.PushBytes(sent, recv)
		}
	}()

	// â”€â”€ 3. Discovery & auth setup â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

		// torrentSlotMagnet/torrentSlotHash track the CURRENT magnet/infohash
		// per slot (a software package slug, "manifest", or "versions") --
		// see the Windows client's identical comment (main_windows.go) for
		// the full rationale. Not user-facing: gated purely by the
		// arbiter's manifest torrent_enabled flag, independent of tunnel
		// state.
		torrentEngine     *core.TorrentEngine
		torrentOnce       sync.Once
		torrentSlotMagnet = make(map[string]string)
		torrentSlotHash   = make(map[string]metainfo.Hash)
		torrentMu         sync.Mutex
		torrentUpdateOnce sync.Once
		// wireTorrent/torrentUpdater are assigned later (after adir/
		// globalDisc/the daemon's updater are in scope); forward-declared
		// so wireDHT's fetch callback (defined earlier than the assignment)
		// can call wireTorrent on every manifest refresh.
		wireTorrent    func()
		torrentUpdater *core.Updater
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

	// torrentSyncSlot brings one named slot (a software package slug,
	// "manifest", or "versions") to the given magnet, stopping and deleting
	// the previous torrent for that slot if its magnet changed (a new
	// version got published) -- see the Windows client's identical
	// function for the full rationale.
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
			core.Log.Printf("torrent: add %s (%s): %v", label, magnet, err)
			return
		}
		if hadHash {
			if err := torrentEngine.Remove(prevHash, true); err != nil {
				core.Log.Printf("torrent: remove stale %s: %v", label, err)
			}
		}
		torrentMu.Lock()
		torrentSlotMagnet[slot] = magnet
		torrentSlotHash[slot] = newHash
		torrentMu.Unlock()
		core.Log.Printf("torrent: added %s", label)
	}
	// torrentCheckUpdate mirrors the Windows client's function of the same
	// name: once the "versions" torrent (see snc-arbiter/torrent_magnets.go
	// and deploy/torrent/sync-and-publish.sh's "versions" product) finishes
	// downloading, checks whether the "linux" slug's version is newer than
	// this build; if so, waits for that slug's own torrent (the published
	// .deb -- see ApplyTorrentDownloadedDeb's doc comment for why a .deb
	// and not a raw binary) to finish, extracts the binary from it, and
	// fires the same torrentUpdater.OnReady callback the HTTP-delivered
	// update path already uses -- identical update-available UX regardless
	// of which channel delivered the bytes.
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
			core.Log.Printf("torrent: parse versions.json: %v", err)
			return
		}
		entry, ok := versions["linux"]
		if !ok || !entry.Available || entry.Version == "" || entry.Version <= core.Version {
			return
		}
		torrentMu.Lock()
		wantHash, haveWant := torrentSlotHash["linux"]
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
			debPath := filepath.Join(dataDir, softwareName)
			if err := core.ApplyTorrentDownloadedDeb(debPath); err != nil {
				core.Log.Printf("torrent: apply update failed: %v", err)
				return
			}
			core.Log.Printf("torrent: update %s ready to install", entry.Version)
			if torrentUpdater != nil && torrentUpdater.OnReady != nil {
				torrentUpdater.OnReady(entry.Version)
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
			torrentEngine = core.NewTorrentEngine(filepath.Join(adir, "torrents"))
			if err := torrentEngine.Start(); err != nil {
				core.Log.Printf("torrent: engine start failed: %v", err)
				torrentEngine = nil
				return
			}
			core.Log.Printf("torrent: engine started")
			// See the Windows client's identical comment: globalDisc's
			// magnets are only populated after its first successful fetch,
			// which races this call -- poll independently rather than
			// depending on any one fetch-completion hook firing again.
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
					go snlin.ShowNotifications(newMsgs)
				}
			})
			globalDisc.Start(10 * time.Minute)
			if wireTorrent != nil {
				wireTorrent()
			}
		})
	}

	// initClubDiscovery creates one ClubDiscoverer per known club slug and
	// starts polling -- mirrors the Windows/macOS clients' function of the
	// same name (see tunnel_cat/docs/club-membership.md). Unlike
	// initDiscovery, this needs a live session token (tokenFn), so it can
	// only run after login, not at daemon startup. Safe to call multiple
	// times; runs only on the first call. Pushes theme/badge updates to the
	// tray process via ipc.PushClubTheme, which forwards them to the window
	// over the daemon <-> tray IPC socket.
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
			// pushes it to the window -- called after any membership change,
			// admin-status change, or preview-override change. The admin's
			// preview override only ever applies while isAdmin is true.
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
					// marker and no fabricated number.
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

	startDiscovery := func(srvURL string, kd *core.KeyData, node *core.DHTNode) {
		initDiscovery(srvURL, kd)
		wireDHT(node)
	}

	getControls := func() []string {
		if len(forcedControls) > 0 {
			return forcedControls
		}
		discoveredMu.RLock()
		defer discoveredMu.RUnlock()
		return append([]string{}, discoveredControls...)
	}

	// â”€â”€ Device & node identity â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	deviceID := loadOrCreateDeviceID(adir)
	core.Log.Printf("device ID: %.8s...", deviceID)

	nodeID, err := core.LoadOrGenNodeID(adir)
	if err != nil {
		core.Log.Printf("warn: node ID: %v", err)
		nodeID = "unknown"
	}
	core.Log.Printf("node ID: %.8s...", nodeID)

	var dhtNode *core.DHTNode
	if dhtID, err := core.ParseDHTID(nodeID); err == nil {
		if udpConn, err := net.ListenUDP("udp", &net.UDPAddr{}); err == nil {
			peersPath := filepath.Join(adir, "peers.json")
			dhtNode = core.NewDHTNode(dhtID, udpConn, peersPath)
			dhtNode.Bootstrap(nil)
			dhtNode.LoadRelays(filepath.Join(adir, "dht_relays.json")) //nolint:errcheck
			dhtNode.Start()
			core.Log.Printf("dht: node started id=%.8s... addr=%s", nodeID, udpConn.LocalAddr())
		}
	}

	router := core.NewRouter()

	// connStatsCollector lives for the whole process (created once, like
	// router above), not per-connect -- its event counters must accumulate
	// across reconnects and only get drained by the uploader's own tick
	// (see ConnStatsCollector.Snapshot).
	connStatsCollector := core.NewConnStatsCollector(filepath.Join(adir, "connstats.json"))

	if cc := loadCountry(adir); cc != "" {
		lastKnownCountry = cc
		core.Log.Printf("geo: loaded persisted country %q", cc)
	}

	settings := loadClientSettings(adir)

	if settings.PreferredRegion != "" {
		lastKnownCountry = settings.PreferredRegion
		router.SetMyCountry(settings.PreferredRegion)
	}

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

	var fwMgr snlin.FirewallManager

	var (
		socksLn           net.Listener
		socks5            *core.SOCKS5Server
		tunBridge         *core.TUNBridge
		routes            *snlin.RouteManager
		dns               *snlin.DNSManager
		dohProxy          *snlin.DoHProxy
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

	// wildcatEnabled and wildcatToken are set via "wildcat" IPC command from the tray.
	// Accessed only from the main IPC loop goroutine and onConnect/onDisconnect
	// goroutines started by it â€” not concurrently.
	var (
		wildcatEnabled = settings.WildcatEnabled
		wildcatToken   string
	)
	// wildcatEnabledAtomic mirrors wildcatEnabled for logUploader's background
	// ticker goroutine, which is genuinely concurrent with the IPC loop (unlike
	// the two goroutines the comment above scopes plain wildcatEnabled to) --
	// see logUploader.Start's wildcatActive param below.
	var wildcatEnabledAtomic atomic.Bool
	wildcatEnabledAtomic.Store(settings.WildcatEnabled)

	// Network monitor: detect gateway change (sleep/wake, WiFi handoff).
	netMon := snlin.NewNetworkMonitor(func() {
		core.Log.Printf("netmon: gateway changed â€” triggering reconnect")
		ipc.TriggerReconnect()
	})
	stopNetMon := netMon.Start()
	defer stopNetMon()

	// â”€â”€ Auto-login with saved key â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if keyStr, err := snlin.LoadKey(adir); err == nil {
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
				if saveErr := snlin.SaveKey(adir, newKeyStr); saveErr != nil {
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
			go startDiscovery(ensureHTTPS(bootstrapSeed), kd, dhtNode)

			authLogin := func(url string) *core.Authenticator {
				core.Log.Printf("auto-auth: user %s @ %s", kd.Username, url)
				a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
				a.SetKeyAuth(kd)
				a.SetDeviceInfo(kd.KeyID, deviceID, "Linux")
				if err := a.Login(); err != nil {
					core.Log.Printf("auto-auth: failed %s: %v", url, err)
					return nil
				}
				core.Log.Printf("auto-auth: OK %s token=%s...", url, a.Token()[:8])
				return a
			}
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
					if a := authLogin(u); a != nil {
						select {
						case resultCh <- authResult{a, u}:
						default:
						}
					}
				}(ensureHTTPS(node))
			}
			go func() { authWg.Wait(); close(resultCh) }()

			authOK := false
			if res, ok := <-resultCh; ok {
				authOK = true
				applyAutoAuth(res.a, res.url)
			}

			if !authOK {
				if cachedRelays, _ := core.LoadRelayList(filepath.Join(adir, "relays.json")); len(cachedRelays) > 0 {
					cc := loadCountry(adir)
					for _, relay := range core.RelaysByCountry(cachedRelays, cc) {
						if a := authLogin(ensureHTTPS(relay.Addr)); a != nil {
							authOK = true
							applyAutoAuth(a, ensureHTTPS(relay.Addr))
							break
						}
					}
				}
			}
			if !authOK {
				core.Log.Printf("auto-auth: all nodes unreachable")
			}
		}
	}

	initialLogin := savedKey != nil

	// â”€â”€ onLoginWithKey â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

		tryLoginAuth := func(url string) bool {
			core.Log.Printf("login: auth user %s @ %s", kd.Username, url)
			a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
			a.SetKeyAuth(kd)
			a.SetDeviceInfo(kd.KeyID, deviceID, "Linux")
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

		// A cached manifest (from a prior session, any account) is network-
		// wide and authoritative once it exists — see core.BootstrapControlList.
		loginNodes := core.BootstrapControlList(
			core.ReadManifestCacheControls(manifestCacheFile(), kd.ArbiterPubkey), kd.Nodes())
		if len(forcedControls) > 0 {
			loginNodes = forcedControls
		}
		authOK := false
		for _, node := range loginNodes {
			if tryLoginAuth(ensureHTTPS(node)) {
				authOK = true
				break
			}
		}
		if !authOK {
			if cachedRelays, _ := core.LoadRelayList(filepath.Join(adir, "relays.json")); len(cachedRelays) > 0 {
				cc := loadCountry(adir)
				for _, relay := range core.RelaysByCountry(cachedRelays, cc) {
					if tryLoginAuth(ensureHTTPS(relay.Addr)) {
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
		if err := snlin.SaveKey(adir, keyStr); err != nil {
			core.Log.Printf("warn: save key: %v", err)
		}
		return nil
	}

	onLogout := func() {
		savedKey = nil
		dialer = nil
		serverURL = ""
		os.Remove(filepath.Join(adir, "key.dat")) //nolint:errcheck
	}

	// â”€â”€ onConnect â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	onConnect := func(autoReconnect bool) error {
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

		if dialer == nil && savedKey != nil {
			srvURL := pickServerURL(savedKey)
			core.Log.Printf("connect: re-auth user %s @ %s", savedKey.Username, srvURL)
			a := core.NewAuthenticator(srvURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetKeyAuth(savedKey)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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
								ra.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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

		var fetchedRelays []core.RelayEntry
		if relays, err := core.FetchRelayList(relayAPIURL); err == nil {
			fetchedRelays = relays
			router.UpdateRelays(relays)
			core.SaveRelayList(filepath.Join(adir, "relays.json"), relays) //nolint:errcheck
		} else {
			core.Log.Printf("connect: relay list fetch failed (%v) â€” routing direct", err)
			router.UpdateRelays(nil)
		}

		if dhtNode != nil {
			seeds := make([]string, 0, len(fetchedRelays))
			for _, r := range fetchedRelays {
				seeds = append(seeds, r.Addr)
			}
			dhtNode.Bootstrap(seeds)

			stop := make(chan struct{})
			dhtMergerStop = stop
			go func() {
				t := time.NewTicker(5 * time.Minute)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						entries := dhtNode.Relays()
						if len(entries) > 0 {
							dhtNode.SaveRelays(filepath.Join(adir, "dht_relays.json")) //nolint:errcheck
							router.MergeDHTRelays(entries)
							router.ProbeDataPlane(3 * time.Second)
							router.BuildPaths()
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

		if better, p := router.PrimaryIsBetter(serverURL, 1.5); better && p != nil {
			preferred := ensureHTTPS(p.ControlAddr)
			a := core.NewAuthenticator(preferred, savedKey.APIKey, savedKey.Username, savedKey.Password)
			a.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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

		var myIPErr error
		publicIP, myIPErr = core.FetchMyIP(relayAPIURL)
		if myIPErr != nil {
			core.Log.Printf("connect: myIP fetch failed: %v", myIPErr)
		} else {
			core.Log.Printf("connect: myIP=%s", publicIP)
			dialer.SetClientIP(publicIP)
		}

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
							a.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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
				ra.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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

		if dialer != nil {
			dialer.ClearDialFunc()
			dialer.Auth().ClearDialFunc()
		}

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
			discoveredMu.RUnlock()
			var fallback []string
			for _, addr := range allControls {
				if !qualSet[addr] {
					fallback = append(fallback, addr)
				}
			}
			need := 5 - len(viable)
			extra := buildViableAddrs(fallback)
			if len(extra) > need {
				extra = extra[:need]
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
					a.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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
					core.Log.Printf("tunnel: first failure on %s â€” instant eviction", ctrlURL)
					pool.Evict(td)
					startSilentRefresh()
				}
			})
			td.SetDataFailHook(3, func() {
				core.Log.Printf("tunnel: data-plane failure on %s â€” starting silent refresh", ctrlURL)
				core.SetTunnelHealthy(false)
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
		// dialer's control node -- distinct from attachDataFailHook's
		// data-plane failures, which fire on Dial/stream outcomes and never
		// trigger for a dialer whose Authenticator can't complete a login at
		// all. Without this, such a dialer just retries the same fixed node
		// forever (refreshToken's own 3-hour patience window) even though the
		// router's RTT/liveness probe -- a different, unauthenticated
		// transport -- may still report it "alive" (e.g. TCP blocked but UDP
		// still answering /p/v1/ping; see the 2026-08-11 control incident).
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
						core.Log.Printf("tunnel: silentRefresh PANIC: %v\n%s", r, debug.Stack())
					}
				}()
				deadline := time.Now().Add(2 * time.Minute)
				retryDelay := 5 * time.Second
				core.Log.Printf("tunnel: silent path refresh started (2 min deadline)")

				for time.Now().Before(deadline) {
					router.ProbeDataPlane(5 * time.Second)
					router.BuildPaths()
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
						a.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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

		for _, td := range initialPoolDialers {
			attachDataFailHook(td)
			attachUDPFailedHook(td)
			attachAuthFailHook(td)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("SOCKS5 listen: %w", err)
		}
		socksLn = ln
		core.Log.Printf("SOCKS5 listening on %s", ln.Addr())

		tunBridge = core.NewTUNBridge(ln.Addr().String())
		if err := tunBridge.Start(); err != nil {
			socksLn.Close()
			socksLn = nil
			tunBridge = nil
			return fmt.Errorf("TUN: %w", err)
		}

		routes = &snlin.RouteManager{}
		dnsAddr := ""
		if ipc.IsDOH() {
			proxy := snlin.NewDoHProxy()
			if err := proxy.Start(); err != nil {
				core.Log.Printf("warn: DoH proxy: %v â€” falling back to plain DNS", err)
			} else {
				dohProxy = proxy
				dnsAddr = "127.0.0.1"
			}
			core.Log.Printf("connect: DoH mode")
		}
		routes.Prepare(serverHost(effectiveURL))
		if err := routes.Apply(); err != nil {
			tunBridge.Stop()
			tunBridge = nil
			socksLn.Close()
			socksLn = nil
			routes = nil
			return fmt.Errorf("routes: %w", err)
		}

		for _, ctrlAddr := range allCtrlAddrs {
			if ips, err := net.LookupHost(hostNameOf(ctrlAddr)); err == nil {
				for _, ip := range ips {
					routes.AddBypass(ip)
				}
			}
		}

		dns = &snlin.DNSManager{}
		if err := dns.Apply(routes.PhysIface(), dnsAddr); err != nil {
			core.Log.Printf("warn: DNS: %v", err)
		}

		if err := fwMgr.Apply(routes.PhysIface()); err != nil {
			core.Log.Printf("warn: firewall: %v", err)
		}

		if err := core.WriteWatchdogState(core.WatchdogState{
			Connected:     true,
			OrigGW:        routes.OrigGW(),
			MainPID:       os.Getpid(),
			TunnelHealthy: true,
		}); err != nil {
			core.Log.Printf("warn: write watchdog state: %v", err)
		}

		decoyMgr = core.NewDecoyManager(routes.LocalAddr())
		dialer.SetActivityHook(decoyMgr.MarkActivity)
		decoyMgr.Start()
		core.Log.Printf("connect: decoy manager started physIP=%s", routes.LocalAddr())

		capturedBypassMgr := bypassMgr
		socks5 = core.NewSOCKS5ServerWithPool("", dialerPool, capturedBypassMgr)
		if settings.BlockQUIC != nil {
			socks5.BlockQUIC = *settings.BlockQUIC
		} else {
			cc := lastKnownCountry
			socks5.BlockQUIC = cc == "RU" || cc == "CN"
		}
		if socks5.BlockQUIC {
			core.Log.Printf("connect: QUIC (UDP:443) blocked")
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
		core.Log.Printf("connect: realtime UDP trial dialer ready via %s", effectiveURL)
		go socks5.Serve(socksLn) //nolint:errcheck

		if poolRefreshStop != nil {
			close(poolRefreshStop)
		}
		poolRefreshStop = make(chan struct{})
		dialerPool.StartManagement(10*time.Second, poolRefreshStop)

		capturedRefillStop := poolRefreshStop
		capturedRefillPool := dialerPool
		go func() {
			defer func() {
				if r := recover(); r != nil {
					core.Log.Printf("connect: poolRefill PANIC: %v\n%s", r, debug.Stack())
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
				select {
				case <-capturedRefillStop:
					return
				default:
				}
				for _, addr := range buildViableAddrs(router.QualifyingControlAddrs()) {
					ctrlURL := ensureHTTPS(addr)
					if capturedRefillPool.Has(ctrlURL) {
						continue
					}
					a := core.NewAuthenticator(ctrlURL, savedKey.APIKey, savedKey.Username, savedKey.Password)
					a.SetDeviceInfo(savedKey.KeyID, deviceID, "Linux")
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

		capturedWdStop := poolRefreshStop
		capturedWdPool := dialerPool
		go func() {
			defer func() {
				if r := recover(); r != nil {
					core.Log.Printf("tunnel: dataWatchdog PANIC: %v\n%s", r, debug.Stack())
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
		logUploader = core.NewLogUploader(nodeID, "linux")
		logUploader.Start(
			func() *core.TunnelDialer {
				if dialerPool == nil {
					return nil
				}
				return dialerPool.Pick()
			},
			func() bool {
				return wildcatEnabledAtomic.Load()
			},
		)

		// Connection-stats upload: same channel/cadence as log upload above,
		// separate endpoint -- see core.ConnStatsUploader. connStatsCollector
		// itself lives for the whole process (declared once near router at
		// the top of main), only the uploader is recreated per connect.
		if connStatsUploader != nil {
			connStatsUploader.Stop()
		}
		connStatsUploader = core.NewConnStatsUploader(connStatsCollector, dialerPool, router, nodeID, "linux", savedKey.Username)
		connStatsUploader.Start(
			func() *core.TunnelDialer {
				if dialerPool == nil {
					return nil
				}
				return dialerPool.Pick()
			},
			func() bool {
				return wildcatEnabledAtomic.Load()
			},
		)

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

		updater := core.NewUpdater(getControls)
		updater.OnReady = func(v string) {
			ipc.PushUpdate(v)
		}
		updater.Start()
		torrentUpdater = updater

		core.Log.Printf("connected: srvURL=%s region=%q", serverURL, settings.PreferredRegion)
		// This is the non-WildCat connect path -- the WildCat branch further up
		// this function returns before ever reaching here (see its own
		// IncConnect/StartWildcatSession calls).
		connStatsCollector.IncConnect(!autoReconnect)
		return nil
	}

	// â”€â”€ onDisconnect â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	onDisconnect := func(autoReconnect bool) {
		connStatsCollector.IncDisconnect(!autoReconnect)
		if autoReconnect {
			core.Log.Println("disconnecting... (auto-reconnect)")
		} else {
			core.Log.Println("disconnecting... (user-initiated)")
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
		if dohProxy != nil {
			dohProxy.Stop()
			dohProxy = nil
		}
		if dns != nil {
			dns.Restore()
			dns = nil
		}
		fwMgr.Remove()
		dialerPool = nil
		core.Log.Printf("disconnected")
	}

	onBlockQUICChange := func(v bool) {
		settings.BlockQUIC = &v
		saveClientSettings(adir, settings)
		core.Log.Printf("Disable QUIC: %v â€” applying live", v)
		if socks5 != nil {
			socks5.BlockQUIC = v
		}
	}
	onRegionChange := func(r string) {
		settings.PreferredRegion = r
		saveClientSettings(adir, settings)
		discoveredMu.Lock()
		if r == "" {
			if gpsCountry != "" {
				lastKnownCountry = gpsCountry
			}
		} else {
			lastKnownCountry = r
		}
		discoveredMu.Unlock()
		ipc.TriggerReconnect()
	}

	// â”€â”€ Tray init / relaunch monitor â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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
					autoConnect := settings.AutoConnect
					if firstConnect && watchdogRestart {
						if _, err := os.Stat(filepath.Join(adir, "user_disconnected")); err == nil {
							autoConnect = false
							core.Log.Printf("watchdog restart: user_disconnected flag â€” suppressing auto-connect")
						}
					}
					firstConnect = false
					ipc.SendInit(core.Version, logDir,
						initialLogin, autoConnect,
						settings.DOHEnabled, initBlockQUIC,
						settings.WildcatEnabled,
						settings.PreferredRegion)
				} else if prev != nil && ipcSession != nil {
					sess, sock := ipcSession, ipcSocket
					go func() {
						time.Sleep(1 * time.Second)
						core.Log.Printf("tray: process exited â€” relaunching")
						startTrayProcess(sess, sock, true)
					}()
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// â”€â”€ IPC command loop â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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
						ipc.PushStatus("connected", "")
					}
				}(cmd.AutoReconnect)

			case "disconnect":
				core.Log.Printf("ipc: disconnect autoReconnect=%v", cmd.AutoReconnect)
				ipc.PushStatus("pending", "Disconnectingâ€¦")
				go func(auto bool) {
					onDisconnect(auto)
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
				// Admin-only theme preview override, selected from the
				// tray's Club Theme submenu. setClubThemePreview itself
				// re-checks isAdmin server-side and no-ops otherwise.
				if setClubThemePreview != nil {
					setClubThemePreview(cmd.PreviewTheme)
				}

			case "key":
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
				core.Log.Printf("ipc: settings blockQUIC=%v region=%q doh=%v",
					cmd.BlockQUIC, cmd.Region, cmd.DOH)
				onBlockQUICChange(cmd.BlockQUIC)
				onRegionChange(cmd.Region)

			case "wildcat":
				core.Log.Printf("ipc: wildcat enabled=%v tokenLen=%d", cmd.WildcatEnabled, len(cmd.WildcatToken))
				wildcatEnabled = cmd.WildcatEnabled
				wildcatEnabledAtomic.Store(cmd.WildcatEnabled)
				if cmd.WildcatToken != "" {
					wildcatToken = cmd.WildcatToken
				}
				settings.WildcatEnabled = cmd.WildcatEnabled
				saveClientSettings(adir, settings)
				// WildCat switches the underlying transport â€” reconnect to apply.

			case "quit":
				// Tray user clicked Quit. Write clean-shutdown flag so the
				// watchdog knows not to restart the daemon.
				snlin.SignalCleanShutdown()
				core.Log.Printf("ipc: quit â€” daemon exiting")
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
	snlin.SignalCleanShutdown()
	core.Log.Println("clean shutdown signaled")
}

// â”€â”€ Helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func relaunchAsRoot() {
	exe, err := os.Executable()
	if err != nil {
		debugLog(fmt.Sprintf("relaunchAsRoot: os.Executable error: %v", err))
		return
	}
	debugLog(fmt.Sprintf("relaunchAsRoot: exe=%s", exe))
	cmd := exec.Command("pkexec", exe)
	if err := cmd.Start(); err != nil {
		debugLog(fmt.Sprintf("relaunchAsRoot: pkexec: %v", err))
	}
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
		if pid := snlin.ReadPIDFile(); pid > 0 {
			core.Log.Printf("single-instance: killing old pid %d", pid)
			snlin.TerminateProcess(pid)
			snlin.WaitProcessTimeout(pid, 5*time.Second)
		}
		f2, err2 := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
		if err2 != nil {
			return true
		}
		if err2 := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err2 != nil {
			f2.Close()
			return false
		}
		return true
	}
	return true
}

func killStaleProcesses() {
	myPID := os.Getpid()
	out, err := exec.Command("pgrep", "-x", "shortnerdcat").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid == myPID {
			continue
		}
		core.Log.Printf("startup: terminating stale pid=%d", pid)
		syscall.Kill(pid, syscall.SIGTERM) //nolint:errcheck
	}
	time.Sleep(500 * time.Millisecond)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, _ := strconv.Atoi(strings.TrimSpace(line))
		if pid != 0 && pid != myPID {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
		}
	}
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

// handleDeepLink processes a navlink:// URI received from xdg-open.
// It parses the activation key and writes it to a pending-key file that the
// tray process polls; this avoids IPC protocol changes for an edge-case path.
func handleDeepLink(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "navlink" {
		debugLog(fmt.Sprintf("deep-link: malformed URL %q: %v", rawURL, err))
		return
	}
	key := u.Query().Get("key")
	if key == "" {
		debugLog(fmt.Sprintf("deep-link: no key= in %q", rawURL))
		return
	}
	dest := pendingKeyPath()
	if err := os.WriteFile(dest, []byte(key), 0600); err != nil {
		debugLog(fmt.Sprintf("deep-link: write pending key: %v", err))
		return
	}
	debugLog(fmt.Sprintf("deep-link: wrote pending key (%d chars) to %s", len(key), dest))
}

// pendingKeyPath returns the file the deep-link handler writes the key into
// and the tray process polls for.
func pendingKeyPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, "snc_pending_key")
}

// openLogsToUser makes the log directory readable by non-root users.
func openLogsToUser(logDir string) {
	os.Chmod(filepath.Dir(logDir), 0711) //nolint:errcheck
	os.Chmod(logDir, 0755)               //nolint:errcheck
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

// â”€â”€ Settings â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type clientSettings struct {
	AutoConnect     bool   `json:"auto_connect"`
	DOHEnabled      bool   `json:"doh_enabled"`
	BlockQUIC       *bool  `json:"block_quic,omitempty"`
	PreferredRegion string `json:"preferred_region,omitempty"`
	WildcatEnabled  bool   `json:"wildcat_enabled,omitempty"`
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

// â”€â”€ Persistence helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

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
	snlin.ShowNotifications(msgs)
}

// â”€â”€ CLI subcommand support â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

const cliSocketPath = "/var/lib/shortnerdcat/cli.sock"

type cliRequest struct {
	T   string `json:"t"`
	Key string `json:"key,omitempty"`
}

type cliResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	State   string `json:"state,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`
	LogDir  string `json:"log_dir,omitempty"`
	Latest  string `json:"latest,omitempty"`
}

// daemonRespondsToStatus reports whether a daemon is not just listening on
// cliSocketPath but actually answers a "status" request within a short
// deadline -- the real health check that replaced a bare socket-connect
// test at the caller (see its doc comment for why the difference matters).
func daemonRespondsToStatus() bool {
	c, err := net.DialTimeout("unix", cliSocketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(1500 * time.Millisecond)) //nolint:errcheck
	if err := json.NewEncoder(c).Encode(cliRequest{T: "status"}); err != nil {
		return false
	}
	var resp cliResponse
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return false
	}
	return resp.OK
}

// runCLIClient sends a command to the running daemon's CLI socket and prints
// the result. Called when the binary is invoked as "shortnerdcat <subcommand>".
func runCLIClient(args []string) {
	req := cliRequest{T: args[0]}
	if args[0] == "key" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: shortnerdcat key <KEY>")
			os.Exit(1)
		}
		req.Key = args[1]
	}

	c, err := net.DialTimeout("unix", cliSocketPath, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: daemon not running (%v)\n", err)
		os.Exit(1)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

	if err := json.NewEncoder(c).Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "error: send: %v\n", err)
		os.Exit(1)
	}

	var resp cliResponse
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "error: recv: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "error: %s\n", resp.Error)
		os.Exit(1)
	}

	switch req.T {
	case "status":
		if resp.Elapsed != "" {
			fmt.Printf("%s (%s)\n", resp.State, resp.Elapsed)
		} else {
			fmt.Println(resp.State)
		}
	case "logs":
		fmt.Println(resp.LogDir)
		if resp.Latest != "" {
			fmt.Println(filepath.Join(resp.LogDir, resp.Latest))
		}
	default:
		fmt.Println("ok")
	}
}

// runCLIServer listens on cliSocketPath and handles each CLI request in its
// own goroutine. The socket is world-readable so non-root users can query it.
func runCLIServer(socketPath, logDir string, srv *ipcServer) {
	os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		core.Log.Printf("cli: listen %s: %v", socketPath, err)
		return
	}
	os.Chmod(socketPath, 0666) //nolint:errcheck
	defer ln.Close()
	core.Log.Printf("cli: listening on %s", socketPath)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleCLIConn(c, logDir, srv)
	}
}

func handleCLIConn(c net.Conn, logDir string, srv *ipcServer) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

	var req cliRequest
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		return
	}

	var resp cliResponse
	switch req.T {
	case "status":
		state, elapsed := srv.CurrentStatus()
		resp.OK = true
		resp.State = state
		resp.Elapsed = elapsed

	case "connect":
		select {
		case srv.cmdCh <- snmac.IPCCmd{T: "connect"}:
			resp.OK = true
		default:
			resp.Error = "command queue full"
		}

	case "disconnect":
		select {
		case srv.cmdCh <- snmac.IPCCmd{T: "disconnect"}:
			resp.OK = true
		default:
			resp.Error = "command queue full"
		}

	case "key":
		if req.Key == "" {
			resp.Error = "key required"
		} else {
			select {
			case srv.cmdCh <- snmac.IPCCmd{T: "key", Key: req.Key}:
				resp.OK = true
			default:
				resp.Error = "command queue full"
			}
		}

	case "logs":
		resp.OK = true
		resp.LogDir = logDir
		if entries, err := os.ReadDir(logDir); err == nil {
			for i := len(entries) - 1; i >= 0; i-- {
				if !entries[i].IsDir() {
					resp.Latest = entries[i].Name()
					break
				}
			}
		}

	default:
		resp.Error = "unknown command: " + req.T
	}

	json.NewEncoder(c).Encode(resp) //nolint:errcheck
}

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
