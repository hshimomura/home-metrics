package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"

type alertRule struct {
	ID               int64
	UserID           int64
	MAC              string
	Metric           string
	Operator         string
	Threshold        float64
	CooldownDuration time.Duration
	LastNotifiedAt   *time.Time
}

type latestValue struct {
	TS    time.Time
	Value float64
}

type notificationResult struct {
	Status       string
	ErrorMessage *string
	SentAt       *time.Time
}

type notifier interface {
	Notify(ctx context.Context, db *pgx.Conn, rule alertRule, value latestValue, now time.Time) (notificationResult, error)
	Mode() string
}

var metricColumns = map[string]string{
	"temperature_c":    "temperature_c",
	"humidity_percent": "humidity_percent",
	"battery_percent":  "battery_percent",
	"rssi_dbm":         "rssi_dbm",
	"pressure_hpa":     "pressure_hpa",
	"co2_ppm":          "co2_ppm",
	"lux":              "lux",
	"etvoc":            "etvoc",
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("BLE_DB_DSN")
	if dsn == "" {
		dsn = defaultDBDSN
	}
	interval := envDuration("ALERT_WORKER_INTERVAL", time.Minute)
	runOnceOnly := envBool("ALERT_WORKER_RUN_ONCE", false)
	dryRun := envBool("ALERT_WORKER_DRY_RUN", true)

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())

	alertNotifier, err := newNotifierFromEnv(dryRun, http.DefaultClient)
	if err != nil {
		log.Fatalf("configure notifier: %v", err)
	}

	log.Printf("alert worker started interval=%s db=%s mode=%s", interval, dsn, alertNotifier.Mode())
	if err := runOnce(ctx, db, alertNotifier); err != nil {
		log.Printf("alert check: %v", err)
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
			if err := runOnce(ctx, db, alertNotifier); err != nil {
				log.Printf("alert check: %v", err)
			}
		}
	}
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

func runOnce(ctx context.Context, db *pgx.Conn, alertNotifier notifier) error {
	rules, err := loadRules(ctx, db)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if err := evaluateRule(ctx, db, alertNotifier, rule, time.Now()); err != nil {
			log.Printf("rule=%d mac=%s metric=%s: %v", rule.ID, rule.MAC, rule.Metric, err)
		}
	}
	return nil
}

func loadRules(ctx context.Context, db *pgx.Conn) ([]alertRule, error) {
	rows, err := db.Query(ctx, `
		SELECT
			r.id,
			r.user_id,
			r.mac,
			r.metric,
			r.operator,
			r.threshold,
			EXTRACT(EPOCH FROM r.cooldown_duration)::bigint AS cooldown_seconds,
			s.last_notified_at
		FROM alert_rules r
		LEFT JOIN alert_rule_state s ON s.alert_rule_id = r.id
		JOIN devices d ON d.mac = r.mac
		WHERE r.enabled AND d.enabled
		ORDER BY r.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []alertRule
	for rows.Next() {
		var rule alertRule
		var cooldownSeconds int64
		var lastNotifiedAt pgtype.Timestamptz
		if err := rows.Scan(
			&rule.ID,
			&rule.UserID,
			&rule.MAC,
			&rule.Metric,
			&rule.Operator,
			&rule.Threshold,
			&cooldownSeconds,
			&lastNotifiedAt,
		); err != nil {
			return nil, err
		}
		rule.CooldownDuration = time.Duration(cooldownSeconds) * time.Second
		if lastNotifiedAt.Valid {
			rule.LastNotifiedAt = &lastNotifiedAt.Time
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func evaluateRule(ctx context.Context, db *pgx.Conn, alertNotifier notifier, rule alertRule, now time.Time) error {
	value, err := loadLatestValue(ctx, db, rule.MAC, rule.Metric)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !matches(rule.Operator, value.Value, rule.Threshold) {
		return upsertState(ctx, db, rule.ID, nil, rule.LastNotifiedAt, value.Value)
	}
	if rule.LastNotifiedAt != nil && now.Sub(*rule.LastNotifiedAt) < rule.CooldownDuration {
		return upsertState(ctx, db, rule.ID, &value.TS, rule.LastNotifiedAt, value.Value)
	}

	result, err := alertNotifier.Notify(ctx, db, rule, value, now)
	if err != nil {
		return err
	}
	if err := insertNotificationEvent(ctx, db, rule, value, result); err != nil {
		return err
	}
	notifyAt := result.SentAt
	if notifyAt == nil && result.Status == "dry_run" {
		notifyAt = &now
	}
	if err := upsertState(ctx, db, rule.ID, &value.TS, notifyAt, value.Value); err != nil {
		return err
	}
	log.Printf(
		"%s notification rule=%d mac=%s metric=%s value=%.2f threshold=%s%.2f ts=%s",
		result.Status,
		rule.ID,
		rule.MAC,
		rule.Metric,
		value.Value,
		rule.Operator,
		rule.Threshold,
		value.TS.Format(time.RFC3339),
	)
	return nil
}

func loadLatestValue(ctx context.Context, db *pgx.Conn, mac string, metric string) (latestValue, error) {
	column, ok := metricColumns[metric]
	if !ok {
		return latestValue{}, fmt.Errorf("unsupported metric %q", metric)
	}
	query := fmt.Sprintf(`
		SELECT ts, %s
		FROM sensor_minute
		WHERE mac = $1 AND %s IS NOT NULL
		ORDER BY ts DESC
		LIMIT 1
	`, column, column)

	var value latestValue
	err := db.QueryRow(ctx, query, mac).Scan(&value.TS, &value.Value)
	return value, err
}

func matches(operator string, value float64, threshold float64) bool {
	switch strings.TrimSpace(operator) {
	case ">":
		return value > threshold
	case ">=":
		return value >= threshold
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	default:
		return false
	}
}

func insertNotificationEvent(ctx context.Context, db *pgx.Conn, rule alertRule, value latestValue, result notificationResult) error {
	_, err := db.Exec(ctx, `
		INSERT INTO notification_events (
			alert_rule_id,
			user_id,
			mac,
			metric,
			value,
			threshold,
			triggered_at,
			sent_at,
			status,
			error_message
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, rule.ID, rule.UserID, rule.MAC, rule.Metric, value.Value, rule.Threshold, value.TS, result.SentAt, result.Status, result.ErrorMessage)
	return err
}

func upsertState(
	ctx context.Context,
	db *pgx.Conn,
	ruleID int64,
	lastTriggeredAt *time.Time,
	lastNotifiedAt *time.Time,
	lastValue float64,
) error {
	_, err := db.Exec(ctx, `
		INSERT INTO alert_rule_state (
			alert_rule_id,
			last_triggered_at,
			last_notified_at,
			last_value,
			updated_at
		)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (alert_rule_id) DO UPDATE SET
			last_triggered_at = EXCLUDED.last_triggered_at,
			last_notified_at = COALESCE(EXCLUDED.last_notified_at, alert_rule_state.last_notified_at),
			last_value = EXCLUDED.last_value,
			updated_at = now()
	`, ruleID, lastTriggeredAt, lastNotifiedAt, lastValue)
	return err
}
