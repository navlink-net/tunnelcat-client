// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	appName    = "ShortNerdCat"

	// installerFlagKeyPath/installerFlagValueName must match SncInstaller's
	// Autostart.MarkInstalledByInstaller (snc/win/Installer/SncInstaller/
	// Autostart.cs) exactly -- this is how the client tells whether it was
	// set up by the installer (direct self-update from here on) or is a
	// pre-installer install (one-time migration via the installer instead;
	// see IsInstalledByInstaller and core.Updater's two update strategies).
	installerFlagKeyPath   = `Software\ShortNerdCat`
	installerFlagValueName = "InstalledByInstaller"
)

// IsInstalledByInstaller reports whether SncInstaller set up this install.
// A pre-installer install (manually unzipped, or not yet migrated) has no
// such flag and reports false.
func IsInstalledByInstaller() bool {
	val, err := regGetString(syscall.HKEY_CURRENT_USER, installerFlagKeyPath, installerFlagValueName)
	return err == nil && val == "1"
}

// RegisterAutostart adds the current executable (with --watchdog flag) to the
// Windows startup registry key, replacing any stale entries that point to old
// paths of our exe (accumulated from previous installations/downloads).
func RegisterAutostart() error {
	exe, err := currentExePath()
	if err != nil {
		return err
	}
	cleanStaleAutostartEntries(exe)
	return regSetString(syscall.HKEY_CURRENT_USER, runKeyPath, appName, `"`+exe+`" --watchdog`)
}

// RemoveAutostart removes the autostart registry entry.
func RemoveAutostart() error {
	return regDeleteValue(syscall.HKEY_CURRENT_USER, runKeyPath, appName)
}

// IsCurrentExeAutostarted returns true only when the Run entry already points
// to the current executable with the --watchdog flag — not just any path.
// Use this instead of the old IsAutostartEnabled so that a new version always
// re-registers itself even if an older path is already present.
func IsCurrentExeAutostarted() bool {
	val, err := regGetString(syscall.HKEY_CURRENT_USER, runKeyPath, appName)
	if err != nil || val == "" {
		return false
	}
	exe, err := currentExePath()
	if err != nil {
		return false
	}
	return strings.EqualFold(extractRunExePath(val), exe)
}

// cleanStaleAutostartEntries removes all Run-key entries (under any name)
// whose command points to an exe with the same base filename as ours but from
// a different path — leftovers from previous downloads or installations.
// The entry named appName is skipped because RegisterAutostart will overwrite it.
func cleanStaleAutostartEntries(currentExe string) {
	ourName := strings.ToLower(filepath.Base(currentExe))
	vals := regEnumValues(syscall.HKEY_CURRENT_USER, runKeyPath)
	for name, val := range vals {
		if strings.EqualFold(name, appName) {
			continue // will be overwritten by the caller
		}
		path := extractRunExePath(val)
		if strings.EqualFold(filepath.Base(path), ourName) {
			regDeleteValue(syscall.HKEY_CURRENT_USER, runKeyPath, name) //nolint:errcheck
		}
	}
}

// extractRunExePath returns the bare executable path from a Run-key value.
// Run values look like: `"C:\path\exe.exe" --watchdog` or `C:\path\exe.exe`.
func extractRunExePath(val string) string {
	val = strings.TrimSpace(val)
	if strings.HasPrefix(val, `"`) {
		end := strings.Index(val[1:], `"`)
		if end >= 0 {
			return val[1 : end+1]
		}
	}
	if idx := strings.IndexByte(val, ' '); idx >= 0 {
		return val[:idx]
	}
	return val
}

func currentExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

// ── registry helpers ──────────────────────────────────────────────────────────

var (
	modAdvapi32     = syscall.MustLoadDLL("advapi32.dll")
	procRegOpenKey  = modAdvapi32.MustFindProc("RegOpenKeyExW")
	procRegSetValue = modAdvapi32.MustFindProc("RegSetValueExW")
	procRegGetValue = modAdvapi32.MustFindProc("RegGetValueW")
	procRegDelValue = modAdvapi32.MustFindProc("RegDeleteValueW")
	procRegCloseKey = modAdvapi32.MustFindProc("RegCloseKey")
	procRegEnumVal  = modAdvapi32.MustFindProc("RegEnumValueW")
)

const (
	keySetValue   = 0x0002
	keyQueryValue = 0x0001
	regSZ         = 1
	// RRF_RT_REG_SZ | RRF_NOEXPAND
	rrf = 0x00000002 | 0x10000000
)

func regOpenKey(root syscall.Handle, path string, access uint32) (syscall.Handle, error) {
	pathPtr, _ := syscall.UTF16PtrFromString(path)
	var key syscall.Handle
	r, _, _ := procRegOpenKey.Call(
		uintptr(root),
		uintptr(unsafe.Pointer(pathPtr)),
		0,
		uintptr(access),
		uintptr(unsafe.Pointer(&key)),
	)
	if r != 0 {
		return 0, syscall.Errno(r)
	}
	return key, nil
}

func regSetString(root syscall.Handle, keyPath, valueName, value string) error {
	key, err := regOpenKey(root, keyPath, keySetValue)
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(uintptr(key)) //nolint:errcheck

	namePtr, _ := syscall.UTF16PtrFromString(valueName)
	data, _ := syscall.UTF16FromString(value)
	dataBytes := unsafe.Slice((*byte)(unsafe.Pointer(&data[0])), len(data)*2)

	r, _, _ := procRegSetValue.Call(
		uintptr(key),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(&dataBytes[0])),
		uintptr(len(dataBytes)),
	)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func regGetString(root syscall.Handle, keyPath, valueName string) (string, error) {
	keyPtr, _ := syscall.UTF16PtrFromString(keyPath)
	namePtr, _ := syscall.UTF16PtrFromString(valueName)
	buf := make([]uint16, 512)
	size := uint32(len(buf) * 2)
	r, _, _ := procRegGetValue.Call(
		uintptr(root),
		uintptr(unsafe.Pointer(keyPtr)),
		uintptr(unsafe.Pointer(namePtr)),
		rrf,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 {
		return "", syscall.Errno(r)
	}
	return syscall.UTF16ToString(buf), nil
}

func regDeleteValue(root syscall.Handle, keyPath, valueName string) error {
	key, err := regOpenKey(root, keyPath, keySetValue)
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(uintptr(key)) //nolint:errcheck

	namePtr, _ := syscall.UTF16PtrFromString(valueName)
	r, _, _ := procRegDelValue.Call(uintptr(key), uintptr(unsafe.Pointer(namePtr)))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// regEnumValues returns all string values in the given Run key as name→data map.
func regEnumValues(root syscall.Handle, keyPath string) map[string]string {
	key, err := regOpenKey(root, keyPath, keyQueryValue)
	if err != nil {
		return nil
	}
	defer procRegCloseKey.Call(uintptr(key)) //nolint:errcheck

	result := make(map[string]string)
	for i := uint32(0); ; i++ {
		nameBuf := make([]uint16, 256)
		nameLen := uint32(len(nameBuf)) // in chars (including NUL)
		dataBuf := make([]uint16, 1024)
		dataLen := uint32(len(dataBuf) * 2) // in bytes
		var typ uint32

		r, _, _ := procRegEnumVal.Call(
			uintptr(key),
			uintptr(i),
			uintptr(unsafe.Pointer(&nameBuf[0])),
			uintptr(unsafe.Pointer(&nameLen)),
			0,
			uintptr(unsafe.Pointer(&typ)),
			uintptr(unsafe.Pointer(&dataBuf[0])),
			uintptr(unsafe.Pointer(&dataLen)),
		)
		const errorNoMoreItems = 259
		if r == errorNoMoreItems {
			break
		}
		if r != 0 {
			break
		}
		if typ == regSZ {
			name := syscall.UTF16ToString(nameBuf[:nameLen])
			val := syscall.UTF16ToString(dataBuf[:dataLen/2])
			result[name] = val
		}
	}
	return result
}
