# home-metrics

Home Metrics collects environmental, plant, energy, and UPS measurements into
PostgreSQL/TimescaleDB. It provides a read-only REST API, a metrics page, and an
admin page for collector and schema status.

The service stores normalized readings rather than raw Cisco MQTT or Firehose
payloads. Alert rules, APNs, webhooks, notification history, and device
maintenance controls are outside the current scope.

## Components

```text
cmd/hm-api-server/                     REST API and web pages
cmd/hm-db-migrate/                     schema migration CLI
cmd/hm-db-maint/                       rollups and retention
cmd/hm-db-check/                       database consistency checks
cmd/hm-ble-collector/                  local BlueZ BLE collector
cmd/hm-cisco-iot-orchestrator-collector/
                                       Cisco Sensor Connect MQTT and GATT
cmd/hm-cisco-spaces-collector/         optional Cisco Spaces Firehose
cmd/hm-nature-remo-collector/          Nature Remo power
cmd/hm-echonet-collector/              ECHONET Lite energy
cmd/hm-apcupsd-collector/              APC UPS metrics
cmd/hm-energy-influx-import/           historical energy import
internal/sensor/                       canonical sensor metric model
internal/sensorstore/                  ownership and sparse DB writes
db/schema.sql                          fresh-install schema
db/migrations/                         existing-database migrations
web/                                   metrics and admin pages
```

## Documentation

- [Architecture and data model](docs/architecture.md)
- [REST API and client contract](docs/api.md)
- [OpenAPI contract](docs/openapi.yaml)
- [Release and deployment](docs/release.md)
- [Xiaomi Flower Care integration](docs/xiaomi-flower-care-cisco-sensor-connect.md)

These are the only active design and operation documents. Completed plans and
downstream RoomPlus/Grafana artifacts are intentionally not duplicated here.

## Configuration

Start from:

- `examples/home-metrics.compose.env.example` for Docker Compose;
- `examples/home-metrics.env.example` for direct commands or systemd;
- `examples/sensors.json.example` for sensor ownership and decoder metadata.

Every sensor should have one explicit `ingest_source`. Docker Compose maps
`BLE_INGEST_SOURCE`, `CISCO_SPACES_INGEST_SOURCE`, and
`CISCO_IOT_ORCH_INGEST_SOURCE` to the command-level
`SENSOR_INGEST_SOURCE`. Cisco Sensor Connect stores its ownership as
`cisco_sensor_connect`.

Common API and health settings:

```sh
API_TOKEN=change-me
API_REQUIRE_TOKEN=true
API_ALLOWED_ORIGINS=https://example.invalid
COLLECTOR_STATUS_STALE_AFTER=5m
GATT_CONTROL_STATUS_STALE_AFTER=26h
CISCO_SPACES_COLLECTOR_ENABLED=false
```

Keep `CISCO_SPACES_COLLECTOR_ENABLED=false` while that optional profile is
stopped so historical status rows do not degrade health.

## Docker Compose

Start the core services:

```sh
docker compose up -d db hm-db-migrate hm-db-maint hm-api-server
```

Start the collectors required by the deployment:

```sh
docker compose --profile cisco-iot up -d hm-cisco-iot-orchestrator-collector
docker compose --profile nature-remo up -d hm-nature-remo-collector
docker compose --profile apcupsd up -d hm-apcupsd-collector
docker compose --profile echonet up -d hm-echonet-collector
```

Alternative sensor paths are separate profiles:

```sh
docker compose --profile ble up -d hm-ble-collector
docker compose --profile cisco-spaces up -d hm-cisco-spaces-collector
```

## Cisco Sensor Connect

Use separate onboarding, control, and data application IDs and API keys. The
required orchestrator-side order is:

1. SCIM onboard each BLE device.
2. Register the data application.
3. Register the BLE topic.
4. Subscribe each device to the topic.
5. Start the collector and verify MQTT status and minute telemetry.

The MQTT collector decodes protobuf messages in memory and writes sparse,
minute-level median readings. Optional Xiaomi Flower Care GATT polling reads
battery at a low frequency and can perform bounded, read-only history backfill.
See the dedicated [Flower Care document](docs/xiaomi-flower-care-cisco-sensor-connect.md)
for configuration, FE95 objects, UUIDs, and status behavior.

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
`X-API-Token` when a token is configured.

## Build And Test

```sh
make test
make build
```

CI runs both commands and lints `docs/openapi.yaml`. The Container workflow
publishes `main`, `sha-<short-sha>`, and release-tag images to GHCR.

## Deployment

Production deployment is managed by `ioslab-docs/servicecore` and pins the
published image by digest. After changing `main`:

1. Confirm the home-metrics CI and Container workflows.
2. Update the servicecore image digest.
3. Confirm servicecore Docker check and nms4 deploy.
4. Verify the running digest, migrations, health, and recent telemetry.

See [docs/release.md](docs/release.md) for the complete release, migration,
rollback, and nms4 verification procedure.
