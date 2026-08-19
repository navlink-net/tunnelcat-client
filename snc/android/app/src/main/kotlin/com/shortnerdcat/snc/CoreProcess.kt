// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.app.ActivityManager
import android.content.Context
import android.util.Log
import java.io.File
import java.io.FileInputStream
import java.io.IOException

object CoreProcess {
    private const val TAG = "CoreProcess"

    fun binaryFile(context: Context): File =
        File(context.applicationInfo.nativeLibraryDir, "libsnc_core.so")

    fun start(
        context: Context,
        key: String,
        tunFd: Int,
        protectSocket: String,
        uidSocket: String? = null,
        ipcSocket: String,
        logDir: String,
        dataDir: String,
        disableUdp: Boolean = false,
        blockQuic: Boolean = false,
        disableBypass: Boolean = false,
        disableIpv6: Boolean = false,
        countryCC: String? = null,
        proxyOnly: Boolean = false,
        manual: Boolean = true,
    ): Process {
        val binary = binaryFile(context)
        if (!binary.exists()) throw IOException("snc-core binary not found: ${binary.absolutePath}")
        Log.i(TAG, "launching ${binary.absolutePath} (${binary.length()} bytes)")

        val memInfo = ActivityManager.MemoryInfo()
        (context.getSystemService(Context.ACTIVITY_SERVICE) as ActivityManager).getMemoryInfo(memInfo)
        val goMemLimitMiB = (memInfo.totalMem / 12 / (1024 * 1024))
            .coerceIn(64L, 256L)
        Log.i(TAG, "GOMEMLIMIT=${goMemLimitMiB}MiB (totalRam=${memInfo.totalMem / (1024*1024)}MiB)")
        val dataChanDepth = (goMemLimitMiB / 8).coerceIn(8L, 64L)
        Log.i(TAG, "SNC_DATACHAN_DEPTH=$dataChanDepth")

        val envList = mutableListOf(
            "SNC_KEY=$key",
            "SNC_TUN_FD=$tunFd",
            "SNC_PROTECT_SOCKET=$protectSocket",
            "SNC_SOCKET=$ipcSocket",
            "SNC_LOG_DIR=$logDir",
            "SNC_DATA_DIR=$dataDir",
        )
        if (!uidSocket.isNullOrBlank()) envList.add("SNC_UID_SOCKET=$uidSocket")
        // Connection-stats admin-dashboard feature: manual vs auto connect count
        // (see core.ConnStatsCollector.IncConnect on the Go side). Only set when
        // false -- absence means manual, matching every other "off by default"
        // flag in this env list.
        if (!manual) envList.add("SNC_AUTO_RECONNECT=1")
        if (proxyOnly) envList.add("SNC_PROXY_ONLY=1")
        if (disableUdp) envList.add("SNC_DISABLE_UDP=1")
        if (blockQuic) envList.add("SNC_BLOCK_QUIC=1")
        if (disableBypass) envList.add("SNC_DISABLE_BYPASS=1")
        if (disableIpv6) envList.add("SNC_DISABLE_IPV6=1")
        if (!countryCC.isNullOrBlank()) envList.add("SNC_CC=${countryCC.uppercase()}")
        envList.add("GOMEMLIMIT=${goMemLimitMiB}MiB")
        envList.add("SNC_DATACHAN_DEPTH=$dataChanDepth")
        val env = envList.toTypedArray()

        val result = NativeHelper.forkExec(binary.absolutePath, env, tunFd)
        val pid = result[0]
        val logReadFd = result[1]
        if (pid < 0) throw IOException("forkExec failed (errno in native layer)")
        Log.i(TAG, "snc-core started pid=$pid logReadFd=$logReadFd")

        val proc = NativeProcess(pid, logReadFd)

        // Drain child stdout+stderr into logcat on a background thread.
        Thread({
            try {
                FileInputStream("/proc/self/fd/$logReadFd").bufferedReader().forEachLine { line ->
                    Log.i("snc-core", line)
                }
            } catch (_: IOException) {}
        }, "snc-core-log").apply { isDaemon = true; start() }

        return proc
    }

}

// NativeProcess wraps a raw child PID as a java.lang.Process.
internal class NativeProcess(private val pid: Int, private val logReadFd: Int) : Process() {
    @Volatile private var exitCode: Int? = null

    override fun getOutputStream() = java.io.OutputStream.nullOutputStream()
    override fun getInputStream() = java.io.InputStream.nullInputStream()
    override fun getErrorStream() = java.io.InputStream.nullInputStream()

    override fun waitFor(): Int {
        if (exitCode != null) return exitCode!!
        return NativeHelper.waitForPid(pid).also { exitCode = it }
    }

    override fun exitValue(): Int =
        exitCode ?: throw IllegalThreadStateException("process $pid still running")

    override fun destroy() {
        NativeHelper.killPid(pid)
        // If SIGTERM cleanup takes too long the TUN fd stays open and Android keeps
        // the VPN interface active. Force-kill after 2 s so the fd is released promptly.
        val savedPid = pid
        Thread({ Thread.sleep(2000L); NativeHelper.sigkillPid(savedPid) }, "snc-kill")
            .apply { isDaemon = true; start() }
    }
}
