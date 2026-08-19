// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Foundation

// ── App → Extension ───────────────────────────────────────────────────────────

/// A command sent from the main app to the Network Extension via
/// NETunnelProviderSession.sendProviderMessage(_:responseHandler:).
struct IPCCommand: Codable {
    let cmd: Command

    enum Command: String, Codable {
        case status      // request current status
        case reconnect   // trigger pool rebuild
    }

    init(cmd: Command) {
        self.cmd = cmd
    }

    static func status()    -> IPCCommand { .init(cmd: .status)    }
    static func reconnect() -> IPCCommand { .init(cmd: .reconnect) }
}

// ── Extension → App ───────────────────────────────────────────────────────────

/// Reply sent back through the responseHandler of sendProviderMessage.
struct IPCReply: Codable {
    let state: String      // "idle" | "connecting" | "connected" | "error"
    let error: String?
}

// ── Shared defaults keys ──────────────────────────────────────────────────────

enum SharedDefaultsKey {
    static let key         = "snc_key"          // subscription key string
    static let tunnelState = "snc_tunnel_state" // last-known state string
}
