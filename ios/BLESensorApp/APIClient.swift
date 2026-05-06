import Foundation

final class APIClient {
    var baseURL: URL
    var apiToken: String

    init(baseURL: URL = URL(string: "http://localhost:8080")!, apiToken: String = "") {
        self.baseURL = baseURL
        self.apiToken = apiToken
    }

    func devices() async throws -> [Device] {
        try await get("/api/devices")
    }

    func latest(mac: String) async throws -> LatestValue {
        try await get("/api/devices/\(mac)/latest")
    }

    func alertRules() async throws -> [AlertRule] {
        try await get("/api/alert-rules")
    }

    func notificationEvents() async throws -> [NotificationEvent] {
        try await get("/api/notification-events?limit=20")
    }

    func energyLatest() async throws -> [EnergyLatest] {
        try await get("/api/energy/latest")
    }

    func registerDeviceToken(_ token: String, bundleID: String, environment: String, deviceName: String?) async throws {
        let body: [String: Any?] = [
            "apns_device_token": token,
            "app_bundle_id": bundleID,
            "apns_environment": environment,
            "device_name": deviceName,
        ]
        _ = try await request("/api/ios/devices", method: "POST", body: body)
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        let data = try await request(path, method: "GET", body: nil)
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom(Self.decodeDate)
        return try decoder.decode(T.self, from: data)
    }

    private static func decodeDate(_ decoder: Decoder) throws -> Date {
        let container = try decoder.singleValueContainer()
        let value = try container.decode(String.self)
        let formats: [ISO8601DateFormatter.Options] = [
            [.withInternetDateTime, .withFractionalSeconds],
            [.withInternetDateTime],
        ]
        for options in formats {
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = options
            if let date = formatter.date(from: value) {
                return date
            }
        }
        throw DecodingError.dataCorruptedError(in: container, debugDescription: "Invalid date: \(value)")
    }

    private func request(_ path: String, method: String, body: [String: Any?]?) async throws -> Data {
        guard let url = URL(string: path, relativeTo: baseURL) else {
            throw URLError(.badURL)
        }
        var request = URLRequest(url: url)
        request.httpMethod = method
        if !apiToken.isEmpty {
            request.setValue("Bearer \(apiToken)", forHTTPHeaderField: "Authorization")
        }
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let compactBody = body.compactMapValues { $0 }
            request.httpBody = try JSONSerialization.data(withJSONObject: compactBody)
        }
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let httpResponse = response as? HTTPURLResponse, (200..<300).contains(httpResponse.statusCode) else {
            throw URLError(.badServerResponse)
        }
        return data
    }
}
