// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import NetworkExtension
import os.log
import Darwin

final class PacketTunnelProvider: NEPacketTunnelProvider {

    private let log = OSLog(subsystem: "net.shortnerdcat.ios.tunnel",
                            category: "PacketTunnelProvider")

    private var statusTimer: DispatchSourceTimer?
    private let shared = UserDefaults(suiteName: "group.net.shortnerdcat")!
    private let monitor = NetworkMonitor()

    // Bridge: Go reads/writes goBridgeFD; we forward packets between it and
    // packetFlow. Go's tun2socks iobased layer treats the fd as a TUN device
    // with a 4-byte prefix it neither inspects on read nor sets on write, so
    // any 4-byte filler works as the prefix.
    private var goBridgeFD: Int32 = -1
    private var swiftBridgeFD: Int32 = -1
    private var bridgeRunning = false

    // MARK: - NEPacketTunnelProvider

    override func startTunnel(options: [String: NSObject]?,
                              completionHandler: @escaping (Error?) -> Void) {
        os_log("startTunnel", log: log, type: .info)

        guard let key = shared.string(forKey: SharedDefaultsKey.key), !key.isEmpty else {
            os_log("no subscription key", log: log, type: .error)
            completionHandler(TunnelError.noKey)
            return
        }

        // Clear any stale terminal state from a previous failure.
        shared.set("connecting", forKey: SharedDefaultsKey.tunnelState)

        // 1. Configure virtual interface: 10.0.0.2/32, DNS 8.8.8.8, MTU 1280.
        //    Routing: default route through tunnel (NEIPv4Route.default()).
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "10.0.0.1")

        let ipv4 = NEIPv4Settings(addresses: ["10.0.0.2"], subnetMasks: ["255.255.255.255"])
        ipv4.includedRoutes = [NEIPv4Route.default()]
        // Home-country bypass routes are added dynamically by the Go BypassManager;
        // we apply them as excludedRoutes after connecting if available.
        settings.ipv4Settings = ipv4

        // IPv6: always captured into the tunnel, unconditionally -- this
        // client previously set no ipv6Settings at all, which meant iOS
        // never claimed IPv6 traffic for the VPN and every IPv6-capable app
        // used the real network interface directly, completely bypassing
        // the tunnel. That's a real IP leak (confirmed live 2026-08-16 on
        // Android, same underlying mistake: not routing IPv6 into the
        // tunnel is not the same as blocking it -- see the matching fixes
        // in routes.go/routes_darwin.go/routes_linux.go/SNCVpnService.kt).
        // No exit in the fleet can dial IPv6 at all (confirmed 2026-08-12,
        // see admin_ipv6.go), so capturing it here just means IPv6
        // connections fail cleanly through the tunnel instead of leaking
        // around it -- safe either way, and simpler than threading the
        // arbiter's live kill-switch state into the extension process.
        let ipv6 = NEIPv6Settings(addresses: ["fd00::2"], networkPrefixLengths: [128])
        ipv6.includedRoutes = [NEIPv6Route.default()]
        settings.ipv6Settings = ipv6

        settings.mtu = NSNumber(value: 1280)
        settings.dnsSettings = NEDNSSettings(servers: ["8.8.8.8", "8.8.4.4"])

        setTunnelNetworkSettings(settings) { [weak self] error in
            guard let self else { return }

            if let error {
                os_log("setTunnelNetworkSettings failed: %{public}@",
                       log: self.log, type: .error, error.localizedDescription)
                completionHandler(error)
                return
            }

            // 2. Create a socketpair bridge: Go gets one end as its "tun fd",
            //    we forward packets between the other end and packetFlow.
            //    This avoids relying on NetworkExtension's private utun fd,
            //    which is no longer reachable from the extension process on iOS 26.
            var fds: [Int32] = [-1, -1]
            guard socketpair(AF_UNIX, SOCK_DGRAM, 0, &fds) == 0 else {
                os_log("socketpair failed: %d", log: self.log, type: .error, errno)
                completionHandler(TunnelError.noTunFD)
                return
            }
            self.goBridgeFD = fds[0]
            self.swiftBridgeFD = fds[1]

            // manual: the main app passes ["manual": true] when the user taps
            // Connect (see VPNManager's startVPNTunnel(options:) call); a nil/
            // absent value means the system triggered this start (on-demand
            // rule, boot, etc.) -- feeds the admin dashboard's connection-stats
            // feature (see core.ConnStatsCollector).
            let manual = (options?["manual"] as? Bool) ?? false
            self.launchGoCore(tunFD: self.goBridgeFD, key: key,
                              manual: manual, completionHandler: completionHandler)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason,
                             completionHandler: @escaping () -> Void) {
        os_log("stopTunnel reason=%d", log: log, type: .info, reason.rawValue)
        stopStatusPolling()
        monitor.stop()
        bridgeRunning = false
        GoCore.stop(manual: reason == .userInitiated)
        if swiftBridgeFD >= 0 { close(swiftBridgeFD); swiftBridgeFD = -1 }
        shared.set("idle", forKey: SharedDefaultsKey.tunnelState)
        // Allow Go goroutines a moment to flush logs before the process exits.
        DispatchQueue.global().asyncAfter(deadline: .now() + 0.3) {
            completionHandler()
        }
    }

    /// Handles JSON commands sent by the main app via NETunnelProviderSession.
    override func handleAppMessage(_ messageData: Data,
                                   completionHandler: ((Data?) -> Void)?) {
        guard let cmd = try? JSONDecoder().decode(IPCCommand.self, from: messageData) else {
            os_log("handleAppMessage: decode failed", log: log, type: .error)
            completionHandler?(nil)
            return
        }
        os_log("handleAppMessage: cmd=%{public}@", log: log, type: .info, cmd.cmd.rawValue)
        switch cmd.cmd {
        case .status:
            completionHandler?(GoCore.statusData())

        case .reconnect:
            os_log("handleAppMessage: reconnect", log: log, type: .info)
            GoCore.reconnect()
            completionHandler?(GoCore.statusData())
        }
    }

    // MARK: - Private

    private func launchGoCore(tunFD: Int32, key: String, manual: Bool,
                              completionHandler: @escaping (Error?) -> Void) {
        os_log("bridge fd=%d", log: log, type: .info, tunFD)

        let container = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.net.shortnerdcat")!
        let logDir  = container.appendingPathComponent("Library/Logs/tunnel")
        let dataDir = container.appendingPathComponent("Library/Application Support/tunnel")
        try? FileManager.default.createDirectory(at: logDir,  withIntermediateDirectories: true)
        try? FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
        SwiftLog.shared.setup(logDir: logDir)
        SwiftLog.shared.log("PacketTunnelProvider: startTunnel key=\(key.prefix(8))…")

        let ok = GoCore.start(key: key, logDir: logDir, dataDir: dataDir,
                              tunFD: tunFD, manual: manual)
        guard ok else {
            os_log("GoCore.start returned false", log: log, type: .error)
            completionHandler(TunnelError.coreStartFailed)
            return
        }

        bridgeRunning = true
        forwardFlowToGo()
        forwardGoToFlow()

        awaitConnected(timeout: 45) { [weak self] connected in
            guard let self else { return }
            if connected {
                os_log("tunnel connected", log: self.log, type: .info)
                self.shared.set("connected", forKey: SharedDefaultsKey.tunnelState)
                self.startStatusPolling()
                self.monitor.start { [weak self] in self?.handleNetworkChange() }
                completionHandler(nil)
            } else {
                let errMsg = GoCore.status().error ?? "unknown error"
                os_log("tunnel failed: %{public}@", log: self.log, type: .error, errMsg)
                self.bridgeRunning = false
                // Not user-initiated -- internal cleanup after a connect attempt
                // that never fully succeeded (Go already counted the connect once
                // the dialer pool was built, so this balances it -- see
                // ConnStatsCollector).
                GoCore.stop(manual: false)
                completionHandler(TunnelError.connectTimeout)
            }
        }
    }

    /// Forwards packets from the system (packetFlow) into the Go core's bridge fd.
    /// Go's iobased reader skips a fixed 4-byte prefix without inspecting it, so
    /// any 4 filler bytes work here.
    private func forwardFlowToGo() {
        packetFlow.readPacketObjects { [weak self] packets in
            guard let self, self.bridgeRunning else { return }
            var prefix = [UInt8](repeating: 0, count: 4)
            for packet in packets {
                var buf = Data(prefix)
                buf.append(packet.data)
                buf.withUnsafeBytes { ptr in
                    _ = send(self.swiftBridgeFD, ptr.baseAddress, ptr.count, 0)
                }
            }
            self.forwardFlowToGo()
        }
    }

    /// Forwards packets written by the Go core (4-byte zero prefix + raw IP packet)
    /// back into packetFlow, determining IPv4/IPv6 from the IP header version nibble.
    private func forwardGoToFlow() {
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            let bufSize = 4 + 1280 + 64
            var buf = [UInt8](repeating: 0, count: bufSize)
            while self.bridgeRunning {
                let n = recv(self.swiftBridgeFD, &buf, bufSize, 0)
                if n <= 0 {
                    if !self.bridgeRunning { return }
                    if n < 0 && errno == EINTR { continue }
                    os_log("forwardGoToFlow: recv ended n=%d errno=%d", log: self.log, type: .info, n, errno)
                    return
                }
                guard n > 4 else { continue }
                let ipPacket = Data(buf[4..<Int(n)])
                let version = ipPacket.first.map { $0 >> 4 } ?? 4
                let family: sa_family_t = version == 6 ? sa_family_t(AF_INET6) : sa_family_t(AF_INET)
                self.packetFlow.writePacketObjects([NEPacket(data: ipPacket, protocolFamily: family)])
            }
        }
    }

    /// Polls GoCore.status() every second until state is "connected" or "error".
    private func awaitConnected(timeout: TimeInterval, completion: @escaping (Bool) -> Void) {
        let deadline = Date().addingTimeInterval(timeout)
        func tick() {
            let s = GoCore.status()
            switch s.state {
            case .connected:
                completion(true)
            case .error:
                completion(false)
            default:
                guard Date() < deadline else { completion(false); return }
                DispatchQueue.global().asyncAfter(deadline: .now() + 1.0, execute: tick)
            }
        }
        tick()
    }

    private func startStatusPolling() {
        let timer = DispatchSource.makeTimerSource(queue: .global())
        timer.schedule(deadline: .now() + 5, repeating: 5)
        timer.setEventHandler { [weak self] in
            guard let self else { return }
            let state = GoCore.status().state.rawValue
            self.shared.set(state, forKey: SharedDefaultsKey.tunnelState)
            if let rss = Self.residentMemoryBytes() {
                SwiftLog.shared.log("mem: rss=\(rss / 1024 / 1024)MB")
            }
        }
        timer.resume()
        statusTimer = timer
    }

    /// Current process resident memory in bytes, via task_info(TASK_VM_INFO).
    /// Used to correlate jetsam kills (no stopTunnel call, no crash report) with
    /// memory growth — the process is silently SIGKILLed when it exceeds the
    /// Network Extension's jetsam memory limit.
    private static func residentMemoryBytes() -> UInt64? {
        var info = task_vm_info_data_t()
        var count = mach_msg_type_number_t(MemoryLayout<task_vm_info_data_t>.size / MemoryLayout<integer_t>.size)
        let result = withUnsafeMutablePointer(to: &info) {
            $0.withMemoryRebound(to: integer_t.self, capacity: Int(count)) {
                task_info(mach_task_self_, task_flavor_t(TASK_VM_INFO), $0, &count)
            }
        }
        guard result == KERN_SUCCESS else { return nil }
        return info.phys_footprint
    }

    private func stopStatusPolling() {
        statusTimer?.cancel()
        statusTimer = nil
    }

    private func handleNetworkChange() {
        os_log("network change — triggering reconnect", log: log, type: .info)
        GoCore.reconnect()
    }
}

// MARK: - Errors

enum TunnelError: LocalizedError {
    case noKey, noTunFD, coreStartFailed, connectTimeout

    var errorDescription: String? {
        switch self {
        case .noKey:           return "No subscription key configured"
        case .noTunFD:         return "Could not obtain tunnel file descriptor"
        case .coreStartFailed: return "Tunnel core failed to initialize"
        case .connectTimeout:  return "Connection timed out — check your key and network"
        }
    }
}
