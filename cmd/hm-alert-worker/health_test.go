package main

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestHealthKeySanitizesSegments(t *testing.T) {
	got := healthKey("metric", "sensor_minute", "00:fa:b6:07:de:49", "data")
	want := "metric:sensor_minute:00_fa_b6_07_de_49:data"
	if got != want {
		t.Fatalf("healthKey = %q, want %q", got, want)
	}
}

func TestNewHealthEventIDIsStableShape(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 11, 12, 0, time.UTC)
	got := newHealthEventID(now, "collector:hm-test:default", "firing")
	if !strings.HasPrefix(got, "hm-20260515101112-") {
		t.Fatalf("event id = %q, want timestamp prefix", got)
	}
	if len(got) != len("hm-20260515101112-1234567890") {
		t.Fatalf("event id length = %d", len(got))
	}
}

func TestShouldNotifyHealthAlert(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cfg := healthConfig{NotificationCooldown: time.Hour}
	alert := healthAlert{Status: healthStatusFiring, Severity: "warning"}
	if !shouldNotifyHealthAlert(healthState{}, false, alert, cfg, now) {
		t.Fatal("new firing alert should notify")
	}
	recent := now.Add(-10 * time.Minute)
	if shouldNotifyHealthAlert(healthState{Status: healthStatusFiring, Severity: "warning", LastNotifiedAt: &recent}, true, alert, cfg, now) {
		t.Fatal("same firing alert inside cooldown should not notify")
	}
	escalated := healthAlert{Status: healthStatusFiring, Severity: "critical"}
	if !shouldNotifyHealthAlert(healthState{Status: healthStatusFiring, Severity: "warning", LastNotifiedAt: &recent}, true, escalated, cfg, now) {
		t.Fatal("severity escalation should notify")
	}
	resolved := healthAlert{Status: healthStatusResolved, Severity: "info"}
	if !shouldNotifyHealthAlert(healthState{Status: healthStatusFiring, Severity: "warning", LastNotifiedAt: &recent}, true, resolved, cfg, now) {
		t.Fatal("resolved alert after notification should notify")
	}
	if shouldNotifyHealthAlert(healthState{Status: healthStatusFiring, Severity: "warning"}, true, resolved, cfg, now) {
		t.Fatal("resolved alert without prior notification should not notify")
	}
}

func TestFailedHealthDeliveryDoesNotCountAsNotified(t *testing.T) {
	result := healthNotificationResult{Status: "failed"}
	if result.countsAsNotified() {
		t.Fatal("failed delivery should remain retryable")
	}
	for _, status := range []string{"sent", "dry_run", "skipped"} {
		if !(healthNotificationResult{Status: status}).countsAsNotified() {
			t.Fatalf("%s should count as notified", status)
		}
	}
}

func TestCollectorThresholdDefaults(t *testing.T) {
	for _, name := range []string{
		"HEALTH_COLLECTOR_STALE_AFTER",
		"HEALTH_DATA_STALE_AFTER",
		"HEALTH_COLLECTOR_HEARTBEAT_WARNING_AFTER",
		"HEALTH_COLLECTOR_HEARTBEAT_CRITICAL_AFTER",
		"HEALTH_COLLECTOR_DATA_WARNING_AFTER",
		"HEALTH_COLLECTOR_DATA_CRITICAL_AFTER",
		"HEALTH_ECHONET_HEARTBEAT_WARNING_AFTER",
		"HEALTH_ECHONET_HEARTBEAT_CRITICAL_AFTER",
		"HEALTH_ECHONET_DATA_WARNING_AFTER",
		"HEALTH_ECHONET_DATA_CRITICAL_AFTER",
		"HEALTH_CISCO_SPACES_HEARTBEAT_WARNING_AFTER",
		"HEALTH_CISCO_SPACES_HEARTBEAT_CRITICAL_AFTER",
		"HEALTH_CISCO_SPACES_DATA_WARNING_AFTER",
		"HEALTH_CISCO_SPACES_DATA_CRITICAL_AFTER",
	} {
		t.Setenv(name, "")
	}
	cfg := newHealthConfigFromEnv()
	echonet := collectorThresholds(cfg, "hm-echonet-collector")
	if echonet.HeartbeatWarningAfter != 5*time.Minute || echonet.HeartbeatCriticalAfter != 15*time.Minute {
		t.Fatalf("echonet heartbeat thresholds = %s/%s", echonet.HeartbeatWarningAfter, echonet.HeartbeatCriticalAfter)
	}
	if echonet.DataWarningAfter != 15*time.Minute || echonet.DataCriticalAfter != 30*time.Minute {
		t.Fatalf("echonet data thresholds = %s/%s", echonet.DataWarningAfter, echonet.DataCriticalAfter)
	}
	cisco := collectorThresholds(cfg, "hm-cisco-spaces-collector")
	if cisco.HeartbeatWarningAfter != 5*time.Minute || cisco.DataWarningAfter != 15*time.Minute {
		t.Fatalf("cisco thresholds = heartbeat %s data %s", cisco.HeartbeatWarningAfter, cisco.DataWarningAfter)
	}
	defaults := collectorThresholds(cfg, "hm-other")
	if defaults.DataWarningAfter != 15*time.Minute || defaults.DataCriticalAfter != 30*time.Minute {
		t.Fatalf("default data thresholds = %s/%s", defaults.DataWarningAfter, defaults.DataCriticalAfter)
	}
}

func TestCollectorHeartbeatAgeUsesLastSuccess(t *testing.T) {
	now := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-30 * time.Second)
	lastSuccessAt := pgtype.Timestamptz{Time: now.Add(-6 * time.Minute), Valid: true}
	age, summary := collectorHeartbeatAge(now, updatedAt, lastSuccessAt, pgtype.Timestamptz{}, "hm-test/default")
	if age != 6*time.Minute {
		t.Fatalf("heartbeat age = %s", age)
	}
	if !strings.Contains(summary, "has not reported collector success") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestCollectorHeartbeatAgeUsesFirstFailureWhenNeverSuccessful(t *testing.T) {
	now := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-30 * time.Second)
	firstFailureAt := pgtype.Timestamptz{Time: now.Add(-16 * time.Minute), Valid: true}
	age, summary := collectorHeartbeatAge(now, updatedAt, pgtype.Timestamptz{}, firstFailureAt, "hm-test/default")
	if age != 16*time.Minute {
		t.Fatalf("heartbeat age = %s", age)
	}
	if !strings.Contains(summary, "has not reported collector success") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestEvaluateCollectorHealthRowIgnoresFreshDataWithFailures(t *testing.T) {
	now := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC)
	alert := evaluateCollectorHealthRow(testHealthConfig(), collectorHealthRow{
		CollectorName:       "hm-echonet-collector",
		TargetType:          "echonet_device",
		TargetKey:           "solar",
		LastSuccessAt:       pgtype.Timestamptz{Time: now.Add(-1 * time.Minute), Valid: true},
		LastDataAt:          pgtype.Timestamptz{Time: now.Add(-1 * time.Minute), Valid: true},
		FirstFailureAt:      pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		LastFailureAt:       pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		LastError:           "timeout waiting echonet response",
		ConsecutiveFailures: 2,
		UpdatedAt:           now.Add(-30 * time.Second),
	}, now)
	if alert.Status != healthStatusResolved {
		t.Fatalf("status = %s summary=%s", alert.Status, alert.Summary)
	}
	if alert.Impact["consecutive_failures"] != 2 {
		t.Fatalf("impact should preserve failure context: %#v", alert.Impact)
	}
}

func TestEvaluateCollectorHealthRowFiresOnDataStale(t *testing.T) {
	now := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC)
	alert := evaluateCollectorHealthRow(testHealthConfig(), collectorHealthRow{
		CollectorName:       "hm-echonet-collector",
		TargetType:          "echonet_device",
		TargetKey:           "solar",
		LastSuccessAt:       pgtype.Timestamptz{Time: now.Add(-1 * time.Minute), Valid: true},
		LastDataAt:          pgtype.Timestamptz{Time: now.Add(-16 * time.Minute), Valid: true},
		LastFailureAt:       pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		LastError:           "timeout waiting echonet response",
		ConsecutiveFailures: 2,
		UpdatedAt:           now.Add(-30 * time.Second),
	}, now)
	if alert.Status != healthStatusFiring || alert.Title != "Collector data stale" || alert.Severity != "warning" {
		t.Fatalf("alert = %#v", alert)
	}
	if !strings.Contains(alert.Summary, "Recent context: 2 consecutive failure") {
		t.Fatalf("summary missing failure context: %q", alert.Summary)
	}
}

func TestEvaluateCollectorHealthRowFiresAtDataWarningBoundary(t *testing.T) {
	now := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC)
	alert := evaluateCollectorHealthRow(testHealthConfig(), collectorHealthRow{
		CollectorName: "hm-echonet-collector",
		TargetType:    "echonet_device",
		TargetKey:     "solar",
		LastSuccessAt: pgtype.Timestamptz{Time: now.Add(-1 * time.Minute), Valid: true},
		LastDataAt:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		UpdatedAt:     now.Add(-30 * time.Second),
	}, now)
	if alert.Status != healthStatusFiring || alert.Title != "Collector data stale" || alert.Severity != "warning" {
		t.Fatalf("alert = %#v", alert)
	}
}

func TestEvaluateCollectorHealthRowCriticalAtDataCriticalBoundary(t *testing.T) {
	now := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC)
	alert := evaluateCollectorHealthRow(testHealthConfig(), collectorHealthRow{
		CollectorName: "hm-echonet-collector",
		TargetType:    "echonet_device",
		TargetKey:     "solar",
		LastSuccessAt: pgtype.Timestamptz{Time: now.Add(-1 * time.Minute), Valid: true},
		LastDataAt:    pgtype.Timestamptz{Time: now.Add(-30 * time.Minute), Valid: true},
		UpdatedAt:     now.Add(-30 * time.Second),
	}, now)
	if alert.Status != healthStatusFiring || alert.Title != "Collector data stale" || alert.Severity != "critical" {
		t.Fatalf("alert = %#v", alert)
	}
}

func TestEvaluateCollectorHealthRowFiresOnNeverSuccessFailureDuration(t *testing.T) {
	now := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC)
	alert := evaluateCollectorHealthRow(testHealthConfig(), collectorHealthRow{
		CollectorName:       "hm-echonet-collector",
		TargetType:          "echonet_device",
		TargetKey:           "solar",
		FirstFailureAt:      pgtype.Timestamptz{Time: now.Add(-16 * time.Minute), Valid: true},
		LastFailureAt:       pgtype.Timestamptz{Time: now.Add(-1 * time.Minute), Valid: true},
		LastError:           "timeout waiting echonet response",
		ConsecutiveFailures: 16,
		UpdatedAt:           now.Add(-30 * time.Second),
	}, now)
	if alert.Status != healthStatusFiring || alert.Title != "Collector heartbeat stale" || alert.Severity != "critical" {
		t.Fatalf("alert = %#v", alert)
	}
	if !strings.Contains(alert.Summary, "has not reported collector success for 16m") {
		t.Fatalf("summary = %q", alert.Summary)
	}
}

func testHealthConfig() healthConfig {
	defaults := collectorHealthThresholds{
		HeartbeatWarningAfter:  5 * time.Minute,
		HeartbeatCriticalAfter: 15 * time.Minute,
		DataWarningAfter:       15 * time.Minute,
		DataCriticalAfter:      30 * time.Minute,
	}
	return healthConfig{
		DefaultThresholds: defaults,
		CollectorOverrides: map[string]collectorHealthThresholds{
			"hm-echonet-collector": defaults,
		},
	}
}

func TestAppendFailureContext(t *testing.T) {
	got := appendFailureContext("data is stale.", 2, "timeout waiting echonet response")
	if !strings.Contains(got, "2 consecutive failure") || !strings.Contains(got, "timeout waiting echonet response") {
		t.Fatalf("summary = %q", got)
	}
	if got := appendFailureContext("data is stale.", 0, "timeout"); got != "data is stale." {
		t.Fatalf("summary without failures = %q", got)
	}
}
