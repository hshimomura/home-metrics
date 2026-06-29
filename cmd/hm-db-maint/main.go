package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"home-metrics/internal/sensor"

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
	retainAlertEvents := envDuration("DB_MAINT_RETAIN_ALERT_EVENTS", 90*24*time.Hour)
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
	if err := retainSensorAlertEvents(ctx, db, retainAlertEvents); err != nil {
		log.Fatalf("retain sensor alert events: %v", err)
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
	cutoff, err := ensureAccuracyCutoff(ctx, db)
	if err != nil {
		return err
	}
	if err := refreshRollup(ctx, db, "sensor_1hour", "1 hour", "sensor_minute", lookback, cutoff, false); err != nil {
		return err
	}
	if err := refreshRollup(ctx, db, "sensor_12hour", "12 hours", "sensor_1hour", lookback, cutoff, true); err != nil {
		return err
	}
	if err := refreshRollup(ctx, db, "sensor_1day", "1 day", "sensor_1hour", lookback, cutoff, true); err != nil {
		return err
	}
	return nil
}

func ensureAccuracyCutoff(ctx context.Context, db *pgx.Conn) (time.Time, error) {
	var cutoff time.Time
	err := db.QueryRow(ctx, `
		INSERT INTO rollup_accuracy_state (id, accuracy_cutoff)
		SELECT true, time_bucket('1 day', COALESCE(min(ts), now())) + interval '1 day'
		FROM sensor_minute
		ON CONFLICT (id) DO UPDATE SET accuracy_cutoff = rollup_accuracy_state.accuracy_cutoff
		RETURNING accuracy_cutoff
	`).Scan(&cutoff)
	if err != nil {
		return time.Time{}, fmt.Errorf("initialize rollup accuracy cutoff: %w", err)
	}
	log.Printf("rollup accuracy cutoff=%s", cutoff.Format(time.RFC3339))
	return cutoff, nil
}

func refreshRollup(ctx context.Context, db execer, target string, bucket string, source string, lookback time.Duration, cutoff time.Time, weighted bool) error {
	command := buildRollupSQL(target, source, weighted)
	tag, err := db.Exec(ctx, command, bucket, intervalSeconds(lookback), cutoff)
	if err != nil {
		return err
	}
	log.Printf("refreshed %s from %s rows=%d", target, source, tag.RowsAffected())
	return nil
}

func buildRollupSQL(target string, source string, weighted bool) string {
	columns := []string{"ts", "mac"}
	selects := []string{"time_bucket($1::interval, ts) AS bucket", "mac"}
	updates := make([]string, 0, len(sensor.Metrics)*2+1)
	for _, metric := range sensor.Metrics {
		columns = append(columns, metric.Column)
		if weighted {
			selects = append(selects, fmt.Sprintf(
				"CASE WHEN COALESCE(sum(%[1]s_count), 0) > 0 THEN sum(%[1]s * %[1]s_count)::double precision / sum(%[1]s_count) END",
				metric.Column,
			))
		} else {
			selects = append(selects, "avg("+metric.Column+")")
		}
		updates = append(updates, metric.Column+" = EXCLUDED."+metric.Column)
	}
	for _, metric := range sensor.Metrics {
		countColumn := metric.Column + "_count"
		columns = append(columns, countColumn)
		if weighted {
			selects = append(selects, "sum("+countColumn+")")
		} else {
			selects = append(selects, "count("+metric.Column+")")
		}
		updates = append(updates, countColumn+" = EXCLUDED."+countColumn)
	}
	columns = append(columns, "updated_at")
	selects = append(selects, "now()")
	updates = append(updates, "updated_at = now()")
	return fmt.Sprintf(`
		INSERT INTO %s (%s)
		SELECT %s
		FROM %s
		WHERE ts >= GREATEST(now() - $2::interval, $3::timestamptz)
		GROUP BY bucket, mac
		ON CONFLICT (ts, mac) DO UPDATE SET %s
	`, target, strings.Join(columns, ", "), strings.Join(selects, ", "), source, strings.Join(updates, ", "))
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

func retainSensorAlertEvents(ctx context.Context, db execer, retain time.Duration) error {
	tag, err := db.Exec(ctx, `
		DELETE FROM sensor_alert_events
		WHERE occurred_at < now() - $1::interval
	`, intervalSeconds(retain))
	if err != nil {
		return err
	}
	log.Printf("retained sensor_alert_events retain=%s deleted=%d", retain, tag.RowsAffected())
	return nil
}

func intervalSeconds(duration time.Duration) string {
	return fmt.Sprintf("%f seconds", duration.Seconds())
}
