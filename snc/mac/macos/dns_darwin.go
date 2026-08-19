// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin

package macos

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"tunnel_cat/snc/core"
)

const (
	dnsServer    = "1.1.1.1"
	dnsCmdTimeout = 8 * time.Second
)

// DNSManager configures the system DNS to route queries through the tunnel.
//
// Strategy: find the active network service (e.g. "Wi-Fi") for the interface
// carrying the default route, then use networksetup to point its DNS to
// 1.1.1.1. Because the 1.1.1.1 host route is bypassed via the original gateway
// during a normal session, and removed during a tunneled-DNS session, all DNS
// traffic naturally flows through or around the tunnel as intended.
type DNSManager struct {
	service string // active networksetup service name, e.g. "Wi-Fi"
}

// Apply detects the active network service and sets its DNS server to 1.1.1.1.
func (d *DNSManager) Apply() error {
	svc, err := activeNetworkService()
	if err != nil {
		return fmt.Errorf("dns: detect active service: %w", err)
	}
	d.service = svc

	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "networksetup", "-setdnsservers", svc, dnsServer).CombinedOutput()
	if err != nil {
		return fmt.Errorf("dns: setdnsservers %q %s: %w (out: %s)", svc, dnsServer, err, strings.TrimSpace(string(out)))
	}
	core.Log.Printf("dns: set %q DNS â†’ %s", svc, dnsServer)
	return nil
}

// Restore removes the DNS override, reverting to DHCP-provided servers.
func (d *DNSManager) Restore() {
	if d.service == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "networksetup", "-setdnsservers", d.service, "empty").CombinedOutput()
	if err != nil {
		core.Log.Printf("dns: restore %q: %v (out: %s)", d.service, err, strings.TrimSpace(string(out)))
	} else {
		core.Log.Printf("dns: restored %q DNS to DHCP", d.service)
	}
}

// CleanupDNS resets the DNS for all non-disabled network services.
// Called by the watchdog on unclean exit when the service name is unknown.
func CleanupDNS() {
	services, err := listNetworkServices()
	if err != nil {
		core.Log.Printf("dns: cleanup: list services: %v", err)
		return
	}
	for _, svc := range services {
		ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
		out, err := exec.CommandContext(ctx, "networksetup", "-setdnsservers", svc, "empty").CombinedOutput()
		cancel()
		if err != nil {
			core.Log.Printf("dns: cleanup %q: %v (%s)", svc, err, strings.TrimSpace(string(out)))
		}
	}
	core.Log.Printf("dns: cleanup: reset all services")
}

// activeNetworkService returns the networksetup service name (e.g. "Wi-Fi")
// for the interface that carries the current default route.
func activeNetworkService() (string, error) {
	// Find the interface name from the default route.
	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "route", "get", "8.8.8.8").Output()
	if err != nil {
		return "", err
	}
	iface := ""
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "interface:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				iface = parts[1]
			}
		}
	}
	if iface == "" {
		return "", fmt.Errorf("could not parse interface from route get output")
	}

	// Map interface name â†’ networksetup service name.
	services, err := listNetworkServicesWithDevice()
	if err != nil {
		return "", err
	}
	if svc, ok := services[iface]; ok {
		return svc, nil
	}
	return "", fmt.Errorf("no networksetup service found for interface %q", iface)
}

// listNetworkServices returns all enabled service names from networksetup.
func listNetworkServices() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, err
	}
	var services []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "An asterisk") || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	return services, nil
}

// listNetworkServicesWithDevice returns a map of {device â†’ service name}
// by parsing "networksetup -listnetworkserviceorder" output.
func listNetworkServicesWithDevice() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dnsCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return nil, err
	}

	// Output looks like:
	//   (1) Wi-Fi
	//   (Hardware Port: Wi-Fi, Device: en0)
	//   (2) USB 10/100/1000 LAN
	//   (Hardware Port: USB 10/100/1000 LAN, Device: en5)
	result := make(map[string]string)
	var lastName string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "(") && strings.Contains(line, "Hardware Port:") {
			// parse Device: enX
			if idx := strings.Index(line, "Device:"); idx >= 0 {
				dev := strings.TrimRight(strings.TrimSpace(line[idx+7:]), ")")
				if lastName != "" && dev != "" {
					result[dev] = lastName
				}
			}
			lastName = ""
		} else if strings.HasPrefix(line, "(") {
			// Service name line like "(1) Wi-Fi"
			if idx := strings.Index(line, ") "); idx >= 0 {
				lastName = line[idx+2:]
			}
		}
	}
	return result, nil
}
