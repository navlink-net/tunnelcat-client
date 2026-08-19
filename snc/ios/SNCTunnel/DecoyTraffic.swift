// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Foundation

/// Generates background HTTPS requests to camouflage VPN traffic.
///
/// Targets global CDNs (jsDelivr, Cloudflare, Google Fonts, …).
///
/// Inside NEPacketTunnelProvider, URLSession connections are NOT subject to the
/// VPN tunnel — they go directly through the physical interface. This gives us
/// the same effect as VpnService.protect() on Android without any special setup.
final class DecoyTraffic {
    private var timer: DispatchSourceTimer?
    private lazy var session: URLSession = {
        let cfg = URLSessionConfiguration.default
        cfg.timeoutIntervalForRequest = 10
        cfg.timeoutIntervalForResource = 15
        return URLSession(configuration: cfg)
    }()

    // Global CDNs — mirrors core/decoy.go CDN pool
    private let normalTargets: [URL] = [
        "https://cdn.jsdelivr.net/npm/bootstrap@5/dist/js/bootstrap.min.js",
        "https://cdnjs.cloudflare.com/ajax/libs/jquery/3.7.1/jquery.min.js",
        "https://fonts.googleapis.com/css2?family=Roboto&display=swap",
        "https://unpkg.com/react@18/umd/react.production.min.js",
        "https://cdn.jsdelivr.net/npm/lodash@4/lodash.min.js",
    ].compactMap(URL.init)

    private let userAgents = [
        "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
        "Mozilla/5.0 (iPhone; CPU iPhone OS 16_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/116.0.5845.103 Mobile/15E148 Safari/604.1",
        "Mozilla/5.0 (iPhone; CPU iPhone OS 17_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Mobile/15E148 Safari/604.1",
    ]

    init() {}

    func start() {
        let t = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .background))
        // Irregular pacing: 3–12 s base interval, same as Go DecoyManager.
        t.schedule(deadline: .now() + 3, repeating: .never)
        t.setEventHandler { [weak self] in self?.fireAndReschedule() }
        t.resume()
        timer = t
    }

    func stop() {
        timer?.cancel()
        timer = nil
    }

    private func fireAndReschedule() {
        fire()
        let interval = Double.random(in: 3...12)
        timer?.schedule(deadline: .now() + interval, repeating: .never)
        timer?.resume()
    }

    private func fire() {
        let targets = normalTargets
        guard !targets.isEmpty else { return }
        let url = targets.randomElement()!
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue(userAgents.randomElement()!, forHTTPHeaderField: "User-Agent")
        req.setValue("keep-alive", forHTTPHeaderField: "Connection")
        session.dataTask(with: req) { _, _, _ in }.resume()
    }
}
