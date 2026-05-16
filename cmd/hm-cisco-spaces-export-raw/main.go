package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"

type exportRow struct {
	ReceivedAt      time.Time       `json:"received_at"`
	ID              int64           `json:"id"`
	RecordUID       *string         `json:"record_uid,omitempty"`
	RecordTimestamp *time.Time      `json:"record_timestamp,omitempty"`
	EventType       *string         `json:"event_type,omitempty"`
	DeviceMAC       *string         `json:"device_mac,omitempty"`
	DeviceID        *string         `json:"device_id,omitempty"`
	DeviceLabel     *string         `json:"device_label,omitempty"`
	LocationID      *string         `json:"location_id,omitempty"`
	MapID           *string         `json:"map_id,omitempty"`
	PayloadSHA256   string          `json:"payload_sha256"`
	Payload         json.RawMessage `json:"payload"`
}

func main() {
	var (
		fromRaw   = flag.String("from", "", "start time, RFC3339; defaults to one hour ago")
		toRaw     = flag.String("to", "", "end time, RFC3339; defaults to now")
		macRaw    = flag.String("mac", "", "optional device MAC filter")
		recordUID = flag.String("record-uid", "", "optional recordUid filter")
		outPath   = flag.String("out", "-", "output path, or - for stdout")
		redact    = flag.Bool("redact", false, "redact tenant and location labels in payload")
		dbDSN     = flag.String("db-dsn", envString("BLE_DB_DSN", defaultDBDSN), "PostgreSQL DSN")
	)
	flag.Parse()

	to, err := parseOptionalTime(*toRaw, time.Now().UTC())
	if err != nil {
		log.Fatalf("parse --to: %v", err)
	}
	fromDefault := to.Add(-time.Hour)
	from, err := parseOptionalTime(*fromRaw, fromDefault)
	if err != nil {
		log.Fatalf("parse --from: %v", err)
	}
	if !from.Before(to) {
		log.Fatal("--from must be before --to")
	}
	mac := ""
	if strings.TrimSpace(*macRaw) != "" {
		mac = normalizeMAC(*macRaw)
		if mac == "" {
			log.Fatalf("invalid --mac=%q", *macRaw)
		}
	}

	var out io.WriteCloser
	if *outPath == "-" {
		out = nopWriteCloser{Writer: os.Stdout}
	} else {
		file, err := os.Create(*outPath)
		if err != nil {
			log.Fatalf("create %s: %v", *outPath, err)
		}
		out = file
	}
	defer out.Close()

	ctx := context.Background()
	db, err := pgx.Connect(ctx, *dbDSN)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())

	count, err := exportRawEvents(ctx, db, out, from, to, mac, strings.TrimSpace(*recordUID), *redact)
	if err != nil {
		log.Fatalf("export raw events: %v", err)
	}
	log.Printf("exported Cisco Spaces raw events rows=%d from=%s to=%s mac=%s out=%s", count, from.Format(time.RFC3339), to.Format(time.RFC3339), mac, *outPath)
}

func exportRawEvents(ctx context.Context, db *pgx.Conn, out io.Writer, from time.Time, to time.Time, mac string, recordUID string, redact bool) (int, error) {
	conditions := []string{"received_at >= $1", "received_at < $2"}
	args := []any{from, to}
	if mac != "" {
		args = append(args, mac)
		conditions = append(conditions, fmt.Sprintf("device_mac = $%d", len(args)))
	}
	if recordUID != "" {
		args = append(args, recordUID)
		conditions = append(conditions, fmt.Sprintf("record_uid = $%d", len(args)))
	}
	query := `
		SELECT
			received_at,
			id,
			record_uid,
			record_timestamp,
			event_type,
			device_mac,
			device_id,
			device_label,
			location_id,
			map_id,
			payload_sha256,
			payload
		FROM cisco_spaces_raw_events
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY received_at, id`

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	encoder := json.NewEncoder(out)
	count := 0
	for rows.Next() {
		var row exportRow
		if err := rows.Scan(
			&row.ReceivedAt,
			&row.ID,
			&row.RecordUID,
			&row.RecordTimestamp,
			&row.EventType,
			&row.DeviceMAC,
			&row.DeviceID,
			&row.DeviceLabel,
			&row.LocationID,
			&row.MapID,
			&row.PayloadSHA256,
			&row.Payload,
		); err != nil {
			return count, err
		}
		if redact {
			payload, err := redactPayload(row.Payload)
			if err != nil {
				return count, err
			}
			row.Payload = payload
			if row.DeviceLabel != nil {
				redacted := "[REDACTED]"
				row.DeviceLabel = &redacted
			}
		}
		if err := encoder.Encode(row); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

func redactPayload(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	redactValue(value)
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func redactValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if redactKey(key) {
				typed[key] = "[REDACTED]"
				continue
			}
			redactValue(nested)
		}
	case []any:
		for _, nested := range typed {
			redactValue(nested)
		}
	}
}

func redactKey(key string) bool {
	switch strings.ToLower(key) {
	case "spacestenantid", "spacestenantname", "partnertenantid", "partnertenantname",
		"sourcelocationid", "label", "devicename":
		return true
	default:
		return false
	}
}

func parseOptionalTime(raw string, fallback time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func normalizeMAC(value string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(value) {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			b.WriteRune(ch)
		}
	}
	raw := b.String()
	if len(raw) != 12 {
		return ""
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, raw[i:i+2])
	}
	return strings.Join(parts, ":")
}

func envString(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}
