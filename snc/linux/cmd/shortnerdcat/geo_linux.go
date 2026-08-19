// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package main

import (
	"os"
	"os/exec"
	"strings"

	"tunnel_cat/snc/core"
)

// detectDeviceCC returns the best available ISO 3166-1 alpha-2 country code
// for the device's location, trying sources in priority order:
//  1. LANG / LANGUAGE environment variable (e.g. "en_US.UTF-8" â†’ "US")
//  2. localectl system locale (systemd systems)
//  3. IANA timezone â†’ country fallback
//
// Returns "" if none of the sources yield a result.
func detectDeviceCC() string {
	if cc := localeCC(); cc != "" {
		core.Log.Printf("geo: locale country=%q", cc)
		return cc
	}
	if cc := localectlCC(); cc != "" {
		core.Log.Printf("geo: localectl country=%q", cc)
		return cc
	}
	if cc := core.TimezoneCC(); cc != "" {
		core.Log.Printf("geo: timezone country=%q", cc)
		return cc
	}
	return ""
}

// localeCC extracts the country code from the LANG or LANGUAGE environment variable.
// LANG typically looks like "ru_RU.UTF-8" or "en_US.UTF-8".
func localeCC() string {
	for _, env := range []string{"LANG", "LANGUAGE"} {
		locale := os.Getenv(env)
		if locale == "" {
			continue
		}
		// Strip encoding suffix.
		if i := strings.IndexByte(locale, '.'); i >= 0 {
			locale = locale[:i]
		}
		// Strip modifier.
		if i := strings.IndexByte(locale, '@'); i >= 0 {
			locale = locale[:i]
		}
		// Split on '_' or '-' to get language and territory.
		locale = strings.ReplaceAll(locale, "-", "_")
		parts := strings.SplitN(locale, "_", 2)
		if len(parts) < 2 {
			continue
		}
		cc := strings.ToUpper(parts[1])
		if len(cc) == 2 {
			return cc
		}
	}
	return ""
}

// localectlCC reads the system locale via localectl (systemd-only).
func localectlCC() string {
	out, err := exec.Command("localectl", "status").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Line like: "System Locale: LANG=ru_RU.UTF-8"
		if strings.HasPrefix(line, "System Locale:") {
			if i := strings.Index(line, "LANG="); i >= 0 {
				lang := line[i+5:]
				if j := strings.IndexAny(lang, " .\t"); j >= 0 {
					lang = lang[:j]
				}
				lang = strings.ReplaceAll(lang, "-", "_")
				parts := strings.SplitN(lang, "_", 2)
				if len(parts) == 2 {
					cc := strings.ToUpper(parts[1])
					if len(cc) == 2 {
						return cc
					}
				}
			}
		}
	}
	return ""
}
