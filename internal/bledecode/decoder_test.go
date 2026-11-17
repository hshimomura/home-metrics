package bledecode

import (
	"encoding/hex"
	"testing"
)

func TestDecodeAdvertisementProtocols(t *testing.T) {
	tests := []struct {
		name   string
		hex    string
		assert func(*testing.T, Reading)
	}{
		{
			name: "BTHome motion",
			hex:  "0201060c16d2fc440011016421016400",
			assert: func(t *testing.T, reading Reading) {
				if reading.Motion == nil || !*reading.Motion || reading.PacketID == nil || *reading.PacketID != 17 {
					t.Fatalf("reading=%+v", reading)
				}
			},
		},
		{
			name: "environment",
			hex:  "0201061b166afe0305177bd47b44041f071a0403ff00000313991303200a00",
			assert: func(t *testing.T, reading Reading) {
				if reading.TemperatureC == nil || *reading.TemperatureC != 19.6 || reading.CO2PPM == nil || *reading.CO2PPM != 1050 {
					t.Fatalf("reading=%+v", reading)
				}
			},
		},
		{
			name: "Xiaomi FE95",
			hex:  "020106030295fe131695fe71209800977d73147e855c0d0810010b",
			assert: func(t *testing.T, reading Reading) {
				if reading.SoilMoisturePercent == nil || *reading.SoilMoisturePercent != 11 || reading.HumidityPercent != nil {
					t.Fatalf("reading=%+v", reading)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := hex.DecodeString(test.hex)
			if err != nil {
				t.Fatal(err)
			}
			test.assert(t, DecodeAdvertisement(payload))
		})
	}
}

func TestDecodeServiceDataRejectsMalformedBTHome(t *testing.T) {
	reading := DecodeServiceData([]byte{0x44, 0x11, 0x01, 0x64, 0x21})
	if reading.Motion != nil || reading.BatteryPercent != nil || reading.PacketID != nil {
		t.Fatalf("reading=%+v", reading)
	}
}
