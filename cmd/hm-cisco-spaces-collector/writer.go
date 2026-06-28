package main

import (
	"context"
	"strings"

	"home-metrics/internal/sensor"
	"home-metrics/internal/sensorstore"

	"github.com/jackc/pgx/v5"
)

func writeReading(ctx context.Context, db *pgx.Conn, reading sensorReading) error {
	if err := upsertDevice(ctx, db, reading); err != nil {
		return err
	}
	_, err := sensorstore.UpsertMinute(ctx, db, sensor.Reading{
		TS: reading.TS, MAC: reading.MAC,
		TemperatureC: reading.TemperatureC, HumidityPercent: reading.HumidityPercent,
		BatteryPercent: reading.BatteryPercent, PressureHPa: reading.PressureHPa,
		CO2PPM: reading.CO2PPM, Lux: reading.Lux, ETVOC: reading.ETVOC,
	})
	return err
}

func upsertDevice(ctx context.Context, db *pgx.Conn, reading sensorReading) error {
	label := strings.TrimSpace(reading.Label)
	hasLabel := validDeviceLabel(reading.MAC, label)
	if !hasLabel {
		label = reading.MAC
	}
	location := ""
	if hasLabel {
		location = label
	}
	return sensorstore.SyncDevice(ctx, db, sensor.Device{
		MAC: reading.MAC, Label: label, Location: location,
		IngestSource: "cisco_spaces", IngestSourceExplicit: true,
		SensorCategory: "environment", Enabled: true,
		PreserveExistingLabel: !hasLabel, PreserveExistingLocation: true,
	})
}

func validDeviceLabel(mac string, label string) bool {
	label = strings.TrimSpace(label)
	if label == "" {
		return false
	}
	return normalizeMAC(label) != mac
}
