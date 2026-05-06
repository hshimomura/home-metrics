import SwiftUI

@MainActor
final class AppState: ObservableObject {
    @Published var apiBaseURL = "http://localhost:8080"
    @Published var apiToken = ""
    @Published var devices: [Device] = []
    @Published var latest: [String: LatestValue] = [:]
    @Published var rules: [AlertRule] = []
    @Published var events: [NotificationEvent] = []
    @Published var errorMessage: String?

    func refresh() async {
        guard let url = URL(string: apiBaseURL) else {
            errorMessage = "Invalid API URL"
            return
        }
        let client = APIClient(baseURL: url, apiToken: apiToken)
        do {
            devices = try await client.devices()
            var latestValues: [String: LatestValue] = [:]
            for device in devices {
                latestValues[device.mac] = try? await client.latest(mac: device.mac)
            }
            latest = latestValues
            rules = try await client.alertRules()
            events = try await client.notificationEvents()
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

struct ContentView: View {
    @StateObject private var state = AppState()

    var body: some View {
        NavigationStack {
            List {
                Section("API") {
                    TextField("Base URL", text: $state.apiBaseURL)
                        .textInputAutocapitalization(.never)
                        .keyboardType(.URL)
                    SecureField("API token", text: $state.apiToken)
                    Button("Refresh") {
                        Task { await state.refresh() }
                    }
                }

                if let errorMessage = state.errorMessage {
                    Section("Error") {
                        Text(errorMessage)
                            .foregroundStyle(.red)
                    }
                }

                Section("Devices") {
                    ForEach(state.devices) { device in
                        let values = state.latest[device.mac]?.values ?? [:]
                        VStack(alignment: .leading, spacing: 4) {
                            Text(device.label)
                                .font(.headline)
                            Text(device.mac)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                            HStack {
                                metric("Temp", values["temperature_c"] ?? nil, "C")
                                metric("Humidity", values["humidity_percent"] ?? nil, "%")
                                metric("Battery", values["battery_percent"] ?? nil, "%")
                            }
                        }
                    }
                }

                Section("Alert Rules") {
                    ForEach(state.rules) { rule in
                        VStack(alignment: .leading) {
                            Text("\(rule.metric) \(rule.operator) \(rule.threshold, specifier: "%.1f")")
                            Text(rule.mac)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }

                Section("Notification Events") {
                    ForEach(state.events) { event in
                        VStack(alignment: .leading) {
                            Text("\(event.status): \(event.metric)")
                            Text("\(event.mac) \(event.value.map { String(format: "%.1f", $0) } ?? "-")")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            }
            .navigationTitle("BLE Sensors")
            .task { await state.refresh() }
        }
    }

    private func metric(_ name: String, _ value: Double?, _ unit: String) -> some View {
        VStack(alignment: .leading) {
            Text(name)
                .font(.caption)
                .foregroundStyle(.secondary)
            Text(value.map { String(format: "%.1f %@", $0, unit) } ?? "-")
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}
