// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

// CGo bridge for IOKit sleep/wake notifications.
// The exported Go symbol (WatchPowerEvents) and shared state (globalOnWake)
// live in power_darwin.go so that gopls can resolve them without CGo.

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation

#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>
#include <CoreFoundation/CoreFoundation.h>

// Definitions live in power_darwin.c to satisfy CGO's rule:
// files with //export must not have C definitions in their preamble.
extern void startPowerWatcher(void);
extern void allowPowerChange(long notificationID);
*/
import "C"
import (
	"runtime"
	"unsafe"

	"tunnel_cat/snc/core"
)

// sncPowerCallback is the C-callable IOKit notification callback.
// It must be exported so CGo can produce a function pointer for IOKit.
//
//export sncPowerCallback
func sncPowerCallback(refcon unsafe.Pointer, service C.io_service_t, messageType C.natural_t, messageArgument unsafe.Pointer) {
	switch uint32(messageType) {
	case 0xe0000300: // kIOMessageSystemWillSleep
		// Acknowledge the sleep request so macOS is not held waiting.
		C.allowPowerChange(C.long(uintptr(messageArgument)))
		core.Log.Printf("power: system going to sleep â€” acknowledged")

	case 0xe0000320, // kIOMessageSystemHasPoweredOn
		0xe0000200: // kIOMessageSystemWillPowerOn
		fn := globalOnWake.Load()
		if fn != nil && *fn != nil {
			core.Log.Printf("power: wake event (0x%x) â€” triggering reconnect", uint32(messageType))
			// Refresh LastAlive so the watchdog does not misread the stale
			// pre-sleep timestamp as a frozen process, and record LastWake so
			// the connectivity probe skips its 60-second post-wake grace window.
			core.TouchAlive()
			core.TouchLastWake()
			go (*fn)()
		}
	}
}

// powerStartWatcher launches the IOKit CFRunLoop on a dedicated OS thread.
func powerStartWatcher() {
	go func() {
		// CFRunLoop must stay on the same OS thread it is created on.
		runtime.LockOSThread()
		C.startPowerWatcher()
	}()
}
