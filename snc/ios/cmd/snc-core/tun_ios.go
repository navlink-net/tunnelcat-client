// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build ios

package main

import (
	"fmt"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/engine"
	snc "tunnel_cat/snc/core"
)

// startTUN wires the utun fd provided by NEPacketTunnelProvider into the
// tun2socks engine (gVisor netstack) and routes all traffic to the SOCKS5
// server at socksAddr.
//
// The Network Extension process owns the utun interface; all IP/routing
// configuration is handled by setTunnelNetworkSettings on the Swift side.
// Go receives the already-configured fd and only needs to attach to it.
func startTUN(tunFD int, socksAddr string) error {
	snc.Log.Printf("TUN: starting fd=%d â†’ SOCKS5 %s", tunFD, socksAddr)
	engine.Insert(&engine.Key{
		Device:   fmt.Sprintf("fd://%d", tunFD),
		Proxy:    "socks5://" + socksAddr,
		MTU:      1280,
		LogLevel: "error",
	})
	if err := safeEngineStart(); err != nil {
		return fmt.Errorf("engine start: %w", err)
	}
	snc.Log.Printf("TUN: started fd=%d", tunFD)
	return nil
}

// stopTUN shuts down the gVisor netstack. The utun fd is owned by the
// Network Extension; we do not close it.
func stopTUN() {
	snc.Log.Printf("TUN: stopping")
	done := make(chan struct{})
	go func() { engine.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		snc.Log.Printf("TUN: engine.Stop() timeout")
	}
	snc.Log.Printf("TUN: stopped")
}

// safeEngineStart wraps engine.Start() with panic recovery.
// tun2socks uses WriteThenPanic in debug mode; we catch it here and
// return an error so the caller can handle it gracefully.
func safeEngineStart() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	engine.Start()
	return nil
}
