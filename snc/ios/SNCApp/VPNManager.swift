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

    // MARK: - Log-upload preference (see docs/LOG_UPLOAD_PRIVACY.md)

    /// Like send(_:completion:) but decodes a LogUploadPrefReply -- that reply
    /// is an unrelated account-preference payload, not tunnel status, so it
    /// deliberately doesn't share IPCReply's fixed shape.
    private func sendLogUploadPref(_ cmd: IPCCommand, completion: @escaping (LogUploadPrefReply?) -> Void) {
        guard let session = manager?.connection as? NETunnelProviderSession,
              let data = try? JSONEncoder().encode(cmd) else {
            log.error("sendLogUploadPref: no active session or encode failed for cmd=\(cmd.cmd.rawValue, privacy: .public)")
            completion(nil)
            return
        }
        do {
            try session.sendProviderMessage(data) { replyData in
                guard let d = replyData,
                      let reply = try? JSONDecoder().decode(LogUploadPrefReply.self, from: d) else {
                    self.log.error("sendLogUploadPref: no reply or decode failed for cmd=\(cmd.cmd.rawValue, privacy: .public)")
                    completion(nil)
                    return
                }
                completion(reply)
            }
        } catch {
            log.error("sendLogUploadPref: sendProviderMessage failed: \(error.localizedDescription, privacy: .public)")
            completion(nil)
        }
    }

    /// Fetches this account's current log-upload preference from the arbiter
    /// via the extension. nil when there's no active tunnel session to ask
    /// over; an all-false reply means the extension couldn't reach the
    /// arbiter yet.
    func fetchLogUploadPref(completion: @escaping (LogUploadPrefReply?) -> Void) {
        sendLogUploadPref(.getLogUploadPref(), completion: completion)
    }

    /// Sets this account's own log-upload preference (see
    /// docs/LOG_UPLOAD_PRIVACY.md) via the extension and returns the
    /// resulting state.
    func setLogUploadPref(_ on: Bool, completion: @escaping (LogUploadPrefReply?) -> Void) {
        sendLogUploadPref(.setLogUploadPref(on), completion: completion)
    }

    // MARK: - Settings

    func saveKey(_ key: String) {
        shared.set(key, forKey: SharedDefaultsKey.key)
    }

    func loadKey() -> String? {
        shared.string(forKey: SharedDefaultsKey.key)
    }

    func setWildcat(_ on: Bool) {
        log.info("setWildcat: \(on), status=\(self.connectionStatus.rawValue)")
        shared.set(on, forKey: SharedDefaultsKey.wildcatOn)
        if connectionStatus == .connected {
            send(.setWildcat(on))
        }
    }

    var wildcatEnabled: Bool {
        shared.bool(forKey: SharedDefaultsKey.wildcatOn)
    }

    /// Last-known log-upload preference (see docs/LOG_UPLOAD_PRIVACY.md), so
    /// the menu can show something immediately without a network round-trip.
    /// Defaults to true when never fetched, matching the server-side default
    /// (nothing changes for anyone until someone actively flips a switch).
    /// Kept honest by fetchLogUploadPref, called once per connect.
    var logUploadCached: Bool {
        get { shared.object(forKey: SharedDefaultsKey.logUploadCache) as? Bool ?? true }
        set { shared.set(newValue, forKey: SharedDefaultsKey.logUploadCache) }
    }

    // MARK: - WildCat Token

    /// Stores the WildCat access token and pushes it to the NE extension if connected.
    /// Token expiry is set conservatively to 18 min.
    func setWildcatToken(_ token: String) {
        log.info("setWildcatToken: len=\(token.count), status=\(self.connectionStatus.rawValue)")
        let expiry = Date().addingTimeInterval(18 * 60).timeIntervalSince1970
        shared.set(token, forKey: SharedDefaultsKey.wildcatToken)
        shared.set(expiry, forKey: SharedDefaultsKey.wildcatTokenExpiry)
        if connectionStatus == .connected || connectionStatus == .connecting {
            send(.setWildcatToken(token))
        }
    }

    /// Returns the cached WildCat access token if one exists and hasn't expired, nil otherwise.
    func storedWildcatToken() -> String? {
        guard let token = shared.string(forKey: SharedDefaultsKey.wildcatToken), !token.isEmpty else { return nil }
        let expiry = shared.double(forKey: SharedDefaultsKey.wildcatTokenExpiry)
        guard expiry > 0, Date().timeIntervalSince1970 < expiry else { return nil }
        return token
    }
}

enum VPNError: LocalizedError {
    case notLoaded
    var errorDescription: String? { L.t("vpn.notLoaded") }
}
