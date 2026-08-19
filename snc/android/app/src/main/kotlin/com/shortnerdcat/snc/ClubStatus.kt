// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.content.Context
import android.util.Log
import org.json.JSONObject
import java.io.File

// Snapshot of Cat Club / Elite Cat Club membership state, mirroring
// android-core's ClubStatusResponse (see tunnel_cat/android-core/ipc.go).
// Read from $filesDir/snc.club_status, written by snc-core every 5s
// alongside snc.cidr_status/snc.manifest_status (see main_linux.go) --
// there is no request/response IPC round-trip for this on the polling
// path since Kotlin's fire-and-forget sendIpc has no reader.
//
// Cached to SharedPreferences on every successful non-empty read: a fresh
// snc-core process (background service restart, VPN reconnect, network
// change, ...) starts with a blank clubState and reports ClubStatus.NONE
// for a few seconds -- sometimes tens of seconds on a slow network -- until
// its own poll (ClubDiscoverer / AdminStatusPoller) completes. This happens
// periodically on an otherwise-idle, already-connected app (any reconnect
// respawns snc-core), not just at app cold start.
//
// A blank reading is therefore NOT immediately trusted as "confirmed no
// membership": it must persist for BLANK_CONFIRM_MS before read() actually
// reports it and updates the cache. Until then, the last known-good
// (non-blank) cached value keeps being returned, so the badge/theme/menu
// don't flicker to Regular and back on every reconnect. A genuine
// revocation still eventually takes effect once the blank state has held
// for the full confirm window.
data class ClubStatus(
    val theme: String,        // "", "catclub", or "elite" -- the account's REAL membership theme
    val badgeText: String,
    val isAdmin: Boolean,
    val canRecommend: Boolean,
) {
    companion object {
        val NONE = ClubStatus(theme = "", badgeText = "", isAdmin = false, canRecommend = false)
        private const val PREF_CACHE_KEY = "club_status_cache"
        private const val PREF_PREVIEW_KEY = "club_theme_preview"
        private const val PREF_BLANK_SINCE_KEY = "club_status_blank_since"

        // Comfortably longer than a normal reconnect's re-poll time (the
        // first poll after Start() fires immediately, but the network round
        // trip through a freshly-reconnected tunnel can take several
        // seconds to tens of seconds) while still being short enough that a
        // real revocation shows up promptly rather than looking broken.
        private const val BLANK_CONFIRM_MS = 90_000L

        private fun isBlank(s: ClubStatus) = !s.isAdmin && s.theme.isEmpty() && !s.canRecommend

        fun read(context: Context): ClubStatus {
            val f = File(context.filesDir, "snc.club_status")
            val fresh = if (f.exists()) {
                try {
                    val j = JSONObject(f.readText())
                    ClubStatus(
                        theme = j.optString("theme", ""),
                        badgeText = j.optString("badge_text", ""),
                        isAdmin = j.optBoolean("is_admin", false),
                        canRecommend = j.optBoolean("can_recommend", false),
                    )
                } catch (e: Exception) {
                    Log.w("ClubStatus", "read: $e")
                    null
                }
            } else null

            val prefs = context.getSharedPreferences("snc", Context.MODE_PRIVATE)

            if (fresh != null && !isBlank(fresh)) {
                // Real, non-blank state -- always trust it immediately and
                // clear any pending "blank since" countdown.
                prefs.edit()
                    .putString(PREF_CACHE_KEY, JSONObject().apply {
                        put("theme", fresh.theme)
                        put("badge_text", fresh.badgeText)
                        put("is_admin", fresh.isAdmin)
                        put("can_recommend", fresh.canRecommend)
                    }.toString())
                    .remove(PREF_BLANK_SINCE_KEY)
                    .apply()
                return fresh
            }

            val cachedRaw = prefs.getString(PREF_CACHE_KEY, null)
            val cached = cachedRaw?.let {
                try {
                    val j = JSONObject(it)
                    ClubStatus(
                        theme = j.optString("theme", ""),
                        badgeText = j.optString("badge_text", ""),
                        isAdmin = j.optBoolean("is_admin", false),
                        canRecommend = j.optBoolean("can_recommend", false),
                    )
                } catch (e: Exception) {
                    null
                }
            }

            if (fresh == null) {
                // No file yet at all (process just started, hasn't written
                // its first tick) -- same debounce treatment as a blank read.
                return cached ?: NONE
            }
            // fresh is blank (isBlank(fresh) == true here).
            if (cached == null || isBlank(cached)) {
                // Nothing to protect -- genuinely nothing cached yet, or the
                // cache itself was already blank. Report as-is, no countdown needed.
                return fresh
            }
            val now = System.currentTimeMillis()
            val blankSince = prefs.getLong(PREF_BLANK_SINCE_KEY, 0L)
            if (blankSince == 0L) {
                prefs.edit().putLong(PREF_BLANK_SINCE_KEY, now).apply()
                return cached // just started seeing blanks -- keep showing cache
            }
            if (now - blankSince < BLANK_CONFIRM_MS) {
                return cached // still within the debounce window -- keep showing cache
            }
            // Confirmed: blank has persisted long enough to trust as real
            // (e.g. genuine membership revocation). Adopt it as the new cache.
            prefs.edit()
                .putString(PREF_CACHE_KEY, JSONObject().apply {
                    put("theme", fresh.theme)
                    put("badge_text", fresh.badgeText)
                    put("is_admin", fresh.isAdmin)
                    put("can_recommend", fresh.canRecommend)
                }.toString())
                .remove(PREF_BLANK_SINCE_KEY)
                .apply()
            return fresh
        }

        // Admin theme-preview override: purely a client-side UI convenience
        // (see keyenc.go's IsAdmin doc comment -- "never a real permission"),
        // so it never needs to reach snc-core/Go at all. Persisted so the
        // choice survives app restarts; applies instantly since every reader
        // (ConnectionFragment, MainActivity's menu) calls effectiveTheme()
        // directly rather than waiting on any poll.
        // theme: "" (regular), "catclub", or "elite" -- an explicit choice,
        // including "regular", is still a preview override and must win over
        // the real theme. clearPreview() is the only way back to "no override".
        fun setPreview(context: Context, theme: String) {
            context.getSharedPreferences("snc", Context.MODE_PRIVATE)
                .edit().putString(PREF_PREVIEW_KEY, theme).apply()
        }

        fun clearPreview(context: Context) {
            context.getSharedPreferences("snc", Context.MODE_PRIVATE)
                .edit().remove(PREF_PREVIEW_KEY).apply()
        }

        // null = no override active (not the same as "" which means the
        // admin explicitly chose the Regular preview).
        fun getPreview(context: Context): String? =
            context.getSharedPreferences("snc", Context.MODE_PRIVATE)
                .getString(PREF_PREVIEW_KEY, null)

        // effectiveTheme is what should actually be rendered: the admin's
        // preview override if one is set and the account is currently admin,
        // else the account's real discovered membership theme.
        fun effectiveTheme(context: Context, status: ClubStatus): String {
            if (!status.isAdmin) return status.theme
            return getPreview(context) ?: status.theme
        }

        // effectiveBadgeText: the preview must look exactly like the real
        // member-facing badge, no distinguishing marker text at all.
        fun effectiveBadgeText(context: Context, status: ClubStatus): String {
            if (!status.isAdmin) return status.badgeText
            return when (getPreview(context)) {
                "catclub" -> "Cat Club Member"
                "elite" -> "Elite Cat Club Member"
                "" -> "" // explicit Regular preview
                null -> status.badgeText // no override active
                else -> status.badgeText
            }
        }
    }
}
