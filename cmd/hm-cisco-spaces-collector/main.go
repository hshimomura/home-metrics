package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	defaultDBDSN             = "dbname=ble_sensors host=/var/run/postgresql"
	defaultFirehoseURL       = "https://partners.dnaspaces.io/api/partners/v1/firehose/events"
	defaultSensorsFile       = "/etc/home-metrics/sensors.json"
	defaultSampleWindow      = 5
	defaultFieldFreshness    = time.Minute
	defaultUploadInterval    = time.Minute
	defaultReconnectMinDelay = time.Second
	defaultReconnectMaxDelay = time.Minute
)

type config struct {
	DBDSN             string
	APIKey            string
	FirehoseURL       string
	SampleWindow      int
	FieldFreshness    time.Duration
	UploadInterval    time.Duration
	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration
	BatteryMode       string
	BatteryAllowlist  map[string]bool
	DryRun            bool
	Debug             bool
	PruneBLESensors   bool
	SensorsFile       string
}

type firehoseEvent struct {
	EventType     string         `json:"eventType"`
	RecordTS      int64          `json:"recordTimestamp"`
	IOTTelemetry  telemetry      `json:"iotTelemetry"`
	RawProperties map[string]any `json:"-"`
}

type telemetry struct {
	DeviceInfo      deviceInfo       `json:"deviceInfo"`
	Temperature     *temperatureData `json:"temperature"`
	Humidity        *humidityData    `json:"humidity"`
	AirPressure     *airPressureData `json:"airPressure"`
	CarbonEmissions *carbonData      `json:"carbonEmissions"`
	Illuminance     *illuminanceData `json:"illuminance"`
	Battery         *batteryData     `json:"battery"`
	TVOC            *tvocData        `json:"tvoc"`
}

type deviceInfo struct {
	DeviceID         string `json:"deviceId"`
	DeviceMACAddress string `json:"deviceMacAddress"`
	Label            string `json:"label"`
}

type temperatureData struct {
	TemperatureC float64 `json:"temperatureInCelsius"`
}

type humidityData struct {
	HumidityPercent float64 `json:"humidityInPercentage"`
}

type airPressureData struct {
	Pressure float64 `json:"pressure"`
}

type carbonData struct {
	CO2PPM float64 `json:"co2Ppm"`
}

type illuminanceData struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

type batteryData struct {
	Value float64 `json:"value"`
}

type tvocData struct {
	ValueInPPB float64 `json:"valueInPpb"`
}

type metric string

const (
	metricTemperature metric = "temperature"
	metricHumidity    metric = "humidity"
	metricBattery     metric = "battery"
	metricPressure    metric = "pressure"
	metricCO2         metric = "co2"
	metricLux         metric = "lux"
	metricETVOC       metric = "etvoc"
)

type metricValue struct {
	Metric metric
	Value  float64
}

type sensorReading struct {
	TS              time.Time
	MAC             string
	Label           string
	TemperatureC    *float64
	HumidityPercent *float64
	BatteryPercent  *float64
	PressureHPa     *float64
	CO2PPM          *float64
	Lux             *float64
	ETVOC           *float64
}

type targetDevice struct {
	MAC string `json:"mac"`
}

type targetConfig struct {
	Devices []targetDevice `json:"devices"`
}

type recentMetric struct {
	TS    time.Time
	Value float64
}

type processor struct {
	cfg         config
	windows     map[string]map[metric][]float64
	lastMetrics map[string]map[metric]recentMetric
	lastUploads map[string]time.Time
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := loadConfig()
	if source := strings.ToLower(envString("SENSOR_INGEST_SOURCE", "cisco_spaces")); source != "cisco_spaces" && source != "cisco-spaces" {
		log.Printf("Cisco Spaces collector disabled by SENSOR_INGEST_SOURCE=%s", source)
		return
	}
	if cfg.APIKey == "" {
		log.Fatal("CISCO_SPACES_API_KEY is required")
	}

	var db *pgx.Conn
	if !cfg.DryRun {
		var err error
		db, err = pgx.Connect(ctx, cfg.DBDSN)
		if err != nil {
			log.Fatalf("connect database: %v", err)
		}
		defer db.Close(context.Background())
		if cfg.PruneBLESensors {
			if err := pruneConfiguredBLESensors(ctx, db, cfg.SensorsFile); err != nil {
				log.Printf("prune configured BLE sensors: %v", err)
			}
		}
	}

	p := newProcessor(cfg)
	client := &http.Client{}
	log.Printf("Cisco Spaces collector started url=%s db=%s dry_run=%t", cfg.FirehoseURL, cfg.DBDSN, cfg.DryRun)
	streamWithReconnect(ctx, client, cfg, func(event firehoseEvent) {
		reading, ok, err := p.processEvent(event)
		if err != nil {
			log.Printf("process Cisco Spaces event: %v", err)
			return
		}
		if !ok {
			return
		}
		if cfg.DryRun {
			log.Printf("dry-run Cisco Spaces reading mac=%s ts=%s", reading.MAC, reading.TS.Format(time.RFC3339))
			return
		}
		if err := writeReading(ctx, db, reading); err != nil {
			log.Printf("write Cisco Spaces reading mac=%s ts=%s: %v", reading.MAC, reading.TS.Format(time.RFC3339), err)
			return
		}
		log.Printf("stored Cisco Spaces reading mac=%s ts=%s", reading.MAC, reading.TS.Format(time.RFC3339))
	})
}

func loadConfig() config {
	cfg := config{
		DBDSN:             envString("BLE_DB_DSN", defaultDBDSN),
		APIKey:            envString("CISCO_SPACES_API_KEY", envString("DNASPACES_API_KEY", "")),
		FirehoseURL:       envString("CISCO_SPACES_FIREHOSE_URL", envString("FIREHOSE_URL", defaultFirehoseURL)),
		SampleWindow:      envInt("CISCO_SPACES_SAMPLE_WINDOW", defaultSampleWindow),
		FieldFreshness:    envDuration("CISCO_SPACES_FIELD_FRESHNESS", defaultFieldFreshness),
		UploadInterval:    envDuration("CISCO_SPACES_UPLOAD_INTERVAL", defaultUploadInterval),
		ReconnectMinDelay: envDuration("CISCO_SPACES_RECONNECT_MIN_DELAY", defaultReconnectMinDelay),
		ReconnectMaxDelay: envDuration("CISCO_SPACES_RECONNECT_MAX_DELAY", defaultReconnectMaxDelay),
		BatteryMode:       strings.ToLower(envString("CISCO_SPACES_BATTERY_MODE", "all")),
		BatteryAllowlist:  parseMACSet(os.Getenv("CISCO_SPACES_BATTERY_ALLOWLIST")),
		DryRun:            envBool("CISCO_SPACES_DRY_RUN", false),
		Debug:             envBool("CISCO_SPACES_DEBUG", false),
		PruneBLESensors:   envBool("CISCO_SPACES_PRUNE_CONFIGURED_BLE_SENSORS", true),
		SensorsFile:       envString("BLE_SENSORS_FILE", defaultSensorsFile),
	}
	if cfg.SampleWindow < 1 {
		cfg.SampleWindow = defaultSampleWindow
	}
	if cfg.ReconnectMinDelay <= 0 {
		cfg.ReconnectMinDelay = defaultReconnectMinDelay
	}
	if cfg.ReconnectMaxDelay < cfg.ReconnectMinDelay {
		cfg.ReconnectMaxDelay = cfg.ReconnectMinDelay
	}
	switch cfg.BatteryMode {
	case "all", "allowlist", "off":
	default:
		log.Printf("invalid CISCO_SPACES_BATTERY_MODE=%q, using all", cfg.BatteryMode)
		cfg.BatteryMode = "all"
	}
	return cfg
}

func newProcessor(cfg config) *processor {
	return &processor{
		cfg:         cfg,
		windows:     map[string]map[metric][]float64{},
		lastMetrics: map[string]map[metric]recentMetric{},
		lastUploads: map[string]time.Time{},
	}
}

func streamWithReconnect(ctx context.Context, client *http.Client, cfg config, handle func(firehoseEvent)) {
	backoff := cfg.ReconnectMinDelay
	for ctx.Err() == nil {
		err := streamOnce(ctx, client, cfg, handle)
		if ctx.Err() != nil {
			return
		}
		log.Printf("Cisco Spaces stream ended: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > cfg.ReconnectMaxDelay {
			backoff = cfg.ReconnectMaxDelay
		}
	}
}

func streamOnce(ctx context.Context, client *http.Client, cfg config, handle func(firehoseEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.FirehoseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("firehose status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event firehoseEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			log.Printf("decode Cisco Spaces event: %v", err)
			continue
		}
		handle(event)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("firehose response closed")
}

func (p *processor) processEvent(event firehoseEvent) (sensorReading, bool, error) {
	if event.EventType != "IOT_TELEMETRY" {
		return sensorReading{}, false, nil
	}
	if event.RecordTS <= 0 {
		p.debug("skip Cisco Spaces event reason=missing_record_ts")
		return sensorReading{}, false, nil
	}
	info := event.IOTTelemetry.DeviceInfo
	mac := normalizeMAC(info.DeviceMACAddress)
	if mac == "" {
		p.debug("skip Cisco Spaces event reason=missing_device_mac device_id=%q label=%q", info.DeviceID, info.Label)
		return sensorReading{}, false, nil
	}
	if deviceIDConflictsWithMAC(info) {
		p.debug("skip Cisco Spaces event reason=device_id_mismatch mac=%s device_id=%q label=%q", mac, info.DeviceID, info.Label)
		return sensorReading{}, false, nil
	}

	ts := time.UnixMilli(event.RecordTS).UTC()
	values := p.extractValues(mac, event.IOTTelemetry)
	if len(values) == 0 {
		p.debug("skip Cisco Spaces event reason=no_metric_values mac=%s device_id=%q label=%q ts=%s", mac, info.DeviceID, info.Label, ts.Format(time.RFC3339))
	}
	for _, value := range values {
		p.addValue(mac, value.Metric, value.Value, ts)
	}
	if lastUpload, ok := p.lastUploads[mac]; ok && ts.Sub(lastUpload) < p.cfg.UploadInterval {
		p.debug("skip Cisco Spaces event reason=upload_interval mac=%s label=%q ts=%s last_upload=%s values=%d", mac, info.Label, ts.Format(time.RFC3339), lastUpload.Format(time.RFC3339), len(values))
		return sensorReading{}, false, nil
	}
	reading := p.buildReading(mac, strings.TrimSpace(info.Label), ts)
	if reading.empty() {
		p.debug("skip Cisco Spaces event reason=empty_reading mac=%s label=%q ts=%s values=%d windows=%s", mac, info.Label, ts.Format(time.RFC3339), len(values), p.debugWindows(mac))
		return sensorReading{}, false, nil
	}
	p.lastUploads[mac] = ts
	p.debug("accept Cisco Spaces event mac=%s label=%q ts=%s values=%d windows=%s", mac, info.Label, ts.Format(time.RFC3339), len(values), p.debugWindows(mac))
	return reading, true, nil
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

func writeReading(ctx context.Context, db *pgx.Conn, reading sensorReading) error {
	if err := upsertDevice(ctx, db, reading); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `
		INSERT INTO sensor_minute (
			ts,
			mac,
			temperature_c,
			humidity_percent,
			battery_percent,
			pressure_hpa,
			co2_ppm,
			lux,
			etvoc
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (ts, mac) DO UPDATE SET
			temperature_c = COALESCE(EXCLUDED.temperature_c, sensor_minute.temperature_c),
			humidity_percent = COALESCE(EXCLUDED.humidity_percent, sensor_minute.humidity_percent),
			battery_percent = COALESCE(EXCLUDED.battery_percent, sensor_minute.battery_percent),
			pressure_hpa = COALESCE(EXCLUDED.pressure_hpa, sensor_minute.pressure_hpa),
			co2_ppm = COALESCE(EXCLUDED.co2_ppm, sensor_minute.co2_ppm),
			lux = COALESCE(EXCLUDED.lux, sensor_minute.lux),
			etvoc = COALESCE(EXCLUDED.etvoc, sensor_minute.etvoc),
			inserted_at = now()
	`, reading.TS,
		reading.MAC,
		nullablePtr(reading.TemperatureC),
		nullablePtr(reading.HumidityPercent),
		nullablePtr(reading.BatteryPercent),
		nullablePtr(reading.PressureHPa),
		nullablePtr(reading.CO2PPM),
		nullablePtr(reading.Lux),
		nullablePtr(reading.ETVOC),
	)
	return err
}

func pruneConfiguredBLESensors(ctx context.Context, db *pgx.Conn, path string) error {
	macs, err := loadConfiguredSensorMACs(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("configured BLE sensors file not found, skip prune path=%s", path)
			return nil
		}
		return err
	}
	if len(macs) == 0 {
		return nil
	}

	var deleted int64
	var disabled int64
	for _, mac := range macs {
		tag, err := db.Exec(ctx, `
			DELETE FROM devices d
			WHERE d.mac = $1
				AND COALESCE(d.sensor_category, '') <> 'Cisco Spaces'
				AND NOT EXISTS (SELECT 1 FROM sensor_minute s WHERE s.mac = d.mac)
				AND NOT EXISTS (SELECT 1 FROM sensor_1hour s WHERE s.mac = d.mac)
				AND NOT EXISTS (SELECT 1 FROM sensor_12hour s WHERE s.mac = d.mac)
				AND NOT EXISTS (SELECT 1 FROM sensor_1day s WHERE s.mac = d.mac)
				AND NOT EXISTS (SELECT 1 FROM alert_rules r WHERE r.mac = d.mac)
		`, mac)
		if err != nil {
			return fmt.Errorf("delete configured BLE sensor %s: %w", mac, err)
		}
		if tag.RowsAffected() > 0 {
			deleted += tag.RowsAffected()
			continue
		}

		tag, err = db.Exec(ctx, `
			UPDATE devices
			SET enabled = false,
				updated_at = now()
			WHERE mac = $1
				AND enabled
				AND COALESCE(sensor_category, '') <> 'Cisco Spaces'
		`, mac)
		if err != nil {
			return fmt.Errorf("disable configured BLE sensor %s: %w", mac, err)
		}
		disabled += tag.RowsAffected()
	}
	if deleted > 0 || disabled > 0 {
		log.Printf("pruned configured BLE sensors deleted=%d disabled=%d source=%s", deleted, disabled, path)
	}
	return nil
}

func loadConfiguredSensorMACs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var config targetConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	var macs []string
	for _, device := range config.Devices {
		mac := normalizeMAC(device.MAC)
		if mac == "" || seen[mac] {
			continue
		}
		seen[mac] = true
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	return macs, nil
}

func upsertDevice(ctx context.Context, db *pgx.Conn, reading sensorReading) error {
	label := strings.TrimSpace(reading.Label)
	if label == "" {
		label = reading.MAC
	}
	_, err := db.Exec(ctx, `
		INSERT INTO devices (mac, label, sensor_category, location)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (mac) DO UPDATE SET
			label = EXCLUDED.label,
			sensor_category = COALESCE(devices.sensor_category, EXCLUDED.sensor_category),
			location = COALESCE(devices.location, EXCLUDED.location),
			updated_at = now()
	`, reading.MAC, label, "Cisco Spaces", label)
	return err
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

func normalizeMAC(value string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(value) {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			b.WriteRune(ch)
		}
	}
	raw := b.String()
	if len(raw) != 12 {
		return ""
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, raw[i:i+2])
	}
	return strings.Join(parts, ":")
}

func parseMACSet(raw string) map[string]bool {
	result := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if mac := normalizeMAC(part); mac != "" {
			result[mac] = true
		}
	}
	return result
}

func nullablePtr(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
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

func envString(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %d", name, value, fallback)
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("invalid %s=%q, using %t", name, value, fallback)
		return fallback
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %s", name, value, fallback)
		return fallback
	}
	return parsed
}

func floatPtr(value float64) *float64 {
	return &value
}
