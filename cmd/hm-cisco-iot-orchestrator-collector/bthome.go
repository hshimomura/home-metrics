package main

import (
	"context"
	"log"
	"time"
)

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
