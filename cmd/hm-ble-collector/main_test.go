package main

import (
	"testing"
	"time"
)

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
