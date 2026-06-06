package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultDBDSN = "dbname=ble_sensors host=/var/run/postgresql"

type device struct {
	MAC        string
	Label      string
	SensorCategory string
}

type sensorField struct {
	Name   string
	Column string
	Unit   string
}

var sensorFields = []sensorField{
	{Name: "Temperature", Column: "temperature_c", Unit: "C"},
	{Name: "Humidity", Column: "humidity_percent", Unit: "%RH"},
	{Name: "Battery", Column: "battery_percent", Unit: "%"},
	{Name: "RSSI", Column: "rssi_dbm", Unit: "dBm"},
	{Name: "Pressure", Column: "pressure_hpa", Unit: "hPa"},
	{Name: "CO2", Column: "co2_ppm", Unit: "ppm"},
	{Name: "Lux", Column: "lux", Unit: "lux"},
	{Name: "eTVOC", Column: "etvoc", Unit: ""},
	{Name: "Soil moisture", Column: "soil_moisture_percent", Unit: "%"},
	{Name: "Conductivity", Column: "conductivity_us_cm", Unit: "uS/cm"},
}

func main() {
	ctx := context.Background()
	dsn := os.Getenv("BLE_DB_DSN")
	if dsn == "" {
		dsn = defaultDBDSN
	}

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close(ctx)

	devices, err := loadDevices(ctx, db)
	if err != nil {
		log.Fatalf("load devices: %v", err)
	}
	if len(devices) == 0 {
		log.Fatal("no devices found")
	}

	reader := bufio.NewReader(os.Stdin)
	selectedDevice, err := chooseDevice(reader, devices)
	if err != nil {
		log.Fatal(err)
	}
	selectedField, err := chooseField(reader)
	if err != nil {
		log.Fatal(err)
	}

	if err := printLatest(ctx, db, selectedDevice, selectedField); err != nil {
		log.Fatalf("query latest values: %v", err)
	}
}

func loadDevices(ctx context.Context, db *pgx.Conn) ([]device, error) {
	rows, err := db.Query(ctx, `
		SELECT mac, label, COALESCE(sensor_category, '')
		FROM devices
		WHERE enabled
		ORDER BY mac
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []device
	for rows.Next() {
		var d device
		if err := rows.Scan(&d.MAC, &d.Label, &d.SensorCategory); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func chooseDevice(reader *bufio.Reader, devices []device) (device, error) {
	fmt.Println("Devices:")
	for i, d := range devices {
		fmt.Printf("%2d. %-17s %-12s %s\n", i+1, d.MAC, d.Label, d.SensorCategory)
	}
	index, err := promptIndex(reader, "Select device", len(devices))
	if err != nil {
		return device{}, err
	}
	return devices[index], nil
}

func chooseField(reader *bufio.Reader) (sensorField, error) {
	fmt.Println()
	fmt.Println("Sensor fields:")
	for i, field := range sensorFields {
		label := field.Name
		if field.Unit != "" {
			label += " (" + field.Unit + ")"
		}
		fmt.Printf("%2d. %s\n", i+1, label)
	}
	index, err := promptIndex(reader, "Select field", len(sensorFields))
	if err != nil {
		return sensorField{}, err
	}
	return sensorFields[index], nil
}

func promptIndex(reader *bufio.Reader, label string, max int) (int, error) {
	for {
		fmt.Printf("%s [1-%d]: ", label, max)
		input, err := reader.ReadString('\n')
		if err != nil {
			return 0, err
		}
		input = strings.TrimSpace(input)
		value, err := strconv.Atoi(input)
		if err != nil || value < 1 || value > max {
			fmt.Printf("Please enter a number from 1 to %d.\n", max)
			continue
		}
		return value - 1, nil
	}
}

func printLatest(ctx context.Context, db *pgx.Conn, d device, field sensorField) error {
	query := fmt.Sprintf(`
		SELECT ts, %s
		FROM sensor_minute
		WHERE mac = $1 AND %s IS NOT NULL
		ORDER BY ts DESC
		LIMIT 5
	`, field.Column, field.Column)

	rows, err := db.Query(ctx, query, d.MAC)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println()
	fmt.Printf("Latest 5 values: %s / %s\n", d.Label, field.Name)
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "Time\tValue\tUnit")

	count := 0
	for rows.Next() {
		var ts time.Time
		var value float64
		if err := rows.Scan(&ts, &value); err != nil {
			return err
		}
		fmt.Fprintf(
			writer,
			"%s\t%.2f\t%s\n",
			ts.Local().Format("2006-01-02 15:04:05 MST"),
			value,
			field.Unit,
		)
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		fmt.Fprintln(writer, "(no rows)\t\t")
	}
	return writer.Flush()
}
