// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Foundation

/// Typed Swift wrapper around the Go static library C exports.
///
/// All functions are safe to call from any thread. The Go runtime manages
/// its own goroutine scheduler independently of the Swift/ObjC thread pool.
enum GoCore {

    // MARK: - Lifecycle

    /// Starts the tunnel goroutines.
    /// - Parameter manual: true for a genuine user-initiated connect, false for
    ///   an on-demand/system-triggered start -- feeds the admin dashboard's
    ///   connection-stats feature (see core.ConnStatsCollector).
    /// - Returns: `true` on success; `false` if the key is invalid or logging failed.
    static func start(key: String, logDir: URL, dataDir: URL,
                      tunFD: Int32, wildcatMode: Bool, manual: Bool) -> Bool {
        key.withCString { cKey in
            logDir.path.withCString { cLog in
                dataDir.path.withCString { cData in
                    SNCStart(cKey, cLog, cData, tunFD, wildcatMode ? 1 : 0, manual ? 1 : 0) == 0
                }
            }
        }
    }

    /// Signals the tunnel goroutines to stop. Returns immediately; shutdown is asynchronous.
    /// - Parameter manual: true for a genuine user-initiated disconnect
    ///   (NEProviderStopReason.userInitiated), false otherwise.
    static func stop(manual: Bool) {
        SNCStop(manual ? 1 : 0)
    }

    // MARK: - Status

    /// Returns the current tunnel status decoded from Go's JSON.
    static func status() -> TunnelStatus {
        guard let cStr = SNCGetStatus() else { return .init(state: .idle) }
        defer { SNCFreeString(cStr) }
        let data = Data(String(cString: cStr).utf8)
        return (try? JSONDecoder().decode(TunnelStatus.self, from: data)) ?? .init(state: .idle)
    }

    /// Returns the raw JSON status bytes for passing back through handleAppMessage.
    static func statusData() -> Data? {
        guard let cStr = SNCGetStatus() else { return nil }
        defer { SNCFreeString(cStr) }
        return Data(String(cString: cStr).utf8)
    }

    // MARK: - Control

    /// Enables or disables WildCat mode. Can be called while the tunnel is running.
    static func setWildcat(_ enabled: Bool) {
        SNCSetWildcat(enabled ? 1 : 0)
    }

    /// Passes a fresh access token to the WildCat credential manager.
    /// Must be called before starting in WildCat mode, and again whenever the
    /// token is refreshed.
    static func setWildcatToken(_ token: String) {
        token.withCString { SNCSetWildcatToken($0) }
    }

    /// Triggers a pool rebuild (call on network interface change).
    static func reconnect() {
        SNCReconnect()
    }
}

// MARK: - Status model

struct TunnelStatus: Codable {
    let state: TunnelState
    let error: String?
    /// Cumulative application-payload bytes sent/received by the Go core
    /// process since it started (per-session, resets on extension restart --
    /// see core.TotalBytes on the Go side). Absent/undecodable on older
    /// payloads decodes to 0 via the defaulted init below.
    let bytesSent: Int64
    let bytesRecv: Int64

    init(state: TunnelState, error: String? = nil, bytesSent: Int64 = 0, bytesRecv: Int64 = 0) {
        self.state = state
        self.error = error
        self.bytesSent = bytesSent
        self.bytesRecv = bytesRecv
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        state = try c.decode(TunnelState.self, forKey: .state)
        error = try c.decodeIfPresent(String.self, forKey: .error)
        bytesSent = try c.decodeIfPresent(Int64.self, forKey: .bytesSent) ?? 0
        bytesRecv = try c.decodeIfPresent(Int64.self, forKey: .bytesRecv) ?? 0
    }
}

enum TunnelState: String, Codable {
    case idle, connecting, connected, error
}
