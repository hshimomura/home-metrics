# Sensor Threshold Alerts

Home Metrics evaluates per-device sensor thresholds on the server. The feature
records current state and firing/resolved transitions. It does not send APNs,
call webhooks, register iOS devices, or manage users.

## Runtime Model

`hm-sensor-alert-worker` evaluates all rules at startup and every
`SENSOR_ALERT_WORKER_INTERVAL` (default `1m`). A PostgreSQL advisory lock keeps
multiple worker containers from evaluating the same rules concurrently.

Rules use inclusive comparisons:

- `above` fires when `value >= trigger_threshold` and resolves when
  `value <= clear_threshold`;
- `below` fires when `value <= trigger_threshold` and resolves when
  `value >= clear_threshold`.

For an `above` rule, the clear threshold must be lower than the trigger. For a
`below` rule, it must be higher. This hysteresis avoids repeated state changes
near the boundary.

`for_duration_seconds` requires fresh, distinct measurements to remain beyond
the trigger for that duration. Re-evaluating the same database row does not
advance the pending state. `max_data_age_seconds` rejects stale measurements;
use a value appropriate for the metric cadence. For example, temperature may
use 600 seconds while a daily GATT battery value may use 108000 seconds.

Missing or stale data does not clear an active alert. The alert remains firing
and exposes `evaluation_status=no_data` or `stale`. Disabling a firing rule or
its device creates a resolved event with `rule_disabled` or `device_disabled`.

## Create A Rule

All rule fields are required. This example fires after temperature remains at
or above 30 C for five minutes and resolves at or below 29 C:

```sh
curl -X POST https://metrics.ioslab.jp/api/sensor-alert-rules \
  -H "Authorization: Bearer $API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Env high temperature",
    "mac": "d3:8d:7f:32:1e:65",
    "metric": "temperature_c",
    "direction": "above",
    "trigger_threshold": 30,
    "clear_threshold": 29,
    "for_duration_seconds": 300,
    "max_data_age_seconds": 600,
    "severity": "warning",
    "enabled": true
  }'
```

Response:

```json
{
  "id": 1,
  "name": "Env high temperature",
  "mac": "d3:8d:7f:32:1e:65",
  "device_label": "Env",
  "metric": "temperature_c",
  "direction": "above",
  "trigger_threshold": 30,
  "clear_threshold": 29,
  "for_duration_seconds": 300,
  "max_data_age_seconds": 600,
  "severity": "warning",
  "enabled": true,
  "created_at": "2026-06-29T12:00:00Z",
  "updated_at": "2026-06-29T12:00:00Z"
}
```

A low soil-moisture rule uses the opposite direction, for example trigger at
15 percent and clear at 18 percent.

## Rule Operations

```text
GET    /api/sensor-alert-rules
POST   /api/sensor-alert-rules
PUT    /api/sensor-alert-rules/{id}
DELETE /api/sensor-alert-rules/{id}
```

`PUT` is a full replacement and requires the same fields as `POST`. Deleting a
rule removes its current state. Existing transition events retain their
snapshot fields and omit `alert_rule_id` after deletion.

## Current State

`GET /api/sensor-alerts` returns evaluated rules. Filter by `normal`, `pending`,
or `firing`:

```sh
curl -H "Authorization: Bearer $API_TOKEN" \
  'https://metrics.ioslab.jp/api/sensor-alerts?status=firing'
```

```json
[
  {
    "rule_id": 1,
    "rule_name": "Env high temperature",
    "mac": "d3:8d:7f:32:1e:65",
    "device_label": "Env",
    "metric": "temperature_c",
    "severity": "warning",
    "direction": "above",
    "trigger_threshold": 30,
    "clear_threshold": 29,
    "status": "firing",
    "evaluation_status": "ok",
    "fired_at": "2026-06-29T12:05:00Z",
    "last_value": 30.8,
    "last_value_at": "2026-06-29T12:05:00Z",
    "last_evaluated_at": "2026-06-29T12:05:20Z"
  }
]
```

## Transition Events

`GET /api/sensor-alert-events` returns newest events first. Optional filters:

- `since`: RFC3339 timestamp;
- `event_type`: `firing` or `resolved`;
- `mac`;
- `limit`: 1-500, default 100.

Events are state transitions, not delivery attempts. There is no `sent` or
notification-device state. `hm-db-maint` retains events for
`DB_MAINT_RETAIN_ALERT_EVENTS` (default `2160h`, 90 days).

## Client Contract

The metrics page polls `status=firing` and displays active alerts. A mobile
client can do the same and use `/api/sensor-alert-events?since=...` to avoid
missing a short firing/resolved cycle between refreshes.

Without APNs, an iOS app cannot receive an immediate alert while suspended or
terminated. Do not restore old APNs entitlements, token registration, or removed
API calls unless background delivery is approved as a separate feature. A
client should also avoid evaluating the same server-managed rule locally,
because two sources of truth can disagree about freshness and hysteresis.
