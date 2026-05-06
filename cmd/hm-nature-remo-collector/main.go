package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	defaultDBDSN     = "dbname=ble_sensors host=/var/run/postgresql"
	defaultAPIURL    = "https://api.nature.global/1/appliances"
	defaultDeviceKey = "remo-e"
	sourceName       = "nature_remo"
)

type appliance struct {
	Type       string     `json:"type"`
	SmartMeter smartMeter `json:"smart_meter"`
}

type smartMeter struct {
	ECHONETLiteProperties []echonetLiteProperty `json:"echonetlite_properties"`
}

type echonetLiteProperty struct {
	EPC       int    `json:"epc"`
	Val       string `json:"val"`
	UpdatedAt string `json:"updated_at"`
}

type powerReading struct {
	TS                    time.Time
	MeasuredInstantaneous float64
}

type energyReading struct {
	TS       time.Time
	Source   string
	Device   string
	Metric   string
	Value    float64
	Unit     string
	Property string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := envString("BLE_DB_DSN", defaultDBDSN)
	token := strings.TrimSpace(os.Getenv("NATURE_REMO_TOKEN"))
	if token == "" {
		log.Fatal("NATURE_REMO_TOKEN is required")
	}
	apiURL := envString("NATURE_REMO_API_URL", defaultAPIURL)
	deviceKey := envString("NATURE_REMO_DEVICE_KEY", defaultDeviceKey)
	interval := envDuration("NATURE_REMO_INTERVAL", time.Minute)
	runOnceOnly := envBool("NATURE_REMO_RUN_ONCE", false)

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())

	client := &http.Client{Timeout: 20 * time.Second}
	log.Printf("nature remo collector started interval=%s device=%s", interval, deviceKey)
	if err := runOnce(ctx, db, client, apiURL, token, deviceKey); err != nil {
		log.Printf("collect nature remo: %v", err)
	}
	if runOnceOnly {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := runOnce(ctx, db, client, apiURL, token, deviceKey); err != nil {
				log.Printf("collect nature remo: %v", err)
			}
		}
	}
}

func runOnce(ctx context.Context, db *pgx.Conn, client *http.Client, apiURL string, token string, deviceKey string) error {
	reading, err := fetchPowerReading(ctx, client, apiURL, token)
	if err != nil {
		return err
	}
	rows := []energyReading{
		{
			TS:       reading.TS,
			Source:   sourceName,
			Device:   deviceKey,
			Metric:   "measured_instantaneous_w",
			Value:    reading.MeasuredInstantaneous,
			Unit:     "W",
			Property: "measured_instantaneous",
		},
	}
	if err := upsertEnergyReadings(ctx, db, rows); err != nil {
		return err
	}
	log.Printf(
		"stored nature remo power ts=%s measured_instantaneous_w=%.0f",
		reading.TS.Format(time.RFC3339),
		reading.MeasuredInstantaneous,
	)
	return nil
}

func fetchPowerReading(ctx context.Context, client *http.Client, apiURL string, token string) (powerReading, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return powerReading{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return powerReading{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return powerReading{}, fmt.Errorf("nature remo api status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var appliances []appliance
	if err := json.NewDecoder(res.Body).Decode(&appliances); err != nil {
		return powerReading{}, err
	}
	return parsePowerReading(appliances, time.Now())
}

func parsePowerReading(appliances []appliance, fallbackNow time.Time) (powerReading, error) {
	props, ok := findSmartMeterProperties(appliances)
	if !ok {
		return powerReading{}, errors.New("EL_SMART_METER appliance not found")
	}

	values := map[int]int64{}
	var newest time.Time
	for _, prop := range props {
		value, err := parseIntValue(prop.Val)
		if err != nil {
			continue
		}
		values[prop.EPC] = value
		if ts, err := time.Parse(time.RFC3339, prop.UpdatedAt); err == nil && ts.After(newest) {
			newest = ts
		}
	}
	if newest.IsZero() {
		newest = fallbackNow
	}

	measuredInstantaneous, ok := values[231]
	if !ok {
		return powerReading{}, errors.New("measured instantaneous power EPC 231 not found")
	}

	return powerReading{
		TS:                    newest,
		MeasuredInstantaneous: float64(measuredInstantaneous),
	}, nil
}

func findSmartMeterProperties(appliances []appliance) ([]echonetLiteProperty, bool) {
	for _, item := range appliances {
		if item.Type == "EL_SMART_METER" {
			return item.SmartMeter.ECHONETLiteProperties, true
		}
	}
	return nil, false
}

func parseIntValue(raw string) (int64, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, errors.New("empty value")
	}
	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		return value, nil
	}
	if strings.HasPrefix(text, "0x") || strings.HasPrefix(text, "0X") {
		return strconv.ParseInt(text[2:], 16, 64)
	}
	return strconv.ParseInt(text, 16, 64)
}

func upsertEnergyReadings(ctx context.Context, db *pgx.Conn, rows []energyReading) error {
	for _, row := range rows {
		_, err := db.Exec(ctx, `
			INSERT INTO energy_readings (
				ts,
				source,
				device_key,
				metric,
				value,
				unit,
				raw_property
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (ts, source, device_key, metric) DO UPDATE SET
				value = EXCLUDED.value,
				unit = EXCLUDED.unit,
				raw_property = EXCLUDED.raw_property,
				inserted_at = now()
		`, row.TS, row.Source, row.Device, row.Metric, row.Value, row.Unit, row.Property)
		if err != nil {
			return err
		}
	}
	return nil
}

func envString(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
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
