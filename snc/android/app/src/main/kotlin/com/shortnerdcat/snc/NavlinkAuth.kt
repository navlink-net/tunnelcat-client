// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.Cookie
import okhttp3.CookieJar
import okhttp3.HttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/**
 * Minimal in-memory cookie jar: navlink.net sets one session cookie on login
 * that must be replayed on the very next call ([freeKey]). No persistence
 * across process restarts is needed for that, so a plain per-host list is
 * enough — avoids pulling in the separate okhttp-urlconnection artifact just
 * for JavaNetCookieJar.
 */
private class SimpleCookieJar : CookieJar {
    private val store = mutableMapOf<String, List<Cookie>>()

    override fun saveFromResponse(url: HttpUrl, cookies: List<Cookie>) {
        store[url.host] = cookies
    }

    override fun loadForRequest(url: HttpUrl): List<Cookie> = store[url.host] ?: emptyList()
}

/**
 * Direct (non-tunneled) HTTPS client for navlink.net's existing account-login
 * and free-key-issuance endpoints — the "no key yet" login path.
 *
 * Deliberately its own [OkHttpClient], independent of anything SNC tunnel
 * related: a device with no key yet has no VPN/tunnel running (SNCVpnService
 * is only started once a key exists), so this client is structurally
 * guaranteed to go straight over the device's normal network stack. Never
 * wire this through a tunnel-aware client, even if one becomes reachable
 * later in the process.
 */
object NavlinkAuth {
    private const val BASE = "https://navlink.net"
    private val JSON = "application/json".toMediaType()

    private val client = OkHttpClient.Builder()
        .cookieJar(SimpleCookieJar())
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(10, TimeUnit.SECONDS)
        .build()

    class NavlinkException(val statusCode: Int, message: String) : Exception(message)

    /**
     * Reports whether navlink.net is reachable directly, right now. Any HTTP
     * response (regardless of status code) counts as reachable — only a
     * network/TLS-level failure or timeout counts as unreachable.
     */
    suspend fun probe(): Boolean = withContext(Dispatchers.IO) {
        try {
            val probeClient = client.newBuilder()
                .connectTimeout(4, TimeUnit.SECONDS)
                .readTimeout(4, TimeUnit.SECONDS)
                .build()
            val req = Request.Builder().url("$BASE/").get().build()
            probeClient.newCall(req).execute().use { true }
        } catch (e: Exception) {
            false
        }
    }

    /**
     * Authenticates an existing navlink.net account. On success the session
     * cookie is retained by this object's cookie jar for a subsequent
     * [freeKey] call. Throws [NavlinkException] on a well-formed error
     * response (e.g. wrong password), or a plain IOException on network
     * failure.
     */
    suspend fun login(email: String, password: String) = withContext(Dispatchers.IO) {
        val body = JSONObject().put("email", email).put("password", password)
            .toString().toRequestBody(JSON)
        val req = Request.Builder().url("$BASE/api/account/login").post(body).build()
        client.newCall(req).execute().use { resp ->
            if (!resp.isSuccessful) {
                throw NavlinkException(resp.code, readErrorMessage(resp.body?.string()))
            }
        }
    }

    data class IssuedKey(val key: String, val keyId: String, val clientId: String)

    /** Issues a fresh key for the account authenticated by the preceding [login] call. */
    suspend fun freeKey(): IssuedKey = withContext(Dispatchers.IO) {
        val req = Request.Builder().url("$BASE/api/key/free").post(ByteArray(0).toRequestBody(null)).build()
        client.newCall(req).execute().use { resp ->
            val text = resp.body?.string() ?: ""
            if (!resp.isSuccessful) {
                throw NavlinkException(resp.code, readErrorMessage(text))
            }
            val json = JSONObject(text)
            IssuedKey(
                key = json.getString("key"),
                keyId = json.optString("key_id"),
                clientId = json.optString("client_id"),
            )
        }
    }

    private fun readErrorMessage(body: String?): String {
        if (body.isNullOrEmpty()) return "request failed"
        return try {
            JSONObject(body).optString("error", body)
        } catch (e: Exception) {
            body
        }
    }
}
