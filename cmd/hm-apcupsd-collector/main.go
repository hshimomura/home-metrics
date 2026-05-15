package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"home-metrics/internal/collectorstatus"

	"github.com/jackc/pgx/v5"
)

const (
	defaultDBDSN     = "dbname=ble_sensors host=/var/run/postgresql"
	defaultServer    = "tcp://127.0.0.1:3551"
	defaultDeviceKey = "ups"
	sourceName       = "apcupsd"
)

var numberPattern = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

type energyReading struct {
	TS       time.Time
	Source   string
	Device   string
	Metric   string
	Value    float64
	Unit     string
	Property string
}

type upsStatus struct {
	TS             time.Time
	InputVoltage   float64
	LoadPercent    float64
	BatteryCharge  float64
	BatteryVoltage float64
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := envString("BLE_DB_DSN", defaultDBDSN)
	server := envString("APCUPSD_SERVER", defaultServer)
	deviceKey := envString("APCUPSD_DEVICE_KEY", defaultDeviceKey)
	label := envString("APCUPSD_LABEL", "APC UPS")
	location := envString("APCUPSD_LOCATION", "Home")
	interval := envDuration("APCUPSD_INTERVAL", time.Minute)
	timeout := envDuration("APCUPSD_TIMEOUT", 5*time.Second)
	runOnceOnly := envBool("APCUPSD_RUN_ONCE", false)

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())

	statusTarget := collectorstatus.Target{
		CollectorName: "hm-apcupsd-collector",
		TargetType:    "apcupsd_server",
		TargetKey:     deviceKey,
	}
	log.Printf("apcupsd collector started server=%s interval=%s device=%s", server, interval, deviceKey)
	if err := runOnce(ctx, db, server, timeout, deviceKey, label, location); err != nil {
		log.Printf("collect apcupsd: %v", err)
		reportCollectorFailure(ctx, db, statusTarget, err)
	} else {
		reportCollectorSuccess(ctx, db, statusTarget)
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
			if err := runOnce(ctx, db, server, timeout, deviceKey, label, location); err != nil {
				log.Printf("collect apcupsd: %v", err)
				reportCollectorFailure(ctx, db, statusTarget, err)
			} else {
				reportCollectorSuccess(ctx, db, statusTarget)
			}
		}
	}
}

func reportCollectorSuccess(ctx context.Context, db *pgx.Conn, target collectorstatus.Target) {
	if err := collectorstatus.MarkDataSuccess(ctx, db, target); err != nil {
		log.Printf("record collector success: %v", err)
	}
}

func reportCollectorFailure(ctx context.Context, db *pgx.Conn, target collectorstatus.Target, failure error) {
	if err := collectorstatus.MarkFailure(ctx, db, target, failure); err != nil {
		log.Printf("record collector failure: %v", err)
	}
}

func runOnce(ctx context.Context, db *pgx.Conn, server string, timeout time.Duration, deviceKey string, label string, location string) error {
	raw, err := fetchStatus(server, timeout)
	if err != nil {
		return err
	}
	status, err := parseStatus(raw, time.Now())
	if err != nil {
		return err
	}
	status.TS = status.TS.Truncate(time.Minute)
	if err := upsertDevice(ctx, db, deviceKey, label, location); err != nil {
		return err
	}
	if err := upsertMetricDefinitions(ctx, db); err != nil {
		return err
	}
	readings := []energyReading{
		{TS: status.TS, Source: sourceName, Device: deviceKey, Metric: "input_voltage_v", Value: status.InputVoltage, Unit: "V", Property: "LINEV"},
		{TS: status.TS, Source: sourceName, Device: deviceKey, Metric: "load_percent", Value: status.LoadPercent, Unit: "%", Property: "LOADPCT"},
		{TS: status.TS, Source: sourceName, Device: deviceKey, Metric: "battery_charge_percent", Value: status.BatteryCharge, Unit: "%", Property: "BCHARGE"},
		{TS: status.TS, Source: sourceName, Device: deviceKey, Metric: "battery_voltage_v", Value: status.BatteryVoltage, Unit: "V", Property: "BATTV"},
	}
	if err := upsertEnergyReadings(ctx, db, readings); err != nil {
		return err
	}
	log.Printf("stored apcupsd ts=%s input_voltage_v=%.1f load_percent=%.1f battery_charge_percent=%.1f battery_voltage_v=%.1f", status.TS.Format(time.RFC3339), status.InputVoltage, status.LoadPercent, status.BatteryCharge, status.BatteryVoltage)
	return nil
}

func fetchStatus(server string, timeout time.Duration) (string, error) {
	network, address, err := parseServer(server)
	if err != nil {
		return "", err
	}
	conn, err := net.DialTimeout(network, address, timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := writeNISCommand(conn, "status"); err != nil {
		return "", err
	}
	return readNISResponse(conn)
}

func parseServer(raw string) (string, string, error) {
	if !strings.Contains(raw, "://") {
		return "tcp", raw, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if parsed.Scheme != "tcp" {
		return "", "", fmt.Errorf("unsupported apcupsd server scheme %q", parsed.Scheme)
	}
	return parsed.Scheme, parsed.Host, nil
}

func writeNISCommand(conn net.Conn, command string) error {
	payload := []byte(command)
	if len(payload) > 65535 {
		return errors.New("apcupsd command too long")
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
	if _, err := conn.Write(header[:]); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

func readNISResponse(conn net.Conn) (string, error) {
	var builder strings.Builder
	for {
		var header [2]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			if errors.Is(err, io.EOF) && builder.Len() > 0 {
				break
			}
			return "", err
		}
		length := binary.BigEndian.Uint16(header[:])
		if length == 0 {
			break
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		builder.Write(buf)
	}
	return builder.String(), nil
}

func parseStatus(raw string, fallbackNow time.Time) (upsStatus, error) {
	fields := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return upsStatus{}, err
	}
	ts := fallbackNow
	if rawDate := fields["DATE"]; rawDate != "" {
		if parsed, err := time.Parse("2006-01-02 15:04:05 -0700", strings.Join(strings.Fields(rawDate), " ")); err == nil {
			ts = parsed
		}
	}
	input, err := parseNumberField(fields, "LINEV")
	if err != nil {
		return upsStatus{}, err
	}
	load, err := parseNumberField(fields, "LOADPCT")
	if err != nil {
		return upsStatus{}, err
	}
	charge, err := parseNumberField(fields, "BCHARGE")
	if err != nil {
		return upsStatus{}, err
	}
	battery, err := parseNumberField(fields, "BATTV")
	if err != nil {
		return upsStatus{}, err
	}
	return upsStatus{TS: ts, InputVoltage: input, LoadPercent: load, BatteryCharge: charge, BatteryVoltage: battery}, nil
}

func parseNumberField(fields map[string]string, key string) (float64, error) {
	value, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("%s not found", key)
	}
	match := numberPattern.FindString(value)
	if match == "" {
		return 0, fmt.Errorf("%s has no numeric value", key)
	}
	return strconv.ParseFloat(match, 64)
}

func upsertDevice(ctx context.Context, db *pgx.Conn, deviceKey string, label string, location string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO energy_devices (source, device_key, label, location)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (source, device_key) DO UPDATE SET
			label = EXCLUDED.label,
			location = EXCLUDED.location,
			updated_at = now()
	`, sourceName, deviceKey, label, location)
	return err
}

func upsertMetricDefinitions(ctx context.Context, db *pgx.Conn) error {
	definitions := []struct{ metric, display, unit, property string }{
		{"input_voltage_v", "UPS input voltage", "V", "LINEV"},
		{"load_percent", "UPS load", "%", "LOADPCT"},
		{"battery_charge_percent", "UPS battery charge", "%", "BCHARGE"},
		{"battery_voltage_v", "UPS battery voltage", "V", "BATTV"},
	}
	for _, item := range definitions {
		_, err := db.Exec(ctx, `
			INSERT INTO energy_metric_definitions (source, metric, display_name, unit, raw_property)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (source, metric) DO UPDATE SET
				display_name = EXCLUDED.display_name,
				unit = EXCLUDED.unit,
				raw_property = EXCLUDED.raw_property,
				updated_at = now()
		`, sourceName, item.metric, item.display, item.unit, item.property)
		if err != nil {
			return err
		}
	}
	return nil
}

func upsertEnergyReadings(ctx context.Context, db *pgx.Conn, rows []energyReading) error {
	for _, row := range rows {
		_, err := db.Exec(ctx, `
			INSERT INTO energy_readings (ts, source, device_key, metric, value, unit, raw_property)
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
