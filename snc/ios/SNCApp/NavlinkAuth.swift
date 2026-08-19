// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import Foundation

/// Direct (non-tunneled) HTTPS client for navlink.net's existing account-login
/// and free-key-issuance endpoints — the "no key yet" login path.
///
/// Uses `URLSession(configuration: .default)`, deliberately independent of
/// anything tunnel-related: a device with no key yet has no VPN tunnel
/// running (VPNManager only connects once a key exists), so this session is
/// structurally guaranteed to go straight over the device's normal network
/// stack. The default configuration's `HTTPCookieStorage` persists the
/// session cookie set by `login` for the subsequent `freeKey` call within
/// the same process. Never wire this through a tunnel-aware session, even if
/// one becomes reachable later in the process.
enum NavlinkAuth {

    private static let baseURL = URL(string: "https://navlink.net")!

    private static let session: URLSession = {
        let config = URLSessionConfiguration.default
        config.timeoutIntervalForRequest = 10
        return URLSession(configuration: config)
    }()

    struct NavlinkError: Error, LocalizedError {
        let statusCode: Int
        let message: String
        var errorDescription: String? { message }
    }

    /// Reports whether navlink.net is reachable directly, right now. Any HTTP
    /// response (regardless of status code) counts as reachable — only a
    /// network/TLS-level failure or timeout counts as unreachable.
    static func probe(completion: @escaping (Bool) -> Void) {
        var req = URLRequest(url: baseURL)
        req.httpMethod = "GET"
        req.timeoutInterval = 4
        let probeSession: URLSession = {
            let config = URLSessionConfiguration.default
            config.timeoutIntervalForRequest = 4
            return URLSession(configuration: config)
        }()
        probeSession.dataTask(with: req) { _, response, error in
            completion(error == nil && response != nil)
        }.resume()
    }

    /// Authenticates an existing navlink.net account. On success the session
    /// cookie is retained by `session`'s cookie storage for a subsequent
    /// `freeKey` call.
    static func login(email: String, password: String, completion: @escaping (Error?) -> Void) {
        var req = URLRequest(url: baseURL.appendingPathComponent("api/account/login"))
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: ["email": email, "password": password])

        session.dataTask(with: req) { data, response, error in
            if let error {
                completion(error)
                return
            }
            let status = (response as? HTTPURLResponse)?.statusCode ?? 0
            if status != 200 {
                completion(NavlinkError(statusCode: status, message: Self.errorMessage(from: data)))
                return
            }
            completion(nil)
        }.resume()
    }

    struct IssuedKey {
        let key: String
        let keyID: String
        let clientID: String
    }

    /// Issues a fresh key for the account authenticated by the preceding
    /// `login` call, using the same session (and thus the same cookie).
    static func freeKey(completion: @escaping (Result<IssuedKey, Error>) -> Void) {
        var req = URLRequest(url: baseURL.appendingPathComponent("api/key/free"))
        req.httpMethod = "POST"

        session.dataTask(with: req) { data, response, error in
            if let error {
                completion(.failure(error))
                return
            }
            let status = (response as? HTTPURLResponse)?.statusCode ?? 0
            guard status == 200, let data else {
                completion(.failure(NavlinkError(statusCode: status, message: Self.errorMessage(from: data))))
                return
            }
            guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let key = json["key"] as? String else {
                completion(.failure(NavlinkError(statusCode: status, message: "malformed response")))
                return
            }
            let issued = IssuedKey(
                key: key,
                keyID: json["key_id"] as? String ?? "",
                clientID: json["client_id"] as? String ?? "")
            completion(.success(issued))
        }.resume()
    }

    private static func errorMessage(from data: Data?) -> String {
        guard let data, !data.isEmpty else { return "request failed" }
        if let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           let msg = json["error"] as? String {
            return msg
        }
        return String(data: data, encoding: .utf8) ?? "request failed"
    }
}
