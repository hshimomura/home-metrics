package main

import (
	"testing"
	"time"
)

func TestBuildGetFrame(t *testing.T) {
	got := buildGetFrame(0x1234, eoj{0x02, 0x79, 0x01}, []byte{0xe0})
	want := []byte{
		0x10, 0x81,
		0x12, 0x34,
		0x05, 0xff, 0x01,
		0x02, 0x79, 0x01,
		0x62,
		0x01,
		0xe0, 0x00,
	}
	if string(got) != string(want) {
		t.Fatalf("buildGetFrame() = %x, want %x", got, want)
	}
}

func TestParseFrame(t *testing.T) {
	raw := []byte{
		0x10, 0x81,
		0x12, 0x34,
		0x02, 0x7d, 0x01,
		0x05, 0xff, 0x01,
		0x72,
		0x02,
		0xe4, 0x01, 0x36,
		0xd3, 0x02, 0xfb, 0x48,
	}
	got, err := parseFrame(raw)
	if err != nil {
		t.Fatalf("parseFrame() error = %v", err)
	}
	if got.TID != 0x1234 || got.SEOJ != (eoj{0x02, 0x7d, 0x01}) || got.ESV != esvGetRes {
		t.Fatalf("unexpected frame: %+v", got)
	}
	if len(got.Properties) != 2 {
		t.Fatalf("properties len = %d, want 2", len(got.Properties))
	}
	if got.Properties[0].EPC != 0xe4 || got.Properties[0].EDT[0] != 0x36 {
		t.Fatalf("unexpected first property: %+v", got.Properties[0])
	}
}

func TestDecodeSigned(t *testing.T) {
	got, err := decodeSigned([]byte{0xfb, 0x48})
	if err != nil {
		t.Fatalf("decodeSigned() error = %v", err)
	}
	if got != -1208 {
		t.Fatalf("decodeSigned() = %v, want -1208", got)
	}
}

func TestDecodeUnsigned(t *testing.T) {
	got, err := decodeUnsigned([]byte{0x00, 0x1d, 0x89, 0x46})
	if err != nil {
		t.Fatalf("decodeUnsigned() error = %v", err)
	}
	if got != 1935686 {
		t.Fatalf("decodeUnsigned() = %v, want 1935686", got)
	}
}

func TestDecodeReadings(t *testing.T) {
	frame := frame{
		SEOJ: eoj{0x02, 0x7d, 0x01},
		ESV:  esvGetRes,
		Properties: []propertyValue{
			{EPC: 0xe4, EDT: []byte{0x36}},
			{EPC: 0xd3, EDT: []byte{0xfb, 0x48}},
		},
	}
	got := decodeReadings(time.Date(2026, 5, 5, 11, 0, 0, 0, time.UTC), defaultDeviceKey, frame)
	if len(got) != 2 {
		t.Fatalf("decodeReadings len = %d, want 2", len(got))
	}
	if got[0].Metric != "battery_remaining" || got[0].Value != 54 {
		t.Fatalf("unexpected first reading: %+v", got[0])
	}
	if got[1].Metric != "battery_power_w" || got[1].Value != -1208 {
		t.Fatalf("unexpected second reading: %+v", got[1])
	}
}
