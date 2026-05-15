# Backend Health Monitoring 運用設計

この文書は、Home Metrics の backend health monitoring / admin webhook の
運用設計です。

以前の「バックエンド拡張計画」は実装済みになったため、この文書では
現在の実装、運用時に見る場所、設定値、残っている改善項目だけを扱います。

## 現在の到達点

実装済み:

- collector が `collector_status` に成功・失敗・データ書き込み状態を自己申告する。
- `hm-alert-worker` が collector / sensor / energy freshness を評価する。
- health incident は `health_alert_state` に保存する。
- admin webhook の配送履歴は `health_notification_events` に保存する。
- 管理 API と管理 UI を提供する。
- `hm-db-migrate` が `db/migrations/` を適用し、`schema_migrations` で checksum を管理する。
- Docker Compose では API / collector / alert worker / maintenance が `hm-db-migrate`
  成功後に起動する。
- 管理者向け通知は webhook を主経路にする。Apple Push はこの系統では主経路にしない。

主な entrypoint:

```text
GET /api/health
GET /api/health/details
GET /api/admin/collector-status
GET /api/admin/health-alerts
GET /api/admin/health-notification-events
POST /api/admin/health-alerts/{alert_key}/test-webhook
GET /admin
```

## DB モデル

### collector_status

collector ごとの liveness と data freshness を保存する。

```sql
CREATE TABLE IF NOT EXISTS collector_status (
    collector_name text NOT NULL,
    target_type text NOT NULL,
    target_key text NOT NULL DEFAULT 'default',
    last_attempt_at timestamptz,
    last_success_at timestamptz,
    last_data_at timestamptz,
    first_failure_at timestamptz,
    last_failure_at timestamptz,
    last_error text,
    consecutive_failures integer NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collector_name, target_type, target_key)
);
```

identity key は必ず `(collector_name, target_type, target_key)` とする。
single-target collector でも `target_key = 'default'` などの明示 key を使う。

各 timestamp の意味:

- `last_attempt_at`: collector が収集処理を試みた。成功・失敗の両方で更新する。
- `last_success_at`: collector が収集経路を完了した。データが 0 件でも collector
  自体が動作していることを示す。
- `last_data_at`: collector が利用可能なデータを DB に書けた。
- `first_failure_at`: 連続失敗が始まった時刻。一度も成功していない collector が
  失敗し続ける場合の時間判定に使う。成功時に clear する。
- `last_failure_at` / `last_error`: 直近の失敗時刻と理由。
- `updated_at`: `collector_status` row が最後に更新された時刻。
- `consecutive_failures`: alert trigger ではなく、payload / summary の context。

主な `target_type`:

```text
cisco_spaces_firehose
nature_remo_device
apcupsd_server
echonet_device
ble_adapter
```

### health_alert_state

現在の health incident 状態を保存する。値閾値 alert の `alert_rules` とは分ける。

主な用途:

- active incident の確認
- 通知 cooldown / recovery 通知の判断
- `/api/health/details` の summary
- `/admin` の Health Alerts 表示

alert key は具体的にする。

```text
collector:<collector_name>:<target_type>:<target_key>
device:<mac>
metric:<source>:<device_key>:<metric>
```

粗い key、例えば `collector:hm-echonet-collector` だけにすると、複数 target や
metric ごとの freshness incident を区別できない。

### health_notification_events

health alert の webhook 配送結果を保存する。

保存するもの:

- `event_id`
- `alert_key`
- `channel_type`
- `status`: `pending`, `dry_run`, `sent`, `failed`, `skipped`
- HTTP status / response preview / error
- `created_at`

既存の `notification_events` は `mac text NOT NULL REFERENCES devices(mac)` を持つため、
collector health event はここへ無理に入れない。fake device row や fake MAC は作らない。

### admin_notification_channels

将来の webhook channel 管理用 table。`health_notification_events.channel_id` から
参照できる。

現時点の production 運用では、generic webhook の送信先は主に env で設定する。

```text
WEBHOOK_RELAY_URL
WEBHOOK_RELAY_TOKEN
WEBHOOK_RELAY_TIMEOUT
```

`admin_notification_channels` は Slack / Discord / ntfy / Gotify / email などへ
channel を増やす時の保存先として残している。

### schema_migrations

既存 PostgreSQL volume では `/docker-entrypoint-initdb.d/10-schema.sql` は再実行されない。
そのため、既存 DB の schema 更新は `hm-db-migrate` と `db/migrations/` で行う。

運用ルール:

- `db/schema.sql` は新規 DB 初期化用の full schema。
- `db/migrations/` は既存 DB 更新用。
- 一度適用した migration file は checksum 対象なので変更しない。
- 既存 migration を直す代わりに、新しい migration を追加する。
- checksum が変わった適用済み migration は `hm-db-migrate` が失敗させる。

## Collector Health 判定

collector health は、次の 3 種類を分けて判定する。

| 判定 | 主に見る値 | 意味 | 通知 |
| --- | --- | --- | --- |
| heartbeat stale | `updated_at`, `last_success_at`, `first_failure_at` | collector process / stream / DB reporting が止まった、または一度も成功していない | する |
| data stale | `last_data_at` | collector は動いているが有効データが記録されていない | する |
| failure context | `consecutive_failures`, `last_error`, `last_failure_at` | 直近の失敗理由 | 単独ではしない |

重要:

- `consecutive_failures` は alert trigger ではない。
- `consecutive_failures > 0` だけでは firing にしない。
- 失敗回数と `last_error` は webhook payload / summary の補助情報として使う。
- severity は欠損時間で決める。

lab 用の初期値:

| collector | heartbeat warning | heartbeat critical | data warning | data critical |
| --- | ---: | ---: | ---: | ---: |
| default | 5m | 15m | 15m | 30m |
| `hm-echonet-collector` | 5m | 15m | 15m | 30m |
| `hm-cisco-spaces-collector` | 5m | 15m | 15m | 30m |
| `hm-nature-remo-collector` | 5m | 15m | 15m | 30m |
| `hm-apcupsd-collector` | 5m | 15m | 15m | 30m |

境界は「以上」で判定する。例えば data warning `15m` なら、ちょうど 15 分で
warning firing になる。

## 設定

health evaluator / webhook:

```text
HEALTH_EVALUATOR_ENABLED=true
HEALTH_WEBHOOK_DRY_RUN=false
HEALTH_NOTIFICATION_COOLDOWN=1h
HOME_METRICS_BASE_URL=
WEBHOOK_RELAY_URL=http://webhook-relay-web:8097/webhooks/home-metrics
WEBHOOK_RELAY_TOKEN=<secret>
WEBHOOK_RELAY_TIMEOUT=10s
```

collector health thresholds:

```text
HEALTH_COLLECTOR_HEARTBEAT_WARNING_AFTER=5m
HEALTH_COLLECTOR_HEARTBEAT_CRITICAL_AFTER=15m
HEALTH_COLLECTOR_DATA_WARNING_AFTER=15m
HEALTH_COLLECTOR_DATA_CRITICAL_AFTER=30m

HEALTH_ECHONET_DATA_WARNING_AFTER=15m
HEALTH_ECHONET_DATA_CRITICAL_AFTER=30m
HEALTH_CISCO_SPACES_HEARTBEAT_WARNING_AFTER=5m
HEALTH_CISCO_SPACES_HEARTBEAT_CRITICAL_AFTER=15m
HEALTH_CISCO_SPACES_DATA_WARNING_AFTER=15m
HEALTH_CISCO_SPACES_DATA_CRITICAL_AFTER=30m
HEALTH_NATURE_REMO_DATA_WARNING_AFTER=15m
HEALTH_NATURE_REMO_DATA_CRITICAL_AFTER=30m
HEALTH_APCUPSD_DATA_WARNING_AFTER=15m
HEALTH_APCUPSD_DATA_CRITICAL_AFTER=30m
```

後方互換:

- `HEALTH_COLLECTOR_STALE_AFTER` は `HEALTH_COLLECTOR_HEARTBEAT_WARNING_AFTER`
  として扱う。
- `HEALTH_DATA_STALE_AFTER` は `HEALTH_COLLECTOR_DATA_WARNING_AFTER` として扱う。
- 明示 critical threshold がない場合、heartbeat は warning threshold の 3 倍、
  data は warning threshold の 2 倍を使う。

Cisco Spaces:

```text
CISCO_SPACES_STREAM_HEARTBEAT=60s
CISCO_SPACES_ALLOW_SECONDARY=false
```

`CISCO_SPACES_STREAM_HEARTBEAT` は Firehose 接続中の collector liveness を
`collector_status.last_success_at` / `updated_at` に記録する間隔。
telemetry 保存時だけ `last_data_at` を更新するため、「stream は生きているが
データが来ていない」を分けて見られる。

## Admin Webhook

管理者向け backend health 通知は webhook を主経路にする。

Apple Push は利用者端末向け通知や補助的な即時通知には使えるが、backend 運用通知の
主経路にしない。理由は、通知許可、集中モード、APNs token 失効、アプリ未起動などの
端末状態に依存するため。

nms4 の webhook receiver:

```text
Service: webhook-relay
Host: nms4
Container: webhook-relay-web
Health check: http://127.0.0.1:8097/healthz
Endpoint: POST /webhooks/home-metrics
Auth: Authorization: Bearer <HOME_METRICS_WEBHOOK_TOKEN>
Content-Type: application/json
```

home-metrics container から使う URL:

```text
WEBHOOK_RELAY_URL=http://webhook-relay-web:8097/webhooks/home-metrics
```

注意:

- home-metrics container から `127.0.0.1:8097` に投げない。container 自身の
  loopback を見に行ってしまう。
- `webhook-relay-web` は `home-metrics_default` Docker network へ参加させる。
- token は Git に保存しない。nms4 の `.env` で管理する。
- `WEBHOOK_RELAY_URL` は API response に出さない。配送結果だけを保存する。

payload は home-metrics 側で enrichment する。webhook-relay 側だけで生成しない。
home-metrics は DB の domain metadata と health state を持っているため、対象名、
impact metric、timestamps、suggested actions、admin deep link を組み立てられる。

含める主な情報:

- event id / status / severity / title / summary
- source / labels
- impacted metrics
- `last_attempt_at`, `last_success_at`, `last_data_at`, `first_failure_at`,
  `last_failure_at`, `updated_at`
- age-like context は summary 文に含める。明示 field として `last_data_age` などを
  返す contract にはまだしていない。
- `consecutive_failures`, `last_error`
- suggested actions
- `/admin#alert=...` deep link

## Admin API / UI

HTML route の `/` と `/admin` は静的に配信される。画面内で呼び出す `/api/admin/*` は
token 認証対象。

管理 UI:

```text
/admin
```

見るもの:

- overall health summary
- collector status
- active / resolved health alerts
- webhook delivery events
- test webhook

collector status では以下を見る。

```text
collector_name
target_type
target_key
last_attempt_at
last_success_at
last_data_at
first_failure_at
last_failure_at
consecutive_failures
last_error
updated_at
```

## Migration 運用

Docker Compose:

- `hm-db-migrate` は one-shot service。
- `db` が healthy になった後に実行される。
- API / collector / alert worker / maintenance は `hm-db-migrate` 成功後に起動する。
- deploy 後は `docker compose ps -a hm-db-migrate` で exit code を確認する。

systemd / non-Docker:

```sh
sudo env DB_MIGRATIONS_DIR=/usr/local/share/home-metrics/migrations hm-db-migrate
```

その後に API / collector / alert worker を起動する。

確認 SQL:

```sql
select version, applied_at, checksum from schema_migrations order by version;
```

## 運用確認

collector status:

```sql
select
  collector_name,
  target_type,
  target_key,
  last_success_at,
  last_data_at,
  first_failure_at,
  last_failure_at,
  consecutive_failures,
  last_error
from collector_status
order by collector_name, target_type, target_key;
```

active health alerts:

```sql
select status, severity, title, source, updated_at
from health_alert_state
where status = 'firing'
order by updated_at desc;
```

webhook delivery failures:

```sql
select event_id, alert_key, status, http_status, error, created_at
from health_notification_events
where status = 'failed'
order by created_at desc
limit 20;
```

API:

```sh
curl -H "Authorization: Bearer $API_TOKEN" http://localhost:8080/api/health/details
curl -H "Authorization: Bearer $API_TOKEN" http://localhost:8080/api/admin/collector-status
```

## Cisco Spaces Firehose

Cisco Spaces Firehose は同時接続が複数あると event が分散する可能性がある。
通常運用では production `hm-cisco-spaces-collector` を1つだけ動かす。

`hm-cisco-spaces-collector` は DB advisory lock を使う single-run guard を持つ。
起動時に `pg_try_advisory_lock` を取得し、取得できない場合は secondary 起動として
終了する。`CISCO_SPACES_ALLOW_SECONDARY=true` の場合だけ secondary 起動を許可する。

diagnostic run を行う場合:

```sh
docker compose --profile cisco-spaces stop hm-cisco-spaces-collector
timeout 240s docker compose --profile cisco-spaces run --rm \
  -e CISCO_SPACES_DRY_RUN=true \
  -e CISCO_SPACES_DEBUG=true \
  hm-cisco-spaces-collector
docker compose --profile cisco-spaces up -d hm-cisco-spaces-collector
```

advisory lock は二重起動を防ぐが、既存 production collector を自動停止するものではない。
diagnostic run では上のように production collector を明示的に止める。

## 残っている改善項目

短期:

- `/admin` で Cisco Spaces Firehose lock / secondary 起動状態を見えるようにする。
- `/admin` から ack / mute / manual resolve を扱う。
- `/api/admin/schema` などで current schema version を返す。

中期:

- admin webhook channel の複数化。
- Slack / Discord / ntfy / Gotify / email などの channel type 追加。
- 管理 UI の filter / search / deep link 強化。

長期:

- username/password または外部認証基盤。
- reverse proxy / Tailscale / Cloudflare Access との統合。
- Apple Push を backend health の副経路として fan-out するか再検討。
