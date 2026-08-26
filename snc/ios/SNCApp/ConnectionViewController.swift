// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import UIKit
import NetworkExtension
import PhotosUI
import os.log

final class ConnectionViewController: UIViewController {

    private let log = Logger(subsystem: "net.shortnerdcat.client", category: "ConnectionVC")

    // MARK: - UI elements

    private let imgState: UIImageView = {
        let v = UIImageView()
        v.contentMode = .scaleAspectFit
        v.translatesAutoresizingMaskIntoConstraints = false
        return v
    }()

    private let lblStatus: UILabel = {
        let l = UILabel()
        l.font = SNCTheme.Font.medium(17)
        l.textColor = SNCTheme.textPrimary
        l.textAlignment = .center
        l.numberOfLines = 0
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    // Orange banner shown in WildCat mode while connected/connecting.
    private let ribbonWildCat: UILabel = {
        let l = UILabel()
        l.text = L.t("wildcat.ribbon")
        l.font = SNCTheme.Font.bold(13)
        l.textColor = SNCTheme.textPrimary
        l.textAlignment = .center
        l.backgroundColor = SNCTheme.warmAmber
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    // Tappable banner shown when UpdateChecker finds a newer App Store version.
    // Opens the App Store page — iOS (App Store distribution) gives no silent
    // download/install path, so this is the full extent of the "update" UI.
    private let ribbonUpdate: UILabel = {
        let l = UILabel()
        l.font = SNCTheme.Font.bold(13)
        l.textColor = SNCTheme.textPrimary
        l.textAlignment = .center
        l.backgroundColor = SNCTheme.screenCyan
        l.isUserInteractionEnabled = true
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    private let txtKey: UITextView = {
        let v = UITextView()
        v.autocorrectionType = .no
        v.autocapitalizationType = .none
        v.spellCheckingType = .no
        v.backgroundColor = SNCTheme.surface
        v.textColor = SNCTheme.textPrimary
        v.font = SNCTheme.Font.regular(16)
        v.layer.cornerRadius = 8
        v.textContainerInset = UIEdgeInsets(top: 10, left: 8, bottom: 10, right: 8)
        v.translatesAutoresizingMaskIntoConstraints = false
        return v
    }()

    private let txtKeyPlaceholder: UILabel = {
        let l = UILabel()
        l.text = L.t("key.placeholder")
        l.textColor = SNCTheme.textMuted
        l.font = SNCTheme.Font.regular(16)
        l.numberOfLines = 0
        l.isUserInteractionEnabled = false
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    private let btnScan: UIButton = {
        var cfg = UIButton.Configuration.tinted()
        cfg.title = L.t("key.scanQR")
        cfg.image = UIImage(systemName: "qrcode.viewfinder")
        cfg.imagePadding = 6
        cfg.cornerStyle = .medium
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnScanFile: UIButton = {
        var cfg = UIButton.Configuration.tinted()
        cfg.title = L.t("key.scanFromPhoto")
        cfg.image = UIImage(systemName: "photo")
        cfg.imagePadding = 6
        cfg.cornerStyle = .medium
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    // Shown under the scan buttons only once navlink.net has been probed
    // reachable — switches from manual key entry to credential login.
    private let btnKeyEntryLogin: UIButton = {
        let b = UIButton(type: .system)
        b.setTitle(L.t("key.logInInstead"), for: .normal)
        b.setTitleColor(SNCTheme.textMuted, for: .normal)
        b.titleLabel?.font = SNCTheme.Font.medium(14)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    // MARK: - "Do you have a key?" prompt (shown first when there's no saved key)

    private let lblHaveKeyQuestion: UILabel = {
        let l = UILabel()
        l.text = "Do you have a ShortNerdCat activation key?"
        l.font = SNCTheme.Font.medium(17)
        l.textColor = SNCTheme.textPrimary
        l.textAlignment = .center
        l.numberOfLines = 0
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    private let btnHaveKeyYes: UIButton = {
        var cfg = UIButton.Configuration.filled()
        cfg.cornerStyle = .large
        cfg.baseBackgroundColor = SNCTheme.screenCyan
        cfg.title = "Yes, I have a key"
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnHaveKeyNo: UIButton = {
        var cfg = UIButton.Configuration.tinted()
        cfg.cornerStyle = .large
        cfg.title = "No, I don't have one"
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    // MARK: - Credential login (email/password against navlink.net)

    private let txtEmail: UITextField = {
        let f = UITextField()
        f.placeholder = L.t("credential.emailPlaceholder")
        f.keyboardType = .emailAddress
        f.autocapitalizationType = .none
        f.autocorrectionType = .no
        f.borderStyle = .roundedRect
        f.translatesAutoresizingMaskIntoConstraints = false
        return f
    }()

    private let txtPassword: UITextField = {
        let f = UITextField()
        f.placeholder = L.t("credential.passwordPlaceholder")
        f.isSecureTextEntry = true
        f.borderStyle = .roundedRect
        f.translatesAutoresizingMaskIntoConstraints = false
        return f
    }()

    private let btnTogglePassword: UIButton = {
        let b = UIButton(type: .system)
        b.setImage(UIImage(systemName: "eye"), for: .normal)
        b.tintColor = .secondaryLabel
        b.frame = CGRect(x: 0, y: 0, width: 36, height: 24)
        b.contentMode = .scaleAspectFit
        return b
    }()

    private let lblCredentialError: UILabel = {
        let l = UILabel()
        l.font = SNCTheme.Font.regular(13)
        l.textColor = .systemRed
        l.textAlignment = .center
        l.numberOfLines = 0
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    private let btnDoLogin: UIButton = {
        var cfg = UIButton.Configuration.filled()
        cfg.cornerStyle = .large
        cfg.baseBackgroundColor = SNCTheme.screenCyan
        cfg.title = L.t("credential.login")
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnSwitchToKeyEntry: UIButton = {
        let b = UIButton(type: .system)
        b.setTitle(L.t("credential.iHaveKey"), for: .normal)
        b.setTitleColor(SNCTheme.textMuted, for: .normal)
        b.titleLabel?.font = SNCTheme.Font.medium(14)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnConnect: UIButton = {
        var cfg = UIButton.Configuration.filled()
        cfg.cornerStyle = .large
        cfg.baseBackgroundColor = SNCTheme.screenCyan
        cfg.title = L.t("connect.button")
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnLogout: UIButton = {
        let b = UIButton(type: .system)
        b.setTitle(L.t("logout.removeKey"), for: .normal)
        b.setTitleColor(.systemRed, for: .normal)
        b.titleLabel?.font = SNCTheme.Font.medium(15)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let lblVersion: UILabel = {
        let l = UILabel()
        l.font = SNCTheme.Font.regular(12)
        l.textColor = SNCTheme.textMuted
        l.textAlignment = .center
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    // Live uplink/downlink counter, bottom-right of the screen, above the
    // version label. Hidden while not connected -- see startTrafficTimer/
    // stopTrafficTimer and the SNCGetStatus â†’ IPCReply.bytesSent/bytesRecv
    // channel in GoCore.swift / IPCProtocol.swift.
    private let lblTraffic: UILabel = {
        let l = UILabel()
        l.font = SNCTheme.Font.regular(12)
        l.textColor = SNCTheme.textMuted
        l.textAlignment = .right
        l.isHidden = true
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    private var pollTimer: Timer?
    private var tokenRefreshTimer: Timer?
    private var trafficTimer: Timer?
    private var pendingWildcatReconnect = false
    private var mainStackCenterY: NSLayoutConstraint!

    // Baseline for edge-detecting a genuine transition INTO .connected (not
    // just re-observing "still connected" on a later notification/poll) --
    // used to fire the connect haptic exactly once per real connect, and to
    // start/stop the traffic-counter timer. Set once from the real status
    // right after VPNManager finishes loading, so an app relaunch that finds
    // the tunnel already connected doesn't spuriously buzz.
    private var lastKnownStatus: NEVPNStatus = .invalid

    // "No key yet" flow: which of the three screens is currently shown.
    // Defaults to key-entry (safe fallback matching the unreachable case);
    // the have-key prompt is no longer entered automatically. Reset to
    // key-entry on logout so the flow restarts cleanly next time.
    private enum LoginScreen { case keyEntry, haveKeyPrompt, credentialLogin }
    private var loginScreen: LoginScreen = .keyEntry

    // Set once by a background probe of navlink.net (see NavlinkAuth.probe) --
    // decides whether the credential-login path is offered at all.
    private var navlinkReachable = false

    // True once the user has manually switched login screens -- once set,
    // the reachability probe's completion handler no longer auto-switches
    // from key-entry to credential-login on its own.
    private var userSwitchedLoginScreen = false

    // MARK: - Lifecycle

    override func viewDidLoad() {
        super.viewDidLoad()
        title = "ShortNerdCat"
        view.backgroundColor = SNCTheme.bgPrimary
        setupLayout()
        txtKey.delegate = self
        buildMenu()

        let version = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "1.0"
        lblVersion.text = "v\(version)"
        SLog("ConnectionVC: viewDidLoad v\(version) wildcatEnabled=\(VPNManager.shared.wildcatEnabled)")

        let hasKey = VPNManager.shared.loadKey().map { !$0.isEmpty } ?? false
        SLog("ConnectionVC: hasKey=\(hasKey), initialStatus=\(VPNManager.shared.connectionStatus.rawValue)")
        if hasKey {
            txtKey.text = VPNManager.shared.loadKey()
            txtKeyPlaceholder.isHidden = true
        } else {
            // Direct (non-tunneled) reachability probe -- decides whether the
            // credential-login path is offered at all. No VPN tunnel exists
            // yet at this point (VPNManager only connects once a key exists),
            // so NavlinkAuth's own URLSession is structurally guaranteed to
            // go straight to navlink.net.
            NavlinkAuth.probe { [weak self] reachable in
                DispatchQueue.main.async {
                    guard let self else { return }
                    self.navlinkReachable = reachable
                    if reachable && !self.userSwitchedLoginScreen && self.loginScreen == .keyEntry {
                        self.loginScreen = .credentialLogin
                    }
                    self.updateUI()
                }
            }
        }

        VPNManager.shared.load { [weak self] err in
            DispatchQueue.main.async {
                if let err {
                    self?.showAlert(String(format: L.t("alert.vpnSetupFailed"), err.localizedDescription))
                }
                guard let self else { return }
                // Baseline only -- do NOT treat "already connected at launch" as a
                // fresh transition (no haptic), but do start the traffic timer.
                self.lastKnownStatus = VPNManager.shared.connectionStatus
                if self.lastKnownStatus == .connected {
                    self.startTrafficTimer()
                }
                self.updateUI()
            }
        }

        NotificationCenter.default.addObserver(
            self, selector: #selector(vpnStatusChanged),
            name: .NEVPNStatusDidChange, object: nil)

        NotificationCenter.default.addObserver(
            self, selector: #selector(keyboardWillShow(_:)),
            name: UIResponder.keyboardWillShowNotification, object: nil)
        NotificationCenter.default.addObserver(
            self, selector: #selector(keyboardWillHide),
            name: UIResponder.keyboardWillHideNotification, object: nil)

        NotificationCenter.default.addObserver(
            self, selector: #selector(didBecomeActive),
            name: UIApplication.didBecomeActiveNotification, object: nil)

        NotificationCenter.default.addObserver(
            self, selector: #selector(updateAvailable),
            name: UpdateChecker.updateAvailableNotification, object: nil)
        UpdateChecker.shared.checkIfNeeded()

        updateUI()
    }

    override func viewWillAppear(_ animated: Bool) {
        super.viewWillAppear(animated)
        startPoll()
        updateUI()
        // Restart token refresh timer if WildCat is enabled and no timer is running.
        if VPNManager.shared.wildcatEnabled && tokenRefreshTimer == nil {
            startTokenRefresh()
        }
        if VPNManager.shared.connectionStatus == .connected {
            startTrafficTimer()
        }
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        stopPoll()
        stopTrafficTimer()
        // Token refresh timer keeps running while the app is active; stops only
        // when WildCat is disabled or the VC is deallocated.
    }

    // MARK: - Layout

    private func setupLayout() {
        let keyStack = UIStackView(arrangedSubviews: [txtKey, btnScan, btnScanFile, btnKeyEntryLogin])
        keyStack.axis = .vertical
        keyStack.spacing = 8
        keyStack.alignment = .fill
        keyStack.translatesAutoresizingMaskIntoConstraints = false

        let haveKeyStack = UIStackView(arrangedSubviews: [lblHaveKeyQuestion, btnHaveKeyYes, btnHaveKeyNo])
        haveKeyStack.axis = .vertical
        haveKeyStack.spacing = 12
        haveKeyStack.alignment = .fill
        haveKeyStack.translatesAutoresizingMaskIntoConstraints = false

        let credentialStack = UIStackView(arrangedSubviews: [txtEmail, txtPassword, lblCredentialError, btnDoLogin, btnSwitchToKeyEntry])
        credentialStack.axis = .vertical
        credentialStack.spacing = 10
        credentialStack.alignment = .fill
        credentialStack.translatesAutoresizingMaskIntoConstraints = false

        let actionStack = UIStackView(arrangedSubviews: [btnConnect, btnLogout])
        actionStack.axis = .vertical
        actionStack.spacing = 12
        actionStack.alignment = .center
        actionStack.translatesAutoresizingMaskIntoConstraints = false

        let mainStack = UIStackView(arrangedSubviews: [imgState, lblStatus, ribbonWildCat, ribbonUpdate, keyStack, haveKeyStack, credentialStack, actionStack])
        mainStack.axis = .vertical
        mainStack.spacing = 20
        mainStack.alignment = .center
        mainStack.setCustomSpacing(12, after: imgState)
        mainStack.setCustomSpacing(0, after: lblStatus)
        mainStack.setCustomSpacing(8, after: ribbonWildCat)
        mainStack.setCustomSpacing(28, after: ribbonUpdate)
        mainStack.translatesAutoresizingMaskIntoConstraints = false

        txtKey.addSubview(txtKeyPlaceholder)
        NSLayoutConstraint.activate([
            txtKeyPlaceholder.topAnchor.constraint(equalTo: txtKey.topAnchor, constant: 10),
            txtKeyPlaceholder.leadingAnchor.constraint(equalTo: txtKey.leadingAnchor, constant: 13),
            txtKeyPlaceholder.trailingAnchor.constraint(equalTo: txtKey.trailingAnchor, constant: -8),
        ])

        view.addSubview(mainStack)
        view.addSubview(lblVersion)
        view.addSubview(lblTraffic)

        NSLayoutConstraint.activate([
            mainStack.centerXAnchor.constraint(equalTo: view.centerXAnchor),
        ])
        mainStackCenterY = mainStack.centerYAnchor.constraint(equalTo: view.centerYAnchor, constant: -30)
        mainStackCenterY.isActive = true
        NSLayoutConstraint.activate([
            mainStack.leadingAnchor.constraint(greaterThanOrEqualTo: view.leadingAnchor, constant: 32),
            mainStack.trailingAnchor.constraint(lessThanOrEqualTo: view.trailingAnchor, constant: -32),

            imgState.widthAnchor.constraint(equalToConstant: 96),
            imgState.heightAnchor.constraint(equalToConstant: 96),

            ribbonWildCat.leadingAnchor.constraint(equalTo: view.leadingAnchor),
            ribbonWildCat.trailingAnchor.constraint(equalTo: view.trailingAnchor),
            ribbonWildCat.heightAnchor.constraint(equalToConstant: 36),

            ribbonUpdate.leadingAnchor.constraint(equalTo: view.leadingAnchor),
            ribbonUpdate.trailingAnchor.constraint(equalTo: view.trailingAnchor),
            ribbonUpdate.heightAnchor.constraint(equalToConstant: 36),

            keyStack.leadingAnchor.constraint(equalTo: view.leadingAnchor, constant: 32),
            keyStack.trailingAnchor.constraint(equalTo: view.trailingAnchor, constant: -32),
            txtKey.heightAnchor.constraint(equalToConstant: 100),

            haveKeyStack.leadingAnchor.constraint(equalTo: view.leadingAnchor, constant: 32),
            haveKeyStack.trailingAnchor.constraint(equalTo: view.trailingAnchor, constant: -32),

            credentialStack.leadingAnchor.constraint(equalTo: view.leadingAnchor, constant: 32),
            credentialStack.trailingAnchor.constraint(equalTo: view.trailingAnchor, constant: -32),

            btnConnect.widthAnchor.constraint(equalToConstant: 220),
            btnConnect.heightAnchor.constraint(equalToConstant: 50),

            lblVersion.centerXAnchor.constraint(equalTo: view.centerXAnchor),
            lblVersion.bottomAnchor.constraint(equalTo: view.safeAreaLayoutGuide.bottomAnchor, constant: -16),

            // Bottom-right, above the version label (the persistent bottom
            // fixture nearest the connection-status indicator).
            lblTraffic.trailingAnchor.constraint(equalTo: view.safeAreaLayoutGuide.trailingAnchor, constant: -16),
            lblTraffic.bottomAnchor.constraint(equalTo: lblVersion.topAnchor, constant: -8),
        ])

        btnConnect.addTarget(self, action: #selector(connectTapped), for: .touchUpInside)
        btnLogout.addTarget(self, action: #selector(logoutTapped), for: .touchUpInside)
        btnScan.addTarget(self, action: #selector(scanTapped), for: .touchUpInside)
        btnScanFile.addTarget(self, action: #selector(scanFileTapped), for: .touchUpInside)
        ribbonUpdate.addGestureRecognizer(UITapGestureRecognizer(target: self, action: #selector(updateBannerTapped)))

        btnKeyEntryLogin.addTarget(self, action: #selector(keyEntryLoginTapped), for: .touchUpInside)
        btnHaveKeyYes.addTarget(self, action: #selector(haveKeyYesTapped), for: .touchUpInside)
        btnHaveKeyNo.addTarget(self, action: #selector(haveKeyNoTapped), for: .touchUpInside)
        btnSwitchToKeyEntry.addTarget(self, action: #selector(switchToKeyEntryTapped), for: .touchUpInside)
        btnDoLogin.addTarget(self, action: #selector(doLoginTapped), for: .touchUpInside)

        btnTogglePassword.addTarget(self, action: #selector(togglePasswordTapped), for: .touchUpInside)
        txtPassword.rightView = btnTogglePassword
        txtPassword.rightViewMode = .always
    }

    // MARK: - Keyboard

    @objc private func keyboardWillShow(_ n: Notification) {
        guard let info = n.userInfo,
              let frame = (info[UIResponder.keyboardFrameEndUserInfoKey] as? NSValue)?.cgRectValue,
              let duration = (info[UIResponder.keyboardAnimationDurationUserInfoKey] as? NSNumber)?.doubleValue
        else { return }
        let overlap = frame.height - view.safeAreaInsets.bottom
        UIView.animate(withDuration: duration) {
            self.mainStackCenterY.constant = -30 - overlap / 2
            self.view.layoutIfNeeded()
        }
    }

    @objc private func keyboardWillHide() {
        UIView.animate(withDuration: 0.25) {
            self.mainStackCenterY.constant = -30
            self.view.layoutIfNeeded()
        }
    }

    // MARK: - Menu

    private func buildMenu() {
        let wildcatOn = VPNManager.shared.wildcatEnabled

        let wildcatAction = UIAction(
            title: L.t("menu.wildcat"),
            image: UIImage(systemName: "pawprint"),
            state: wildcatOn ? .on : .off
        ) { [weak self] _ in
            guard let self else { return }
            if VPNManager.shared.wildcatEnabled {
                self.disableWildcat()
            } else {
                self.showWildcatWarning { self.enableWildcat() }
            }
        }

        let logsAction = UIAction(
            title: L.t("menu.shareLogs"),
            image: UIImage(systemName: "square.and.arrow.up")
        ) { [weak self] _ in
            self?.shareLogs()
        }

        navigationItem.rightBarButtonItem = UIBarButtonItem(
            image: UIImage(systemName: "ellipsis.circle"),
            menu: UIMenu(children: [wildcatAction, logsAction]))
    }

    // MARK: - Actions

    @objc private func connectTapped() {
        if !VPNManager.shared.isLoaded {
            btnConnect.isEnabled = false
            VPNManager.shared.load { [weak self] err in
                DispatchQueue.main.async {
                    self?.btnConnect.isEnabled = true
                    if let err { self?.showAlert(err.localizedDescription); return }
                    self?.connectTapped()
                }
            }
            return
        }
        let status = VPNManager.shared.connectionStatus
        log.info("connectTapped: status=\(status.rawValue)")
        if status == .connected || status == .connecting || status == .disconnecting {
            log.info("connectTapped: disconnecting")
            VPNManager.shared.disconnect()
            updateUI()
            return
        }
        let key = txtKey.text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !key.isEmpty else {
            showAlert(L.t("alert.enterKeyFirst"))
            return
        }
        VPNManager.shared.saveKey(key)
        doConnect()
    }

    // Starts the tunnel. WildCat credential acquisition (if enabled and the
    // cached token is missing/stale) happens elsewhere; the toggle itself is
    // unconditional here.
    private func doConnect() {
        let wildcatOn = VPNManager.shared.wildcatEnabled
        let hasToken  = VPNManager.shared.storedWildcatToken() != nil
        log.info("doConnect: wildcat=\(wildcatOn), hasToken=\(hasToken)")
        connectAndReport()
    }

    private func connectAndReport() {
        VPNManager.shared.connect { [weak self] err in
            DispatchQueue.main.async {
                if let err {
                    self?.log.error("doConnect: connect error: \(err.localizedDescription, privacy: .public)")
                    self?.showAlert(err.localizedDescription)
                }
                self?.updateUI()
            }
        }
        updateUI()
    }

    @objc private func logoutTapped() {
        SLog("user: remove key tapped")
        let alert = UIAlertController(
            title: L.t("logout.confirmTitle"),
            message: L.t("logout.confirmMessage"),
            preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: L.t("logout.confirmRemove"), style: .destructive) { [weak self] _ in
            SLog("user: key removed confirmed")
            VPNManager.shared.disconnect()
            VPNManager.shared.saveKey("")
            self?.txtKey.text = ""
            self?.loginScreen = .keyEntry
            self?.userSwitchedLoginScreen = false
            self?.updateUI()
        })
        alert.addAction(UIAlertAction(title: L.t("common.cancel"), style: .cancel))
        present(alert, animated: true)
    }

    @objc private func scanTapped() {
        log.info("user: scan QR tapped")
        let scanner = QRScanViewController { [weak self] content in
            self?.dismiss(animated: true) { self?.handleScanned(content) }
        }
        present(scanner, animated: true)
    }

    @objc private func scanFileTapped() {
        log.info("user: scan QR from file tapped")
        var config = PHPickerConfiguration()
        config.filter = .images
        config.selectionLimit = 1
        let picker = PHPickerViewController(configuration: config)
        picker.delegate = self
        present(picker, animated: true)
    }

    // MARK: - "Do you have a key?" / credential login

    @objc private func keyEntryLoginTapped() {
        userSwitchedLoginScreen = true
        loginScreen = .credentialLogin
        updateUI()
    }

    @objc private func haveKeyYesTapped() {
        userSwitchedLoginScreen = true
        loginScreen = .keyEntry
        updateUI()
    }

    @objc private func haveKeyNoTapped() {
        userSwitchedLoginScreen = true
        loginScreen = navlinkReachable ? .credentialLogin : .keyEntry
        updateUI()
    }

    @objc private func switchToKeyEntryTapped() {
        userSwitchedLoginScreen = true
        loginScreen = .keyEntry
        updateUI()
    }

    @objc private func togglePasswordTapped() {
        txtPassword.isSecureTextEntry.toggle()
        // Toggling isSecureTextEntry drops the current text on some iOS versions
        // unless the field is forced to re-layout its text storage.
        if let existing = txtPassword.text {
            txtPassword.text = nil
            txtPassword.text = existing
        }
        let symbol = txtPassword.isSecureTextEntry ? "eye" : "eye.slash"
        btnTogglePassword.setImage(UIImage(systemName: symbol), for: .normal)
    }

    @objc private func doLoginTapped() {
        let email = (txtEmail.text ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        let password = txtPassword.text ?? ""
        guard !email.isEmpty, !password.isEmpty else {
            lblCredentialError.text = L.t("credential.missingFields")
            updateUI()
            return
        }
        lblCredentialError.text = ""
        btnDoLogin.isEnabled = false
        updateUI()
        NavlinkAuth.login(email: email, password: password) { [weak self] error in
            guard let self else { return }
            if let error {
                DispatchQueue.main.async {
                    self.btnDoLogin.isEnabled = true
                    self.lblCredentialError.text = error.localizedDescription
                    self.updateUI()
                }
                return
            }
            NavlinkAuth.freeKey { result in
                DispatchQueue.main.async {
                    self.btnDoLogin.isEnabled = true
                    switch result {
                    case .success(let issued):
                        self.applyKey(issued.key)
                    case .failure(let err):
                        self.lblCredentialError.text = err.localizedDescription
                        self.updateUI()
                    }
                }
            }
        }
    }

    @objc private func vpnStatusChanged() {
        DispatchQueue.main.async {
            let status = VPNManager.shared.connectionStatus
            self.log.info("vpnStatusChanged: status=\(status.rawValue), pendingReconnect=\(self.pendingWildcatReconnect)")

            // Genuine transition into the fully-connected state (not merely
            // re-observing "still connected" on a later notification) --
            // fires the triple haptic exactly once per real connect, and
            // starts/stops the live traffic counter in step with it.
            if status == .connected && self.lastKnownStatus != .connected {
                self.triggerConnectHaptic()
            }
            if status == .connected {
                self.startTrafficTimer()
            } else {
                self.stopTrafficTimer()
            }
            self.lastKnownStatus = status

            if self.pendingWildcatReconnect, status == .disconnected {
                self.pendingWildcatReconnect = false
                if (VPNManager.shared.loadKey() ?? "").isEmpty == false {
                    self.log.info("vpnStatusChanged: auto-reconnecting after WildCat toggle")
                    self.doConnect()
                }
                return
            }
            self.updateUI()
        }
    }

    /// Three short haptic pulses on a genuine transition into "connected" --
    /// must run on the main app process (this class), never inside the
    /// Network Extension, which cannot trigger haptics at all.
    private func triggerConnectHaptic() {
        log.info("triggerConnectHaptic: firing 3 short impact pulses")
        let generator = UIImpactFeedbackGenerator(style: .medium)
        generator.prepare()
        for i in 0..<3 {
            DispatchQueue.main.asyncAfter(deadline: .now() + Double(i) * 0.1) {
                generator.impactOccurred()
            }
        }
    }

    @objc private func didBecomeActive() {
        UpdateChecker.shared.checkIfNeeded()
        updateUI()
    }

    @objc private func updateAvailable() {
        DispatchQueue.main.async { [weak self] in self?.updateUI() }
    }

    @objc private func updateBannerTapped() {
        guard let update = UpdateChecker.shared.availableUpdate else { return }
        log.info("user: update banner tapped, version=\(update.version)")
        UIApplication.shared.open(update.storeURL)
    }

    // MARK: - WildCat enable / disable

    // Toggle is unconditional — no credential gate at toggle time.
    // A token is acquired at connect time (doConnect), matching Android/macOS/Windows behavior.
    private func enableWildcat() {
        log.info("enableWildcat: status=\(VPNManager.shared.connectionStatus.rawValue)")
        VPNManager.shared.setWildcat(true)
        startTokenRefresh()
        buildMenu()
        updateUI()
        let s = VPNManager.shared.connectionStatus
        if s == .connected || s == .connecting {
            log.info("enableWildcat: disconnecting for reconnect")
            pendingWildcatReconnect = true
            VPNManager.shared.disconnect()
        }
    }

    private func showWildcatWarning(onConfirm: @escaping () -> Void) {
        let alert = UIAlertController(
            title: L.t("wildcat.warning.title"),
            message: L.t("wildcat.warning.message"),
            preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: L.t("common.ok"), style: .default) { _ in onConfirm() })
        present(alert, animated: true)
    }

    private func disableWildcat() {
        log.info("disableWildcat: status=\(VPNManager.shared.connectionStatus.rawValue)")
        stopTokenRefresh()
        VPNManager.shared.setWildcat(false)
        buildMenu()
        updateUI()
        let s = VPNManager.shared.connectionStatus
        if s == .connected || s == .connecting {
            log.info("disableWildcat: disconnecting for reconnect")
            pendingWildcatReconnect = true
            VPNManager.shared.disconnect()
        }
    }

    // MARK: - Token refresh

    private func startTokenRefresh() {
        stopTokenRefresh()
        tokenRefreshTimer = Timer.scheduledTimer(withTimeInterval: 10 * 60, repeats: true) { [weak self] _ in
            self?.performTokenRefresh()
        }
    }

    private func stopTokenRefresh() {
        tokenRefreshTimer?.invalidate()
        tokenRefreshTimer = nil
    }

    private func performTokenRefresh() {
        log.info("performTokenRefresh: starting silent refresh")
    }

    // MARK: - Polling

    private func startPoll() {
        pollTimer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            self?.updateUI()
        }
    }

    private func stopPoll() {
        pollTimer?.invalidate()
        pollTimer = nil
    }

    // MARK: - Live traffic counter

    /// Starts (or no-ops if already running) a 1s timer that pulls the
    /// current uplink/downlink byte counts through the existing status IPC
    /// channel (VPNManager.fetchStatus â†’ IPCReply, sourced from
    /// core.TotalBytes on the Go side) and refreshes lblTraffic.
    private func startTrafficTimer() {
        guard trafficTimer == nil else { return }
        lblTraffic.isHidden = false
        refreshTraffic()
        trafficTimer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
            self?.refreshTraffic()
        }
    }

    private func stopTrafficTimer() {
        trafficTimer?.invalidate()
        trafficTimer = nil
        lblTraffic.isHidden = true
        lblTraffic.text = nil
    }

    private func refreshTraffic() {
        VPNManager.shared.fetchStatus { [weak self] reply in
            guard let self, let reply else { return }
            DispatchQueue.main.async {
                // Guard against a reply arriving just after disconnect raced
                // ahead of it -- don't resurrect a hidden counter.
                guard !self.lblTraffic.isHidden else { return }
                self.lblTraffic.text = Self.formattedTraffic(sent: reply.bytesSent, recv: reply.bytesRecv)
            }
        }
    }

    private static func formattedTraffic(sent: Int64, recv: Int64) -> String {
        let fmt = ByteCountFormatter()
        fmt.countStyle = .binary
        fmt.allowedUnits = [.useKB, .useMB, .useGB]
        fmt.isAdaptive = true
        fmt.includesUnit = true
        fmt.includesCount = true
        return "\u{2191} \(fmt.string(fromByteCount: sent))  \u{2193} \(fmt.string(fromByteCount: recv))"
    }

    // MARK: - State

    private func updateUI() {
        let neStatus      = VPNManager.shared.connectionStatus
        let tunnelState   = VPNManager.shared.tunnelState
        let wildcat       = VPNManager.shared.wildcatEnabled
        var hasKey        = !(VPNManager.shared.loadKey() ?? "").isEmpty

        let connected     = neStatus == .connected
        let connecting    = neStatus == .connecting || neStatus == .reasserting
        let disconnecting = neStatus == .disconnecting
        let keyDenied     = tunnelState == "key_denied"
        let wildcatExpired = tunnelState == "wildcat_auth_expired"
        let busy          = connected || connecting

        // Clearing the saved key is what actually gets the user back to a
        // login screen: showKeyEntry/showHaveKeyPrompt/showCredentialLogin
        // below are all gated on hasKey alone, not on keyDenied by itself --
        // without this, a denial only changed the icon/status text and left
        // the connect screen up with a dead-end error, same bug the
        // Windows/Mac/Linux/Android clients had for their own "login error"/
        // "key_denied" state (found auditing all 5 platforms, 2026-08-16).
        // Self-limiting: once cleared, hasKey is false, so this can't fire
        // again on the next updateUI() until a fresh key is entered.
        if keyDenied && hasKey {
            VPNManager.shared.saveKey("")
            hasKey = false
        }

        // WildCat ribbon
        ribbonWildCat.isHidden = !(busy && wildcat)

        // Update-available ribbon — tap opens the App Store page (see UpdateChecker).
        if let update = UpdateChecker.shared.availableUpdate {
            ribbonUpdate.text = String(format: L.t("status.updateAvailable"), update.version)
            ribbonUpdate.isHidden = false
        } else {
            ribbonUpdate.isHidden = true
        }

        // Status image
        let imageName: String
        switch true {
        case keyDenied, wildcatExpired:
            imageName = "snc_error"
        case connected && wildcat:
            imageName = "snc_wildcat"
        case connected:
            imageName = "snc_connected"
        case connecting || disconnecting:
            imageName = "snc_connecting"
        case neStatus == .invalid:
            imageName = "snc_error"
        default:
            imageName = "snc_idle"
        }
        imgState.image = UIImage(named: imageName)
        imgState.tintColor = nil

        // Status text
        lblStatus.text = {
            if keyDenied      { return L.t("status.keyRejected") }
            if wildcatExpired { return L.t("status.wildcatExpired") }
            if connected      { return wildcat ? L.t("status.connectedWildcat") : L.t("status.connected") }
            if connecting    { return L.t("status.connecting") }
            if disconnecting { return L.t("status.disconnecting") }
            return hasKey ? L.t("status.tapConnect") : L.t("status.enterKey")
        }()

        // "No key yet" flow: exactly one of key-entry / have-key-prompt /
        // credential-login is visible, chosen by loginScreen (see the
        // handlers above). All three collapse once a key is saved.
        let showKeyEntry = !hasKey && loginScreen == .keyEntry
        let showHaveKeyPrompt = !hasKey && loginScreen == .haveKeyPrompt
        let showCredentialLogin = !hasKey && loginScreen == .credentialLogin

        txtKey.isHidden             = !showKeyEntry
        txtKeyPlaceholder.isHidden  = !showKeyEntry || !txtKey.text.isEmpty
        btnScan.isHidden            = !showKeyEntry
        btnScanFile.isHidden        = !showKeyEntry
        btnKeyEntryLogin.isHidden   = !(showKeyEntry && navlinkReachable)

        lblHaveKeyQuestion.isHidden = !showHaveKeyPrompt
        btnHaveKeyYes.isHidden      = !showHaveKeyPrompt
        btnHaveKeyNo.isHidden       = !showHaveKeyPrompt

        txtEmail.isHidden           = !showCredentialLogin
        txtPassword.isHidden        = !showCredentialLogin
        btnDoLogin.isHidden         = !showCredentialLogin
        btnSwitchToKeyEntry.isHidden = !showCredentialLogin
        lblCredentialError.isHidden = !showCredentialLogin || (lblCredentialError.text ?? "").isEmpty

        // Connect / Disconnect button — disabled while disconnecting to prevent double-tap
        var cfg = UIButton.Configuration.filled()
        cfg.cornerStyle = .large
        cfg.baseBackgroundColor = busy ? .systemRed : SNCTheme.screenCyan
        cfg.title = busy ? L.t("connect.disconnect") : L.t("connect.button")
        btnConnect.configuration = cfg
        btnConnect.isEnabled = !disconnecting

        // Logout button — visible only when key is saved
        btnLogout.isHidden = !hasKey
    }

    // MARK: - Helpers

    private func handleScanned(_ content: String) {
        if content.hasPrefix("http://") || content.hasPrefix("https://") {
            guard let url = URL(string: content) else { return }
            URLSession.shared.dataTask(with: url) { [weak self] data, _, _ in
                let key = (data.flatMap { String(data: $0, encoding: .utf8) } ?? "")
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                DispatchQueue.main.async {
                    if key.isEmpty { self?.showAlert(L.t("alert.fetchKeyFailed")) }
                    else           { self?.applyKey(key) }
                }
            }.resume()
        } else if content.hasPrefix("navlink://") {
            if let comps = URLComponents(string: content),
               let key = comps.queryItems?.first(where: { $0.name == "key" })?.value {
                applyKey(key)
            }
        } else {
            applyKey(content.trimmingCharacters(in: .whitespacesAndNewlines))
        }
    }

    func applyKey(_ key: String) {
        guard !key.isEmpty else { showAlert(L.t("alert.emptyKey")); return }
        txtKey.text = key
        VPNManager.shared.saveKey(key)
        updateUI()
        showAlert(L.t("alert.keySaved"))
    }

    private func showAlert(_ message: String) {
        let alert = UIAlertController(title: nil, message: message, preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: "OK", style: .default))
        present(alert, animated: true)
    }

    private func shareLogs() {
        SLog("shareLogs: requested")
        guard let container = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.net.shortnerdcat") else {
            showAlert(L.t("alert.noAppContainer")); return
        }
        let logDir = container.appendingPathComponent("Library/Logs/tunnel")
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            let zipURL = FileManager.default.temporaryDirectory
                .appendingPathComponent("snc-logs.zip")
            try? FileManager.default.removeItem(at: zipURL)

            let logFiles = (try? FileManager.default.contentsOfDirectory(
                at: logDir, includingPropertiesForKeys: [.contentModificationDateKey]))
                .map { files in files.filter { $0.pathExtension == "log" }
                    .sorted { $0.lastPathComponent > $1.lastPathComponent }
                    .prefix(15)
                    .map { $0 }
                } ?? []

            guard !logFiles.isEmpty else {
                DispatchQueue.main.async { self.showAlert(L.t("alert.noLogsFound")) }
                return
            }

            // Build a single concatenated text file (no third-party zip needed).
            var combined = "ShortNerdCat iOS logs — \(Date())\n\n"
            for f in logFiles {
                combined += "=== \(f.lastPathComponent) ===\n"
                combined += (try? String(contentsOf: f, encoding: .utf8)) ?? "(unreadable)\n"
                combined += "\n"
            }
            let txtURL = FileManager.default.temporaryDirectory
                .appendingPathComponent("snc-logs.txt")
            try? combined.write(to: txtURL, atomically: true, encoding: .utf8)
            SLog("shareLogs: prepared \(logFiles.count) files → snc-logs.txt")

            DispatchQueue.main.async {
                let vc = UIActivityViewController(activityItems: [txtURL], applicationActivities: nil)
                vc.popoverPresentationController?.barButtonItem = self.navigationItem.rightBarButtonItem
                self.present(vc, animated: true)
            }
        }
    }
}

// MARK: - UITextViewDelegate

extension ConnectionViewController: UITextViewDelegate {
    func textViewDidChange(_ textView: UITextView) {
        txtKeyPlaceholder.isHidden = !textView.text.isEmpty
    }
}

// MARK: - PHPickerViewControllerDelegate

extension ConnectionViewController: PHPickerViewControllerDelegate {
    func picker(_ picker: PHPickerViewController, didFinishPicking results: [PHPickerResult]) {
        dismiss(animated: true)
        guard let provider = results.first?.itemProvider,
              provider.canLoadObject(ofClass: UIImage.self) else { return }
        provider.loadObject(ofClass: UIImage.self) { [weak self] obj, _ in
            guard let image = obj as? UIImage,
                  let ciImage = CIImage(image: image) else { return }
            let detector = CIDetector(ofType: CIDetectorTypeQRCode,
                                      context: nil,
                                      options: [CIDetectorAccuracy: CIDetectorAccuracyHigh])
            let features = detector?.features(in: ciImage) as? [CIQRCodeFeature] ?? []
            guard let msg = features.first?.messageString else {
                DispatchQueue.main.async {
                    self?.showAlert(L.t("alert.noQRFound"))
                }
                return
            }
            DispatchQueue.main.async {
                self?.handleScanned(msg)
            }
        }
    }
}
