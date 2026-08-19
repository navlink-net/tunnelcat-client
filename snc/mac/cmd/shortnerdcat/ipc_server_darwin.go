// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package main

import (
	"net"
	"sync"

	snmac "shortnerdcat/snc/mac/macos"
	"tunnel_cat/snc/core"
)

// ipcServer is the root-process side of the tray IPC.
// It accepts one connection from the tray process, forwards commands to cmdCh,
// and lets the VPN logic push status updates back to the tray.
type ipcServer struct {
	mu   sync.Mutex
	conn *snmac.IPCConn

	// settings last received from the tray process
	doh    bool
	region string

	// cmdCh receives commands from the tray process (connect, disconnect, key, â€¦)
	cmdCh chan snmac.IPCCmd

	// reconnectCh receives reconnect signals (from tray retry logic or power events).
	reconnectCh chan struct{}

	// keyCh receives the key string when the tray responds to an "ask_key" message.
	// "" means the user cancelled.
	keyCh chan string
}

func newIPCServer() *ipcServer {
	return &ipcServer{
		cmdCh:       make(chan snmac.IPCCmd, 16),
		reconnectCh: make(chan struct{}, 1),
		keyCh:       make(chan string), // unbuffered: select detects AskKey() presence
	}
}

// acceptLoop waits for the tray process to connect, sends the init message,
// then pumps incoming commands into cmdCh. Runs in a goroutine.
func (s *ipcServer) acceptLoop(ln net.Listener) {
	defer ln.Close()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conn := snmac.NewIPCConn(c)
		s.mu.Lock()
		s.conn = conn
		s.mu.Unlock()
		core.Log.Printf("ipc: tray connected")
		s.readLoop(conn)
		s.mu.Lock()
		s.conn = nil
		s.mu.Unlock()
		core.Log.Printf("ipc: tray disconnected")
	}
}

func (s *ipcServer) readLoop(conn *snmac.IPCConn) {
	for {
		cmd, err := conn.ReadCmd()
		if err != nil {
			return
		}
		switch cmd.T {
		case "settings":
			s.mu.Lock()
			s.doh = cmd.DOH
			s.region = cmd.Region
			s.mu.Unlock()
			// settings change may trigger reconnect â€” send to cmdCh for handling
			select {
			case s.cmdCh <- cmd:
			default:
			}
		case "reconnect":
			select {
			case s.reconnectCh <- struct{}{}:
			default:
			}
		case "key", "key_cancel":
			// If AskKey() is blocking on keyCh, deliver directly (unbuffered handoff).
			// Otherwise route to cmdCh so the main loop handles user-initiated login.
			select {
			case s.keyCh <- cmd.Key:
			default:
				select {
				case s.cmdCh <- cmd:
				default:
					core.Log.Printf("ipc: cmdCh full, dropped %q", cmd.T)
				}
			}
		default:
			select {
			case s.cmdCh <- cmd:
			default:
				core.Log.Printf("ipc: cmdCh full, dropped %q", cmd.T)
			}
		}
	}
}

// SendInit sends the init message to the tray process after it connects.
func (s *ipcServer) SendInit(version, logDir string, initialLogin, autoConnect, doh, blockQUIC bool, region string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{ //nolint:errcheck
		T:           "init",
		Version:     version,
		LogDir:      logDir,
		InitLogin:   initialLogin,
		AutoConnect: autoConnect,
		DOH:         doh,
		BlockQUIC:   blockQUIC,
		Region:      region,
	})
}

// PushStatus sends a status update to the tray process.
func (s *ipcServer) PushStatus(state, msg string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "status", State: state, Msg: msg}) //nolint:errcheck
}

// PushClubTheme sends the current club membership theme + badge text, live
// admin status, and Cat Club recommend eligibility to the tray process,
// which forwards it to the window (see tunnel_cat/docs/club-membership.md).
// isAdmin gates the tray's Club Theme preview submenu; canRecommend gates
// the "Recommend new Cat Club members" menu item. Safe to call from any
// goroutine (e.g. a ClubDiscoverer membership callback or AdminStatusPoller).
func (s *ipcServer) PushClubTheme(theme, badge string, isAdmin, canRecommend bool) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{ //nolint:errcheck
		T: "club_theme", ClubTheme: theme, ClubBadge: badge,
		IsAdmin: isAdmin, CanRecommend: canRecommend,
	})
}

// PushAuthWarn tells the tray to show an auth-warning tooltip.
func (s *ipcServer) PushAuthWarn(msg string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "auth_warn", Msg: msg}) //nolint:errcheck
}

// ClearAuthWarn tells the tray to restore normal connected state after a warning.
func (s *ipcServer) ClearAuthWarn() {
	s.PushAuthWarn("")
}

// PushUpdate tells the tray a new version is available.
func (s *ipcServer) PushUpdate(version string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "update", UpdateVersion: version}) //nolint:errcheck
}

// PushNotify sends notification strings to the tray.
func (s *ipcServer) PushNotify(msgs []string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "notify", Msgs: msgs}) //nolint:errcheck
}

// AskKey asks the tray to show a key-entry dialog and waits for the result.
// Returns ("", false) if the user cancelled or tray disconnected.
func (s *ipcServer) AskKey() (string, bool) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return "", false
	}
	conn.SendMsg(snmac.IPCMsg{T: "ask_key"}) //nolint:errcheck
	key := <-s.keyCh
	return key, key != ""
}

// TriggerReconnect sends a reconnect signal to the tray (which will
// call doReconnect via its retry/reconnect machinery).
func (s *ipcServer) TriggerReconnect() {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "reconnect"}) //nolint:errcheck
}

func (s *ipcServer) IsDOH() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doh
}

func (s *ipcServer) Region() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.region
}
