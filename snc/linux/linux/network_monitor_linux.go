// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"time"

	"tunnel_cat/snc/core"
)

// NetworkMonitor polls for default gateway changes and fires OnChange when
// the gateway or interface changes.  This covers WiFi handoff, cable
// plug/unplug, and DHCP lease renewal â€” cases where split-tunnel routes
// silently break because the original gateway IP is stale.
//
// Usage:
//
//	m := NewNetworkMonitor(func() { tray.TriggerReconnect() })
//	stopFn := m.Start()
//	// later:
//	stopFn()
type NetworkMonitor struct {
	OnChange func()
}

// NewNetworkMonitor creates a monitor that calls onChange on every gateway change.
func NewNetworkMonitor(onChange func()) *NetworkMonitor {
	return &NetworkMonitor{OnChange: onChange}
}

// Start launches the polling goroutine and returns a stop function.
func (m *NetworkMonitor) Start() (stop func()) {
	stopCh := make(chan struct{})
	go m.run(stopCh)
	return func() { close(stopCh) }
}

func (m *NetworkMonitor) run(stopCh <-chan struct{}) {
	gw, iface, err := defaultGatewayLinux()
	if err != nil {
		core.Log.Printf("netmon: initial gateway read failed: %v", err)
	}
	prev := gw + ";" + iface

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}
		gw2, iface2, err := defaultGatewayLinux()
		if err != nil {
			continue
		}
		cur := gw2 + ";" + iface2
		if cur != prev {
			core.Log.Printf("netmon: gateway changed %q â†’ %q â€” triggering reconnect", prev, cur)
			prev = cur
			if m.OnChange != nil {
				go m.OnChange()
			}
		}
	}
}
