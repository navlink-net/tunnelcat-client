// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

// CGo bridge to the Objective-C splash screen and About dialog.
// Kept separate from window_cgo_darwin.go to avoid mixing concerns.

/*
#cgo LDFLAGS: -framework Cocoa
#include "window_cocoa.h"
#include <stdlib.h>
*/
import "C"

import (
	"time"
	"unsafe"
)

// ShowSplash displays the startup splash screen for duration, then dismisses it.
// Blocks the goroutine for duration so the caller can sequence the splash before
// creating the main window.  Must be called after the Cocoa run loop has started
// (i.e., from within the tray ready callback goroutine).
func ShowSplash(version string, duration time.Duration) {
	logo := readAsset("logo.png")
	if len(logo) == 0 {
		return
	}
	cv := C.CString(version)
	// snc_splash_open copies pngData into NSData synchronously, so it is safe
	// to let the Go GC reclaim `logo` once the call returns.
	C.snc_splash_open((*C.uchar)(unsafe.Pointer(&logo[0])), C.int(len(logo)), cv)
	C.free(unsafe.Pointer(cv))

	time.Sleep(duration)

	// snc_splash_close is asynchronous (dispatch_async to main queue); the next
	// dispatch_async calls from NewSNCWindow will be queued after it, so the
	// splash is guaranteed to close before the app window appears.
	C.snc_splash_close()
}

