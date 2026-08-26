# ShortNerdCat — Architecture

**Current as of:** 2026-08-14

**Repo split (2026-08-04):** this file predates a monorepo trim that moved
most of what it describes out of `shortnerdcat` into sibling repos, all
still under a local Go module `replace` (`shortnerdcat`'s `go.mod` replaces
`tunnel_cat => ../tunnel_cat`). `shortnerdcat` itself is now client-only.
Every "Location:" line below has been updated to say which repo a path is
relative to — treat a bare path as relative to whichever repo its section
names.

- **`shortnerdcat`** (this repo) — the 5 client apps (`snc/{win,mac,linux,android,ios}/`) only.
- **`tunnel_cat`** — server (`snc-arbiter/`, `snc-control/`, `snc-exit/`), the shared client tunnel core (`snc/core/`), the covert-transport relay package, `wlwtp`, deploy tooling, SNC_SDK.
- **`ratatosk`** / **`tunnel_cat_bots`** — the two bot-controller daemons described in §11 (moved out of both repos above; kept here only as a pointer).
- **`sniffing_cat`** — the standalone reachability-scanner app, formerly §2.10 here.
- **`tunnel_cat_apps`** — the Proekt/Lisinder white-label products (not covered by this doc).

---

## 1. Concept

ShortNerdCat is an obfuscated proxy tunnel with DPI bypass. Traffic is indistinguishable from HTTPS at the DPI level. Architecture is multi-node, one physical host per role.

**Naming:** `exit` in code and API; "egress" only in arbiter UI.

---

## 2. Components

All 5 client apps below are in the **`shortnerdcat`** repo; the 3 node roles
below them are in the **`tunnel_cat`** repo (see repo-split note above).

```
┌───────────────────────┐  ┌───────────────────────┐  ┌───────────────────────┐
│  Windows (snc/win/)   │  │  macOS (snc/mac/)     │  │  Linux (snc/linux/)   │
│  WinTun → SOCKS5      │  │  utun → SOCKS5        │  │  wintun-equiv → SOCKS5│
│  uTLS, DHT, relay,    │  │  uTLS, DHT, relay,    │  │  uTLS, DHT, relay,    │
│  WildCat              │  │  WildCat              │  │  WildCat              │
│  WebView2, DoH, OTA   │  │  pf, launchctl, OTA   │  │  GTK/WebKit2GTK, OTA  │
└──────────┬────────────┘  └──────────┬────────────┘  └──────────┬────────────┘
           │                          │                          │
┌───────────────────────┐  ┌───────────────────────┐             │
│  Android (snc/android)│  │  iOS (snc/ios/)       │             │
│  Go bin + VpnService  │  │  Go c-archive +       │             │
│  BrowseFragment,      │  │  NEPacketTunnel ext.  │             │
│  WildCat, DoT proxy   │  │  Swift UI             │             │
└──────────┬────────────┘  └──────────┬────────────┘             │
           │                          │                          │
           └──────────────┬───────────┴──────────────────────────┘
                          │  raw TCP (uTLS + channel byte in session ID)
                          ▼
┌──────────────────────────────────────────────────────────────┐
│  snc-control  (one per region/country)                       │
│  Pure TCP pass-through. Does not terminate TLS, sees no      │
│  payload. Relay tracker API (same :443, demuxed by channel   │
│  byte). Exit health probe + data-plane probe. Gossip.        │
│  CIDR-based client country lookup. OTA updater.              │
│  IP address is rotated periodically (yc_rotate.py /          │
│  cloudru_rotate.py, tunnel_cat/tools/) — node_id persists.   │
└────────────────────┬─────────────────────────────────────────┘
                     │  raw TCP (forwarded as-is)
                     ▼
┌──────────────────────────────────────────────────────────────┐
│  snc-exit  (one per datacenter)                              │
│  Tunnel handler: HTTP → TCP → Internet                       │
│  Auth: SNC session token → arbiter validation cache          │
│  Peer exit routing (geo-aware, whitelist fallback).          │
│  UDP relay + drain. Data-plane self-probe. Bypass CIDR.      │
│  OTA cache. Proxy to arbiter (/p/node/v1/*).                 │
│  Egress capabilities self-probe (reports to arbiter).        │
│  BananaMeter tunnel-diagnostics probe (client+node signal).  │
└─────────┬──────────────────────────┬────────────────────────-┘
          │ heartbeat                │ proxy
          ▼                         ▼
┌──────────────────────────────────────────────────────────────┐
│  snc-arbiter  (single instance)                              │
│  SQLite WAL. Auth via Camerlengo. First user = admin.        │
│  Node management: submit → approve → heartbeat.              │
│  Admin UI. Key generation (V2 signed). OTA upload + push.    │
│  SSH auto-deploy. Whitelist API (peer-routing). Probe-sites  │
│  API. Egress caps view. Downloads page. Public tech page.    │
│  Admin kill switches: relay registration, IPv6, torrent.     │
│  Per-user connection-health stats (BananaMeter, conn-stats). │
└──────────────────────────────────────────────────────────────┘
```

**WildCat subsystem** (cross-platform, see section 2.8):

```
┌──────────────────────────────────────────────────────────────┐
│  WildCat (tunnel_cat repo, not part of this public mirror)     │
│  Covert transport over a third party's video-call TURN relays │
│  Credentials fetched at runtime via the user's own login      │
│  Used by Windows, macOS, Linux, Android when tunnel is blocked │
└──────────────────────────────────────────────────────────────┘
```

### 2.1. win-client

Location: `snc/win/` (shortnerdcat repo) — shared tunnel logic is `snc/core/` in the **tunnel_cat** repo (Go module `replace`)

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
| Discovery | `core/discovery.go` | Ed25519-signed manifest; cache `manifest.json`; refresh every 10 min; WildCat OAuth token callback |
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
| WildCat creds | `core/wildcat_creds.go` | Per-platform WildCat credential storage |
| TZ/country | `core/tz_country.go` | Timezone-to-country fallback for country detection without GPS |
| Headers | `core/headers.go` | Pool of realistic browser User-Agent strings |
| Log upload | `core/log_upload.go` | Periodic log shipping to arbiter via tunnel |
| Tray | `windows/tray.go` | System tray: icons, menu, relay toggle |
| Dialog | `windows/dialog.go` | Key-string entry dialog; DPAPI storage |
| Routes | `windows/routes.go` | Original gateway save, default route via TUN, `Restore()` on disconnect; `getDefaultGateway()` retries 5× (route.exe transiently fails during WinTun adapter creation) |
| Browser window | `windows/browserwindow.go` | In-app browser via WebView2 (Edge runtime) |
| UI window | `windows/uiwindow.go` | Main application window host |
| DoH proxy | `windows/doh_proxy.go` | Local DNS-over-HTTPS proxy; redirects system DNS to Cloudflare/Google |

### 2.2. snc-control

Location: `snc-control/` (**tunnel_cat** repo)

| Module | File | Purpose |
|---|---|---|
| Entry point | `main.go` | Flag parsing, component wiring, `net.Listen(:443)` → accept loop |
| TCP proxy | `handler.go` | Per-connection: read channel byte from TLS session ID → route to relay API or pass-through to exit |
| Exit registry | `exits.go` | Periodically fetches exit list from arbiter; `PickFor(clientCC)` — geo-aware selection excluding control region and client country |
| Exit health | `exit_health.go` | HTTP probe every 10 s; dead after 2 consecutive failures, alive after 2 successes; EMA traffic RTT; `allDeadRestartAfter = 10 min` → os.Exit(1) |
| Probe sites | `probe_sites.go` | Per-region URL sets for data-plane probe; fetched from arbiter every 2 h, disk-cached |
| Relay API | `relay_api.go` | TLS relay tracker: register / heartbeat / list; Ed25519 + timestamp verification; stores country_code |
| Relay registry | `relay_registry.go` | In-memory relay roster; TTL eviction; de-duplicate by NodeID |
| Relay mux | (signal_mux + relay_api) | yamux reverse-tunnel for clients behind NAT (ChSignal channel) |
| Signal mux | `signal_mux.go` | WebSocket hub; pushes `relay_request` events to registered clients |
| Gossip | `gossip.go` | `POST /p/v1/control/sync` — relay roster exchange between peer controls |
| CIDR cache | `cidr_cache.go` | Fetches CIDR data from arbiter; `lookupCC(ip)` → ISO country code; used both for own region auto-detect and per-connection client country lookup |
| Region detect | `region.go` | `detectRegionByIP()`: resolves control's own public IP to region code via CIDR data |
| UDP reflector | `udp_reflector.go` | UDP myip service on same port: echoes client's source address; also used by NAT prober |
| OTA | `updater.go` | Polls random exit for new version; SHA-256 verify; os.Exit(0) → systemd restart |
| Log uploader | `log_uploader.go` | Ships control logs to arbiter periodically |
| TLS | `tls.go` | Self-signed cert generation for relay API (cert pinned by relay clients) |
| Logging | `snclog.go` | Structured leveled logger |

### 2.3. snc-exit

Location: `snc-exit/` (**tunnel_cat** repo)

| Module | File | Purpose |
|---|---|---|
| Entry point | `main.go` | Flag parsing, TLS setup, HTTP server start, component wiring |
| Tunnel handler | `handler.go` | HTTP POST to tunnel paths → TCP to target; streaming mode; padding; geo-routing loop guard; `X-Client-IP` forwarding |
| Auth | `auth.go` | Validates `X-Session` token against arbiter; 1-min local cache, 3-h stale fallback |
| Sessions | `sessions.go` | In-memory `{conn_id → net.Conn}` for streaming tunnel connections |
| Whitelist | `whitelist.go` | Ed25519-signed whitelist from arbiter; domain → allow; drives exit-to-exit peer-routing fallback; TTL refresh |
| Blacklist | `blacklist.go` | Exit address blacklist (e.g. known bad IPs); loaded from arbiter |
| Peer registry | `peers.go` | Fetches peer exit list from arbiter; geo-aware peer selection for multi-exit routing |
| Peer connection | `peer_conn.go` | Dials peer exit with node token authentication; proxies tunnel streams between exits |
| UDP sessions | `udp_sessions.go` | `/api/udp/relay` (send) + `/api/udp/drain` (long-poll receive) for SOCKS5 UDP ASSOCIATE fallback |
| Data-plane probe | `data_plane_probe.go` | TCP dial 1.1.1.1:80 every 30 s; `isOK()` used in health reports; also serves `/p/v1/probe?url=` for control to check data-plane |
| CIDR cache | `cidr_cache.go` | Country lookup from CIDR data; used for client geo-routing decisions in handler |
| Stats | `stats.go` | Per-session traffic counters; periodic push to arbiter |
| OTA cache | `control_cache.go` | Caches client binary; pull every 6 h + on refresh; SHA-256 verify |
| Node proxy | `node_proxy.go` | `/p/node/v1/*`: reverse proxy to arbiter; serves OTA binary; Ed25519-verified refresh |
| Arbiter client | `arbiter.go` | Low-level HTTP client for arbiter API calls (exits list, whitelist, probe sites) |
| Log uploader | `log_uploader.go` | Ships exit logs to arbiter |
| TLS | `tls.go` | Let's Encrypt / manual cert |
| Logging | `snclog.go` | Structured leveled logger |

### 2.4. snc-arbiter

Location: `snc-arbiter/` (**tunnel_cat** repo)

| Module | File | Purpose |
|---|---|---|
| DB | `db.go` | SQLite WAL: `users`, `nodes`, `sessions`, `keys`, `devices`, `notifications`, `user_stats`, `user_failures`; `IsOnline()` (TTL 90 s) |
| Handler | `handler.go` | HTTP router; auth middleware; admin middleware; CIDR serving |
| Auth client | (in handler) | Proxy auth calls to Camerlengo; used by SessionManager and node validation |
| Session manager | `sessions.go` | Issues opaque SNC tokens (ChaCha20-Poly1305, arbiter key); re-validates with Camerlengo every 15 min; stale TTL 3 h; device-binding on first activation; multi-device detection with graduated warnings |
| Notifications | `notifications.go`, `admin_notify.go` | Broadcast and per-user notifications: stored in DB, embedded in manifest and key-auth responses; admin CRUD UI; 24 h TTL, idempotent by ID |
| User stats | `db_stats.go`, `admin_stats.go` | Per-user traffic counters and connection failures reported by exit nodes; admin `/admin/users` dashboard with Suspend/Resume |
| Admin dashboard | `templates/admin_dashboard.html` | Node status overview with tabs: nodes, users, keys, OTA, notifications |
| Key generation | `admin_keygen.go` | `GET /admin/key` + `POST /admin/key/generate` — V2 Ed25519-signed key-string generation |
| Key encoding | `keyenc.go` | `buildSignedKey()`: constructs and signs V2 key-string |
| Node management | (in handler) | submit / approve / reject / suspend; heartbeat API; fingerprint update from exit heartbeat |
| Exit API | (in handler) | `GET /api/exits` — Ed25519-signed JSON list of exits with fingerprints and regions |
| Whitelist API | (in handler) | `GET /api/whitelist` — Ed25519-signed domain whitelist; drives exit-to-exit peer-routing fallback |
| Probe-sites API | (in handler) | `GET /api/probe-sites` — per-region URL sets for data-plane probing |
| CIDR | (in handler) | `GET /api/cidr/all` — country CIDR data from ipdeny.com |
| OTA | `admin_update.go` | Upload → SHA-256 → atomic rename → `NotifyAll()`; platform-specific paths: `version`/`client` (Win), `client-macos-*`, `client-android-*`, `client-ios-*` |
| Downloads page | `admin_downloads.go` | `/admin/downloads` — manages public download links for each client platform |
| Egress caps | `admin_egress_caps.go` | `GET /admin/egress/{id}/caps` — per-exit view of whitelist domain probe results (`ok`/`fail`/`unknown`/`na`); crosses `node.Capabilities` map with whitelist DB |
| WildCat admin | `admin_wildcat.go` | Admin UI for WildCat OAuth token management and delivery via manifest |
| Technology page | `technology.go` | `GET /technology` — public Navlink marketing page rendered with i18n |
| Notifier | `notifier.go` | Push refresh: exits `/p/node/v1/refresh`, controls `/p/v1/refresh` |
| SSH deploy | `deployer.go` (provisioner) | SSH → host, write systemd unit, revoke operator SSH key after deploy |
| Crypto | `crypto.go` | AES-256-GCM for SSH credentials in DB |

### 2.5. macos-client

Location: `snc/mac/` (shortnerdcat repo)

Shares `snc/core (tunnel_cat repo)` package — same tunnel, auth, router, discovery, bypass, WildCat, OTA, dialer pool logic. Platform-specific modules in `snc/mac/macos/` and `snc/mac/cmd/shortnerdcat/`.

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
| `wildcat_only` | false | Force WildCat transport; bypasses normal tunnel |
| `preferred_region` | "" | Override automatic country detection |

**WildCat mode on macOS:** when enabled, all SOCKS5 traffic is routed through the covert relay pool. pf DNS rules remain active. Credential-domain and relay IPs are resolved/added to bypass before routes are applied so the pool does not loop back through TUN.

### 2.6. linux-client

Location: `snc/linux/` (shortnerdcat repo)

Shares `snc/core (tunnel_cat repo)` package — same tunnel, auth, router, discovery, bypass, WildCat, OTA, dialer pool logic as every other platform. Platform-specific modules in `snc/linux/linux/` and `snc/linux/cmd/shortnerdcat/`. Packaged as a `.deb` (`snc/linux/debian/`) with a systemd user unit (`shortnerdcat.service`) and a polkit policy (`org.shortnerdcat.policy`) for the privileged network operations a non-root desktop session can't do directly.

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
| WildCat auth | `linux/wildcatauth_linux.go` | WildCat's login WebView panel, Linux GTK equivalent of the Windows/macOS one (not part of this public mirror) |
| Watchdog helpers | `linux/watchdog_linux.go` | Shared watchdog state helpers used by `cmd/shortnerdcat/watchdog_linux.go` |

Packaging: `debian/control` + `debian/postinst`/`debian/prerm` install the systemd unit and polkit policy on `apt install`; `build.sh`/`deploy.sh` mirror the Windows/macOS build+upload flow.

### 2.7. android-client (SNC Android app)

Location: `snc/android/` (shortnerdcat repo)

Go core (`snc/android/cmd/snc-core/`) compiled as a native binary (`GOOS=android GOARCH=arm64`).
Shares `snc/core (tunnel_cat repo)` package — same tunnel, auth, router, discovery, bypass, VLESS, OTA logic.
Kotlin wrapper launches the binary as a subprocess; IPC via Unix domain socket.

| Module | Location | Purpose |
|---|---|---|
| Go core entry point | `cmd/snc-core/main_linux.go` | Bootstrap auth (`raceAuth`), SOCKS5, bypass, discovery, decoy, DHT, WildCat, IPC handler |
| TUN setup | `cmd/snc-core/tun_android.go` | `tun.CreateTUNFromFD(fd, mtu)` → gVisor netstack → SOCKS5 |
| DoT proxy | `cmd/snc-core/dot_proxy_linux.go` | DNS-over-TLS proxy; intercepts DNS-over-TLS queries from apps |
| DNS proxy | `cmd/snc-core/dns_linux.go` | Local DNS proxy (UDP :53); forwards to DoT or resolves via tunnel |
| VPN service | `app/.../SNCVpnService.kt` | `android.net.VpnService`; `establish()` → TUN fd → Go core; protect-socket loop (SCM_RIGHTS); network callbacks on background HandlerThread (not main thread) |
| Core process | `app/.../CoreProcess.kt` | Subprocess lifecycle: start/stop, generation counter, lastError propagation |
| Main activity | `app/.../MainActivity.kt` | Hosts ViewPager2 (3 tabs): ConnectionFragment, BrowseFragment, AppsFragment; `offscreenPageLimit = 2`; auto-switches to Browse on tunnel ready |
| Connection fragment | `app/.../ConnectionFragment.kt` | Connect/Disconnect, key entry, QR scanner; reads `@Volatile` VpnService state fields (no locks) |
| Browse fragment | `app/.../BrowseFragment.kt` | In-app browser: multi-tab WebView, tab persistence, wildcat mode ribbon |
| Browse proxy | (in Go core) | Local HTTP proxy for BrowseFragment; routes WebView traffic through the active transport (the covert relay in WildCat mode) |
| Apps fragment | `app/.../AppsFragment.kt` | Per-app bypass list UI |
| Excluded apps | `app/.../ExcludedApps.kt` | `VpnService.Builder.addDisallowedApplication()` list |
| Notifications | `app/.../SNCVpnService.kt` | Reads `snc.notif` JSON file written by Go core; posts system notification via `NotificationManager` |
| Build | `build.bat` | Cross-compile arm64 + x86_64; sign APK (`tf38key.jks`); copy `.so` to jniLibs |

**IPC protocol (Go ↔ Kotlin, Unix socket):** JSON messages — `protect`, `state`, `error`, `wildcat`, `screen-off/on`, `nettype`, `reconnect`, `status`, `stop`.
**Notification delivery:** Go core writes `$dataDir/snc.notif` (JSON array of strings); Kotlin polls on connect and shows system notification. Seen IDs deduplicated via `notif_seen.json`.
**Split tunneling:** `protect()` on Go sockets via SCM_RIGHTS bypasses TUN. Per-app exclusions via `addDisallowedApplication`. Full `0.0.0.0/0` route in TUN; CIDR-level bypass (home-region IPs) handled in Go SOCKS5 dialer via `BypassManager`.
**UI thread safety:** VpnService network callbacks run on a background HandlerThread (not `Looper.getMainLooper()`), preventing Binder IPC calls (`getNetworkCapabilities`, etc.) from blocking the main thread under VPN load.

### 2.8. WildCat subsystem

Location: a covert-transport package in the **tunnel_cat** repo (not part of this public
mirror — see the note at the top of this document).

Covert transport that tunnels SNC connections over an unrelated third party's video-call TURN
relays. To DPI, traffic looks like an ordinary video call — not a VPN connection. Traffic flow:
client → DTLS/smux (or WLWTP) → the relay (obtained by having the device create its own private
video conference with that third party) → snc-control's relay listener port.

Credentials are fetched at **runtime**, not baked in at build time: a credential manager
exchanges the device's own live session token through the third party's own API to obtain TURN
username/password scoped to a conference the device itself created. No build-time secret, no
key rotation concerns — a fresh conference is created whenever credentials expire.

The client side opens a WebView/WKWebView (or, on Android, the app's own Browse tab) panel to
acquire the session token that seeds this flow; the resulting access token is handed to the
credential manager as an `accessToken func(ctx) (string, error)` callback. None of this client
login/credential-exchange logic, nor the relay-pool package itself, is part of this public
mirror — the general covert-transport principle is documented here, but the specifics of which
third-party service is used and how its credentials are obtained are deliberately not published.

### 2.9. ios-client

Location: `snc/ios/` (shortnerdcat repo)

Go core compiled as a static c-archive (`GOOS=ios CGO_ENABLED=1`), linked into the `SNCTunnel` Network Extension target. Phases 1–4 complete; phase 5 (App Store distribution) pending.

| Module | Location | Purpose |
|---|---|---|
| Go core library | `cmd/snc-core/lib_ios.go` | Exported C entry points: `SNCStart(key, logDir, dataDir, tunFD, wildcatMode)`, `SNCStop()`, `SNCGetStatus()`, `SNCSetWildcat()`, `SNCReconnect()`; full tunnel stack (auth, SOCKS5, router, bypass, DHT, WildCat, dialer pool, decoy) |
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
| Browser VC | `SNCApp/BrowserViewController.swift` | In-app WKWebView browser (tab reuse, wildcat mode) |
| Region VC | `SNCApp/RegionViewController.swift` | Preferred region picker |
| App delegate | `SNCApp/AppDelegate.swift`, `SceneDelegate.swift` | App lifecycle; background-refresh registration |

**iOS-specific notes:**
- No `VpnService.protect()` equivalent needed — iOS routes all sockets created by the extension through the physical interface automatically.
- WildCat mode supported: `SNCSetWildcat(1)` switches the Go tunnel to use the WildCat mux.
- State is shared between app and extension via App Group container (`group.com.shortnerdcat.snc`).

---

### 2.10. SniffingCat (standalone Android app) — moved out 2026-08-04

**No longer in this repo.** SniffingCat (standalone APK, no SNC tunnel
dependency — passively measures network reachability and stores structured
JSONL logs) was split out into its own **`sniffing_cat`** repo during the
2026-08-04 monorepo trim, full history preserved via `git filter-repo`. See
that repo's own docs for current architecture; not covered here.

---

## 3. Data Flows

### 3.1. Tunnel Traffic (main path)

```
win-client → snc-control:443  (raw TCP, uTLS, ChTunnel 0x00 in session ID)
           → snc-exit:443     (HTTP obfuscated POST)
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

### 3.3. WildCat Data Path

```
Client browser/app → TUN/SOCKS5 → TunnelDialer pool
  SNC stack: uTLS ClientHello + channel byte + encrypted HTTP POST
  transport = the covert relay pool's Dial
  → relay pool: WLWTP session to the third-party TURN relay
    TURN Send Indication → UDP to control node
  → snc-control: WLWTP listener → proxyToExit()
    PickForClient(ctrlIP, clientCC) → consistent hash → exit node
  → Exit node (standard SNC-framed HTTP POST, same handling as regular tunnel traffic)
    → target TCP
```

Relay credentials are fetched at runtime by creating the device's own private video
conference with the third party — no build-time secret, no OAuth token delivered via the
manifest. This replaced the earlier Yandex Disk (WebDAV/REST, batch-file) transport,
which has been fully retired. See `WILDCAT.md` for the full WLWTP protocol detail; the
credential-exchange and relay-pool implementation is not part of this public mirror.

### 3.4. Client Authentication

```
1. Client parses key-string → username, password
2. Client POST /api/auth/login (via exit proxy) → arbiter SessionManager.Login()
     → Camerlengo password validation
     → multi-device detection (key_id / device_id):
         same key, different device_id, within 10 s window → async warning goroutine
         warning throttle: 1/hour per key; 3 warnings → SetUserEnabled(false)
         warnings delivered via per-user notification (in auth response + manifest)
     → returns opaque SNC token (ChaCha20-Poly1305, stored in arbiter DB)
         response includes pending notifications array if any
3. Every tunnel request: X-Session: <token>
4. snc-exit: authClient.validateSession(token)
     → GET arbiter /api/auth/session → {username, clientID}
     → local cache 1 min; stale fallback 3 h
5. Arbiter re-validates token against Camerlengo every 15 min
```

**Auth backoff:** `authFails`/`authSkipUntil` maps in the dialer track per-control failure counts. Backoff only triggers on `r.rejected = true` (credential invalid). Network errors increment a separate counter and preserve the existing dialer pool for the current control.

### 3.5. Node Discovery

```
snc-arbiter signs manifest (Ed25519) → list of controls + exit count + WildCat token (obfuscated)
snc-exit caches and serves: GET /api/manifest
win-client / macos-client / linux-client / android-client / ios-client:
  fetch via tunnel → cache manifest.json → refresh every 10 min
  WildCat token callback: applied to wildcat.Transport on receipt
```

Each node receives ≤ 12 random neighbors (prevents full topology disclosure).

### 3.6. Exit List (for controls)

```
snc-arbiter: GET /api/exits → Ed25519-signed JSON with addr, fingerprint, region
snc-exit: proxies via /p/node/v1/arbiter/* → disk cache
snc-control: refresh every 60 s via random exit
  → verifyExitFingerprint(addr, expectedFP): TLS-dial + SHA-256 of leaf cert
  → exits with fingerprint mismatch are skipped
```

TLS certificates: all nodes (arbiter, exit, control) use **Let's Encrypt** IP certificates
(`certbot --preferred-profile shortlived --ip-address <IP>`, ~160 h validity).
Auto-renewal: `certbot-renew.timer` every 6 h.
Fingerprint sync: each exit sends its current TLS fingerprint in heartbeat → arbiter updates DB → control picks it up at next refresh.

### 3.7. OTA Updates

```
Admin uploads binary → snc-arbiter /admin/update  (platform-specific paths)
  → NotifyAll(): Ed25519-signed push (60 s validity window)
    → snc-exit /p/node/v1/refresh → pull + cache + SHA-256
      → snc-control /p/v1/refresh → pull + SHA-256 → os.Exit(0) [systemd restart]
        → win-client / macos-client: poll every 6 h → SHA-256 → write pending update file
          → applied at next process start (rename-dance + OTA signal to watchdog)
```

Platform binary paths in OTA cache: `version`/`client` (Windows), `client-macos-amd64`/`client-macos-arm64`, `client-android-arm64`/`client-android-x86_64`, `client-ios-arm64`.

### 3.8. Split Tunneling

```
snc-arbiter: fetches CIDR (ipdeny.com: RU, US, CN, IR, EU) → GET /api/cidr/all
snc-exit: background fetch every hour → serves GET /api/bypass/cidr (Ed25519-signed)
win-client / macos-client / linux-client / android-client / ios-client:
  fetch at connect → CIDRSet in memory
  on each new SOCKS5 connection: ShouldBypass(dest_ip)
    YES → bypassDialer → direct connection bypassing tunnel
    NO  → TunnelDialer → through tunnel
```

**Windows:** `bypassDialer` binds to original NIC IP (via `windows/routes.go`).
**macOS:** `bypassDialer` binds to physical NIC IP; WildCat backend IPs added as explicit bypass routes before TUN starts.
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
- **WildCat mode** (macOS): Russian CDN/cloud targets at higher rate — traffic profile matches ordinary Russian browsing rather than CDN-heavy Western pattern

Decoy connections are identical in TLS fingerprint to tunnel connections.

### 4.5. DNS over HTTPS (DoH)

When connected, system DNS is redirected to DoH (Cloudflare / Google) so plaintext DNS queries do not reveal destinations that are being tunneled. Cleaned up on disconnect (`CleanupDoH()`). On macOS, pf rules additionally block DNS on the physical NIC to prevent leaks if route table manipulation fails.

### 4.6. WildCat Covert Transport

When direct TCP to controls is blocked (e.g. deep inspection blocking unknown TLS traffic),
WildCat provides an alternative data path indistinguishable from an ordinary video call with
an unrelated third-party service:
- All connections go to a TURN relay obtained by creating the device's own private video
  conference with that service
- Credentials are the same TURN username/password any client of that service would receive
  for a real call
- No port 443 anomalies — standard TURN/DTLS traffic to a well-known consumer service

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

**Invariant (M4.3):** only the control→exit hop crosses national borders. Client traffic never exits in the client's own country if alternatives exist.

**Control → exit selection (`exits.go:PickFor`):**
- Excludes exits whose region == control's own region
- Additionally excludes exits whose region == client's country (derived from `X-Client-IP` → `cidrCache.lookupCC()`)
- Fallback: if no exits remain after filtering, uses all alive exits (logged as warning)
- Client country resolved per connection — not cached across connections

**Exit → peer exit selection (`handler.go`, `peers.go`):**
- When forwarding to a peer exit, excludes peers whose region == client's country
- `X-Client-IP` header carries the client's public IP end-to-end through the chain

**Relay selection (`router.go`):**
- `SetMyCountry(cc)` allows filtering relays by country at client side
- Regional relay filtering: client avoids relays in its own country when foreign ones are available

**Country detection (clients):**
1. GPS (CoreLocation on macOS/iOS) — highest confidence
2. CIDR lookup of public IP via bypass CIDR data
3. Timezone → country table (tz_country.go) — fallback
4. Persisted `country.txt` — used from first connect until a better source arrives
5. `preferred_region` setting overrides all automatic detection

---

## 7. Health Monitoring and Self-Liveness

### 7.1. Control → Exit Health Probe

Control probes each exit every 10 s via HTTPS:

```
GET https://<exit>/p/v1/ping
```

- Dead after 2 consecutive failures; alive after 2 consecutive successes
- EMA of traffic dial RTT also tracked (updated on real proxied connections)

### 7.2. Control Data-Plane Probe

Control asks each exit to verify outbound internet connectivity using per-region probe URLs:

```
GET https://<exit>/p/v1/probe?url=<encoded-url>
Exit: HEAD <url> → 200 = ok
```

- Probe URL sets fetched from arbiter every 2 h, disk-cached (probe_sites.go)
- Threshold: ≥ 20% of URLs must succeed
- Failure marks the exit as data-plane-dead (separate from TCP-ping health)

### 7.3. Control Self-Liveness

`healthLoop` in exit_health.go tracks time since last alive exit:

```
allDeadSince → if > 10 min → os.Exit(1)
```

systemd unit: `Restart=always`, `RestartSec=5s`. This ensures the control restarts if all exits become unreachable (network partition, mass failure).

**IP rotation (added 2026-08-13):** some control nodes have their public IP
periodically rotated by an external daemon (`yc_rotate.py` for Yandex Cloud,
`cloudru_rotate.py` for cloud.ru; both `tunnel_cat/tools/`, run outside the
control process itself). The node's `node_id` and history persist across a
rotation — the daemon calls the arbiter's address-only
`/api/admin/node/{id}/update`, not a deregister+reregister. Static
per-server host lists for these roles are no longer meaningful (retired
2026-08-13, `tunnel_cat/deploy/servers/{control,exit}.txt`); the arbiter's
own live node list is the only current source of truth for which address a
given control/exit is reachable at. When the rotation daemon fails partway
(observed: an SSH permission-fixup step failing after a new TLS cert is
already issued but before the node's own env file / systemd unit is updated
and the arbiter told the new address), the node keeps running under its old
self-reported IP and cert while only being reachable at the new one — the
arbiter shows a stale address until this is corrected by hand.

### 7.4. Exit Data-Plane Self-Probe

`dataPlaneProber` (data_plane_probe.go) on the exit side:

```
TCP dial 1.1.1.1:80 every 30 s → isOK() bool
```

Result is reported to control in HTTP probe responses (`/p/v1/probe`) and may be surfaced in heartbeat.

### 7.5. Win-Client / macOS-Client Watchdog

Same binary, launched with `--watchdog` flag (Windows) or as a separate process via `snmac.StartWatchdog()` (macOS):

- **Windows:** named mutex `SNCWatchdog`; state file `APPDATA\ShortNerdCat\watchdog-state.json`; OTA signal via Windows Named Event `Global\SNCCleanShutdown`
- **macOS:** PID file `~/.shortnerdcat/snc_watchdog.pid`; state file `~/.shortnerdcat/watchdog-state.json`; OTA signal via `snmac.SignalUpdateRestart()`; both processes monitor each other (3 s restart delay)
- State fields: `MainPID`, `Connected`, `OrigGW`, `LastAlive`, `TunnelHealthy`
- Main process writes `LastAlive` every 60 s; watchdog detects frozen main process via stale timestamp
- On crash: watchdog reads `Connected` + `OrigGW`, restores default routes

### 7.6. Silent Refresh (Client-Side)

When a data-plane failure is detected (pool eviction or data watchdog), the client attempts a silent path refresh without disconnecting:

```
silentRefresh goroutine (2 min deadline, exp backoff 5 s → 30 s):
  1. ProbeDataPlane(5 s) → BuildPaths()
  2. For each viable control: Login() + NewControlDialer()
  3. Also: UDP relay controls (hole-punch peers for blocked controls)
  4. pool.Swap(freshDialers) → SetTunnelHealthy(true)
  5. If timeout: TriggerReconnect() (full disconnect + reconnect)
```

In WildCat mode: silent refresh only re-auths the primary dialer via the mux (no control probing). Pool always holds exactly one dialer (the mux handle).

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
| SSH credentials in DB | AES-256-GCM (`--db-key`) |
| Unauthorized tunnel access | 404 (not 401/403) — does not reveal service presence |
| Split-tunnel CIDR tampering | Ed25519-signed; client verifies before applying |
| ClientID forgery | Key-string Ed25519-signed by arbiter; client rejects invalid signature |
| Regional routing verification | relay country_code verified via NodeID signature at registration |
| Multi-device key abuse | Same key from two device IDs within 10 s → graduated warning (×3 → account suspended); 1 warning/hour throttle |
| WildCat OAuth token | Delivered obfuscated via signed manifest; deobfuscated only in process memory |
| DNS leaks on macOS | pf anchor blocks DNS on physical NIC while tunnel is up |
| Client telemetry auth | One shared `clientTelemetryKey` (bearer) across app-log/bananameter/log-upload/conn-stats uploads, consolidated 2026-08-13 from 4 separate per-endpoint keys; server accepts either the shared key or a build's old legacy key indefinitely, for already-shipped clients |
| Device log content in transit | Uploaded directly client→arbiter over the client's own live tunnel dialer (`log_upload.go`), not relayed through control/exit — closed a 2026-08-11 incident where a hop-by-hop-only relay path exposed plaintext log content (including a real OAuth token) to any relaying control/exit |
| Legacy Camerlengo command-proxy fallback | Closed 2026-08-12 — `handleClientRequest` used to blind-proxy any unrecognized `.command` straight to Camerlengo using the arbiter's own key; audited that no client relied on it, then removed the fallback entirely |
| Control-node TLS pinning | Client pins each control's cert fingerprint to the value in the signed manifest, not just on first-seen (2026-08-07) |
| IPv6 kill switch | Admin-controlled, broadcast via signed manifest (`ipv6_enabled`); when off, clients force-disable IPv6 and hide the manual toggle — added because no exit in the fleet actually has IPv6 egress, so routing IPv6 into the tunnel just handed apps a dead address |

---

## 11. Bot Controllers

**Not in this repo, and no longer in a single repo either.** Since the
2026-08-04 monorepo trim, the two daemons described below live in different
repos — `botcontroller.py` (Telegram) → **`tunnel_cat_bots`**;
`ratatoskcontroller.py` (Ratatosk group chat) → **`ratatosk/botcontroller/`**.
Both are still deployed as systemd services on the arbiter host
(`62.238.9.103`), giving the "Tunnel Cat" support/mascot persona a presence
in two chat surfaces. Neither talks to Telegram or the in-house Ratatosk
chat protocol directly — both go through **Camerlengo** (reached only via
the `squirrelwisdom.com` HTTPS proxy — never call the `backend.` host
directly; external service, code in `d:\REPO\reforce`, read-only reference —
never edit without explicit permission), which owns the actual bot
connections and message queues.

```
┌──────────────────────┐        ┌──────────────────────┐
│  botcontroller.py     │        │  ratatoskcontroller.py│
│  (Telegram, "TunnelCat│        │  (Ratatosk group chat,│
│   Bot Controller")    │        │   bot email cat@      │
│  service: botcontroller│       │   navlink.net)        │
│  .service              │       │  service: ratatosk-   │
│                        │       │  controller.service    │
└──────────┬─────────────┘        └──────────┬─────────────┘
           │  telegram:getMessages/reply       │  ratatosk:getMessages/reply
           │  (poll loop, JSON over HTTPS)      │  (poll loop, JSON over HTTPS)
           └──────────────────┬─────────────────┘
                               ▼
                  Camerlengo (via squirrelwisdom.com proxy, HTTPS only)
                  TelegramCommands.py / RatatoskCommands.py
                  owns the actual Telegram Bot API / Ratatosk
                  connection and the per-bot message queue
```

Both controllers share the same intent set, implemented independently in
each file (`detect_platform`, `detect_support_intent`, `detect_log_intent`):

- **Email in message → issue key.** Calls arbiter
  `POST /admin/api/users/{email}/issue-key` (`X-Admin-Token`) and replies
  with the raw key string.
- **Platform keyword ("android", "windows", "mac", "ios") → download link.**
  Replies with a **link to the arbiter's public download page**
  (`/download`, `/download/apk`, `/download/dmg`) — the bot does not send
  the binary itself, only a URL the user opens in a browser. iOS has no
  link (not yet released) and gets a "coming soon" reply.
- **"log"/"support" keywords → two-step collection.** Sets a per-user
  pending-action flag, waits for the next message, then emails it to
  `kk@partners.solutions` via SMTP (`mail.partners.solutions`).
- **Anything else → LLM.** Forwards to Camerlengo `ai:resolve` with a fixed
  "Tunnel Cat" system prompt and a rolling per-user conversation history
  (last 40 turns in memory, not persisted across restarts).

Both `telegram:reply` / `ratatosk:reply` (Camerlengo, `reforce/TelegramCommands.py`,
`reforce/RatatoskCommands.py`) and the underlying transport
(`BotMaster.ExternalBot.send_message`, `reforce/BotMaster.py:633`) are
**text-only** — there is currently no Camerlengo command or bot-side method
that attaches a file/document to a chat message in either Telegram or
Ratatosk. Sending an actual binary (not just a link) into the chat would
require adding that capability in Camerlengo first.

The arbiter's `/download*` file routes themselves (`snc-arbiter/download.go`,
`admin_downloads.go`) are public and unauthenticated, with no rate limit —
any of these bots (or a future one) can already resolve "give me the
Android app" to a working URL without any new arbiter-side work.
