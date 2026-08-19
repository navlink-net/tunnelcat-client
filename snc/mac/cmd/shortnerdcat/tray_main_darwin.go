// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	snmac "shortnerdcat/snc/mac/macos"
	"shortnerdcat/snc/shared/navlinkauth"
	"tunnel_cat/snc/core"
)

// obtainActivationKey runs the "do you have a key?" flow: existing users go
// straight to the (unchanged) native key-entry panel; users with no key are
// offered a direct (non-tunneled) navlink.net email/password login that
// automatically issues a key, but only when navlink.net is actually
// reachable — otherwise login could never succeed anyway, so we go straight
// to manual key entry. Returns a raw activation-key string exactly as if it
// had been typed into the key-entry panel.
//
// Note: unlike Windows, the key-entry panel itself does not grow a "Login"
// button when navlink.net is reachable (that would need extra native Cocoa
// UI this session can't build-verify without a Mac) — "Yes, I have a key"
// always goes straight to key entry here. The credential-login screen still
// offers "I Have a Key" to switch the other way, so the full flow works,
// just asymmetrically between platforms.
func obtainActivationKey(appWin *snmac.SNCWindow) (string, error) {
	getKeyManually := func() (string, error) {
		if appWin != nil {
			return appWin.ShowKeyEntry()
		}
		return showKeyDialog()
	}

	if appWin == nil {
		// No window yet (edge case during early startup) — can't show the
		// have-key-prompt/credential-login panels, fall back to manual entry
		// exactly as before this change.
		return getKeyManually()
	}

	hasKey := appWin.ShowHaveKeyPrompt()
	if hasKey {
		return getKeyManually()
	}

	nc := navlinkauth.New()
	ctx := context.Background()
	if !nc.Probe(ctx) {
		return getKeyManually()
	}

	for {
		email, password, wantsKeyMode, err := appWin.ShowCredentialLogin()
		if wantsKeyMode {
			return getKeyManually()
		}
		if err != nil {
			return "", err
		}
		if loginErr := nc.Login(ctx, email, password); loginErr != nil {
			snmac.ShowError("Could not log in", loginErr.Error())
			continue
		}
		keyStr, _, _, keyErr := nc.FreeKey(ctx)
		if keyErr != nil {
			snmac.ShowError("Logged in, but could not get a key", keyErr.Error())
			continue
		}
		return keyStr, nil
	}
}

// runTrayProcess is the entry point for --tray mode (runs as the logged-in user).
// It connects to the IPC socket, creates TrayApp, and drives the UI.
func runTrayProcess(socketPath string, watchdogRestart bool) {
	var updatePromptOnce sync.Once

	ipc, err := snmac.IPCDial(socketPath, 15*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray: connect IPC: %v\n", err)
		os.Exit(1)
	}

	// First message must be "init".
	init, err := ipc.ReadMsg()
	if err != nil || init.T != "init" {
		fmt.Fprintf(os.Stderr, "tray: bad init: t=%q err=%v\n", init.T, err)
		os.Exit(1)
	}

	core.Version = init.Version
	logDir := init.LogDir

	// sendCmd is safe to call from any goroutine.
	sendCmd := func(cmd snmac.IPCCmd) {
		ipc.SendCmd(cmd) //nolint:errcheck
	}

	var (
		trayApp *snmac.TrayApp
		appWin  *snmac.SNCWindow
	)

	// Tray â†’ Main callbacks: fire-and-forget, main sends back a "status" update.
	onConnect := func(autoReconnect bool) error {
		sendCmd(snmac.IPCCmd{T: "connect", AutoReconnect: autoReconnect})
		return nil // tray is already in pending state; real result comes via "status"
	}
	onDisconnect := func(autoReconnect bool) {
		sendCmd(snmac.IPCCmd{T: "disconnect", AutoReconnect: autoReconnect})
	}
	onLogin := func() error {
		keyStr, kerr := obtainActivationKey(appWin)
		if kerr != nil || keyStr == "" {
			return fmt.Errorf("login cancelled")
		}
		sendCmd(snmac.IPCCmd{T: "key", Key: keyStr})
		return nil
	}
	onLogout := func() { sendCmd(snmac.IPCCmd{T: "logout"}) }
	onBlockQUICChange := func(v bool) {
		sendCmd(snmac.IPCCmd{T: "settings", BlockQUIC: v, DOH: trayApp.IsDNSOverHTTPSEnabled()})
	}
	onRegionChange := func(r string) {
		sendCmd(snmac.IPCCmd{T: "settings", Region: r, DOH: trayApp.IsDNSOverHTTPSEnabled()})
	}

	trayApp = snmac.NewTrayApp(
		init.Version,
		init.InitLogin,
		init.AutoConnect,
		init.DOH,
		init.BlockQUIC,
		init.Region,
		onLogin, onLogout, onConnect, onDisconnect,
		nil, // onDNSOverHTTPSChange — not needed on macOS MVP
		onBlockQUICChange, onRegionChange,
	)
	// Register the tray so native menu bar callbacks can reach it.
	snmac.SetMenuTray(trayApp)

	trayApp.SetShareLogsCallback(func() {
		exec.Command("/usr/bin/open", logDir).Run() //nolint:errcheck
	})

	trayApp.SetReadyCallback(func() {
		fmt.Fprintf(os.Stderr, "tray: readyCb called\n")
		if !watchdogRestart {
			fmt.Fprintf(os.Stderr, "tray: showing splash\n")
			snmac.ShowSplash(init.Version, 2500*time.Millisecond)
			fmt.Fprintf(os.Stderr, "tray: splash done\n")
		}

		fmt.Fprintf(os.Stderr, "tray: creating SNCWindow\n")
		appWin = snmac.NewSNCWindow(
			func() { trayApp.TriggerConnect() },
			func() { trayApp.TriggerDisconnect() },
			func(s snmac.AppSettings) { trayApp.ApplyWindowSettings(s) },
		)
		fmt.Fprintf(os.Stderr, "tray: SNCWindow created appWin=%p\n", appWin)
		appWin.BuildAppMenu()
		trayApp.SetBeforeQuitCallback(func() {
			appWin.CancelKeyEntry()
			sendCmd(snmac.IPCCmd{T: "quit"})
		})
		trayApp.SetOpenWindowCallback(func() {
			appWin.PushSettings(trayApp.GetAppSettings())
			appWin.PushStatus(trayApp.GetAppStatus())
			appWin.Show()
		})
		trayApp.SetStatusCallback(func() {
			appWin.PushStatus(trayApp.GetAppStatus())
			appWin.PushSettings(trayApp.GetAppSettings())
		})
		appWin.SetPageReadyCallback(func() {
			appWin.PushStatus(trayApp.GetAppStatus())
			appWin.PushSettings(trayApp.GetAppSettings())
		})
		appWin.SetRecommendCallback(func(username string) {
			// The daemon holds the session token needed to actually call
			// the arbiter -- forward, don't handle here.
			sendCmd(snmac.IPCCmd{T: "recommend", TargetUsername: username})
		})
		appWin.SetClubThemePreviewCallback(func(theme string) {
			sendCmd(snmac.IPCCmd{T: "club_theme_preview", PreviewTheme: theme})
		})
		fmt.Fprintf(os.Stderr, "tray: calling appWin.PushStatus\n")
		appWin.PushStatus(trayApp.GetAppStatus())
		fmt.Fprintf(os.Stderr, "tray: calling appWin.Show\n")
		appWin.Show()
		fmt.Fprintf(os.Stderr, "tray: appWin.Show returned\n")

		fmt.Fprintf(os.Stderr, "tray: calling TriggerLoginIfNeeded\n")
		trayApp.TriggerLoginIfNeeded()
		fmt.Fprintf(os.Stderr, "tray: readyCb complete\n")
	})

	// Read status updates from main in background.
	go func() {
		for {
			msg, err := ipc.ReadMsg()
			if err != nil {
				// Main process gone â€” quit.
				trayApp.Quit()
				return
			}
			switch msg.T {
			case "status":
				trayApp.ApplyIPCStatus(msg.State, msg.Msg)
				if appWin != nil {
					appWin.PushStatus(trayApp.GetAppStatus())
				}
			case "ask_key":
				go func() {
					keyStr, kerr := obtainActivationKey(appWin)
					if kerr != nil || keyStr == "" {
						sendCmd(snmac.IPCCmd{T: "key_cancel"})
					} else {
						sendCmd(snmac.IPCCmd{T: "key", Key: keyStr})
					}
				}()
			case "update":
				trayApp.NotifyUpdateReady(msg.UpdateVersion)
				v := msg.UpdateVersion
				updatePromptOnce.Do(func() {
					go func() {
						if snmac.ShowUpdateAvailableAlert(v) {
							go core.ApplyPendingUpdate()
						}
					}()
				})
			case "notify":
				snmac.ShowNotifications(msg.Msgs)
			case "auth_warn":
				if msg.Msg == "" {
					trayApp.ClearAuthWarning()
				} else {
					trayApp.SetAuthWarning(msg.Msg)
				}
			case "reconnect":
				core.Log.Printf("tray: received reconnect from daemon")
				trayApp.TriggerReconnect()
			case "club_theme":
				if appWin != nil {
					appWin.PushClubTheme(msg.ClubTheme, msg.ClubBadge, msg.IsAdmin, msg.CanRecommend)
				}
			}
		}
	}()

	// Register navlink:// URL scheme handler before the Cocoa run loop starts.
	snmac.RegisterURLHandler()

	// Forward any navlink://activate?key=... deep link keys to the daemon as "key" IPC commands.
	go func() {
		for key := range snmac.DeepLinkKeysCh {
			core.Log.Printf("tray: deep link key received, forwarding to daemon")
			sendCmd(snmac.IPCCmd{T: "key", Key: key})
		}
	}()

	trayApp.Run()
}
