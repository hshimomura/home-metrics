package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type deviceResponse struct {
	MAC            string              `json:"mac"`
	Label          string              `json:"label"`
	Location       string              `json:"location,omitempty"`
	Enabled        bool                `json:"enabled"`
	IngestSource   string              `json:"ingest_source,omitempty"`
	SensorTypeCode string              `json:"sensor_type_code,omitempty"`
	SensorType     *sensorTypeResponse `json:"sensor_type,omitempty"`
	SensorCategory string              `json:"sensor_category,omitempty"`
}

type sensorTypeResponse struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"`
	Vendor      string `json:"vendor,omitempty"`
	Model       string `json:"model,omitempty"`
}

type latestResponse struct {
	Device          deviceResponse       `json:"device"`
	TS              time.Time            `json:"ts"`
	Values          map[string]*float64  `json:"values"`
	ValueTimestamps map[string]time.Time `json:"value_timestamps,omitempty"`
}

type seriesPoint struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

type sensorMetric struct {
	Key    string
	Column string
}

var sensorMetrics = []sensorMetric{
	{Key: "temperature_c", Column: "temperature_c"},
	{Key: "humidity_percent", Column: "humidity_percent"},
	{Key: "battery_percent", Column: "battery_percent"},
	{Key: "rssi_dbm", Column: "rssi_dbm"},
	{Key: "pressure_hpa", Column: "pressure_hpa"},
	{Key: "co2_ppm", Column: "co2_ppm"},
	{Key: "lux", Column: "lux"},
	{Key: "etvoc", Column: "etvoc"},
	{Key: "soil_moisture_percent", Column: "soil_moisture_percent"},
	{Key: "conductivity_us_cm", Column: "conductivity_us_cm"},
}

var metricColumns = buildMetricColumns(sensorMetrics)

var latestSnapshotQuery = buildLatestSnapshotQuery(sensorMetrics)

var rangeIntervals = map[string]struct {
	Lookback string
	Bucket   string
	Source   string
}{
	"1d": {"1 day", "8 minutes", "sensor_minute"},
	"1w": {"7 days", "1 hour", "sensor_1hour"},
	"1m": {"30 days", "4 hours", "sensor_1hour"},
	"3m": {"90 days", "12 hours", "sensor_12hour"},
	"1y": {"365 days", "1 day", "sensor_1day"},
}

func (api *apiServer) handleDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT
			d.mac,
			d.label,
			COALESCE(d.location, ''),
			d.enabled,
			COALESCE(d.ingest_source, ''),
			COALESCE(d.sensor_type_code, ''),
			COALESCE(d.sensor_category, ''),
			COALESCE(st.code, ''),
			COALESCE(st.display_name, ''),
			COALESCE(st.category, ''),
			COALESCE(st.vendor, ''),
			COALESCE(st.model, '')
		FROM devices d
		LEFT JOIN sensor_types st ON st.code = d.sensor_type_code
		ORDER BY d.mac
	`)
	if err != nil {
		log.Printf("query devices: %v", err)
		writeError(w, http.StatusInternalServerError, "query devices")
		return
	}
	defer rows.Close()

	devices := []deviceResponse{}
	for rows.Next() {
		var d deviceResponse
		if err := scanDeviceResponse(rows, &d); err != nil {
			log.Printf("scan devices: %v", err)
			writeError(w, http.StatusInternalServerError, "scan devices")
			return
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		log.Printf("read devices: %v", err)
		writeError(w, http.StatusInternalServerError, "read devices")
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

func (api *apiServer) handleDeviceLatest(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	device, err := api.loadDevice(r.Context(), mac)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query device")
		return
	}

	snapshot, err := api.loadLatestSnapshot(r.Context(), mac)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "sensor value not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "query latest value")
		return
	}

	writeJSON(w, http.StatusOK, latestResponse{
		Device:          device,
		TS:              snapshot.TS,
		Values:          snapshot.Values,
		ValueTimestamps: snapshot.ValueTimestamps,
	})
}

type latestSnapshot struct {
	TS              time.Time
	Values          map[string]*float64
	ValueTimestamps map[string]time.Time
}

func (api *apiServer) loadLatestSnapshot(ctx context.Context, mac string) (latestSnapshot, error) {
	row := api.db.QueryRow(ctx, latestSnapshotQuery, mac)

	timestamps := make([]pgtype.Timestamptz, len(sensorMetrics))
	readings := make([]pgtype.Float8, len(sensorMetrics))
	scanArgs := make([]any, 0, len(sensorMetrics)*2)
	for i := range sensorMetrics {
		scanArgs = append(scanArgs, &timestamps[i], &readings[i])
	}
	if err := row.Scan(scanArgs...); err != nil {
		return latestSnapshot{}, err
	}
	return assembleLatestSnapshot(sensorMetrics, timestamps, readings)
}

func assembleLatestSnapshot(metrics []sensorMetric, timestamps []pgtype.Timestamptz, readings []pgtype.Float8) (latestSnapshot, error) {
	snapshot := latestSnapshot{
		Values:          make(map[string]*float64, len(metrics)),
		ValueTimestamps: map[string]time.Time{},
	}
	for i, metric := range metrics {
		snapshot.Values[metric.Key] = floatPtrFromPg(readings[i])
		if timestamps[i].Valid {
			ts := timestamps[i].Time
			snapshot.ValueTimestamps[metric.Key] = ts
			if snapshot.TS.IsZero() || ts.After(snapshot.TS) {
				snapshot.TS = ts
			}
		}
	}
	if snapshot.TS.IsZero() {
		return latestSnapshot{}, pgx.ErrNoRows
	}
	return snapshot, nil
}

func buildMetricColumns(metrics []sensorMetric) map[string]string {
	columns := make(map[string]string, len(metrics))
	for _, metric := range metrics {
		columns[metric.Key] = metric.Column
	}
	return columns
}

func buildLatestSnapshotQuery(metrics []sensorMetric) string {
	var b strings.Builder
	b.WriteString("SELECT\n")
	for i := range metrics {
		if i > 0 {
			b.WriteString(",\n")
		}
		alias := fmt.Sprintf("m%d", i)
		b.WriteString(fmt.Sprintf("\t%s.ts, %s.value", alias, alias))
	}
	b.WriteString("\nFROM (SELECT 1) base\n")
	for i, metric := range metrics {
		alias := fmt.Sprintf("m%d", i)
		b.WriteString(fmt.Sprintf(
			"LEFT JOIN LATERAL (SELECT ts, %s AS value FROM sensor_minute WHERE mac = $1 AND %s IS NOT NULL ORDER BY ts DESC LIMIT 1) %s ON true\n",
			metric.Column,
			metric.Column,
			alias,
		))
	}
	return b.String()
}

func (api *apiServer) handleDeviceSeries(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	metric := r.URL.Query().Get("metric")
	column, ok := metricColumns[metric]
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported metric")
		return
	}
	rangeKey := r.URL.Query().Get("range")
	if rangeKey == "" {
		rangeKey = "1d"
	}
	interval, ok := rangeIntervals[rangeKey]
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported range")
		return
	}

	query := fmt.Sprintf(`
		SELECT time_bucket($2::interval, ts) AS bucket, avg(%s) AS value
		FROM %s
		WHERE mac = $1
			AND ts >= now() - $3::interval
			AND %s IS NOT NULL
		GROUP BY bucket
		ORDER BY bucket
	`, column, interval.Source, column)

	rows, err := api.db.Query(r.Context(), query, mac, interval.Bucket, interval.Lookback)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query series")
		return
	}
	defer rows.Close()

	points := []seriesPoint{}
	for rows.Next() {
		var point seriesPoint
		if err := rows.Scan(&point.TS, &point.Value); err != nil {
			writeError(w, http.StatusInternalServerError, "scan series")
			return
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read series")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mac":    mac,
		"metric": metric,
		"range":  rangeKey,
		"points": points,
	})
}

func (api *apiServer) loadDevice(ctx context.Context, mac string) (deviceResponse, error) {
	var d deviceResponse
	row := api.db.QueryRow(ctx, `
		SELECT
			d.mac,
			d.label,
			COALESCE(d.location, ''),
			d.enabled,
			COALESCE(d.ingest_source, ''),
			COALESCE(d.sensor_type_code, ''),
			COALESCE(d.sensor_category, ''),
			COALESCE(st.code, ''),
			COALESCE(st.display_name, ''),
			COALESCE(st.category, ''),
			COALESCE(st.vendor, ''),
			COALESCE(st.model, '')
		FROM devices d
		LEFT JOIN sensor_types st ON st.code = d.sensor_type_code
		WHERE d.mac = $1
	`, mac)
	if err := scanDeviceResponse(row, &d); err != nil {
		return deviceResponse{}, err
	}
	return d, nil
}

func floatPtrFromPg(value pgtype.Float8) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

type deviceScanner interface {
	Scan(dest ...any) error
}

func scanDeviceResponse(row deviceScanner, d *deviceResponse) error {
	var st sensorTypeResponse
	if err := row.Scan(
		&d.MAC,
		&d.Label,
		&d.Location,
		&d.Enabled,
		&d.IngestSource,
		&d.SensorTypeCode,
		&d.SensorCategory,
		&st.Code,
		&st.DisplayName,
		&st.Category,
		&st.Vendor,
		&st.Model,
	); err != nil {
		return err
	}
	if st.Code != "" {
		d.SensorType = &st
	}
	return nil
}
