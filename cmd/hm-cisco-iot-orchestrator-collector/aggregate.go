package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"home-metrics/internal/collectorstatus"
	"home-metrics/internal/sensor"
	"home-metrics/internal/sensorstore"
)

func (c *collector) add(r bleReading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	window := r.TS.Truncate(time.Minute)
	key := r.SensorMAC + "|" + window.Format(time.RFC3339)
	agg := c.windows[key]
	if agg == nil {
		agg = &aggregate{
			SensorMAC:      r.SensorMAC,
			Label:          r.Label,
			Location:       r.Location,
			IngestSource:   r.IngestSource,
			SensorTypeCode: r.SensorTypeCode,
			SensorCategory: r.SensorCategory,
			Window:         window,
		}
		c.windows[key] = agg
	}
	comparable := readingKey(r)
	if comparable == agg.LastComparable {
		return
	}
	agg.LastComparable = comparable
	appendPtr(&agg.RSSI, r.RSSI)
	appendPtr(&agg.TemperatureC, r.TemperatureC)
	appendPtr(&agg.HumidityPercent, r.HumidityPercent)
	appendPtr(&agg.BatteryPercent, r.BatteryPercent)
	appendPtr(&agg.PressureHPa, r.PressureHPa)
	appendPtr(&agg.CO2PPM, r.CO2PPM)
	appendPtr(&agg.Lux, r.Lux)
	appendPtr(&agg.ETVOC, r.ETVOC)
	appendPtr(&agg.SoilMoisturePercent, r.SoilMoisturePercent)
	appendPtr(&agg.ConductivityUSCM, r.ConductivityUSCM)
}

func (c *collector) flushCompleted(ctx context.Context, currentWindow time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	flushed := 0
	for key, agg := range c.windows {
		if agg.Window.Before(currentWindow) {
			wrote, err := c.flushAggregate(ctx, agg)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if wrote {
				flushed++
			}
			delete(c.windows, key)
		}
	}
	return flushed, errors.Join(errs...)
}

func (c *collector) flushAll(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for key, agg := range c.windows {
		if _, err := c.flushAggregate(ctx, agg); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(c.windows, key)
	}
	return errors.Join(errs...)
}

func (c *collector) flushAggregate(ctx context.Context, agg *aggregate) (bool, error) {
	if c.flushFn != nil {
		return c.flushFn(ctx, agg)
	}
	return c.flush(ctx, agg)
}

func (c *collector) pendingSummary(now time.Time) (int, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := len(c.windows)
	if count == 0 {
		return 0, 0
	}
	var oldest time.Time
	for _, agg := range c.windows {
		if oldest.IsZero() || agg.Window.Before(oldest) {
			oldest = agg.Window
		}
	}
	if oldest.IsZero() || now.Before(oldest) {
		return count, 0
	}
	return count, now.Sub(oldest)
}

func (c *collector) flush(ctx context.Context, agg *aggregate) (bool, error) {
	if c.db == nil || agg.empty() {
		return false, nil
	}
	reading := bleReading{
		TS:                  agg.Window,
		SensorMAC:           agg.SensorMAC,
		TemperatureC:        nullableMedianFloat(agg.TemperatureC),
		HumidityPercent:     nullableMedianFloat(agg.HumidityPercent),
		BatteryPercent:      nullableMedianFloat(agg.BatteryPercent),
		RSSI:                nullableMedianFloat(agg.RSSI),
		PressureHPa:         nullableMedianFloat(agg.PressureHPa),
		CO2PPM:              nullableMedianFloat(agg.CO2PPM),
		Lux:                 nullableMedianFloat(agg.Lux),
		ETVOC:               nullableMedianFloat(agg.ETVOC),
		SoilMoisturePercent: nullableMedianFloat(agg.SoilMoisturePercent),
		ConductivityUSCM:    nullableMedianFloat(agg.ConductivityUSCM),
	}
	if _, err := upsertSensorMinuteReading(ctx, c.db, reading); err != nil {
		return false, fmt.Errorf("insert %s %s: %w", agg.SensorMAC, agg.Window.Format(time.RFC3339), err)
	}
	log.Printf("flushed Cisco Sensor Connect sensor=%s minute=%s", agg.SensorMAC, agg.Window.Format(time.RFC3339))
	return true, nil
}

func backfillSensorMinuteReadings(ctx context.Context, db sensorMinuteExecer, readings []bleReading) (int, error) {
	var inserted int
	for _, reading := range readings {
		ok, err := upsertSensorMinuteReading(ctx, db, reading)
		if err != nil {
			return inserted, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, nil
}

func upsertSensorMinuteReading(ctx context.Context, db sensorMinuteExecer, reading bleReading) (bool, error) {
	if db == nil || reading.empty() {
		return false, nil
	}
	if strings.TrimSpace(reading.SensorMAC) == "" {
		return false, errors.New("sensor MAC is required")
	}
	if reading.TS.IsZero() {
		return false, errors.New("sensor timestamp is required")
	}
	return sensorstore.UpsertMinute(ctx, db, sensor.Reading{
		TS: reading.TS, MAC: reading.SensorMAC,
		TemperatureC: reading.TemperatureC, HumidityPercent: reading.HumidityPercent,
		BatteryPercent: reading.BatteryPercent, RSSI: reading.RSSI,
		PressureHPa: reading.PressureHPa, CO2PPM: reading.CO2PPM,
		Lux: reading.Lux, ETVOC: reading.ETVOC,
		SoilMoisturePercent: reading.SoilMoisturePercent,
		ConductivityUSCM:    reading.ConductivityUSCM,
	})
}

func ensureDevices(ctx context.Context, db sensorDB, targets map[string]targetDevice) error {
	for _, target := range targets {
		enabled := target.Enabled == nil || *target.Enabled
		if err := syncDevice(ctx, db, target.MAC, target.Label, target.Location, target.IngestSource, target.SensorTypeCode, target.SensorCategory, enabled); err != nil {
			return err
		}
	}
	return nil
}

func syncDevice(ctx context.Context, db sensorDB, mac string, label string, location string, ingestSource string, sensorTypeCode string, sensorCategory string, enabled bool) error {
	label = strings.TrimSpace(label)
	if label == "" || normalizeMAC(label) == mac {
		label = mac
	}
	location = strings.TrimSpace(location)
	if location == "" {
		location = label
	}
	ingestSource = normalizeIngestSource(ingestSource)
	sensorTypeCode = strings.TrimSpace(sensorTypeCode)
	sensorCategory = strings.TrimSpace(sensorCategory)
	return sensorstore.SyncDevice(ctx, db, sensor.Device{
		MAC: mac, Label: label, Location: location,
		IngestSource: ingestSource, IngestSourceExplicit: true,
		SensorTypeCode: sensorTypeCode, SensorCategory: sensorCategory,
		Enabled: enabled,
	})
}

func (r *statusReporter) MarkSuccess(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	if err := collectorstatus.MarkSuccess(ctx, r.db, r.target); err != nil {
		log.Printf("record collector success: %v", err)
	}
}

func (r *statusReporter) MarkDataSuccess(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	if err := collectorstatus.MarkDataSuccess(ctx, r.db, r.target); err != nil {
		log.Printf("record collector data success: %v", err)
	}
}

func (r *statusReporter) MarkFailure(ctx context.Context, failure error) {
	if r == nil || r.db == nil {
		return
	}
	if err := collectorstatus.MarkFailure(ctx, r.db, r.target, failure); err != nil {
		log.Printf("record collector failure: %v", err)
	}
}

func (agg *aggregate) empty() bool {
	return len(agg.RSSI) == 0 &&
		len(agg.TemperatureC) == 0 &&
		len(agg.HumidityPercent) == 0 &&
		len(agg.BatteryPercent) == 0 &&
		len(agg.PressureHPa) == 0 &&
		len(agg.CO2PPM) == 0 &&
		len(agg.Lux) == 0 &&
		len(agg.ETVOC) == 0 &&
		len(agg.SoilMoisturePercent) == 0 &&
		len(agg.ConductivityUSCM) == 0
}

func readingKey(r bleReading) string {
	return fmt.Sprintf(
		"rssi=%s|t=%s|h=%s|b=%s|p=%s|co2=%s|lux=%s|etvoc=%s|soil=%s|cond=%s",
		ptrKey(r.RSSI),
		ptrKey(r.TemperatureC),
		ptrKey(r.HumidityPercent),
		ptrKey(r.BatteryPercent),
		ptrKey(r.PressureHPa),
		ptrKey(r.CO2PPM),
		ptrKey(r.Lux),
		ptrKey(r.ETVOC),
		ptrKey(r.SoilMoisturePercent),
		ptrKey(r.ConductivityUSCM),
	)
}

func ptrKey(value *float64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func appendPtr(values *[]float64, value *float64) {
	if value != nil {
		*values = append(*values, *value)
	}
}

func nullableMedianFloat(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return floatPtr(values[mid])
	}
	return floatPtr((values[mid-1] + values[mid]) / 2)
}
