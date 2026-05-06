# Client Development Handoff

この文書は iOS app / frontend 側の開発者へ渡すための実装分担メモです。
API の詳細な request / response は [api.md](api.md) を参照してください。

## Goal

BLE sensor、電力、UPS の値をクライアントで確認できるようにし、
ユーザーが設定したしきい値を超えたときに Apple Push Notification service
経由で iOS app へ user-visible push notification を送る。

APNs は backend 側の実送信、sandbox/production routing、device token 登録 API
まで実装済みです。Apple Developer account や bundle ID などの実値は
public repository に含めず、各 deployment の `/etc/home-metrics/home-metrics.env`
で管理します。

## Current Stage

現在の段階は「API 接続、値の表示、しきい値 rule 管理、通知履歴確認、
APNs device token 登録、sandbox/production push test」までを
クライアント開発できる状態です。

| Area | Status | Notes |
| --- | --- | --- |
| PostgreSQL / TimescaleDB | Implemented | TimescaleDB の metrics database に保存 |
| BLE sensor collection | Implemented | `Sensor1`, `Sensor2`, `Sensor3` は現在未使用 |
| Energy collection | Implemented | `echonet`, `apcupsd`, `nature_remo` は稼働中 |
| REST API | Implemented | `cmd/hm-api-server`。仕様は `docs/api.md` |
| Alert rule DB/API | Implemented | single user `user_id=1` 前提 |
| Alert worker | Implemented | `ALERT_WORKER_DRY_RUN` で dry-run / APNs 実送信を切替 |
| APNs sender | Implemented | device の `apns_environment` ごとに sandbox / production endpoint へ送信 |
| iOS skeleton | Minimal | API 接続、device/rule/event 表示のたたき台あり |
| iOS push permission/token registration | Client responsibility | `/api/ios/devices` に token と environment を登録 |
| Alert rule editing UI | Not implemented | API は実装済み |
| Web graph UI | Implemented | `web/index.html` で Sensor/Energy の 1d 詳細グラフを表示 |
| iOS graph UI | Not implemented | BLE/Energy series API は実装済み |

## Architecture

```text
BLE / energy collectors
  -> PostgreSQL / TimescaleDB
  -> hm-api-server
       -> iOS app / frontend

hm-alert-worker
  -> alert_rules を評価
  -> alert_rule_state を更新
  -> notification_events を作成
  -> APNs 送信 または dry-run 記録

iOS app
  -> センサー値表示
  -> しきい値 rule 登録/編集
  -> APNs device token 登録
  -> notification history 表示
  -> push 通知受信
```

## Server Responsibilities

サーバ側で持つ機能:

- センサー/電力/UPS データを収集して DB に保存する。
- 表示用 API を提供する。
  - device 一覧
  - latest values
  - time series
  - energy latest
- しきい値 rule を永続化する。
  - 対象 sensor
  - metric
  - operator
  - threshold
  - cooldown
  - enabled
- iOS device token を永続化する。
  - APNs device token
  - bundle ID
  - sandbox / production
  - enabled
- hm-alert-worker でしきい値を評価する。
- cooldown を適用して通知頻度を制御する。
- 通知結果を `notification_events` に記録する。
- APNs provider として push を送信する。
- APNs から invalid token 応答が返った場合、該当 token を disabled にする。
- API token による簡易認証を提供する。

サーバ側では持たない方針:

- iOS 画面上の単位変換や表示文言の細かい出し分け。
- ユーザーに push permission を促す UI。
- iOS device token の取得処理。
- client local settings の保存。
- graph の表示状態、選択中 range、UI sort/filter 状態。

## Client Responsibilities

クライアント側で持つ機能:

- API base URL と API token を設定/保存する。
- API へ `Authorization: Bearer <token>` を付けてアクセスする。
- BLE sensor 一覧を表示する。
- latest values を表示する。
- time series を取得して graph 表示する。
- energy latest を表示する。
- energy series を取得して graph 表示する。
- alert rule 一覧を表示する。
- alert rule の作成/更新/削除 UI を提供する。
- notification event 履歴を表示する。
- iOS の notification permission を要求する。
- APNs device token を取得する。
- APNs device token を backend の `/api/ios/devices` に登録/更新する。
- token 更新時、再インストール時、端末変更時に backend 登録を更新する。
- push notification を受信したとき、該当 rule / sensor / metric へ遷移する。
- backend が dry-run の間は `notification_events` を使って通知予定を確認する。
- Xcode Debug build は `apns_environment=sandbox`、TestFlight/App Store build は
  `apns_environment=production` として登録する。

クライアント側では持たない方針:

- しきい値判定の最終責任。
- cooldown 判定。
- push 送信判断。
- APNs provider 送信。
- センサー生データの長期保存。

## Development Phases

### Phase 1: Read-only App

目的: APNs なしで、データ閲覧アプリとして成立させる。

Client tasks:

- API base URL / token 入力を実装する。
- `GET /api/health` で接続確認する。
- `GET /api/devices` を表示する。
- `GET /api/devices/{mac}/latest` を表示する。
- `GET /api/energy/latest` を表示する。
- `GET /api/energy/series` を使い、energy graph を表示する。
- error / loading / empty state を実装する。

Server status:

- 実装済み。

### Phase 2: Graph

目的: センサーごとの時系列を確認できるようにする。

Client tasks:

- `GET /api/devices/{mac}/series?metric=...&range=...` を呼ぶ。
- range selector を実装する。
  - `1d`
  - `1w`
  - `1m`
  - `3m`
  - `1y`
- metric selector を実装する。
- 欠損値や空配列を扱う。

Server status:

- series API 実装済み。
- 1 年表示は `sensor_1day` を 1 day bucket で返す。
- energy series API は広い期間で `energy_1hour`, `energy_12hour`,
  `energy_1day` continuous aggregate を使う。
- energy series API は BLE series と同じ `range + points` 形式に加えて、
  `unit` を必ず返す。

### Phase 3: Alert Rule UI

目的: しきい値 rule をアプリから管理できるようにする。

Client tasks:

- `GET /api/alert-rules` を表示する。
- `POST /api/alert-rules` で rule を作成する。
- `PUT /api/alert-rules/{id}` で rule を更新する。
- `DELETE /api/alert-rules/{id}` で rule を削除する。
- metric / operator / threshold / cooldown / enabled の入力 UI を作る。
- `PUT` は partial update ではなく full replace として扱う。
- cooldown は秒で API に送るが、UI では分/時間/日で入力してよい。

Server status:

- API 実装済み。
- hm-alert-worker 実装済み。dry-run / APNs 実送信は deployment の
  `ALERT_WORKER_DRY_RUN` で切り替える。

### Phase 4: Dry-run Notification Verification

目的: APNs 実送信なしで、通知判定が期待通りか検証する。

Client tasks:

- `GET /api/notification-events` を表示する。
- `status=dry_run` を「通知予定」として UI 表示する。
- rule detail から関連 event を filter する。
- alert rule 作成後、実際に event が出るか確認できる導線を作る。

Server status:

- dry-run event 記録は実装済み。
- APNs 実送信を止めたい環境では hm-alert-worker.service を
  `ALERT_WORKER_DRY_RUN=true` で稼働させる。

### Phase 5: APNs Device Registration

目的: push 送信できる前段として、端末 token と APNs environment を
backend に登録する。

Client tasks:

- app 起動時または設定画面で notification permission を要求する。
- APNs device token を取得する。
- token を hex string 等、backend と合意した形式で送る。
- `/api/ios/devices` に token を登録する。
- token 更新時に再登録する。
- user が通知を無効化する UI を用意する場合は、該当 `ios_device.enabled=false`
  へ更新する。

Server status:

- `/api/ios/devices` API は実装済み。
- APNs credential は repository には含めない。各 deployment の
  `/etc/home-metrics/home-metrics.env` に設定する。

### Phase 6: APNs Production/Sandbox Send

目的: 実際に iOS へ push notification を送る。

Client tasks:

- Bundle ID を backend 設定と一致させる。
- sandbox build では `apns_environment=sandbox` を登録する。
- production/TestFlight/App Store build では `production` を登録する。
- push 受信時の画面遷移を実装する。

Server tasks:

- APNs Auth Key `.p8` を安全な場所に配置する。
- `APNS_KEY_FILE`, `APNS_KEY_ID`, `APNS_TEAM_ID`, `APNS_BUNDLE_ID` を設定する。
- `ALERT_WORKER_DRY_RUN=false` に切り替える。
- sandbox で実機通知を確認する。
- sandbox と production は device 登録の `apns_environment` に応じて
  backend が APNs endpoint を切り替える。

Server status:

- APNs sender code は実装済み。
- sandbox / production endpoint は device 登録ごとに自動選択する。
- `APNS_ENVIRONMENT` は backend では使わない。

## Current Backend Facts For Client

Base URL:

```text
https://metrics.example.com
```

Primary docs:

- `docs/api.md`
- `docs/client-development-handoff.md`

Current active services:

- `hm-api-server.service`
- `hm-ble-collector.service`
- `hm-alert-worker.service`
- `hm-echonet-collector.service`
- `hm-apcupsd-collector.service`
- `hm-nature-remo-collector.service`
- `hm-db-maint.timer`

Current push mode:

```text
ALERT_WORKER_DRY_RUN=true
```

Current user model:

```text
single user: user_id = 1
```

Currently unused BLE sensors:

```text
Sensor1
Sensor2
Sensor3
```

## Data And UI Notes

Device list:

- `GET /api/devices` は DB 上の device を返す。
- UI は `enabled=true` のみ表示する方針が安全。
- 現在未使用の `Sensor1`, `Sensor2`, `Sensor3` は DB から削除済み。

Metric display:

- API は machine-readable metric name を返す。
- 表示名、単位、並び順は client 側で持ってよい。
- energy API は `unit` を返すため、energy 画面ではそれを優先する。

Date/time:

- API の日時は RFC3339/RFC3339Nano。
- UI 表示は端末 locale/timezone に合わせてよい。
- server は JST 環境だが、client は API の offset 付き日時を parse する。

Error handling:

- backend は `{"error":"message"}` を返す。
- token 不一致は `401 unauthorized`。
- delete 成功は `204` で body なし。

## Items To Confirm Now

この段階で曖昧なまま進めると手戻りになりやすい事項です。

### App Identity

- Bundle ID は何にするか。
  - README 例: `org.example.home-metrics`
- App 表示名は何にするか。
- sandbox / production / TestFlight の扱いをどう分けるか。

### Network And Security

- iOS app から API base URL は何にするか。
  - 公開入口例: `https://metrics.example.com`
- HTTPS 化するか。
  - nms の nginx で HTTPS 終端済み。
  - backend は nginx などの reverse proxy から hm-api-server へ転送する。
- `API_TOKEN` を app に埋め込むのか、ユーザー入力/設定配布にするのか。
- API token を複数ユーザー/複数端末で分ける必要があるか。

### User Model

- 当面 single user `user_id=1` のままでよいか。
- 将来的に複数ユーザー、家族共有、端末ごとの権限を持たせるか。
- alert rule は全端末共通か、ユーザーごとか。

### Alert Semantics

- 条件が継続している場合、cooldown 後に再通知してよいか。
  - 現在の backend は「再通知する」設計。
- recovery notification が必要か。
  - 例: 温度が正常値に戻ったら通知。
- hysteresis が必要か。
  - 例: 35 C 超えで通知、33 C 未満に戻るまで再通知しない。
- metric ごとの推奨 threshold / 単位 / 入力範囲を決めるか。
- cooldown の最小値/最大値を UI で制限するか。

### Push UX

- push の title/body 文言を誰が管理するか。
  - 現在は server 側で固定文言を生成。
- push tap 時にどの画面へ遷移するか。
  - sensor detail
  - alert rule detail
  - notification event detail
- 通知を一時停止する UI が必要か。
- quiet hours / do-not-disturb 的な時間帯制御が必要か。

### Client UX Scope

- 初期版で graph まで入れるか、latest view 優先にするか。
- energy view を BLE sensors と同じ階層にするか、別 tab にするか。
- alert rule 作成 UI は sensor detail から作るか、専用 rule list から作るか。
- disabled device / 未使用 device を UI に出すか。

### APNs Operational Decisions

- APNs Auth Key `.p8` は repository 外に置く。
- sandbox と production の hm-alert-worker は分けない。device 登録の
  `apns_environment` で backend が endpoint を切り替える。
- Bundle ID と `APNS_BUNDLE_ID` は deployment ごとの実値として
  `/etc/home-metrics/home-metrics.env` に置く。
- invalid token の扱い。
  - backend は invalid token を disabled にする実装。
  - client 側で再登録導線が必要か確認する。

### Data Scope

- Nature Remo は `NATURE_REMO_TOKEN` 設定済みで稼働中。
- APC UPS の metric を alert 対象に含めるか。
  - 現在の alert rule API は BLE sensor metrics のみ対応。
- energy metrics にも threshold alert が必要か。
  - 必要なら server API/schema/worker の拡張が必要。

## Recommended Immediate Client Work

1. `docs/api.md` を元に API client を整える。
2. API base URL / token 設定画面を作る。
3. `GET /api/health` 接続確認を作る。
4. device latest と energy latest / series の read-only UI を作る。
5. alert rule list / create / edit / delete を作る。
6. notification event list を作り、dry-run で動作確認する。
7. APNs permission/token registration を接続し、Debug は sandbox、
   TestFlight/App Store は production として登録する。

この順序なら push 受信前でも、API と通知履歴の大半を先に固められる。
