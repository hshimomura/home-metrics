package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeMaintExecer struct {
	sql  string
	args []any
}

func (f *fakeMaintExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sql = sql
	f.args = args
	return pgconn.NewCommandTag("DELETE 2"), nil
}

func TestRetainSensorMinuteDeletesOldRows(t *testing.T) {
	db := &fakeMaintExecer{}
	retain := 14 * 24 * time.Hour
	if err := retainSensorMinute(context.Background(), db, retain); err != nil {
		t.Fatalf("retainSensorMinute returned error: %v", err)
	}
	if !strings.Contains(db.sql, "DELETE FROM sensor_minute") {
		t.Fatalf("unexpected SQL: %s", db.sql)
	}
	if !strings.Contains(db.sql, "ts < now() - $1::interval") {
		t.Fatalf("SQL must delete by ts retention: %s", db.sql)
	}
	if len(db.args) != 1 || db.args[0] != intervalSeconds(retain) {
		t.Fatalf("args = %#v, want retain interval", db.args)
	}
}

func TestRetainSensorAlertEventsDeletesOldRows(t *testing.T) {
	db := &fakeMaintExecer{}
	retain := 90 * 24 * time.Hour
	if err := retainSensorAlertEvents(context.Background(), db, retain); err != nil {
		t.Fatalf("retainSensorAlertEvents returned error: %v", err)
	}
	if !strings.Contains(db.sql, "DELETE FROM sensor_alert_events") {
		t.Fatalf("unexpected SQL: %s", db.sql)
	}
	if !strings.Contains(db.sql, "occurred_at < now() - $1::interval") {
		t.Fatalf("SQL must delete by event retention: %s", db.sql)
	}
}

func TestBuildMinuteRollupSQLIncludesMetricCountsAndCutoff(t *testing.T) {
	sql := buildRollupSQL("sensor_1hour", "sensor_minute", false)
	for _, fragment := range []string{
		"avg(temperature_c)",
		"count(temperature_c)",
		"temperature_c_count = EXCLUDED.temperature_c_count",
		"GREATEST(now() - $2::interval, $3::timestamptz)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("minute rollup SQL missing %q: %s", fragment, sql)
		}
	}
}

func TestBuildWeightedRollupSQLUsesMetricSpecificCount(t *testing.T) {
	sql := buildRollupSQL("sensor_1day", "sensor_1hour", true)
	for _, fragment := range []string{
		"sum(temperature_c * temperature_c_count)",
		"sum(temperature_c_count)",
		"sum(battery_percent * battery_percent_count)",
		"sum(battery_percent_count)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("weighted rollup SQL missing %q: %s", fragment, sql)
		}
	}
}
