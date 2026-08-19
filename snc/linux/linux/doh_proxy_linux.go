// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"tunnel_cat/snc/core"
)

const (
	dohProxyListenAddr = "127.0.0.1:53"
	// Use the hostname URL so TLS is verified against a DNS name SAN, not an IP SAN.
	// Cloudflare's cert includes "1.1.1.1" as a DNS SAN only (not an IP SAN), so Go's
	// x509 IP-address verification fails when the URL contains a bare IP.  Using the
	// hostname lets Go verify the DNS SAN "cloudflare-dns.com" instead, which passes.
	// The DialContext hardcodes 1.1.1.1:443 so no DNS lookup is needed.
	dohProxyUpstream = "https://cloudflare-dns.com/dns-query"
	dohProxyDialAddr = "1.1.1.1:443"
	dohProxyTimeout  = 5 * time.Second
	dohProxyMaxMsg   = 4096
)

// DoHProxy forwards local UDP DNS queries to 1.1.1.1 via HTTPS (RFC 8484).
//
// The HTTPS request goes out through the TUN adapter (the 1.1.1.1 bypass route
// must NOT be present), so the query travels encrypted through the SOCKS5/tunnel
// stack to the exit node and on to Cloudflare.
//
// Lifecycle:
//
//	proxy, err := NewDoHProxy()
//	proxy.Start()
//	... (tunnel is up, system DNS set to 127.0.0.1 via resolvectl)
//	proxy.Stop()
type DoHProxy struct {
	conn   *net.UDPConn
	client *http.Client
	wg     sync.WaitGroup
	stopCh chan struct{}
}

func NewDoHProxy() *DoHProxy {
	// Connect to 1.1.1.1:443 via TUN (bypass route is absent) but verify TLS
	// against "cloudflare-dns.com" (DNS name) rather than the raw IP address.
	// Cloudflare's cert only has "1.1.1.1" as a DNS SAN, so connecting via IP
	// triggers Go's IP-SAN check which fails (no matching IP SAN in the cert).
	// Pinning the dial address and verifying the hostname avoids this entirely.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, dohProxyDialAddr)
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: dohProxyTimeout,
	}
	return &DoHProxy{
		client: &http.Client{Timeout: dohProxyTimeout, Transport: transport},
		stopCh: make(chan struct{}),
	}
}

func (p *DoHProxy) Start() error {
	addr, err := net.ResolveUDPAddr("udp4", dohProxyListenAddr)
	if err != nil {
		return fmt.Errorf("doh-proxy: resolve listen addr: %w", err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("doh-proxy: bind %s: %w (need root or port not in use)", dohProxyListenAddr, err)
	}
	p.conn = conn
	p.wg.Add(1)
	go p.serve()
	core.Log.Printf("doh-proxy: listening on %s â€” forwarding to %s via tunnel", dohProxyListenAddr, dohProxyUpstream)
	return nil
}

func (p *DoHProxy) Stop() {
	if p.conn == nil {
		return
	}
	close(p.stopCh)
	p.conn.Close()
	p.wg.Wait()
	p.conn = nil
	core.Log.Printf("doh-proxy: stopped")
}

func (p *DoHProxy) serve() {
	defer p.wg.Done()
	buf := make([]byte, dohProxyMaxMsg)
	for {
		n, src, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-p.stopCh:
				return
			default:
				core.Log.Printf("doh-proxy: recv error: %v â€” continuing", err)
				continue
			}
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		go p.forward(query, src)
	}
}

func (p *DoHProxy) forward(query []byte, src *net.UDPAddr) {
	resp, err := p.query(query)
	if err != nil {
		core.Log.Printf("doh-proxy: forward to %s: %v", src, err)
		return
	}
	core.GlobalDNSCache.ParseAndLearn(resp)
	if _, err := p.conn.WriteToUDP(resp, src); err != nil {
		core.Log.Printf("doh-proxy: write response to %s: %v", src, err)
	}
	core.Log.Printf("doh-proxy: resolved %dB query â†’ %dB response for %s", len(query), len(resp), src)
}

func (p *DoHProxy) query(msg []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dohProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dohProxyUpstream, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", dohProxyUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, dohProxyMaxMsg))
}
