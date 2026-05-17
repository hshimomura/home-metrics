package main

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

func newProcessor(cfg config) *processor {
	return &processor{
		cfg:         cfg,
		windows:     map[string]map[metric][]float64{},
		lastMetrics: map[string]map[metric]recentMetric{},
		lastUploads: map[string]time.Time{},
	}
}

func (p *processor) processEvent(event firehoseEvent) (sensorReading, bool, string, error) {
	if event.EventType != "IOT_TELEMETRY" {
		return sensorReading{}, false, "non_iot_telemetry_event", nil
	}
	if event.RecordTS <= 0 {
		p.debug("skip Cisco Spaces event reason=missing_record_ts")
		return sensorReading{}, false, "missing_record_ts", nil
	}
	info := event.IOTTelemetry.DeviceInfo
	mac := normalizeMAC(info.DeviceMACAddress)
	if mac == "" {
		p.debug("skip Cisco Spaces event reason=missing_device_mac device_id=%q label=%q", info.DeviceID, info.Label)
		return sensorReading{}, false, "missing_device_mac", nil
	}
	if deviceIDConflictsWithMAC(info) {
		p.debug("skip Cisco Spaces event reason=device_id_mismatch mac=%s device_id=%q label=%q", mac, info.DeviceID, info.Label)
		return sensorReading{}, false, "device_id_mismatch", nil
	}

	ts := time.UnixMilli(event.RecordTS).UTC()
	values := p.extractValues(mac, event.IOTTelemetry)
	ignoreReason := ""
	if len(values) == 0 {
		ignoreReason = "no_metric_values"
		p.debug("skip Cisco Spaces event reason=no_metric_values mac=%s device_id=%q label=%q ts=%s", mac, info.DeviceID, info.Label, ts.Format(time.RFC3339))
	}
	for _, value := range values {
		p.addValue(mac, value.Metric, value.Value, ts)
	}
	if lastUpload, ok := p.lastUploads[mac]; ok && ts.Sub(lastUpload) < p.cfg.UploadInterval {
		p.debug("skip Cisco Spaces event reason=upload_interval mac=%s label=%q ts=%s last_upload=%s values=%d", mac, info.Label, ts.Format(time.RFC3339), lastUpload.Format(time.RFC3339), len(values))
		return sensorReading{}, false, "upload_interval", nil
	}
	reading := p.buildReading(mac, strings.TrimSpace(info.Label), ts)
	if reading.empty() {
		if ignoreReason == "" {
			ignoreReason = "empty_reading"
		}
		p.debug("skip Cisco Spaces event reason=empty_reading mac=%s label=%q ts=%s values=%d windows=%s", mac, info.Label, ts.Format(time.RFC3339), len(values), p.debugWindows(mac))
		return sensorReading{}, false, ignoreReason, nil
	}
	p.lastUploads[mac] = ts
	p.debug("accept Cisco Spaces event mac=%s label=%q ts=%s values=%d windows=%s", mac, info.Label, ts.Format(time.RFC3339), len(values), p.debugWindows(mac))
	return reading, true, "", nil
}

func (p *processor) debug(format string, args ...any) {
	if !p.cfg.Debug {
		return
	}
	log.Printf(format, args...)
}

func (p *processor) debugWindows(mac string) string {
	windows := p.windows[mac]
	if len(windows) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(windows))
	for name, values := range windows {
		parts = append(parts, fmt.Sprintf("%s:%d", name, len(values)))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}

func (p *processor) extractValues(mac string, item telemetry) []metricValue {
	var values []metricValue
	if item.Temperature != nil && item.Temperature.TemperatureC != 0 {
		values = append(values, metricValue{Metric: metricTemperature, Value: item.Temperature.TemperatureC})
	}
	if item.Humidity != nil && item.Humidity.HumidityPercent != 255 {
		values = append(values, metricValue{Metric: metricHumidity, Value: item.Humidity.HumidityPercent})
	}
	if item.AirPressure != nil && item.AirPressure.Pressure != 0 {
		values = append(values, metricValue{Metric: metricPressure, Value: item.AirPressure.Pressure})
	}
	if item.CarbonEmissions != nil {
		values = append(values, metricValue{Metric: metricCO2, Value: item.CarbonEmissions.CO2PPM})
	}
	if item.Illuminance != nil && item.Illuminance.Unit == "LUX" && item.Illuminance.Value != 65535 {
		values = append(values, metricValue{Metric: metricLux, Value: item.Illuminance.Value})
	}
	if item.Battery != nil && p.acceptBattery(mac) {
		values = append(values, metricValue{Metric: metricBattery, Value: item.Battery.Value})
	}
	if item.TVOC != nil {
		values = append(values, metricValue{Metric: metricETVOC, Value: item.TVOC.ValueInPPB / 1000})
	}
	return values
}

func (p *processor) acceptBattery(mac string) bool {
	switch p.cfg.BatteryMode {
	case "off":
		return false
	case "allowlist":
		return p.cfg.BatteryAllowlist[mac]
	default:
		return true
	}
}

func (p *processor) addValue(mac string, name metric, value float64, ts time.Time) {
	if !isFinite(value) {
		return
	}
	if p.windows[mac] == nil {
		p.windows[mac] = map[metric][]float64{}
	}
	values := append(p.windows[mac][name], value)
	if len(values) > p.cfg.SampleWindow {
		values = values[len(values)-p.cfg.SampleWindow:]
	}
	p.windows[mac][name] = values
	if len(values) < p.cfg.SampleWindow {
		return
	}
	if p.lastMetrics[mac] == nil {
		p.lastMetrics[mac] = map[metric]recentMetric{}
	}
	p.lastMetrics[mac][name] = recentMetric{TS: ts, Value: median(values)}
}

func (p *processor) buildReading(mac string, label string, ts time.Time) sensorReading {
	reading := sensorReading{
		TS:    ts.Truncate(time.Minute),
		MAC:   mac,
		Label: label,
	}
	fields := p.lastMetrics[mac]
	reading.TemperatureC = p.fresh(fields, metricTemperature, ts)
	reading.HumidityPercent = p.fresh(fields, metricHumidity, ts)
	reading.BatteryPercent = p.fresh(fields, metricBattery, ts)
	reading.PressureHPa = p.fresh(fields, metricPressure, ts)
	reading.CO2PPM = p.fresh(fields, metricCO2, ts)
	reading.Lux = p.fresh(fields, metricLux, ts)
	reading.ETVOC = p.fresh(fields, metricETVOC, ts)
	return reading
}

func (p *processor) fresh(fields map[metric]recentMetric, name metric, now time.Time) *float64 {
	field, ok := fields[name]
	if !ok || now.Sub(field.TS) > p.cfg.FieldFreshness {
		return nil
	}
	return floatPtr(field.Value)
}

func (r sensorReading) empty() bool {
	return r.TemperatureC == nil &&
		r.HumidityPercent == nil &&
		r.BatteryPercent == nil &&
		r.PressureHPa == nil &&
		r.CO2PPM == nil &&
		r.Lux == nil &&
		r.ETVOC == nil
}

func deviceIDConflictsWithMAC(info deviceInfo) bool {
	deviceID := normalizeMAC(info.DeviceID)
	deviceMAC := normalizeMAC(info.DeviceMACAddress)
	return deviceID != "" && deviceMAC != "" && deviceID != deviceMAC
}

func median(values []float64) float64 {
	copied := append([]float64(nil), values...)
	sort.Float64s(copied)
	mid := len(copied) / 2
	if len(copied)%2 == 1 {
		return copied[mid]
	}
	return (copied[mid-1] + copied[mid]) / 2
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func floatPtr(value float64) *float64 {
	return &value
}
