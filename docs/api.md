# Home Metrics API

`hm-api-server` serves read-only sensor, energy, collector status, and
operational status APIs.
Alarm rules, APNs, notification history, admin webhook delivery, and maintenance
mode APIs are not implemented.

Source layout:

- `server.go`: route registration, auth/CORS, web file serving, JSON helpers.
- `sensors.go`: `/api/devices` endpoints.
- `energy.go`: `/api/energy` endpoints.
- `admin_status.go`: `/api/health/details` and `/api/admin/*` status
  endpoints.

All `/api/*` endpoints except `/api/health` require bearer token or
`X-API-Token` when `API_REQUIRE_TOKEN=true` or `API_TOKEN` is configured.

## Web UI

- `GET /` returns the metrics UI.
- `GET /admin` returns the admin UI. The admin UI shows operational status:
  overall collector summary, Cisco Spaces firehose lock/status, collector status
  rows, and schema migration status.

## Health

### GET /api/health

Returns `{"status":"ok"}` when the database is reachable.

### GET /api/health/details

Returns database status and collector status summary. `stale_collectors` is
counted from `collector_status` when a row has no successful run, has
consecutive failures, or has not updated within `COLLECTOR_STATUS_STALE_AFTER`
(default `5m`). GATT control targets use `GATT_CONTROL_STATUS_STALE_AFTER`
(default `26h`) because they normally run once per day. The optional
`hm-cisco-spaces-collector` row is excluded unless
`CISCO_SPACES_COLLECTOR_ENABLED=true`.

```json
{
  "status": "ok",
  "database": "ok",
  "collector_targets": 4,
  "stale_collectors": 0
}
```

## Sensors

### GET /api/devices

Returns configured sensor devices.

Device responses include explicit metadata fields for stable classification:

- `ingest_source`: telemetry ingest path, for example `cisco_sensor_connect`.
- `sensor_type_code`: concrete sensor model/decoder, for example
  `xiaomi_flower_care`.
- `sensor_type`: optional sensor type details from `sensor_types`.
- `sensor_category`: client-facing category, for example `plant` or
  `environment`.

RoomPlus and Grafana classify plant sensors with `sensor_category == "plant"`.

### GET /api/devices/{mac}/latest

Returns the latest sensor snapshot for one device from `sensor_minute`.
The response keeps the existing `device`, `ts`, and `values` shape. `values`
contains the latest non-null value for each supported metric, so sparse metrics
such as Flower Care `battery_percent` can be shown even when they are measured
less frequently than advertisement metrics. Metrics with no stored value are
returned as `null`.

`value_timestamps` is an optional object keyed by metric name. It contains only
metrics that have a value and records the timestamp of that metric's latest
measurement. The top-level `ts` is the maximum timestamp across
`value_timestamps`, representing the device's latest telemetry time. The API
returns `404` when no metric has any value for the device.

### GET /api/devices/{mac}/series

Query parameters:

- `metric`: one of `temperature_c`, `humidity_percent`, `battery_percent`,
  `rssi_dbm`, `pressure_hpa`, `co2_ppm`, `lux`, `etvoc`,
  `soil_moisture_percent`, `conductivity_us_cm`.
- `range`: one of `1d`, `1w`, `1m`, `3m`, `1y`. Defaults to `1d`.

Series responses remain based on the original measurement timestamps and do not
copy sparse latest values forward.

## Energy

### GET /api/energy/latest

Optional query parameters:

- `source`
- `device_key`

### GET /api/energy/series

Required query parameters:

- `source`
- `device_key`
- `metric`

Optional query parameter:

- `range`: one of `1d`, `1w`, `1m`, `3m`, `1y`. Defaults to `1d`.

## Admin

### GET /api/admin/collector-status

Returns expected rows from `collector_status`. Optional collectors that are
intentionally disabled, such as `hm-cisco-spaces-collector` when
`CISCO_SPACES_COLLECTOR_ENABLED=false`, are omitted from this response in the
same way disabled collector profiles do not appear in the admin collector list.
Cisco Sensor Connect GATT rows use `target_type=gatt_control` and one normalized
MAC address per `target_key`, so one device cannot clear another device's
failure. MQTT and GATT statuses are independent.

### GET /api/admin/cisco-spaces-firehose

Returns Cisco Spaces firehose advisory-lock status and collector status for
`hm-cisco-spaces-collector`. When `CISCO_SPACES_COLLECTOR_ENABLED=false`, the
mode is `disabled`.

### GET /api/admin/schema

Returns applied DB migrations.

## Removed APIs

The following older API families are intentionally unsupported:

- `/api/alert-rules`
- `/api/notification-events`
- `/api/ios/devices`
- `/api/admin/health-alerts`
- `/api/admin/health-notification-events`
- `/api/admin/devices/{mac}/maintenance`

They return the generic unsupported endpoint response when requested.
