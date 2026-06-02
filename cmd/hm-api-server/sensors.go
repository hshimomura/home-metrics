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
	MAC        string `json:"mac"`
	Label      string `json:"label"`
	SensorCategory string `json:"sensor_category,omitempty"`
	Location   string `json:"location,omitempty"`
	Enabled    bool   `json:"enabled"`
}

type latestResponse struct {
	Device deviceResponse      `json:"device"`
	TS     time.Time           `json:"ts"`
	Values map[string]*float64 `json:"values"`
}

type seriesPoint struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

var metricColumns = map[string]string{
	"temperature_c":    "temperature_c",
	"humidity_percent": "humidity_percent",
	"battery_percent":  "battery_percent",
	"rssi_dbm":         "rssi_dbm",
	"pressure_hpa":     "pressure_hpa",
	"co2_ppm":          "co2_ppm",
	"lux":              "lux",
	"etvoc":            "etvoc",
}

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
			mac,
			label,
			COALESCE(sensor_category, ''),
			COALESCE(location, ''),
			enabled
		FROM devices
		ORDER BY mac
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
		if err := rows.Scan(&d.MAC, &d.Label, &d.SensorCategory, &d.Location, &d.Enabled); err != nil {
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

	row := api.db.QueryRow(r.Context(), `
		SELECT
			ts,
			temperature_c,
			humidity_percent,
			battery_percent,
			rssi_dbm,
			pressure_hpa,
			co2_ppm,
			lux,
			etvoc
		FROM sensor_minute
		WHERE mac = $1
		  AND (
		    temperature_c IS NOT NULL OR
		    humidity_percent IS NOT NULL OR
		    battery_percent IS NOT NULL OR
		    pressure_hpa IS NOT NULL OR
		    co2_ppm IS NOT NULL OR
		    lux IS NOT NULL OR
		    etvoc IS NOT NULL
		  )
		ORDER BY ts DESC
		LIMIT 1
	`, mac)

	var ts time.Time
	values := map[string]*float64{}
	var temperature, humidity, battery, rssi, pressure, co2, lux, etvoc pgtype.Float8
	if err := row.Scan(&ts, &temperature, &humidity, &battery, &rssi, &pressure, &co2, &lux, &etvoc); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "sensor value not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "query latest value")
		return
	}
	values["temperature_c"] = floatPtrFromPg(temperature)
	values["humidity_percent"] = floatPtrFromPg(humidity)
	values["battery_percent"] = floatPtrFromPg(battery)
	values["rssi_dbm"] = floatPtrFromPg(rssi)
	values["pressure_hpa"] = floatPtrFromPg(pressure)
	values["co2_ppm"] = floatPtrFromPg(co2)
	values["lux"] = floatPtrFromPg(lux)
	values["etvoc"] = floatPtrFromPg(etvoc)

	writeJSON(w, http.StatusOK, latestResponse{Device: device, TS: ts, Values: values})
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
	err := api.db.QueryRow(ctx, `
		SELECT
			mac,
			label,
			COALESCE(sensor_category, ''),
			COALESCE(location, ''),
			enabled
		FROM devices
		WHERE mac = $1
	`, mac).Scan(&d.MAC, &d.Label, &d.SensorCategory, &d.Location, &d.Enabled)
	return d, err
}

func floatPtrFromPg(value pgtype.Float8) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}
