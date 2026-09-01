// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

// snc-core-proxy â€” Windows SOCKS5 proxy subprocess for Ratatosk.
//
// Launched by Ratatosk's SncManager as a child process. Configuration is
// passed through environment variables:
//
//	SNC_KEY       SNC key string (required)
//	SNC_DATA_DIR  directory for state files and credential cache (required)
//	SNC_LOG_DIR   directory for log files (default = SNC_DATA_DIR)
//
// State files written to SNC_DATA_DIR:
//
//	snc.state       "ok" | "no_controls" | "key_error"
//	snc.socks       "127.0.0.1:PORT" once SOCKS5 is ready (written after snc.state=ok)
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"tunnel_cat/binlog"
	"tunnel_cat/logevent"
	core "tunnel_cat/snc/core"
)

func writeState(dir, state string) {
	_ = os.WriteFile(filepath.Join(dir, "snc.state"), []byte(state), 0o600)
}

func ensureHTTPS(s string) string {
	if strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://" + s
}

// ctrlHostPort normalises a key node address to host:port (default port 443).
func ctrlHostPort(node string) string {
	if _, _, err := net.SplitHostPort(node); err == nil {
		return node
	}
	return node + ":443"
}

func waitForShutdown() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

func main() {
	keyStr := os.Getenv("SNC_KEY")
	dataDir := os.Getenv("SNC_DATA_DIR")
	logDir := os.Getenv("SNC_LOG_DIR")
	if logDir == "" {
		logDir = dataDir
	}
	if keyStr == "" || dataDir == "" {
		fmt.Fprintln(os.Stderr, "snc-core-proxy: SNC_KEY and SNC_DATA_DIR are required")
		if dataDir != "" {
			writeState(dataDir, "key_error")
		}
		os.Exit(1)
	}

	if err := core.InitLogging(logDir); err != nil {
		fmt.Fprintf(os.Stderr, "snc-core-proxy: logging: %v\n", err)
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy,
		logevent.Str(logevent.AttrStage, "starting"))

	_ = os.Remove(filepath.Join(dataDir, "snc.state"))
	_ = os.Remove(filepath.Join(dataDir, "snc.socks"))
	_ = os.Remove(filepath.Join(dataDir, "snc.recoverable"))

	kd, err := core.ParseKeyString(keyStr)
	if err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy,
			logevent.Str(logevent.AttrStage, "invalid_key"),
			logevent.Str(logevent.AttrErr, err.Error()))
		writeState(dataDir, "key_error")
		os.Exit(1)
	}
	if len(kd.Nodes()) == 0 {
		logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy, logevent.Str(logevent.AttrStage, "no_server_addrs"))
		writeState(dataDir, "key_error")
		os.Exit(1)
	}

	// Legacy (V1, unsigned) key: its ControlNodes/Servers list is not
	// verifiable, and this edition has no migration path for it -- refuse to
	// start rather than dialing its unverifiable node list.
	if kd.IsLegacy() {
		core.Log.Printf("snc-core-proxy: legacy V1 key detected for %s, rejected (no migration path)", kd.Username)
		writeState(dataDir, "key_error")
		os.Exit(1)
	}

	runSnc(dataDir, kd)
}

// runSnc bootstraps the normal SNC control-node SOCKS5 proxy: races auth
// against all control nodes and serves SOCKS5 over the first that succeeds.
// Writes snc.state=no_controls and exits if none of them authenticate.
func runSnc(dataDir string, kd *core.KeyData) {
	type authResult struct {
		url  string
		auth *core.Authenticator
	}
	nodes := kd.Nodes()
	ch := make(chan authResult, len(nodes))
	for _, node := range nodes {
		go func(node string) {
			url := ensureHTTPS(node)
			a := core.NewAuthenticator(url, kd.APIKey, kd.Username, kd.Password)
			a.SetKeyAuth(kd)
			if err := a.Login(); err != nil {
				logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy,
					logevent.Str(logevent.AttrStage, "auth_failed"),
					logevent.Str(logevent.AttrAddr, url),
					logevent.Str(logevent.AttrErr, err.Error()))
				ch <- authResult{}
				return
			}
			ch <- authResult{url, a}
		}(node)
	}
	var bootstrapAuth *core.Authenticator
	var bootstrapURL string
	for range nodes {
		r := <-ch
		if r.auth != nil && bootstrapAuth == nil {
			bootstrapAuth = r.auth
			bootstrapURL = r.url
		}
	}
	if bootstrapAuth == nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy, logevent.Str(logevent.AttrStage, "all_controls_unreachable"))
		writeState(dataDir, "no_controls")
		os.Exit(0)
	}
	logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy,
		logevent.Str(logevent.AttrStage, "auth_ok"),
		logevent.Str(logevent.AttrAddr, bootstrapURL))

	dialer := core.NewTunnelDialer(bootstrapAuth)
	pool := core.NewDialerPool([]*core.TunnelDialer{dialer})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy,
			logevent.Str(logevent.AttrStage, "socks5_listen_failed"),
			logevent.Str(logevent.AttrErr, err.Error()))
		os.Exit(1)
	}
	socks5 := core.NewSOCKS5ServerWithPool("", pool, nil)
	go socks5.Serve(ln) //nolint:errcheck

	socksAddr := ln.Addr().String()
	_ = os.WriteFile(filepath.Join(dataDir, "snc.socks"), []byte(socksAddr), 0o600)
	writeState(dataDir, "ok")
	logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy,
		logevent.Str(logevent.AttrStage, "socks5_ready"),
		logevent.Str(logevent.AttrAddr, socksAddr))

	waitForShutdown()
	logevent.Emit(binlog.TagSystem, logevent.EventWinCoreProxy, logevent.Str(logevent.AttrStage, "shutting_down"))
	ln.Close()
}
