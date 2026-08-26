// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.Manifest
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import androidx.viewpager2.widget.ViewPager2
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.Log
import android.view.Menu
import android.view.MenuItem
import android.view.View
import android.view.ViewGroup
import android.widget.EditText
import android.widget.Toast
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import androidx.core.content.FileProvider
import androidx.fragment.app.Fragment
import androidx.lifecycle.lifecycleScope
import androidx.viewpager2.adapter.FragmentStateAdapter
import com.google.android.material.tabs.TabLayoutMediator
import com.shortnerdcat.snc.databinding.ActivityMainBinding
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.File

class MainActivity : AppCompatActivity() {

    private lateinit var binding: ActivityMainBinding

    // True once UpdateChecker (running inside SncBackgroundService — see that class) has
    // silently downloaded and SHA-256-verified a newer APK — only then does the "Update
    // ready" menu entry appear (tapping installs immediately, no download wait, since the
    // file is already on disk and verified).
    private var updateReady = false

    // Tracks which version the "update available" dialog was already shown for, so it
    // doesn't reappear every time onResume runs during the same Activity lifetime — only
    // when a newer version becomes ready. Mirrors Ratatosk's in-memory dialogDismissed.
    private var updateDialogShownForVersion: String? = null

    private var vpnWasRunning = false
    private var tunnelWasReady = false

    // Cat Club / Elite Cat Club state, refreshed from disk every 5s while
    // resumed (see clubPollRunnable) -- same cadence snc-core writes at.
    private var clubStatus: ClubStatus = ClubStatus.NONE
    private val clubPollHandler = Handler(Looper.getMainLooper())
    private val clubPollRunnable = object : Runnable {
        override fun run() {
            val fresh = ClubStatus.read(this@MainActivity)
            if (fresh != clubStatus) {
                clubStatus = fresh
                invalidateOptionsMenu()
            }
            clubPollHandler.postDelayed(this, 5000)
        }
    }

    private val updateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            if (intent.action == ACTION_UPDATE_READY) {
                updateReady = true
                invalidateOptionsMenu()
                maybeShowUpdateDialog()
            }
        }
    }

    // Received when SNCVpnService has no WildCat token during a sticky reconnect (e.g. after 8h).
    // Re-runs the Browse-tab token flow and restarts the VPN with the fresh token.
    private val wildcatTokenNeededReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            if (intent.action == SNCVpnService.ACTION_WILDCAT_LOGIN_REQUIRED) {
                val key = intent.getStringExtra(SNCVpnService.EXTRA_KEY) ?: run {
                    Log.w(TAG, "wildcatTokenNeededReceiver: no key in broadcast")
                    return
                }
                Log.i(TAG, "wildcatTokenNeededReceiver: refreshing WildCat token for reconnect")
                startWildCatConnect(key)
            }
        }
    }

    private val vpnStateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            val running = SNCVpnService.isRunning
            val tunnelReady = SNCVpnService.isTunnelReady
            if (tunnelReady && !tunnelWasReady) {
                // onVpnConnected() updates BrowseFragment's wildcatMode flag -- this is
                // functional setup, not a UI concern, so it always runs in the background.
                // The Browse tab itself is never auto-shown here: it stays hidden except
                // during an actual interactive login (see startWildCatConnectInteractive).
                // Previously this force-switched to the Browse tab on every WildCat connect
                // regardless of how the login happened -- that was the actual cause of the
                // tab flashing open even when acquireTokenSilently() never touched it.
                val frag = supportFragmentManager.findFragmentByTag("f2") as? BrowseFragment
                frag?.onVpnConnected()
            } else if (!running && vpnWasRunning) {
                // Just disconnected — reset browse proxy state so pages load directly.
                val frag = supportFragmentManager.findFragmentByTag("f2") as? BrowseFragment
                frag?.onVpnDisconnected()
            }
            if (running != vpnWasRunning) {
                // Refresh the overflow menu so the Disable QUIC item's
                // WildCat-locked visibility (see onPrepareOptionsMenu) tracks
                // connect/disconnect live, not just next time it's opened.
                invalidateOptionsMenu()
            }
            vpnWasRunning = running
            tunnelWasReady = tunnelReady
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        setTheme(R.style.Theme_SNC)
        super.onCreate(savedInstanceState)
        binding = ActivityMainBinding.inflate(layoutInflater)
        setContentView(binding.root)

        setSupportActionBar(binding.toolbar)
        supportActionBar?.setDisplayShowTitleEnabled(true)

        // Android 13+ requires runtime permission to post non-foreground notifications.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED
        ) {
            ActivityCompat.requestPermissions(this, arrayOf(Manifest.permission.POST_NOTIFICATIONS), 0)
        }

        // Request coarse location for device-physical country detection (GPS/network CC).
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.ACCESS_COARSE_LOCATION)
                != PackageManager.PERMISSION_GRANTED
        ) {
            ActivityCompat.requestPermissions(this, arrayOf(Manifest.permission.ACCESS_COARSE_LOCATION), 1)
        }

        ExcludedApps.initIfNeeded(this)

        // Show the "ready to install" menu entry immediately if a verified APK from a
        // prior session is still sitting on disk — no need to wait for a fresh check.
        if (UpdateChecker.readyVersion(this) != null) {
            updateReady = true
        }

        binding.viewPager.adapter = object : FragmentStateAdapter(this) {
            override fun getItemCount() = 3
            override fun createFragment(position: Int): Fragment = when (position) {
                0 -> ConnectionFragment()
                1 -> AppsFragment()
                else -> BrowseFragment()
            }
        }

        // Disable swipe so tabs feel stable; user taps the tab label to navigate.
        binding.viewPager.isUserInputEnabled = false
        // Pre-create all three fragments so WebView is initialized at app startup,
        // not on the first VPN connect when the Browse tab auto-opens.
        binding.viewPager.offscreenPageLimit = 2

        TabLayoutMediator(binding.tabLayout, binding.viewPager) { tab, position ->
            tab.text = when (position) {
                0 -> getString(R.string.tab_connect)
                1 -> getString(R.string.tab_apps)
                else -> getString(R.string.tab_browse)
            }
        }.attach()
        // Browse exists only to host the login flow -- it is not a general-purpose
        // browser the user opens on their own, so its tab label stays hidden
        // except for the duration of an actual interactive login (see
        // startWildCatConnectInteractive). The fragment itself stays alive
        // (offscreenPageLimit below) so its WebView/cookies are warm; only
        // the tab's visual entry in the TabLayout is toggled.
        setBrowseTabVisible(false)

        vpnWasRunning = SNCVpnService.isRunning
        tunnelWasReady = SNCVpnService.isTunnelReady

        startForegroundService(Intent(this, SncBackgroundService::class.java))

        handleNavigationIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleNavigationIntent(intent)
    }

    private fun handleNavigationIntent(intent: Intent) {
        val tab = intent.getIntExtra(EXTRA_NAVIGATE_TAB, -1)
        if (tab >= 0) {
            binding.viewPager.setCurrentItem(tab, false)
        }
        // Handle navlink://activate?key=... deep links (from email button or OS camera QR scan).
        val data = intent.data
        if (data?.scheme == "navlink" && data.host == "activate") {
            val key = data.getQueryParameter("key") ?: return
            if (key.isBlank()) return
            binding.viewPager.setCurrentItem(0, false)
            binding.root.post {
                (supportFragmentManager.findFragmentByTag("f0") as? ConnectionFragment)?.receiveKey(key)
            }
        }
    }

    override fun onResume() {
        super.onResume()
        // Ensure background service is running every time the app comes to foreground.
        // Handles the case where Android killed the service under memory pressure.
        if (!SNCVpnService.isRunning) {
            startForegroundService(Intent(this, SncBackgroundService::class.java))
        }
        registerReceiver(
            updateReceiver,
            IntentFilter(ACTION_UPDATE_READY),
            Context.RECEIVER_NOT_EXPORTED
        )
        registerReceiver(
            vpnStateReceiver,
            IntentFilter(SNCVpnService.ACTION_STATE_CHANGED),
            Context.RECEIVER_NOT_EXPORTED
        )
        registerReceiver(
            wildcatTokenNeededReceiver,
            IntentFilter(SNCVpnService.ACTION_WILDCAT_LOGIN_REQUIRED),
            Context.RECEIVER_NOT_EXPORTED
        )
        clubStatus = ClubStatus.read(this)
        clubPollHandler.post(clubPollRunnable)
        // The update broadcast is only delivered while this receiver is registered (i.e.
        // while the Activity is resumed) — re-sync from the persisted ready-state here to
        // catch a download that completed while this Activity was paused/backgrounded.
        if (UpdateChecker.readyVersion(this) != null) {
            updateReady = true
        }
        maybeShowUpdateDialog()

        // If SNCVpnService broadcast ACTION_WILDCAT_LOGIN_REQUIRED while the app was in the background,
        // the receiver was unregistered and the signal was lost — pick it up via the SharedPrefs flag.
        val prefs = getSharedPreferences("snc", MODE_PRIVATE)
        if (prefs.getBoolean("wildcat_login_needed", false)) {
            // commit() not apply() — must be synchronous so the flag is gone even if ANR
            // kills the process immediately after this line.
            prefs.edit().remove("wildcat_login_needed").commit()
            val key = prefs.getString("key", null)
            if (key != null && getSharedPreferences("snc", MODE_PRIVATE).getBoolean("wildcat", false)) {
                Log.i(TAG, "onResume: stale wildcat_login_needed flag — retrying WildCat connect")
                startWildCatConnect(key)
            }
        }
    }

    override fun onPause() {
        super.onPause()
        try { unregisterReceiver(updateReceiver) } catch (_: Exception) {}
        try { unregisterReceiver(vpnStateReceiver) } catch (_: Exception) {}
        try { unregisterReceiver(wildcatTokenNeededReceiver) } catch (_: Exception) {}
        clubPollHandler.removeCallbacks(clubPollRunnable)
    }

    override fun onDestroy() {
        super.onDestroy()
    }

    override fun onCreateOptionsMenu(menu: Menu): Boolean {
        menuInflater.inflate(R.menu.menu_main, menu)
        return true
    }

    override fun onPrepareOptionsMenu(menu: Menu): Boolean {
        val prefs = getSharedPreferences("snc", MODE_PRIVATE)
        val wildcatOn = prefs.getBoolean("wildcat", false)
menu.findItem(R.id.menu_wildcat)?.isChecked = wildcatOn
        // Arbiter-controlled kill switch (see admin_ipv6.go): "0" means no exit
        // in the fleet has IPv6 at all right now, so this is no longer a user
        // choice -- force it on and hide the manual toggle entirely. Absent/empty
        // file means the arbiter hasn't said (or this is an old cached manifest);
        // fall back to the normal user-controlled behavior in that case.
        val ipv6StateFile = File(filesDir, "snc.ipv6")
        val arbiterIPv6Off = ipv6StateFile.exists() && ipv6StateFile.readText().trim() == "0"
        menu.findItem(R.id.menu_disable_ipv6)?.apply {
            isVisible = !arbiterIPv6Off
            isChecked = arbiterIPv6Off || wildcatOn || prefs.getBoolean("disable_ipv6", false)
            isEnabled = !wildcatOn && !arbiterIPv6Off
        }
        menu.findItem(R.id.menu_disable_udp)?.apply {
            isChecked = prefs.getBoolean("disable_udp", false)
            isEnabled = !wildcatOn
        }
        menu.findItem(R.id.menu_disable_quic)?.apply {
            isVisible = true
            isChecked = prefs.getBoolean("disable_quic", false)
        }
        menu.findItem(R.id.menu_disable_bypass)?.apply {
            isVisible = true
            isChecked = prefs.getBoolean("disable_bypass", false)
        }
        val version = UpdateChecker.readyVersion(this)
        menu.findItem(R.id.menu_update)?.apply {
            isVisible = updateReady && version != null
            if (version != null) title = getString(R.string.update_ready) + " ($version)"
        }
        menu.findItem(R.id.menu_logout)?.isVisible = prefs.getString("key", "").isNullOrEmpty().not()

        menu.findItem(R.id.menu_recommend_cat_club)?.isVisible = clubStatus.canRecommend
        menu.findItem(R.id.menu_club_theme)?.isVisible = clubStatus.isAdmin
        if (clubStatus.isAdmin) {
            val checkedId = when (ClubStatus.effectiveTheme(this, clubStatus)) {
                "catclub" -> R.id.menu_club_theme_catclub
                "elite" -> R.id.menu_club_theme_elite
                else -> R.id.menu_club_theme_regular
            }
            menu.findItem(checkedId)?.isChecked = true
        }
        return super.onPrepareOptionsMenu(menu)
    }

    override fun onOptionsItemSelected(item: MenuItem): Boolean {
        val prefs = getSharedPreferences("snc", MODE_PRIVATE)
        return when (item.itemId) {
            R.id.menu_wildcat -> {
                // Set the checkbox immediately — no gate, no login prerequisite.
                // Token acquisition happens at Connect time via the Browse tab.
                val enabling = !item.isChecked
                if (enabling) {
                    // Unconditional informational warning, shown every time WildCat is
                    // turned on -- no "don't show again", no cancel option.
                    AlertDialog.Builder(this)
                        .setTitle(R.string.wildcat_warning_title)
                        .setMessage(R.string.wildcat_warning_message)
                        .setPositiveButton(R.string.ok) { _, _ -> setWildcatEnabled(true) }
                        .show()
                } else {
                    setWildcatEnabled(false)
                }
                true
            }
            R.id.menu_disable_ipv6 -> {
                val newVal = !item.isChecked
                item.isChecked = newVal
                prefs.edit().putBoolean("disable_ipv6", newVal).apply()
                if (SNCVpnService.isRunning) {
                    Toast.makeText(this, getString(R.string.reconnect_for_changes), Toast.LENGTH_SHORT).show()
                }
                true
            }
            R.id.menu_disable_udp -> {
                val newVal = !item.isChecked
                item.isChecked = newVal
                prefs.edit().putBoolean("disable_udp", newVal).apply()
                if (SNCVpnService.isRunning) {
                    Toast.makeText(this, getString(R.string.reconnect_for_changes), Toast.LENGTH_SHORT).show()
                }
                true
            }
            R.id.menu_disable_quic -> {
                val newVal = !item.isChecked
                item.isChecked = newVal
                prefs.edit().putBoolean("disable_quic", newVal).apply()
                if (SNCVpnService.isRunning) {
                    Toast.makeText(this, getString(R.string.reconnect_for_changes), Toast.LENGTH_SHORT).show()
                }
                true
            }
            R.id.menu_disable_bypass -> {
                // Turns off the general CIDR/home-TLD direct-dial bypass only.
                // LAN addresses and WildCat's own direct-dial hosts are decided
                // independently on the Go side (never routed through
                // BypassManager.ShouldBypass at all) and stay bypassed regardless
                // -- "system-necessary" bypass, per this toggle's intent.
                val newVal = !item.isChecked
                item.isChecked = newVal
                prefs.edit().putBoolean("disable_bypass", newVal).apply()
                if (SNCVpnService.isRunning) {
                    Toast.makeText(this, getString(R.string.reconnect_for_changes), Toast.LENGTH_SHORT).show()
                }
                true
            }
            R.id.menu_share_logs -> {
                shareLogs()
                true
            }
            R.id.menu_update -> {
                installReadyUpdate()
                true
            }
            R.id.menu_logout -> {
                (supportFragmentManager.findFragmentByTag("f0") as? ConnectionFragment)?.confirmLogout()
                true
            }
            R.id.menu_recommend_cat_club -> {
                showRecommendDialog()
                true
            }
            R.id.menu_club_theme_regular -> { setClubThemePreview(""); true }
            R.id.menu_club_theme_catclub -> { setClubThemePreview("catclub"); true }
            R.id.menu_club_theme_elite -> { setClubThemePreview("elite"); true }
            else -> super.onOptionsItemSelected(item)
        }
    }

    // Orchestrate the WildCat connect flow:
    //   1. Switch to Browse tab immediately.
    //   2. Navigate Browse tab to acquire a WildCat token.
    //   3. If user is already logged in → token obtained silently, no UI shown.
    //   4. Restore Browse URL, switch back to Connect tab.
    //   5. Start SNCVpnService with the token as an Intent extra.
    //
    // Shows or hides the Browse tab's label in the TabLayout. TabLayout has
    // no public per-tab visibility API, so this reaches into its internal
    // tab-strip ViewGroup (a long-standing, stable pattern for this exact
    // gap in Material's TabLayout) rather than touching the ViewPager2
    // adapter's item count -- doing it via the adapter would destroy/recreate
    // the Browse fragment (and its WebView) on every show/hide, which is
    // exactly the "keep it warm" behavior offscreenPageLimit exists to avoid.
    // Programmatic navigation (viewPager.setCurrentItem(2, ...)) still works
    // correctly regardless of whether the tab label itself is visible.
    private fun setBrowseTabVisible(visible: Boolean) {
        val tabStrip = binding.tabLayout.getChildAt(0) as? ViewGroup ?: return
        tabStrip.getChildAt(2)?.visibility = if (visible) View.VISIBLE else View.GONE
    }

    // Called from ConnectionFragment.launchVpn (connect-time) and from
    // wildcatTokenNeededReceiver (8h reconnect when cached token has expired).
    //
    // Always tries a silent token acquisition first (cached token, or a
    // cookie-based native fetch if a session already exists) so the
    // Browse tab is never shown at all when the user is already logged into
    // the login provider -- it only flashes open for a genuinely new, interactive login.
    fun startWildCatConnect(key: String) {
        LogEvent.emitSystem(LogEvents.WildcatConnectStage, LogAttrs.ATTR_Stage to "start")
        WildcatAuth.acquireTokenSilently(this) { token ->
            if (token != null) {
                Log.i(TAG, "startWildCatConnect: silent token acquired — starting VPN, Browse tab not shown")
                LogEvent.emitSystem(LogEvents.WildcatConnectStage, LogAttrs.ATTR_Stage to "silent_token_ok")
                startForegroundService(
                    Intent(this, SNCVpnService::class.java)
                        .putExtra(SNCVpnService.EXTRA_KEY, key)
                        .putExtra(SNCVpnService.EXTRA_WILDCAT_TOKEN, token)
                )
            } else {
                startWildCatConnectInteractive(key)
            }
        }
    }

    // Interactive fallback: no cached/silent session was available, so the
    // user must actually log in. Reveals the Browse tab for the duration of
    // the login flow only, and hides it again afterward regardless of outcome.
    private fun startWildCatConnectInteractive(key: String) {
        setBrowseTabVisible(true)
        binding.viewPager.setCurrentItem(2, true)
        // postDelayed instead of post: viewPager.post fires on the next frame (~16ms), still
        // during the tab-switch animation. After hours of WebView use the navigation call
        // blocks the main thread long enough to trigger ANR. Wait for animation to settle.
        findBrowseFragmentAndLogin(key, retriesLeft = 4, delayMs = 400)
    }

    // 2026-08-16: was a single findFragmentByTag attempt 400ms after the tab
    // switch, with no retry -- any device/timing where the fragment transaction
    // hadn't settled by then (slower fragment manager, different animation
    // duration, etc.) silently reset the connect attempt and never showed the
    // login WebView at all, with no way to tell from a single failed lookup
    // whether that's what actually happened. Retrying a few times with a short
    // backoff before giving up doesn't change anything about the already-working
    // case (found on the first try, same as before) -- it only gives a slower
    // fragment attach more chances to be seen before we conclude it's missing.
    private fun findBrowseFragmentAndLogin(key: String, retriesLeft: Int, delayMs: Long) {
        binding.root.postDelayed({
            val browseFrag = supportFragmentManager.findFragmentByTag("f2") as? BrowseFragment
            if (browseFrag == null) {
                if (retriesLeft > 0) {
                    LogEvent.emitSystem(
                        LogEvents.WildcatConnectStage,
                        LogAttrs.ATTR_Stage to "browse_frag_retry",
                        LogAttrs.ATTR_RetriesLeft to retriesLeft.toLong(),
                    )
                    findBrowseFragmentAndLogin(key, retriesLeft - 1, delayMs)
                    return@postDelayed
                }
                Log.w(TAG, "startWildCatConnect: Browse fragment not found — resetting connect")
                LogEvent.emitSystem(LogEvents.WildcatConnectStage, LogAttrs.ATTR_Stage to "browse_frag_not_found")
                setBrowseTabVisible(false)
                (supportFragmentManager.findFragmentByTag("f0") as? ConnectionFragment)?.resetConnect()
                Toast.makeText(this, getString(R.string.wildcat_login_failed), Toast.LENGTH_SHORT).show()
                return@postDelayed
            }
            val savedUrl = browseFrag.getCurrentTabUrl()
            browseFrag.startWildcatLogin { token ->
                // Restore Browse tab content, switch back to Connect, and hide
                // the Browse tab label again regardless of outcome.
                browseFrag.returnToUrl(savedUrl)
                binding.viewPager.setCurrentItem(0, true)
                setBrowseTabVisible(false)
                if (token != null) {
                    Log.i(TAG, "startWildCatConnect: token acquired — starting VPN")
                    LogEvent.emitSystem(LogEvents.WildcatConnectStage, LogAttrs.ATTR_Stage to "token_acquired")
                    startForegroundService(
                        Intent(this, SNCVpnService::class.java)
                            .putExtra(SNCVpnService.EXTRA_KEY, key)
                            .putExtra(SNCVpnService.EXTRA_WILDCAT_TOKEN, token)
                    )
                } else {
                    Log.w(TAG, "startWildCatConnect: login failed — resetting connect state")
                    LogEvent.emitSystem(LogEvents.WildcatConnectStage, LogAttrs.ATTR_Stage to "login_failed")
                    (supportFragmentManager.findFragmentByTag("f0") as? ConnectionFragment)?.resetConnect()
                    Toast.makeText(this, getString(R.string.wildcat_login_failed), Toast.LENGTH_SHORT).show()
                }
            }
        }, delayMs)
    }

    private fun setWildcatEnabled(enabled: Boolean) {
        val prefs = getSharedPreferences("snc", MODE_PRIVATE)
        prefs.edit().putBoolean("wildcat", enabled).apply()
        invalidateOptionsMenu()
        if (SNCVpnService.isRunning || SNCVpnService.isConnecting) {
            startService(Intent(this, SNCVpnService::class.java).apply {
                action = SNCVpnService.ACTION_SET_WILDCAT
                putExtra(SNCVpnService.EXTRA_WILDCAT, enabled)
            })
        }
        (supportFragmentManager.findFragmentByTag("f0") as? ConnectionFragment)?.onModeChanged()
    }

    // The APK was already downloaded and SHA-256-verified in the background by
    // UpdateChecker — this just launches the system installer from that file. No network
    // wait: the whole point of the silent-download scheme is that by the time the user
    // sees the menu entry, the file is already sitting on disk and ready to go.
    private fun installReadyUpdate() {
        val apk = UpdateChecker.readyApkFile(this)
        if (apk == null) {
            Log.w("Updater", "installReadyUpdate: no verified APK on disk")
            return
        }
        val uri = FileProvider.getUriForFile(this, "$packageName.provider", apk)
        val intent = Intent(Intent.ACTION_INSTALL_PACKAGE).apply {
            setDataAndType(uri, "application/vnd.android.package-archive")
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
        runCatching { startActivity(intent) }
            .onFailure { Log.e("Updater", "installReadyUpdate: startActivity failed", it) }
    }

    // Proactive "update available" prompt, shown at most once per Activity lifetime for a
    // given version (mirrors Ratatosk's AlertDialog with Install/Later). "Later" just
    // dismisses — the menu entry (onPrepareOptionsMenu) remains as a fallback entry point.
    private fun maybeShowUpdateDialog() {
        val version = UpdateChecker.readyVersion(this) ?: return
        if (version == updateDialogShownForVersion) return
        updateDialogShownForVersion = version
        AlertDialog.Builder(this)
            .setTitle(getString(R.string.update_dialog_title))
            .setMessage(getString(R.string.update_dialog_message, version))
            .setPositiveButton(getString(R.string.update_dialog_install)) { _, _ -> installReadyUpdate() }
            .setNegativeButton(getString(R.string.update_dialog_later), null)
            .show()
    }

    private fun shareLogs() {
        lifecycleScope.launch {
            val zip = withContext(Dispatchers.IO) {
                val out = File(cacheDir, "snc-logs.zip")
                zipLogs(File(filesDir, "logs"), out)
                out
            }
            if (!zip.exists() || zip.length() == 0L) {
                Toast.makeText(this@MainActivity, getString(R.string.no_logs_found), Toast.LENGTH_SHORT).show()
                return@launch
            }
            val uri = FileProvider.getUriForFile(this@MainActivity, "$packageName.provider", zip)
            val intent = Intent(Intent.ACTION_SEND).apply {
                type = "application/zip"
                putExtra(Intent.EXTRA_STREAM, uri)
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
            startActivity(Intent.createChooser(intent, getString(R.string.share_snc_logs_chooser)))
        }
    }

    private fun zipLogs(logsDir: File, out: File) {
        val logFiles = logsDir.listFiles { f -> f.name.endsWith(".log") }
            ?: return
        if (logFiles.isEmpty()) return
        java.util.zip.ZipOutputStream(out.outputStream().buffered()).use { zos ->
            logFiles.sortedByDescending { it.lastModified() }.take(10).forEach { f ->
                zos.putNextEntry(java.util.zip.ZipEntry(f.name))
                f.inputStream().use { it.copyTo(zos) }
                zos.closeEntry()
            }
        }
    }

    // setClubThemePreview is a purely client-side, admin-only UI convenience
    // (see keyenc.go's IsAdmin doc comment -- "never a real permission"), so
    // it never touches snc-core/IPC at all -- see ClubStatus.setPreview.
    // Applies instantly: no tunnel/connection required, no round-trip. The
    // chevron itself is drawn as an overlay on the illustration by
    // ConnectionFragment (which re-reads ClubStatus every 2s), not here.
    private fun setClubThemePreview(theme: String) {
        ClubStatus.setPreview(this, theme)
        invalidateOptionsMenu()
    }

    // showRecommendDialog prompts for a navlink.net username and sends a Cat
    // Club recommendation for it via the running tunnel (snc.RecommendCatClubMember,
    // relayed through whichever control the current session is authenticated
    // against -- see club_discovery.go). Requires an active connection since
    // the recommendation call is only reachable through the tunnel session.
    private fun showRecommendDialog() {
        val input = EditText(this).apply { hint = getString(R.string.recommend_cat_club_hint) }
        AlertDialog.Builder(this)
            .setTitle(getString(R.string.recommend_cat_club))
            .setView(input)
            .setPositiveButton(getString(R.string.recommend_cat_club_send)) { _, _ ->
                val username = input.text.toString().trim()
                if (username.isEmpty()) return@setPositiveButton
                lifecycleScope.launch {
                    val resp = withContext(Dispatchers.IO) {
                        SNCVpnService.sendIpcWithResponse(
                            """{"cmd":"club-recommend","args":{"username":"${username.replace("\"", "")}"}}""",
                            "club-recommend"
                        )
                    }
                    val json = resp?.let { runCatching { JSONObject(it) }.getOrNull() }
                    val ok = json?.optBoolean("ok", false) ?: false
                    if (ok) {
                        Toast.makeText(this@MainActivity, getString(R.string.recommend_cat_club_sent), Toast.LENGTH_SHORT).show()
                    } else {
                        val err = json?.optString("error").takeUnless { it.isNullOrEmpty() } ?: "no connection"
                        Toast.makeText(this@MainActivity, getString(R.string.recommend_cat_club_failed, err), Toast.LENGTH_LONG).show()
                    }
                }
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }

    companion object {
        private const val TAG = "MainActivity"
        // Intent extra: which tab index to switch to when the activity opens.
        // Sent by notification PendingIntents so tapping any notification lands on Connect.
        const val EXTRA_NAVIGATE_TAB = "navigate_tab"
    }
}
