package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultAPIURL       = "https://192.168.67.6:8081"
	defaultControlAppID = "control"
	defaultSensorsFile  = "/etc/home-metrics/sensors.json"

	flowerCareDataService        = "1204"
	flowerCareHistoryService     = "1206"
	flowerCareDataServiceFull    = "00001204-0000-1000-8000-00805f9b34fb"
	flowerCareHistoryServiceFull = "00001206-0000-1000-8000-00805f9b34fb"

	flowerCareModeCharacteristic     = "00001a00-0000-1000-8000-00805f9b34fb"
	flowerCareRealtimeCharacteristic = "00001a01-0000-1000-8000-00805f9b34fb"
	flowerCareBatteryCharacteristic  = "00001a02-0000-1000-8000-00805f9b34fb"
	flowerCareHistoryCommand         = "00001a10-0000-1000-8000-00805f9b34fb"
	flowerCareHistoryData            = "00001a11-0000-1000-8000-00805f9b34fb"
	flowerCareEpoch                  = "00001a12-0000-1000-8000-00805f9b34fb"
)

type probeConfig struct {
	APIURL                      string
	ControlAppID                string
	ControlAPIKey               string
	DeviceID                    string
	SensorsFile                 string
	HistoryLimit                int
	HistoryStart                int
	HistoryOnly                 bool
	ReconnectBeforeHistoryEntry bool
	FullUUIDs                   bool
	ReadDelay                   time.Duration
	TLSSkipVerify               bool
	Timeout                     time.Duration
}

type targetConfig struct {
	Devices []targetDevice `json:"devices"`
}

type targetDevice struct {
	MAC            string             `json:"mac"`
	Label          string             `json:"label"`
	SensorTypeCode string             `json:"sensor_type_code"`
	GATTBattery    *gattBatteryConfig `json:"gatt_battery"`
}

type gattBatteryConfig struct {
	Enabled  *bool  `json:"enabled"`
	DeviceID string `json:"device_id"`
}

type decodedReading struct {
	TemperatureC        float64 `json:"temperature_c"`
	Lux                 uint32  `json:"lux"`
	SoilMoisturePercent uint8   `json:"soil_moisture_percent"`
	ConductivityUSCM    uint16  `json:"conductivity_us_cm"`
}

type historyEntry struct {
	Index               int       `json:"index"`
	DeviceTimestampSec  uint32    `json:"device_timestamp_sec"`
	EstimatedTS         time.Time `json:"estimated_ts"`
	RawHex              string    `json:"raw_hex"`
	TemperatureC        float64   `json:"temperature_c"`
	Lux                 uint32    `json:"lux"`
	SoilMoisturePercent uint8     `json:"soil_moisture_percent"`
	ConductivityUSCM    uint16    `json:"conductivity_us_cm"`
}

type probeResult struct {
	DeviceID                 string          `json:"device_id"`
	DeviceEpochSec           uint32          `json:"device_epoch_sec"`
	HostReadTime             time.Time       `json:"host_read_time"`
	BatteryRawHex            string          `json:"battery_raw_hex,omitempty"`
	BatteryPercent           *uint8          `json:"battery_percent,omitempty"`
	Firmware                 string          `json:"firmware,omitempty"`
	RealtimeRawHex           string          `json:"realtime_raw_hex,omitempty"`
	Realtime                 *decodedReading `json:"realtime,omitempty"`
	HistoryInitRawHex        string          `json:"history_init_raw_hex,omitempty"`
	HistoryEntryCount        uint16          `json:"history_entry_count"`
	HistoryEntriesRead       int             `json:"history_entries_read"`
	HistoryEntries           []historyEntry  `json:"history_entries"`
	HistoryReadStoppedReason string          `json:"history_read_stopped_reason,omitempty"`
}

func main() {
	cfg := loadProbeConfig()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	if cfg.DeviceID == "" {
		deviceID, err := deviceIDFromSensorsFile(cfg.SensorsFile)
		if err != nil {
			fatal(err)
		}
		cfg.DeviceID = deviceID
	}
	if cfg.ControlAPIKey == "" {
		fatal(errors.New("CISCO_IOT_ORCH_CONTROL_API_KEY is required"))
	}

	client := &controlClient{cfg: cfg, httpClient: httpClient(cfg)}
	result, err := client.probe(ctx)
	if err != nil {
		fatal(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fatal(err)
	}
}

func loadProbeConfig() probeConfig {
	var cfg probeConfig
	flag.StringVar(&cfg.APIURL, "api-url", envString("CISCO_IOT_ORCH_API_URL", defaultAPIURL), "Cisco IoT Orchestrator API URL")
	flag.StringVar(&cfg.ControlAppID, "control-app", envString("CISCO_IOT_ORCH_CONTROL_APP_ID", defaultControlAppID), "control app ID")
	flag.StringVar(&cfg.ControlAPIKey, "control-api-key", envString("CISCO_IOT_ORCH_CONTROL_API_KEY", ""), "control API key")
	flag.StringVar(&cfg.DeviceID, "device-id", envString("FLOWERCARE_DEVICE_ID", ""), "Cisco Sensor Connect BLE device ID")
	flag.StringVar(&cfg.SensorsFile, "sensors-file", envString("BLE_SENSORS_FILE", defaultSensorsFile), "sensors.json path used when device-id is omitted")
	flag.IntVar(&cfg.HistoryLimit, "history-limit", envInt("FLOWERCARE_HISTORY_LIMIT", 8), "maximum history entries to read")
	flag.IntVar(&cfg.HistoryStart, "history-start", envInt("FLOWERCARE_HISTORY_START", 0), "first history entry index to request")
	flag.BoolVar(&cfg.HistoryOnly, "history-only", envBool("FLOWERCARE_HISTORY_ONLY", false), "skip battery and real-time reads")
	flag.BoolVar(&cfg.ReconnectBeforeHistoryEntry, "reconnect-before-history-entry", envBool("FLOWERCARE_RECONNECT_BEFORE_HISTORY_ENTRY", false), "disconnect and reconnect after history init before selecting entries")
	flag.BoolVar(&cfg.FullUUIDs, "full-uuids", envBool("FLOWERCARE_FULL_UUIDS", false), "use full 128-bit service UUIDs in Cisco control requests")
	flag.DurationVar(&cfg.ReadDelay, "read-delay", envDuration("FLOWERCARE_GATT_READ_DELAY", 500*time.Millisecond), "delay after GATT writes before reading")
	flag.BoolVar(&cfg.TLSSkipVerify, "tls-skip-verify", envBool("CISCO_IOT_ORCH_TLS_SKIP_VERIFY", true), "skip TLS verification")
	flag.DurationVar(&cfg.Timeout, "timeout", envDuration("FLOWERCARE_GATT_TIMEOUT", 90*time.Second), "overall probe timeout")
	flag.Parse()
	if cfg.HistoryLimit < 0 {
		cfg.HistoryLimit = 0
	}
	if cfg.HistoryStart < 0 {
		cfg.HistoryStart = 0
	}
	return cfg
}

func deviceIDFromSensorsFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read sensors file: %w", err)
	}
	var cfg targetConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse sensors file: %w", err)
	}
	for _, device := range cfg.Devices {
		if device.GATTBattery == nil {
			continue
		}
		if device.GATTBattery.Enabled != nil && !*device.GATTBattery.Enabled {
			continue
		}
		if strings.TrimSpace(device.SensorTypeCode) == "xiaomi_flower_care" && strings.TrimSpace(device.GATTBattery.DeviceID) != "" {
			return strings.TrimSpace(device.GATTBattery.DeviceID), nil
		}
	}
	return "", errors.New("no xiaomi_flower_care device_id found in sensors file")
}

type controlClient struct {
	cfg        probeConfig
	httpClient *http.Client
}

func (c *controlClient) probe(ctx context.Context) (probeResult, error) {
	if err := c.connect(ctx); err != nil {
		return probeResult{}, err
	}
	defer func() {
		_, _ = c.post(context.Background(), "/control/connectivity/disconnect", c.baseBody())
	}()

	hostReadTime := time.Now()
	result := probeResult{
		DeviceID:     c.cfg.DeviceID,
		HostReadTime: hostReadTime,
	}

	if !c.cfg.HistoryOnly {
		if payload, err := c.read(ctx, c.dataServiceID(), flowerCareBatteryCharacteristic); err == nil {
			result.BatteryRawHex = strings.ToLower(hex.EncodeToString(payload))
			if len(payload) > 0 {
				battery := payload[0]
				result.BatteryPercent = &battery
			}
			if len(payload) >= 3 {
				result.Firmware = string(payload[2:])
			}
		} else {
			return result, fmt.Errorf("read battery: %w", err)
		}

		if err := c.write(ctx, c.dataServiceID(), flowerCareModeCharacteristic, []byte{0xa0, 0x1f}); err != nil {
			return result, fmt.Errorf("write realtime mode: %w", err)
		}
		c.waitAfterWrite(ctx)
		if payload, err := c.read(ctx, c.dataServiceID(), flowerCareRealtimeCharacteristic); err == nil {
			result.RealtimeRawHex = strings.ToLower(hex.EncodeToString(payload))
			reading, err := decodeRealtime(payload)
			if err != nil {
				return result, fmt.Errorf("decode realtime: %w", err)
			}
			result.Realtime = &reading
		} else {
			return result, fmt.Errorf("read realtime: %w", err)
		}
	}

	epochPayload, err := c.read(ctx, c.historyServiceID(), flowerCareEpoch)
	if err != nil {
		return result, fmt.Errorf("read device epoch: %w", err)
	}
	if len(epochPayload) < 4 {
		return result, fmt.Errorf("device epoch payload too short: % x", epochPayload)
	}
	result.DeviceEpochSec = binary.LittleEndian.Uint32(epochPayload[:4])
	result.HostReadTime = time.Now()

	if err := c.write(ctx, c.historyServiceID(), flowerCareHistoryCommand, []byte{0xa0, 0x00, 0x00}); err != nil {
		return result, fmt.Errorf("write history init: %w", err)
	}
	c.waitAfterWrite(ctx)
	initPayload, err := c.read(ctx, c.historyServiceID(), flowerCareHistoryData)
	if err != nil {
		return result, fmt.Errorf("read history init: %w", err)
	}
	result.HistoryInitRawHex = strings.ToLower(hex.EncodeToString(initPayload))
	if len(initPayload) < 2 {
		return result, fmt.Errorf("history init payload too short: % x", initPayload)
	}
	result.HistoryEntryCount = binary.LittleEndian.Uint16(initPayload[:2])

	limit := int(result.HistoryEntryCount)
	if c.cfg.HistoryLimit < limit {
		limit = c.cfg.HistoryLimit
	}
	if c.cfg.ReconnectBeforeHistoryEntry && limit > 0 {
		if _, err := c.post(ctx, "/control/connectivity/disconnect", c.baseBody()); err != nil {
			result.HistoryReadStoppedReason = fmt.Sprintf("disconnect before entries: %v", err)
			return result, nil
		}
		c.waitAfterWrite(ctx)
		if err := c.connect(ctx); err != nil {
			result.HistoryReadStoppedReason = fmt.Sprintf("reconnect before entries: %v", err)
			return result, nil
		}
	}
	for i := 0; i < limit; i++ {
		entryIndex := c.cfg.HistoryStart + i
		cmd := []byte{0xa1, byte(entryIndex), byte(entryIndex >> 8)}
		if err := c.write(ctx, c.historyServiceID(), flowerCareHistoryCommand, cmd); err != nil {
			result.HistoryReadStoppedReason = fmt.Sprintf("write entry %d: %v", entryIndex, err)
			break
		}
		c.waitAfterWrite(ctx)
		payload, err := c.read(ctx, c.historyServiceID(), flowerCareHistoryData)
		if err != nil {
			result.HistoryReadStoppedReason = fmt.Sprintf("read entry %d: %v", entryIndex, err)
			break
		}
		entry, err := decodeHistoryEntry(entryIndex, payload, result.DeviceEpochSec, result.HostReadTime)
		if err != nil {
			result.HistoryReadStoppedReason = fmt.Sprintf("decode entry %d: %v", entryIndex, err)
			break
		}
		result.HistoryEntries = append(result.HistoryEntries, entry)
	}
	result.HistoryEntriesRead = len(result.HistoryEntries)
	return result, nil
}

func (c *controlClient) waitAfterWrite(ctx context.Context) {
	if c.cfg.ReadDelay <= 0 {
		return
	}
	timer := time.NewTimer(c.cfg.ReadDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (c *controlClient) connect(ctx context.Context) error {
	body := c.baseBody()
	services := []map[string]string{{"serviceID": c.historyServiceID()}}
	if !c.cfg.HistoryOnly {
		services = append([]map[string]string{{"serviceID": c.dataServiceID()}}, services...)
	}
	body["ble"] = map[string]any{
		"services": services,
	}
	_, err := c.post(ctx, "/control/connectivity/connect", body)
	return err
}

func (c *controlClient) dataServiceID() string {
	if c.cfg.FullUUIDs {
		return flowerCareDataServiceFull
	}
	return flowerCareDataService
}

func (c *controlClient) historyServiceID() string {
	if c.cfg.FullUUIDs {
		return flowerCareHistoryServiceFull
	}
	return flowerCareHistoryService
}

func (c *controlClient) read(ctx context.Context, serviceID string, characteristicID string) ([]byte, error) {
	body := c.baseBody()
	body["ble"] = map[string]any{
		"serviceID":        serviceID,
		"characteristicID": characteristicID,
	}
	response, err := c.post(ctx, "/control/data/read", body)
	if err != nil {
		return nil, err
	}
	var value struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(response, &value); err != nil {
		return nil, err
	}
	return decodeHexValue(value.Value)
}

func (c *controlClient) write(ctx context.Context, serviceID string, characteristicID string, payload []byte) error {
	body := c.baseBody()
	body["ble"] = map[string]any{
		"serviceID":        serviceID,
		"characteristicID": characteristicID,
	}
	body["value"] = strings.ToLower(hex.EncodeToString(payload))
	_, err := c.post(ctx, "/control/data/write", body)
	return err
}

func (c *controlClient) baseBody() map[string]any {
	return map[string]any{
		"technology": "ble",
		"id":         strings.TrimSpace(c.cfg.DeviceID),
		"controlApp": c.cfg.ControlAppID,
	}
}

func (c *controlClient) post(ctx context.Context, path string, body map[string]any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.APIURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", c.cfg.ControlAPIKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limited, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s status=%s body=%s", path, resp.Status, string(limited))
	}
	var result struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(limited, &result); err == nil && strings.EqualFold(result.Status, "FAILURE") {
		if result.Reason == "" {
			result.Reason = string(limited)
		}
		return nil, fmt.Errorf("%s failed: %s", path, result.Reason)
	}
	return limited, nil
}

func decodeRealtime(payload []byte) (decodedReading, error) {
	if len(payload) < 10 {
		return decodedReading{}, fmt.Errorf("payload too short: % x", payload)
	}
	tempRaw := int16(binary.LittleEndian.Uint16(payload[0:2]))
	return decodedReading{
		TemperatureC:        float64(tempRaw) / 10,
		Lux:                 binary.LittleEndian.Uint32(payload[3:7]),
		SoilMoisturePercent: payload[7],
		ConductivityUSCM:    binary.LittleEndian.Uint16(payload[8:10]),
	}, nil
}

func decodeHistoryEntry(index int, payload []byte, deviceEpoch uint32, hostReadTime time.Time) (historyEntry, error) {
	if len(payload) < 14 {
		return historyEntry{}, fmt.Errorf("payload too short: % x", payload)
	}
	deviceTS := binary.LittleEndian.Uint32(payload[0:4])
	if deviceTS <= 1 {
		return historyEntry{}, fmt.Errorf("invalid device timestamp: %d raw=% x", deviceTS, payload)
	}
	if deviceTS > deviceEpoch {
		return historyEntry{}, fmt.Errorf("device timestamp exceeds device epoch: timestamp=%d epoch=%d raw=% x", deviceTS, deviceEpoch, payload)
	}
	tempRaw := int16(binary.LittleEndian.Uint16(payload[4:6]))
	estimated := hostReadTime
	if deviceEpoch >= deviceTS {
		estimated = hostReadTime.Add(-time.Duration(deviceEpoch-deviceTS) * time.Second)
	}
	return historyEntry{
		Index:               index,
		DeviceTimestampSec:  deviceTS,
		EstimatedTS:         estimated.Truncate(time.Minute),
		RawHex:              strings.ToLower(hex.EncodeToString(payload)),
		TemperatureC:        float64(tempRaw) / 10,
		Lux:                 binary.LittleEndian.Uint32(payload[7:11]),
		SoilMoisturePercent: payload[11],
		ConductivityUSCM:    binary.LittleEndian.Uint16(payload[12:14]),
	}, nil
}

func decodeHexValue(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, ":", "")
	value = strings.TrimPrefix(value, "0x")
	if value == "" {
		return nil, errors.New("empty hex value")
	}
	data, err := hex.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func httpClient(cfg probeConfig) *http.Client {
	if !cfg.TLSSkipVerify {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Lab IoT Orchestrator uses an IP-address HTTPS endpoint.
		},
		Timeout: cfg.Timeout,
	}
}

func envString(name string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
