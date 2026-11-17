package bledecode

import (
	"encoding/binary"
	"math"
)

type Reading struct {
	Motion              *bool
	PacketID            *int
	TemperatureC        *float64
	HumidityPercent     *float64
	BatteryPercent      *float64
	PressureHPa         *float64
	CO2PPM              *float64
	Lux                 *float64
	ETVOC               *float64
	SoilMoisturePercent *float64
	ConductivityUSCM    *float64
}

func DecodeAdvertisement(data []byte) Reading {
	serviceData := serviceDataFromAdvertisement(data)
	if len(serviceData) == 0 {
		return DecodeServiceData(data)
	}
	var reading Reading
	for _, payload := range serviceData {
		reading.Merge(DecodeServiceData(payload))
	}
	return reading
}

func DecodeServiceData(data []byte) Reading {
	if len(data) > 0 && data[0]>>5 == 2 {
		return decodeBTHome(data)
	}
	if looksLikeXiaomiFE95(data) {
		return Sanitize(decodeXiaomiFE95(data))
	}

	var reading Reading
	if len(data) >= 6 && data[0] == 0xa1 && data[1] == 0x01 {
		reading.BatteryPercent = floatPtr(float64(data[2]))
		reading.TemperatureC = floatPtr(float64(int8(data[3])))
		humidity := float64(data[5])
		if humidity <= 100 {
			reading.HumidityPercent = &humidity
		}
	}
	if len(data) >= 5 && data[0] == 0x02 && data[1] == 0x80 && data[2] == 0x02 {
		for _, value := range data[3:5] {
			if value <= 100 && (reading.BatteryPercent == nil || float64(value) > *reading.BatteryPercent) {
				reading.BatteryPercent = floatPtr(float64(value))
			}
		}
	}
	if index := indexMarker(data, []byte{0x03, 0x13}); index >= 0 && index+4 <= len(data) {
		raw := int16(binary.LittleEndian.Uint16(data[index+2 : index+4]))
		reading.TemperatureC = floatPtr(round(float64(raw)/256.0, 2))
	}
	if index := indexMarker(data, []byte{0x02, 0x12}); index >= 0 && index+3 <= len(data) {
		humidity := float64(data[index+2])
		if humidity <= 100 {
			reading.HumidityPercent = &humidity
		}
	}
	if len(data) >= 7 && data[0] == 0x03 && data[1] == 0x05 && data[2] == 0x17 {
		reading.PressureHPa = floatPtr(round(float64(math.Float32frombits(binary.LittleEndian.Uint32(data[3:7]))), 2))
		if index := indexMarker(data, []byte{0x04, 0x1f}); index >= 0 && index+5 <= len(data) {
			value := float64(binary.LittleEndian.Uint16(data[index+3 : index+5]))
			switch data[index+2] {
			case 0x07:
				reading.CO2PPM = &value
			case 0x08:
				reading.ETVOC = &value
			}
		}
		if index := indexMarker(data, []byte{0x03, 0x20}); index >= 0 && index+4 <= len(data) {
			reading.Lux = floatPtr(float64(binary.LittleEndian.Uint16(data[index+2 : index+4])))
		}
	}
	reading.Merge(decodeXiaomiFE95(data))
	return Sanitize(reading)
}

func Sanitize(reading Reading) Reading {
	reading.TemperatureC = sanitizeRange(reading.TemperatureC, -40, 85)
	reading.HumidityPercent = sanitizeRange(reading.HumidityPercent, 0, 100)
	reading.BatteryPercent = sanitizeRange(reading.BatteryPercent, 0, 100)
	reading.PressureHPa = sanitizeRange(reading.PressureHPa, 300, 1100)
	reading.CO2PPM = sanitizeRange(reading.CO2PPM, 0, 10000)
	reading.Lux = sanitizeRange(reading.Lux, 0, 65534)
	reading.ETVOC = sanitizeRange(reading.ETVOC, 0, 60000)
	reading.SoilMoisturePercent = sanitizeRange(reading.SoilMoisturePercent, 0, 100)
	reading.ConductivityUSCM = sanitizeRange(reading.ConductivityUSCM, 0, 10000)
	return reading
}

func (reading *Reading) Merge(other Reading) {
	if other.Motion != nil {
		reading.Motion = other.Motion
	}
	if other.PacketID != nil {
		reading.PacketID = other.PacketID
	}
	if other.TemperatureC != nil {
		reading.TemperatureC = other.TemperatureC
	}
	if other.HumidityPercent != nil {
		reading.HumidityPercent = other.HumidityPercent
	}
	if other.BatteryPercent != nil {
		reading.BatteryPercent = other.BatteryPercent
	}
	if other.PressureHPa != nil {
		reading.PressureHPa = other.PressureHPa
	}
	if other.CO2PPM != nil {
		reading.CO2PPM = other.CO2PPM
	}
	if other.Lux != nil {
		reading.Lux = other.Lux
	}
	if other.ETVOC != nil {
		reading.ETVOC = other.ETVOC
	}
	if other.SoilMoisturePercent != nil {
		reading.SoilMoisturePercent = other.SoilMoisturePercent
	}
	if other.ConductivityUSCM != nil {
		reading.ConductivityUSCM = other.ConductivityUSCM
	}
}

func serviceDataFromAdvertisement(data []byte) [][]byte {
	var payloads [][]byte
	for len(data) >= 2 {
		length := int(data[0])
		if length == 0 || length+1 > len(data) {
			break
		}
		adData := data[2 : length+1]
		if data[1] == 0x16 && len(adData) >= 2 {
			uuid := binary.LittleEndian.Uint16(adData[:2])
			if uuid == 0xfe6a || uuid == 0xffe1 || uuid == 0xfeaa || uuid == 0xfe95 || uuid == 0xfcd2 {
				payloads = append(payloads, adData[2:])
			}
		}
		data = data[length+1:]
	}
	return payloads
}

func decodeBTHome(data []byte) Reading {
	var reading Reading
	if len(data) < 1 || data[0]>>5 != 2 || data[0]&1 != 0 || data[0]&0x1a != 0 {
		return reading
	}
	sizes := map[byte]int{0x00: 1, 0x01: 1, 0x21: 1, 0x3a: 1, 0x64: 1, 0xf0: 2, 0xf1: 4, 0xf2: 3}
	for index := 1; index < len(data); {
		id := data[index]
		index++
		size, ok := sizes[id]
		if !ok {
			break
		}
		if index+size > len(data) {
			return Reading{}
		}
		value := data[index]
		index += size
		switch id {
		case 0x00:
			packetID := int(value)
			reading.PacketID = &packetID
		case 0x01:
			if value > 100 {
				return Reading{}
			}
			reading.BatteryPercent = floatPtr(float64(value))
		case 0x21:
			if value > 1 {
				return Reading{}
			}
			motion := value == 1
			reading.Motion = &motion
		}
	}
	return reading
}

func looksLikeXiaomiFE95(data []byte) bool {
	return len(data) >= 15 && data[0] == 0x71 && data[1] == 0x20 && data[2] == 0x98 && data[3] == 0x00
}

func decodeXiaomiFE95(data []byte) Reading {
	var reading Reading
	if !looksLikeXiaomiFE95(data) {
		return reading
	}
	for offset := 12; offset+3 <= len(data); {
		objectID := binary.LittleEndian.Uint16(data[offset : offset+2])
		length := int(data[offset+2])
		start := offset + 3
		end := start + length
		if end > len(data) {
			break
		}
		value := data[start:end]
		switch objectID {
		case 0x1004:
			if len(value) >= 2 {
				raw := int16(binary.LittleEndian.Uint16(value[:2]))
				reading.TemperatureC = floatPtr(round(float64(raw)/10.0, 1))
			}
		case 0x1007:
			if len(value) >= 3 {
				reading.Lux = floatPtr(float64(uint32(value[0]) | uint32(value[1])<<8 | uint32(value[2])<<16))
			}
		case 0x1008:
			if len(value) >= 1 {
				reading.SoilMoisturePercent = floatPtr(float64(value[0]))
			}
		case 0x1009:
			if len(value) >= 2 {
				reading.ConductivityUSCM = floatPtr(float64(binary.LittleEndian.Uint16(value[:2])))
			}
		}
		offset = end
	}
	return reading
}

func indexMarker(data []byte, marker []byte) int {
	for index := 0; index+len(marker) <= len(data); index++ {
		match := true
		for markerIndex := range marker {
			if data[index+markerIndex] != marker[markerIndex] {
				match = false
				break
			}
		}
		if match {
			return index
		}
	}
	return -1
}

func sanitizeRange(value *float64, minimum float64, maximum float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < minimum || *value > maximum {
		return nil
	}
	return value
}

func round(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func floatPtr(value float64) *float64 {
	return &value
}
