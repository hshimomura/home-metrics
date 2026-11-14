package main

import (
	"testing"
	"time"
)

func TestConfigRejectsUnsafeIntervals(t *testing.T) {
	valid := loadConfig()
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*config){func(c *config) { c.StreamHeartbeat = 0 }, func(c *config) { c.StreamHeartbeat = 61 * time.Second }, func(c *config) { c.AggregateFlush = -time.Second }, func(c *config) { c.ReconnectMaxDelay = 0 }, func(c *config) { c.MQTTMaxPacket = 0 }} {
		cfg := valid
		mutate(&cfg)
		if cfg.validate() == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
}
func TestDecodeRejectsMalformedWireAndFutureTimestamp(t *testing.T) {
	if _, err := parseDataSubscription(protoField(1, 0, protoVarint(1))); err == nil {
		t.Fatal("accepted wrong wire type")
	}
	if _, _, err := consumeVarint([]byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 2}); err == nil {
		t.Fatal("accepted overflowing varint")
	}
	mac := "fc:4d:6a:26:53:e3"
	future := time.Now().Add(24 * time.Hour)
	payload := protoMessage(1, protoField(1, 2, protoString(mac)), protoField(2, 2, protoBytes([]byte{0x44, 1, 100, 0x21, 1})), protoMessage(3, protoField(1, 0, protoVarint(uint64(future.Unix())))))
	readings, err := decodeDataBatch(payload, map[string]targetDevice{mac: {MAC: mac, SensorTypeCode: "shelly_blu_motion_zb"}})
	if err != nil || len(readings) != 1 || !readings[0].TS.Equal(readings[0].ReceivedAt) {
		t.Fatalf("future timestamp adopted: %+v err=%v", readings, err)
	}
}
