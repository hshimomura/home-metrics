package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRedactPayload(t *testing.T) {
	raw := json.RawMessage(`{
		"spacesTenantId": "tenant-1",
		"spacesTenantName": "Tenant",
		"partnerTenantId": "partner-1",
		"iotTelemetry": {
			"deviceInfo": {
				"deviceMacAddress": "00:fa:b6:07:de:49",
				"label": "Kitchen"
			}
		}
	}`)
	got, err := redactPayload(raw)
	if err != nil {
		t.Fatalf("redact payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode redacted payload: %v", err)
	}
	if decoded["spacesTenantId"] != "[REDACTED]" {
		t.Fatalf("spacesTenantId = %v", decoded["spacesTenantId"])
	}
	telemetry := decoded["iotTelemetry"].(map[string]any)
	info := telemetry["deviceInfo"].(map[string]any)
	if info["deviceMacAddress"] != "00:fa:b6:07:de:49" {
		t.Fatalf("deviceMacAddress = %v", info["deviceMacAddress"])
	}
	if info["label"] != "[REDACTED]" {
		t.Fatalf("label = %v", info["label"])
	}
}

func TestParseOptionalTime(t *testing.T) {
	fallback := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	got, err := parseOptionalTime("", fallback)
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if !got.Equal(fallback) {
		t.Fatalf("empty time = %s", got)
	}
	got, err = parseOptionalTime("2026-05-16T21:00:00+09:00", fallback)
	if err != nil {
		t.Fatalf("parse explicit: %v", err)
	}
	want := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("explicit time = %s, want %s", got, want)
	}
}

func TestNormalizeMAC(t *testing.T) {
	if got := normalizeMAC("00-FA-B6-07-DE-49"); got != "00:fa:b6:07:de:49" {
		t.Fatalf("normalize MAC = %q", got)
	}
	if got := normalizeMAC("not-a-mac"); got != "" {
		t.Fatalf("invalid MAC = %q", got)
	}
}
