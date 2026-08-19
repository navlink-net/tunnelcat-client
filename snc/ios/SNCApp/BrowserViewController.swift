// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import UIKit
import WebKit

/// In-app browser. WKWebView loads directly — traffic goes through the
/// active VPN tunnel.
///
/// Tab state is persisted to the shared App Group UserDefaults so it survives
/// app restarts.
final class BrowserViewController: UIViewController {

    private static let homeURL         = URL(string: "https://www.navlink.net")!
    private static let prefsTabsKey    = "snc_browse_tabs"
    private static let prefsCurrentKey = "snc_browse_current"

    // MARK: - UI

    private var webView: WKWebView!
    private let toolbar    = UIView()
    private let urlBar     = UITextField()
    private let btnBack    = UIButton(type: .system)
    private let btnForward = UIButton(type: .system)
    private let btnReload  = UIButton(type: .system)
    private let btnNewTab  = UIButton(type: .system)
    private let progress   = UIProgressView(progressViewStyle: .bar)

    private var progressObs: NSKeyValueObservation?

    // MARK: - Tab model

    private struct Tab {
        var url: URL
        var title: String
    }

    private var tabs: [Tab] = [Tab(url: BrowserViewController.homeURL, title: "")]
    private var currentTab = 0

    // MARK: - Lifecycle

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = UIColor(red: 0.02, green: 0.02, blue: 0.1, alpha: 1)
        setupWebView()
        setupChrome()
        loadPersistedTabs()
        navigate(to: tabs[currentTab].url)
    }

    override func viewWillAppear(_ animated: Bool) {
        super.viewWillAppear(animated)
        updateNavButtons()
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        captureCurrentURL()
        persistTabs()
    }

    // MARK: - Setup

    private func setupWebView() {
        let cfg = WKWebViewConfiguration()
        cfg.allowsInlineMediaPlayback = true
        cfg.mediaTypesRequiringUserActionForPlayback = []

        webView = WKWebView(frame: .zero, configuration: cfg)
        webView.allowsBackForwardNavigationGestures = true
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.translatesAutoresizingMaskIntoConstraints = false

        progressObs = webView.observe(\.estimatedProgress, options: .new) { [weak self] wv, _ in
            DispatchQueue.main.async {
                let p = Float(wv.estimatedProgress)
                self?.progress.setProgress(p, animated: true)
                self?.progress.isHidden = p >= 1.0
            }
        }
    }

    private func setupChrome() {
        // URL bar
        urlBar.placeholder = "Search or enter URL"
        urlBar.borderStyle = .roundedRect
        urlBar.backgroundColor = UIColor(white: 1, alpha: 0.08)
        urlBar.textColor = .white
        urlBar.tintColor = .systemBlue
        urlBar.attributedPlaceholder = NSAttributedString(
            string: "Search or enter URL",
            attributes: [.foregroundColor: UIColor(white: 1, alpha: 0.4)])
        urlBar.keyboardType = .URL
        urlBar.autocapitalizationType = .none
        urlBar.autocorrectionType = .no
        urlBar.returnKeyType = .go
        urlBar.delegate = self
        urlBar.translatesAutoresizingMaskIntoConstraints = false

        // Nav buttons
        let buttons: [(UIButton, String)] = [
            (btnBack,    "chevron.left"),
            (btnForward, "chevron.right"),
            (btnReload,  "arrow.clockwise"),
            (btnNewTab,  "square.on.square"),
        ]
        for (btn, sym) in buttons {
            btn.setImage(UIImage(systemName: sym), for: .normal)
            btn.tintColor = .white
            btn.translatesAutoresizingMaskIntoConstraints = false
        }
        btnBack.addTarget(self,    action: #selector(backTapped),    for: .touchUpInside)
        btnForward.addTarget(self, action: #selector(forwardTapped), for: .touchUpInside)
        btnReload.addTarget(self,  action: #selector(reloadTapped),  for: .touchUpInside)
        btnNewTab.addTarget(self,  action: #selector(tabsTapped),    for: .touchUpInside)

        // Progress bar
        progress.tintColor = .systemBlue
        progress.isHidden = true
        progress.translatesAutoresizingMaskIntoConstraints = false

        // Toolbar background
        toolbar.backgroundColor = UIColor(red: 0.04, green: 0.04, blue: 0.12, alpha: 1)
        toolbar.translatesAutoresizingMaskIntoConstraints = false

        let navStack = UIStackView(arrangedSubviews: [btnBack, btnForward, btnReload, btnNewTab])
        navStack.axis = .horizontal
        navStack.spacing = 0
        navStack.distribution = .fillEqually
        navStack.translatesAutoresizingMaskIntoConstraints = false

        toolbar.addSubview(urlBar)
        toolbar.addSubview(navStack)

        view.addSubview(webView)
        view.addSubview(toolbar)
        view.addSubview(progress)

        let safe = view.safeAreaLayoutGuide
        NSLayoutConstraint.activate([
            toolbar.topAnchor.constraint(equalTo: safe.topAnchor),
            toolbar.leadingAnchor.constraint(equalTo: view.leadingAnchor),
            toolbar.trailingAnchor.constraint(equalTo: view.trailingAnchor),
            toolbar.heightAnchor.constraint(equalToConstant: 88),

            urlBar.topAnchor.constraint(equalTo: toolbar.topAnchor, constant: 8),
            urlBar.leadingAnchor.constraint(equalTo: toolbar.leadingAnchor, constant: 12),
            urlBar.trailingAnchor.constraint(equalTo: toolbar.trailingAnchor, constant: -12),
            urlBar.heightAnchor.constraint(equalToConstant: 36),

            navStack.leadingAnchor.constraint(equalTo: toolbar.leadingAnchor, constant: 16),
            navStack.trailingAnchor.constraint(equalTo: toolbar.trailingAnchor, constant: -16),
            navStack.bottomAnchor.constraint(equalTo: toolbar.bottomAnchor, constant: -4),
            navStack.heightAnchor.constraint(equalToConstant: 36),

            progress.topAnchor.constraint(equalTo: toolbar.bottomAnchor),
            progress.leadingAnchor.constraint(equalTo: view.leadingAnchor),
            progress.trailingAnchor.constraint(equalTo: view.trailingAnchor),

            webView.topAnchor.constraint(equalTo: toolbar.bottomAnchor),
            webView.leadingAnchor.constraint(equalTo: view.leadingAnchor),
            webView.trailingAnchor.constraint(equalTo: view.trailingAnchor),
            webView.bottomAnchor.constraint(equalTo: safe.bottomAnchor),
        ])
    }

    // MARK: - Navigation

    private func navigate(to url: URL) {
        urlBar.text = url.absoluteString
        webView.load(URLRequest(url: url))
        tabs[currentTab].url = url
    }

    private func normalizeInput(_ input: String) -> URL {
        let s = input.trimmingCharacters(in: .whitespaces)
        if s.hasPrefix("http://") || s.hasPrefix("https://") {
            return URL(string: s) ?? Self.homeURL
        }
        if s.contains(".") && !s.contains(" ") {
            return URL(string: "https://\(s)") ?? Self.homeURL
        }
        let q = s.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? s
        return URL(string: "https://www.google.com/search?q=\(q)") ?? Self.homeURL
    }

    @objc private func backTapped()    { webView.goBack() }
    @objc private func forwardTapped() { webView.goForward() }
    @objc private func reloadTapped()  { webView.reload() }

    @objc private func tabsTapped() {
        let alert = UIAlertController(title: "Tabs", message: nil, preferredStyle: .actionSheet)

        for (i, tab) in tabs.enumerated() {
            let label = tab.title.isEmpty ? tab.url.absoluteString : tab.title
            let prefix = i == currentTab ? "✓ " : "   "
            alert.addAction(UIAlertAction(title: prefix + String(label.prefix(40)), style: .default) { [weak self] _ in
                self?.switchToTab(i)
            })
        }
        alert.addAction(UIAlertAction(title: "+ New tab", style: .default) { [weak self] _ in
            self?.openNewTab()
        })
        if tabs.count > 1 {
            alert.addAction(UIAlertAction(title: "Close current tab", style: .destructive) { [weak self] _ in
                self?.closeCurrentTab()
            })
        }
        alert.addAction(UIAlertAction(title: "Cancel", style: .cancel))

        alert.popoverPresentationController?.sourceView = btnNewTab
        present(alert, animated: true)
    }

    private func openNewTab() {
        captureCurrentURL()
        tabs.append(Tab(url: Self.homeURL, title: ""))
        currentTab = tabs.count - 1
        navigate(to: Self.homeURL)
        persistTabs()
    }

    private func closeCurrentTab() {
        tabs.remove(at: currentTab)
        currentTab = max(0, currentTab - 1)
        navigate(to: tabs[currentTab].url)
        persistTabs()
    }

    private func switchToTab(_ index: Int) {
        guard index != currentTab else { return }
        captureCurrentURL()
        currentTab = index
        navigate(to: tabs[currentTab].url)
    }

    private func captureCurrentURL() {
        if let url = webView.url,
           url.scheme == "https" || url.scheme == "http" {
            tabs[currentTab].url = url
        }
    }

    private func updateNavButtons() {
        btnBack.alpha    = webView?.canGoBack    == true ? 1.0 : 0.35
        btnForward.alpha = webView?.canGoForward == true ? 1.0 : 0.35
    }

    // MARK: - Persistence

    private func persistTabs() {
        let data: [[String: String]] = tabs.map { ["url": $0.url.absoluteString, "title": $0.title] }
        let prefs = UserDefaults(suiteName: "group.net.shortnerdcat")
        prefs?.set(data, forKey: Self.prefsTabsKey)
        prefs?.set(currentTab, forKey: Self.prefsCurrentKey)
    }

    private func loadPersistedTabs() {
        let prefs = UserDefaults(suiteName: "group.net.shortnerdcat")
        guard let raw = prefs?.array(forKey: Self.prefsTabsKey) as? [[String: String]],
              !raw.isEmpty else { return }
        let loaded = raw.compactMap { dict -> Tab? in
            guard let s = dict["url"], let u = URL(string: s) else { return nil }
            return Tab(url: u, title: dict["title"] ?? "")
        }
        if !loaded.isEmpty {
            tabs = loaded
            currentTab = min(max(0, prefs?.integer(forKey: Self.prefsCurrentKey) ?? 0),
                            tabs.count - 1)
        }
    }
}

// MARK: - WKNavigationDelegate

extension BrowserViewController: WKNavigationDelegate {

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        progress.isHidden = true
        if let url = webView.url { tabs[currentTab].url = url; urlBar.text = url.absoluteString }
        if let t = webView.title, !t.isEmpty { tabs[currentTab].title = t }
        updateNavButtons()
        persistTabs()
    }

    func webView(_ webView: WKWebView, didStartProvisionalNavigation navigation: WKNavigation!) {
        progress.setProgress(0.05, animated: false)
        progress.isHidden = false
        updateNavButtons()
    }

    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
        progress.isHidden = true
        updateNavButtons()
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        progress.isHidden = true
    }
}

// MARK: - WKUIDelegate

extension BrowserViewController: WKUIDelegate {
    // Open target=_blank links in the same view instead of dropping them.
    func webView(_ webView: WKWebView,
                 createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction,
                 windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = navigationAction.request.url { navigate(to: url) }
        return nil
    }
}

// MARK: - UITextFieldDelegate

extension BrowserViewController: UITextFieldDelegate {
    func textFieldShouldReturn(_ textField: UITextField) -> Bool {
        navigate(to: normalizeInput(textField.text ?? ""))
        textField.resignFirstResponder()
        return true
    }

    func textFieldDidBeginEditing(_ textField: UITextField) {
        textField.selectAll(nil)
    }
}
