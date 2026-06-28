package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-metrics/internal/sensorstore"
)

func TestLoadTargetRegistrySelectsOnlyBLEOwnedDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensors.json")
	data := `{"devices":[
		{"mac":"aa:bb:cc:dd:ee:01","label":"local","ingest_source":"ble","enabled":true},
		{"mac":"aa:bb:cc:dd:ee:02","label":"remote","ingest_source":"cisco_sensor_connect","enabled":true}
	]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := loadTargetRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.All) != 1 || len(registry.Enabled) != 1 {
		t.Fatalf("registry sizes all=%d enabled=%d", len(registry.All), len(registry.Enabled))
	}
	if _, ok := registry.All["aa:bb:cc:dd:ee:01"]; !ok {
		t.Fatal("BLE-owned device missing")
	}
}

func TestSensorMinuteUpsertUsesSparseMerge(t *testing.T) {
	for _, column := range []string{
		"temperature_c", "humidity_percent", "battery_percent", "rssi_dbm",
		"pressure_hpa", "co2_ppm", "lux", "etvoc",
	} {
		want := column + " = COALESCE(EXCLUDED." + column + ", sensor_minute." + column + ")"
		if !strings.Contains(sensorstore.UpsertMinuteSQL, want) {
			t.Fatalf("upsert SQL does not preserve sparse %s", column)
		}
	}
}

func TestBLEOutlierFilterSkipsSingleSpike(t *testing.T) {
	c := testCollector()
	base := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	for i, value := range []float64{24.0, 24.1, 24.2} {
		got := c.filterMetric("aa:bb:cc:dd:ee:ff", "temperature_c", base.Add(time.Duration(i)*time.Minute), floatPtr(value))
		if got == nil {
			t.Fatalf("seed value %d was filtered", i)
		}
	}

	if got := c.filterMetric("aa:bb:cc:dd:ee:ff", "temperature_c", base.Add(3*time.Minute), floatPtr(12)); got != nil {
		t.Fatalf("single spike = %v, want filtered", *got)
	}
	if got := c.filterMetric("aa:bb:cc:dd:ee:ff", "temperature_c", base.Add(4*time.Minute), floatPtr(24.3)); got == nil || *got != 24.3 {
		t.Fatalf("normal value after spike = %v, want 24.3", got)
	}
}

func TestBLEOutlierFilterAcceptsConfirmedShift(t *testing.T) {
	c := testCollector()
	base := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	for i, value := range []float64{24.0, 24.1, 24.2} {
		if got := c.filterMetric("aa:bb:cc:dd:ee:ff", "temperature_c", base.Add(time.Duration(i)*time.Minute), floatPtr(value)); got == nil {
			t.Fatalf("seed value %d was filtered", i)
		}
	}

	if got := c.filterMetric("aa:bb:cc:dd:ee:ff", "temperature_c", base.Add(3*time.Minute), floatPtr(19)); got != nil {
		t.Fatalf("first shifted value = %v, want pending", *got)
	}
	if got := c.filterMetric("aa:bb:cc:dd:ee:ff", "temperature_c", base.Add(4*time.Minute), floatPtr(19.2)); got == nil || *got != 19.2 {
		t.Fatalf("confirmed shift = %v, want 19.2", got)
	}
}

func TestSanitizeReadingDropsSentinelsAndImplausibleValues(t *testing.T) {
	got := sanitizeReading(reading{
		TemperatureC:    floatPtr(1000),
		HumidityPercent: floatPtr(255),
		BatteryPercent:  floatPtr(101),
		PressureHPa:     floatPtr(0),
		CO2PPM:          floatPtr(500),
		Lux:             floatPtr(65535),
		ETVOC:           floatPtr(1000000),
	})
	if got.TemperatureC != nil || got.HumidityPercent != nil || got.BatteryPercent != nil || got.PressureHPa != nil || got.ETVOC != nil {
		t.Fatalf("implausible values were not dropped: %+v", got)
	}
	if got.CO2PPM == nil || *got.CO2PPM != 500 {
		t.Fatalf("co2 = %v, want 500", got.CO2PPM)
	}
	if got.Lux != nil {
		t.Fatalf("lux sentinel = %v, want filtered", *got.Lux)
	}
}

func TestDecodeServiceDataExtractsEnvGasValues(t *testing.T) {
	got := decodeServiceData("0305177bd47b44041f071a0403ff00000313991303200a00")
	if got.PressureHPa == nil || *got.PressureHPa != 1007.32 {
		t.Fatalf("pressure=%v, want 1007.32", got.PressureHPa)
	}
	if got.TemperatureC == nil || *got.TemperatureC != 19.6 {
		t.Fatalf("temperature=%v, want 19.6", got.TemperatureC)
	}
	if got.CO2PPM == nil || *got.CO2PPM != 1050 {
		t.Fatalf("co2=%v, want 1050", got.CO2PPM)
	}
	if got.Lux == nil || *got.Lux != 10 {
		t.Fatalf("lux=%v, want 10", got.Lux)
	}

	got = decodeServiceData("0305177bd47b44041f08070003ff00000313991303200a00")
	if got.ETVOC == nil || *got.ETVOC != 7 {
		t.Fatalf("etvoc=%v, want 7", got.ETVOC)
	}
}

func testCollector() *collector {
	return &collector{
		outliers: outlierConfig{
			Enabled:       true,
			HistorySize:   5,
			ConfirmWindow: 10 * time.Minute,
			Thresholds: map[string]float64{
				"temperature_c": 3,
			},
		},
		metricHistory: map[string]map[string]*metricHistory{},
	}
}
