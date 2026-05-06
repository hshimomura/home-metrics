package main

import (
	"testing"
	"time"
)

func TestParsePowerReading(t *testing.T) {
	appliances := []appliance{
		{Type: "AC"},
		{
			Type: "EL_SMART_METER",
			SmartMeter: smartMeter{ECHONETLiteProperties: []echonetLiteProperty{
				{EPC: 211, Val: "1", UpdatedAt: "2026-05-05T01:00:00Z"},
				{EPC: 224, Val: "12345", UpdatedAt: "2026-05-05T01:00:01Z"},
				{EPC: 225, Val: "1", UpdatedAt: "2026-05-05T01:00:02Z"},
				{EPC: 227, Val: "9", UpdatedAt: "2026-05-05T01:00:03Z"},
				{EPC: 231, Val: "456", UpdatedAt: "2026-05-05T01:00:04Z"},
			}},
		},
	}

	got, err := parsePowerReading(appliances, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("parsePowerReading() error = %v", err)
	}
	if got.MeasuredInstantaneous != 456 {
		t.Fatalf("MeasuredInstantaneous = %v, want 456", got.MeasuredInstantaneous)
	}
	wantTS := time.Date(2026, 5, 5, 1, 0, 4, 0, time.UTC)
	if !got.TS.Equal(wantTS) {
		t.Fatalf("TS = %s, want %s", got.TS, wantTS)
	}
}

func TestParseIntValue(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{name: "decimal", raw: "231", want: 231},
		{name: "hex prefix", raw: "0xE7", want: 231},
		{name: "hex fallback", raw: "E7", want: 231},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIntValue(tt.raw)
			if err != nil {
				t.Fatalf("parseIntValue() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseIntValue() = %d, want %d", got, tt.want)
			}
		})
	}
}
