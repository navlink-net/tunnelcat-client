// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tunnel_cat/snc/core"
)

// Watchdog coordination uses three files in the app-data directory:
//
//	snc_watchdog.lock   â€” exclusive flock held by the watchdog for its lifetime
//	snc_shutdown        â€” created by main before a clean quit
//	snc_update          â€” created by core.UpdateSignalFunc before OTA os.Exit

const (
	wdLockFile     = "snc_watchdog.lock"
	wdShutdownFile = "snc_shutdown"
	wdUpdateFile   = "snc_update"
)

func wdPath(name string) string {
	dir := os.Getenv("APPDATA")
	if dir == "" {
		// Fallback when launched by launchd as root with no HOME in environment.
		dir = "/var/root/.shortnerdcat"
	}
	return filepath.Join(dir, name)
}

// HoldWatchdogLock creates (or opens) the watchdog lockfile and acquires an
// exclusive flock. Returns the open file (keep it open for the process
// lifetime â€” OS releases the lock on close/exit) or an error if another
// watchdog is already running.
func HoldWatchdogLock() (*os.File, error) {
	p := wdPath(wdLockFile)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("watchdog: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("watchdog: already running")
	}
	return f, nil
}

// WatchdogRunning returns true if another watchdog holds the lock.
func WatchdogRunning() bool {
	p := wdPath(wdLockFile)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return true // locked by another process
	}
	// We got it â€” release immediately and report no watchdog.
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return false
}

// SignalCleanShutdown creates the clean-shutdown flag file.
// Called by main before a user-initiated Quit.
func SignalCleanShutdown() {
	p := wdPath(wdShutdownFile)
	os.WriteFile(p, []byte{}, 0600) //nolint:errcheck
}

// SignalUpdateRestart creates the OTA update flag file.
// Wired to core.UpdateSignalFunc; called by ApplyPendingUpdate before os.Exit.
func SignalUpdateRestart() {
	p := wdPath(wdUpdateFile)
	os.WriteFile(p, []byte{}, 0600) //nolint:errcheck
}

// IsCleanShutdown returns true if the clean-shutdown flag file exists.
func IsCleanShutdown() bool {
	_, err := os.Stat(wdPath(wdShutdownFile))
	return err == nil
}

// IsUpdateRestart returns true if the OTA update flag file exists.
func IsUpdateRestart() bool {
	_, err := os.Stat(wdPath(wdUpdateFile))
	return err == nil
}

// ClearWatchdogFlags removes the shutdown and update flag files.
// Called at watchdog startup to clear stale flags from a previous session.
func ClearWatchdogFlags() {
	os.Remove(wdPath(wdShutdownFile)) //nolint:errcheck
	os.Remove(wdPath(wdUpdateFile))   //nolint:errcheck
}

// WatchdogStartMain launches the current executable without --watchdog.
func WatchdogStartMain() (*os.Process, error) {
	return wdLaunchMain()
}

// WatchdogRestartMain launches the current executable with --restarted
// so the app skips any first-run setup.
func WatchdogRestartMain() (*os.Process, error) {
	return wdLaunchMain("--restarted")
}

func wdLaunchMain(extraArgs ...string) (*os.Process, error) {
	exe, err := watchdogSelfExe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, extraArgs...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("watchdog: start main: %w", err)
	}
	return cmd.Process, nil
}

// StartWatchdog launches the current executable with --watchdog.
func StartWatchdog() (*os.Process, error) {
	exe, err := watchdogSelfExe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "--watchdog")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("watchdog: start watchdog process: %w", err)
	}
	return cmd.Process, nil
}

func watchdogSelfExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("watchdog: find self: %w", err)
	}
	return filepath.Abs(exe)
}

// WaitProcess blocks until the process with the given PID exits.
// Returns when the process exits or when stopCh is closed.
func WaitProcess(pid int, stopCh <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if !processAlive(pid) {
				return
			}
		}
	}
}

// WaitProcessTimeout waits up to d for the process to exit.
// Returns true if exited, false if timeout elapsed.
// When d <= 0, does a single instant check (non-blocking).
func WaitProcessTimeout(pid int, d time.Duration) bool {
	if d <= 0 {
		return !processAlive(pid)
	}
	deadline := time.Now().Add(d)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		if !processAlive(pid) {
			return true
		}
	}
	return false
}

// TerminateProcess sends SIGTERM then SIGKILL to the given PID.
func TerminateProcess(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	p.Signal(syscall.SIGTERM) //nolint:errcheck
	time.Sleep(500 * time.Millisecond)
	if processAlive(pid) {
		p.Signal(syscall.SIGKILL) //nolint:errcheck
	}
}

// processAlive returns true if a process with pid is running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks for process existence without actually sending a signal.
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

// CleanupSession restores networking state after an unclean main exit.
func CleanupSession(origGW string) {
	core.Log.Printf("watchdog: cleanup: restoring networking state (origGW=%q)", origGW)
	CleanupDNS()
	CleanupSplitRoutes(origGW)
}

// WritePIDFile writes the given PID to a file so the watchdog can find the
// running main process after a restart.
func WritePIDFile(pid int) {
	p := wdPath("snc_main.pid")
	os.WriteFile(p, []byte(strconv.Itoa(pid)), 0600) //nolint:errcheck
}

// ReadPIDFile returns the PID from the PID file, or 0 on error.
func ReadPIDFile() int {
	p := wdPath("snc_main.pid")
	data, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}
