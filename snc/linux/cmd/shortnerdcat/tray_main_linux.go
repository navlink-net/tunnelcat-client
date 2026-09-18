// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	snlin "shortnerdcat/snc/linux/linux"
	snmac "shortnerdcat/snc/mac/macos"
	"shortnerdcat/snc/shared/navlinkauth"
	"tunnel_cat/snc/core"
)

// runTrayProcess is the entry point for --tray mode (runs as the logged-in user).
// It connects to the IPC socket, creates TrayApp, and drives the systray UI.
func runTrayProcess(socketPath string, watchdogRestart bool) {
	// Force software rendering for every WebKit2GTK view this process ever
	// creates (the app window). Must be set before WebKit's
	// process-wide GL/compositing setup runs, which happens the moment
	// systray.Run (below) initializes GTK -- setting it here, first thing in
	// this dedicated --tray process, guarantees that.
	//
	// Repeated support reports (Aug 2026): on some Xubuntu/XFCE machines
	// (typically VMs or older Mesa/software-GL setups), WebKit's accelerated
	// compositor partially fails -- image layers still raster (users saw the
	// idle-cat illustration render fine) but text/box layers and pointer
	// input routing, which ride the same compositor, silently don't. No
	// error, no crash, just a dead-looking gray window. This is a known
	// webkit2gtk failure class on such setups, not a bug in this app's own
	// HTML/JS (see buildAppWindowHTML/webkit_web_view_load_html -- the whole
	// page is a single local string, no network/IPC load path to fail).
	os.Setenv("WEBKIT_DISABLE_COMPOSITING_MODE", "1")

	ipc, err := snmac.IPCDial(socketPath, 15*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray: connect IPC: %v\n", err)
		os.Exit(1)
	}

	// First message must be "init".
	initMsg, err := ipc.ReadMsg()
	if err != nil || initMsg.T != "init" {
		fmt.Fprintf(os.Stderr, "tray: bad init: t=%q err=%v\n", initMsg.T, err)
		os.Exit(1)
	}

	core.Version = initMsg.Version
	logDir := initMsg.LogDir

	sendCmd := func(cmd snmac.IPCCmd) {
		ipc.SendCmd(cmd) //nolint:errcheck
	}

	var trayApp *snlin.TrayApp
	var appWin *snlin.AppWindow
	nc := navlinkauth.New()

	onConnect := func(autoReconnect bool) error {
		sendCmd(snmac.IPCCmd{T: "connect", AutoReconnect: autoReconnect})
		return nil
	}
	onDisconnect := func(autoReconnect bool) {
		sendCmd(snmac.IPCCmd{T: "disconnect", AutoReconnect: autoReconnect})
	}
	// onLogin opens the app window which contains a built-in key-entry panel.
	// Returns a sentinel error so doLogin knows the key is being collected
	// asynchronously (the window's OnKey callback sends it when ready).
	onLogin := func() error {
		appWin.Show()
		return fmt.Errorf("key entry in progress")
	}
	onLogout := func() { sendCmd(snmac.IPCCmd{T: "logout"}) }
	onBlockQUICChange := func(v bool) {
		sendCmd(snmac.IPCCmd{T: "settings", BlockQUIC: v, DOH: trayApp.IsDNSOverHTTPSEnabled()})
	}
	onRegionChange := func(r string) {
		sendCmd(snmac.IPCCmd{T: "settings", Region: r, DOH: trayApp.IsDNSOverHTTPSEnabled()})
	}

	onWildcatChange := func(enabled bool) {
		sendCmd(snmac.IPCCmd{T: "wildcat", WildcatEnabled: enabled})
	}

	trayApp = snlin.NewTrayApp(
		initMsg.Version,
		initMsg.InitLogin,
		initMsg.AutoConnect,
		initMsg.DOH,
		initMsg.BlockQUIC,
		initMsg.WildcatEnabled,
		initMsg.Region,
		onLogin, onLogout, onConnect, onDisconnect,
		nil, // onDNSOverHTTPSChange â€” sent via settings cmd
		onBlockQUICChange, onWildcatChange, onRegionChange,
	)

	trayApp.SetShareLogsCallback(func() {
		// Open the log directory with xdg-open (works on most desktop environments).
		exec.Command("xdg-open", logDir).Start() //nolint:errcheck
	})

	// â”€â”€ App window (WebKit2GTK) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// Created lazily on first "Open Window" click; pushes live status updates.
	// appWin declared above (before onLogin) so closures can reference it.
	appWin = snlin.NewAppWindow(snlin.AppWindowCallbacks{
		OnConnect:    func() { sendCmd(snmac.IPCCmd{T: "connect"}) },
		OnDisconnect: func() { sendCmd(snmac.IPCCmd{T: "disconnect"}) },
		OnSettings: func(s snlin.AppSettings) {
			if s.WildCat != trayApp.IsWildcatEnabled() {
				onWildcatChange(s.WildCat)
			}
			// All other settings go through the "settings" IPC command.
			sendCmd(snmac.IPCCmd{
				T:         "settings",
				DOH:       s.DoH,
				BlockQUIC: s.BlockQUIC,
				Region:    s.Region,
			})
		},
		OnPageReady: func() {
			// Re-push current state so the window is correct even if it loaded late.
			appWin.PushStatus(trayApp.GetAppStatus())
			appWin.PushSettings(trayApp.GetAppSettings())
			// Direct (non-tunneled) reachability probe -- decides whether the
			// credential-login path is offered at all. No tunnel exists yet at
			// this point (login hasn't happened), so nc's plain http.Client is
			// structurally guaranteed to go straight to navlink.net.
			go appWin.PushNavlinkReachable(nc.Probe(context.Background()))
		},
		OnKey: func(keyStr string) {
			// User submitted an activation key from the window's login panel.
			sendCmd(snmac.IPCCmd{T: "key", Key: keyStr})
		},
		OnLogUploadToggle: func(enabled bool) {
			sendCmd(snmac.IPCCmd{T: "log_upload_pref", LogUploadEnabled: enabled})
		},
		OnHaveKeyAnswer: func(hasKey bool) {
			// No daemon-side action needed -- the window's own JS already
			// switched to the right screen (key entry or credential login).
		},
		OnCredentialLogin: func(email, password string) {
			go func() {
				ctx := context.Background()
				if err := nc.Login(ctx, email, password); err != nil {
					appWin.RunCredentialError(err.Error())
					return
				}
				keyStr, _, _, err := nc.FreeKey(ctx)
				if err != nil {
					appWin.RunCredentialError(err.Error())
					return
				}
				// Feed the issued key into the exact same path as manual entry.
				sendCmd(snmac.IPCCmd{T: "key", Key: keyStr})
			}()
		},
		OnKeyModeSwitch: func() {},
		OnRecommend: func(username string) {
			// The daemon holds the session token needed to actually call
			// the arbiter -- forward, don't handle here.
			sendCmd(snmac.IPCCmd{T: "recommend", TargetUsername: username})
		},
		OnClubThemePreview: func(theme string) {
			sendCmd(snmac.IPCCmd{T: "club_theme_preview", PreviewTheme: theme})
		},
	})

	trayApp.SetStatusCallback(func() {
		appWin.PushStatus(trayApp.GetAppStatus())
	})

	trayApp.SetOpenWindowCallback(func() {
		appWin.Show()
		appWin.PushSettings(trayApp.GetAppSettings())
	})

	trayApp.SetBeforeQuitCallback(func() {
		// Ask the daemon to record clean shutdown and exit.
		// Tray runs as non-root and cannot write to /var/lib/shortnerdcat directly.
		sendCmd(snmac.IPCCmd{T: "quit"})
		time.Sleep(200 * time.Millisecond)
	})

	trayApp.SetReadyCallback(func() {
		if !watchdogRestart {
			snlin.ShowNotification("ShortNerdCat", "v"+initMsg.Version+snlin.T("notify_started_suffix"))
		}
		trayApp.TriggerLoginIfNeeded()
	})

	// Read status updates from main in background.
	go func() {
		for {
			msg, err := ipc.ReadMsg()
			if err != nil {
				trayApp.Quit()
				return
			}
			switch msg.T {
			case "status":
				trayApp.ApplyIPCStatus(msg.State, msg.Msg)
				// Auth failures â†’ show error in window's login panel.
				if msg.State == "error" && appWin != nil {
					appWin.RunLoginError(msg.Msg)
				}
				// After successful login the window's login overlay should disappear.
				if msg.State == "logged_in" || msg.State == "connected" {
					appWin.PushStatus(trayApp.GetAppStatus())
				}

			case "ask_key":
				// Daemon needs a key (e.g. re-auth after expiry) â€” open window.
				go appWin.Show()

			case "update":
				trayApp.NotifyUpdateReady(msg.UpdateVersion)

			case "notify":
				snlin.ShowNotifications(msg.Msgs)

			case "auth_warn":
				if msg.Msg == "" {
					trayApp.ClearAuthWarning()
				} else {
					trayApp.SetAuthWarning(msg.Msg)
				}

			case "reconnect":
				core.Log.Printf("tray: received reconnect from daemon")
				trayApp.TriggerReconnect()

			case "wildcat_status":
				if !msg.WildcatOK {
					core.Log.Printf("tray: WildCat connect failed â€” reverting checkbox")
					trayApp.SetWildcatChecked(false)
				}

			case "club_theme":
				if appWin != nil {
					appWin.PushClubTheme(msg.ClubTheme, msg.ClubBadge, msg.IsAdmin, msg.CanRecommend)
				}

			case "bytes":
				if appWin != nil {
					appWin.PushBytes(msg.BytesSent, msg.BytesRecv)
				}

			case "log_upload_pref":
				if appWin != nil {
					appWin.PushLogUploadPref(msg.LogUploadOK, core.LogUploadPrefResponse{
						Enabled:       msg.LogUploadEnabled,
						Effective:     msg.LogUploadEffective,
						AdminDisabled: msg.LogUploadAdminDisabled,
						GlobalEnabled: msg.LogUploadGlobalEnabled,
					})
				}
			}
		}
	}()

	// Poll for a pending activation key written by the deep-link handler.
	// The deep-link binary (navlink:// URI) writes the key to a file in
	// XDG_RUNTIME_DIR; we pick it up here and send it to the daemon via IPC.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			data, err := os.ReadFile(pendingKeyPath())
			if err != nil {
				continue
			}
			key := strings.TrimSpace(string(data))
			os.Remove(pendingKeyPath()) //nolint:errcheck
			if key == "" {
				continue
			}
			core.Log.Printf("deep-link: pending key found (%d chars) â€” forwarding to daemon", len(key))
			sendCmd(snmac.IPCCmd{T: "key", Key: key})
		}
	}()

	trayApp.Run()
}
