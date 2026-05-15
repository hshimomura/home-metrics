# 管理者向け Webhook 実装計画

この文書は、Home Metrics の backend health / collector health 通知を
`webhook-relay` へ送るための実装計画です。

目的は、データ収集停止や freshness 異常を Apple Push だけに依存せず、管理者向け
webhook 経由で通知できるようにすることです。

## 前提

Webhook receiver は nms4 上の `webhook-relay` service です。

```text
Service: webhook-relay
Host: nms4
Container: webhook-relay-web
Health check: http://127.0.0.1:8097/healthz
Endpoint: POST /webhooks/home-metrics
Auth: Authorization: Bearer <HOME_METRICS_WEBHOOK_TOKEN>
Content-Type: application/json
```

home-metrics container から使う URL は、Docker network 経由の名前解決を前提にします。

```text
WEBHOOK_RELAY_URL=http://webhook-relay-web:8097/webhooks/home-metrics
WEBHOOK_RELAY_TOKEN=<HOME_METRICS_WEBHOOK_TOKEN>
```

`webhook-relay` は host の `127.0.0.1:8097` に bind しているため、home-metrics
container から `127.0.0.1:8097` に投げてはいけません。container 自身の loopback を
見に行ってしまいます。

推奨構成は、`webhook-relay-web` を `home-metrics_default` Docker network に参加させ、
home-metrics から `http://webhook-relay-web:8097/webhooks/home-metrics` へ POST する形です。

Token は Git に保存しません。nms4 上では以下の値を
`/srv/docker/home-metrics/.env` の `WEBHOOK_RELAY_TOKEN` に設定します。

```sh
sudo awk -F= '$1=="HOME_METRICS_WEBHOOK_TOKEN"{print $2}' /srv/docker/webhook-relay/.env
```

## Payload Contract

最初の payload は `webhook-relay` 側の `home-metrics` provider に合わせます。

```json
{
  "event_id": "hm-20260515-001",
  "status": "firing",
  "severity": "warning",
  "title": "Room temperature threshold exceeded",
  "source": "living-room",
  "summary": "Temperature is 29.8C for 10 minutes.",
  "labels": {
    "sensor": "nature-remo",
    "metric": "temperature"
  },
  "url": "https://home-metrics.example/events/hm-20260515-001"
}
```

必須フィールド:

- `event_id`: Home Metrics 側で一意な通知イベント ID。
- `status`: `info`, `firing`, `resolved` のいずれかから開始する。
- `severity`: `info`, `warning`, `critical` のいずれかから開始する。
- `title`: 通知タイトル。
- `source`: collector / device / metric などの発生源。
- `summary`: 管理者が次の行動を判断できる短い説明。

任意フィールド:

- `labels`: 機械処理しやすい key-value。
- `url`: 管理 UI や event detail へのリンク。最初は空でもよい。

## 実装方針

`hm-alert-worker` に health evaluator と webhook notifier を追加します。

既存の閾値 alert は APNs / `notification_events` を使っていますが、collector health
は性質が違います。既存の `alert_rules` に無理に押し込まず、health alert 専用の状態と
配送履歴を持たせます。

### DB

`collector_status` はすでに collector の自己申告用として追加済みです。

次に追加する候補:

```sql
CREATE TABLE IF NOT EXISTS health_alert_state (
    alert_key text PRIMARY KEY,
    status text NOT NULL,
    severity text NOT NULL,
    title text NOT NULL,
    source text NOT NULL,
    summary text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    first_fired_at timestamptz,
    last_evaluated_at timestamptz NOT NULL,
    last_notified_at timestamptz,
    resolved_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS health_notification_events (
    id bigserial PRIMARY KEY,
    event_id text NOT NULL UNIQUE,
    alert_key text NOT NULL REFERENCES health_alert_state(alert_key) ON DELETE CASCADE,
    channel_id bigint,
    channel_type text NOT NULL,
    status text NOT NULL,
    http_status integer,
    response_body text,
    error text,
    created_at timestamptz NOT NULL DEFAULT now()
);
```

`WEBHOOK_RELAY_URL` と `Authorization` header の token は保存しません。配送履歴には
HTTP status、短い response body、error だけを残します。

### Alert Key

`alert_key` は曖昧な文字列にしません。

例:

```text
collector:hm-cisco-spaces-collector:cisco_spaces_firehose:default
collector:hm-echonet-collector:echonet_device:echonet-device
metric:sensor_minute:00:fa:b6:07:de:49:temperature_c
metric:energy_readings:nature-remo:remo-e:grid_power_w
```

実装では `:` を含む MAC address などは key segment 内で `_` に置き換えます。

## Phase 1: Webhook Client

`internal/adminwebhook` のような package を作り、HTTP POST を担当させます。

設定:

```text
WEBHOOK_RELAY_URL
WEBHOOK_RELAY_TOKEN
WEBHOOK_RELAY_TIMEOUT=10s
```

挙動:

- `WEBHOOK_RELAY_URL` が空なら webhook notifier は disabled。
- `WEBHOOK_RELAY_TOKEN` が空で webhook enabled なら起動時に error。
- `Authorization: Bearer <token>` を付ける。
- `Content-Type: application/json` を付ける。
- 2xx は成功。
- 非 2xx / timeout / JSON encode error は失敗として返す。
- logs に token を出さない。

テスト:

- payload JSON が contract に合う。
- Authorization header が付く。
- token は log / error 文字列に含まれない。
- 2xx / 4xx / 5xx / timeout の扱い。

## Phase 2: Health Evaluator

`hm-alert-worker` に health evaluator を追加します。

見るもの:

- `collector_status.updated_at`: collector process / stream liveness。
- `collector_status.last_data_at`: collector は動いているがデータが来ていない状態。
- `collector_status.consecutive_failures`: 連続失敗。
- `sensor_minute` / `energy_readings`: device / metric freshness。

最初の判定例:

- Cisco Spaces collector の `updated_at` が 5 分以上古い。
- Cisco Spaces collector の `updated_at` は新しいが `last_data_at` が 30 分以上古い。
- Nature Remo / ECHONET / apcupsd collector が連続失敗している。
- 特定 sensor / energy device の最新時刻が閾値より古い。

## Phase 3: 通知 Suppression と Resolve

同じ incident を毎分送らないようにします。

- `firing` 初回は即通知。
- 同じ `alert_key` の継続中は cooldown 中なら通知しない。
- severity が上がった場合は cooldown 中でも通知する。
- 復旧したら `resolved` を 1 回通知する。
- `health_alert_state.last_notified_at` を更新する。

## Phase 4: API / UI

最初は API だけ薄く出します。

候補:

```text
GET /api/admin/collector-status
GET /api/admin/health-alerts
GET /api/admin/health-notification-events
POST /api/admin/health-alerts/{alert_key}/test-webhook
```

これらは認証強化後に公開するのが安全です。少なくとも production では
`API_REQUIRE_TOKEN=true` と `API_TOKEN` 必須化を先に入れます。

## Phase 5: 設定と compose

`hm-alert-worker` には health evaluator と webhook 用の環境変数を渡します。

```text
HEALTH_EVALUATOR_ENABLED
HEALTH_WEBHOOK_DRY_RUN
HEALTH_COLLECTOR_STALE_AFTER
HEALTH_DATA_STALE_AFTER
HEALTH_SENSOR_STALE_AFTER
HEALTH_ENERGY_STALE_AFTER
HEALTH_NOTIFICATION_COOLDOWN
HOME_METRICS_BASE_URL
WEBHOOK_RELAY_URL
WEBHOOK_RELAY_TOKEN
WEBHOOK_RELAY_TIMEOUT
```

`hm-api-server` は admin API の test webhook 用に `WEBHOOK_RELAY_*` を参照します。

## Phase 6: Auth Hardening

production では `API_REQUIRE_TOKEN=true` を設定します。

- `API_REQUIRE_TOKEN=true` かつ `API_TOKEN` が空なら API server は起動失敗する。
- `/` と `/api/health` は引き続き認証不要。
- `/api/health/details` と `/api/admin/*` は `API_TOKEN` が設定されていれば認証必須。
- DB DSN、webhook URL、token は log / API response に出さない。

## nms4 Deploy 前提

servicecore 側で必要な設定:

- `webhook-relay-web` を `home-metrics_default` network に参加させる。
- `hm-alert-worker` に `WEBHOOK_RELAY_URL` と `WEBHOOK_RELAY_TOKEN` を渡す。
- `/srv/docker/home-metrics/.env` に token を置く。
- `/srv/docker/webhook-relay/.env` の `HOME_METRICS_WEBHOOK_TOKEN` と一致させる。

疎通確認:

```sh
curl -X POST "$WEBHOOK_RELAY_URL" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $WEBHOOK_RELAY_TOKEN" \
  --data '{"event_id":"hm-test-1","status":"info","severity":"info","title":"test","source":"home-metrics","summary":"webhook test"}'
```

container から確認する場合は、`hm-alert-worker` と同じ network namespace / env に近い
形で実行します。host の `127.0.0.1:8097` に投げる確認だけでは、container からの到達性を
確認したことになりません。

## 完了条件

- webhook client の unit test がある。
- health evaluator の主要判定に unit test がある。
- dry-run で DB に `health_notification_events.status = 'dry_run'` を残せる。
- nms4 container から `webhook-relay-web:8097` へ疎通できる。
- token が Git / logs / API response に出ない。
- GitHub Actions の `make test` / `make build` が成功する。
