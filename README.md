# home-metrics

Home Metrics stores BLE environmental sensor data, energy readings, and
collector runtime status in PostgreSQL/TimescaleDB. The web UI shows current and
historical measurements. The admin UI is intentionally limited to collector and
operational status.

Alarm rules, APNs push notifications, notification history, admin webhook
delivery, and sensor maintenance mode are not part of the current implementation.

## Layout

```text
cmd/hm-api-server/                     REST API and web UI
cmd/hm-ble-collector/                  local BLE scanner collector
cmd/hm-cisco-spaces-collector/         Cisco Spaces firehose collector
cmd/hm-cisco-iot-orchestrator-collector/
                                       Cisco Sensor Connect (IoT Orchestrator) MQTT collector
cmd/hm-db-migrate/                     schema migration CLI
cmd/hm-db-maint/                       rollup refresh and retention CLI
cmd/hm-nature-remo-collector/          Nature Remo energy collector
cmd/hm-echonet-collector/              ECHONET Lite energy collector
cmd/hm-apcupsd-collector/              APC UPS energy collector
db/schema.sql                          fresh DB schema
db/migrations/                         incremental migrations
docs/api.md                            REST API reference
docs/openapi.yaml                      OpenAPI summary
docs/xiaomi-flower-care-cisco-sensor-connect.md
                                       Xiaomi Flower Care preparation notes
web/                                   metrics and admin pages
```

`cmd/hm-api-server/` is split by responsibility:

- `main.go`: process startup, database connection, graceful shutdown.
- `server.go`: routing, web file serving, auth, CORS, and JSON helpers.
- `sensors.go`: sensor device, latest value, and sensor series endpoints.
- `energy.go`: energy latest and series endpoints.
- `admin_status.go`: collector/admin status endpoints.
- `admin_types.go`: admin response types and small PostgreSQL helpers.
- `config.go`: environment parsing helpers.

## Data Model

Main tables:

- `devices`: configured BLE/environment sensors.
- `sensor_minute`: one row per device/minute. Collectors store minute-level
  median samples for environmental values.
- `sensor_1hour`, `sensor_12hour`, `sensor_1day`: rollup tables refreshed by
  `hm-db-maint`.
- `energy_devices`, `energy_readings`, `energy_metric_definitions`: power and
  energy readings.
- `collector_status`: one row per collector target, updated by collectors.
- `schema_migrations`: migration state.

Migration `0008_drop_alarm_features.sql` removes the previous alarm/APNs/webhook
tables and the old device maintenance columns from existing databases.
Migrations `` and
`` update existing user-facing
Cisco Sensor Connect device type labels.
Migration `0011_drop_cisco_spaces_raw_events.sql` removes the retired Cisco
Spaces raw event table from existing databases. Cisco Spaces firehose data is
now decoded directly into `sensor_minute`; the collector keeps runtime state in
`collector_status` and no longer stores raw firehose payloads for replay/export.
Migration `0012_add_plant_sensor_metrics.sql` adds plant sensor columns
(`soil_moisture_percent` and `conductivity_us_cm`) to `sensor_minute` and all
rollup tables.

## Configuration

Use `examples/home-metrics.env.example` or
`examples/home-metrics.compose.env.example` as the starting point.

For direct, non-Docker collector runs, set `SENSOR_INGEST_SOURCE` for the
collector command you are starting. For Docker Compose, collector-specific
profile variables such as `CISCO_IOT_ORCH_INGEST_SOURCE` are mapped to
`SENSOR_INGEST_SOURCE` inside the service definition.

Common API settings:

```sh
API_TOKEN=change-me
API_REQUIRE_TOKEN=false
API_ALLOWED_ORIGINS=http://localhost:8080
COLLECTOR_STATUS_STALE_AFTER=5m
CISCO_SPACES_COLLECTOR_ENABLED=false
```

`COLLECTOR_STATUS_STALE_AFTER` controls `/api/health/details` and the admin UI
summary. A collector is stale when it has never succeeded, has consecutive
failures, or has not updated within this duration.

`hm-cisco-spaces-collector` is an optional profile. Keep
`CISCO_SPACES_COLLECTOR_ENABLED=false` when it is intentionally stopped; the
admin summary will show it as disabled instead of treating the stale
`collector_status` row as an alert. Set it to `true` only when the Cisco Spaces
firehose collector is expected to be running.

The Cisco Spaces firehose collector does not keep a raw event database. It
decodes incoming events and stores only the normalized minute-level sensor data
plus collector status.

## Docker Compose

Start the API, migration, and maintenance services:

```sh
docker compose up -d --build hm-api-server hm-db-maint
```

Enable Cisco Spaces firehose collection:

```sh
docker compose --profile cisco-spaces up -d hm-cisco-spaces-collector
```

Enable Cisco Sensor Connect (IoT Orchestrator) collection:

```sh
docker compose --profile cisco-iot up -d hm-cisco-iot-orchestrator-collector
```

## Cisco Sensor Connect (IoT Orchestrator)

The `hm-cisco-iot-orchestrator-collector` command receives BLE advertisements
from the IoT Orchestrator MQTT broker and exports decoded temperature, humidity,
battery, RSSI, lux, CO2, pressure, eTVOC, soil moisture, and conductivity values
to `sensor_minute`.

The internal collector, profile, and environment variable names intentionally
keep `cisco_iot_orchestrator` / `CISCO_IOT_ORCH_*` because they identify the
IoT Orchestrator endpoint. User-facing device labels use
`Cisco Sensor Connect (IoT Orchestrator)`.

Required application IDs and API keys:

- onboarding app: `CISCO_IOT_ORCH_ONBOARD_APP_ID` /
  `CISCO_IOT_ORCH_ONBOARD_API_KEY`
- control app: `CISCO_IOT_ORCH_CONTROL_APP_ID` /
  `CISCO_IOT_ORCH_CONTROL_API_KEY`
- data app: `CISCO_IOT_ORCH_DATA_APP_ID` /
  `CISCO_IOT_ORCH_DATA_API_KEY`

For the current lab, the data topic is:

```sh
CISCO_IOT_ORCH_TOPIC=ioslab/home-metrics/ble/advertisements/v1
```

Important setup sequence:

1. Register the target BLE devices with the onboarding app.
2. Register the data app with `registerDataApp`.
3. Register the advertisement topic with `registerTopic`.
4. Subscribe each BLE device to the data topic.
5. Start `hm-cisco-iot-orchestrator-collector` and verify `collector_status`
   plus `sensor_minute`.

The implementation treats advertisement telemetry as the source of truth. It
does not store raw MQTT/protobuf telemetry payloads. The collector decodes each
MQTT message in memory, keeps only minute-level aggregate windows, writes the
median values to `sensor_minute`, and drops successfully flushed windows.
`CISCO_IOT_ORCH_AGGREGATE_FLUSH_INTERVAL` controls periodic aggregate flushes
so the last minute is written even if no later MQTT message arrives. If database
writes fail, pending aggregate windows are retained and summarized in logs at
`CISCO_IOT_ORCH_PENDING_LOG_INTERVAL`.

Xiaomi Flower Care / MiFlora plant support also uses advertisement telemetry
for temperature, illuminance, soil moisture, and conductivity. Flower Care
battery and firmware are exposed through connected GATT reads, so the collector
can optionally poll only the battery characteristic at a very low frequency.
Configure this per device with `gatt_battery` in `sensors.json`; devices without
that block remain advertisement-only.

Example Flower Care target:

```json
{
  "mac": "5C:85:7E:14:73:7D",
  "label": "blue berry 1",
  "sensor_category": "Cisco Sensor Connect (IoT Orchestrator)",
  "gatt_battery": {
    "enabled": true,
    "device_id": "48c71db0-ce81-43c2-849f-5da7fef23ec4",
    "service_id": "1204",
    "characteristic_id": "00001a02-0000-1000-8000-00805f9b34fb",
    "poll_interval": "24h",
    "jitter": "30m",
    "advertisement_max_age": "10m"
  }
}
```

The scheduler uses the latest stored `battery_percent` to choose the next
polling time, then adds random jitter. With the default settings, a configured
plant sensor is polled once every 24 hours plus or minus 30 minutes. Before
connecting, the collector verifies that a recent advertisement was received; if
the latest telemetry is older than `advertisement_max_age`, the GATT poll is
skipped and retried later. Each successful poll connects through the control
API, reads `1a02`, stores only `battery_percent`, and disconnects. Real-time
GATT sensor reads and history reads are intentionally not part of the production
collector.

See
[docs/xiaomi-flower-care-cisco-sensor-connect.md](docs/xiaomi-flower-care-cisco-sensor-connect.md)
for preparation notes for testing Xiaomi Flower Care / MiFlora plant sensors.

## Web UI

- Metrics UI: `http://localhost:8080/`
- Admin UI: `http://localhost:8080/admin`

The admin UI shows:

- collector summary from `/api/health/details`
- Cisco Spaces firehose lock and status from `/api/admin/cisco-spaces-firehose`
- all collector status rows from `/api/admin/collector-status`
- schema migration status from `/api/admin/schema`

The admin UI does not expose alarm, webhook, APNs, or maintenance controls.

This is intentional: collector visibility is provided through
`collector_status`. Automated alarm evaluation, webhook delivery, APNs delivery,
and user notification history were removed to keep operations simple.

## API

See [docs/api.md](docs/api.md) and [docs/openapi.yaml](docs/openapi.yaml).

Implemented endpoints:

```text
GET /api/health
GET /api/health/details
GET /api/devices
GET /api/devices/{mac}/latest
GET /api/devices/{mac}/series
GET /api/energy/latest
GET /api/energy/series
GET /api/admin/schema
GET /api/admin/cisco-spaces-firehose
GET /api/admin/collector-status
```

## Build And Test

```sh
make test
make build
```

The Docker image builds every command listed in `tools/build.sh`.

## Deployment

CI builds and publishes the container image. Deploy with the image digest
produced by CI/CD, then run:

```sh
docker compose up -d hm-db-migrate
docker compose up -d hm-api-server hm-db-maint
docker compose --profile cisco-iot up -d hm-cisco-iot-orchestrator-collector
```

After deployment, verify:

```sh
curl -H "Authorization: Bearer $API_TOKEN" https://metrics.ioslab.jp/api/health/details
curl -H "Authorization: Bearer $API_TOKEN" https://metrics.ioslab.jp/api/admin/collector-status
```

## Systemd

Systemd unit files are provided under `deploy/` for non-Compose deployments.
There is no `hm-alert-worker` unit because alarm processing has been removed.
