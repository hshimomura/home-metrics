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
	AlertKey        string            `json:"alert_key"`
	Status          string            `json:"status"`
	Severity        string            `json:"severity"`
	Title           string            `json:"title"`
	Source          string            `json:"source"`
	Summary         string            `json:"summary"`
	Labels          map[string]string `json:"labels"`
	FirstFiredAt    *time.Time        `json:"first_fired_at,omitempty"`
	LastEvaluatedAt time.Time         `json:"last_evaluated_at"`
	LastNotifiedAt  *time.Time        `json:"last_notified_at,omitempty"`
	ResolvedAt      *time.Time        `json:"resolved_at,omitempty"`
	UpdatedAt       time.Time         `json:"updated_at"`
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
			alert_key,
			status,
			severity,
			title,
			source,
			summary,
			labels,
			first_fired_at,
			last_evaluated_at,
			last_notified_at,
			resolved_at,
			updated_at
		FROM health_alert_state
	`
	args := []any{}
	if status != "" {
		if status != "firing" && status != "resolved" {
			writeError(w, http.StatusBadRequest, "invalid status")
			return
		}
		query += " WHERE status = $1"
		args = append(args, status)
	}
	query += " ORDER BY updated_at DESC, alert_key LIMIT 500"
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
			alert_key,
			status,
			severity,
			title,
			source,
			summary,
			labels,
			first_fired_at,
			last_evaluated_at,
			last_notified_at,
			resolved_at,
			updated_at
		FROM health_alert_state
		WHERE alert_key = $1
	`, key)
	return scanHealthAlert(row)
}

func scanHealthAlert(row scanner) (healthAlertResponse, error) {
	var item healthAlertResponse
	var labels []byte
	var firstFiredAt, lastNotifiedAt, resolvedAt pgtype.Timestamptz
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
		&resolvedAt,
		&item.UpdatedAt,
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
	item.ResolvedAt = timePtrFromPg(resolvedAt)
	return item, nil
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
