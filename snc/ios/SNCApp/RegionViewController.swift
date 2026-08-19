// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import UIKit

/// Shows the automatically-detected home country and explains that CIDR bypass
/// routes for that country are excluded from the tunnel.
///
/// iOS does not support per-app VPN routing (unlike Android's VpnService split
/// tunnel), so bypass is country-level only.  The Go BypassManager fetches the
/// signed CIDR list from the exit node and applies it automatically.
final class RegionViewController: UIViewController {

    private let lblCountry = UILabel()
    private var pollTimer: Timer?

    override func viewDidLoad() {
        super.viewDidLoad()
        title = "Bypass"
        view.backgroundColor = UIColor(red: 0.02, green: 0.02, blue: 0.1, alpha: 1)
        setupLayout()
        updateCountry()
    }

    override func viewWillAppear(_ animated: Bool) {
        super.viewWillAppear(animated)
        updateCountry()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 10, repeats: true) { [weak self] _ in
            self?.updateCountry()
        }
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        pollTimer?.invalidate()
        pollTimer = nil
    }

    // MARK: - Layout

    private func setupLayout() {
        let iconView = UIImageView(image: UIImage(systemName: "map.fill"))
        iconView.contentMode = .scaleAspectFit
        iconView.tintColor = .systemBlue
        iconView.translatesAutoresizingMaskIntoConstraints = false

        let symCfg = UIImage.SymbolConfiguration(pointSize: 52, weight: .regular)
        iconView.preferredSymbolConfiguration = symCfg

        let lblHeading = UILabel()
        lblHeading.text = "Home-country bypass"
        lblHeading.font = .systemFont(ofSize: 18, weight: .semibold)
        lblHeading.textColor = UIColor(white: 1, alpha: 0.9)
        lblHeading.textAlignment = .center

        lblCountry.font = .systemFont(ofSize: 32, weight: .bold)
        lblCountry.textColor = .white
        lblCountry.textAlignment = .center
        lblCountry.text = "—"

        let lblBody = UILabel()
        lblBody.font = .systemFont(ofSize: 14)
        lblBody.textColor = UIColor(white: 1, alpha: 0.55)
        lblBody.textAlignment = .center
        lblBody.numberOfLines = 0
        lblBody.text = "Routes for your home country are automatically excluded from the tunnel, so local services work at full speed without going through the VPN."

        let lblNote = UILabel()
        lblNote.font = .systemFont(ofSize: 12)
        lblNote.textColor = UIColor(white: 1, alpha: 0.3)
        lblNote.textAlignment = .center
        lblNote.numberOfLines = 0
        lblNote.text = "Detected from timezone · Refreshed each session\niOS does not support per-app split tunnel"

        let stack = UIStackView(arrangedSubviews: [iconView, lblHeading, lblCountry, lblBody, lblNote])
        stack.axis = .vertical
        stack.alignment = .center
        stack.spacing = 18
        stack.setCustomSpacing(24, after: iconView)
        stack.setCustomSpacing(4, after: lblHeading)
        stack.setCustomSpacing(28, after: lblCountry)
        stack.translatesAutoresizingMaskIntoConstraints = false

        view.addSubview(stack)

        NSLayoutConstraint.activate([
            stack.centerXAnchor.constraint(equalTo: view.centerXAnchor),
            stack.centerYAnchor.constraint(equalTo: view.centerYAnchor),
            stack.leadingAnchor.constraint(greaterThanOrEqualTo: view.leadingAnchor, constant: 36),
            stack.trailingAnchor.constraint(lessThanOrEqualTo: view.trailingAnchor, constant: -36),
            lblBody.leadingAnchor.constraint(equalTo: view.leadingAnchor, constant: 36),
            lblBody.trailingAnchor.constraint(equalTo: view.trailingAnchor, constant: -36),
            lblNote.leadingAnchor.constraint(equalTo: view.leadingAnchor, constant: 36),
            lblNote.trailingAnchor.constraint(equalTo: view.trailingAnchor, constant: -36),
        ])
    }

    // MARK: - Country detection

    private func updateCountry() {
        guard let container = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.net.shortnerdcat") else { return }
        let path = container
            .appendingPathComponent("Library/Application Support/tunnel/country.txt")
        if let cc = try? String(contentsOf: path, encoding: .utf8)
            .trimmingCharacters(in: .whitespacesAndNewlines), !cc.isEmpty {
            lblCountry.text = countryFlag(cc) + " " + cc
        } else {
            lblCountry.text = "Detecting…"
        }
    }

    private func countryFlag(_ cc: String) -> String {
        cc.uppercased().unicodeScalars.compactMap {
            Unicode.Scalar(127397 + $0.value)
        }.map(String.init).joined()
    }
}
