package main

import (
	"context"
	"log"
	"time"
)

// decodeBTHome accepts plaintext v2 objects. Unsupported objects terminate
// parsing; their length cannot safely be inferred from an object ID.
func decodeBTHome(data []byte) bleReading {
	r := bleReading{}
	if len(data) < 1 || data[0]>>5 != 2 || data[0]&1 != 0 || data[0]&0x1a != 0 {
		return r
	}
	sizes := map[byte]int{0x00: 1, 0x01: 1, 0x21: 1, 0x3a: 1, 0x64: 1, 0xf0: 2, 0xf1: 4, 0xf2: 3}
	for i := 1; i < len(data); {
		id := data[i]
		i++
		n, ok := sizes[id]
		if !ok {
			break
		}
		if i+n > len(data) {
			return bleReading{}
		}
		v := data[i]
		i += n
		switch id {
		case 0x00:
			x := int(v)
			r.PacketID = &x
		case 0x01:
			if v > 100 {
				return bleReading{}
			}
			r.BatteryPercent = floatPtr(float64(v))
		case 0x21:
			if v > 1 {
				return bleReading{}
			}
			x := v == 1
			r.Motion = &x
		}
	}
	return r
}

func (c *collector) storeMotion(ctx context.Context, r bleReading) (bool, error) {
	if r.SensorTypeCode != "shelly_blu_motion_zb" {
		return false, nil
	}
	if c.db == nil {
		log.Printf("Shelly observation sensor=%s observed=%s motion=%v battery=%s ap=%s", r.SensorMAC, r.TS.Format(time.RFC3339Nano), motionLabel(r.Motion), ptrKey(r.BatteryPercent), r.APMAC)
		return false, nil
	}
	var changed bool
	err := c.db.QueryRow(ctx, `SELECT record_sensor_motion($1,$2,$3,$4,$5,$6,$7)`, r.SensorMAC, r.TS, r.ReceivedAt, r.Motion, r.BatteryPercent, r.PacketID, r.APMAC).Scan(&changed)
	if err == nil && changed {
		log.Printf("stored Shelly motion sensor=%s observed=%s motion=%s ap=%s", r.SensorMAC, r.TS.Format(time.RFC3339Nano), motionLabel(r.Motion), r.APMAC)
	}
	return changed, err
}

func motionLabel(v *bool) string {
	if v == nil {
		return "absent"
	}
	if *v {
		return "detected"
	}
	return "clear"
}
