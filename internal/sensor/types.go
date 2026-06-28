package sensor

import (
	"strings"
	"time"
)

type Device struct {
	MAC                      string
	Label                    string
	Location                 string
	IngestSource             string
	IngestSourceExplicit     bool
	SensorTypeCode           string
	SensorCategory           string
	Enabled                  bool
	PreserveExistingLabel    bool
	PreserveExistingLocation bool
}

type Reading struct {
	TS                  time.Time
	MAC                 string
	TemperatureC        *float64
	HumidityPercent     *float64
	BatteryPercent      *float64
	RSSI                *float64
	PressureHPa         *float64
	CO2PPM              *float64
	Lux                 *float64
	ETVOC               *float64
	SoilMoisturePercent *float64
	ConductivityUSCM    *float64
}

func NormalizeMAC(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("-", "", ":", "", ".", "")
	compact := replacer.Replace(value)
	if len(compact) != 12 {
		return ""
	}
	for _, r := range compact {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return ""
		}
	}
	parts := make([]string, 0, 6)
	for i := 0; i < len(compact); i += 2 {
		parts = append(parts, compact[i:i+2])
	}
	return strings.Join(parts, ":")
}

func (r Reading) Empty() bool {
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
