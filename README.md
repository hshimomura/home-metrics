# home-metrics

Home Metrics collects environmental, plant, energy, and UPS measurements into
PostgreSQL/TimescaleDB. It provides a read-only REST API, a metrics page, and an
admin page for collector and schema status.

The current service does not implement alert rules, APNs, webhooks,
notification history, or device maintenance controls. It stores normalized
sensor and energy readings rather than raw Cisco MQTT or Firehose payloads.

## Components

```text
cmd/hm-api-server/                     REST API and web pages
cmd/hm-db-migrate/                     immutable schema migrations
cmd/hm-db-maint/                       sensor rollups and retention
cmd/hm-db-check/                       database consistency checks
cmd/hm-ble-collector/                  local BlueZ BLE advertisements
cmd/hm-cisco-iot-orchestrator-collector/
                                       Cisco Sensor Connect MQTT and GATT
cmd/hm-cisco-spaces-collector/         optional Cisco Spaces Firehose
cmd/hm-nature-remo-collector/          Nature Remo power
cmd/hm-echonet-collector/              ECHONET Lite energy
cmd/hm-apcupsd-collector/              APC UPS metrics
cmd/hm-energy-influx-import/           supported historical energy import
internal/sensor/                       canonical sensor metric model
internal/sensorstore/                  ownership and sparse DB writes
db/schema.sql                          current fresh-install schema
db/migrations/                         incremental existing-DB migrations
web/                                   metrics and admin pages
```

## Documentation

- [Documentation map](docs/README.md)
- [Architecture](docs/architecture.md)
- [REST API](docs/api.md)
- [OpenAPI contract](docs/openapi.yaml)
- [Client contract](docs/client-contract.md)
- [Release and deployment](docs/release.md)
- [Xiaomi Flower Care](docs/xiaomi-flower-care-cisco-sensor-connect.md)

## Data Model

Sensor identity and classification live in `devices` and `sensor_types`.
Normalized sensor telemetry is stored in `sensor_minute`; one-hour, 12-hour,
and daily tables hold averages plus metric-specific sample counts. Plant
metrics use dedicated `soil_moisture_percent` and `conductivity_us_cm` columns.

`hm-db-maint` refreshes rollups and deletes expired minute data. The Compose
defaults are 14 days for both refresh lookback and minute retention. Weighted
12-hour and daily averages are guaranteed only at or after the immutable
`rollup_accuracy_state.accuracy_cutoff`; older averages are preserved because
their original counts are unavailable.

Energy and UPS values use `energy_devices`, `energy_readings`, and
`energy_metric_definitions`. Collector runtime state uses `collector_status`.

`db/schema.sql` is the current fresh-install snapshot. Existing databases are
upgraded by `hm-db-migrate`; migration files are immutable after deployment.

## Configuration

Use one of these templates:

- `examples/home-metrics.compose.env.example` for Docker Compose
- `examples/home-metrics.env.example` for direct command or systemd use
- `examples/sensors.json.example` for sensor ownership and decoder metadata

Common API and status settings:

```sh
API_TOKEN=change-me
API_REQUIRE_TOKEN=true
API_ALLOWED_ORIGINS=https://example.invalid
COLLECTOR_STATUS_STALE_AFTER=5m
GATT_CONTROL_STATUS_STALE_AFTER=26h
CISCO_SPACES_COLLECTOR_ENABLED=false
```

Every configured sensor should set `ingest_source`. The supported ownership
values are collector-specific; the Cisco Sensor Connect database value is
`cisco_sensor_connect`. Docker Compose maps `BLE_INGEST_SOURCE`,
`CISCO_SPACES_INGEST_SOURCE`, and `CISCO_IOT_ORCH_INGEST_SOURCE` to
`SENSOR_INGEST_SOURCE` inside their respective containers. For a direct command,
set `SENSOR_INGEST_SOURCE` itself.

One device has one owning collector. An explicit source may claim a database
device whose owner is `NULL`, but it cannot overwrite another non-null source.
The owner can synchronize `enabled=false` and later re-enable the device.

## Docker Compose

Start the core services:

```sh
docker compose up -d db hm-db-migrate hm-db-maint hm-api-server
```

Enable the collectors needed by the deployment:

```sh
docker compose --profile cisco-iot up -d hm-cisco-iot-orchestrator-collector
docker compose --profile nature-remo up -d hm-nature-remo-collector
docker compose --profile apcupsd up -d hm-apcupsd-collector
docker compose --profile echonet up -d hm-echonet-collector
```

Other optional sensor paths:

```sh
docker compose --profile ble up -d hm-ble-collector
docker compose --profile cisco-spaces up -d hm-cisco-spaces-collector
```

The Cisco Spaces profile is intentionally optional. Keep
`CISCO_SPACES_COLLECTOR_ENABLED=false` while it is stopped so old status rows do
not degrade health. Set it to `true` only when the profile is expected to run.
Starting it does not prune or disable configured devices.

## Cisco Sensor Connect

`hm-cisco-iot-orchestrator-collector` receives BLE advertisement telemetry from
the Cisco Sensor Connect (IoT Orchestrator) MQTT endpoint. The internal command,
profile, and environment variables retain the `cisco_iot_orchestrator` and
`CISCO_IOT_ORCH_*` names; the database ownership value is
`cisco_sensor_connect`.

Use separate application IDs and API keys for onboarding, control, and data:

```sh
CISCO_IOT_ORCH_ONBOARD_APP_ID=onboard
CISCO_IOT_ORCH_ONBOARD_API_KEY=...
CISCO_IOT_ORCH_CONTROL_APP_ID=control
CISCO_IOT_ORCH_CONTROL_API_KEY=...
CISCO_IOT_ORCH_DATA_APP_ID=data
CISCO_IOT_ORCH_DATA_API_KEY=...
CISCO_IOT_ORCH_TOPIC=ioslab/home-metrics/ble/advertisements/v1
```

The orchestrator-side setup order is:

1. SCIM onboard each BLE device with the onboarding application.
2. Register the data application.
3. Register the advertisement topic.
4. Subscribe each device to that topic.
5. Start the collector and verify MQTT status and minute telemetry.

The collector decodes MQTT/protobuf messages in memory, merges sparse BLE
objects into per-minute windows, and stores median values. Successful windows
are discarded; failed DB writes remain pending for retry. MQTT reconnect delay
resets after connect and subscribe succeed.

### Optional Flower Care GATT

Xiaomi Flower Care advertisements provide temperature, illuminance, soil
moisture, and conductivity. Battery and firmware require a connected GATT read.
Add a `gatt_battery` block to a Flower Care target only when battery visibility
is needed:

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

The collector requires a recent advertisement, serializes control sessions,
connects, reads, stores `battery_percent`, and disconnects. It schedules the
next read at 24 hours plus or minus jitter. Firmware is logged but not stored.
`history_backfill` is opt-in and defaults to false; when enabled it reads up to
the configured number of hourly history entries with short GATT sessions. The
collector never clears history from the device.

MQTT and GATT status are independent. Each GATT device has
`target_type=gatt_control` and normalized MAC as `target_key`.

## API And Web

- Metrics UI: `http://localhost:8080/`
- Admin UI: `http://localhost:8080/admin`
- API reference: [docs/api.md](docs/api.md)

Implemented endpoints:

```text
GET /api/health
GET /api/health/details
GET /api/devices
GET /api/devices/{mac}/latest
GET /api/devices/{mac}/series
GET /api/energy/latest
GET /api/energy/series
GET /api/admin/collector-status
GET /api/admin/cisco-spaces-firehose
GET /api/admin/schema
```

Except for `/api/health`, API endpoints require a bearer token or
`X-API-Token` when token enforcement is enabled. The admin page shows collector,
Cisco Spaces Firehose, and schema status only.

## Build And Test

```sh
make test
make build
```

CI runs both commands and lints `docs/openapi.yaml`. The Container workflow
publishes `main`, `sha-<short-sha>`, and release-tag images to
`ghcr.io/hshimomura/home-metrics`.

## Deployment

Production deployment is managed by `ioslab-docs/servicecore` and pins the
published image by digest. The required sequence is:

1. Merge and confirm the home-metrics CI and Container workflows.
2. Resolve the published GHCR digest.
3. Update the servicecore home-metrics digest.
4. Confirm servicecore Docker check and nms4 deploy.
5. Verify the running digest, migrations, collector status, and telemetry.

Example health checks:

```sh
curl -H "Authorization: Bearer $API_TOKEN" \
  https://metrics.ioslab.jp/api/health/details
curl -H "Authorization: Bearer $API_TOKEN" \
  https://metrics.ioslab.jp/api/admin/collector-status
```

See [docs/release.md](docs/release.md) for migration and release rules.
