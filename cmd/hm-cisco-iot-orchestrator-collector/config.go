package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func loadConfig() config {
	return config{
		DBDSN:             envString("BLE_DB_DSN", defaultDBDSN),
		APIURL:            envString("CISCO_IOT_ORCH_API_URL", defaultAPIURL),
		MQTTAddr:          envString("CISCO_IOT_ORCH_MQTT_ADDR", defaultMQTTAddr),
		MQTTMaxPacket:     envInt("CISCO_IOT_ORCH_MQTT_MAX_PACKET_BYTES", defaultMQTTMaxPacket),
		OnboardAppID:      envString("CISCO_IOT_ORCH_ONBOARD_APP_ID", defaultOnboardAppID),
		OnboardAPIKey:     envString("CISCO_IOT_ORCH_ONBOARD_API_KEY", ""),
		ControlAppID:      envString("CISCO_IOT_ORCH_CONTROL_APP_ID", defaultControlAppID),
		ControlAPIKey:     envString("CISCO_IOT_ORCH_CONTROL_API_KEY", ""),
		DataAppID:         envString("CISCO_IOT_ORCH_DATA_APP_ID", defaultDataAppID),
		DataAPIKey:        envString("CISCO_IOT_ORCH_DATA_API_KEY", ""),
		Topic:             envString("CISCO_IOT_ORCH_TOPIC", defaultTopic),
		SensorsFile:       envString("BLE_SENSORS_FILE", defaultSensorsFile),
		RegisterDataApp:   envBool("CISCO_IOT_ORCH_REGISTER_DATA_APP", false),
		DryRun:            envBool("CISCO_IOT_ORCH_DRY_RUN", false),
		Debug:             envBool("CISCO_IOT_ORCH_DEBUG", false),
		ReconnectMinDelay: envDuration("CISCO_IOT_ORCH_RECONNECT_MIN_DELAY", defaultReconnectMinDelay),
		ReconnectMaxDelay: envDuration("CISCO_IOT_ORCH_RECONNECT_MAX_DELAY", defaultReconnectMaxDelay),
		StreamHeartbeat:   envDuration("CISCO_IOT_ORCH_STREAM_HEARTBEAT", defaultStreamHeartbeat),
		AggregateFlush:    envDuration("CISCO_IOT_ORCH_AGGREGATE_FLUSH_INTERVAL", defaultAggregateFlush),
		PendingLog:        envDuration("CISCO_IOT_ORCH_PENDING_LOG_INTERVAL", defaultPendingLog),
		TLSSkipVerify:     envBool("CISCO_IOT_ORCH_TLS_SKIP_VERIFY", true),
		ControlMu:         &sync.Mutex{},
	}
}

func loadTargets(path string) (map[string]targetDevice, error) {
	registry, err := loadTargetRegistry(path)
	if err != nil {
		return nil, err
	}
	return registry.Enabled, nil
}

func loadTargetRegistry(path string) (targetRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return targetRegistry{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg targetConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return targetRegistry{}, fmt.Errorf("parse %s: %w", path, err)
	}
	registry := targetRegistry{All: map[string]targetDevice{}, Enabled: map[string]targetDevice{}}
	for _, device := range cfg.Devices {
		device.MAC = normalizeMAC(device.MAC)
		device.Label = strings.TrimSpace(device.Label)
		if device.MAC == "" {
			return targetRegistry{}, errors.New("sensor mac is required")
		}
		if _, duplicate := registry.All[device.MAC]; duplicate {
			return targetRegistry{}, fmt.Errorf("duplicate sensor mac %s", device.MAC)
		}
		if device.Label == "" {
			device.Label = device.MAC
		}
		device.Location = strings.TrimSpace(device.Location)
		if strings.TrimSpace(device.IngestSource) == "" {
			return targetRegistry{}, fmt.Errorf("sensor %s ingest_source is required", device.MAC)
		}
		device.IngestSource = normalizeIngestSource(device.IngestSource)
		if device.IngestSource == "ble" || device.IngestSource == "cisco_spaces" {
			continue
		}
		if device.IngestSource != sensorConnectIngestSource {
			return targetRegistry{}, fmt.Errorf(
				"sensor %s ingest_source %q is not owned by this collector",
				device.MAC, device.IngestSource,
			)
		}
		device.SensorTypeCode = strings.TrimSpace(device.SensorTypeCode)
		device.SensorCategory = strings.TrimSpace(device.SensorCategory)
		registry.All[device.MAC] = device
		if device.Enabled == nil || *device.Enabled {
			registry.Enabled[device.MAC] = device
		}
	}
	return registry, nil
}

func normalizeIngestSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	switch source {
	case "", "cisco_iot_orchestrator", "cisco-iot-orchestrator", "cisco_iot", "cisco-iot", sensorConnectIngestSource:
		return sensorConnectIngestSource
	default:
		return source
	}
}

func envString(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
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

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %d", name, value, fallback)
		return fallback
	}
	return parsed
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
