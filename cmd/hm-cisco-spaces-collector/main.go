package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"home-metrics/internal/collectorstatus"

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
	defaultStreamHeartbeat   = time.Minute
	ciscoSpacesLockKey       = int64(734829148912345)
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
	StreamHeartbeat   time.Duration
	BatteryMode       string
	BatteryAllowlist  map[string]bool
	DryRun            bool
	AllowSecondary    bool
	Debug             bool
}

type firehoseEvent struct {
	RecordUID     string         `json:"recordUid"`
	EventType     string         `json:"eventType"`
	RecordTS      int64          `json:"recordTimestamp"`
	IOTTelemetry  telemetry      `json:"iotTelemetry"`
	RawProperties map[string]any `json:"-"`
}

type telemetry struct {
	DeviceInfo       deviceInfo       `json:"deviceInfo"`
	Temperature      *temperatureData `json:"temperature"`
	Humidity         *humidityData    `json:"humidity"`
	AirPressure      *airPressureData `json:"airPressure"`
	CarbonEmissions  *carbonData      `json:"carbonEmissions"`
	Illuminance      *illuminanceData `json:"illuminance"`
	Battery          *batteryData     `json:"battery"`
	TVOC             *tvocData        `json:"tvoc"`
	DetectedPosition *positionData    `json:"detectedPosition"`
	Location         *locationData    `json:"location"`
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

type positionData struct {
	MapID      string `json:"mapId"`
	LocationID string `json:"locationId"`
}

type locationData struct {
	LocationID string `json:"locationId"`
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

type statusReporter struct {
	mu     *sync.Mutex
	db     *pgx.Conn
	target collectorstatus.Target
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
	var lockDB *pgx.Conn
	if !cfg.DryRun || !cfg.AllowSecondary {
		var err error
		db, err = pgx.Connect(ctx, cfg.DBDSN)
		if err != nil {
			log.Fatalf("connect database: %v", err)
		}
		defer db.Close(context.Background())
		lockDB = db
	}
	if !cfg.AllowSecondary {
		if lockDB == nil {
			var err error
			lockDB, err = pgx.Connect(ctx, cfg.DBDSN)
			if err != nil {
				log.Fatalf("connect database for Cisco Spaces lock: %v", err)
			}
			defer lockDB.Close(context.Background())
		}
		release, err := acquireCiscoSpacesLock(ctx, lockDB)
		if err != nil {
			log.Fatalf("acquire Cisco Spaces collector lock: %v", err)
		}
		defer release()
	}
	p := newProcessor(cfg)
	client := &http.Client{}
	statusTarget := collectorstatus.Target{
		CollectorName: "hm-cisco-spaces-collector",
		TargetType:    "cisco_spaces_firehose",
		TargetKey:     "default",
	}
	dbMu := &sync.Mutex{}
	reporter := &statusReporter{mu: dbMu, db: db, target: statusTarget}
	log.Printf("Cisco Spaces collector started url=%s db=%s dry_run=%t", cfg.FirehoseURL, cfg.DBDSN, cfg.DryRun)
	streamWithReconnect(ctx, client, cfg, func(err error) {
		reporter.MarkFailure(ctx, err)
	}, func() {
		reporter.MarkSuccess(ctx)
	}, func(event firehoseEvent, raw []byte) {
		reading, ok, _, err := p.processEvent(event)
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
		dbMu.Lock()
		err = writeReading(ctx, db, reading)
		dbMu.Unlock()
		if err != nil {
			log.Printf("write Cisco Spaces reading mac=%s ts=%s: %v", reading.MAC, reading.TS.Format(time.RFC3339), err)
			reporter.MarkFailure(ctx, err)
			return
		}
		reporter.MarkDataSuccess(ctx)
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
		StreamHeartbeat:   envDuration("CISCO_SPACES_STREAM_HEARTBEAT", defaultStreamHeartbeat),
		BatteryMode:       strings.ToLower(envString("CISCO_SPACES_BATTERY_MODE", "all")),
		BatteryAllowlist:  parseMACSet(os.Getenv("CISCO_SPACES_BATTERY_ALLOWLIST")),
		DryRun:            envBool("CISCO_SPACES_DRY_RUN", false),
		AllowSecondary:    envBool("CISCO_SPACES_ALLOW_SECONDARY", false),
		Debug:             envBool("CISCO_SPACES_DEBUG", false),
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
	if cfg.StreamHeartbeat <= 0 {
		cfg.StreamHeartbeat = defaultStreamHeartbeat
	}
	switch cfg.BatteryMode {
	case "all", "allowlist", "off":
	default:
		log.Printf("invalid CISCO_SPACES_BATTERY_MODE=%q, using all", cfg.BatteryMode)
		cfg.BatteryMode = "all"
	}
	return cfg
}

func acquireCiscoSpacesLock(ctx context.Context, db *pgx.Conn) (func(), error) {
	var locked bool
	if err := db.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, ciscoSpacesLockKey).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("another hm-cisco-spaces-collector already holds the Firehose lock")
	}
	log.Printf("acquired Cisco Spaces Firehose advisory lock key=%d", ciscoSpacesLockKey)
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released bool
		if err := db.QueryRow(releaseCtx, `SELECT pg_advisory_unlock($1)`, ciscoSpacesLockKey).Scan(&released); err != nil {
			log.Printf("release Cisco Spaces Firehose advisory lock: %v", err)
			return
		}
		if !released {
			log.Printf("Cisco Spaces Firehose advisory lock was not held during release")
		}
	}, nil
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
