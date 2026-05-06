package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	defaultDBDSN         = "dbname=ble_sensors host=/var/run/postgresql"
	defaultDeviceKey     = "echonet-device"
	defaultLocalAddr     = ":3610"
	defaultMulticastAddr = "224.0.23.0:3610"
	sourceName           = "echonet"
	esvGet               = byte(0x62)
	esvGetRes            = byte(0x72)
	esvGetSNA            = byte(0x52)
	esvInf               = byte(0x73)
)

var controllerEOJ = eoj{0x05, 0xff, 0x01}
var nodeProfileEOJ = eoj{0x0e, 0xf0, 0x01}

type eoj [3]byte

type metricConfig struct {
	EOJ         eoj
	EPC         byte
	Metric      string
	Unit        string
	RawProperty string
	Decoder     func([]byte) (float64, error)
}

type propertyValue struct {
	SEOJ eoj
	EPC  byte
	EDT  []byte
}

type frame struct {
	TID        uint16
	SEOJ       eoj
	DEOJ       eoj
	ESV        byte
	Properties []propertyValue
}

type energyReading struct {
	TS          time.Time
	DeviceKey   string
	Instance    string
	Metric      string
	Value       float64
	Unit        string
	RawProperty string
}

var metrics = []metricConfig{
	{
		EOJ:         eoj{0x02, 0x79, 0x01},
		EPC:         0xe0,
		Metric:      "solar_generation_w",
		Unit:        "W",
		RawProperty: "instantaneousElectricPowerGeneration",
		Decoder:     decodeSigned,
	},
	{
		EOJ:         eoj{0x02, 0x7d, 0x01},
		EPC:         0xe4,
		Metric:      "battery_remaining",
		Unit:        "%",
		RawProperty: "remainingCapacity3",
		Decoder:     decodeUnsigned,
	},
	{
		EOJ:         eoj{0x02, 0x7d, 0x01},
		EPC:         0xd3,
		Metric:      "battery_power_w",
		Unit:        "W",
		RawProperty: "instantaneousChargingAndDischargingElectricPower",
		Decoder:     decodeSigned,
	},
}

var nextTID atomic.Uint32

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := envString("BLE_DB_DSN", defaultDBDSN)
	deviceKey := envString("ECHONET_DEVICE_KEY", defaultDeviceKey)
	label := envString("ECHONET_LABEL", "ECHONET Lite device")
	location := strings.TrimSpace(os.Getenv("ECHONET_LOCATION"))
	localAddr := envString("ECHONET_LOCAL_ADDR", defaultLocalAddr)
	targetIP := strings.TrimSpace(os.Getenv("ECHONET_TARGET_IP"))
	multicastAddr := envString("ECHONET_MULTICAST_ADDR", defaultMulticastAddr)
	pollInterval := envDuration("ECHONET_POLL_INTERVAL", 10*time.Second)
	requestTimeout := envDuration("ECHONET_REQUEST_TIMEOUT", 3*time.Second)
	runOnceOnly := envBool("ECHONET_RUN_ONCE", false)

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(context.Background())
	if err := upsertDevice(ctx, db, deviceKey, label, location); err != nil {
		log.Fatalf("ensure energy device: %v", err)
	}
	if err := upsertMetricDefinitions(ctx, db); err != nil {
		log.Fatalf("ensure metric definitions: %v", err)
	}

	conn, err := listenUDP(localAddr)
	if err != nil {
		log.Fatalf("listen echonet udp: %v", err)
	}
	defer conn.Close()

	targetAddr, err := resolveTarget(ctx, conn, targetIP, multicastAddr, requestTimeout)
	if err != nil {
		log.Fatalf("resolve echonet target: %v", err)
	}
	log.Printf("echonet collector started target=%s device_key=%s interval=%s", targetAddr, deviceKey, pollInterval)

	if err := pollOnce(ctx, conn, db, targetAddr, deviceKey, requestTimeout); err != nil {
		log.Printf("poll echonet: %v", err)
	}
	if runOnceOnly {
		return
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pollOnce(ctx, conn, db, targetAddr, deviceKey, requestTimeout); err != nil {
				log.Printf("poll echonet: %v", err)
			}
		}
	}
}

func listenUDP(localAddr string) (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp4", localAddr)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp4", addr)
}

func resolveTarget(ctx context.Context, conn *net.UDPConn, targetIP string, multicastAddr string, timeout time.Duration) (*net.UDPAddr, error) {
	if targetIP != "" {
		return net.ResolveUDPAddr("udp4", net.JoinHostPort(targetIP, "3610"))
	}
	addr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return nil, err
	}
	tid := newTID()
	if _, err := conn.WriteToUDP(buildGetFrame(tid, nodeProfileEOJ, []byte{0xd6}), addr); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			return nil, err
		}
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
					continue
				}
			}
			return nil, err
		}
		f, err := parseFrame(buf[:n])
		if err != nil || f.TID != tid || f.ESV != esvGetRes {
			continue
		}
		for _, prop := range f.Properties {
			if prop.EPC == 0xd6 && hasTargetInstances(prop.EDT) {
				log.Printf("discovered echonet target ip=%s instances=%x", remote.IP, prop.EDT)
				return remote, nil
			}
		}
	}
	return nil, errors.New("echonet target not found; set ECHONET_TARGET_IP")
}

func pollOnce(ctx context.Context, conn *net.UDPConn, db *pgx.Conn, target *net.UDPAddr, deviceKey string, timeout time.Duration) error {
	byEOJ := map[eoj][]byte{}
	for _, metric := range metrics {
		byEOJ[metric.EOJ] = append(byEOJ[metric.EOJ], metric.EPC)
	}
	for targetEOJ, epcs := range byEOJ {
		if err := requestAndStore(ctx, conn, db, target, deviceKey, targetEOJ, epcs, timeout); err != nil {
			return err
		}
	}
	return nil
}

func requestAndStore(ctx context.Context, conn *net.UDPConn, db *pgx.Conn, target *net.UDPAddr, deviceKey string, targetEOJ eoj, epcs []byte, timeout time.Duration) error {
	tid := newTID()
	if _, err := conn.WriteToUDP(buildGetFrame(tid, targetEOJ, epcs), target); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			return err
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					continue
				}
			}
			return err
		}
		f, err := parseFrame(buf[:n])
		if err != nil {
			continue
		}
		if f.TID != tid && f.ESV != esvInf {
			continue
		}
		if f.ESV == esvGetSNA {
			return fmt.Errorf("echonet get not accepted seoj=%s", formatEOJ(f.SEOJ))
		}
		if f.ESV != esvGetRes && f.ESV != esvInf {
			continue
		}
		readings := decodeReadings(time.Now().Truncate(time.Minute), deviceKey, f)
		if len(readings) == 0 {
			continue
		}
		if err := upsertReadings(ctx, db, readings); err != nil {
			return err
		}
		for _, reading := range readings {
			log.Printf("stored echonet ts=%s instance=%s metric=%s value=%.3f unit=%s", reading.TS.Format(time.RFC3339), reading.Instance, reading.Metric, reading.Value, reading.Unit)
		}
		return nil
	}
	return fmt.Errorf("timeout waiting echonet response eoj=%s epcs=%x", formatEOJ(targetEOJ), epcs)
}

func buildGetFrame(tid uint16, deoj eoj, epcs []byte) []byte {
	frame := []byte{0x10, 0x81, byte(tid >> 8), byte(tid), controllerEOJ[0], controllerEOJ[1], controllerEOJ[2], deoj[0], deoj[1], deoj[2], esvGet, byte(len(epcs))}
	for _, epc := range epcs {
		frame = append(frame, epc, 0x00)
	}
	return frame
}

func parseFrame(data []byte) (frame, error) {
	if len(data) < 12 || data[0] != 0x10 || data[1] != 0x81 {
		return frame{}, errors.New("invalid echonet frame")
	}
	f := frame{
		TID:  binary.BigEndian.Uint16(data[2:4]),
		SEOJ: eoj{data[4], data[5], data[6]},
		DEOJ: eoj{data[7], data[8], data[9]},
		ESV:  data[10],
	}
	opc := int(data[11])
	pos := 12
	for i := 0; i < opc; i++ {
		if pos+2 > len(data) {
			return frame{}, errors.New("truncated property header")
		}
		epc := data[pos]
		pdc := int(data[pos+1])
		pos += 2
		if pos+pdc > len(data) {
			return frame{}, errors.New("truncated property data")
		}
		edt := append([]byte(nil), data[pos:pos+pdc]...)
		pos += pdc
		f.Properties = append(f.Properties, propertyValue{SEOJ: f.SEOJ, EPC: epc, EDT: edt})
	}
	return f, nil
}

func decodeReadings(ts time.Time, deviceKey string, f frame) []energyReading {
	var readings []energyReading
	for _, prop := range f.Properties {
		metric, ok := metricFor(f.SEOJ, prop.EPC)
		if !ok || len(prop.EDT) == 0 {
			continue
		}
		value, err := metric.Decoder(prop.EDT)
		if err != nil {
			log.Printf("decode echonet seoj=%s epc=%02x edt=%x: %v", formatEOJ(f.SEOJ), prop.EPC, prop.EDT, err)
			continue
		}
		readings = append(readings, energyReading{
			TS:          ts,
			DeviceKey:   deviceKey,
			Instance:    formatEOJ(metric.EOJ),
			Metric:      metric.Metric,
			Value:       value,
			Unit:        metric.Unit,
			RawProperty: metric.RawProperty,
		})
	}
	return readings
}

func metricFor(sourceEOJ eoj, epc byte) (metricConfig, bool) {
	for _, metric := range metrics {
		if metric.EOJ == sourceEOJ && metric.EPC == epc {
			return metric, true
		}
	}
	return metricConfig{}, false
}

func upsertReadings(ctx context.Context, db *pgx.Conn, readings []energyReading) error {
	for _, reading := range readings {
		unit := any(nil)
		if reading.Unit != "" {
			unit = reading.Unit
		}
		_, err := db.Exec(ctx, `
			INSERT INTO energy_readings (
				ts,
				source,
				device_key,
				instance,
				metric,
				value,
				unit,
				raw_property,
				raw_topic
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULL)
			ON CONFLICT (ts, source, device_key, metric) DO UPDATE SET
				instance = EXCLUDED.instance,
				value = EXCLUDED.value,
				unit = EXCLUDED.unit,
				raw_property = EXCLUDED.raw_property,
				raw_topic = EXCLUDED.raw_topic,
				inserted_at = now()
		`, reading.TS, sourceName, reading.DeviceKey, reading.Instance, reading.Metric, reading.Value, unit, reading.RawProperty)
		if err != nil {
			return err
		}
	}
	return nil
}

func upsertDevice(ctx context.Context, db *pgx.Conn, deviceKey string, label string, location string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO energy_devices (source, device_key, label, location)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (source, device_key) DO UPDATE SET
			label = EXCLUDED.label,
			location = EXCLUDED.location,
			updated_at = now()
	`, sourceName, deviceKey, label, location)
	return err
}

func upsertMetricDefinitions(ctx context.Context, db *pgx.Conn) error {
	for _, metric := range metrics {
		_, err := db.Exec(ctx, `
			INSERT INTO energy_metric_definitions (
				source,
				metric,
				display_name,
				unit,
				raw_property,
				raw_instance
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (source, metric) DO UPDATE SET
				unit = EXCLUDED.unit,
				raw_property = EXCLUDED.raw_property,
				raw_instance = EXCLUDED.raw_instance,
				updated_at = now()
		`, sourceName, metric.Metric, metric.Metric, metric.Unit, metric.RawProperty, formatEOJ(metric.EOJ))
		if err != nil {
			return err
		}
	}
	return nil
}

func decodeUnsigned(data []byte) (float64, error) {
	if len(data) == 0 || len(data) > 8 {
		return 0, fmt.Errorf("unsupported unsigned length %d", len(data))
	}
	var value uint64
	for _, b := range data {
		value = (value << 8) | uint64(b)
	}
	return float64(value), nil
}

func decodeSigned(data []byte) (float64, error) {
	if len(data) == 0 || len(data) > 8 {
		return 0, fmt.Errorf("unsupported signed length %d", len(data))
	}
	unsigned := uint64(0)
	for _, b := range data {
		unsigned = (unsigned << 8) | uint64(b)
	}
	shift := uint(64 - len(data)*8)
	return float64(int64(unsigned<<shift) >> shift), nil
}

func hasTargetInstances(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	count := int(data[0])
	for i := 0; i < count; i++ {
		pos := 1 + i*3
		if pos+3 > len(data) {
			break
		}
		instance := eoj{data[pos], data[pos+1], data[pos+2]}
		for _, metric := range metrics {
			if metric.EOJ == instance {
				return true
			}
		}
	}
	return false
}

func newTID() uint16 {
	return uint16(nextTID.Add(1))
}

func formatEOJ(value eoj) string {
	return hex.EncodeToString(value[:])
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func envString(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
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
	value := strings.TrimSpace(os.Getenv(name))
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
