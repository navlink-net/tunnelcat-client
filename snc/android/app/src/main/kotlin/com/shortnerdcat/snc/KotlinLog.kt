// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.util.Log
import java.io.ByteArrayOutputStream
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

// Binlog is a Kotlin implementation of tunnel_cat/binlog's wire format --
// must match tunnel_cat/binlog/binlog.go byte-for-byte (see that file's doc
// comment for the format itself and the reasoning behind it). Kept minimal
// and dependency-free, mirroring binlog's own "small, standalone" design.
//
// 2026-08-17: clients must never store or transmit plain-text log content
// (see rotatingWriter in tunnel_cat/snc/core/log.go for the Go-side
// equivalent of this same change, and its doc comment for the incident that
// prompted it -- an earlier "convert clients to binary logging" pass only
// ever reached the Go core's old in-memory upload ring buffer, never this
// file, which kept writing plain text via appendText the whole time).
internal object Binlog {
    private const val START_MAGIC = 0xC5A7
    private const val END_MAGIC = 0x7A5C

    // CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF) -- must match
    // tunnel_cat/binlog/binlog.go's crc16Table exactly.
    private val crcTable: IntArray = IntArray(256).also { table ->
        for (i in 0 until 256) {
            var crc = i shl 8
            repeat(8) {
                // Mask to 16 bits after EVERY shift, not just once at the end --
                // Go's crc is a uint16 for this whole loop, so each of the 8
                // inner shifts truncates before the next iteration's 0x8000
                // check. Skipping intermediate masking here would let bit 16+
                // survive across iterations and desync from Go's table.
                crc = if (crc and 0x8000 != 0) ((crc shl 1) xor 0x1021) and 0xFFFF else (crc shl 1) and 0xFFFF
            }
            table[i] = crc
        }
    }

    private fun crc16(data: ByteArray, len: Int): Int {
        var crc = 0xFFFF
        for (i in 0 until len) {
            val b = data[i].toInt() and 0xFF
            crc = ((crc shl 8) xor crcTable[(crc ushr 8) xor b]) and 0xFFFF
        }
        return crc
    }

    private fun putUvarint(out: ByteArrayOutputStream, value: Int) {
        var v = value.toLong() and 0xFFFFFFFFL
        while (v >= 0x80) {
            out.write(((v and 0x7F) or 0x80).toInt())
            v = v ushr 7
        }
        out.write(v.toInt())
    }

    private fun putU16BE(out: ByteArrayOutputStream, v: Int) {
        out.write((v ushr 8) and 0xFF)
        out.write(v and 0xFF)
    }

    /** Frames one record (tag + payload) exactly as binlog.AppendRecord does. */
    fun appendRecord(tag: Int, payload: ByteArray): ByteArray {
        val body = ByteArrayOutputStream(1 + 5 + payload.size)
        body.write(tag and 0xFF)
        putUvarint(body, payload.size)
        body.write(payload)
        val bodyBytes = body.toByteArray()
        val crc = crc16(bodyBytes, bodyBytes.size)

        val out = ByteArrayOutputStream(2 + bodyBytes.size + 2 + 2)
        putU16BE(out, START_MAGIC)
        out.write(bodyBytes)
        putU16BE(out, crc)
        putU16BE(out, END_MAGIC)
        return out.toByteArray()
    }
}

// KotlinLog writes Kotlin-side lifecycle events to snc_lifecycle.log in the Go log
// directory, binlog-framed under TagSystem (matching Go's own subsystemTags mapping
// of process/lifecycle housekeeping lines) so it decodes with the exact same
// tools/decode_snc_log.py used for the Go core's own log. The file is included in
// the log ZIP automatically (any *.log in that directory is zipped). Format matches
// Go logs so all events are easy to correlate once decoded.
object KotlinLog {
    private const val TAG = "KotlinLog"
    private const val TAG_SYSTEM = 7 // binlog.TagSystem -- keep in sync with tunnel_cat/binlog/binlog.go
    private val dateFmt = SimpleDateFormat("yyyy/MM/dd HH:mm:ss", Locale.US)

    @Volatile private var logDir: File? = null

    // init is idempotent — subsequent calls reuse the same directory.
    fun init(dir: File) {
        dir.mkdirs()
        logDir = dir
    }

    // 2026-08-06 incident: appendText failures used to be swallowed silently, so a
    // write outage (storage issue, permission revoked, etc.) left no trace anywhere
    // -- snc_lifecycle.log just stopped mid-stream with nothing in logcat either.
    // Logging the failure to Logcat doesn't fix the underlying write outage, but at
    // least it survives in `adb logcat`/bugreport even when the file itself can't
    // be written, instead of vanishing completely.
    fun log(msg: String) {
        val dir = logDir ?: return
        val line = "${dateFmt.format(Date())} [kl] $msg\n"
        try {
            val framed = Binlog.appendRecord(TAG_SYSTEM, line.toByteArray(Charsets.UTF_8))
            File(dir, "snc_lifecycle.log").appendBytes(framed)
        } catch (e: Exception) {
            Log.w(TAG, "append failed, dir=$dir: $e")
        }
    }
}
