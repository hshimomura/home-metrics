package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/jackc/pgx/v5"
)

const (
	bluezName              = "org.bluez"
	objectManagerInterface = "org.freedesktop.DBus.ObjectManager"
	adapterInterface       = "org.bluez.Adapter1"
	deviceInterface        = "org.bluez.Device1"
	defaultAdapterPath     = dbus.ObjectPath("/org/bluez/hci0")
	defaultDBDSN           = "dbname=ble_sensors host=/var/run/postgresql"
	defaultSensorsFile     = "/etc/home-metrics/sensors.json"
)

type outlierConfig struct {
	Enabled       bool
	HistorySize   int
	ConfirmWindow time.Duration
	Thresholds    map[string]float64
}

type metricHistory struct {
	Accepted []float64
	Pending  *pendingOutlier
}

type pendingOutlier struct {
	Value float64
	TS    time.Time
}

type targetDevice struct {
	MAC        string `json:"mac"`
	Label      string `json:"label"`
	DeviceType string `json:"device_type"`
	Location   string `json:"location"`
	Enabled    *bool  `json:"enabled"`
}

type targetConfig struct {
	Devices []targetDevice `json:"devices"`
}

type reading struct {
	TS              time.Time
	SensorMAC       string
	RSSI            *float64
	TemperatureC    *float64
	HumidityPercent *float64
	BatteryPercent  *float64
	PressureHPa     *float64
	CO2PPM          *float64
	Lux             *float64
	ETVOC           *float64
}

type aggregate struct {
	SensorMAC       string
	Window          time.Time
	RSSI            []float64
	TemperatureC    []float64
	HumidityPercent []float64
	BatteryPercent  []float64
	PressureHPa     []float64
	CO2PPM          []float64
	Lux             []float64
	ETVOC           []float64
	LastComparable  string
}

type collector struct {
	db            *pgx.Conn
	windows       map[string]*aggregate
	outliers      outlierConfig
	metricHistory map[string]map[string]*metricHistory
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if source := strings.ToLower(envString("SENSOR_INGEST_SOURCE", "ble")); source != "ble" {
		log.Printf("BLE collector disabled by SENSOR_INGEST_SOURCE=%s", source)
		return
	}

	dsn := envString("BLE_DB_DSN", defaultDBDSN)
	pollInterval := envDuration("BLE_POLL_INTERVAL", 2*time.Second)
	outliers := loadOutlierConfig()
	adapterPath := defaultAdapterPath
	if value := os.Getenv("BLE_ADAPTER"); value != "" {
		adapterPath = dbus.ObjectPath(value)
	}
	targets, err := loadTargets(envString("BLE_SENSORS_FILE", defaultSensorsFile))
	if err != nil {
		log.Fatalf("load BLE sensors: %v", err)
	}
	if len(targets) == 0 {
		log.Fatal("no enabled BLE sensors configured")
	}

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())

	if err := ensureDevices(ctx, db, targets); err != nil {
		log.Fatalf("ensure devices: %v", err)
	}

	systemBus, err := dbus.SystemBus()
	if err != nil {
		log.Fatalf("connect system bus: %v", err)
	}
	defer systemBus.Close()

	if err := startDiscovery(systemBus, adapterPath); err != nil {
		log.Fatalf("start BLE discovery: %v", err)
	}
	defer func() {
		if err := stopDiscovery(systemBus, adapterPath); err != nil {
			log.Printf("stop BLE discovery: %v", err)
		}
	}()

	c := &collector{
		db:            db,
		windows:       make(map[string]*aggregate),
		outliers:      outliers,
		metricHistory: make(map[string]map[string]*metricHistory),
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	log.Printf("BLE collector started adapter=%s poll=%s db=%s outlier_filter=%t", adapterPath, pollInterval, dsn, outliers.Enabled)
	for {
		select {
		case <-ctx.Done():
			if err := c.flushAll(context.Background()); err != nil {
				log.Printf("flush on shutdown: %v", err)
			}
			return
		case <-ticker.C:
			readings, err := pollReadings(systemBus, targets)
			if err != nil {
				log.Printf("poll readings: %v", err)
				continue
			}
			now := time.Now()
			for _, r := range readings {
				c.add(r)
			}
			if err := c.flushCompleted(ctx, now.Truncate(time.Minute)); err != nil {
				log.Printf("flush completed windows: %v", err)
			}
		}
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
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

func envFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		log.Printf("invalid %s=%q, using %.3f", name, value, fallback)
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

func loadOutlierConfig() outlierConfig {
	return outlierConfig{
		Enabled:       envBool("BLE_OUTLIER_FILTER", true),
		HistorySize:   envInt("BLE_OUTLIER_HISTORY_SIZE", 5),
		ConfirmWindow: envDuration("BLE_OUTLIER_CONFIRM_WINDOW", 10*time.Minute),
		Thresholds: map[string]float64{
			"temperature_c":    envFloat("BLE_OUTLIER_TEMP_DELTA", 3),
			"humidity_percent": envFloat("BLE_OUTLIER_HUMIDITY_DELTA", 20),
			"battery_percent":  envFloat("BLE_OUTLIER_BATTERY_DELTA", 50),
			"pressure_hpa":     envFloat("BLE_OUTLIER_PRESSURE_DELTA", 10),
			"co2_ppm":          envFloat("BLE_OUTLIER_CO2_DELTA", 500),
			"etvoc":            envFloat("BLE_OUTLIER_ETVOC_DELTA", 500),
			"rssi_dbm":         envFloat("BLE_OUTLIER_RSSI_DELTA", 40),
		},
	}
}

func loadTargets(path string) (map[string]targetDevice, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var config targetConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	targets := make(map[string]targetDevice)
	for _, device := range config.Devices {
		device.MAC = strings.ToLower(strings.TrimSpace(device.MAC))
		device.Label = strings.TrimSpace(device.Label)
		device.DeviceType = strings.TrimSpace(device.DeviceType)
		device.Location = strings.TrimSpace(device.Location)
		if device.MAC == "" {
			return nil, errors.New("sensor mac is required")
		}
		if device.Label == "" {
			return nil, fmt.Errorf("sensor %s label is required", device.MAC)
		}
		if device.Enabled != nil && !*device.Enabled {
			continue
		}
		if device.Location == "" {
			device.Location = device.Label
		}
		targets[device.MAC] = device
	}
	return targets, nil
}

func startDiscovery(conn *dbus.Conn, adapterPath dbus.ObjectPath) error {
	adapter := conn.Object(bluezName, adapterPath)
	filter := map[string]dbus.Variant{
		"Transport": dbus.MakeVariant("le"),
	}
	if err := adapter.Call(adapterInterface+".SetDiscoveryFilter", 0, filter).Err; err != nil {
		log.Printf("SetDiscoveryFilter warning: %v", err)
	}
	return adapter.Call(adapterInterface+".StartDiscovery", 0).Err
}

func stopDiscovery(conn *dbus.Conn, adapterPath dbus.ObjectPath) error {
	return conn.Object(bluezName, adapterPath).Call(adapterInterface+".StopDiscovery", 0).Err
}

func pollReadings(conn *dbus.Conn, targets map[string]targetDevice) ([]reading, error) {
	manager := conn.Object(bluezName, dbus.ObjectPath("/"))
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := manager.Call(objectManagerInterface+".GetManagedObjects", 0).Store(&objects); err != nil {
		return nil, err
	}

	now := time.Now()
	var readings []reading
	for _, interfaces := range objects {
		props, ok := interfaces[deviceInterface]
		if !ok {
			continue
		}
		address := strings.ToLower(getString(props, "Address"))
		target, ok := targets[address]
		if !ok {
			continue
		}
		payload := firstServiceDataHex(props)
		if payload == "" {
			continue
		}
		decoded := decodeServiceData(payload)
		rssi := floatPtr(float64(getInt16(props, "RSSI")))
		if getVariant(props, "RSSI") == nil {
			rssi = nil
		}
		decoded.TS = now
		decoded.SensorMAC = target.MAC
		decoded.RSSI = rssi
		readings = append(readings, decoded)
	}
	return readings, nil
}

func getVariant(props map[string]dbus.Variant, key string) *dbus.Variant {
	value, ok := props[key]
	if !ok {
		return nil
	}
	return &value
}

func getString(props map[string]dbus.Variant, key string) string {
	value := getVariant(props, key)
	if value == nil {
		return ""
	}
	switch v := value.Value().(type) {
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func getInt16(props map[string]dbus.Variant, key string) int16 {
	value := getVariant(props, key)
	if value == nil {
		return 0
	}
	switch v := value.Value().(type) {
	case int16:
		return v
	case int32:
		return int16(v)
	case int64:
		return int16(v)
	default:
		return 0
	}
}

func firstServiceDataHex(props map[string]dbus.Variant) string {
	value := getVariant(props, "ServiceData")
	if value == nil {
		return ""
	}
	serviceData, ok := value.Value().(map[string]dbus.Variant)
	if !ok {
		return ""
	}

	preferred := []string{
		"0000fe6a-0000-1000-8000-00805f9b34fb",
		"0000ffe1-0000-1000-8000-00805f9b34fb",
		"0000feaa-0000-1000-8000-00805f9b34fb",
	}
	for _, key := range preferred {
		if value, ok := serviceData[key]; ok {
			bytes, ok := value.Value().([]byte)
			if ok && len(bytes) > 0 {
				return hex.EncodeToString(bytes)
			}
		}
	}

	keys := make([]string, 0, len(serviceData))
	for key := range serviceData {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		bytes, ok := serviceData[key].Value().([]byte)
		if ok && len(bytes) > 0 {
			return hex.EncodeToString(bytes)
		}
	}
	return ""
}

func decodeServiceData(payloadHex string) reading {
	data, err := hex.DecodeString(payloadHex)
	if err != nil {
		return reading{}
	}
	r := reading{}

	if len(data) >= 6 && data[0] == 0xa1 && data[1] == 0x01 {
		r.BatteryPercent = floatPtr(float64(data[2]))
		tempRaw := int8(data[3])
		r.TemperatureC = floatPtr(float64(tempRaw))
		humidity := float64(data[5])
		if humidity <= 100 {
			r.HumidityPercent = floatPtr(humidity)
		}
	}

	if len(data) >= 5 && data[0] == 0x02 && data[1] == 0x80 && data[2] == 0x02 {
		var candidates []float64
		for _, value := range data[3:5] {
			if value <= 100 {
				candidates = append(candidates, float64(value))
			}
		}
		if len(candidates) > 0 {
			r.BatteryPercent = floatPtr(max(candidates))
		}
	}

	if idx := indexMarker(data, []byte{0x03, 0x13}); idx >= 0 && idx+4 <= len(data) {
		tempRaw := int16(uint16(data[idx+2]) | uint16(data[idx+3])<<8)
		r.TemperatureC = floatPtr(round(float64(tempRaw)/256.0, 2))
	}
	if idx := indexMarker(data, []byte{0x02, 0x12}); idx >= 0 && idx+3 <= len(data) {
		humidity := float64(data[idx+2])
		if humidity <= 100 {
			r.HumidityPercent = floatPtr(humidity)
		}
	}

	if len(data) >= 24 && data[0] == 0x03 && data[1] == 0x05 && data[2] == 0x17 {
		bits := uint32(data[3]) | uint32(data[4])<<8 | uint32(data[5])<<16 | uint32(data[6])<<24
		r.PressureHPa = floatPtr(round(float64(math.Float32frombits(bits)), 2))

		if idx := indexMarker(data, []byte{0x04, 0x1f}); idx >= 0 && idx+5 <= len(data) {
			subtype := data[idx+2]
			value := float64(uint16(data[idx+3]) | uint16(data[idx+4])<<8)
			switch subtype {
			case 0x08:
				r.CO2PPM = floatPtr(value)
			case 0x07:
				r.ETVOC = floatPtr(value)
			}
		}
		if idx := indexMarker(data, []byte{0x03, 0x20}); idx >= 0 && idx+4 <= len(data) {
			r.Lux = floatPtr(float64(uint16(data[idx+2]) | uint16(data[idx+3])<<8))
		}
	}

	return r
}

func sanitizeReading(r reading) reading {
	r.TemperatureC = sanitizeRange(r.TemperatureC, -40, 85)
	r.HumidityPercent = sanitizeRange(r.HumidityPercent, 0, 100)
	r.BatteryPercent = sanitizeRange(r.BatteryPercent, 0, 100)
	r.RSSI = sanitizeRange(r.RSSI, -127, 20)
	r.PressureHPa = sanitizeRange(r.PressureHPa, 300, 1100)
	r.CO2PPM = sanitizeRange(r.CO2PPM, 0, 10000)
	r.Lux = sanitizeRange(r.Lux, 0, 65534)
	r.ETVOC = sanitizeRange(r.ETVOC, 0, 60000)
	return r
}

func sanitizeRange(value *float64, minValue float64, maxValue float64) *float64 {
	if value == nil || !isFinite(*value) || *value < minValue || *value > maxValue {
		return nil
	}
	return value
}

func indexMarker(data []byte, marker []byte) int {
	for i := 0; i+len(marker) <= len(data); i++ {
		match := true
		for j := range marker {
			if data[i+j] != marker[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func (c *collector) add(r reading) {
	r = sanitizeReading(r)
	window := r.TS.Truncate(time.Minute)
	key := r.SensorMAC + "|" + window.Format(time.RFC3339)
	agg := c.windows[key]
	if agg == nil {
		agg = &aggregate{
			SensorMAC: r.SensorMAC,
			Window:    window,
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
}

func readingKey(r reading) string {
	return fmt.Sprintf(
		"rssi=%s|t=%s|h=%s|b=%s|p=%s|co2=%s|lux=%s|etvoc=%s",
		ptrKey(r.RSSI),
		ptrKey(r.TemperatureC),
		ptrKey(r.HumidityPercent),
		ptrKey(r.BatteryPercent),
		ptrKey(r.PressureHPa),
		ptrKey(r.CO2PPM),
		ptrKey(r.Lux),
		ptrKey(r.ETVOC),
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

func (c *collector) flushCompleted(ctx context.Context, currentWindow time.Time) error {
	var errs []error
	for key, agg := range c.windows {
		if agg.Window.Before(currentWindow) {
			if err := c.flush(ctx, agg); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(c.windows, key)
		}
	}
	return errors.Join(errs...)
}

func (c *collector) flushAll(ctx context.Context) error {
	var errs []error
	for key, agg := range c.windows {
		if err := c.flush(ctx, agg); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(c.windows, key)
	}
	return errors.Join(errs...)
}

func (c *collector) flush(ctx context.Context, agg *aggregate) error {
	if agg.empty() {
		return nil
	}
	temperature := c.filterMetric(agg.SensorMAC, "temperature_c", agg.Window, nullableMedianFloat(agg.TemperatureC))
	humidity := c.filterMetric(agg.SensorMAC, "humidity_percent", agg.Window, nullableMedianFloat(agg.HumidityPercent))
	battery := c.filterMetric(agg.SensorMAC, "battery_percent", agg.Window, nullableMedianFloat(agg.BatteryPercent))
	rssi := c.filterMetric(agg.SensorMAC, "rssi_dbm", agg.Window, nullableMedianFloat(agg.RSSI))
	pressure := c.filterMetric(agg.SensorMAC, "pressure_hpa", agg.Window, nullableMedianFloat(agg.PressureHPa))
	co2 := c.filterMetric(agg.SensorMAC, "co2_ppm", agg.Window, nullableMedianFloat(agg.CO2PPM))
	lux := nullableMedianFloat(agg.Lux)
	etvoc := c.filterMetric(agg.SensorMAC, "etvoc", agg.Window, nullableMedianFloat(agg.ETVOC))
	if temperature == nil && humidity == nil && battery == nil && rssi == nil &&
		pressure == nil && co2 == nil && lux == nil && etvoc == nil {
		return nil
	}
	_, err := c.db.Exec(ctx, `
		INSERT INTO sensor_minute (
			ts, mac, temperature_c, humidity_percent, battery_percent,
			rssi_dbm, pressure_hpa, co2_ppm, lux, etvoc
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (ts, mac) DO UPDATE SET
			temperature_c = EXCLUDED.temperature_c,
			humidity_percent = EXCLUDED.humidity_percent,
			battery_percent = EXCLUDED.battery_percent,
			rssi_dbm = EXCLUDED.rssi_dbm,
			pressure_hpa = EXCLUDED.pressure_hpa,
			co2_ppm = EXCLUDED.co2_ppm,
			lux = EXCLUDED.lux,
			etvoc = EXCLUDED.etvoc,
			inserted_at = now()
	`, agg.Window, agg.SensorMAC,
		nullablePtr(temperature),
		nullablePtr(humidity),
		nullablePtr(battery),
		nullablePtr(rssi),
		nullablePtr(pressure),
		nullablePtr(co2),
		nullablePtr(lux),
		nullablePtr(etvoc),
	)
	if err != nil {
		return fmt.Errorf("insert %s %s: %w", agg.SensorMAC, agg.Window.Format(time.RFC3339), err)
	}
	log.Printf("flushed sensor=%s minute=%s", agg.SensorMAC, agg.Window.Format(time.RFC3339))
	return nil
}

func (c *collector) filterMetric(mac string, name string, ts time.Time, value *float64) *float64 {
	if value == nil || !c.outliers.Enabled {
		return value
	}
	threshold, ok := c.outliers.Thresholds[name]
	if !ok || threshold <= 0 || c.outliers.HistorySize <= 0 {
		return value
	}
	if c.metricHistory[mac] == nil {
		c.metricHistory[mac] = map[string]*metricHistory{}
	}
	history := c.metricHistory[mac][name]
	if history == nil {
		history = &metricHistory{}
		c.metricHistory[mac][name] = history
	}
	if len(history.Accepted) < 3 {
		c.acceptMetric(history, *value)
		return value
	}
	baseline := median(history.Accepted)
	if math.Abs(*value-baseline) <= threshold {
		c.acceptMetric(history, *value)
		return value
	}
	if history.Pending != nil &&
		ts.Sub(history.Pending.TS) <= c.outliers.ConfirmWindow &&
		math.Abs(*value-history.Pending.Value) <= threshold {
		c.acceptMetric(history, history.Pending.Value)
		c.acceptMetric(history, *value)
		return value
	}
	history.Pending = &pendingOutlier{Value: *value, TS: ts}
	log.Printf("skip BLE outlier sensor=%s metric=%s ts=%s value=%.3f baseline=%.3f threshold=%.3f", mac, name, ts.Format(time.RFC3339), *value, baseline, threshold)
	return nil
}

func (c *collector) acceptMetric(history *metricHistory, value float64) {
	history.Accepted = append(history.Accepted, value)
	if len(history.Accepted) > c.outliers.HistorySize {
		history.Accepted = history.Accepted[len(history.Accepted)-c.outliers.HistorySize:]
	}
	history.Pending = nil
}

func (agg *aggregate) empty() bool {
	return len(agg.RSSI) == 0 &&
		len(agg.TemperatureC) == 0 &&
		len(agg.HumidityPercent) == 0 &&
		len(agg.BatteryPercent) == 0 &&
		len(agg.PressureHPa) == 0 &&
		len(agg.CO2PPM) == 0 &&
		len(agg.Lux) == 0 &&
		len(agg.ETVOC) == 0
}

func ensureDevices(ctx context.Context, db *pgx.Conn, targets map[string]targetDevice) error {
	for _, target := range targets {
		_, err := db.Exec(ctx, `
			INSERT INTO devices (mac, label, device_type, location)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (mac) DO UPDATE SET
				label = EXCLUDED.label,
				device_type = EXCLUDED.device_type,
				location = EXCLUDED.location,
				updated_at = now()
		`, target.MAC, target.Label, target.DeviceType, target.Location)
		if err != nil {
			return err
		}
	}
	return nil
}

func nullableMedianFloat(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	return floatPtr(median(values))
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

func max(values []float64) float64 {
	result := values[0]
	for _, value := range values[1:] {
		if value > result {
			result = value
		}
	}
	return result
}

func round(value float64, places int) float64 {
	factor := math.Pow10(places)
	return math.Round(value*factor) / factor
}

func floatPtr(value float64) *float64 {
	return &value
}
