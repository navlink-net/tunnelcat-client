// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Foundation

// ── App → Extension ───────────────────────────────────────────────────────────

/// A command sent from the main app to the Network Extension via
/// NETunnelProviderSession.sendProviderMessage(_:responseHandler:).
struct IPCCommand: Codable {
    let cmd: Command
    /// Generic boolean argument (used by setWildcat).
    let value: Bool?
    /// String payload (used by setWildcatToken).
    let token: String?

    enum Command: String, Codable {
        case status      // request current status
        case setWildcat  // value = true/false
        case reconnect   // trigger pool rebuild
        case setWildcatToken  // token = WildCat access token string
    }

    init(cmd: Command, value: Bool? = nil, token: String? = nil) {
        self.cmd = cmd
        self.value = value
        self.token = token
    }

    static func status()                 -> IPCCommand { .init(cmd: .status)                  }
    static func setWildcat(_ on: Bool)   -> IPCCommand { .init(cmd: .setWildcat, value: on)   }
    static func reconnect()              -> IPCCommand { .init(cmd: .reconnect)               }
    static func setWildcatToken(_ t: String)  -> IPCCommand { .init(cmd: .setWildcatToken, token: t)    }
}

// ── Extension → App ───────────────────────────────────────────────────────────

/// Reply sent back through the responseHandler of sendProviderMessage.
struct IPCReply: Codable {
    let state: String      // "idle" | "connecting" | "connected" | "error"
    let error: String?
    /// Cumulative application-payload bytes sent/received by the Go core
    /// process since it started (per-session, resets on extension restart --
    /// see core.TotalBytes on the Go side / TunnelStatus in GoCore.swift).
    /// Defaults to 0 when absent so older extension payloads still decode.
    let bytesSent: Int64
    let bytesRecv: Int64

    init(state: String, error: String? = nil, bytesSent: Int64 = 0, bytesRecv: Int64 = 0) {
        self.state = state
        self.error = error
        self.bytesSent = bytesSent
        self.bytesRecv = bytesRecv
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        state = try c.decode(String.self, forKey: .state)
        error = try c.decodeIfPresent(String.self, forKey: .error)
        bytesSent = try c.decodeIfPresent(Int64.self, forKey: .bytesSent) ?? 0
        bytesRecv = try c.decodeIfPresent(Int64.self, forKey: .bytesRecv) ?? 0
    }
}

// ── Shared defaults keys ──────────────────────────────────────────────────────

enum SharedDefaultsKey {
    static let key           = "snc_key"                 // subscription key string
    static let wildcatOn     = "snc_wildcat"             // Bool
    static let tunnelState   = "snc_tunnel_state"        // last-known state string
    static let wildcatToken       = "snc_wildcat_token"            // String: WildCat access token
    static let wildcatTokenExpiry = "snc_wildcat_token_expiry"     // Double: timeIntervalSince1970
}
