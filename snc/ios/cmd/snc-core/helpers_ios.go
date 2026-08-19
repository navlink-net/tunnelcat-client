// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build ios

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	snc "tunnel_cat/snc/core"
)

// â”€â”€ Persistence â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func loadOrCreateDeviceID(dir string) string {
	path := filepath.Join(dir, "device_id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	id := uuid.New().String()
	os.WriteFile(path, []byte(id), 0600) //nolint:errcheck
	return id
}

func saveCountry(dir, cc string) {
	os.WriteFile(filepath.Join(dir, "country.txt"), []byte(cc), 0600) //nolint:errcheck
}

func loadCountry(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "country.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeState writes a one-word tunnel state to snc.state so the Swift app
// can poll it via the shared App Group container.
func writeState(dir, state string) {
	os.WriteFile(filepath.Join(dir, "snc.state"), []byte(state), 0644) //nolint:errcheck
}

// attachFatalErrorHook wires SetFatalErrorHook on an already-authenticated
// dialer -- the bootstrap-time isDenial/writeState("key_denied") pair in
// lib_ios.go only covers auth failures during the initial connect/pool-build
// loop. Once a session is running, a token refresh can still fail and give
// up permanently (TunnelDialer.refreshToken's fatal path, tunnel_cat/snc/core/
// tunnel.go) -- until this, nothing on iOS ever wired that hook at all, so a
// live "your key/session was rejected" event went completely unsurfaced: the
// tunnel just stopped working with no state change and no UI reaction
// (found while auditing all 5 platforms for this, 2026-08-16 -- see the
// Windows/Mac/Linux/Android commits from the same investigation).
// Reuses the existing "key_denied" state string/App-Group pipeline already
// wired for the bootstrap case, rather than inventing a second signal.
func attachFatalErrorHook(dataDir string, d *snc.TunnelDialer) {
	d.SetFatalErrorHook(func(err error) {
		snc.Log.Printf("snc-core-ios: fatal re-auth failure: %v", err)
		if strings.Contains(err.Error(), "server unavailable") {
			return // transient/arbiter-down exhaustion -- keep retrying silently
		}
		writeState(dataDir, "key_denied")
	})
}

// â”€â”€ Notifications â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func notifSeenPath(dir string) string {
	return filepath.Join(dir, "notif_seen.json")
}

func loadNotifSeen(dir string) map[string]bool {
	data, err := os.ReadFile(notifSeenPath(dir))
	if err != nil {
		return map[string]bool{}
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return map[string]bool{}
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func saveNotifSeen(dir string, seen map[string]bool) {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	data, _ := json.Marshal(ids)
	os.WriteFile(notifSeenPath(dir), data, 0600) //nolint:errcheck
}

// writeNotifsSeen filters new notifications, appends them to snc.notif for
// the Swift app to display via UNUserNotificationCenter, and records seen IDs.
func writeNotifsSeen(dir string, notifs []snc.Notification) {
	if len(notifs) == 0 {
		return
	}
	seen := loadNotifSeen(dir)
	now := time.Now().Unix()
	var newMsgs []string
	for _, n := range notifs {
		if seen[n.ID] || now-n.CreatedAt > 24*3600 {
			continue
		}
		newMsgs = append(newMsgs, n.Message)
		seen[n.ID] = true
	}
	if len(newMsgs) == 0 {
		return
	}
	saveNotifSeen(dir, seen)
	data, _ := json.Marshal(newMsgs)
	os.WriteFile(filepath.Join(dir, "snc.notif"), data, 0644) //nolint:errcheck
	snc.Log.Printf("snc-core-ios: wrote %d notification(s) to snc.notif", len(newMsgs))
}

// writeUserNotifs drains per-user notifications from a fresh Authenticator
// and writes them to snc.notif with deduplication.
func writeUserNotifs(dir string, a *snc.Authenticator) {
	writeNotifsSeen(dir, a.DrainNotifications())
}
