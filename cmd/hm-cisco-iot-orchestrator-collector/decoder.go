package main

import (
	"encoding/binary"
	"fmt"
	"time"

	"home-metrics/internal/bledecode"
)

func decodeBLEPayload(data []byte) bleReading {
	return bleReadingFromDecoded(bledecode.DecodeAdvertisement(data))
}

func bleReadingFromDecoded(decoded bledecode.Reading) bleReading {
	return bleReading{
		Motion:              decoded.Motion,
		PacketID:            decoded.PacketID,
		TemperatureC:        decoded.TemperatureC,
		HumidityPercent:     decoded.HumidityPercent,
		BatteryPercent:      decoded.BatteryPercent,
		PressureHPa:         decoded.PressureHPa,
		CO2PPM:              decoded.CO2PPM,
		Lux:                 decoded.Lux,
		ETVOC:               decoded.ETVOC,
		SoilMoisturePercent: decoded.SoilMoisturePercent,
		ConductivityUSCM:    decoded.ConductivityUSCM,
	}
}

func decodeFlowerCareRealtimeGATT(data []byte) (bleReading, error) {
	if len(data) < 10 {
		return bleReading{}, fmt.Errorf("Flower Care real-time GATT payload too short: % x", data)
	}
	tempRaw := int16(binary.LittleEndian.Uint16(data[0:2]))
	reading := bleReading{
		TemperatureC:        floatPtr(round(float64(tempRaw)/10.0, 1)),
		Lux:                 floatPtr(float64(binary.LittleEndian.Uint32(data[3:7]))),
		SoilMoisturePercent: floatPtr(float64(data[7])),
		ConductivityUSCM:    floatPtr(float64(binary.LittleEndian.Uint16(data[8:10]))),
	}
	return sanitizeReading(reading), nil
}

func decodeFlowerCareHistoryGATT(data []byte, deviceEpoch uint32, hostReadTime time.Time) (bleReading, error) {
	if len(data) < 14 {
		return bleReading{}, fmt.Errorf("Flower Care history GATT payload too short: % x", data)
	}
	deviceTimestamp := binary.LittleEndian.Uint32(data[0:4])
	if deviceTimestamp <= 1 {
		return bleReading{}, fmt.Errorf("Flower Care history GATT timestamp invalid: %d raw=% x", deviceTimestamp, data)
	}
	if deviceTimestamp > deviceEpoch {
		return bleReading{}, fmt.Errorf("Flower Care history GATT timestamp exceeds device epoch: timestamp=%d epoch=%d raw=% x", deviceTimestamp, deviceEpoch, data)
	}
	reading, err := decodeFlowerCareRealtimeGATT(append([]byte{
		data[4], data[5], data[6],
		data[7], data[8], data[9], data[10],
		data[11], data[12], data[13],
	}, data[14:]...))
	if err != nil {
		return bleReading{}, err
	}
	reading.TS = flowerCareHistoryWallTime(deviceTimestamp, deviceEpoch, hostReadTime).Truncate(time.Minute)
	return reading, nil
}

func flowerCareHistoryWallTime(deviceTimestamp uint32, deviceEpoch uint32, hostReadTime time.Time) time.Time {
	if deviceEpoch < deviceTimestamp {
		return hostReadTime
	}
	return hostReadTime.Add(-time.Duration(deviceEpoch-deviceTimestamp) * time.Second)
}

func sanitizeReading(reading bleReading) bleReading {
	reading.TemperatureC = sanitizeRange(reading.TemperatureC, -40, 85)
	reading.HumidityPercent = sanitizeRange(reading.HumidityPercent, 0, 100)
	reading.BatteryPercent = sanitizeRange(reading.BatteryPercent, 0, 100)
	reading.RSSI = sanitizeRange(reading.RSSI, -127, 20)
	reading.PressureHPa = sanitizeRange(reading.PressureHPa, 300, 1100)
	reading.CO2PPM = sanitizeRange(reading.CO2PPM, 0, 10000)
	reading.Lux = sanitizeRange(reading.Lux, 0, 65534)
	reading.ETVOC = sanitizeRange(reading.ETVOC, 0, 60000)
	reading.SoilMoisturePercent = sanitizeRange(reading.SoilMoisturePercent, 0, 100)
	reading.ConductivityUSCM = sanitizeRange(reading.ConductivityUSCM, 0, 10000)
	return reading
}

func (reading bleReading) empty() bool {
	return reading.Motion == nil && reading.TemperatureC == nil &&
		reading.HumidityPercent == nil &&
		reading.BatteryPercent == nil &&
		reading.RSSI == nil &&
		reading.PressureHPa == nil &&
		reading.CO2PPM == nil &&
		reading.Lux == nil &&
		reading.ETVOC == nil &&
		reading.SoilMoisturePercent == nil &&
		reading.ConductivityUSCM == nil
}
