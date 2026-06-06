package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("BLE_DB_DSN")
	if dsn == "" {
		dsn = defaultDBDSN
	}
	retainMinute := envDuration("DB_MAINT_RETAIN_MINUTE", 14*24*time.Hour)
	refreshLookback := envDuration("DB_MAINT_REFRESH_LOOKBACK", 48*time.Hour)

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())

	if err := refreshRollups(ctx, db, refreshLookback); err != nil {
		log.Fatalf("refresh rollups: %v", err)
	}
	if err := retainSensorMinute(ctx, db, retainMinute); err != nil {
		log.Fatalf("retain sensor_minute: %v", err)
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

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func refreshRollups(ctx context.Context, db *pgx.Conn, lookback time.Duration) error {
	if err := refreshRollup(ctx, db, "sensor_1hour", "1 hour", "sensor_minute", lookback); err != nil {
		return err
	}
	if err := refreshRollup(ctx, db, "sensor_12hour", "12 hours", "sensor_1hour", lookback); err != nil {
		return err
	}
	if err := refreshRollup(ctx, db, "sensor_1day", "1 day", "sensor_1hour", lookback); err != nil {
		return err
	}
	return nil
}

func refreshRollup(ctx context.Context, db *pgx.Conn, target string, bucket string, source string, lookback time.Duration) error {
	command := `
		INSERT INTO ` + target + ` (
			ts,
			mac,
			temperature_c,
			humidity_percent,
			battery_percent,
			rssi_dbm,
			pressure_hpa,
			co2_ppm,
			lux,
			etvoc,
			soil_moisture_percent,
			conductivity_us_cm,
			updated_at
		)
		SELECT
			time_bucket($1::interval, ts) AS bucket,
			mac,
			avg(temperature_c),
			avg(humidity_percent),
			avg(battery_percent),
			avg(rssi_dbm),
			avg(pressure_hpa),
			avg(co2_ppm),
			avg(lux),
			avg(etvoc),
			avg(soil_moisture_percent),
			avg(conductivity_us_cm),
			now()
		FROM ` + source + `
		WHERE ts >= now() - $2::interval
		GROUP BY bucket, mac
		ON CONFLICT (ts, mac) DO UPDATE SET
			temperature_c = EXCLUDED.temperature_c,
			humidity_percent = EXCLUDED.humidity_percent,
			battery_percent = EXCLUDED.battery_percent,
			rssi_dbm = EXCLUDED.rssi_dbm,
			pressure_hpa = EXCLUDED.pressure_hpa,
			co2_ppm = EXCLUDED.co2_ppm,
			lux = EXCLUDED.lux,
			etvoc = EXCLUDED.etvoc,
			soil_moisture_percent = EXCLUDED.soil_moisture_percent,
			conductivity_us_cm = EXCLUDED.conductivity_us_cm,
			updated_at = now()
	`
	tag, err := db.Exec(ctx, command, bucket, intervalSeconds(lookback))
	if err != nil {
		return err
	}
	log.Printf("refreshed %s from %s rows=%d", target, source, tag.RowsAffected())
	return nil
}

func retainSensorMinute(ctx context.Context, db execer, retain time.Duration) error {
	tag, err := db.Exec(ctx, `
		DELETE FROM sensor_minute
		WHERE ts < now() - $1::interval
	`, intervalSeconds(retain))
	if err != nil {
		return err
	}
	log.Printf("retained sensor_minute retain=%s deleted=%d", retain, tag.RowsAffected())
	return nil
}

func intervalSeconds(duration time.Duration) string {
	return fmt.Sprintf("%f seconds", duration.Seconds())
}
