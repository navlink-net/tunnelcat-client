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
                      tunFD: Int32, manual: Bool) -> Bool {
        key.withCString { cKey in
            logDir.path.withCString { cLog in
                dataDir.path.withCString { cData in
                    SNCStart(cKey, cLog, cData, tunFD, manual ? 1 : 0) == 0
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

    /// Triggers a pool rebuild (call on network interface change).
    static func reconnect() {
        SNCReconnect()
    }
}

// MARK: - Status model

struct TunnelStatus: Codable {
    let state: TunnelState
    let error: String?

    init(state: TunnelState, error: String? = nil) {
        self.state = state
        self.error = error
    }
}

enum TunnelState: String, Codable {
    case idle, connecting, connected, error
}
