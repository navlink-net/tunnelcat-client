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
import android.webkit.WebSettings
import android.webkit.WebView
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
        navigateTo(tabs[currentTab].url)
    }

    // Called by MainActivity when VPN disconnects.
    fun onVpnDisconnected() {
        Log.d(TAG, "onVpnDisconnected")
        // Leave current page as-is; new navigations go directly via device network.
    }

    // Called by MainActivity when VPN connects.
    fun onVpnConnected() {
        Log.d(TAG, "onVpnConnected")
        // Do not trigger a page load here. Loading from a VPN state callback
        // fires during ViewPager layout transitions and can permanently freeze
        // UI input events. The user navigates to the Browse tab and loads
        // pages themselves.
    }

    private fun reloadCurrentTab() {
        Log.d(TAG, "reloadCurrentTab")
        webView?.reload()
    }

    private fun setupWebView() {
        val wv = webView ?: return
        // Logged once per WebView creation -- cheap and the single most direct
        // way to confirm or rule out "outdated WebView / stale root store" as
        // the cause of a TLS trust failure (old, infrequently-updated devices
        // often run a WebView build whose bundled root store predates a
        // legitimate, currently-valid CA -- same symptom as a real MITM, but
        // nothing to do with the network).
        try {
            val pkg = android.webkit.WebView.getCurrentWebViewPackage()
            KotlinLog.log("BrowseFragment: WebView package=${pkg?.packageName} versionName=${pkg?.versionName} " +
                "androidSdk=${android.os.Build.VERSION.SDK_INT} androidRelease=${android.os.Build.VERSION.RELEASE} " +
                "device=${android.os.Build.MANUFACTURER}/${android.os.Build.MODEL}")
        } catch (e: Exception) {
            KotlinLog.log("BrowseFragment: WebView package info unavailable: $e androidSdk=${android.os.Build.VERSION.SDK_INT}")
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
            // No legitimate getUserMedia() use case in this in-app browser.
            // Deny explicitly rather than leaving the request unanswered
            // (the base WebChromeClient.onPermissionRequest is a no-op, which
            // leaves the page's getUserMedia() promise hanging forever).
            override fun onPermissionRequest(request: PermissionRequest) {
                Log.w(TAG, "onPermissionRequest: origin=${request.origin} resources=${request.resources.joinToString()}")
                KotlinLog.log("BrowseFragment: onPermissionRequest origin=${request.origin} resources=${request.resources.joinToString()} -> denied")
                request.deny()
            }

            override fun onProgressChanged(view: WebView, newProgress: Int) {
                progressBar?.progress = newProgress
                progressBar?.visibility = if (newProgress < 100) View.VISIBLE else View.GONE
            }

            override fun onReceivedTitle(view: WebView, title: String?) {
                // Ignore titles from the connecting page (data: URL) and blank placeholders.
                if (!title.isNullOrEmpty() && !title.startsWith("data:")) {
                    tabs[currentTab] = tabs[currentTab].copy(title = title)
                }
            }

            // No override existed before -- JS-level failures (script errors, CSP
            // violations blocking a resource, mixed-content refusals) are otherwise
            // completely invisible: a page can fire onPageFinished successfully while
            // never actually rendering real content because its own JS threw partway
            // through.
            override fun onConsoleMessage(msg: android.webkit.ConsoleMessage): Boolean {
                Log.d(TAG, "console[${msg.messageLevel()}] ${msg.message()} (${msg.sourceId()}:${msg.lineNumber()})")
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
            Toast.makeText(requireContext(), "Downloading $fileName", Toast.LENGTH_SHORT).show()
        }

        wv.webViewClient = object : WebViewClient() {
            private var autoRetryPending = false

            override fun onReceivedError(view: WebView, request: android.webkit.WebResourceRequest, error: android.webkit.WebResourceError) {
                if (!request.isForMainFrame) return
                val url = request.url?.toString() ?: ""
                Log.w(TAG, "main frame error ${error.errorCode}: ${error.description} url=$url")
                KotlinLog.log("BrowseFragment: onReceivedError code=${error.errorCode} desc=${error.description} url=$url")
                view.post { showErrorPage(view, url) }
                if (SNCVpnService.isTunnelReady && !autoRetryPending) {
                    autoRetryPending = true
                    view.postDelayed({
                        autoRetryPending = false
                        val target = tabs.getOrNull(currentTab)?.url ?: HOME_URL
                        view.loadUrl(target)
                    }, 2000)
                }
            }

            // TLS-level failures (bad/interfering cert, e.g. DPI/MITM tampering)
            // are otherwise invisible: the default WebViewClient behavior
            // (cancel the load) still applies here, this only adds visibility
            // into *why* a load silently stopped.
            override fun onReceivedSslError(view: WebView, handler: SslErrorHandler, error: SslError) {
                Log.w(TAG, "SSL error ${error.primaryError}: url=${error.url}")
                // Log the actual cert chain, not just the error code -- primaryError alone
                // (e.g. 3 = SSL_UNTRUSTED) doesn't say WHOSE cert was rejected. Whether
                // issuedBy is a real public CA (Let's Encrypt, DigiCert, ...) vs something
                // unrecognizable (a MITM/interception CA, a self-signed cert) settles
                // whether this is a trust-policy gap or real interception without needing
                // to ask the user to check their device's CA store by hand.
                val cert = error.certificate
                KotlinLog.log("BrowseFragment: onReceivedSslError primaryError=${error.primaryError} url=${error.url} " +
                    "issuedTo='${cert.issuedTo?.dName}' issuedBy='${cert.issuedBy?.dName}' validFrom=${cert.validNotBeforeDate} validTo=${cert.validNotAfterDate}")
                handler.cancel()
            }

            override fun onPageStarted(view: WebView, url: String?, favicon: Bitmap?) {
                // Suppressing WebView callbacks prevents stale/cancelled page loads from
                // overwriting the URL bar mid-navigation (e.g. interrupted navlink.net load
                // firing onPageFinished after the user has already typed a new address).
                val skip = url == null || url.startsWith("data:") || url == "about:blank"
                if (!skip) {
                    urlBar?.setText(url)
                    pageSpinner?.visibility = View.VISIBLE
                }
                updateNavButtons()
            }

            override fun onPageFinished(view: WebView, url: String?) {
                pageSpinner?.visibility = View.GONE
                val skip = url == null || url.startsWith("data:") || url == "about:blank"
                if (!skip) {
                    urlBar?.setText(url)
                    updateCurrentTabUrl(url)
                    persistTabs()
                }
                updateNavButtons()
            }
        }
    }

    // navigateTo is the single entry point for all page loads.
    private fun navigateTo(input: String) {
        val url = normalizeUrl(input)
        urlBar?.setText(url)
        Log.d(TAG, "navigateTo: vpnRunning=${SNCVpnService.isRunning} url=$url")
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
