package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func insertCiscoSpacesRawEvent(ctx context.Context, db *pgx.Conn, event firehoseEvent, raw []byte) error {
	meta := extractRawEventMetadata(event, raw)
	_, err := db.Exec(ctx, `
		INSERT INTO cisco_spaces_raw_events (
			record_uid,
			record_timestamp,
			event_type,
			device_mac,
			device_id,
			device_label,
			location_id,
			map_id,
			payload,
			payload_sha256
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)
	`, nullableString(meta.RecordUID, meta.RecordUID != ""),
		meta.RecordTS,
		nullableString(meta.EventType, meta.EventType != ""),
		nullableString(meta.DeviceMAC, meta.DeviceMAC != ""),
		nullableString(meta.DeviceID, meta.DeviceID != ""),
		nullableString(meta.DeviceLabel, meta.DeviceLabel != ""),
		nullableString(meta.LocationID, meta.LocationID != ""),
		nullableString(meta.MapID, meta.MapID != ""),
		string(raw),
		meta.PayloadSHA256,
	)
	return err
}

func extractRawEventMetadata(event firehoseEvent, raw []byte) rawEventMetadata {
	sum := sha256.Sum256(raw)
	meta := rawEventMetadata{
		RecordUID:     strings.TrimSpace(event.RecordUID),
		EventType:     strings.TrimSpace(event.EventType),
		DeviceMAC:     normalizeMAC(event.IOTTelemetry.DeviceInfo.DeviceMACAddress),
		DeviceID:      strings.TrimSpace(event.IOTTelemetry.DeviceInfo.DeviceID),
		DeviceLabel:   strings.TrimSpace(event.IOTTelemetry.DeviceInfo.Label),
		PayloadSHA256: hex.EncodeToString(sum[:]),
	}
	if event.RecordTS > 0 {
		ts := time.UnixMilli(event.RecordTS).UTC()
		meta.RecordTS = &ts
	}
	if event.IOTTelemetry.Location != nil {
		meta.LocationID = strings.TrimSpace(event.IOTTelemetry.Location.LocationID)
	}
	if event.IOTTelemetry.DetectedPosition != nil {
		if meta.LocationID == "" {
			meta.LocationID = strings.TrimSpace(event.IOTTelemetry.DetectedPosition.LocationID)
		}
		meta.MapID = strings.TrimSpace(event.IOTTelemetry.DetectedPosition.MapID)
	}
	return meta
}
