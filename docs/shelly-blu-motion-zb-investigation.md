# Shelly BLU Motion ZB の BLE 調査

調査日: 2026-11-14（JST）。pve2 の BlueZ を使って探索した。実機確認とメーカー仕様を区別して記載する。

## 実機の特定

| 項目 | 実測結果 |
| --- | --- |
| 探索ホスト | `pve2` |
| アダプター | `hci0` / `08:9D:F4:38:BD:4F` |
| センサー名 | `SBMO-103Z` |
| BLE MAC | `FC:4D:6A:26:53:E3` |
| RSSI | 発見時 `-60 dBm` |
| Service UUID | `0000fcd2-0000-1000-8000-00805f9b34fb`（BTHome v2） |
| Manufacturer ID | `0x0BA9`（Shelly / Allterco） |
| メーカー広告の機種 ID | `0x0013`（BLU Motion ZB と一致） |
| 広告暗号化 | 受信した広告は暗号化なし |

最初の探索では対象を受信しなかったが、ユーザーによる操作後に発見した。BLE 広告を読むためのペアリングは不要だった。

2026-11-14 15:36:35 JST に BlueZ キャッシュから取得した ServiceData:

```text
44 00 08 01 64 3a 01 64 01
```

| バイト列 | 意味 |
| --- | --- |
| `44` | BTHome v2、暗号化なし、イベント駆動フラグあり |
| `00 08` | packet ID = 8 |
| `01 64` | 電池残量 = 100% |
| `3a 01` | ボタンの短押し1回 |
| `64 01` | 明るさ = twilight（薄明） |

直前には packet ID = 7 の同形式広告も取得した。メーカー広告は次のとおり。

```text
01 01 00 0b 13 00 0a e3 53 26 6a 4d fc
```

`0b 13 00` は機種 ID、`0a` に続く6バイトは MAC の little-endian 表現である。

15:40:27 JST の再探索でも `44000c01643a016401` を受信した。packet ID = 12 で、同じ電池・短押し・薄明を示した。RSSI は `-45`〜`-46 dBm`。この初回調査では人感 `0x21` の検出・解除広告は確認できなかった。途中で探索停止を確認したため、最後は `StartDiscovery` 後に `Discovering=true` を確認して再探索した。後述の長時間観測では人感を受信できた。

## BLE capability（公式仕様）

[BLU Motion ZB 技術仕様](https://shelly-api-docs.shelly.cloud/docs-ble/Devices/BLU_ZB/BluMotionZB/)による。

| 項目 | 広告オブジェクト | 内容 |
| --- | --- | --- |
| 人感 | `0x21` | `0`: clear、`1`: motion detected |
| 明るさ | `0x64` | `0`: dark、`1`: twilight、`2`: bright |
| 電池残量 | `0x01` | 0–100% |
| ボタン | `0x3A` | 短押しイベント |
| 機種・FW | `0xF0` / `0xF1` / `0xF2` | 機種 ID / FW バージョン。別の周期広告 |

ZB 版の広告は明るさを3段階で送る。lux 数値の `0x05` は本機の広告仕様に記載がなく、`0x64` を既存の `lux` 列に入れてはいけない。温湿度の取得も仕様にない。

人感は検出開始と解除で広告される。保持時間は既定60秒で、動きが続くと延長され、その間の動きごとに新規イベントを送る動作ではない。通常の周期広告は900秒、beacon mode 有効時は30秒。機種・FW 広告は120分周期とされる。これらは実機の周期測定値ではない。

設定用 GATT には保持時間、検知感度、LED、beacon mode、Zigbee 有効化、明るさ閾値、最新 BTHome sample がある。閾値の既定値は約50/500 lux。初回の接続試行では BlueZ が対象を `not available` と返したが、後述の追加診断で設定値・FW・GATT 一覧を取得できた。

[共通仕様](https://shelly-api-docs.shelly.cloud/docs-ble/common/)では設定アクセスにペアリングが必要。設定調査時は本機のボタン4回で BLE ペアリングモードに入れる。設定アクセスは広告の受信とは別に扱う。

## home-metrics への組み込み判断

現状のまま機器登録するだけでは対応できない。

- `cmd/hm-ble-collector/main.go` に BTHome v2 デコーダーがない。Service UUID `0xFCD2` でデコーダーを選び、object ID に従って読む処理が必要。
- `internal/sensor/types.go`、`internal/sensor/metrics.go`、DB に人感・3段階明るさ・ボタンイベントの表現がない。電池と RSSI は既存項目を使える。
- 現 collector は2秒ごとの `GetManagedObjects` で BlueZ キャッシュを読み、現在時刻を付けている。イベントセンサーでは未受信の期間も新しい観測として扱う危険がある。`PropertiesChanged` / `InterfacesAdded` を用い、実受信時刻と値の更新を区別する。
- packet ID を使って短時間の同一広告の繰り返しを排除する。ただし8bitカウンターの循環・再起動・別種パケットに注意し、ID 単独を永続的な一意キーにしない。
- 分単位の平均値だけでは検出と解除の順序を失う。人感は時刻付きの状態遷移として保存し、最新状態とイベント履歴を扱う構成が適する。PIR の motion clear を在室人数ゼロの証明として扱わない。
- 鮮度監視は通常周期900秒とイベント駆動を考慮する。キャッシュの再読込で last-seen を更新しない。

まず pve2 で広告を収集し、人感・電池・明るさを ingest する方針が適する。Zigbee coordinator やクラウドは BLE 広告収集に不要。本調査ではアプリ実装、DB 登録、サービス配備、センサー設定変更を行っていない。

## Web 追加調査（2026-11-14）

### 指定記事と実機調査の関係

[Scargill の実機記事](https://tech.scargill.net/brand-new-shelly-blu-motion-zb/)は2026-08-05公開、08-21更新。主に Zigbee2MQTT での試用報告である。

- ボタン5回で Zigbee の参加に成功した。
- 当初は電池100%以外の変化が見えず、ボタン操作と待機、向きの変更を経て occupancy の検出・解除を確認した。
- 明るさは閾値を変更しても dark のままで、記事末尾時点で解決していない。

これは著者の環境での観測であり、BLE 人感広告の検証結果ではない。今回の pve2 での人感未受信と同じ原因とは判断できず、「ボタンを押すと PIR が有効になる」という仕様の根拠にもならない。

### 旧 BLU Motion と ZB 版の区別

| 項目 | BLU Motion（旧モデル） | BLU Motion ZB（今回） |
| --- | --- | --- |
| 型番 | `SBMO-003Z` | `SBMO-103Z` |
| 明るさの BLE オブジェクト | `0x05`: 数値 lux | `0x64`: dark / twilight / bright |
| テスト操作の記載 | 2回押しで1分間の motion test mode | 参照した ZB 版技術仕様には同操作の記載なし |

出典: [旧モデル仕様](https://shelly-api-docs.shelly.cloud/docs-ble/Devices/BLU/motion/)、[ZB 版仕様](https://shelly-api-docs.shelly.cloud/docs-ble/Devices/BLU_ZB/BluMotionZB/)。旧モデルの pulse count 設定やテスト操作を ZB 版へ無条件に適用しない。

### 公開実装と既知報告

[Zigbee2MQTT の機器資料](https://www.zigbee2mqtt.io/devices/SBMO-103Z.html)では occupancy、battery_low、battery、light_level、暗・明閾値、感度、identify を公開している。light_level は3段階で、lux は閾値の単位である。Zigbee 側には取得項目がそろうが、それらすべてが BLE 広告にも同じ形で載るわけではない。

[ZHA issue #5158](https://github.com/zigpy/zha-device-handlers/issues/5158)には人感は動くが明るさが表示されないという利用者の一次報告がある。調査時点で issue は Open。独自クラスター `0xFC21` を含む機器情報が添付されており、明るさは受信側の対応状況も確認すべきである。ただし、この報告は指定記事の dark 固定や BLE 未受信の原因を確定するものではない。

[BLU+ZB changelog](https://shelly-api-docs.shelly.cloud/docs-ble/changelog_mg27/)では2026-04-17の `BLU_FW 1.2.15` に Motion ZB の追加が記載される。参照した変更履歴から、本機の人感未受信を直す特定 FW は確認できなかった。別機種の修正やページ最上部のバージョンを、本機の最新 FW と解釈しない。

### Home Assistant Community の指定スレッド

[指定投稿 #15 と前後の議論](https://community.home-assistant.io/t/shelly-blu-motion-is-diggity-bomb/676200)は2024年の旧 BLU Motion の話で、今回の ZB 版の検証ではない。個別投稿 URL の取得は失敗したが、スレッド全体の本文で確認した。

- #15 は、クラウドを無効にした環境で LED と blind time を変更する方法を質問している。
- #17 は Shelly BLE Debug アプリでの設定を紹介し、#19 の質問者は同アプリで解決しクラウドアカウントも不要だったと報告している。
- この報告は、広告の収集と設定用 BLE 接続を分ける構成の参考になる。ZB 版でアプリの全設定が同様に利用できるかは別途実機確認する。

[メーカー共通仕様](https://shelly-api-docs.shelly.cloud/docs-ble/common/)でも BLE Debug による FW 更新とペアリング後の設定アクセスが案内され、[ZB 版仕様](https://shelly-api-docs.shelly.cloud/docs-ble/Devices/BLU_ZB/BluMotionZB/)には LED、保持時間、感度などの GATT 項目が記載されている。したがって home-metrics の常時収集は pve2、初期設定・診断は BLE アプリまたはペアリングした GATT クライアント、という役割分担が候補になる。

### 次回の実機検証

1. pve2 で探索が継続していることを確認し、ボタン広告が安定して届く状態を作る。
2. センサーの視野を無人にして保持時間が過ぎるのを待ち、その後に横切って `0x21=1`、離れて `0x21=0` を確認する。保持時間の実値は未取得であり、60秒は既定値にすぎない。
3. 人感広告が出ない場合は、BLE ペアリングモードで FW、感度、保持時間、BTHome sample を読み取り、広告と突き合わせる。必要な場合だけ設定変更や FW 更新を別途検討する。
4. 明るさは暗所・通常照明・強い光の各条件で `0x64` を比較し、必要なら閾値の実値も読む。

## BLE / GATT 追加診断（2026-11-14）

ユーザーがボタンを4回押し、BLE ペアリングモードに入れた後、pve2 の BlueZ D-Bus API から接続した。スマートフォンの Shelly BLE Debug アプリ自体を操作したものではなく、GATT を直接読んだ診断である。

接続中の状態は `Connected=1`、`ServicesResolved=1`、`Paired=0`、`Bonded=0`。今回の実機・モードではボンディングせずに次の読み取りが成功した。共通仕様のペアリング記述と区別し、設定書き込みも同様に可能とは判断しない。

| 項目 | 実機の読み取り値 |
| --- | --- |
| Manufacturer | `Shelly` |
| Firmware revision | `260511/v1.2.20@10419653` |
| 人感保持時間 | `60` 秒（`3c00`） |
| 検知感度 | `1`: medium（中） |
| LED level | `1`: low（低輝度） |
| Beacon mode | `0`: disabled |
| Zigbee | `0`: disabled |
| Dark threshold | `50`（`3200`、仕様上 ~lux） |
| Bright threshold | `500`（`f401`、仕様上 ~lux） |
| BTHome sample | `44000d5a10ffa90b01` |

GATT の Shelly service `de8a5aac-a99b-c315-0c80-60d4cbb51225` と Device Information service `0x180A` を確認した。BTHome sample は従来のボタン・人感パケットと異なる内容のため、生データとして記録し、人感状態には解釈していない。

設定の書き込み、ペアリングの実行、暗号鍵の読み取り、FW 更新は行っていない。診断後に `Disconnect` が成功した。beacon mode が無効であることは通常周期900秒という仕様と整合するが、人感広告を未受信だった原因はまだ特定できていない。

## 長時間広告観測と対応範囲の確定

2026-11-14 15:50:13〜16:07:29 JST に追加観測し、人感の検出・解除を受信できた。ユーザーが十分なデータを得たと判断したため、当初の30分予定を約17分16秒で終了した。

観測した人感状態は `0 → 1 → 0 → 1 → 0`。人感広告には電池100%が含まれていた。BlueZ 更新256件、異なる payload 7種類であり、更新件数は人感イベント件数とは異なる。探索停止を含むため連続30分の観測とは扱わない。

対応対象はユーザーの指示により **人感の検出・解除イベントと電池残量の保存** とする。明るさ・ボタンの永続化は対象外。具体的な Cisco AP での収集・実装計画と実測時刻は [Cisco AP 対応計画](shelly-blu-motion-zb-cisco-plan.md) にまとめた。

## 参照

- [Shelly BLU Motion ZB 技術仕様](https://shelly-api-docs.shelly.cloud/docs-ble/Devices/BLU_ZB/BluMotionZB/)
- [Shelly BLE 共通仕様](https://shelly-api-docs.shelly.cloud/docs-ble/common/)
- [BTHome v2 フォーマット](https://bthome.io/format/)
