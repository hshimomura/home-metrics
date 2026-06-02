package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func writeReading(ctx context.Context, db *pgx.Conn, reading sensorReading) error {
	if err := upsertDevice(ctx, db, reading); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `
		INSERT INTO sensor_minute (
			ts,
			mac,
			temperature_c,
			humidity_percent,
			battery_percent,
			pressure_hpa,
			co2_ppm,
			lux,
			etvoc
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (ts, mac) DO UPDATE SET
			temperature_c = COALESCE(EXCLUDED.temperature_c, sensor_minute.temperature_c),
			humidity_percent = COALESCE(EXCLUDED.humidity_percent, sensor_minute.humidity_percent),
			battery_percent = COALESCE(EXCLUDED.battery_percent, sensor_minute.battery_percent),
			pressure_hpa = COALESCE(EXCLUDED.pressure_hpa, sensor_minute.pressure_hpa),
			co2_ppm = COALESCE(EXCLUDED.co2_ppm, sensor_minute.co2_ppm),
			lux = COALESCE(EXCLUDED.lux, sensor_minute.lux),
			etvoc = COALESCE(EXCLUDED.etvoc, sensor_minute.etvoc),
			inserted_at = now()
	`, reading.TS,
		reading.MAC,
		nullablePtr(reading.TemperatureC),
		nullablePtr(reading.HumidityPercent),
		nullablePtr(reading.BatteryPercent),
		nullablePtr(reading.PressureHPa),
		nullablePtr(reading.CO2PPM),
		nullablePtr(reading.Lux),
		nullablePtr(reading.ETVOC),
	)
	return err
}

func pruneConfiguredBLESensors(ctx context.Context, db *pgx.Conn, path string) error {
	macs, err := loadConfiguredSensorMACs(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("configured BLE sensors file not found, skip prune path=%s", path)
			return nil
		}
		return err
	}
	if len(macs) == 0 {
		return nil
	}

	var deleted int64
	var disabled int64
	for _, mac := range macs {
		tag, err := db.Exec(ctx, `
			DELETE FROM devices d
			WHERE d.mac = $1
				AND COALESCE(d.sensor_category, '') <> 'Cisco Spaces'
				AND NOT EXISTS (SELECT 1 FROM sensor_minute s WHERE s.mac = d.mac)
				AND NOT EXISTS (SELECT 1 FROM sensor_1hour s WHERE s.mac = d.mac)
				AND NOT EXISTS (SELECT 1 FROM sensor_12hour s WHERE s.mac = d.mac)
				AND NOT EXISTS (SELECT 1 FROM sensor_1day s WHERE s.mac = d.mac)
		`, mac)
		if err != nil {
			return fmt.Errorf("delete configured BLE sensor %s: %w", mac, err)
		}
		if tag.RowsAffected() > 0 {
			deleted += tag.RowsAffected()
			continue
		}

		tag, err = db.Exec(ctx, `
			UPDATE devices
			SET enabled = false,
				updated_at = now()
			WHERE mac = $1
				AND enabled
				AND COALESCE(sensor_category, '') <> 'Cisco Spaces'
		`, mac)
		if err != nil {
			return fmt.Errorf("disable configured BLE sensor %s: %w", mac, err)
		}
		disabled += tag.RowsAffected()
	}
	if deleted > 0 || disabled > 0 {
		log.Printf("pruned configured BLE sensors deleted=%d disabled=%d source=%s", deleted, disabled, path)
	}
	return nil
}

func loadConfiguredSensorMACs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var config targetConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	var macs []string
	for _, device := range config.Devices {
		mac := normalizeMAC(device.MAC)
		if mac == "" || seen[mac] {
			continue
		}
		seen[mac] = true
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	return macs, nil
}

func upsertDevice(ctx context.Context, db *pgx.Conn, reading sensorReading) error {
	label := strings.TrimSpace(reading.Label)
	hasLabel := validDeviceLabel(reading.MAC, label)
	if !hasLabel {
		label = reading.MAC
	}
	_, err := db.Exec(ctx, `
		INSERT INTO devices (mac, label, sensor_category, location)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (mac) DO UPDATE SET
			label = CASE WHEN $5 THEN EXCLUDED.label ELSE devices.label END,
			sensor_category = COALESCE(devices.sensor_category, EXCLUDED.sensor_category),
			location = COALESCE(devices.location, EXCLUDED.location),
			updated_at = now()
	`, reading.MAC, label, "Cisco Spaces", nullableString(label, hasLabel), hasLabel)
	return err
}

func validDeviceLabel(mac string, label string) bool {
	label = strings.TrimSpace(label)
	if label == "" {
		return false
	}
	return normalizeMAC(label) != mac
}

func nullablePtr(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
