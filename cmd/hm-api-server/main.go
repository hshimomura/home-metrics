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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultAddr  = ":8080"
	defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"
)

type apiServer struct {
	db             *pgxpool.Pool
	apiToken       string
	allowedOrigins map[string]bool
}

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
	addr := os.Getenv("API_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	apiToken := strings.TrimSpace(os.Getenv("API_TOKEN"))
	if envBool("API_REQUIRE_TOKEN", false) && apiToken == "" {
		log.Fatal("API_TOKEN is required when API_REQUIRE_TOKEN=true")
	}
	allowedOrigins := parseAllowedOrigins(os.Getenv("API_ALLOWED_ORIGINS"))

	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close()

	server := &http.Server{
		Addr:              addr,
		Handler:           newRouter(&apiServer{db: db, apiToken: apiToken, allowedOrigins: allowedOrigins}),
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

	log.Printf("api server started addr=%s db=%s", addr, redactDSN(dsn))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen api server: %v", err)
	}
}

func newRouter(api *apiServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", api.handleWebIndex)
	mux.HandleFunc("GET /admin", api.handleWebAdmin)
	mux.HandleFunc("GET /admin.html", api.handleWebAdmin)
	mux.HandleFunc("GET /api/health", api.handleHealth)
	mux.HandleFunc("GET /api/health/details", api.handleHealthDetails)
	mux.HandleFunc("GET /api/admin/schema", api.handleSchema)
	mux.HandleFunc("GET /api/admin/cisco-spaces-firehose", api.handleCiscoSpacesFirehoseStatus)
	mux.HandleFunc("GET /api/admin/collector-status", api.handleCollectorStatus)
	mux.HandleFunc("GET /api/devices", api.handleDevices)
	mux.HandleFunc("GET /api/devices/{mac}/latest", api.handleDeviceLatest)
	mux.HandleFunc("GET /api/devices/{mac}/series", api.handleDeviceSeries)
	mux.HandleFunc("GET /api/energy/latest", api.handleEnergyLatest)
	mux.HandleFunc("GET /api/energy/series", api.handleEnergySeries)
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
	api.serveWebFile(w, r, "web/index.html", "index.html")
}

func (api *apiServer) handleWebAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" && r.URL.Path != "/admin.html" {
		http.NotFound(w, r)
		return
	}
	api.serveWebFile(w, r, "web/admin.html", "admin.html")
}

func (api *apiServer) serveWebFile(w http.ResponseWriter, r *http.Request, path string, name string) {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("open %s: %v", path, err)
		http.Error(w, "web page unavailable", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		log.Printf("stat %s: %v", path, err)
		http.Error(w, "web page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, name, info.ModTime(), file)
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

func envBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("invalid %s=%q, using %t", name, value, fallback)
		return fallback
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %s", name, value, fallback)
		return fallback
	}
	return parsed
}

func redactDSN(dsn string) string {
	if strings.TrimSpace(dsn) == "" {
		return ""
	}
	return "configured"
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
