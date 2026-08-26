// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

// snc-probe â€” command-line tunnel tester for ShortNerdCat.
//
// Usage:
//
//	snc-probe -key <key-string> [flags]
//
// Flags:
//
//	-key        <string>  Activation key string (or set env SNC_KEY).
//	-server     <url>     Override server URL (default: first node from key).
//	-fetch      <url>     URL to fetch via the tunnel (default: http://example.com).
//	-socks5     <addr>    Start a SOCKS5 proxy at addr and block (e.g. 127.0.0.1:1080).
//	                      When set, auto-probe is skipped.
//	-router               Run data-plane probe on all controls, print qualifying pool, exit.
//	-quic-tunnel          Force QUIC transport to the control (replaces the old
//	                      SNCU UDP fallback -- see snc/core/quic_relay.go).
//	-quic-probe <addr>    Perform a real QUIC handshake to host:port and print
//	                      the RTT. No key required.
//	-udp-probe  <addr>    Send a UDP NAT-reflection probe to host:port and print the
//	                      observed external address. No key required.
//	-proxy      <url>     Test an external SOCKS5 proxy (e.g. BlackBadger):
//	                      socks5://user:pass@host:port. Uses -fetch as the target URL.
//	                      No SNC key required.
//	-download             Stream the full response body of -fetch URL and print
//	                      live throughput (bytes/s). Ctrl-C to stop.
//	-timeout    <dur>     Dial + request timeout (default: 15s).
//	-v                    Verbose tunnel logging.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/proxy"

	"shortnerdcat/snc/shared/keymigrate"
	"tunnel_cat/snc/core"
)

func main() {
	keyFlag := flag.String("key", "", "Activation key string (or env SNC_KEY)")
	serverFlag := flag.String("server", "", "Override server URL")
	fetchFlag := flag.String("fetch", "http://example.com", "URL to fetch via tunnel")
	socks5Flag := flag.String("socks5", "", "Start SOCKS5 proxy at addr and block (e.g. 127.0.0.1:1080)")
	routerFlag := flag.Bool("router", false, "Data-plane probe all controls, print qualifying pool, exit")
	bypassFlag := flag.Bool("bypass", false, "Full routing check: fetch bypass CIDRs, classify controls by country, apply hard filter, exit.")
	logUploadFlag := flag.Bool("log-upload", false, "Test log upload: write a probe entry to LogRing, POST to /p/v1/log/upload, print result, exit.")
	quicTunnelFlag := flag.Bool("quic-tunnel", false, "Force QUIC transport to the control; combine with -fetch or -socks5")
	quicProbeFlag := flag.String("quic-probe", "", "Perform a real QUIC handshake to host:port, print the RTT, exit. No key needed.")
	udpProbeFlag := flag.String("udp-probe", "", "Send UDP NAT-reflection probe to host:port, print observed external address, exit. No key needed.")
	udpFailoverTestFlag := flag.Bool("udp-failover-test", false, "Live SOCKS5 UDP ASSOCIATE test: pin the realtime UDP dialer to a deliberately dead address, confirm the session still self-heals onto the pool and keeps delivering real UDP responses. Requires -key.")
	udpFailoverBadAddrFlag := flag.String("udp-failover-bad-addr", "198.51.100.1:443", "Address the realtime UDP dialer is pointed at for -udp-failover-test (default: TEST-NET-2, guaranteed unreachable).")
	udpFailoverTargetFlag := flag.String("udp-failover-target", "8.8.8.8:53", "Real UDP target -udp-failover-test relays DNS queries to.")
	proxyFlag := flag.String("proxy", "", "Test external SOCKS5 proxy (socks5://[user:pass@]host:port); fetches -fetch URL through it. No key needed.")
	manifestFlag := flag.Bool("manifest", false, "Fetch /p/v1/manifest via relay API, print node count, exit. Requires -key.")
	tlsProbeFlag := flag.Bool("tls-probe", false, "Test each uTLS browser preset against the control server; reports which presets can authenticate. Requires -key.")
	downloadFlag := flag.Bool("download", false, "Stream full response body of -fetch URL and print live throughput. Ctrl-C to stop.")
	timeoutFlag := flag.Duration("timeout", 15*time.Second, "Dial + request timeout")
	verboseFlag := flag.Bool("v", false, "Verbose tunnel logging")
	flag.Parse()

	if !*verboseFlag {
		core.Log.SetOutput(io.Discard)
	}

	// â”€â”€ 0a. -quic-probe: standalone QUIC handshake test, no key needed â”€â”€â”€â”€â”€â”€
	if *quicProbeFlag != "" {
		addr := *quicProbeFlag
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = addr + ":443"
		}
		fmt.Printf("quic-probe %s â€¦ ", addr)
		rtt, ok := core.ProbeControlQUIC(addr, *timeoutFlag)
		if !ok {
			fmt.Printf("FAIL\n")
			os.Exit(1)
		}
		fmt.Printf("OK  rtt=%s\n", rtt.Round(time.Millisecond))
		return
	}

	// â”€â”€ 0. -udp-probe: standalone UDP reachability test, no key needed â”€â”€â”€â”€â”€â”€â”€â”€
	if *udpProbeFlag != "" {
		addr := *udpProbeFlag
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = addr + ":443"
		}
		fmt.Printf("udp-probe %s â€¦ ", addr)
		ep, err := core.ProbeExternalEndpoint(addr, "")
		if err != nil {
			fmt.Printf("FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("OK  external=%s\n", ep)
		return
	}

	// â”€â”€ -proxy: test external SOCKS5 proxy (e.g. BlackBadger). No key needed. â”€
	if *proxyFlag != "" {
		runProxyTest(*proxyFlag, *fetchFlag, *timeoutFlag)
		return
	}

	// â”€â”€ 1. Resolve key â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	keyStr := *keyFlag
	if keyStr == "" {
		keyStr = os.Getenv("SNC_KEY")
	}
	if keyStr == "" {
		die("no key: use -key <string> or set SNC_KEY")
	}

	kd, err := core.ParseKeyString(keyStr)
	if err != nil {
		die("invalid key: %v", err)
	}

	nodes := kd.Nodes()
	if len(nodes) == 0 {
		die("key contains no server addresses")
	}

	// Legacy (V1, unsigned) key: its ControlNodes/Servers list is not
	// verifiable (see snc/shared/keymigrate's doc comment), so it must not
	// be dialed as-is. Migrate first; on failure, refuse to proceed rather
	// than falling back to its own (unverifiable) node list.
	if kd.IsLegacy() {
		fmt.Printf("legacy V1 key detected for %s, migrating to V2 â€¦ ", kd.Username)
		migCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, newKD, migErr := keymigrate.Migrate(migCtx, kd)
		cancel()
		if migErr != nil {
			fmt.Println("FAIL")
			die("legacy key migration: %v", migErr)
		}
		fmt.Printf("OK (key_id=%.8sâ€¦)\n", newKD.KeyID)
		kd = newKD
		nodes = kd.Nodes()
	}

	serverURL := *serverFlag
	if serverURL == "" {
		serverURL = ensureHTTPS(nodes[0])
	}

	// â”€â”€ 2. Authenticate â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	fmt.Printf("auth  %s @ %s â€¦ ", kd.Username, serverURL)
	auth := core.NewAuthenticator(serverURL, kd.APIKey, kd.Username, kd.Password)
	auth.SetKeyAuth(kd)
	if err := auth.Login(); err != nil {
		fmt.Println("FAIL")
		die("login: %v", err)
	}
	fmt.Printf("OK (token=%.8sâ€¦)\n", auth.Token())

	// FetchMyIP smoke-test â€” verifies the h1 uTLS channel works.
	if ip, err := core.FetchMyIP(serverURL); err != nil {
		fmt.Printf("myip  FAIL: %v\n", err)
	} else {
		fmt.Printf("myip  %s\n", ip)
	}

	// â”€â”€ 3e. -quic-tunnel mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	var dialer *core.TunnelDialer
	if *quicTunnelFlag {
		controlAddr := nodes[0]
		if _, _, err := net.SplitHostPort(controlAddr); err != nil {
			controlAddr = controlAddr + ":443"
		}
		fmt.Printf("quic-tunnel  handshaking to %s â€¦ ", controlAddr)
		rtt, ok := core.ProbeControlQUIC(controlAddr, *timeoutFlag)
		if !ok {
			fmt.Printf("FAIL\n")
			os.Exit(1)
		}
		fmt.Printf("OK  rtt=%s\n", rtt.Round(time.Millisecond))
		dialer = core.NewQUICRelayDialer(controlAddr, auth)
	} else {
		dialer = core.NewTunnelDialer(auth)
	}

	// â”€â”€ 3f. -udp-failover-test mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if *udpFailoverTestFlag {
		runUDPFailoverTest(kd, nodes, *udpFailoverBadAddrFlag, *udpFailoverTargetFlag, *timeoutFlag)
		return
	}

	// â”€â”€ 3b. -manifest mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if *manifestFlag {
		runManifestProbe(serverURL, kd.ArbiterPubkey, *timeoutFlag)
		return
	}

	// â”€â”€ 3b2. -tls-probe mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if *tlsProbeFlag {
		runTLSProbe(serverURL, kd)
		return
	}

	// â”€â”€ 3c. -log-upload mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if *logUploadFlag {
		nodeID := "snc-probe"
		lu := core.NewLogUploader(nodeID, "windows")
		dialer := core.NewTunnelDialer(auth)
		fmt.Printf("log-upload  POST https://navlink.net/api/log/client-upload (via tunnel) â€¦ ")
		lu.UploadOnce(dialer)
		fmt.Println("done (see log above for status)")

		fmt.Printf("log-upload  GET  %s/p/v1/log/get?node=%s â€¦ ", serverURL, nodeID)
		client := core.NewRelayAPIH1Client()
		req, _ := http.NewRequest(http.MethodGet,
			serverURL+"/p/v1/log/get?node="+nodeID, nil)
		req.Header.Set("X-Session", auth.Token())
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("FAIL: %v\n", err)
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			fmt.Printf("status=%d  bytes=%d\n", resp.StatusCode, len(body))
		}
		return
	}

	// â”€â”€ 3a. -router / -bypass mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if *routerFlag || *bypassFlag {
		fmt.Printf("\nâ”€â”€ router probe (timeout=%s) â”€â”€\n", *timeoutFlag)
		router := core.NewRouter()

		ctrlAddrs := make([]string, len(nodes))
		for i, n := range nodes {
			if _, _, err := net.SplitHostPort(n); err != nil {
				ctrlAddrs[i] = n + ":443"
			} else {
				ctrlAddrs[i] = n
			}
		}

		if *bypassFlag {
			fmt.Println("bypass: detecting public IPâ€¦")
			myIP, ipErr := core.FetchMyIP(serverURL)
			if ipErr != nil {
				fmt.Printf("bypass: public IP detect failed: %v\n", ipErr)
			} else {
				fmt.Printf("bypass: public IP = %s\n", myIP)
			}

			bm, berr := core.NewBypassManager([]string{serverURL}, kd.ArbiterPubkey, "")
			if berr != nil {
				fmt.Printf("bypass: init failed: %v\n", berr)
			} else {
				bm.SetToken(auth.Token())
				if myIP != "" {
					bm.SetMyIP(myIP)
				}
				bm.Start()
				fmt.Print("bypass: waiting for CIDRs (up to 8s)â€¦")
				deadline := time.Now().Add(8 * time.Second)
				for time.Now().Before(deadline) {
					if bm.Country() != "" {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				clientCC := bm.Country()
				fmt.Printf("  country=%q\n", clientCC)

				if clientCC != "" {
					router.SetMyCountry(clientCC)
					regions := make(map[string]string)
					for _, addr := range ctrlAddrs {
						host := addr
						if h, _, err := net.SplitHostPort(addr); err == nil {
							host = h
						}
						ips, err := net.LookupHost(host)
						if err != nil || len(ips) == 0 {
							fmt.Printf("bypass: resolve %s failed: %v\n", host, err)
							continue
						}
						ip := net.ParseIP(ips[0])
						if ip != nil && bm.ContainsIP(ip) {
							regions[addr] = clientCC
							fmt.Printf("bypass: %s â†’ in-country (%s)\n", addr, clientCC)
						} else {
							fmt.Printf("bypass: %s â†’ out-of-country\n", addr)
						}
					}
					router.SetControlsWithRegions(ctrlAddrs, regions)
				} else {
					fmt.Println("bypass: country unknown â€” no filter applied")
					router.SetControls(ctrlAddrs)
				}
			}
		} else {
			router.SetControls(ctrlAddrs)
		}
		if relays, err := core.FetchRelayList(serverURL); err == nil {
			router.UpdateRelays(relays)
			fmt.Printf("relays  %d fetched\n", len(relays))
		} else {
			fmt.Printf("relays  FAIL: %v\n", err)
		}
		fmt.Println("probing controls (TCP vs QUIC, racing on RTT â€” whichever is faster wins, not a TCP-first fallback)â€¦")
		router.ProbeDataPlane(*timeoutFlag)
		router.BuildPaths()
		qualifying := router.QualifyingControlAddrs()
		fmt.Printf("\nqualifying controls (%d):\n", len(qualifying))
		for _, addr := range qualifying {
			fmt.Printf("  %-40s  transport=%-4s  rtt=%s\n",
				addr, router.ControlTransportName(addr), router.ControlRTT(addr).Round(time.Millisecond))
		}
		paths := router.Paths()
		fmt.Printf("\ntop paths (%d):\n", len(paths))
		for i, p := range paths {
			relay := "direct"
			if len(p.Relays) > 0 {
				relay = p.Relays[0].Addr
			}
			fmt.Printf("  [%d] score=%.0f  control=%s  via=%s\n", i, p.Score, p.ControlAddr, relay)
		}
		return
	}

	// â”€â”€ 3c. -socks5 mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if *socks5Flag != "" {
		ln, err := net.Listen("tcp", *socks5Flag)
		if err != nil {
			die("listen %s: %v", *socks5Flag, err)
		}
		socks := core.NewSOCKS5Server(*socks5Flag, dialer)
		fmt.Printf("socks5 listening on %s  (Ctrl-C to stop)\n", ln.Addr())
		fmt.Printf("test:  curl --proxy socks5h://%s http://example.com\n", ln.Addr())
		go socks.Serve(ln) //nolint:errcheck

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Println("\nstopping")
		ln.Close()
		return
	}

	// â”€â”€ 3d. Auto-probe / download mode â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	if *downloadFlag {
		runDownloadProbe(dialer, *fetchFlag)
	} else {
		runAutoProbe(dialer, *fetchFlag, *timeoutFlag)
	}
}

// runDownloadProbe streams the full response body of fetchURL through the tunnel
// and prints live throughput once per second. Stops on EOF, error, or Ctrl-C.
func runDownloadProbe(dialer *core.TunnelDialer, fetchURL string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	socksAddr := ln.Addr().String()
	socks := core.NewSOCKS5Server("", dialer)
	go socks.Serve(ln) //nolint:errcheck

	proxyURL, _ := url.Parse("socks5h://" + socksAddr)
	// No timeout on the client: body may stream for a long time.
	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	fmt.Printf("download  GET %s via socks5://%s\n", fetchURL, socksAddr)
	t0 := time.Now()
	req, _ := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("download  FAIL: %v\n", err)
		ln.Close()
		return
	}
	fmt.Printf("download  status=%d  streamingâ€¦ (Ctrl-C to stop)\n", resp.StatusCode)

	var totalBytes int64
	buf := make([]byte, 32*1024)
	done := make(chan error, 1)
	go func() {
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				atomic.AddInt64(&totalBytes, int64(n))
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastBytes int64
	for {
		select {
		case <-ticker.C:
			now := atomic.LoadInt64(&totalBytes)
			speed := now - lastBytes
			lastBytes = now
			elapsed := time.Since(t0).Round(time.Second)
			fmt.Printf("\r  %6s  total=%-12s  speed=%s/s   ",
				elapsed, fmtBytes(now), fmtBytes(speed))
		case err := <-done:
			fmt.Println()
			now := atomic.LoadInt64(&totalBytes)
			elapsed := time.Since(t0)
			avg := int64(0)
			if s := elapsed.Seconds(); s > 0 {
				avg = int64(float64(now) / s)
			}
			if err == io.EOF {
				fmt.Printf("download  OK  total=%s  time=%s  avg=%s/s\n",
					fmtBytes(now), elapsed.Round(time.Millisecond), fmtBytes(avg))
			} else {
				fmt.Printf("download  stopped (%v)  total=%s  time=%s  avg=%s/s\n",
					err, fmtBytes(now), elapsed.Round(time.Millisecond), fmtBytes(avg))
			}
			resp.Body.Close()
			ln.Close()
			return
		}
	}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func runAutoProbe(dialer *core.TunnelDialer, fetchURL string, timeout time.Duration) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	socksAddr := ln.Addr().String()
	socks := core.NewSOCKS5Server("", dialer)
	go socks.Serve(ln) //nolint:errcheck

	proxyURL, _ := url.Parse("socks5h://" + socksAddr)
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	fmt.Printf("probe %s via socks5://%s â€¦ ", fetchURL, socksAddr)
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
	resp, err := httpClient.Do(req)
	elapsed := time.Since(start).Round(time.Millisecond)

	if err != nil {
		fmt.Printf("FAIL (%s)\n", elapsed)
		die("fetch: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	ln.Close()

	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 80 {
		snippet = snippet[:80] + "â€¦"
	}
	fmt.Printf("OK  status=%d  dur=%s\n", resp.StatusCode, elapsed)
	if len(snippet) > 0 {
		fmt.Printf("body  %s\n", snippet)
	}
}

// runProxyTest connects to an external SOCKS5 proxy and fetches fetchURL through
// it, printing timing and status. Intended for testing BlackBadger endpoints.
func runProxyTest(proxyAddr, fetchURL string, timeout time.Duration) {
	u, err := url.Parse(proxyAddr)
	if err != nil {
		die("proxy: invalid URL: %v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "socks5" && scheme != "socks5h" {
		die("proxy: scheme must be socks5 or socks5h, got %q", u.Scheme)
	}
	var pauth *proxy.Auth
	if u.User != nil {
		pw, _ := u.User.Password()
		pauth = &proxy.Auth{User: u.User.Username(), Password: pw}
	}
	socksDialer, err := proxy.SOCKS5("tcp", u.Host, pauth, proxy.Direct)
	if err != nil {
		die("proxy: %v", err)
	}
	transport := &http.Transport{
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		},
	}
	client := &http.Client{Timeout: timeout, Transport: transport}

	fmt.Printf("proxy  %s  target %s â€¦ ", proxyAddr, fetchURL)
	start := time.Now()
	resp, err := client.Get(fetchURL)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("FAIL (%s): %v\n", elapsed, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 80 {
		snippet = snippet[:80] + "â€¦"
	}
	fmt.Printf("OK  status=%d  dur=%s\n", resp.StatusCode, elapsed)
	if len(snippet) > 0 {
		fmt.Printf("body  %s\n", snippet)
	}
}

func runManifestProbe(serverURL, pubkeyHex string, timeout time.Duration) {
	d, err := core.NewDiscoverer(serverURL, pubkeyHex, core.NewDiscoveryClient(), "", nil)
	if err != nil {
		fmt.Printf("manifest  FAIL (init): %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("manifest  GET %s/p/v1/manifest â€¦ ", serverURL)
	start := time.Now()
	nodes, err := d.ProbeManifest(timeout)
	dur := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("FAIL (%s): %v\n", dur, err)
		os.Exit(1)
	}
	fmt.Printf("OK  dur=%s  nodes=%d\n", dur, nodes)
}

// runUDPFailoverTest is a live regression check for the 2026-08-19/20
// incident: a call's realtime UDP dialer dying mid-session used to leave
// every destination already pinned to it stuck retrying a dead transport
// until each one independently burned through its own circuit-breaker
// threshold (snc/core/udp_assoc.go's dialerFor). It builds a real SOCKS5
// UDP ASSOCIATE session backed by a genuine pool of authenticated controls,
// but deliberately points RealtimeUDPDialer at badAddr (default: an
// unroutable TEST-NET-2 address) so the realtime path can never work --
// then confirms the session still delivers real UDP responses (via the
// pool fallback) within a bounded time instead of hanging or requiring a
// fresh SOCKS5 session.
func runUDPFailoverTest(kd *core.KeyData, nodes []string, badAddr, target string, timeout time.Duration) {
	fmt.Printf("\nâ”€â”€ udp-failover-test (bad-addr=%s target=%s) â”€â”€\n", badAddr, target)

	// â”€â”€ 1. Build a real pool: authenticate against every control in the key. â”€â”€
	var dialers []*core.TunnelDialer
	for _, n := range nodes {
		serverURL := ensureHTTPS(n)
		a := core.NewAuthenticator(serverURL, kd.APIKey, kd.Username, kd.Password)
		a.SetKeyAuth(kd)
		if err := a.Login(); err != nil {
			fmt.Printf("pool  %-40s  FAIL: %v\n", serverURL, err)
			continue
		}
		fmt.Printf("pool  %-40s  OK\n", serverURL)
		dialers = append(dialers, core.NewTunnelDialer(a))
	}
	if len(dialers) == 0 {
		die("udp-failover-test: could not authenticate against any control -- no pool to fall back to")
	}
	pool := core.NewDialerPool(dialers)

	// â”€â”€ 2. Realtime dialer pinned to a deliberately dead address. â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	rtAuth := core.NewAuthenticator(ensureHTTPS(nodes[0]), kd.APIKey, kd.Username, kd.Password)
	rtAuth.SetKeyAuth(kd)
	if err := rtAuth.Login(); err != nil {
		die("udp-failover-test: realtime dialer auth: %v", err)
	}
	rtDialer := core.NewQUICRelayDialer(badAddr, rtAuth)
	fmt.Printf("realtime  pinned to %s via QUIC (deliberately unreachable)\n", badAddr)

	// â”€â”€ 3. Real SOCKS5 server: normal pool + the broken realtime dialer. â”€â”€â”€â”€â”€
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	defer ln.Close()
	socks := core.NewSOCKS5ServerWithPool("", pool, nil)
	socks.RealtimeUDPDialer = rtDialer
	go socks.Serve(ln) //nolint:errcheck
	fmt.Printf("socks5  listening on %s\n", ln.Addr())

	// â”€â”€ 4. Real SOCKS5 UDP ASSOCIATE handshake against our own server. â”€â”€â”€â”€â”€â”€
	ctrl, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		die("udp-failover-test: dial socks5: %v", err)
	}
	defer ctrl.Close()
	relayAddr, err := socks5UDPAssociate(ctrl)
	if err != nil {
		die("udp-failover-test: UDP ASSOCIATE: %v", err)
	}
	fmt.Printf("udp-assoc  relay=%s (control conn open, keeps the session alive)\n", relayAddr)

	targetAddr, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		die("udp-failover-test: resolve target %s: %v", target, err)
	}
	relay, err := net.ResolveUDPAddr("udp4", relayAddr)
	if err != nil {
		die("udp-failover-test: resolve relay %s: %v", relayAddr, err)
	}
	uconn, err := net.DialUDP("udp4", nil, relay)
	if err != nil {
		die("udp-failover-test: dial relay: %v", err)
	}
	defer uconn.Close()

	query := dnsQuery("example.com.")
	deadline := time.Now().Add(timeout * 6) // real recovery involves retries + a circuit-breaker cooldown -- give it real room
	attempt := 0
	t0 := time.Now()
	for time.Now().Before(deadline) {
		attempt++
		frame := buildSocks5UDPFrame(targetAddr, query)
		if _, err := uconn.Write(frame); err != nil {
			fmt.Printf("attempt %-3d  write FAIL: %v\n", attempt, err)
			time.Sleep(time.Second)
			continue
		}
		uconn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
		buf := make([]byte, 65536)
		n, err := uconn.Read(buf)
		if err != nil {
			fmt.Printf("attempt %-3d  %-8s  no response yet\n", attempt, time.Since(t0).Round(time.Millisecond))
			continue
		}
		payload, perr := parseSocks5UDPFrame(buf[:n])
		if perr != nil || len(payload) < 12 {
			fmt.Printf("attempt %-3d  %-8s  malformed reply: %v\n", attempt, time.Since(t0).Round(time.Millisecond), perr)
			continue
		}
		fmt.Printf("attempt %-3d  %-8s  OK -- %dB real DNS reply received through the pool fallback\n",
			attempt, time.Since(t0).Round(time.Millisecond), len(payload))
		fmt.Printf("\nudp-failover-test  PASS  recovered in %s (%d attempt(s))\n", time.Since(t0).Round(time.Millisecond), attempt)
		return
	}
	fmt.Printf("\nudp-failover-test  FAIL  no real response within %s (%d attempts) -- session never recovered onto the pool\n",
		timeout*6, attempt)
	os.Exit(1)
}

// socks5UDPAssociate performs a minimal RFC 1928 handshake (no-auth) and
// issues a UDP ASSOCIATE (CMD=0x03) request. Returns the server's BND
// address -- where the caller must send UDP datagrams for this session.
// The TCP conn must be kept open for the UDP association's lifetime.
func socks5UDPAssociate(ctrl net.Conn) (string, error) {
	if _, err := ctrl.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return "", fmt.Errorf("greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(ctrl, reply); err != nil {
		return "", fmt.Errorf("greeting reply: %w", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return "", fmt.Errorf("unexpected greeting reply % x", reply)
	}

	req := []byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0} // CMD=UDP ASSOCIATE, ATYP=IPv4, 0.0.0.0:0
	if _, err := ctrl.Write(req); err != nil {
		return "", fmt.Errorf("associate req: %w", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(ctrl, head); err != nil {
		return "", fmt.Errorf("associate reply header: %w", err)
	}
	if head[1] != 0x00 {
		return "", fmt.Errorf("associate denied, REP=0x%02x", head[1])
	}
	var addr string
	switch head[3] {
	case 0x01: // IPv4
		b := make([]byte, 4+2)
		if _, err := io.ReadFull(ctrl, b); err != nil {
			return "", fmt.Errorf("associate reply addr: %w", err)
		}
		addr = fmt.Sprintf("%d.%d.%d.%d:%d", b[0], b[1], b[2], b[3], int(b[4])<<8|int(b[5]))
	default:
		return "", fmt.Errorf("unsupported BND.ATYP 0x%02x", head[3])
	}
	return addr, nil
}

// buildSocks5UDPFrame wraps payload in the RFC 1928 Â§7 UDP request header.
func buildSocks5UDPFrame(dst *net.UDPAddr, payload []byte) []byte {
	ip4 := dst.IP.To4()
	buf := make([]byte, 0, 10+len(payload))
	buf = append(buf, 0x00, 0x00, 0x00, 0x01) // RSV, RSV, FRAG=0, ATYP=IPv4
	buf = append(buf, ip4...)
	buf = append(buf, byte(dst.Port>>8), byte(dst.Port))
	buf = append(buf, payload...)
	return buf
}

// parseSocks5UDPFrame strips the RFC 1928 Â§7 UDP header, returning the payload.
func parseSocks5UDPFrame(data []byte) ([]byte, error) {
	if len(data) < 10 {
		return nil, fmt.Errorf("short frame (%dB)", len(data))
	}
	if data[2] != 0x00 {
		return nil, fmt.Errorf("fragmented reply not supported (FRAG=0x%02x)", data[2])
	}
	switch data[3] {
	case 0x01: // IPv4
		return data[10:], nil
	case 0x04: // IPv6
		if len(data) < 22 {
			return nil, fmt.Errorf("short IPv6 frame (%dB)", len(data))
		}
		return data[22:], nil
	default:
		return nil, fmt.Errorf("unsupported ATYP 0x%02x", data[3])
	}
}

// dnsQuery builds a minimal, valid DNS query for name's A record.
func dnsQuery(name string) []byte {
	buf := []byte{
		0x12, 0x34, // ID
		0x01, 0x00, // flags: standard query, recursion desired
		0x00, 0x01, // QDCOUNT=1
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // ANCOUNT/NSCOUNT/ARCOUNT=0
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0x00)       // root label
	buf = append(buf, 0x00, 0x01) // QTYPE=A
	buf = append(buf, 0x00, 0x01) // QCLASS=IN
	return buf
}

func die(format string, args ...interface{}) {
	log.Fatalf("error: "+format, args...)
}

func ensureHTTPS(u string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "https://" + u
	}
	return u
}

func runTLSProbe(serverURL string, kd *core.KeyData) {
	fmt.Printf("tls-probe  target=%s\n", serverURL)
	fmt.Printf("%-10s  %-8s  %s\n", "PRESET", "RESULT", "DETAIL")
	fmt.Println(strings.Repeat("-", 60))

	for _, preset := range core.AllUTLSPresets() {
		name := core.PresetName(preset)
		auth := core.NewAuthenticator(serverURL, kd.APIKey, kd.Username, kd.Password)
		auth.SetKeyAuth(kd)
		auth.SetHTTPClientForPreset(preset)

		t0 := time.Now()
		err := auth.Login()
		dur := time.Since(t0).Round(time.Millisecond)

		if err != nil {
			fmt.Printf("%-10s  %-8s  %v  (%s)\n", name, "FAIL", err, dur)
		} else {
			fmt.Printf("%-10s  %-8s  token=%.12sâ€¦  (%s)\n", name, "OK", auth.Token(), dur)
		}
	}
}
