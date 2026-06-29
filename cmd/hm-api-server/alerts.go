package main

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type sensorAlertRuleRequest struct {
	Name               string   `json:"name"`
	MAC                string   `json:"mac"`
	Metric             string   `json:"metric"`
	Direction          string   `json:"direction"`
	TriggerThreshold   *float64 `json:"trigger_threshold"`
	ClearThreshold     *float64 `json:"clear_threshold"`
	ForDurationSeconds *int64   `json:"for_duration_seconds"`
	MaxDataAgeSeconds  *int64   `json:"max_data_age_seconds"`
	Severity           string   `json:"severity"`
	Enabled            *bool    `json:"enabled"`
}

type sensorAlertRuleResponse struct {
	ID                 int64     `json:"id"`
	Name               string    `json:"name"`
	MAC                string    `json:"mac"`
	DeviceLabel        string    `json:"device_label"`
	Metric             string    `json:"metric"`
	Direction          string    `json:"direction"`
	TriggerThreshold   float64   `json:"trigger_threshold"`
	ClearThreshold     float64   `json:"clear_threshold"`
	ForDurationSeconds int64     `json:"for_duration_seconds"`
	MaxDataAgeSeconds  int64     `json:"max_data_age_seconds"`
	Severity           string    `json:"severity"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type sensorAlertResponse struct {
	RuleID           int64      `json:"rule_id"`
	RuleName         string     `json:"rule_name"`
	MAC              string     `json:"mac"`
	DeviceLabel      string     `json:"device_label"`
	Metric           string     `json:"metric"`
	Severity         string     `json:"severity"`
	Direction        string     `json:"direction"`
	TriggerThreshold float64    `json:"trigger_threshold"`
	ClearThreshold   float64    `json:"clear_threshold"`
	Status           string     `json:"status"`
	EvaluationStatus string     `json:"evaluation_status"`
	PendingSince     *time.Time `json:"pending_since,omitempty"`
	FiredAt          *time.Time `json:"fired_at,omitempty"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
	LastValue        *float64   `json:"last_value,omitempty"`
	LastValueAt      *time.Time `json:"last_value_at,omitempty"`
	LastEvaluatedAt  *time.Time `json:"last_evaluated_at,omitempty"`
}

type sensorAlertEventResponse struct {
	ID               int64      `json:"id"`
	AlertRuleID      *int64     `json:"alert_rule_id,omitempty"`
	EventType        string     `json:"event_type"`
	Reason           string     `json:"reason"`
	RuleName         string     `json:"rule_name"`
	MAC              string     `json:"mac"`
	Metric           string     `json:"metric"`
	Severity         string     `json:"severity"`
	Direction        string     `json:"direction"`
	TriggerThreshold float64    `json:"trigger_threshold"`
	ClearThreshold   float64    `json:"clear_threshold"`
	Value            *float64   `json:"value,omitempty"`
	ValueAt          *time.Time `json:"value_at,omitempty"`
	OccurredAt       time.Time  `json:"occurred_at"`
}

type validatedSensorAlertRule struct {
	Name               string
	MAC                string
	Metric             string
	Direction          string
	TriggerThreshold   float64
	ClearThreshold     float64
	ForDurationSeconds int64
	MaxDataAgeSeconds  int64
	Severity           string
	Enabled            bool
}

func (api *apiServer) handleSensorAlertRules(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), sensorAlertRuleSelect+` ORDER BY r.id`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query sensor alert rules")
		return
	}
	defer rows.Close()
	rules := []sensorAlertRuleResponse{}
	for rows.Next() {
		var rule sensorAlertRuleResponse
		if err := scanSensorAlertRule(rows, &rule); err != nil {
			writeError(w, http.StatusInternalServerError, "scan sensor alert rules")
			return
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read sensor alert rules")
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func (api *apiServer) handleCreateSensorAlertRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := decodeAndValidateSensorAlertRule(w, r)
	if !ok {
		return
	}
	if exists, err := api.deviceExists(r, rule.MAC); err != nil {
		writeError(w, http.StatusInternalServerError, "query device")
		return
	} else if !exists {
		writeError(w, http.StatusBadRequest, "device not found")
		return
	}

	var id int64
	err := api.db.QueryRow(r.Context(), `
		INSERT INTO sensor_alert_rules (
			name, mac, metric, direction, trigger_threshold, clear_threshold,
			for_duration, max_data_age, severity, enabled
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7::double precision * interval '1 second', $8::double precision * interval '1 second', $9, $10)
		RETURNING id
	`, rule.Name, rule.MAC, rule.Metric, rule.Direction, rule.TriggerThreshold, rule.ClearThreshold,
		rule.ForDurationSeconds, rule.MaxDataAgeSeconds, rule.Severity, rule.Enabled).Scan(&id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create sensor alert rule")
		return
	}
	created, err := api.loadSensorAlertRule(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query created sensor alert rule")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (api *apiServer) handleUpdateSensorAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := parseAlertRuleID(w, r.PathValue("id"))
	if !ok {
		return
	}
	rule, ok := decodeAndValidateSensorAlertRule(w, r)
	if !ok {
		return
	}
	if exists, err := api.deviceExists(r, rule.MAC); err != nil {
		writeError(w, http.StatusInternalServerError, "query device")
		return
	} else if !exists {
		writeError(w, http.StatusBadRequest, "device not found")
		return
	}

	tag, err := api.db.Exec(r.Context(), `
		UPDATE sensor_alert_rules
		SET name = $2,
			mac = $3,
			metric = $4,
			direction = $5,
			trigger_threshold = $6,
			clear_threshold = $7,
			for_duration = $8::double precision * interval '1 second',
			max_data_age = $9::double precision * interval '1 second',
			severity = $10,
			enabled = $11,
			updated_at = now()
		WHERE id = $1
	`, id, rule.Name, rule.MAC, rule.Metric, rule.Direction, rule.TriggerThreshold, rule.ClearThreshold,
		rule.ForDurationSeconds, rule.MaxDataAgeSeconds, rule.Severity, rule.Enabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update sensor alert rule")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "sensor alert rule not found")
		return
	}
	updated, err := api.loadSensorAlertRule(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query updated sensor alert rule")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (api *apiServer) handleDeleteSensorAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := parseAlertRuleID(w, r.PathValue("id"))
	if !ok {
		return
	}
	tag, err := api.db.Exec(r.Context(), `DELETE FROM sensor_alert_rules WHERE id = $1`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete sensor alert rule")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "sensor alert rule not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *apiServer) handleSensorAlerts(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && status != "normal" && status != "pending" && status != "firing" {
		writeError(w, http.StatusBadRequest, "unsupported alert status")
		return
	}
	rows, err := api.db.Query(r.Context(), `
		SELECT
			r.id, r.name, r.mac, d.label, r.metric, r.severity, r.direction,
			r.trigger_threshold, r.clear_threshold,
			s.status, s.evaluation_status, s.pending_since, s.fired_at, s.resolved_at,
			s.last_value, s.last_value_at, s.last_evaluated_at
		FROM sensor_alert_states s
		JOIN sensor_alert_rules r ON r.id = s.alert_rule_id
		JOIN devices d ON d.mac = r.mac
		WHERE ($1 = '' OR s.status = $1)
		ORDER BY
			CASE s.status WHEN 'firing' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END,
			r.id
	`, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query sensor alerts")
		return
	}
	defer rows.Close()
	alerts := []sensorAlertResponse{}
	for rows.Next() {
		var alert sensorAlertResponse
		var pendingSince, firedAt, resolvedAt, lastValueAt, lastEvaluatedAt pgtype.Timestamptz
		var lastValue pgtype.Float8
		if err := rows.Scan(
			&alert.RuleID, &alert.RuleName, &alert.MAC, &alert.DeviceLabel, &alert.Metric,
			&alert.Severity, &alert.Direction, &alert.TriggerThreshold, &alert.ClearThreshold,
			&alert.Status, &alert.EvaluationStatus, &pendingSince, &firedAt, &resolvedAt,
			&lastValue, &lastValueAt, &lastEvaluatedAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "scan sensor alerts")
			return
		}
		alert.PendingSince = pgTimestampPointer(pendingSince)
		alert.FiredAt = pgTimestampPointer(firedAt)
		alert.ResolvedAt = pgTimestampPointer(resolvedAt)
		alert.LastValueAt = pgTimestampPointer(lastValueAt)
		alert.LastEvaluatedAt = pgTimestampPointer(lastEvaluatedAt)
		alert.LastValue = pgFloatPointer(lastValue)
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read sensor alerts")
		return
	}
	writeJSON(w, http.StatusOK, alerts)
}

func (api *apiServer) handleSensorAlertEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = parsed
	}
	eventType := strings.TrimSpace(r.URL.Query().Get("event_type"))
	if eventType != "" && eventType != "firing" && eventType != "resolved" {
		writeError(w, http.StatusBadRequest, "unsupported event type")
		return
	}
	mac := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mac")))
	var since *time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		since = &parsed
	}

	rows, err := api.db.Query(r.Context(), `
		SELECT
			id, alert_rule_id, event_type, reason, rule_name, mac, metric, severity,
			direction, trigger_threshold, clear_threshold, value, value_at, occurred_at
		FROM sensor_alert_events
		WHERE ($1::timestamptz IS NULL OR occurred_at >= $1)
			AND ($2 = '' OR event_type = $2)
			AND ($3 = '' OR mac = $3)
		ORDER BY occurred_at DESC, id DESC
		LIMIT $4
	`, since, eventType, mac, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query sensor alert events")
		return
	}
	defer rows.Close()
	events := []sensorAlertEventResponse{}
	for rows.Next() {
		var event sensorAlertEventResponse
		var ruleID pgtype.Int8
		var value pgtype.Float8
		var valueAt pgtype.Timestamptz
		if err := rows.Scan(
			&event.ID, &ruleID, &event.EventType, &event.Reason, &event.RuleName,
			&event.MAC, &event.Metric, &event.Severity, &event.Direction,
			&event.TriggerThreshold, &event.ClearThreshold, &value, &valueAt, &event.OccurredAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "scan sensor alert events")
			return
		}
		if ruleID.Valid {
			id := ruleID.Int64
			event.AlertRuleID = &id
		}
		event.Value = pgFloatPointer(value)
		event.ValueAt = pgTimestampPointer(valueAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read sensor alert events")
		return
	}
	writeJSON(w, http.StatusOK, events)
}

const sensorAlertRuleSelect = `
	SELECT
		r.id, r.name, r.mac, d.label, r.metric, r.direction,
		r.trigger_threshold, r.clear_threshold,
		EXTRACT(EPOCH FROM r.for_duration)::bigint,
		EXTRACT(EPOCH FROM r.max_data_age)::bigint,
		r.severity, r.enabled, r.created_at, r.updated_at
	FROM sensor_alert_rules r
	JOIN devices d ON d.mac = r.mac
`

func (api *apiServer) loadSensorAlertRule(r *http.Request, id int64) (sensorAlertRuleResponse, error) {
	var rule sensorAlertRuleResponse
	err := scanSensorAlertRule(api.db.QueryRow(r.Context(), sensorAlertRuleSelect+` WHERE r.id = $1`, id), &rule)
	return rule, err
}

type sensorAlertRuleScanner interface {
	Scan(dest ...any) error
}

func scanSensorAlertRule(row sensorAlertRuleScanner, rule *sensorAlertRuleResponse) error {
	return row.Scan(
		&rule.ID, &rule.Name, &rule.MAC, &rule.DeviceLabel, &rule.Metric, &rule.Direction,
		&rule.TriggerThreshold, &rule.ClearThreshold, &rule.ForDurationSeconds,
		&rule.MaxDataAgeSeconds, &rule.Severity, &rule.Enabled, &rule.CreatedAt, &rule.UpdatedAt,
	)
}

func decodeAndValidateSensorAlertRule(w http.ResponseWriter, r *http.Request) (validatedSensorAlertRule, bool) {
	var request sensorAlertRuleRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return validatedSensorAlertRule{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "JSON body must contain one object")
		return validatedSensorAlertRule{}, false
	}
	rule, err := validateSensorAlertRule(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return validatedSensorAlertRule{}, false
	}
	return rule, true
}

func validateSensorAlertRule(request sensorAlertRuleRequest) (validatedSensorAlertRule, error) {
	name := strings.TrimSpace(request.Name)
	if name == "" || len(name) > 200 {
		return validatedSensorAlertRule{}, errors.New("name is required and must be at most 200 characters")
	}
	mac := strings.ToLower(strings.TrimSpace(request.MAC))
	if mac == "" {
		return validatedSensorAlertRule{}, errors.New("mac is required")
	}
	metric := strings.TrimSpace(request.Metric)
	if _, ok := metricColumns[metric]; !ok {
		return validatedSensorAlertRule{}, errors.New("unsupported metric")
	}
	direction := strings.TrimSpace(request.Direction)
	if direction != "above" && direction != "below" {
		return validatedSensorAlertRule{}, errors.New("direction must be above or below")
	}
	if request.TriggerThreshold == nil || request.ClearThreshold == nil ||
		math.IsNaN(valueOrZero(request.TriggerThreshold)) || math.IsInf(valueOrZero(request.TriggerThreshold), 0) ||
		math.IsNaN(valueOrZero(request.ClearThreshold)) || math.IsInf(valueOrZero(request.ClearThreshold), 0) {
		return validatedSensorAlertRule{}, errors.New("finite trigger_threshold and clear_threshold are required")
	}
	if direction == "above" && *request.ClearThreshold >= *request.TriggerThreshold {
		return validatedSensorAlertRule{}, errors.New("above rules require clear_threshold below trigger_threshold")
	}
	if direction == "below" && *request.ClearThreshold <= *request.TriggerThreshold {
		return validatedSensorAlertRule{}, errors.New("below rules require clear_threshold above trigger_threshold")
	}
	if request.ForDurationSeconds == nil || *request.ForDurationSeconds < 0 || *request.ForDurationSeconds > 30*24*60*60 {
		return validatedSensorAlertRule{}, errors.New("for_duration_seconds must be between 0 and 2592000")
	}
	if request.MaxDataAgeSeconds == nil || *request.MaxDataAgeSeconds < 1 || *request.MaxDataAgeSeconds > 365*24*60*60 {
		return validatedSensorAlertRule{}, errors.New("max_data_age_seconds must be between 1 and 31536000")
	}
	severity := strings.TrimSpace(request.Severity)
	if severity != "info" && severity != "warning" && severity != "critical" {
		return validatedSensorAlertRule{}, errors.New("severity must be info, warning, or critical")
	}
	if request.Enabled == nil {
		return validatedSensorAlertRule{}, errors.New("enabled is required")
	}
	return validatedSensorAlertRule{
		Name: name, MAC: mac, Metric: metric, Direction: direction,
		TriggerThreshold: *request.TriggerThreshold, ClearThreshold: *request.ClearThreshold,
		ForDurationSeconds: *request.ForDurationSeconds, MaxDataAgeSeconds: *request.MaxDataAgeSeconds,
		Severity: severity, Enabled: *request.Enabled,
	}, nil
}

func (api *apiServer) deviceExists(r *http.Request, mac string) (bool, error) {
	var exists bool
	err := api.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM devices WHERE mac = $1)`, mac).Scan(&exists)
	return exists, err
}

func parseAlertRuleID(w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid sensor alert rule id")
		return 0, false
	}
	return id, true
}

func valueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func pgTimestampPointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func pgFloatPointer(value pgtype.Float8) *float64 {
	if !value.Valid {
		return nil
	}
	result := value.Float64
	return &result
}
