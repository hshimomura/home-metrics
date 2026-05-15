# バックエンド拡張計画

この文書は、Home Metrics のバックエンドを今後拡張するための計画です。

目的は、データ収集の失敗をログだけでなく DB 上の状態として見えるようにし、
管理者へ通知できるようにし、将来的な管理 API / 管理 UI / 認証強化につなげる
ことです。

## 目的

- グラフが古くなってから気づくのではなく、データ収集異常を早めに検出する。
- collector 停止、対象機器停止、ネットワーク timeout、DB 書き込み失敗を区別する。
- 異常時に管理者へ push 通知する。ただし同じ異常で通知を連発しない。
- 管理者向け通知は Apple Push だけに依存せず、webhook を主経路にできるようにする。
- 将来の管理 UI のために、health 状態を API で取得できるようにする。
- API を外部公開しやすいように、認証と secret 取り扱いを段階的に強化する。

## 現状

- `/api/health` は API と DB ping の軽量チェックだけを行っている。
- 各 collector は失敗をログに出すが、成功・失敗状態を DB には保存していない。
- `sensor_minute` や `energy_readings` の最新時刻から device freshness は推測できる。
  ただし stale data だけでは、なぜ収集が止まったのかは分からない。
- `hm-alert-worker` は既存の閾値 alert を評価し、`notification_events` に通知履歴を
  保存している。
- 既存の alert tables は値の閾値 alert 向けであり、collector 停止や freshness
  異常とは意味が違う。
- `app_users` は通知ユーザー用途であり、認証ユーザーとして使うか、別テーブルを
  作るかはまだ設計判断が必要。

## 設計方針

- UI からではなく、collector の自己申告と DB 状態化から始める。
- 閾値 alert と health alert を同じルール構造に無理に押し込まない。
- 既存の `notification_events` は使える範囲では活用するが、collector health を
  表現するために無理な device 行や fake MAC は作らない。
- persisted health model が固まってから API と UI を広げる。
- health event は配送経路から独立して保存し、webhook / Apple Push などへ fan-out
  できるようにする。
- username/password 認証は後段にし、まず production token の必須化と secret log
  抑止を行う。

## Phase 1: collector_status の追加

最初に `collector_status` テーブルを追加し、各 collector が成功・失敗を upsert
するようにする。

推奨 schema:

```sql
CREATE TABLE IF NOT EXISTS collector_status (
    collector_name text NOT NULL,
    target_type text NOT NULL,
    target_key text NOT NULL DEFAULT 'default',
    last_attempt_at timestamptz,
    last_success_at timestamptz,
    last_data_at timestamptz,
    last_failure_at timestamptz,
    last_error text,
    consecutive_failures integer NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collector_name, target_type, target_key)
);
```

identity key は必ず `(collector_name, target_type, target_key)` とする。
これを明示しないと、実装時に conflict target が曖昧になり、複数 target を扱う
collector で row の上書きや重複が起きやすい。

`target_key` は single-target collector でも必ず入れる。特別な target を持たない
collector は `target_key = 'default'` を使う。

想定する `target_type` 例:

```text
cisco_spaces_firehose
nature_remo_device
apcupsd_server
echonet_device
ble_adapter
```

対象 collector:

- `hm-cisco-spaces-collector`
- `hm-nature-remo-collector`
- `hm-apcupsd-collector`
- `hm-echonet-collector`
- `hm-ble-collector`, deploy で有効化している場合

各 collector が記録するべき状態:

- 収集 loop を試みた時刻
- 収集 loop が成功した時刻
- 利用可能なデータを DB に書けた時刻
- API error、network timeout、target device timeout などの読み取り失敗
- DB 書き込み失敗

意味の切り分け:

- `last_attempt_at`: collector が収集処理を試みた。成功・失敗の両方で更新する。
- `last_success_at`: collector が収集経路を完了した。データが 0 件でも、collector
  自体が動作していることを示す。
- `last_data_at`: collector が利用可能なデータを DB に書けた。
- `last_failure_at` / `last_error`: collector が処理を試みたが失敗した。
- `updated_at` が古い: collector 自体が停止、または stuck している可能性がある。
- `consecutive_failures`: 復旧通知や通知 suppression の判断に使う。

完了条件:

- `db/schema.sql` に `collector_status` が追加されている。
- active production collector が成功・失敗を upsert している。
- long-lived stream collector は、接続維持中に heartbeat で `last_success_at` /
  `updated_at` を更新する。Cisco Spaces では `CISCO_SPACES_STREAM_HEARTBEAT`
  を使い、telemetry 保存時だけ `last_data_at` を更新する。
- container logs を読まずに SQL だけで collector の状態を確認できる。
- この phase では push 通知までは実装しない。

## Phase 2: Health Evaluator の追加

`hm-alert-worker` に health evaluator を追加する。ただし既存の閾値 alert 評価とは
分けて実装する。

health evaluator が見るもの:

- `collector_status.updated_at` による collector の生存状態
- `collector_status.consecutive_failures` と `last_error` による連続失敗
- `collector_status.last_data_at` による「collector は動いているがデータが来ていない」
  状態
- `sensor_minute` による BLE / Cisco Spaces sensor freshness
- `energy_readings` による Nature Remo / ECHONET / APC UPS freshness

判定例:

- Cisco Spaces collector が 5 分以上 `collector_status` を更新していない。
- Cisco Spaces collector は動いているが、特定の configured sensor が 30 分以上
  更新されていない。
- ECHONET collector が対象機器 timeout を連続している。
- APC UPS collector が apcupsd 読み取りに連続失敗している。
- Nature Remo collector が API error、または成功書き込みの stale を起こしている。

注意:

- これらを既存の `alert_rules` に無理に入れない。
- 値の閾値 alert と、状態・freshness alert は semantics が異なる。
- 同じ worker process 内に実装してもよいが、評価ロジックと状態保存は分離する。

## Phase 3: health_alert_state と通知履歴

異常通知は、1 incident につき必要な回数だけ送る。毎分同じ通知を送らないために、
health alert 専用の状態を持つ。

推奨:

- 現在の incident 状態は `health_alert_state` に保存する。
- 通知履歴は `health_notification_events` を新設するか、既存の
  `notification_events` を polymorphic に拡張する。

重要な制約:

- 既存の `notification_events` は `mac text NOT NULL REFERENCES devices(mac)` を
  持つため、そのままでは collector health event を保存できない。
- fake device row や fake MAC を作って health event を押し込まない。
- 既存テーブルを再利用するなら、`mac` nullable 化に加えて
  `event_type`, `source_type`, `source_key` のような polymorphic key が必要。
- 迷う場合は `health_notification_events` を別テーブルにする方が実装リスクは低い。

### Canonical Alert Key

`health_alert_state` の identity key は、曖昧な文字列にしない。

推奨形式:

```text
collector:<collector_name>:<target_type>:<target_key>
device:<mac>
metric:<source>:<device_key>:<metric>
```

例:

```text
collector:hm-echonet-collector:echonet_device:fe00008031323832343134363830000001
collector:hm-apcupsd-collector:apcupsd_server:pve1
device:00:fa:b6:07:de:49
metric:echonet:fe00008031323832343134363830000001:solar_generation_w
metric:apcupsd:pve1:load_percent
```

`collector:hm-echonet-collector` のような粗い key だけでは、複数 target、環境差、
metric ごとの freshness incident を区別できない。

`health_alert_state` が持つべき情報:

- `alert_key`
- `status`: `healthy`, `degraded`, `failing`, `recovered`
- `first_detected_at`
- `last_detected_at`
- `last_notification_at`
- `last_recovery_notification_at`
- `last_message`
- `updated_at`

通知動作:

- 正常から failing に入った時に通知する。
- 同じ incident が継続している間は連続通知しない。
- 長時間継続する場合だけ reminder を送れるようにする。
- 復旧時は必要に応じて recovery 通知を送る。

## Phase 4: Admin Webhook 通知

管理者向けのバックエンド異常通知は、Apple Push だけに寄せない。
Apple Push は利用者端末への通知には向いているが、運用通知の主経路にすると
端末設定、通知許可、集中モード、APNs token 失効、アプリ未起動などに影響される。

推奨方針:

- 管理者向け重大通知は webhook を主経路にする。
- Apple Push は管理者本人への補助的な即時通知、または利用者向け通知に使う。
- health event / notification event は配送先から独立して保存する。
- 1つの incident から複数 channel に fan-out できるようにする。

最初に対応する channel:

```text
generic_webhook
```

将来追加しやすい channel:

```text
slack_webhook
discord_webhook
ntfy
gotify
email
apns
```

### admin_notification_channels

管理者向け通知先を保存する table を追加する。

推奨 schema:

```sql
CREATE TABLE IF NOT EXISTS admin_notification_channels (
    id bigserial PRIMARY KEY,
    channel_type text NOT NULL,
    name text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    target text,
    secret_ref text,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

設計上の注意:

- webhook URL のような secret は、可能なら DB に直書きせず `secret_ref` で外部 secret
  を参照する。
- 最初の実装で secret 管理を簡略化する場合でも、API response や logs に webhook URL
  をそのまま出さない。
- `channel_type = 'generic_webhook'` は、HTTP POST で JSON payload を送るだけの
  最小実装にする。
- Slack / Discord 固有 payload は最初から作り込まず、必要になった channel だけ追加する。

### Webhook Payload

generic webhook の payload は、配送先に依存しない health event として扱う。

例:

```json
{
  "event_type": "health_alert",
  "status": "failing",
  "severity": "warning",
  "alert_key": "metric:echonet:fe00008031323832343134363830000001:solar_generation_w",
  "title": "Solar generation data is stale",
  "message": "No solar_generation_w reading for 30 minutes.",
  "first_detected_at": "2026-05-15T10:30:00Z",
  "last_detected_at": "2026-05-15T11:00:00Z",
  "source": {
    "type": "metric",
    "source": "echonet",
    "device_key": "fe00008031323832343134363830000001",
    "metric": "solar_generation_w"
  }
}
```

最低限含めるもの:

- `event_type`
- `status`
- `severity`
- `alert_key`
- `title`
- `message`
- `first_detected_at`
- `last_detected_at`
- `source`

### Delivery State

health incident の状態と、channel ごとの配送結果は分ける。

`health_alert_state` は incident の現在状態を持つ。

channel ごとの配送履歴は `health_notification_events` に保存する。

追加で持つとよい情報:

```text
channel_id
delivery_status   -- success, failed, skipped
attempted_at
response_code
response_body_preview
error
```

これにより、health incident は発生しているが webhook 配送だけ失敗している、という
状態を区別できる。

### Apple Push との関係

Apple Push は以下の用途に向いている。

- 利用者向けの値異常通知
- 管理者本人への補助的な即時通知
- モバイルアプリ内の状態更新導線

一方で、バックエンド運用通知の主経路は webhook を推奨する。

理由:

- 管理者が複数人になっても扱いやすい。
- 通知履歴、再通知、復旧通知、ack / mute を設計しやすい。
- Slack / Discord / Teams / ntfy / Gotify / email などへ展開しやすい。
- APNs token 失効や端末側通知設定に依存しない。

結論:

```text
主経路: admin webhook
副経路: Apple Push
保存: health event は配送 channel から独立
```

## Phase 5: Health API

`/api/health` は load balancer や簡易監視用の軽量 endpoint として残す。

詳細 endpoint は health model が安定してから追加する。

候補:

```text
GET /api/health/details
GET /api/admin/collector-status
```

`/api/health/details` が返すもの:

- API / DB の基本状態
- collector status summary
- stale device count
- stale energy metric count
- active health incident count

`/api/admin/collector-status` が返すもの:

- collector ごとの target
- last success / last failure
- consecutive failures
- last error
- updated age

管理 UI はこの API が安定してから作る。

## Phase 6: 認証強化

username/password 認証を作る前に、現在の token model を production で安全にする。

最初の hardening mechanism は明示する。

推奨:

```text
API_REQUIRE_TOKEN=true
```

動作:

- `API_REQUIRE_TOKEN=true` かつ `API_TOKEN` が空なら、API server は起動失敗する。
- Docker / systemd の production example では `API_REQUIRE_TOKEN=true` を設定する。
- test / local development では明示的に `API_REQUIRE_TOKEN=false` を許可する。
- logs に full DSN、API token、APNs secrets を出さない。

代替案:

```text
API_ENV=production
```

ただし `API_ENV=production` は意味が広くなりやすい。最初の実装では
`API_REQUIRE_TOKEN=true` の方が責務が明確。

外部公開する場合は、まず外側の認証レイヤも検討する。

- Tailscale
- Cloudflare Access
- reverse proxy basic auth / OIDC

自前 login が必要になったら、先に以下を決める。

- `app_users` を認証ユーザーにも拡張するか。
- 通知ユーザーと認証ユーザーを別テーブルにするか。
- password hash、session、token revoke、refresh token をどう扱うか。

## Phase 7: 管理 UI

管理 UI は、health model と API が安定してから作る。

最初の管理 UI で見るもの:

- collector status
- stale sensors
- stale energy metrics
- recent health incidents
- notification state
- 必要なら acknowledge / mute 操作

保存モデルが固まる前に大きな UI を作ると作り直しが増えるため、最初は API と
簡単な確認画面に留める。

## 最初の実装チケット

```text
Add collector_status reporting for collectors
```

scope:

- `collector_status` schema を追加する。
- `(collector_name, target_type, target_key)` を primary key にする。
- collector status upsert 用の小さな共通 helper を追加する。
- active collectors が成功・失敗を DB に記録する。
- SQL または最小限の `/api/health/details` で状態確認できるようにする。
- この ticket では push 通知は実装しない。

この順序なら、collector failure が container logs だけでなく queryable state になる。
その後の通知、API、管理 UI、認証強化を上に載せやすくなる。

## 次の通知実装チケット

```text
Add generic admin webhook notifications for health alerts
```

scope:

- `admin_notification_channels` schema を追加する。
- `health_notification_events` に channel ごとの配送結果を保存する。
- `generic_webhook` channel に JSON payload を POST する。
- webhook URL / secret は logs と API response に出さない。
- Apple Push はこの ticket では主経路にしない。
