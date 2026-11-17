package main

import (
	"testing"

	"home-metrics/internal/bledecode"
)

func FuzzBLEDecoders(f *testing.F) {
	f.Add([]byte{0x44, 0, 0x11, 1, 100, 0x21, 1})
	f.Add([]byte{2, 1, 6, 0x0c, 0x16, 0xd2, 0xfc, 0x44, 0, 0x11, 1, 100, 0x21, 1, 0x64, 0})
	f.Add([]byte{0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		bledecode.DecodeAdvertisement(b)
		bledecode.DecodeServiceData(b)
		parseDataBatch(b)
	})
}
