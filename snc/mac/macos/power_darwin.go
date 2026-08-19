// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

// Public API for macOS sleep/wake notifications.
// CGo internals (IOKit callback + CFRunLoop) live in power_cgo_darwin.go;
// this file is CGo-free so that gopls and other tools can resolve
// WatchPowerEvents correctly — mirroring the window_darwin.go / window_cgo_darwin.go split.

import (
	"sync/atomic"
)

// globalOnWake holds the user-supplied wake callback registered via
// WatchPowerEvents.  Stored atomically so the CGo callback in
// power_cgo_darwin.go can read it without a mutex.
var globalOnWake atomic.Pointer[func()]

// WatchPowerEvents starts the IOKit sleep/wake listener.  onWake is called
// (in a new goroutine) each time the system wakes from sleep.
func WatchPowerEvents(onWake func()) {
	if onWake == nil {
		return
	}
	globalOnWake.Store(&onWake)
	powerStartWatcher()
}
