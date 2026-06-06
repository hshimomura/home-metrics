# Xiaomi Flower Care Design for Cisco Sensor Connect

This note collects design and implementation details for supporting Xiaomi
Flower Care Plant Sensor devices, also known as MiFlora or `HHCCJCY01`, with
Cisco Sensor Connect (IoT Orchestrator).

The target is to determine whether the sensor can be handled through the Cisco
AP infrastructure and whether `home-metrics` should support it as a plant sensor
source.

## Expected Device Behavior

The sensor is a BLE plant monitor. Public integrations and reverse-engineering
notes describe these measurements:

- air temperature, degrees Celsius
- illuminance, lux
- soil moisture, percent
- soil conductivity/fertility, microSiemens per centimeter
- battery level and firmware version, through a connected GATT read

The important implementation detail is that Flower Care has both passive
advertisement data and connected GATT characteristics. Home Assistant documents
that `HHCCJCY01` can be discovered from BLE broadcasts, but battery level may
require connecting to the device and reading characteristics. It also notes that
old firmware may not send the expected beacons and should be updated with the
Flower Care app. The lowest confirmed working firmware in that documentation is
`3.2.1`.

For Cisco Sensor Connect and `home-metrics`, the preferred path is passive
advertisement collection. `home-metrics` should build historical data from the
beacons it receives and stores over time, rather than reading the sensor's
on-device history service.

This means there are two practical paths:

- connectionless advertisement subscription for temperature, light, moisture,
  and conductivity if the AP receives the expected `FE95` beacon payloads
- connected GATT read for battery and for deterministic real-time reads if
  advertisement data is incomplete or encrypted

History GATT reads are out of scope for the first implementation and should not
be needed when the beacon stream is received reliably.

## Cisco Sensor Connect Fit

Cisco Sensor Connect for IoT Services exposes APIs to onboard BLE devices,
control them, and receive telemetry through MQTT. Cisco's API list includes BLE
connect, disconnect, write, read, discovery, data app registration, topic
registration, subscribe, and unsubscribe operations.

Before using IoT Orchestrator, do a direct BLE scan from `pve2`. This provides a
known-good local baseline and avoids mixing three unknowns at once: Flower Care
firmware behavior, Cisco AP BLE reception, and MQTT/protobuf decoding.

The `pve2` host has been prepared for this:

- `bluez`
- `python3-dbus`
- `python3-gi`
- Bluetooth service enabled and active
- controller `hci0`, address `08:9D:F4:38:BD:4F`

The existing repository scanner works there:

```sh
scp tools/scan_bluez.py pve2:/tmp/home-metrics-scan-bluez.py
ssh pve2 'python3 /tmp/home-metrics-scan-bluez.py --seconds 60 --all --targets /nonexistent --jsonl /tmp/home-metrics-bluez-scan.jsonl'
```

The scan output should be checked for `service_data`. A useful quick summary is:

```sh
ssh pve2 "python3 - <<'PY'
import json
path = '/tmp/home-metrics-bluez-scan.jsonl'
records = service = manufacturer = 0
with open(path, encoding='utf-8') as f:
    for line in f:
        records += 1
        row = json.loads(line)
        service += bool(row.get('service_data'))
        manufacturer += bool(row.get('manufacturer_data'))
print({'records': records, 'with_service_data': service, 'with_manufacturer_data': manufacturer})
PY"
```

For Flower Care, look for:

- name or alias similar to `Flower care`
- service data UUID `0000fe95-0000-1000-8000-00805f9b34fb`
- whether payloads appear plaintext or encrypted
- whether the MAC address is stable or randomized

The first test should stay close to the current Minew S1 flow:

1. Add the Flower Care MAC address to the IoT Orchestrator allow-list policy.
2. SCIM onboard the BLE device with `random=false`.
3. Confirm the device appears in the IoT Orchestrator BLE inventory.
4. Subscribe the device to the existing advertisement topic.
5. Capture MQTT advertisement messages and inspect whether service data includes
   Xiaomi `FE95` data.

This path has been validated with a real Flower Care device:

- MAC: `5c:85:7e:14:73:7d`
- Label: `blue berry 1`
- SCIM device ID: `48c71db0-ce81-43c2-849f-5da7fef23ec4`
- SCIM application IDs: `onboard`, `control`, and `data`
- `isRandom`: `false`
- MQTT topic: `ioslab/home-metrics/ble/advertisements/v1`

The device was not reachable from the `pve2` local BLE scanner after being
placed in soil, but Cisco AP reception through Sensor Connect did receive the
advertisements. This confirms that the Cisco AP infrastructure is the relevant
ingest path for this placement.

If advertisements are not sufficient, use the connected GATT path:

1. Discover services for the onboarded device.
2. Connect to service `00001204-0000-1000-8000-00805f9b34fb`.
3. Write command `a0 1f` to characteristic
   `00001a00-0000-1000-8000-00805f9b34fb`.
4. Read real-time values from characteristic
   `00001a01-0000-1000-8000-00805f9b34fb`.
5. Read battery/firmware from characteristic
   `00001a02-0000-1000-8000-00805f9b34fb`.
6. Disconnect after read to minimize battery impact and AP connection slot use.

## BLE UUIDs and GATT Characteristics

Known UUIDs from public MiFlora/Flower Care implementations:

| Purpose | UUID |
| --- | --- |
| discovery / Xiaomi root service | `0000fe95-0000-1000-8000-00805f9b34fb` |
| real-time data service | `00001204-0000-1000-8000-00805f9b34fb` |
| mode / command characteristic | `00001a00-0000-1000-8000-00805f9b34fb` |
| real-time sensor data characteristic | `00001a01-0000-1000-8000-00805f9b34fb` |
| battery and firmware characteristic | `00001a02-0000-1000-8000-00805f9b34fb` |
| history data service, not used initially | `00001206-0000-1000-8000-00805f9b34fb` |
| history command characteristic, not used initially | `00001a10-0000-1000-8000-00805f9b34fb` |
| history data characteristic, not used initially | `00001a11-0000-1000-8000-00805f9b34fb` |
| device epoch characteristic, not used initially | `00001a12-0000-1000-8000-00805f9b34fb` |

History reads are not required when `home-metrics` continuously stores received
beacon data.

## Real-Time GATT Decode

Before reading real-time values, write `a0 1f` to characteristic `1a00`.
Then read 16 bytes from characteristic `1a01`.

Example real-time payload:

```text
0e 01 00 48 02 00 00 28 d0 00 02 3c 00 fb 34 9b
```

Decode:

| Bytes | Type | Meaning | Example |
| --- | --- | --- | --- |
| `00-01` | int16 little-endian, divide by 10 | temperature C | `0x010e` = `270` -> `27.0 C` |
| `02` | unknown | usually `00` | `00` |
| `03-06` | uint32 little-endian | illuminance lux | `0x00000248` = `584 lux` |
| `07` | uint8 | soil moisture percent | `0x28` = `40%` |
| `08-09` | uint16 little-endian | conductivity | `0x00d0` = `208 uS/cm` |
| `10-15` | unknown/trailing bytes | ignore initially | `02 3c 00 fb 34 9b` |

Battery/firmware characteristic `1a02` example:

```text
5a 2b 33 2e 32 2e 32
```

Decode:

- byte `00`: battery percent, `0x5a` = `90%`
- byte `01`: delimiter/unknown, commonly `0x2b`
- bytes `02-06`: firmware ASCII, `33 2e 32 2e 32` = `3.2.2`

## Advertisement Decode Work

The current `hm-cisco-iot-orchestrator-collector` decoder does not yet parse
Xiaomi `FE95` / MiFlora payloads. It currently extracts service data for known
Minew / Env formats and decodes temperature, humidity, battery, RSSI, pressure,
CO2, lux, and eTVOC.

The observed Sensor Connect MQTT message carries the full BLE advertisement in
the protobuf `DataSubscription.data` bytes. The `FE95` service data is embedded
inside a standard advertisement data structure:

```text
020106030295fe131695fe71209800977d73147e855c0d0810010b
```

The service-data portion is:

```text
71209800977d73147e855c0d0810010b
```

The embedded MAC bytes `7d73147e855c` are the reverse of
`5c:85:7e:14:73:7d`.

Observed object IDs and values:

| Object ID | Payload example | Meaning | Decoded value | Verified against app |
| --- | --- | --- | --- | --- |
| `0x1004` | `0d0410020601` | temperature | `26.2 C` | same value family |
| `0x1007` | `0d071003be0700` | illuminance | `1982 lux` | same value family |
| `0x1008` | `0d0810010b` | soil moisture | `11%` | yes |
| `0x1009` | `0d0910024100` | fertility / conductivity | `65 uS/cm` | yes |

The mobile Flower Care application showed fertility `65` and moisture `11`
while Sensor Connect delivered `0x1009 = 65` and `0x1008 = 11`. Therefore
`0x1008` should be treated as soil moisture and `0x1009` should be treated as
fertility/conductivity.

Expected decoder work:

- include service data UUID `0xfe95` in advertisement extraction
- identify whether the Flower Care advertisement is plaintext MiBeacon, older
  MiFlora service data, or encrypted MiBeacon
- decode temperature, lux, soil moisture, and conductivity from advertisements
  when present
- store decoded beacon values in `home-metrics` and let the database provide
  history
- keep connected GATT real-time reads as a fallback path when advertisement data
  is incomplete or encrypted
- keep connected GATT battery/firmware reads as optional low-frequency polling

Do not map soil moisture to `humidity_percent`. Both values are percentages,
but `humidity_percent` means air relative humidity in the current data model,
while Flower Care `0x1008` means soil moisture. Mixing them would make RoomPlus
and web UI behavior ambiguous once multiple plant sensors are added.

## Proposed home-metrics Data Model

Current `sensor_minute` has no columns for plant-specific values. Add these
nullable columns to `sensor_minute` and all rollup tables:

```text
soil_moisture_percent double precision
conductivity_us_cm double precision
```

The DB/API metric name is `conductivity_us_cm`. The Flower Care mobile
application exposes this value as fertility, but the underlying physical value
is conductivity measured in `uS/cm`. Use `Fertility` as the user-facing label
and `conductivity_us_cm` as the storage/API field.

Use the existing `sensor_minute` table rather than creating a separate plant
table for the first implementation. The reasons are:

- the data is still one-minute sensor telemetry keyed by time and device MAC
- the existing API and UI already handle sparse metric columns
- temperature and lux are shared with existing sensors
- several Flower Care sensors can be added without adding new tables
- rollups can use the same median/average refresh strategy as other metrics

The tradeoff is that `sensor_minute` becomes a general environmental and plant
telemetry table. This is acceptable because the metrics remain nullable and
explicitly named. A separate plant schema can still be introduced later if
plant-specific metadata, calibration, or plant lifecycle records become
important.

Do not store `0x1008` in `humidity_percent`. Store it only in
`soil_moisture_percent`.

## Proposed Implementation Plan

Implement support in small, reviewable steps:

1. Add schema support.
   - Add a numbered migration for `soil_moisture_percent` and
     `conductivity_us_cm`.
   - Update `db/schema.sql` for fresh installs.
   - Add the same columns to `sensor_1hour`, `sensor_12hour`, and
     `sensor_1day`. This is mandatory because `/api/devices/{mac}/series`
     serves longer ranges from rollup tables.
   - Update `hm-db-maint` rollup refresh logic for the two new metrics.
   - Update `hm-db-check` so operational checks include the plant metrics.

2. Extend internal reading and aggregate types.
   - Add `SoilMoisturePercent` and `ConductivityUSCM` to the Cisco Sensor
     Connect collector reading and aggregate structs.
   - Include both values in minute median aggregation.
   - Upsert both values into `sensor_minute` using the same
     `COALESCE(EXCLUDED.value, sensor_minute.value)` pattern as existing
     sparse advertisement fields.
   - Extend `targetDevice` usage so `sensor_category` from the sensors file is
     passed to `upsertDevice`. Flower Care targets should use
     `Cisco Sensor Connect (IoT Orchestrator)` instead of the generic
     `Cisco Sensor Connect (IoT Orchestrator)` device type.

3. Decode Xiaomi `FE95` advertisement data.
   - Update `serviceDataFromAdvertisement` to extract UUID `0xfe95`.
   - Add a Xiaomi decoder for observed MiBeacon object IDs.
   - Map `0x1004` to `temperature_c`.
   - Map `0x1007` to `lux`.
   - Map `0x1008` to `soil_moisture_percent`.
   - Map `0x1009` to `conductivity_us_cm`.
   - Preserve existing Minew and Env decoder behavior.

4. Add tests.
   - Add decoder fixtures for the real Sensor Connect MQTT advertisement
     examples captured from `5c:85:7e:14:73:7d`.
   - Verify that `0x1008` does not populate `humidity_percent`.
   - Verify sparse message merging across separate advertisements in the same
     minute window.
   - Verify schema and API behavior where plant metrics are absent for normal
     room sensors.

5. Expose the new metrics through API and UI.
   - Add `soil_moisture_percent` and `conductivity_us_cm` to latest and series
     queries.
   - Update the latest query `SELECT`, non-null `OR` condition, and response
     values map so rows containing only plant metrics can become the latest
     reading.
   - Update `docs/openapi.yaml` and `docs/api.md`.
   - Update the web UI metric list. Suggested labels:
     `Soil moisture` with unit `%`, and `Fertility` with unit `uS/cm`.
   - Keep `humidity_percent` displayed as air humidity.

6. Register Flower Care devices as configured targets.
   - Add each Flower Care MAC to the sensor configuration with a plant-oriented
     label.
   - Keep `random=false` for Flower Care public MAC addresses.
   - Use SCIM onboarding app `onboard`, control app `control`, and data app
     `data`.
   - Register the advertisement topic after onboarding.

The first configured Flower Care target is:

```json
{
  "mac": "5C:85:7E:14:73:7D",
  "label": "blue berry 1",
  "sensor_category": "Cisco Sensor Connect (IoT Orchestrator)"
}
```

## Device Type and User-Facing Semantics

Use a plant-specific label for Flower Care devices so users do not confuse soil
moisture with room humidity. Suggested user-facing device type:

```text
Cisco Sensor Connect (IoT Orchestrator)
```

The internal ingest source should stay `cisco_iot_orchestrator` because it
identifies the transport and deployment path, not the sensor category.

RoomPlus and the web UI should treat `humidity_percent` and
`soil_moisture_percent` as distinct metrics. If a compact display needs only
one moisture-like value, choose by metric availability and device type rather
than by writing soil moisture into the air humidity column.

## Arrival Checklist

When the device arrives:

1. Insert battery and confirm it advertises as `Flower care` or similar.
2. Record MAC address and firmware version.
3. If firmware is older than `3.2.1`, update using the official Flower Care app.
4. Put the device near a target AP and confirm it appears in IoT Orchestrator AP
   inventory or live logs.
5. Add MAC prefix/device MAC to the IoT Orchestrator allow-list policy.
6. SCIM onboard with `random=false`.
7. Subscribe to the advertisement topic.
8. Capture MQTT payloads for at least 10 minutes.
9. Check whether `FE95` service data is present and whether data appears
   encrypted.
10. If battery is needed, test connect/write/read/disconnect using the GATT
    characteristics above.
11. Store `0x1008` as `soil_moisture_percent`.
12. Store `0x1009` as `conductivity_us_cm`.
13. Implement decoder and tests using captured payloads.

## Implementation Notes

Initial code changes are likely to touch:

- `cmd/hm-cisco-iot-orchestrator-collector/main.go`
- `cmd/hm-cisco-iot-orchestrator-collector/main_test.go`
- `db/schema.sql`
- a new numbered migration for plant metric columns
- `cmd/hm-db-maint/main.go`
- `cmd/hm-api-server/sensors.go`
- `cmd/hm-api-server/main_test.go`
- `cmd/hm-db-check/main.go`
- `docs/openapi.yaml` and `docs/api.md` if new API fields are exposed
- `web/index.html` if plant values should be shown in the UI

Potential decoder test fixtures:

- raw MQTT advertisements carrying `FE95` service data:
  - soil moisture: `020106030295fe131695fe71209800977d73147e855c0d0810010b`
  - fertility: `020106030295fe141695fe71209800987d73147e855c0d0910024100`
  - temperature: `020106030295fe141695fe71209800997d73147e855c0d0410020601`
  - lux: `020106030295fe151695fe712098009a7d73147e855c0d071003be0700`
- a GATT real-time read response:
  `0e01004802000028d000023c00fb349b`
- a GATT battery/firmware read response:
  `5a2b332e322e32`

## Open Questions

- Does the purchased unit use model `HHCCJCY01`, `HHCCJCY10`, or another
  variant?
- Can Cisco Sensor Connect reliably perform the required write/read sequence
  against service `1204` without pairing?
- How many connected BLE slots are available on the nearby AP, and how often can
  battery polling run without affecting other devices?
- Should battery and firmware be polled by GATT later, or is advertisement-only
  operation sufficient for plant monitoring?
- Should future plant-specific metadata, such as plant name, pot location, and
  watering threshold, live in `devices` or in a separate plant table?

## References

- Cisco Sensor Connect for IoT Services overview:
  <https://developer.cisco.com/docs/spaces-connect-for-iot-services/>
- Cisco Sensor Connect control operations on BLE devices:
  <https://developer.cisco.com/docs/spaces-connect-for-iot-services/control-operations-on-ble-devices>
- Cisco Sensor Connect data telemetry from BLE devices:
  <https://developer.cisco.com/docs/spaces-connect-for-iot-services/data-telemetry-from-ble-devices>
- Cisco Sensor Connect quick start guide:
  <https://www.cisco.com/c/en/us/td/docs/wireless/spaces/iot-orchestrator/qsg/sensor-connect-iot-qsg.html>
- Home Assistant Xiaomi BLE Flower Care notes:
  <https://www.home-assistant.io/integrations/xiaomi_ble/#plant-sensor-flower-care--miflora-hhccjcy01>
- Xiaomi Flower Care API / MiFlora protocol notes:
  <https://github.com/vrachieru/xiaomi-flower-care-api>
- FlowerCareESP32 protocol and sample payloads:
  <https://github.com/SusanneThroner/FlowerCareESP32>
