package collectorstatus

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

const maxErrorLength = 2048

type Target struct {
	CollectorName string
	TargetType    string
	TargetKey     string
}

func (t Target) normalized() Target {
	if strings.TrimSpace(t.TargetKey) == "" {
		t.TargetKey = "default"
	}
	return t
}

func MarkSuccess(ctx context.Context, db *pgx.Conn, target Target) error {
	return markSuccess(ctx, db, target, false)
}

func MarkDataSuccess(ctx context.Context, db *pgx.Conn, target Target) error {
	return markSuccess(ctx, db, target, true)
}

func markSuccess(ctx context.Context, db *pgx.Conn, target Target, dataWritten bool) error {
	target = target.normalized()
	_, err := db.Exec(ctx, `
		INSERT INTO collector_status (
			collector_name,
			target_type,
			target_key,
			last_attempt_at,
			last_success_at,
			last_data_at,
			first_failure_at,
			last_error,
			consecutive_failures,
			updated_at
		)
		VALUES ($1, $2, $3, now(), now(), CASE WHEN $4 THEN now() ELSE NULL END, NULL, NULL, 0, now())
		ON CONFLICT (collector_name, target_type, target_key) DO UPDATE SET
			last_attempt_at = EXCLUDED.last_attempt_at,
			last_success_at = EXCLUDED.last_success_at,
			last_data_at = CASE
				WHEN $4 THEN EXCLUDED.last_data_at
				ELSE collector_status.last_data_at
			END,
			first_failure_at = NULL,
			last_error = NULL,
			consecutive_failures = 0,
			updated_at = now()
	`, target.CollectorName, target.TargetType, target.TargetKey, dataWritten)
	return err
}

func MarkFailure(ctx context.Context, db *pgx.Conn, target Target, failure error) error {
	target = target.normalized()
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	message = truncate(message, maxErrorLength)
	_, err := db.Exec(ctx, `
		INSERT INTO collector_status (
			collector_name,
			target_type,
			target_key,
			last_attempt_at,
			first_failure_at,
			last_failure_at,
			last_error,
			consecutive_failures,
			updated_at
		)
		VALUES ($1, $2, $3, now(), now(), now(), $4, 1, now())
		ON CONFLICT (collector_name, target_type, target_key) DO UPDATE SET
			last_attempt_at = EXCLUDED.last_attempt_at,
			first_failure_at = CASE
				WHEN collector_status.consecutive_failures = 0 OR collector_status.first_failure_at IS NULL
					THEN EXCLUDED.first_failure_at
				ELSE collector_status.first_failure_at
			END,
			last_failure_at = EXCLUDED.last_failure_at,
			last_error = EXCLUDED.last_error,
			consecutive_failures = collector_status.consecutive_failures + 1,
			updated_at = now()
	`, target.CollectorName, target.TargetType, target.TargetKey, message)
	return err
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
