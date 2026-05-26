package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessEventWritesMedianAfterSampleWindow(t *testing.T) {
	cfg := config{
		SampleWindow:   5,
		FieldFreshness: time.Minute,
		UploadInterval: time.Minute,
		BatteryMode:    "all",
	}
	p := newProcessor(cfg)

	var got sensorReading
	var ok bool
	for i, temp := range []float64{24.0, 25.0, 26.0, 27.0, 28.0} {
		event := testEvent(1_700_000_000_000+int64(i)*15_000, "aa:bb:cc:dd:ee:ff")
		event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: temp}
		event.IOTTelemetry.Humidity = &humidityData{HumidityPercent: 40 + float64(i)}
		var err error
		got, ok, _, err = p.processEvent(event)
		if err != nil {
			t.Fatalf("process event: %v", err)
		}
	}
	if !ok {
		t.Fatal("expected fifth sample to produce a reading")
	}
	if got.TemperatureC == nil || *got.TemperatureC != 26 {
		t.Fatalf("temperature median = %v, want 26", got.TemperatureC)
	}
	if got.HumidityPercent == nil || *got.HumidityPercent != 42 {
		t.Fatalf("humidity median = %v, want 42", got.HumidityPercent)
	}
}

func TestProcessEventSkipsSentinelsAndConflictingIDs(t *testing.T) {
	cfg := config{
		SampleWindow:   1,
		FieldFreshness: time.Minute,
		UploadInterval: time.Minute,
		BatteryMode:    "all",
	}
	p := newProcessor(cfg)

	event := testEvent(1_700_000_000_000, "aa:bb:cc:dd:ee:ff")
	event.IOTTelemetry.DeviceInfo.DeviceID = "11:22:33:44:55:66"
	if _, ok, reason, err := p.processEvent(event); err != nil || ok || reason != "device_id_mismatch" {
		t.Fatalf("conflicting IDs got ok=%t err=%v, want skipped", ok, err)
	}

	event = testEvent(1_700_000_060_000, "aa:bb:cc:dd:ee:ff")
	event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: 0}
	event.IOTTelemetry.Humidity = &humidityData{HumidityPercent: 255}
	event.IOTTelemetry.Illuminance = &illuminanceData{Value: 65535, Unit: "LUX"}
	if _, ok, reason, err := p.processEvent(event); err != nil || ok || reason != "no_metric_values" {
		t.Fatalf("sentinel-only event got ok=%t err=%v, want skipped", ok, err)
	}
}

func TestProcessEventRejectsCiscoSpacesTemperatureOutliers(t *testing.T) {
	cfg := config{
		SampleWindow:   1,
		FieldFreshness: time.Minute,
		UploadInterval: time.Minute,
		BatteryMode:    "all",
	}
	p := newProcessor(cfg)

	event := testEvent(1_700_000_000_000, "aa:bb:cc:dd:ee:ff")
	event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: -45}
	event.IOTTelemetry.Humidity = &humidityData{HumidityPercent: 99}
	event.IOTTelemetry.Battery = &batteryData{Value: 90}
	got, ok, _, err := p.processEvent(event)
	if err != nil {
		t.Fatalf("process event: %v", err)
	}
	if !ok {
		t.Fatal("expected battery-only reading")
	}
	if got.TemperatureC != nil {
		t.Fatalf("temperature = %v, want nil", *got.TemperatureC)
	}
	if got.HumidityPercent != nil {
		t.Fatalf("humidity = %v, want nil", *got.HumidityPercent)
	}
	assertPtr(t, "battery", got.BatteryPercent, 90)

	event = testEvent(1_700_000_060_000, "aa:bb:cc:dd:ee:ff")
	event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: 24.5}
	event.IOTTelemetry.Humidity = &humidityData{HumidityPercent: 99}
	got, ok, _, err = p.processEvent(event)
	if err != nil {
		t.Fatalf("process normal event: %v", err)
	}
	if !ok {
		t.Fatal("expected normal reading")
	}
	assertPtr(t, "temperature", got.TemperatureC, 24.5)
	assertPtr(t, "humidity", got.HumidityPercent, 99)
}

func TestProcessEventRejectsShortLivedCiscoSpacesTemperatureDrops(t *testing.T) {
	cfg := config{
		SampleWindow:   5,
		FieldFreshness: time.Minute,
		UploadInterval: 0,
		BatteryMode:    "all",
	}
	p := newProcessor(cfg)
	mac := "aa:bb:cc:dd:ee:ff"
	baseTS := int64(1_700_000_000_000)

	var got sensorReading
	var ok bool
	for i, temp := range []float64{28.70, 28.72, 28.73, 28.74, 28.73} {
		event := testEvent(baseTS+int64(i)*1_000, mac)
		event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: temp}
		var err error
		got, ok, _, err = p.processEvent(event)
		if err != nil {
			t.Fatalf("process normal event %d: %v", i, err)
		}
	}
	if !ok {
		t.Fatal("expected normal baseline reading")
	}
	assertPtr(t, "baseline temperature", got.TemperatureC, 28.73)

	for i, temp := range []float64{18.55, 19.96, 21.17, 23.11, 24.56} {
		event := testEvent(baseTS+60_000+int64(i)*1_000, mac)
		event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: temp}
		event.IOTTelemetry.Humidity = &humidityData{HumidityPercent: 57}
		var err error
		got, ok, _, err = p.processEvent(event)
		if err != nil {
			t.Fatalf("process dropped event %d: %v", i, err)
		}
	}
	if !ok {
		t.Fatal("expected reading with previous temperature")
	}
	assertPtr(t, "temperature after short drop", got.TemperatureC, 28.73)
	assertPtr(t, "humidity after short drop", got.HumidityPercent, 57)

	event := testEvent(baseTS+70_000, mac)
	event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: 28.66}
	got, ok, _, err := p.processEvent(event)
	if err != nil {
		t.Fatalf("process recovered event: %v", err)
	}
	if !ok {
		t.Fatal("expected recovered reading")
	}
	assertPtr(t, "recovered temperature", got.TemperatureC, 28.73)
}

func TestProcessEventParsesReferenceShape(t *testing.T) {
	const raw = `{
		"eventType": "IOT_TELEMETRY",
		"recordTimestamp": 1700000000000,
		"iotTelemetry": {
			"deviceInfo": {
				"deviceId": "aa:bb:cc:dd:ee:ff",
				"deviceMacAddress": "AA-BB-CC-DD-EE-FF",
				"label": "Lab Sensor"
			},
			"temperature": {"temperatureInCelsius": 21.5},
			"humidity": {"humidityInPercentage": 45},
			"airPressure": {"pressure": 1012.3},
			"carbonEmissions": {"co2Ppm": 550},
			"illuminance": {"unit": "LUX", "value": 123},
			"battery": {"value": 98},
			"tvoc": {"valueInPpb": 7000}
		}
	}`
	var event firehoseEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := newProcessor(config{
		SampleWindow:   1,
		FieldFreshness: time.Minute,
		UploadInterval: time.Minute,
		BatteryMode:    "all",
	})
	got, ok, _, err := p.processEvent(event)
	if err != nil {
		t.Fatalf("process event: %v", err)
	}
	if !ok {
		t.Fatal("expected reading")
	}
	if got.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("mac = %q", got.MAC)
	}
	if got.Label != "Lab Sensor" {
		t.Fatalf("label = %q", got.Label)
	}
	assertPtr(t, "temperature", got.TemperatureC, 21.5)
	assertPtr(t, "humidity", got.HumidityPercent, 45)
	assertPtr(t, "pressure", got.PressureHPa, 1012.3)
	assertPtr(t, "co2", got.CO2PPM, 550)
	assertPtr(t, "lux", got.Lux, 123)
	assertPtr(t, "battery", got.BatteryPercent, 98)
	assertPtr(t, "etvoc", got.ETVOC, 7)
}

func TestExtractRawEventMetadata(t *testing.T) {
	const raw = `{
		"recordUid": "event-123",
		"eventType": "IOT_TELEMETRY",
		"recordTimestamp": 1700000000000,
		"iotTelemetry": {
			"deviceInfo": {
				"deviceId": "25Zj0003",
				"deviceMacAddress": "00-FA-B6-07-DE-4C",
				"label": "DC"
			},
			"detectedPosition": {
				"mapId": "map-1",
				"locationId": "location-from-position"
			},
			"location": {
				"locationId": "location-1"
			}
		}
	}`
	var event firehoseEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := extractRawEventMetadata(event, []byte(raw))
	if got.RecordUID != "event-123" {
		t.Fatalf("record uid = %q", got.RecordUID)
	}
	if got.EventType != "IOT_TELEMETRY" {
		t.Fatalf("event type = %q", got.EventType)
	}
	if got.DeviceMAC != "00:fa:b6:07:de:4c" {
		t.Fatalf("device mac = %q", got.DeviceMAC)
	}
	if got.DeviceID != "25Zj0003" {
		t.Fatalf("device id = %q", got.DeviceID)
	}
	if got.DeviceLabel != "DC" {
		t.Fatalf("device label = %q", got.DeviceLabel)
	}
	if got.LocationID != "location-1" {
		t.Fatalf("location id = %q", got.LocationID)
	}
	if got.MapID != "map-1" {
		t.Fatalf("map id = %q", got.MapID)
	}
	if got.RecordTS == nil || !got.RecordTS.Equal(time.UnixMilli(1700000000000).UTC()) {
		t.Fatalf("record ts = %v", got.RecordTS)
	}
	sum := sha256.Sum256([]byte(raw))
	if got.PayloadSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("payload sha256 = %q", got.PayloadSHA256)
	}
}

func TestBatteryAllowlist(t *testing.T) {
	p := newProcessor(config{
		SampleWindow:     1,
		FieldFreshness:   time.Minute,
		UploadInterval:   time.Minute,
		BatteryMode:      "allowlist",
		BatteryAllowlist: parseMACSet("aa:bb:cc:dd:ee:ff"),
	})
	event := testEvent(1_700_000_000_000, "11:22:33:44:55:66")
	event.IOTTelemetry.Battery = &batteryData{Value: 90}
	if _, ok, reason, err := p.processEvent(event); err != nil || ok || reason != "no_metric_values" {
		t.Fatalf("non-allowlisted battery got ok=%t err=%v, want skipped", ok, err)
	}

	event = testEvent(1_700_000_060_000, "aa:bb:cc:dd:ee:ff")
	event.IOTTelemetry.Battery = &batteryData{Value: 91}
	got, ok, _, err := p.processEvent(event)
	if err != nil {
		t.Fatalf("process allowlisted battery: %v", err)
	}
	if !ok {
		t.Fatal("expected allowlisted battery reading")
	}
	assertPtr(t, "battery", got.BatteryPercent, 91)
}

func TestLoadConfiguredSensorMACs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensors.json")
	if err := os.WriteFile(path, []byte(`{
		"devices": [
			{"mac": "AA-BB-CC-DD-EE-01"},
			{"mac": "aa:bb:cc:dd:ee:01"},
			{"mac": "AA:BB:CC:DD:EE:02"},
			{"mac": "not-a-mac"}
		]
	}`), 0o600); err != nil {
		t.Fatalf("write sensors: %v", err)
	}
	got, err := loadConfiguredSensorMACs(path)
	if err != nil {
		t.Fatalf("load configured sensors: %v", err)
	}
	want := []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}
	if len(got) != len(want) {
		t.Fatalf("macs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("macs = %v, want %v", got, want)
		}
	}
}

func TestValidDeviceLabelRejectsBlankAndMACLabels(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:ff"
	cases := []struct {
		name  string
		label string
		want  bool
	}{
		{name: "blank", label: " ", want: false},
		{name: "same mac", label: "aa:bb:cc:dd:ee:ff", want: false},
		{name: "same mac uppercase", label: "AA:BB:CC:DD:EE:FF", want: false},
		{name: "same mac hyphenated", label: "aa-bb-cc-dd-ee-ff", want: false},
		{name: "human label", label: "Desk", want: true},
		{name: "contains mac but not exact", label: "Desk aa:bb:cc:dd:ee:ff", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validDeviceLabel(mac, tc.label); got != tc.want {
				t.Fatalf("validDeviceLabel(%q, %q) = %t, want %t", mac, tc.label, got, tc.want)
			}
		})
	}
}

func testEvent(recordTS int64, mac string) firehoseEvent {
	return firehoseEvent{
		EventType: "IOT_TELEMETRY",
		RecordTS:  recordTS,
		IOTTelemetry: telemetry{
			DeviceInfo: deviceInfo{
				DeviceID:         mac,
				DeviceMACAddress: mac,
				Label:            "Sensor",
			},
		},
	}
}

func assertPtr(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %v, want %v", name, *got, want)
	}
}
