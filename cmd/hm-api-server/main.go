package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultAddr   = ":8080"
	defaultDBDSN  = "dbname=ble_sensors host=/var/run/postgresql"
	defaultUserID = int64(1)

	testNotificationEventCreatedAt = "2026-05-06T14:13:16.978914+09:00"
)

type apiServer struct {
	db             *pgxpool.Pool
	apiToken       string
	allowedOrigins map[string]bool
	apns           *apnsTestSender
}

type deviceResponse struct {
	MAC        string `json:"mac"`
	Label      string `json:"label"`
	DeviceType string `json:"device_type,omitempty"`
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

type alertRuleResponse struct {
	ID              int64      `json:"id"`
	UserID          int64      `json:"user_id"`
	MAC             string     `json:"mac"`
	Metric          string     `json:"metric"`
	Operator        string     `json:"operator"`
	Threshold       float64    `json:"threshold"`
	CooldownSeconds int64      `json:"cooldown_seconds"`
	Enabled         bool       `json:"enabled"`
	LastNotifiedAt  *time.Time `json:"last_notified_at,omitempty"`
	LastValue       *float64   `json:"last_value,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type alertRuleRequest struct {
	MAC             string   `json:"mac"`
	Metric          string   `json:"metric"`
	Operator        string   `json:"operator"`
	Threshold       *float64 `json:"threshold"`
	CooldownSeconds *int64   `json:"cooldown_seconds"`
	Enabled         *bool    `json:"enabled"`
}

type iosDeviceRequest struct {
	APNSDeviceToken string  `json:"apns_device_token"`
	AppBundleID     string  `json:"app_bundle_id"`
	APNSEnvironment string  `json:"apns_environment"`
	DeviceName      *string `json:"device_name"`
	Enabled         *bool   `json:"enabled"`
}

type iosDeviceResponse struct {
	ID              int64      `json:"id"`
	UserID          int64      `json:"user_id"`
	APNSDeviceToken string     `json:"apns_device_token"`
	AppBundleID     string     `json:"app_bundle_id"`
	APNSEnvironment string     `json:"apns_environment"`
	DeviceName      *string    `json:"device_name,omitempty"`
	Enabled         bool       `json:"enabled"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type testNotificationResponse struct {
	ID                         int64      `json:"id"`
	DeviceName                 *string    `json:"device_name,omitempty"`
	AppBundleID                string     `json:"app_bundle_id"`
	APNSEnvironment            string     `json:"apns_environment"`
	NotificationEventID        int64      `json:"notification_event_id"`
	NotificationEventCreatedAt time.Time  `json:"notification_event_created_at"`
	Status                     string     `json:"status"`
	SentAt                     *time.Time `json:"sent_at,omitempty"`
}

type testNotificationEvent struct {
	ID           int64
	AlertRuleID  *int64
	UserID       *int64
	MAC          string
	Metric       string
	Value        *float64
	Threshold    *float64
	TriggeredAt  time.Time
	SentAt       *time.Time
	Status       string
	ErrorMessage *string
	CreatedAt    time.Time
}

type notificationEventResponse struct {
	ID           int64      `json:"id"`
	AlertRuleID  *int64     `json:"alert_rule_id,omitempty"`
	UserID       *int64     `json:"user_id,omitempty"`
	MAC          string     `json:"mac"`
	Metric       string     `json:"metric"`
	Value        *float64   `json:"value,omitempty"`
	Threshold    *float64   `json:"threshold,omitempty"`
	TriggeredAt  time.Time  `json:"triggered_at"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
	Status       string     `json:"status"`
	ErrorMessage *string    `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("BLE_DB_DSN")
	if dsn == "" {
		dsn = defaultDBDSN
	}
	log.Printf("dsn: %s", dsn)
	addr := os.Getenv("API_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	apiToken := strings.TrimSpace(os.Getenv("API_TOKEN"))
	allowedOrigins := parseAllowedOrigins(os.Getenv("API_ALLOWED_ORIGINS"))
	apnsSender, err := newAPNSTestSenderFromEnv(http.DefaultClient)
	if err != nil {
		log.Printf("configure APNs test sender: %v", err)
	}

	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close()

	server := &http.Server{
		Addr:              addr,
		Handler:           newRouter(&apiServer{db: db, apiToken: apiToken, allowedOrigins: allowedOrigins, apns: apnsSender}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown api server: %v", err)
		}
	}()

	log.Printf("api server started addr=%s db=%s", addr, dsn)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen api server: %v", err)
	}
}

func newRouter(api *apiServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", api.handleWebIndex)
	mux.HandleFunc("GET /api/health", api.handleHealth)
	mux.HandleFunc("GET /api/devices", api.handleDevices)
	mux.HandleFunc("GET /api/devices/{mac}/latest", api.handleDeviceLatest)
	mux.HandleFunc("GET /api/devices/{mac}/series", api.handleDeviceSeries)
	mux.HandleFunc("GET /api/alert-rules", api.handleAlertRules)
	mux.HandleFunc("POST /api/alert-rules", api.handleCreateAlertRule)
	mux.HandleFunc("PUT /api/alert-rules/{id}", api.handleUpdateAlertRule)
	mux.HandleFunc("POST /api/alert-rules/{id}/reset-cooldown", api.handleResetAlertRuleCooldown)
	mux.HandleFunc("DELETE /api/alert-rules/{id}", api.handleDeleteAlertRule)
	mux.HandleFunc("GET /api/notification-events", api.handleNotificationEvents)
	mux.HandleFunc("DELETE /api/notification-events", api.handleDeleteNotificationEvents)
	mux.HandleFunc("GET /api/energy/latest", api.handleEnergyLatest)
	mux.HandleFunc("GET /api/energy/series", api.handleEnergySeries)
	mux.HandleFunc("GET /api/ios/devices", api.handleIOSDevices)
	mux.HandleFunc("POST /api/ios/devices", api.handleRegisterIOSDevice)
	mux.HandleFunc("PUT /api/ios/devices/{id}", api.handleUpdateIOSDevice)
	mux.HandleFunc("POST /api/ios/devices/{id}/test-notification", api.handleTestIOSDeviceNotification)
	mux.HandleFunc("DELETE /api/ios/devices/{id}", api.handleDeleteIOSDevice)
	mux.HandleFunc("GET /api/", api.handleUnsupportedAPIEndpoint)
	mux.HandleFunc("POST /api/", api.handleUnsupportedAPIEndpoint)
	mux.HandleFunc("PUT /api/", api.handleUnsupportedAPIEndpoint)
	mux.HandleFunc("DELETE /api/", api.handleUnsupportedAPIEndpoint)
	return api.withCORS(api.withAuth(withJSON(mux)))
}

func (api *apiServer) handleWebIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open("web/index.html")
	if err != nil {
		log.Printf("open web index: %v", err)
		http.Error(w, "web index unavailable", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		log.Printf("stat web index: %v", err)
		http.Error(w, "web index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", info.ModTime(), file)
}

func (api *apiServer) handleUnsupportedAPIEndpoint(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "unsupported endpoint")
}

func withJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
		}
		next.ServeHTTP(w, r)
	})
}

func (api *apiServer) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/health" || api.apiToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" {
			token = r.Header.Get("X-API-Token")
		}
		if token != api.apiToken {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (api *apiServer) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if api.isOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Token")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (api *apiServer) isOriginAllowed(origin string) bool {
	if origin == "" || len(api.allowedOrigins) == 0 {
		return false
	}
	return api.allowedOrigins["*"] || api.allowedOrigins[origin]
}

func (api *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := api.db.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (api *apiServer) handleDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT mac, label, COALESCE(device_type, ''), COALESCE(location, ''), enabled
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
		if err := rows.Scan(&d.MAC, &d.Label, &d.DeviceType, &d.Location, &d.Enabled); err != nil {
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

func (api *apiServer) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT
			r.id,
			r.user_id,
			r.mac,
			r.metric,
			r.operator,
			r.threshold,
			EXTRACT(EPOCH FROM r.cooldown_duration)::bigint,
			r.enabled,
			s.last_notified_at,
			s.last_value,
			r.created_at,
			r.updated_at
		FROM alert_rules r
		LEFT JOIN alert_rule_state s ON s.alert_rule_id = r.id
		WHERE r.user_id = $1
		ORDER BY r.id
	`, defaultUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query alert rules")
		return
	}
	defer rows.Close()

	rules := []alertRuleResponse{}
	for rows.Next() {
		rule, err := scanAlertRule(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scan alert rules")
			return
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read alert rules")
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func (api *apiServer) handleCreateAlertRule(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeAlertRuleRequest(w, r, false)
	if !ok {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	cooldownSeconds := int64(24 * 60 * 60)
	if req.CooldownSeconds != nil {
		cooldownSeconds = *req.CooldownSeconds
	}

	row := api.db.QueryRow(r.Context(), `
		INSERT INTO alert_rules (
			user_id,
			mac,
			metric,
			operator,
			threshold,
			cooldown_duration,
			enabled
		)
		VALUES ($1, $2, $3, $4, $5, make_interval(secs => $6::double precision), $7)
		RETURNING
			id,
			user_id,
			mac,
			metric,
			operator,
			threshold,
			EXTRACT(EPOCH FROM cooldown_duration)::bigint,
			enabled,
			NULL::timestamptz,
			NULL::double precision,
			created_at,
			updated_at
	`, defaultUserID, strings.ToLower(req.MAC), req.Metric, req.Operator, *req.Threshold, float64(cooldownSeconds), enabled)
	rule, err := scanAlertRule(row)
	if err != nil {
		writeError(w, http.StatusBadRequest, "create alert rule")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (api *apiServer) handleUpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	req, ok := decodeAlertRuleRequest(w, r, false)
	if !ok {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	cooldownSeconds := int64(24 * 60 * 60)
	if req.CooldownSeconds != nil {
		cooldownSeconds = *req.CooldownSeconds
	}

	row := api.db.QueryRow(r.Context(), `
		UPDATE alert_rules
		SET
			mac = $3,
			metric = $4,
			operator = $5,
			threshold = $6,
			cooldown_duration = make_interval(secs => $7::double precision),
			enabled = $8,
			updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING
			id,
			user_id,
			mac,
			metric,
			operator,
			threshold,
			EXTRACT(EPOCH FROM cooldown_duration)::bigint,
			enabled,
			NULL::timestamptz,
			NULL::double precision,
			created_at,
			updated_at
	`, id, defaultUserID, strings.ToLower(req.MAC), req.Metric, req.Operator, *req.Threshold, float64(cooldownSeconds), enabled)
	rule, err := scanAlertRule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "alert rule not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "update alert rule")
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (api *apiServer) handleDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	tag, err := api.db.Exec(r.Context(), `
		DELETE FROM alert_rules
		WHERE id = $1 AND user_id = $2
	`, id, defaultUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete alert rule")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "alert rule not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *apiServer) handleResetAlertRuleCooldown(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	var exists bool
	if err := api.db.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM alert_rules
			WHERE id = $1 AND user_id = $2
		)
	`, id, defaultUserID).Scan(&exists); err != nil {
		writeError(w, http.StatusInternalServerError, "query alert rule")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "alert rule not found")
		return
	}

	if _, err := api.db.Exec(r.Context(), `
		UPDATE alert_rule_state
		SET last_notified_at = NULL,
			updated_at = now()
		WHERE alert_rule_id = $1
	`, id); err != nil {
		writeError(w, http.StatusInternalServerError, "reset alert rule cooldown")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *apiServer) handleNotificationEvents(w http.ResponseWriter, r *http.Request) {
	limit := int64(50)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = parsed
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !validNotificationStatus(status) {
		writeError(w, http.StatusBadRequest, "unsupported status")
		return
	}
	mac := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mac")))
	ruleID, hasRuleID, ok := optionalInt64(w, r.URL.Query().Get("alert_rule_id"), "alert_rule_id")
	if !ok {
		return
	}

	rows, err := api.db.Query(r.Context(), `
		SELECT
			id,
			alert_rule_id,
			user_id,
			mac,
			metric,
			value,
			threshold,
			triggered_at,
			sent_at,
			status,
			error_message,
			created_at
		FROM notification_events
		WHERE user_id = $1
			AND ($2 = '' OR status = $2)
			AND ($3 = '' OR mac = $3)
			AND ($4::boolean = false OR alert_rule_id = $5)
		ORDER BY created_at DESC
		LIMIT $6
	`, defaultUserID, status, mac, hasRuleID, ruleID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query notification events")
		return
	}
	defer rows.Close()

	events := []notificationEventResponse{}
	for rows.Next() {
		event, err := scanNotificationEvent(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scan notification events")
			return
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read notification events")
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (api *apiServer) handleDeleteNotificationEvents(w http.ResponseWriter, r *http.Request) {
	if _, err := api.db.Exec(r.Context(), `
		DELETE FROM notification_events
		WHERE user_id = $1
	`, defaultUserID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete notification events")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func (api *apiServer) handleIOSDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := api.db.Query(r.Context(), `
		SELECT
			id,
			user_id,
			apns_device_token,
			app_bundle_id,
			apns_environment,
			device_name,
			enabled,
			last_seen_at,
			created_at,
			updated_at
		FROM ios_devices
		WHERE user_id = $1
		ORDER BY id
	`, defaultUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query ios devices")
		return
	}
	defer rows.Close()

	devices := []iosDeviceResponse{}
	for rows.Next() {
		device, err := scanIOSDevice(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scan ios devices")
			return
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "read ios devices")
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

func (api *apiServer) handleRegisterIOSDevice(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeIOSDeviceRequest(w, r)
	if !ok {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row := api.db.QueryRow(r.Context(), `
		INSERT INTO ios_devices (
			user_id,
			apns_device_token,
			app_bundle_id,
			apns_environment,
			device_name,
			enabled,
			last_seen_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (apns_device_token, app_bundle_id, apns_environment) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			device_name = EXCLUDED.device_name,
			enabled = EXCLUDED.enabled,
			last_seen_at = now(),
			updated_at = now()
		RETURNING
			id,
			user_id,
			apns_device_token,
			app_bundle_id,
			apns_environment,
			device_name,
			enabled,
			last_seen_at,
			created_at,
			updated_at
	`, defaultUserID, req.APNSDeviceToken, req.AppBundleID, req.APNSEnvironment, req.DeviceName, enabled)
	device, err := scanIOSDevice(row)
	if err != nil {
		writeError(w, http.StatusBadRequest, "register ios device")
		return
	}
	writeJSON(w, http.StatusCreated, device)
}

func (api *apiServer) handleUpdateIOSDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	req, ok := decodeIOSDeviceRequest(w, r)
	if !ok {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row := api.db.QueryRow(r.Context(), `
		UPDATE ios_devices
		SET
			apns_device_token = $3,
			app_bundle_id = $4,
			apns_environment = $5,
			device_name = $6,
			enabled = $7,
			last_seen_at = now(),
			updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING
			id,
			user_id,
			apns_device_token,
			app_bundle_id,
			apns_environment,
			device_name,
			enabled,
			last_seen_at,
			created_at,
			updated_at
	`, id, defaultUserID, req.APNSDeviceToken, req.AppBundleID, req.APNSEnvironment, req.DeviceName, enabled)
	device, err := scanIOSDevice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "ios device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "update ios device")
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func (api *apiServer) handleDeleteIOSDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	tag, err := api.db.Exec(r.Context(), `
		DELETE FROM ios_devices
		WHERE id = $1 AND user_id = $2
	`, id, defaultUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete ios device")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "ios device not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *apiServer) handleTestIOSDeviceNotification(w http.ResponseWriter, r *http.Request) {
	if api.apns == nil {
		writeError(w, http.StatusServiceUnavailable, "APNs test sender is not configured")
		return
	}
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	target, err := api.apns.loadTarget(r.Context(), api.db, defaultUserID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "ios device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query ios device")
		return
	}
	if !target.Enabled {
		writeError(w, http.StatusBadRequest, "ios device is disabled")
		return
	}
	if target.AppBundleID != api.apns.bundleID || target.APNSEnvironment != api.apns.environment {
		writeError(w, http.StatusBadRequest, "ios device does not match APNs bundle/environment")
		return
	}
	event, err := api.loadTestNotificationEvent(r.Context())
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "test notification event not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query test notification event")
		return
	}
	now := time.Now()
	if err := api.apns.send(r.Context(), api.db, target, event, now); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, testNotificationResponse{
		ID:                         target.ID,
		DeviceName:                 target.DeviceName,
		AppBundleID:                target.AppBundleID,
		APNSEnvironment:            target.APNSEnvironment,
		NotificationEventID:        event.ID,
		NotificationEventCreatedAt: event.CreatedAt,
		Status:                     "sent",
		SentAt:                     &now,
	})
}

func (api *apiServer) loadTestNotificationEvent(ctx context.Context) (testNotificationEvent, error) {
	eventCreatedAt, err := time.Parse(time.RFC3339Nano, testNotificationEventCreatedAt)
	if err != nil {
		return testNotificationEvent{}, err
	}
	row := api.db.QueryRow(ctx, `
		SELECT
			id,
			alert_rule_id,
			user_id,
			mac,
			metric,
			value,
			threshold,
			triggered_at,
			sent_at,
			status,
			error_message,
			created_at
		FROM notification_events
		WHERE created_at = $1
		LIMIT 1
	`, eventCreatedAt)
	return scanTestNotificationEvent(row)
}

func (api *apiServer) loadDevice(ctx context.Context, mac string) (deviceResponse, error) {
	var d deviceResponse
	err := api.db.QueryRow(ctx, `
		SELECT mac, label, COALESCE(device_type, ''), COALESCE(location, ''), enabled
		FROM devices
		WHERE mac = $1
	`, mac).Scan(&d.MAC, &d.Label, &d.DeviceType, &d.Location, &d.Enabled)
	return d, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAlertRule(row scanner) (alertRuleResponse, error) {
	var rule alertRuleResponse
	var lastNotifiedAt pgtype.Timestamptz
	var lastValue pgtype.Float8
	if err := row.Scan(
		&rule.ID,
		&rule.UserID,
		&rule.MAC,
		&rule.Metric,
		&rule.Operator,
		&rule.Threshold,
		&rule.CooldownSeconds,
		&rule.Enabled,
		&lastNotifiedAt,
		&lastValue,
		&rule.CreatedAt,
		&rule.UpdatedAt,
	); err != nil {
		return alertRuleResponse{}, err
	}
	if lastNotifiedAt.Valid {
		rule.LastNotifiedAt = &lastNotifiedAt.Time
	}
	if lastValue.Valid {
		rule.LastValue = &lastValue.Float64
	}
	return rule, nil
}

func scanIOSDevice(row scanner) (iosDeviceResponse, error) {
	var device iosDeviceResponse
	var deviceName pgtype.Text
	var lastSeenAt pgtype.Timestamptz
	if err := row.Scan(
		&device.ID,
		&device.UserID,
		&device.APNSDeviceToken,
		&device.AppBundleID,
		&device.APNSEnvironment,
		&deviceName,
		&device.Enabled,
		&lastSeenAt,
		&device.CreatedAt,
		&device.UpdatedAt,
	); err != nil {
		return iosDeviceResponse{}, err
	}
	if deviceName.Valid {
		device.DeviceName = &deviceName.String
	}
	if lastSeenAt.Valid {
		device.LastSeenAt = &lastSeenAt.Time
	}
	return device, nil
}

func scanNotificationEvent(row scanner) (notificationEventResponse, error) {
	var event notificationEventResponse
	var alertRuleID pgtype.Int8
	var userID pgtype.Int8
	var value pgtype.Float8
	var threshold pgtype.Float8
	var sentAt pgtype.Timestamptz
	var errorMessage pgtype.Text
	if err := row.Scan(
		&event.ID,
		&alertRuleID,
		&userID,
		&event.MAC,
		&event.Metric,
		&value,
		&threshold,
		&event.TriggeredAt,
		&sentAt,
		&event.Status,
		&errorMessage,
		&event.CreatedAt,
	); err != nil {
		return notificationEventResponse{}, err
	}
	if alertRuleID.Valid {
		event.AlertRuleID = &alertRuleID.Int64
	}
	if userID.Valid {
		event.UserID = &userID.Int64
	}
	if value.Valid {
		event.Value = &value.Float64
	}
	if threshold.Valid {
		event.Threshold = &threshold.Float64
	}
	if sentAt.Valid {
		event.SentAt = &sentAt.Time
	}
	if errorMessage.Valid {
		event.ErrorMessage = &errorMessage.String
	}
	return event, nil
}

func scanTestNotificationEvent(row scanner) (testNotificationEvent, error) {
	var event testNotificationEvent
	var alertRuleID pgtype.Int8
	var userID pgtype.Int8
	var value pgtype.Float8
	var threshold pgtype.Float8
	var sentAt pgtype.Timestamptz
	var errorMessage pgtype.Text
	if err := row.Scan(
		&event.ID,
		&alertRuleID,
		&userID,
		&event.MAC,
		&event.Metric,
		&value,
		&threshold,
		&event.TriggeredAt,
		&sentAt,
		&event.Status,
		&errorMessage,
		&event.CreatedAt,
	); err != nil {
		return testNotificationEvent{}, err
	}
	if alertRuleID.Valid {
		event.AlertRuleID = &alertRuleID.Int64
	}
	if userID.Valid {
		event.UserID = &userID.Int64
	}
	if value.Valid {
		event.Value = &value.Float64
	}
	if threshold.Valid {
		event.Threshold = &threshold.Float64
	}
	if sentAt.Valid {
		event.SentAt = &sentAt.Time
	}
	if errorMessage.Valid {
		event.ErrorMessage = &errorMessage.String
	}
	return event, nil
}

func decodeAlertRuleRequest(w http.ResponseWriter, r *http.Request, partial bool) (alertRuleRequest, bool) {
	var req alertRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return req, false
	}
	req.MAC = strings.ToLower(strings.TrimSpace(req.MAC))
	req.Metric = strings.TrimSpace(req.Metric)
	req.Operator = strings.TrimSpace(req.Operator)
	if !partial {
		if req.MAC == "" || req.Metric == "" || req.Operator == "" || req.Threshold == nil {
			writeError(w, http.StatusBadRequest, "mac, metric, operator, and threshold are required")
			return req, false
		}
	}
	if req.Metric != "" {
		if _, ok := metricColumns[req.Metric]; !ok {
			writeError(w, http.StatusBadRequest, "unsupported metric")
			return req, false
		}
	}
	if req.Operator != "" && !validOperator(req.Operator) {
		writeError(w, http.StatusBadRequest, "unsupported operator")
		return req, false
	}
	if req.CooldownSeconds != nil && *req.CooldownSeconds < 0 {
		writeError(w, http.StatusBadRequest, "cooldown_seconds must be non-negative")
		return req, false
	}
	return req, true
}

func decodeIOSDeviceRequest(w http.ResponseWriter, r *http.Request) (iosDeviceRequest, bool) {
	var req iosDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return req, false
	}
	req.APNSDeviceToken = strings.TrimSpace(req.APNSDeviceToken)
	req.AppBundleID = strings.TrimSpace(req.AppBundleID)
	req.APNSEnvironment = strings.TrimSpace(req.APNSEnvironment)
	if req.APNSDeviceToken == "" || req.AppBundleID == "" || req.APNSEnvironment == "" {
		writeError(w, http.StatusBadRequest, "apns_device_token, app_bundle_id, and apns_environment are required")
		return req, false
	}
	if req.APNSEnvironment != "sandbox" && req.APNSEnvironment != "production" {
		writeError(w, http.StatusBadRequest, "apns_environment must be sandbox or production")
		return req, false
	}
	return req, true
}

func parseAllowedOrigins(raw string) map[string]bool {
	origins := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(item)
		if origin != "" {
			origins[origin] = true
		}
	}
	return origins
}

func parseID(w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func optionalInt64(w http.ResponseWriter, raw string, name string) (int64, bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return 0, false, false
	}
	return value, true, true
}

func validOperator(operator string) bool {
	switch operator {
	case ">", ">=", "<", "<=":
		return true
	default:
		return false
	}
}

func validNotificationStatus(status string) bool {
	switch status {
	case "pending", "dry_run", "sent", "failed", "skipped":
		return true
	default:
		return false
	}
}

func floatPtrFromPg(value pgtype.Float8) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		log.Printf("write json response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
