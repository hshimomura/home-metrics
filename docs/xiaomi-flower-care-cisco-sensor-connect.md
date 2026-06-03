# Xiaomi Flower Care Preparation for Cisco Sensor Connect

This note collects preparation details for testing a Xiaomi Flower Care Plant
Sensor, also known as MiFlora or `HHCCJCY01`, with Cisco Sensor Connect
(IoT Orchestrator).

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

When the sensor arrives, capture the MQTT advertisement payload first. Then add
decoder support only after confirming the actual AP/MQTT payload shape. Expected
work:

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

Do not map soil moisture to `humidity_percent`. Soil moisture is a different
measurement and should get its own data model field or a plant-specific table.

## home-metrics Data Model Impact

Current `sensor_minute` has no columns for plant-specific values:

- soil moisture percent
- conductivity, uS/cm

Before storing Flower Care values, decide one of these approaches:

1. Extend `sensor_minute` with `soil_moisture_percent` and
   `conductivity_us_cm`.
2. Add plant-specific tables, for example `plant_devices` and
   `plant_sensor_minute`.

The first approach is simpler and keeps one time-series API. The second approach
keeps environmental room sensors separate from plant sensors. Because Flower
Care has plant-specific semantics, the second approach may be cleaner if more
plant sensors are expected.

For the first test, it is acceptable to decode and log Flower Care MQTT/GATT
values without writing them to PostgreSQL until the data model decision is made.

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
11. Decide schema support for soil moisture and conductivity.
12. Implement decoder and tests using captured payloads.

## Implementation Notes

Initial code changes are likely to touch:

- `cmd/hm-cisco-iot-orchestrator-collector/main.go`
- `cmd/hm-cisco-iot-orchestrator-collector/main_test.go`
- `db/schema.sql`
- a new numbered migration if plant values are stored
- `docs/openapi.yaml` and `docs/api.md` if new API fields are exposed
- `web/index.html` if plant values should be shown in the UI

Potential decoder test fixtures:

- a raw MQTT advertisement carrying `FE95` service data
- a GATT real-time read response:
  `0e01004802000028d000023c00fb349b`
- a GATT battery/firmware read response:
  `5a2b332e322e32`

## Open Questions

- Does the purchased unit use model `HHCCJCY01`, `HHCCJCY10`, or another
  variant?
- Does its firmware broadcast plaintext sensor values, encrypted MiBeacon
  values, or only discovery information?
- Can Cisco Sensor Connect reliably perform the required write/read sequence
  against service `1204` without pairing?
- How many connected BLE slots are available on the nearby AP, and how often can
  battery polling run without affecting other devices?
- Should plant measurements be part of the existing sensor API or a separate
  plant API?

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
