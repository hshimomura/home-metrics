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

type targetDevice struct {
	MAC        string `json:"mac"`
	Label      string `json:"label"`
	SensorCategory string `json:"sensor_category"`
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
	db      *pgx.Conn
	windows map[string]*aggregate
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
		db:      db,
		windows: make(map[string]*aggregate),
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	log.Printf("BLE collector started adapter=%s poll=%s db=%s", adapterPath, pollInterval, dsn)
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
		device.SensorCategory = strings.TrimSpace(device.SensorCategory)
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
		nullableMedian(agg.TemperatureC),
		nullableMedian(agg.HumidityPercent),
		nullableMedian(agg.BatteryPercent),
		nullableMedian(agg.RSSI),
		nullableMedian(agg.PressureHPa),
		nullableMedian(agg.CO2PPM),
		nullableMedian(agg.Lux),
		nullableMedian(agg.ETVOC),
	)
	if err != nil {
		return fmt.Errorf("insert %s %s: %w", agg.SensorMAC, agg.Window.Format(time.RFC3339), err)
	}
	log.Printf("flushed sensor=%s minute=%s", agg.SensorMAC, agg.Window.Format(time.RFC3339))
	return nil
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
			INSERT INTO devices (mac, label, sensor_category, location)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (mac) DO UPDATE SET
				label = EXCLUDED.label,
				sensor_category = EXCLUDED.sensor_category,
				location = EXCLUDED.location,
				updated_at = now()
		`, target.MAC, target.Label, target.SensorCategory, target.Location)
		if err != nil {
			return err
		}
	}
	return nil
}

func nullableMedian(values []float64) any {
	if len(values) == 0 {
		return nil
	}
	return median(values)
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
