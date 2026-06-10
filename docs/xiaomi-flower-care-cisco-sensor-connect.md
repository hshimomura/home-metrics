# Xiaomi Flower Care Design for Cisco Sensor Connect

This note describes the current `home-metrics` support for Xiaomi Flower Care
Plant Sensor devices, also known as MiFlora or `HHCCJCY01`, with Cisco Sensor
Connect (IoT Orchestrator).

The current implementation uses Cisco APs as the BLE receiver infrastructure,
receives Cisco Sensor Connect MQTT advertisement telemetry, decodes Xiaomi
`FE95` advertisement payloads, and stores plant readings in the existing
`home-metrics` time-series tables.

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
  and conductivity when the AP receives the expected `FE95` beacon payloads
- connected GATT read for battery and for deterministic real-time reads if
  advertisement data is incomplete or encrypted

`home-metrics` uses the advertisement path for the primary plant measurements.
GATT reads are used only as optional low-frequency auxiliary polling. A GATT
read requires a BLE connection, characteristic read/write operations, and
disconnect handling, which can increase sensor battery usage, consume AP BLE
connection slots, and pause normal advertisements while connected.

The production collector uses GATT for battery and can optionally use Flower
Care history reads to fill small gaps. GATT real-time reads are still diagnostic
only. Advertisement telemetry remains the source of truth when it is available.

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
- Label: `Blueberry1`
- SCIM device ID: `48c71db0-ce81-43c2-849f-5da7fef23ec4`
- SCIM application IDs: `onboard`, `control`, and `data`
- `isRandom`: `false`
- MQTT topic: `ioslab/home-metrics/ble/advertisements/v1`

The device was not reachable from the `pve2` local BLE scanner after being
placed in soil, but Cisco AP reception through Sensor Connect did receive the
advertisements. This confirms that the Cisco AP infrastructure is the relevant
ingest path for this placement.

The connected GATT path has also been validated through Cisco Sensor Connect:

1. Discover services for the onboarded device.
2. Connect to service `00001204-0000-1000-8000-00805f9b34fb`.
3. Write command `a0 1f` to characteristic
   `00001a00-0000-1000-8000-00805f9b34fb`.
4. Read real-time values from characteristic
   `00001a01-0000-1000-8000-00805f9b34fb`.
5. Optionally read battery/firmware from characteristic
   `00001a02-0000-1000-8000-00805f9b34fb`.
6. Disconnect after read to minimize battery impact and AP connection slot use.

The production collector implements the connect, battery read, and disconnect
portion as scheduled battery polling. If `history_backfill` is explicitly
enabled for the device, the same low-frequency poll also reads Flower Care
history through service `1206` using one short connection per entry and backfills
sparse history points into `sensor_minute`. It does not write `a0 1f` or read
real-time data from `1a01` in production.

On 2026-06-07, a local probe in `tools/flowercare_gatt_probe.go` confirmed that
Cisco Sensor Connect can read `Blueberry1` battery, firmware, real-time plant
values, device epoch, history count, and individual history entries. The stable
pattern is to use a short history-only connection and read one entry per
connection. The first entry is selected with `a1 01 00`, the second with
`a1 02 00`, and so on. Trying to read multiple entries in one connection was
less reliable, and concurrent control API calls are rejected by Cisco Sensor
Connect.

## BLE UUIDs and GATT Characteristics

Known UUIDs from public MiFlora/Flower Care implementations:

| Purpose | UUID |
| --- | --- |
| discovery / Xiaomi root service | `0000fe95-0000-1000-8000-00805f9b34fb` |
| real-time data service | `00001204-0000-1000-8000-00805f9b34fb` |
| mode / command characteristic | `00001a00-0000-1000-8000-00805f9b34fb` |
| real-time sensor data characteristic | `00001a01-0000-1000-8000-00805f9b34fb` |
| battery and firmware characteristic | `00001a02-0000-1000-8000-00805f9b34fb` |
| history data service | `00001206-0000-1000-8000-00805f9b34fb` |
| history command characteristic | `00001a10-0000-1000-8000-00805f9b34fb` |
| history data characteristic | `00001a11-0000-1000-8000-00805f9b34fb` |
| device epoch characteristic | `00001a12-0000-1000-8000-00805f9b34fb` |

History reads are not required when `home-metrics` continuously stores received
beacon data. They are available as an explicit, low-frequency backfill path for
small gaps caused by a temporary GATT connection or missed advertisements.

## GATT Decode Reference

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

This repository has also validated a real Cisco Sensor Connect GATT battery
read from `Blueberry1`:

```text
64 39 33 2e 33 2e 36
```

Decode:

- byte `00`: `0x64` = `100%`
- byte `01`: delimiter/unknown, observed as `0x39` on this unit
- bytes `02-06`: firmware ASCII, `33 2e 33 2e 36` = `3.3.6`

The production collector stores only the battery percentage. Firmware is logged
for troubleshooting and is not stored in the database.

History uses service `1206`. The protocol below is based on
`SusanneThroner/FlowerCareESP32` and the live `Blueberry1` probe:

1. Read the device epoch from characteristic `1a12`.
2. Write history init command `a0 00 00` to characteristic `1a10`.
3. Read characteristic `1a11`; bytes `00-01` are the history entry count as a
   little-endian `uint16`.
4. Select an entry by writing `a1 <index little-endian uint16>` to
   characteristic `1a10`.
5. Read the selected 16-byte entry from characteristic `1a11`.

The history entry payload is decoded as:

| Bytes | Type | Meaning |
| --- | --- | --- |
| `00-03` | uint32 little-endian | device timestamp seconds |
| `04-05` | int16 little-endian, divide by 10 | temperature C |
| `06` | unknown | ignore |
| `07-10` | uint32 little-endian | illuminance lux |
| `11` | uint8 | soil moisture percent |
| `12-13` | uint16 little-endian | conductivity uS/cm |
| `14-15` | unknown | ignore |

Because the history timestamp is relative to the Flower Care device epoch, map
it to wall-clock time at read time:

```text
entry_time = host_read_time - (device_epoch_now - entry_device_timestamp)
```

GATT history has second-level timestamps on the device. `home-metrics` stores
sensor data in `sensor_minute`, so truncate the resulting wall-clock timestamp
to the minute before inserting it.

Even though Flower Care history appears to be hourly, `home-metrics` keeps plant
readings in `sensor_minute` for now. Other APIs and dashboards already consume
that table, passive BLE advertisement scanning does not increase the sensor's
power draw, and using one common time-series table keeps latest and series
semantics consistent. GATT history backfill therefore adds sparse hourly points
into the minute table instead of creating a separate plant-history table.

The live probe result for `Blueberry1` on 2026-06-07:

```text
battery raw: 64 39 33 2e 33 2e 36
battery: 100%
firmware: 3.3.6
real-time raw: ed 00 03 d3 10 00 00 1a 17 01 02 3c 00 fb 34 9b
real-time decoded: 23.7 C, 4307 lux, 26%, 279 uS/cm
device epoch: 66815 seconds
history init raw: 02 00 2e 8c d6 13 08 00 d8 15 08 00 00 00 00 00
history count: 2
history entry 1 raw: 20 fd 00 00 ee 00 03 86 10 00 00 1a 19 01 00 00
history entry 1 decoded: 2026-06-07 11:58 JST, 23.8 C, 4230 lux, 26%, 281 uS/cm
history entry 2 raw: 10 ef 00 00 eb 00 03 c2 10 00 00 1a 1d 01 00 00
history entry 2 decoded: 2026-06-07 10:58 JST, 23.5 C, 4290 lux, 26%, 285 uS/cm
history entry 3 raw: ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff
history entry 3 decoded: invalid empty sentinel, do not ingest
```

The GATT data granularity is therefore:

- battery/firmware: one sparse point at the poll time
- real-time plant data: one current point at the poll time
- history plant data: one or more device-timestamped entries with second-level
  source timestamps, stored in `sensor_minute` at minute granularity

Operational notes from the probe:

- Use `history-only` GATT sessions for history reads. Battery and real-time reads
  can be done in a separate session.
- Use one connection per history entry: connect to service `1206`, read epoch,
  write init, read count, write the entry selector, read entry data, disconnect.
- Populate full device metadata on every history reading before backfill:
  `SensorMAC`, `Label`, `Location`, `IngestSource`, `SensorTypeCode`, and
  `SensorCategory`. `sensor_minute` upsert requires `SensorMAC`, and carrying
  the rest keeps poller wiring safe.
- Do not run GATT sessions in parallel inside the same collector process. Cisco
  Sensor Connect returned `No more than one API at a time` when two probes were
  started concurrently. The collector has a `ControlMu` mutex in `config`, and
  battery/history helpers hold it across the full connect, read/write, and
  disconnect sequence so battery, real-time, and history sessions cannot
  interleave.
- A longer read delay is not always safer. A 3-second delay caused Cisco Sensor
  Connect to report that the device was already disconnected. A 1-second delay
  worked for the successful entry reads.
- Treat all-`ff` entries or entries with a device timestamp greater than the
  current device epoch as invalid sentinel rows.
- Stop before reading past the reported history count. If the count is `0`, or
  the requested entry index is greater than the count, return a clean empty
  result without sending an entry selector such as `a1 01 00`. If a later entry
  fails after at least one successful entry, return partial readings and keep a
  stop reason for logging instead of silently treating every stop as a clean
  success.
- Public reverse-engineering notes describe a history clear command: write
  `a2 00 00` to the history command characteristic after a successful sync. The
  collector intentionally does not implement or send this command. Sensor-side
  history cleanup remains the responsibility of the official smartphone app,
  because syncing through that app clears the on-device history. `home-metrics`
  treats GATT history as read-only auxiliary backfill data.

## Current Advertisement Decode

The current `hm-cisco-iot-orchestrator-collector` decoder extracts BLE service
data from Cisco Sensor Connect MQTT payloads, including UUID `0xfe95`, and
parses observed plaintext Xiaomi MiBeacon object IDs.

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

Observed object IDs and values currently decoded:

| Object ID | Payload example | Meaning | Decoded value | Verified against app |
| --- | --- | --- | --- | --- |
| `0x1004` | `0d0410020601` | temperature | `26.2 C` | same value family |
| `0x1007` | `0d071003be0700` | illuminance | `1982 lux` | same value family |
| `0x1008` | `0d0810010b` | soil moisture | `11%` | yes |
| `0x1009` | `0d0910024100` | conductivity | `65 uS/cm` | yes |

The mobile Flower Care application showed fertility `65` and moisture `11`
while Sensor Connect delivered `0x1009 = 65` and `0x1008 = 11`. Therefore
`0x1008` is stored as soil moisture and `0x1009` is stored as conductivity.

Current decoder behavior:

- includes service data UUID `0xfe95` in advertisement extraction
- decodes the observed plaintext Xiaomi MiBeacon object IDs
- decodes temperature, lux, soil moisture, and conductivity from advertisements
  when present
- stores decoded beacon values in `home-metrics` and lets the database provide
  history
- optionally connects to Flower Care devices over GATT for battery-only polling
  when `gatt_battery` is configured for that target
- leaves `battery_percent` unset for Flower Care devices that do not have
  `gatt_battery` configured

Do not map soil moisture to `humidity_percent`. Both values are percentages,
but `humidity_percent` means air relative humidity in the current data model,
while Flower Care `0x1008` means soil moisture. Mixing them would make RoomPlus
and web UI behavior ambiguous once multiple plant sensors are added.

## Current home-metrics Data Model

Plant-specific values are stored as nullable columns in `sensor_minute` and all
rollup tables:

```text
soil_moisture_percent double precision
conductivity_us_cm double precision
```

The DB/API metric name is `conductivity_us_cm`. The Flower Care mobile
application exposes this value as fertility, but the underlying physical value
is conductivity measured in `uS/cm`. Use `Conductivity` as the user-facing label
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

## Implemented home-metrics Changes

The current implementation includes:

1. Schema support.
   - Migration `0012_add_plant_sensor_metrics.sql` adds
     `soil_moisture_percent` and `conductivity_us_cm`.
   - `db/schema.sql` includes the columns for fresh installs.
   - `sensor_1hour`, `sensor_12hour`, and `sensor_1day` include the same
     columns because `/api/devices/{mac}/series` serves longer ranges from
     rollup tables.
   - `hm-db-maint` rolls up both plant metrics.
   - `hm-db-check` includes the plant metrics in operational checks.

2. Cisco Sensor Connect collector support.
   - The internal reading and aggregate structs include
     `SoilMoisturePercent` and `ConductivityUSCM`.
   - Minute aggregation merges sparse Flower Care advertisements in the same
     one-minute bucket.
   - `sensor_minute` upserts use the same sparse
     `COALESCE(EXCLUDED.value, sensor_minute.value)` pattern as existing
     advertisement fields.
   - Device upsert uses explicit `ingest_source`, `sensor_type_code`, and
     `sensor_category` metadata from the sensor configuration.

3. Xiaomi `FE95` advertisement decoding.
   - `serviceDataFromAdvertisement` extracts UUID `0xfe95`.
   - `0x1004` maps to `temperature_c`.
   - `0x1007` maps to `lux`.
   - `0x1008` maps to `soil_moisture_percent`.
   - `0x1009` maps to `conductivity_us_cm`.
   - Existing Minew and Env decoder behavior is preserved.

4. Tests.
   - Decoder tests use real Sensor Connect advertisement examples captured from
     `5c:85:7e:14:73:7d`.
   - Tests verify that `0x1008` does not populate `humidity_percent`.
   - Tests verify sparse message merging across separate advertisements in the
     same minute window.

5. API and UI exposure.
   - `/api/devices/{mac}/latest` and `/api/devices/{mac}/series` expose
     `soil_moisture_percent` and `conductivity_us_cm`.
   - The latest API is a metric-level snapshot. It keeps the existing
     `device`, `ts`, and `values` response shape, but each value is the latest
     non-null value for that specific metric.
   - `value_timestamps` records the measurement time for each metric that has a
     value. This lets clients display low-frequency GATT battery readings
     without copying battery into every minute row.
   - The top-level `ts` is the newest timestamp across the metric values.
   - `/api/devices/{mac}/series` remains based on the original measurement
     timestamps.
   - `docs/openapi.yaml`, `docs/api.md`, and the web UI list the new metrics.
   - The user-facing label is `Conductivity`; the storage/API name is
     `conductivity_us_cm`.

6. Flower Care registration.
   - Each Flower Care MAC is added to the sensor configuration with a
     plant-oriented label.
   - `random=false` is used for Flower Care public MAC addresses.
   - SCIM onboarding uses application ID `onboard`; control uses `control`;
     data uses `data`.
   - The advertisement topic is registered and subscribed after onboarding.

7. Optional low-frequency GATT battery polling.
   - `gatt_battery` in `sensors.json` enables battery polling for a specific
     Flower Care target.
   - The default interval is `24h` with `30m` random jitter.
   - Before connecting, the collector checks that recent advertisement telemetry
     exists. The default `advertisement_max_age` is `10m`.
   - The collector connects to service `1204`, reads characteristic
     `00001a02-0000-1000-8000-00805f9b34fb`, stores byte `0` as
     `battery_percent`, logs the firmware string, and disconnects.
   - Battery readings are intentionally sparse. The API latest snapshot exposes
     the latest battery value with its own timestamp; the collector does not
     write copied battery values into later advertisement rows.
   - GATT polling failures are logged but do not mark the MQTT collector as
     unhealthy, because this is auxiliary telemetry.

8. GATT real-time decode and history backfill.
   - The collector has decode helpers for Flower Care real-time GATT payloads
     from `1204/1a01` and history entries from `1206/1a11`.
   - The collector also has a shared PostgreSQL `sensor_minute` upsert helper for
     GATT backfill rows. It uses the same `ON CONFLICT (ts, mac)` and
     `COALESCE(EXCLUDED.metric, sensor_minute.metric)` pattern as advertisement
     ingest, so a GATT row can fill missing values without overwriting existing
     advertisement values.
   - History timestamps are mapped from device seconds to wall-clock time with
     the device epoch read from `1a12`, then truncated to the minute for
     `sensor_minute`.
   - A standalone probe, `tools/flowercare_gatt_probe.go`, can confirm Cisco
     Sensor Connect GATT behavior without writing to PostgreSQL.
   - The live Cisco control API history entry read works when each entry is read
     through an independent history-only connection. This is reflected in
     `readGATTFlowerCareHistory`, which opens one short connection per entry.
   - `readGATTFlowerCareHistory` receives the full `targetDevice`, not only the
     GATT device ID, so returned readings are ready for `sensor_minute` backfill.
   - The helper stops before requesting an entry beyond the reported history
     count, treats count `0` as a clean empty history, and records partial-stop
     reasons for later logging.
   - GATT control sessions are serialized through `config.ControlMu` across the
     full connect, read/write, and disconnect sequence; keep this requirement
     when wiring battery, real-time, and history polling together.
   - Production history backfill is wired into the same low-frequency GATT
     poller as battery polling and runs only when `history_backfill` is true for
     the device.
   - Sensor-side history deletion is intentionally not implemented. Although the
     known protocol can mark historical data as synchronized by writing
     `a2 00 00` to `1a10`, `home-metrics` keeps GATT history read-only and relies
     on the smartphone app to clear device history when the app performs its own
     sync.
   - Keep limits conservative: one device at a time, short timeouts, random
     jitter, and a maximum history entry count. The FlowerCareESP32 reference
     notes that long history reads may lose the BLE connection after roughly
     110 entries.

The relevant `gatt_battery` config shape is:

```json
{
  "gatt": {
    "enabled": true,
    "device_id": "48c71db0-ce81-43c2-849f-5da7fef23ec4",
    "poll_interval": "24h",
    "jitter": "30m",
    "advertisement_max_age": "10m",
    "battery": true,
    "realtime": true,
    "history_backfill": false,
    "max_history_entries": 24
  }
}
```

The first configured Flower Care target is:

```json
{
  "mac": "5C:85:7E:14:73:7D",
  "label": "Blueberry1",
  "ingest_source": "cisco_sensor_connect",
  "sensor_type_code": "xiaomi_flower_care",
  "sensor_category": "plant",
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

The current lab deployment has four Flower Care targets. `Blueberry2`,
`Blueberry3`, and `Blueberry4` were identified with a pve2 BlueZ scan and then
SCIM onboarded with `isRandom=false`. Their advertisement topic subscription
uses `registerTopic` with `technology=ble`, `ble.type=advertisements`,
`ble.serviceID=fe95`, and `ble.characteristicID=fe95`.

| Label | MAC | SCIM device ID |
| --- | --- | --- |
| Blueberry1 | `5C:85:7E:14:73:7D` | `48c71db0-ce81-43c2-849f-5da7fef23ec4` |
| Blueberry2 | `5C:85:7E:14:70:AC` | `4f9c3816-709e-446b-83ca-d600b05883a7` |
| Blueberry3 | `5C:85:7E:14:70:9F` | `ba879169-a08b-46b8-ae3c-0212d3f9f101` |
| Blueberry4 | `5C:85:7E:14:73:4A` | `fb404a13-a2c9-4ef9-8030-892fd8a37723` |

## Device Metadata

The collector writes explicit device metadata:

```text
ingest_source: cisco_sensor_connect
sensor_type_code: xiaomi_flower_care
sensor_category: plant
```

`sensor_types` stores the user-facing display name and model metadata.

## Device Metadata and User-Facing Semantics

Use a plant-specific label for Flower Care devices so users do not confuse soil
moisture with room humidity. Do not encode this classification in labels or
transport names.

`ingest_source` identifies the transport and deployment path.
`sensor_type_code` identifies the concrete decoder/model family.
`sensor_category` identifies the user-facing category that RoomPlus and Grafana
can filter on.

RoomPlus and the web UI should treat `humidity_percent` and
`soil_moisture_percent` as distinct metrics. If a compact display needs only
one moisture-like value, choose by metric availability and `sensor_category`
rather than by writing soil moisture into the air humidity column.

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
10. Confirm `0x1008` is stored as `soil_moisture_percent`.
11. Confirm `0x1009` is stored as `conductivity_us_cm`.
12. Add `gatt_battery` only if battery visibility is required. Keep the default
    24-hour interval and 30-minute jitter unless there is a clear operational
    reason to poll more often.

## Implementation Notes

The Flower Care support touches:

- `cmd/hm-cisco-iot-orchestrator-collector/main.go`
- `cmd/hm-cisco-iot-orchestrator-collector/main_test.go`
- `tools/flowercare_gatt_probe.go`
- `db/schema.sql`
- `db/migrations/0012_add_plant_sensor_metrics.sql`
- `cmd/hm-db-maint/main.go`
- `cmd/hm-api-server/sensors.go`
- `cmd/hm-api-server/main_test.go`
- `cmd/hm-db-check/main.go`
- `docs/openapi.yaml` and `docs/api.md`
- `web/index.html`

Implemented decoder test fixtures:

- raw MQTT advertisements carrying `FE95` service data:
  - soil moisture: `020106030295fe131695fe71209800977d73147e855c0d0810010b`
  - conductivity: `020106030295fe141695fe71209800987d73147e855c0d0910024100`
  - temperature: `020106030295fe141695fe71209800997d73147e855c0d0410020601`
  - lux: `020106030295fe151695fe712098009a7d73147e855c0d071003be0700`
- a GATT real-time read response, reference only:
  `0e01004802000028d000023c00fb349b`
- a GATT battery/firmware read response, reference only:
  `5a2b332e322e32`
- a real Cisco Sensor Connect GATT battery/firmware response from `Blueberry1`:
  `6439332e332e36`, decoded as `battery_percent=100` and firmware `3.3.6`
- a real Cisco Sensor Connect GATT real-time response from `Blueberry1`:
  `ed0003d31000001a1701023c00fb349b`, decoded as `23.7 C`, `4307 lux`,
  `26%`, and `279 uS/cm`
- a FlowerCareESP32 history entry response:
  `a0224c00140100e90300000528000000`, decoded as device timestamp `4989600`,
  `27.6 C`, `1001 lux`, `5%`, and `40 uS/cm`
- real Cisco Sensor Connect history entries from `Blueberry1`:
  - entry `1`, `20fd0000ee0003861000001a19010000`, decoded as `2026-06-07
    11:58 JST`, `23.8 C`, `4230 lux`, `26%`, and `281 uS/cm`
  - entry `2`, `10ef0000eb0003c21000001a1d010000`, decoded as `2026-06-07
    10:58 JST`, `23.5 C`, `4290 lux`, `26%`, and `285 uS/cm`
  - entry `3`, `ffffffffffffffffffffffffffffffff`, rejected as an invalid sentinel
- GATT history read sequencing:
  `readGATTFlowerCareHistory` sends `a00000`, then `a10100`, disconnects, and
  repeats with `a00000`, `a10200` for the next entry
- PostgreSQL missing-data backfill behavior:
  `backfillSensorMinuteReadings` inserts minute-truncated GATT readings into
  `sensor_minute` and preserves existing non-null values with `COALESCE` during
  conflicts

Validation run on 2026-06-07:

```sh
go test ./...
GO111MODULE=off go build -o /tmp/flowercare-gatt-probe ./tools/flowercare_gatt_probe.go
```

Both commands completed successfully. The live `Blueberry1` probe confirmed
battery, firmware, real-time data, device epoch, history count, and two valid
history entries. Production backfill remains opt-in through `history_backfill`
because connected GATT pauses advertisements and Cisco control API calls must be
serialized.

## Open Questions

- Does the purchased unit use model `HHCCJCY01`, `HHCCJCY10`, or another
  variant?
- If more plant sensors are added, how many connected BLE slots are available
  on the nearby AP, and should battery polling be staggered more aggressively
  than the default 30-minute jitter?
- Should firmware be stored in a device metadata table later, or is logging it
  during battery polling sufficient?
- Should automated production history backfill become adaptive and stop after
  the newest already-stored history minute is found, instead of always walking up
  to `max_history_entries`?
- Is advertisement plus opt-in history backfill sufficient for plant monitoring,
  or should production eventually add a separate real-time GATT poll mode?
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
