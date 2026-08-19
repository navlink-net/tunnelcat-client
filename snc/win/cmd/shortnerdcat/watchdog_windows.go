// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package main

// runAsWatchdog implements the watchdog mode of the binary (--watchdog flag).
//
// The watchdog's responsibilities:
//  1. Hold a named mutex so only one watchdog runs at a time.
//  2. Create the lifecycle events that main uses to signal clean/OTA exits.
//  3. Clean up any stale networking state left by a previous crash.
//  4. Start (or attach to) the main process.
//  5. Monitor main in a loop:
//     - SNCCleanShutdown signaled â†’ user quit cleanly â†’ watchdog exits.
//     - SNCUpdateRestart signaled â†’ OTA applied â†’ wait briefly, restart main.
//     - No signal â†’ unclean exit (crash/kill) â†’ cleanup + restart main.
//  6. Main reciprocally monitors the watchdog and restarts it if it dies.
//  7. While the tunnel reports healthy, probe 5 international sites every 5 s;
//     if all 5 are unreachable for 3 consecutive rounds â†’ kill + restart main.

import (
	"io"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"tunnel_cat/snc/core"
	snwin "shortnerdcat/snc/win/windows"
)

// runAsWatchdog is called from main() when os.Args[1] == "--watchdog".
func runAsWatchdog() {
	// Elevate to Administrator so route/DoH commands work.
	if !isAdmin() {
		relaunchAsAdmin("--watchdog")
		return
	}

	// Prevent duplicate watchdog instances.
	_, err := snwin.HoldWatchdogMutex()
	if err != nil {
		// Another watchdog is already running; exit silently.
		return
	}
	// mutex handle intentionally leaked â€” OS releases it on process exit.

	if err := core.InitLogging(`C:\.shortnerdcat\logs`); err != nil {
		os.Stderr.WriteString("watchdog: logging init: " + err.Error() + "\n")
	}
	core.Log.Printf("watchdog: started pid=%d", os.Getpid())

	// Create lifecycle events before starting main so main can signal them
	// even during its very first startup (e.g. ApplyPendingUpdate).
	events, err := snwin.CreateWatchdogEvents()
	if err != nil {
		core.Log.Printf("watchdog: create events failed: %v â€” exiting", err)
		return
	}
	defer events.Close()

	// Cleanup any stale networking state from a previous crash.
	st := core.ReadWatchdogState()
	if st.Connected {
		core.Log.Printf("watchdog: stale state: connected=true â€” cleaning up")
		snwin.CleanupSession(st.OrigGW)
	}
	// Reset state file so any stale TunnelHealthy=true from a previous run
	// (e.g. after a reboot without clean shutdown) does not mislead the probe.
	// Main will write a fresh state with its PID shortly after startup.
	core.WriteWatchdogState(core.WatchdogState{}) //nolint:errcheck

	// Obtain a handle to the main process: either attach to one already running
	// (e.g. user launched shortnerdcat.exe directly before the watchdog) or start
	// a fresh one.
	mainHandle := attachOrStartMain(st.MainPID)
	if mainHandle == syscall.InvalidHandle {
		core.Log.Printf("watchdog: failed to obtain main process handle â€” exiting")
		return
	}

	for {
		// Run a connectivity probe goroutine for this main instance.
		// It signals probeKill when it decides main should be restarted.
		// Close probeStop to tell the goroutine to exit when main dies.
		probeStop := make(chan struct{})
		probeKill := make(chan struct{}, 1)
		go runConnectivityProbe(probeStop, probeKill)

		// Monitor main, polling every 5 s so probe kills and the clean-shutdown
		// deadline are acted on promptly.
		shutdownSignaled := false
		var shutdownDeadline time.Time
		mainDead := false

		for !mainDead {
			if snwin.WaitProcessTimeout(mainHandle, 5*time.Second) {
				mainDead = true
				break
			}

			// Probe-triggered restart â€” ignored once shutdown is in progress.
			if !shutdownSignaled {
				select {
				case <-probeKill:
					core.Log.Printf("watchdog: connectivity probe triggered restart â€” killing main")
					snwin.TerminateProcess(mainHandle)
					mainDead = true
				default:
				}
				if mainDead {
					break
				}
			}

			// Detect clean-shutdown signal from main (set before os.Exit).
			if !shutdownSignaled && events.IsCleanShutdown() {
				shutdownSignaled = true
				shutdownDeadline = time.Now().Add(60 * time.Second)
				core.Log.Printf("watchdog: clean shutdown signaled â€” waiting up to 60 s for main to exit")
			}

			// Enforce the shutdown deadline.
			if shutdownSignaled && time.Now().After(shutdownDeadline) {
				core.Log.Printf("watchdog: clean shutdown timed out â€” force-killing main")
				snwin.TerminateProcess(mainHandle)
				mainDead = true
				break
			}

			// Liveness: LastAlive stale? (Skip during intentional shutdown.)
			if !shutdownSignaled {
				st := core.ReadWatchdogState()
				if st.LastAlive > 0 && time.Since(time.Unix(st.LastAlive, 0)) > 5*time.Minute {
					// Before killing, wait 15 s and re-read â€” this handles
					// hibernate/sleep resume where wall-clock time jumped but
					// main's power-event handler hasn't called TouchAlive yet.
					core.Log.Printf("watchdog: LastAlive stale by %s â€” waiting 15 s before killing", time.Since(time.Unix(st.LastAlive, 0)).Round(time.Second))
					time.Sleep(15 * time.Second)
					st = core.ReadWatchdogState()
					if st.LastAlive > 0 && time.Since(time.Unix(st.LastAlive, 0)) > 5*time.Minute {
						core.Log.Printf("watchdog: main appears frozen (last alive %s ago) â€” killing", time.Since(time.Unix(st.LastAlive, 0)).Round(time.Second))
						snwin.TerminateProcess(mainHandle)
						mainDead = true
					}
				}
			}
		}

		close(probeStop) // tell probe goroutine to exit
		syscall.CloseHandle(mainHandle) //nolint:errcheck
		core.Log.Printf("watchdog: main process exited")

		// If clean shutdown was signaled (either detected during the loop or now),
		// do not restart main â€” the user intentionally quit.
		if shutdownSignaled || events.IsCleanShutdown() {
			core.Log.Printf("watchdog: clean shutdown â€” exiting")
			return
		}

		if events.IsUpdateRestart() {
			core.Log.Printf("watchdog: OTA update signal â€” restarting main after 2s")
			time.Sleep(2 * time.Second)
		} else {
			// Unclean exit (crash or kill): restore networking then restart.
			st = core.ReadWatchdogState()
			if st.Connected {
				core.Log.Printf("watchdog: crash detected â€” cleaning up stale session")
				snwin.CleanupSession(st.OrigGW)
				// Reset state so subsequent crash iterations don't re-run cleanup
				// against the same stale Connected=true entry.
				core.WriteWatchdogState(core.WatchdogState{}) //nolint:errcheck
			}
			time.Sleep(3 * time.Second)
		}

		mainHandle = startMainWithRetry()
		if mainHandle == syscall.InvalidHandle {
			core.Log.Printf("watchdog: could not restart main â€” giving up")
			return
		}
	}
}

// connProbeSites are the targets used to probe internet connectivity through the
// tunnel.  They span multiple countries and providers; if even one responds the
// tunnel is considered healthy.  Plain HTTP is used to avoid TLS overhead.
var connProbeSites = []string{
	"http://example.com",       // IANA (US)
	"http://www.google.com",    // Google (US/global)
	"http://www.amazon.co.uk",  // Amazon (UK)
	"http://www.naver.com",     // Naver (Korea)
	"http://www.yahoo.co.jp",   // Yahoo Japan
}

// runConnectivityProbe probes external connectivity every 5 s while the tunnel
// reports healthy.  If all connProbeSites fail for 3 consecutive rounds it
// sends on killCh and returns.  Exits when stopCh is closed.
func runConnectivityProbe(stopCh <-chan struct{}, killCh chan<- struct{}) {
	client := &http.Client{
		Timeout: 4 * time.Second,
		// Do not follow redirects â€” a redirect response still proves connectivity.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	allFailRounds := 0
	var healthyBeganAt time.Time // when TunnelHealthy first became true in this probe's run

	for {
		select {
		case <-stopCh:
			return
		case <-time.After(5 * time.Second):
		}

		st := core.ReadWatchdogState()
		if !st.Connected || !st.TunnelHealthy {
			// Tunnel not up or main is in a recovery path â€” reset counter and wait.
			allFailRounds = 0
			healthyBeganAt = time.Time{}
			continue
		}

		// Track when tunnel health was first observed in this probe run.
		if healthyBeganAt.IsZero() {
			healthyBeganAt = time.Now()
		}
		// Give the tunnel 30 s to warm up before treating probe failures as fatal.
		// This prevents the probe from killing main immediately after TUN comes up,
		// before the underlying apps have established any connections.
		const probeGracePeriod = 30 * time.Second
		if time.Since(healthyBeganAt) < probeGracePeriod {
			allFailRounds = 0
			continue
		}

		// Probe all sites in parallel; one success is enough to clear the counter.
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
			core.Log.Printf("watchdog: probe: connectivity confirmed dead â€” signaling restart")
			select {
			case killCh <- struct{}{}:
			default:
			}
			return
		}
	}
}

// attachOrStartMain returns a process handle for the main process.
// If the main process is already running (PID from state file), opens it.
// Otherwise starts a new one.
func attachOrStartMain(statePID int) syscall.Handle {
	if statePID != 0 {
		h := snwin.OpenProcessForWait(statePID)
		if h != syscall.InvalidHandle {
			core.Log.Printf("watchdog: attached to existing main pid=%d", statePID)
			return h
		}
	}
	// Start main fresh.
	proc, err := snwin.WatchdogStartMain()
	if err != nil {
		core.Log.Printf("watchdog: start main failed: %v", err)
		return syscall.InvalidHandle
	}
	core.Log.Printf("watchdog: main started pid=%d", proc.Pid)
	h := snwin.OpenProcessForWait(proc.Pid)
	if h == syscall.InvalidHandle {
		core.Log.Printf("watchdog: open main process handle failed â€” using proc.Wait fallback")
		proc.Wait() //nolint:errcheck
		return syscall.InvalidHandle
	}
	return h
}

// startMainWithRetry tries to restart the main process after it exits.
// Before launching a new copy it checks the watchdog state file: if a new
// instance has already taken over (e.g. an in-place upgrade where the new
// binary killed the old one and is now running), it attaches to that process
// instead of spawning yet another copy.
func startMainWithRetry() syscall.Handle {
	// Give the new instance a moment to write its PID before we check.
	time.Sleep(500 * time.Millisecond)
	st := core.ReadWatchdogState()
	if st.MainPID != 0 {
		if h := snwin.OpenProcessForWait(st.MainPID); h != syscall.InvalidHandle {
			core.Log.Printf("watchdog: new main already running pid=%d â€” attaching", st.MainPID)
			return h
		}
	}

	for attempt := 1; attempt <= 2; attempt++ {
		proc, err := snwin.WatchdogRestartMain()
		if err == nil {
			h := snwin.OpenProcessForWait(proc.Pid)
			if h != syscall.InvalidHandle {
				core.Log.Printf("watchdog: main restarted pid=%d (attempt %d)", proc.Pid, attempt)
				return h
			}
		}
		core.Log.Printf("watchdog: restart attempt %d failed: %v â€” retrying in 10s", attempt, err)
		time.Sleep(10 * time.Second)
	}
	return syscall.InvalidHandle
}
