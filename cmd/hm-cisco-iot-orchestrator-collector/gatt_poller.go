package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"home-metrics/internal/collectorstatus"
)

func runGATTBatteryPoller(ctx context.Context, cfg config, targets map[string]targetDevice, c *collector) {
	pollTargets := gattBatteryTargets(targets)
	if len(pollTargets) == 0 {
		return
	}
	if strings.TrimSpace(cfg.ControlAPIKey) == "" {
		log.Printf("Cisco Sensor Connect GATT battery polling disabled: CISCO_IOT_ORCH_CONTROL_API_KEY is empty")
		return
	}
	nextDue := map[string]time.Time{}
	now := time.Now()
	for mac, target := range pollTargets {
		nextDue[mac] = initialGATTBatteryDue(ctx, c.db, target, now)
		log.Printf("scheduled Cisco Sensor Connect GATT battery poll sensor=%s due=%s", mac, nextDue[mac].Format(time.RFC3339))
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now = <-ticker.C:
		}
		for mac, target := range pollTargets {
			if now.Before(nextDue[mac]) {
				continue
			}
			reporter := &statusReporter{
				db: c.db,
				target: collectorstatus.Target{
					CollectorName: "hm-cisco-iot-orchestrator-collector",
					TargetType:    "gatt_control",
					TargetKey:     mac,
				},
			}
			dataWritten, err := pollGATTBattery(ctx, cfg, target, c)
			reportGATTResult(ctx, reporter, dataWritten, err)
			if err != nil {
				log.Printf("poll Cisco Sensor Connect GATT battery sensor=%s: %v", mac, err)
				nextDue[mac] = now.Add(15 * time.Minute)
				continue
			}
			nextDue[mac] = nextGATTBatteryDue(now, target)
			log.Printf("scheduled next Cisco Sensor Connect GATT battery poll sensor=%s due=%s", mac, nextDue[mac].Format(time.RFC3339))
		}
	}
}

func reportGATTResult(ctx context.Context, reporter statusMarker, dataWritten bool, err error) {
	if reporter == nil {
		return
	}
	if dataWritten {
		reporter.MarkDataSuccess(ctx)
	}
	if err != nil {
		reporter.MarkFailure(ctx, err)
		return
	}
	if !dataWritten {
		reporter.MarkSuccess(ctx)
	}
}

func gattBatteryTargets(targets map[string]targetDevice) map[string]targetDevice {
	out := map[string]targetDevice{}
	for mac, target := range targets {
		if !gattBatteryEnabled(target) {
			continue
		}
		out[mac] = target
	}
	return out
}

func gattBatteryEnabled(target targetDevice) bool {
	if target.GATTBattery == nil {
		return false
	}
	if target.GATTBattery.Enabled != nil && !*target.GATTBattery.Enabled {
		return false
	}
	return strings.TrimSpace(target.GATTBattery.DeviceID) != ""
}

func initialGATTBatteryDue(ctx context.Context, db sensorDB, target targetDevice, now time.Time) time.Time {
	if db == nil {
		return now.Add(randomNonNegativeDuration(gattBatteryJitter(target)))
	}
	lastBattery, err := latestBatteryAt(ctx, db, target.MAC)
	if err != nil {
		log.Printf("read latest GATT battery time sensor=%s: %v", target.MAC, err)
		return now.Add(randomNonNegativeDuration(gattBatteryJitter(target)))
	}
	if lastBattery.IsZero() {
		return now.Add(randomNonNegativeDuration(gattBatteryJitter(target)))
	}
	due := lastBattery.Add(gattBatteryPollInterval(target)).Add(randomSignedDuration(gattBatteryJitter(target)))
	if due.Before(now) {
		return now
	}
	return due
}

func nextGATTBatteryDue(now time.Time, target targetDevice) time.Time {
	return now.Add(gattBatteryPollInterval(target)).Add(randomSignedDuration(gattBatteryJitter(target)))
}

func latestBatteryAt(ctx context.Context, db sensorDB, mac string) (time.Time, error) {
	var ts *time.Time
	err := db.QueryRow(ctx, `
		SELECT max(ts)
		FROM sensor_minute
		WHERE mac = $1 AND battery_percent IS NOT NULL
	`, mac).Scan(&ts)
	if err != nil {
		return time.Time{}, err
	}
	if ts == nil {
		return time.Time{}, nil
	}
	return *ts, nil
}

func latestTelemetryAt(ctx context.Context, db sensorDB, mac string) (time.Time, error) {
	var ts *time.Time
	err := db.QueryRow(ctx, `
		SELECT max(ts)
		FROM sensor_minute
		WHERE mac = $1 AND (
			temperature_c IS NOT NULL OR humidity_percent IS NOT NULL OR
			battery_percent IS NOT NULL OR rssi_dbm IS NOT NULL OR
			pressure_hpa IS NOT NULL OR co2_ppm IS NOT NULL OR
			lux IS NOT NULL OR etvoc IS NOT NULL OR
			soil_moisture_percent IS NOT NULL OR conductivity_us_cm IS NOT NULL
		)
	`, mac).Scan(&ts)
	if err != nil {
		return time.Time{}, err
	}
	if ts == nil {
		return time.Time{}, nil
	}
	return *ts, nil
}

func pollGATTBattery(ctx context.Context, cfg config, target targetDevice, c *collector) (bool, error) {
	if c == nil || c.db == nil {
		return false, nil
	}
	lastTelemetry, err := latestTelemetryAt(ctx, c.db, target.MAC)
	if err != nil {
		return false, err
	}
	maxAge := gattBatteryAdvertisementMaxAge(target)
	if lastTelemetry.IsZero() || time.Since(lastTelemetry) > maxAge {
		return false, fmt.Errorf("latest advertisement is stale: latest=%s max_age=%s", lastTelemetry.Format(time.RFC3339), maxAge)
	}
	battery, firmware, err := readGATTBattery(ctx, cfg, *target.GATTBattery)
	if err != nil {
		return false, err
	}
	now := time.Now()
	c.add(bleReading{
		TS:             now,
		SensorMAC:      target.MAC,
		Label:          target.Label,
		Location:       strings.TrimSpace(target.Location),
		IngestSource:   target.IngestSource,
		SensorTypeCode: target.SensorTypeCode,
		SensorCategory: target.SensorCategory,
		BatteryPercent: floatPtr(float64(battery)),
	})
	if _, err := c.flushCompleted(ctx, now.Add(time.Minute).Truncate(time.Minute)); err != nil {
		return false, err
	}
	log.Printf("stored Cisco Sensor Connect GATT battery sensor=%s battery=%d firmware=%q", target.MAC, battery, firmware)
	if err := pollGATTHistoryBackfill(ctx, cfg, target, c.db); err != nil {
		return true, err
	}
	return true, nil
}

func pollGATTHistoryBackfill(ctx context.Context, cfg config, target targetDevice, db sensorDB) error {
	if !gattHistoryBackfillEnabled(target) || db == nil {
		return nil
	}
	result, err := readGATTFlowerCareHistory(ctx, cfg, target, gattHistoryMaxEntries(target))
	if err != nil {
		return fmt.Errorf("read Flower Care GATT history: %w", err)
	}
	if result.StopReason != "" {
		log.Printf("Flower Care GATT history partial sensor=%s count=%d readings=%d reason=%s", target.MAC, result.Count, len(result.Readings), result.StopReason)
	}
	if len(result.Readings) == 0 {
		log.Printf("Flower Care GATT history empty sensor=%s count=%d", target.MAC, result.Count)
		return nil
	}
	inserted, err := backfillSensorMinuteReadings(ctx, db, result.Readings)
	if err != nil {
		return err
	}
	log.Printf("stored Flower Care GATT history backfill sensor=%s count=%d readings=%d rows=%d", target.MAC, result.Count, len(result.Readings), inserted)
	return nil
}

func readGATTBattery(ctx context.Context, cfg config, batteryCfg gattBatteryConfig) (int, string, error) {
	var battery int
	var firmware string
	err := withGATTControlSession(cfg, func() error {
		baseBody := map[string]any{
			"technology": "ble",
			"id":         strings.TrimSpace(batteryCfg.DeviceID),
			"controlApp": cfg.ControlAppID,
		}
		serviceID := gattBatteryServiceID(batteryCfg)
		characteristicID := gattBatteryCharacteristicID(batteryCfg)
		if _, err := controlPost(ctx, cfg, "/control/connectivity/connect", map[string]any{
			"technology": "ble",
			"id":         strings.TrimSpace(batteryCfg.DeviceID),
			"controlApp": cfg.ControlAppID,
			"ble": map[string]any{
				"services": []map[string]string{{"serviceID": serviceID}},
			},
		}); err != nil {
			return err
		}
		defer func() {
			if _, err := controlPost(context.Background(), cfg, "/control/connectivity/disconnect", baseBody); err != nil {
				log.Printf("disconnect Cisco Sensor Connect GATT device=%s: %v", strings.TrimSpace(batteryCfg.DeviceID), err)
			}
		}()
		body, err := controlPost(ctx, cfg, "/control/data/read", map[string]any{
			"technology": "ble",
			"id":         strings.TrimSpace(batteryCfg.DeviceID),
			"controlApp": cfg.ControlAppID,
			"ble": map[string]any{
				"serviceID":        serviceID,
				"characteristicID": characteristicID,
			},
		})
		if err != nil {
			return err
		}
		var response struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("parse GATT battery response: %w", err)
		}
		payload, err := decodeHexValue(response.Value)
		if err != nil {
			return err
		}
		if len(payload) < 1 {
			return errors.New("empty GATT battery payload")
		}
		battery = int(payload[0])
		if battery < 0 || battery > 100 {
			return fmt.Errorf("GATT battery out of range: %d", battery)
		}
		if len(payload) >= 3 {
			firmware = string(payload[2:])
		}
		return nil
	})
	return battery, firmware, err
}

func waitForGATTRead(ctx context.Context, delay time.Duration) {
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func gattBatteryServiceID(cfg gattBatteryConfig) string {
	if value := strings.TrimSpace(cfg.ServiceID); value != "" {
		return value
	}
	return flowerCareDataService
}

func gattBatteryCharacteristicID(cfg gattBatteryConfig) string {
	if value := strings.TrimSpace(cfg.CharacteristicID); value != "" {
		return value
	}
	return flowerCareBatteryCharacteristic
}

func gattBatteryPollInterval(target targetDevice) time.Duration {
	if target.GATTBattery == nil {
		return defaultGATTBatteryPoll
	}
	return parsePositiveDuration(target.GATTBattery.PollInterval, defaultGATTBatteryPoll)
}

func gattBatteryJitter(target targetDevice) time.Duration {
	if target.GATTBattery == nil {
		return defaultGATTBatteryJitter
	}
	return parseNonNegativeDuration(target.GATTBattery.Jitter, defaultGATTBatteryJitter)
}

func gattBatteryAdvertisementMaxAge(target targetDevice) time.Duration {
	if target.GATTBattery == nil {
		return defaultGATTAdvMaxAge
	}
	return parsePositiveDuration(target.GATTBattery.AdvertisementMaxAge, defaultGATTAdvMaxAge)
}

func gattHistoryBackfillEnabled(target targetDevice) bool {
	if !gattBatteryEnabled(target) || target.GATTBattery.HistoryBackfill == nil {
		return false
	}
	return *target.GATTBattery.HistoryBackfill
}

func gattHistoryMaxEntries(target targetDevice) int {
	if target.GATTBattery == nil || target.GATTBattery.MaxHistoryEntries <= 0 {
		return defaultGATTHistoryEntries
	}
	return target.GATTBattery.MaxHistoryEntries
}

func parsePositiveDuration(value string, fallback time.Duration) time.Duration {
	parsed := parseNonNegativeDuration(value, fallback)
	if parsed <= 0 {
		return fallback
	}
	return parsed
}

func parseNonNegativeDuration(value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func randomSignedDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return randomNonNegativeDuration(2*max) - max
}

func randomNonNegativeDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return time.Duration(time.Now().UnixNano() % int64(max))
	}
	value := binary.BigEndian.Uint64(buf)
	return time.Duration(value % uint64(max))
}
