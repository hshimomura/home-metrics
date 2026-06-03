CREATE EXTENSION IF NOT EXISTS timescaledb;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version bigint PRIMARY KEY,
    name text NOT NULL,
    checksum text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS devices (
    mac text PRIMARY KEY,
    label text NOT NULL,
    sensor_category text,
    location text,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sensor_minute (
    ts timestamptz NOT NULL,
    mac text NOT NULL REFERENCES devices(mac),
    temperature_c double precision,
    humidity_percent double precision,
    battery_percent double precision,
    rssi_dbm double precision,
    pressure_hpa double precision,
    co2_ppm double precision,
    lux double precision,
    etvoc double precision,
    inserted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ts, mac)
);

SELECT create_hypertable('sensor_minute', 'ts', if_not_exists => true);

CREATE INDEX IF NOT EXISTS sensor_minute_mac_ts_desc_idx
    ON sensor_minute (mac, ts DESC);

CREATE TABLE IF NOT EXISTS sensor_1hour (
    ts timestamptz NOT NULL,
    mac text NOT NULL REFERENCES devices(mac),
    temperature_c double precision,
    humidity_percent double precision,
    battery_percent double precision,
    rssi_dbm double precision,
    pressure_hpa double precision,
    co2_ppm double precision,
    lux double precision,
    etvoc double precision,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ts, mac)
);

SELECT create_hypertable('sensor_1hour', 'ts', if_not_exists => true);

CREATE INDEX IF NOT EXISTS sensor_1hour_mac_ts_desc_idx
    ON sensor_1hour (mac, ts DESC);

CREATE TABLE IF NOT EXISTS sensor_12hour (
    ts timestamptz NOT NULL,
    mac text NOT NULL REFERENCES devices(mac),
    temperature_c double precision,
    humidity_percent double precision,
    battery_percent double precision,
    rssi_dbm double precision,
    pressure_hpa double precision,
    co2_ppm double precision,
    lux double precision,
    etvoc double precision,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ts, mac)
);

SELECT create_hypertable('sensor_12hour', 'ts', if_not_exists => true);

CREATE INDEX IF NOT EXISTS sensor_12hour_mac_ts_desc_idx
    ON sensor_12hour (mac, ts DESC);

CREATE TABLE IF NOT EXISTS sensor_1day (
    ts timestamptz NOT NULL,
    mac text NOT NULL REFERENCES devices(mac),
    temperature_c double precision,
    humidity_percent double precision,
    battery_percent double precision,
    rssi_dbm double precision,
    pressure_hpa double precision,
    co2_ppm double precision,
    lux double precision,
    etvoc double precision,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ts, mac)
);

SELECT create_hypertable('sensor_1day', 'ts', if_not_exists => true);

CREATE INDEX IF NOT EXISTS sensor_1day_mac_ts_desc_idx
    ON sensor_1day (mac, ts DESC);

CREATE TABLE IF NOT EXISTS collector_status (
    collector_name text NOT NULL,
    target_type text NOT NULL,
    target_key text NOT NULL DEFAULT 'default',
    last_attempt_at timestamptz,
    last_success_at timestamptz,
    last_data_at timestamptz,
    first_failure_at timestamptz,
    last_failure_at timestamptz,
    last_error text,
    consecutive_failures integer NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collector_name, target_type, target_key)
);

CREATE TABLE IF NOT EXISTS energy_devices (
    source text NOT NULL,
    device_key text NOT NULL,
    label text NOT NULL,
    location text,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, device_key)
);

CREATE TABLE IF NOT EXISTS energy_readings (
    ts timestamptz NOT NULL,
    source text NOT NULL,
    device_key text NOT NULL,
    instance text,
    metric text NOT NULL,
    value double precision NOT NULL,
    unit text,
    raw_property text,
    raw_topic text,
    inserted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ts, source, device_key, metric)
);

SELECT create_hypertable('energy_readings', 'ts', if_not_exists => true);

CREATE INDEX IF NOT EXISTS energy_readings_device_metric_ts_desc_idx
    ON energy_readings (source, device_key, metric, ts DESC);

CREATE TABLE IF NOT EXISTS energy_metric_definitions (
    source text NOT NULL,
    metric text NOT NULL,
    display_name text NOT NULL,
    unit text,
    raw_property text,
    raw_instance text,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, metric)
);

INSERT INTO energy_metric_definitions (
    source,
    metric,
    display_name,
    unit,
    raw_property,
    raw_instance
)
VALUES
    ('nature_remo', 'measured_instantaneous_w', 'Measured instantaneous power', 'W', 'measured_instantaneous', NULL),
    ('echonet', 'solar_generation_w', 'Solar generation', 'W', 'instantaneousElectricPowerGeneration', '027901'),
    ('echonet', 'battery_remaining', 'Battery remaining', '%', 'remainingCapacity3', '027d01'),
    ('echonet', 'battery_power_w', 'Battery charge/discharge power', 'W', 'instantaneousChargingAndDischargingElectricPower', '027d01'),
    ('apcupsd', 'input_voltage_v', 'UPS input voltage', 'V', 'LINEV', NULL),
    ('apcupsd', 'load_percent', 'UPS load', '%', 'LOADPCT', NULL),
    ('apcupsd', 'battery_charge_percent', 'UPS battery charge', '%', 'BCHARGE', NULL),
    ('apcupsd', 'battery_voltage_v', 'UPS battery voltage', 'V', 'BATTV', NULL)
ON CONFLICT (source, metric) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    unit = EXCLUDED.unit,
    raw_property = EXCLUDED.raw_property,
    raw_instance = EXCLUDED.raw_instance,
    updated_at = now();

DELETE FROM energy_metric_definitions
WHERE source = 'echonet'
  AND metric = 'reverse_energy';
