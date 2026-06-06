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
	"strings"
	"testing"
	"time"
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
		Label:               "blue berry 1",
		SensorCategory:          "Cisco Sensor Connect (IoT Orchestrator)",
		SoilMoisturePercent: floatPtr(11),
	})
	c.add(bleReading{
		TS:               window.Add(15 * time.Second),
		SensorMAC:        "5c:85:7e:14:73:7d",
		Label:            "blue berry 1",
		SensorCategory:       "Cisco Sensor Connect (IoT Orchestrator)",
		ConductivityUSCM: floatPtr(65),
	})

	agg := c.windows["5c:85:7e:14:73:7d|"+window.Format(time.RFC3339)]
	if agg == nil {
		t.Fatal("aggregate not found")
	}
	if agg.SensorCategory != "Cisco Sensor Connect (IoT Orchestrator)" {
		t.Fatalf("device type=%q, want plant type", agg.SensorCategory)
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
