// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.PowerManager
import android.provider.Settings
import androidx.appcompat.app.AlertDialog

// Battery-optimization exemption (2026-08-12).
//
// This is a VPN: it must be exempt from battery optimization for as long as
// it's connected, full stop -- not a discretionary setting, no menu toggle.
// A foreground service's PARTIAL_WAKE_LOCK keeps the CPU awake but does
// nothing for Doze's network restrictions -- Doze (and OEM battery managers
// on Xiaomi/Huawei/Samsung, which are more aggressive still) can throttle a
// non-exempt app's network I/O with the screen off even while its foreground
// service keeps running.
//
// Android does not allow granting this silently -- REQUEST_IGNORE_BATTERY_
// OPTIMIZATIONS requires the system's own confirmation dialog, by design, so
// apps can't mass-opt-out of battery saving on their own. Since it can't be
// forced, the app instead asks every time it's not yet granted (see
// ensureExempt, called on every Connect tap) rather than asking once and
// giving up -- there's no "later" setting for this to live in.
object BatteryOptimization {
    fun isExempt(context: Context): Boolean {
        val pm = context.getSystemService(Context.POWER_SERVICE) as? PowerManager ?: return true
        return pm.isIgnoringBatteryOptimizations(context.packageName)
    }

    private fun requestIntent(context: Context): Intent =
        Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
            .setData(Uri.parse("package:${context.packageName}"))

    /** Call on every connect attempt. No-op if already exempt. */
    fun ensureExempt(activity: Activity, launch: (Intent) -> Unit) {
        if (isExempt(activity)) return
        AlertDialog.Builder(activity)
            .setTitle(R.string.battery_opt_dialog_title)
            .setMessage(R.string.battery_opt_dialog_message)
            .setPositiveButton(R.string.battery_opt_dialog_allow) { _, _ -> launch(requestIntent(activity)) }
            .setNegativeButton(R.string.battery_opt_dialog_not_now, null)
            .show()
    }
}
