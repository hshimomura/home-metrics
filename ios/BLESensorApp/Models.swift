import Foundation

struct Device: Decodable, Identifiable {
    var id: String { mac }
    let mac: String
    let label: String
    let deviceType: String?
    let location: String?
    let enabled: Bool

    enum CodingKeys: String, CodingKey {
        case mac
        case label
        case deviceType = "sensor_category"
        case location
        case enabled
    }
}

struct LatestValue: Decodable {
    let device: Device
    let ts: Date
    let values: [String: Double?]
}

struct AlertRule: Decodable, Identifiable {
    let id: Int
    let mac: String
    let metric: String
    let `operator`: String
    let threshold: Double
    let cooldownSeconds: Int
    let enabled: Bool

    enum CodingKeys: String, CodingKey {
        case id
        case mac
        case metric
        case `operator`
        case threshold
        case cooldownSeconds = "cooldown_seconds"
        case enabled
    }
}

struct NotificationEvent: Decodable, Identifiable {
    let id: Int
    let mac: String
    let metric: String
    let value: Double?
    let status: String
    let errorMessage: String?
    let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id
        case mac
        case metric
        case value
        case status
        case errorMessage = "error_message"
        case createdAt = "created_at"
    }
}

struct EnergyLatest: Decodable, Identifiable {
    var id: String { "\(source):\(deviceKey):\(metric)" }
    let ts: Date
    let source: String
    let deviceKey: String
    let label: String?
    let location: String?
    let metric: String
    let value: Double
    let unit: String?

    enum CodingKeys: String, CodingKey {
        case ts
        case source
        case deviceKey = "device_key"
        case label
        case location
        case metric
        case value
        case unit
    }
}
