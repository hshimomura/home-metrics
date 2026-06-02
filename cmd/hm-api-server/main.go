package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultAddr  = ":8080"
	defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"
)

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

	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close()

	api := &apiServer{
		db:             db,
		apiToken:       apiToken,
		allowedOrigins: parseAllowedOrigins(os.Getenv("API_ALLOWED_ORIGINS")),
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           newRouter(api),
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
