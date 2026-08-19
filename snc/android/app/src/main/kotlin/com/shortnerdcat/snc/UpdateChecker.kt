// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.content.Context
import android.content.Intent
import android.util.Log
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.InetSocketAddress
import java.net.Proxy
import java.net.Socket
import java.security.MessageDigest
import java.security.cert.X509Certificate
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocket
import javax.net.ssl.TrustManager
import javax.net.ssl.X509TrustManager

private const val TAG = "UpdateChecker"
private const val PREF_UPDATE_VERSION = "update_version"
private const val PREF_UPDATE_URL = "update_url"
private const val PREF_UPDATE_SHA256 = "update_sha256"
private const val PREF_READY_VERSION = "update_ready_version"
private const val PREF_READY_PATH = "update_ready_path"
private const val CHECK_INTERVAL_MS = 5 * 60 * 1000L
const val ACTION_UPDATE_READY = "com.shortnerdcat.snc.UPDATE_READY"

// 2026-08-18 correction: the control's real, kept-up-to-date OTA cache
// (client_cache.go writing cacheDir/<slug>/{bin,version,sha256}, refreshed
// from the arbiter every 15 min or immediately on /p/v1/refresh) is served
// by a wholly separate TLS listener on this dedicated port (update_http.go,
// snc-control/main.go's --update-port, default 8090) -- NOT through the
// :443 relay-API SNI mechanism. That path (relay_api.go's serveUpdate,
// backed by h.updateDir) is a different, legacy cache that nothing keeps
// populated for Android anymore -- confirmed live: a real control answered
// "already at version 202608172121" for its client_cache but 404'd
// /p/v1/update/client-android-version. Desktop clients (updater_linux.go
// etc.) already hit this exact port directly and have never needed
// anything else. Restored to match them.
private const val UPDATE_PORT = 8090

private const val APK_FILE_NAME = "snc-update.apk"

// Polls control nodes for a newer client version and, when one is found, downloads and
// SHA-256-verifies the APK silently in the background — entirely on this checker's own
// daemon thread, so the user sees nothing until the file is sitting on disk, verified and
// ready to install. MainActivity only needs to surface the "Update ready" menu entry and
// launch the installer from the persisted path — no waiting, no re-download.
//
// Runs for the lifetime of SncBackgroundService, i.e. including while SNCVpnService's
// full-tunnel VPN is active.
//
// 2026-08-17: stopped routing through snc-core's local SOCKS5 proxy (which put
// these requests on the same exit-relay path as ordinary user traffic --
// confirmed on a real RU device: same-country-redirect forwarded update
// checks through peer exits that couldn't reach the control's update port,
// rejecting update checks outright even though the control itself was
// reachable directly). This traffic is 100% control-to-client (checking our
// own control node for a newer build) -- an exit has no business seeing it
// at all. Now connects straight to the control's dedicated update port
// (UPDATE_PORT, see its doc comment) -- a wholly separate TLS listener from
// the :443 tunnel/relay-API traffic, so no exit or same-country-redirect
// logic is anywhere in this path at all -- protected via VpnService.protect()
// so this socket isn't captured by the app's own TUN. This is the same class
// of connection the main tunnel data path already depends on working
// (protect() has to succeed for the VPN to function at all), so it doesn't
// reintroduce the SOCKS5 route's original motivation in any new way -- see
// git history for the 2026-08-06/2026-08-15 incidents that motivated SOCKS5
// in the first place, before exits turned out to be a bigger, unrelated
// problem, and 2026-08-18 for the first (wrong) attempt at this fix, which
// pointed at a control endpoint nothing actually keeps populated.
class UpdateChecker(private val context: Context) {

    @Volatile private var running = false
    private var thread: Thread? = null

    private val trustAll = object : X509TrustManager {
        override fun checkClientTrusted(chain: Array<out X509Certificate>, authType: String) {}
        override fun checkServerTrusted(chain: Array<out X509Certificate>, authType: String) {}
        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }

    // Controls are addressed by IP so hostname verification is intentionally skipped.
    private val sslContext: SSLContext by lazy {
        SSLContext.getInstance("TLS").also { it.init(null, arrayOf<TrustManager>(trustAll), null) }
    }

    fun start() {
        if (running) return
        running = true
        KotlinLog.log("UpdateChecker: start()")
        thread = Thread({ loop() }, "snc-update-checker").apply { isDaemon = true; start() }
    }

    fun stop() {
        running = false
        thread?.interrupt()
    }

    private fun loop() {
        while (running) {
            try {
                check()
            } catch (_: InterruptedException) {
                break
            } catch (e: Exception) {
                Log.w(TAG, "check error: $e")
                KotlinLog.log("UpdateChecker: check() threw, loop continues: $e")
            }
            try {
                Thread.sleep(CHECK_INTERVAL_MS)
            } catch (_: InterruptedException) {
                break
            }
        }
    }

    private fun check() {
        val controls = readControls()
        val vpnActive = SNCVpnService.isActive()
        if (controls.isEmpty()) {
            Log.d(TAG, "no controls in snc.controls — skipping check")
            KotlinLog.log("UpdateChecker: check skipped, no controls in snc.controls (vpnActive=$vpnActive)")
            return
        }
        Log.d(TAG, "checking ${controls.size} control(s), current=${BuildConfig.VERSION_NAME}")
        KotlinLog.log("UpdateChecker: check start, ${controls.size} control(s), current=${BuildConfig.VERSION_NAME} vpnActive=$vpnActive")

        for (host in controls) {
            // Strip port from stored address (e.g. "62.238.3.12:443") -- always use
            // UPDATE_PORT instead, never the stored port.
            val ip = host.substringBefore(':')
            val remoteVersion = httpGetText(ip, UPDATE_PORT, "/client-android-version")
            if (remoteVersion == null) {
                Log.w(TAG, "fetch failed: $ip:$UPDATE_PORT/client-android-version")
                KotlinLog.log("UpdateChecker: fetch failed control=$ip vpnActive=$vpnActive")
                continue
            }
            val version = remoteVersion.trim()
            Log.d(TAG, "control=$ip remote=$version current=${BuildConfig.VERSION_NAME}")
            if (!isValidVersion(version)) {
                Log.w(TAG, "invalid version string: '$version'")
                KotlinLog.log("UpdateChecker: control=$ip returned invalid version string '$version'")
                continue
            }
            val prefs = context.getSharedPreferences("snc", Context.MODE_PRIVATE)
            if (version <= BuildConfig.VERSION_NAME) {
                Log.d(TAG, "up to date (remote=$version current=${BuildConfig.VERSION_NAME})")
                KotlinLog.log("UpdateChecker: up to date, control=$ip remote=$version current=${BuildConfig.VERSION_NAME}")
                clearAll(prefs)
                return
            }

            // Already downloaded and verified for this exact version — nothing to do,
            // the "ready" broadcast was already sent when it finished.
            if (prefs.getString(PREF_READY_VERSION, null) == version &&
                File(prefs.getString(PREF_READY_PATH, "") ?: "").exists()
            ) {
                return
            }

            Log.i(TAG, "update available: $version (current ${BuildConfig.VERSION_NAME}) — downloading silently")
            KotlinLog.log("UpdateChecker: update available $version (current ${BuildConfig.VERSION_NAME}) via control=$ip vpnActive=$vpnActive — downloading silently")
            val sha256 = httpGetText(ip, UPDATE_PORT, "/client-android.sha256")?.trim()?.split(" ")?.firstOrNull() ?: ""
            prefs.edit()
                .putString(PREF_UPDATE_VERSION, version)
                .putString(PREF_UPDATE_URL, "https://$ip:$UPDATE_PORT/client-android")
                .putString(PREF_UPDATE_SHA256, sha256)
                .apply()

            val apk = downloadAndVerify(ip, UPDATE_PORT, "/client-android", sha256)
            if (apk == null) {
                Log.w(TAG, "silent download/verification failed for $version — will retry on next check")
                KotlinLog.log("UpdateChecker: download/verification FAILED for $version via control=$ip vpnActive=$vpnActive — will retry")
                return
            }
            Log.i(TAG, "update $version downloaded and verified — ready to install")
            KotlinLog.log("UpdateChecker: update $version downloaded and verified — ready to install")
            prefs.edit()
                .putString(PREF_READY_VERSION, version)
                .putString(PREF_READY_PATH, apk.absolutePath)
                .apply()
            context.sendBroadcast(Intent(ACTION_UPDATE_READY).setPackage(context.packageName))
            return
        }
        Log.w(TAG, "no control returned a valid version")
        KotlinLog.log("UpdateChecker: no control returned a valid version (${controls.size} tried, vpnActive=$vpnActive)")
    }

    // snc-core's local SOCKS5 proxy, when the VPN is active -- see openConnection's
    // "outside" vs "inside the tunnel" doc comment for why this is a fallback, not
    // the primary path.
    private fun readSocksProxy(): Proxy? {
        return try {
            val raw = File(context.filesDir, "snc.socks").readText().trim()
            val port = raw.substringAfterLast(':').toIntOrNull() ?: return null
            Proxy(Proxy.Type.SOCKS, InetSocketAddress("127.0.0.1", port))
        } catch (e: Exception) {
            null
        }
    }

    // Opens a TLS connection to the control's dedicated update port (UPDATE_PORT,
    // see its doc comment) -- a wholly separate listener from :443, so no exit or
    // same-country-redirect logic is anywhere in this path regardless of transport.
    //
    // Two transports, tried in order, restoring BOTH update paths (2026-08-18,
    // after a same-day regression left some already-updated devices unable to
    // reach either):
    //  1. Direct, protect()'d socket ("outside the tunnel") -- the fast, normal
    //     path; works whenever the OS honours protect() correctly.
    //  2. snc-core's local SOCKS5 proxy ("inside the tunnel"), when VPN is active
    //     -- covers the OEM/lockdown devices where protect() can report success
    //     while the OS still silently drops that socket's traffic (suspected
    //     2026-08-15, never fully confirmed). This still targets the same
    //     UPDATE_PORT/host as #1, not any :443/exit-relayed path -- only the
    //     transport differs, so it isn't subject to same-country-redirect.
    // TODO(remove-fallback): #2 is a temporary safety net for devices stuck on
    // builds that predate this fix. Once those have had a chance to recover,
    // drop it back to direct-only -- keeping both permanently reintroduces the
    // slower, less reliable path for everyone as a matter of routine.
    private fun openConnection(host: String, port: Int, connectTimeoutMs: Int): SSLSocket? {
        try {
            return openDirect(host, port, connectTimeoutMs)
        } catch (e: Exception) {
            Log.w(TAG, "openConnection: direct attempt failed, trying SOCKS5: $e")
            KotlinLog.log("UpdateChecker: direct attempt to $host:$port failed ($e), trying SOCKS5")
        }
        return openViaSocks(host, port, connectTimeoutMs)
    }

    private fun openDirect(host: String, port: Int, connectTimeoutMs: Int): SSLSocket {
        val raw = Socket()
        try {
            if (!SNCVpnService.protectIfActive(raw)) {
                raw.close()
                throw IOException("protect() refused socket")
            }
            raw.connect(InetSocketAddress(host, port), connectTimeoutMs)
            raw.soTimeout = connectTimeoutMs
            val ssl = sslContext.socketFactory.createSocket(raw, host, port, true) as SSLSocket
            ssl.startHandshake()
            return ssl
        } catch (e: Exception) {
            try { raw.close() } catch (_: Exception) {}
            throw e
        }
    }

    private fun openViaSocks(host: String, port: Int, connectTimeoutMs: Int): SSLSocket? {
        val proxy = if (SNCVpnService.isActive()) readSocksProxy() else null
        if (proxy == null) return null
        val raw = Socket(proxy)
        try {
            raw.connect(InetSocketAddress(host, port), connectTimeoutMs)
            raw.soTimeout = connectTimeoutMs
            val ssl = sslContext.socketFactory.createSocket(raw, host, port, true) as SSLSocket
            ssl.startHandshake()
            return ssl
        } catch (e: Exception) {
            try { raw.close() } catch (_: Exception) {}
            throw e
        }
    }

    private fun sendGet(sock: Socket, host: String, path: String) {
        val req = "GET $path HTTP/1.1\r\nHost: $host\r\nConnection: close\r\nUser-Agent: snc-update-checker\r\n\r\n"
        sock.getOutputStream().apply { write(req.toByteArray(Charsets.US_ASCII)); flush() }
    }

    // Reads a single CRLF-terminated line (without the trailing CRLF). Returns null
    // only on EOF with nothing read at all.
    private fun readLine(input: InputStream): String? {
        val buf = ByteArrayOutputStream(128)
        var b = input.read()
        if (b == -1) return null
        while (b != -1 && b != '\n'.code) {
            if (b != '\r'.code) buf.write(b)
            b = input.read()
        }
        return buf.toString("ISO-8859-1")
    }

    // Reads the status line + headers, leaving the stream positioned at the start of
    // the body. Header names are lowercased for case-insensitive lookup.
    private fun readStatusAndHeaders(input: InputStream): Pair<Int, Map<String, String>> {
        val statusLine = readLine(input) ?: throw IOException("empty response")
        val parts = statusLine.split(" ", limit = 3)
        if (parts.size < 2) throw IOException("bad status line: $statusLine")
        val code = parts[1].toIntOrNull() ?: throw IOException("bad status code: $statusLine")
        val headers = mutableMapOf<String, String>()
        while (true) {
            val line = readLine(input) ?: throw IOException("truncated headers")
            if (line.isEmpty()) break
            val idx = line.indexOf(':')
            if (idx > 0) {
                headers[line.substring(0, idx).trim().lowercase()] = line.substring(idx + 1).trim()
            }
        }
        return code to headers
    }

    // Copies the response body to sink, honouring Transfer-Encoding: chunked or
    // Content-Length, falling back to read-until-EOF (valid since every request here
    // sends "Connection: close"). The control's update-file server (Go net/http) sends
    // small text responses with Content-Length but switches to chunked for the
    // multi-MB APK once a single Write() exceeds its small internal buffer, so both
    // paths are handled rather than assuming one by response size.
    private fun copyBody(input: InputStream, headers: Map<String, String>, sink: OutputStream, digest: MessageDigest? = null) {
        val chunked = headers["transfer-encoding"]?.lowercase()?.contains("chunked") == true
        val buf = ByteArray(8192)
        if (chunked) {
            while (true) {
                val sizeLine = readLine(input) ?: throw IOException("truncated chunk header")
                val size = sizeLine.substringBefore(';').trim().toIntOrNull(16)
                    ?: throw IOException("bad chunk size: $sizeLine")
                if (size == 0) {
                    while (true) {
                        val trailer = readLine(input) ?: break
                        if (trailer.isEmpty()) break
                    }
                    break
                }
                var remaining = size
                while (remaining > 0) {
                    val n = input.read(buf, 0, minOf(buf.size, remaining))
                    if (n == -1) throw IOException("truncated chunk body")
                    sink.write(buf, 0, n)
                    digest?.update(buf, 0, n)
                    remaining -= n
                }
                readLine(input) // CRLF terminating this chunk
            }
        } else {
            val declaredLen = headers["content-length"]?.toLongOrNull()
            var remaining = declaredLen ?: Long.MAX_VALUE
            var written = 0L
            while (remaining > 0) {
                val toRead = if (remaining < buf.size) remaining.toInt() else buf.size
                val n = input.read(buf, 0, toRead)
                if (n == -1) {
                    if (declaredLen != null) throw IOException("truncated body: expected $declaredLen got $written")
                    break // EOF with no declared length -- expected, we sent Connection: close
                }
                sink.write(buf, 0, n)
                digest?.update(buf, 0, n)
                written += n
                remaining -= n
            }
        }
    }

    private fun httpGetText(host: String, port: Int, path: String): String? {
        return try {
            openConnection(host, port, 5_000)?.use { sock ->
                sendGet(sock, host, path)
                val input = sock.getInputStream()
                val (code, headers) = readStatusAndHeaders(input)
                if (code != 200) return null
                val out = ByteArrayOutputStream()
                copyBody(input, headers, out)
                out.toString("UTF-8")
            } ?: run {
                Log.w(TAG, "$host:$port$path: protect() refused socket (VPN active)")
                KotlinLog.log("UpdateChecker: PROTECT REFUSED $host:$port$path (VpnService.protect() returned false while VPN active)")
                null
            }
        } catch (e: Exception) {
            Log.w(TAG, "httpGetText $host:$port$path failed: $e")
            KotlinLog.log("UpdateChecker: httpGetText $host:$port$path FAILED vpnActive=${SNCVpnService.isActive()}: $e")
            null
        }
    }

    // Downloads the APK to a stable location in filesDir (survives cache eviction — the
    // file must still be there whenever the user taps "Install", possibly much later) and
    // verifies its SHA-256 against the control-provided digest. Three attempts — a
    // transient network hiccup shouldn't block the whole update.
    private fun downloadAndVerify(host: String, port: Int, path: String, expectedSha256: String): File? {
        val out = File(context.filesDir, APK_FILE_NAME)
        repeat(3) { attempt ->
            try {
                val sock = openConnection(host, port, 30_000)
                if (sock == null) {
                    Log.w(TAG, "download attempt ${attempt + 1}/3: protect() refused socket (VPN active)")
                    KotlinLog.log("UpdateChecker: PROTECT REFUSED download attempt ${attempt + 1}/3 $host:$port$path (VpnService.protect() returned false while VPN active)")
                    return@repeat
                }
                sock.use {
                    it.soTimeout = 300_000
                    sendGet(it, host, path)
                    val input = it.getInputStream()
                    val (code, headers) = readStatusAndHeaders(input)
                    if (code != 200) throw IOException("HTTP $code")
                    val digest = MessageDigest.getInstance("SHA-256")
                    out.outputStream().use { fileOut -> copyBody(input, headers, fileOut, digest) }
                    val actualSha256 = digest.digest().joinToString("") { b -> "%02x".format(b) }
                    if (expectedSha256.isNotEmpty() && actualSha256 != expectedSha256) {
                        Log.w(TAG, "SHA-256 mismatch attempt ${attempt + 1}/3: expected $expectedSha256 got $actualSha256")
                        out.delete()
                        return@repeat
                    }
                    return out
                }
            } catch (e: Exception) {
                Log.w(TAG, "download attempt ${attempt + 1}/3 failed: $e")
                KotlinLog.log("UpdateChecker: download attempt ${attempt + 1}/3 $host:$port$path FAILED vpnActive=${SNCVpnService.isActive()}: $e")
                out.delete()
            }
        }
        return null
    }

    private fun clearAll(prefs: android.content.SharedPreferences) {
        val readyPath = prefs.getString(PREF_READY_PATH, null)
        if (readyPath != null) File(readyPath).delete()
        prefs.edit()
            .remove(PREF_UPDATE_VERSION).remove(PREF_UPDATE_URL).remove(PREF_UPDATE_SHA256)
            .remove(PREF_READY_VERSION).remove(PREF_READY_PATH)
            .apply()
    }

    private fun readControls(): List<String> {
        val file = File(context.filesDir, "snc.controls")
        if (!file.exists()) return emptyList()
        return file.readLines().map { it.trim() }.filter { it.isNotEmpty() }
    }

    private fun isValidVersion(s: String): Boolean =
        s.length >= 8 && s.all { it.isDigit() }

    companion object {
        // Version + path of the APK that's downloaded, verified and ready to install.
        // Both must be present and the file must still exist for the update to be offered.
        fun readyVersion(context: Context): String? {
            val prefs = context.getSharedPreferences("snc", Context.MODE_PRIVATE)
            val version = prefs.getString(PREF_READY_VERSION, null) ?: return null
            val path = prefs.getString(PREF_READY_PATH, null) ?: return null
            if (!File(path).exists()) return null
            // Already running this version or newer — clear stale state immediately.
            if (version <= BuildConfig.VERSION_NAME) {
                File(path).delete()
                prefs.edit()
                    .remove(PREF_UPDATE_VERSION).remove(PREF_UPDATE_URL).remove(PREF_UPDATE_SHA256)
                    .remove(PREF_READY_VERSION).remove(PREF_READY_PATH)
                    .apply()
                return null
            }
            return version
        }

        fun readyApkFile(context: Context): File? {
            val prefs = context.getSharedPreferences("snc", Context.MODE_PRIVATE)
            val path = prefs.getString(PREF_READY_PATH, null) ?: return null
            return File(path).takeIf { it.exists() }
        }
    }
}
