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
        l.text = "Paste subscription key"
        l.textColor = SNCTheme.textMuted
        l.font = SNCTheme.Font.regular(16)
        l.numberOfLines = 0
        l.isUserInteractionEnabled = false
        l.translatesAutoresizingMaskIntoConstraints = false
        return l
    }()

    private let btnScan: UIButton = {
        var cfg = UIButton.Configuration.tinted()
        cfg.title = "Scan QR"
        cfg.image = UIImage(systemName: "qrcode.viewfinder")
        cfg.imagePadding = 6
        cfg.cornerStyle = .medium
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnScanFile: UIButton = {
        var cfg = UIButton.Configuration.tinted()
        cfg.title = "Scan from Photo"
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
        b.setTitle("Log In Instead", for: .normal)
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
        f.placeholder = "Email"
        f.keyboardType = .emailAddress
        f.autocapitalizationType = .none
        f.autocorrectionType = .no
        f.borderStyle = .roundedRect
        f.translatesAutoresizingMaskIntoConstraints = false
        return f
    }()

    private let txtPassword: UITextField = {
        let f = UITextField()
        f.placeholder = "Password"
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
        cfg.title = "Login"
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnSwitchToKeyEntry: UIButton = {
        let b = UIButton(type: .system)
        b.setTitle("I Have a Key", for: .normal)
        b.setTitleColor(SNCTheme.textMuted, for: .normal)
        b.titleLabel?.font = SNCTheme.Font.medium(14)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnConnect: UIButton = {
        var cfg = UIButton.Configuration.filled()
        cfg.cornerStyle = .large
        cfg.baseBackgroundColor = SNCTheme.screenCyan
        cfg.title = "Connect"
        let b = UIButton(configuration: cfg)
        b.translatesAutoresizingMaskIntoConstraints = false
        return b
    }()

    private let btnLogout: UIButton = {
        let b = UIButton(type: .system)
        b.setTitle("Remove Key", for: .normal)
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

    private var pollTimer: Timer?
    private var mainStackCenterY: NSLayoutConstraint!

    // "No key yet" flow: which of the three screens is currently shown.
    // Starts at the have-key prompt; explicitly reset there on logout so the
    // flow restarts cleanly next time.
    private enum LoginScreen { case keyEntry, haveKeyPrompt, credentialLogin }
    private var loginScreen: LoginScreen = .haveKeyPrompt

    // Set once by a background probe of navlink.net (see NavlinkAuth.probe) --
    // decides whether the credential-login path is offered at all.
    private var navlinkReachable = false

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
        SLog("ConnectionVC: viewDidLoad v\(version)")

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
                    self?.navlinkReachable = reachable
                    self?.updateUI()
                }
            }
        }

        VPNManager.shared.load { [weak self] err in
            DispatchQueue.main.async {
                if let err {
                    self?.showAlert("VPN setup failed: \(err.localizedDescription)")
                }
                self?.updateUI()
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
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        stopPoll()
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

        let mainStack = UIStackView(arrangedSubviews: [imgState, lblStatus, ribbonUpdate, keyStack, haveKeyStack, credentialStack, actionStack])
        mainStack.axis = .vertical
        mainStack.spacing = 20
        mainStack.alignment = .center
        mainStack.setCustomSpacing(12, after: imgState)
        mainStack.setCustomSpacing(0, after: lblStatus)
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
        let logsAction = UIAction(
            title: "Share Logs",
            image: UIImage(systemName: "square.and.arrow.up")
        ) { [weak self] _ in
            self?.shareLogs()
        }

        navigationItem.rightBarButtonItem = UIBarButtonItem(
            image: UIImage(systemName: "ellipsis.circle"),
            menu: UIMenu(children: [logsAction]))
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
            showAlert("Enter your subscription key first")
            return
        }
        VPNManager.shared.saveKey(key)
        doConnect()
    }

    private func doConnect() {
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
            title: "Remove Key",
            message: "Remove your SNC key from this device?",
            preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: "Remove", style: .destructive) { [weak self] _ in
            SLog("user: key removed confirmed")
            VPNManager.shared.disconnect()
            VPNManager.shared.saveKey("")
            self?.txtKey.text = ""
            self?.loginScreen = .haveKeyPrompt
            self?.updateUI()
        })
        alert.addAction(UIAlertAction(title: "Cancel", style: .cancel))
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
        loginScreen = .credentialLogin
        updateUI()
    }

    @objc private func haveKeyYesTapped() {
        loginScreen = .keyEntry
        updateUI()
    }

    @objc private func haveKeyNoTapped() {
        loginScreen = navlinkReachable ? .credentialLogin : .keyEntry
        updateUI()
    }

    @objc private func switchToKeyEntryTapped() {
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
            lblCredentialError.text = "Please enter email and password"
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
            self.log.info("vpnStatusChanged: status=\(status.rawValue)")
            self.updateUI()
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

    // MARK: - State

    private func updateUI() {
        let neStatus      = VPNManager.shared.connectionStatus
        let tunnelState   = VPNManager.shared.tunnelState
        var hasKey        = !(VPNManager.shared.loadKey() ?? "").isEmpty

        let connected     = neStatus == .connected
        let connecting    = neStatus == .connecting || neStatus == .reasserting
        let disconnecting = neStatus == .disconnecting
        let keyDenied     = tunnelState == "key_denied"
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

        // Update-available ribbon — tap opens the App Store page (see UpdateChecker).
        if let update = UpdateChecker.shared.availableUpdate {
            ribbonUpdate.text = "Update available (v\(update.version)) — tap to update"
            ribbonUpdate.isHidden = false
        } else {
            ribbonUpdate.isHidden = true
        }

        // Status image
        let imageName: String
        switch true {
        case keyDenied:
            imageName = "snc_error"
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
            if keyDenied      { return "Key rejected — check your subscription" }
            if connected      { return "Connected" }
            if connecting    { return "Connecting…" }
            if disconnecting { return "Disconnecting…" }
            return hasKey ? "Tap Connect to start" : "Enter key to get started"
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
        cfg.title = busy ? "Disconnect" : "Connect"
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
                    if key.isEmpty { self?.showAlert("Failed to fetch key from URL") }
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
        guard !key.isEmpty else { showAlert("Empty key received"); return }
        txtKey.text = key
        VPNManager.shared.saveKey(key)
        updateUI()
        showAlert("Key saved — tap Connect")
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
            showAlert("Cannot access app container"); return
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
                DispatchQueue.main.async { self.showAlert("No logs found") }
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
                    self?.showAlert("No QR code found in image")
                }
                return
            }
            DispatchQueue.main.async {
                self?.handleScanned(msg)
            }
        }
    }
}
