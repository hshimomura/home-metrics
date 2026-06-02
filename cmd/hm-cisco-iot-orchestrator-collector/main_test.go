package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
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
