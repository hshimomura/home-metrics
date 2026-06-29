# Architecture

Home Metrics collects environmental, plant, power, and UPS measurements into
PostgreSQL/TimescaleDB. It exposes telemetry APIs, sensor threshold rule APIs,
and two small server-rendered web pages. Alert evaluation is intentionally
separate from notification delivery. APNs, webhooks, user notification-device
registration, and device maintenance workflows are not implemented.

## Runtime Components

| Component | Responsibility |
| --- | --- |
| `hm-api-server` | REST API, metrics page, and admin status page. |
| `hm-sensor-alert-worker` | Evaluates sensor threshold rules and records state transitions. |
| `hm-db-migrate` | Applies immutable numbered migrations. |
| `hm-db-maint` | Refreshes sensor rollups and enforces minute retention. |
| `hm-ble-collector` | Reads BLE advertisements from a local BlueZ adapter. |
| `hm-cisco-iot-orchestrator-collector` | Reads Cisco Sensor Connect MQTT telemetry and performs optional GATT control. |
| `hm-cisco-spaces-collector` | Reads the optional Cisco Spaces Firehose stream. |
| `hm-nature-remo-collector` | Stores Nature Remo power readings. |
| `hm-echonet-collector` | Stores ECHONET Lite energy readings. |
| `hm-apcupsd-collector` | Stores APC UPS readings. |
| `hm-energy-influx-import` | Imports supported historical energy data. |
| `hm-db-check` | Performs database and metric consistency checks. |

Docker Compose is the primary deployment model. Optional collectors are
enabled with profiles. Systemd units under `deploy/` support selected
non-Compose installations; they do not represent every Compose service.

## Sensor Ingestion

Every sensor reading is normalized to `internal/sensor.Reading`. The shared
`internal/sensorstore` package owns device reconciliation and sparse minute
upserts. Protocol decoding remains inside each collector.

The supported sensor metric keys are:

```text
temperature_c
humidity_percent
battery_percent
rssi_dbm
pressure_hpa
co2_ppm
lux
etvoc
soil_moisture_percent
conductivity_us_cm
```

Writes use sparse merge semantics. When multiple partial readings target the
same `(ts, mac)` minute, only non-null incoming metrics replace stored values.
A partial advertisement or GATT reading therefore cannot erase a metric written
earlier in that minute.

The direct BlueZ collector enables an outlier confirmation filter by default.
After an initial history is established, a value beyond its metric threshold is
held as pending and accepted only when a similar follow-up arrives within the
confirmation window. `BLE_OUTLIER_*` settings in the environment examples
control the history size, window, and metric thresholds.

Cisco Sensor Connect MQTT payloads are decoded in memory. The collector keeps
per-device minute aggregation windows, writes their median metric values, and
drops successfully flushed windows. Raw MQTT/protobuf and Cisco Spaces
Firehose payloads are not persisted.

## Device Registry

`devices` stores identity and current collection metadata:

- `ingest_source`: collector ownership, such as `ble`, `cisco_spaces`, or
  `cisco_sensor_connect`.
- `sensor_type_code`: optional stable decoder/model code joined to
  `sensor_types`.
- `sensor_category`: client-facing classification such as `environment` or
  `plant`.
- `enabled`: whether the owning collector should collect the device.

Ownership is one device to one collector. A source explicitly configured for a
device may claim an existing `NULL` owner once. A different non-null owner is a
configuration conflict and is not overwritten. The same owner can disable and
re-enable its device. Collector startup does not migrate ownership, prune
another source's devices, or infer ownership from a default source.

To move a device between sources:

1. Stop the old collector.
2. Change `devices.ingest_source` with both MAC and expected old source in the
   SQL `WHERE` clause; require exactly one updated row.
3. Change the sensor configuration.
4. Start the new collector.
5. Verify `devices`, `collector_status`, and recent telemetry.

An empty `sensor_type_code` is allowed for a generic device. A non-empty code
must exist in `sensor_types`. When a sensor type is selected, its category is
the default category; an explicit conflicting category is rejected.

## Storage And Retention

`sensor_minute` is the source table for environmental and plant telemetry.
Plant values use dedicated columns; soil moisture is never written to air
humidity.

The rollup tables are:

- `sensor_1hour`
- `sensor_12hour`
- `sensor_1day`

Each table stores an average and a non-null sample count for every metric.
`sensor_1hour` is built from minute values. The 12-hour and daily tables use
metric-specific weighted averages:

```text
sum(lower_average * lower_count) / sum(lower_count)
```

`rollup_accuracy_state.accuracy_cutoff` is initialized once to the first full
daily boundary after the oldest retained minute. `hm-db-maint` only rebuilds
buckets at or after that cutoff. Older averages are preserved because their
original sample counts cannot be reconstructed. Accuracy means the average of
non-null normalized `sensor_minute` values, not the average of raw packets.

Compose currently defaults both minute retention and rollup refresh lookback to
`336h` (14 days). `hm-db-maint` runs hourly in the Compose loop.

Energy data is independent of the sensor rollups:

- `energy_devices` identifies source/device pairs.
- `energy_readings` stores timestamped metric values.
- `energy_metric_definitions` stores display metadata for supported metrics.

## Latest And Series Semantics

`GET /api/devices/{mac}/latest` constructs a metric-level snapshot. Each metric
is selected from its newest non-null row, so a daily GATT battery value remains
visible beside newer advertisement values. `value_timestamps` records the
measurement time of each returned metric. No values are copied forward in the
database.

`GET /api/devices/{mac}/series` keeps original measurement timing and selects a
source table by requested range:

| Range | Lookback | Response bucket | Source |
| --- | --- | --- | --- |
| `1d` | 1 day | 8 minutes | `sensor_minute` |
| `1w` | 7 days | 1 hour | `sensor_1hour` |
| `1m` | 30 days | 4 hours | `sensor_1hour` |
| `3m` | 90 days | 12 hours | `sensor_12hour` |
| `1y` | 365 days | 1 day | `sensor_1day` |

## Sensor Threshold Alerts

Sensor alerts operate on the newest non-null value for one device metric. The
worker uses `internal/sensor.Metrics` as the only metric-to-column registry and
does not copy values forward in `sensor_minute`.

Each rule has separate trigger and clear thresholds. This hysteresis prevents
rapid firing/resolution near a boundary. A rule can also require multiple
fresh observations spanning `for_duration`; repeated evaluation of the same
measurement timestamp does not advance that duration. `max_data_age` is set
per rule so frequent temperature telemetry and daily battery telemetry can use
different freshness limits.

The state machine is:

```text
normal -> pending -> firing -> normal
```

Only transitions into `firing` and back to `normal` create
`sensor_alert_events`. Missing or stale data never resolves a firing alert;
the state remains firing and `evaluation_status` reports `no_data` or `stale`.
Disabling a rule or device resolves an active alert with an explicit reason.

The worker holds a PostgreSQL advisory lock so only one evaluator is active.
State updates and event inserts occur in one transaction. APNs and webhook
delivery are deliberately absent; clients poll current state or transition
events. See `docs/sensor-alerts.md` for rule examples and client behavior.

## Collector Status

Collectors report one row per `(collector_name, target_type, target_key)` to
`collector_status`. Success, data success, and failure update distinct fields.
The API considers a target stale when it has never succeeded, has consecutive
failures, or has not been updated within its threshold.

The default stale threshold is five minutes. Cisco Sensor Connect GATT targets
use 26 hours because normal polling is once every 24 hours with jitter. MQTT and
GATT health are independent. Every GATT device uses
`target_type=gatt_control` and its normalized MAC as `target_key`, preventing
one device's success from clearing another device's failure.

The optional Cisco Spaces collector is excluded from normal health and admin
collector lists when `CISCO_SPACES_COLLECTOR_ENABLED=false`. Its dedicated
status endpoint still reports that it is disabled.

## Database And Process Lifecycle

The Cisco Sensor Connect collector uses `pgxpool.Pool` because MQTT flushes,
status writes, and GATT work can overlap. Shutdown follows this order:

1. Cancel MQTT and GATT work.
2. Wait for the GATT worker.
3. Flush final completed aggregation windows.
4. Close the database pool.

GATT control sessions are serialized because the orchestrator accepts one
control operation at a time. A battery write followed by a history failure is a
partial success: `last_data_at` is updated, then the final target health is
marked failed.

## Contract Enforcement

`internal/sensor.Metrics` is the canonical Go metric registry. Contract tests
ensure every metric is represented in the fresh schema, weighted-rollup
migration, OpenAPI contract, and web UI. CI runs Go tests, builds every runtime
command, and lints `docs/openapi.yaml`.
