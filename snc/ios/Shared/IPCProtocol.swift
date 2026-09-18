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
        case getLogUploadPref // fetch this account's log-upload preference (see docs/LOG_UPLOAD_PRIVACY.md)
        case setLogUploadPref // value = true/false; set this account's own preference
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
    static func getLogUploadPref()       -> IPCCommand { .init(cmd: .getLogUploadPref)        }
    static func setLogUploadPref(_ on: Bool) -> IPCCommand { .init(cmd: .setLogUploadPref, value: on) }
}

/// Reply to getLogUploadPref/setLogUploadPref -- mirrors
/// tunnel_cat/snc/core.LogUploadPrefResponse (same field names as the Go
/// JSON, see lib_ios.go's SNCGetLogUploadPref). Deliberately NOT folded
/// into IPCReply: that type is fixed-shape tunnel status, and adding an
/// unrelated account-preference payload to it would make every existing
/// status decode path carry fields it never uses.
struct LogUploadPrefReply: Codable {
    /// True only when the arbiter actually answered. An all-false reply with
    /// ok == false means "couldn't reach the arbiter" (no live tunnel dialer
    /// yet, or the request failed) -- explicit rather than inferred from the
    /// other fields, because an all-false reply with ok == true is also a
    /// real, legitimate state (global kill switch off AND this user opted
    /// out), and the two must not be confused.
    let ok: Bool
    let enabled: Bool        // this account's own preference
    let adminDisabled: Bool  // staff override, if any
    let globalEnabled: Bool  // system-wide kill switch
    let effective: Bool      // what actually happens right now (AND of all three)

    enum CodingKeys: String, CodingKey {
        case ok
        case enabled
        case adminDisabled = "admin_disabled"
        case globalEnabled = "global_enabled"
        case effective
    }
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
    static let logUploadCache = "snc_log_upload_cache"   // Bool: last-known log-upload preference (see docs/LOG_UPLOAD_PRIVACY.md); absent = never fetched, treated as true
}
