// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"tunnel_cat/snc/core"
)

// LoggedInSession describes a user session on the local display.
type LoggedInSession struct {
	UID     string // numeric UID
	Display string // e.g. ":0"
	DBus    string // DBUS_SESSION_BUS_ADDRESS
	Home    string // home directory
}

// DetectLoggedInSession finds the first active graphical session.
//
// Strategy:
//  1. Use loginctl list-sessions if available.
//  2. Fall back to scanning /run/user/ directories.
//  3. For each candidate UID, pull DISPLAY / DBUS_SESSION_BUS_ADDRESS from
//     the environment of a process owned by that UID.
func DetectLoggedInSession() *LoggedInSession {
	// Try loginctl first â€” cleanest method on systemd systems.
	if sess := sessionViaLoginctl(); sess != nil {
		return sess
	}
	// Fall back: scan /run/user directories.
	return sessionViaRunUser()
}

func sessionViaLoginctl() *LoggedInSession {
	out, err := exec.Command("loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return nil
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Fields: SESSION UID USER SEAT TTY
		if len(fields) < 3 {
			continue
		}
		uid := fields[1]
		sessionID := fields[0]
		// Check if this is a graphical session.
		typeOut, err := exec.Command("loginctl", "show-session", sessionID, "--property=Type", "--value").Output()
		if err != nil {
			continue
		}
		sessType := strings.TrimSpace(string(typeOut))
		if sessType != "x11" && sessType != "wayland" && sessType != "mir" {
			continue
		}
		sess := &LoggedInSession{UID: uid}
		// Get home directory.
		if homeOut, err := exec.Command("loginctl", "show-user", uid, "--property=HomeDirectory", "--value").Output(); err == nil {
			sess.Home = strings.TrimSpace(string(homeOut))
		} else {
			sess.Home = homeFromPasswd(uid)
		}
		// Pull DISPLAY and DBUS from process environment.
		enrichFromProcEnv(sess)
		if sess.Display != "" || sess.DBus != "" {
			core.Log.Printf("session: detected via loginctl uid=%s type=%s", uid, sessType)
			return sess
		}
	}
	return nil
}

func sessionViaRunUser() *LoggedInSession {
	entries, err := os.ReadDir("/run/user")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		uid := e.Name()
		if _, err := strconv.Atoi(uid); err != nil {
			continue
		}
		if uid == "0" {
			continue // skip root
		}
		sess := &LoggedInSession{
			UID:  uid,
			Home: homeFromPasswd(uid),
		}
		enrichFromProcEnv(sess)
		if sess.Display != "" || sess.DBus != "" {
			core.Log.Printf("session: detected via /run/user uid=%s", uid)
			return sess
		}
	}
	return nil
}

// enrichFromProcEnv reads /proc/<pid>/environ for processes owned by uid
// to extract DISPLAY and DBUS_SESSION_BUS_ADDRESS.
func enrichFromProcEnv(sess *LoggedInSession) {
	uidNum, err := strconv.Atoi(sess.UID)
	if err != nil {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		// Check process owner via /proc/<pid>/status.
		statusPath := filepath.Join("/proc", e.Name(), "status")
		statusData, err := os.ReadFile(statusPath)
		if err != nil {
			continue
		}
		var procUID int = -1
		for _, line := range strings.Split(string(statusData), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					procUID, _ = strconv.Atoi(fields[1])
				}
				break
			}
		}
		if procUID != uidNum {
			continue
		}
		envPath := filepath.Join("/proc", e.Name(), "environ")
		envData, err := os.ReadFile(envPath)
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(envData), "\x00") {
			if strings.HasPrefix(kv, "DISPLAY=") && sess.Display == "" {
				sess.Display = strings.TrimPrefix(kv, "DISPLAY=")
			}
			if strings.HasPrefix(kv, "DBUS_SESSION_BUS_ADDRESS=") && sess.DBus == "" {
				sess.DBus = strings.TrimPrefix(kv, "DBUS_SESSION_BUS_ADDRESS=")
			}
		}
		if sess.Display != "" && sess.DBus != "" {
			return
		}
	}
}

// homeFromPasswd reads /etc/passwd to find the home directory for the given UID.
func homeFromPasswd(uid string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.SplitN(sc.Text(), ":", 7)
		if len(fields) >= 6 && fields[2] == uid {
			return fields[5]
		}
	}
	return ""
}
