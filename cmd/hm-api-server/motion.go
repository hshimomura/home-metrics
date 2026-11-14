package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type motionResponse struct {
	Motion     *bool      `json:"motion"`
	ObservedAt *time.Time `json:"observed_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	Fresh      bool       `json:"fresh"`
	APMAC      *string    `json:"ap_mac,omitempty"`
}

type motionEventResponse struct {
	ID         int64     `json:"id"`
	ObservedAt time.Time `json:"observed_at"`
	ReceivedAt time.Time `json:"received_at"`
	Motion     bool      `json:"motion"`
	Initial    bool      `json:"initial"`
	PacketID   *int      `json:"packet_id"`
	APMAC      *string   `json:"ap_mac"`
}

func (api *apiServer) loadMotion(ctx context.Context, mac string, snapshot *latestSnapshot) (*motionResponse, error) {
	var m motionResponse
	var battery *float64
	var batteryAt *time.Time
	err := api.db.QueryRow(ctx, `SELECT motion,motion_observed_at,last_seen_at,battery_percent,battery_observed_at,ap_mac FROM sensor_motion_state WHERE mac=$1`, mac).Scan(&m.Motion, &m.ObservedAt, &m.LastSeenAt, &battery, &batteryAt, &m.APMAC)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Fresh = time.Since(m.LastSeenAt) <= envDuration("MOTION_SENSOR_STALE_AFTER", 45*time.Minute)
	if snapshot.Values == nil {
		snapshot.Values = map[string]*float64{}
	}
	if snapshot.ValueTimestamps == nil {
		snapshot.ValueTimestamps = map[string]time.Time{}
	}
	if battery != nil && batteryAt != nil {
		snapshot.Values["battery_percent"] = battery
		snapshot.ValueTimestamps["battery_percent"] = *batteryAt
	}
	if m.LastSeenAt.After(snapshot.TS) {
		snapshot.TS = m.LastSeenAt
	}
	return &m, nil
}

func motionEventParams(r *http.Request, now time.Time) (time.Time, int, error) {
	since := now.Add(-24 * time.Hour)
	var err error
	if v := r.URL.Query().Get("since"); v != "" {
		since, err = time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return since, 0, err
		}
	}
	limit := 1000
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil {
			return since, 0, err
		}
	}
	if limit < 1 || limit > 5000 {
		return since, 0, errors.New("limit must be 1..5000")
	}
	return since, limit, nil
}

func (api *apiServer) handleMotionEvents(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	device, err := api.loadDevice(r.Context(), mac)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "device not found")
		return
	}
	if err != nil {
		writeError(w, 500, "query device")
		return
	}
	if device.SensorCategory != "motion" {
		writeError(w, 400, "device is not a motion sensor")
		return
	}
	since, limit, err := motionEventParams(r, time.Now())
	if err != nil {
		writeError(w, 400, "invalid since or limit")
		return
	}
	rows, err := api.db.Query(r.Context(), `SELECT id,observed_at,received_at,motion,initial,packet_id,ap_mac FROM sensor_motion_events WHERE mac=$1 AND observed_at >= $2 ORDER BY observed_at DESC,id DESC LIMIT $3`, mac, since, limit)
	if err != nil {
		writeError(w, 500, "query motion events")
		return
	}
	defer rows.Close()
	events := []motionEventResponse{}
	for rows.Next() {
		var e motionEventResponse
		if err := rows.Scan(&e.ID, &e.ObservedAt, &e.ReceivedAt, &e.Motion, &e.Initial, &e.PacketID, &e.APMAC); err != nil {
			writeError(w, 500, "scan motion event")
			return
		}
		events = append(events, e)
	}
	if rows.Err() != nil {
		writeError(w, 500, "read motion events")
		return
	}
	writeJSON(w, 200, map[string]any{"mac": mac, "events": events})
}
