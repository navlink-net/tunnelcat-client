// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package macos

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// IPCMsg is sent from the main (root/VPN) process to the tray (user) process.
type IPCMsg struct {
	T string `json:"t"`

	// "init" — sent once after socket connect
	Version     string `json:"version,omitempty"`
	InitLogin   bool   `json:"init_login,omitempty"`
	AutoConnect bool   `json:"auto_connect,omitempty"`
	DOH         bool   `json:"doh,omitempty"`
	BlockQUIC   bool   `json:"block_quic,omitempty"`
	Region      string `json:"region,omitempty"`
	LogDir      string `json:"log_dir,omitempty"`

	// "status" — state machine update
	// State: "idle" | "pending" | "connected" | "error" | "login_error"
	State string `json:"state,omitempty"`
	Msg   string `json:"msg,omitempty"` // error text, auth warning text, etc.

	// "update" — new client version available
	UpdateVersion string `json:"update_version,omitempty"`

	// "notify" — push notification strings
	Msgs []string `json:"msgs,omitempty"`

	// "ask_key" — main wants a subscription key from the user

	// "club_theme" — daemon reports the current club membership theme, once
	// ClubDiscoverer confirms it (see tunnel_cat/docs/club-membership.md).
	// ClubTheme is "" (regular), "catclub", or "elite"; ClubBadge is the
	// header chevron text ("Cat Club Member #N"), "" for the regular tier.
	// ClubTheme reflects the *effective* theme (admin preview override applied
	// server-side in the daemon if one is active), same as every other client.
	ClubTheme string `json:"club_theme,omitempty"`
	ClubBadge string `json:"club_badge,omitempty"`

	// "admin_status" — daemon reports live admin status (see
	// core.AdminStatusPoller's doc comment for why this is polled live from
	// navlink.net rather than trusted once from the key string). Gates the
	// tray's admin-only Club Theme preview submenu. Also included on every
	// "club_theme" push so the tray never has to reconcile two independent
	// message types racing each other.
	IsAdmin bool `json:"is_admin,omitempty"`

	// "can_recommend" — included alongside club_theme/admin_status pushes;
	// gates the tray's "Recommend new Cat Club members" menu item. True iff
	// the account currently has direct or subsumed Cat Club access.
	CanRecommend bool `json:"can_recommend,omitempty"`
}

// IPCCmd is sent from the tray (user) process to the main (root/VPN) process.
type IPCCmd struct {
	T string `json:"t"`
	// "key" — user submitted subscription key
	Key string `json:"key,omitempty"`
	// "settings"
	BlockQUIC     bool   `json:"block_quic,omitempty"`
	DOH           bool   `json:"doh,omitempty"`
	Region        string `json:"region,omitempty"`
	AutoReconnect bool   `json:"auto_reconnect,omitempty"`

	// "recommend" — tray forwards a Cat Club recommendation submitted in the
	// Settings panel; the daemon holds the session token needed to actually
	// call the arbiter, so this can't be done tray-side (see
	// tunnel_cat/docs/club-membership.md).
	TargetUsername string `json:"target_username,omitempty"`

	// "club_theme_preview" — admin-only theme preview override, selected
	// from the tray's Club Theme submenu. PreviewTheme is "" (clear
	// override, back to the account's real theme), "catclub", or "elite".
	// The daemon ignores this if the account isn't currently admin (same
	// server-side gate as every other client -- the tray hiding/graying the
	// menu is a UI nicety, not the actual enforcement).
	PreviewTheme string `json:"preview_theme,omitempty"`
}

// IPCConn wraps a net.Conn with line-delimited JSON encoding.
type IPCConn struct {
	conn net.Conn
	scan *bufio.Scanner
}

func NewIPCConn(c net.Conn) *IPCConn {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &IPCConn{conn: c, scan: sc}
}

func (c *IPCConn) SendMsg(m IPCMsg) error {
	b, _ := json.Marshal(m)
	b = append(b, '\n')
	_, err := c.conn.Write(b)
	return err
}

func (c *IPCConn) SendCmd(cmd IPCCmd) error {
	b, _ := json.Marshal(cmd)
	b = append(b, '\n')
	_, err := c.conn.Write(b)
	return err
}

func (c *IPCConn) ReadMsg() (IPCMsg, error) {
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return IPCMsg{}, err
		}
		return IPCMsg{}, fmt.Errorf("connection closed")
	}
	var m IPCMsg
	return m, json.Unmarshal(c.scan.Bytes(), &m)
}

func (c *IPCConn) ReadCmd() (IPCCmd, error) {
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return IPCCmd{}, err
		}
		return IPCCmd{}, fmt.Errorf("connection closed")
	}
	var cmd IPCCmd
	return cmd, json.Unmarshal(c.scan.Bytes(), &cmd)
}

func (c *IPCConn) Close() { c.conn.Close() }

func IPCSocketPath(uid string) string {
	return fmt.Sprintf("/tmp/snc-%s.sock", uid)
}

// IPCListen creates a Unix socket owned by uid so the user process can connect.
func IPCListen(path, uid string) (net.Listener, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0600) //nolint:errcheck
	if n, err := strconv.Atoi(uid); err == nil {
		os.Chown(path, n, -1) //nolint:errcheck
	}
	return ln, nil
}

// IPCDial connects to the Unix socket, retrying until timeout.
func IPCDial(path string, timeout time.Duration) (*IPCConn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", path)
		if err == nil {
			return NewIPCConn(c), nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("dial %s: %w", path, lastErr)
}
