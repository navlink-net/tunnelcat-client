// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.graphics.BitmapFactory
import android.graphics.Color
import android.graphics.drawable.GradientDrawable
import android.net.Uri
import android.net.VpnService
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.fragment.app.Fragment
import androidx.lifecycle.lifecycleScope
import com.google.zxing.BinaryBitmap
import com.google.zxing.NotFoundException
import com.google.zxing.RGBLuminanceSource
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.QRCodeReader
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import com.shortnerdcat.snc.databinding.FragmentConnectionBinding
import kotlinx.coroutines.launch
import java.io.File
import java.net.URL
import java.util.Locale

class ConnectionFragment : Fragment() {

    private var _binding: FragmentConnectionBinding? = null
    private val binding get() = _binding!!

    private var pendingConnect = false

    // Last-applied values for updateUI()'s heavier view mutations (image/background
    // swaps, text). updateUI() runs on every 2s poll tick regardless of whether
    // anything actually changed (see dotPollRunnable) -- re-applying an identical
    // drawable/text every tick still triggers a real redraw (setImageResource in
    // particular redecodes the bitmap), which showed up as visible UI jitter.
    // Plain visibility toggles aren't guarded here: View.setVisibility() already
    // no-ops internally when the value is unchanged.
    private var lastImgRes = 0
    private var lastBadgeText: String? = null
    private var lastStatusText: String? = null
    private var lastStatusBg = 0
    private var lastConnectText: String? = null
    private var lastConnectBg = 0
    private var lastRibbonText: String? = null
    private var lastRibbonBg = 0

    // "No key yet" flow: which of the three screens (key entry / "do you have
    // a key?" / credential login) is currently shown. Starts at key entry --
    // the safe default that always works even before the reachability probe
    // resolves. HAVE_KEY_PROMPT is no longer entered automatically (kept only
    // as dead code / for its XML group + button handlers); explicitly reset
    // back to KEY_ENTRY on logout so the flow restarts cleanly.
    private enum class LoginScreen { KEY_ENTRY, HAVE_KEY_PROMPT, CREDENTIAL_LOGIN }
    private var loginScreen = LoginScreen.KEY_ENTRY

    // Set once by a background probe of navlink.net (see NavlinkAuth.probe) --
    // decides whether the credential-login path is offered at all.
    private var navlinkReachable = false

    // True once the user has manually switched login screens (Yes/No, "Log In
    // Instead", "I Have a Key"). Guards the probe-completion auto-switch below
    // from yanking the user to a different screen after they've already made
    // a deliberate choice in the brief window before the probe resolves.
    private var userSwitchedScreen = false

    private val stateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) = updateUI()
    }

    // SncBackgroundService keeps refreshing snc.cidr_status/snc.manifest_status while the
    // VPN is disconnected, but it has no broadcast to tell us — so poll while this screen
    // is visible to pick up cached → fresh transitions without requiring a state change.
    private val dotPollHandler = Handler(Looper.getMainLooper())
    private val dotPollRunnable = object : Runnable {
        override fun run() {
            updateUI()
            dotPollHandler.postDelayed(this, 2000)
        }
    }

    // Live uplink/downlink counter (snc.bytes, written every 1s by the Go core --
    // see main_linux.go's traffic-activity ticker). Separate 1s ticker from
    // dotPollRunnable above since the byte display needs a tighter refresh
    // cadence than the cache-status dots.
    private var lastBandwidthText: String? = null
    private val bandwidthPollHandler = Handler(Looper.getMainLooper())
    private val bandwidthPollRunnable = object : Runnable {
        override fun run() {
            updateBandwidth()
            bandwidthPollHandler.postDelayed(this, 1000)
        }
    }

    private val vpnPermission =
        registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
            if (result.resultCode == android.app.Activity.RESULT_OK) {
                launchVpn()
            } else {
                pendingConnect = false
                updateUI()
            }
        }

    // Result ignored -- see BatteryOptimization.ensureExempt's own comment.
    private val batteryOptLauncher =
        registerForActivityResult(ActivityResultContracts.StartActivityForResult()) {}

    private val qrLauncher = registerForActivityResult(ScanContract()) { result ->
        val content = result.contents ?: return@registerForActivityResult
        handleScannedContent(content)
    }

    private val imageLauncher = registerForActivityResult(ActivityResultContracts.GetContent()) { uri: Uri? ->
        uri ?: return@registerForActivityResult
        decodeQRFromUri(uri)
    }

    override fun onCreateView(inflater: LayoutInflater, container: ViewGroup?, savedInstanceState: Bundle?): View {
        _binding = FragmentConnectionBinding.inflate(inflater, container, false)
        return binding.root
    }

    override fun onViewCreated(view: View, savedInstanceState: Bundle?) {
        super.onViewCreated(view, savedInstanceState)

        binding.textVersion.text = BuildConfig.VERSION_NAME
        binding.editKey.setText(prefs().getString("key", ""))

        binding.btnActivateKey.setOnClickListener {
            val key = binding.editKey.text.toString().trim()
            if (key.isEmpty()) {
                Toast.makeText(requireContext(), getString(R.string.err_enter_key), Toast.LENGTH_SHORT).show()
                return@setOnClickListener
            }
            activateKey(key)
        }

        binding.btnHaveKeyYes.setOnClickListener {
            userSwitchedScreen = true
            loginScreen = LoginScreen.KEY_ENTRY
            updateUI()
        }
        binding.btnHaveKeyNo.setOnClickListener {
            userSwitchedScreen = true
            loginScreen = if (navlinkReachable) LoginScreen.CREDENTIAL_LOGIN else LoginScreen.KEY_ENTRY
            updateUI()
        }
        binding.btnKeyEntryLogin.setOnClickListener {
            userSwitchedScreen = true
            loginScreen = LoginScreen.CREDENTIAL_LOGIN
            updateUI()
        }
        binding.btnSwitchToKeyEntry.setOnClickListener {
            userSwitchedScreen = true
            loginScreen = LoginScreen.KEY_ENTRY
            updateUI()
        }
        binding.btnCreateAccount.setOnClickListener {
            val dialog = SignupDialogFragment()
            dialog.onSignupDone = { email ->
                // Registration ends here, with the confirmation email sent --
                // the rest is the ordinary login flow, same as for any other
                // account, once the user has clicked the link and knows
                // their password. See SignupDialogFragment's own doc comment.
                userSwitchedScreen = true
                loginScreen = LoginScreen.CREDENTIAL_LOGIN
                _binding?.editEmail?.setText(email)
                updateUI()
                _binding?.editPassword?.requestFocus()
            }
            dialog.show(parentFragmentManager, SignupDialogFragment.TAG)
        }
        binding.btnTogglePassword.setOnClickListener {
            val showing = binding.editPassword.inputType and
                android.text.InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD != 0
            if (showing) {
                binding.editPassword.inputType =
                    android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_VARIATION_PASSWORD
                binding.btnTogglePassword.setImageResource(R.drawable.ic_eye)
            } else {
                binding.editPassword.inputType =
                    android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD
                binding.btnTogglePassword.setImageResource(R.drawable.ic_eye_off)
            }
            binding.editPassword.setSelection(binding.editPassword.text?.length ?: 0)
        }
        binding.btnDoLogin.setOnClickListener {
            val email = binding.editEmail.text.toString().trim()
            val password = binding.editPassword.text.toString()
            if (email.isEmpty() || password.isEmpty()) {
                Toast.makeText(requireContext(), getString(R.string.err_enter_email_password), Toast.LENGTH_SHORT).show()
                return@setOnClickListener
            }
            binding.btnDoLogin.isEnabled = false
            viewLifecycleOwner.lifecycleScope.launch {
                try {
                    NavlinkAuth.login(email, password)
                    val issued = NavlinkAuth.freeKey()
                    activateKey(issued.key)
                } catch (e: Exception) {
                    Toast.makeText(requireContext(), getString(R.string.err_login_failed, e.message), Toast.LENGTH_LONG).show()
                } finally {
                    _binding?.btnDoLogin?.isEnabled = true
                }
            }
        }

        // Direct (non-tunneled) reachability probe -- decides whether the
        // credential-login path is offered at all. No VPN/tunnel exists yet
        // at this point (SNCVpnService only starts once a key is present),
        // so NavlinkAuth's own OkHttpClient is structurally guaranteed to go
        // straight to navlink.net.
        viewLifecycleOwner.lifecycleScope.launch {
            navlinkReachable = NavlinkAuth.probe()
            if (navlinkReachable && loginScreen == LoginScreen.KEY_ENTRY && !userSwitchedScreen) {
                loginScreen = LoginScreen.CREDENTIAL_LOGIN
            }
            updateUI()
        }

        binding.btnConnect.setOnClickListener {
            if (SNCVpnService.isRunning || SNCVpnService.isConnecting || pendingConnect) {
                LogEvent.emitSystem(LogEvents.UiButtonPressed, LogAttrs.ATTR_Button to "disconnect")
                pendingConnect = false
                stopVpn()
            } else {
                LogEvent.emitSystem(LogEvents.UiButtonPressed, LogAttrs.ATTR_Button to "connect")
                proceedWithConnect()
            }
        }

        binding.btnScan.setOnClickListener {
            qrLauncher.launch(ScanOptions().apply {
                setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                setPrompt(getString(R.string.scan_qr_prompt))
                setBeepEnabled(false)
                setOrientationLocked(true)
            })
        }

        binding.btnScanFile.setOnClickListener {
            imageLauncher.launch("image/*")
        }
    }

    override fun onResume() {
        super.onResume()
        requireContext().registerReceiver(
            stateReceiver,
            IntentFilter(SNCVpnService.ACTION_STATE_CHANGED),
            Context.RECEIVER_NOT_EXPORTED
        )
        dotPollHandler.post(dotPollRunnable)
        bandwidthPollHandler.post(bandwidthPollRunnable)
        updateUI()
    }

    override fun onPause() {
        super.onPause()
        requireContext().unregisterReceiver(stateReceiver)
        dotPollHandler.removeCallbacks(dotPollRunnable)
        bandwidthPollHandler.removeCallbacks(bandwidthPollRunnable)
    }

    override fun onDestroyView() {
        super.onDestroyView()
        _binding = null
        // Reset updateUI()'s change-detection cache: the next binding's views
        // start from Android's XML-declared defaults, not whatever the old
        // (now-destroyed) views last showed, so the cache must not skip the
        // first real update against them.
        lastImgRes = 0
        lastStatusText = null
        lastStatusBg = 0
        lastConnectText = null
        lastConnectBg = 0
        lastRibbonText = null
        lastRibbonBg = 0
        lastBandwidthText = null
    }

    // Reads snc.bytes ("<sent> <recv>", written every 1s by the Go core's
    // traffic-activity ticker -- see main_linux.go) and updates the
    // bottom-right uplink/downlink counter. Hidden while disconnected since
    // core.TotalBytes() is per-process and stale/zero once the tunnel exits.
    private fun updateBandwidth() {
        val b = _binding ?: return
        if (!SNCVpnService.isRunning) {
            if (b.textBandwidth.visibility != View.GONE) b.textBandwidth.visibility = View.GONE
            lastBandwidthText = null
            return
        }
        val raw = try {
            File(requireContext().filesDir, "snc.bytes").readText().trim()
        } catch (_: Exception) {
            null
        }
        val parts = raw?.split(" ")
        val sent = parts?.getOrNull(0)?.toLongOrNull() ?: 0L
        val recv = parts?.getOrNull(1)?.toLongOrNull() ?: 0L
        val text = "↑ ${formatBytesHuman(sent)}  ↓ ${formatBytesHuman(recv)}"
        if (text != lastBandwidthText) {
            b.textBandwidth.text = text
            lastBandwidthText = text
        }
        if (b.textBandwidth.visibility != View.VISIBLE) b.textBandwidth.visibility = View.VISIBLE
    }

    private fun formatBytesHuman(bytes: Long): String {
        val kb = 1024.0
        val mb = kb * 1024
        val gb = mb * 1024
        return when {
            bytes >= gb -> String.format(Locale.US, "%.1f GB", bytes / gb)
            bytes >= mb -> String.format(Locale.US, "%.1f MB", bytes / mb)
            bytes >= kb -> String.format(Locale.US, "%.1f KB", bytes / kb)
            else -> "$bytes B"
        }
    }

    private fun handleScannedContent(content: String) {
        when {
            content.startsWith("http://") || content.startsWith("https://") -> {
                Toast.makeText(requireContext(), getString(R.string.fetching_key), Toast.LENGTH_SHORT).show()
                Thread {
                    try {
                        val key = URL(content).readText().trim()
                        requireActivity().runOnUiThread { setKey(key) }
                    } catch (e: Exception) {
                        requireActivity().runOnUiThread {
                            Toast.makeText(requireContext(), getString(R.string.err_fetch_key_failed, e.message), Toast.LENGTH_LONG).show()
                        }
                    }
                }.start()
            }
            content.startsWith("navlink://") -> {
                val uri = android.net.Uri.parse(content)
                setKey(uri.getQueryParameter("key")?.trim() ?: "")
            }
            else -> setKey(content.trim())
        }
    }

    private fun decodeQRFromUri(uri: Uri) {
        Thread {
            try {
                val stream = requireContext().contentResolver.openInputStream(uri)
                    ?: return@Thread
                val bitmap = BitmapFactory.decodeStream(stream)
                stream.close()
                if (bitmap == null) {
                    requireActivity().runOnUiThread {
                        Toast.makeText(requireContext(), getString(R.string.err_could_not_read_image), Toast.LENGTH_SHORT).show()
                    }
                    return@Thread
                }
                val w = bitmap.width
                val h = bitmap.height
                val pixels = IntArray(w * h)
                bitmap.getPixels(pixels, 0, w, 0, 0, w, h)
                val source = RGBLuminanceSource(w, h, pixels)
                val binary = BinaryBitmap(HybridBinarizer(source))
                val result = QRCodeReader().decode(binary)
                requireActivity().runOnUiThread { handleScannedContent(result.text) }
            } catch (e: NotFoundException) {
                requireActivity().runOnUiThread {
                    Toast.makeText(requireContext(), getString(R.string.err_no_qr_found), Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                requireActivity().runOnUiThread {
                    Toast.makeText(requireContext(), getString(R.string.err_generic, e.message), Toast.LENGTH_SHORT).show()
                }
            }
        }.start()
    }

    // Called from MainActivity when a navlink:// deep link is received (OS camera QR or email tap).
    fun receiveKey(key: String) = setKey(key)

    // Called from MainActivity's "Logout" overflow-menu item.
    fun confirmLogout() {
        androidx.appcompat.app.AlertDialog.Builder(requireContext())
            .setTitle(R.string.logout)
            .setMessage(R.string.logout_confirm_message)
            .setPositiveButton(R.string.logout_remove) { _, _ ->
                pendingConnect = false
                stopVpn()
                prefs().edit().remove("key").apply()
                binding.editKey.setText("")
                loginScreen = LoginScreen.KEY_ENTRY
                userSwitchedScreen = false
                if (navlinkReachable) {
                    loginScreen = LoginScreen.CREDENTIAL_LOGIN
                }
                updateUI()
            }
            .setNegativeButton(R.string.cancel, null)
            .show()
    }

    private fun setKey(key: String) {
        if (key.isEmpty()) {
            Toast.makeText(requireContext(), getString(R.string.err_empty_key_received), Toast.LENGTH_SHORT).show()
            return
        }
        binding.editKey.setText(key)
        activateKey(key)
        Toast.makeText(requireContext(), getString(R.string.key_saved_tap_connect), Toast.LENGTH_SHORT).show()
    }

    // Shared by manual key entry, QR/deep-link scanning, and the navlink.net
    // credential-login path (see btnDoLogin above) -- all three end up here so
    // there is exactly one place that persists a key and resumes the service.
    private fun activateKey(key: String) {
        prefs().edit().putString("key", key).apply()
        // resumeAfterVpn() → launchCore() acquires @Synchronized and forks snc-core.
        // Must not run on the main thread to avoid ANR.
        val bgSvc = SncBackgroundService.instance
        if (bgSvc != null) Thread({ bgSvc.resumeAfterVpn() }, "snc-bg-resume").start()
        updateUI()
    }

    private fun requestVpnPermission() {
        val intent = VpnService.prepare(requireContext())
        if (intent != null) vpnPermission.launch(intent) else launchVpn()
    }

    private fun launchVpn() {
        val key = prefs().getString("key", null) ?: return
        val wildcatMode = prefs().getBoolean("wildcat", false)
        if (wildcatMode) {
            // In WildCat mode the user ALWAYS goes through the Browse tab first to acquire
            // a token. MainActivity orchestrates the full flow and starts the service
            // with the token as an Intent extra once login completes.
            (activity as? MainActivity)?.startWildCatConnect(key) ?: resetConnect()
        } else {
            requireActivity().startForegroundService(
                Intent(requireContext(), SNCVpnService::class.java).putExtra(SNCVpnService.EXTRA_KEY, key)
            )
            updateUI()
        }
    }

    // Reset a pending connect that was handed off to MainActivity but did not result
    // in a VPN start (e.g. login failed or Browse fragment was unavailable).
    fun resetConnect() {
        pendingConnect = false
        updateUI()
    }

    private fun stopVpn() {
        requireActivity().startService(
            Intent(requireContext(), SNCVpnService::class.java).apply {
                action = SNCVpnService.ACTION_STOP
            }
        )
    }

    private fun updateUI() {
        val b = _binding ?: return
        val running = SNCVpnService.isRunning
        val error = SNCVpnService.isError
        val keyDenied = SNCVpnService.isKeyDenied
        if (SNCVpnService.isConnecting || running || error) pendingConnect = false
        val connecting = SNCVpnService.isConnecting || pendingConnect
        val busy = running || connecting
        val hasKey = prefs().getString("key", "").isNullOrEmpty().not()
        val wildcatMode = prefs().getBoolean("wildcat", false)

        when {
            !busy -> b.ribbonMode.visibility = View.GONE
            wildcatMode -> {
                val ribbonText = getString(R.string.wildcat_ribbon_text)
                if (ribbonText != lastRibbonText) {
                    b.ribbonMode.text = ribbonText
                    lastRibbonText = ribbonText
                }
                val ribbonBg = Color.parseColor("#BF5600")
                if (ribbonBg != lastRibbonBg) {
                    b.ribbonMode.setBackgroundColor(ribbonBg)
                    lastRibbonBg = ribbonBg
                }
                b.ribbonMode.visibility = View.VISIBLE
            }
            else -> b.ribbonMode.visibility = View.GONE
        }

        // Cat Club / Elite Cat Club theming: same idle/connecting/connected/
        // wildcat/error illustration set as the regular tier, just a
        // different theme-suffixed drawable (see ClubStatus + snc.club_status,
        // written by snc-core every 5s -- mirrors the Windows client's
        // themedCatPNGs picking by aw.ClubTheme).
        val ctx = requireContext()
        val clubStatus = ClubStatus.read(ctx)
        val clubTheme = ClubStatus.effectiveTheme(ctx, clubStatus)
        val clubBadgeText = ClubStatus.effectiveBadgeText(ctx, clubStatus)
        fun themed(base: Int, catclub: Int, elite: Int) = when (clubTheme) {
            "catclub" -> catclub
            "elite" -> elite
            else -> base
        }
        val imgRes = when {
            keyDenied  -> themed(R.drawable.snc_error, R.drawable.snc_error_catclub, R.drawable.snc_error_elite)
            running && (!SNCVpnService.isTunnelReady || SNCVpnService.isReconnecting) ->
                themed(R.drawable.snc_connecting, R.drawable.snc_connecting_catclub, R.drawable.snc_connecting_elite)
            running    -> when {
                wildcatMode -> themed(R.drawable.snc_wildcat, R.drawable.snc_wildcat_catclub, R.drawable.snc_wildcat_elite)
                else        -> themed(R.drawable.snc_connected, R.drawable.snc_connected_catclub, R.drawable.snc_connected_elite)
            }
            connecting -> themed(R.drawable.snc_connecting, R.drawable.snc_connecting_catclub, R.drawable.snc_connecting_elite)
            error      -> themed(R.drawable.snc_error, R.drawable.snc_error_catclub, R.drawable.snc_error_elite)
            else       -> themed(R.drawable.snc_idle, R.drawable.snc_idle_catclub, R.drawable.snc_idle_elite)
        }
        if (imgRes != lastImgRes) {
            b.imgState.setImageResource(imgRes)
            lastImgRes = imgRes
        }

        // Club-membership chevron: full-width bar pinned to the very top of
        // the illustration, mirroring the bottom status bar's shape/position.
        // Cat Club: #59C8FF bg / white text. Elite: #3B2412 bg / #FFD700
        // (gold) text -- same palette as the win/mac clients.
        if (clubBadgeText != lastBadgeText) {
            if (clubBadgeText.isEmpty()) {
                b.textClubBadge.visibility = View.GONE
            } else {
                b.textClubBadge.text = clubBadgeText
                val elite = clubTheme == "elite"
                b.textClubBadge.setBackgroundColor(if (elite) Color.parseColor("#3B2412") else Color.parseColor("#59C8FF"))
                b.textClubBadge.setTextColor(if (elite) Color.parseColor("#FFD700") else Color.parseColor("#FFFFFF"))
                b.textClubBadge.visibility = View.VISIBLE
            }
            lastBadgeText = clubBadgeText
        }

        val statusText = when {
            keyDenied  -> getString(R.string.status_key_denied)
            running && (!SNCVpnService.isTunnelReady || SNCVpnService.isReconnecting) -> getString(R.string.status_connecting)
            running    -> if (wildcatMode) getString(R.string.status_connected_wildcat) else getString(R.string.status_connected)
            connecting -> getString(R.string.status_connecting)
            error      -> SNCVpnService.lastError ?: getString(R.string.status_error)
            else       -> getString(R.string.status_disconnected)
        }
        if (statusText != lastStatusText) {
            b.textStatus.text = statusText
            lastStatusText = statusText
        }

        // Bottom status-bar color, mirroring the win/mac/linux clients:
        // gray=disconnected, orange=connecting, green=connected, black=wildcat, red=error.
        val statusBg = when {
            keyDenied || error -> Color.parseColor("#C0392B")
            running && (!SNCVpnService.isTunnelReady || SNCVpnService.isReconnecting) -> Color.parseColor("#E08A2E")
            running && wildcatMode -> Color.BLACK
            running -> Color.parseColor("#2E8B3D")
            connecting -> Color.parseColor("#E08A2E")
            else -> Color.parseColor("#5B6470")
        }
        if (statusBg != lastStatusBg) {
            b.textStatus.setBackgroundColor(statusBg)
            lastStatusBg = statusBg
        }

        val showKeyEntry = !hasKey && loginScreen == LoginScreen.KEY_ENTRY
        val showHaveKeyPrompt = !hasKey && loginScreen == LoginScreen.HAVE_KEY_PROMPT
        val showCredentialLogin = !hasKey && loginScreen == LoginScreen.CREDENTIAL_LOGIN

        val keyVisible = if (showKeyEntry) View.VISIBLE else View.GONE
        b.editKey.visibility = keyVisible
        b.btnScan.visibility = keyVisible
        b.btnScanFile.visibility = keyVisible
        b.btnActivateKey.visibility = keyVisible
        b.btnKeyEntryLogin.visibility = if (showKeyEntry && navlinkReachable) View.VISIBLE else View.GONE
        b.haveKeyGroup.visibility = if (showHaveKeyPrompt) View.VISIBLE else View.GONE
        b.credentialLoginGroup.visibility = if (showCredentialLogin) View.VISIBLE else View.GONE

        b.btnConnect.visibility = if (hasKey) View.VISIBLE else View.GONE
        val connectText = getString(if (running || connecting) R.string.disconnect else R.string.connect)
        if (connectText != lastConnectText) {
            b.btnConnect.text = connectText
            lastConnectText = connectText
        }
        val connectBg = if (running || connecting) R.drawable.btn_pill_disconnect else R.drawable.btn_pill_connect
        if (connectBg != lastConnectBg) {
            b.btnConnect.setBackgroundResource(connectBg)
            lastConnectBg = connectBg
        }
        b.btnConnect.isEnabled = true

        b.editKey.isEnabled = !busy

        // While running/connecting, reflect SNCVpnService's in-memory status (kept fresh by
        // its own watcher). While disconnected, read the status files directly: manifest
        // status keeps updating because SncBackgroundService runs its own snc-core in the
        // background; CIDR status just holds at whatever the last VPN session wrote, since
        // bypass (and therefore CIDR fetching) is skipped entirely in proxy-only mode.
        b.statusDots.visibility = View.VISIBLE
        if (running || connecting) {
            setDotColor(b.dotCidr, SNCVpnService.cidrStatus)
            setDotColor(b.dotManifest, SNCVpnService.manifestStatus)
        } else {
            setDotColor(b.dotCidr, readStatusFile("snc.cidr_status"))
            setDotColor(b.dotManifest, readStatusFile("snc.manifest_status"))
        }
    }

    private fun readStatusFile(fileName: String): String =
        try { File(requireContext().filesDir, fileName).readText().trim().ifEmpty { "none" } }
        catch (_: Exception) { "none" }

    private fun setDotColor(view: View, status: String) {
        // Called every 2s from the poll loop -- guard against rebuilding and
        // reassigning a fresh GradientDrawable (and the redraw that causes)
        // when the status hasn't actually changed since the last tick.
        if (view.tag == status) return
        view.tag = status
        val color = when (status) {
            "fresh"  -> Color.parseColor("#4CAF50") // green
            "cached" -> Color.parseColor("#FFC107") // yellow
            else     -> Color.parseColor("#F44336") // red
        }
        val d = GradientDrawable()
        d.shape = GradientDrawable.OVAL
        d.setColor(color)
        view.background = d
    }

    fun onModeChanged() = updateUI()

    private fun proceedWithConnect() {
        pendingConnect = true
        updateUI()
        activity?.let { a -> BatteryOptimization.ensureExempt(a) { intent -> batteryOptLauncher.launch(intent) } }
        requestVpnPermission()
    }

    private fun prefs() = requireContext().getSharedPreferences("snc", Context.MODE_PRIVATE)
}
