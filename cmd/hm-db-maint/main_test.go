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
