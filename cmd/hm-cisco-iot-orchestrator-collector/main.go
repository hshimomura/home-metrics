package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"home-metrics/internal/collectorstatus"

	"github.com/jackc/pgx/v5"
)

const (
	defaultDBDSN             = "dbname=ble_sensors host=/var/run/postgresql"
	defaultSensorsFile       = "/etc/home-metrics/sensors.json"
	defaultAPIURL            = "https://192.168.67.6:8081"
	defaultMQTTAddr          = "192.168.67.6:41883"
	defaultOnboardAppID      = "onboard"
	defaultControlAppID      = "control"
	defaultDataAppID         = "data"
	defaultTopic             = "ioslab/home-metrics/ble/advertisements/v1"
	defaultReconnectMinDelay = time.Second
	defaultReconnectMaxDelay = time.Minute
	defaultStreamHeartbeat   = time.Minute
)

type config struct {
	DBDSN             string
	APIURL            string
	MQTTAddr          string
	OnboardAppID      string
	OnboardAPIKey     string
	ControlAppID      string
	ControlAPIKey     string
	DataAppID         string
	DataAPIKey        string
	Topic             string
	SensorsFile       string
	RegisterDataApp   bool
	DryRun            bool
	Debug             bool
	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration
	StreamHeartbeat   time.Duration
}

type targetDevice struct {
	MAC        string `json:"mac"`
	Label      string `json:"label"`
	SensorCategory string `json:"sensor_category"`
	Location   string `json:"location"`
	Enabled    *bool  `json:"enabled"`
}

type targetConfig struct {
	Devices []targetDevice `json:"devices"`
}

type bleReading struct {
	TS              time.Time
	SensorMAC       string
	Label           string
	RSSI            *float64
	TemperatureC    *float64
	HumidityPercent *float64
	BatteryPercent  *float64
	PressureHPa     *float64
	CO2PPM          *float64
	Lux             *float64
	ETVOC           *float64
}

type aggregate struct {
	SensorMAC       string
	Label           string
	Window          time.Time
	RSSI            []float64
	TemperatureC    []float64
	HumidityPercent []float64
	BatteryPercent  []float64
	PressureHPa     []float64
	CO2PPM          []float64
	Lux             []float64
	ETVOC           []float64
	LastComparable  string
}

type collector struct {
	db      *pgx.Conn
	windows map[string]*aggregate
}

type statusReporter struct {
	mu     sync.Mutex
	db     *pgx.Conn
	target collectorstatus.Target
}

type dataSubscription struct {
	DeviceID    string
	Data        []byte
	TS          time.Time
	APMAC       string
	BLEMAC      string
	RSSI        *int32
	Application string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := loadConfig()
	if source := strings.ToLower(envString("SENSOR_INGEST_SOURCE", "cisco_iot_orchestrator")); source != "cisco_iot_orchestrator" && source != "cisco-iot-orchestrator" && source != "cisco_iot" && source != "cisco-iot" {
		log.Printf("Cisco IoT Orchestrator collector disabled by SENSOR_INGEST_SOURCE=%s", source)
		return
	}
	if cfg.DataAPIKey == "" {
		log.Fatal("CISCO_IOT_ORCH_DATA_API_KEY is required")
	}
	targets, err := loadTargets(cfg.SensorsFile)
	if err != nil {
		log.Fatalf("load BLE sensors: %v", err)
	}

	var db *pgx.Conn
	if !cfg.DryRun {
		db, err = pgx.Connect(ctx, cfg.DBDSN)
		if err != nil {
			log.Fatalf("connect database: %v", err)
		}
		defer db.Close(context.Background())
		if err := ensureDevices(ctx, db, targets); err != nil {
			log.Fatalf("ensure devices: %v", err)
		}
	}

	reporter := &statusReporter{
		db: db,
		target: collectorstatus.Target{
			CollectorName: "hm-cisco-iot-orchestrator-collector",
			TargetType:    "mqtt",
			TargetKey:     cfg.MQTTAddr + "/" + cfg.Topic,
		},
	}
	c := &collector{db: db, windows: map[string]*aggregate{}}

	if cfg.RegisterDataApp {
		if err := registerDataApp(ctx, cfg); err != nil {
			reporter.MarkFailure(ctx, err)
			log.Fatalf("register data app: %v", err)
		}
	}

	log.Printf("Cisco IoT Orchestrator collector started mqtt=%s topic=%s data_app=%s control_app=%s dry_run=%t", cfg.MQTTAddr, cfg.Topic, cfg.DataAppID, cfg.ControlAppID, cfg.DryRun)
	runWithReconnect(ctx, cfg, targets, c, reporter)
	if err := c.flushAll(context.Background()); err != nil {
		log.Printf("flush on shutdown: %v", err)
	}
}

func runWithReconnect(ctx context.Context, cfg config, targets map[string]targetDevice, c *collector, reporter *statusReporter) {
	delay := cfg.ReconnectMinDelay
	for ctx.Err() == nil {
		err := runMQTT(ctx, cfg, func(topic string, payload []byte) {
			if cfg.Debug {
				log.Printf("mqtt message topic=%s bytes=%d", topic, len(payload))
			}
			readings, err := decodeDataBatch(payload, targets)
			if err != nil {
				log.Printf("decode MQTT protobuf: %v", err)
				reporter.MarkFailure(ctx, err)
				return
			}
			if len(readings) == 0 {
				reporter.MarkSuccess(ctx)
				return
			}
			for _, reading := range readings {
				c.add(reading)
			}
			flushed, err := c.flushCompleted(ctx, time.Now().Truncate(time.Minute))
			if err != nil {
				log.Printf("flush Cisco IoT readings: %v", err)
				reporter.MarkFailure(ctx, err)
				return
			}
			if flushed > 0 {
				reporter.MarkDataSuccess(ctx)
			} else {
				reporter.MarkSuccess(ctx)
			}
		})
		if ctx.Err() != nil {
			return
		}
		log.Printf("MQTT stream ended: %v; reconnecting in %s", err, delay)
		reporter.MarkFailure(ctx, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > cfg.ReconnectMaxDelay {
			delay = cfg.ReconnectMaxDelay
		}
	}
}

func registerDataApp(ctx context.Context, cfg config) error {
	body := map[string]any{
		"dataApps":   []map[string]string{{"dataAppID": cfg.DataAppID}},
		"topic":      cfg.Topic,
		"dataFormat": "default",
		"controlApp": cfg.ControlAppID,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(cfg.APIURL, "/") + "/control/registration/registerDataApp"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", cfg.ControlAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("register data app status=%s body=%s", resp.Status, string(limited))
	}
	var result struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(limited, &result); err == nil && strings.EqualFold(result.Status, "FAILURE") {
		if result.Reason == "" {
			result.Reason = string(limited)
		}
		return fmt.Errorf("register data app failed: %s", result.Reason)
	}
	log.Printf("registered Cisco IoT data app app=%s control_app=%s topic=%s", cfg.DataAppID, cfg.ControlAppID, cfg.Topic)
	return nil
}

func loadConfig() config {
	return config{
		DBDSN:             envString("BLE_DB_DSN", defaultDBDSN),
		APIURL:            envString("CISCO_IOT_ORCH_API_URL", defaultAPIURL),
		MQTTAddr:          envString("CISCO_IOT_ORCH_MQTT_ADDR", defaultMQTTAddr),
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
	}
}

func loadTargets(path string) (map[string]targetDevice, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg targetConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	targets := map[string]targetDevice{}
	for _, device := range cfg.Devices {
		device.MAC = normalizeMAC(device.MAC)
		device.Label = strings.TrimSpace(device.Label)
		if device.MAC == "" {
			return nil, errors.New("sensor mac is required")
		}
		if device.Label == "" {
			device.Label = device.MAC
		}
		if device.Enabled != nil && !*device.Enabled {
			continue
		}
		targets[device.MAC] = device
	}
	return targets, nil
}

func decodeDataBatch(payload []byte, targets map[string]targetDevice) ([]bleReading, error) {
	messages, err := parseDataBatch(payload)
	if err != nil {
		return nil, err
	}
	var readings []bleReading
	for _, msg := range messages {
		mac := normalizeMAC(msg.BLEMAC)
		if mac == "" {
			mac = normalizeMAC(msg.DeviceID)
		}
		target, ok := targets[mac]
		if !ok {
			continue
		}
		decoded := decodeBLEPayload(msg.Data)
		if decoded.empty() {
			continue
		}
		if msg.TS.IsZero() {
			decoded.TS = time.Now()
		} else {
			decoded.TS = msg.TS
		}
		decoded.SensorMAC = mac
		decoded.Label = target.Label
		if msg.RSSI != nil {
			decoded.RSSI = floatPtr(float64(*msg.RSSI))
		}
		readings = append(readings, decoded)
	}
	return readings, nil
}

func parseDataBatch(data []byte) ([]dataSubscription, error) {
	var messages []dataSubscription
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return nil, err
		}
		data = rest
		if field != 1 || wire != 2 {
			data, err = skipProtoValue(wire, data)
			if err != nil {
				return nil, err
			}
			continue
		}
		item, rest, err := consumeBytes(data)
		if err != nil {
			return nil, err
		}
		data = rest
		msg, err := parseDataSubscription(item)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

func parseDataSubscription(data []byte) (dataSubscription, error) {
	var msg dataSubscription
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return msg, err
		}
		data = rest
		switch field {
		case 1:
			msg.DeviceID, data, err = consumeString(data)
		case 2:
			msg.Data, data, err = consumeBytes(data)
		case 3:
			var value []byte
			value, data, err = consumeBytes(data)
			msg.TS = parseTimestamp(value)
		case 4:
			msg.APMAC, data, err = consumeString(data)
		case 12:
			var value []byte
			value, data, err = consumeBytes(data)
			msg.BLEMAC, msg.RSSI = parseBLEAdvertisement(value)
		case 16:
			var value []byte
			value, data, err = consumeBytes(data)
			msg.Application = parseApplicationEvent(value)
		default:
			data, err = skipProtoValue(wire, data)
		}
		if err != nil {
			return msg, err
		}
	}
	return msg, nil
}

func parseTimestamp(data []byte) time.Time {
	var seconds int64
	var nanos int32
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return time.Time{}
		}
		data = rest
		switch field {
		case 1:
			value, rest, err := consumeVarint(data)
			if err != nil {
				return time.Time{}
			}
			seconds = int64(value)
			data = rest
		case 2:
			value, rest, err := consumeVarint(data)
			if err != nil {
				return time.Time{}
			}
			nanos = int32(value)
			data = rest
		default:
			data, err = skipProtoValue(wire, data)
			if err != nil {
				return time.Time{}
			}
		}
	}
	if seconds == 0 && nanos == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, int64(nanos)).UTC()
}

func parseBLEAdvertisement(data []byte) (string, *int32) {
	var mac string
	var rssi *int32
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return mac, rssi
		}
		data = rest
		switch field {
		case 1:
			mac, data, err = consumeString(data)
		case 2:
			var value uint64
			value, data, err = consumeVarint(data)
			signed := int32(value)
			rssi = &signed
		default:
			data, err = skipProtoValue(wire, data)
		}
		if err != nil {
			return mac, rssi
		}
	}
	return mac, rssi
}

func parseApplicationEvent(data []byte) string {
	for len(data) > 0 {
		field, wire, rest, err := consumeKey(data)
		if err != nil {
			return ""
		}
		data = rest
		if field == 1 && wire == 2 {
			value, _, err := consumeString(data)
			if err != nil {
				return ""
			}
			return value
		}
		data, err = skipProtoValue(wire, data)
		if err != nil {
			return ""
		}
	}
	return ""
}

func consumeKey(data []byte) (uint64, uint64, []byte, error) {
	key, rest, err := consumeVarint(data)
	if err != nil {
		return 0, 0, data, err
	}
	return key >> 3, key & 0x7, rest, nil
}

func consumeVarint(data []byte) (uint64, []byte, error) {
	var value uint64
	for i := 0; i < len(data) && i < 10; i++ {
		b := data[i]
		value |= uint64(b&0x7f) << uint(7*i)
		if b < 0x80 {
			return value, data[i+1:], nil
		}
	}
	return 0, data, io.ErrUnexpectedEOF
}

func consumeBytes(data []byte) ([]byte, []byte, error) {
	length, rest, err := consumeVarint(data)
	if err != nil {
		return nil, data, err
	}
	if length > uint64(len(rest)) {
		return nil, data, io.ErrUnexpectedEOF
	}
	return rest[:length], rest[length:], nil
}

func consumeString(data []byte) (string, []byte, error) {
	value, rest, err := consumeBytes(data)
	if err != nil {
		return "", data, err
	}
	return string(value), rest, nil
}

func skipProtoValue(wire uint64, data []byte) ([]byte, error) {
	switch wire {
	case 0:
		_, rest, err := consumeVarint(data)
		return rest, err
	case 1:
		if len(data) < 8 {
			return data, io.ErrUnexpectedEOF
		}
		return data[8:], nil
	case 2:
		_, rest, err := consumeBytes(data)
		return rest, err
	case 5:
		if len(data) < 4 {
			return data, io.ErrUnexpectedEOF
		}
		return data[4:], nil
	default:
		return data, fmt.Errorf("unsupported protobuf wire type %d", wire)
	}
}

func runMQTT(ctx context.Context, cfg config, onMessage func(topic string, payload []byte)) error {
	conn, err := net.DialTimeout("tcp", cfg.MQTTAddr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	clientID := "home-metrics-" + randomHex(4)
	if err := mqttConnect(conn, clientID, cfg.DataAppID, cfg.DataAPIKey); err != nil {
		return err
	}
	if err := mqttSubscribe(conn, 1, cfg.Topic); err != nil {
		return err
	}
	heartbeat := time.NewTicker(cfg.StreamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			if err := writeMQTTPacket(conn, 0xc0, nil); err != nil {
				return err
			}
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		packetType, payload, err := readMQTTPacket(conn)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		if packetType == 3 {
			topic, body, err := parsePublish(payload)
			if err != nil {
				return err
			}
			onMessage(topic, body)
		}
	}
}

func mqttConnect(conn net.Conn, clientID string, username string, password string) error {
	var variable bytes.Buffer
	writeMQTTString(&variable, "MQTT")
	variable.WriteByte(4)
	variable.WriteByte(0x80 | 0x40 | 0x02)
	_ = binary.Write(&variable, binary.BigEndian, uint16(60))
	writeMQTTString(&variable, clientID)
	writeMQTTString(&variable, username)
	writeMQTTString(&variable, password)
	if err := writeMQTTPacket(conn, 0x10, variable.Bytes()); err != nil {
		return err
	}
	packetType, payload, err := readMQTTPacket(conn)
	if err != nil {
		return err
	}
	if packetType != 2 || len(payload) < 2 {
		return fmt.Errorf("unexpected MQTT CONNACK packet type=%d", packetType)
	}
	if payload[1] != 0 {
		return fmt.Errorf("MQTT connect rejected code=%d", payload[1])
	}
	return nil
}

func mqttSubscribe(conn net.Conn, packetID uint16, topic string) error {
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, packetID)
	writeMQTTString(&payload, topic)
	payload.WriteByte(0)
	if err := writeMQTTPacket(conn, 0x82, payload.Bytes()); err != nil {
		return err
	}
	packetType, body, err := readMQTTPacket(conn)
	if err != nil {
		return err
	}
	if packetType != 9 || len(body) < 3 {
		return fmt.Errorf("unexpected MQTT SUBACK packet type=%d", packetType)
	}
	if body[len(body)-1] == 0x80 {
		return errors.New("MQTT subscribe rejected")
	}
	return nil
}

func parsePublish(payload []byte) (string, []byte, error) {
	if len(payload) < 2 {
		return "", nil, io.ErrUnexpectedEOF
	}
	topicLen := int(binary.BigEndian.Uint16(payload[:2]))
	if len(payload) < 2+topicLen {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(payload[2 : 2+topicLen]), payload[2+topicLen:], nil
}

func writeMQTTPacket(conn net.Conn, header byte, payload []byte) error {
	var frame bytes.Buffer
	frame.WriteByte(header)
	writeRemainingLength(&frame, len(payload))
	frame.Write(payload)
	_, err := conn.Write(frame.Bytes())
	return err
}

func readMQTTPacket(conn net.Conn) (int, []byte, error) {
	header := make([]byte, 1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	length, err := readRemainingLength(conn)
	if err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return int(header[0] >> 4), payload, nil
}

func writeRemainingLength(buf *bytes.Buffer, length int) {
	for {
		encoded := byte(length % 128)
		length /= 128
		if length > 0 {
			encoded |= 128
		}
		buf.WriteByte(encoded)
		if length == 0 {
			return
		}
	}
}

func readRemainingLength(r io.Reader) (int, error) {
	var multiplier int = 1
	var value int
	for i := 0; i < 4; i++ {
		var encoded [1]byte
		if _, err := io.ReadFull(r, encoded[:]); err != nil {
			return 0, err
		}
		value += int(encoded[0]&127) * multiplier
		if encoded[0]&128 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, errors.New("malformed MQTT remaining length")
}

func writeMQTTString(buf *bytes.Buffer, value string) {
	_ = binary.Write(buf, binary.BigEndian, uint16(len(value)))
	buf.WriteString(value)
}

func randomHex(bytesLen int) string {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

func decodeBLEPayload(data []byte) bleReading {
	reading := decodeServiceData(hex.EncodeToString(data))
	for _, serviceData := range serviceDataFromAdvertisement(data) {
		reading.merge(decodeServiceData(hex.EncodeToString(serviceData)))
	}
	return reading
}

func serviceDataFromAdvertisement(data []byte) [][]byte {
	var out [][]byte
	for len(data) >= 2 {
		length := int(data[0])
		if length == 0 {
			break
		}
		if length+1 > len(data) {
			break
		}
		adType := data[1]
		adData := data[2 : length+1]
		if adType == 0x16 && len(adData) >= 2 {
			uuid := binary.LittleEndian.Uint16(adData[:2])
			if uuid == 0xfe6a || uuid == 0xffe1 || uuid == 0xfeaa {
				out = append(out, adData[2:])
			}
		}
		data = data[length+1:]
	}
	return out
}

func decodeServiceData(payloadHex string) bleReading {
	data, err := hex.DecodeString(payloadHex)
	if err != nil {
		return bleReading{}
	}
	r := bleReading{}
	if len(data) >= 6 && data[0] == 0xa1 && data[1] == 0x01 {
		r.BatteryPercent = floatPtr(float64(data[2]))
		tempRaw := int8(data[3])
		r.TemperatureC = floatPtr(float64(tempRaw))
		humidity := float64(data[5])
		if humidity <= 100 {
			r.HumidityPercent = floatPtr(humidity)
		}
	}
	if len(data) >= 5 && data[0] == 0x02 && data[1] == 0x80 && data[2] == 0x02 {
		var candidates []float64
		for _, value := range data[3:5] {
			if value <= 100 {
				candidates = append(candidates, float64(value))
			}
		}
		if len(candidates) > 0 {
			r.BatteryPercent = floatPtr(max(candidates))
		}
	}
	if idx := indexMarker(data, []byte{0x03, 0x13}); idx >= 0 && idx+4 <= len(data) {
		tempRaw := int16(uint16(data[idx+2]) | uint16(data[idx+3])<<8)
		r.TemperatureC = floatPtr(round(float64(tempRaw)/256.0, 2))
	}
	if idx := indexMarker(data, []byte{0x02, 0x12}); idx >= 0 && idx+3 <= len(data) {
		humidity := float64(data[idx+2])
		if humidity <= 100 {
			r.HumidityPercent = floatPtr(humidity)
		}
	}
	if len(data) >= 7 && data[0] == 0x03 && data[1] == 0x05 && data[2] == 0x17 {
		bits := uint32(data[3]) | uint32(data[4])<<8 | uint32(data[5])<<16 | uint32(data[6])<<24
		r.PressureHPa = floatPtr(round(float64(math.Float32frombits(bits)), 2))
		if idx := indexMarker(data, []byte{0x04, 0x1f}); idx >= 0 && idx+5 <= len(data) {
			subtype := data[idx+2]
			value := float64(uint16(data[idx+3]) | uint16(data[idx+4])<<8)
			switch subtype {
			case 0x07:
				r.CO2PPM = floatPtr(value)
			case 0x08:
				r.ETVOC = floatPtr(value)
			}
		}
		if idx := indexMarker(data, []byte{0x03, 0x20}); idx >= 0 && idx+4 <= len(data) {
			r.Lux = floatPtr(float64(uint16(data[idx+2]) | uint16(data[idx+3])<<8))
		}
	}
	return sanitizeReading(r)
}

func sanitizeReading(r bleReading) bleReading {
	r.TemperatureC = sanitizeRange(r.TemperatureC, -40, 85)
	r.HumidityPercent = sanitizeRange(r.HumidityPercent, 0, 100)
	r.BatteryPercent = sanitizeRange(r.BatteryPercent, 0, 100)
	r.RSSI = sanitizeRange(r.RSSI, -127, 20)
	r.PressureHPa = sanitizeRange(r.PressureHPa, 300, 1100)
	r.CO2PPM = sanitizeRange(r.CO2PPM, 0, 10000)
	r.Lux = sanitizeRange(r.Lux, 0, 65534)
	r.ETVOC = sanitizeRange(r.ETVOC, 0, 60000)
	return r
}

func (r *bleReading) merge(other bleReading) {
	if other.TemperatureC != nil {
		r.TemperatureC = other.TemperatureC
	}
	if other.HumidityPercent != nil {
		r.HumidityPercent = other.HumidityPercent
	}
	if other.BatteryPercent != nil {
		r.BatteryPercent = other.BatteryPercent
	}
	if other.PressureHPa != nil {
		r.PressureHPa = other.PressureHPa
	}
	if other.CO2PPM != nil {
		r.CO2PPM = other.CO2PPM
	}
	if other.Lux != nil {
		r.Lux = other.Lux
	}
	if other.ETVOC != nil {
		r.ETVOC = other.ETVOC
	}
}

func (r bleReading) empty() bool {
	return r.TemperatureC == nil &&
		r.HumidityPercent == nil &&
		r.BatteryPercent == nil &&
		r.RSSI == nil &&
		r.PressureHPa == nil &&
		r.CO2PPM == nil &&
		r.Lux == nil &&
		r.ETVOC == nil
}

func (c *collector) add(r bleReading) {
	window := r.TS.Truncate(time.Minute)
	key := r.SensorMAC + "|" + window.Format(time.RFC3339)
	agg := c.windows[key]
	if agg == nil {
		agg = &aggregate{SensorMAC: r.SensorMAC, Label: r.Label, Window: window}
		c.windows[key] = agg
	}
	comparable := readingKey(r)
	if comparable == agg.LastComparable {
		return
	}
	agg.LastComparable = comparable
	appendPtr(&agg.RSSI, r.RSSI)
	appendPtr(&agg.TemperatureC, r.TemperatureC)
	appendPtr(&agg.HumidityPercent, r.HumidityPercent)
	appendPtr(&agg.BatteryPercent, r.BatteryPercent)
	appendPtr(&agg.PressureHPa, r.PressureHPa)
	appendPtr(&agg.CO2PPM, r.CO2PPM)
	appendPtr(&agg.Lux, r.Lux)
	appendPtr(&agg.ETVOC, r.ETVOC)
}

func (c *collector) flushCompleted(ctx context.Context, currentWindow time.Time) (int, error) {
	var errs []error
	flushed := 0
	for key, agg := range c.windows {
		if agg.Window.Before(currentWindow) {
			wrote, err := c.flush(ctx, agg)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if wrote {
				flushed++
			}
			delete(c.windows, key)
		}
	}
	return flushed, errors.Join(errs...)
}

func (c *collector) flushAll(ctx context.Context) error {
	var errs []error
	for key, agg := range c.windows {
		if _, err := c.flush(ctx, agg); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(c.windows, key)
	}
	return errors.Join(errs...)
}

func (c *collector) flush(ctx context.Context, agg *aggregate) (bool, error) {
	if c.db == nil || agg.empty() {
		return false, nil
	}
	if err := upsertDevice(ctx, c.db, agg.SensorMAC, agg.Label); err != nil {
		return false, err
	}
	_, err := c.db.Exec(ctx, `
		INSERT INTO sensor_minute (
			ts, mac, temperature_c, humidity_percent, battery_percent,
			rssi_dbm, pressure_hpa, co2_ppm, lux, etvoc
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (ts, mac) DO UPDATE SET
			temperature_c = COALESCE(EXCLUDED.temperature_c, sensor_minute.temperature_c),
			humidity_percent = COALESCE(EXCLUDED.humidity_percent, sensor_minute.humidity_percent),
			battery_percent = COALESCE(EXCLUDED.battery_percent, sensor_minute.battery_percent),
			rssi_dbm = COALESCE(EXCLUDED.rssi_dbm, sensor_minute.rssi_dbm),
			pressure_hpa = COALESCE(EXCLUDED.pressure_hpa, sensor_minute.pressure_hpa),
			co2_ppm = COALESCE(EXCLUDED.co2_ppm, sensor_minute.co2_ppm),
			lux = COALESCE(EXCLUDED.lux, sensor_minute.lux),
			etvoc = COALESCE(EXCLUDED.etvoc, sensor_minute.etvoc),
			inserted_at = now()
	`, agg.Window, agg.SensorMAC,
		nullablePtr(nullableMedianFloat(agg.TemperatureC)),
		nullablePtr(nullableMedianFloat(agg.HumidityPercent)),
		nullablePtr(nullableMedianFloat(agg.BatteryPercent)),
		nullablePtr(nullableMedianFloat(agg.RSSI)),
		nullablePtr(nullableMedianFloat(agg.PressureHPa)),
		nullablePtr(nullableMedianFloat(agg.CO2PPM)),
		nullablePtr(nullableMedianFloat(agg.Lux)),
		nullablePtr(nullableMedianFloat(agg.ETVOC)),
	)
	if err != nil {
		return false, fmt.Errorf("insert %s %s: %w", agg.SensorMAC, agg.Window.Format(time.RFC3339), err)
	}
	log.Printf("flushed Cisco IoT sensor=%s minute=%s", agg.SensorMAC, agg.Window.Format(time.RFC3339))
	return true, nil
}

func ensureDevices(ctx context.Context, db *pgx.Conn, targets map[string]targetDevice) error {
	for _, target := range targets {
		if err := upsertDevice(ctx, db, target.MAC, target.Label); err != nil {
			return err
		}
	}
	return nil
}

func upsertDevice(ctx context.Context, db *pgx.Conn, mac string, label string) error {
	label = strings.TrimSpace(label)
	if label == "" || normalizeMAC(label) == mac {
		label = mac
	}
	_, err := db.Exec(ctx, `
		INSERT INTO devices (mac, label, sensor_category, location)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (mac) DO UPDATE SET
			label = EXCLUDED.label,
			sensor_category = COALESCE(devices.sensor_category, EXCLUDED.sensor_category),
			location = COALESCE(devices.location, EXCLUDED.location),
			updated_at = now()
	`, mac, label, "Cisco IoT Orchestrator", label)
	return err
}

func (r *statusReporter) MarkSuccess(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := collectorstatus.MarkSuccess(ctx, r.db, r.target); err != nil {
		log.Printf("record collector success: %v", err)
	}
}

func (r *statusReporter) MarkDataSuccess(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := collectorstatus.MarkDataSuccess(ctx, r.db, r.target); err != nil {
		log.Printf("record collector data success: %v", err)
	}
}

func (r *statusReporter) MarkFailure(ctx context.Context, failure error) {
	if r == nil || r.db == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := collectorstatus.MarkFailure(ctx, r.db, r.target, failure); err != nil {
		log.Printf("record collector failure: %v", err)
	}
}

func (agg *aggregate) empty() bool {
	return len(agg.RSSI) == 0 &&
		len(agg.TemperatureC) == 0 &&
		len(agg.HumidityPercent) == 0 &&
		len(agg.BatteryPercent) == 0 &&
		len(agg.PressureHPa) == 0 &&
		len(agg.CO2PPM) == 0 &&
		len(agg.Lux) == 0 &&
		len(agg.ETVOC) == 0
}

func readingKey(r bleReading) string {
	return fmt.Sprintf(
		"rssi=%s|t=%s|h=%s|b=%s|p=%s|co2=%s|lux=%s|etvoc=%s",
		ptrKey(r.RSSI),
		ptrKey(r.TemperatureC),
		ptrKey(r.HumidityPercent),
		ptrKey(r.BatteryPercent),
		ptrKey(r.PressureHPa),
		ptrKey(r.CO2PPM),
		ptrKey(r.Lux),
		ptrKey(r.ETVOC),
	)
}

func ptrKey(value *float64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func appendPtr(values *[]float64, value *float64) {
	if value != nil {
		*values = append(*values, *value)
	}
}

func nullableMedianFloat(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return floatPtr(values[mid])
	}
	return floatPtr((values[mid-1] + values[mid]) / 2)
}

func nullablePtr(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func normalizeMAC(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", ":")
	value = strings.ReplaceAll(value, ".", "")
	if len(value) == 12 && !strings.Contains(value, ":") {
		parts := make([]string, 0, 6)
		for i := 0; i < 12; i += 2 {
			parts = append(parts, value[i:i+2])
		}
		value = strings.Join(parts, ":")
	}
	return value
}

func sanitizeRange(value *float64, minValue float64, maxValue float64) *float64 {
	if value == nil || !isFinite(*value) || *value < minValue || *value > maxValue {
		return nil
	}
	return value
}

func indexMarker(data []byte, marker []byte) int {
	for i := 0; i+len(marker) <= len(data); i++ {
		match := true
		for j := range marker {
			if data[i+j] != marker[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func max(values []float64) float64 {
	result := values[0]
	for _, value := range values[1:] {
		if value > result {
			result = value
		}
	}
	return result
}

func round(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func floatPtr(value float64) *float64 {
	return &value
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
