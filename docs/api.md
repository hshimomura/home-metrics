# Home Metrics API Specification

この文書はフロントエンド実装向けの API 仕様です。対象 backend は
`cmd/hm-api-server` です。

## Base URL

公開環境では任意の reverse proxy や load balancer から `hm-api-server` へ転送します。以下はデプロイ先 URL の例です。

```text
https://metrics.example.com
```

同一 host で配信される簡易 UI は `/` から開けます。

## Authentication

`API_TOKEN` が backend に設定されている場合、`/` と `/api/health` 以外の
API には token が必要です。未設定の場合は認証なしです。

推奨 header:

```http
Authorization: Bearer <API_TOKEN>
```

代替 header:

```http
X-API-Token: <API_TOKEN>
```

## Common Response Rules

すべての `/api/*` endpoint は JSON を返します。

日時は Go の `time.Time` を JSON encode した RFC3339/RFC3339Nano 形式です。

エラー時の body:

```json
{
  "error": "message"
}
```

主な status code:

| Status | Meaning |
| ---: | --- |
| 200 | OK |
| 201 | Created |
| 204 | No Content |
| 400 | Bad request / validation error |
| 401 | Unauthorized |
| 404 | Not found |
| 500 | Backend or DB error |
| 503 | DB unavailable |

## Types

### Device

```ts
type Device = {
  mac: string;
  label: string;
  device_type?: string;
  location?: string;
  enabled: boolean;
};
```

### Sensor Metric

BLE sensor series / latest / alert rule で使える metric:

```text
temperature_c
humidity_percent
battery_percent
rssi_dbm
pressure_hpa
co2_ppm
lux
etvoc
```

### Energy Metric

`/api/energy/latest` で返る主な metric:

```text
nature_remo / measured_instantaneous_w
echonet     / solar_generation_w
echonet     / battery_remaining
echonet     / battery_power_w
apcupsd     / input_voltage_v
apcupsd     / load_percent
apcupsd     / battery_charge_percent
apcupsd     / battery_voltage_v
```

`nature_remo`, `echonet`, `apcupsd` の各 collector は任意で稼働させます。

### Alert Operator

```text
>
>=
<
<=
```

### Notification Status

```text
pending
dry_run
sent
failed
skipped
```

APNs 実送信を止めたい環境では、hm-alert-worker を
`ALERT_WORKER_DRY_RUN=true` で動かします。

## Endpoints

### GET /api/health

DB 接続の health check です。認証不要です。

Response `200`:

```json
{
  "status": "ok"
}
```

DB に接続できない場合は `503`。

### GET /api/devices

BLE sensor device 一覧を返します。現在は `enabled=false` も含めて DB 上の
device をすべて返します。

Response `200`:

```json
[
  {
    "mac": "aa:bb:cc:dd:ee:01",
    "label": "Greenhouse",
    "device_type": "Minew",
    "location": "Greenhouse",
    "enabled": true
  }
]
```

### GET /api/devices/{mac}/latest

指定 BLE sensor の最新値を返します。`mac` は lowercase/uppercase どちらでも
受け付けますが、response では DB の値が返ります。

Response `200`:

```json
{
  "device": {
    "mac": "aa:bb:cc:dd:ee:01",
    "label": "Greenhouse",
    "device_type": "Minew",
    "location": "Greenhouse",
    "enabled": true
  },
  "ts": "2026-05-06T08:00:00+09:00",
  "values": {
    "temperature_c": 24.6,
    "humidity_percent": 51,
    "battery_percent": 98,
    "rssi_dbm": -67,
    "pressure_hpa": null,
    "co2_ppm": null,
    "lux": null,
    "etvoc": null
  }
}
```

Errors:

| Status | Error |
| ---: | --- |
| 404 | `device not found` |
| 404 | `sensor value not found` |

### GET /api/devices/{mac}/series

指定 BLE sensor の時系列を返します。

Query:

| Name | Required | Values | Default |
| --- | --- | --- | --- |
| `metric` | yes | Sensor Metric | none |
| `range` | no | `1d`, `1w`, `1m`, `3m`, `1y` | `1d` |

Backend の bucket / source table:

| Range | Bucket | Source |
| --- | --- | --- |
| `1d` | 8 minutes | `sensor_minute` |
| `1w` | 1 hour | `sensor_1hour` |
| `1m` | 4 hours | `sensor_1hour` |
| `3m` | 12 hours | `sensor_12hour` |
| `1y` | 1 day | `sensor_1day` |

Example:

```http
GET /api/devices/aa:bb:cc:dd:ee:01/series?metric=temperature_c&range=1d
```

Response `200`:

```json
{
  "mac": "aa:bb:cc:dd:ee:01",
  "metric": "temperature_c",
  "range": "1d",
  "points": [
    {
      "ts": "2026-05-06T08:00:00+09:00",
      "value": 24.6
    }
  ]
}
```

Errors:

| Status | Error |
| ---: | --- |
| 400 | `unsupported metric` |
| 400 | `unsupported range` |

### GET /api/energy/latest

電力/UPS 系の最新値を返します。

Query:

| Name | Required | Description |
| --- | --- | --- |
| `source` | no | `nature_remo`, `echonet`, `apcupsd` などで絞り込み |
| `device_key` | no | source 内の device key で絞り込み |

Example:

```http
GET /api/energy/latest?source=apcupsd&device_key=ups
```

Response `200`:

```json
[
  {
    "ts": "2026-05-06T08:00:00+09:00",
    "source": "apcupsd",
    "device_key": "ups",
    "label": "APC RS 550S",
    "location": "Home",
    "metric": "input_voltage_v",
    "value": 101,
    "unit": "V"
  }
]
```

### GET /api/energy/series

電力/UPS 系の時系列を返します。広い期間では `energy_readings` raw ではなく
TimescaleDB の continuous aggregate を参照します。

Query:

| Name | Required | Description |
| --- | --- | --- |
| `source` | yes | `nature_remo`, `echonet`, `apcupsd` など |
| `device_key` | yes | source 内の device key |
| `metric` | yes | Energy Metric |
| `range` | no | `1d`, `1w`, `1m`, `3m`, `1y`。default `1d` |

Backend の bucket / source:

| Range | Bucket | Source |
| --- | --- | --- |
| `1d` | 8 minutes | `energy_readings` |
| `1w` | 1 hour | `energy_1hour` |
| `1m` | 4 hours | `energy_1hour` |
| `3m` | 12 hours | `energy_12hour` |
| `1y` | 1 day | `energy_1day` |

Example:

```http
GET /api/energy/series?source=apcupsd&device_key=ups&metric=load_percent&range=1m
```

Response `200`:

```json
{
  "source": "apcupsd",
  "device_key": "ups",
  "metric": "load_percent",
  "range": "1m",
  "unit": "%",
  "points": [
    {
      "ts": "2026-05-06T00:00:00+09:00",
      "value": 45.2
    }
  ]
}
```

Errors:

| Status | Error |
| ---: | --- |
| 400 | `source, device_key, and metric are required` |
| 400 | `unsupported range` |
| 404 | `energy metric not found` |
| 404 | `unsupported endpoint` |

### GET /api/alert-rules

しきい値通知 rule 一覧を返します。現状は single user `user_id=1` 固定です。

Response `200`:

```json
[
  {
    "id": 1,
    "user_id": 1,
    "mac": "aa:bb:cc:dd:ee:01",
    "metric": "temperature_c",
    "operator": ">",
    "threshold": 35,
    "cooldown_seconds": 86400,
    "enabled": true,
    "last_notified_at": "2026-05-06T08:00:00+09:00",
    "last_value": 36.1,
    "created_at": "2026-05-06T07:00:00+09:00",
    "updated_at": "2026-05-06T07:00:00+09:00"
  }
]
```

`last_notified_at` と `last_value` は未評価時には省略されます。

### POST /api/alert-rules

しきい値通知 rule を作成します。

Request:

```ts
type AlertRuleRequest = {
  mac: string;
  metric: SensorMetric;
  operator: ">" | ">=" | "<" | "<=";
  threshold: number;
  cooldown_seconds?: number;
  enabled?: boolean;
};
```

Defaults:

| Field | Default |
| --- | --- |
| `cooldown_seconds` | `86400` |
| `enabled` | `true` |

Example:

```json
{
  "mac": "aa:bb:cc:dd:ee:01",
  "metric": "temperature_c",
  "operator": ">",
  "threshold": 35,
  "cooldown_seconds": 86400,
  "enabled": true
}
```

Response `201`: `AlertRule`

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid json` |
| 400 | `mac, metric, operator, and threshold are required` |
| 400 | `unsupported metric` |
| 400 | `unsupported operator` |
| 400 | `cooldown_seconds must be non-negative` |
| 400 | `create alert rule` |

### PUT /api/alert-rules/{id}

しきい値通知 rule を更新します。部分更新ではなく full replace です。

Request は `POST /api/alert-rules` と同じです。

Response `200`: `AlertRule`

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid id` |
| 400 | `invalid json` |
| 400 | validation errors |
| 404 | `alert rule not found` |

### DELETE /api/alert-rules/{id}

しきい値通知 rule を削除します。

Response `204`: body なし

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid id` |
| 404 | `alert rule not found` |

### POST /api/alert-rules/{id}/reset-cooldown

指定 rule の cooldown 状態をリセットします。`alert_rule_state.last_notified_at` を
`NULL` にするため、現在しきい値を超えていれば次回の alert worker 評価で再通知できます。

Response `204`: body なし

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid id` |
| 404 | `alert rule not found` |
| 500 | `query alert rule` |
| 500 | `reset alert rule cooldown` |

### DELETE /api/notification-events

現在の user の notification event 履歴をすべて削除します。

Response `204`: body なし

Errors:

| Status | Error |
| ---: | --- |
| 500 | `delete notification events` |

### GET /api/notification-events

しきい値判定で作られた通知 event 履歴を返します。

Query:

| Name | Required | Description |
| --- | --- | --- |
| `limit` | no | 1-500。default `50` |
| `status` | no | Notification Status |
| `mac` | no | BLE sensor MAC |
| `alert_rule_id` | no | alert rule id |

Response `200`:

```json
[
  {
    "id": 10,
    "alert_rule_id": 1,
    "user_id": 1,
    "mac": "aa:bb:cc:dd:ee:01",
    "metric": "temperature_c",
    "value": 36.1,
    "threshold": 35,
    "triggered_at": "2026-05-06T08:00:00+09:00",
    "sent_at": "2026-05-06T08:00:01+09:00",
    "status": "dry_run",
    "error_message": "optional message",
    "created_at": "2026-05-06T08:00:01+09:00"
  }
]
```

Nullable fields may be omitted: `alert_rule_id`, `user_id`, `value`, `threshold`,
`sent_at`, `error_message`.

Errors:

| Status | Error |
| ---: | --- |
| 400 | `limit must be between 1 and 500` |
| 400 | `unsupported status` |
| 400 | `invalid alert_rule_id` |

### GET /api/ios/devices

iOS push token 登録済み device 一覧を返します。現状は single user `user_id=1`
固定です。

Response `200`:

```json
[
  {
    "id": 1,
    "user_id": 1,
    "apns_device_token": "abcdef...",
    "app_bundle_id": "org.example.home-metrics",
    "apns_environment": "sandbox",
    "device_name": "iPhone",
    "enabled": true,
    "last_seen_at": "2026-05-06T08:00:00+09:00",
    "created_at": "2026-05-06T07:00:00+09:00",
    "updated_at": "2026-05-06T08:00:00+09:00"
  }
]
```

`device_name`, `disabled_reason`, `disabled_at`, `last_seen_at` は無い場合に
省略されます。APNs が `BadDeviceToken`, `Unregistered`, `410 Gone` を返した
場合は backend が device を `enabled=false` にし、`disabled_reason` と
`disabled_at` を保存します。

### POST /api/ios/devices

iOS device token を登録します。同じ
`apns_device_token + app_bundle_id + apns_environment` は upsert されます。

Request:

```ts
type IOSDeviceRequest = {
  apns_device_token: string;
  app_bundle_id: string;
  apns_environment: "sandbox" | "production";
  device_name?: string | null;
  enabled?: boolean;
};
```

Default:

| Field | Default |
| --- | --- |
| `enabled` | `true` |

Example:

```json
{
  "apns_device_token": "abcdef...",
  "app_bundle_id": "org.example.home-metrics",
  "apns_environment": "sandbox",
  "device_name": "iPhone",
  "enabled": true
}
```

Response `201`: `IOSDevice`

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid json` |
| 400 | `apns_device_token, app_bundle_id, and apns_environment are required` |
| 400 | `apns_environment must be sandbox or production` |
| 400 | `register ios device` |

### PUT /api/ios/devices/{id}

iOS device token 登録を更新します。部分更新ではなく full replace です。

Request は `POST /api/ios/devices` と同じです。

Response `200`: `IOSDevice`

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid id` |
| 400 | validation errors |
| 404 | `ios device not found` |

### DELETE /api/ios/devices/{id}

iOS device token 登録を削除します。

Response `204`: body なし

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid id` |
| 404 | `ios device not found` |

### POST /api/ios/devices/{id}/test-notification

指定した iOS device に APNs test notification を送信します。
test payload はデフォルトでは最新の `notification_events` を元に作ります。
特定の event を使いたい環境では、サーバ側で
`APNS_TEST_NOTIFICATION_EVENT_CREATED_AT` に RFC3339 timestamp を設定します。

サーバ側の `APNS_BUNDLE_ID` に一致し、かつ `enabled=true` の device のみ
送信対象です。APNs endpoint は device の `apns_environment` に応じて
`sandbox` / `production` を自動で切り替えます。APNs が
`BadDeviceToken` や `Unregistered` を返した場合は、その device を
`enabled=false` にし、`disabled_reason` と `disabled_at` を保存します。

Response `200`:

```json
{
  "id": 1,
  "device_name": "iPhone",
  "app_bundle_id": "org.example.home-metrics",
  "apns_environment": "sandbox",
  "notification_event_id": 42,
  "notification_event_created_at": "2026-01-02T03:04:05Z",
  "status": "sent",
  "sent_at": "2026-01-02T03:05:00Z"
}
```

Errors:

| Status | Error |
| ---: | --- |
| 400 | `invalid id` |
| 400 | `ios device is disabled` |
| 400 | `ios device does not match APNs bundle` |
| 404 | `ios device not found` |
| 404 | `test notification event not found` |
| 500 | `query test notification event` |
| 502 | APNs error |
| 503 | `APNs test sender is not configured` |

## Frontend Notes

- `GET /api/devices` は今の実装では disabled device も返すため、UI 側では
  `enabled === true` のみ表示するのが安全です。
- `Sensor1`, `Sensor2`, `Sensor3` は現在未使用扱いで DB から削除済みです。
- `PUT` endpoint は partial update ではなく full replace です。
- `cooldown_seconds=0` は許可されますが、連続通知になりやすいため UI では
  最小値を設けることを推奨します。
- APNs 本番送信前でも、hm-alert-worker dry-run により `notification_events` で
  「通知されるはずだった event」を確認できます。
- CORS は `API_ALLOWED_ORIGINS` に完全一致する origin のみ許可されます。
  `*` を設定した場合は任意 origin を許可します。
