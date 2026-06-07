package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDecodeDataBatchWithServiceData(t *testing.T) {
	ts := time.Date(2026, 6, 2, 12, 34, 0, 0, time.UTC)
	payload := protoMessage(1, protoFields(
		protoField(1, 2, protoString("AA:BB:CC:DD:EE:01")),
		protoField(2, 2, protoBytes([]byte{0xa1, 0x01, 88, 24, 0, 55})),
		protoMessage(3, protoField(
			1, 0, protoVarint(uint64(ts.Unix())),
		)),
		protoMessage(12, protoFields(
			protoField(1, 2, protoString("AA:BB:CC:DD:EE:01")),
			protoField(2, 0, protoVarint(uint64(uint32(0xffffffc4)))),
		)),
	))

	readings, err := decodeDataBatch(payload, map[string]targetDevice{
		"aa:bb:cc:dd:ee:01": {MAC: "aa:bb:cc:dd:ee:01", Label: "Desk"},
	})
	if err != nil {
		t.Fatalf("decodeDataBatch: %v", err)
	}
	if len(readings) != 1 {
		t.Fatalf("readings=%d, want 1", len(readings))
	}
	got := readings[0]
	if got.SensorMAC != "aa:bb:cc:dd:ee:01" || got.Label != "Desk" {
		t.Fatalf("target = %s %s", got.SensorMAC, got.Label)
	}
	if got.TemperatureC == nil || *got.TemperatureC != 24 {
		t.Fatalf("temperature=%v, want 24", got.TemperatureC)
	}
	if got.HumidityPercent == nil || *got.HumidityPercent != 55 {
		t.Fatalf("humidity=%v, want 55", got.HumidityPercent)
	}
	if got.BatteryPercent == nil || *got.BatteryPercent != 88 {
		t.Fatalf("battery=%v, want 88", got.BatteryPercent)
	}
	if got.RSSI == nil || *got.RSSI != -60 {
		t.Fatalf("rssi=%v, want -60", got.RSSI)
	}
}

func TestDecodeBLEPayloadExtractsAdvertisementServiceData(t *testing.T) {
	adv := []byte{
		2, 0x01, 0x06,
		9, 0x16, 0xe1, 0xff, 0xa1, 0x01, 90, 23, 0, 51,
	}
	got := decodeBLEPayload(adv)
	if got.TemperatureC == nil || *got.TemperatureC != 23 {
		t.Fatalf("temperature=%v, want 23", got.TemperatureC)
	}
	if got.HumidityPercent == nil || *got.HumidityPercent != 51 {
		t.Fatalf("humidity=%v, want 51", got.HumidityPercent)
	}
}

func TestDecodeMinewS1ScanOnlyAdvertisementSample(t *testing.T) {
	ts := time.Date(2026, 6, 2, 15, 14, 16, 0, time.UTC)
	adv := mustHex(t, "0201061b166afe03050610ff2098041103ffff041600ffff03133a1a02123b")
	payload := protoMessage(1, protoFields(
		protoField(1, 2, protoString("d96231e7-13b4-4ccd-bafa-ec5c60b95c88")),
		protoField(2, 2, protoBytes(adv)),
		protoMessage(3, protoField(
			1, 0, protoVarint(uint64(ts.Unix())),
		)),
		protoField(4, 2, protoString("84:5a:3e:d6:b7:80")),
		protoMessage(12, protoFields(
			protoField(1, 2, protoString("00:FA:B6:07:DE:4B")),
			protoField(2, 0, protoVarint(uint64(uint32(0xffffffbc)))),
		)),
	))
	const samplePayloadHex = "0a7d0a2464393632333165372d313362342d346363642d626166612d656335633630623935633838121f0201061b166afe03050610ff2098041103ffff041600ffff03133a1a02123b1a0608c8e6fbd006221138343a35613a33653a64363a62373a383062190a1130303a46413a42363a30373a44453a344210bcffffff0f"
	if hex.EncodeToString(payload) != samplePayloadHex {
		t.Fatalf("payload hex=%s, want %s", hex.EncodeToString(payload), samplePayloadHex)
	}

	readings, err := decodeDataBatch(payload, map[string]targetDevice{
		"00:fa:b6:07:de:4b": {MAC: "00:fa:b6:07:de:4b", Label: "Living"},
	})
	if err != nil {
		t.Fatalf("decodeDataBatch: %v", err)
	}
	if len(readings) != 1 {
		t.Fatalf("readings=%d, want 1", len(readings))
	}
	got := readings[0]
	if got.SensorMAC != "00:fa:b6:07:de:4b" || got.Label != "Living" {
		t.Fatalf("target = %s %s", got.SensorMAC, got.Label)
	}
	if got.TemperatureC == nil || *got.TemperatureC != 26.23 {
		t.Fatalf("temperature=%v, want 26.23", got.TemperatureC)
	}
	if got.HumidityPercent == nil || *got.HumidityPercent != 59 {
		t.Fatalf("humidity=%v, want 59", got.HumidityPercent)
	}
	if got.BatteryPercent != nil {
		t.Fatalf("battery=%v, want nil", got.BatteryPercent)
	}
	if got.RSSI == nil || *got.RSSI != -68 {
		t.Fatalf("rssi=%v, want -68", got.RSSI)
	}

	battery := decodeBLEPayload(mustHex(t, "02010611166afe02800206530432355a6930303032"))
	if battery.BatteryPercent == nil || *battery.BatteryPercent != 83 {
		t.Fatalf("battery=%v, want 83", battery.BatteryPercent)
	}
}

func TestDecodeBLEPayloadExtractsEnvServiceData(t *testing.T) {
	adv := mustHex(t, "0201061b166afe0305177bd47b44041f071a0403ff00000313991303200a00")
	got := decodeBLEPayload(adv)
	if got.TemperatureC == nil || *got.TemperatureC != 19.6 {
		t.Fatalf("temperature=%v, want 19.6", got.TemperatureC)
	}
	if got.PressureHPa == nil || *got.PressureHPa != 1007.32 {
		t.Fatalf("pressure=%v, want 1007.32", got.PressureHPa)
	}
	if got.CO2PPM == nil || *got.CO2PPM != 1050 {
		t.Fatalf("co2=%v, want 1050", got.CO2PPM)
	}
	if got.Lux == nil || *got.Lux != 10 {
		t.Fatalf("lux=%v, want 10", got.Lux)
	}

	adv = mustHex(t, "0201061b166afe0305177bd47b44041f08070003ff00000313991303200a00")
	got = decodeBLEPayload(adv)
	if got.ETVOC == nil || *got.ETVOC != 7 {
		t.Fatalf("etvoc=%v, want 7", got.ETVOC)
	}
}

func TestDecodeBLEPayloadExtractsXiaomiFlowerCareServiceData(t *testing.T) {
	cases := []struct {
		name         string
		payloadHex   string
		assertMetric func(t *testing.T, got bleReading)
	}{
		{
			name:       "soil moisture",
			payloadHex: "020106030295fe131695fe71209800977d73147e855c0d0810010b",
			assertMetric: func(t *testing.T, got bleReading) {
				if got.SoilMoisturePercent == nil || *got.SoilMoisturePercent != 11 {
					t.Fatalf("soil moisture=%v, want 11", got.SoilMoisturePercent)
				}
				if got.HumidityPercent != nil {
					t.Fatalf("humidity=%v, want nil", got.HumidityPercent)
				}
			},
		},
		{
			name:       "conductivity",
			payloadHex: "020106030295fe141695fe71209800987d73147e855c0d0910024100",
			assertMetric: func(t *testing.T, got bleReading) {
				if got.ConductivityUSCM == nil || *got.ConductivityUSCM != 65 {
					t.Fatalf("conductivity=%v, want 65", got.ConductivityUSCM)
				}
			},
		},
		{
			name:       "temperature",
			payloadHex: "020106030295fe141695fe71209800997d73147e855c0d0410020601",
			assertMetric: func(t *testing.T, got bleReading) {
				if got.TemperatureC == nil || *got.TemperatureC != 26.2 {
					t.Fatalf("temperature=%v, want 26.2", got.TemperatureC)
				}
			},
		},
		{
			name:       "lux",
			payloadHex: "020106030295fe151695fe712098009a7d73147e855c0d071003be0700",
			assertMetric: func(t *testing.T, got bleReading) {
				if got.Lux == nil || *got.Lux != 1982 {
					t.Fatalf("lux=%v, want 1982", got.Lux)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeBLEPayload(mustHex(t, tc.payloadHex))
			tc.assertMetric(t, got)
		})
	}
}

func TestCollectorAggregatesSparseFlowerCareAdvertisements(t *testing.T) {
	window := time.Date(2026, 6, 6, 9, 10, 0, 0, time.UTC)
	c := &collector{windows: map[string]*aggregate{}}
	c.add(bleReading{
		TS:                  window.Add(5 * time.Second),
		SensorMAC:           "5c:85:7e:14:73:7d",
		Label:               "Blueberry1",
		IngestSource:        sensorConnectIngestSource,
		SensorTypeCode:      "xiaomi_flower_care",
		SensorCategory:      "plant",
		SoilMoisturePercent: floatPtr(11),
	})
	c.add(bleReading{
		TS:               window.Add(15 * time.Second),
		SensorMAC:        "5c:85:7e:14:73:7d",
		Label:            "Blueberry1",
		IngestSource:     sensorConnectIngestSource,
		SensorTypeCode:   "xiaomi_flower_care",
		SensorCategory:   "plant",
		ConductivityUSCM: floatPtr(65),
	})

	agg := c.windows["5c:85:7e:14:73:7d|"+window.Format(time.RFC3339)]
	if agg == nil {
		t.Fatal("aggregate not found")
	}
	if agg.IngestSource != sensorConnectIngestSource {
		t.Fatalf("ingest source=%q, want %q", agg.IngestSource, sensorConnectIngestSource)
	}
	if agg.SensorTypeCode != "xiaomi_flower_care" {
		t.Fatalf("sensor type code=%q, want xiaomi_flower_care", agg.SensorTypeCode)
	}
	if agg.SensorCategory != "plant" {
		t.Fatalf("sensor category=%q, want plant", agg.SensorCategory)
	}
	if got := nullableMedianFloat(agg.SoilMoisturePercent); got == nil || *got != 11 {
		t.Fatalf("soil moisture median=%v, want 11", got)
	}
	if got := nullableMedianFloat(agg.ConductivityUSCM); got == nil || *got != 65 {
		t.Fatalf("conductivity median=%v, want 65", got)
	}
	if len(agg.HumidityPercent) != 0 {
		t.Fatalf("humidity samples=%v, want none", agg.HumidityPercent)
	}
}

func TestDecodeFlowerCareRealtimeGATT(t *testing.T) {
	got, err := decodeFlowerCareRealtimeGATT(mustHex(t, "ed0003d31000001a1701023c00fb349b"))
	if err != nil {
		t.Fatalf("decodeFlowerCareRealtimeGATT: %v", err)
	}
	if got.TemperatureC == nil || *got.TemperatureC != 23.7 {
		t.Fatalf("temperature=%v, want 23.7", got.TemperatureC)
	}
	if got.Lux == nil || *got.Lux != 4307 {
		t.Fatalf("lux=%v, want 4307", got.Lux)
	}
	if got.SoilMoisturePercent == nil || *got.SoilMoisturePercent != 26 {
		t.Fatalf("soil moisture=%v, want 26", got.SoilMoisturePercent)
	}
	if got.ConductivityUSCM == nil || *got.ConductivityUSCM != 279 {
		t.Fatalf("conductivity=%v, want 279", got.ConductivityUSCM)
	}
}

func TestDecodeFlowerCareHistoryGATTMapsSecondsToMinute(t *testing.T) {
	hostReadTime := time.Date(2026, 6, 7, 12, 32, 30, 0, time.UTC)
	got, err := decodeFlowerCareHistoryGATT(
		mustHex(t, "a0224c00140100e90300000528000000"),
		4990145,
		hostReadTime,
	)
	if err != nil {
		t.Fatalf("decodeFlowerCareHistoryGATT: %v", err)
	}
	wantTS := time.Date(2026, 6, 7, 12, 23, 0, 0, time.UTC)
	if !got.TS.Equal(wantTS) {
		t.Fatalf("timestamp=%s, want %s", got.TS, wantTS)
	}
	if got.TemperatureC == nil || *got.TemperatureC != 27.6 {
		t.Fatalf("temperature=%v, want 27.6", got.TemperatureC)
	}
	if got.Lux == nil || *got.Lux != 1001 {
		t.Fatalf("lux=%v, want 1001", got.Lux)
	}
	if got.SoilMoisturePercent == nil || *got.SoilMoisturePercent != 5 {
		t.Fatalf("soil moisture=%v, want 5", got.SoilMoisturePercent)
	}
	if got.ConductivityUSCM == nil || *got.ConductivityUSCM != 40 {
		t.Fatalf("conductivity=%v, want 40", got.ConductivityUSCM)
	}
}

func TestDecodeFlowerCareHistoryGATTRejectsEmptySentinel(t *testing.T) {
	_, err := decodeFlowerCareHistoryGATT(
		mustHex(t, "ffffffffffffffffffffffffffffffff"),
		68760,
		time.Date(2026, 6, 7, 13, 4, 54, 0, time.UTC),
	)
	if err == nil {
		t.Fatal("decodeFlowerCareHistoryGATT error = nil, want invalid sentinel error")
	}
	if !strings.Contains(err.Error(), "exceeds device epoch") {
		t.Fatalf("error=%v, want exceeds device epoch", err)
	}
}

type fakeSensorMinuteExecer struct {
	sql   []string
	args  [][]any
	err   error
	calls int
}

func (f *fakeSensorMinuteExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	f.sql = append(f.sql, sql)
	f.args = append(f.args, args)
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestBackfillSensorMinuteReadingsUsesPostgresUpsertForMissingMetrics(t *testing.T) {
	db := &fakeSensorMinuteExecer{}
	ts := time.Date(2026, 6, 7, 12, 23, 25, 0, time.UTC)
	inserted, err := backfillSensorMinuteReadings(context.Background(), db, []bleReading{
		{
			TS:                  ts,
			SensorMAC:           "5c:85:7e:14:73:7d",
			TemperatureC:        floatPtr(27.6),
			Lux:                 floatPtr(1001),
			SoilMoisturePercent: floatPtr(5),
			ConductivityUSCM:    floatPtr(40),
		},
		{
			TS:        ts.Add(time.Minute),
			SensorMAC: "5c:85:7e:14:73:7d",
		},
	})
	if err != nil {
		t.Fatalf("backfillSensorMinuteReadings: %v", err)
	}
	if inserted != 1 || db.calls != 1 {
		t.Fatalf("inserted=%d calls=%d, want 1/1", inserted, db.calls)
	}
	sql := db.sql[0]
	for _, want := range []string{
		"INSERT INTO sensor_minute",
		"ON CONFLICT (ts, mac) DO UPDATE SET",
		"temperature_c = COALESCE(EXCLUDED.temperature_c, sensor_minute.temperature_c)",
		"lux = COALESCE(EXCLUDED.lux, sensor_minute.lux)",
		"soil_moisture_percent = COALESCE(EXCLUDED.soil_moisture_percent, sensor_minute.soil_moisture_percent)",
		"conductivity_us_cm = COALESCE(EXCLUDED.conductivity_us_cm, sensor_minute.conductivity_us_cm)",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL missing %q:\n%s", want, sql)
		}
	}
	args := db.args[0]
	if got, ok := args[0].(time.Time); !ok || !got.Equal(ts.Truncate(time.Minute)) {
		t.Fatalf("timestamp arg=%#v, want minute-truncated %s", args[0], ts.Truncate(time.Minute))
	}
	if args[1] != "5c:85:7e:14:73:7d" {
		t.Fatalf("mac arg=%#v", args[1])
	}
	if args[2] == nil || args[8] == nil || args[10] == nil || args[11] == nil {
		t.Fatalf("plant metric args missing: %#v", args)
	}
	if args[3] != nil || args[4] != nil || args[5] != nil {
		t.Fatalf("non-GATT metrics should remain nil for sparse backfill: %#v", args)
	}
}

func TestLoadTargetsParsesFlowerCareHistoryBackfillConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensors.json")
	if err := os.WriteFile(path, []byte(`{
		"devices": [{
			"mac": "5C:85:7E:14:73:7D",
			"label": "Blueberry1",
			"ingest_source": "cisco_sensor_connect",
			"sensor_type_code": "xiaomi_flower_care",
			"gatt_battery": {
				"enabled": true,
				"device_id": "device-1",
				"history_backfill": true,
				"max_history_entries": 12
			}
		}]
	}`), 0o600); err != nil {
		t.Fatalf("write sensors config: %v", err)
	}

	targets, err := loadTargets(path)
	if err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	target := targets["5c:85:7e:14:73:7d"]
	if target.GATTBattery == nil {
		t.Fatal("GATT battery config not parsed")
	}
	if !gattHistoryBackfillEnabled(target) {
		t.Fatal("history backfill should be enabled")
	}
	if got := gattHistoryMaxEntries(target); got != 12 {
		t.Fatalf("max history entries=%d, want 12", got)
	}
	if target.SensorCategory != "plant" {
		t.Fatalf("sensor category=%q, want plant", target.SensorCategory)
	}
}

func TestGATTHistoryBackfillDefaultsDisabledAndConservativeLimit(t *testing.T) {
	target := targetDevice{
		GATTBattery: &gattBatteryConfig{DeviceID: "device-1"},
	}
	if gattHistoryBackfillEnabled(target) {
		t.Fatal("history backfill should default to disabled")
	}
	if got := gattHistoryMaxEntries(target); got != defaultGATTHistoryEntries {
		t.Fatalf("max history entries=%d, want %d", got, defaultGATTHistoryEntries)
	}
}

func TestNormalizeSensorMetadataDefaultsFlowerCareCategory(t *testing.T) {
	if got := normalizeIngestSource(""); got != sensorConnectIngestSource {
		t.Fatalf("ingest source=%q, want %q", got, sensorConnectIngestSource)
	}
	if got := normalizeSensorCategory("", "xiaomi_flower_care"); got != "plant" {
		t.Fatalf("sensor category=%q, want plant", got)
	}
	if got := normalizeSensorCategory("custom", "xiaomi_flower_care"); got != "custom" {
		t.Fatalf("explicit sensor category=%q, want custom", got)
	}
}

func TestDecodeBLEPayloadIgnoresMarkersOutsideServiceData(t *testing.T) {
	adv := mustHex(t, "0201060313000011166afe02800206530432355a6930303032")
	got := decodeBLEPayload(adv)
	if got.TemperatureC != nil {
		t.Fatalf("temperature=%v, want nil", *got.TemperatureC)
	}
	if got.BatteryPercent == nil || *got.BatteryPercent != 83 {
		t.Fatalf("battery=%v, want 83", got.BatteryPercent)
	}
}

func TestCollectorFlushCompletedDeletesStoredWindow(t *testing.T) {
	window := time.Date(2026, 6, 3, 12, 34, 0, 0, time.UTC)
	c := &collector{windows: map[string]*aggregate{}}
	c.add(bleReading{
		TS:           window.Add(10 * time.Second),
		SensorMAC:    "00:fa:b6:07:de:4b",
		Label:        "Living",
		TemperatureC: floatPtr(26.5),
	})
	c.flushFn = func(ctx context.Context, agg *aggregate) (bool, error) {
		if agg.SensorMAC != "00:fa:b6:07:de:4b" || !agg.Window.Equal(window) {
			t.Fatalf("flush aggregate = %s %s", agg.SensorMAC, agg.Window)
		}
		return true, nil
	}

	flushed, err := c.flushCompleted(context.Background(), window.Add(time.Minute))
	if err != nil {
		t.Fatalf("flushCompleted: %v", err)
	}
	if flushed != 1 {
		t.Fatalf("flushed=%d, want 1", flushed)
	}
	if len(c.windows) != 0 {
		t.Fatalf("pending windows=%d, want 0", len(c.windows))
	}
}

func TestCollectorFlushCompletedKeepsWindowOnFailure(t *testing.T) {
	window := time.Date(2026, 6, 3, 12, 34, 0, 0, time.UTC)
	c := &collector{windows: map[string]*aggregate{}}
	c.add(bleReading{
		TS:           window.Add(10 * time.Second),
		SensorMAC:    "00:fa:b6:07:de:4b",
		Label:        "Living",
		TemperatureC: floatPtr(26.5),
	})
	c.flushFn = func(ctx context.Context, agg *aggregate) (bool, error) {
		return false, errors.New("database unavailable")
	}

	flushed, err := c.flushCompleted(context.Background(), window.Add(time.Minute))
	if err == nil {
		t.Fatal("flushCompleted error = nil, want error")
	}
	if flushed != 0 {
		t.Fatalf("flushed=%d, want 0", flushed)
	}
	if len(c.windows) != 1 {
		t.Fatalf("pending windows=%d, want 1", len(c.windows))
	}
	count, oldest := c.pendingSummary(window.Add(3 * time.Minute))
	if count != 1 || oldest != 3*time.Minute {
		t.Fatalf("pendingSummary=(%d,%s), want (1,3m0s)", count, oldest)
	}
}

func TestReadMQTTPacketRejectsOversizedPayload(t *testing.T) {
	var packet bytes.Buffer
	packet.WriteByte(0x30)
	writeRemainingLength(&packet, 5)
	packet.Write([]byte("hello"))

	_, _, err := readMQTTPacket(&packet, 4)
	if err == nil {
		t.Fatal("readMQTTPacket error = nil, want oversized packet error")
	}
	if !strings.Contains(err.Error(), "MQTT packet too large") {
		t.Fatalf("readMQTTPacket error = %v, want MQTT packet too large", err)
	}
}

func TestReadMQTTPacketAcceptsPayloadAtLimit(t *testing.T) {
	var packet bytes.Buffer
	packet.WriteByte(0x30)
	writeRemainingLength(&packet, 5)
	packet.Write([]byte("hello"))

	packetType, payload, err := readMQTTPacket(&packet, 5)
	if err != nil {
		t.Fatalf("readMQTTPacket: %v", err)
	}
	if packetType != 3 || string(payload) != "hello" {
		t.Fatalf("packet=(%d,%q), want (3,hello)", packetType, string(payload))
	}
}

func TestReadGATTBatteryConnectsReadsAndDisconnects(t *testing.T) {
	var paths []string
	var connectBody map[string]any
	var readBody map[string]any
	var disconnected bool
	originalDo := doHTTPRequest
	t.Cleanup(func() { doHTTPRequest = originalDo })
	doHTTPRequest = func(cfg config, r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		response := ``
		switch r.URL.Path {
		case "/control/connectivity/connect":
			connectBody = body
			response = `{"status":"SUCCESS","id":"device-1"}`
		case "/control/data/read":
			readBody = body
			response = `{"status":"SUCCESS","id":"device-1","value":"6439332E332E36"}`
		case "/control/connectivity/disconnect":
			disconnected = true
			response = `{"status":"SUCCESS","id":"device-1"}`
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Body:       io.NopCloser(strings.NewReader(`not found`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	}

	battery, firmware, err := readGATTBattery(context.Background(), config{
		APIURL:        "https://orchestrator.example",
		ControlAppID:  "control",
		ControlAPIKey: "key",
	}, gattBatteryConfig{
		DeviceID: "device-1",
	})
	if err != nil {
		t.Fatalf("readGATTBattery: %v", err)
	}
	if battery != 100 {
		t.Fatalf("battery=%d, want 100", battery)
	}
	if firmware != "3.3.6" {
		t.Fatalf("firmware=%q, want 3.3.6", firmware)
	}
	if !disconnected {
		t.Fatal("disconnect was not called")
	}
	if strings.Join(paths, ",") != "/control/connectivity/connect,/control/data/read,/control/connectivity/disconnect" {
		t.Fatalf("paths=%v", paths)
	}
	ble, ok := connectBody["ble"].(map[string]any)
	if !ok {
		t.Fatalf("connect ble body=%#v", connectBody["ble"])
	}
	services, ok := ble["services"].([]any)
	if !ok || len(services) != 1 {
		t.Fatalf("connect services=%#v", ble["services"])
	}
	service, ok := services[0].(map[string]any)
	if !ok || service["serviceID"] != "1204" {
		t.Fatalf("connect service=%#v", services[0])
	}
	readBLE, ok := readBody["ble"].(map[string]any)
	if !ok {
		t.Fatalf("read ble body=%#v", readBody["ble"])
	}
	if readBLE["serviceID"] != "1204" {
		t.Fatalf("read serviceID=%#v, want 1204", readBLE["serviceID"])
	}
	if readBLE["characteristicID"] != "00001a02-0000-1000-8000-00805f9b34fb" {
		t.Fatalf("read characteristicID=%#v", readBLE["characteristicID"])
	}
}

func TestReadGATTFlowerCareHistoryReadsOneEntryPerConnection(t *testing.T) {
	var paths []string
	var writes []string
	entry := 0
	originalDo := doHTTPRequest
	t.Cleanup(func() { doHTTPRequest = originalDo })
	doHTTPRequest = func(cfg config, r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		response := `{"status":"SUCCESS","id":"device-1"}`
		switch r.URL.Path {
		case "/control/connectivity/connect", "/control/connectivity/disconnect":
		case "/control/data/write":
			value, _ := body["value"].(string)
			writes = append(writes, value)
		case "/control/data/read":
			ble, _ := body["ble"].(map[string]any)
			characteristicID, _ := ble["characteristicID"].(string)
			switch characteristicID {
			case flowerCareEpoch:
				response = `{"status":"SUCCESS","id":"device-1","value":"a1644c00"}`
			case flowerCareHistoryData:
				if len(writes) == 0 || writes[len(writes)-1] == "a00000" {
					response = `{"status":"SUCCESS","id":"device-1","value":"02000000000000000000000000000000"}`
				} else {
					entry++
					if entry == 1 {
						response = `{"status":"SUCCESS","id":"device-1","value":"a0224c00140100e90300000528000000"}`
					} else {
						response = `{"status":"SUCCESS","id":"device-1","value":"90624c00ee0003861000001a19010000"}`
					}
				}
			default:
				t.Fatalf("unexpected read characteristic=%s", characteristicID)
			}
		default:
			t.Fatalf("unexpected path=%s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	}

	result, err := readGATTFlowerCareHistory(context.Background(), config{
		APIURL:        "https://orchestrator.example",
		ControlAppID:  "control",
		ControlAPIKey: "key",
	}, targetDevice{
		MAC:            "5c:85:7e:14:73:7d",
		Label:          "Blueberry1",
		Location:       "Blueberry1",
		IngestSource:   sensorConnectIngestSource,
		SensorTypeCode: "xiaomi_flower_care",
		SensorCategory: "plant",
		GATTBattery:    &gattBatteryConfig{DeviceID: "device-1"},
	}, 3)
	if err != nil {
		t.Fatalf("readGATTFlowerCareHistory: %v", err)
	}
	readings := result.Readings
	if len(readings) != 2 {
		t.Fatalf("readings=%d, want 2", len(readings))
	}
	if strings.Join(writes, ",") != "a00000,a10100,a00000,a10200" {
		t.Fatalf("writes=%v", writes)
	}
	if result.Count != 2 {
		t.Fatalf("history count=%d, want 2", result.Count)
	}
	if result.StopReason != "" {
		t.Fatalf("stop reason=%q, want empty", result.StopReason)
	}
	if readings[0].SensorMAC != "5c:85:7e:14:73:7d" ||
		readings[0].Label != "Blueberry1" ||
		readings[0].IngestSource != sensorConnectIngestSource ||
		readings[0].SensorTypeCode != "xiaomi_flower_care" ||
		readings[0].SensorCategory != "plant" {
		t.Fatalf("metadata not populated: %#v", readings[0])
	}
	if got := readings[0].TemperatureC; got == nil || *got != 27.6 {
		t.Fatalf("entry1 temperature=%v, want 27.6", got)
	}
	if got := readings[1].TemperatureC; got == nil || *got != 23.8 {
		t.Fatalf("entry2 temperature=%v, want 23.8", got)
	}
	connects := 0
	for _, path := range paths {
		if path == "/control/connectivity/connect" {
			connects++
		}
	}
	if connects != 2 {
		t.Fatalf("connects=%d, want one per entry; paths=%v", connects, paths)
	}
}

func TestReadGATTFlowerCareHistoryReturnsEmptyWhenCountZero(t *testing.T) {
	var writes []string
	var disconnected bool
	originalDo := doHTTPRequest
	t.Cleanup(func() { doHTTPRequest = originalDo })
	doHTTPRequest = func(cfg config, r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		response := `{"status":"SUCCESS","id":"device-1"}`
		switch r.URL.Path {
		case "/control/connectivity/connect":
		case "/control/connectivity/disconnect":
			disconnected = true
		case "/control/data/write":
			value, _ := body["value"].(string)
			writes = append(writes, value)
		case "/control/data/read":
			ble, _ := body["ble"].(map[string]any)
			characteristicID, _ := ble["characteristicID"].(string)
			switch characteristicID {
			case flowerCareEpoch:
				response = `{"status":"SUCCESS","id":"device-1","value":"a1644c00"}`
			case flowerCareHistoryData:
				response = `{"status":"SUCCESS","id":"device-1","value":"00000000000000000000000000000000"}`
			default:
				t.Fatalf("unexpected read characteristic=%s", characteristicID)
			}
		default:
			t.Fatalf("unexpected path=%s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	}

	result, err := readGATTFlowerCareHistory(context.Background(), config{
		APIURL:        "https://orchestrator.example",
		ControlAppID:  "control",
		ControlAPIKey: "key",
	}, targetDevice{
		MAC:         "5c:85:7e:14:73:7d",
		Label:       "Blueberry1",
		GATTBattery: &gattBatteryConfig{DeviceID: "device-1"},
	}, 3)
	if err != nil {
		t.Fatalf("readGATTFlowerCareHistory: %v", err)
	}
	if len(result.Readings) != 0 {
		t.Fatalf("readings=%d, want empty", len(result.Readings))
	}
	if result.Count != 0 {
		t.Fatalf("history count=%d, want 0", result.Count)
	}
	if result.StopReason != "" {
		t.Fatalf("stop reason=%q, want empty", result.StopReason)
	}
	if strings.Join(writes, ",") != "a00000" {
		t.Fatalf("writes=%v, want init only", writes)
	}
	if !disconnected {
		t.Fatal("disconnect was not called")
	}
}

func TestReadGATTFlowerCareHistoryReturnsPartialStopReason(t *testing.T) {
	entry := 0
	originalDo := doHTTPRequest
	t.Cleanup(func() { doHTTPRequest = originalDo })
	doHTTPRequest = func(cfg config, r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		response := `{"status":"SUCCESS","id":"device-1"}`
		switch r.URL.Path {
		case "/control/connectivity/connect", "/control/connectivity/disconnect", "/control/data/write":
		case "/control/data/read":
			ble, _ := body["ble"].(map[string]any)
			characteristicID, _ := ble["characteristicID"].(string)
			switch characteristicID {
			case flowerCareEpoch:
				response = `{"status":"SUCCESS","id":"device-1","value":"a1644c00"}`
			case flowerCareHistoryData:
				entry++
				switch entry {
				case 1, 3:
					response = `{"status":"SUCCESS","id":"device-1","value":"03000000000000000000000000000000"}`
				case 2:
					response = `{"status":"SUCCESS","id":"device-1","value":"a0224c00140100e90300000528000000"}`
				default:
					response = `{"status":"FAILURE","reason":"transient read failure"}`
				}
			default:
				t.Fatalf("unexpected read characteristic=%s", characteristicID)
			}
		default:
			t.Fatalf("unexpected path=%s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	}

	result, err := readGATTFlowerCareHistory(context.Background(), config{
		APIURL:        "https://orchestrator.example",
		ControlAppID:  "control",
		ControlAPIKey: "key",
	}, targetDevice{
		MAC:         "5c:85:7e:14:73:7d",
		Label:       "Blueberry1",
		GATTBattery: &gattBatteryConfig{DeviceID: "device-1"},
	}, 3)
	if err != nil {
		t.Fatalf("readGATTFlowerCareHistory: %v", err)
	}
	if len(result.Readings) != 1 {
		t.Fatalf("readings=%d, want partial 1", len(result.Readings))
	}
	if !strings.Contains(result.StopReason, "entry 2") || !strings.Contains(result.StopReason, "transient read failure") {
		t.Fatalf("stop reason=%q", result.StopReason)
	}
}

func TestWithGATTControlSessionSerializesWholeSession(t *testing.T) {
	cfg := config{ControlMu: &sync.Mutex{}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	active := 0
	maxActive := 0

	run := func() {
		defer wg.Done()
		if err := withGATTControlSession(cfg, func() error {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			time.Sleep(10 * time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
			return nil
		}); err != nil {
			t.Errorf("withGATTControlSession: %v", err)
		}
	}

	wg.Add(2)
	go run()
	go run()
	wg.Wait()

	if maxActive != 1 {
		t.Fatalf("max active sessions=%d, want 1", maxActive)
	}
}

func TestDecodeHexValueParsesFlowerCareBatteryPayload(t *testing.T) {
	payload, err := decodeHexValue("6439332E332E36")
	if err != nil {
		t.Fatalf("decodeHexValue: %v", err)
	}
	if len(payload) != 7 || payload[0] != 100 || string(payload[2:]) != "3.3.6" {
		t.Fatalf("payload=% x", payload)
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	data, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func protoMessage(field int, payload ...[]byte) []byte {
	return protoField(field, 2, protoBytes(bytes.Join(payload, nil)))
}

func protoFields(payload ...[]byte) []byte {
	return bytes.Join(payload, nil)
}

func protoString(value string) []byte {
	return protoBytes([]byte(value))
}

func protoBytes(value []byte) []byte {
	var buf bytes.Buffer
	writeTestVarint(&buf, uint64(len(value)))
	buf.Write(value)
	return buf.Bytes()
}

func protoVarint(value uint64) []byte {
	var buf bytes.Buffer
	writeTestVarint(&buf, value)
	return buf.Bytes()
}

func protoField(field int, wire int, payload []byte) []byte {
	var buf bytes.Buffer
	writeTestVarint(&buf, uint64(field<<3|wire))
	buf.Write(payload)
	return buf.Bytes()
}

func writeTestVarint(buf *bytes.Buffer, value uint64) {
	tmp := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(tmp, value)
	buf.Write(tmp[:n])
}
