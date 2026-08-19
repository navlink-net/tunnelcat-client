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

    private val vpnStateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            val running = SNCVpnService.isRunning
            val tunnelReady = SNCVpnService.isTunnelReady
            if (tunnelReady && !tunnelWasReady) {
                val frag = supportFragmentManager.findFragmentByTag("f2") as? BrowseFragment
                frag?.onVpnConnected()
            } else if (!running && vpnWasRunning) {
                // Just disconnected — reset browse proxy state so pages load directly.
                val frag = supportFragmentManager.findFragmentByTag("f2") as? BrowseFragment
                frag?.onVpnDisconnected()
            }
            if (running != vpnWasRunning) {
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
        // Pre-create all three fragments so the Browse tab's WebView is
        // initialized at app startup rather than on first visit.
        binding.viewPager.offscreenPageLimit = 2

        TabLayoutMediator(binding.tabLayout, binding.viewPager) { tab, position ->
            tab.text = when (position) {
                0 -> getString(R.string.tab_connect)
                1 -> getString(R.string.tab_apps)
                else -> getString(R.string.tab_browse)
            }
        }.attach()

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
        clubStatus = ClubStatus.read(this)
        clubPollHandler.post(clubPollRunnable)
        // The update broadcast is only delivered while this receiver is registered (i.e.
        // while the Activity is resumed) — re-sync from the persisted ready-state here to
        // catch a download that completed while this Activity was paused/backgrounded.
        if (UpdateChecker.readyVersion(this) != null) {
            updateReady = true
        }
        maybeShowUpdateDialog()
    }

    override fun onPause() {
        super.onPause()
        try { unregisterReceiver(updateReceiver) } catch (_: Exception) {}
        try { unregisterReceiver(vpnStateReceiver) } catch (_: Exception) {}
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
        // Arbiter-controlled kill switch (see admin_ipv6.go): "0" means no exit
        // in the fleet has IPv6 at all right now, so this is no longer a user
        // choice -- force it on and hide the manual toggle entirely. Absent/empty
        // file means the arbiter hasn't said (or this is an old cached manifest);
        // fall back to the normal user-controlled behavior in that case.
        val ipv6StateFile = File(filesDir, "snc.ipv6")
        val arbiterIPv6Off = ipv6StateFile.exists() && ipv6StateFile.readText().trim() == "0"
        menu.findItem(R.id.menu_disable_ipv6)?.apply {
            isVisible = !arbiterIPv6Off
            isChecked = arbiterIPv6Off || prefs.getBoolean("disable_ipv6", false)
            isEnabled = !arbiterIPv6Off
        }
        menu.findItem(R.id.menu_disable_udp)?.apply {
            isChecked = prefs.getBoolean("disable_udp", false)
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
                // LAN addresses are decided independently on the Go side (never
                // routed through BypassManager.ShouldBypass at all) and stay
                // bypassed regardless -- "system-necessary" bypass, per this
                // toggle's intent.
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
                Toast.makeText(this@MainActivity, "No logs found", Toast.LENGTH_SHORT).show()
                return@launch
            }
            val uri = FileProvider.getUriForFile(this@MainActivity, "$packageName.provider", zip)
            val intent = Intent(Intent.ACTION_SEND).apply {
                type = "application/zip"
                putExtra(Intent.EXTRA_STREAM, uri)
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
            startActivity(Intent.createChooser(intent, "Share SNC logs"))
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
