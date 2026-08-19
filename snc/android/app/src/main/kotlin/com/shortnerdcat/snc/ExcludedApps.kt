// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

package com.shortnerdcat.snc

import android.content.Context
import org.json.JSONArray

object ExcludedApps {

    private const val PREFS_NAME = "snc"
    private const val KEY_EXCLUDED = "excluded_apps"
    private const val KEY_INITIALIZED = "excluded_apps_initialized"

    // Telephony and IMS packages that are ALWAYS excluded from the tunnel, regardless of user
    // configuration.  These processes handle mobile data session management, VoLTE/VoNR
    // registration, and carrier IMS signaling.  If their traffic enters the TUN, gVisor drops
    // the IPv6 packets (Verizon IMS is IPv6-only), which breaks the radio PDN connection and
    // causes the 4G/5G icon to flicker.  Non-existent packages are silently skipped by
    // addDisallowedApplication, so listing all known vendor variants is safe.
    val TELEPHONY_ALWAYS_EXCLUDED = setOf(
        // AOSP telephony / IMS
        "com.android.phone",
        "com.android.ims",
        "com.android.server.telecom",
        // Qualcomm IMS stack (Snapdragon — Verizon, T-Mobile, AT&T flagship devices)
        "com.qualcomm.qti.ims",
        "com.qualcomm.qti.telephonyservice",
        "com.qualcomm.qti.imscmservice",
        "com.qualcomm.qti.uceShimService",
        "com.qualcomm.qti.networksetting",
        "com.qualcomm.qti.ltebc",
        "com.qualcomm.qti.radioconfiginterface",
        // Verizon carrier services
        "com.verizon.obdm_permissions",
        "com.vzw.apnservice4",
        "com.vzw.apnservice",
        "com.verizon.services",
        "com.verizon.llkagent",
        // Samsung IMS (Galaxy devices on Verizon/T-Mobile)
        "com.samsung.android.ims",
        "com.samsung.android.telephony",
        "com.samsung.android.imsioservice",  // Samsung IMS I/O service (S-series, Verizon)
        "com.samsung.android.rcsioservice",  // Samsung RCS I/O service (shares IMS bearer)
        "com.samsung.android.incallui",      // handles in-call SIP signaling
        "com.samsung.rcs",
        // MediaTek IMS (mid-range devices)
        "com.mediatek.ims",
        "com.mediatek.lte.volte",
        // T-Mobile / TMUS carrier stack
        "com.tmobile.pr.adapt",
        "com.tmobile.ims",
        // General carrier config framework
        "com.android.carrierdefaultapp",
        "com.android.carrierconfig",
        "com.android.mms.service",           // MMS service uses mobile bearer
        // Google Pixel IMS / carrier stack (AOSP + Pixel devices)
        "com.google.android.ims",
        "com.google.android.carrier",
        "com.google.android.dialer",
        // OnePlus / OPPO IMS
        "com.oneplus.ims",
        "com.coloros.ims",
    )

    // Apps pre-checked by default on first launch (if installed).
    val DEFAULT_EXCLUDED = setOf(
        // Banking — confirmed VPN detection/blocking (RKS Global, Meduza, April 2026)
        "ru.sberbankmobile",                    // Сбербанк Онлайн
        "com.idamob.tinkoff.android",           // Т-Банк (old package)
        "ru.tinkoff",                           // Т-Банк (current package)
        "ru.alfabank.mobile.android",           // Альфа-Банк
        "ru.vtb24.mobilebanking.android",       // ВТБ Онлайн
        "ru.raiffeisen.mbank",                  // Райффайзен Банк
        "ru.gazprombank.mobile",                // Газпромбанк
        "ru.promsvyazbank.mbank",               // ПСБ (Промсвязьбанк)
        "ru.rshb.mobilebank",                   // Россельхозбанк
        "ru.sovcombank.halvacard",              // Халва (Совкомбанк)
        "ru.rosbank.android",                   // Росбанк
        "ru.pochtabank.android",                // Почта Банк
        "com.yandex.money",                     // ЮMoney
        "ru.mts.money.wallet",                  // МТС Деньги
        "ru.mts.mymts",                         // Мой МТС
        // Shopping & Marketplaces — confirmed VPN detection
        "ru.ozon.app.android",                  // Ozon
        "com.wildberries.ru",                   // Wildberries
        "ru.avito.android",                     // Авито
        "ru.yandex.market",                     // Яндекс Маркет
        "ru.lamoda.mobile",                     // Lamoda
        "ru.sbermegamarket.sbermegamarket",     // Мегамаркет (alt package)
        "ru.megamarket.marketplace",            // Мегамаркет (current package)
        // Social / Messaging — confirmed VPN detection
        "com.vkontakte.android",                // ВКонтакте (classic package)
        "com.vk.vkcompose",                     // ВКонтакте (current package)
        "ru.vk.video",                          // VK Видео
        "ru.vk.music",                          // VK Музыка
        "ru.ok.android",                        // Одноклассники
        "ru.mail.mailapp",                      // Почта Mail.ru
        // Yandex ecosystem — confirmed VPN detection
        "ru.yandex.searchplugin",               // Яндекс (основное приложение)
        "ru.yandex.browser",                    // Яндекс Браузер
        "ru.yandex.yandexmaps",                 // Яндекс Карты
        "ru.yandex.music",                      // Яндекс Музыка
        "ru.yandex.taxi",                       // Яндекс Go (такси)
        // Media — confirmed VPN detection
        "ru.kinopoisk",                         // Кинопоиск
        "ru.rutube",                            // RuTube
        // App store
        "ru.vk.store",                          // RuStore
        // Government / state apps
        "ru.gosuslugi.portal.android",          // Госуслуги
        "ru.max.messenger",                     // МАКС (alt package)
        "ru.oneme.app",                         // МАКС (current package, confirmed surveillance)
        "ru.mos.gosuslugi",                     // Госуслуги Москвы
        "ru.mos.mos",                           // Mos.ru
    )

    fun load(context: Context): MutableSet<String> {
        val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
        val json = prefs.getString(KEY_EXCLUDED, null) ?: return mutableSetOf()
        return try {
            val arr = JSONArray(json)
            (0 until arr.length()).mapTo(mutableSetOf()) { arr.getString(it) }
        } catch (_: Exception) {
            mutableSetOf()
        }
    }

    fun save(context: Context, packages: Set<String>) {
        val arr = JSONArray()
        packages.forEach { arr.put(it) }
        context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            .edit().putString(KEY_EXCLUDED, arr.toString()).apply()
    }

    // On first launch, seed the exclusion list with DEFAULT_EXCLUDED apps that are installed.
    fun initIfNeeded(context: Context) {
        val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
        if (prefs.getBoolean(KEY_INITIALIZED, false)) return
        val installed = context.packageManager
            .getInstalledApplications(0)
            .map { it.packageName }
            .toSet()
        val seed = DEFAULT_EXCLUDED.intersect(installed)
        save(context, seed)
        prefs.edit().putBoolean(KEY_INITIALIZED, true).apply()
    }
}
