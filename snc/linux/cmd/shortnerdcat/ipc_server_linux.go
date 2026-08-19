// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package main

import (
	"fmt"
	"net"
	"sync"
	"time"

	snmac "shortnerdcat/snc/mac/macos"
	"tunnel_cat/snc/core"
)

// ipcServer is the root-process side of the tray IPC.
// Accepts one connection from the tray process, forwards commands to cmdCh,
// and lets the VPN logic push status updates back to the tray.
type ipcServer struct {
	mu   sync.Mutex
	conn *snmac.IPCConn

	doh    bool
	region string

	// cliState tracks the current tunnel state for CLI status queries.
	cliState       string
	cliConnectedAt time.Time

	cmdCh       chan snmac.IPCCmd
	reconnectCh chan struct{}
	keyCh       chan string
}

func newIPCServer() *ipcServer {
	return &ipcServer{
		cmdCh:       make(chan snmac.IPCCmd, 16),
		reconnectCh: make(chan struct{}, 1),
		keyCh:       make(chan string),
	}
}

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

func (s *ipcServer) PushStatus(state, msg string) {
	s.mu.Lock()
	conn := s.conn
	if state == "connected" && s.cliState != "connected" {
		s.cliConnectedAt = time.Now()
	}
	s.cliState = state
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "status", State: state, Msg: msg}) //nolint:errcheck
}

// PushClubTheme sends the current club membership theme + badge text + live
// admin status + Cat Club recommend eligibility to the tray process, which
// forwards it to the window (see tunnel_cat/docs/club-membership.md).
// Mirrors the macOS daemon's method of the same name -- both share
// snmac.IPCMsg. Safe to call from any goroutine (e.g. a ClubDiscoverer
// membership callback or AdminStatusPoller).
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

// CurrentStatus returns the current tunnel state and elapsed time (for CLI queries).
func (s *ipcServer) CurrentStatus() (state, elapsed string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state = s.cliState
	if state == "connected" && !s.cliConnectedAt.IsZero() {
		dur := time.Since(s.cliConnectedAt)
		h := int(dur.Hours())
		m := int(dur.Minutes()) % 60
		sec := int(dur.Seconds()) % 60
		elapsed = fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
	}
	return
}

func (s *ipcServer) PushAuthWarn(msg string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "auth_warn", Msg: msg}) //nolint:errcheck
}

func (s *ipcServer) ClearAuthWarn() {
	s.PushAuthWarn("")
}

func (s *ipcServer) PushUpdate(version string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "update", UpdateVersion: version}) //nolint:errcheck
}

func (s *ipcServer) PushNotify(msgs []string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SendMsg(snmac.IPCMsg{T: "notify", Msgs: msgs}) //nolint:errcheck
}

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
