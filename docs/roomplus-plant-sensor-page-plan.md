# RoomPlus Plant Sensor Page Plan

This note describes the current `home-metrics` and RoomPlus contract for plant
sensors. The goal is to classify devices by explicit metadata fields rather
than by labels or MAC addresses.

## Goal

RoomPlus should show plant sensors on a dedicated Plants page. Plant sensors
should be identified from `home-metrics` device metadata.

The first plant target is the Xiaomi Flower Care / MiFlora device collected
through Cisco Sensor Connect:

```text
mac: 5c:85:7e:14:73:7d
label: Blueberry1
ingest_source: cisco_sensor_connect
sensor_type_code: xiaomi_flower_care
sensor_category: plant
```

## Device Metadata

Use separate fields for separate meanings:

```text
ingest_source      where telemetry comes from
sensor_type_code   concrete sensor model/protocol decoder
sensor_category    product/use category for clients
```

Current values:

| Field | Example | Meaning |
| --- | --- | --- |
| `ingest_source` | `cisco_sensor_connect` | Telemetry is received through Cisco Sensor Connect (IoT Orchestrator). |
| `sensor_type_code` | `xiaomi_flower_care` | Decoder/model family for Xiaomi Flower Care / MiFlora. |
| `sensor_category` | `plant` | RoomPlus places this device on the Plants page. |

## Schema

`home-metrics` uses a normalized sensor type table and explicit metadata
columns on `devices`:

```sql
CREATE TABLE sensor_types (
    code text PRIMARY KEY,
    display_name text NOT NULL,
    category text NOT NULL,
    vendor text,
    model text,
    notes text
);

ALTER TABLE devices
    ADD COLUMN ingest_source text,
    ADD COLUMN sensor_type_code text REFERENCES sensor_types(code),
    ADD COLUMN sensor_category text;
```

Initial sensor type rows:

```sql
INSERT INTO sensor_types (code, display_name, category, vendor, model)
VALUES
  ('xiaomi_flower_care', 'Xiaomi Flower Care', 'plant', 'Xiaomi / HHCC', 'HHCCJCY01'),
  ('minew_s1', 'Minew S1', 'environment', 'Minew', 'S1'),
  ('env_ble', 'Environmental BLE Sensor', 'environment', NULL, NULL)
ON CONFLICT (code) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  category = EXCLUDED.category,
  vendor = EXCLUDED.vendor,
  model = EXCLUDED.model;
```

Backfill examples:

```sql
UPDATE devices
SET ingest_source = 'cisco_sensor_connect',
    sensor_type_code = 'xiaomi_flower_care',
    sensor_category = 'plant',
    updated_at = now()
WHERE mac = '5c:85:7e:14:73:7d';

UPDATE devices
SET ingest_source = 'cisco_sensor_connect',
    sensor_category = 'environment',
    updated_at = now()
WHERE ingest_source IS NULL;
```

`sensor_category` is a denormalized client-facing copy of
`sensor_types.category`. The collector/API should fill it from
`sensor_type_code` when it is omitted. A device-level `sensor_category` may be
used as an explicit override, but tests or migration checks should catch
unexpected mismatches between `devices.sensor_type_code`,
`sensor_types.category`, and `devices.sensor_category`.

Do not create a separate plant telemetry table. Plant readings remain normal
time-series telemetry keyed by MAC and timestamp in:

- `sensor_minute`
- `sensor_1hour`
- `sensor_12hour`
- `sensor_1day`

Plant-specific values remain nullable metric columns:

```text
soil_moisture_percent
conductivity_us_cm
```

## API Contract

RoomPlus consumes `/api/devices` and classifies devices by `sensor_category`.

Example `/api/devices` item:

```json
{
  "mac": "5c:85:7e:14:73:7d",
  "label": "Blueberry1",
  "location": "Blueberry1",
  "enabled": true,
  "ingest_source": "cisco_sensor_connect",
  "sensor_type_code": "xiaomi_flower_care",
  "sensor_type": {
    "code": "xiaomi_flower_care",
    "display_name": "Xiaomi Flower Care",
    "category": "plant",
    "vendor": "Xiaomi / HHCC",
    "model": "HHCCJCY01"
  },
  "sensor_category": "plant"
}
```

`sensor_category` is intentionally duplicated from `sensor_types.category` on
the device response. This keeps common client filtering simple and lets the API
hide join details.

Swift model shape:

```swift
struct DeviceSummary: Decodable, Identifiable {
    var id: String { mac }

    let mac: String
    let label: String
    let location: String?
    let enabled: Bool
    let ingestSource: String?
    let sensorTypeCode: String?
    let sensorType: SensorTypeSummary?
    let sensorCategory: String?

    var isPlantSensor: Bool {
        sensorCategory == "plant"
    }

    enum CodingKeys: String, CodingKey {
        case mac
        case label
        case location
        case enabled
        case ingestSource = "ingest_source"
        case sensorTypeCode = "sensor_type_code"
        case sensorType = "sensor_type"
        case sensorCategory = "sensor_category"
    }
}

struct SensorTypeSummary: Decodable {
    let code: String
    let displayName: String
    let category: String
    let vendor: String?
    let model: String?

    enum CodingKeys: String, CodingKey {
        case code
        case displayName = "display_name"
        case category
        case vendor
        case model
    }
}
```

The checked-in Grafana dashboard uses `sensor_category` for plant filters. Make
sure the target database has the current schema before importing or refreshing
that dashboard.

## Plant Metrics

RoomPlus includes plant metrics in its metric model:

```text
soil_moisture_percent
conductivity_us_cm
```

Display labels:

```text
soil_moisture_percent -> Soil moisture, %
conductivity_us_cm -> Conductivity, uS/cm
```

The API/storage name remains `conductivity_us_cm` because the physical value is
conductivity. Some plant apps describe this value as fertility, but RoomPlus
uses `Conductivity` or the Japanese label `導電率` to stay aligned with the API
and web UI.

Example `/api/devices/{mac}/latest` response for a plant sensor:

```json
{
  "device": {
    "mac": "5c:85:7e:14:73:7d",
    "label": "Blueberry1",
    "location": "Blueberry1",
    "enabled": true,
    "ingest_source": "cisco_sensor_connect",
    "sensor_type_code": "xiaomi_flower_care",
    "sensor_category": "plant"
  },
  "ts": "2026-06-06T12:34:00Z",
  "values": {
    "temperature_c": 23.4,
    "humidity_percent": null,
    "battery_percent": 100,
    "rssi_dbm": null,
    "pressure_hpa": null,
    "co2_ppm": null,
    "lux": 1200,
    "etvoc": null,
    "soil_moisture_percent": 28,
    "conductivity_us_cm": 208
  },
  "value_timestamps": {
    "temperature_c": "2026-06-06T12:34:00Z",
    "battery_percent": "2026-06-05T12:42:00Z",
    "lux": "2026-06-06T12:34:00Z",
    "soil_moisture_percent": "2026-06-06T12:34:00Z",
    "conductivity_us_cm": "2026-06-06T12:34:00Z"
  }
}
```

The `values` object includes all known sensor metric keys; unavailable metrics
are returned as `null`. `value_timestamps` contains only metrics that have a
non-null value and records when each metric was measured. The top-level `ts` is
the newest timestamp across the metric values and remains the device's latest
telemetry time.

For Xiaomi Flower Care sensors, `battery_percent` is optional. Passive
advertisements do not provide it, but `home-metrics` can poll the Flower Care
GATT battery characteristic at a low frequency when `gatt_battery` is configured
for that device. `/api/devices/{mac}/latest` exposes the latest non-null value
per metric, so RoomPlus can display battery even when it is older than the
latest soil, temperature, or lux advertisement. RoomPlus can use
`value_timestamps.battery_percent` to de-emphasize a stale battery value when
needed. `/api/devices/{mac}/series` remains based on the original measurement
timestamps and does not copy battery values forward.

## Page Split

RoomPlus should split devices in the client:

```text
Sensors page:
  devices where enabled == true and sensor_category != "plant"

Plants page:
  devices where enabled == true and sensor_category == "plant"
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

RoomPlus classifies a plant sensor through `sensor_category = "plant"`.

Metric-based classification can be useful only for diagnostics:

```text
latest contains soil_moisture_percent or conductivity_us_cm -> likely plant
```

This diagnostic rule should not decide page placement because it requires
telemetry before classification and can hide a plant device when recent plant
values are missing.

If `/api/devices/{mac}/latest` returns `404`, RoomPlus should keep the device in
the Plants page and render it as `No recent reading` or equivalent. A missing
latest reading means no current telemetry is available; it does not mean the
device should be removed from the list.

## Future Plant Metadata

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

## Implementation State

1. Added `sensor_types`, `devices.ingest_source`,
   `devices.sensor_type_code`, and `devices.sensor_category`.
2. Updated the Cisco Sensor Connect collector to read and upsert
   `ingest_source`, `sensor_type_code`, and `sensor_category`.
3. Updated `/api/devices` and `/api/devices/{mac}/latest` device payloads to
   expose the new fields.
4. Backfilled `5c:85:7e:14:73:7d` with
   `sensor_type_code = 'xiaomi_flower_care'` and
   `sensor_category = 'plant'`.
5. Updated RoomPlus models to decode `ingest_source`, `sensor_type_code`,
   `sensor_type`, and `sensor_category`.
6. Classified plant sensors only with `sensor_category == "plant"`.
7. Added `soil_moisture_percent` and `conductivity_us_cm` to the RoomPlus metric
   model.
8. Treated `enabled == false` devices as hidden from the default Sensors and
   Plants pages.
9. Handled latest `404` as `No recent reading`, not as a missing device.
10. Added a Plants tab/page and filtered plant devices into it.
11. Filtered plant devices out of the normal Sensors page.
12. Updated RoomPlus API contract checks and mock data.
13. Added UI tests or previews for at least one plant device.
14. Moved Grafana plant dashboard filters to `sensor_category`.
15. Removed legacy classification metadata from `home-metrics`.

## Non-Goals

- Do not create a separate plant database.
- Do not create a separate plant time-series table unless plant readings need a
  fundamentally different retention, rollup, or ownership model.
- Do not classify plant sensors by label text, MAC address, or metric presence
  in RoomPlus.
