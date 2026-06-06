package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseAllowedOrigins(t *testing.T) {
	origins := parseAllowedOrigins("https://example.test, http://localhost:3000,")
	if !origins["https://example.test"] {
		t.Fatal("missing https://example.test")
	}
	if !origins["http://localhost:3000"] {
		t.Fatal("missing http://localhost:3000")
	}
	if origins[""] {
		t.Fatal("empty origin must not be allowed")
	}
}

func TestMetricColumns(t *testing.T) {
	if len(metricColumns) != len(sensorMetrics) {
		t.Fatalf("metricColumns length = %d, want %d", len(metricColumns), len(sensorMetrics))
	}
	for _, metric := range sensorMetrics {
		if metricColumns[metric.Key] != metric.Column {
			t.Fatalf("metricColumns[%q] = %q, want %q", metric.Key, metricColumns[metric.Key], metric.Column)
		}
	}
	if _, ok := metricColumns["raw_payload"]; ok {
		t.Fatal("raw_payload must not be an API metric")
	}
}

func TestSensorMetricsIncludeExpectedMetrics(t *testing.T) {
	for _, key := range []string{
		"temperature_c",
		"humidity_percent",
		"battery_percent",
		"rssi_dbm",
		"pressure_hpa",
		"co2_ppm",
		"lux",
		"etvoc",
		"soil_moisture_percent",
		"conductivity_us_cm",
	} {
		if metricColumns[key] != key {
			t.Fatalf("metricColumns[%q] = %q, want %q", key, metricColumns[key], key)
		}
	}
}

func TestAssembleLatestSnapshotMergesSparseMetricRows(t *testing.T) {
	now := time.Date(2026, 6, 6, 22, 35, 0, 0, time.UTC)
	batteryTS := now.Add(-3 * time.Minute)
	timestamps := make([]pgtype.Timestamptz, len(sensorMetrics))
	readings := make([]pgtype.Float8, len(sensorMetrics))

	for i, metric := range sensorMetrics {
		switch metric.Key {
		case "temperature_c":
			timestamps[i] = pgtype.Timestamptz{Time: now, Valid: true}
			readings[i] = pgtype.Float8{Float64: 22.4, Valid: true}
		case "battery_percent":
			timestamps[i] = pgtype.Timestamptz{Time: batteryTS, Valid: true}
			readings[i] = pgtype.Float8{Float64: 100, Valid: true}
		}
	}

	snapshot, err := assembleLatestSnapshot(sensorMetrics, timestamps, readings)
	if err != nil {
		t.Fatalf("assemble snapshot: %v", err)
	}
	if !snapshot.TS.Equal(now) {
		t.Fatalf("snapshot TS = %s, want %s", snapshot.TS, now)
	}
	if got := snapshot.Values["temperature_c"]; got == nil || *got != 22.4 {
		t.Fatalf("temperature value = %v, want 22.4", got)
	}
	if got := snapshot.Values["battery_percent"]; got == nil || *got != 100 {
		t.Fatalf("battery value = %v, want 100", got)
	}
	if got := snapshot.Values["humidity_percent"]; got != nil {
		t.Fatalf("humidity value = %v, want nil", *got)
	}
	if !snapshot.ValueTimestamps["temperature_c"].Equal(now) {
		t.Fatalf("temperature timestamp = %s, want %s", snapshot.ValueTimestamps["temperature_c"], now)
	}
	if !snapshot.ValueTimestamps["battery_percent"].Equal(batteryTS) {
		t.Fatalf("battery timestamp = %s, want %s", snapshot.ValueTimestamps["battery_percent"], batteryTS)
	}
	if _, ok := snapshot.ValueTimestamps["humidity_percent"]; ok {
		t.Fatal("value_timestamps must omit metrics with no value")
	}
}

func TestAssembleLatestSnapshotReturnsNoRowsWhenAllMetricsAreNull(t *testing.T) {
	_, err := assembleLatestSnapshot(
		sensorMetrics,
		make([]pgtype.Timestamptz, len(sensorMetrics)),
		make([]pgtype.Float8, len(sensorMetrics)),
	)
	if err != pgx.ErrNoRows {
		t.Fatalf("error = %v, want pgx.ErrNoRows", err)
	}
}

func TestLatestResponseKeepsExistingShapeAndAddsValueTimestamps(t *testing.T) {
	ts := time.Date(2026, 6, 6, 22, 35, 0, 0, time.UTC)
	batteryTS := ts.Add(-24 * time.Hour)
	value := 100.0
	resp := latestResponse{
		Device: deviceResponse{MAC: "5c:85:7e:14:73:7d", Label: "blue berry 1", Enabled: true},
		TS:     ts,
		Values: map[string]*float64{
			"battery_percent": &value,
			"temperature_c":   nil,
		},
		ValueTimestamps: map[string]time.Time{"battery_percent": batteryTS},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	text := string(body)
	for _, want := range []string{`"device":`, `"ts":`, `"values":`, `"value_timestamps":`, `"battery_percent":100`, `"temperature_c":null`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %s: %s", want, text)
		}
	}
}

func TestRangeIntervals(t *testing.T) {
	for _, rangeKey := range []string{"1d", "1w", "1m", "3m", "1y"} {
		interval, ok := rangeIntervals[rangeKey]
		if !ok {
			t.Fatalf("range %q is missing", rangeKey)
		}
		if interval.Lookback == "" || interval.Bucket == "" {
			t.Fatalf("range %q has empty interval: %+v", rangeKey, interval)
		}
		if interval.Source == "" {
			t.Fatalf("range %q has empty source: %+v", rangeKey, interval)
		}
	}
}

func TestEnergyRangeIntervals(t *testing.T) {
	wantSources := map[string]string{
		"1d": "energy_readings",
		"1w": "energy_1hour",
		"1m": "energy_1hour",
		"3m": "energy_12hour",
		"1y": "energy_1day",
	}
	for rangeKey, source := range wantSources {
		interval, ok := energyRangeIntervals[rangeKey]
		if !ok {
			t.Fatalf("energy range %q is missing", rangeKey)
		}
		if interval.Lookback == "" || interval.Bucket == "" {
			t.Fatalf("energy range %q has empty interval: %+v", rangeKey, interval)
		}
		if interval.Source != source {
			t.Fatalf("energy range %q source = %q, want %q", rangeKey, interval.Source, source)
		}
	}
}

func TestCollectorExpectedTreatsCiscoSpacesAsOptional(t *testing.T) {
	t.Setenv("CISCO_SPACES_COLLECTOR_ENABLED", "")
	if collectorExpected("hm-cisco-spaces-collector", "cisco_spaces_firehose", "default") {
		t.Fatal("Cisco Spaces collector must be optional by default")
	}
	if !collectorExpected("hm-cisco-iot-orchestrator-collector", "mqtt", "192.168.67.6:41883/topic") {
		t.Fatal("non-optional collectors must be expected")
	}

	t.Setenv("CISCO_SPACES_COLLECTOR_ENABLED", "true")
	if !collectorExpected("hm-cisco-spaces-collector", "cisco_spaces_firehose", "default") {
		t.Fatal("Cisco Spaces collector must be expected when enabled")
	}
}

func TestEnergySeriesResponseIncludesUnit(t *testing.T) {
	resp := energySeriesResponse{
		Source:    "echonet",
		DeviceKey: "echonet-device",
		Metric:    "solar_generation_w",
		Range:     "1d",
		Unit:      "W",
		Points:    []seriesPoint{},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !strings.Contains(string(body), `"unit":"W"`) {
		t.Fatalf("energy series response must include unit: %s", body)
	}
}

func TestUnsupportedAPIEndpointReturnsJSONError(t *testing.T) {
	api := &apiServer{}
	req := httptest.NewRequest(http.MethodGet, "/api/not-supported", nil)
	rec := httptest.NewRecorder()

	api.handleUnsupportedAPIEndpoint(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), `"error":"unsupported endpoint"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}
