# Home Metrics API

`hm-api-server` exposes sensor and energy telemetry, sensor threshold rule
management, current alert state, collector status, and schema data.
`docs/openapi.yaml` is the machine-readable contract; this document explains
runtime semantics that are easier to read in prose.

## Authentication

`GET /api/health` is public. Other `/api/*` endpoints require authentication
when `API_TOKEN` is non-empty. Either header is accepted:

```text
Authorization: Bearer <token>
X-API-Token: <token>
```

`API_REQUIRE_TOKEN=true` requires a non-empty token at server startup.
`API_ALLOWED_ORIGINS` is a comma-separated CORS allow-list; `*` allows every
origin. Web pages are served at `/` and `/admin`.

Errors use this shape:

```json
{"error":"message"}
```

## Health

### `GET /api/health`

Pings PostgreSQL. A reachable database returns:

```json
{"status":"ok"}
```

Database failure returns `503`.

### `GET /api/health/details`

Summarizes expected `collector_status` targets:

```json
{
  "status": "ok",
  "database": "ok",
  "collector_targets": 4,
  "stale_collectors": 0
}
```

A target is stale when it has never succeeded, has consecutive failures, or
has not been updated within `COLLECTOR_STATUS_STALE_AFTER` (default `5m`).
`gatt_control` targets use `GATT_CONTROL_STATUS_STALE_AFTER` (default `26h`).
The Cisco Spaces target is excluded when
`CISCO_SPACES_COLLECTOR_ENABLED=false`.

## Sensor Devices

### `GET /api/devices`

Returns all configured devices, including disabled devices. Each item contains:

- `mac`, `label`, optional `location`, and `enabled`;
- optional `ingest_source`;
- optional `sensor_type_code`;
- optional joined `sensor_type` details;
- optional client-facing `sensor_category`.

Clients must use server-provided metadata rather than labels, MAC prefixes,
collector process names, or recent metric presence:

| Field | Meaning |
| --- | --- |
| `ingest_source` | Telemetry transport and owning collector. |
| `sensor_type_code` | Stable decoder/model identifier. |
| `sensor_type` | Optional display metadata joined from `sensor_types`. |
| `sensor_category` | Client-facing category such as `plant` or `environment`. |
| `enabled` | Whether the owning collector enables the device. |

Normal device lists should include `enabled == true`. Plant views should select
`sensor_category == "plant"`. A latest-reading `404` does not remove a
configured device; clients should keep it visible with a no-data state.

Metric semantics are stable across latest and series responses:

| Key | Meaning | Unit |
| --- | --- | --- |
| `temperature_c` | Temperature | C |
| `humidity_percent` | Air relative humidity | %RH |
| `battery_percent` | Device battery | % |
| `rssi_dbm` | Received signal strength | dBm |
| `pressure_hpa` | Air pressure | hPa |
| `co2_ppm` | Carbon dioxide | ppm |
| `lux` | Illuminance | lux |
| `etvoc` | Equivalent total volatile organic compounds | device-defined |
| `soil_moisture_percent` | Soil moisture | % |
| `conductivity_us_cm` | Soil conductivity | uS/cm |

Soil moisture must not be substituted for air humidity. The stable conductivity
name is `conductivity_us_cm`; user interfaces should label it `Conductivity` or
`導電率`, not `Fertility`.

### `GET /api/devices/{mac}/latest`

Returns one metric-level snapshot. For every supported metric, `values` holds
its newest non-null value or `null` when no value exists. `value_timestamps`
contains only metrics with a value and records each measurement time. Top-level
`ts` is the newest timestamp in that object.

This lets a low-frequency battery reading remain visible without copying it
into newer rows. The endpoint returns `404` when the device does not exist or
when no metric has ever been stored for it.

### `GET /api/devices/{mac}/series`

Required query parameter:

- `metric`: `temperature_c`, `humidity_percent`, `battery_percent`,
  `rssi_dbm`, `pressure_hpa`, `co2_ppm`, `lux`, `etvoc`,
  `soil_moisture_percent`, or `conductivity_us_cm`.

Optional `range` defaults to `1d`:

| Range | Lookback | Bucket | Source table |
| --- | --- | --- | --- |
| `1d` | 1 day | 8 minutes | `sensor_minute` |
| `1w` | 7 days | 1 hour | `sensor_1hour` |
| `1m` | 30 days | 4 hours | `sensor_1hour` |
| `3m` | 90 days | 12 hours | `sensor_12hour` |
| `1y` | 365 days | 1 day | `sensor_1day` |

The response is:

```json
{
  "mac": "5c:85:7e:14:73:7d",
  "metric": "soil_moisture_percent",
  "range": "1d",
  "points": [{"ts":"2026-06-28T03:12:00Z","value":26}]
}
```

Series data follows stored measurement timing. Sparse values are not carried
forward.

For compatibility, clients should treat optional metadata as optional, ignore
unknown future metric keys, use `value_timestamps` when metric age matters, and
avoid all removed classification fields.

## Sensor Threshold Alerts

Alert rules are authenticated deployment-wide resources. They are not attached
to application users or iOS notification devices. The server evaluates each
rule against the latest non-null value of its selected metric.

### `GET /api/sensor-alert-rules`

Returns all rules ordered by ID. Durations are integer seconds. Joined
`device_label` is included for display.

### `POST /api/sensor-alert-rules`

Creates a rule. The JSON body requires:

- `name`, `mac`, `metric`, and `enabled`;
- `direction`: `above` or `below`;
- `trigger_threshold` and `clear_threshold`;
- `for_duration_seconds` from 0 through 2592000;
- `max_data_age_seconds` from 1 through 31536000;
- `severity`: `info`, `warning`, or `critical`.

For `above`, clear must be lower than trigger. For `below`, clear must be
higher. The MAC must already exist in `devices`, and metric must be in the
canonical sensor metric registry. Success returns `201` and the created rule.

### `PUT /api/sensor-alert-rules/{id}`

Fully replaces an existing rule using the same required body as `POST`.

### `DELETE /api/sensor-alert-rules/{id}`

Deletes the rule and current state, returning `204`. Historical transition
events remain as snapshots and omit `alert_rule_id`.

### `GET /api/sensor-alerts`

Returns current state for rules evaluated by the worker. Optional `status` is
`normal`, `pending`, or `firing`. Important fields are:

- `status`: threshold state;
- `evaluation_status`: `ok`, `stale`, `no_data`, or `disabled`;
- `last_value` and `last_value_at`;
- `pending_since`, `fired_at`, and `resolved_at` when applicable.

Stale or missing data does not resolve a firing alert.

### `GET /api/sensor-alert-events`

Returns firing/resolved transitions newest first. Filters are `since` (RFC3339),
`event_type`, `mac`, and `limit` (1-500, default 100). These are evaluation
events, not APNs or webhook delivery records.

See [sensor-alerts.md](sensor-alerts.md) for state-machine semantics, complete
request/response examples, and the mobile-client contract.

## Energy

### `GET /api/energy/latest`

Returns the newest reading for every enabled source/device/metric combination.
Optional filters:

- `source`
- `device_key`

Each item includes `ts`, `source`, `device_key`, optional label/location,
`metric`, `value`, and optional `unit`.

### `GET /api/energy/series`

Required query parameters:

- `source`
- `device_key`
- `metric`

`range` accepts the same values as sensor series. It uses `energy_readings` for
`1d`, `energy_1hour` for `1w` and `1m`, `energy_12hour` for `3m`, and
`energy_1day` for `1y`. An unknown or disabled metric returns `404`.

## Admin

### `GET /api/admin/collector-status`

Returns expected collector targets ordered by collector, type, and key.
Timestamp fields are omitted until they have a value. Intentionally disabled
Cisco Spaces status is filtered out.

Cisco Sensor Connect MQTT and GATT are independent targets. GATT rows use
`target_type=gatt_control` and normalized MAC as `target_key`, so one device
cannot clear another device's error.

### `GET /api/admin/cisco-spaces-firehose`

Returns advisory-lock state, configured mode, collector activity, timestamps,
and failure state for the optional Cisco Spaces Firehose collector. Mode is
`disabled` when `CISCO_SPACES_COLLECTOR_ENABLED=false`.

### `GET /api/admin/schema`

Returns the current migration version and all applied migrations:

```json
{
  "current_version": 1,
  "migrations": [
    {
      "version": 1,
      "name": "initial_schema",
      "checksum": "...",
      "applied_at": "2026-06-28T03:16:43Z"
    }
  ]
}
```

Unregistered `/api/*` routes return `404` with `unsupported endpoint`.
