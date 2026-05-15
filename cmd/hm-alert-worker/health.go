package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"home-metrics/internal/adminwebhook"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	healthStatusFiring   = "firing"
	healthStatusResolved = "resolved"
)

type healthConfig struct {
	Enabled              bool
	CollectorStaleAfter  time.Duration
	DataStaleAfter       time.Duration
	SensorStaleAfter     time.Duration
	EnergyStaleAfter     time.Duration
	NotificationCooldown time.Duration
	BaseURL              string
}

type healthAlert struct {
	Key      string
	Status   string
	Severity string
	Title    string
	Source   string
	Summary  string
	Labels   map[string]string
}

type healthState struct {
	Status         string
	Severity       string
	FirstFiredAt   *time.Time
	LastNotifiedAt *time.Time
}

type healthNotificationResult struct {
	Status       string
	HTTPStatus   *int
	ResponseBody *string
	ErrorMessage *string
}

func (r healthNotificationResult) countsAsNotified() bool {
	return r.Status == "sent" || r.Status == "dry_run" || r.Status == "skipped"
}

type healthNotifier interface {
	Notify(ctx context.Context, alert healthAlert, eventID string, now time.Time, baseURL string) healthNotificationResult
	Mode() string
}

type dryRunHealthNotifier struct{}

func (dryRunHealthNotifier) Notify(context.Context, healthAlert, string, time.Time, string) healthNotificationResult {
	return healthNotificationResult{Status: "dry_run"}
}

func (dryRunHealthNotifier) Mode() string {
	return "health-dry-run"
}

type webhookHealthNotifier struct {
	client *adminwebhook.Client
}

func (n webhookHealthNotifier) Notify(ctx context.Context, alert healthAlert, eventID string, now time.Time, baseURL string) healthNotificationResult {
	if n.client == nil {
		msg := "admin webhook is not configured"
		return healthNotificationResult{Status: "skipped", ErrorMessage: &msg}
	}
	payload := adminwebhook.Payload{
		EventID:  eventID,
		Status:   alert.Status,
		Severity: alert.Severity,
		Title:    alert.Title,
		Source:   alert.Source,
		Summary:  alert.Summary,
		Labels:   alert.Labels,
		URL:      healthAlertURL(baseURL, alert.Key),
	}
	result, err := n.client.Send(ctx, payload)
	httpStatus := result.StatusCode
	responseBody := result.ResponseBody
	if err != nil {
		msg := err.Error()
		return healthNotificationResult{
			Status:       "failed",
			HTTPStatus:   &httpStatus,
			ResponseBody: nullableString(responseBody),
			ErrorMessage: &msg,
		}
	}
	return healthNotificationResult{
		Status:       "sent",
		HTTPStatus:   &httpStatus,
		ResponseBody: nullableString(responseBody),
	}
}

func (webhookHealthNotifier) Mode() string {
	return "health-webhook"
}

func newHealthConfigFromEnv() healthConfig {
	return healthConfig{
		Enabled:              envBool("HEALTH_EVALUATOR_ENABLED", false),
		CollectorStaleAfter:  envDuration("HEALTH_COLLECTOR_STALE_AFTER", 5*time.Minute),
		DataStaleAfter:       envDuration("HEALTH_DATA_STALE_AFTER", 30*time.Minute),
		SensorStaleAfter:     envDuration("HEALTH_SENSOR_STALE_AFTER", 30*time.Minute),
		EnergyStaleAfter:     envDuration("HEALTH_ENERGY_STALE_AFTER", 30*time.Minute),
		NotificationCooldown: envDuration("HEALTH_NOTIFICATION_COOLDOWN", time.Hour),
		BaseURL:              strings.TrimRight(strings.TrimSpace(os.Getenv("HOME_METRICS_BASE_URL")), "/"),
	}
}

func newHealthNotifierFromEnv(dryRun bool, httpClient *http.Client) (healthNotifier, error) {
	if dryRun {
		return dryRunHealthNotifier{}, nil
	}
	timeout := envDuration("WEBHOOK_RELAY_TIMEOUT", 10*time.Second)
	client, err := adminwebhook.New(os.Getenv("WEBHOOK_RELAY_URL"), os.Getenv("WEBHOOK_RELAY_TOKEN"), timeout, httpClient)
	if err != nil {
		return nil, err
	}
	return webhookHealthNotifier{client: client}, nil
}

func runHealthOnce(ctx context.Context, db *pgx.Conn, notifier healthNotifier, cfg healthConfig, now time.Time) error {
	if !cfg.Enabled {
		return nil
	}
	alerts, err := evaluateHealth(ctx, db, cfg, now)
	if err != nil {
		return err
	}
	for _, alert := range alerts {
		if err := applyHealthAlert(ctx, db, notifier, cfg, alert, now); err != nil {
			log.Printf("health alert key=%s: %v", alert.Key, err)
		}
	}
	return nil
}

func evaluateHealth(ctx context.Context, db *pgx.Conn, cfg healthConfig, now time.Time) ([]healthAlert, error) {
	alerts, err := evaluateCollectorHealth(ctx, db, cfg, now)
	if err != nil {
		return nil, err
	}
	deviceAlerts, err := evaluateSensorFreshness(ctx, db, cfg, now)
	if err != nil {
		return nil, err
	}
	alerts = append(alerts, deviceAlerts...)
	energyAlerts, err := evaluateEnergyFreshness(ctx, db, cfg, now)
	if err != nil {
		return nil, err
	}
	alerts = append(alerts, energyAlerts...)
	goneAlerts, err := resolveMissingHealthAlerts(ctx, db, alerts)
	if err != nil {
		return nil, err
	}
	alerts = append(alerts, goneAlerts...)
	return alerts, nil
}

func evaluateCollectorHealth(ctx context.Context, db *pgx.Conn, cfg healthConfig, now time.Time) ([]healthAlert, error) {
	rows, err := db.Query(ctx, `
		SELECT
			collector_name,
			target_type,
			target_key,
			last_success_at,
			last_data_at,
			COALESCE(last_error, ''),
			consecutive_failures,
			updated_at
		FROM collector_status
		ORDER BY collector_name, target_type, target_key
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []healthAlert
	for rows.Next() {
		var collectorName, targetType, targetKey, lastError string
		var lastSuccessAt, lastDataAt pgtype.Timestamptz
		var consecutiveFailures int
		var updatedAt time.Time
		if err := rows.Scan(
			&collectorName,
			&targetType,
			&targetKey,
			&lastSuccessAt,
			&lastDataAt,
			&lastError,
			&consecutiveFailures,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		key := healthKey("collector", collectorName, targetType, targetKey)
		labels := map[string]string{
			"kind":           "collector",
			"collector_name": collectorName,
			"target_type":    targetType,
			"target_key":     targetKey,
		}
		source := collectorName + "/" + targetKey
		if now.Sub(updatedAt) > cfg.CollectorStaleAfter {
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "critical",
				Title:    "Collector heartbeat stale",
				Source:   source,
				Summary:  fmt.Sprintf("%s has not updated collector_status for %s.", source, roundDuration(now.Sub(updatedAt))),
				Labels:   labels,
			})
			continue
		}
		if consecutiveFailures > 0 {
			summary := fmt.Sprintf("%s has %d consecutive failure(s).", source, consecutiveFailures)
			if strings.TrimSpace(lastError) != "" {
				summary += " Last error: " + trimForSummary(lastError)
			}
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "warning",
				Title:    "Collector failures detected",
				Source:   source,
				Summary:  summary,
				Labels:   labels,
			})
			continue
		}
		if lastDataAt.Valid && now.Sub(lastDataAt.Time) > cfg.DataStaleAfter {
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "warning",
				Title:    "Collector data stale",
				Source:   source,
				Summary:  fmt.Sprintf("%s is alive, but no data has been recorded for %s.", source, roundDuration(now.Sub(lastDataAt.Time))),
				Labels:   labels,
			})
			continue
		}
		if !lastDataAt.Valid && lastSuccessAt.Valid && now.Sub(lastSuccessAt.Time) > cfg.DataStaleAfter {
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "warning",
				Title:    "Collector has no data",
				Source:   source,
				Summary:  fmt.Sprintf("%s has not recorded data since it began reporting success.", source),
				Labels:   labels,
			})
			continue
		}
		alerts = append(alerts, healthAlert{
			Key:      key,
			Status:   healthStatusResolved,
			Severity: "info",
			Title:    "Collector recovered",
			Source:   source,
			Summary:  fmt.Sprintf("%s is reporting normally.", source),
			Labels:   labels,
		})
	}
	return alerts, rows.Err()
}

func evaluateSensorFreshness(ctx context.Context, db *pgx.Conn, cfg healthConfig, now time.Time) ([]healthAlert, error) {
	rows, err := db.Query(ctx, `
		SELECT d.mac, d.label, max(s.ts)
		FROM devices d
		LEFT JOIN sensor_minute s ON s.mac = d.mac
		WHERE d.enabled
		GROUP BY d.mac, d.label
		ORDER BY d.mac
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []healthAlert
	for rows.Next() {
		var mac, label string
		var latest pgtype.Timestamptz
		if err := rows.Scan(&mac, &label, &latest); err != nil {
			return nil, err
		}
		key := healthKey("metric", "sensor_minute", mac, "data")
		labels := map[string]string{"kind": "sensor", "mac": mac, "label": label}
		if !latest.Valid {
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "warning",
				Title:    "Sensor data missing",
				Source:   mac,
				Summary:  fmt.Sprintf("%s has no sensor_minute data.", labelOrKey(label, mac)),
				Labels:   labels,
			})
			continue
		}
		if now.Sub(latest.Time) > cfg.SensorStaleAfter {
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "warning",
				Title:    "Sensor data stale",
				Source:   mac,
				Summary:  fmt.Sprintf("%s sensor data is stale for %s.", labelOrKey(label, mac), roundDuration(now.Sub(latest.Time))),
				Labels:   labels,
			})
			continue
		}
		alerts = append(alerts, healthAlert{
			Key:      key,
			Status:   healthStatusResolved,
			Severity: "info",
			Title:    "Sensor data recovered",
			Source:   mac,
			Summary:  fmt.Sprintf("%s sensor data is fresh.", labelOrKey(label, mac)),
			Labels:   labels,
		})
	}
	return alerts, rows.Err()
}

func evaluateEnergyFreshness(ctx context.Context, db *pgx.Conn, cfg healthConfig, now time.Time) ([]healthAlert, error) {
	rows, err := db.Query(ctx, `
		SELECT d.source, d.device_key, d.label, m.metric, max(r.ts)
		FROM energy_devices d
		JOIN energy_metric_definitions m ON m.source = d.source AND m.enabled
		LEFT JOIN energy_readings r
			ON r.source = d.source
			AND r.device_key = d.device_key
			AND r.metric = m.metric
		WHERE d.enabled
		GROUP BY d.source, d.device_key, d.label, m.metric
		ORDER BY d.source, d.device_key, m.metric
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []healthAlert
	for rows.Next() {
		var source, deviceKey, label, metric string
		var latest pgtype.Timestamptz
		if err := rows.Scan(&source, &deviceKey, &label, &metric, &latest); err != nil {
			return nil, err
		}
		key := healthKey("metric", "energy_readings", source, deviceKey, metric)
		labels := map[string]string{
			"kind":       "energy",
			"source":     source,
			"device_key": deviceKey,
			"metric":     metric,
			"label":      label,
		}
		alertSource := source + "/" + deviceKey
		if !latest.Valid {
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "warning",
				Title:    "Energy data missing",
				Source:   alertSource,
				Summary:  fmt.Sprintf("%s has no %s readings.", labelOrKey(label, deviceKey), metric),
				Labels:   labels,
			})
			continue
		}
		if now.Sub(latest.Time) > cfg.EnergyStaleAfter {
			alerts = append(alerts, healthAlert{
				Key:      key,
				Status:   healthStatusFiring,
				Severity: "warning",
				Title:    "Energy data stale",
				Source:   alertSource,
				Summary:  fmt.Sprintf("%s %s readings are stale for %s.", labelOrKey(label, deviceKey), metric, roundDuration(now.Sub(latest.Time))),
				Labels:   labels,
			})
			continue
		}
		alerts = append(alerts, healthAlert{
			Key:      key,
			Status:   healthStatusResolved,
			Severity: "info",
			Title:    "Energy data recovered",
			Source:   alertSource,
			Summary:  fmt.Sprintf("%s %s readings are fresh.", labelOrKey(label, deviceKey), metric),
			Labels:   labels,
		})
	}
	return alerts, rows.Err()
}

func applyHealthAlert(ctx context.Context, db *pgx.Conn, notifier healthNotifier, cfg healthConfig, alert healthAlert, now time.Time) error {
	previous, err := loadHealthState(ctx, db, alert.Key)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	existed := err == nil
	if err := upsertHealthState(ctx, db, alert, previous, now); err != nil {
		return err
	}
	if !shouldNotifyHealthAlert(previous, existed, alert, cfg, now) {
		return nil
	}
	eventID := newHealthEventID(now, alert.Key, alert.Status)
	result := notifier.Notify(ctx, alert, eventID, now, cfg.BaseURL)
	if err := insertHealthNotificationEvent(ctx, db, alert, eventID, result); err != nil {
		return err
	}
	if result.countsAsNotified() {
		if err := markHealthNotified(ctx, db, alert.Key, now); err != nil {
			return err
		}
	}
	log.Printf("health %s alert=%s severity=%s status=%s", result.Status, alert.Key, alert.Severity, alert.Status)
	return nil
}

func loadHealthState(ctx context.Context, db *pgx.Conn, key string) (healthState, error) {
	var state healthState
	var firstFiredAt, lastNotifiedAt pgtype.Timestamptz
	err := db.QueryRow(ctx, `
		SELECT status, severity, first_fired_at, last_notified_at
		FROM health_alert_state
		WHERE alert_key = $1
	`, key).Scan(&state.Status, &state.Severity, &firstFiredAt, &lastNotifiedAt)
	if firstFiredAt.Valid {
		state.FirstFiredAt = &firstFiredAt.Time
	}
	if lastNotifiedAt.Valid {
		state.LastNotifiedAt = &lastNotifiedAt.Time
	}
	return state, err
}

func upsertHealthState(ctx context.Context, db *pgx.Conn, alert healthAlert, previous healthState, now time.Time) error {
	labels, err := json.Marshal(alert.Labels)
	if err != nil {
		return err
	}
	var firstFiredAt *time.Time
	var resolvedAt *time.Time
	if alert.Status == healthStatusFiring {
		if previous.Status == healthStatusFiring && previous.FirstFiredAt != nil {
			firstFiredAt = previous.FirstFiredAt
		} else {
			firstFiredAt = &now
		}
	} else {
		resolvedAt = &now
	}
	_, err = db.Exec(ctx, `
		INSERT INTO health_alert_state (
			alert_key,
			status,
			severity,
			title,
			source,
			summary,
			labels,
			first_fired_at,
			last_evaluated_at,
			resolved_at,
			updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $9)
		ON CONFLICT (alert_key) DO UPDATE SET
			status = EXCLUDED.status,
			severity = EXCLUDED.severity,
			title = EXCLUDED.title,
			source = EXCLUDED.source,
			summary = EXCLUDED.summary,
			labels = EXCLUDED.labels,
			first_fired_at = CASE
				WHEN EXCLUDED.status = 'firing' THEN EXCLUDED.first_fired_at
				ELSE health_alert_state.first_fired_at
			END,
			last_evaluated_at = EXCLUDED.last_evaluated_at,
			resolved_at = CASE
				WHEN EXCLUDED.status = 'resolved' THEN EXCLUDED.resolved_at
				ELSE NULL
			END,
			updated_at = EXCLUDED.updated_at
	`, alert.Key, alert.Status, alert.Severity, alert.Title, alert.Source, alert.Summary, string(labels), firstFiredAt, now, resolvedAt)
	return err
}

func shouldNotifyHealthAlert(previous healthState, existed bool, alert healthAlert, cfg healthConfig, now time.Time) bool {
	if alert.Status == healthStatusResolved {
		return existed && previous.Status == healthStatusFiring && previous.LastNotifiedAt != nil
	}
	if !existed || previous.Status != healthStatusFiring {
		return true
	}
	if severityRank(alert.Severity) > severityRank(previous.Severity) {
		return true
	}
	return previous.LastNotifiedAt == nil || now.Sub(*previous.LastNotifiedAt) >= cfg.NotificationCooldown
}

func insertHealthNotificationEvent(ctx context.Context, db *pgx.Conn, alert healthAlert, eventID string, result healthNotificationResult) error {
	_, err := db.Exec(ctx, `
		INSERT INTO health_notification_events (
			event_id,
			alert_key,
			channel_type,
			status,
			http_status,
			response_body,
			error
		)
		VALUES ($1, $2, 'generic_webhook', $3, $4, $5, $6)
	`, eventID, alert.Key, result.Status, result.HTTPStatus, result.ResponseBody, result.ErrorMessage)
	return err
}

func markHealthNotified(ctx context.Context, db *pgx.Conn, key string, now time.Time) error {
	_, err := db.Exec(ctx, `
		UPDATE health_alert_state
		SET last_notified_at = $2, updated_at = $2
		WHERE alert_key = $1
	`, key, now)
	return err
}

func resolveMissingHealthAlerts(ctx context.Context, db *pgx.Conn, current []healthAlert) ([]healthAlert, error) {
	currentKeys := map[string]bool{}
	for _, alert := range current {
		currentKeys[alert.Key] = true
	}
	rows, err := db.Query(ctx, `
		SELECT alert_key, severity, source, labels
		FROM health_alert_state
		WHERE status = 'firing'
		ORDER BY alert_key
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []healthAlert
	for rows.Next() {
		var key, severity, source string
		var labelsJSON []byte
		if err := rows.Scan(&key, &severity, &source, &labelsJSON); err != nil {
			return nil, err
		}
		if currentKeys[key] {
			continue
		}
		labels := map[string]string{}
		if len(labelsJSON) > 0 {
			if err := json.Unmarshal(labelsJSON, &labels); err != nil {
				return nil, err
			}
		}
		alerts = append(alerts, healthAlert{
			Key:      key,
			Status:   healthStatusResolved,
			Severity: "info",
			Title:    "Health target no longer monitored",
			Source:   source,
			Summary:  "The previously firing health target is no longer part of the active evaluation set.",
			Labels:   labels,
		})
		if severity == "critical" {
			alerts[len(alerts)-1].Summary = "The previously critical health target is no longer part of the active evaluation set."
		}
	}
	return alerts, rows.Err()
}

func healthKey(parts ...string) string {
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(strings.TrimSpace(part), ":", "_")
	}
	return strings.Join(parts, ":")
}

func newHealthEventID(now time.Time, key string, status string) string {
	hash := sha1.Sum([]byte(now.Format(time.RFC3339Nano) + ":" + key + ":" + status))
	return "hm-" + now.UTC().Format("20060102150405") + "-" + hex.EncodeToString(hash[:])[:10]
}

func healthAlertURL(baseURL string, key string) string {
	if baseURL == "" {
		return ""
	}
	return baseURL + "/admin/health-alerts/" + key
}

func nullableString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

func roundDuration(value time.Duration) time.Duration {
	if value < time.Minute {
		return value.Round(time.Second)
	}
	return value.Round(time.Minute)
}

func trimForSummary(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 240 {
		return value
	}
	return value[:240] + "..."
}

func labelOrKey(label string, key string) string {
	if strings.TrimSpace(label) != "" {
		return label
	}
	return key
}
