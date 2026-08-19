// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Network

/// Watches for physical network path changes and calls the callback when the
/// path becomes satisfied (Wi-Fi ↔ cellular switch, reconnect after sleep).
final class NetworkMonitor {
    private var monitor: NWPathMonitor?
    private var onChange: (() -> Void)?
    private var lastPath: NWPath?

    func start(onChange: @escaping () -> Void) {
        self.onChange = onChange
        let m = NWPathMonitor()
        monitor = m
        m.pathUpdateHandler = { [weak self] path in
            guard let self else { return }
            // Fire only on meaningful changes: interface type switch or
            // transition from unsatisfied → satisfied (reconnect after outage).
            if path.status == .satisfied, path.status != self.lastPath?.status {
                self.onChange?()
            }
            self.lastPath = path
        }
        m.start(queue: DispatchQueue(label: "net.shortnerdcat.tunnel.netmon"))
    }

    func stop() {
        monitor?.cancel()
        monitor = nil
    }
}
