// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.app.DownloadManager
import android.content.Context
import android.graphics.Bitmap
import android.net.Uri
import android.net.http.SslError
import android.os.Bundle
import android.os.Environment
import android.util.Log
import android.widget.Toast
import android.view.KeyEvent
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.view.inputmethod.EditorInfo
import android.webkit.CookieManager
import android.webkit.PermissionRequest
import android.webkit.SslErrorHandler
import android.webkit.URLUtil
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebStorage
import android.webkit.WebViewClient
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.ImageButton
import android.widget.PopupMenu
import android.widget.ProgressBar
import androidx.fragment.app.Fragment
import org.json.JSONArray
import org.json.JSONObject
import java.net.URLEncoder

class BrowseFragment : Fragment() {

    private data class BrowseTab(val url: String = HOME_URL, val title: String = "")

    private var webViewContainer: FrameLayout? = null
    private var webView: WebView? = null
    private var urlBar: EditText? = null
    private var btnNavBack: ImageButton? = null
    private var btnNavForward: ImageButton? = null
    private var btnReload: ImageButton? = null
    private var btnTabs: ImageButton? = null
    private var progressBar: ProgressBar? = null
    private var pageSpinner: ProgressBar? = null

    private val tabs = mutableListOf(BrowseTab())
    private var currentTab = 0

    // When non-null, the Browse tab is in login mode for WildCat credential
    // acquisition; cleared automatically when login is detected or abandoned.
    @Volatile private var loginCallback: ((String?) -> Unit)? = null

    // User-agent saved before the login flow; restored when it finishes.
    private var savedUserAgent: String? = null

    private var mobileRedirectDone = false

    private var wildcatExtractAttempts = 0

    // WildCat mode flag.
    private var wildcatMode = false

    // Periodic WebView cleanup to prevent V8 heap and BFCache accumulation over
    // long sessions (8 h+ causes main-thread GC pauses long enough for ANR).
    private val cleanupHandler = android.os.Handler(android.os.Looper.getMainLooper())
    private val cleanupRunnable = object : Runnable {
        override fun run() {
            if (progressBar?.visibility != View.VISIBLE) {
                webView?.clearHistory()
                Log.d(TAG, "periodic cleanup: history cleared")
            }
            cleanupHandler.postDelayed(this, CLEANUP_INTERVAL_MS)
        }
    }

    override fun onCreateView(inflater: LayoutInflater, container: ViewGroup?, savedInstanceState: Bundle?): View =
        inflater.inflate(R.layout.fragment_browse, container, false)

    override fun onViewCreated(view: View, savedInstanceState: Bundle?) {
        super.onViewCreated(view, savedInstanceState)

        webViewContainer = view.findViewById(R.id.webViewContainer)
        urlBar        = view.findViewById(R.id.urlBar)
        btnNavBack    = view.findViewById(R.id.btnNavBack)
        btnNavForward = view.findViewById(R.id.btnNavForward)
        btnReload     = view.findViewById(R.id.btnReload)
        btnTabs       = view.findViewById(R.id.btnTabs)
        progressBar   = view.findViewById(R.id.progressBar)
        pageSpinner   = view.findViewById(R.id.pageSpinner)

        // WebView is created lazily in onResume() — see createWebView().

        btnNavBack?.setOnClickListener    { Log.d(TAG, "back tapped");    webView?.goBack()    }
        btnNavForward?.setOnClickListener { Log.d(TAG, "forward tapped"); webView?.goForward() }
        btnReload?.setOnClickListener     { Log.d(TAG, "reload tapped");  reloadCurrentTab()   }
        btnTabs?.setOnClickListener       { Log.d(TAG, "tabs tapped");    showTabsMenu(it)     }

        urlBar?.setOnEditorActionListener { _, actionId, event ->
            if (actionId == EditorInfo.IME_ACTION_GO ||
                (event?.keyCode == KeyEvent.KEYCODE_ENTER && event.action == KeyEvent.ACTION_DOWN)) {
                val input = urlBar?.text.toString().trim()
                Log.d(TAG, "url bar: navigate to $input")
                navigateTo(input)
                true
            } else false
        }
    }

    override fun onResume() {
        super.onResume()
        if (webView == null) createWebView()
        cleanupHandler.postDelayed(cleanupRunnable, CLEANUP_INTERVAL_MS)
    }

    override fun onPause() {
        super.onPause()
        cleanupHandler.removeCallbacks(cleanupRunnable)
        updateCurrentTabUrl(webView?.url)
        persistTabs()
        val wv = webView
        webView = null
        wv?.let { webViewContainer?.removeView(it); it.destroy() }
    }

    override fun onDestroyView() {
        val wv = webView
        webView = null
        webViewContainer = null
        pageSpinner = null
        wv?.destroy()
        super.onDestroyView()
    }

    // Creates a fresh WebView, attaches it to the container, and loads the current tab.
    // Called from onResume() so the WebView renderer (and its V8 heap) starts clean on
    // every tab switch, preventing multi-day V8 accumulation that causes ANR on loadUrl().
    // Cookies are in CookieManager and survive WebView recreation.
    private fun createWebView() {
        val ctx = context ?: return
        val container = webViewContainer ?: return
        val wv = WebView(ctx)
        container.addView(wv, 0, FrameLayout.LayoutParams(
            FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
        webView = wv
        setupWebView()
        loadPersistedTabs()
        updateWildcatMode()
        navigateTo(tabs[currentTab].url)
    }

    // Returns true if a login flow is already in progress (waiting for login completion).
    fun isWildcatLoginActive(): Boolean = loginCallback != null

    // Starts WildCat credential acquisition via the Browse tab. The caller restores
    // the previous URL via returnToUrl() and switches back to the Connect tab.
    fun startWildcatLogin(onDone: (String?) -> Unit) {
        onDone(null)
    }

    // Complete the login flow: restore the original User-Agent, clear the login
    // callback, and deliver the result. Idempotent — safe to call regardless of
    // success or failure.
    private fun finishWildcatLogin(cb: (String?) -> Unit, token: String?) {
        loginCallback = null
        savedUserAgent?.let { webView?.settings?.userAgentString = it }
        savedUserAgent = null
        cb(token)
    }

    // Returns the URL of the currently active tab. Used by callers that need to
    // save and restore the Browse position around the login flow.
    fun getCurrentTabUrl(): String = tabs.getOrNull(currentTab)?.url ?: HOME_URL

    // Navigate Browse tab to url after a login flow completes. Public wrapper
    // around the private navigateTo so callers (e.g. MainActivity) can restore
    // the previous page without needing fragment-internal access.
    fun returnToUrl(url: String) { navigateTo(url) }

    // Called by MainActivity when VPN disconnects.
    fun onVpnDisconnected() {
        Log.d(TAG, "onVpnDisconnected")
        // Leave current page as-is; new navigations go directly via device network.
    }

    // Called by MainActivity when VPN connects.
    fun onVpnConnected() {
        Log.d(TAG, "onVpnConnected")
        updateWildcatMode()
        // Do not trigger a page load here (WildCat or otherwise). Loading from a
        // VPN state callback fires during ViewPager layout transitions and can
        // permanently freeze UI input events. The user navigates to the Browse
        // tab and loads pages themselves.
    }

    private fun reloadCurrentTab() {
        Log.d(TAG, "reloadCurrentTab")
        webView?.reload()
    }

    private fun setupWebView() {
        val wv = webView ?: return
        // Logged once per WebView creation, not gated on loginCallback -- this
        // is cheap and the single most direct way to confirm or rule out "outdated
        // WebView / stale root store" as the cause of a TLS trust failure (2026-08-15
        // WildCat login investigation: old, infrequently-updated devices often run
        // a WebView build whose bundled root store predates a legitimate, currently-
        // valid CA -- same symptom as a real MITM, but nothing to do with the network).
        try {
            val pkg = android.webkit.WebView.getCurrentWebViewPackage()
            LogEvent.emitSystem(
                LogEvents.BrowseWebviewInfo,
                LogAttrs.ATTR_Ok to true,
                LogAttrs.ATTR_Pkg to (pkg?.packageName ?: ""),
                LogAttrs.ATTR_VersionName to (pkg?.versionName ?: ""),
                LogAttrs.ATTR_AndroidSdk to android.os.Build.VERSION.SDK_INT.toLong(),
                LogAttrs.ATTR_AndroidRelease to android.os.Build.VERSION.RELEASE,
                LogAttrs.ATTR_Device to "${android.os.Build.MANUFACTURER}/${android.os.Build.MODEL}",
            )
        } catch (e: Exception) {
            LogEvent.emitSystem(
                LogEvents.BrowseWebviewInfo,
                LogAttrs.ATTR_Ok to false,
                LogAttrs.ATTR_AndroidSdk to android.os.Build.VERSION.SDK_INT.toLong(),
                LogAttrs.ATTR_Err to e.toString(),
            )
        }
        wv.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            mediaPlaybackRequiresUserGesture = false
            mixedContentMode = WebSettings.MIXED_CONTENT_COMPATIBILITY_MODE
        }
        CookieManager.getInstance().setAcceptCookie(true)
        CookieManager.getInstance().setAcceptThirdPartyCookies(wv, true)

        wv.webChromeClient = object : WebChromeClient() {
            // This WebView only ever loads the WildCat login provider for a login token
            // acquisition (see startWildcatLogin) -- never a general browsing
            // surface, so there's no legitimate getUserMedia() use case here.
            // Deny explicitly rather than leaving the request unanswered
            // (the base WebChromeClient.onPermissionRequest is a no-op, which
            // leaves the page's getUserMedia() promise hanging forever).
            override fun onPermissionRequest(request: PermissionRequest) {
                Log.w(TAG, "onPermissionRequest: origin=${request.origin} resources=${request.resources.joinToString()}")
                LogEvent.emitSystem(
                    LogEvents.BrowsePermissionDenied,
                    LogAttrs.ATTR_Origin to request.origin.toString(),
                    LogAttrs.ATTR_Resources to request.resources.joinToString(),
                )
                request.deny()
            }

            override fun onProgressChanged(view: WebView, newProgress: Int) {
                progressBar?.progress = newProgress
                progressBar?.visibility = if (newProgress < 100) View.VISIBLE else View.GONE
                if (loginCallback != null) {
                    LogEvent.emitSystem(
                        LogEvents.WildcatLoginProgress,
                        LogAttrs.ATTR_Url to (view.url ?: ""),
                        LogAttrs.ATTR_Progress to newProgress.toLong(),
                    )
                }
            }

            override fun onReceivedTitle(view: WebView, title: String?) {
                // Was silent before for the ignored cases -- during the 2026-08-15 WildCat
                // login investigation the *raw* title (blank vs data: vs real) at every
                // callback turned out to matter, not just the one value finally kept.
                if (loginCallback != null) {
                    LogEvent.emitSystem(
                        LogEvents.WildcatLoginNote,
                        LogAttrs.ATTR_Url to (view.url ?: ""),
                        LogAttrs.ATTR_Kind to "title",
                        LogAttrs.ATTR_Text to (title?.take(120) ?: ""),
                    )
                }
                // Ignore titles from the connecting page (data: URL) and blank placeholders.
                if (!title.isNullOrEmpty() && !title.startsWith("data:")) {
                    tabs[currentTab] = tabs[currentTab].copy(title = title)
                }
            }

            // No override existed before -- JS-level failures (script errors, CSP
            // violations blocking a resource, mixed-content refusals) are otherwise
            // completely invisible: a page can fire onPageFinished successfully while
            // never actually rendering real content because its own JS threw partway
            // through. See 2026-08-15 WildCat login investigation -- the client
            // reports the login page opens fine in a normal browser, so a WebView-specific JS/CSP
            // failure is a live suspect and this is the only way to see one.
            override fun onConsoleMessage(msg: android.webkit.ConsoleMessage): Boolean {
                if (loginCallback != null) {
                    Log.d(TAG, "console[${msg.messageLevel()}] ${msg.message()} (${msg.sourceId()}:${msg.lineNumber()})")
                    LogEvent.emitSystem(
                        LogEvents.WildcatLoginNote,
                        LogAttrs.ATTR_Url to msg.sourceId(),
                        LogAttrs.ATTR_Kind to "console",
                        LogAttrs.ATTR_Text to "[${msg.messageLevel()}] ${msg.message()} (line ${msg.lineNumber()})",
                    )
                }
                return false
            }
        }

        wv.addJavascriptInterface(RetryInterface(), "SNC")

        wv.setDownloadListener { url, userAgent, contentDisposition, mimetype, _ ->
            val fileName = URLUtil.guessFileName(url, contentDisposition, mimetype)
            val request = DownloadManager.Request(Uri.parse(url)).apply {
                setMimeType(mimetype)
                addRequestHeader("User-Agent", userAgent)
                setTitle(fileName)
                setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
                setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, fileName)
            }
            val dm = requireContext().getSystemService(Context.DOWNLOAD_SERVICE) as DownloadManager
            dm.enqueue(request)
            Toast.makeText(requireContext(), getString(R.string.downloading_file, fileName), Toast.LENGTH_SHORT).show()
        }

        wv.webViewClient = object : WebViewClient() {
            private var autoRetryPending = false

            // In WildCat mode all link navigations go through navigateTo (SOCKS5/TUN).
            // Fall back to direct load if VPN is not running.
            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                val url = request.url
                // Diagnostic only: WebView cannot natively hand off non-http(s) schemes
                // (custom schemes, intent:// URIs) to installed apps the way Chrome does —
                // it silently no-ops or errors instead. Logging every such attempt here so
                // we can see exactly what URL the "log in via app" button tries to load,
                // before writing the actual Intent-launch handoff.
                if (url.scheme != "http" && url.scheme != "https") {
                    Log.i(TAG, "shouldOverrideUrlLoading: non-http(s) scheme=${url.scheme} url=$url")
                    LogEvent.emitSystem(
                        LogEvents.BrowseNonHttpScheme,
                        LogAttrs.ATTR_Scheme to (url.scheme ?: ""),
                        LogAttrs.ATTR_Url to url.toString(),
                    )
                }
                if (!wildcatMode || !SNCVpnService.isRunning) return false
                Log.d(TAG, "shouldOverrideUrlLoading wc: $url")
                view.post { navigateTo(url.toString()) }
                return true
            }

            override fun onReceivedError(view: WebView, request: android.webkit.WebResourceRequest, error: android.webkit.WebResourceError) {
                if (!request.isForMainFrame) return
                val url = request.url?.toString() ?: ""
                Log.w(TAG, "main frame error ${error.errorCode}: ${error.description} url=$url")
                // Was Logcat-only before -- invisible in the uploaded diagnostic log, which
                // made a real login-page load failure indistinguishable from "page loaded but
                // empty" when reading snc-logs.zip after the fact. See 2026-08-15 WildCat
                // login investigation.
                LogEvent.emitSystem(
                    LogEvents.BrowseLoadError,
                    LogAttrs.ATTR_Code to error.errorCode.toLong(),
                    LogAttrs.ATTR_Desc to error.description.toString(),
                    LogAttrs.ATTR_Url to url,
                    LogAttrs.ATTR_WildcatMode to wildcatMode,
                )
                view.post { showErrorPage(view, url) }
                if (SNCVpnService.isTunnelReady && !autoRetryPending) {
                    autoRetryPending = true
                    view.postDelayed({
                        autoRetryPending = false
                        val target = tabs.getOrNull(currentTab)?.url ?: HOME_URL
                        // In WildCat mode, navigateTo handles the full fetch cycle.
                        if (wildcatMode) navigateTo(target) else view.loadUrl(target)
                    }, 2000)
                }
            }

            // No override existed before -- TLS-level failures (bad/interfering cert,
            // e.g. DPI/MITM tampering) were invisible everywhere, not just in the
            // uploaded log: the default WebViewClient behavior (cancel the load) still
            // applies here, this only adds visibility into *why* a load silently
            // stopped. See 2026-08-15 WildCat login investigation.
            override fun onReceivedSslError(view: WebView, handler: SslErrorHandler, error: SslError) {
                Log.w(TAG, "SSL error ${error.primaryError}: url=${error.url}")
                // Log the actual cert chain, not just the error code -- primaryError alone
                // (e.g. 3 = SSL_UNTRUSTED) doesn't say WHOSE cert was rejected. Whether
                // issuedBy is a real public CA (Let's Encrypt, DigiCert, ...) vs something
                // unrecognizable (a MITM/interception CA, a self-signed cert) settles the
                // 2026-08-15 "is this a trust-policy gap or real interception" question
                // without needing to ask the user to check their device's CA store by hand.
                val cert = error.certificate
                LogEvent.emitSystem(
                    LogEvents.BrowseSslError,
                    LogAttrs.ATTR_PrimaryError to error.primaryError.toLong(),
                    LogAttrs.ATTR_Url to error.url,
                    LogAttrs.ATTR_WildcatMode to wildcatMode,
                    LogAttrs.ATTR_IssuedTo to (cert.issuedTo?.dName ?: ""),
                    LogAttrs.ATTR_IssuedBy to (cert.issuedBy?.dName ?: ""),
                    LogAttrs.ATTR_ValidFrom to cert.validNotBeforeDate.toString(),
                    LogAttrs.ATTR_ValidTo to cert.validNotAfterDate.toString(),
                )
                handler.cancel()
            }

            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
                // Was the Yandex browse-proxy resource-cache hook (wcResourceCache),
                // removed 2026-08-15 along with that whole mechanism. WildCat mode
                // now routes everything through SOCKS5/TUN like a normal load, so
                // there's nothing to intercept here.
                return null
            }

            override fun onPageStarted(view: WebView, url: String?, favicon: Bitmap?) {
                if (loginCallback != null) {
                    LogEvent.emitSystem(
                        LogEvents.WildcatLoginNote,
                        LogAttrs.ATTR_Url to (url ?: ""),
                        LogAttrs.ATTR_Kind to "page_started",
                        LogAttrs.ATTR_Text to "",
                    )
                }
                // Pre-existing behavior, unchanged by the 2026-08-15 Yandex-ZIP removal:
                // WildCat mode suppresses these WebView-driven URL bar updates. Suppressing
                // WebView callbacks prevents stale/cancelled page loads from
                // overwriting the URL bar mid-navigation (e.g. interrupted navlink.net load
                // firing onPageFinished after the user has already typed a new address).
                val skip = url == null || url.startsWith("data:") || url == "about:blank" || wildcatMode
                if (!skip) urlBar?.setText(url)
                if (!wildcatMode && !skip) pageSpinner?.visibility = View.VISIBLE
                updateNavButtons()
            }

            override fun onPageFinished(view: WebView, url: String?) {
                pageSpinner?.visibility = View.GONE
                val skip = url == null || url.startsWith("data:") || url == "about:blank" || wildcatMode
                if (!skip) {
                    urlBar?.setText(url)
                    updateCurrentTabUrl(url)
                    persistTabs()
                }
                updateNavButtons()
            }
        }
    }

    // navigateTo is the single entry point for all page loads. Always a direct
    // WebView load: in WildCat mode traffic goes through SOCKS5/TUN (a covert relay
    // backend, with certain domains dialing directly per WildcatDirectHosts
    // -- see snc/core/socks5.go); in plain TUN mode it's just a normal load.
    // (The Yandex-Disk-backed browse-proxy/ZIP-fetch alternative path was
    // removed 2026-08-15 -- the WildCat relay replaced it; see the 2026-08-15 WildCat
    // login investigation.)
    private fun navigateTo(input: String) {
        val url = normalizeUrl(input)
        urlBar?.setText(url)
        Log.d(TAG, "navigateTo: wildcatMode=$wildcatMode vpnRunning=${SNCVpnService.isRunning} url=$url")
        if (loginCallback != null) {
            LogEvent.emitSystem(
                LogEvents.WildcatLoginNavigate,
                LogAttrs.ATTR_WildcatMode to wildcatMode,
                LogAttrs.ATTR_VpnRunning to SNCVpnService.isRunning,
                LogAttrs.ATTR_Url to url,
            )
        }
        webView?.loadUrl(url)
    }

    private fun normalizeUrl(input: String): String {
        val trimmed = input.trim()
        return when {
            trimmed.startsWith("http://") || trimmed.startsWith("https://") -> trimmed
            trimmed.contains(".") -> "https://$trimmed"
            else -> "https://www.google.com/search?q=${URLEncoder.encode(trimmed, "UTF-8")}"
        }
    }

    private fun updateWildcatMode() {
        wildcatMode = requireContext()
            .getSharedPreferences("snc", Context.MODE_PRIVATE)
            .getBoolean("wildcat", false)
    }

    private fun showTabsMenu(anchor: View) {
        val popup = PopupMenu(requireContext(), anchor)
        tabs.forEachIndexed { i, tab ->
            val label = tab.title.ifEmpty { tab.url }.take(32)
            popup.menu.add(0, i, i, if (i == currentTab) "✓ $label" else "   $label")
        }
        popup.menu.add(0, MENU_NEW_TAB,   tabs.size,     getString(R.string.browse_new_tab))
        popup.menu.add(0, MENU_CLOSE_TAB, tabs.size + 1, getString(R.string.browse_close_tab))
        popup.setOnMenuItemClickListener { item ->
            when (item.itemId) {
                MENU_NEW_TAB   -> openNewTab()
                MENU_CLOSE_TAB -> closeCurrentTab()
                else           -> switchToTab(item.itemId)
            }
            true
        }
        popup.show()
    }

    private fun closeCurrentTab() {
        if (tabs.size <= 1) {
            tabs[0] = BrowseTab()
            navigateTo(HOME_URL)
        } else {
            tabs.removeAt(currentTab)
            currentTab = currentTab.coerceAtMost(tabs.size - 1)
            navigateTo(tabs[currentTab].url)
        }
        persistTabs()
    }

    private fun openNewTab() {
        tabs.add(BrowseTab())
        currentTab = tabs.size - 1
        navigateTo(HOME_URL)
        persistTabs()
    }

    private fun switchToTab(index: Int) {
        if (index == currentTab) return
        updateCurrentTabUrl(webView?.url)
        currentTab = index.coerceIn(0, tabs.size - 1)
        navigateTo(tabs[currentTab].url)
    }

    private fun updateCurrentTabUrl(url: String?) {
        if (!url.isNullOrEmpty() && url != "about:blank" && !url.startsWith("data:")) {
            tabs[currentTab] = tabs[currentTab].copy(url = url)
        }
    }

    private fun updateNavButtons() {
        btnNavBack?.isEnabled    = webView?.canGoBack()    == true
        btnNavForward?.isEnabled = webView?.canGoForward() == true
    }

    private fun persistTabs() {
        val ctx = context ?: return
        val arr = JSONArray()
        tabs.forEach { tab ->
            arr.put(JSONObject().apply {
                put("url",   tab.url)
                put("title", tab.title)
            })
        }
        ctx.getSharedPreferences(BROWSE_PREFS, Context.MODE_PRIVATE).edit()
            .putString("tabs", arr.toString())
            .putInt("currentTab", currentTab)
            .apply()
    }

    private fun loadPersistedTabs() {
        val prefs = requireContext().getSharedPreferences(BROWSE_PREFS, Context.MODE_PRIVATE)
        val json = prefs.getString("tabs", null) ?: return
        try {
            val arr = JSONArray(json)
            if (arr.length() == 0) return
            tabs.clear()
            for (i in 0 until arr.length()) {
                val obj = arr.getJSONObject(i)
                tabs.add(BrowseTab(
                    url   = obj.optString("url",   HOME_URL),
                    title = obj.optString("title", "")
                ))
            }
            currentTab = prefs.getInt("currentTab", 0).coerceIn(0, tabs.size - 1)
        } catch (_: Exception) {}
    }

    private inner class RetryInterface {
        @android.webkit.JavascriptInterface
        fun retry() {
            val url = tabs.getOrNull(currentTab)?.url ?: HOME_URL
            Log.d(TAG, "JS retry: $url")
            view?.post { navigateTo(url) }
        }
    }

    private fun showErrorPage(wv: WebView, url: String) {
        val safeUrl = url.replace("&", "&amp;").replace("<", "&lt;").take(80)
        val html = """<!DOCTYPE html><html><head>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0d0d1a;color:#c0c8e0;font-family:-apple-system,sans-serif;
  display:flex;flex-direction:column;align-items:center;justify-content:center;
  min-height:100vh;text-align:center;padding:24px}
.icon{font-size:48px;margin-bottom:24px;opacity:.5}
h1{color:#e0e8ff;font-size:22px;margin-bottom:12px}
.url{font-family:monospace;font-size:11px;color:#4060a0;background:#161628;
  padding:8px 16px;border-radius:6px;margin-bottom:24px;
  max-width:100%;word-break:break-all}
p{color:#6070a0;font-size:14px;margin-bottom:32px;line-height:1.6}
button{background:linear-gradient(135deg,#1a3060,#2060c0);color:#e0e8ff;
  border:none;padding:14px 40px;font-size:16px;border-radius:8px;
  cursor:pointer;letter-spacing:.5px}
button:active{opacity:.7}
</style></head><body>
<div class="icon">&#x26A1;</div>
<h1>Connection failed</h1>
<div class="url">$safeUrl</div>
<p>Could not load this page.<br>Check your connection and try again.</p>
<button onclick="window.SNC.retry()">Try again</button>
</body></html>"""
        val b64 = android.util.Base64.encodeToString(html.toByteArray(Charsets.UTF_8), android.util.Base64.NO_PADDING)
        wv.loadData(b64, "text/html", "base64")
    }

    companion object {
        private const val TAG          = "BrowseFragment"
        private const val HOME_URL     = "https://www.navlink.net"
        private const val BROWSE_PREFS = "snc_browse"
        private const val MENU_NEW_TAB   = Int.MAX_VALUE
        private const val MENU_CLOSE_TAB = Int.MAX_VALUE - 1
        private const val CLEANUP_INTERVAL_MS = 60 * 60 * 1000L  // 1 hour
    }
}
