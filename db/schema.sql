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

CREATE TABLE IF NOT EXISTS app_users (
    id bigserial PRIMARY KEY,
    display_name text NOT NULL DEFAULT 'default',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ios_devices (
    id bigserial PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    apns_device_token text NOT NULL,
    app_bundle_id text NOT NULL,
    apns_environment text NOT NULL CHECK (apns_environment IN ('sandbox', 'production')),
    device_name text,
    enabled boolean NOT NULL DEFAULT true,
    disabled_reason text,
    disabled_at timestamptz,
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (apns_device_token, app_bundle_id, apns_environment)
);

CREATE TABLE IF NOT EXISTS alert_rules (
    id bigserial PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    mac text NOT NULL REFERENCES devices(mac),
    metric text NOT NULL CHECK (
        metric IN (
            'temperature_c',
            'humidity_percent',
            'battery_percent',
            'rssi_dbm',
            'pressure_hpa',
            'co2_ppm',
            'lux',
            'etvoc'
        )
    ),
    operator text NOT NULL CHECK (operator IN ('>', '>=', '<', '<=')),
    threshold double precision NOT NULL,
    cooldown_duration interval NOT NULL DEFAULT interval '24 hours',
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS alert_rules_enabled_metric_idx
    ON alert_rules (enabled, mac, metric);

CREATE TABLE IF NOT EXISTS alert_rule_state (
    alert_rule_id bigint PRIMARY KEY REFERENCES alert_rules(id) ON DELETE CASCADE,
    last_triggered_at timestamptz,
    last_notified_at timestamptz,
    last_value double precision,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS notification_events (
    id bigserial PRIMARY KEY,
    alert_rule_id bigint REFERENCES alert_rules(id) ON DELETE SET NULL,
    user_id bigint REFERENCES app_users(id) ON DELETE SET NULL,
    mac text NOT NULL REFERENCES devices(mac),
    metric text NOT NULL,
    value double precision,
    threshold double precision,
    triggered_at timestamptz NOT NULL,
    sent_at timestamptz,
    status text NOT NULL CHECK (status IN ('pending', 'dry_run', 'sent', 'failed', 'skipped')),
    error_message text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS notification_events_user_created_idx
    ON notification_events (user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS notification_events_rule_created_idx
    ON notification_events (alert_rule_id, created_at DESC);

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

CREATE TABLE IF NOT EXISTS health_alert_state (
    alert_key text PRIMARY KEY,
    status text NOT NULL CHECK (status IN ('firing', 'resolved')),
    severity text NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    title text NOT NULL,
    source text NOT NULL,
    summary text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    first_fired_at timestamptz,
    last_evaluated_at timestamptz NOT NULL,
    last_notified_at timestamptz,
    acknowledged_at timestamptz,
    acknowledged_by text,
    muted_until timestamptz,
    muted_by text,
    muted_reason text,
    manually_resolved_at timestamptz,
    manually_resolved_by text,
    resolved_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS health_alert_state_status_updated_idx
    ON health_alert_state (status, updated_at DESC);

CREATE TABLE IF NOT EXISTS admin_notification_channels (
    id bigserial PRIMARY KEY,
    channel_type text NOT NULL CHECK (channel_type IN ('generic_webhook')),
    name text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    target text,
    secret_ref text,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS health_notification_events (
    id bigserial PRIMARY KEY,
    event_id text NOT NULL UNIQUE,
    alert_key text NOT NULL REFERENCES health_alert_state(alert_key) ON DELETE CASCADE,
    channel_id bigint REFERENCES admin_notification_channels(id) ON DELETE SET NULL,
    channel_type text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'dry_run', 'sent', 'failed', 'skipped')),
    http_status integer,
    response_body text,
    error text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS health_notification_events_alert_created_idx
    ON health_notification_events (alert_key, created_at DESC);

CREATE INDEX IF NOT EXISTS health_notification_events_status_created_idx
    ON health_notification_events (status, created_at DESC);

CREATE TABLE IF NOT EXISTS cisco_spaces_raw_events (
    received_at timestamptz NOT NULL DEFAULT now(),
    id bigserial NOT NULL,
    record_uid text,
    record_timestamp timestamptz,
    event_type text,
    device_mac text,
    device_id text,
    device_label text,
    location_id text,
    map_id text,
    payload jsonb NOT NULL,
    payload_sha256 text NOT NULL,
    process_status text NOT NULL DEFAULT 'pending'
        CHECK (process_status IN ('pending', 'processed', 'failed', 'ignored')),
    processed_at timestamptz,
    process_error text,
    processor_version text,
    PRIMARY KEY (received_at, id)
);

SELECT create_hypertable('cisco_spaces_raw_events', 'received_at', if_not_exists => true);

CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_record_uid_idx
    ON cisco_spaces_raw_events (record_uid)
    WHERE record_uid IS NOT NULL;

CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_device_received_idx
    ON cisco_spaces_raw_events (device_mac, received_at DESC);

CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_status_received_idx
    ON cisco_spaces_raw_events (process_status, received_at);

CREATE INDEX IF NOT EXISTS cisco_spaces_raw_events_received_idx
    ON cisco_spaces_raw_events (received_at DESC);

CREATE TABLE IF NOT EXISTS cisco_spaces_processing_events (
    id bigserial PRIMARY KEY,
    raw_received_at timestamptz NOT NULL,
    raw_id bigint NOT NULL,
    output_ts timestamptz,
    output_mac text,
    output_metric text,
    output_table text NOT NULL DEFAULT 'sensor_minute',
    processor_run_id text,
    processor_version text,
    status text NOT NULL CHECK (status IN ('processed', 'ignored', 'failed')),
    reason text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cisco_spaces_processing_events_raw_idx
    ON cisco_spaces_processing_events (raw_received_at, raw_id);

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

INSERT INTO app_users (id, display_name)
VALUES (1, 'default')
ON CONFLICT (id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    updated_at = now();

SELECT setval(
    pg_get_serial_sequence('app_users', 'id'),
    GREATEST((SELECT max(id) FROM app_users), 1)
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
