package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"home-metrics/internal/adminwebhook"

	"github.com/jackc/pgx/v5"
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
	AlertKey           string               `json:"alert_key"`
	Status             string               `json:"status"`
	Severity           string               `json:"severity"`
	Title              string               `json:"title"`
	Source             string               `json:"source"`
	Summary            string               `json:"summary"`
	Labels             map[string]string    `json:"labels"`
	FirstFiredAt       *time.Time           `json:"first_fired_at,omitempty"`
	LastEvaluatedAt    time.Time            `json:"last_evaluated_at"`
	LastNotifiedAt     *time.Time           `json:"last_notified_at,omitempty"`
	AcknowledgedAt     *time.Time           `json:"acknowledged_at,omitempty"`
	AcknowledgedBy     string               `json:"acknowledged_by,omitempty"`
	MutedUntil         *time.Time           `json:"muted_until,omitempty"`
	MutedBy            string               `json:"muted_by,omitempty"`
	MutedReason        string               `json:"muted_reason,omitempty"`
	ManuallyResolvedAt *time.Time           `json:"manually_resolved_at,omitempty"`
	ManuallyResolvedBy string               `json:"manually_resolved_by,omitempty"`
	ResolvedAt         *time.Time           `json:"resolved_at,omitempty"`
	Maintenance        *maintenanceResponse `json:"maintenance,omitempty"`
	UpdatedAt          time.Time            `json:"updated_at"`
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

type maintenanceRequest struct {
	AlertKey    string `json:"alert_key,omitempty"`
	TargetKind  string `json:"target_kind,omitempty"`
	TargetLabel string `json:"target_label,omitempty"`
	Duration    string `json:"duration,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Actor       string `json:"actor,omitempty"`
}

type maintenanceResponse struct {
	AlertKey    string     `json:"alert_key"`
	TargetKind  string     `json:"target_kind"`
	TargetLabel string     `json:"target_label"`
	Reason      string     `json:"reason,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
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

func (api *apiServer) handleHealthDetails(w http.ResponseWriter, r *http.Request) {
	if err := api.db.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	var res healthDetailsResponse
	res.Status = "ok"
	res.Database = "ok"
	res.WebhookConfigured = api.adminWebhook != nil
	err := api.db.QueryRow(r.Context(), `
		SELECT
			(SELECT count(*) FROM collector_status),
			(SELECT count(*) FROM health_alert_state WHERE status = 'firing' AND labels->>'kind' = 'collector'),
			(SELECT count(*) FROM health_alert_state WHERE status = 'firing'),
			(SELECT count(*) FROM health_notification_events WHERE status = 'failed' AND created_at >= now() - interval '24 hours')
	`).Scan(&res.CollectorTargets, &res.StaleCollectors, &res.ActiveHealthAlerts, &res.FailedDeliveries24h)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query health details")
		return
	}
	if res.StaleCollectors > 0 || res.ActiveHealthAlerts > 0 || res.FailedDeliveries24h > 0 {
		res.Status = "degraded"
	}
	writeJSON(w, http.StatusOK, res)
}

func (api *apiServer) handleSchema(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT version, name, checksum, applied_at
		FROM schema_migrations
		ORDER BY version
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query schema migrations")
		return
	}
	defer rows.Close()

	res := schemaResponse{Migrations: []schemaMigrationResponse{}}
	for rows.Next() {
		var item schemaMigrationResponse
		if err := rows.Scan(&item.Version, &item.Name, &item.Checksum, &item.AppliedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "scan schema migration")
			return
		}
		if item.Version > res.CurrentVersion {
			res.CurrentVersion = item.Version
		}
		res.Migrations = append(res.Migrations, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read schema migrations")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (api *apiServer) handleCiscoSpacesFirehoseStatus(w http.ResponseWriter, r *http.Request) {
	res := ciscoSpacesFirehoseStatusResponse{
		LockKey:                    ciscoSpacesFirehoseLockKey,
		ConfiguredSecondaryAllowed: envBool("CISCO_SPACES_ALLOW_SECONDARY", false),
	}
	classID, objID := advisoryLockParts(ciscoSpacesFirehoseLockKey)
	var pid pgtype.Int4
	err := api.db.QueryRow(r.Context(), `
		SELECT pid
		FROM pg_locks
		WHERE locktype = 'advisory'
		  AND mode = 'ExclusiveLock'
		  AND granted
		  AND classid::bigint = $1
		  AND objid::bigint = $2
		  AND objsubid = 1
		LIMIT 1
	`, classID, objID).Scan(&pid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "query Cisco Spaces advisory lock")
		return
	}
	if pid.Valid {
		res.LockHeld = true
		res.LockHolderPID = &pid.Int32
	}

	var lastAttemptAt, lastSuccessAt, lastDataAt, firstFailureAt, lastFailureAt pgtype.Timestamptz
	var updatedAt pgtype.Timestamptz
	err = api.db.QueryRow(r.Context(), `
		SELECT
			last_attempt_at,
			last_success_at,
			last_data_at,
			first_failure_at,
			last_failure_at,
			COALESCE(last_error, ''),
			consecutive_failures,
			updated_at
		FROM collector_status
		WHERE collector_name = 'hm-cisco-spaces-collector'
		  AND target_type = 'cisco_spaces_firehose'
		  AND target_key = 'default'
	`).Scan(
		&lastAttemptAt,
		&lastSuccessAt,
		&lastDataAt,
		&firstFailureAt,
		&lastFailureAt,
		&res.LastError,
		&res.ConsecutiveFailures,
		&updatedAt,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "query Cisco Spaces collector status")
		return
	}
	if err == nil {
		res.CollectorStatusPresent = true
		res.LastAttemptAt = timePtrFromPg(lastAttemptAt)
		res.LastSuccessAt = timePtrFromPg(lastSuccessAt)
		res.LastDataAt = timePtrFromPg(lastDataAt)
		res.FirstFailureAt = timePtrFromPg(firstFailureAt)
		res.LastFailureAt = timePtrFromPg(lastFailureAt)
		res.CollectorUpdatedAt = timePtrFromPg(updatedAt)
		if res.CollectorUpdatedAt != nil {
			window := envDuration("HEALTH_CISCO_SPACES_HEARTBEAT_WARNING_AFTER", envDuration("HEALTH_COLLECTOR_HEARTBEAT_WARNING_AFTER", 5*time.Minute))
			res.CollectorReporting = time.Since(*res.CollectorUpdatedAt) < window
		}
	}
	res.Mode = "not-running"
	if res.LockHeld {
		res.Mode = "primary-lock-held"
	} else if res.CollectorReporting {
		res.Mode = "secondary-or-unlocked-reporting"
	} else if res.ConfiguredSecondaryAllowed {
		res.Mode = "secondary-allowed-idle"
	}
	writeJSON(w, http.StatusOK, res)
}

func (api *apiServer) handleCollectorStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT
			collector_name,
			target_type,
			target_key,
			last_attempt_at,
			last_success_at,
			last_data_at,
			first_failure_at,
			last_failure_at,
			COALESCE(last_error, ''),
			consecutive_failures,
			updated_at
		FROM collector_status
		ORDER BY collector_name, target_type, target_key
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query collector status")
		return
	}
	defer rows.Close()

	items := []collectorStatusResponse{}
	for rows.Next() {
		var item collectorStatusResponse
		var lastAttemptAt, lastSuccessAt, lastDataAt, firstFailureAt, lastFailureAt pgtype.Timestamptz
		if err := rows.Scan(
			&item.CollectorName,
			&item.TargetType,
			&item.TargetKey,
			&lastAttemptAt,
			&lastSuccessAt,
			&lastDataAt,
			&firstFailureAt,
			&lastFailureAt,
			&item.LastError,
			&item.ConsecutiveFailures,
			&item.UpdatedAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "scan collector status")
			return
		}
		item.LastAttemptAt = timePtrFromPg(lastAttemptAt)
		item.LastSuccessAt = timePtrFromPg(lastSuccessAt)
		item.LastDataAt = timePtrFromPg(lastDataAt)
		item.FirstFailureAt = timePtrFromPg(firstFailureAt)
		item.LastFailureAt = timePtrFromPg(lastFailureAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read collector status")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (api *apiServer) handleHealthAlerts(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	query := `
		SELECT
			h.alert_key,
			h.status,
			h.severity,
			h.title,
			h.source,
			h.summary,
			h.labels,
			h.first_fired_at,
			h.last_evaluated_at,
			h.last_notified_at,
			h.acknowledged_at,
			COALESCE(h.acknowledged_by, ''),
			h.muted_until,
			COALESCE(h.muted_by, ''),
			COALESCE(h.muted_reason, ''),
			h.manually_resolved_at,
			COALESCE(h.manually_resolved_by, ''),
			h.resolved_at,
			h.updated_at,
			COALESCE(m.alert_key, ''),
			COALESCE(m.target_kind, ''),
			COALESCE(m.target_label, ''),
			COALESCE(m.reason, ''),
			m.started_at,
			m.ends_at,
			COALESCE(m.created_by, ''),
			m.updated_at
		FROM health_alert_state h
		LEFT JOIN health_maintenance_targets m
			ON m.alert_key = h.alert_key
			AND (m.ends_at IS NULL OR m.ends_at > now())
	`
	args := []any{}
	if status != "" {
		if status != "firing" && status != "resolved" {
			writeError(w, http.StatusBadRequest, "invalid status")
			return
		}
		query += " WHERE h.status = $1"
		args = append(args, status)
	}
	query += " ORDER BY h.updated_at DESC, h.alert_key LIMIT 500"
	rows, err := api.db.Query(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query health alerts")
		return
	}
	defer rows.Close()

	items := []healthAlertResponse{}
	for rows.Next() {
		item, err := scanHealthAlert(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scan health alert")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read health alerts")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (api *apiServer) handleMaintenanceTargets(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT alert_key, target_kind, target_label, COALESCE(reason, ''), started_at, ends_at, COALESCE(created_by, ''), updated_at
		FROM health_maintenance_targets
		WHERE ends_at IS NULL OR ends_at > now()
		ORDER BY updated_at DESC, alert_key
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query maintenance targets")
		return
	}
	defer rows.Close()

	items := []maintenanceResponse{}
	for rows.Next() {
		item, err := scanMaintenance(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scan maintenance target")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read maintenance targets")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (api *apiServer) handleStartMaintenance(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeMaintenanceRequest(w, r)
	if !ok {
		return
	}
	req.AlertKey = strings.TrimSpace(req.AlertKey)
	if req.AlertKey == "" {
		writeError(w, http.StatusBadRequest, "alert_key is required")
		return
	}
	alertLabels := map[string]string{}
	req.TargetKind = strings.TrimSpace(req.TargetKind)
	req.TargetLabel = strings.TrimSpace(req.TargetLabel)
	if alert, err := api.loadHealthAlert(r.Context(), req.AlertKey); err == nil {
		alertLabels = alert.Labels
		if req.TargetKind == "" {
			req.TargetKind = alert.Labels["kind"]
		}
		if req.TargetLabel == "" {
			req.TargetLabel = alert.Source
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "query health alert")
		return
	}
	if req.TargetKind == "" || req.TargetLabel == "" {
		if req.TargetKind == "" {
			req.TargetKind = "health_alert"
		}
		if req.TargetLabel == "" {
			req.TargetLabel = req.AlertKey
		}
	}
	endsAt, err := maintenanceEndsAt(req.Duration)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid maintenance duration")
		return
	}
	actor := healthAlertActor(req.Actor)
	row := api.db.QueryRow(r.Context(), `
		INSERT INTO health_maintenance_targets (
			alert_key,
			target_kind,
			target_label,
			reason,
			started_at,
			ends_at,
			created_by,
			updated_at
		)
		VALUES ($1, $2, $3, NULLIF($4, ''), now(), $5, $6, now())
		ON CONFLICT (alert_key) DO UPDATE SET
			target_kind = EXCLUDED.target_kind,
			target_label = EXCLUDED.target_label,
			reason = EXCLUDED.reason,
			started_at = EXCLUDED.started_at,
			ends_at = EXCLUDED.ends_at,
			created_by = EXCLUDED.created_by,
			updated_at = EXCLUDED.updated_at
		RETURNING alert_key, target_kind, target_label, COALESCE(reason, ''), started_at, ends_at, COALESCE(created_by, ''), updated_at
	`, req.AlertKey, req.TargetKind, req.TargetLabel, strings.TrimSpace(req.Reason), endsAt, actor)
	item, err := scanMaintenance(row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "start maintenance")
		return
	}
	if item.TargetKind == "sensor" {
		if mac := strings.TrimSpace(alertLabels["mac"]); mac != "" {
			if _, err := api.db.Exec(r.Context(), `
				UPDATE devices
				SET maintenance_mode = true,
					maintenance_reason = NULLIF($2, ''),
					maintenance_since = $3,
					updated_at = now()
				WHERE mac = $1
			`, mac, item.Reason, item.StartedAt); err != nil {
				writeError(w, http.StatusInternalServerError, "sync device maintenance")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, item)
}

func (api *apiServer) handleEndMaintenance(w http.ResponseWriter, r *http.Request) {
	alertKey := strings.TrimSpace(r.PathValue("alert_key"))
	if alertKey == "" {
		writeError(w, http.StatusNotFound, "maintenance target not found")
		return
	}
	row := api.db.QueryRow(r.Context(), `
		DELETE FROM health_maintenance_targets
		WHERE alert_key = $1
		RETURNING alert_key, target_kind, target_label, COALESCE(reason, ''), started_at, ends_at, COALESCE(created_by, ''), updated_at
	`, alertKey)
	item, err := scanMaintenance(row)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "maintenance target not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "end maintenance")
		return
	}
	if item.TargetKind == "sensor" {
		if mac, err := api.healthAlertMAC(r.Context(), item.AlertKey); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "query device maintenance")
			return
		} else if mac != "" {
			if _, err := api.db.Exec(r.Context(), `
				UPDATE devices
				SET maintenance_mode = false,
					maintenance_reason = NULL,
					maintenance_since = NULL,
					updated_at = now()
				WHERE mac = $1
			`, mac); err != nil {
				writeError(w, http.StatusInternalServerError, "sync device maintenance")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, item)
}

func (api *apiServer) handleAckHealthAlert(w http.ResponseWriter, r *http.Request) {
	alertKey := strings.TrimSpace(r.PathValue("alert_key"))
	if alertKey == "" {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	req, ok := decodeHealthAlertOperationRequest(w, r)
	if !ok {
		return
	}
	actor := healthAlertActor(req.Actor)
	tag, err := api.db.Exec(r.Context(), `
		UPDATE health_alert_state
		SET acknowledged_at = now(),
			acknowledged_by = $2,
			updated_at = now()
		WHERE alert_key = $1
	`, alertKey, actor)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ack health alert")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	api.writeHealthAlert(w, r, alertKey)
}

func (api *apiServer) handleMuteHealthAlert(w http.ResponseWriter, r *http.Request) {
	alertKey := strings.TrimSpace(r.PathValue("alert_key"))
	if alertKey == "" {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	req, ok := decodeHealthAlertOperationRequest(w, r)
	if !ok {
		return
	}
	duration := time.Hour
	if strings.TrimSpace(req.Duration) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(req.Duration))
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid mute duration")
			return
		}
		duration = parsed
	}
	mutedUntil := time.Now().Add(duration)
	tag, err := api.db.Exec(r.Context(), `
		UPDATE health_alert_state
		SET muted_until = $2,
			muted_by = $3,
			muted_reason = NULLIF($4, ''),
			updated_at = now()
		WHERE alert_key = $1
	`, alertKey, mutedUntil, healthAlertActor(req.Actor), strings.TrimSpace(req.Reason))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mute health alert")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	api.writeHealthAlert(w, r, alertKey)
}

func (api *apiServer) handleResolveHealthAlert(w http.ResponseWriter, r *http.Request) {
	alertKey := strings.TrimSpace(r.PathValue("alert_key"))
	if alertKey == "" {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	req, ok := decodeHealthAlertOperationRequest(w, r)
	if !ok {
		return
	}
	tag, err := api.db.Exec(r.Context(), `
		UPDATE health_alert_state
		SET status = 'resolved',
			severity = 'info',
			resolved_at = now(),
			manually_resolved_at = now(),
			manually_resolved_by = $2,
			updated_at = now()
		WHERE alert_key = $1
	`, alertKey, healthAlertActor(req.Actor))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve health alert")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	api.writeHealthAlert(w, r, alertKey)
}

func (api *apiServer) handleDeviceMaintenance(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(strings.TrimSpace(r.PathValue("mac")))
	if mac == "" {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	req, ok := decodeHealthAlertOperationRequest(w, r)
	if !ok {
		return
	}
	maintenanceMode := true
	if req.MaintenanceMode != nil {
		maintenanceMode = *req.MaintenanceMode
	}
	reason := strings.TrimSpace(req.Reason)
	actor := healthAlertActor(req.Actor)
	alertKey := healthKey("metric", "sensor_minute", mac, "data")
	tx, err := api.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "begin device maintenance")
		return
	}
	defer tx.Rollback(r.Context())

	var rowsAffected int64
	if maintenanceMode {
		tag, execErr := tx.Exec(r.Context(), `
			UPDATE devices
			SET maintenance_mode = true,
				maintenance_reason = NULLIF($2, ''),
				maintenance_since = now(),
				updated_at = now()
			WHERE mac = $1
		`, mac, reason)
		err = execErr
		rowsAffected = tag.RowsAffected()
	} else {
		tag, execErr := tx.Exec(r.Context(), `
			UPDATE devices
			SET maintenance_mode = false,
				maintenance_reason = NULL,
				maintenance_since = NULL,
				updated_at = now()
			WHERE mac = $1
		`, mac)
		err = execErr
		rowsAffected = tag.RowsAffected()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update device maintenance")
		return
	}
	if rowsAffected == 0 {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if maintenanceMode {
		var label string
		err = tx.QueryRow(r.Context(), `
			SELECT COALESCE(NULLIF(label, ''), mac)
			FROM devices
			WHERE mac = $1
		`, mac).Scan(&label)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query device maintenance label")
			return
		}
		_, err = tx.Exec(r.Context(), `
			INSERT INTO health_maintenance_targets (
				alert_key,
				target_kind,
				target_label,
				reason,
				started_at,
				ends_at,
				created_by,
				updated_at
			)
			VALUES ($1, 'sensor', $2, NULLIF($3, ''), now(), NULL, $4, now())
			ON CONFLICT (alert_key) DO UPDATE SET
				target_kind = EXCLUDED.target_kind,
				target_label = EXCLUDED.target_label,
				reason = EXCLUDED.reason,
				started_at = EXCLUDED.started_at,
				ends_at = NULL,
				created_by = EXCLUDED.created_by,
				updated_at = EXCLUDED.updated_at
		`, alertKey, label, reason, actor)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "start device maintenance")
			return
		}
	} else {
		_, err = tx.Exec(r.Context(), `
			DELETE FROM health_maintenance_targets
			WHERE alert_key = $1
		`, alertKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "end device maintenance")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit device maintenance")
		return
	}

	device, err := api.loadDevice(r.Context(), mac)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query device")
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func (api *apiServer) handleHealthNotificationEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT
			id,
			event_id,
			alert_key,
			channel_id,
			channel_type,
			status,
			http_status,
			COALESCE(response_body, ''),
			COALESCE(error, ''),
			created_at
		FROM health_notification_events
		ORDER BY created_at DESC
		LIMIT 200
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query health notification events")
		return
	}
	defer rows.Close()

	items := []healthNotificationEventResponse{}
	for rows.Next() {
		var item healthNotificationEventResponse
		var channelID pgtype.Int8
		var httpStatus pgtype.Int4
		if err := rows.Scan(
			&item.ID,
			&item.EventID,
			&item.AlertKey,
			&channelID,
			&item.ChannelType,
			&item.Status,
			&httpStatus,
			&item.ResponseBody,
			&item.Error,
			&item.CreatedAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "scan health notification event")
			return
		}
		if channelID.Valid {
			item.ChannelID = &channelID.Int64
		}
		if httpStatus.Valid {
			item.HTTPStatus = &httpStatus.Int32
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read health notification events")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (api *apiServer) handleTestHealthWebhook(w http.ResponseWriter, r *http.Request) {
	if api.adminWebhook == nil {
		writeError(w, http.StatusServiceUnavailable, "admin webhook is not configured")
		return
	}
	alertKey := strings.TrimSpace(r.PathValue("alert_key"))
	alert, err := api.loadHealthAlert(r.Context(), alertKey)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query health alert")
		return
	}
	now := time.Now()
	eventID := newAPIHealthEventID(now, alert.AlertKey, "test")
	result, err := api.adminWebhook.Send(r.Context(), adminwebhook.Payload{
		EventID:  eventID,
		Status:   "info",
		Severity: alert.Severity,
		Title:    "Test: " + alert.Title,
		Source:   alert.Source,
		Summary:  alert.Summary,
		Labels:   alert.Labels,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "send admin webhook")
		return
	}
	writeJSON(w, http.StatusOK, testHealthWebhookResponse{
		EventID:    eventID,
		Status:     "sent",
		HTTPStatus: result.StatusCode,
	})
}

func (api *apiServer) loadHealthAlert(ctx context.Context, key string) (healthAlertResponse, error) {
	row := api.db.QueryRow(ctx, `
		SELECT
			h.alert_key,
			h.status,
			h.severity,
			h.title,
			h.source,
			h.summary,
			h.labels,
			h.first_fired_at,
			h.last_evaluated_at,
			h.last_notified_at,
			h.acknowledged_at,
			COALESCE(h.acknowledged_by, ''),
			h.muted_until,
			COALESCE(h.muted_by, ''),
			COALESCE(h.muted_reason, ''),
			h.manually_resolved_at,
			COALESCE(h.manually_resolved_by, ''),
			h.resolved_at,
			h.updated_at,
			COALESCE(m.alert_key, ''),
			COALESCE(m.target_kind, ''),
			COALESCE(m.target_label, ''),
			COALESCE(m.reason, ''),
			m.started_at,
			m.ends_at,
			COALESCE(m.created_by, ''),
			m.updated_at
		FROM health_alert_state h
		LEFT JOIN health_maintenance_targets m
			ON m.alert_key = h.alert_key
			AND (m.ends_at IS NULL OR m.ends_at > now())
		WHERE h.alert_key = $1
	`, key)
	return scanHealthAlert(row)
}

func scanHealthAlert(row scanner) (healthAlertResponse, error) {
	var item healthAlertResponse
	var labels []byte
	var firstFiredAt, lastNotifiedAt, acknowledgedAt, mutedUntil, manuallyResolvedAt, resolvedAt pgtype.Timestamptz
	var maintenanceAlertKey, maintenanceTargetKind, maintenanceTargetLabel, maintenanceReason, maintenanceCreatedBy string
	var maintenanceStartedAt, maintenanceEndsAt, maintenanceUpdatedAt pgtype.Timestamptz
	if err := row.Scan(
		&item.AlertKey,
		&item.Status,
		&item.Severity,
		&item.Title,
		&item.Source,
		&item.Summary,
		&labels,
		&firstFiredAt,
		&item.LastEvaluatedAt,
		&lastNotifiedAt,
		&acknowledgedAt,
		&item.AcknowledgedBy,
		&mutedUntil,
		&item.MutedBy,
		&item.MutedReason,
		&manuallyResolvedAt,
		&item.ManuallyResolvedBy,
		&resolvedAt,
		&item.UpdatedAt,
		&maintenanceAlertKey,
		&maintenanceTargetKind,
		&maintenanceTargetLabel,
		&maintenanceReason,
		&maintenanceStartedAt,
		&maintenanceEndsAt,
		&maintenanceCreatedBy,
		&maintenanceUpdatedAt,
	); err != nil {
		return healthAlertResponse{}, err
	}
	item.Labels = map[string]string{}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &item.Labels); err != nil {
			return healthAlertResponse{}, err
		}
	}
	item.FirstFiredAt = timePtrFromPg(firstFiredAt)
	item.LastNotifiedAt = timePtrFromPg(lastNotifiedAt)
	item.AcknowledgedAt = timePtrFromPg(acknowledgedAt)
	item.MutedUntil = timePtrFromPg(mutedUntil)
	item.ManuallyResolvedAt = timePtrFromPg(manuallyResolvedAt)
	item.ResolvedAt = timePtrFromPg(resolvedAt)
	if maintenanceAlertKey != "" && maintenanceStartedAt.Valid && maintenanceUpdatedAt.Valid {
		item.Maintenance = &maintenanceResponse{
			AlertKey:    maintenanceAlertKey,
			TargetKind:  maintenanceTargetKind,
			TargetLabel: maintenanceTargetLabel,
			Reason:      maintenanceReason,
			StartedAt:   maintenanceStartedAt.Time,
			EndsAt:      timePtrFromPg(maintenanceEndsAt),
			CreatedBy:   maintenanceCreatedBy,
			UpdatedAt:   maintenanceUpdatedAt.Time,
		}
	}
	return item, nil
}

func (api *apiServer) writeHealthAlert(w http.ResponseWriter, r *http.Request, alertKey string) {
	alert, err := api.loadHealthAlert(r.Context(), alertKey)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "health alert not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query health alert")
		return
	}
	writeJSON(w, http.StatusOK, alert)
}

func (api *apiServer) healthAlertMAC(ctx context.Context, alertKey string) (string, error) {
	var mac string
	err := api.db.QueryRow(ctx, `
		SELECT COALESCE(labels->>'mac', '')
		FROM health_alert_state
		WHERE alert_key = $1
	`, alertKey).Scan(&mac)
	return mac, err
}

func decodeHealthAlertOperationRequest(w http.ResponseWriter, r *http.Request) (healthAlertOperationRequest, bool) {
	var req healthAlertOperationRequest
	if r.Body == nil || r.ContentLength == 0 {
		return req, true
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return healthAlertOperationRequest{}, false
	}
	return req, true
}

func decodeMaintenanceRequest(w http.ResponseWriter, r *http.Request) (maintenanceRequest, bool) {
	var req maintenanceRequest
	if r.Body == nil || r.ContentLength == 0 {
		return req, true
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return maintenanceRequest{}, false
	}
	return req, true
}

func maintenanceEndsAt(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return nil, errors.New("invalid duration")
	}
	endsAt := time.Now().Add(duration)
	return &endsAt, nil
}

func scanMaintenance(row scanner) (maintenanceResponse, error) {
	var item maintenanceResponse
	var endsAt pgtype.Timestamptz
	if err := row.Scan(
		&item.AlertKey,
		&item.TargetKind,
		&item.TargetLabel,
		&item.Reason,
		&item.StartedAt,
		&endsAt,
		&item.CreatedBy,
		&item.UpdatedAt,
	); err != nil {
		return maintenanceResponse{}, err
	}
	item.EndsAt = timePtrFromPg(endsAt)
	return item, nil
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
