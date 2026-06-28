package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"home-metrics/internal/collectorstatus"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
	defaultGATTHistoryEntries = 24
	flowerCareGATTReadDelay   = time.Second

	flowerCareDataService            = "1204"
	flowerCareHistoryService         = "1206"
	flowerCareModeCharacteristic     = "00001a00-0000-1000-8000-00805f9b34fb"
	flowerCareRealtimeCharacteristic = "00001a01-0000-1000-8000-00805f9b34fb"
	flowerCareBatteryCharacteristic  = "00001a02-0000-1000-8000-00805f9b34fb"
	flowerCareHistoryCommand         = "00001a10-0000-1000-8000-00805f9b34fb"
	flowerCareHistoryData            = "00001a11-0000-1000-8000-00805f9b34fb"
	flowerCareEpoch                  = "00001a12-0000-1000-8000-00805f9b34fb"
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
	ControlMu         *sync.Mutex
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
	HistoryBackfill     *bool  `json:"history_backfill"`
	MaxHistoryEntries   int    `json:"max_history_entries"`
}

type targetConfig struct {
	Devices []targetDevice `json:"devices"`
}

type targetRegistry struct {
	All     map[string]targetDevice
	Enabled map[string]targetDevice
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
	db      sensorDB
	windows map[string]*aggregate
	flushFn func(context.Context, *aggregate) (bool, error)
}

type statusReporter struct {
	db     collectorstatus.Execer
	target collectorstatus.Target
}

type statusMarker interface {
	MarkSuccess(context.Context)
	MarkDataSuccess(context.Context)
	MarkFailure(context.Context, error)
}

type sensorMinuteExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type sensorDB interface {
	sensorMinuteExecer
	QueryRow(context.Context, string, ...any) pgx.Row
}

type gattHistoryReadResult struct {
	Readings   []bleReading
	Count      uint16
	StopReason string
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

type reconnectBackoff struct {
	min     time.Duration
	max     time.Duration
	current time.Duration
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
	registry, err := loadTargetRegistry(cfg.SensorsFile)
	if err != nil {
		log.Fatalf("load BLE sensors: %v", err)
	}
	targets := registry.Enabled

	var db *pgxpool.Pool
	if !cfg.DryRun {
		db, err = pgxpool.New(ctx, cfg.DBDSN)
		if err != nil {
			log.Fatalf("connect database: %v", err)
		}
		if err := db.Ping(ctx); err != nil {
			db.Close()
			log.Fatalf("ping database: %v", err)
		}
		if err := ensureDevices(ctx, db, registry.All); err != nil {
			db.Close()
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
	var workers sync.WaitGroup
	if !cfg.DryRun && db != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runGATTBatteryPoller(ctx, cfg, targets, c)
		}()
	}
	runWithReconnect(ctx, cfg, targets, c, reporter)
	stop()
	workers.Wait()
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFlush()
	if err := c.flushAll(flushCtx); err != nil {
		log.Printf("flush on shutdown: %v", err)
	}
	if db != nil {
		db.Close()
	}
}

func runWithReconnect(ctx context.Context, cfg config, targets map[string]targetDevice, c *collector, reporter *statusReporter) {
	backoff := newReconnectBackoff(cfg.ReconnectMinDelay, cfg.ReconnectMaxDelay)
	lastPendingLog := time.Time{}
	for ctx.Err() == nil {
		err := runMQTT(ctx, cfg, func() {
			backoff.Reset()
			reporter.MarkSuccess(ctx)
		}, func(topic string, payload []byte) {
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
		delay := backoff.Next()
		log.Printf("MQTT stream ended: %v; reconnecting in %s", err, delay)
		reporter.MarkFailure(ctx, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func newReconnectBackoff(minimum time.Duration, maximum time.Duration) *reconnectBackoff {
	if minimum <= 0 {
		minimum = time.Second
	}
	if maximum < minimum {
		maximum = minimum
	}
	return &reconnectBackoff{min: minimum, max: maximum, current: minimum}
}

func (b *reconnectBackoff) Reset() {
	b.current = b.min
}

func (b *reconnectBackoff) Next() time.Duration {
	delay := b.current
	b.current *= 2
	if b.current > b.max {
		b.current = b.max
	}
	return delay
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
