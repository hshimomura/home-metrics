package main

import (
	"encoding/json"
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
		got, ok, err = p.processEvent(event)
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
	if _, ok, err := p.processEvent(event); err != nil || ok {
		t.Fatalf("conflicting IDs got ok=%t err=%v, want skipped", ok, err)
	}

	event = testEvent(1_700_000_060_000, "aa:bb:cc:dd:ee:ff")
	event.IOTTelemetry.Temperature = &temperatureData{TemperatureC: 0}
	event.IOTTelemetry.Humidity = &humidityData{HumidityPercent: 255}
	event.IOTTelemetry.Illuminance = &illuminanceData{Value: 65535, Unit: "LUX"}
	if _, ok, err := p.processEvent(event); err != nil || ok {
		t.Fatalf("sentinel-only event got ok=%t err=%v, want skipped", ok, err)
	}
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
			"tvoc": {"valueInPpb": 7}
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
	got, ok, err := p.processEvent(event)
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
	if _, ok, err := p.processEvent(event); err != nil || ok {
		t.Fatalf("non-allowlisted battery got ok=%t err=%v, want skipped", ok, err)
	}

	event = testEvent(1_700_000_060_000, "aa:bb:cc:dd:ee:ff")
	event.IOTTelemetry.Battery = &batteryData{Value: 91}
	got, ok, err := p.processEvent(event)
	if err != nil {
		t.Fatalf("process allowlisted battery: %v", err)
	}
	if !ok {
		t.Fatal("expected allowlisted battery reading")
	}
	assertPtr(t, "battery", got.BatteryPercent, 91)
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
