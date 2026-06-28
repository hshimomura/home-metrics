package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

func readGATTFlowerCareHistory(ctx context.Context, cfg config, target targetDevice, maxEntries int) (gattHistoryReadResult, error) {
	if maxEntries <= 0 {
		return gattHistoryReadResult{}, nil
	}
	if target.GATTBattery == nil {
		return gattHistoryReadResult{}, errors.New("GATT config is required")
	}
	result := gattHistoryReadResult{}
	var historyCount uint16
	for entryIndex := 1; entryIndex <= maxEntries; entryIndex++ {
		if historyCount > 0 && entryIndex > int(historyCount) {
			break
		}
		count, reading, err := readGATTFlowerCareHistoryEntry(ctx, cfg, *target.GATTBattery, entryIndex, flowerCareGATTReadDelay)
		if err != nil {
			result.StopReason = err.Error()
			if entryIndex == 1 {
				return result, err
			}
			log.Printf("stop Flower Care history read sensor=%s entry=%d readings=%d reason=%v", target.MAC, entryIndex, len(result.Readings), err)
			return result, nil
		}
		if historyCount == 0 {
			historyCount = count
			result.Count = count
		}
		if reading == nil {
			break
		}
		reading.SensorMAC = target.MAC
		reading.Label = target.Label
		reading.Location = strings.TrimSpace(target.Location)
		reading.IngestSource = target.IngestSource
		reading.SensorTypeCode = target.SensorTypeCode
		reading.SensorCategory = target.SensorCategory
		result.Readings = append(result.Readings, *reading)
	}
	return result, nil
}

func readGATTFlowerCareHistoryEntry(ctx context.Context, cfg config, batteryCfg gattBatteryConfig, entryIndex int, readDelay time.Duration) (uint16, *bleReading, error) {
	if entryIndex <= 0 {
		return 0, nil, fmt.Errorf("history entry index must be positive: %d", entryIndex)
	}
	deviceID := strings.TrimSpace(batteryCfg.DeviceID)
	if deviceID == "" {
		return 0, nil, errors.New("GATT device ID is required")
	}
	var historyCount uint16
	var reading *bleReading
	err := withGATTControlSession(cfg, func() error {
		if err := controlConnect(ctx, cfg, deviceID, []string{flowerCareHistoryService}); err != nil {
			return err
		}
		defer func() {
			if err := controlDisconnect(context.Background(), cfg, deviceID); err != nil {
				log.Printf("disconnect Cisco Sensor Connect GATT device=%s: %v", deviceID, err)
			}
		}()

		epochPayload, err := controlRead(ctx, cfg, deviceID, flowerCareHistoryService, flowerCareEpoch)
		if err != nil {
			return fmt.Errorf("read Flower Care history epoch: %w", err)
		}
		if len(epochPayload) < 4 {
			return fmt.Errorf("Flower Care history epoch payload too short: % x", epochPayload)
		}
		deviceEpoch := binary.LittleEndian.Uint32(epochPayload[:4])
		hostReadTime := time.Now()

		if err := controlWrite(ctx, cfg, deviceID, flowerCareHistoryService, flowerCareHistoryCommand, []byte{0xa0, 0x00, 0x00}); err != nil {
			return fmt.Errorf("write Flower Care history init: %w", err)
		}
		waitForGATTRead(ctx, readDelay)
		initPayload, err := controlRead(ctx, cfg, deviceID, flowerCareHistoryService, flowerCareHistoryData)
		if err != nil {
			return fmt.Errorf("read Flower Care history init: %w", err)
		}
		if len(initPayload) < 2 {
			return fmt.Errorf("Flower Care history init payload too short: % x", initPayload)
		}
		historyCount = binary.LittleEndian.Uint16(initPayload[:2])
		if historyCount == 0 || entryIndex > int(historyCount) {
			return nil
		}

		entryCommand := []byte{0xa1, byte(entryIndex), byte(entryIndex >> 8)}
		if err := controlWrite(ctx, cfg, deviceID, flowerCareHistoryService, flowerCareHistoryCommand, entryCommand); err != nil {
			return fmt.Errorf("write Flower Care history entry %d: %w", entryIndex, err)
		}
		waitForGATTRead(ctx, readDelay)
		entryPayload, err := controlRead(ctx, cfg, deviceID, flowerCareHistoryService, flowerCareHistoryData)
		if err != nil {
			return fmt.Errorf("read Flower Care history entry %d: %w", entryIndex, err)
		}
		decoded, err := decodeFlowerCareHistoryGATT(entryPayload, deviceEpoch, hostReadTime)
		if err != nil {
			return fmt.Errorf("decode Flower Care history entry %d: %w", entryIndex, err)
		}
		reading = &decoded
		return nil
	})
	return historyCount, reading, err
}
