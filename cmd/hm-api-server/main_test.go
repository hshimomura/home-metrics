package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidOperator(t *testing.T) {
	for _, operator := range []string{">", ">=", "<", "<="} {
		if !validOperator(operator) {
			t.Fatalf("validOperator(%q) = false, want true", operator)
		}
	}
	if validOperator("=") {
		t.Fatal("validOperator(\"=\") = true, want false")
	}
}

func TestValidNotificationStatus(t *testing.T) {
	for _, status := range []string{"pending", "dry_run", "sent", "failed", "skipped"} {
		if !validNotificationStatus(status) {
			t.Fatalf("validNotificationStatus(%q) = false, want true", status)
		}
	}
	if validNotificationStatus("would_send") {
		t.Fatal("validNotificationStatus(\"would_send\") = true, want false")
	}
}

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
	for _, metric := range []string{
		"temperature_c",
		"humidity_percent",
		"battery_percent",
		"rssi_dbm",
		"pressure_hpa",
		"co2_ppm",
		"lux",
		"etvoc",
	} {
		if metricColumns[metric] != metric {
			t.Fatalf("metricColumns[%q] = %q, want %q", metric, metricColumns[metric], metric)
		}
	}
	if _, ok := metricColumns["raw_payload"]; ok {
		t.Fatal("raw_payload must not be an API metric")
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
