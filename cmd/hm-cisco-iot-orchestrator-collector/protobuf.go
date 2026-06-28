package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

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
		decoded.Location = strings.TrimSpace(target.Location)
		decoded.IngestSource = target.IngestSource
		decoded.SensorTypeCode = target.SensorTypeCode
		decoded.SensorCategory = target.SensorCategory
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
