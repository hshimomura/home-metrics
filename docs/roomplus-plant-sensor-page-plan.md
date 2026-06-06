# RoomPlus Plant Sensor Page Plan

This note describes how RoomPlus should separate plant sensors from normal room
sensors while continuing to use the existing `home-metrics` sensor API.

## Goal

RoomPlus should show plant sensors on a dedicated page. Plant sensors should be
recognized by data from `home-metrics`, not by hard-coded MAC addresses or
labels.

The first plant target is the Xiaomi Flower Care / MiFlora device collected
through Cisco Sensor Connect:

```text
mac: 5c:85:7e:14:73:7d
label: blue berry 1
sensor_category: Cisco Sensor Connect (IoT Orchestrator)
```

## Current home-metrics Signals

`home-metrics` already has the data needed to identify and display plant
sensors:

- `devices.sensor_category`
- `soil_moisture_percent`
- `conductivity_us_cm`

Plant readings are stored in the same sensor time-series tables as the existing
environmental readings:

- `sensor_minute`
- `sensor_1hour`
- `sensor_12hour`
- `sensor_1day`

This is intentional. Plant readings are still time-series telemetry keyed by
device MAC and timestamp. Splitting plant readings into a separate database or
parallel time-series table would duplicate the API, rollup, backup, and client
logic without solving the page separation problem.

## `sensor_category` Should Not Be Reduced to `plant`

Using `devices.sensor_category = 'plant'` would work as a very small classifier, but
it would overload `sensor_category` with a different meaning.

The current value:

```text
Cisco Sensor Connect (IoT Orchestrator)
```

preserves two useful pieces of information:

- the transport/integration family: Cisco Sensor Connect
- the user-facing sensor class: Plant

If RoomPlus needs a short stable value, prefer adding a separate category field
or deriving one in the app/API:

```text
sensor_category: Cisco Sensor Connect (IoT Orchestrator)
category: plant
```

For the first RoomPlus implementation, avoid a DB migration and derive the
category from `sensor_category`:

```text
sensor_category == "Cisco Sensor Connect (IoT Orchestrator)" -> category plant
otherwise -> category environment
```

If more classes are added later, such as `energy`, `weather`, or `occupancy`,
then adding an explicit `devices.device_category` column or API-only `category`
field becomes reasonable.

## Recommended RoomPlus Contract

RoomPlus should consume `/api/devices` and decode `sensor_category`.

Example `/api/devices` item:

```json
{
  "mac": "5c:85:7e:14:73:7d",
  "label": "blue berry 1",
  "sensor_category": "Cisco Sensor Connect (IoT Orchestrator)",
  "enabled": true
}
```

Suggested Swift model shape:

```swift
struct DeviceSummary: Decodable, Identifiable {
    var id: String { mac }

    let mac: String
    let label: String
    let deviceType: String?
    let location: String?
    let enabled: Bool

    var isPlantSensor: Bool {
        deviceType == "Cisco Sensor Connect (IoT Orchestrator)"
    }

    enum CodingKeys: String, CodingKey {
        case mac
        case label
        case deviceType = "sensor_category"
        case location
        case enabled
    }
}
```

RoomPlus should add plant metrics to its metric model:

```text
soil_moisture_percent
conductivity_us_cm
```

Suggested display labels:

```text
soil_moisture_percent -> Soil moisture, %
conductivity_us_cm -> Conductivity, uS/cm
```

The API/storage name remains `conductivity_us_cm` because the physical value is
conductivity. Some plant apps describe this value as fertility, but RoomPlus
should prefer `Conductivity` or the Japanese label `導電率` to stay aligned with
the API and web UI.

Example `/api/devices/{mac}/latest` response for a plant sensor:

```json
{
  "device": {
    "mac": "5c:85:7e:14:73:7d",
    "label": "blue berry 1",
    "sensor_category": "Cisco Sensor Connect (IoT Orchestrator)",
    "enabled": true
  },
  "ts": "2026-06-06T12:34:00Z",
  "values": {
    "temperature_c": 23.4,
    "humidity_percent": null,
    "battery_percent": null,
    "rssi_dbm": null,
    "pressure_hpa": null,
    "co2_ppm": null,
    "lux": 1200,
    "etvoc": null,
    "soil_moisture_percent": 28,
    "conductivity_us_cm": 208
  }
}
```

`location` is omitted when empty because the API uses `omitempty`. The Cisco
Sensor Connect collector may default `location` to the device label when no
explicit location is configured, so a Flower Care device may also return
`"location": "blue berry 1"`. RoomPlus should decode it as optional and avoid
using it for plant classification. The `values` object includes all known
sensor metric keys; unavailable metrics are returned as `null`.

For Xiaomi Flower Care sensors, `battery_percent` is expected to be `null` in
the current implementation. The sensor exposes battery/firmware through a
connected GATT read, but `home-metrics` intentionally uses only passive
advertisement telemetry for Flower Care to avoid extra sensor battery drain and
AP BLE connection slot usage.

## Page Split

RoomPlus should split devices in the client:

```text
Sensors page:
  devices where enabled == true and isPlantSensor == false

Plants page:
  devices where enabled == true and isPlantSensor == true
```

Disabled devices should not appear in the normal page lists by default. If
RoomPlus adds a settings or diagnostics page later, disabled devices can be
shown there separately.

The Plants page should make plant-specific values primary:

1. `soil_moisture_percent`
2. `conductivity_us_cm`
3. `lux`
4. `temperature_c`
5. `battery_percent`, only when present

Normal room sensors should continue to prioritize:

1. `temperature_c`
2. `humidity_percent`
3. `co2_ppm`
4. `lux`
5. `battery_percent`

Do not mix `soil_moisture_percent` into `humidity_percent`. They are both
percentages, but they describe different physical measurements.

## Fallback Classification

Metric-based classification can be used only as a fallback:

```text
latest contains soil_moisture_percent or conductivity_us_cm -> plant-like
```

This should not be the primary rule because it requires latest telemetry before
classification and can hide a plant device when recent plant values are missing.
`sensor_category` should remain the primary signal.

If `/api/devices/{mac}/latest` returns `404`, RoomPlus should keep the device in
the Plants page and render it as `No recent reading` or equivalent. A missing
latest reading means no current telemetry is available; it does not mean the
device should be removed from the list.

## Future Metadata

If plant-specific management becomes useful, add metadata rather than splitting
the telemetry database.

Possible future table:

```sql
CREATE TABLE plant_devices (
    mac text PRIMARY KEY REFERENCES devices(mac),
    plant_name text NOT NULL,
    species text,
    pot_location text,
    soil_moisture_warning_percent double precision,
    conductivity_target_min_us_cm double precision,
    conductivity_target_max_us_cm double precision,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

This keeps telemetry in the existing sensor tables and adds only plant-specific
metadata.

## Implementation Steps

1. Verify the DB row for `5c:85:7e:14:73:7d` has
   `sensor_category = 'Cisco Sensor Connect (IoT Orchestrator)'`.
2. Update RoomPlus device API models to decode `sensor_category`.
3. Add `isPlantSensor` or equivalent derived classification.
4. Add `soil_moisture_percent` and `conductivity_us_cm` to the RoomPlus metric
   model.
5. Treat `enabled == false` devices as hidden from the default Sensors and
   Plants pages.
6. Handle latest `404` as `No recent reading`, not as a missing device.
7. Add a Plants tab/page and filter plant devices into it.
8. Filter plant devices out of the normal Sensors page.
9. Update RoomPlus API contract checks and mock data.
10. Add UI tests or previews for at least one plant device.

## Non-Goals

- Do not create a separate plant database for the first implementation.
- Do not create a separate plant time-series table unless plant readings need a
  fundamentally different retention, rollup, or ownership model.
- Do not classify plant sensors by label text or MAC address in RoomPlus.
