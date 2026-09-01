// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"strings"
	"syscall"
	"unsafe"
)

// currentLang is detected once at process startup from the user's Windows UI
// language. "ru" selects Russian translations from stringsRU (falling back
// to English for any key missing there); anything else uses English.
var currentLang = detectLang()

// detectLang reads the user's Windows UI language via GetLocaleInfoEx
// (LOCALE_SNAME on the user-default locale), following the same
// LazyDLL/NewProc("GetLocaleInfoEx") pattern as systemCC() in
// cmd/shortnerdcat/geo_windows.go, but reading LOCALE_SNAME (a full locale
// name like "ru-RU" or "en-US") instead of LOCALE_SISO3166CTRYNAME.
func detectLang() string {
	const localeSName = 0x5c // LOCALE_SNAME
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetLocaleInfoEx")
	buf := make([]uint16, 85) // LOCALE_NAME_MAX_LENGTH
	r, _, _ := proc.Call(
		0, // LOCALE_NAME_USER_DEFAULT = NULL
		localeSName,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r == 0 {
		return "en"
	}
	name := strings.ToLower(syscall.UTF16ToString(buf))
	if strings.HasPrefix(name, "ru") {
		return "ru"
	}
	return "en"
}

// T returns the localized string for key. If currentLang is "ru" and a
// Russian translation exists, that's returned; otherwise it falls back to
// the English string, and finally to the key itself if even that's missing
// (so a missing translation shows up as an obviously-wrong string rather
// than an empty label).
func T(key string) string {
	if currentLang == "ru" {
		if s, ok := stringsRU[key]; ok {
			return s
		}
	}
	if s, ok := stringsEN[key]; ok {
		return s
	}
	return key
}
