package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"home-metrics/internal/collectorstatus"

	"github.com/jackc/pgx/v5"
)

const (
	defaultDBDSN              = "dbname=ble_sensors host=/var/run/postgresql"
	defaultSensorsFile        = "/etc/home-metrics/sensors.json"
	defaultAPIURL             = "https://192.168.67.6:8081"
	defaultMQTTAddr           = "192.168.67.6:41883"
	defaultOnboardAppID       = "onboard"
	defaultControlAppID       = "control"
	defaultDataAppID          = "data"
	defaultTopic              = "ioslab/home-metrics/ble/advertisements/v1"
	sensorConnectIngestSource = "cisco_sensor_connect"
	defaultReconnectMinDelay  = time.Second
	defaultReconnectMaxDelay  = time.Minute
	defaultStreamHeartbeat    = time.Minute
	defaultAggregateFlush     = 10 * time.Second
	defaultPendingLog         = 5 * time.Minute
	defaultMQTTMaxPacket      = 1 << 20
	defaultGATTBatteryPoll    = 24 * time.Hour
	defaultGATTBatteryJitter  = 30 * time.Minute
	defaultGATTAdvMaxAge      = 10 * time.Minute
)

type config struct {
	DBDSN             string
	APIURL            string
	MQTTAddr          string
	OnboardAppID      string
	OnboardAPIKey     string
	ControlAppID      string
	ControlAPIKey     string
	DataAppID         string
	DataAPIKey        string
	Topic             string
	SensorsFile       string
	RegisterDataApp   bool
	DryRun            bool
	Debug             bool
	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration
	StreamHeartbeat   time.Duration
	AggregateFlush    time.Duration
	PendingLog        time.Duration
	MQTTMaxPacket     int
	TLSSkipVerify     bool
}

type targetDevice struct {
	MAC            string             `json:"mac"`
	Label          string             `json:"label"`
	Location       string             `json:"location"`
	IngestSource   string             `json:"ingest_source"`
	SensorTypeCode string             `json:"sensor_type_code"`
	SensorCategory string             `json:"sensor_category"`
	Enabled        *bool              `json:"enabled"`
	GATTBattery    *gattBatteryConfig `json:"gatt_battery"`
}

type gattBatteryConfig struct {
	Enabled             *bool  `json:"enabled"`
	DeviceID            string `json:"device_id"`
	ServiceID           string `json:"service_id"`
	CharacteristicID    string `json:"characteristic_id"`
	PollInterval        string `json:"poll_interval"`
	Jitter              string `json:"jitter"`
	AdvertisementMaxAge string `json:"advertisement_max_age"`
}

type targetConfig struct {
	Devices []targetDevice `json:"devices"`
}

type bleReading struct {
	TS                  time.Time
	SensorMAC           string
	Label               string
	Location            string
	IngestSource        string
	SensorTypeCode      string
	SensorCategory      string
	RSSI                *float64
	TemperatureC        *float64
	HumidityPercent     *float64
	BatteryPercent      *float64
	PressureHPa         *float64
	CO2PPM              *float64
	Lux                 *float64
	ETVOC               *float64
	SoilMoisturePercent *float64
	ConductivityUSCM    *float64
}

type aggregate struct {
	SensorMAC           string
	Label               string
	Location            string
	IngestSource        string
	SensorTypeCode      string
	SensorCategory      string
	Window              time.Time
	RSSI                []float64
	TemperatureC        []float64
	HumidityPercent     []float64
	BatteryPercent      []float64
	PressureHPa         []float64
	CO2PPM              []float64
	Lux                 []float64
	ETVOC               []float64
	SoilMoisturePercent []float64
	ConductivityUSCM    []float64
	LastComparable      string
}

type collector struct {
	mu      sync.Mutex
	db      *pgx.Conn
	windows map[string]*aggregate
	flushFn func(context.Context, *aggregate) (bool, error)
}

type statusReporter struct {
	mu     sync.Mutex
	db     *pgx.Conn
	target collectorstatus.Target
}

type dataSubscription struct {
	DeviceID    string
	Data        []byte
	TS          time.Time
	APMAC       string
	BLEMAC      string
	RSSI        *int32
	Application string
}

var doHTTPRequest = func(cfg config, req *http.Request) (*http.Response, error) {
	return httpClient(cfg).Do(req)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := loadConfig()
	if source := strings.ToLower(envString("SENSOR_INGEST_SOURCE", "cisco_iot_orchestrator")); source != "cisco_iot_orchestrator" && source != "cisco-iot-orchestrator" && source != "cisco_iot" && source != "cisco-iot" {
		log.Printf("Cisco Sensor Connect (IoT Orchestrator) collector disabled by SENSOR_INGEST_SOURCE=%s", source)
		return
	}
	if cfg.DataAPIKey == "" {
		log.Fatal("CISCO_IOT_ORCH_DATA_API_KEY is required")
	}
	targets, err := loadTargets(cfg.SensorsFile)
	if err != nil {
		log.Fatalf("load BLE sensors: %v", err)
	}

	var db *pgx.Conn
	if !cfg.DryRun {
		db, err = pgx.Connect(ctx, cfg.DBDSN)
		if err != nil {
			log.Fatalf("connect database: %v", err)
		}
		defer db.Close(context.Background())
		if err := ensureDevices(ctx, db, targets); err != nil {
			log.Fatalf("ensure devices: %v", err)
		}
	}

	reporter := &statusReporter{
		db: db,
		target: collectorstatus.Target{
			CollectorName: "hm-cisco-iot-orchestrator-collector",
			TargetType:    "mqtt",
			TargetKey:     cfg.MQTTAddr + "/" + cfg.Topic,
		},
	}
	c := &collector{db: db, windows: map[string]*aggregate{}}

	if cfg.RegisterDataApp {
		if err := registerDataApp(ctx, cfg); err != nil {
			reporter.MarkFailure(ctx, err)
			log.Fatalf("register data app: %v", err)
		}
	}

	log.Printf("Cisco Sensor Connect (IoT Orchestrator) collector started mqtt=%s topic=%s data_app=%s control_app=%s dry_run=%t", cfg.MQTTAddr, cfg.Topic, cfg.DataAppID, cfg.ControlAppID, cfg.DryRun)
	if !cfg.DryRun && db != nil {
		go runGATTBatteryPoller(ctx, cfg, targets, c, reporter)
	}
	runWithReconnect(ctx, cfg, targets, c, reporter)
	if err := c.flushAll(context.Background()); err != nil {
		log.Printf("flush on shutdown: %v", err)
	}
}

func runWithReconnect(ctx context.Context, cfg config, targets map[string]targetDevice, c *collector, reporter *statusReporter) {
	delay := cfg.ReconnectMinDelay
	lastPendingLog := time.Time{}
	for ctx.Err() == nil {
		err := runMQTT(ctx, cfg, func(topic string, payload []byte) {
			if cfg.Debug {
				log.Printf("mqtt message topic=%s bytes=%d", topic, len(payload))
			}
			readings, err := decodeDataBatch(payload, targets)
			if err != nil {
				log.Printf("decode MQTT protobuf: %v", err)
				reporter.MarkFailure(ctx, err)
				return
			}
			for _, reading := range readings {
				c.add(reading)
			}
			lastPendingLog = flushPending(ctx, c, reporter, "message", cfg.PendingLog, lastPendingLog)
		}, func() {
			lastPendingLog = flushPending(ctx, c, reporter, "ticker", cfg.PendingLog, lastPendingLog)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			count, oldest := c.pendingSummary(time.Now())
			if count > 0 {
				log.Printf("Cisco Sensor Connect aggregate pending after MQTT stream end windows=%d oldest_age=%s", count, oldest.Round(time.Second))
			}
		}
		log.Printf("MQTT stream ended: %v; reconnecting in %s", err, delay)
		reporter.MarkFailure(ctx, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > cfg.ReconnectMaxDelay {
			delay = cfg.ReconnectMaxDelay
		}
	}
}

func flushPending(ctx context.Context, c *collector, reporter *statusReporter, reason string, logEvery time.Duration, lastLog time.Time) time.Time {
	now := time.Now()
	flushed, err := c.flushCompleted(ctx, now.Truncate(time.Minute))
	if err != nil {
		count, oldest := c.pendingSummary(now)
		log.Printf("flush Cisco Sensor Connect readings reason=%s pending_windows=%d oldest_age=%s: %v", reason, count, oldest.Round(time.Second), err)
		reporter.MarkFailure(ctx, err)
		return maybeLogPending(c, now, logEvery, lastLog)
	}
	if flushed > 0 {
		reporter.MarkDataSuccess(ctx)
	} else {
		reporter.MarkSuccess(ctx)
	}
	return maybeLogPending(c, now, logEvery, lastLog)
}

func maybeLogPending(c *collector, now time.Time, logEvery time.Duration, lastLog time.Time) time.Time {
	if logEvery <= 0 || (!lastLog.IsZero() && now.Sub(lastLog) < logEvery) {
		return lastLog
	}
	count, oldest := c.pendingSummary(now)
	if count == 0 || oldest < time.Minute {
		return lastLog
	}
	log.Printf("Cisco Sensor Connect aggregate pending windows=%d oldest_age=%s", count, oldest.Round(time.Second))
	return now
}

func registerDataApp(ctx context.Context, cfg config) error {
	body := map[string]any{
		"dataApps":   []map[string]string{{"dataAppID": cfg.DataAppID}},
		"topic":      cfg.Topic,
		"dataFormat": "default",
		"controlApp": cfg.ControlAppID,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(cfg.APIURL, "/") + "/control/registration/registerDataApp"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", cfg.ControlAPIKey)
	resp, err := doHTTPRequest(cfg, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("register data app status=%s body=%s", resp.Status, string(limited))
	}
	var result struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(limited, &result); err == nil && strings.EqualFold(result.Status, "FAILURE") {
		if result.Reason == "" {
			result.Reason = string(limited)
		}
		return fmt.Errorf("register data app failed: %s", result.Reason)
	}
	log.Printf("registered Cisco Sensor Connect data app app=%s control_app=%s topic=%s", cfg.DataAppID, cfg.ControlAppID, cfg.Topic)
	return nil
}

func runGATTBatteryPoller(ctx context.Context, cfg config, targets map[string]targetDevice, c *collector, reporter *statusReporter) {
	pollTargets := gattBatteryTargets(targets)
	if len(pollTargets) == 0 {
		return
	}
	if strings.TrimSpace(cfg.ControlAPIKey) == "" {
		log.Printf("Cisco Sensor Connect GATT battery polling disabled: CISCO_IOT_ORCH_CONTROL_API_KEY is empty")
		return
	}
	nextDue := map[string]time.Time{}
	now := time.Now()
	for mac, target := range pollTargets {
		nextDue[mac] = initialGATTBatteryDue(ctx, c.db, target, now)
		log.Printf("scheduled Cisco Sensor Connect GATT battery poll sensor=%s due=%s", mac, nextDue[mac].Format(time.RFC3339))
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now = <-ticker.C:
		}
		for mac, target := range pollTargets {
			if now.Before(nextDue[mac]) {
				continue
			}
			if err := pollGATTBattery(ctx, cfg, target, c); err != nil {
				log.Printf("poll Cisco Sensor Connect GATT battery sensor=%s: %v", mac, err)
				nextDue[mac] = now.Add(15 * time.Minute)
				continue
			}
			reporter.MarkDataSuccess(ctx)
			nextDue[mac] = nextGATTBatteryDue(now, target)
			log.Printf("scheduled next Cisco Sensor Connect GATT battery poll sensor=%s due=%s", mac, nextDue[mac].Format(time.RFC3339))
		}
	}
}

func gattBatteryTargets(targets map[string]targetDevice) map[string]targetDevice {
	out := map[string]targetDevice{}
	for mac, target := range targets {
		if !gattBatteryEnabled(target) {
			continue
		}
		out[mac] = target
	}
	return out
}

func gattBatteryEnabled(target targetDevice) bool {
	if target.GATTBattery == nil {
		return false
	}
	if target.GATTBattery.Enabled != nil && !*target.GATTBattery.Enabled {
		return false
	}
	return strings.TrimSpace(target.GATTBattery.DeviceID) != ""
}

func initialGATTBatteryDue(ctx context.Context, db *pgx.Conn, target targetDevice, now time.Time) time.Time {
	if db == nil {
		return now.Add(randomNonNegativeDuration(gattBatteryJitter(target)))
	}
	lastBattery, err := latestBatteryAt(ctx, db, target.MAC)
	if err != nil {
		log.Printf("read latest GATT battery time sensor=%s: %v", target.MAC, err)
		return now.Add(randomNonNegativeDuration(gattBatteryJitter(target)))
	}
	if lastBattery.IsZero() {
		return now.Add(randomNonNegativeDuration(gattBatteryJitter(target)))
	}
	due := lastBattery.Add(gattBatteryPollInterval(target)).Add(randomSignedDuration(gattBatteryJitter(target)))
	if due.Before(now) {
		return now
	}
	return due
}

func nextGATTBatteryDue(now time.Time, target targetDevice) time.Time {
	return now.Add(gattBatteryPollInterval(target)).Add(randomSignedDuration(gattBatteryJitter(target)))
}

func latestBatteryAt(ctx context.Context, db *pgx.Conn, mac string) (time.Time, error) {
	var ts *time.Time
	err := db.QueryRow(ctx, `
		SELECT max(ts)
		FROM sensor_minute
		WHERE mac = $1 AND battery_percent IS NOT NULL
	`, mac).Scan(&ts)
	if err != nil {
		return time.Time{}, err
	}
	if ts == nil {
		return time.Time{}, nil
	}
	return *ts, nil
}

func latestTelemetryAt(ctx context.Context, db *pgx.Conn, mac string) (time.Time, error) {
	var ts *time.Time
	err := db.QueryRow(ctx, `
		SELECT max(ts)
		FROM sensor_minute
		WHERE mac = $1 AND (
			temperature_c IS NOT NULL OR humidity_percent IS NOT NULL OR
			battery_percent IS NOT NULL OR rssi_dbm IS NOT NULL OR
			pressure_hpa IS NOT NULL OR co2_ppm IS NOT NULL OR
			lux IS NOT NULL OR etvoc IS NOT NULL OR
			soil_moisture_percent IS NOT NULL OR conductivity_us_cm IS NOT NULL
		)
	`, mac).Scan(&ts)
	if err != nil {
		return time.Time{}, err
	}
	if ts == nil {
		return time.Time{}, nil
	}
	return *ts, nil
}

func pollGATTBattery(ctx context.Context, cfg config, target targetDevice, c *collector) error {
	if c == nil || c.db == nil {
		return nil
	}
	lastTelemetry, err := latestTelemetryAt(ctx, c.db, target.MAC)
	if err != nil {
		return err
	}
	maxAge := gattBatteryAdvertisementMaxAge(target)
	if lastTelemetry.IsZero() || time.Since(lastTelemetry) > maxAge {
		return fmt.Errorf("latest advertisement is stale: latest=%s max_age=%s", lastTelemetry.Format(time.RFC3339), maxAge)
	}
	battery, firmware, err := readGATTBattery(ctx, cfg, *target.GATTBattery)
	if err != nil {
		return err
	}
	now := time.Now()
	c.add(bleReading{
		TS:             now,
		SensorMAC:      target.MAC,
		Label:          target.Label,
		Location:       strings.TrimSpace(target.Location),
		IngestSource:   target.IngestSource,
		SensorTypeCode: target.SensorTypeCode,
		SensorCategory: target.SensorCategory,
		BatteryPercent: floatPtr(float64(battery)),
	})
	if _, err := c.flushCompleted(ctx, now.Add(time.Minute).Truncate(time.Minute)); err != nil {
		return err
	}
	log.Printf("stored Cisco Sensor Connect GATT battery sensor=%s battery=%d firmware=%q", target.MAC, battery, firmware)
	return nil
}

func readGATTBattery(ctx context.Context, cfg config, batteryCfg gattBatteryConfig) (int, string, error) {
	baseBody := map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(batteryCfg.DeviceID),
		"controlApp": cfg.ControlAppID,
	}
	serviceID := gattBatteryServiceID(batteryCfg)
	characteristicID := gattBatteryCharacteristicID(batteryCfg)
	if _, err := controlPost(ctx, cfg, "/control/connectivity/connect", map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(batteryCfg.DeviceID),
		"controlApp": cfg.ControlAppID,
		"ble": map[string]any{
			"services": []map[string]string{{"serviceID": serviceID}},
		},
	}); err != nil {
		return 0, "", err
	}
	defer func() {
		if _, err := controlPost(context.Background(), cfg, "/control/connectivity/disconnect", baseBody); err != nil {
			log.Printf("disconnect Cisco Sensor Connect GATT device=%s: %v", strings.TrimSpace(batteryCfg.DeviceID), err)
		}
	}()
	body, err := controlPost(ctx, cfg, "/control/data/read", map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(batteryCfg.DeviceID),
		"controlApp": cfg.ControlAppID,
		"ble": map[string]any{
			"serviceID":        serviceID,
			"characteristicID": characteristicID,
		},
	})
	if err != nil {
		return 0, "", err
	}
	var response struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, "", fmt.Errorf("parse GATT battery response: %w", err)
	}
	payload, err := decodeHexValue(response.Value)
	if err != nil {
		return 0, "", err
	}
	if len(payload) < 1 {
		return 0, "", errors.New("empty GATT battery payload")
	}
	battery := int(payload[0])
	if battery < 0 || battery > 100 {
		return 0, "", fmt.Errorf("GATT battery out of range: %d", battery)
	}
	firmware := ""
	if len(payload) >= 3 {
		firmware = string(payload[2:])
	}
	return battery, firmware, nil
}

func controlPost(ctx context.Context, cfg config, path string, body map[string]any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.APIURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", cfg.ControlAPIKey)
	resp, err := doHTTPRequest(cfg, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s status=%s body=%s", path, resp.Status, string(limited))
	}
	var result struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(limited, &result); err == nil && strings.EqualFold(result.Status, "FAILURE") {
		if result.Reason == "" {
			result.Reason = string(limited)
		}
		return nil, fmt.Errorf("%s failed: %s", path, result.Reason)
	}
	return limited, nil
}

func decodeHexValue(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, ":", "")
	value = strings.TrimPrefix(value, "0x")
	if value == "" {
		return nil, errors.New("empty hex value")
	}
	data, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode hex value %q: %w", value, err)
	}
	return data, nil
}

func httpClient(cfg config) *http.Client {
	if !cfg.TLSSkipVerify {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Lab IoT Orchestrator uses an IP-address HTTPS endpoint.
		},
		Timeout: 60 * time.Second,
	}
}

func gattBatteryServiceID(cfg gattBatteryConfig) string {
	if value := strings.TrimSpace(cfg.ServiceID); value != "" {
		return value
	}
	return "1204"
}

func gattBatteryCharacteristicID(cfg gattBatteryConfig) string {
	if value := strings.TrimSpace(cfg.CharacteristicID); value != "" {
		return value
	}
	return "00001a02-0000-1000-8000-00805f9b34fb"
}

func gattBatteryPollInterval(target targetDevice) time.Duration {
	if target.GATTBattery == nil {
		return defaultGATTBatteryPoll
	}
	return parsePositiveDuration(target.GATTBattery.PollInterval, defaultGATTBatteryPoll)
}

func gattBatteryJitter(target targetDevice) time.Duration {
	if target.GATTBattery == nil {
		return defaultGATTBatteryJitter
	}
	return parseNonNegativeDuration(target.GATTBattery.Jitter, defaultGATTBatteryJitter)
}

func gattBatteryAdvertisementMaxAge(target targetDevice) time.Duration {
	if target.GATTBattery == nil {
		return defaultGATTAdvMaxAge
	}
	return parsePositiveDuration(target.GATTBattery.AdvertisementMaxAge, defaultGATTAdvMaxAge)
}

func parsePositiveDuration(value string, fallback time.Duration) time.Duration {
	parsed := parseNonNegativeDuration(value, fallback)
	if parsed <= 0 {
		return fallback
	}
	return parsed
}

func parseNonNegativeDuration(value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func randomSignedDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return randomNonNegativeDuration(2*max) - max
}

func randomNonNegativeDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return time.Duration(time.Now().UnixNano() % int64(max))
	}
	value := binary.BigEndian.Uint64(buf)
	return time.Duration(value % uint64(max))
}

func loadConfig() config {
	return config{
		DBDSN:             envString("BLE_DB_DSN", defaultDBDSN),
		APIURL:            envString("CISCO_IOT_ORCH_API_URL", defaultAPIURL),
		MQTTAddr:          envString("CISCO_IOT_ORCH_MQTT_ADDR", defaultMQTTAddr),
		MQTTMaxPacket:     envInt("CISCO_IOT_ORCH_MQTT_MAX_PACKET_BYTES", defaultMQTTMaxPacket),
		OnboardAppID:      envString("CISCO_IOT_ORCH_ONBOARD_APP_ID", defaultOnboardAppID),
		OnboardAPIKey:     envString("CISCO_IOT_ORCH_ONBOARD_API_KEY", ""),
		ControlAppID:      envString("CISCO_IOT_ORCH_CONTROL_APP_ID", defaultControlAppID),
		ControlAPIKey:     envString("CISCO_IOT_ORCH_CONTROL_API_KEY", ""),
		DataAppID:         envString("CISCO_IOT_ORCH_DATA_APP_ID", defaultDataAppID),
		DataAPIKey:        envString("CISCO_IOT_ORCH_DATA_API_KEY", ""),
		Topic:             envString("CISCO_IOT_ORCH_TOPIC", defaultTopic),
		SensorsFile:       envString("BLE_SENSORS_FILE", defaultSensorsFile),
		RegisterDataApp:   envBool("CISCO_IOT_ORCH_REGISTER_DATA_APP", false),
		DryRun:            envBool("CISCO_IOT_ORCH_DRY_RUN", false),
		Debug:             envBool("CISCO_IOT_ORCH_DEBUG", false),
		ReconnectMinDelay: envDuration("CISCO_IOT_ORCH_RECONNECT_MIN_DELAY", defaultReconnectMinDelay),
		ReconnectMaxDelay: envDuration("CISCO_IOT_ORCH_RECONNECT_MAX_DELAY", defaultReconnectMaxDelay),
		StreamHeartbeat:   envDuration("CISCO_IOT_ORCH_STREAM_HEARTBEAT", defaultStreamHeartbeat),
		AggregateFlush:    envDuration("CISCO_IOT_ORCH_AGGREGATE_FLUSH_INTERVAL", defaultAggregateFlush),
		PendingLog:        envDuration("CISCO_IOT_ORCH_PENDING_LOG_INTERVAL", defaultPendingLog),
		TLSSkipVerify:     envBool("CISCO_IOT_ORCH_TLS_SKIP_VERIFY", true),
	}
}

func loadTargets(path string) (map[string]targetDevice, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg targetConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	targets := map[string]targetDevice{}
	for _, device := range cfg.Devices {
		device.MAC = normalizeMAC(device.MAC)
		device.Label = strings.TrimSpace(device.Label)
		if device.MAC == "" {
			return nil, errors.New("sensor mac is required")
		}
		if device.Label == "" {
			device.Label = device.MAC
		}
		device.Location = strings.TrimSpace(device.Location)
		device.IngestSource = normalizeIngestSource(device.IngestSource)
		device.SensorTypeCode = strings.TrimSpace(device.SensorTypeCode)
		device.SensorCategory = normalizeSensorCategory(device.SensorCategory, device.SensorTypeCode)
		if device.Enabled != nil && !*device.Enabled {
			continue
		}
		targets[device.MAC] = device
	}
	return targets, nil
}

func normalizeIngestSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return sensorConnectIngestSource
	}
	return source
}

func normalizeSensorCategory(category string, sensorTypeCode string) string {
	category = strings.TrimSpace(category)
	if category != "" {
		return category
	}
	switch strings.TrimSpace(sensorTypeCode) {
	case "xiaomi_flower_care":
		return "plant"
	case "minew_s1", "env_ble":
		return "environment"
	default:
		return ""
	}
}

func decodeDataBatch(payload []byte, targets map[string]targetDevice) ([]bleReading, error) {
	messages, err := parseDataBatch(payload)
	if err != nil {
		return nil, err
	}
	var readings []bleReading
	for _, msg := range messages {
		mac := normalizeMAC(msg.BLEMAC)
		if mac == "" {
			mac = normalizeMAC(msg.DeviceID)
		}
		target, ok := targets[mac]
		if !ok {
			continue
		}
		decoded := decodeBLEPayload(msg.Data)
		if decoded.empty() {
			continue
		}
		if msg.TS.IsZero() {
			decoded.TS = time.Now()
		} else {
			decoded.TS = msg.TS
		}
		decoded.SensorMAC = mac
		decoded.Label = target.Label
		decoded.Location = strings.TrimSpace(target.Location)
		decoded.IngestSource = target.IngestSource
		decoded.SensorTypeCode = target.SensorTypeCode
		decoded.SensorCategory = target.SensorCategory
		if msg.RSSI != nil {
			decoded.RSSI = floatPtr(float64(*msg.RSSI))
		}
		readings = append(readings, decoded)
	}
	return readings, nil
}

func parseDataBatch(data []byte) ([]dataSubscription, error) {
	var messages []dataSubscription
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return nil, err
		}
		data = rest
		if field != 1 || wire != 2 {
			data, err = skipProtoValue(wire, data)
			if err != nil {
				return nil, err
			}
			continue
		}
		item, rest, err := consumeBytes(data)
		if err != nil {
			return nil, err
		}
		data = rest
		msg, err := parseDataSubscription(item)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

func parseDataSubscription(data []byte) (dataSubscription, error) {
	var msg dataSubscription
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return msg, err
		}
		data = rest
		switch field {
		case 1:
			msg.DeviceID, data, err = consumeString(data)
		case 2:
			msg.Data, data, err = consumeBytes(data)
		case 3:
			var value []byte
			value, data, err = consumeBytes(data)
			msg.TS = parseTimestamp(value)
		case 4:
			msg.APMAC, data, err = consumeString(data)
		case 12:
			var value []byte
			value, data, err = consumeBytes(data)
			msg.BLEMAC, msg.RSSI = parseBLEAdvertisement(value)
		case 16:
			var value []byte
			value, data, err = consumeBytes(data)
			msg.Application = parseApplicationEvent(value)
		default:
			data, err = skipProtoValue(wire, data)
		}
		if err != nil {
			return msg, err
		}
	}
	return msg, nil
}

func parseTimestamp(data []byte) time.Time {
	var seconds int64
	var nanos int32
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return time.Time{}
		}
		data = rest
		switch field {
		case 1:
			value, rest, err := consumeVarint(data)
			if err != nil {
				return time.Time{}
			}
			seconds = int64(value)
			data = rest
		case 2:
			value, rest, err := consumeVarint(data)
			if err != nil {
				return time.Time{}
			}
			nanos = int32(value)
			data = rest
		default:
			data, err = skipProtoValue(wire, data)
			if err != nil {
				return time.Time{}
			}
		}
	}
	if seconds == 0 && nanos == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, int64(nanos)).UTC()
}

func parseBLEAdvertisement(data []byte) (string, *int32) {
	var mac string
	var rssi *int32
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return mac, rssi
		}
		data = rest
		switch field {
		case 1:
			mac, data, err = consumeString(data)
		case 2:
			var value uint64
			value, data, err = consumeVarint(data)
			signed := int32(value)
			rssi = &signed
		default:
			data, err = skipProtoValue(wire, data)
		}
		if err != nil {
			return mac, rssi
		}
	}
	return mac, rssi
}

func parseApplicationEvent(data []byte) string {
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return ""
		}
		data = rest
		if field == 1 && wire == 2 {
			value, _, err := consumeString(data)
			if err != nil {
				return ""
			}
			return value
		}
		data, err = skipProtoValue(wire, data)
		if err != nil {
			return ""
		}
	}
	return ""
}

func consumeKey(data []byte) (uint64, uint64, []byte, error) {
	key, rest, err := consumeVarint(data)
	if err != nil {
		return 0, 0, data, err
	}
	return key >> 3, key & 0x7, rest, nil
}

func consumeVarint(data []byte) (uint64, []byte, error) {
	var value uint64
	for i := 0; i < len(data) && i < 10; i++ {
		b := data[i]
		value |= uint64(b&0x7f) << uint(7*i)
		if b < 0x80 {
			return value, data[i+1:], nil
		}
	}
	return 0, data, io.ErrUnexpectedEOF
}

func consumeBytes(data []byte) ([]byte, []byte, error) {
	length, rest, err := consumeVarint(data)
	if err != nil {
		return nil, data, err
	}
	if length > uint64(len(rest)) {
		return nil, data, io.ErrUnexpectedEOF
	}
	return rest[:length], rest[length:], nil
}

func consumeString(data []byte) (string, []byte, error) {
	value, rest, err := consumeBytes(data)
	if err != nil {
		return "", data, err
	}
	return string(value), rest, nil
}

func skipProtoValue(wire uint64, data []byte) ([]byte, error) {
	switch wire {
	case 0:
		_, rest, err := consumeVarint(data)
		return rest, err
	case 1:
		if len(data) < 8 {
			return data, io.ErrUnexpectedEOF
		}
		return data[8:], nil
	case 2:
		_, rest, err := consumeBytes(data)
		return rest, err
	case 5:
		if len(data) < 4 {
			return data, io.ErrUnexpectedEOF
		}
		return data[4:], nil
	default:
		return data, fmt.Errorf("unsupported protobuf wire type %d", wire)
	}
}

func runMQTT(ctx context.Context, cfg config, onMessage func(topic string, payload []byte), onFlushTick func()) error {
	conn, err := net.DialTimeout("tcp", cfg.MQTTAddr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	clientID := "home-metrics-" + randomHex(4)
	if err := mqttConnect(conn, clientID, cfg.DataAppID, cfg.DataAPIKey, cfg.MQTTMaxPacket); err != nil {
		return err
	}
	if err := mqttSubscribe(conn, 1, cfg.Topic, cfg.MQTTMaxPacket); err != nil {
		return err
	}
	heartbeat := time.NewTicker(cfg.StreamHeartbeat)
	defer heartbeat.Stop()
	var flushTick <-chan time.Time
	var flushTicker *time.Ticker
	if cfg.AggregateFlush > 0 {
		flushTicker = time.NewTicker(cfg.AggregateFlush)
		flushTick = flushTicker.C
		defer flushTicker.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			if err := writeMQTTPacket(conn, 0xc0, nil); err != nil {
				return err
			}
		case <-flushTick:
			if onFlushTick != nil {
				onFlushTick()
			}
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		packetType, payload, err := readMQTTPacket(conn, cfg.MQTTMaxPacket)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		if packetType == 3 {
			topic, body, err := parsePublish(payload)
			if err != nil {
				return err
			}
			onMessage(topic, body)
		}
	}
}

func mqttConnect(conn net.Conn, clientID string, username string, password string, maxPacket int) error {
	var variable bytes.Buffer
	writeMQTTString(&variable, "MQTT")
	variable.WriteByte(4)
	variable.WriteByte(0x80 | 0x40 | 0x02)
	_ = binary.Write(&variable, binary.BigEndian, uint16(60))
	writeMQTTString(&variable, clientID)
	writeMQTTString(&variable, username)
	writeMQTTString(&variable, password)
	if err := writeMQTTPacket(conn, 0x10, variable.Bytes()); err != nil {
		return err
	}
	packetType, payload, err := readMQTTPacket(conn, maxPacket)
	if err != nil {
		return err
	}
	if packetType != 2 || len(payload) < 2 {
		return fmt.Errorf("unexpected MQTT CONNACK packet type=%d", packetType)
	}
	if payload[1] != 0 {
		return fmt.Errorf("MQTT connect rejected code=%d", payload[1])
	}
	return nil
}

func mqttSubscribe(conn net.Conn, packetID uint16, topic string, maxPacket int) error {
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, packetID)
	writeMQTTString(&payload, topic)
	payload.WriteByte(0)
	if err := writeMQTTPacket(conn, 0x82, payload.Bytes()); err != nil {
		return err
	}
	packetType, body, err := readMQTTPacket(conn, maxPacket)
	if err != nil {
		return err
	}
	if packetType != 9 || len(body) < 3 {
		return fmt.Errorf("unexpected MQTT SUBACK packet type=%d", packetType)
	}
	if body[len(body)-1] == 0x80 {
		return errors.New("MQTT subscribe rejected")
	}
	return nil
}

func parsePublish(payload []byte) (string, []byte, error) {
	if len(payload) < 2 {
		return "", nil, io.ErrUnexpectedEOF
	}
	topicLen := int(binary.BigEndian.Uint16(payload[:2]))
	if len(payload) < 2+topicLen {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(payload[2 : 2+topicLen]), payload[2+topicLen:], nil
}

func writeMQTTPacket(conn net.Conn, header byte, payload []byte) error {
	var frame bytes.Buffer
	frame.WriteByte(header)
	writeRemainingLength(&frame, len(payload))
	frame.Write(payload)
	_, err := conn.Write(frame.Bytes())
	return err
}

func readMQTTPacket(conn io.Reader, maxPacket int) (int, []byte, error) {
	header := make([]byte, 1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	length, err := readRemainingLength(conn)
	if err != nil {
		return 0, nil, err
	}
	if maxPacket <= 0 {
		maxPacket = defaultMQTTMaxPacket
	}
	if length > maxPacket {
		return 0, nil, fmt.Errorf("MQTT packet too large: %d bytes exceeds %d", length, maxPacket)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return int(header[0] >> 4), payload, nil
}

func writeRemainingLength(buf *bytes.Buffer, length int) {
	for {
		encoded := byte(length % 128)
		length /= 128
		if length > 0 {
			encoded |= 128
		}
		buf.WriteByte(encoded)
		if length == 0 {
			return
		}
	}
}

func readRemainingLength(r io.Reader) (int, error) {
	var multiplier int = 1
	var value int
	for i := 0; i < 4; i++ {
		var encoded [1]byte
		if _, err := io.ReadFull(r, encoded[:]); err != nil {
			return 0, err
		}
		value += int(encoded[0]&127) * multiplier
		if encoded[0]&128 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, errors.New("malformed MQTT remaining length")
}

func writeMQTTString(buf *bytes.Buffer, value string) {
	_ = binary.Write(buf, binary.BigEndian, uint16(len(value)))
	buf.WriteString(value)
}

func randomHex(bytesLen int) string {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

func decodeBLEPayload(data []byte) bleReading {
	serviceDataList := serviceDataFromAdvertisement(data)
	if len(serviceDataList) == 0 {
		return decodeServiceData(hex.EncodeToString(data))
	}
	reading := bleReading{}
	for _, serviceData := range serviceDataList {
		reading.merge(decodeServiceData(hex.EncodeToString(serviceData)))
	}
	return reading
}

func serviceDataFromAdvertisement(data []byte) [][]byte {
	var out [][]byte
	for len(data) >= 2 {
		length := int(data[0])
		if length == 0 {
			break
		}
		if length+1 > len(data) {
			break
		}
		adType := data[1]
		adData := data[2 : length+1]
		if adType == 0x16 && len(adData) >= 2 {
			uuid := binary.LittleEndian.Uint16(adData[:2])
			if uuid == 0xfe6a || uuid == 0xffe1 || uuid == 0xfeaa || uuid == 0xfe95 {
				out = append(out, adData[2:])
			}
		}
		data = data[length+1:]
	}
	return out
}

func decodeServiceData(payloadHex string) bleReading {
	data, err := hex.DecodeString(payloadHex)
	if err != nil {
		return bleReading{}
	}
	r := bleReading{}
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
	if len(data) >= 7 && data[0] == 0x03 && data[1] == 0x05 && data[2] == 0x17 {
		bits := uint32(data[3]) | uint32(data[4])<<8 | uint32(data[5])<<16 | uint32(data[6])<<24
		r.PressureHPa = floatPtr(round(float64(math.Float32frombits(bits)), 2))
		if idx := indexMarker(data, []byte{0x04, 0x1f}); idx >= 0 && idx+5 <= len(data) {
			subtype := data[idx+2]
			value := float64(uint16(data[idx+3]) | uint16(data[idx+4])<<8)
			switch subtype {
			case 0x07:
				r.CO2PPM = floatPtr(value)
			case 0x08:
				r.ETVOC = floatPtr(value)
			}
		}
		if idx := indexMarker(data, []byte{0x03, 0x20}); idx >= 0 && idx+4 <= len(data) {
			r.Lux = floatPtr(float64(uint16(data[idx+2]) | uint16(data[idx+3])<<8))
		}
	}
	r.merge(decodeXiaomiFE95(data))
	return sanitizeReading(r)
}

func decodeXiaomiFE95(data []byte) bleReading {
	r := bleReading{}
	if len(data) < 15 {
		return r
	}
	if data[0] != 0x71 || data[1] != 0x20 || data[2] != 0x98 || data[3] != 0x00 {
		return r
	}
	for offset := 12; offset+3 <= len(data); {
		objectID := uint16(data[offset]) | uint16(data[offset+1])<<8
		length := int(data[offset+2])
		valueStart := offset + 3
		valueEnd := valueStart + length
		if valueEnd > len(data) {
			break
		}
		value := data[valueStart:valueEnd]
		switch objectID {
		case 0x1004:
			if len(value) >= 2 {
				raw := int16(uint16(value[0]) | uint16(value[1])<<8)
				r.TemperatureC = floatPtr(round(float64(raw)/10.0, 1))
			}
		case 0x1007:
			if len(value) >= 3 {
				lux := uint32(value[0]) | uint32(value[1])<<8 | uint32(value[2])<<16
				r.Lux = floatPtr(float64(lux))
			}
		case 0x1008:
			if len(value) >= 1 {
				r.SoilMoisturePercent = floatPtr(float64(value[0]))
			}
		case 0x1009:
			if len(value) >= 2 {
				conductivity := uint16(value[0]) | uint16(value[1])<<8
				r.ConductivityUSCM = floatPtr(float64(conductivity))
			}
		}
		offset = valueEnd
	}
	return r
}

func sanitizeReading(r bleReading) bleReading {
	r.TemperatureC = sanitizeRange(r.TemperatureC, -40, 85)
	r.HumidityPercent = sanitizeRange(r.HumidityPercent, 0, 100)
	r.BatteryPercent = sanitizeRange(r.BatteryPercent, 0, 100)
	r.RSSI = sanitizeRange(r.RSSI, -127, 20)
	r.PressureHPa = sanitizeRange(r.PressureHPa, 300, 1100)
	r.CO2PPM = sanitizeRange(r.CO2PPM, 0, 10000)
	r.Lux = sanitizeRange(r.Lux, 0, 65534)
	r.ETVOC = sanitizeRange(r.ETVOC, 0, 60000)
	r.SoilMoisturePercent = sanitizeRange(r.SoilMoisturePercent, 0, 100)
	r.ConductivityUSCM = sanitizeRange(r.ConductivityUSCM, 0, 10000)
	return r
}

func (r *bleReading) merge(other bleReading) {
	if other.TemperatureC != nil {
		r.TemperatureC = other.TemperatureC
	}
	if other.HumidityPercent != nil {
		r.HumidityPercent = other.HumidityPercent
	}
	if other.BatteryPercent != nil {
		r.BatteryPercent = other.BatteryPercent
	}
	if other.PressureHPa != nil {
		r.PressureHPa = other.PressureHPa
	}
	if other.CO2PPM != nil {
		r.CO2PPM = other.CO2PPM
	}
	if other.Lux != nil {
		r.Lux = other.Lux
	}
	if other.ETVOC != nil {
		r.ETVOC = other.ETVOC
	}
	if other.SoilMoisturePercent != nil {
		r.SoilMoisturePercent = other.SoilMoisturePercent
	}
	if other.ConductivityUSCM != nil {
		r.ConductivityUSCM = other.ConductivityUSCM
	}
}

func (r bleReading) empty() bool {
	return r.TemperatureC == nil &&
		r.HumidityPercent == nil &&
		r.BatteryPercent == nil &&
		r.RSSI == nil &&
		r.PressureHPa == nil &&
		r.CO2PPM == nil &&
		r.Lux == nil &&
		r.ETVOC == nil &&
		r.SoilMoisturePercent == nil &&
		r.ConductivityUSCM == nil
}

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
	if err := upsertDevice(ctx, c.db, agg.SensorMAC, agg.Label, agg.Location, agg.IngestSource, agg.SensorTypeCode, agg.SensorCategory); err != nil {
		return false, err
	}
	_, err := c.db.Exec(ctx, `
		INSERT INTO sensor_minute (
			ts, mac, temperature_c, humidity_percent, battery_percent,
			rssi_dbm, pressure_hpa, co2_ppm, lux, etvoc,
			soil_moisture_percent, conductivity_us_cm
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (ts, mac) DO UPDATE SET
			temperature_c = COALESCE(EXCLUDED.temperature_c, sensor_minute.temperature_c),
			humidity_percent = COALESCE(EXCLUDED.humidity_percent, sensor_minute.humidity_percent),
			battery_percent = COALESCE(EXCLUDED.battery_percent, sensor_minute.battery_percent),
			rssi_dbm = COALESCE(EXCLUDED.rssi_dbm, sensor_minute.rssi_dbm),
			pressure_hpa = COALESCE(EXCLUDED.pressure_hpa, sensor_minute.pressure_hpa),
			co2_ppm = COALESCE(EXCLUDED.co2_ppm, sensor_minute.co2_ppm),
			lux = COALESCE(EXCLUDED.lux, sensor_minute.lux),
			etvoc = COALESCE(EXCLUDED.etvoc, sensor_minute.etvoc),
			soil_moisture_percent = COALESCE(EXCLUDED.soil_moisture_percent, sensor_minute.soil_moisture_percent),
			conductivity_us_cm = COALESCE(EXCLUDED.conductivity_us_cm, sensor_minute.conductivity_us_cm),
			inserted_at = now()
	`, agg.Window, agg.SensorMAC,
		nullablePtr(nullableMedianFloat(agg.TemperatureC)),
		nullablePtr(nullableMedianFloat(agg.HumidityPercent)),
		nullablePtr(nullableMedianFloat(agg.BatteryPercent)),
		nullablePtr(nullableMedianFloat(agg.RSSI)),
		nullablePtr(nullableMedianFloat(agg.PressureHPa)),
		nullablePtr(nullableMedianFloat(agg.CO2PPM)),
		nullablePtr(nullableMedianFloat(agg.Lux)),
		nullablePtr(nullableMedianFloat(agg.ETVOC)),
		nullablePtr(nullableMedianFloat(agg.SoilMoisturePercent)),
		nullablePtr(nullableMedianFloat(agg.ConductivityUSCM)),
	)
	if err != nil {
		return false, fmt.Errorf("insert %s %s: %w", agg.SensorMAC, agg.Window.Format(time.RFC3339), err)
	}
	log.Printf("flushed Cisco Sensor Connect sensor=%s minute=%s", agg.SensorMAC, agg.Window.Format(time.RFC3339))
	return true, nil
}

func ensureDevices(ctx context.Context, db *pgx.Conn, targets map[string]targetDevice) error {
	for _, target := range targets {
		if err := upsertDevice(ctx, db, target.MAC, target.Label, target.Location, target.IngestSource, target.SensorTypeCode, target.SensorCategory); err != nil {
			return err
		}
	}
	return nil
}

func upsertDevice(ctx context.Context, db *pgx.Conn, mac string, label string, location string, ingestSource string, sensorTypeCode string, sensorCategory string) error {
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
	sensorCategory = normalizeSensorCategory(sensorCategory, sensorTypeCode)
	_, err := db.Exec(ctx, `
		INSERT INTO devices (mac, label, location, ingest_source, sensor_type_code, sensor_category)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''))
		ON CONFLICT (mac) DO UPDATE SET
			label = EXCLUDED.label,
			location = COALESCE(devices.location, EXCLUDED.location),
			ingest_source = COALESCE(NULLIF(EXCLUDED.ingest_source, ''), devices.ingest_source),
			sensor_type_code = COALESCE(NULLIF(EXCLUDED.sensor_type_code, ''), devices.sensor_type_code),
			sensor_category = COALESCE(NULLIF(EXCLUDED.sensor_category, ''), devices.sensor_category),
			updated_at = now()
	`, mac, label, location, ingestSource, sensorTypeCode, sensorCategory)
	return err
}

func (r *statusReporter) MarkSuccess(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := collectorstatus.MarkSuccess(ctx, r.db, r.target); err != nil {
		log.Printf("record collector success: %v", err)
	}
}

func (r *statusReporter) MarkDataSuccess(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := collectorstatus.MarkDataSuccess(ctx, r.db, r.target); err != nil {
		log.Printf("record collector data success: %v", err)
	}
}

func (r *statusReporter) MarkFailure(ctx context.Context, failure error) {
	if r == nil || r.db == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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

func nullablePtr(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func normalizeMAC(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", ":")
	value = strings.ReplaceAll(value, ".", "")
	if len(value) == 12 && !strings.Contains(value, ":") {
		parts := make([]string, 0, 6)
		for i := 0; i < 12; i += 2 {
			parts = append(parts, value[i:i+2])
		}
		value = strings.Join(parts, ":")
	}
	return value
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
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func floatPtr(value float64) *float64 {
	return &value
}

func envString(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
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
