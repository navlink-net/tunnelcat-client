// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import UIKit

final class SceneDelegate: UIResponder, UIWindowSceneDelegate {

    var window: UIWindow?

    func scene(_ scene: UIScene, willConnectTo session: UISceneSession,
               options connectionOptions: UIScene.ConnectionOptions) {
        guard let windowScene = scene as? UIWindowScene else { return }

        let conn    = UINavigationController(rootViewController: ConnectionViewController())
        let browser = BrowserViewController()

        conn.tabBarItem    = UITabBarItem(title: L.t("tab.connect"), image: UIImage(systemName: "network"), tag: 0)
        browser.tabBarItem = UITabBarItem(title: L.t("tab.browse"),  image: UIImage(systemName: "safari"),  tag: 1)

        let tab = UITabBarController()
        tab.viewControllers = [conn, browser]

        let appearance = UITabBarAppearance()
        appearance.configureWithOpaqueBackground()
        appearance.backgroundColor = UIColor(red: 0.04, green: 0.04, blue: 0.12, alpha: 1)
        tab.tabBar.standardAppearance = appearance
        tab.tabBar.scrollEdgeAppearance = appearance

        window = UIWindow(windowScene: windowScene)
        window?.rootViewController = tab
        window?.makeKeyAndVisible()

        // Handle navlink://activate?key=... if the app was cold-launched from a deep link.
        for ctx in connectionOptions.urlContexts {
            handleNavlinkURL(ctx.url)
        }
    }

    // Called when the app is already running and a navlink:// URL is opened.
    func scene(_ scene: UIScene, openURLContexts urlContexts: Set<UIOpenURLContext>) {
        for ctx in urlContexts {
            handleNavlinkURL(ctx.url)
        }
    }

    private func handleNavlinkURL(_ url: URL) {
        guard url.scheme == "navlink", url.host == "activate" else { return }
        guard let comps = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let key = comps.queryItems?.first(where: { $0.name == "key" })?.value,
              !key.isEmpty else { return }
        // Deliver key to ConnectionViewController and switch to the Connect tab.
        guard let tab = window?.rootViewController as? UITabBarController else { return }
        tab.selectedIndex = 0
        if let nav = tab.viewControllers?.first as? UINavigationController,
           let conn = nav.topViewController as? ConnectionViewController {
            conn.applyKey(key)
        }
    }
}
