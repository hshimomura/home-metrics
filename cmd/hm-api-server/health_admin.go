package main

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const ciscoSpacesFirehoseLockKey int64 = 734829148912345

type collectorStatusResponse struct {
	CollectorName       string     `json:"collector_name"`
	TargetType          string     `json:"target_type"`
	TargetKey           string     `json:"target_key"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastDataAt          *time.Time `json:"last_data_at,omitempty"`
	FirstFailureAt      *time.Time `json:"first_failure_at,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type healthAlertResponse struct {
	AlertKey           string            `json:"alert_key"`
	Status             string            `json:"status"`
	Severity           string            `json:"severity"`
	Title              string            `json:"title"`
	Source             string            `json:"source"`
	Summary            string            `json:"summary"`
	Labels             map[string]string `json:"labels"`
	FirstFiredAt       *time.Time        `json:"first_fired_at,omitempty"`
	LastEvaluatedAt    time.Time         `json:"last_evaluated_at"`
	LastNotifiedAt     *time.Time        `json:"last_notified_at,omitempty"`
	AcknowledgedAt     *time.Time        `json:"acknowledged_at,omitempty"`
	AcknowledgedBy     string            `json:"acknowledged_by,omitempty"`
	MutedUntil         *time.Time        `json:"muted_until,omitempty"`
	MutedBy            string            `json:"muted_by,omitempty"`
	MutedReason        string            `json:"muted_reason,omitempty"`
	ManuallyResolvedAt *time.Time        `json:"manually_resolved_at,omitempty"`
	ManuallyResolvedBy string            `json:"manually_resolved_by,omitempty"`
	ResolvedAt         *time.Time        `json:"resolved_at,omitempty"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

type healthNotificationEventResponse struct {
	ID           int64     `json:"id"`
	EventID      string    `json:"event_id"`
	AlertKey     string    `json:"alert_key"`
	ChannelID    *int64    `json:"channel_id,omitempty"`
	ChannelType  string    `json:"channel_type"`
	Status       string    `json:"status"`
	HTTPStatus   *int32    `json:"http_status,omitempty"`
	ResponseBody string    `json:"response_body,omitempty"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type healthDetailsResponse struct {
	Status              string `json:"status"`
	Database            string `json:"database"`
	CollectorTargets    int    `json:"collector_targets"`
	StaleCollectors     int    `json:"stale_collectors"`
	ActiveHealthAlerts  int    `json:"active_health_alerts"`
	FailedDeliveries24h int    `json:"failed_deliveries_24h"`
	WebhookConfigured   bool   `json:"webhook_configured"`
}

type testHealthWebhookResponse struct {
	EventID    string `json:"event_id"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type healthAlertOperationRequest struct {
	Actor           string `json:"actor,omitempty"`
	Duration        string `json:"duration,omitempty"`
	Reason          string `json:"reason,omitempty"`
	MaintenanceMode *bool  `json:"maintenance_mode,omitempty"`
}

type schemaMigrationResponse struct {
	Version   int64     `json:"version"`
	Name      string    `json:"name"`
	Checksum  string    `json:"checksum"`
	AppliedAt time.Time `json:"applied_at"`
}

type schemaResponse struct {
	CurrentVersion int64                     `json:"current_version"`
	Migrations     []schemaMigrationResponse `json:"migrations"`
}

type ciscoSpacesFirehoseStatusResponse struct {
	LockKey                    int64      `json:"lock_key"`
	LockHeld                   bool       `json:"lock_held"`
	LockHolderPID              *int32     `json:"lock_holder_pid,omitempty"`
	Mode                       string     `json:"mode"`
	ConfiguredSecondaryAllowed bool       `json:"configured_secondary_allowed"`
	CollectorStatusPresent     bool       `json:"collector_status_present"`
	CollectorReporting         bool       `json:"collector_reporting"`
	CollectorUpdatedAt         *time.Time `json:"collector_updated_at,omitempty"`
	LastAttemptAt              *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt              *time.Time `json:"last_success_at,omitempty"`
	LastDataAt                 *time.Time `json:"last_data_at,omitempty"`
	FirstFailureAt             *time.Time `json:"first_failure_at,omitempty"`
	LastFailureAt              *time.Time `json:"last_failure_at,omitempty"`
	ConsecutiveFailures        int        `json:"consecutive_failures"`
	LastError                  string     `json:"last_error,omitempty"`
}

func healthAlertActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "admin-api"
	}
	return actor
}

func healthKey(parts ...string) string {
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(strings.TrimSpace(part), ":", "_")
	}
	return strings.Join(parts, ":")
}

func advisoryLockParts(key int64) (int64, int64) {
	value := uint64(key)
	return int64(uint32(value >> 32)), int64(uint32(value))
}

func timePtrFromPg(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func newAPIHealthEventID(now time.Time, key string, status string) string {
	hash := sha1.Sum([]byte(now.Format(time.RFC3339Nano) + ":" + key + ":" + status))
	return "hm-" + now.UTC().Format("20060102150405") + "-" + hex.EncodeToString(hash[:])[:10]
}
