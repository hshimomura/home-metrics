# Shelly BLU Motion ZB 対応

## 保存する情報

Cisco AP → Sensor Connect → MQTT → `hm-cisco-iot-orchestrator-collector` の経路で、BTHome v2 の人感状態（`0x21`）と電池残量（`0x01`）を収集する。型番は `SBMO-103Z`。

- 人感は `sensor_motion_events` に検出・解除の状態遷移を保存する。最初の観測は `initial=true` で区別する。分平均やグラフの補間には変換しない。
- 最新状態・最終受信時刻・電池の実観測時刻は `sensor_motion_state` に保持する。
- 電池は既存の `sensor_minute.battery_percent` にも保存し、既存の長期 rollup と series API を使える。
- 明るさ・ボタン・RSSI の測定値は本機の保存対象にしない。受信 AP の MAC は診断用のイベントメタデータとして保持する。

電池は広告に含まれるため、定期 GATT 接続や `gatt_battery` の設定は不要。センサーの Beacon mode は無効のまま検証した。

## 設定

センサー定義例:

```json
{
  "mac": "FC:4D:6A:26:53:E3",
  "label": "Shelly Motion",
  "location": "Shelly Motion",
  "ingest_source": "cisco_sensor_connect",
  "sensor_type_code": "shelly_blu_motion_zb",
  "sensor_category": "motion",
  "enabled": true
}
```

表示名と設置場所は実際の配置に合わせる。センサーの登録情報と MAC は固定値であり、環境に合わせて変更する。

API の `MOTION_SENSOR_STALE_AFTER` は既定45分。これを過ぎると `motion.fresh=false` になる。鮮度が落ちても保存済みの人感状態を勝手に false にしない。PIR の未検出は在室者ゼロを保証しない。

電池の閾値通知には既存の sensor alert rule を使用できる。通常の広告間隔が15分のため、本機にルールを作る場合は `max_data_age` を45分程度に設定する。既存ルールの既定10分では古いデータと判定される可能性がある。本対応で通知ルールの自動作成は行わない。

## Cisco 側の設定

2026-11-14、Sensor Connect 1.2.2 で次を実機確認した。

1. `default-ap-profile` の許可リストに対象の完全 MAC を追加した。既存10件を保持し、追加後は11件。
2. TYO7AP1 / TYO7AP3 は Connected / Radio Up / Scan / In Sync。実機では両方とも C9120AX と表示された。
3. SCIM 登録の UUID は `0f418750-ec9d-4bd4-99f7-8b3ff76f5de8`。既存の `onboard` / `control` / `data` アプリを使用。
4. `/control/registration/registerTopic` に `ble.type=advertisements`、`serviceID=fcd2`、`characteristicID=fcd2` を指定し、`SUCCESS` と実配信を確認した。
5. topic は `ioslab/home-metrics/ble/advertisements/v1`、MQTT は `192.168.67.6:41883`。

`characteristicID=fcd2` は広告登録 API の項目で、接続して読む GATT characteristic の UUID ではない。センサーとのペアリングや FW 更新は行っていない。

SCIM は `versionSupport=["5.0"]`、`isRandom=false`、null pairing で受理され、広告配信できた。今回の短時間の BlueZ 再探索では実際の address type を再取得できなかったため、将来 GATT 制御を追加する際は無線上の address type を改めて照合する。

## デコードと重複・遅延の扱い

AD structures 内の service UUID `0xFCD2` を識別して BTHome v2 として解析する。Cisco が service data のみを渡す形式も解析できる。暗号化・不正な値・既知 object の長さ不足は正常値にしない。未知 object の長さは推測せず、その位置で解析を停止する。

同じ `(MAC, packet ID, 人感状態, 電池値)` の短時間の再送は5秒間まとめる。DB でも1台ごとに行ロックして状態を比較するため、同じ状態の広告や collector 再起動で新しい遷移を増やさない。古い観測時刻のデータは最新状態を巻き戻さない。

センサーの保持時間は実機で60秒。collector は60秒タイマーで解除を生成せず、実際の解除広告を待つ。周期広告も現在状態の観測として扱い、広告だけから「人の動きが生じた瞬間」と断定しない。

## API と画面

既存の認証方式（Bearer または `X-API-Token`）を使用する。

### 最新値

`GET /api/devices/{mac}/latest`

既存の `values.battery_percent` と `value_timestamps.battery_percent` に加え、次の `motion` を返す。電池の latest 時刻は分単位の bucket 時刻ではなく実観測時刻を使う。

```json
{
  "motion": {
    "motion": false,
    "observed_at": "2026-11-14T16:32:25.098+09:00",
    "last_seen_at": "2026-11-14T16:32:25.098+09:00",
    "fresh": true,
    "ap_mac": "0c:75:bd:d2:a5:e0"
  },
  "values": {"battery_percent": 100},
  "value_timestamps": {"battery_percent": "2026-11-14T16:32:25.098+09:00"}
}
```

上記は抜粋。電池のみを受信した段階では `motion.motion` と `observed_at` は null の場合がある。人感センサー以外の latest response に `motion` は追加しない。

### イベント履歴

`GET /api/devices/{mac}/motion-events?since=<RFC3339>&limit=1000`

- `since` の既定は24時間前、境界は inclusive。
- `limit` は1〜5000、既定1000。最新の観測から降順で返す。上限より多い履歴は `since` と取得件数を考慮して扱う。自動の全件取得・ページングは実装していない。
- 各項目は `id`、`observed_at`、`received_at`、`motion`、`initial`、`packet_id`、`ap_mac`。
- 存在しない機器は404、人感以外の機器や不正なパラメーターは400。

Cisco 側の観測時刻がない場合は collector 受信時刻にフォールバックする。現在のレスポンスには時刻の出所を区別するフィールドはない。

Web の Devices に現在の Motion を表示し、本機の選択時に最新50件の人感履歴と電池のグラフを表示する。人感を温湿度用の数値平均グラフへ入れない。

## Grafana ダッシュボード

2026-11-14、[環境ダッシュボード PostgreSQL](https://grafana.ioslab.jp/d/hs9hxbp/1715d73?from=now-24h&to=now&timezone=browser&refresh=1m)（UID `hs9hxbp`）の末尾に追加した。保存後の version は10。既存パネルの定義は変更していない。

| パネル ID | タイトル | 内容 |
| --- | --- | --- |
| 28 | 人感センサー 検知回数 | 検出かつ `initial=false` の件数を棒グラフ化。解除と初期観測は除外。集計幅は最小5分、長い表示期間では自動調整。凡例に期間合計を表示 |
| 29 | 人感センサー バッテリー残量 | 0〜100% の階段状グラフ。凡例に最新値・最小値を表示 |

既存データソース `pve2-postgresql` の実 UID `cfl59gmbbod8gf` を使用する。対象 MAC は `fc:4d:6a:26:53:e3`。2パネルを全幅で縦に並べ、Grafana の自動配置で既存の最終行の空きに移動しないようにした。

検知回数は `sensor_motion_events` を集計し、記録開始前に0回のデータを生成しない。記録開始後の0回は保存イベントがなかったことを示すもので、未受信や停止区間で人が動かなかったことを保証しない。

電池は表示期間に応じて分・1時間・12時間・1日集計を選び、観測件数で重み付けして集約する。直近1日以上は分データを使い、長期 rollup の更新待ちで最新値が表示されなくなるのを避ける。

Grafana の実データソース経由で24時間・7日・90日・1年のクエリーを検証し、ブラウザー上でも検知合計4回と電池100%の描画を確認した。この追加ではアプリのイメージと CI/CD は変更していない。

追加2パネルの再現用定義は [shelly-motion-panels.json](../examples/grafana/shelly-motion-panels.json)。既存ダッシュボード全体を置換せず、対象 MAC・datasource UID・panel ID の競合を確認して末尾へ追加する。

## DB 互換性

`0002_shelly_motion.sql` がセンサー種別、最新状態・イベントテーブル、状態更新関数を追加する。既存の `0001_initial_schema.sql` は変更していない。

新規インストール用の `db/schema.sql` には現行スキーマと、含まれている migration の checksum 台帳を保持する。初期化直後の `hm-db-migrate` が同じ DDL を再適用しないようにする。既存 DB への追加と、新規 DB での schema → migrate の両方を検証した。

`0003_motion_battery_atomic.sql` は人感状態と電池の分データを同じトランザクションで保存する。新規スキーマと台帳は `tools/generate-schema.sh` で生成し、migration の同時実行は DB advisory lock で直列化する。

人感イベントは自動削除・rollup しない。電池の分データは既存の DB maintenance 設定に従って長期集計される。

## 2026-11-14 の開発配備

ユーザーの指示により CI/CD を通さず、nms4 内でビルドした開発イメージを API と Cisco collector に直接適用した。

| 項目 | 内容 |
| --- | --- |
| 開発イメージ | `local/home-metrics:shelly-dev-20261114` |
| イメージ ID | `sha256:7c7c176fc4a8d568ce044359c44109beaaaf467aad12b48ba84e76c4f9434797` |
| 変更した常駐サービス | `hm-api-server`、`hm-cisco-iot-orchestrator-collector` |
| DB | migration 0002 適用済み |
| 変更前バックアップ | `/var/backups/home-metrics/shelly-dev-20261114-161820`（nms4 内、非公開） |
| Compose override | 上記内の `dev-override.json` |
| センサー定義 | `/srv/docker/home-metrics/data/sensors-iot-orchestrator.json` に1台追加 |
| 元の稼働イメージ | `ghcr.io/hshimomura/home-metrics@sha256:63ff54535f8a8ec48e9525f15748d0cf34e08c63c41d4abe0ea30c9612817b06` |

`.env`、servicecore の image digest、Git のコミット・push、GitHub Actions は変更・実行していない。通常の servicecore 配備はこの開発 override を使わないため、通常デプロイで旧イメージに戻る可能性がある。当時は本格反映をユーザーの了承待ちとしていた。

開発版の再作成は nms4 のサービスディレクトリーで次のように行う。

```sh
docker compose -f compose.yaml \
  -f /var/backups/home-metrics/shelly-dev-20261114-161820/dev-override.json \
  --profile cisco-iot up -d --no-deps --force-recreate \
  hm-api-server hm-cisco-iot-orchestrator-collector
```

旧アプリへ戻す場合は元の Compose 設定で同じ2サービスを再作成し、必要なら保全済み sensor JSON を戻す。新しい人感テーブル・既存の記録は削除しない。共有のアプリ登録や既存センサーの購読は一括解除しない。

## 検証結果と制限

2026-11-14 16:45 JST、ユーザーの指示により4組で実機試験を終了した。人感・電池保存の開発段階の機能確認を合格とし、当時の CI/CD 本格配備は未実施・了承待ち。

| 検出時刻（JST） | 解除時刻（JST） | 結果 |
| --- | --- | --- |
| 16:30:53.496 | 16:32:25.098 | 初回の検出状態は `initial=true` |
| 16:34:30.098 | 16:35:32.496 | 検出・解除を保存 |
| 16:37:10.598 | 16:38:47.497 | 再作成後も履歴を保持 |
| 16:40:14.496 | 16:42:30.098 | 最終開発イメージで検出・解除を保存 |

上記試験の16:42:30時点の履歴は8件（初期観測1件、以降の遷移7件）。AP は TYO7AP1 / TYO7AP3 の2台。対応する packet ID は33〜40で、同一広告の繰り返しを別イベントとして保存していない。全観測の電池値は100%。開発版は収集を継続しており、終了確認時には自然な追加受信により履歴が9件になっていた。

- `make test` / `make build`: 成功。
- OpenAPI lint（CI と同じ Redocly 1.34.5）: 成功。既存の `/api/health` の4xx response 未定義に関する warning 1件。
- Web JavaScript の構文: 成功。認証済み画面の見た目の検証は未実施。
- 一時スキーマで、初期状態・重複・遅延・packet ID 循環・電池のみの更新を確認し、ロールバック済み。
- 一時 DB に fresh schema を適用後、開発イメージの `hm-db-migrate` が両 migration を適用済みとして正常終了することを確認。テスト DB は削除済み。
- 実機の検出・解除と電池100%を、TYO7AP1 / TYO7AP3 → MQTT → DB → API で確認。配備後の collector 再作成でも既存イベントは保持された。
- API の未認証401、未知の機器404、人感以外・不正な `limit` / `since` の400を実通信で確認。
- 既存10台の分データ更新の継続を確認。
- 全体 `/api/health/details` は `degraded`。Flower Care 4台の GATT 失敗は今回の入れ替え前（16:15以前）からの状態で、新しい MQTT target は失敗0・受信継続。この既存障害を今回の検証で正常扱い・消去していない。

MQTT は既存実装と同じ QoS 0。広告・ネットワークの欠落時に過去の人感履歴を復元する機能はない。24時間以上の継続試験と、Beacon mode 無効時の15分周期の受信評価は別途必要。

## 関連文書

- [調査記録](shelly-blu-motion-zb-investigation.md)
- [Cisco AP 対応計画](shelly-blu-motion-zb-cisco-plan.md)
- [API 契約](openapi.yaml)

## 障害時の保存契約

人感・バッテリー観測は専用ディスクキューへ同期してから、受信と独立した worker が DB へ保存する。DB の状態遷移と電池の分データは一つのトランザクションで確定し、その後にキューを削除する。保存済み観測の再実行でイベントを増やさない。DB 障害・collector 再起動時はキューを保持する。MQTT は QoS 0 のため、ディスクへ記録する前の障害や無線区間の取りこぼしは回復保証の対象外。

`CISCO_IOT_ORCH_MOTION_QUEUE_DIR`（既定 `/var/lib/home-metrics/motion-queue`）を永続 volume に配置する。キューは最大10,000件。上限・ディスク障害ではエラーを記録し接続を閉じ、未保存データを黙って破棄しない。MQTT の接続状態と `storage/shelly_motion` の保存状態を別々に記録する。残件数と最古観測の待機時間はログへ出す。DB 操作の期限は5秒、再試行は5秒間隔。

送信元が受信時刻より5分を超えて未来の場合は、観測時刻に受信時刻を使う。過去の遅延観測は DB の現在状態を巻き戻さない。初回の人感状態は検出回数から除く。

MQTT の無通信待機はバッファーの先頭を消費せず待つ。パケット受信開始後の10秒タイムアウト、接続・購読応答の10秒タイムアウト、PINGRESP の30秒タイムアウトでは再接続する。終了要求はソケットを閉じ、応答待ちを中断する。

## レビュー後の検証

Go 1.26.11・pgx v5.9.2・x/text v0.41.0 へ更新。Linux 上で `go vet ./...`、`go test -race ./...`、全コマンドのビルドが成功した。隔離 TimescaleDB で新規 schema、0002 から0003への既存データ付き更新、migration 同時起動、複数 AP の同時観測、保存応答の消失を想定した再実行、遅延広告、packet ID 周回、battery-only を検証した。`govulncheck` の報告は0件。fuzz で不正入力を検査した。

CI/CD 前バックアップは nms4 の `/var/backups/home-metrics/pre-cicd-20261114-173356` に非公開で保持する。このバックアップは一時 TimescaleDB へのフルリストアと23件の人感履歴の復元を確認済み。

ユーザーから修正・テスト成功後の本格配備の了承を得た。本格配備では GitHub Actions の CI / Container 成功後、servicecore の digest 更新と配備を行う。
