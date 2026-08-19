// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log

// BootReceiver starts SncBackgroundService automatically after device boot so
// manifests and VK TURN credentials are populated before the user opens the app.
// Only starts the service if a key has been configured; no-ops otherwise.
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val action = intent.action ?: return
        if (action != Intent.ACTION_BOOT_COMPLETED && action != "android.intent.action.QUICKBOOT_POWERON") return

        val key = context.getSharedPreferences("snc", Context.MODE_PRIVATE)
            .getString("key", null)
        if (key.isNullOrBlank()) {
            Log.i(TAG, "boot: no key configured, skipping background service start")
            return
        }

        Log.i(TAG, "boot: starting background service")
        context.startForegroundService(Intent(context, SncBackgroundService::class.java))
    }

    companion object {
        private const val TAG = "BootReceiver"
    }
}
