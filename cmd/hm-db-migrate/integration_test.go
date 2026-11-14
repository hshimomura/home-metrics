package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// HM_TEST_DATABASE_URL must point at an isolated TimescaleDB test server. Each
// test owns a uniquely named database; no existing database is reset or dropped.
func testDatabase(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("HM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HM_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("hm_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	db, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(ctx); admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); admin.Close(ctx) })
	return db
}
func TestDatabaseFreshAndUpgrade(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprint("snapshot=", fresh), func(t *testing.T) {
			ctx := context.Background()
			db := testDatabase(t)
			if fresh {
				schema, err := os.ReadFile(filepath.Join("..", "..", "db", "schema.sql"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(ctx, string(schema)); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join("..", "..", "db", "migrations")
			if err := runMigrations(ctx, db, dir); err != nil {
				t.Fatal(err)
			}
			if err := runMigrations(ctx, db, dir); err != nil {
				t.Fatal("repeat migration:", err)
			}
			var count int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 3 {
				t.Fatalf("ledger count=%d err=%v", count, err)
			}
			if _, err := db.Exec(ctx, `INSERT INTO devices(mac,label,location,ingest_source,sensor_type_code,sensor_category) VALUES('fc:4d:6a:26:53:e3','test','test','cisco_sensor_connect','shelly_blu_motion_zb','motion')`); err != nil {
				t.Fatal(err)
			}
			start := time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)
			write := func(at time.Time, state any, battery any, id int) bool {
				t.Helper()
				var changed bool
				err := db.QueryRow(ctx, `SELECT record_sensor_motion($1,$2,$3,$4,$5,$6,$7)`, "fc:4d:6a:26:53:e3", at, at, state, battery, id, "ap").Scan(&changed)
				if err != nil {
					t.Fatal(err)
				}
				return changed
			}
			if !write(start, false, 100, 255) {
				t.Fatal("missing initial state")
			}
			if !write(start.Add(time.Second), true, 99, 0) {
				t.Fatal("missing transition across packet-ID wrap")
			}
			if write(start.Add(time.Second), true, 99, 0) {
				t.Fatal("retry duplicated transition")
			}
			if write(start.Add(-time.Second), false, 90, 254) {
				t.Fatal("late state rolled back current state")
			}
			if !write(start.Add(2*time.Second), false, 98, 1) {
				t.Fatal("missing clear")
			}
			if write(start.Add(time.Minute), nil, 97, 2) {
				t.Fatal("battery-only created event")
			}
			var detections, events int
			if err := db.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE motion AND NOT initial) FROM sensor_motion_events`).Scan(&events, &detections); err != nil || events != 3 || detections != 1 {
				t.Fatalf("events=%d detection=%d err=%v", events, detections, err)
			}
			var battery float64
			if err := db.QueryRow(ctx, `SELECT battery_percent FROM sensor_minute WHERE ts=$1`, start).Scan(&battery); err != nil || battery != 98 {
				t.Fatalf("battery=%v err=%v", battery, err)
			}
			// Losing a successful response and retrying after later state must preserve
			// both the event count and the newer battery within the same minute.
			write(start.Add(time.Second), true, 99, 0)
			if err := db.QueryRow(ctx, `SELECT battery_percent FROM sensor_minute WHERE ts=$1`, start).Scan(&battery); err != nil || battery != 98 {
				t.Fatalf("retry overwrote battery=%v err=%v", battery, err)
			}
		})
	}
}
func TestDatabaseConcurrentMigrations(t *testing.T) {
	db := testDatabase(t)
	ctx := context.Background()
	second, err := pgx.ConnectConfig(ctx, db.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(ctx)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, conn := range []*pgx.Conn{db, second} {
		wg.Add(1)
		go func(conn *pgx.Conn) {
			defer wg.Done()
			errs <- runMigrations(ctx, conn, filepath.Join("..", "..", "db", "migrations"))
		}(conn)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDatabaseExistingMotionUpgrade(t *testing.T) {
	ctx := context.Background()
	db := testDatabase(t)
	dir := filepath.Join("..", "..", "db", "migrations")
	migrations, err := loadMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = ensureMigrationTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if err = applyMigration(ctx, db, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(ctx, `INSERT INTO devices(mac,label,location) VALUES('fc:4d:6a:26:53:e3','existing','existing'); SELECT record_sensor_motion('fc:4d:6a:26:53:e3',now()-interval '2 minutes',now(),false,100,255,'ap1'); SELECT record_sensor_motion('fc:4d:6a:26:53:e3',now()-interval '1 minute',now(),true,100,0,'ap2')`); err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM sensor_motion_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("history changed count=%d err=%v", count, err)
	}
	// Simultaneous duplicate AP reports serialize on the per-device state row.
	at := time.Now().UTC()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pgx.ConnectConfig(ctx, db.Config().Copy())
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close(ctx)
			_, err = conn.Exec(ctx, `SELECT record_sensor_motion($1,$2,$2,false,99,1,'ap')`, "fc:4d:6a:26:53:e3", at)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM sensor_motion_events`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("duplicate AP event count=%d err=%v", count, err)
	}
}
