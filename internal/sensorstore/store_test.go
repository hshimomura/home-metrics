package sensorstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"home-metrics/internal/sensor"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type fakeRow struct {
	value any
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	switch target := dest[0].(type) {
	case *string:
		*target = r.value.(string)
	case *pgtype.Text:
		if r.value == nil {
			*target = pgtype.Text{}
		} else {
			*target = pgtype.Text{String: r.value.(string), Valid: true}
		}
	default:
		panic("unsupported fake row destination")
	}
	return nil
}

type fakeDB struct {
	rows []pgx.Row
	sql  []string
	args [][]any
}

func (db *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	db.sql = append(db.sql, sql)
	db.args = append(db.args, args)
	row := db.rows[0]
	db.rows = db.rows[1:]
	return row
}

func (db *fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func TestUpsertMinuteUsesSparseMergeForEveryMetric(t *testing.T) {
	for _, column := range []string{
		"temperature_c", "humidity_percent", "battery_percent", "rssi_dbm",
		"pressure_hpa", "co2_ppm", "lux", "etvoc",
		"soil_moisture_percent", "conductivity_us_cm",
	} {
		want := column + " = COALESCE(EXCLUDED." + column + ", sensor_minute." + column + ")"
		if !strings.Contains(UpsertMinuteSQL, want) {
			t.Fatalf("upsert SQL does not preserve sparse %s", column)
		}
	}
}

func TestDeviceSyncSQLAllowsNullClaimButRejectsDifferentOwner(t *testing.T) {
	if !strings.Contains(syncDeviceSQL, "WHERE devices.ingest_source IS NULL") ||
		!strings.Contains(syncDeviceSQL, "OR devices.ingest_source = EXCLUDED.ingest_source") {
		t.Fatalf("device sync ownership predicate is missing")
	}
	if !strings.Contains(syncDeviceSQL, "CASE WHEN $8 THEN devices.label") ||
		!strings.Contains(syncDeviceSQL, "CASE WHEN $9 THEN devices.location") {
		t.Fatalf("device sync preserve predicates are missing")
	}
}

func TestSyncDeviceDerivesCategoryAndSyncsEnabled(t *testing.T) {
	db := &fakeDB{rows: []pgx.Row{
		fakeRow{value: "plant"},
		fakeRow{value: "cisco_sensor_connect"},
	}}
	err := SyncDevice(context.Background(), db, sensor.Device{
		MAC: "5c:85:7e:14:73:7d", Label: "Blueberry1",
		IngestSource: "cisco_sensor_connect", IngestSourceExplicit: true,
		SensorTypeCode: "xiaomi_flower_care", Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := db.args[1]
	if got := args[5]; got != "plant" {
		t.Fatalf("derived category = %#v", got)
	}
	if got := args[6]; got != false {
		t.Fatalf("enabled = %#v", got)
	}
}

func TestSyncDeviceRejectsUnknownTypeAndOwnershipConflict(t *testing.T) {
	unknown := &fakeDB{rows: []pgx.Row{fakeRow{err: pgx.ErrNoRows}}}
	err := SyncDevice(context.Background(), unknown, sensor.Device{
		MAC: "5c:85:7e:14:73:7d", Label: "sensor",
		IngestSource: "cisco_sensor_connect", IngestSourceExplicit: true,
		SensorTypeCode: "unknown", Enabled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "unknown sensor type") {
		t.Fatalf("unknown type error = %v", err)
	}

	conflict := &fakeDB{rows: []pgx.Row{fakeRow{err: pgx.ErrNoRows}}}
	err = SyncDevice(context.Background(), conflict, sensor.Device{
		MAC: "5c:85:7e:14:73:7d", Label: "sensor",
		IngestSource: "cisco_sensor_connect", IngestSourceExplicit: true,
		Enabled: true,
	})
	if !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("ownership error = %v", err)
	}
}
