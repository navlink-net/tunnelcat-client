// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import UIKit

@main
final class AppDelegate: UIResponder, UIApplicationDelegate {

    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?) -> Bool {
        // Init SwiftLog so lifecycle events go to snc_lifecycle.log (same dir as Go logs).
        if let container = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.net.shortnerdcat") {
            let logDir = container.appendingPathComponent("Library/Logs/tunnel")
            SwiftLog.shared.setup(logDir: logDir)
        }
        SLog("AppDelegate: didFinishLaunching")
        UpdateChecker.shared.requestNotificationPermissionIfNeeded()
        VPNManager.shared.load { err in
            if let err { SLog("AppDelegate: VPNManager.load error: \(err.localizedDescription)") }
            else        { SLog("AppDelegate: VPNManager.load OK") }
        }

        return true
    }

    func application(_ application: UIApplication,
                     configurationForConnecting connectingSceneSession: UISceneSession,
                     options: UIScene.ConnectionOptions) -> UISceneConfiguration {
        UISceneConfiguration(name: "Default Configuration", sessionRole: connectingSceneSession.role)
    }
}
