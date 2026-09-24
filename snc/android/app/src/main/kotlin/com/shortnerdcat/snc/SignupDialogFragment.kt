// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.app.Dialog
import android.graphics.Color
import android.graphics.drawable.ColorDrawable
import android.os.Bundle
import android.view.ViewGroup
import android.webkit.JavascriptInterface
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.fragment.app.DialogFragment

// SignupDialogFragment shows the "Create New Account" page (email + our
// existing click-captcha widget) in a plain WebView popup -- same-origin
// (https://navlink.net), same page the Windows/mac/Linux clients' own
// webview popup loads. A dedicated page exists (rather than reusing the
// site's own sign-up modal) specifically so this webview's fetch() calls to
// /api/captcha and /api/account/start are same-origin and need no CORS
// wiring.
//
// Registration ends once the confirmation email is sent -- this dialog's
// only job is to report that email back to the caller (via onSignupDone,
// set before show()) so ConnectionFragment can return to its ordinary
// credential-login fields with the email pre-filled. Everything after that
// (clicking the email link, getting a key/password, logging in) is the
// unmodified existing login flow.
class SignupDialogFragment : DialogFragment() {

    companion object {
        private const val SIGNUP_URL = "https://navlink.net/signup-app.html"
        const val TAG = "SignupDialogFragment"
    }

    // Set by the caller right before show(getParentFragmentManager(), TAG).
    // Not preserved across process death (a short-lived modal dialog, same
    // as every other lambda-callback popup in this codebase) -- if the
    // process is killed while this is open, the dialog is simply gone on
    // return, same as any other transient popup.
    var onSignupDone: ((email: String) -> Unit)? = null

    private var succeeded = false

    override fun onDismiss(dialog: android.content.DialogInterface) {
        super.onDismiss(dialog)
        if (!succeeded) {
            LogEvent.emitSystem(LogEvents.UiButtonPressed, LogAttrs.ATTR_Button to "signup_cancelled")
        }
    }

    override fun onCreateDialog(savedInstanceState: Bundle?): Dialog {
        val webView = WebView(requireContext()).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.cacheMode = WebSettings.LOAD_NO_CACHE
            webViewClient = WebViewClient()
            addJavascriptInterface(object {
                // Called by signup-app.html's JS once /api/account/start
                // succeeds -- see that page's submit handler.
                @JavascriptInterface
                fun postMessage(email: String) {
                    post {
                        succeeded = true
                        LogEvent.emitSystem(LogEvents.UiButtonPressed, LogAttrs.ATTR_Button to "signup_done")
                        onSignupDone?.invoke(email)
                        dismissAllowingStateLoss()
                    }
                }
            }, "sncSignupDone")
            loadUrl(SIGNUP_URL)
        }

        val dialog = Dialog(requireContext())
        dialog.requestWindowFeature(android.view.Window.FEATURE_NO_TITLE)
        dialog.setContentView(webView)
        dialog.window?.setLayout(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT)
        dialog.window?.setBackgroundDrawable(ColorDrawable(Color.BLACK))
        // Shrink the page above the keyboard so the WebView can scroll the
        // focused field / Send button into view instead of being covered.
        dialog.window?.setSoftInputMode(android.view.WindowManager.LayoutParams.SOFT_INPUT_ADJUST_RESIZE)
        return dialog
    }
}
