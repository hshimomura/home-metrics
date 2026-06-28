package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"time"
)

func decodeBLEPayload(data []byte) bleReading {
	serviceDataList := serviceDataFromAdvertisement(data)
	if len(serviceDataList) == 0 {
		return decodeServiceData(hex.EncodeToString(data))
	}
	reading := bleReading{}
	for _, serviceData := range serviceDataList {
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
			if uuid == 0xfe6a || uuid == 0xffe1 || uuid == 0xfeaa || uuid == 0xfe95 {
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
	if looksLikeXiaomiFE95(data) {
		return sanitizeReading(decodeXiaomiFE95(data))
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
	r.merge(decodeXiaomiFE95(data))
	return sanitizeReading(r)
}

func looksLikeXiaomiFE95(data []byte) bool {
	return len(data) >= 15 && data[0] == 0x71 && data[1] == 0x20 && data[2] == 0x98 && data[3] == 0x00
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

func decodeXiaomiFE95(data []byte) bleReading {
	r := bleReading{}
	if len(data) < 15 {
		return r
	}
	if data[0] != 0x71 || data[1] != 0x20 || data[2] != 0x98 || data[3] != 0x00 {
		return r
	}
	for offset := 12; offset+3 <= len(data); {
		objectID := uint16(data[offset]) | uint16(data[offset+1])<<8
		length := int(data[offset+2])
		valueStart := offset + 3
		valueEnd := valueStart + length
		if valueEnd > len(data) {
			break
		}
		value := data[valueStart:valueEnd]
		switch objectID {
		case 0x1004:
			if len(value) >= 2 {
				raw := int16(uint16(value[0]) | uint16(value[1])<<8)
				r.TemperatureC = floatPtr(round(float64(raw)/10.0, 1))
			}
		case 0x1007:
			if len(value) >= 3 {
				lux := uint32(value[0]) | uint32(value[1])<<8 | uint32(value[2])<<16
				r.Lux = floatPtr(float64(lux))
			}
		case 0x1008:
			if len(value) >= 1 {
				r.SoilMoisturePercent = floatPtr(float64(value[0]))
			}
		case 0x1009:
			if len(value) >= 2 {
				conductivity := uint16(value[0]) | uint16(value[1])<<8
				r.ConductivityUSCM = floatPtr(float64(conductivity))
			}
		}
		offset = valueEnd
	}
	return r
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
	r.SoilMoisturePercent = sanitizeRange(r.SoilMoisturePercent, 0, 100)
	r.ConductivityUSCM = sanitizeRange(r.ConductivityUSCM, 0, 10000)
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
	if other.SoilMoisturePercent != nil {
		r.SoilMoisturePercent = other.SoilMoisturePercent
	}
	if other.ConductivityUSCM != nil {
		r.ConductivityUSCM = other.ConductivityUSCM
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
		r.ETVOC == nil &&
		r.SoilMoisturePercent == nil &&
		r.ConductivityUSCM == nil
}
