package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"home-metrics/internal/adminwebhook"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

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
			h.updated_at
		FROM health_alert_state h
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
	var rowsAffected int64
	var err error
	if maintenanceMode {
		tag, execErr := api.db.Exec(r.Context(), `
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
		tag, execErr := api.db.Exec(r.Context(), `
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
			h.updated_at
		FROM health_alert_state h
		WHERE h.alert_key = $1
	`, key)
	return scanHealthAlert(row)
}

func scanHealthAlert(row scanner) (healthAlertResponse, error) {
	var item healthAlertResponse
	var labels []byte
	var firstFiredAt, lastNotifiedAt, acknowledgedAt, mutedUntil, manuallyResolvedAt, resolvedAt pgtype.Timestamptz
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
