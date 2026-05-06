package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"

type importMode string

const (
	modeNatureRemo importMode = "nature-remo"
	modeECHONET    importMode = "echonet"
	modeAPCUPSD    importMode = "apcupsd"
)

type influxRow struct {
	TS          time.Time
	Source      string
	DeviceKey   string
	Instance    string
	Metric      string
	Value       float64
	Unit        string
	RawProperty string
	RawTopic    string
}

type metricConfig struct {
	Metric string
	Unit   string
}

var echonetMetrics = map[string]metricConfig{
	"solar_generation_w": {Metric: "solar_generation_w", Unit: "W"},
	"battery_remaining":  {Metric: "battery_remaining", Unit: "%"},
	"battery_power_w":    {Metric: "battery_power_w", Unit: "W"},
}

var apcupsdMetrics = map[string]metricConfig{
	"input_voltage":          {Metric: "input_voltage_v", Unit: "V"},
	"load_percent":           {Metric: "load_percent", Unit: "%"},
	"battery_charge_percent": {Metric: "battery_charge_percent", Unit: "%"},
	"battery_voltage":        {Metric: "battery_voltage_v", Unit: "V"},
}

func main() {
	var (
		mode        = flag.String("mode", string(modeNatureRemo), "import mode: nature-remo, echonet, or apcupsd")
		influxURL   = flag.String("influx-url", "http://127.0.0.1:8086", "InfluxDB URL")
		influxToken = flag.String("influx-token", "", "InfluxDB token")
		influxOrg   = flag.String("influx-org", "", "InfluxDB org")
		bucket      = flag.String("bucket", "", "InfluxDB bucket; defaults depend on mode")
		start       = flag.String("start", "1970-01-01T00:00:00Z", "start time for import")
		end         = flag.String("end", "", "end time for import")
		dbDSN       = flag.String("db-dsn", defaultDBDSN, "PostgreSQL DSN")
		batchSize   = flag.Int("batch-size", 2000, "PostgreSQL batch size")
	)
	flag.Parse()
	if strings.TrimSpace(*influxToken) == "" {
		log.Fatal("influx-token is required")
	}

	importMode := importMode(*mode)
	if importMode != modeNatureRemo && importMode != modeECHONET && importMode != modeAPCUPSD {
		log.Fatalf("unsupported mode %q", *mode)
	}
	if *bucket == "" {
		switch importMode {
		case modeECHONET, modeAPCUPSD:
			*bucket = "cisco"
		default:
			*bucket = "remo"
		}
	}

	startTime, err := time.Parse(time.RFC3339, *start)
	if err != nil {
		log.Fatalf("invalid start: %v", err)
	}
	endTime := time.Now().UTC()
	if *end != "" {
		endTime, err = time.Parse(time.RFC3339, *end)
		if err != nil {
			log.Fatalf("invalid end: %v", err)
		}
	}

	ctx := context.Background()
	db, err := pgx.Connect(ctx, *dbDSN)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer db.Close(ctx)

	query := buildQuery(importMode, *bucket, startTime, endTime)
	resp, err := queryInflux(ctx, *influxURL, *influxToken, *influxOrg, query)
	if err != nil {
		log.Fatalf("query influx: %v", err)
	}
	defer resp.Body.Close()

	count, err := parseAndImport(ctx, db, resp.Body, importMode, *batchSize)
	if err != nil {
		log.Fatalf("parse/import influx csv: %v", err)
	}
	log.Printf("import completed rows=%d", count)
}

func buildQuery(mode importMode, bucket string, start time.Time, end time.Time) string {
	switch mode {
	case modeAPCUPSD:
		return fmt.Sprintf(`from(bucket:%q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "apcupsd")
  |> filter(fn: (r) => contains(value: r._field, set: ["input_voltage", "load_percent", "battery_charge_percent", "battery_voltage"]))
  |> keep(columns: ["_time", "_field", "_value", "host", "server", "ups_name", "model"])
`, bucket, start.Format(time.RFC3339), end.Format(time.RFC3339))
	case modeECHONET:
		return fmt.Sprintf(`from(bucket:%q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "echonet_power")
  |> filter(fn: (r) => contains(value: r._field, set: ["solar_generation_w", "battery_remaining", "battery_power_w"]))
  |> keep(columns: ["_time", "_field", "_value", "device", "instance", "property", "mqtt_topic"])
  |> sort(columns: ["_time"])
`, bucket, start.Format(time.RFC3339), end.Format(time.RFC3339))
	default:
		return fmt.Sprintf(`from(bucket:%q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "power" and r._field == "measured_instantaneous")
  |> keep(columns: ["_time", "_field", "_value", "host"])
  |> sort(columns: ["_time"])
`, bucket, start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
}

func queryInflux(ctx context.Context, url string, token string, org string, query string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v2/query?org=%s", strings.TrimRight(url, "/"), org), strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+token)
	req.Header.Set("Accept", "text/csv")
	req.Header.Set("Content-Type", "application/vnd.flux")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("influx status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func parseAndImport(ctx context.Context, db *pgx.Conn, body io.Reader, mode importMode, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = 2000
	}
	reader := csv.NewReader(body)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1

	var columns map[string]int
	batchRows := make([]influxRow, 0, batchSize)
	total := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, err
		}
		if len(record) == 0 || strings.HasPrefix(record[0], "#") {
			continue
		}
		if hasCSVHeader(record) {
			columns = map[string]int{}
			for i, name := range record {
				columns[name] = i
			}
			continue
		}
		if columns == nil {
			return total, fmt.Errorf("missing csv header")
		}
		row, ok, err := parseRecord(columns, record, mode)
		if err != nil {
			return total, err
		}
		if !ok {
			continue
		}
		batchRows = append(batchRows, row)
		if len(batchRows) >= batchSize {
			if err := importRows(ctx, db, batchRows); err != nil {
				return total, err
			}
			total += len(batchRows)
			log.Printf("imported rows=%d", total)
			batchRows = batchRows[:0]
		}
	}
	if len(batchRows) > 0 {
		if err := importRows(ctx, db, batchRows); err != nil {
			return total, err
		}
		total += len(batchRows)
		log.Printf("imported rows=%d", total)
	}
	return total, nil
}

func hasCSVHeader(record []string) bool {
	for _, value := range record {
		if value == "_time" || value == "_value" || value == "_field" {
			return true
		}
	}
	return false
}

func parseRecord(columns map[string]int, record []string, mode importMode) (influxRow, bool, error) {
	get := func(name string) (string, error) {
		pos, ok := columns[name]
		if !ok {
			return "", fmt.Errorf("missing column %q", name)
		}
		if pos >= len(record) {
			return "", fmt.Errorf("record too short for column %q", name)
		}
		return record[pos], nil
	}
	getOptional := func(name string) string {
		pos, ok := columns[name]
		if !ok || pos >= len(record) {
			return ""
		}
		return record[pos]
	}
	rawTS, err := get("_time")
	if err != nil {
		return influxRow{}, false, err
	}
	ts, err := time.Parse(time.RFC3339Nano, rawTS)
	if err != nil {
		return influxRow{}, false, fmt.Errorf("parse time %q: %w", rawTS, err)
	}
	rawValue, err := get("_value")
	if err != nil {
		return influxRow{}, false, err
	}
	value, err := strconv.ParseFloat(rawValue, 64)
	if err != nil {
		return influxRow{}, false, fmt.Errorf("parse value %q: %w", rawValue, err)
	}
	field, err := get("_field")
	if err != nil {
		return influxRow{}, false, err
	}

	if mode == modeECHONET {
		config, ok := echonetMetrics[field]
		if !ok {
			return influxRow{}, false, nil
		}
		device := strings.TrimSpace(getOptional("device"))
		if device == "" {
			device = "echonet-device"
		}
		return influxRow{
			TS:          ts,
			Source:      "echonet",
			DeviceKey:   device,
			Instance:    strings.TrimSpace(getOptional("instance")),
			Metric:      config.Metric,
			Value:       value,
			Unit:        config.Unit,
			RawProperty: strings.TrimSpace(getOptional("property")),
			RawTopic:    strings.TrimSpace(getOptional("mqtt_topic")),
		}, true, nil
	}

	if mode == modeAPCUPSD {
		config, ok := apcupsdMetrics[field]
		if !ok {
			return influxRow{}, false, nil
		}
		device := strings.TrimSpace(getOptional("ups_name"))
		if device == "" {
			device = strings.TrimSpace(getOptional("host"))
		}
		if device == "" {
			device = "ups"
		}
		return influxRow{
			TS:          ts,
			Source:      "apcupsd",
			DeviceKey:   device,
			Metric:      config.Metric,
			Value:       value,
			Unit:        config.Unit,
			RawProperty: field,
		}, true, nil
	}

	host := strings.TrimSpace(getOptional("host"))
	if host == "" {
		host = "remo-e"
	}
	return influxRow{
		TS:          ts,
		Source:      "nature_remo",
		DeviceKey:   host,
		Metric:      "measured_instantaneous_w",
		Value:       value,
		Unit:        "W",
		RawProperty: field,
	}, true, nil
}

func importRows(ctx context.Context, db *pgx.Conn, rows []influxRow) error {
	batch := &pgx.Batch{}
	for _, row := range rows {
		unit := any(nil)
		if row.Unit != "" {
			unit = row.Unit
		}
		rawTopic := any(nil)
		if row.RawTopic != "" {
			rawTopic = row.RawTopic
		}
		instance := any(nil)
		if row.Instance != "" {
			instance = row.Instance
		}
		batch.Queue(`
			INSERT INTO energy_readings (
				ts,
				source,
				device_key,
				instance,
				metric,
				value,
				unit,
				raw_property,
				raw_topic
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (ts, source, device_key, metric) DO UPDATE SET
				instance = EXCLUDED.instance,
				value = EXCLUDED.value,
				unit = EXCLUDED.unit,
				raw_property = EXCLUDED.raw_property,
				raw_topic = EXCLUDED.raw_topic,
				inserted_at = now()
		`, row.TS, row.Source, row.DeviceKey, instance, row.Metric, row.Value, unit, row.RawProperty, rawTopic)
	}
	results := db.SendBatch(ctx, batch)
	for range rows {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return err
		}
	}
	return results.Close()
}
