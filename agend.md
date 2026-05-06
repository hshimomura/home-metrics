# Cisco Spaces Firehose Development Notes

## Goal

Add a network-only sensor ingestion path for Cisco Spaces Indoor IoT Firehose.
The operator should be able to choose Cisco Spaces instead of local BLE scanning
from `/etc/home-metrics/home-metrics.env`.

The new path should write into the existing PostgreSQL / TimescaleDB sensor
schema used by `hm-ble-collector`:

- `devices`
- `sensor_minute`
- existing rollups maintained by `hm-db-maint`
- existing API / alert-worker behavior

## Branches And Environments

- Development branch on pve2: `feat/cisco-spaces-firehose`
- Repository path on pve2: `/home/hshimomu/home-metrics`
- Docker test host: `192.168.64.18`
- Docker test path: `/home/hshimomu/docker/home-metrics`
- Current baseline: `main` at `6a4cd5b`

## Reference Implementation

Reference source:
`https://github.com/hshimomura/scripts/blob/main/ciscospaces.py`

Important behavior to carry over:

- Firehose endpoint default:
  `https://partners.dnaspaces.io/api/partners/v1/firehose/events`
- Authentication header:
  `X-API-Key: <api key>`
- Event filter:
  only process `eventType == "IOT_TELEMETRY"`
- Device identity:
  use `iotTelemetry.deviceInfo.deviceMacAddress`
- Timestamp:
  use `recordTimestamp` in milliseconds
- Reconnect:
  streaming HTTP reconnect with exponential backoff
- Smoothing:
  5-sample median per device and metric
- Upload cadence:
  write at most once per device per minute
- Freshness:
  keep a recently seen metric only if it is fresh enough, default 60 seconds
- Sentinel filtering:
  ignore known invalid/sentinel sensor values

The Python script writes to InfluxDB. Home Metrics should instead write directly
to PostgreSQL using the existing schema.

## Firehose Payload Mapping

Reference event shape:

```text
eventType
recordTimestamp
iotTelemetry.deviceInfo.deviceMacAddress
iotTelemetry.deviceInfo.label
iotTelemetry.temperature.temperatureInCelsius
iotTelemetry.humidity.humidityInPercentage
iotTelemetry.airPressure.pressure
iotTelemetry.carbonEmissions.co2Ppm
iotTelemetry.illuminance.value
iotTelemetry.illuminance.unit
iotTelemetry.battery.value
iotTelemetry.tvoc.valueInPpb
```

Map into `sensor_minute`:

```text
deviceMacAddress                  -> devices.mac / sensor_minute.mac
deviceInfo.label                  -> devices.label
recordTimestamp truncated minute  -> sensor_minute.ts
temperatureInCelsius              -> temperature_c
humidityInPercentage              -> humidity_percent
battery.value                     -> battery_percent
airPressure.pressure              -> pressure_hpa
carbonEmissions.co2Ppm            -> co2_ppm
illuminance.value when unit=LUX    -> lux
tvoc.valueInPpb                   -> etvoc
```

No Firehose RSSI field is identified in the reference script, so
`sensor_minute.rssi_dbm` should remain null.

## Sentinel And Filtering Rules

Carry over these filters from the reference implementation:

- temperature: ignore `0`
- humidity: ignore `255`
- air pressure: ignore `0` and missing values
- illuminance: require `unit == "LUX"` and ignore `65535`
- device identity: skip event if normalized `deviceId` and
  `deviceMacAddress` both look like MAC addresses but differ

Battery handling needs a product decision. The reference script only accepts
battery values for a hard-coded allowlist of MAC addresses. For Home Metrics,
prefer a configurable policy:

```text
CISCO_SPACES_BATTERY_MODE=all|allowlist|off
CISCO_SPACES_BATTERY_ALLOWLIST=00:fa:b6:07:de:49,00:fa:b6:07:de:4b
```

Default proposal: `all`, unless field quality proves noisy in production.

## Configuration Proposal

Add to `/etc/home-metrics/home-metrics.env`:

```text
SENSOR_INGEST_SOURCE=ble

CISCO_SPACES_API_KEY=
CISCO_SPACES_FIREHOSE_URL=https://partners.dnaspaces.io/api/partners/v1/firehose/events
CISCO_SPACES_RECONNECT_MIN_DELAY=1s
CISCO_SPACES_RECONNECT_MAX_DELAY=60s
CISCO_SPACES_SAMPLE_WINDOW=5
CISCO_SPACES_FIELD_FRESHNESS=60s
CISCO_SPACES_UPLOAD_INTERVAL=60s
CISCO_SPACES_BATTERY_MODE=all
CISCO_SPACES_BATTERY_ALLOWLIST=
CISCO_SPACES_DRY_RUN=false
```

Input-source behavior:

- `SENSOR_INGEST_SOURCE=ble`: run/use `hm-ble-collector`
- `SENSOR_INGEST_SOURCE=cisco_spaces`: run/use `hm-cisco-spaces-collector`
- Docker Compose should expose both as profile services, while documentation
  should explain that only one sensor ingestion source should normally be
  enabled at a time.

Question to resolve during implementation:

- Should `hm-ble-collector` itself read `SENSOR_INGEST_SOURCE` and exit when not
  selected, or should selection be purely service-level? Initial preference:
  service-level selection for Docker/systemd, because it avoids surprising
  silent exits.

## New Binary

Add a new Go command:

```text
cmd/hm-cisco-spaces-collector/
```

Responsibilities:

- Stream Firehose using `net/http`
- Decode newline-delimited JSON events from the response body
- Reconnect with bounded exponential backoff
- Maintain per-device, per-metric median windows
- Upsert devices into `devices`
- Upsert minute rows into `sensor_minute`
- Log skipped malformed events at debug level or low-noise info level
- Support `CISCO_SPACES_RUN_ONCE` or a testable internal processor for tests

Implementation should reuse current project dependencies where possible. No new
runtime dependency is required beyond the Go standard library and `pgx`.

## PostgreSQL Write Shape

For each minute/device payload, write:

```sql
INSERT INTO sensor_minute (
  ts, mac, temperature_c, humidity_percent, battery_percent,
  pressure_hpa, co2_ppm, lux, etvoc
)
VALUES (...)
ON CONFLICT (ts, mac) DO UPDATE SET ...
```

Only update a metric column when the Cisco Spaces payload has a fresh value.
Avoid overwriting a previously stored value with null when a later Firehose event
for the same minute contains only a subset of metrics.

Implementation options:

1. Build dynamic SQL for only present fields.
2. Use `COALESCE(EXCLUDED.metric, sensor_minute.metric)` in the conflict update.

Initial preference: use `COALESCE` to keep the write path simple and safe.

## Files To Change

Expected files:

```text
Makefile
tools/build.sh
cmd/hm-cisco-spaces-collector/main.go
cmd/hm-cisco-spaces-collector/main_test.go
examples/home-metrics.env.example
examples/home-metrics.compose.env.example
deploy/hm-cisco-spaces-collector.service
compose.yaml
README.md
```

Optional if the setting becomes more complex:

```text
examples/sensors.json.example
docs/
```

## Docker Test Plan

On `192.168.64.18`:

1. Sync branch into `/home/hshimomu/docker/home-metrics`.
2. Rebuild image:

   ```bash
   docker compose build hm-api-server
   ```

3. Start DB/API/maintenance:

   ```bash
   docker compose up -d db hm-api-server hm-db-maint
   ```

4. Test parser without real Cisco Spaces credentials using unit tests.
5. If an API key is available, set `CISCO_SPACES_API_KEY` in `.env` and run:

   ```bash
   docker compose --profile cisco-spaces up -d hm-cisco-spaces-collector
   ```

6. Verify:

   ```bash
   docker compose logs --tail=100 hm-cisco-spaces-collector
   docker compose exec -T db psql -U home_metrics -d ble_sensors \
     -c "select * from sensor_minute order by ts desc limit 10;"
   curl -H "Authorization: Bearer change-me" \
     http://127.0.0.1:8080/api/devices
   ```

## Open Questions

- What is the real Cisco Spaces API key variable name we want to standardize on:
  `CISCO_SPACES_API_KEY` or legacy-compatible `DNASPACES_API_KEY`?
- Should the collector accept all Firehose devices, or only devices listed in
  `/etc/home-metrics/sensors.json`?
- Should Cisco Spaces labels overwrite existing `devices.label` every time, or
  only fill missing/default labels?
- Do we need disk-backed retry queue for PostgreSQL writes, or is reconnect plus
  logging enough because TimescaleDB is local in the same Compose stack?
- Should battery default to `all`, `allowlist`, or `off`?

## Initial Decisions

- Use `CISCO_SPACES_API_KEY` as the primary variable.
- Optionally accept `DNASPACES_API_KEY` as a backward-compatible fallback.
- Write to existing `sensor_minute`; do not add a parallel table.
- Keep rollups/API/alert-worker unchanged.
- Add Cisco Spaces as a separate collector binary and service.
- Treat BLE and Cisco Spaces as mutually exclusive operational choices, selected
  by which service/profile is enabled.
