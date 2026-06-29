package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type apiServer struct {
	db             *pgxpool.Pool
	apiToken       string
	allowedOrigins map[string]bool
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
	mux.HandleFunc("GET /api/sensor-alert-rules", api.handleSensorAlertRules)
	mux.HandleFunc("POST /api/sensor-alert-rules", api.handleCreateSensorAlertRule)
	mux.HandleFunc("PUT /api/sensor-alert-rules/{id}", api.handleUpdateSensorAlertRule)
	mux.HandleFunc("DELETE /api/sensor-alert-rules/{id}", api.handleDeleteSensorAlertRule)
	mux.HandleFunc("GET /api/sensor-alerts", api.handleSensorAlerts)
	mux.HandleFunc("GET /api/sensor-alert-events", api.handleSensorAlertEvents)
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
