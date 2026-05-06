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

## Docker Compose

Docker Compose で PostgreSQL / TimescaleDB と Home Metrics の各 daemon をまとめて起動できます。

```bash
cp examples/home-metrics.compose.env.example .env
docker compose up -d --build hm-api-server hm-alert-worker hm-db-maint
```

DB は `timescale/timescaledb:latest-pg17` を使い、初回起動時に `db/schema.sql` と
`db/energy_optimization.sql` を読み込みます。永続データは `pgdata` volume に保存します。

DB は Compose network 内だけで公開し、collector は `db:5432` へ接続します。
Cisco Spaces Firehose、Nature Remo、apcupsd、ECHONET Lite は通常の Compose
network 上で動作します。BLE collector だけはホストの BlueZ D-Bus を使うため、
`ble` profile の例外として host network を使います。

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
DB をホストに publish しない構成では、BLE profile を使う場合だけ別途
`BLE_DB_DSN` で到達可能なDB接続先を指定してください。

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
  -> 各項目の中央値を sensor_minute に upsert
```

注意:

- 一部の Minew 系センサーは `0000ffe1-0000-1000-8000-00805f9b34fb` の ServiceData を使います。
- これらは `temperature_c`, `humidity_percent`, `battery_percent` を `ffe1` フォーマットから読み取ります。

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
CISCO_SPACES_SAMPLE_WINDOW
CISCO_SPACES_FIELD_FRESHNESS
CISCO_SPACES_UPLOAD_INTERVAL
CISCO_SPACES_BATTERY_MODE
CISCO_SPACES_BATTERY_ALLOWLIST
CISCO_SPACES_DRY_RUN
CISCO_SPACES_DEBUG
CISCO_SPACES_PRUNE_CONFIGURED_BLE_SENSORS
```

collector は `X-API-Key` header で Firehose に接続し、stream が切れた場合は
exponential backoff で再接続します。温度、湿度、気圧、CO2、照度、battery、TVOC
は 5 sample median を使い、既知の sentinel 値は保存しません。Firehose の
`tvoc.valueInPpb` は既存BLE importer の eTVOC と同じ表示単位に合わせるため
1000 で割って保存します。
`CISCO_SPACES_PRUNE_CONFIGURED_BLE_SENSORS=true` の場合、起動時に
`BLE_SENSORS_FILE` の静的BLEセンサーを `devices` から外します。時系列データや
alert rule がない行は削除し、残す必要がある行は `enabled=false` にします。

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

iOS app や別アプリから DB を読むための最小 REST API です。初期実装は single user `id=1` 前提です。`API_TOKEN` を設定すると `/` と `/api/health` 以外に bearer token 認証を要求します。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
API_ADDR=':8080' \
API_TOKEN='change-me' \
API_ALLOWED_ORIGINS='http://localhost:8080' \
hm-api-server
```

systemd では `/etc/home-metrics/home-metrics.env` に `API_TOKEN=...` と必要に応じて `API_ALLOWED_ORIGINS=...` を置きます。このファイルは Git 管理外で、全 service が共通の `EnvironmentFile` として読み込みます。

ブラウザで `http://localhost:8080/` を開くと簡易Web UIで latest values, alert rules, notification events を確認できます。

主な endpoint:

```text
GET    /api/health
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

series API の `range` は `1d`, `1w`, `1m`, `3m`, `1y` に対応しています。

series API の対応 metric は `temperature_c`, `humidity_percent`, `battery_percent`, `rssi_dbm`, `pressure_hpa`, `co2_ppm`, `lux`, `etvoc` です。レスポンスは `{"mac":"...","metric":"temperature_c","range":"1d","points":[{"ts":"...","value":23.4}]}` です。

alert rule API の対応 operator は `>`, `>=`, `<`, `<=` です。`POST /api/alert-rules` と `PUT /api/alert-rules/{id}` は `mac`, `metric`, `operator`, `threshold` が必須です。`PUT` は部分更新ではなく full replace です。`enabled` を省略すると `true`、`cooldown_seconds` を省略すると `86400` 秒になります。

notification events API は `limit`, `status`, `mac`, `alert_rule_id` query に対応しています。`limit` は 1 から 500、`status` は `pending`, `dry_run`, `sent`, `failed`, `skipped` に対応しています。

energy latest API は `source`, `device_key` query で絞り込みできます。レスポンスは `[{ "ts":"...", "source":"echonet", "device_key":"...", "label":"EIBS7", "location":"...", "metric":"solar_generation_w", "value":1200, "unit":"W" }]` の配列です。現時点の metric は Nature Remo E の `measured_instantaneous_w`、ECHONET Lite の `solar_generation_w`, `battery_remaining`, `battery_power_w` です。

energy series API は `source`, `device_key`, `metric`, `range` query に対応しています。`range` は `1d`, `1w`, `1m`, `3m`, `1y` です。レスポンスは `{"source":"echonet","device_key":"...","metric":"solar_generation_w","range":"1d","unit":"W","points":[{"ts":"...","value":1200}]}` です。広い期間では TimescaleDB continuous aggregate の `energy_1hour`, `energy_12hour`, `energy_1day` を使います。

## DB Maintenance

`sensor_minute` から長期保存用テーブルを更新し、古い 1 分値を削除します。

energy 系は `db/energy_optimization.sql` で TimescaleDB continuous aggregate,
1 day chunk interval, columnstore policy, 14 day raw retention policy を設定します。
APC UPS と ECHONET Lite は PostgreSQL 書き込み時に timestamp を 1 分へ丸めます。

```bash
BLE_DB_DSN='dbname=ble_sensors host=/var/run/postgresql' \
DB_MAINT_REFRESH_LOOKBACK=336h \
DB_MAINT_RETAIN_MINUTE=336h \
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
