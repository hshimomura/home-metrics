# Home Metrics Refactoring Plan

Status: implemented and deployed
Last reviewed: 2026-06-28

## 目的

この計画は、現在の動作とAPI互換性を保ちながら、collector間のロジックの
不一致、DB接続の並行利用、device ownershipの曖昧さ、rollupの集計誤差を
段階的に解消するためのものです。

最優先するのは実行時の整合性です。大きな構造変更は、緊急性のある修正と
テストを先に完了した後で行います。各段階は独立してレビュー、CI、デプロイ、
本番確認できる単位にします。

## 実装前に確認された問題

### DB接続の並行利用

`hm-cisco-iot-orchestrator-collector` は単一の `pgx.Conn` をMQTT flush、
collector status、GATT battery poll、GATT history backfillで共有しています。
大部分はmutexで保護されていますが、GATT pollerの初回スケジュール計算など、
保護されないDB参照が残っています。

race detectorはGoメモリ上の競合を検出できますが、同じPostgreSQL connectionを
並行利用した結果の `conn busy` を完全には検出できません。

### MQTTとGATTのstatus混在

現在はGATT battery取得成功がMQTT collectorと同じ `collector_status` targetを
成功状態にします。このため、MQTTが失敗していてもGATT成功によって
`consecutive_failures` と `last_error` が消える可能性があります。

また、GATT statusを単一targetへ分離するだけでは、端末Aの成功が端末Bの失敗を
消してしまいます。

### Device enabledとownership

無効な端末は設定ロード時に除外されるため、`enabled=false` をDBへ同期できません。
反対に、過去に無効化された端末をcollectorが再び収集しても、現在のupsertでは
`enabled=true` に戻せない場合があります。

Cisco Spacesのprune処理は、Cisco Spacesが所有していない端末を削除または無効化する
条件になっています。複数collectorを同じ設定ファイルで試験する場合、別collectorの
端末状態を変更する危険があります。

### Sensor minute書込み規則の不一致

Cisco Sensor ConnectとCisco Spacesのupsertはsparse metricを非null値でmergeしますが、
直接BLE collectorは同一minuteの未取得metricを `NULL` で上書きできるSQLです。
collectorの切替時や複数経路が誤って重なった場合にデータを失う可能性があります。

### Rollupの平均の平均

現在の12時間・1日rollupは、下位rollupのaverageを単純平均します。metricごとの
有効サンプル数が異なるため、特にbatteryなどの低頻度metricでは正確な平均に
なりません。

### IoT collectorの責務過多

`cmd/hm-cisco-iot-orchestrator-collector/main.go` はMQTT、protobuf、BLE decode、
GATT control、Flower Care history、minute aggregation、DB、status、設定を単一ファイルで
扱っています。変更時の影響範囲が広く、個別機能のテストが難しくなっています。

## 確定した設計判断

### GATT statusは端末単位で管理する

GATT controlはMQTT statusから完全に分離します。

- `collector_name`: `hm-cisco-iot-orchestrator-collector`
- `target_type`: `gatt_control`
- `target_key`: 正規化したBLE MAC address
- metricをDBへ書き込んだ場合: 最初に `MarkDataSuccess`
- 接続、read、または後続処理が失敗した場合: 最後に `MarkFailure`
- API処理が成功し、metricを書き込まなかった場合: `MarkSuccess`

SCIM device IDではなくMAC addressをtarget keyにします。MAC addressの方が運用画面で
端末を識別しやすく、SCIM再登録によるdevice ID変更の影響を受けません。

GATT処理は `dataWritten` と `error` を別々に返します。例えばbatteryを書き込んだ後で
history backfillが失敗した場合は、`MarkDataSuccess` で `last_data_at` を更新してから
`MarkFailure` を記録します。これにより、データ取得の事実を残しながら最終healthを
failureにできます。batteryを書き込んだ後にhistoryが0件だった場合は
`MarkDataSuccess` のまま成功とします。

### Device ownershipは1端末1collectorとする

現時点では複数sourceから同じ端末を同時収集しません。

- `devices.ingest_source` を端末の所有sourceとします。
- Cisco Sensor ConnectのDB ownership名は `cisco_sensor_connect` に統一します。
- 起動選択用の `cisco_iot_orchestrator`、`cisco-iot` などはownership判定に使いません。
- collectorは自分が所有する端末だけを無効化できます。
- 同じMACが複数sourceで同時に有効になる設定はエラーとします。
- 同一設定ファイル内の重複MACもエラーとします。
- `sensor_type_code` が空欄の端末は汎用端末として許可します。
- 非空の `sensor_type_code` が `sensor_types` に存在しない場合はエラーとします。

既存の `devices.ingest_source IS NULL` は未取得のownershipとして扱います。sensor設定に
sourceが明示されている場合に限り、そのsourceがNULL ownershipを一度だけ取得できます。
既に非NULLの別sourceが設定されている場合だけownership競合とします。暗黙のdefault
sourceによるownership取得は行いません。

sourceを移行する場合は次の順序を守ります。

1. 旧collectorを停止します。
2. DBのownership sourceを、想定した旧sourceとの一致を条件に明示的に変更します。
3. sensor設定を新sourceへ変更します。
4. 新collectorを起動します。
5. `devices`、`collector_status`、最新telemetryを確認します。

ownership更新SQLは `WHERE mac = $mac AND ingest_source = $expected_old_source` を含め、
更新行数が1であることを必須とします。0行の場合は現在のownershipを確認し、無条件の
上書きは行いません。

第1段階では移行専用コマンドを追加せず、手順と検証SQLを文書化します。移行頻度が
増えた場合は、競合確認付きの管理コマンドを別途検討します。

### Minute書込みはsparse mergeとする

同一 `(ts, mac)` に複数の部分readingが届いた場合、受信した非null metricだけを更新し、
既存の非null metricを `NULL` へ戻しません。この規則を全sensor collectorで統一します。

`sensor_minute` に過去値をコピーして穴埋めする処理は追加しません。低頻度metricの
最新値は、既存の `/api/devices/{mac}/latest` のmetric別snapshotで合成します。

### Rollupはmetric別countを保持する

正確な上位rollupのため、各metricにaverageとcountを保持します。例えばtemperatureは
`temperature_c` と `temperature_c_count`、batteryは `battery_percent` と
`battery_percent_count` を持ちます。

上位rollupは次の加重平均で計算します。

```text
weighted_average = sum(lower_average * lower_count) / sum(lower_count)
count = sum(lower_count)
```

countは行数ではなくmetricごとの非nullサンプル数です。

既存データには次の制約があります。再生成範囲は単純な「現在から14日前」ではなく、
保持データが完全に存在する最初の最大bucket境界を `accuracy_cutoff` として固定します。
現在の最大bucketは1日なので、最古の保持時刻より後にある最初の完全な1日bucket境界を
使用します。境界はrollup SQLの `time_bucket('1 day', ts)` と同じtimezoneとoriginで
計算します。1時間、12時間、1日の各rollupは、bucket startがこのcutoff以上の場合だけ
再生成します。

- `accuracy_cutoff` 以降: 保持中の `sensor_minute` からaverage/countを再生成します。
- 14日より前: 既存rollupのaverageを維持します。
- count不明の過去averageをweighted再集計しません。
- cutoffをまたぐ部分bucketを既存rollupへ上書きしません。
- migration完了後は `accuracy_cutoff` 以降をweighted rollupの保証範囲とします。
- migration前後の保証境界をrelease noteと運用文書に記載します。

ここでいう正確なaverageは、raw advertisement packetの平均ではありません。
`sensor_minute` に保存されたminute単位の非null値に対する `AVG` と同値であることを
保証します。minute値自体はcollectorが生成したmedianまたは正規化済みreadingです。

Composeの現在の既定値は、minute保持期間とrefresh lookbackの両方が14日です。コード単体の
fallback値ではなく、デプロイ時に有効な環境変数も検証対象に含めます。

## 実施計画

### Phase 1: 緊急整合性修正（完了）

このphaseを最初のPRとして実施します。大規模なファイル分割やrollup migrationは
含めません。

1. 直接BLE collectorのminute upsertをsparse mergeへ変更します。
2. Cisco Spaces起動時の自動pruneを起動経路から削除し、collectorの起動だけでdeviceの
   削除または無効化が発生しないようにします。
3. MQTTのCONNACKとSUBACK成功直後にreconnect delayを初期値へ戻します。
4. `runMQTT` にready通知を追加し、backoff resetを単体テスト可能にします。
5. GATT statusをMQTTから分離し、端末別targetとして記録します。
6. GATT失敗もstatusへ記録します。
7. IoT collectorのDB accessを `pgxpool.Pool` へ移行します。
8. GATT pollerの終了を待ってから最終minute flushを行い、その後poolを閉じます。

完了条件:

- MQTT失敗中のGATT成功がMQTT statusを変更しません。
- あるGATT端末の成功が別端末の失敗を消しません。
- 同一minuteへの部分readingが既存metricを消しません。
- Cisco Spaces collectorの起動だけでは、どのsourceの端末も削除・無効化されません。
- reconnect成功後の次回backoffが初期値になります。
- shutdown時にDB利用中のgoroutineが残りません。

必要なテスト:

- collector statusのMQTT/GATT分離テスト
- 複数GATT端末の独立statusテスト
- direct BLE sparse merge regression test
- Cisco Spaces起動時にdevice状態を変更しないregression test
- MQTT ready/backoff reset test
- DB interfaceを使った並行access/lifecycle test
- 可能であればPostgreSQL integration test
- `go test ./...`
- `go test -race` for collector packages
- `go vet ./...`

### Phase 2: Device registryとownership（完了）

1. sensor設定のparse結果に、全端末と有効な収集対象の両方を保持します。
2. `enabled=false` もDBへ同期します。
3. 有効化された端末は `enabled=true` へ戻します。
4. source競合を検出し、暗黙のownership変更を拒否します。
5. 明示sourceによるNULL ownershipの一度限り取得を実装します。
6. 同一設定内の重複MACを拒否します。
7. 非空の未知 `sensor_type_code` を拒否します。
8. source移行手順と、旧sourceを条件に含む検証SQLを運用文書へ追加します。

完了条件:

- 設定変更だけで同一source内の端末を無効化・再有効化できます。
- collectorは別source所有端末を書換えません。
- ownership競合は起動時またはreconcile時に明確なエラーになります。
- 明示sourceはNULL ownershipを取得できますが、非NULLの別sourceを上書きしません。
- 空のsensor typeを持つ汎用端末は従来どおり動作します。

### Phase 3: 共通sensor store（完了）

新しい抽象化を先に作るのではなく、現在のSQL動作をcharacterization testで固定してから
抽出します。

候補パッケージ:

```text
internal/sensor/
  reading.go
  metrics.go
  metadata.go

internal/sensorstore/
  devices.go
  minute.go
```

共通化する責務:

- MAC address正規化
- device metadata検証
- ownershipを考慮したdevice upsert
- enabled同期
- minute sparse upsert
- PostgreSQL executor interface

各collectorは一度に移行せず、IoT Orchestrator、直接BLE、Cisco Spacesの順で段階的に
移行します。collector固有のdecodeやfilterはstoreへ入れません。

### Phase 4: IoT collectorの分割（完了）

外部動作を変えない機械的な分割として実施します。

```text
cmd/hm-cisco-iot-orchestrator-collector/
  main.go
  config.go
  mqtt.go
  protobuf.go
  decoder.go
  aggregate.go
  gatt_poller.go
  gatt_control.go
  flowercare.go
```

- `main.go`: dependency wiring、signal、lifecycleのみ
- `config.go`: envとsensor設定
- `mqtt.go`: connect、subscribe、heartbeat、reconnect-ready通知
- `protobuf.go`: Cisco data subscription parsing
- `decoder.go`: advertisement service data dispatch
- `aggregate.go`: minute medianとpending window
- `gatt_poller.go`: schedulingと端末別status
- `gatt_control.go`: control API session
- `flowercare.go`: FE95、battery、realtime、history decode

分割commitではSQL、API、decode結果、poll間隔を変更しません。

### Phase 5: Metric registry（完了）

metric追加時に必要な変更を一つの契約として扱います。

対象:

- Go上のmetric keyとDB column
- `sensor_minute` とrollup schema/migration
- minute upsert
- rollup SQL
- latest/series API
- DB check
- OpenAPI
- Web UIとclient向けdocs

Go registryからSQL migrationを自動生成することは目的にしません。代わりに、全対象が
同時に更新されたことを検証するcontract testを追加します。

metadata categoryは原則として `sensor_types.category` から解決します。
`devices.sensor_category` をoverrideとして維持する場合は、overrideの用途を明示し、
通常時の不一致をDB checkで検出します。

### Phase 6: Weighted rollup migration（完了）

1. rollup tableへmetric別count列を追加します。
2. 最古の保持時刻から最初の完全な1日bucket境界を `accuracy_cutoff` として記録します。
3. cutoff以降の完全なbucketだけを `sensor_minute` から再生成します。
4. cutoffより前のaverageを維持し、countはunknownとして扱います。
5. 新規bucketをaverage/countで保存します。
6. 上位bucketをweighted averageで生成します。
7. migration日時とcutoffを正確性保証の開始点として記録します。
8. latest APIとseries APIのresponse shapeは変更しません。

移行後は、cutoffより前のrollupとcutoffをまたぐ部分bucketを再集計対象に含めません。
将来古いraw dataを別経路から復元できた場合だけ、専用の明示的backfillとして扱います。

## 非目標

この計画には次を含めません。

- `sensor_minute` へのlatest値のforward fill
- raw MQTT payloadの永続化
- 複数collectorによる同一端末の同時ownership
- alert、webhook、APNs、maintenance機能の再導入
- RoomPlusまたはGrafanaのUI再設計
- Flower Careのon-device history削除

## API互換性

次の既存契約を維持します。

- `/api/devices`
- `/api/devices/{mac}/latest`
- `/api/devices/{mac}/series`
- latestの `device`、`ts`、`values`、`value_timestamps`
- seriesの実測時刻ベースの挙動
- `sensor_category` によるclient分類

内部status targetの追加は後方互換です。Admin UIは新しい `gatt_control` rowを通常の
collector statusとして表示できるようにします。

## リリースとデプロイ

各phaseで次を確認します。

1. local test、race test、build、OpenAPI lintを通します。
2. home-metricsのCIとContainer workflowを完了させます。
3. published image digestをioslab-docs/servicecoreへ反映します。
4. servicecore checkとnms4 deployを完了させます。
5. nms4でcontainer digest、collector logs、`collector_status`、最新telemetryを確認します。
6. migrationを含むphaseでは、適用結果と保証境界を追加確認します。

Phase 1の本番確認では、MQTTと各GATT端末のstatusが独立していること、全sensorのminute
データが継続していること、`conn busy` が再発していないことを重点的に確認します。

## 本番検証結果

2026-06-28にCI、Container workflow、servicecore check、nms4 deployの完了後、次を
確認しました。

- nms4のAPI、DB maintenance、Cisco Sensor Connect、Nature Remo、APC UPS、ECHONET
  collectorが公開済みimageで稼働しています。
- migration `0017_add_weighted_rollup_counts` が適用されています。
- `rollup_accuracy_state.accuracy_cutoff` は `2026-06-15 09:00:00+09:00` です。
- 1時間rollupにmetric別countが保存され、12時間・1日rollupも正常に再生成されました。
- Cisco Sensor Connectの10端末すべてでminute telemetryが継続しています。
- MQTT targetはfailureなしで更新され、API healthは `status=ok`、stale collectorは0です。
- collectorログに `conn busy`、ownership conflict、panicはありません。
- Cisco Spaces collectorは停止状態を維持し、active collector statusの集計対象外です。

GATTは24時間に1回の端末別scheduleで動作します。デプロイ直後に未実行の端末はstatus rowを
持ちません。実行後は正規化MACごとの `gatt_control` rowへ成功、データ取得、失敗を記録し、
MQTT targetとは独立してhealthを評価します。この分離と部分成功時の状態遷移は自動テストでも
検証します。

## 最終完了条件

- collector間でdevice ownershipとminute merge規則が統一されています。
- DB connection lifecycleがgoroutine lifecycleと一致しています。
- MQTTと端末別GATT statusが独立しています。
- sensor設定の無効化と再有効化がDBへ正しく反映されます。
- IoT collectorの責務が分割され、各機能を独立してテストできます。
- metric追加時の変更漏れをcontract testで検出できます。
- migration完了後、`accuracy_cutoff` 以降のrollupがmetric別countによる正確な加重平均です。
- README、API、release、運用文書が実装と一致しています。
- CI/CDとnms4 deployment verificationが再現可能です。
