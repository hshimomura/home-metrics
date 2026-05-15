package main

import (
	"strings"
	"testing"
	"time"
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
