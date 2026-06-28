package sensor

// Metric is the stable API key and PostgreSQL column pair for a sensor value.
type Metric struct {
	Key         string
	Column      string
	DisplayName string
	Unit        string
}

var Metrics = []Metric{
	{Key: "temperature_c", Column: "temperature_c", DisplayName: "Temperature", Unit: "C"},
	{Key: "humidity_percent", Column: "humidity_percent", DisplayName: "Humidity", Unit: "%RH"},
	{Key: "battery_percent", Column: "battery_percent", DisplayName: "Battery", Unit: "%"},
	{Key: "rssi_dbm", Column: "rssi_dbm", DisplayName: "RSSI", Unit: "dBm"},
	{Key: "pressure_hpa", Column: "pressure_hpa", DisplayName: "Pressure", Unit: "hPa"},
	{Key: "co2_ppm", Column: "co2_ppm", DisplayName: "CO2", Unit: "ppm"},
	{Key: "lux", Column: "lux", DisplayName: "Lux", Unit: "lux"},
	{Key: "etvoc", Column: "etvoc", DisplayName: "eTVOC"},
	{Key: "soil_moisture_percent", Column: "soil_moisture_percent", DisplayName: "Soil moisture", Unit: "%"},
	{Key: "conductivity_us_cm", Column: "conductivity_us_cm", DisplayName: "Conductivity", Unit: "uS/cm"},
}

func Columns() map[string]string {
	columns := make(map[string]string, len(Metrics))
	for _, metric := range Metrics {
		columns[metric.Key] = metric.Column
	}
	return columns
}
