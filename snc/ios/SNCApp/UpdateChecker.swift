// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Foundation
import UIKit
import UserNotifications

// Checks the App Store for a newer published version of this app.
//
// Unlike the Android/Windows clients, which silently download and verify a new
// binary themselves, iOS (App Store distribution — confirmed for this app) does
// not allow an app to download and execute new native code. The only feasible
// "update" flow here is: check the App Store for a newer version, and let the
// UI prompt the user to open the App Store page — the OS handles the actual
// update. No download, no SHA-256 verification, no silent install.
//
// Inherits NSObject because UNUserNotificationCenterDelegate is an Obj-C
// protocol — needed so a tap on the system notification opens the App Store
// page directly, instead of just bringing the app to the foreground.
final class UpdateChecker: NSObject {

    static let shared = UpdateChecker()

    static let updateAvailableNotification = Notification.Name("net.shortnerdcat.client.updateAvailable")
    private static let notificationRequestID = "net.shortnerdcat.client.update"
    private static let storeURLUserInfoKey = "storeURL"

    private let bundleID = "net.shortnerdcat.client"
    // No background task — App Store distribution gives no silent-download path
    // anyway, so a foreground-triggered check (cold launch + each time the app
    // becomes active, rate-limited here) is the standard, idiomatic approach.
    private let checkInterval: TimeInterval = 24 * 60 * 60
    private let prefsLastCheck = "update_last_check"
    private let prefsLatestVersion = "update_latest_version"
    private let prefsStoreURL = "update_store_url"
    // Version a system notification was already posted for — distinct from the
    // in-app banner (which can re-show every check): without this, the same
    // not-yet-installed update would re-notify the user every 24h.
    private let prefsNotifiedVersion = "update_notified_version"

    private override init() {
        super.init()
        UNUserNotificationCenter.current().delegate = self
    }

    // Call once at launch (AppDelegate). Safe to call repeatedly — the system
    // only shows its permission prompt once; subsequent calls are no-ops if the
    // user already answered.
    func requestNotificationPermissionIfNeeded() {
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { granted, error in
            if let error {
                SLog("UpdateChecker: notification permission error: \(error.localizedDescription)")
            } else {
                SLog("UpdateChecker: notification permission granted=\(granted)")
            }
        }
    }

    // Latest known App Store version + page URL, if newer than the running build.
    // Backed by UserDefaults so the UI reflects a prior check immediately, before
    // this run's check (if any) completes.
    var availableUpdate: (version: String, storeURL: URL)? {
        let defaults = UserDefaults.standard
        guard let latest = defaults.string(forKey: prefsLatestVersion),
              let urlString = defaults.string(forKey: prefsStoreURL),
              let url = URL(string: urlString),
              isVersion(latest, greaterThan: currentVersion) else { return nil }
        return (latest, url)
    }

    private var currentVersion: String {
        Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "0"
    }

    func checkIfNeeded() {
        let defaults = UserDefaults.standard
        let last = defaults.double(forKey: prefsLastCheck)
        let now = Date().timeIntervalSince1970
        guard now - last >= checkInterval else { return }
        defaults.set(now, forKey: prefsLastCheck)
        check()
    }

    private func check() {
        guard let url = URL(string: "https://itunes.apple.com/lookup?bundleId=\(bundleID)") else { return }
        URLSession.shared.dataTask(with: url) { [weak self] data, _, error in
            guard let self, error == nil, let data else {
                SLog("UpdateChecker: lookup failed: \(error?.localizedDescription ?? "no data")")
                return
            }
            do {
                let result = try JSONDecoder().decode(LookupResponse.self, from: data)
                guard let entry = result.results.first,
                      self.isVersion(entry.version, greaterThan: self.currentVersion) else { return }
                let defaults = UserDefaults.standard
                defaults.set(entry.version, forKey: self.prefsLatestVersion)
                defaults.set(entry.trackViewUrl, forKey: self.prefsStoreURL)
                SLog("UpdateChecker: update available \(entry.version) (current \(self.currentVersion))")
                NotificationCenter.default.post(name: Self.updateAvailableNotification, object: nil)
                self.postSystemNotificationIfNeeded(version: entry.version, storeURL: entry.trackViewUrl)
            } catch {
                SLog("UpdateChecker: decode failed: \(error.localizedDescription)")
            }
        }.resume()
    }

    // Posts a one-off local notification, but only the first time a given
    // version is detected — re-running this every 24h while the user simply
    // hasn't updated yet would be spammy, unlike the passive in-app banner.
    private func postSystemNotificationIfNeeded(version: String, storeURL: String) {
        let defaults = UserDefaults.standard
        guard defaults.string(forKey: prefsNotifiedVersion) != version else { return }
        defaults.set(version, forKey: prefsNotifiedVersion)

        let content = UNMutableNotificationContent()
        content.title = "ShortNerdCat update available"
        content.body = "Version \(version) is ready — tap to open the App Store"
        content.sound = .default
        content.userInfo = [Self.storeURLUserInfoKey: storeURL]

        let request = UNNotificationRequest(
            identifier: Self.notificationRequestID,
            content: content,
            trigger: nil // deliver immediately
        )
        UNUserNotificationCenter.current().add(request) { error in
            if let error {
                SLog("UpdateChecker: notification add failed: \(error.localizedDescription)")
            }
        }
    }

    // Component-wise numeric comparison ("1.10" > "1.9"). App Store marketing
    // versions aren't fixed-width like the Android/Windows build-timestamp
    // versions, so the lexicographic compare used there would be wrong here.
    private func isVersion(_ a: String, greaterThan b: String) -> Bool {
        let pa = a.split(separator: ".").map { Int($0) ?? 0 }
        let pb = b.split(separator: ".").map { Int($0) ?? 0 }
        for i in 0..<max(pa.count, pb.count) {
            let x = i < pa.count ? pa[i] : 0
            let y = i < pb.count ? pb[i] : 0
            if x != y { return x > y }
        }
        return false
    }

    private struct LookupResponse: Decodable {
        let results: [LookupResult]
    }
    private struct LookupResult: Decodable {
        let version: String
        let trackViewUrl: String
    }
}

extension UpdateChecker: UNUserNotificationCenterDelegate {

    // Show the banner/sound even while the app is in the foreground — without
    // this, foreground notifications are silently suppressed by default.
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .sound, .list])
    }

    // Tapping the notification opens the App Store page directly, rather than
    // just bringing the app to the foreground and leaving the user to find the
    // banner themselves.
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        if let urlString = response.notification.request.content.userInfo[Self.storeURLUserInfoKey] as? String,
           let url = URL(string: urlString) {
            UIApplication.shared.open(url)
        }
        completionHandler()
    }
}
