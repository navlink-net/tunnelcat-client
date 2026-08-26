// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.os.IBinder
import android.util.Log
import java.io.File

// SncBackgroundService keeps the gossip mesh (manifest fetch + DHT) warm whenever
// the app process is alive, even with no VPN connection — so manifests and the
// relay pool are already populated by the time the user taps Connect.
//
// Runs snc-core in SNC_PROXY_ONLY mode (no TUN, no protect socket). Paused while
// SNCVpnService's main snc-core is active to avoid concurrent writes to the shared
// dataDir state files (peers.json, manifest.json).
class SncBackgroundService : Service() {

    @Volatile private var coreProcess: Process? = null
    @Volatile private var ipcPath: String? = null

    // Owns the OTA check/download loop for the lifetime of this service rather than
    // MainActivity's — the service outlives the Activity (it keeps the gossip mesh warm
    // even with no UI open), so this is the natural place for an always-on check, mirroring
    // Ratatosk's ChatSyncService-driven OTA loop.
    private val updateChecker by lazy { UpdateChecker(this) }

    private val updateReadyReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            if (intent.action == ACTION_UPDATE_READY) {
                val version = UpdateChecker.readyVersion(this@SncBackgroundService) ?: return
                postUpdateReadyNotification(version)
            }
        }
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        instance = this
        KotlinLog.init(java.io.File(filesDir, "logs"))
        LogEvent.emitSystem(LogEvents.AndroidBgServiceLifecycle, LogAttrs.ATTR_Stage to "created")
        createNotificationChannel()
        createUpdateNotificationChannel()
        startForeground(NOTIFICATION_ID, buildNotification())
        // launchCore() acquires @Synchronized and calls CoreProcess.start() (fork+exec).
        // Dispatch to avoid blocking the main thread and risking an ANR at startup.
        Thread({ launchCore() }, "snc-bg-launch").start()
        registerReceiver(updateReadyReceiver, IntentFilter(ACTION_UPDATE_READY), Context.RECEIVER_NOT_EXPORTED)
        updateChecker.start()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        return START_STICKY
    }

    override fun onDestroy() {
        LogEvent.emitSystem(LogEvents.AndroidBgServiceLifecycle, LogAttrs.ATTR_Stage to "destroyed")
        instance = null
        updateChecker.stop()
        try { unregisterReceiver(updateReadyReceiver) } catch (_: Exception) {}
        // killCore() acquires @Synchronized and calls forkExec-spawned process.destroy();
        // if launchCore() is running on another thread it holds the lock, blocking the main
        // thread. Dispatch to avoid ANR.
        Thread({ killCore() }, "snc-bg-kill").start()
        super.onDestroy()
    }

    // Called by SNCVpnService before launching its own snc-core. Sends a graceful
    // stop to the background snc-core, then destroys it (SIGTERM; SIGKILL in 2 s).
    fun pauseForVpn() {
        LogEvent.emitSystem(LogEvents.AndroidBgServiceLifecycle, LogAttrs.ATTR_Stage to "paused_for_vpn")
        ipcPath?.let { sendStop(it) }
        killCore()
        stopForeground(STOP_FOREGROUND_REMOVE)
    }

    // Called by SNCVpnService after its snc-core has stopped (user disconnect or
    // unrecoverable error). Re-reads prefs so a key configured after first launch
    // is picked up automatically.
    fun resumeAfterVpn() {
        LogEvent.emitSystem(LogEvents.AndroidBgServiceLifecycle, LogAttrs.ATTR_Stage to "resumed_after_vpn")
        startForeground(NOTIFICATION_ID, buildNotification())
        launchCore()
    }

    @Synchronized
    private fun launchCore() {
        if (coreProcess != null) return
        val key = getSharedPreferences("snc", MODE_PRIVATE).getString(PREF_KEY, null)
        if (key.isNullOrBlank()) {
            Log.i(TAG, "no key configured yet — staying idle")
            return
        }
        val logDir = File(filesDir, "logs").also { it.mkdirs() }.absolutePath
        // Abstract namespace, not filesystem -- see SNCVpnService's ipcName note
        // (2026-08-07): the filesystem-namespace version of this kind of socket
        // never delivered a single command in a real user's session.
        val ipc = "snc_bg.${android.os.Process.myPid()}"
        try {
            coreProcess = CoreProcess.start(
                context = this,
                key = key,
                tunFd = -1,
                protectSocket = "",
                ipcSocket = "@$ipc",
                logDir = logDir,
                dataDir = filesDir.absolutePath,
                countryCC = detectCountryCC(this),
                proxyOnly = true,
            )
            ipcPath = ipc
            Log.i(TAG, "background snc-core started")
        } catch (e: Exception) {
            Log.e(TAG, "failed to start snc-core", e)
        }
    }

    @Synchronized
    private fun killCore() {
        coreProcess?.destroy()
        coreProcess = null
        ipcPath = null
    }

    private fun sendStop(path: String) {
        try {
            val sock = LocalSocket()
            sock.connect(LocalSocketAddress(path, LocalSocketAddress.Namespace.ABSTRACT))
            sock.outputStream.write("{\"cmd\":\"stop\"}\n".toByteArray())
            sock.outputStream.flush()
            sock.close()
        } catch (e: Exception) {
            Log.w(TAG, "sendStop: $e")
        }
    }

    private fun createNotificationChannel() {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ID,
                getString(R.string.bg_channel_name),
                NotificationManager.IMPORTANCE_LOW
            )
        )
    }

    private fun buildNotification(): Notification =
        Notification.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.bg_notification_title))
            .setContentText(getString(R.string.bg_notification_text))
            .setSmallIcon(R.mipmap.ic_launcher)
            .setOngoing(true)
            .build()

    // Separate from the ongoing background-sync channel: this one is a one-off,
    // user-visible heads-up so it's noticed even if the app isn't open — IMPORTANCE_DEFAULT
    // instead of the background channel's IMPORTANCE_LOW.
    private fun createUpdateNotificationChannel() {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(
                UPDATE_CHANNEL_ID,
                getString(R.string.update_channel_name),
                NotificationManager.IMPORTANCE_DEFAULT
            )
        )
    }

    private fun postUpdateReadyNotification(version: String) {
        val openIntent = Intent(this, MainActivity::class.java)
            .setFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP)
        val pendingIntent = PendingIntent.getActivity(
            this, 0, openIntent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val notification = Notification.Builder(this, UPDATE_CHANNEL_ID)
            .setContentTitle(getString(R.string.update_notification_title))
            .setContentText(getString(R.string.update_notification_text) + " ($version)")
            .setSmallIcon(R.mipmap.ic_launcher)
            .setContentIntent(pendingIntent)
            .setAutoCancel(true)
            .build()
        getSystemService(NotificationManager::class.java).notify(UPDATE_NOTIFICATION_ID, notification)
    }

    companion object {
        private const val TAG = "SncBackgroundService"
        private const val CHANNEL_ID = "snc_background"
        private const val NOTIFICATION_ID = 2
        private const val UPDATE_CHANNEL_ID = "snc_update"
        private const val UPDATE_NOTIFICATION_ID = 3
        private const val PREF_KEY = "key"

        @Volatile
        var instance: SncBackgroundService? = null
            private set
    }
}
