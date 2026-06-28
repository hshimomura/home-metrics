package sensorstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"home-metrics/internal/sensor"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrOwnershipConflict = errors.New("device ownership conflict")

const syncDeviceSQL = `
	INSERT INTO devices (
		mac, label, location, ingest_source, sensor_type_code, sensor_category, enabled
	)
	VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7)
	ON CONFLICT (mac) DO UPDATE SET
		label = CASE WHEN $8 THEN devices.label ELSE EXCLUDED.label END,
		location = CASE WHEN $9 THEN devices.location ELSE EXCLUDED.location END,
		ingest_source = COALESCE(devices.ingest_source, EXCLUDED.ingest_source),
		sensor_type_code = COALESCE(EXCLUDED.sensor_type_code, devices.sensor_type_code),
		sensor_category = COALESCE(EXCLUDED.sensor_category, devices.sensor_category),
		enabled = EXCLUDED.enabled,
		updated_at = now()
	WHERE devices.ingest_source IS NULL
	   OR devices.ingest_source = EXCLUDED.ingest_source
	RETURNING ingest_source
`

type Execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type DB interface {
	Execer
	QueryRow(context.Context, string, ...any) pgx.Row
}

func SyncDevice(ctx context.Context, db DB, device sensor.Device) error {
	device.MAC = sensor.NormalizeMAC(device.MAC)
	device.Label = strings.TrimSpace(device.Label)
	device.Location = strings.TrimSpace(device.Location)
	device.IngestSource = strings.TrimSpace(device.IngestSource)
	device.SensorTypeCode = strings.TrimSpace(device.SensorTypeCode)
	device.SensorCategory = strings.TrimSpace(device.SensorCategory)
	if device.MAC == "" {
		return errors.New("device MAC is required")
	}
	if device.Label == "" {
		device.Label = device.MAC
	}
	if device.Location == "" {
		device.Location = device.Label
	}
	if device.IngestSourceExplicit && device.IngestSource == "" {
		return fmt.Errorf("device %s has an explicit empty ingest source", device.MAC)
	}
	if device.SensorTypeCode != "" {
		var registeredCategory string
		if err := db.QueryRow(ctx, `SELECT category FROM sensor_types WHERE code = $1`, device.SensorTypeCode).Scan(&registeredCategory); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("device %s uses unknown sensor type %q", device.MAC, device.SensorTypeCode)
		} else if err != nil {
			return fmt.Errorf("validate sensor type %s: %w", device.SensorTypeCode, err)
		}
		if device.SensorCategory == "" {
			device.SensorCategory = registeredCategory
		} else if device.SensorCategory != registeredCategory {
			return fmt.Errorf(
				"device %s sensor category %q does not match sensor type %s category %q",
				device.MAC, device.SensorCategory, device.SensorTypeCode, registeredCategory,
			)
		}
	}

	var source any
	if device.IngestSourceExplicit {
		source = device.IngestSource
	}
	var owner pgtype.Text
	err := db.QueryRow(
		ctx,
		syncDeviceSQL,
		device.MAC,
		device.Label,
		device.Location,
		source,
		device.SensorTypeCode,
		device.SensorCategory,
		device.Enabled,
		device.PreserveExistingLabel,
		device.PreserveExistingLocation,
	).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: device=%s requested_source=%q", ErrOwnershipConflict, device.MAC, device.IngestSource)
	}
	if err != nil {
		return fmt.Errorf("sync device %s: %w", device.MAC, err)
	}
	return nil
}

const UpsertMinuteSQL = `
	INSERT INTO sensor_minute (
		ts, mac, temperature_c, humidity_percent, battery_percent,
		rssi_dbm, pressure_hpa, co2_ppm, lux, etvoc,
		soil_moisture_percent, conductivity_us_cm
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	ON CONFLICT (ts, mac) DO UPDATE SET
		temperature_c = COALESCE(EXCLUDED.temperature_c, sensor_minute.temperature_c),
		humidity_percent = COALESCE(EXCLUDED.humidity_percent, sensor_minute.humidity_percent),
		battery_percent = COALESCE(EXCLUDED.battery_percent, sensor_minute.battery_percent),
		rssi_dbm = COALESCE(EXCLUDED.rssi_dbm, sensor_minute.rssi_dbm),
		pressure_hpa = COALESCE(EXCLUDED.pressure_hpa, sensor_minute.pressure_hpa),
		co2_ppm = COALESCE(EXCLUDED.co2_ppm, sensor_minute.co2_ppm),
		lux = COALESCE(EXCLUDED.lux, sensor_minute.lux),
		etvoc = COALESCE(EXCLUDED.etvoc, sensor_minute.etvoc),
		soil_moisture_percent = COALESCE(EXCLUDED.soil_moisture_percent, sensor_minute.soil_moisture_percent),
		conductivity_us_cm = COALESCE(EXCLUDED.conductivity_us_cm, sensor_minute.conductivity_us_cm),
		inserted_at = now()
`

func UpsertMinute(ctx context.Context, db Execer, reading sensor.Reading) (bool, error) {
	reading.MAC = sensor.NormalizeMAC(reading.MAC)
	if reading.MAC == "" {
		return false, errors.New("sensor MAC is required")
	}
	if reading.TS.IsZero() {
		return false, errors.New("sensor timestamp is required")
	}
	if reading.Empty() {
		return false, nil
	}
	_, err := db.Exec(ctx, UpsertMinuteSQL,
		reading.TS.Truncate(time.Minute), reading.MAC,
		nullable(reading.TemperatureC), nullable(reading.HumidityPercent),
		nullable(reading.BatteryPercent), nullable(reading.RSSI),
		nullable(reading.PressureHPa), nullable(reading.CO2PPM),
		nullable(reading.Lux), nullable(reading.ETVOC),
		nullable(reading.SoilMoisturePercent), nullable(reading.ConductivityUSCM),
	)
	if err != nil {
		return false, fmt.Errorf("upsert sensor_minute %s %s: %w", reading.MAC, reading.TS.Format("2006-01-02T15:04:05Z07:00"), err)
	}
	return true, nil
}

func nullable(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
