// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package main

import (
	"os/exec"
	"strings"

	core "tunnel_cat/snc/core"
)

// detectDeviceCC returns the best available ISO 3166-1 alpha-2 country code
// for the device's current physical location, trying sources in priority order:
//  1. macOS system locale (AppleLocale default) â€” instant, no dialog
//  2. IANA timezone â†’ country fallback
//
// Returns "" if none of the sources yield a result.
func detectDeviceCC() string {
	if cc := systemCC(); cc != "" {
		core.Log.Printf("geo: system-locale country=%q", cc)
		return cc
	}
	if cc := core.TimezoneCC(); cc != "" {
		core.Log.Printf("geo: timezone country=%q", cc)
		return cc
	}
	return ""
}

// systemCC reads the AppleLocale preference (e.g. "en_US") and extracts the
// ISO country code from the territory part.
func systemCC() string {
	out, err := exec.Command("defaults", "read", "-g", "AppleLocale").Output()
	if err != nil {
		return ""
	}
	// AppleLocale looks like "en_US", "en_US@calendar=gregorian", "zh_CN", etc.
	locale := strings.TrimSpace(string(out))
	// Strip options after '@'.
	if i := strings.IndexByte(locale, '@'); i >= 0 {
		locale = locale[:i]
	}
	// Split on '_' or '-'.
	locale = strings.ReplaceAll(locale, "-", "_")
	parts := strings.SplitN(locale, "_", 2)
	if len(parts) < 2 {
		return ""
	}
	cc := strings.ToUpper(parts[1])
	if len(cc) != 2 {
		return ""
	}
	return cc
}
