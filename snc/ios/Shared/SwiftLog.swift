// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Foundation
import os.log

// Binlog is a Swift implementation of tunnel_cat/binlog's wire format --
// must match tunnel_cat/binlog/binlog.go byte-for-byte (see that file's doc
// comment for the format itself and the reasoning behind it). Kept minimal
// and dependency-free, mirroring binlog's own "small, standalone" design.
// Swift's fixed-width UInt16 arithmetic truncates on shift/xor the same way
// Go's uint16 does, so this ports directly without needing manual masking
// the way the Kotlin (Int-based) port did.
//
// 2026-08-17: clients must never store or transmit plain-text log content
// (see rotatingWriter in tunnel_cat/snc/core/log.go for the Go-side
// equivalent of this same change, and its doc comment for the incident that
// prompted it -- an earlier "convert clients to binary logging" pass only
// ever reached the Go core's old in-memory upload ring buffer, never this
// file, which kept writing plain text the whole time).
enum Binlog {
    private static let startMagic: UInt16 = 0xC5A7
    private static let endMagic: UInt16 = 0x7A5C

    // CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF) -- must match
    // tunnel_cat/binlog/binlog.go's crc16Table exactly.
    private static let crcTable: [UInt16] = {
        var table = [UInt16](repeating: 0, count: 256)
        for i in 0..<256 {
            var crc = UInt16(i) << 8
            for _ in 0..<8 {
                if crc & 0x8000 != 0 {
                    crc = (crc << 1) ^ 0x1021
                } else {
                    crc = crc << 1
                }
            }
            table[i] = crc
        }
        return table
    }()

    private static func crc16(_ data: [UInt8]) -> UInt16 {
        var crc: UInt16 = 0xFFFF
        for b in data {
            let idx = Int((crc >> 8) ^ UInt16(b))
            crc = (crc << 8) ^ crcTable[idx]
        }
        return crc
    }

    private static func putUvarint(_ out: inout [UInt8], _ value: Int) {
        var v = UInt64(value)
        while v >= 0x80 {
            out.append(UInt8((v & 0x7F) | 0x80))
            v >>= 7
        }
        out.append(UInt8(v))
    }

    private static func putU16BE(_ out: inout [UInt8], _ v: UInt16) {
        out.append(UInt8(v >> 8))
        out.append(UInt8(v & 0xFF))
    }

    /// Frames one record (tag + payload) exactly as binlog.AppendRecord does.
    static func appendRecord(tag: UInt8, payload: [UInt8]) -> [UInt8] {
        var body: [UInt8] = [tag]
        putUvarint(&body, payload.count)
        body.append(contentsOf: payload)
        let crc = crc16(body)

        var out: [UInt8] = []
        out.reserveCapacity(2 + body.count + 2 + 2)
        putU16BE(&out, startMagic)
        out.append(contentsOf: body)
        putU16BE(&out, crc)
        putU16BE(&out, endMagic)
        return out
    }
}

// SwiftLog writes timestamped lifecycle events to snc_lifecycle.log in the
// shared app group container, binlog-framed under TagSystem (matching Go's
// own subsystemTags mapping of process/lifecycle housekeeping lines) so it
// decodes with the exact same tools/decode_snc_log.py used for the Go
// core's own log. Mirrors Android KotlinLog so all events across Swift
// (UI/VPN) and Go (tunnel core) can be correlated in one place once decoded.
final class SwiftLog {
    static let shared = SwiftLog()
    private init() {}

    private static let tagSystem: UInt8 = 7 // binlog.TagSystem -- keep in sync with tunnel_cat/binlog/binlog.go

    private let osLog = Logger(subsystem: "net.shortnerdcat.client", category: "SwiftLog")
    private let lock  = NSLock()
    private var logFile: URL?

    private let dateFmt: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "yyyy/MM/dd HH:mm:ss"
        f.locale     = Locale(identifier: "en_US_POSIX")
        return f
    }()

    /// Must be called once at app startup with the shared container log directory.
    func setup(logDir: URL) {
        try? FileManager.default.createDirectory(at: logDir, withIntermediateDirectories: true)
        lock.lock()
        logFile = logDir.appendingPathComponent("snc_lifecycle.log")
        lock.unlock()
        log("SwiftLog: initialized, pid=\(ProcessInfo.processInfo.processIdentifier)")
    }

    func log(_ msg: String) {
        osLog.debug("\(msg, privacy: .public)")
        lock.lock()
        guard let url = logFile else { lock.unlock(); return }
        lock.unlock()
        let line = "\(dateFmt.string(from: Date())) [sl] \(msg)\n"
        guard let payload = line.data(using: .utf8) else { return }
        let framed = Binlog.appendRecord(tag: Self.tagSystem, payload: [UInt8](payload))
        let data = Data(framed)
        lock.lock()
        defer { lock.unlock() }
        if FileManager.default.fileExists(atPath: url.path) {
            if let handle = try? FileHandle(forWritingTo: url) {
                handle.seekToEndOfFile()
                handle.write(data)
                try? handle.close()
            }
        } else {
            try? data.write(to: url, options: .atomic)
        }
    }
}

// Convenience global
func SLog(_ msg: String) { SwiftLog.shared.log(msg) }
