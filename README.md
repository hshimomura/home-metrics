# Home Metrics

Home Metrics は、家庭や小規模ラボ向けのセンサー・電力・UPS メトリクスを PostgreSQL / TimescaleDB に保存し、API/Web UI/通知基盤から参照するためのツール群です。BLE collector、Cisco Spaces Indoor IoT Firehose collector、ECHONET Lite、Nature Remo、apcupsd collector を含みます。


## Directory Layout

```text
cmd/hm-ble-collector/   BlueZ D-Bus で BLE を読み、DB に 1 分値を書き込む Go daemon
cmd/hm-cisco-spaces-collector/ Cisco Spaces Firehose で Indoor IoT telemetry を読み、DB に 1 分値を書き込む Go daemon
cmd/hm-alert-worker/    alert_rules を評価し、通知イベントを記録する Go worker
cmd/hm-api-server/      iOS app / external app 向け REST API
cmd/hm-db-maint/        rollup 更新と retention を行う DB maintenance CLI
cmd/hm-nature-remo-collector/ Nature Remo E のスマートメーター値を DB に書き込む Go daemon
cmd/hm-echonet-collector/ ECHONET Lite から EIBS7 の電力系値を DB に書き込む Go daemon
cmd/hm-db-check/        PostgreSQL の最新値を簡易確認する対話 CLI
db/                     PostgreSQL / TimescaleDB schema
deploy/                 systemd unit
tools/                  調査用 Python scripts
web/                    API server から配信する簡易Web UI
```

生成物とローカル cache は Git 管理外です。

```text
.cache/
hm-ble-collector
hm-db-check
hm-alert-worker
hm-api-server
hm-db-maint
hm-cisco-spaces-collector
hm-nature-remo-collector
hm-echonet-collector
bin/
build/
dist/
```

## Release / Deployment

GitHub を canonical repository とし、Docker image は public GHCR image として publish します。
本番 deploy は `ioslab-docs/servicecore` 側で image digest を pin して行います。

詳細は [docs/release.md](docs/release.md) を参照してください。

## Database

DB 名は `ble_sensors` を想定しています。

```bash
psql -d ble_sensors -f db/schema.sql
```

主要テーブル:

```text
devices
  mac          primary key
  label        表示名。Cisco Spaces 側設定により変わり得る
  sensor_category
  location
  enabled

sensor_minute
sensor_1hour
sensor_12hour
sensor_1day
  ts
  mac
  temperature_c
  humidity_percent
  battery_percent
  rssi_dbm
  pressure_hpa
  co2_ppm
  lux
  etvoc
  inserted_at / updated_at

app_users
ios_devices
alert_rules
alert_rule_state
notification_events
  iOS push 通知に向けたユーザー、APNs token、閾値ルール、cooldown 状態、通知履歴

energy_devices
energy_readings
  Nature Remo E / ECHONET 由来の電力系時系列データ
```

`sensor_minute` は TimescaleDB hypertable です。primary key は `(ts, mac)` です。

TimescaleDB は TSL 機能付きの公式 packagecloud 版を使います。Debian 標準の
`postgresql-17-timescaledb` は Apache-only build で
`timescaledb-tsl-*.so` を含まないため、`timescaledb.license = 'timescale'`
や TimescaleDB の TSL 機能が必要な運用では使いません。TSL 機能が必要な環境では
Timescale 公式 packagecloud repository 由来の
`timescaledb-2-postgresql-17` を導入します。

## Build

この環境では通常の Go cache が read-only に当たる場合があるため、プロジェクト配下の `.cache` を使っています。

```bash
make
```

バイナリは `build/` 配下へ出力します。運用時は `make install` で
`/usr/local/bin/<binary>` に配置し、systemd unit は `/usr/local/bin` 側を実行します。

```bash
sudo make install
```

## Configuration

Runtime configuration is kept outside the repository under `/etc/home-metrics/`.

```bash
sudo install -d -m 0755 /etc/home-metrics
sudo install -m 0600 examples/home-metrics.env.example /etc/home-metrics/home-metrics.env
sudo install -m 0644 examples/sensors.json.example /etc/home-metrics/sensors.json
```

Edit `/etc/home-metrics/home-metrics.env` for host-specific settings such as `BLE_DB_DSN`, API token, sensor ingest source, Cisco Spaces API key, APC UPS address, ECHONET target, Nature Remo token, APNs settings, intervals, and retention windows.

Edit `/etc/home-metrics/sensors.json` for BLE sensor MAC addresses, labels, device type, location, and enabled/disabled state. `db/schema.sql` intentionally does not seed personal sensor or energy device rows; use `examples/seed.example.sql` as a starting point for local metadata.

`make install` installs migration files under `/usr/local/share/home-metrics/migrations` by default.
For non-Docker deployments, run `hm-db-migrate` before starting the API, workers, or collectors and set
`DB_MIGRATIONS_DIR=/usr/local/share/home-metrics/migrations` if the default working-directory relative
`db/migrations` path is not available.

## Docker Compose

Docker Compose で PostgreSQL / TimescaleDB と Home Metrics の各 daemon をまとめて起動できます。

```bash
cp examples/home-metrics.compose.env.example .env
docker compose up -d --build hm-api-server hm-alert-worker hm-db-maint
```

DB は `timescale/timescaledb:latest-pg17` を使い、初回起動時に `db/schema.sql` と
`db/energy_optimization.sql` を読み込みます。永続データは `pgdata` volume に保存します。
既存 volume では `/docker-entrypoint-initdb.d/` は再実行されないため、通常起動時は
one-shot の `hm-db-migrate` が `db/migrations/` の未適用 migration を適用します。
適用済み migration は `schema_migrations` に version と checksum を保存します。

DB は Compose network 内だけで公開し、collector は `db:5432` へ接続します。
Cisco Spaces Firehose、Nature Remo、apcupsd、ECHONET Lite、BLE collector は
通常の Compose network 上で動作します。BLE collector は network ではなく
ホストの BlueZ D-Bus へアクセスするため、`/var/run/dbus` を mount します。

```bash
docker compose --profile cisco-spaces --profile echonet up -d --build
```

Cisco Spaces Firehose を使う場合は `.env` に `CISCO_SPACES_API_KEY` を設定し、BLE ではなく `cisco-spaces` profile を有効にします。

```bash
docker compose --profile cisco-spaces up -d --build hm-cisco-spaces-collector
```

BLE collector は `/var/run/dbus` を mount し、`BLE_SENSORS_FILE` として
`SENSORS_FILE` の JSON を `/etc/home-metrics/sensors.json` に mount します。
ECHONET Lite は `ECHONET_TARGET_IP` を設定すると multicast discovery を使わず直接対象へ送信します。

## BLE Collector

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
BLE_ADAPTER=/org/bluez/hci0 \
BLE_POLL_INTERVAL=2s \
hm-ble-collector
```

動作:

```text
BlueZ D-Bus discovery
  -> 対象 MAC の ServiceData を取得
  -> FE6A / FFE1 / FEAA payload を decode
  -> センサー/MAC ごとに 1 分 window へ蓄積
  -> 各項目の中央値を出し、単発スパイクを除外して sensor_minute に upsert
```

注意:

- 一部の Minew 系センサーは `0000ffe1-0000-1000-8000-00805f9b34fb` の ServiceData を使います。
- これらは `temperature_c`, `humidity_percent`, `battery_percent` を `ffe1` フォーマットから読み取ります。
- `BLE_OUTLIER_FILTER=true` の場合、直近正常値から大きく外れた 1 分値は保存しません。
  同じ方向の値が連続した場合は実際の変化として受け入れます。

主な BLE 外れ値フィルタ設定:

```text
BLE_OUTLIER_FILTER
BLE_OUTLIER_HISTORY_SIZE
BLE_OUTLIER_CONFIRM_WINDOW
BLE_OUTLIER_TEMP_DELTA
BLE_OUTLIER_HUMIDITY_DELTA
BLE_OUTLIER_BATTERY_DELTA
BLE_OUTLIER_PRESSURE_DELTA
BLE_OUTLIER_CO2_DELTA
BLE_OUTLIER_ETVOC_DELTA
BLE_OUTLIER_RSSI_DELTA
```

保存する値:

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

## Cisco Spaces Firehose Collector

Cisco Spaces Indoor IoT Firehose から `IOT_TELEMETRY` event を読み、既存の
`devices` と `sensor_minute` に 1 分値を書き込みます。BLE adapter がない VM や
container 環境では、BLE scan の代わりにこの collector を使えます。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
SENSOR_INGEST_SOURCE=cisco_spaces \
CISCO_SPACES_API_KEY='...' \
hm-cisco-spaces-collector
```

主な設定:

```text
CISCO_SPACES_FIREHOSE_URL
CISCO_SPACES_RECONNECT_MIN_DELAY
CISCO_SPACES_RECONNECT_MAX_DELAY
CISCO_SPACES_STREAM_HEARTBEAT
CISCO_SPACES_SAMPLE_WINDOW
CISCO_SPACES_FIELD_FRESHNESS
CISCO_SPACES_UPLOAD_INTERVAL
CISCO_SPACES_BATTERY_MODE
CISCO_SPACES_BATTERY_ALLOWLIST
CISCO_SPACES_DRY_RUN
CISCO_SPACES_ALLOW_SECONDARY
CISCO_SPACES_DEBUG
CISCO_SPACES_PRUNE_CONFIGURED_BLE_SENSORS
```

collector は `X-API-Key` header で Firehose に接続し、stream が切れた場合は
exponential backoff で再接続します。温度、湿度、気圧、CO2、照度、battery、TVOC
は 5 sample median を使い、既知の sentinel 値は保存しません。Firehose の
`tvoc.valueInPpb` は既存BLE importer の eTVOC と同じ表示単位に合わせるため
1000 で割って保存します。
Cisco Spaces Indoor IoT sensor では、温度だけが短時間で大きく低下してすぐ戻る
payload が観測されています。raw JSON は `cisco_spaces_raw_events` に保持したまま、
normalized `sensor_minute` へ入れる前に、直近の accepted temperature より 3°C
以上低い短時間値を workaround として除外します。これは TAC/debug 用 raw 保存には
影響しません。
`CISCO_SPACES_STREAM_HEARTBEAT` は Firehose 接続中に collector liveness を
`collector_status` へ記録する間隔です。telemetry が保存された時刻は
`last_data_at` として別に記録されます。
`CISCO_SPACES_ALLOW_SECONDARY=false` の場合、collector は Firehose 接続前に DB
advisory lock を取得します。同じ DB を使う別の `hm-cisco-spaces-collector` が
すでに動いている場合は起動失敗します。diagnostic run で一時的に重複接続を許可
したい場合だけ、明示的に `CISCO_SPACES_ALLOW_SECONDARY=true` を指定します。
`CISCO_SPACES_PRUNE_CONFIGURED_BLE_SENSORS=true` の場合、起動時に
`BLE_SENSORS_FILE` の静的BLEセンサーを `devices` から外します。時系列データや
alert rule がない行は削除し、残す必要がある行は `enabled=false` にします。

## Cisco IoT Orchestrator Collector

Cisco Sensor Connect / Wireless IoT Orchestrator の MQTT broker から BLE
advertisement telemetry を購読し、既存の `devices` と `sensor_minute` に
1 分値を書き込みます。Cisco Spaces Firehose と異なり、Orchestrator が送る
protobuf `DataBatch` を decode し、BLE payload の `ServiceData` 相当を既存
BLE importer と同じ形式で解釈します。raw telemetry は DB に保存しません。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
SENSOR_INGEST_SOURCE=cisco_iot_orchestrator \
CISCO_IOT_ORCH_API_URL='https://192.168.67.6:8081' \
CISCO_IOT_ORCH_MQTT_ADDR='192.168.67.6:41883' \
CISCO_IOT_ORCH_ONBOARD_APP_ID='onboard' \
CISCO_IOT_ORCH_ONBOARD_API_KEY='...' \
CISCO_IOT_ORCH_CONTROL_APP_ID='control' \
CISCO_IOT_ORCH_CONTROL_API_KEY='...' \
CISCO_IOT_ORCH_DATA_APP_ID='data' \
CISCO_IOT_ORCH_DATA_API_KEY='...' \
CISCO_IOT_ORCH_TOPIC='ioslab/home-metrics/ble/advertisements/v1' \
hm-cisco-iot-orchestrator-collector
```

主な設定:

```text
CISCO_IOT_ORCH_API_URL
CISCO_IOT_ORCH_MQTT_ADDR
CISCO_IOT_ORCH_ONBOARD_APP_ID
CISCO_IOT_ORCH_ONBOARD_API_KEY
CISCO_IOT_ORCH_CONTROL_APP_ID
CISCO_IOT_ORCH_CONTROL_API_KEY
CISCO_IOT_ORCH_DATA_APP_ID
CISCO_IOT_ORCH_DATA_API_KEY
CISCO_IOT_ORCH_TOPIC
CISCO_IOT_ORCH_REGISTER_DATA_APP
CISCO_IOT_ORCH_DRY_RUN
CISCO_IOT_ORCH_DEBUG
CISCO_IOT_ORCH_STREAM_HEARTBEAT
```

MQTT は Orchestrator 側の broker へ subscriber として接続します。デフォルトの
topic は `ioslab/home-metrics/ble/advertisements/v1` です。SCIM onboarding は
`onboard` app、control API は `control` app、MQTT subscriber は data receiver
app を使います。ioslab の試験環境では data receiver app ID に `data` を使います。
`CISCO_IOT_ORCH_REGISTER_DATA_APP=true`
の場合、起動時に `POST /control/registration/registerDataApp` を呼び、control
app と data receiver app の topic 登録を行います。
通常は Orchestrator UI/API 側で登録済みにしておき、この値は `false` のままで
collector だけを起動します。

### Cisco IoT Orchestrator setup

Orchestrator から MQTT で BLE advertisement を受けるには、collector の起動前に
Orchestrator 側で以下を順に登録します。

1. SCIM で BLE device を onboard する
2. `registerDataApp` で data receiver app と MQTT topic を紐付ける
3. `registerTopic` で onboard 済み BLE device ID と advertisement topic を紐付ける

`registerDataApp` と `registerTopic` は control app の API key で実行します。
SCIM onboarding は onboarding app の API key で実行します。Orchestrator の
証明書を信頼していない試験環境では `curl -k` を使います。本番では CA trust を
入れて `-k` に依存しないようにします。

SCIM onboarding body の例:

```json
{
  "schemas": [
    "urn:ietf:params:scim:schemas:core:2.0:Device",
    "urn:ietf:params:scim:schemas:extension:ble:2.0:Device",
    "urn:ietf:params:scim:schemas:extension:endpointAppsExt:2.0:Device"
  ],
  "deviceDisplayName": "home-metrics-00-fa-b6-07-de-4b",
  "adminState": true,
  "urn:ietf:params:scim:schemas:extension:ble:2.0:Device": {
    "versionSupport": ["5.3"],
    "deviceMacAddress": "00:FA:B6:07:DE:4B",
    "isRandom": false,
    "mobility": true,
    "pairingMethods": [
      "urn:ietf:params:scim:schemas:extension:pairingNull:2.0:Device"
    ],
    "urn:ietf:params:scim:schemas:extension:pairingNull:2.0:Device": {},
    "urn:ietf:params:scim:schemas:extension:pairingJustWorks:2.0:Device": {
      "key": 0
    },
    "urn:ietf:params:scim:schemas:extension:pairingPassKey:2.0:Device": {
      "key": 0
    },
    "urn:ietf:params:scim:schemas:extension:pairingOOB:2.0:Device": {
      "key": "",
      "randNumber": 0,
      "confirmationNumber": 0
    }
  },
  "urn:ietf:params:scim:schemas:extension:endpointAppsExt:2.0:Device": {
    "onboardingUrl": "onboard",
    "deviceControlUrl": ["control"],
    "dataReceiverUrl": ["data"]
  }
}
```

```bash
curl -k -X POST "$CISCO_IOT_ORCH_API_URL/scim/v2/Devices" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H "x-api-key: $CISCO_IOT_ORCH_ONBOARD_API_KEY" \
  --data-binary @device.json
```

`isRandom` は通常 `false` です。random/private/static address の device を
onboard する場合だけ `true` を使います。実機では `versionSupport` と pairing
extension を含めた body が安定して受理されました。

Data receiver app 登録の例:

```json
{
  "dataApps": [
    {
      "dataAppID": "data"
    }
  ],
  "topic": "ioslab/home-metrics/ble/advertisements/v1",
  "dataFormat": "default",
  "controlApp": "control"
}
```

```bash
curl -k -X POST "$CISCO_IOT_ORCH_API_URL/control/registration/registerDataApp" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H "x-api-key: $CISCO_IOT_ORCH_CONTROL_API_KEY" \
  --data-binary @register-dataapp.json
```

`dataApps` の要素は `{"dataAppID": "<app id>"}` です。`["data"]` や
`{"app":"data"}` では実機 1.2.1 で `Missing dataApps` または
`Maximum topic limit reached` になるため使いません。

Topic 登録の例:

```json
{
  "technology": "ble",
  "id": "f2c551d1-6d42-41e1-85ce-82cdda2c84d8",
  "controlApp": "control",
  "ids": [
    "f2c551d1-6d42-41e1-85ce-82cdda2c84d8",
    "d96231e7-13b4-4ccd-bafa-ec5c60b95c88"
  ],
  "topic": "ioslab/home-metrics/ble/advertisements/v1",
  "dataFormat": "default",
  "ble": {
    "type": "advertisements",
    "serviceID": "FE6A",
    "characteristicID": "0000"
  }
}
```

```bash
curl -k -X POST "$CISCO_IOT_ORCH_API_URL/control/registration/registerTopic" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H "x-api-key: $CISCO_IOT_ORCH_CONTROL_API_KEY" \
  --data-binary @register-topic.json
```

`ids` は SCIM onboarding response または `GET /scim/v2/Devices?onboardApp=onboard`
で得られる Orchestrator の device UUID です。`id` には `ids` の先頭と同じ値を
入れます。connectionless advertisement を MQTT へ流す用途では
`ble.type=advertisements` を使います。

登録後は data receiver app の ID/API key で MQTT broker に接続し、登録 topic
を subscribe します。`CONNACK` と `SUBACK` が成功しても Data App Topics 登録が
ない場合は publish が届かないため、`registerDataApp` の成功と UI の Data App
Topics を確認します。

collector は 1 分 window ごとに値を蓄積し、`sensor_minute` へ中央値を書き込み
ます。HibouCO2/Env 系の FE6A service data は既存の direct BLE scan 実装と同じ
marker 解釈を使います。

```text
03 05 17 + float32 LE  -> pressure_hpa
03 13 + int16 LE / 256 -> temperature_c
02 12 + uint8          -> humidity_percent
03 20 + uint16 LE      -> lux
04 1f 07 + uint16 LE   -> co2_ppm
04 1f 08 + uint16 LE   -> etvoc
02 80 02 ...           -> battery_percent
```

Compose では `cisco-iot` profile を使います。試験期間中は `cisco-spaces` profile
と同時に起動できますが、同じ BLE sensor の 1 分値に両方の collector が upsert
する点には注意してください。

## DB Check

DB に入った最新値を簡単に確認します。

```bash
hm-db-check
```

デバイスを選び、センサー項目を選ぶと、`sensor_minute` から最新 5 件を表示します。

## Alert Worker

`alert_rules` に登録された条件を定期的に評価します。デフォルトは dry-run で、APNs へ送信せず、送るべき通知を `notification_events.status = 'dry_run'` として記録します。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
ALERT_WORKER_INTERVAL=1m \
hm-alert-worker
```

一回だけ判定して終了する場合:

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
ALERT_WORKER_RUN_ONCE=true \
hm-alert-worker
```

collector / sensor / energy の収集状態を DB に記録し、管理者向け webhook へ送る
health evaluator は別スイッチで有効化します。Apple Push 通知とは独立しており、
health 通知は `health_alert_state` と `health_notification_events` に保存します。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
HEALTH_EVALUATOR_ENABLED=true \
HEALTH_WEBHOOK_DRY_RUN=false \
WEBHOOK_RELAY_URL='http://webhook-relay-web:8097/webhooks/home-metrics' \
WEBHOOK_RELAY_TOKEN='...' \
hm-alert-worker
```

主な設定:

```text
HEALTH_EVALUATOR_ENABLED
HEALTH_WEBHOOK_DRY_RUN
HEALTH_COLLECTOR_HEARTBEAT_WARNING_AFTER
HEALTH_COLLECTOR_HEARTBEAT_CRITICAL_AFTER
HEALTH_COLLECTOR_DATA_WARNING_AFTER
HEALTH_COLLECTOR_DATA_CRITICAL_AFTER
HEALTH_ECHONET_DATA_WARNING_AFTER
HEALTH_ECHONET_DATA_CRITICAL_AFTER
HEALTH_CISCO_SPACES_HEARTBEAT_WARNING_AFTER
HEALTH_CISCO_SPACES_HEARTBEAT_CRITICAL_AFTER
HEALTH_CISCO_SPACES_DATA_WARNING_AFTER
HEALTH_CISCO_SPACES_DATA_CRITICAL_AFTER
HEALTH_NATURE_REMO_DATA_WARNING_AFTER
HEALTH_NATURE_REMO_DATA_CRITICAL_AFTER
HEALTH_APCUPSD_DATA_WARNING_AFTER
HEALTH_APCUPSD_DATA_CRITICAL_AFTER
HEALTH_SENSOR_STALE_AFTER
HEALTH_ENERGY_STALE_AFTER
HEALTH_NOTIFICATION_COOLDOWN
HOME_METRICS_BASE_URL
WEBHOOK_RELAY_URL
WEBHOOK_RELAY_TOKEN
WEBHOOK_RELAY_TIMEOUT
```

`HEALTH_WEBHOOK_DRY_RUN=true` の場合、webhook へは送らず
`health_notification_events.status = 'dry_run'` を記録します。
`WEBHOOK_RELAY_URL` は API response には出さず、配送結果だけを保存します。
collector health の webhook は時間判定を主にします。`consecutive_failures`
と `last_error` は payload の context として含めますが、失敗回数だけでは
通知しません。旧 `HEALTH_COLLECTOR_STALE_AFTER` と `HEALTH_DATA_STALE_AFTER`
は後方互換として warning threshold に map されます。

APNs へ実送信する場合:

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
ALERT_WORKER_DRY_RUN=false \
APNS_KEY_FILE=/path/to/AuthKey_XXXXXXXXXX.p8 \
APNS_KEY_ID=XXXXXXXXXX \
APNS_TEAM_ID=YYYYYYYYYY \
APNS_BUNDLE_ID=org.example.home-metrics \
hm-alert-worker
```

APNs Auth Key は sandbox / production で共通です。送信先 endpoint は
`ios_devices.apns_environment` に応じて自動で切り替えます。Xcode debug
build の token は `sandbox`、TestFlight/App Store build の token は
`production` として登録します。送信対象は `ios_devices.app_bundle_id` が
`APNS_BUNDLE_ID` に一致する有効な token です。

APNs の公開向け設定、sandbox/production の切り替え、秘密情報の扱いは
[docs/apns.md](docs/apns.md) を参照してください。`APNS_ENVIRONMENT` は
互換メモとして残っている環境でも backend では使いません。`.p8` key、
device token、実 bundle ID、Key ID、Team ID は public repository に
commit しないでください。

例: Bed の温度が 35 度を超えたら 24 時間は再通知しないルール。

```sql
INSERT INTO alert_rules (user_id, mac, metric, operator, threshold, cooldown_duration)
VALUES (1, 'aa:bb:cc:dd:ee:02', 'temperature_c', '>', 35, interval '24 hours');
```

## API Server

iOS app や別アプリから DB を読むための最小 REST API です。初期実装は single user `id=1` 前提です。`API_TOKEN` を設定すると `/api/health` 以外の `/api/*` endpoint に bearer token 認証を要求します。production では `API_REQUIRE_TOKEN=true` を設定すると、`API_TOKEN` が空のまま起動することを防げます。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
API_ADDR=':8080' \
API_TOKEN='change-me' \
API_REQUIRE_TOKEN=true \
API_ALLOWED_ORIGINS='http://localhost:8080' \
hm-api-server
```

systemd では `/etc/home-metrics/home-metrics.env` に `API_TOKEN=...` と必要に応じて `API_ALLOWED_ORIGINS=...` を置きます。このファイルは Git 管理外で、全 service が共通の `EnvironmentFile` として読み込みます。

ブラウザで `http://localhost:8080/` を開くと簡易Web UIで latest values, alert rules, notification events を確認できます。
管理画面は `http://localhost:8080/admin` で、collector status、health alerts、webhook delivery を確認できます。HTML route の `/` と `/admin` は公開配信されますが、画面内で呼び出す `/api/admin/*` は token 認証の対象です。

主な endpoint:

```text
GET    /api/health
GET    /api/health/details
GET    /admin
GET    /admin.html
GET    /api/admin/schema
GET    /api/admin/cisco-spaces-firehose
GET    /api/admin/collector-status
GET    /api/admin/health-alerts
GET    /api/admin/health-notification-events
DELETE /api/admin/health-notification-events
POST   /api/admin/health-alerts/{alert_key}/test-webhook
POST   /api/admin/devices/{mac}/maintenance
GET    /api/devices
GET    /api/devices/{mac}/latest
GET    /api/devices/{mac}/series?metric=temperature_c&range=1d
GET    /api/alert-rules
POST   /api/alert-rules
PUT    /api/alert-rules/{id}
DELETE /api/alert-rules/{id}
GET    /api/notification-events
GET    /api/energy/latest
GET    /api/energy/series
GET    /api/ios/devices
POST   /api/ios/devices
PUT    /api/ios/devices/{id}
DELETE /api/ios/devices/{id}
```

全 API は JSON を返します。エラー時の body は `{"error":"message"}` です。認証は `Authorization: Bearer <API_TOKEN>` を基本とし、`X-API-Token: <API_TOKEN>` も受け付けます。

`POST /api/admin/devices/{mac}/maintenance` は sensor device を maintenance mode に入れる、
または解除します。maintenance mode は sensor freshness alert の評価対象から外すための
運用状態で、collector alert や energy alert には使いません。`/admin` の Health Alerts では
sensor alert の行だけに maintenance toggle を表示します。メンテ中もデータ保存や既存グラフは止めません。

```json
{
  "maintenance_mode": true,
  "reason": "maintenance from /admin"
}
```

series API の `range` は `1d`, `1w`, `1m`, `3m`, `1y` に対応しています。

series API の対応 metric は `temperature_c`, `humidity_percent`, `battery_percent`, `rssi_dbm`, `pressure_hpa`, `co2_ppm`, `lux`, `etvoc` です。レスポンスは `{"mac":"...","metric":"temperature_c","range":"1d","points":[{"ts":"...","value":23.4}]}` です。

alert rule API の対応 operator は `>`, `>=`, `<`, `<=` です。`POST /api/alert-rules` と `PUT /api/alert-rules/{id}` は `mac`, `metric`, `operator`, `threshold` が必須です。`PUT` は部分更新ではなく full replace です。`enabled` を省略すると `true`、`cooldown_seconds` を省略すると `86400` 秒になります。

notification events API は `limit`, `status`, `mac`, `alert_rule_id` query に対応しています。`limit` は 1 から 500、`status` は `pending`, `dry_run`, `sent`, `failed`, `skipped` に対応しています。
backend health webhook delivery history は `GET /api/admin/health-notification-events` で最新 200 件を返します。
`DELETE /api/admin/health-notification-events` は webhook delivery history だけを全削除し、
`health_alert_state` や通常の user notification history (`notification_events`) は削除しません。

energy latest API は `source`, `device_key` query で絞り込みできます。レスポンスは `[{ "ts":"...", "source":"echonet", "device_key":"...", "label":"EIBS7", "location":"...", "metric":"solar_generation_w", "value":1200, "unit":"W" }]` の配列です。現時点の metric は Nature Remo E の `measured_instantaneous_w`、ECHONET Lite の `solar_generation_w`, `battery_remaining`, `battery_power_w` です。

energy series API は `source`, `device_key`, `metric`, `range` query に対応しています。`range` は `1d`, `1w`, `1m`, `3m`, `1y` です。レスポンスは `{"source":"echonet","device_key":"...","metric":"solar_generation_w","range":"1d","unit":"W","points":[{"ts":"...","value":1200}]}` です。広い期間では TimescaleDB continuous aggregate の `energy_1hour`, `energy_12hour`, `energy_1day` を使います。

## DB Maintenance

`sensor_minute` から長期保存用テーブルを更新し、古い 1 分値を削除します。
backend health webhook delivery history (`health_notification_events`) も削除対象で、
`DB_MAINT_RETAIN_HEALTH_NOTIFICATION_EVENTS` の既定値は 7 日 (`168h`) です。

energy 系は `db/energy_optimization.sql` で TimescaleDB continuous aggregate,
1 day chunk interval, columnstore policy, 14 day raw retention policy を設定します。
APC UPS と ECHONET Lite は PostgreSQL 書き込み時に timestamp を 1 分へ丸めます。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
DB_MAINT_REFRESH_LOOKBACK=336h \
DB_MAINT_RETAIN_MINUTE=336h \
DB_MAINT_RETAIN_HEALTH_NOTIFICATION_EVENTS=168h \
hm-db-maint
```

rollup:

```text
sensor_1hour   1 hour
sensor_12hour  12 hours
sensor_1day    1 day
```

## Nature Remo E

Nature Remo E の Cloud API からスマートメーター値を取得し、`energy_readings` に保存します。`NATURE_REMO_TOKEN` は systemd の `EnvironmentFile` などで渡します。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
NATURE_REMO_TOKEN='...' \
NATURE_REMO_INTERVAL=60s \
NATURE_REMO_DEVICE_KEY=remo-e \
hm-nature-remo-collector
```

一回だけ取得して終了する場合:

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
NATURE_REMO_TOKEN='...' \
NATURE_REMO_RUN_ONCE=true \
hm-nature-remo-collector
```

保存する metric:

```text
measured_instantaneous_w
```

systemd では `/etc/home-metrics/home-metrics.env` に `NATURE_REMO_TOKEN=...` を置きます。

## ECHONET Lite

EIBS7 から ECHONET Lite UDP で値を取得し、`energy_readings` に保存します。`ECHONET_TARGET_IP` を省略すると multicast discovery を試します。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
ECHONET_TARGET_IP='192.0.2.10' \
ECHONET_POLL_INTERVAL=60s \
hm-echonet-collector
```

一回だけ取得して終了する場合:

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
ECHONET_TARGET_IP='192.0.2.10' \
ECHONET_RUN_ONCE=true \
hm-echonet-collector
```

保存する metric:

```text
solar_generation_w
battery_remaining
battery_power_w
```

## APC UPS

apcupsd NIS から APC UPS の状態を取得し、`energy_readings` に保存します。

```bash
BLE_DB_DSN=dbname=ble_sensors host=/var/run/postgresql \
APCUPSD_SERVER=tcp://127.0.0.1:3551 \
APCUPSD_INTERVAL=60s \
APCUPSD_DEVICE_KEY=ups \
hm-apcupsd-collector
```

保存する metric:

```text
input_voltage_v
load_percent
battery_charge_percent
battery_voltage_v
```

## systemd

```bash
sudo make install
sudo cp deploy/hm-ble-collector.service /etc/systemd/system/
sudo cp deploy/hm-cisco-spaces-collector.service /etc/systemd/system/
sudo cp deploy/hm-api-server.service /etc/systemd/system/
sudo cp deploy/hm-alert-worker.service /etc/systemd/system/
sudo cp deploy/hm-db-maint.service /etc/systemd/system/
sudo cp deploy/hm-db-maint.timer /etc/systemd/system/
sudo cp deploy/hm-nature-remo-collector.service /etc/systemd/system/
sudo cp deploy/hm-echonet-collector.service /etc/systemd/system/
sudo cp deploy/hm-apcupsd-collector.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo env DB_MIGRATIONS_DIR=/usr/local/share/home-metrics/migrations hm-db-migrate
sudo systemctl enable --now hm-ble-collector.service
# Use Cisco Spaces instead of local BLE scanning when the host has no BLE adapter.
# sudo systemctl enable --now hm-cisco-spaces-collector.service
sudo systemctl enable --now hm-api-server.service
sudo systemctl enable --now hm-alert-worker.service
sudo systemctl enable --now hm-db-maint.timer
sudo systemctl enable --now hm-nature-remo-collector.service
sudo systemctl enable --now hm-echonet-collector.service
sudo systemctl enable --now hm-apcupsd-collector.service
```

deploy/*.service は `home-metrics` ユーザーで起動するサンプルです。インストール先では `sudo useradd --system --home-dir /var/lib/home-metrics --create-home --shell /usr/sbin/nologin home-metrics` で実行ユーザーを作成し、BLE を使う場合は `sudo usermod -aG bluetooth home-metrics` で権限を付与します。既存環境で別ユーザーを使う場合は `User`, `Group`, `WorkingDirectory`, `BLE_DB_DSN` をローカル方針に合わせて調整します。

各 service は `/etc/home-metrics/home-metrics.env` を任意の `EnvironmentFile` として読むため、API token、DB DSN、APC UPS address、ECHONET target、Nature Remo token などの環境差分は unit ファイルに直接書かず `/etc/home-metrics/home-metrics.env` 側で管理します。

systemd unit の `ExecStart` は `/usr/local/bin/<binary>` を参照します。更新時は `make && sudo make install` の後、該当 service を restart します。

## Investigation Tools

調査用 Python scripts は `tools/` にあります。

```bash
python3 tools/scan_bluez.py --seconds 60 --jsonl home-metrics-bluez-scan.jsonl
python3 tools/decode_ble_log.py home-metrics-bluez-scan.jsonl --csv home-metrics-decoded.csv
```

`tools/scan_ble.py` は raw HCI socket を使う実験用です。通常は BlueZ D-Bus を使う `tools/scan_bluez.py` または Go collector を使います。

## Retention

このセクションでは sensor 系 retention の基本方針を示します。energy 系の TimescaleDB policy は db/energy_optimization.sql に記録しています。

短期:

```text
sensor_minute  1 minute  14 days
```

長期:

```text
sensor_1hour   1 hour
sensor_12hour  12 hours
sensor_1day    1 day canonical long-term data
```

1 年表示で 180 点前後にしたい場合でも、保存粒度は 1 日のままにし、表示時に 2 日 bucket で平均します。

## License

Home Metrics is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
