// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.ComponentCallbacks2
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.location.Geocoder
import android.location.LocationManager
import android.net.ConnectivityManager
import android.net.LocalServerSocket
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.net.Network
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.os.PowerManager
import android.system.Os
import android.system.OsConstants
import android.telephony.TelephonyManager
import android.os.HandlerThread
import android.util.Log
import android.widget.Toast
import androidx.core.content.ContextCompat
import java.io.File
import java.io.IOException
import java.net.InetAddress
import java.net.InetSocketAddress
import java.util.Locale
import java.util.concurrent.atomic.AtomicBoolean

class SNCVpnService : VpnService() {

    private var coreProcess: Process? = null
    private var protectServer: LocalServerSocket? = null
    private var protectServerThread: Thread? = null
    private var uidServer: LocalServerSocket? = null
    private var uidServerThread: Thread? = null
    private var tunPfd: ParcelFileDescriptor? = null
    private var lastKey: String? = null
    private var ipcPath: String? = null
    private var networkCallback: ConnectivityManager.NetworkCallback? = null
    // Network callbacks run on this background thread so Binder IPC inside
    // onAvailable/onLost (getNetworkCapabilities, etc.) never blocks the main thread.
    private var networkCallbackThread: HandlerThread? = null
    private var screenReceiver: BroadcastReceiver? = null
    private var wakeLock: PowerManager.WakeLock? = null
    private val stateWatchActive = AtomicBoolean(false)
    // Network-quality diagnostics (type/signal/bandwidth/location) for support logs —
    // see networkDiagnosticsLine(). lastLoggedNetDiag avoids re-logging on every
    // onCapabilitiesChanged callback (which can fire every few seconds) when nothing
    // meaningful actually changed; the heartbeat thread logs unconditionally on its
    // own interval so long stable sessions still have timestamped data points.
    private var lastLoggedNetDiag: String? = null
    private val netHeartbeatActive = AtomicBoolean(false)
    private var netHeartbeatThread: Thread? = null
    // Set false right before protectServer.close() in stopVpn() so protectLoop can
    // tell an intentional shutdown (stop retrying) apart from server.accept()
    // throwing for some transient reason while the service is still meant to be
    // running (log and keep accepting) -- see protectLoop's own comment.
    private val protectLoopActive = AtomicBoolean(false)
    // Same shutdown-vs-transient-accept()-error distinction as
    // protectLoopActive, for the per-app UID lookup socket.
    private val uidLoopActive = AtomicBoolean(false)

    override fun onCreate() {
        super.onCreate()
        KotlinLog.init(File(filesDir, "logs"))
        KotlinLog.log("SNCVpnService: created")
        createNotificationChannel()
        serviceInstance = this
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            KotlinLog.log("onStartCommand: ACTION_STOP — user disconnected")
            stopVpn()
            return START_NOT_STICKY
        }
        // Resolve key: prefer the one in the intent (normal start / reconnect),
        // fall back to the persisted key when Android restarts the service after
        // killing it (START_STICKY delivers intent=null in that case).
        val key = intent?.getStringExtra(EXTRA_KEY)
            ?: getSharedPreferences("snc", MODE_PRIVATE).getString(PREF_KEY, null)
            ?: run { stopSelf(); return START_NOT_STICKY }
        val startReason = when {
            intent == null                        -> "sticky-restart"
            intent.getStringExtra(EXTRA_KEY) != null -> "connect"
            else                                  -> "reconnect"
        }
        // Persist so the next sticky restart can recover without user interaction.
        getSharedPreferences("snc", MODE_PRIVATE).edit().putString(PREF_KEY, key).apply()
        KotlinLog.log("onStartCommand: $startReason gen=${generation + 1}")
        // Full teardown of any existing session before starting a new one.
        // Closes TUN fd, kills the Go process, deletes all state files, and
        // unregisters callbacks — so the new session always starts from a clean slate.
        stopVpn(selfStop = false)
        lastKey = key
        intentionalStop = false
        val gen = ++generation
        isRunning = false
        isConnecting = true
        isError = false
        lastError = null
        notifyState()
        startForeground(NOTIFICATION_ID, buildNotification())
        acquireWakeLock()
        // manual: true only for a genuine user-initiated connect (EXTRA_KEY present in
        // the intent, i.e. the "connect" branch of startReason above) -- "reconnect"
        // (internal) and "sticky-restart" (Android relaunching a killed service) are
        // both auto. Threaded through to the Go process as SNC_AUTO_RECONNECT so the
        // admin dashboard's connection-stats feature can count manual vs automatic
        // connects (see core.ConnStatsCollector).
        val manual = startReason == "connect"
        Thread({ startVpn(key, gen, manual) }, "snc-vpn-start").start()
        return START_STICKY
    }

    private fun startVpn(key: String, gen: Int, manual: Boolean = true) {
        // processExited: true if Go exited normally (any code); false if setup threw.
        // wasKeyDenied: captured before stopVpn() clears isKeyDenied.
        // exitState: last value of snc.state read synchronously after exit (may be empty).
        var processExited = false
        var wasKeyDenied = false
        var exitState = ""
        // fatalSetupError: true when setup failed in a way that requires user action
        // (binary missing).  Set inside try so catch can distinguish fatal from
        // transient setup exceptions.
        var fatalSetupError = false
        KotlinLog.log("startVpn: gen=$gen")
        try {
            Log.i(TAG, "startVpn: filesDir=${filesDir.absolutePath}")

            // 1. Locate Go binary (shipped as native library, already executable).
            Log.i(TAG, "step 1: locating binary")
            val binary = CoreProcess.binaryFile(this)
            if (!binary.exists()) { fatalSetupError = true; throw java.io.IOException("snc-core not found: ${binary.absolutePath}") }
            Log.i(TAG, "step 1 OK: ${binary.absolutePath} size=${binary.length()}")

            // 2026-08-07: the IPC command socket (reconnect/nettype/heartbeat/etc.)
            // used to be a plain filesystem-namespace Unix socket and, per real user
            // logs, silently never delivered a single command across multiple full
            // VPN sessions with confirmed network switches -- while this abstract-
            // namespace protect socket has never shown a comparable failure. Reusing
            // the same naming/namespace pattern for the ipc socket below.
            val ipcName = "snc.ipc.${android.os.Process.myPid()}.${gen}"

            // 2. Start protect socket server BEFORE launching Go.
            val protectName = "snc.protect.${android.os.Process.myPid()}.${gen}"
            Log.i(TAG, "step 2: protect socket @$protectName")
            val ps = LocalServerSocket(protectName)
            protectServer = ps
            protectLoopActive.set(true)
            protectServerThread = Thread({
                protectLoop(ps)
            }, "snc-protect").apply { isDaemon = true; start() }
            Log.i(TAG, "step 2 OK")

            // 2b. Start the per-app UID lookup socket server BEFORE launching Go --
            // same reasoning/pattern as the protect socket above. Used by
            // appStickyDialer (android-core/tun_dialer_linux.go) so one app's
            // parallel connections consistently exit through the same control.
            val uidName = "snc.uid.${android.os.Process.myPid()}.${gen}"
            Log.i(TAG, "step 2b: uid socket @$uidName")
            val us = LocalServerSocket(uidName)
            uidServer = us
            uidLoopActive.set(true)
            uidServerThread = Thread({
                uidLoop(us)
            }, "snc-uid").apply { isDaemon = true; start() }
            Log.i(TAG, "step 2b OK")

            // Read prefs and prepare paths early — needed by the main process.
            val snPrefs = getSharedPreferences("snc", MODE_PRIVATE)
            val disableUdp = snPrefs.getBoolean("disable_udp", false)
            val blockQuic = snPrefs.getBoolean("disable_quic", false)
            val disableBypass = snPrefs.getBoolean("disable_bypass", false)
            val disableIpv6 = snPrefs.getBoolean("disable_ipv6", false)
            val logDir = File(filesDir, "logs").also { it.mkdirs() }.absolutePath
            // Detect country once; reused by the main process.
            val countryCC = detectCountryCC(this)

            SncBackgroundService.instance?.pauseForVpn()

            // 3. Create the VPN TUN interface.
            Log.i(TAG, "step 3: VPN establish")
            val builder = Builder()
                .setSession(getString(R.string.app_name))
                .addAddress("10.0.0.2", 32)
                .addAddress("fd00::2", 128)
                .addDnsServer("77.88.8.8")  // Yandex — works in whitelist/RU networks where 8.8.8.8 is blocked
                .addDnsServer("8.8.8.8")
                .setMtu(1280)
            addSplitTunnelRoutes(builder)
            // Always exclude telephony/IMS packages so their IPv6 IMS signaling traffic
            // never enters the TUN.  If it did, gVisor would drop it, breaking the radio
            // PDN connection and causing the 4G/5G icon to flicker (observed on Verizon).
            // Non-installed packages are silently ignored by addDisallowedApplication.
            for (pkg in ExcludedApps.TELEPHONY_ALWAYS_EXCLUDED) {
                try { builder.addDisallowedApplication(pkg) } catch (_: Exception) {}
            }
            // Exclude user-selected apps from the tunnel so they reach the real network.
            // This prevents VPN detection by banking, marketplace, and government apps.
            val excluded = ExcludedApps.load(this)
            for (pkg in excluded) {
                try {
                    builder.addDisallowedApplication(pkg)
                } catch (_: Exception) {
                    // Package not installed — skip silently.
                }
            }
            if (excluded.isNotEmpty()) Log.i(TAG, "step 3: excluded ${excluded.size} apps + telephony from tunnel")
            // establish() can return null transiently after onRevoke() or a network
            // switch before the OS is ready to grant a new TUN fd.  Retry with
            // increasing delays rather than giving up immediately (which would fall
            // through to stopVpn/selfStop=true and leave the user without VPN).
            //
            // Diagnostics below are written to KotlinLog (not just Log.w/Logcat)
            // because the only artifact we ever get back from a client is the
            // KotlinLog-derived zip — there is no adb access to a remote device.
            val prepareIntent = VpnService.prepare(this)
            if (prepareIntent != null) {
                KotlinLog.log("startVpn: VpnService.prepare() returned non-null — VPN permission not granted/revoked")
            }
            val activeVpnInfo = activeForeignVpnInfo()
            if (activeVpnInfo != null) {
                KotlinLog.log("startVpn: another VPN network is currently active ($activeVpnInfo) — establish() will likely fail until it is disconnected")
            }
            var pfd: ParcelFileDescriptor? = null
            val establishDelaysMs = longArrayOf(500, 1000, 2000, 3000)
            for ((attempt, delay) in establishDelaysMs.withIndex()) {
                if (gen != generation) return
                pfd = builder.establish()
                if (pfd != null) break
                KotlinLog.log("startVpn: establish() returned null (attempt ${attempt + 1}/${establishDelaysMs.size}) — retrying in ${delay}ms")
                Log.w(TAG, "step 3: establish() returned null (attempt ${attempt + 1}/${establishDelaysMs.size}) — retrying in ${delay}ms")
                Thread.sleep(delay)
            }
            if (pfd == null) {
                KotlinLog.log("startVpn: establish failed after ${establishDelaysMs.size} attempts " +
                    "(prepareNeeded=${prepareIntent != null}, otherVpnActive=${activeVpnInfo != null})")
                // Neither case is transient — retrying establish() will not help until the
                // user acts, so surface a specific message instead of looping silently forever.
                when {
                    prepareIntent != null -> {
                        fatalSetupError = true
                        throw IOException(getString(R.string.error_vpn_permission_revoked))
                    }
                    activeVpnInfo != null -> {
                        fatalSetupError = true
                        throw IOException(getString(R.string.error_other_vpn_active))
                    }
                    else -> throw IOException("VPN establish() returned null after ${establishDelaysMs.size} attempts")
                }
            }
            // Bail out before wiring up the TUN if a newer attempt has already
            // superseded us — stopVpn() from that attempt may have already closed
            // protectServer, so launching the main process now would hand it a
            // protectSocket name pointing at a torn-down LocalServerSocket (the
            // process then runs unprotected, looping all its dials through this
            // very TUN and timing out forever).
            if (gen != generation) { pfd.close(); return }
            tunPfd = pfd
            KotlinLog.log("startVpn: TUN established")
            Log.i(TAG, "step 3 OK: pfd=$pfd")

            // 4. Get the raw fd without detaching (keep pfd alive to hold the TUN open).
            // forkExec will preserve this fd across execve; ProcessBuilder would close it.
            val tunFd = NativeHelper.fdToInt(pfd.fileDescriptor)
            Log.i(TAG, "step 4: tunFd=$tunFd, clearing CLOEXEC")
            NativeHelper.clearCloexec(tunFd)
            Log.i(TAG, "step 4 OK")

            // 5. Prepare IPC path. Clear snc.state from any previous run so a stale
            // key_error or key_denied doesn't suppress reconnect on the next exit.
            this.ipcPath = ipcName
            // Static mirror so non-service callers (MainActivity's club-theme
            // preview and Recommend dialog) can reach the running snc-core's
            // IPC socket without needing a bound-service reference.
            ipcPathStatic = ipcName
            try { File(filesDir, "snc.state").delete() } catch (_: Exception) {}
            Log.i(TAG, "step 5: logDir=$logDir ipc=@$ipcName")

            // 6. Start the Go core process.
            Log.i(TAG, "step 6: starting snc-core key=<${key.take(8)}…>")
            val proc = CoreProcess.start(
                context = this,
                key = key,
                tunFd = tunFd,
                protectSocket = "@$protectName",
                uidSocket = "@$uidName",
                ipcSocket = "@$ipcName",
                logDir = logDir,
                dataDir = filesDir.absolutePath,
                disableUdp = disableUdp,
                blockQuic = blockQuic,
                disableBypass = disableBypass,
                disableIpv6 = disableIpv6,
                countryCC = countryCC,
                manual = manual,
            )
            coreProcess = proc
            Log.i(TAG, "step 6 OK: process started")

            if (gen != generation) return  // superseded by a newer connection attempt
            // Pre-seed dot status from cache file existence so dots aren't briefly red on reconnect.
            // The Go process will overwrite these files once its own loadCached / refresh runs.
            cidrStatus = if (File(filesDir, "cidr.json").exists()) "cached" else "none"
            manifestStatus = if (File(filesDir, "manifest.json").exists()) "cached" else "none"
            isRunning = true
            isConnecting = false
            notifyState()
            registerNetworkCallback()
            registerScreenReceiver()
            startStateWatch()

            // 7. Block until the Go process exits, then clean up.
            val exitCode = proc.waitFor()
            Log.w(TAG, "snc-core exited code=$exitCode gen=$gen generation=$generation intentional=$intentionalStop")
            processExited = true
            // Read snc.state synchronously — the state-watch polls every 2 s and may not
            // have fired yet if the process exited quickly (e.g. invalid key at startup).
            exitState = try { File(filesDir, "snc.state").readText().trim() } catch (_: Exception) { "" }
            wasKeyDenied = (isKeyDenied || exitState == "key_denied") && gen == generation
            KotlinLog.log("startVpn: process exited code=$exitCode exitState='$exitState' keyDenied=$wasKeyDenied intentional=$intentionalStop")
        } catch (e: Exception) {
            Log.e(TAG, "startVpn FAILED", e)
            if (gen == generation) {
                // Distinguish fatal errors (need user action) from transient errors (retry).
                //
                // Fatal: the binary is missing (broken install) — show the message,
                // stop the service, and wait for the user to fix the issue.
                //
                // Transient: establish() returned null (network not ready after a switch),
                // socket setup failed, process launch failed, or any other IOException —
                // treat as a soft process exit so the reconnect path in finally fires
                // instead of stopVpn/selfStop=true which would delete the key and leave
                // the user permanently disconnected.
                KotlinLog.log("startVpn: exception fatal=$fatalSetupError intentional=$intentionalStop msg=${e.message}")
                when {
                    intentionalStop -> {
                        // User disconnected during setup — no error, no reconnect.
                        // processExited stays false; shouldReconnect is false via intentionalStop.
                    }
                    fatalSetupError -> {
                        lastError = e.message ?: e.javaClass.simpleName
                        isError = true
                    }
                    else -> {
                        // Transient setup error (establish() null, socket hiccup, etc.) —
                        // mark as soft exit so the reconnect path in finally fires instead of
                        // stopVpn/selfStop=true which would delete the key and permanently
                        // disconnect the user.
                        Log.w(TAG, "startVpn transient failure — will reconnect: ${e.message}")
                        processExited = true
                    }
                }
            }
        } finally {
            if (gen == generation) {
                // Reconnect on any unintentional Go process exit unless the key was denied or
                // invalid. Key issues require user action; all other exits (stall, unreachable
                // controls, DPI blocking, transient setup errors) are transient and self-heal
                // on reconnect.  processExited is set to true for transient catch exceptions
                // above so this path fires for them too.
                val wasKeyInvalid = exitState == "key_error"
                val shouldReconnect = processExited && !intentionalStop && !wasKeyDenied && !wasKeyInvalid
                KotlinLog.log("startVpn: finally shouldReconnect=$shouldReconnect processExited=$processExited intentional=$intentionalStop keyDenied=$wasKeyDenied keyInvalid=$wasKeyInvalid")
                val k = if (shouldReconnect) lastKey else null
                if (k != null) {
                    // Reconnect path: clean up the old connection but do NOT call stopSelf().
                    // Calling stopSelf() here and then startService() causes Android to deliver
                    // onStartCommand (starts new snc-core) THEN onDestroy (kills it). Keeping
                    // the service alive avoids the race; onStartCommand handles its own teardown.
                    stopVpn(selfStop = false)
                    Log.i(TAG, "vpn: process exited — reconnecting silently")
                    isConnecting = true
                    notifyState()
                    startService(Intent(this, SNCVpnService::class.java).putExtra(EXTRA_KEY, k))
                } else {
                    stopVpn()
                    if (wasKeyDenied) {
                        isError = true
                        lastError = getString(R.string.status_key_denied)
                        notifyState()
                    } else if (wasKeyInvalid) {
                        isError = true
                        lastError = getString(R.string.status_key_denied) // reuse denied string; both need user action
                        notifyState()
                    }
                }
            }
        }
    }

    // protectLoop accepts connections and handles protect() round-trips.
    //
    // Protocol (Go → Kotlin):
    //   send: 1 byte marker + SCM_RIGHTS(fd)
    //   recv: 1 byte ack (0x01)
    //
    // Two connection styles are supported:
    //   - Persistent (snc-core): one long-lived connection, many round-trips.
    //   - Per-fd (sing-box ProtectPath): a new connection for each fd, one round-trip then close.
    // Both are handled by looping on accept() and spawning a daemon thread per client.
    private fun protectLoop(server: LocalServerSocket) {
        // 2026-08-07: originally, any IOException out of server.accept() (not just a
        // real shutdown-time close) ended this whole while(true) loop for good --
        // every subsequent protect() call for the rest of the session then failed
        // silently, with only a "no protect socket" warning on the Go side and
        // nothing here to explain why. Confirmed as the live root cause of a
        // real user's bypassed (.ru TLD) connections permanently breaking mid-session
        // on Android only, while already-established connections (other apps) kept
        // working -- exactly the shape you'd expect once new sockets stop being
        // protect()'d and start getting captured by the VPN's own TUN interface.
        // Fixed the same way netHeartbeat's equivalent bug was fixed on 2026-08-06:
        // catch per-iteration and keep looping, distinguishing an intentional
        // shutdown (protectLoopActive set false right before stopVpn's server.close())
        // from a transient accept() failure (log, brief backoff, retry).
        while (protectLoopActive.get()) {
            val client: LocalSocket
            try {
                client = server.accept()
            } catch (e: IOException) {
                if (!protectLoopActive.get()) break // intentional shutdown, not a failure
                KotlinLog.log("protect: accept() failed, retrying: $e")
                try { Thread.sleep(1000) } catch (_: InterruptedException) { break }
                continue
            }
            Thread({
                val buf = ByteArray(1)
                try {
                    while (true) {
                        val n = client.inputStream.read(buf)
                        if (n < 0) break
                        // Ancillary file descriptors are available after the read.
                        val fds = client.ancillaryFileDescriptors
                        if (fds != null) {
                            for (fd in fds) {
                                val fdInt = NativeHelper.fdToInt(fd)
                                protect(fdInt)
                                try { Os.close(fd) } catch (_: Exception) {}
                            }
                        }
                        client.outputStream.write(1)
                    }
                } catch (e: IOException) {
                    // Usually benign (client closed its end) but logged anyway per
                    // the 2026-08-07 policy of not swallowing exceptions silently --
                    // this is exactly the kind of client-side error that was
                    // invisible until we started seeing "no protect socket" warnings
                    // on the Go side with nothing here to explain why.
                    KotlinLog.log("protect: client loop ended: $e")
                }
                finally { client.close() }
            }, "snc-protect-client").apply { isDaemon = true; start() }
        }
        try { server.close() } catch (_: IOException) {}
    }

    // uidLoop accepts connections and resolves per-connection app ownership
    // for android-core's appStickyDialer (per-app exit-IP stickiness, see
    // DialerPool.PickForUID on the Go side).
    //
    // Protocol (Go -> Kotlin), newline-delimited text, one round-trip per query:
    //   send: "<proto> <srcIP> <srcPort> <dstIP> <dstPort>\n"   (proto: "tcp"|"udp")
    //   recv: "<uid>\n"                                          (-1 if unresolved)
    //
    // Uses ConnectivityManager.getConnectionOwnerUid -- the API Android grants
    // to the app holding the active VpnService role (the same privilege level
    // that already lets addDisallowedApplication work for the per-app bypass
    // feature), replacing an earlier /proc/net/tcp-based approach on the Go
    // side that Android 11+ silently blocks for unprivileged processes
    // (confirmed live, 2026-08-17: it returned -1 for every single lookup on
    // a real device across two full sessions).
    private fun uidLoop(server: LocalServerSocket) {
        val cm = getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        while (uidLoopActive.get()) {
            val client: LocalSocket
            try {
                client = server.accept()
            } catch (e: IOException) {
                if (!uidLoopActive.get()) break // intentional shutdown, not a failure
                KotlinLog.log("uid: accept() failed, retrying: $e")
                try { Thread.sleep(1000) } catch (_: InterruptedException) { break }
                continue
            }
            Thread({
                try {
                    val reader = client.inputStream.bufferedReader()
                    val out = client.outputStream
                    while (true) {
                        val line = reader.readLine() ?: break
                        val uid = resolveConnectionOwnerUid(cm, line)
                        out.write("$uid\n".toByteArray())
                        out.flush()
                    }
                } catch (e: IOException) {
                    // Usually benign (Go closed its end on reconnect) but logged
                    // anyway per the same not-swallowing-exceptions policy as
                    // protectLoop above.
                    KotlinLog.log("uid: client loop ended: $e")
                } finally {
                    client.close()
                }
            }, "snc-uid-client").apply { isDaemon = true; start() }
        }
        try { server.close() } catch (_: IOException) {}
    }

    // resolveConnectionOwnerUid parses one request line and resolves it via
    // ConnectivityManager. Returns -1 on any parse or lookup failure -- the Go
    // side (DialerPool.PickForUID) treats that as "no stickiness for this one
    // connection", never as a shared sentinel that pins multiple connections
    // together -- see that function's doc comment for why that distinction
    // is exactly the bug this whole mechanism replaced.
    private fun resolveConnectionOwnerUid(cm: ConnectivityManager, line: String): Int {
        // getConnectionOwnerUid needs API 29 (Android 10); minSdk for this app is
        // 26. Below API 29 there's no way to resolve this at all -- degrade to
        // "unresolved" (no stickiness for this connection), not a crash.
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) return -1
        return try {
            val parts = line.trim().split(" ")
            if (parts.size != 5) return -1
            val proto = parts[0]
            val protoNum = when (proto) {
                "tcp" -> OsConstants.IPPROTO_TCP
                "udp" -> OsConstants.IPPROTO_UDP
                else -> return -1
            }
            val local = InetSocketAddress(InetAddress.getByName(parts[1]), parts[2].toInt())
            val remote = InetSocketAddress(InetAddress.getByName(parts[3]), parts[4].toInt())
            cm.getConnectionOwnerUid(protoNum, local, remote)
        } catch (e: Exception) {
            -1
        }
    }

    // onRevoke is called by Android when VPN permission is revoked (e.g. another VPN
    // takes over, or aggressive battery management reclaims the slot).  We attempt a
    // silent restart so the tunnel comes back without user interaction.
    override fun onRevoke() {
        KotlinLog.log("onRevoke: lastKey=${lastKey != null}")
        val k = lastKey
        if (k != null) {
            Log.w(TAG, "VPN permission revoked — restarting silently")
            // Tell Go about the revoke before killing it so the event is visible
            // in snc-core logs (destroy() sends SIGKILL with no log opportunity).
            sendIpc("{\"cmd\":\"notify\",\"args\":{\"msg\":\"vpn-revoked\"}}", "onRevoke")
            stopVpn(selfStop = false)
            startService(Intent(this, SNCVpnService::class.java).putExtra(EXTRA_KEY, k))
        } else {
            stopVpn()
        }
    }

    // selfStop=false is used by the reconnect path to clean up the old connection
    // without destroying the service, so the new onStartCommand is not raced by onDestroy.
    private fun stopVpn(selfStop: Boolean = true) {
        KotlinLog.log("stopVpn: selfStop=$selfStop")
        intentionalStop = true
        stateWatchActive.set(false)
        releaseWakeLock()
        unregisterNetworkCallback()
        unregisterScreenReceiver()
        isRunning = false
        isConnecting = false
        isKeyDenied = false
        isReconnecting = false
        isTunnelReady = false
        File(filesDir, "snc.state").delete()
        File(filesDir, "snc.traffic").delete()
        File(filesDir, "snc.browse").delete()
        // snc.cidr_status / snc.manifest_status are NOT deleted here: they reflect the
        // on-disk cache's freshness, not whether a tunnel is active. Manifest status keeps
        // updating via SncBackgroundService's own snc-core after this; CIDR bypass is
        // skipped entirely in proxy-only mode (no TUN to bypass), so its status simply
        // holds at whatever this session last wrote until the next VPN connection.
        cidrStatus = "none"
        manifestStatus = "none"
        notifIconRes = R.drawable.ic_notif_connected
        notifyState()
        protectLoopActive.set(false)
        protectServerThread?.interrupt()
        protectServer?.close()
        protectServer = null
        uidLoopActive.set(false)
        uidServerThread?.interrupt()
        uidServer?.close()
        uidServer = null
        // Best-effort: tell Go this session is ending (manual = selfStop -- a
        // genuine user/explicit disconnect vs. the teardown-before-reconnect case,
        // where selfStop=false and a new startVpn follows immediately) before
        // destroy() kills the process. Same fire-and-forget,
        // no-response-awaited shape as sendReconnect -- there is no cooperative
        // shutdown handshake here (destroy() sends SIGTERM right after this),
        // so this can race and lose the message on a slow/loaded device. Losing
        // an occasional connection-stats sample is an acceptable cost for a
        // feature that already tolerates whole 5-minute windows going missing
        // on crash (see ConnStatsCollector's doc comment) -- no worse than that.
        sendIpc("{\"cmd\":\"disconnect\",\"args\":{\"manual\":$selfStop}}", "disconnect")
        coreProcess?.destroy()
        coreProcess = null
        tunPfd?.close()
        tunPfd = null
        ipcPath = null
        ipcPathStatic = null
        if (selfStop) {
            // Clear in-memory key so onRevoke() does not restart after explicit disconnect.
            lastKey = null
            // Resume background keepalive on a separate thread — launchCore() calls forkExec()
            // which can be slow on memory-pressured devices and must not block the main thread.
            val bgSvc = SncBackgroundService.instance
            if (bgSvc != null) Thread({ bgSvc.resumeAfterVpn() }, "snc-bg-resume").start()
            // Erase the persisted key so a sticky restart does not silently
            // reconnect after the user explicitly disconnected.
            getSharedPreferences("snc", MODE_PRIVATE).edit().remove(PREF_KEY).apply()
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
        }
    }

    private fun currentNetworkType(): String {
        val cm = getSystemService(ConnectivityManager::class.java) ?: return "none"
        val net = cm.activeNetwork ?: return "none"
        val caps = cm.getNetworkCapabilities(net) ?: return "none"
        return when {
            caps.hasTransport(android.net.NetworkCapabilities.TRANSPORT_WIFI)     -> "wifi"
            caps.hasTransport(android.net.NetworkCapabilities.TRANSPORT_CELLULAR) -> "mobile"
            else -> "other"
        }
    }

    // Checks for a VPN network already active on the device before we attempt
    // establish().  Android only allows one active VpnService system-wide, so an
    // existing VPN network here (ours from a not-yet-torn-down prior generation,
    // or a different app's) explains a null establish() that no amount of retrying
    // will fix.  Returns a short description for logging, or null if no VPN is active.
    private fun activeForeignVpnInfo(): String? {
        val cm = getSystemService(ConnectivityManager::class.java) ?: return null
        val net = cm.activeNetwork ?: return null
        val caps = cm.getNetworkCapabilities(net) ?: return null
        if (!caps.hasTransport(android.net.NetworkCapabilities.TRANSPORT_VPN)) return null
        val transports = listOf(
            android.net.NetworkCapabilities.TRANSPORT_WIFI to "wifi",
            android.net.NetworkCapabilities.TRANSPORT_CELLULAR to "mobile",
            android.net.NetworkCapabilities.TRANSPORT_ETHERNET to "ethernet",
        ).filter { (t, _) -> caps.hasTransport(t) }.map { (_, name) -> name }
        return "underlying=${transports.ifEmpty { listOf("unknown") }}"
    }

    private fun sendNetworkType(type: String) {
        val json = """{"cmd":"nettype","args":{"type":"$type"}}"""
        sendIpc(json, "nettype/$type")
    }

    // Builds a one-line network-quality snapshot for support logs: transport type,
    // signal strength (dBm; needs API 29+, "n/a" below that), estimated downstream
    // bandwidth, and last-known coordinates (same source/permission as detectCountryCC —
    // no new permission, no extra location request, just reused last-known fix).
    // These logs are shared back to us via the in-app "Share Logs" zip, so this is the
    // one place coordinates end up in a file a user hands over — see the conversation
    // that added this for why that trade-off was made deliberately, not by accident.
    private fun networkDiagnosticsLine(): String {
        val cm = getSystemService(ConnectivityManager::class.java)
        val net = cm?.activeNetwork
        val caps = net?.let { cm.getNetworkCapabilities(it) }
        val type = when {
            caps == null -> "none"
            caps.hasTransport(android.net.NetworkCapabilities.TRANSPORT_WIFI)     -> "wifi"
            caps.hasTransport(android.net.NetworkCapabilities.TRANSPORT_CELLULAR) -> "mobile"
            else -> "other"
        }
        val signal = if (caps != null && android.os.Build.VERSION.SDK_INT >= 29) {
            caps.signalStrength.takeIf { it != Int.MIN_VALUE }?.let { "${it}dBm" } ?: "n/a"
        } else "n/a"
        val downKbps = caps?.linkDownstreamBandwidthKbps?.takeIf { it > 0 }?.let { "${it}kbps" } ?: "n/a"
        val loc = lastKnownLocationOrNull()
        val coords = if (loc != null) "%.5f,%.5f".format(loc.latitude, loc.longitude) else "n/a"
        return "type=$type signal=$signal down=$downKbps coords=$coords"
    }

    // Same permission/provider fallback as detectCountryCC() — degrades to null if
    // location permission isn't granted or no fix is cached yet.
    private fun lastKnownLocationOrNull(): android.location.Location? {
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.ACCESS_COARSE_LOCATION)
                != PackageManager.PERMISSION_GRANTED) return null
        return try {
            val lm = getSystemService(Context.LOCATION_SERVICE) as LocationManager
            lm.getLastKnownLocation(LocationManager.GPS_PROVIDER)
                ?: lm.getLastKnownLocation(LocationManager.NETWORK_PROVIDER)
        } catch (e: Exception) {
            null
        }
    }

    private fun registerNetworkCallback() {
        try {
            val cm = getSystemService(ConnectivityManager::class.java) ?: return
            val cb = object : ConnectivityManager.NetworkCallback() {
                override fun onAvailable(network: Network) {
                    val type = currentNetworkType()
                    KotlinLog.log("network: AVAILABLE ${networkDiagnosticsLine()}")
                    sendNetworkType(type)
                    sendReconnect("onAvailable/$type")
                }
                override fun onLost(network: Network) {
                    // caps for the lost network are already gone by the time this fires,
                    // so this logs whatever the (new, if any) active network looks like —
                    // lastLoggedNetDiag/the heartbeat carry the last-seen-good snapshot.
                    KotlinLog.log("network: LOST last-good=${lastLoggedNetDiag ?: "n/a"}")
                    sendNetworkType("none")
                    sendReconnect("onLost")
                }
                override fun onCapabilitiesChanged(network: Network, caps: android.net.NetworkCapabilities) {
                    val line = networkDiagnosticsLine()
                    if (line != lastLoggedNetDiag) {
                        KotlinLog.log("network: $line")
                        lastLoggedNetDiag = line
                    }
                }
            }
            val ht = HandlerThread("snc-network-cb").also { it.start() }
            networkCallbackThread = ht
            cm.registerDefaultNetworkCallback(cb, Handler(ht.looper))
            networkCallback = cb
            Log.i(TAG, "network callback registered")
            KotlinLog.log("network: callback registered")
            // Log the current network type at registration time.
            val line = networkDiagnosticsLine()
            KotlinLog.log("network: $line")
            lastLoggedNetDiag = line
            sendNetworkType(currentNetworkType())
            startNetHeartbeat()
        } catch (e: Exception) {
            // 2026-08-07: if THIS fails, no reconnect-on-network-change ever fires for
            // the entire session, and previously the only trace was Logcat -- gone by
            // the time a user sends us a log ZIP. Must survive into the file log.
            Log.w(TAG, "registerNetworkCallback failed: $e")
            KotlinLog.log("network: callback registration FAILED: $e")
        }
    }

    // Logs a network-diagnostics snapshot every 60s regardless of whether anything
    // changed, so long stable sessions (no onCapabilitiesChanged at all — see the
    // flapping investigation that motivated this) still leave a timeline to correlate
    // against user-reported "it stalled around X" reports.
    private fun startNetHeartbeat() {
        netHeartbeatActive.set(true)
        netHeartbeatThread = Thread({
            while (netHeartbeatActive.get()) {
                try { Thread.sleep(60_000) } catch (_: InterruptedException) { break }
                if (!netHeartbeatActive.get()) break
                // 2026-08-06 incident: an uncaught exception from networkDiagnosticsLine()
                // (e.g. a transient ConnectivityManager/LocationManager binder error) would
                // silently kill this whole daemon thread -- no heartbeat, ever again, for
                // the rest of the process's life, with no trace in Logcat. Catch and log
                // instead of letting one bad iteration end the loop permanently.
                try {
                    KotlinLog.log("network: heartbeat ${networkDiagnosticsLine()}")
                } catch (e: Exception) {
                    Log.w(TAG, "net heartbeat iteration failed: $e")
                }
                // Independent of the file write above: lets Go's own watchdog notice
                // if this thread goes quiet, since Go's log has proven reliable even
                // when snc_lifecycle.log silently stopped during the 2026-08-06 incident.
                sendIpc("{\"cmd\":\"heartbeat\"}", "heartbeat")
            }
        }, "snc-net-heartbeat").apply { isDaemon = true; start() }
    }

    private fun stopNetHeartbeat() {
        netHeartbeatActive.set(false)
        netHeartbeatThread?.interrupt()
        netHeartbeatThread = null
    }

    private fun registerScreenReceiver() {
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) {
                when (intent.action) {
                    Intent.ACTION_SCREEN_OFF -> sendIpc("{\"cmd\":\"screen-off\"}", "screen-off")
                    Intent.ACTION_SCREEN_ON  -> sendIpc("{\"cmd\":\"screen-on\"}",  "screen-on")
                }
            }
        }
        val filter = IntentFilter().apply {
            addAction(Intent.ACTION_SCREEN_OFF)
            addAction(Intent.ACTION_SCREEN_ON)
        }
        registerReceiver(receiver, filter)
        screenReceiver = receiver
        Log.i(TAG, "screen receiver registered")
    }

    private fun unregisterScreenReceiver() {
        screenReceiver?.let {
            try { unregisterReceiver(it) } catch (_: Exception) {}
            screenReceiver = null
            Log.i(TAG, "screen receiver unregistered")
        }
    }

    private fun unregisterNetworkCallback() {
        stopNetHeartbeat()
        val cm = getSystemService(ConnectivityManager::class.java) ?: return
        networkCallback?.let {
            try { cm.unregisterNetworkCallback(it) } catch (_: Exception) {}
            networkCallback = null
            Log.i(TAG, "network callback unregistered")
        }
        networkCallbackThread?.quitSafely()
        networkCallbackThread = null
    }

    // Poll $filesDir/snc.state, snc.notif, and snc.traffic every 2 s.
    // snc.state:   Go writes "ok" or "key_denied" to signal auth status.
    // snc.notif:   Go writes a JSON array of new broadcast message strings; consumed once.
    // snc.traffic: Go writes unix timestamp of last data transfer; drives icon animation.
    private fun startStateWatch() {
        stateWatchActive.set(true)
        val stateFile = File(filesDir, "snc.state")
        val notifFile = File(filesDir, "snc.notif")
        val trafficFile = File(filesDir, "snc.traffic")
        val toastFile = File(filesDir, "snc.toast")
        val cidrStatusFile = File(filesDir, "snc.cidr_status")
        val manifestStatusFile = File(filesDir, "snc.manifest_status")
        Thread({
            var txPhase = true
            while (stateWatchActive.get()) {
                try {
                    if (stateFile.exists()) {
                        val state = stateFile.readText().trim()
                        val denied = state == "key_denied"
                        val reconnecting = state == "connecting"
                        val tunnelOk = state == "ok"
                        if (denied != isKeyDenied) {
                            isKeyDenied = denied
                            if (denied) {
                                // Clearing the saved key is what actually gets the
                                // user back to a login screen: ConnectionFragment's
                                // showKeyEntry/showHaveKeyPrompt/showCredentialLogin
                                // are all gated on hasKey alone (prefs "key" being
                                // non-empty), not on isKeyDenied -- without this,
                                // isKeyDenied only changed the icon/status text and
                                // left the connect screen up with a dead-end error,
                                // same bug the Windows/Mac/Linux clients had for
                                // their own "login error" state (2026-08-16).
                                getSharedPreferences("snc", Context.MODE_PRIVATE).edit().remove("key").apply()
                            }
                            Handler(Looper.getMainLooper()).post { notifyState() }
                        }
                        if (reconnecting != isReconnecting) {
                            isReconnecting = reconnecting
                            Handler(Looper.getMainLooper()).post { notifyState() }
                        }
                        if (tunnelOk && !isTunnelReady) {
                            isTunnelReady = true
                            Handler(Looper.getMainLooper()).post { notifyState() }
                        }
                    }
                    if (notifFile.exists()) {
                        val raw = notifFile.readText().trim()
                        notifFile.delete()
                        showBroadcastNotifications(raw)
                    }
                    if (toastFile.exists()) {
                        val msg = toastFile.readText().trim()
                        toastFile.delete()
                        if (msg.isNotEmpty()) {
                            Handler(Looper.getMainLooper()).post {
                                Toast.makeText(this@SNCVpnService, msg, Toast.LENGTH_SHORT).show()
                            }
                        }
                    }
                    // Animate notification icon: alternate TX/RX arrows when data flowed
                    // within the last 4 s; revert to shield when the tunnel is idle.
                    if (!isKeyDenied) {
                        val nowSecs = System.currentTimeMillis() / 1000L
                        val lastDataSecs = trafficFile.takeIf { it.exists() }
                            ?.readText()?.trim()?.toLongOrNull() ?: 0L
                        val newIcon = if (nowSecs - lastDataSecs < 4L) {
                            txPhase = !txPhase
                            if (txPhase) R.drawable.ic_notif_tx else R.drawable.ic_notif_rx
                        } else {
                            R.drawable.ic_notif_connected
                        }
                        if (newIcon != notifIconRes) {
                            notifIconRes = newIcon
                            getSystemService(NotificationManager::class.java)
                                .notify(NOTIFICATION_ID, buildNotification())
                        }
                    }
                    val newCidr = try { cidrStatusFile.readText().trim() } catch (_: Exception) { "none" }
                    val newManifest = try { manifestStatusFile.readText().trim() } catch (_: Exception) { "none" }
                    if (newCidr != cidrStatus || newManifest != manifestStatus) {
                        cidrStatus = newCidr
                        manifestStatus = newManifest
                        Handler(Looper.getMainLooper()).post { notifyState() }
                    }
                    Thread.sleep(2000)
                } catch (_: InterruptedException) { break }
                  catch (e: Exception) { KotlinLog.log("state-watch: iteration failed: $e") }
            }
        }, "snc-state-watch").apply { isDaemon = true; start() }
    }

    override fun onTrimMemory(level: Int) {
        super.onTrimMemory(level)
        if (level >= ComponentCallbacks2.TRIM_MEMORY_RUNNING_LOW) {
            Log.i(TAG, "onTrimMemory level=$level — sending trim to core")
            sendIpc("{\"cmd\":\"trim\"}", "trim")
        }
    }

    // sendReconnect fires {"cmd":"reconnect"} at the Go IPC socket.
    private fun sendReconnect(reason: String) = sendIpc("{\"cmd\":\"reconnect\"}", "reconnect/$reason")

    // sendIpc sends a single JSON line to the Go IPC socket.
    private fun sendIpc(json: String, tag: String) {
        val path = ipcPath
        if (path == null) {
            // 2026-08-07: a real user's logs showed *zero* IPC commands of any kind
            // (reconnect, nettype, screen-on/off) reaching Go across three
            // full VPN sessions with confirmed network switches -- but the only trace
            // of a failure was ever going to Logcat via the catch below, which isn't
            // in the log ZIP users send us. Logging every path here (including this
            // "no path yet" case, which silently no-op'd before) to KotlinLog so the
            // next report actually shows which failure mode it is.
            KotlinLog.log("ipc: $tag skipped -- no ipcPath set yet")
            return
        }
        Thread({
            // 2026-08-11: ipcPath is set right before CoreProcess.start() launches the Go
            // binary, and registerNetworkCallback() is wired up immediately after -- but
            // Go's own IPC listener only binds fairly late in its startup sequence (after
            // DHT bootstrap, dialer pool setup, etc., per real session logs: "IPC at
            // @snc.ipc..." is one of the last startup lines, not the first). A network
            // event landing in that window (very plausible: onAvailable can fire almost
            // immediately if a network is already up when the callback registers) hit a
            // bare, single-shot connect() and was lost forever -- this was the single
            // largest error class in a real user's 24h logs (140+ "Connection refused").
            // Same retry-with-backoff shape as the establish() race fix above (line ~292).
            val retryDelaysMs = longArrayOf(0, 100, 300, 800)
            var lastErr: Exception? = null
            var sent = false
            for ((attempt, delay) in retryDelaysMs.withIndex()) {
                if (sent) break
                if (delay > 0) Thread.sleep(delay)
                try {
                    val sock = LocalSocket()
                    // Abstract namespace, matching protectServer/protectLoop's proven-
                    // reliable pattern (see the 2026-08-07 note where the filesystem-
                    // namespace version of this socket never delivered a single command
                    // in a real user's session). path is the bare name (no "@").
                    sock.connect(LocalSocketAddress(path, LocalSocketAddress.Namespace.ABSTRACT))
                    sock.outputStream.write("$json\n".toByteArray())
                    sock.outputStream.flush()
                    sock.close()
                    if (attempt > 0) KotlinLog.log("ipc: $tag OK on retry ${attempt + 1}/${retryDelaysMs.size}")
                    sent = true
                } catch (e: Exception) {
                    lastErr = e
                }
            }
            if (!sent) {
                Log.w(TAG, "sendIpc $tag: $lastErr")
                KotlinLog.log("ipc: $tag FAILED after ${retryDelaysMs.size} attempts path=$path: $lastErr")
            }
        }, "snc-ipc").apply { isDaemon = true; start() }
    }

    private fun acquireWakeLock() {
        if (wakeLock?.isHeld == true) return
        val pm = getSystemService(PowerManager::class.java)
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "SNC:VpnWakeLock").apply {
            setReferenceCounted(false)
            acquire()
        }
        Log.i(TAG, "wake lock acquired")
    }

    private fun releaseWakeLock() {
        wakeLock?.let {
            if (it.isHeld) {
                it.release()
                Log.i(TAG, "wake lock released")
            }
            wakeLock = null
        }
    }

    override fun onDestroy() {
        KotlinLog.log("SNCVpnService: destroyed intentional=$intentionalStop")
        serviceInstance = null
        if (!intentionalStop) {
            // Android killed the service (battery optimizer, network change, memory pressure)
            // — not a user action.
            //
            // Increment generation FIRST so the startVpn() background thread's finally block
            // sees gen != generation and exits without calling stopVpn(selfStop=true), which
            // would erase "last_vpn_key".  Without this, stopVpn(selfStop=false) below sets
            // intentionalStop=true, which makes shouldReconnect=false in the finally block,
            // which then calls stopVpn() (default selfStop=true) and erases the key.
            ++generation
            // Clean up resources without erasing the persisted key.  START_STICKY will
            // restart onStartCommand() with intent=null, read the key, and reconnect.
            stopVpn(selfStop = false)
            // Resume background keepalive on a separate thread — launchCore() calls forkExec()
            // which can be slow on memory-pressured devices and must not block the main thread.
            val bgSvc = SncBackgroundService.instance
            if (bgSvc != null) Thread({ bgSvc.resumeAfterVpn() }, "snc-bg-resume").start()
        } else {
            // User explicitly disconnected — full cleanup, erase key so sticky restart
            // does not silently reconnect against the user's intent.
            stopVpn(selfStop = true)
        }
        super.onDestroy()
    }

    // ── Notification ─────────────────────────────────────────────────────────

    private fun createNotificationChannel() {
        val nm = getSystemService(NotificationManager::class.java)
        val ch = NotificationChannel(
            CHANNEL_ID,
            getString(R.string.channel_name),
            NotificationManager.IMPORTANCE_LOW
        ).apply { description = getString(R.string.notification_text) }
        nm.createNotificationChannel(ch)
        val broadcastCh = NotificationChannel(
            CHANNEL_BROADCAST,
            "Tunnel Cat Alerts",
            NotificationManager.IMPORTANCE_DEFAULT
        )
        nm.createNotificationChannel(broadcastCh)
    }

    // Show each message in the JSON array as a separate Android notification.
    // The file has already been consumed (deleted) by the caller.
    private fun showBroadcastNotifications(raw: String) {
        try {
            val arr = org.json.JSONArray(raw)
            val nm = getSystemService(NotificationManager::class.java)
            val openIntent = PendingIntent.getActivity(
                this, NOTIFICATION_BROADCAST_BASE,
                Intent(this, MainActivity::class.java).apply {
                    flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
                    putExtra(MainActivity.EXTRA_NAVIGATE_TAB, 0)
                },
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
            )
            for (i in 0 until arr.length()) {
                val msg = arr.getString(i)
                val n = Notification.Builder(this, CHANNEL_BROADCAST)
                    .setContentTitle("Tunnel Cat")
                    .setContentText(msg)
                    .setSmallIcon(android.R.drawable.ic_dialog_info)
                    .setContentIntent(openIntent)
                    .setAutoCancel(true)
                    .setStyle(Notification.BigTextStyle().bigText(msg))
                    .build()
                nm.notify(NOTIFICATION_BROADCAST_BASE + i, n)
            }
        } catch (_: Exception) {}
    }

    private fun buildNotification(): Notification {
        val stopIntent = PendingIntent.getService(
            this, 0,
            Intent(this, SNCVpnService::class.java).apply { action = ACTION_STOP },
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )
        val openIntent = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java).apply {
                flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
                putExtra(MainActivity.EXTRA_NAVIGATE_TAB, 0)
            },
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )
        val (text, icon) = if (isKeyDenied)
            getString(R.string.notification_text_key_denied) to android.R.drawable.ic_dialog_alert
        else
            getString(R.string.notification_text) to notifIconRes

        return Notification.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.notification_title))
            .setContentText(text)
            .setSmallIcon(icon)
            .setContentIntent(openIntent)
            .addAction(
                Notification.Action.Builder(
                    null, getString(R.string.disconnect), stopIntent
                ).build()
            )
            .setOngoing(true)
            .build()
    }

    private fun notifyState() {
        if (isRunning || isConnecting) {
            getSystemService(NotificationManager::class.java)
                .notify(NOTIFICATION_ID, buildNotification())
        }
        sendBroadcast(Intent(ACTION_STATE_CHANGED).setPackage(packageName))
    }

    // addSplitTunnelRoutes routes all traffic through the TUN interface.
    // Home-region bypass is handled per-connection by the Go core (BypassManager),
    // which dials home-country IPs directly via the protect mechanism.
    private fun addSplitTunnelRoutes(builder: Builder) {
        builder.addRoute("0.0.0.0", 0)
        // Always capture IPv6 into the TUN, regardless of whether IPv6 is
        // meant to be "disabled" (arbiter kill switch or user preference).
        // Omitting this route is NOT the same as blocking IPv6: with no ::/0
        // route on the VPN interface, the OS simply routes IPv6-destined
        // packets via the real underlying network interface instead --
        // completely bypassing the VPN. Confirmed as a real, live traffic
        // leak 2026-08-16 (arbiter's ipv6_enabled=0, yet IPv6 packets left
        // the device directly). The actual enforcement now happens on the Go
        // side, per-dial, in appStickyDialer.ipv6Blocked (see
        // android-core/tun_dialer_linux.go and main_linux.go's
        // SNC_DISABLE_IPV6) -- traffic is captured here and then either
        // relayed or dropped, never left to the OS's own routing table.
        // Mirrors the same fix already shipped for Windows (routes.go,
        // 2026-08-15, Windows Firewall block) and now Mac/Linux.
        builder.addRoute("::", 0)
    }

    companion object {
        const val ACTION_STOP = "com.shortnerdcat.snc.STOP"
        // SharedPreferences key used to persist the subscription key across
        // Android process kills so START_STICKY can recover automatically.
        private const val PREF_KEY = "last_vpn_key"
        const val ACTION_STATE_CHANGED = "com.shortnerdcat.snc.STATE_CHANGED"
        const val EXTRA_KEY = "key"
        private const val NOTIFICATION_ID = 1
        private const val CHANNEL_ID = "vpn_status"
        private const val CHANNEL_BROADCAST = "snc_broadcast"
        private const val NOTIFICATION_BROADCAST_BASE = 100
        private const val TAG = "SNCVpnService"

        @Volatile
        var notifIconRes: Int = R.drawable.ic_notif_connected
            private set

        // Static mirror of the running instance's ipcPath -- see the write
        // site in startVpn() for why. null whenever no tunnel process is up.
        @Volatile
        var ipcPathStatic: String? = null
            internal set

        // sendIpcWithResponse is the request/response counterpart to the
        // instance-only fire-and-forget sendIpc() above: MainActivity's club
        // theme preview and Recommend dialog need to know whether the
        // command actually succeeded (e.g. "not admin", "no account with
        // that username"), not just that the socket write didn't throw.
        // Blocks the calling thread -- always invoke from a background
        // dispatcher. Returns null on any connect/write/read/timeout failure.
        fun sendIpcWithResponse(json: String, tag: String, timeoutMs: Int = 8000): String? {
            val path = ipcPathStatic ?: run {
                KotlinLog.log("ipc: $tag skipped -- no running tunnel (ipcPathStatic unset)")
                return null
            }
            return try {
                val sock = LocalSocket()
                sock.connect(LocalSocketAddress(path, LocalSocketAddress.Namespace.ABSTRACT))
                sock.soTimeout = timeoutMs
                sock.outputStream.write("$json\n".toByteArray())
                sock.outputStream.flush()
                val line = sock.inputStream.bufferedReader().readLine()
                sock.close()
                line
            } catch (e: Exception) {
                Log.w(TAG, "sendIpcWithResponse $tag: $e")
                KotlinLog.log("ipc: $tag FAILED path=$path: $e")
                null
            }
        }

        @Volatile
        var isRunning: Boolean = false
            private set

        @Volatile
        var isConnecting: Boolean = false
            private set

        @Volatile
        var isError: Boolean = false
            private set

        @Volatile
        var lastError: String? = null
            private set

        @Volatile
        var isKeyDenied: Boolean = false
            private set

        // True while snc.state == "connecting": the tunnel is reconnecting after a
        // network change. The UI shows a spinner instead of "Connected" to avoid
        // misleading the user while reconnect is in progress.
        @Volatile
        var isReconnecting: Boolean = false
            private set

        // True once snc.state first reaches "ok": the tunnel is established and
        // traffic can flow.  Reset to false on disconnect.
        @Volatile
        var isTunnelReady: Boolean = false
            private set

        // CIDR bypass list status: "none" / "cached" / "fresh".
        @Volatile
        var cidrStatus: String = "none"
            internal set

        // Manifest (control node list) status: "none" / "cached" / "fresh".
        @Volatile
        var manifestStatus: String = "none"
            internal set

        @Volatile
        private var intentionalStop: Boolean = false

        @Volatile
        private var generation: Int = 0

        @Volatile
        private var serviceInstance: SNCVpnService? = null

        // protectIfActive lets other in-process components (e.g. UpdateChecker, which
        // isn't itself a VpnService) bypass the VPN tunnel for their own sockets.
        // Returns true when the socket is safe to
        // use: either genuinely protected, or there's no active VPN to loop into in the
        // first place (full-tunnel routing only exists while this service is up).
        // Returns false only when a VPN *is* active and protect() itself failed --
        // callers must not proceed in that case, or the socket will route into the
        // TUN and hang/loop instead of failing cleanly.
        fun protectIfActive(socket: java.net.Socket): Boolean {
            val s = serviceInstance ?: return true
            return s.protect(socket)
        }

        // Whether a VpnService instance is currently registered -- purely
        // informational (e.g. so UpdateChecker's diagnostic log can record
        // "was the VPN up at the moment this attempt was made" without
        // needing its own separate tracking of connection state).
        fun isActive(): Boolean = serviceInstance != null
    }
}

/**
 * Detects the device's current physical country code using the best available
 * source, in priority order:
 *  1. GPS last-known location → Geocoder (offline, country-level)
 *  2. Cell network registration country (no permission needed)
 *  3. System timezone → country (no permission needed)
 *
 * Returns an uppercase ISO 3166-1 alpha-2 code (e.g. "RU") or null if none
 * of the sources yield a result.
 */
fun detectCountryCC(ctx: Context): String? {
    // 1. GPS via last-known location (fast: uses cached fix, no network round-trip).
    //    Requires ACCESS_COARSE_LOCATION; degrades gracefully if denied.
    if (ContextCompat.checkSelfPermission(ctx, Manifest.permission.ACCESS_COARSE_LOCATION)
            == PackageManager.PERMISSION_GRANTED) {
        try {
            val lm = ctx.getSystemService(Context.LOCATION_SERVICE) as LocationManager
            val loc = lm.getLastKnownLocation(LocationManager.GPS_PROVIDER)
                ?: lm.getLastKnownLocation(LocationManager.NETWORK_PROVIDER)
            if (loc != null && Geocoder.isPresent()) {
                @Suppress("DEPRECATION")
                val addrs = Geocoder(ctx).getFromLocation(loc.latitude, loc.longitude, 1)
                val cc = addrs?.firstOrNull()?.countryCode?.uppercase()
                if (!cc.isNullOrBlank()) {
                    Log.i("SNCVpnService", "detectCountryCC: GPS→$cc")
                    return cc
                }
            }
        } catch (e: Exception) {
            Log.w("SNCVpnService", "detectCountryCC: GPS failed: $e")
        }
    }

    // 2. Cell network country: the ISO code of the network the device is registered on.
    //    Updates when roaming internationally. No permission required.
    try {
        val tm = ctx.getSystemService(Context.TELEPHONY_SERVICE) as TelephonyManager
        val cc = tm.networkCountryIso?.uppercase()
        if (!cc.isNullOrBlank()) {
            Log.i("SNCVpnService", "detectCountryCC: network→$cc")
            return cc
        }
    } catch (e: Exception) {
        Log.w("SNCVpnService", "detectCountryCC: TelephonyManager failed: $e")
    }

    // 3. Timezone → country (covers WiFi-only devices and tablets without SIM).
    val tzId = java.util.TimeZone.getDefault().id
    val cc = tzToCountry[tzId]
    if (cc != null) {
        Log.i("SNCVpnService", "detectCountryCC: timezone($tzId)→$cc")
        return cc
    }

    Log.w("SNCVpnService", "detectCountryCC: no country detected")
    return null
}

// tzToCountry mirrors win-client/core/tz_country.go for use in Kotlin.
// Only the most common IANA timezone names are included; GPS and network
// country sources handle the rest.
private val tzToCountry: Map<String, String> = mapOf(
    "Europe/Moscow" to "RU", "Europe/Kaliningrad" to "RU", "Europe/Samara" to "RU",
    "Europe/Volgograd" to "RU", "Europe/Astrakhan" to "RU", "Europe/Saratov" to "RU",
    "Europe/Ulyanovsk" to "RU", "Europe/Kirov" to "RU",
    "Asia/Yekaterinburg" to "RU", "Asia/Omsk" to "RU", "Asia/Novosibirsk" to "RU",
    "Asia/Novokuznetsk" to "RU", "Asia/Krasnoyarsk" to "RU", "Asia/Barnaul" to "RU",
    "Asia/Tomsk" to "RU", "Asia/Irkutsk" to "RU", "Asia/Chita" to "RU",
    "Asia/Yakutsk" to "RU", "Asia/Khandyga" to "RU", "Asia/Vladivostok" to "RU",
    "Asia/Ust-Nera" to "RU", "Asia/Magadan" to "RU", "Asia/Sakhalin" to "RU",
    "Asia/Srednekolymsk" to "RU", "Asia/Kamchatka" to "RU", "Asia/Anadyr" to "RU",
    "Europe/Simferopol" to "UA", "Europe/Kiev" to "UA", "Europe/Kyiv" to "UA",
    "Europe/Uzhgorod" to "UA", "Europe/Zaporozhye" to "UA",
    "Europe/Minsk" to "BY",
    "Europe/Vilnius" to "LT", "Europe/Riga" to "LV", "Europe/Tallinn" to "EE",
    "Europe/Warsaw" to "PL", "Europe/Prague" to "CZ", "Europe/Bratislava" to "SK",
    "Europe/Budapest" to "HU", "Europe/Bucharest" to "RO", "Europe/Sofia" to "BG",
    "Europe/Berlin" to "DE", "Europe/Vienna" to "AT", "Europe/Zurich" to "CH",
    "Europe/Paris" to "FR", "Europe/Brussels" to "BE", "Europe/Amsterdam" to "NL",
    "Europe/Luxembourg" to "LU", "Europe/London" to "GB", "Europe/Dublin" to "IE",
    "Europe/Lisbon" to "PT", "Europe/Madrid" to "ES", "Europe/Rome" to "IT",
    "Europe/Athens" to "GR", "Europe/Istanbul" to "TR", "Europe/Helsinki" to "FI",
    "Europe/Stockholm" to "SE", "Europe/Oslo" to "NO", "Europe/Copenhagen" to "DK",
    "Europe/Belgrade" to "RS", "Europe/Zagreb" to "HR", "Europe/Sarajevo" to "BA",
    "Europe/Ljubljana" to "SI", "Europe/Skopje" to "MK", "Europe/Podgorica" to "ME",
    "Europe/Tirane" to "AL", "Europe/Chisinau" to "MD",
    "Asia/Shanghai" to "CN", "Asia/Urumqi" to "CN",
    "Asia/Tokyo" to "JP", "Asia/Seoul" to "KR", "Asia/Pyongyang" to "KP",
    "Asia/Kolkata" to "IN", "Asia/Karachi" to "PK", "Asia/Dhaka" to "BD",
    "Asia/Colombo" to "LK", "Asia/Kathmandu" to "NP",
    "Asia/Bangkok" to "TH", "Asia/Ho_Chi_Minh" to "VN", "Asia/Phnom_Penh" to "KH",
    "Asia/Vientiane" to "LA", "Asia/Yangon" to "MM",
    "Asia/Jakarta" to "ID", "Asia/Makassar" to "ID", "Asia/Jayapura" to "ID",
    "Asia/Kuala_Lumpur" to "MY", "Asia/Kuching" to "MY",
    "Asia/Singapore" to "SG", "Asia/Manila" to "PH", "Asia/Taipei" to "TW",
    "Asia/Hong_Kong" to "HK", "Asia/Macau" to "MO",
    "Asia/Dubai" to "AE", "Asia/Riyadh" to "SA", "Asia/Baghdad" to "IQ",
    "Asia/Tehran" to "IR", "Asia/Jerusalem" to "IL", "Asia/Beirut" to "LB",
    "Asia/Amman" to "JO", "Asia/Damascus" to "SY", "Asia/Kuwait" to "KW",
    "Asia/Qatar" to "QA", "Asia/Bahrain" to "BH", "Asia/Muscat" to "OM",
    "Asia/Aden" to "YE", "Asia/Kabul" to "AF",
    "Asia/Tashkent" to "UZ", "Asia/Samarkand" to "UZ",
    "Asia/Almaty" to "KZ", "Asia/Aqtobe" to "KZ", "Asia/Aqtau" to "KZ",
    "Asia/Oral" to "KZ", "Asia/Qostanay" to "KZ", "Asia/Qyzylorda" to "KZ",
    "Asia/Tbilisi" to "GE", "Asia/Baku" to "AZ", "Asia/Yerevan" to "AM",
    "Asia/Bishkek" to "KG", "Asia/Dushanbe" to "TJ", "Asia/Ashgabat" to "TM",
    "Asia/Ulaanbaatar" to "MN",
    "America/New_York" to "US", "America/Chicago" to "US", "America/Denver" to "US",
    "America/Los_Angeles" to "US", "America/Anchorage" to "US", "America/Adak" to "US",
    "America/Phoenix" to "US", "America/Boise" to "US", "America/Detroit" to "US",
    "America/Juneau" to "US", "America/Nome" to "US", "America/Sitka" to "US",
    "America/Metlakatla" to "US", "America/Yakutat" to "US", "America/Menominee" to "US",
    "Pacific/Honolulu" to "US",
    "America/Toronto" to "CA", "America/Vancouver" to "CA", "America/Winnipeg" to "CA",
    "America/Edmonton" to "CA", "America/Halifax" to "CA", "America/St_Johns" to "CA",
    "America/Mexico_City" to "MX", "America/Cancun" to "MX", "America/Tijuana" to "MX",
    "America/Sao_Paulo" to "BR", "America/Manaus" to "BR", "America/Belem" to "BR",
    "America/Fortaleza" to "BR", "America/Recife" to "BR",
    "America/Argentina/Buenos_Aires" to "AR",
    "America/Santiago" to "CL", "America/Lima" to "PE", "America/Bogota" to "CO",
    "America/Caracas" to "VE", "America/Asuncion" to "PY", "America/Montevideo" to "UY",
    "America/La_Paz" to "BO", "America/Guayaquil" to "EC",
    "Africa/Cairo" to "EG", "Africa/Johannesburg" to "ZA", "Africa/Lagos" to "NG",
    "Africa/Nairobi" to "KE", "Africa/Casablanca" to "MA", "Africa/Algiers" to "DZ",
    "Africa/Tunis" to "TN", "Africa/Tripoli" to "LY", "Africa/Khartoum" to "SD",
    "Africa/Addis_Ababa" to "ET",
    "Australia/Sydney" to "AU", "Australia/Melbourne" to "AU", "Australia/Brisbane" to "AU",
    "Australia/Perth" to "AU", "Australia/Adelaide" to "AU", "Australia/Darwin" to "AU",
    "Pacific/Auckland" to "NZ",
    "Atlantic/Reykjavik" to "IS"
)
