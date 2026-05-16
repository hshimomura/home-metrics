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

func TestRetainHealthNotificationEventHistoryDeletesOldRows(t *testing.T) {
	db := &fakeMaintExecer{}
	retain := 7 * 24 * time.Hour
	if err := retainHealthNotificationEventHistory(context.Background(), db, retain); err != nil {
		t.Fatalf("retainHealthNotificationEventHistory returned error: %v", err)
	}
	if !strings.Contains(db.sql, "DELETE FROM health_notification_events") {
		t.Fatalf("unexpected SQL: %s", db.sql)
	}
	if !strings.Contains(db.sql, "created_at < now() - $1::interval") {
		t.Fatalf("SQL must delete by created_at retention: %s", db.sql)
	}
	if len(db.args) != 1 || db.args[0] != intervalSeconds(retain) {
		t.Fatalf("args = %#v, want retain interval", db.args)
	}
}
