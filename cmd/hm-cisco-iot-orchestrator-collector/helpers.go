package main

import (
	"math"

	"home-metrics/internal/sensor"
)

func normalizeMAC(value string) string {
	return sensor.NormalizeMAC(value)
}

func sanitizeRange(value *float64, minValue float64, maxValue float64) *float64 {
	if value == nil || !isFinite(*value) || *value < minValue || *value > maxValue {
		return nil
	}
	return value
}

func round(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func floatPtr(value float64) *float64 {
	return &value
}
