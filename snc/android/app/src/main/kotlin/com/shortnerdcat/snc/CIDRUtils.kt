// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

object CIDRUtils {

    data class Route(val address: String, val prefixLength: Int)

    // Maximum number of routes we will pass to VpnService.Builder.establish().
    // Android's Binder IPC has a ~1 MB parcel limit; each route costs ~40 bytes,
    // so 20 000 routes ≈ 800 KB leaves comfortable headroom.
    private const val MAX_VPN_ROUTES = 20_000

    /**
     * Returns routes covering 0.0.0.0/0 minus the excluded CIDRs.
     * Merges nearby ranges (gap ≤ [mergeGap] addresses) before inverting to keep
     * the output route count below [MAX_VPN_ROUTES].  If the result would still
     * exceed the limit the function returns null, signalling the caller to fall
     * back to routing all traffic through the tunnel.
     */
    fun invertIPv4(excludedCidrs: List<String>, mergeGap: Long = 4096L): List<Route>? {
        val excluded = mutableListOf<LongRange>()
        for (cidr in excludedCidrs) {
            val slash = cidr.indexOf('/').takeIf { it >= 0 } ?: continue
            val ip = ipToLong(cidr.substring(0, slash)) ?: continue
            val prefix = cidr.substring(slash + 1).toIntOrNull() ?: continue
            if (prefix < 0 || prefix > 32) continue
            val hostBits = 32 - prefix
            val mask = if (hostBits == 32) 0xFFFFFFFFL else (1L shl hostBits) - 1L
            val start = ip and mask.inv()
            val end = start or mask
            excluded.add(start..end)
        }
        excluded.sortBy { it.first }

        // Merge overlapping, adjacent, and nearby ranges (gap ≤ mergeGap addresses).
        val merged = mutableListOf<LongRange>()
        for (r in excluded) {
            if (merged.isEmpty() || r.first > merged.last().last + 1 + mergeGap) {
                merged.add(r)
            } else {
                val prev = merged.removeLast()
                merged.add(prev.first..maxOf(prev.last, r.last))
            }
        }

        val result = mutableListOf<Route>()
        var pos = 0L
        for (range in merged) {
            if (pos < range.first) {
                result.addAll(rangeToCIDRs(pos, range.first - 1))
            }
            if (range.last + 1 > pos) pos = range.last + 1
        }
        if (pos <= 0xFFFFFFFFL) {
            result.addAll(rangeToCIDRs(pos, 0xFFFFFFFFL))
        }
        if (result.size > MAX_VPN_ROUTES) return null
        return result
    }

    private fun rangeToCIDRs(start: Long, end: Long): List<Route> {
        val result = mutableListOf<Route>()
        var pos = start
        while (pos <= end) {
            var bits = 0
            while (bits < 32) {
                val size = 1L shl (bits + 1)
                if (pos % size != 0L || pos + size - 1 > end) break
                bits++
            }
            result.add(Route(longToIp(pos), 32 - bits))
            pos += 1L shl bits
            if (pos < 0) break // overflow past 255.255.255.255
        }
        return result
    }

    private fun ipToLong(ip: String): Long? {
        val parts = ip.split('.')
        if (parts.size != 4) return null
        var result = 0L
        for (part in parts) {
            val n = part.toIntOrNull() ?: return null
            if (n < 0 || n > 255) return null
            result = (result shl 8) or n.toLong()
        }
        return result
    }

    private fun longToIp(n: Long): String {
        return "${(n shr 24) and 0xFF}.${(n shr 16) and 0xFF}.${(n shr 8) and 0xFF}.${n and 0xFF}"
    }
}
