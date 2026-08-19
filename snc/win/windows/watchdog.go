// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

// Watchdog coordination: named events, session cleanup, and process helpers.
//
// Two named kernel events coordinate the main â†” watchdog lifecycle.
// The watchdog creates both events at startup and holds their handles open for
// its entire lifetime (so they survive after the main process exits).
// Main opens the events by name when it needs to signal them.
//
//	Global\SNCCleanShutdown  â€” main signals before a user-initiated Quit.
//	                            Watchdog sees this and exits instead of restarting.
//	Global\SNCUpdateRestart  â€” main signals via core.UpdateSignalFunc before OTA
//	                            os.Exit. Watchdog sees this, waits for the new
//	                            binary to settle, then continues monitoring.
//
// Watchdog presence is advertised via a named mutex:
//
//	Global\SNCWatchdogRunning â€” held by the watchdog for its entire lifetime.
//	                             Main checks this to avoid starting a duplicate.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"tunnel_cat/snc/core"
)

const (
	wdEventCleanShutdown = "Global\\SNCCleanShutdown"
	wdEventUpdateRestart = "Global\\SNCUpdateRestart"
	wdMutexName          = "Global\\SNCWatchdogRunning"
)

const (
	wdEventAccess = 0x0002 | 0x00100000 // EVENT_MODIFY_STATE | SYNCHRONIZE
	wdSynchronize = 0x00100000
	wdWaitObject0 = 0x00000000
	wdInfinite    = 0xFFFFFFFF
	wdProcessSync = 0x00100000 | 0x1000 // SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION
)

var (
	modKernel32       = syscall.MustLoadDLL("kernel32.dll")
	procCreateEventW  = modKernel32.MustFindProc("CreateEventW")
	procOpenEventW    = modKernel32.MustFindProc("OpenEventW")
	procSetEventW     = modKernel32.MustFindProc("SetEvent")
	procWaitSingleObj = modKernel32.MustFindProc("WaitForSingleObject")
	procOpenProcess   = modKernel32.MustFindProc("OpenProcess")
	procCreateMutexW  = modKernel32.MustFindProc("CreateMutexW")
)

// â”€â”€ Events â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// WatchdogEvents holds handles to the two lifecycle coordination events.
// Created by the watchdog at startup so they exist before main is launched.
type WatchdogEvents struct {
	cleanShutdown syscall.Handle
	updateRestart syscall.Handle
}

// CreateWatchdogEvents creates the two named lifecycle events.
// Call this before starting the main process.
func CreateWatchdogEvents() (*WatchdogEvents, error) {
	cs, err := createEvent(wdEventCleanShutdown)
	if err != nil {
		return nil, fmt.Errorf("watchdog: create clean-shutdown event: %w", err)
	}
	ur, err := createEvent(wdEventUpdateRestart)
	if err != nil {
		syscall.CloseHandle(cs) //nolint:errcheck
		return nil, fmt.Errorf("watchdog: create update-restart event: %w", err)
	}
	return &WatchdogEvents{cleanShutdown: cs, updateRestart: ur}, nil
}

// Close releases the event handles.
func (e *WatchdogEvents) Close() {
	syscall.CloseHandle(e.cleanShutdown) //nolint:errcheck
	syscall.CloseHandle(e.updateRestart) //nolint:errcheck
}

// IsCleanShutdown polls the clean-shutdown event (zero-timeout, non-blocking).
func (e *WatchdogEvents) IsCleanShutdown() bool {
	r, _, _ := procWaitSingleObj.Call(uintptr(e.cleanShutdown), 0)
	return r == wdWaitObject0
}

// IsUpdateRestart polls the update-restart event (zero-timeout, non-blocking).
func (e *WatchdogEvents) IsUpdateRestart() bool {
	r, _, _ := procWaitSingleObj.Call(uintptr(e.updateRestart), 0)
	return r == wdWaitObject0
}

func createEvent(name string) (syscall.Handle, error) {
	namePtr, _ := syscall.UTF16PtrFromString(name)
	// manual-reset=true(1), initial-state=not-signaled(0)
	h, _, err := procCreateEventW.Call(0, 1, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return syscall.InvalidHandle, err
	}
	return syscall.Handle(h), nil
}

// SignalCleanShutdown opens and signals the clean-shutdown event.
// Called by main before a user-initiated Quit.
func SignalCleanShutdown() { openAndSignal(wdEventCleanShutdown) }

// SignalUpdateRestart opens and signals the update-restart event.
// Wired to core.UpdateSignalFunc; called by ApplyPendingUpdate before OTA os.Exit.
func SignalUpdateRestart() { openAndSignal(wdEventUpdateRestart) }

func openAndSignal(name string) {
	namePtr, _ := syscall.UTF16PtrFromString(name)
	h, _, _ := procOpenEventW.Call(wdEventAccess, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return // event not yet created (no watchdog running) â€” safe no-op
	}
	defer syscall.CloseHandle(syscall.Handle(h)) //nolint:errcheck
	procSetEventW.Call(h)
}

// â”€â”€ Watchdog mutex â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// HoldWatchdogMutex creates the named mutex that advertises the watchdog's presence.
// The returned handle must be kept open (leaked or stored) for the process lifetime.
// Returns an error if a watchdog is already running.
func HoldWatchdogMutex() (syscall.Handle, error) {
	namePtr, _ := syscall.UTF16PtrFromString(wdMutexName)
	h, _, err := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
	if err == syscall.Errno(183) { // ERROR_ALREADY_EXISTS
		if h != 0 {
			syscall.CloseHandle(syscall.Handle(h)) //nolint:errcheck
		}
		return syscall.InvalidHandle, fmt.Errorf("watchdog already running")
	}
	if h == 0 {
		return syscall.InvalidHandle, fmt.Errorf("watchdog: create mutex: %w", err)
	}
	return syscall.Handle(h), nil
}

// WatchdogRunning returns true if the watchdog process is currently running.
func WatchdogRunning() bool {
	namePtr, _ := syscall.UTF16PtrFromString(wdMutexName)
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return false
	}
	already := err == syscall.Errno(183)
	syscall.CloseHandle(syscall.Handle(h)) //nolint:errcheck
	return already
}

// â”€â”€ Process helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// WatchdogStartMain launches the current executable without the --watchdog flag.
// Used by the watchdog for the initial main process start.
func WatchdogStartMain() (*os.Process, error) {
	return watchdogLaunchMain()
}

// WatchdogRestartMain launches main with --restarted so the splash is suppressed.
// Used by the watchdog when restarting after a crash.
func WatchdogRestartMain() (*os.Process, error) {
	return watchdogLaunchMain("--restarted")
}

func watchdogLaunchMain(extraArgs ...string) (*os.Process, error) {
	exe, err := watchdogSelfExe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, extraArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("watchdog: start main: %w", err)
	}
	return cmd.Process, nil
}

// StartWatchdog launches the current executable with the --watchdog flag.
// Used by main to start the watchdog if it is not already running.
func StartWatchdog() (*os.Process, error) {
	exe, err := watchdogSelfExe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "--watchdog")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
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

// OpenProcessForWait opens a process handle suitable for WaitForSingleObject.
// Returns syscall.InvalidHandle on failure (PID not found or access denied).
func OpenProcessForWait(pid int) syscall.Handle {
	h, _, _ := procOpenProcess.Call(wdProcessSync, 0, uintptr(pid))
	return syscall.Handle(h)
}

// WaitProcess blocks until the process identified by h exits.
func WaitProcess(h syscall.Handle) {
	procWaitSingleObj.Call(uintptr(h), wdInfinite)
}

// WaitProcessTimeout waits up to d for the process to exit.
// Returns true if the process exited, false if the timeout elapsed.
func WaitProcessTimeout(h syscall.Handle, d time.Duration) bool {
	ms := uint32(d.Milliseconds())
	r, _, _ := procWaitSingleObj.Call(uintptr(h), uintptr(ms))
	return r == wdWaitObject0
}

// TerminateProcess forcibly terminates the process with the given handle.
func TerminateProcess(h syscall.Handle) {
	procTerminate := modKernel32.MustFindProc("TerminateProcess")
	procTerminate.Call(uintptr(h), 1) //nolint:errcheck
}

// â”€â”€ Session cleanup â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// CleanupSession restores networking state after an unclean main exit.
// Removes DoH config and split-tunnel routes. Idempotent â€” safe to call even if
// cleanup was already performed.
func CleanupSession(origGW string) {
	core.Log.Printf("watchdog: cleanup: restoring networking state (origGW=%q)", origGW)
	CleanupDoH()
	cleanupSplitRoutes(origGW)
}

// cleanupSplitRoutes removes split-tunnel routes without a live RouteManager.
func cleanupSplitRoutes(origGW string) {
	routeCmd("delete", "0.0.0.0", "mask", "128.0.0.0")       //nolint:errcheck
	routeCmd("delete", "128.0.0.0", "mask", "128.0.0.0")     //nolint:errcheck
	routeCmd("delete", "1.1.1.1", "mask", "255.255.255.255") //nolint:errcheck
	if origGW != "" {
		// Re-add plain-DNS bypass so 1.1.1.1 is reachable via the original gateway.
		routeCmd("add", "1.1.1.1", "mask", "255.255.255.255", origGW) //nolint:errcheck
	}
	core.Log.Printf("watchdog: cleanup: routes removed")
}
