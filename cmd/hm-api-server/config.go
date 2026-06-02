package main

import (
	"log"
	"os"
	"strings"
	"time"
)

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
