# Xiaomi Flower Care With Cisco Sensor Connect

This document describes the implemented Xiaomi Flower Care / MiFlora
`HHCCJCY01` support. Cisco APs receive BLE advertisements, Cisco Sensor Connect
(IoT Orchestrator) publishes them over MQTT, and
`hm-cisco-iot-orchestrator-collector` decodes and stores the measurements.

Primary telemetry is connectionless advertisement data. Connected GATT is an
optional, low-frequency path for battery and bounded history backfill.

## Supported Measurements

| Measurement | Source | Stored metric |
| --- | --- | --- |
| Temperature | FE95 advertisement | `temperature_c` |
| Illuminance | FE95 advertisement | `lux` |
| Soil moisture | FE95 advertisement | `soil_moisture_percent` |
| Soil conductivity | FE95 advertisement | `conductivity_us_cm` |
| Battery | GATT `1204/1a02` | `battery_percent` |
| Firmware | GATT `1204/1a02` | logged only |
| Historical plant readings | opt-in GATT `1206` | sparse `sensor_minute` rows |

Flower Care does not provide air relative humidity. Object `0x1008` is soil
moisture and must not populate `humidity_percent`. Although the mobile app may
label conductivity as fertility, the API and UI use `Conductivity`, measured in
`uS/cm`.

## Device Configuration

Use explicit transport, model, and client category metadata:

```json
{
  "mac": "5C:85:7E:14:73:7D",
  "label": "Blueberry1",
  "location": "Greenhouse",
  "ingest_source": "cisco_sensor_connect",
  "sensor_type_code": "xiaomi_flower_care",
  "sensor_category": "plant",
  "enabled": true,
  "gatt_battery": {
    "enabled": true,
    "device_id": "48c71db0-ce81-43c2-849f-5da7fef23ec4",
    "service_id": "1204",
    "characteristic_id": "00001a02-0000-1000-8000-00805f9b34fb",
    "poll_interval": "24h",
    "jitter": "30m",
    "advertisement_max_age": "10m",
    "history_backfill": false,
    "max_history_entries": 24
  }
}
```

`gatt_battery` is optional. Without it, all passive plant measurements continue
to work and `battery_percent` remains absent. This is the preferred default
when battery visibility is not required.

## Cisco Sensor Connect Setup

The deployment uses distinct application IDs and credentials:

- onboarding application: SCIM device registration;
- control application: connect, read, write, and disconnect;
- data application: MQTT topic registration and subscription.

The order matters:

1. Add the MAC or prefix to the AP scan allow-list and push the policy.
2. SCIM onboard the device using the onboarding application and
   `isRandom=false` for the observed stable Flower Care MAC.
3. Register the data application.
4. Register a BLE advertisement topic for service `fe95`.
5. Subscribe the onboarded device to that topic.
6. Confirm MQTT telemetry before enabling optional GATT polling.

The production topic is configured with `CISCO_IOT_ORCH_TOPIC`; the repository
default is `ioslab/home-metrics/ble/advertisements/v1`.

The orchestrator `device_id` returned by SCIM is required for GATT control and
belongs in `gatt_battery.device_id`. It is distinct from the BLE MAC used as the
database device key.

## Advertisement Decode

Cisco Sensor Connect carries the BLE advertisement in the protobuf
`DataSubscription.data` bytes. The collector scans the normal BLE advertising
data structures and extracts service data UUID `0xfe95`.

Observed full advertisement:

```text
020106030295fe131695fe71209800977d73147e855c0d0810010b
```

Extracted FE95 service data:

```text
71209800977d73147e855c0d0810010b
```

The embedded MAC bytes `7d73147e855c` are reverse byte order for
`5c:85:7e:14:73:7d`.

Implemented plaintext MiBeacon objects:

| Object | Example object bytes | Decode |
| --- | --- | --- |
| `0x1004` | `0d0410020601` | signed little-endian temperature / 10 = `26.2 C` |
| `0x1007` | `0d071003be0700` | little-endian illuminance = `1982 lux` |
| `0x1008` | `0d0810010b` | soil moisture = `11%` |
| `0x1009` | `0d0910024100` | conductivity = `65 uS/cm` |

Real decoder fixtures:

```text
soil moisture: 020106030295fe131695fe71209800977d73147e855c0d0810010b
conductivity:  020106030295fe141695fe71209800987d73147e855c0d0910024100
temperature:   020106030295fe141695fe71209800997d73147e855c0d0410020601
illuminance:   020106030295fe151695fe712098009a7d73147e855c0d071003be0700
```

Separate objects can arrive in separate advertisements. The collector combines
their decoded values in the same minute aggregation window and the database
uses sparse merge, so a moisture-only message cannot erase temperature or lux.

Encrypted or unsupported MiBeacon objects are ignored; the collector does not
attempt to decrypt them.

## GATT UUIDs

| Purpose | UUID |
| --- | --- |
| Xiaomi advertisement service | `0000fe95-0000-1000-8000-00805f9b34fb` |
| Real-time/battery service | `00001204-0000-1000-8000-00805f9b34fb` |
| Mode command | `00001a00-0000-1000-8000-00805f9b34fb` |
| Real-time values | `00001a01-0000-1000-8000-00805f9b34fb` |
| Battery and firmware | `00001a02-0000-1000-8000-00805f9b34fb` |
| History service | `00001206-0000-1000-8000-00805f9b34fb` |
| History command | `00001a10-0000-1000-8000-00805f9b34fb` |
| History data | `00001a11-0000-1000-8000-00805f9b34fb` |
| Device epoch | `00001a12-0000-1000-8000-00805f9b34fb` |

Production does not use the real-time `1a01` path. It is implemented in the
standalone diagnostic probe only. Advertisement data remains the source of
truth for current plant readings.

## Battery Polling

For each enabled `gatt_battery` target, the collector:

1. Schedules from the latest stored battery timestamp, using `poll_interval`
   plus signed random `jitter`.
2. Requires recent telemetry within `advertisement_max_age`.
3. Connects through the control application to service `1204`.
4. Reads characteristic `1a02`.
5. Stores byte zero as `battery_percent` and logs the firmware bytes.
6. Disconnects.

If no prior battery exists, the first poll is scheduled after a non-negative
random delay up to `jitter`. A failed poll retries after 15 minutes. Control
sessions are serialized across all devices.

Real response from Blueberry1:

```text
64 39 33 2e 33 2e 36
```

- `0x64` = `100%` battery;
- firmware bytes decode to `3.3.6`;
- the separator byte is device-dependent and is not interpreted.

Battery is intentionally sparse. The latest API retrieves the newest non-null
battery and returns its own timestamp; it does not copy battery into subsequent
advertisement rows.

## Optional History Backfill

`history_backfill` defaults to false. When enabled after a successful battery
read, the collector reads at most `max_history_entries` entries. Every entry
uses a separate short connection because this was more reliable with Cisco
Sensor Connect.

For each entry:

1. Connect to service `1206`.
2. Read device epoch from `1a12`.
3. Write `a0 00 00` to `1a10` and read the history count from `1a11`.
4. Write `a1 <index little-endian>` to `1a10`.
5. Read and decode the selected `1a11` entry.
6. Disconnect.

History entry layout:

| Bytes | Decode |
| --- | --- |
| `00-03` | device timestamp, uint32 little-endian |
| `04-05` | temperature, int16 little-endian / 10 |
| `07-10` | illuminance, uint32 little-endian |
| `11` | soil moisture percent |
| `12-13` | conductivity uS/cm |

Wall-clock time is calculated at read time:

```text
entry_time = host_read_time - (device_epoch_now - entry_device_timestamp)
```

The result is truncated to a minute and sparse-upserted into `sensor_minute`.
All-`ff` entries, future device timestamps, and entries beyond the reported
count are rejected. If a later entry fails, already decoded entries are kept
and the stop reason is logged.

The collector never sends the known history-clear command `a2 00 00`. Device
history remains read-only; cleanup is left to the official mobile application.

## Collector Status

MQTT and GATT have separate status targets. Each Flower Care GATT target uses:

```text
collector_name = hm-cisco-iot-orchestrator-collector
target_type = gatt_control
target_key = normalized BLE MAC
```

If battery storage succeeds but history fails, the collector first records data
success and then failure. This preserves `last_data_at` while leaving final
health failed until a later successful poll. GATT uses the 26-hour stale
threshold instead of the normal five-minute threshold.

## Diagnostics

`tools/scan_bluez.py` can establish a direct BlueZ advertisement baseline when
the sensor is within host range. `tools/flowercare_gatt_probe.go` exercises
Cisco Sensor Connect GATT without writing to PostgreSQL.

Useful checks after adding a device:

1. Confirm FE95 advertisements reach MQTT.
2. Confirm `devices` metadata is `cisco_sensor_connect`,
   `xiaomi_flower_care`, and `plant`.
3. Confirm `sensor_minute` receives temperature, lux, soil moisture, and
   conductivity without `humidity_percent`.
4. If GATT is enabled, confirm a MAC-specific `gatt_control` status row after
   the first scheduled poll.
5. Confirm `/api/devices/{mac}/latest` shows battery with its own
   `value_timestamps.battery_percent`.

## References

- [Cisco Sensor Connect for IoT Services](https://developer.cisco.com/docs/spaces-connect-for-iot-services/)
- [Cisco BLE control operations](https://developer.cisco.com/docs/spaces-connect-for-iot-services/control-operations-on-ble-devices/)
- [Cisco BLE data telemetry](https://developer.cisco.com/docs/spaces-connect-for-iot-services/data-telemetry-from-ble-devices/)
- [Cisco Sensor Connect quick start](https://www.cisco.com/c/en/us/td/docs/wireless/spaces/iot-orchestrator/qsg/sensor-connect-iot-qsg.html)
- [Home Assistant Xiaomi BLE Flower Care notes](https://www.home-assistant.io/integrations/xiaomi_ble/#plant-sensor-flower-care--miflora-hhccjcy01)
- [xiaomi-flower-care-api protocol notes](https://github.com/vrachieru/xiaomi-flower-care-api)
- [FlowerCareESP32 protocol notes](https://github.com/SusanneThroner/FlowerCareESP32)
