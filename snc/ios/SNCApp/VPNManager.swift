// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import NetworkExtension
import Foundation
import os.log

/// Manages the NETunnelProviderManager lifecycle and provides a typed IPC
/// channel to the SNCTunnel Network Extension.
final class VPNManager {

    static let shared = VPNManager()

    private let log = Logger(subsystem: "net.shortnerdcat.client", category: "VPNManager")
    private var manager: NETunnelProviderManager?
    private let shared = UserDefaults(suiteName: "group.net.shortnerdcat")!
    var isLoaded: Bool { manager != nil }

    // MARK: - Setup

    /// Loads an existing VPN configuration or creates a new one.
    func load(completion: @escaping (Error?) -> Void) {
        NETunnelProviderManager.loadAllFromPreferences { [weak self] managers, error in
            guard let self else { return }
            if let error {
                completion(error)
                return
            }
            // Reuse existing config or create fresh.
            let m = managers?.first ?? NETunnelProviderManager()
            let proto = (m.protocolConfiguration as? NETunnelProviderProtocol)
                        ?? NETunnelProviderProtocol()
            proto.providerBundleIdentifier = "net.shortnerdcat.client.tunnel"
            proto.serverAddress            = "ShortNerdCat"
            m.protocolConfiguration        = proto
            m.localizedDescription         = "ShortNerdCat"
            m.isEnabled                    = true
            self.log.info("load: saving VPN config (bundleID=net.shortnerdcat.client.tunnel)")
            m.saveToPreferences { saveErr in
                if let saveErr {
                    self.log.error("load: saveToPreferences failed: \(saveErr.localizedDescription, privacy: .public)")
                    completion(saveErr)
                    return
                }
                self.log.info("load: VPN config saved OK")
                self.manager = m
                completion(nil)
            }
        }
    }

    // MARK: - Connect / Disconnect

    func connect(completion: @escaping (Error?) -> Void) {
        guard let m = manager else {
            log.error("connect: manager not loaded")
            completion(VPNError.notLoaded)
            return
        }
        log.info("connect: calling startVPNTunnel, current status=\(m.connection.status.rawValue)")
        do {
            // This is the app's only call site for startVPNTunnel -- always a
            // genuine user-initiated connect (on-demand/system-triggered starts
            // invoke the extension's startTunnel directly, without the app
            // calling this method at all, and arrive with nil options). Feeds
            // the admin dashboard's connection-stats feature (see
            // core.ConnStatsCollector.IncConnect on the Go side).
            try m.connection.startVPNTunnel(options: ["manual": true as NSNumber])
            log.info("connect: startVPNTunnel OK")
            completion(nil)
        } catch {
            log.error("connect: startVPNTunnel failed: \(error.localizedDescription, privacy: .public)")
            completion(error)
        }
    }

    func disconnect() {
        log.info("disconnect: stopVPNTunnel")
        manager?.connection.stopVPNTunnel()
    }

    // MARK: - Status

    var connectionStatus: NEVPNStatus {
        manager?.connection.status ?? .invalid
    }

    var tunnelState: String {
        shared.string(forKey: SharedDefaultsKey.tunnelState) ?? "idle"
    }

    // MARK: - IPC

    /// Sends a command to the extension and decodes the reply.
    func send(_ cmd: IPCCommand, completion: ((IPCReply?) -> Void)? = nil) {
        guard let session = manager?.connection as? NETunnelProviderSession,
              let data = try? JSONEncoder().encode(cmd) else {
            log.error("send: no active session or encode failed for cmd=\(cmd.cmd.rawValue, privacy: .public)")
            completion?(nil)
            return
        }
        do {
            try session.sendProviderMessage(data) { replyData in
                guard let d = replyData,
                      let reply = try? JSONDecoder().decode(IPCReply.self, from: d) else {
                    self.log.error("send: no reply or decode failed for cmd=\(cmd.cmd.rawValue, privacy: .public)")
                    completion?(nil)
                    return
                }
                completion?(reply)
            }
        } catch {
            log.error("send: sendProviderMessage failed: \(error.localizedDescription, privacy: .public)")
            completion?(nil)
        }
    }

    /// Convenience: fetch current tunnel status from the extension.
    func fetchStatus(completion: @escaping (IPCReply?) -> Void) {
        send(.status(), completion: completion)
    }

    // MARK: - Settings

    func saveKey(_ key: String) {
        shared.set(key, forKey: SharedDefaultsKey.key)
    }

    func loadKey() -> String? {
        shared.string(forKey: SharedDefaultsKey.key)
    }

}

enum VPNError: LocalizedError {
    case notLoaded
    var errorDescription: String? { "VPN configuration not loaded yet" }
}
