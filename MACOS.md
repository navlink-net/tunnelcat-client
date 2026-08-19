# macOS Client — Implementation

ShortNerdCat macOS client is a system-tray VPN app for macOS 12+. It is
functionally equivalent to the Windows client: same tunnel architecture, same
transport modes (normal TCP, VLESS), same discovery and relay subsystems.

---

## 1. Package Layout

```
shortnerdcat/
  win-client/core/
    tun_darwin.go          ← TUNBridge (utun + tun2socks)
    updater_darwin.go      ← OTA binary-replace updater

  macos-client/
    macos/                 ← platform layer (mirrors win-client/windows/)
      routes_darwin.go     ← split-tunnel route management via /sbin/route
      dns_darwin.go        ← system DNS via networksetup
      autostart_darwin.go  ← LaunchAgent plist registration
      tray_darwin.go       ← system tray (getlantern/systray)
      power_darwin.go      ← sleep/wake events via IOKit (CGo)
      watchdog_darwin.go   ← crash-recovery watchdog subprocess
      notification_darwin.go ← osascript notifications
      key_darwin.go        ← activation key persistence (0600 file)
      firewall_darwin.go   ← pf anchor DNS-leak prevention
    cmd/shortnerdcat/
      main_darwin.go       ← app entry point, all tunnel logic
      watchdog_darwin.go   ← watchdog mode implementation
      geo_darwin.go        ← GPS / CoreLocation country detection

    bundle/
      Info.plist
      entitlements.plist
      ShortNerdCat.sh      ← CFBundleExecutable privilege launcher

  build.sh                 ← macOS build: compile → lipo → .app → .dmg
```

---

## 2. Traffic Path

```
Browser
  │
  ▼  (via system resolver → 1.1.1.1 → TUN)
TUNBridge (utun0)          tun_darwin.go
  │  tun2socks SOCKS5 forwarder
  ▼
SOCKS5Server  127.0.0.1:random
  │
  └── DialerPool → TunnelDialer → control:443 → exit → target
```

DNS always resolves via 1.1.1.1 which is routed through the TUN (DoH / DNS-in-tunnel).
Control IPs have bypass host routes via the original gateway.

---

## 3. Core Platform Stubs (`win-client/core/`)

### `tun_darwin.go` — TUNBridge

- Creates a `utun` device using the tun2socks / sing-box engine.
- TUN address: `198.18.0.1/15` (same as Windows).
- Forwards all TUN traffic to the local SOCKS5 listener.
- `Start()` / `Stop()` match the Windows `TUNBridge` interface exactly.
- No PnP settle delay needed (utun creation is synchronous).

### `updater_darwin.go` — OTA Updater

- Downloads new binary to a temp file next to the running binary.
- Atomically renames old → new, then re-execs via `syscall.Exec`.
- No admin required when the `.app` bundle is in `~/Applications`.

---

## 4. Platform Package (`macos-client/macos/`)

### `routes_darwin.go` — RouteManager

Installs split-tunnel routes using `/sbin/route` and `netstat`.

**Fields captured during `Apply()`:**
- `origGW` — original default gateway (from `netstat -rn -f inet`)
- `physIface` — outbound interface name, e.g. `en0` (column 4 of netstat default line)
- `localAddr` — physical outbound IP (UDP dial trick to 8.8.8.8:80 before routes change)

**Route set installed:**
```
/sbin/route add -host <server_ip> <origGW>    # bypass for control IPs (from Prepare)
/sbin/route add -host 1.1.1.1  <origGW>       # DNS bypass (skipped in DoH mode)
/sbin/route add -net 0/1       <tunGW>         # split-tunnel: lower half
/sbin/route add -net 128/1     <tunGW>         # split-tunnel: upper half
/sbin/route add -inet6 ::/1    <tunIPv6Addr>   # IPv6
/sbin/route add -inet6 8000::/1 <tunIPv6Addr>  # IPv6
```

**`AddBypass(ip)`** — adds a host route for `ip` via `origGW` after Apply; used for
additional control IPs discovered post-connect.

**Exported getters:** `OrigGW()`, `PhysIface()`, `LocalAddr()`.

**`CleanupSplitRoutes(origGW)`** — standalone cleanup called by the watchdog after
an unclean exit (no live `RouteManager` available).

### `dns_darwin.go` — DNSManager

- Detects the active network service by matching `route get 8.8.8.8` interface output
  against `networksetup -listallnetworkservices`.
- `Apply()`: `networksetup -setdnsservers <service> 1.1.1.1`
- `Restore()`: `networksetup -setdnsservers <service> empty`

### `autostart_darwin.go`

- Writes `~/Library/LaunchAgents/net.navlink.shortnerdcat.plist`.
- `RunAtLoad: true`, `KeepAlive: false`, entry point is the `.app` bundle launcher.
- `RegisterAutostart()` / `UnregisterAutostart()` / `IsAutostartRegistered()`.

### `tray_darwin.go` — TrayApp

Full feature parity with the Windows tray. Key behaviours:

- **Icons**: idle / connecting / connected / error — PNG loaded from embedded assets.
- **`connectedIcon()`**: picks VLESS icon > standard connected icon.
- **`doConnect(autoReconnect)`**: 90 s deadline, panic recovery via `runtime/debug`,
  starts `tickElapsed` (tooltip `Connected HH:MM:SS`) and `runWatchdog`.
- **`runWatchdog`**: polls `core.TunnelMonitor.IsStuck()` every 2 s, fires reconnect.
- **`scheduleRetry`**: exponential back-off 30 s → 5 min.
- **`showAbout(version)`**: `osascript display dialog` (no splash window on macOS).
- **Auth warning / login error** states with icon changes.
- **Regions**: submenu with radio-button checkboxes.
- **DoH toggle**: saves to settings; takes effect on next connect (route-level, not runtime).
- **Quit**: blinks connecting↔idle, waits up to 30 s for disconnect, calls
  `snmac.SignalCleanShutdown()`.

### `power_darwin.go`

- CGo: wraps `IORegisterForSystemPower` / `IOAllowPowerChange` via a C helper.
- Fires `onWake` callback in a new goroutine on each system wake from sleep.
- Main calls `snmac.WatchPowerEvents(...)` once before the tray starts.

### `watchdog_darwin.go`

- Writes a PID file; re-execs the binary with `--watchdog` on crash.
- Watchdog process reads the `WatchdogState` JSON and calls `CleanupSplitRoutes`
  if the main process was connected when it died.

### `notification_darwin.go`

```go
ShowNotification(title, body string)   // osascript display notification
ShowNotifications(msgs []string)       // one notification per message
```

### `key_darwin.go`

- `SaveKey(appDataDir, keyStr)` → `appDataDir/key.dat` (mode 0600).
- `LoadKey(appDataDir)` → trimmed string.

### `firewall_darwin.go` — FirewallManager

DNS-leak prevention via pf anchor. Acts as a safety net if routing table changes fail.

**`Apply(physIface)`:**
1. Reads current in-kernel main ruleset via `pfctl -s rules`.
2. Prepends `anchor "shortnerdcat"` declaration and reloads with `pfctl -f -`
   (preserves existing rules, adds anchor to evaluation chain).
3. Loads DNS-block rules into the anchor via `pfctl -a shortnerdcat -f -`:
   ```
   block out quick on <physIface> proto { tcp udp } to any port 53
   ```
4. Enables pf with `pfctl -e` (no-op if already enabled).

**`Remove()`:** `pfctl -a shortnerdcat -F all` — flushes anchor rules on disconnect.

Idempotent: `Apply` is a no-op if already applied; `Remove` is a no-op if not applied.

---

## 5. Entry Point (`macos-client/cmd/shortnerdcat/main_darwin.go`)

### Startup sequence

1. Parse `--watchdog` flag → run watchdog supervisor.
2. Parse `--user-home` flag → fix `HOME` / `APPDATA` when running under osascript.
3. Single-instance lockfile: `~/.shortnerdcat/app.lock`.
4. Set up log file: `~/.shortnerdcat/logs/`.
5. Apply pending OTA update if present (`core.ApplyPendingUpdate`).
6. Load or generate device ID and node ID.
7. Start DHT node (kept alive across connect/disconnect).
8. Start geo detection goroutine (CoreLocation GPS or timezone fallback).
9. Load saved key; attempt silent auto-auth.
10. Start discovery (`initDiscovery` + `wireDHT`).
11. `snmac.WatchPowerEvents` — register wake callback.
12. Build `TrayApp` with all callbacks.
13. `trayApp.Run()` — blocks until Quit.

### Data directory

`~/.shortnerdcat/` (via `APPDATA` env var set in `init()`).

Stored files: `key.dat`, `device_id`, `node_id`, `settings.json`, `peers.json`,
`relays.json`, `cidr.json`, `notif_seen.json`, `country.txt`,
`watchdog.json`.

Logs: `~/.shortnerdcat/logs/`.

---

## 6. Transport Modes

### Normal (TCP)

- `DialerPool` with up to 5 qualifying control nodes selected by RTT.
- Pool refill every 15 s; RTT-based promotion every 10 s.
- Silent path refresh on data-plane failure (up to 2 min, exponential back-off).
- Data-plane watchdog fires silent refresh if no data for 30 s.

### VLESS

Not present on the current macOS client (no `sing-box`/VLESS references anywhere in
`main_darwin.go` or `tray_darwin.go`) — matches Windows, which also has no VLESS mode.

---

## 7. Always-On Subsystems

These start at connect and stop at disconnect regardless of transport mode.

| Subsystem | Start | Stop |
|---|---|---|
| DHT node | app startup | app exit |
| Discovery | first connect | never (discOnce) |
| BypassManager | onConnect | onDisconnect |
| DecoyManager | onConnect | onDisconnect |
| RelayClient (public IP) | onConnect goroutine | onDisconnect |
| NATRelayClient (yamux) | onConnect goroutine (fallback) | onDisconnect |
| LogUploader | onConnect | onDisconnect |
| CountryChecker | onConnect goroutine | onDisconnect (close chan) |
| DHT merger goroutine | onConnect (if DHT node) | onDisconnect (close chan) |
| pf FirewallManager | onConnect (after routes) | onDisconnect |

### DHT node

Initialized once at startup from `node_id`. Bootstrapped with relay addresses
fetched at each connect. Every 5 min the DHT relay registry is merged into the
router and paths are rebuilt.

### Relay clients

After routes are up, a background goroutine tries:
1. Public-IP relay (`NewRelayClient`) if `myIP == localAddr` (not behind NAT).
2. NAT relay (`NewNATRelayClient`) with yamux reverse-tunnel as fallback.

---

## 8. DoH

When DoH is enabled (default):
- `routes.DisableDNSBypass()` prevents the 1.1.1.1 host route from being installed.
- System DNS is set to 1.1.1.1 by `DNSManager`.
- Queries to 1.1.1.1 flow through the TUN → SOCKS5 → exit node → Cloudflare HTTPS.
- pf firewall additionally blocks port 53 on the physical NIC as a safety net.

DoH toggle in the tray saves to settings; takes effect on next reconnect (changing
the DNS bypass route requires `routes.Remove()` + `routes.Apply()`).

---

## 9. App Bundle & Build

### Bundle structure

```
ShortNerdCat.app/
  Contents/
    Info.plist              LSUIElement=true (tray-only, no Dock icon)
    MacOS/
      ShortNerdCat          universal binary (lipo amd64 + arm64)
      ShortNerdCat.sh       CFBundleExecutable; privilege launcher
    Resources/
      AppIcon.icns
      snc_idle.png / snc_connected.png / snc_connecting.png / snc_error.png
      snc_vless.png
```

`CFBundleIdentifier`: `net.navlink.shortnerdcat`  
`LSMinimumSystemVersion`: `12.0`

### Build script (`build.sh`)

```
CGO_ENABLED=1 GOARCH=amd64 GOOS=darwin go build -ldflags="-X shortnerdcat/snc/win/core.Version=..." ./snc/mac/cmd/shortnerdcat
CGO_ENABLED=1 GOARCH=arm64 GOOS=darwin go build ...
lipo -create amd64_bin arm64_bin -output ShortNerdCat
# assemble .app bundle
# codesign --deep --force --timestamp --options runtime --entitlements entitlements.plist
# hdiutil → .dmg → xcrun notarytool submit → xcrun stapler staple
```

**Must build on macOS** — CGo is required for `power_darwin.go` (IOKit) and
indirectly for tun2socks/gvisor.

Apple Developer cert: `Developer ID Application: <Your Name> (<TEAMID>)` — set via the
`CERT_ID` / `APPLE_TEAM_ID` env vars, see `build.sh`.
Notarization keychain profile: `notarization-profile`.

---

## 10. Known Limitations

| Item | Status |
|---|---|
| DoH runtime toggle | Not wired; requires reconnect to take effect |
| pf firewall requires root | `pfctl` needs root; app runs as root (utun requirement) |
