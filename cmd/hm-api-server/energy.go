package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type energyLatestResponse struct {
	TS        time.Time `json:"ts"`
	Source    string    `json:"source"`
	DeviceKey string    `json:"device_key"`
	Label     string    `json:"label,omitempty"`
	Location  string    `json:"location,omitempty"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
	Unit      string    `json:"unit,omitempty"`
}

type energySeriesResponse struct {
	Source    string        `json:"source"`
	DeviceKey string        `json:"device_key"`
	Metric    string        `json:"metric"`
	Range     string        `json:"range"`
	Unit      string        `json:"unit"`
	Points    []seriesPoint `json:"points"`
}

var energyRangeIntervals = map[string]struct {
	Lookback string
	Bucket   string
	Source   string
}{
	"1d": {"1 day", "8 minutes", "energy_readings"},
	"1w": {"7 days", "1 hour", "energy_1hour"},
	"1m": {"30 days", "4 hours", "energy_1hour"},
	"3m": {"90 days", "12 hours", "energy_12hour"},
	"1y": {"365 days", "1 day", "energy_1day"},
}

func (api *apiServer) handleEnergyLatest(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	deviceKey := strings.TrimSpace(r.URL.Query().Get("device_key"))

	rows, err := api.db.Query(r.Context(), `
		SELECT
			r.ts,
			md.source,
			d.device_key,
			COALESCE(d.label, ''),
			COALESCE(d.location, ''),
			md.metric,
			r.value,
			COALESCE(r.unit, md.unit, '')
		FROM energy_devices d
		JOIN energy_metric_definitions md
			ON md.source = d.source
		JOIN LATERAL (
			SELECT ts, value, unit
			FROM energy_readings r
			WHERE r.source = d.source
				AND r.device_key = d.device_key
				AND r.metric = md.metric
			ORDER BY r.ts DESC
			LIMIT 1
		) r ON true
		WHERE ($1 = '' OR d.source = $1)
			AND ($2 = '' OR d.device_key = $2)
			AND d.enabled
		ORDER BY md.source, d.device_key, md.metric
	`, source, deviceKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query energy latest")
		return
	}
	defer rows.Close()

	readings := []energyLatestResponse{}
	for rows.Next() {
		var reading energyLatestResponse
		if err := rows.Scan(
			&reading.TS,
			&reading.Source,
			&reading.DeviceKey,
			&reading.Label,
			&reading.Location,
			&reading.Metric,
			&reading.Value,
			&reading.Unit,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "scan energy latest")
			return
		}
		readings = append(readings, reading)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read energy latest")
		return
	}
	writeJSON(w, http.StatusOK, readings)
}

func (api *apiServer) handleEnergySeries(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	deviceKey := strings.TrimSpace(r.URL.Query().Get("device_key"))
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if source == "" || deviceKey == "" || metric == "" {
		writeError(w, http.StatusBadRequest, "source, device_key, and metric are required")
		return
	}
	rangeKey := strings.TrimSpace(r.URL.Query().Get("range"))
	if rangeKey == "" {
		rangeKey = "1d"
	}
	interval, ok := energyRangeIntervals[rangeKey]
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported range")
		return
	}

	var unit string
	if err := api.db.QueryRow(r.Context(), `
		SELECT COALESCE(unit, '')
		FROM energy_metric_definitions
		WHERE source = $1
			AND metric = $2
			AND enabled
	`, source, metric).Scan(&unit); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "energy metric not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query energy metric")
		return
	}

	query := fmt.Sprintf(`
		SELECT time_bucket($4::interval, ts) AS bucket, avg(value) AS value
		FROM %s
		WHERE source = $1
			AND device_key = $2
			AND metric = $3
			AND ts >= now() - $5::interval
		GROUP BY bucket
		ORDER BY bucket
	`, interval.Source)

	rows, err := api.db.Query(r.Context(), query, source, deviceKey, metric, interval.Bucket, interval.Lookback)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query energy series")
		return
	}
	defer rows.Close()

	points := []seriesPoint{}
	for rows.Next() {
		var point seriesPoint
		if err := rows.Scan(&point.TS, &point.Value); err != nil {
			writeError(w, http.StatusInternalServerError, "scan energy series")
			return
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read energy series")
		return
	}
	writeJSON(w, http.StatusOK, energySeriesResponse{
		Source:    source,
		DeviceKey: deviceKey,
		Metric:    metric,
		Range:     rangeKey,
		Unit:      unit,
		Points:    points,
	})
}
