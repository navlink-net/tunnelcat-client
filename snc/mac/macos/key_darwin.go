// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"os"
	"path/filepath"
	"strings"
)

// SaveKey writes the activation key string to appDataDir/key.dat.
// Protected by Unix file permissions (0600); the directory is owned by the user.
func SaveKey(appDataDir, keyStr string) error {
	if err := os.MkdirAll(appDataDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(appDataDir, "key.dat"), []byte(keyStr), 0600)
}

// LoadKey reads the stored activation key string from appDataDir/key.dat.
func LoadKey(appDataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(appDataDir, "key.dat"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
