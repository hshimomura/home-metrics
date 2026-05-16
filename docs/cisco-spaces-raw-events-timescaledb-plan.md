# Cisco Spaces Raw Events on TimescaleDB 実行プラン

## 結論

Cisco Spaces Firehose の raw JSON 保存、1分値生成、debug / replay / export を
目的にするなら、現時点では MongoDB を追加せず、既存の PostgreSQL + TimescaleDB に
raw event store を作る。

理由:

- raw 保存期間は 14日で十分なので、長期 document store を別運用する必要が薄い。
- 正規化先の `sensor_minute` / `devices` と同じ DB に置けるため、trace と replay が簡単。
- 既存の `hm-db-migrate`、backup、deploy、health check の運用に乗せられる。
- TimescaleDB の hypertable / retention policy で 14日保持を自然に表現できる。
- Cisco TAC 向け export は `jsonb` から NDJSON を出せば足りる。

MongoDB は、raw JSON の長期保存、巨大 volume、柔軟な探索が主業務化した段階で
再検討する。

## 目的

この設計で満たしたいこと:

- Cisco Spaces から受信した raw event を改変せず 14日保存する。
- Firehose への接続は production receiver 1つに寄せ、debug が Firehose に直接接続しない。
- raw event から現在の 1分値を生成し、既存 `sensor_minute` に保存する。
- decoder 修正後に raw event を replay できる。
- Cisco TAC case 用に、指定期間 / device / record の raw JSON を export できる。
- raw event と normalized reading の対応関係を追える。

## 全体構成

```text
Cisco Spaces Firehose
        ↓
hm-cisco-spaces-receiver
        ↓ raw JSON + metadata
cisco_spaces_raw_events  -- TimescaleDB hypertable, 14日保持
        ↓
hm-cisco-spaces-processor
        ↓
sensor_minute / devices
        ↓
API / UI / alert-worker

debug / replay / export tools
        ↑
cisco_spaces_raw_events
```

## DB モデル

### cisco_spaces_raw_events

Firehose から受け取った event を raw JSON のまま保存する。

```sql
CREATE TABLE IF NOT EXISTS cisco_spaces_raw_events (
    received_at timestamptz NOT NULL DEFAULT now(),
    id bigserial NOT NULL,
    record_uid text,
    record_timestamp timestamptz,
    event_type text,
    device_mac text,
    device_id text,
    device_label text,
    location_id text,
    map_id text,
    payload jsonb NOT NULL,
    payload_sha256 text NOT NULL,
    process_status text NOT NULL DEFAULT 'pending'
        CHECK (process_status IN ('pending', 'processed', 'failed', 'ignored')),
    processed_at timestamptz,
    process_error text,
    processor_version text,
    PRIMARY KEY (received_at, id)
);

SELECT create_hypertable('cisco_spaces_raw_events', 'received_at', if_not_exists => true);
```

主な考え方:

- `payload` は Cisco Spaces から受け取った event object をそのまま保存する。
- `record_uid` が存在する場合は dedupe の主な材料にする。ただし hypertable 上の
  global unique index にはしない。
- `payload_sha256` は `record_uid` が無い event の重複検出と case export の照合に使う。
- `device_mac`, `device_id`, `location_id` などは検索用に raw JSON から抽出して持つ。
- 抽出 metadata は検索・index・admin 表示用であり、source of truth は常に `payload`。
  抽出に失敗した field は `NULL` のままにし、raw event 自体は保存する。
- `process_status` は production processor の処理結果を表す。

### metadata 抽出 mapping

既存 Cisco Spaces collector と同じ payload shape を前提に、検索用 metadata を
best effort で抽出する。存在しない path は `NULL` にする。

| raw table column | JSON path | 備考 |
| --- | --- | --- |
| `record_uid` | `recordUid` | 無い event も許容する |
| `record_timestamp` | `recordTimestamp` | epoch millis を `timestamptz` に変換する |
| `event_type` | `eventType` | 主に `IOT_TELEMETRY` |
| `device_id` | `iotTelemetry.deviceInfo.deviceId` | Cisco Spaces 側の device id |
| `device_mac` | `iotTelemetry.deviceInfo.deviceMacAddress` | 正規化時の主 key |
| `device_label` | `iotTelemetry.deviceInfo.label` | 表示名候補 |
| `location_id` | `iotTelemetry.location.locationId` または `iotTelemetry.detectedPosition.locationId` | best effort |
| `map_id` | `iotTelemetry.detectedPosition.mapId` | best effort |

既存 decoder が metric 抽出で見る主な JSON path:

| metric / field | JSON path |
| --- | --- |
| temperature | `iotTelemetry.temperature.temperatureInCelsius` |
| humidity | `iotTelemetry.humidity.humidityInPercentage` |
| pressure | `iotTelemetry.airPressure.pressure` |
| CO2 | `iotTelemetry.carbonEmissions.co2Ppm` |
| illuminance | `iotTelemetry.illuminance.value` / `iotTelemetry.illuminance.unit` |
| battery | `iotTelemetry.battery.value` |
| TVOC | `iotTelemetry.tvoc.valueInPpb` |

processor 分離後も、正規化処理はまず `payload` を入力にする。抽出済み metadata は
query / filter / export の補助として使い、metric 計算の唯一の入力にはしない。

### index

最初は最小限にする。

```sql
CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_record_uid_idx
    ON cisco_spaces_raw_events (record_uid)
    WHERE record_uid IS NOT NULL;

CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_device_received_idx
    ON cisco_spaces_raw_events (device_mac, received_at DESC);

CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_status_received_idx
    ON cisco_spaces_raw_events (process_status, received_at);

CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_received_idx
    ON cisco_spaces_raw_events (received_at DESC);
```

TimescaleDB hypertable の unique index は partitioning column を含む必要がある。
`received_at` hypertable で `UNIQUE (record_uid)` を作ると migration で失敗し得るため、
raw table には通常 index だけを置く。

global dedupe が必要になった場合は、hypertable とは別に dedupe table を作る。

```sql
CREATE TABLE IF NOT EXISTS cisco_spaces_raw_event_dedup (
    record_uid text PRIMARY KEY,
    raw_received_at timestamptz NOT NULL,
    raw_id bigint NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now()
);
```

receiver は raw insert と dedupe insert を同一 transaction で行い、
`record_uid` が既に存在する場合は duplicate として扱う。初期 PR では
global dedupe table は必須にしない。

`payload` の GIN index は初期導入では作らない。必要な debug query が固まってから
追加する。raw JSON 全体への GIN index は disk 使用量と write cost が増えやすい。

### retention

raw event は 14日保持する。

```sql
SELECT add_retention_policy('cisco_spaces_raw_events', INTERVAL '14 days');
```

retention は `hm-db-migrate` か追加 migration で設定する。既存 production DB では
`/docker-entrypoint-initdb.d/10-schema.sql` は再実行されないため、新規 migration として
追加する。

### raw event と normalized reading の対応

既存 `sensor_minute` の primary key は `(ts, mac)` なので、複数 raw event が同じ minute に
集約される。1対1対応ではなく、trace 用の別 table を持つ。

```sql
CREATE TABLE IF NOT EXISTS cisco_spaces_processing_events (
    id bigserial PRIMARY KEY,
    raw_received_at timestamptz NOT NULL,
    raw_id bigint NOT NULL,
    output_ts timestamptz,
    output_mac text,
    output_metric text,
    output_table text NOT NULL DEFAULT 'sensor_minute',
    processor_run_id text,
    processor_version text,
    status text NOT NULL CHECK (status IN ('processed', 'ignored', 'failed')),
    reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (raw_received_at, raw_id)
        REFERENCES cisco_spaces_raw_events (received_at, id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS cisco_spaces_processing_events_raw_idx
    ON cisco_spaces_processing_events (raw_received_at, raw_id);
```

この table は「この raw event がどの normalized row に寄与したか」を調べるためのもの。
1 raw event から複数 output row / metric / replay run が出る可能性があるため、
primary key は独立した `id` にする。必須ではないが、TAC case や decoder regression
調査で効く。

## component 分割

### hm-cisco-spaces-receiver

責務:

- Cisco Spaces Firehose に接続する唯一の production process。
- DB advisory lock で二重起動を防ぐ。
- 受信 event を `cisco_spaces_raw_events` に保存する。
- raw 保存に成功した event だけを processor 対象にする。
- raw 保存失敗時の failure policy を明確にする。

failure policy:

- DB が一時的に落ちている場合は同期 retry / reconnect する。
- DB insert 失敗を silently ignore しない。失敗継続は `collector_status` と health alert で
  data-loss risk として通知する。
- HTTP stream から event を読んだ後、DB insert 前に process が落ちるケースまでは
  初期実装では完全には防がない。
- 完全な no-drop を目指す場合は local disk spool が必要になる。これは初期 PR の範囲外。

### hm-cisco-spaces-processor

責務:

- `cisco_spaces_raw_events` の `pending` event を読む。
- 既存 decoder を使って `sensor_minute` / `devices` に書く。
- 処理結果を `process_status`, `processed_at`, `process_error` に保存する。
- `cisco_spaces_processing_events` に trace を保存する。

処理単位:

- 通常運用は小さな batch で `pending` を処理する。
- `FOR UPDATE SKIP LOCKED` を使えば、将来 processor を複数に増やせる。
- ただし初期導入では processor は 1つでよい。

例:

```sql
SELECT received_at, id, payload
FROM cisco_spaces_raw_events
WHERE process_status = 'pending'
ORDER BY received_at, id
LIMIT 500
FOR UPDATE SKIP LOCKED;
```

## 1分値生成

現在の collector は Firehose event を直接 decode して `sensor_minute` に書いている。
raw store 導入後は、同じ decode logic を processor に移す。

初期移行では挙動を変えすぎない:

- upload interval / minute bucket の既存挙動を維持する。
- `device_id` と `deviceMacAddress` の整合性 check を維持する。
- label 更新 logic を維持する。
- `no_metric_values`, `device_id_mismatch`, `upload_interval` などの ignore reason を
  `process_status='ignored'` と `process_error` または trace reason に残す。

## replay

decoder 修正後に過去 raw event を再処理できるようにする。

CLI 例:

```sh
hm-cisco-spaces-replay \
  --from "2026-05-15T22:00:00+09:00" \
  --to "2026-05-15T23:00:00+09:00" \
  --mac "00:fa:b6:07:de:49" \
  --dry-run
```

replay mode:

- `--dry-run`: normalized table には書かず、decode result / ignore reason を出す。
- `--write`: `sensor_minute` を再作成する。既存 row の上書き方針を明示する。
- `--processor-version`: replay 時の decoder version を記録する。

注意:

- replay は既存 `sensor_minute` の値を変える可能性があるため、最初は dry-run を主にする。
- write replay を入れる場合は対象期間 / MAC を必須にする。
- replay 後は `hm-db-maint` の rollup 更新が必要になる。

## Cisco TAC case export

raw JSON は Cisco TAC に提出できる形で export する。

CLI 例:

```sh
hm-cisco-spaces-export-raw \
  --from "2026-05-15T22:00:00+09:00" \
  --to "2026-05-15T23:00:00+09:00" \
  --mac "00:fa:b6:07:de:49" \
  --redact \
  --out cisco-spaces-case-20260515.ndjson
```

export format:

- NDJSON を標準にする。
- 1行に `received_at`, extracted metadata, `payload` を含める。
- redaction option で tenant id / partner tenant id / location name などを伏せる。
- `payload_sha256` を含め、提出後の照合ができるようにする。

redaction 方針:

- API token は raw payload に通常含まれないが、念のため export tool 側で禁止 key を持つ。
- tenant id、location tree、device label、MAC address は case の目的に応じて残すか伏せる。
- Cisco TAC に調査してもらうには device MAC / record uid / timestamp は残した方が有用。

## migration plan

Phase 1: schema

- `cisco_spaces_raw_events` を migration で追加する。
- hypertable 化、index、14日 retention を追加する。
- `cisco_spaces_processing_events` は Phase 1 で一緒に入れるか、processor 実装時に入れる。

Phase 2: receiver

- 既存 `hm-cisco-spaces-collector` を `receiver + inline processor` に分ける準備をする。
- 最初は raw 保存後に同じ process 内で decode してもよい。
- raw 保存が確認できたら processor を別 binary に分離する。

Phase 3: processor

- `hm-cisco-spaces-processor` を追加する。
- processor は raw event store だけを入力にする。
- production write path を processor 側へ移す。

Phase 4: debug / replay / export

- `hm-cisco-spaces-export-raw` を追加する。
- `hm-cisco-spaces-replay --dry-run` を追加する。
- 必要になってから `--write` replay を追加する。

Phase 5: admin visibility

- `/admin` に raw event ingest rate、pending / failed process count、oldest pending age を表示する。
- `/api/admin/cisco-spaces-raw-events/summary` のような summary endpoint を追加する。
- failed processing が一定以上なら health alert にする。

## health monitoring

追加する collector status:

```text
hm-cisco-spaces-receiver / cisco_spaces_firehose / default
hm-cisco-spaces-processor / cisco_spaces_raw_events / default
```

追加する health alert:

- receiver heartbeat stale
- raw event data stale
- processor heartbeat stale
- pending raw events stale
- process_status=failed が増えている

admin webhook payload には以下を入れる:

- oldest pending raw event age
- failed raw event count
- latest received_at
- latest processed_at
- suggested action: export raw events / inspect processor error

## 運用コマンド例

直近1時間の raw event 件数:

```sql
SELECT date_trunc('minute', received_at) AS minute, count(*)
FROM cisco_spaces_raw_events
WHERE received_at >= now() - interval '1 hour'
GROUP BY minute
ORDER BY minute DESC;
```

特定 MAC の raw event:

```sql
SELECT received_at, record_uid, event_type, device_id, location_id, payload
FROM cisco_spaces_raw_events
WHERE device_mac = '00:fa:b6:07:de:49'
  AND received_at >= now() - interval '6 hours'
ORDER BY received_at DESC
LIMIT 100;
```

processor failure:

```sql
SELECT received_at, id, device_mac, process_error
FROM cisco_spaces_raw_events
WHERE process_status = 'failed'
ORDER BY received_at DESC
LIMIT 100;
```

## 判断基準

PostgreSQL + TimescaleDB を続ける条件:

- raw retention が 14日程度。
- export / replay は時間範囲と device key が主な検索軸。
- production DB と raw store を同居させても disk / backup が許容範囲。
- raw JSON 全文探索が頻繁ではない。

MongoDB を再検討する条件:

- raw retention を長期化したい。
- raw JSON の ad-hoc exploration が日常的になる。
- raw event volume が大きく、PostgreSQL backup / vacuum / storage への影響が目立つ。
- production normalized DB と raw event store の障害分離が必要になる。

## 最初に作る最小 PR

最小 PR は MongoDB を入れず、次だけに絞る。

1. `cisco_spaces_raw_events` migration
2. raw event insert helper
3. `hm-cisco-spaces-collector` が受信 event を raw 保存する
4. raw 保存後に既存 decode / write path を実行する
5. 14日 retention
6. raw export CLI の dry-run または read-only export

この段階では processor 分離までは必須にしない。まず raw JSON が durable に残り、
Cisco TAC case 用に export できる状態を作る。
