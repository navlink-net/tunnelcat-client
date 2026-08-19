// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package main

// runAsWatchdog implements the watchdog mode (--watchdog flag).
//
// Responsibilities:
//  1. Hold the lockfile so only one watchdog runs at a time.
//  2. Clean up stale networking state from a previous crash.
//  3. Start (or attach to) the main process.
//  4. Monitor main in a loop:
//     - snc_shutdown flag â†’ user quit cleanly â†’ watchdog exits.
//     - snc_update flag   â†’ OTA applied â†’ restart main.
//     - Unclean exit      â†’ cleanup + restart.
//  5. Connectivity probe: probe sites every 5 s; 3 consecutive all-fail
//     rounds while tunnel is healthy â†’ kill + restart main.

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"tunnel_cat/snc/core"
	snlin "shortnerdcat/snc/linux/linux"
)

func runAsWatchdog() {
	lockFile, err := snlin.HoldWatchdogLock()
	if err != nil {
		debugLog(fmt.Sprintf("watchdog: lock failed: %v â€” exiting", err))
		return
	}
	defer lockFile.Close()
	debugLog("watchdog: lock acquired")

	snlin.ClearWatchdogFlags()

	logDir := appDataDir() + "/logs"
	if err := core.InitLogging(logDir); err != nil {
		debugLog(fmt.Sprintf("watchdog: InitLogging: %v", err))
	}
	openLogsToUser(logDir)
	core.Log.Printf("watchdog: started")

	core.UpdateSignalFunc = snlin.SignalUpdateRestart

	st := core.ReadWatchdogState()
	if st.Connected {
		core.Log.Printf("watchdog: stale connected=true â€” cleaning up")
		snlin.CleanupSession(st.OrigGW)
	}
	core.WriteWatchdogState(core.WatchdogState{}) //nolint:errcheck

	mainPID := attachOrStartMain(st.MainPID)
	if mainPID == 0 {
		core.Log.Printf("watchdog: failed to obtain main process â€” exiting")
		return
	}

	for {
		probeStop := make(chan struct{})
		probeKill := make(chan struct{}, 1)
		go runConnectivityProbe(probeStop, probeKill)

		shutdownSignaled := false
		var shutdownDeadline time.Time
		mainDead := false

		for !mainDead {
			if snlin.WaitProcessTimeout(mainPID, 5*time.Second) {
				mainDead = true
				break
			}

			if !shutdownSignaled {
				select {
				case <-probeKill:
					core.Log.Printf("watchdog: probe triggered restart â€” killing main")
					snlin.TerminateProcess(mainPID)
					mainDead = true
				default:
				}
				if mainDead {
					break
				}
			}

			if !shutdownSignaled && snlin.IsCleanShutdown() {
				shutdownSignaled = true
				shutdownDeadline = time.Now().Add(60 * time.Second)
				core.Log.Printf("watchdog: clean shutdown signaled â€” waiting up to 60 s")
			}

			if shutdownSignaled && time.Now().After(shutdownDeadline) {
				core.Log.Printf("watchdog: clean shutdown timed out â€” force-killing main")
				snlin.TerminateProcess(mainPID)
				mainDead = true
				break
			}

			if !shutdownSignaled {
				st := core.ReadWatchdogState()
				if st.LastAlive > 0 && time.Since(time.Unix(st.LastAlive, 0)) > 5*time.Minute {
					core.Log.Printf("watchdog: LastAlive stale â€” waiting 15 s before killing")
					time.Sleep(15 * time.Second)
					st = core.ReadWatchdogState()
					if st.LastAlive > 0 && time.Since(time.Unix(st.LastAlive, 0)) > 5*time.Minute {
						core.Log.Printf("watchdog: main appears frozen â€” killing")
						snlin.TerminateProcess(mainPID)
						mainDead = true
					}
				}
			}
		}

		close(probeStop)
		core.Log.Printf("watchdog: main process exited (pid=%d)", mainPID)

		if shutdownSignaled || snlin.IsCleanShutdown() {
			core.Log.Printf("watchdog: clean shutdown â€” exiting")
			return
		}

		if snlin.IsUpdateRestart() {
			core.Log.Printf("watchdog: OTA update signal â€” restarting main after 2 s")
			time.Sleep(2 * time.Second)
		} else {
			st = core.ReadWatchdogState()
			if st.Connected {
				core.Log.Printf("watchdog: crash detected â€” cleaning up stale session")
				snlin.CleanupSession(st.OrigGW)
				core.WriteWatchdogState(core.WatchdogState{}) //nolint:errcheck
			}
			time.Sleep(3 * time.Second)
		}

		mainPID = startMainWithRetry()
		if mainPID == 0 {
			core.Log.Printf("watchdog: could not restart main â€” giving up")
			return
		}
	}
}

func attachOrStartMain(statePID int) int {
	if statePID != 0 && !snlin.WaitProcessTimeout(statePID, 0) {
		core.Log.Printf("watchdog: attached to existing main pid=%d", statePID)
		return statePID
	}
	proc, err := snlin.WatchdogStartMain()
	if err != nil {
		core.Log.Printf("watchdog: start main failed: %v", err)
		return 0
	}
	core.Log.Printf("watchdog: main started pid=%d", proc.Pid)
	return proc.Pid
}

func startMainWithRetry() int {
	time.Sleep(500 * time.Millisecond)
	if pid := snlin.ReadPIDFile(); pid != 0 && !snlin.WaitProcessTimeout(pid, 0) {
		core.Log.Printf("watchdog: new main already running pid=%d â€” attaching", pid)
		return pid
	}
	for attempt := 1; attempt <= 2; attempt++ {
		proc, err := snlin.WatchdogRestartMain()
		if err == nil {
			core.Log.Printf("watchdog: main restarted pid=%d (attempt %d)", proc.Pid, attempt)
			return proc.Pid
		}
		core.Log.Printf("watchdog: restart attempt %d failed: %v â€” retrying in 10 s", attempt, err)
		time.Sleep(10 * time.Second)
	}
	return 0
}

var connProbeSites = []string{
	"http://example.com",
	"http://www.google.com",
	"http://www.amazon.co.uk",
	"http://www.naver.com",
	"http://www.yahoo.co.jp",
}

func runConnectivityProbe(stopCh <-chan struct{}, killCh chan<- struct{}) {
	client := &http.Client{
		Timeout: 4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	allFailRounds := 0
	var healthyBeganAt time.Time

	for {
		select {
		case <-stopCh:
			return
		case <-time.After(5 * time.Second):
		}

		st := core.ReadWatchdogState()
		if !st.Connected || !st.TunnelHealthy {
			allFailRounds = 0
			healthyBeganAt = time.Time{}
			continue
		}

		const wakeGrace = 60 * time.Second
		if st.LastWake > 0 && time.Since(time.Unix(st.LastWake, 0)) < wakeGrace {
			allFailRounds = 0
			healthyBeganAt = time.Time{}
			continue
		}

		if healthyBeganAt.IsZero() {
			healthyBeganAt = time.Now()
		}
		const probeGrace = 30 * time.Second
		if time.Since(healthyBeganAt) < probeGrace {
			allFailRounds = 0
			continue
		}

		results := make([]bool, len(connProbeSites))
		var wg sync.WaitGroup
		for i, site := range connProbeSites {
			wg.Add(1)
			go func(i int, site string) {
				defer wg.Done()
				resp, err := client.Head(site)
				if err == nil {
					io.Copy(io.Discard, resp.Body) //nolint:errcheck
					resp.Body.Close()
					results[i] = true
				}
			}(i, site)
		}
		wg.Wait()

		anyOK := false
		for _, ok := range results {
			if ok {
				anyOK = true
				break
			}
		}

		if anyOK {
			allFailRounds = 0
			continue
		}

		allFailRounds++
		core.Log.Printf("watchdog: probe: all %d sites unreachable (round %d/3)", len(connProbeSites), allFailRounds)

		if allFailRounds >= 3 {
			core.Log.Printf("watchdog: probe: connectivity dead â€” signaling restart")
			select {
			case killCh <- struct{}{}:
			default:
			}
			return
		}
	}
}
