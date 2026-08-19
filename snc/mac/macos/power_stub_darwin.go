// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin && !cgo

package macos

// powerStartWatcher is a no-op stub used when CGo is unavailable (e.g. when
// gopls analyses the package on a non-macOS host).  The real implementation
// lives in power_cgo_darwin.go and is compiled into production macOS builds.
func powerStartWatcher() {}
