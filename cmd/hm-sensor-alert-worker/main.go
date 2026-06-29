package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"home-metrics/internal/sensor"
	"home-metrics/internal/sensoralert"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"

type dbRule struct {
	ID            int64
	Name          string
	MAC           string
	Metric        string
	Severity      string
	DeviceEnabled bool
	Rule          sensoralert.Rule
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := envString("BLE_DB_DSN", defaultDBDSN)
	interval := envDuration("SENSOR_ALERT_WORKER_INTERVAL", time.Minute)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		log.Fatalf("acquire alert worker lock connection: %v", err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('home-metrics:sensor-alert-worker'))`); err != nil {
		log.Fatalf("acquire alert worker advisory lock: %v", err)
	}
	defer lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('home-metrics:sensor-alert-worker'))`)

	log.Printf("sensor alert worker started interval=%s", interval)
	runCycle(ctx, pool)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCycle(ctx, pool)
		}
	}
}

func runCycle(ctx context.Context, pool *pgxpool.Pool) {
	if err := evaluateRules(ctx, pool, time.Now()); err != nil {
		log.Printf("evaluate sensor alerts: %v", err)
	}
}

func evaluateRules(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	rules, err := loadRules(ctx, pool)
	if err != nil {
		return err
	}
	var errs []error
	for _, rule := range rules {
		if err := evaluateDBRule(ctx, pool, rule, now); err != nil {
			errs = append(errs, fmt.Errorf("rule=%d mac=%s metric=%s: %w", rule.ID, rule.MAC, rule.Metric, err))
		}
	}
	return errors.Join(errs...)
}

func loadRules(ctx context.Context, pool *pgxpool.Pool) ([]dbRule, error) {
	rows, err := pool.Query(ctx, `
		SELECT
			r.id,
			r.name,
			r.mac,
			r.metric,
			r.direction,
			r.trigger_threshold,
			r.clear_threshold,
			EXTRACT(EPOCH FROM r.for_duration)::bigint,
			EXTRACT(EPOCH FROM r.max_data_age)::bigint,
			r.severity,
			r.enabled,
			d.enabled
		FROM sensor_alert_rules r
		JOIN devices d ON d.mac = r.mac
		ORDER BY r.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []dbRule
	for rows.Next() {
		var rule dbRule
		var direction string
		var forSeconds, maxAgeSeconds int64
		if err := rows.Scan(
			&rule.ID,
			&rule.Name,
			&rule.MAC,
			&rule.Metric,
			&direction,
			&rule.Rule.TriggerThreshold,
			&rule.Rule.ClearThreshold,
			&forSeconds,
			&maxAgeSeconds,
			&rule.Severity,
			&rule.Rule.Enabled,
			&rule.DeviceEnabled,
		); err != nil {
			return nil, err
		}
		rule.Rule.Direction = sensoralert.Direction(direction)
		rule.Rule.ForDuration = time.Duration(forSeconds) * time.Second
		rule.Rule.MaxDataAge = time.Duration(maxAgeSeconds) * time.Second
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func evaluateDBRule(ctx context.Context, pool *pgxpool.Pool, rule dbRule, now time.Time) error {
	column, ok := sensor.Columns()[rule.Metric]
	if !ok {
		return fmt.Errorf("unsupported metric %q", rule.Metric)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	state, err := loadState(ctx, tx, rule.ID)
	if err != nil {
		return err
	}
	var observation *sensoralert.Observation
	if rule.Rule.Enabled && rule.DeviceEnabled {
		observation, err = loadLatestObservation(ctx, tx, rule.MAC, column)
		if err != nil {
			return err
		}
	}
	result := sensoralert.Evaluate(now, rule.Rule, rule.DeviceEnabled, state, observation)
	if err := saveState(ctx, tx, rule.ID, result.State); err != nil {
		return err
	}
	if result.Event != nil {
		if err := insertEvent(ctx, tx, rule, *result.Event, now); err != nil {
			return err
		}
		log.Printf("sensor alert %s rule=%d mac=%s metric=%s reason=%s", result.Event.Type, rule.ID, rule.MAC, rule.Metric, result.Event.Reason)
	}
	return tx.Commit(ctx)
}

func loadState(ctx context.Context, tx pgx.Tx, ruleID int64) (sensoralert.State, error) {
	var state sensoralert.State
	var status, evaluationStatus string
	var pendingSince, firedAt, resolvedAt, lastValueAt, lastEvaluatedAt pgtype.Timestamptz
	var lastValue pgtype.Float8
	err := tx.QueryRow(ctx, `
		SELECT status, pending_since, fired_at, resolved_at, last_value, last_value_at, last_evaluated_at, evaluation_status
		FROM sensor_alert_states
		WHERE alert_rule_id = $1
		FOR UPDATE
	`, ruleID).Scan(&status, &pendingSince, &firedAt, &resolvedAt, &lastValue, &lastValueAt, &lastEvaluatedAt, &evaluationStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return sensoralert.State{Status: sensoralert.StatusNormal, EvaluationStatus: sensoralert.EvaluationNoData}, nil
	}
	if err != nil {
		return sensoralert.State{}, err
	}
	state.Status = sensoralert.Status(status)
	state.EvaluationStatus = sensoralert.EvaluationStatus(evaluationStatus)
	state.PendingSince = timestampPointer(pendingSince)
	state.FiredAt = timestampPointer(firedAt)
	state.ResolvedAt = timestampPointer(resolvedAt)
	state.LastValueAt = timestampPointer(lastValueAt)
	state.LastEvaluatedAt = timestampPointer(lastEvaluatedAt)
	if lastValue.Valid {
		value := lastValue.Float64
		state.LastValue = &value
	}
	return state, nil
}

func loadLatestObservation(ctx context.Context, tx pgx.Tx, mac, column string) (*sensoralert.Observation, error) {
	query := fmt.Sprintf(`
		SELECT ts, %s
		FROM sensor_minute
		WHERE mac = $1 AND %s IS NOT NULL
		ORDER BY ts DESC
		LIMIT 1
	`, column, column)
	var observation sensoralert.Observation
	err := tx.QueryRow(ctx, query, mac).Scan(&observation.Time, &observation.Value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &observation, nil
}

func saveState(ctx context.Context, tx pgx.Tx, ruleID int64, state sensoralert.State) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sensor_alert_states (
			alert_rule_id, status, pending_since, fired_at, resolved_at,
			last_value, last_value_at, last_evaluated_at, evaluation_status, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (alert_rule_id) DO UPDATE SET
			status = EXCLUDED.status,
			pending_since = EXCLUDED.pending_since,
			fired_at = EXCLUDED.fired_at,
			resolved_at = EXCLUDED.resolved_at,
			last_value = EXCLUDED.last_value,
			last_value_at = EXCLUDED.last_value_at,
			last_evaluated_at = EXCLUDED.last_evaluated_at,
			evaluation_status = EXCLUDED.evaluation_status,
			updated_at = now()
	`, ruleID, state.Status, state.PendingSince, state.FiredAt, state.ResolvedAt, state.LastValue, state.LastValueAt, state.LastEvaluatedAt, state.EvaluationStatus)
	return err
}

func insertEvent(ctx context.Context, tx pgx.Tx, rule dbRule, event sensoralert.Event, occurredAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sensor_alert_events (
			alert_rule_id, event_type, reason, rule_name, mac, metric, severity,
			direction, trigger_threshold, clear_threshold, value, value_at, occurred_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, rule.ID, event.Type, event.Reason, rule.Name, rule.MAC, rule.Metric, rule.Severity,
		rule.Rule.Direction, rule.Rule.TriggerThreshold, rule.Rule.ClearThreshold, event.Value, event.ValueAt, occurredAt)
	return err
}

func timestampPointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		log.Printf("invalid %s=%q, using %s", name, value, fallback)
		return fallback
	}
	return parsed
}
