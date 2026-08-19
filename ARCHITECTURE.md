# ShortNerdCat — Architecture

**Current as of:** 2026-08-14

This repo (`github.com/navlink-net/tunnelcat-client`) contains the client
apps only: `snc/{win,mac,linux,android,ios}/`. The client talks to a
control/exit/arbiter server-side stack that lives in the sibling
**tunnelcat-server** repo; server-side internals are documented there, not
here. This doc covers the client architecture: transport, DPI resistance,
auth, key-string format, and per-platform implementation.

---

## 1. Concept

ShortNerdCat is an obfuscated proxy tunnel with DPI bypass. Traffic is
indistinguishable from HTTPS at the DPI level. On the wire, a client dials a
**control** node over TLS; the control node passes traffic through to an
**exit** node, which makes the actual outbound connection. A separate
**arbiter** service issues session tokens, signs the node/key manifests the
client verifies, and pushes OTA updates. See the tunnelcat-server repo for
how those three roles are implemented; from the client's point of view they
are just addresses and public keys carried in the key-string (§9).

---

## 2. Components

### 2.1. win-client

Location: `snc/win/` — shared tunnel logic is in `snc/core/`.

| Module | File | Purpose |
|---|---|---|
| Entry point | `cmd/shortnerdcat/main_windows.go` | UAC elevation, logging, watchdog launch, auto-connect |
| TUN | `core/tun.go` | WinTun adapter `ShortNerdCat`, 198.18.0.1/16, MTU 1500; tun2socks/gVisor stack |
| Tunnel | `core/tunnel.go` | HTTP obfuscator: chunked body 4–64 KB, padding ≥ 512 B, jitter 0–50 ms; `SetDataFailHook(threshold, fn)` — 3 consecutive dial errors → control marked dead |
| Session | `core/session.go` | TunnelConn (net.Conn), sequential X-Seq ordering |
| SOCKS5 | `core/socks5.go` | SOCKS5 CONNECT (TCP) + UDP ASSOCIATE; bypass for local CIDRs |
| UDP ASSOCIATE | `core/udp_assoc.go` | Variant C (bypass socket, physical NIC) with tunnel fallback; drain loop via HTTP tunnel |
| Router | `core/router.go` | RTT-score path selection across all live controls; in-country hard filter; TCP-preference hard filter (UDP-only controls excluded when any TCP control alive); `PrimaryIsBetter(addr, 1.5)` — switch only if ≥50% RTT improvement or current dead; `MarkControlDataDead()` excludes DPI-blocked control; `SetMyCountry()` for regional relay filtering |
| Dialer pool | `core/dialer_pool.go` | Pool of TunnelDialers per control; RTT-weighted `Pick()`; `Get(ctrlURL)` for carry-forward; `Swap()`, `Evict()`, `Add()`, `Has()`; `StartManagement()` background drain loop |
| Control dialer | `core/dial_control.go` | Dials control, wraps with uTLS + channel byte; manages per-control dial state |
| TLS / channel byte | `core/tls.go` | uTLS browser fingerprint rotation; channel byte {0xA7, type} in TLS ClientHello session ID bytes [0:2] |
| Relay | `core/relay.go` | RelayClient: TCP listener + register/heartbeat + bidirectional proxy; country_code in payload |
| UDP relay | `core/udp_relay.go` | UDPRelayConn / UDPRelayDialer: data-path via UDP hole-punch (R5); replaces old TCP proxy relay |
| NAT prober | `core/nat.go` | Encrypted UDP reflection to control → discovers external UDP endpoint (for hole-punch) |
| Hole punch | `core/holepunch.go` | UDP hole-punch coordination; on success registers P2P relay path with −30 ms score bonus |
| DHT | `core/dht.go`, `dht_node.go`, `dht_wire.go` | Kademlia XOR (k=8, 256-bit IDs); Ed25519-signed relay entries; encrypted UDP wire; bootstrap from control list + peers.json |
| Signal | `core/signal.go` | WebSocket to control: receives `relay_request` push events; exponential backoff |
| Discovery | `core/discovery.go` | Ed25519-signed manifest; cache `manifest.json`; refresh every 10 min |
| Bypass | `core/bypass.go` | Split-tunnel: CIDR fetch + `ShouldBypass(addr)` + `Country()` (ISO code from CIDR data) |
| CIDR | `core/cidr.go` | In-memory CIDR set; country lookup for bypass routing decisions |
| DNS cache | `core/dns_cache.go` | In-process DNS response cache; reduces lookup latency under VPN |
| Pacing | `core/pacing.go` | Adaptive write pacing to prevent bursty traffic signatures |
| Decoy | `core/decoy.go` | uTLS requests to CDN endpoints every 20–60 s; suppressed if tunnel active within 500 ms; dormant after 10 min idle |
| NodeID | `core/nodeid.go` | Ed25519 keypair; pubkey = NodeID (64 hex); privkey DPAPI-encrypted (Windows) |
| OTA | `core/updater.go` | Version check every 6 h via control; rename-dance applied at next process start |
| Watchdog state | `core/watchdog_state.go` | State file `APPDATA\ShortNerdCat\watchdog-state.json`: MainPID, Connected, OrigGW, LastAlive |
| Connectivity check | `core/watchdog.go` | `CheckConnectivity()`: TCP dial through TUN to verify end-to-end tunnel health |
| Key | `core/key.go` | Key-string parse: BLAKE2b-256 + XChaCha20-Poly1305; V1/V2 magic; Ed25519 signature verify |
| TZ/country | `core/tz_country.go` | Timezone-to-country fallback for country detection without GPS |
| Headers | `core/headers.go` | Pool of realistic browser User-Agent strings |
| Log upload | `core/log_upload.go` | Periodic log shipping to arbiter via tunnel |
| Tray | `windows/tray.go` | System tray: icons, menu, relay toggle |
| Dialog | `windows/dialog.go` | Key-string entry dialog; DPAPI storage |
| Routes | `windows/routes.go` | Original gateway save, default route via TUN, `Restore()` on disconnect; `getDefaultGateway()` retries 5× (route.exe transiently fails during WinTun adapter creation) |
| Browser window | `windows/browserwindow.go` | In-app browser via WebView2 (Edge runtime) |
| UI window | `windows/uiwindow.go` | Main application window host |
| DoH proxy | `windows/doh_proxy.go` | Local DNS-over-HTTPS proxy; redirects system DNS to Cloudflare/Google |

### 2.2. Server side (control / exit / arbiter)

The client dials a **control** node over TLS (raw TCP, uTLS fingerprint,
channel byte demuxing — see §4). Control passes tunnel traffic through
without terminating TLS to an **exit** node, which makes the outbound
connection and streams data back. A single **arbiter** service issues
session tokens, signs the key-strings, manifests, and CIDR/whitelist data
the client verifies (§8–9), and distributes OTA updates. None of this is
implemented in this repo — see the **tunnelcat-server** repo for the
control/exit/arbiter implementation. From the client's perspective, only
control addresses and the arbiter's public key (both carried in the
key-string) are ever known; exit and arbiter network addresses are never
exposed to the client directly (§10).

### 2.3. macos-client

Location: `snc/mac/`

Shares the `snc/core` package — same tunnel, auth, router, discovery,
bypass, OTA, dialer pool logic. Platform-specific modules in
`snc/mac/macos/` and `snc/mac/cmd/shortnerdcat/`.

**Process model:** two processes per session:
- **Root process** (`cmd/shortnerdcat/main_darwin.go`): runs as root (via `osascript` privilege elevation). Owns TUN (`utun`), routes, pf firewall, SOCKS5, all tunnel logic. Starts the tray process.
- **Tray process** (`--tray <socket>` flag): runs as the logged-in user via `launchctl asuser <uid>`. Owns the menu bar icon, UI dialogs. Communicates with root process via Unix socket IPC.

| Module | File | Purpose |
|---|---|---|
| Entry point | `cmd/shortnerdcat/main_darwin.go` | Privilege check → osascript relaunch as root; root: logging, watchdog, IPC server, tunnel lifecycle (onConnect/onDisconnect) |
| IPC server | `cmd/shortnerdcat/ipc_server_darwin.go` | JSON-framed Unix socket; root receives commands (connect/disconnect/key/settings/quit); pushes status to tray |
| Tray main | `cmd/shortnerdcat/tray_main_darwin.go` | Tray process entry: connects to root IPC socket, calls `snmac.RunTray()` |
| Tray | `macos/tray_darwin.go` | NSStatusBar menu bar icon and menu (Objective-C via CGo); connect/disconnect/settings/quit items |
| Window | `macos/window_darwin.go`, `window_cgo_darwin.go` | Settings/key entry window using NSPanel (CGo bridge) |
| Splash | `macos/splash_cgo_darwin.go` | First-run splash screen shown before key is entered |
| Key storage | `macos/key_darwin.go` | Keychain-based key persistence (`SecItemAdd` / `SecItemCopyMatching`) |
| Routes | `macos/routes_darwin.go` | RouteManager: `route add default`, `route delete`; saves/restores original gateway |
| DNS | `macos/dns_darwin.go` | DNSManager: `networksetup -setdnsservers` → Cloudflare/Google DoH; `Restore()` on disconnect |
| Firewall | `macos/firewall_darwin.go` | pfwMgr: pf anchor rules to block DNS on physical NIC (prevent DNS leaks if routes fail) |
| Power events | `macos/power_darwin.go` | `WatchPowerEvents()`: `IORegisterForSystemPower` → wake callback → trigger reconnect |
| Autostart | `macos/autostart_darwin.go` | `RegisterAutostart()` / `UnregisterAutostart()` via LaunchAgent plist |
| Watchdog | `macos/watchdog_darwin.go`, `cmd/shortnerdcat/watchdog_darwin.go` | PID file; `WatchdogRunning()` check; `StartWatchdog()`; restores routes on crash; `SignalUpdateRestart()` for OTA |
| IPC | `macos/ipc.go` | `IPCSocketPath(uid)`, `IPCListen(path, uid)` — Unix socket helpers |
| Notifications | `macos/notification_darwin.go` | `ShowNotifications(msgs)`: `NSUserNotification` (or `UNUserNotificationCenter` on macOS 10.14+) |
| Geo | `cmd/shortnerdcat/geo_darwin.go` | `detectDeviceCC()`: CoreLocation GPS + locale + timezone fallback for country detection |

**Settings** (stored in `~/.shortnerdcat/settings.json`):

| Field | Default | Purpose |
|---|---|---|
| `auto_connect` | true | Connect automatically on app start |
| `doh_enabled` | true | Route DNS through tunnel (DoH) |
| `preferred_region` | "" | Override automatic country detection |

### 2.4. linux-client

Location: `snc/linux/`

Shares the `snc/core` package — same tunnel, auth, router, discovery,
bypass, OTA, dialer pool logic as every other platform. Platform-specific
modules in `snc/linux/linux/` and `snc/linux/cmd/shortnerdcat/`. Packaged as
a `.deb` (`snc/linux/debian/`) with a systemd user unit
(`shortnerdcat.service`) and a polkit policy (`org.shortnerdcat.policy`) for
the privileged network operations a non-root desktop session can't do
directly.

**Process model:** mirrors macOS — root-privileged daemon plus a per-user tray, communicating over a Unix socket. GUI is GTK/WebKit2GTK (a native embedded browser window, not Electron) rather than a native Cocoa/WinAPI shell.

| Module | File | Purpose |
|---|---|---|
| Entry point | `cmd/shortnerdcat/main_linux.go` | Privilege check, logging, watchdog launch, IPC server, tunnel lifecycle |
| IPC server | `cmd/shortnerdcat/ipc_server_linux.go` | JSON-framed Unix socket; root process receives commands, pushes status to the app window/tray |
| Tray entry | `cmd/shortnerdcat/tray_main_linux.go` | Tray process entry point |
| Watchdog | `cmd/shortnerdcat/watchdog_linux.go` | `--watchdog` mode: PID file, restores routes on crash, OTA restart signal |
| Geo | `cmd/shortnerdcat/geo_linux.go` | Locale/timezone fallback for country detection (no GPS on desktop Linux) |
| App window | `linux/app_window_linux.go` (+ `app_window_linux.c`) | GTK/WebKit2GTK app window — key entry, status, in-app browser |
| Tray | `linux/tray_linux.go` | System tray icon/menu (AppIndicator-style) |
| Routes | `linux/routes_linux.go` | Default route via TUN; saves/restores original gateway |
| DNS | `linux/dns_linux.go` | DoH redirect; restore on disconnect |
| Firewall | `linux/firewall_linux.go` | nftables/iptables rules to block DNS leaks on the physical NIC |
| Key storage | `linux/key_linux.go` | Key-string persistence (Secret Service / plain file, distro-dependent) |
| Session | `linux/session_linux.go` | Session/state helpers shared by app window and tray |
| Network monitor | `linux/network_monitor_linux.go` | Watches for network-change events to trigger reconnect |
| Notifications | `linux/notification_linux.go` | Desktop notifications (`org.freedesktop.Notifications`) |
| Autostart | `linux/autostart_linux.go` | `.desktop` autostart entry management |
| Watchdog helpers | `linux/watchdog_linux.go` | Shared watchdog state helpers used by `cmd/shortnerdcat/watchdog_linux.go` |

Packaging: `debian/control` + `debian/postinst`/`debian/prerm` install the systemd unit and polkit policy on `apt install`; `build.sh`/`deploy.sh` mirror the Windows/macOS build+upload flow.

### 2.5. android-client (SNC Android app)

Location: `snc/android/`

Go core (`snc/android/cmd/snc-core/`) compiled as a native binary (`GOOS=android GOARCH=arm64`).
Shares the `snc/core` package — same tunnel, auth, router, discovery, bypass, OTA logic.
Kotlin wrapper launches the binary as a subprocess; IPC via Unix domain socket.

| Module | Location | Purpose |
|---|---|---|
| Go core entry point | `cmd/snc-core/main_linux.go` | Bootstrap auth (`raceAuth`), SOCKS5, bypass, discovery, decoy, DHT, IPC handler |
| TUN setup | `cmd/snc-core/tun_android.go` | `tun.CreateTUNFromFD(fd, mtu)` → gVisor netstack → SOCKS5 |
| DoT proxy | `cmd/snc-core/dot_proxy_linux.go` | DNS-over-TLS proxy; intercepts DNS-over-TLS queries from apps |
| DNS proxy | `cmd/snc-core/dns_linux.go` | Local DNS proxy (UDP :53); forwards to DoT or resolves via tunnel |
| VPN service | `app/.../SNCVpnService.kt` | `android.net.VpnService`; `establish()` → TUN fd → Go core; protect-socket loop (SCM_RIGHTS); network callbacks on background HandlerThread (not main thread) |
| Core process | `app/.../CoreProcess.kt` | Subprocess lifecycle: start/stop, generation counter, lastError propagation |
| Main activity | `app/.../MainActivity.kt` | Hosts ViewPager2 (3 tabs): ConnectionFragment, BrowseFragment, AppsFragment; `offscreenPageLimit = 2`; auto-switches to Browse on tunnel ready |
| Connection fragment | `app/.../ConnectionFragment.kt` | Connect/Disconnect, key entry, QR scanner; reads `@Volatile` VpnService state fields (no locks) |
| Browse fragment | `app/.../BrowseFragment.kt` | In-app browser: multi-tab WebView, tab persistence |
| Browse proxy | (in Go core) | Local HTTP proxy for BrowseFragment; routes WebView traffic through the active tunnel transport |
| Apps fragment | `app/.../AppsFragment.kt` | Per-app bypass list UI |
| Excluded apps | `app/.../ExcludedApps.kt` | `VpnService.Builder.addDisallowedApplication()` list |
| Notifications | `app/.../SNCVpnService.kt` | Reads `snc.notif` JSON file written by Go core; posts system notification via `NotificationManager` |
| Build | `build.bat` | Cross-compile arm64 + x86_64; sign APK; copy `.so` to jniLibs |

**IPC protocol (Go ↔ Kotlin, Unix socket):** JSON messages — `protect`, `state`, `error`, `screen-off/on`, `nettype`, `reconnect`, `status`, `stop`.
**Notification delivery:** Go core writes `$dataDir/snc.notif` (JSON array of strings); Kotlin polls on connect and shows system notification. Seen IDs deduplicated via `notif_seen.json`.
**Split tunneling:** `protect()` on Go sockets via SCM_RIGHTS bypasses TUN. Per-app exclusions via `addDisallowedApplication`. Full `0.0.0.0/0` route in TUN; CIDR-level bypass (home-region IPs) handled in Go SOCKS5 dialer via `BypassManager`.
**UI thread safety:** VpnService network callbacks run on a background HandlerThread (not `Looper.getMainLooper()`), preventing Binder IPC calls (`getNetworkCapabilities`, etc.) from blocking the main thread under VPN load.

### 2.6. ios-client

Location: `snc/ios/`

Go core compiled as a static c-archive (`GOOS=ios CGO_ENABLED=1`), linked into the `SNCTunnel` Network Extension target. Phases 1–4 complete; phase 5 (App Store distribution) pending.

| Module | Location | Purpose |
|---|---|---|
| Go core library | `cmd/snc-core/lib_ios.go` | Exported C entry points: `SNCStart(key, logDir, dataDir, tunFD)`, `SNCStop()`, `SNCGetStatus()`, `SNCReconnect()`; full tunnel stack (auth, SOCKS5, router, bypass, DHT, dialer pool, decoy) |
| TUN setup | `cmd/snc-core/tun_ios.go` | `tun.CreateTUNFromFD(fd, mtu)` → gVisor → SOCKS5; iOS routes extension-process sockets through physical NIC automatically (no `protect()` IPC needed) |
| Helpers | `cmd/snc-core/helpers_ios.go` | iOS-specific utility functions |
| GoCore bridge | `SNCTunnel/GoCore/GoCore.swift` | Swift wrapper around C entry points (via bridging header); manages state transitions |
| Packet tunnel | `SNCTunnel/PacketTunnelProvider.swift` | `NEPacketTunnelProvider` subclass; calls `SNCStart` on `startTunnel`, `SNCStop` on `stopTunnel`; polls `SNCGetStatus()` for state |
| Network monitor | `SNCTunnel/NetworkMonitor.swift` | `NWPathMonitor` watcher; triggers reconnect on network change |
| Decoy traffic | `SNCTunnel/DecoyTraffic.swift` | iOS-side decoy HTTP requests (CDN targets) |
| IPC protocol | `Shared/IPCProtocol.swift` | Shared types for App ↔ Extension communication via App Group |
| VPN manager | `SNCApp/VPNManager.swift` | `NEVPNManager` wrapper; `connect()` / `disconnect()`; reads extension status |
| Connection VC | `SNCApp/ConnectionViewController.swift` | Main UI: status image, connect button, key entry, QR scan |
| QR scan VC | `SNCApp/QRScanViewController.swift` | AVFoundation QR scanner |
| Browser VC | `SNCApp/BrowserViewController.swift` | In-app WKWebView browser (tab reuse) |
| Region VC | `SNCApp/RegionViewController.swift` | Preferred region picker |
| App delegate | `SNCApp/AppDelegate.swift`, `SceneDelegate.swift` | App lifecycle; background-refresh registration |

**iOS-specific notes:**
- No `VpnService.protect()` equivalent needed — iOS routes all sockets created by the extension through the physical interface automatically.
- State is shared between app and extension via App Group container (`group.com.shortnerdcat.snc`).

---

## 3. Data Flows

### 3.1. Tunnel Traffic (main path)

```
win-client → control:443  (raw TCP, uTLS, ChTunnel 0x00 in session ID)
           → exit:443     (HTTP obfuscated POST)
             X-Session / X-Conn / X-Seq / X-Target / X-Client-IP
           → target TCP        (from exit's IP)
```

Exit may forward to a peer exit in a different region for geo-routing:

```
win-client → control → exit-A → exit-B (peer, target region) → target TCP
             X-Peer-Token authenticates exit-A to exit-B
```

### 3.2. Relay Routes

```
A: Client → Relay(public IP) → Control → Exit → Internet
B: Client → Relay → Relay → Control → Exit → Internet
   (max 3 relay hops; maxRelays = 3)

NAT reverse-tunnel (ChSignal 0x02):
   Client/Relay → yamux → Control signal mux → relay stream → Exit

UDP relay (R5, hole-punch data path):
   Client ←UDP→ Relay peer (via UDPRelayConn)
   Control addr referenced in UDPRelayDialer; auth via hole-punched peer
```

Path selection (`core/router.go`):

```
score = w1 × totalRTT + w2 × totalLoss + w3 × (10 ms × numRelays) + instabilityPenalty
      − 30 ms  (if UDP hole-punch P2P link is live)
```

Paths are generated for every live control. All paths within 30% of the best RTT are shuffled randomly — traffic is spread across comparable controls and relays rather than pinned to one.

Data-plane failure detection (DPI body-stripping):

```
TunnelDialer.Dial() → 3 consecutive errors → SetDataFailHook fires
  → router.MarkControlDataDead(addr, +10 min)  — TCP alive but data dead
  → TriggerReconnect()
  → onConnect(): BuildPaths() skips dead control → Primary() → different control
```

**Dialer pool carry-forward:** when controls fail re-auth due to network errors (not credential rejection), the old dialer pool is preserved rather than dropped. Only `r.rejected = true` in the auth response triggers pool eviction and auth backoff. Transient network errors reuse existing dialers and retry.

### 3.3. Client Authentication

```
1. Client parses key-string → username, password
2. Client POST /api/auth/login (via exit proxy) → arbiter SessionManager.Login()
     → multi-device detection (key_id / device_id):
         same key, different device_id, within 10 s window → async warning goroutine
         warning throttle: 1/hour per key; 3 warnings → SetUserEnabled(false)
         warnings delivered via per-user notification (in auth response + manifest)
     → returns opaque SNC token (ChaCha20-Poly1305, stored in arbiter DB)
         response includes pending notifications array if any
3. Every tunnel request: X-Session: <token>
4. exit: authClient.validateSession(token)
     → GET arbiter /api/auth/session → {username, clientID}
     → local cache 1 min; stale fallback 3 h
```

**Auth backoff:** `authFails`/`authSkipUntil` maps in the dialer track per-control failure counts. Backoff only triggers on `r.rejected = true` (credential invalid). Network errors increment a separate counter and preserve the existing dialer pool for the current control.

### 3.4. Node Discovery

```
arbiter signs manifest (Ed25519) → list of controls + exit count
exit caches and serves: GET /api/manifest
win-client / macos-client / linux-client / android-client / ios-client:
  fetch via tunnel → cache manifest.json → refresh every 10 min
```

Each node receives ≤ 12 random neighbors (prevents full topology disclosure).

### 3.5. Exit List (for controls)

```
arbiter: GET /api/exits → Ed25519-signed JSON with addr, fingerprint, region
exit: proxies via /p/node/v1/arbiter/* → disk cache
control: refresh every 60 s via random exit
  → verifyExitFingerprint(addr, expectedFP): TLS-dial + SHA-256 of leaf cert
  → exits with fingerprint mismatch are skipped
```

TLS certificates: all server-side nodes use short-lived Let's Encrypt IP
certificates, auto-renewed well before expiry.
Fingerprint sync: each exit sends its current TLS fingerprint in heartbeat → arbiter updates DB → control picks it up at next refresh.

### 3.6. OTA Updates

```
Admin uploads binary → arbiter /admin/update  (platform-specific paths)
  → NotifyAll(): Ed25519-signed push (60 s validity window)
    → exit /p/node/v1/refresh → pull + cache + SHA-256
      → control /p/v1/refresh → pull + SHA-256 → restart
        → win-client / macos-client: poll every 6 h → SHA-256 → write pending update file
          → applied at next process start (rename-dance + OTA signal to watchdog)
```

Platform binary paths in OTA cache: `version`/`client` (Windows), `client-macos-amd64`/`client-macos-arm64`, `client-android-arm64`/`client-android-x86_64`, `client-ios-arm64`.

### 3.7. Split Tunneling

```
arbiter: fetches country CIDR data → GET /api/cidr/all
exit: background fetch every hour → serves GET /api/bypass/cidr (Ed25519-signed)
win-client / macos-client / linux-client / android-client / ios-client:
  fetch at connect → CIDRSet in memory
  on each new SOCKS5 connection: ShouldBypass(dest_ip)
    YES → bypassDialer → direct connection bypassing tunnel
    NO  → TunnelDialer → through tunnel
```

**Windows:** `bypassDialer` binds to original NIC IP (via `windows/routes.go`).
**macOS:** `bypassDialer` binds to physical NIC IP.
**Android:** `bypassDialer` uses `VpnService.protect()` IPC — bypasses TUN without route manipulation.
Android VPN routes: always `0.0.0.0/0` (full capture). Split happens at the SOCKS5-dialer level, not in the VPN routing table. CIDR inversion rejected — Android Binder IPC limit (~1 MB) is incompatible with ~29 K per-country CIDRs.

---

## 4. DPI Resistance

### 4.1. Channel Byte Demultiplexing

Every SNC connection embeds a two-byte marker in the TLS ClientHello Session ID (bytes [0:2]):

| Byte 0 | Byte 1 | Channel |
|---|---|---|
| 0xA7 (magic) | 0x00 | ChTunnel — pass-through to exit |
| 0xA7 | 0x01 | ChRelayAPI — relay tracker HTTP API |
| 0xA7 | 0x02 | ChSignal — NAT relay yamux reverse-tunnel |
| 0xA7 | 0x03 | ChNATRelay — routing through a NAT relay node |

Standard TLS 1.3 clients send a random 32-byte session ID; collision probability ≈ 1/256 per connection. SNI is kept empty (RFC 6066 prohibits SNI for IP-addressed servers).

### 4.2. uTLS Browser Fingerprint

All outbound TLS connections use [uTLS](https://github.com/refraction-networking/utls) with rotating browser presets:

| Preset | Weight |
|---|---|
| Chrome_Auto | 2× (dominant in the wild) |
| Firefox_Auto | 1× |
| Edge_Auto | 1× |
| Safari_Auto | 1× |

A new preset is picked randomly per connection. ALPN: h2 + http/1.1. SNI: empty. The result is a TLS ClientHello that matches a real browser fingerprint database, including extension order, cipher suites, and supported groups.

### 4.3. HTTP Obfuscation

Tunnel traffic is wrapped in HTTP POST requests:

- **Path:** one of several static-looking paths (`/api/media/upload`, `/api/content/submit`, etc.)
- **Body:** chunked encoding, random chunk sizes 4–64 KB, padding ≥ 512 B per chunk
- **Timing:** random jitter 0–50 ms between writes; adaptive pacing (pacing.go) smooths burst signatures
- **Headers:** realistic browser headers from a curated pool (headers.go)
- **X-headers:** SNC-specific headers (X-Session, X-Conn, X-Seq, X-Target) blend with the HTTP metadata — no unique prefix pattern

### 4.4. Decoy Traffic

`DecoyManager` (decoy.go) sends uTLS requests to popular CDN endpoints:

- Targets: jsDelivr, cdnjs, googleapis, fonts.googleapis, jQuery, unpkg/React
- Interval: random 20–60 s when tunnel is lightly loaded
- Suppressed: if tunnel had activity within 500 ms (to avoid doubling real activity)
- Dormant: after 10 min without tunnel activity (mimics closed browser tab)

Decoy connections are identical in TLS fingerprint to tunnel connections.

### 4.5. DNS over HTTPS (DoH)

When connected, system DNS is redirected to DoH (Cloudflare / Google) so plaintext DNS queries do not reveal destinations that are being tunneled. Cleaned up on disconnect (`CleanupDoH()`). On macOS, pf rules additionally block DNS on the physical NIC to prevent leaks if route table manipulation fails.

---

## 5. UDP ASSOCIATE (SOCKS5)

Handles SOCKS5 UDP ASSOCIATE — used by applications sending UDP (QUIC, SRTP, gaming, etc.):

**Variant C — direct bypass (preferred on Windows/macOS):**
```
App UDP datagram → SOCKS5 client local socket
  → udpAssocSession (bypass socket bound to physical NIC)
  → direct UDP to target (not through TUN)
```
- `dstStats` tracks sent/received per destination
- If 3 datagrams sent with no response in 3 s → fall back to Variant A (tunnel)
- Bypass socket uses physical NIC IP (not TUN), similar to bypass TCP dialer

**Variant A — HTTP tunnel fallback:**
```
App UDP datagram → SOCKS5
  → POST /api/udp/relay  (exit sends to target via UDP)
  ← POST /api/udp/drain  (long-poll: exit drains responses back to client)
```
- Used when direct UDP is blocked or when `tunnelOnly = true` (Android)
- TCP control connection kept alive per RFC 1928 for the ASSOCIATE lifetime

---

## 6. Geo-Routing Policy

**Invariant:** only the control→exit hop crosses national borders. Client traffic never exits in the client's own country if alternatives exist.

**Country detection (clients):**
1. GPS (CoreLocation on macOS/iOS) — highest confidence
2. CIDR lookup of public IP via bypass CIDR data
3. Timezone → country table (tz_country.go) — fallback
4. Persisted `country.txt` — used from first connect until a better source arrives
5. `preferred_region` setting overrides all automatic detection

**Relay selection (`router.go`):**
- `SetMyCountry(cc)` allows filtering relays by country at client side
- Regional relay filtering: client avoids relays in its own country when foreign ones are available

---

## 7. Health Monitoring and Self-Liveness (Client-Side)

### 7.1. Win-Client / macOS-Client Watchdog

Same binary, launched with `--watchdog` flag (Windows) or as a separate process via `snmac.StartWatchdog()` (macOS):

- **Windows:** named mutex `SNCWatchdog`; state file `APPDATA\ShortNerdCat\watchdog-state.json`; OTA signal via Windows Named Event `Global\SNCCleanShutdown`
- **macOS:** PID file `~/.shortnerdcat/snc_watchdog.pid`; state file `~/.shortnerdcat/watchdog-state.json`; OTA signal via `snmac.SignalUpdateRestart()`; both processes monitor each other (3 s restart delay)
- State fields: `MainPID`, `Connected`, `OrigGW`, `LastAlive`, `TunnelHealthy`
- Main process writes `LastAlive` every 60 s; watchdog detects frozen main process via stale timestamp
- On crash: watchdog reads `Connected` + `OrigGW`, restores default routes

### 7.2. Silent Refresh (Client-Side)

When a data-plane failure is detected (pool eviction or data watchdog), the client attempts a silent path refresh without disconnecting:

```
silentRefresh goroutine (2 min deadline, exp backoff 5 s → 30 s):
  1. ProbeDataPlane(5 s) → BuildPaths()
  2. For each viable control: Login() + NewControlDialer()
  3. Also: UDP relay controls (hole-punch peers for blocked controls)
  4. pool.Swap(freshDialers) → SetTunnelHealthy(true)
  5. If timeout: TriggerReconnect() (full disconnect + reconnect)
```

---

## 8. NodeID and Node Security

**Problem:** a plain hex NodeID can be copied and used to register a relay impersonating another node.

**Solution:**
- NodeID = Ed25519 pubkey (64 hex chars)
- Private key encrypted with DPAPI (`nodeid.key`) on Windows, Keychain on macOS/iOS
- Every register/heartbeat is signed: `Ed25519Sign(privkey, JSON_body)`
- Payload includes `"ts": unix_timestamp`; control rejects if `|now − ts| > 60 s`
- Control: `verifyNodeSig(nodeID, body, X-Node-Sig, ts)`

Backward compatibility: old NodeID = 32 hex chars → detected by length → auto-regenerated.

---

## 9. Key-String Format

```
[4 B magic][24 B nonce][XChaCha20-Poly1305 encrypted JSON] → base64url (no padding)
```

Encryption key: `BLAKE2b-256(hardcoded_32B_secret || 0x01)` — baked into the binary.

**Versions:**
- V1 magic `0x534E4301` (`SNC\x01`) — legacy, no signature; accepted for backward compatibility
- V2 magic `0x534E4302` (`SNC\x02`) — Ed25519-signed by arbiter; generated via `/admin/key`

JSON payload (V2):
```json
{
  "username": "user@example.com",
  "password": "...",
  "control_nodes": ["1.2.3.4:443", "5.6.7.8:443"],
  "arbiter_pubkey": "<hex Ed25519 pubkey>",
  "api_key": "<arbiter API key>",
  "client_id": "clt_<24hex>",
  "key_id": "<key record ID in arbiter DB>",
  "sig": "<base64url Ed25519 signature over canonical JSON without sig field>"
}
```

Client on V2 parse: verifies signature against `arbiter_pubkey`. If `arbiter_pubkey == ""` (dev mode): verification skipped.

---

## 10. Security

| Aspect | Solution |
|---|---|
| Client does not know exit or arbiter addresses | Only controls in key-string; exits hidden |
| NodeID spoofing | Ed25519 signature + DPAPI/Keychain private key |
| Relay registration replay | Timestamp window 60 s in signed payload |
| Manifest / exit list substitution | Ed25519 signature by arbiter; pubkey in binary |
| OTA substitution | SHA-256 at every level; only applied if version is strictly newer |
| Unauthorized tunnel access | 404 (not 401/403) — does not reveal service presence |
| Split-tunnel CIDR tampering | Ed25519-signed; client verifies before applying |
| ClientID forgery | Key-string Ed25519-signed by arbiter; client rejects invalid signature |
| Regional routing verification | relay country_code verified via NodeID signature at registration |
| Multi-device key abuse | Same key from two device IDs within 10 s → graduated warning (×3 → account suspended); 1 warning/hour throttle |
| DNS leaks on macOS | pf anchor blocks DNS on physical NIC while tunnel is up |
| Device log content in transit | Uploaded directly client→arbiter over the client's own live tunnel dialer (`log_upload.go`), not relayed through control/exit, so plaintext log content never passes through a relaying control/exit |
| Control-node TLS pinning | Client pins each control's cert fingerprint to the value in the signed manifest, not just on first-seen |
| IPv6 kill switch | Server-controlled, broadcast via signed manifest (`ipv6_enabled`); when off, clients force-disable IPv6 and hide the manual toggle |
