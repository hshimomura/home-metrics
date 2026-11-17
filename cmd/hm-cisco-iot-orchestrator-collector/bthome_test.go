package main

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"home-metrics/internal/bledecode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestShellyCapturedAdvertisements(t *testing.T) {
	for _, tc := range []struct {
		hex     string
		motion  string
		battery bool
	}{
		{"440010016421006400", "clear", true},
		{"440011016421016400", "detected", true},
		{"44001401643a016400", "absent", true},
		{"44001101642102", "absent", false},
		{"440011016421", "absent", false},
		{"45001101642101", "absent", false},
		{"24001101642101", "absent", false},
		{"4400110164f01300f2140201", "absent", true},
	} {
		t.Run(tc.hex, func(t *testing.T) {
			b, _ := hex.DecodeString(tc.hex)
			r := bledecode.DecodeServiceData(b)
			if motionLabel(r.Motion) != tc.motion {
				t.Fatalf("motion=%s want %s", motionLabel(r.Motion), tc.motion)
			}
			if (r.BatteryPercent != nil) != tc.battery {
				t.Fatalf("battery=%v", r.BatteryPercent)
			}
			if tc.battery && *r.BatteryPercent != 100 {
				t.Fatalf("battery=%v", *r.BatteryPercent)
			}
			if r.Lux != nil || r.TemperatureC != nil {
				t.Fatal("unsupported measurement populated")
			}
		})
	}
	// A complete service-data AD structure must preserve motion and packet ID.
	b, _ := hex.DecodeString("0201060c16d2fc440011016421016400")
	r := decodeBLEPayload(b)
	if r.Motion == nil || !*r.Motion || r.PacketID == nil || *r.PacketID != 17 {
		t.Fatalf("advertisement not decoded: %+v", r)
	}
}

type motionTestDB struct {
	calls int
	err   error
}
type motionTestRow struct{ err error }

func (d *motionTestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (d *motionTestDB) QueryRow(context.Context, string, ...any) pgx.Row {
	d.calls++
	return motionTestRow{err: d.err}
}
func (r motionTestRow) Scan(args ...any) error {
	if r.err != nil {
		return r.err
	}
	*args[0].(*bool) = true
	return nil
}

func TestMotionFailedWritesAreRetried(t *testing.T) {
	d := &motionTestDB{err: errors.New("temporary database error")}
	c := collector{db: d}
	r := queuedReading(true, time.Now())
	if _, err := c.storeMotion(context.Background(), r); err == nil {
		t.Fatal("write failure hidden")
	}
	d.err = nil
	if _, err := c.storeMotion(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if d.calls != 2 {
		t.Fatal("failed observation not retried")
	}
}
