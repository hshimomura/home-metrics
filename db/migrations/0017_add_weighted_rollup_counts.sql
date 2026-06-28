CREATE TABLE IF NOT EXISTS rollup_accuracy_state (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    accuracy_cutoff timestamptz NOT NULL,
    initialized_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE sensor_1hour
    ADD COLUMN IF NOT EXISTS temperature_c_count bigint,
    ADD COLUMN IF NOT EXISTS humidity_percent_count bigint,
    ADD COLUMN IF NOT EXISTS battery_percent_count bigint,
    ADD COLUMN IF NOT EXISTS rssi_dbm_count bigint,
    ADD COLUMN IF NOT EXISTS pressure_hpa_count bigint,
    ADD COLUMN IF NOT EXISTS co2_ppm_count bigint,
    ADD COLUMN IF NOT EXISTS lux_count bigint,
    ADD COLUMN IF NOT EXISTS etvoc_count bigint,
    ADD COLUMN IF NOT EXISTS soil_moisture_percent_count bigint,
    ADD COLUMN IF NOT EXISTS conductivity_us_cm_count bigint;

ALTER TABLE sensor_12hour
    ADD COLUMN IF NOT EXISTS temperature_c_count bigint,
    ADD COLUMN IF NOT EXISTS humidity_percent_count bigint,
    ADD COLUMN IF NOT EXISTS battery_percent_count bigint,
    ADD COLUMN IF NOT EXISTS rssi_dbm_count bigint,
    ADD COLUMN IF NOT EXISTS pressure_hpa_count bigint,
    ADD COLUMN IF NOT EXISTS co2_ppm_count bigint,
    ADD COLUMN IF NOT EXISTS lux_count bigint,
    ADD COLUMN IF NOT EXISTS etvoc_count bigint,
    ADD COLUMN IF NOT EXISTS soil_moisture_percent_count bigint,
    ADD COLUMN IF NOT EXISTS conductivity_us_cm_count bigint;

ALTER TABLE sensor_1day
    ADD COLUMN IF NOT EXISTS temperature_c_count bigint,
    ADD COLUMN IF NOT EXISTS humidity_percent_count bigint,
    ADD COLUMN IF NOT EXISTS battery_percent_count bigint,
    ADD COLUMN IF NOT EXISTS rssi_dbm_count bigint,
    ADD COLUMN IF NOT EXISTS pressure_hpa_count bigint,
    ADD COLUMN IF NOT EXISTS co2_ppm_count bigint,
    ADD COLUMN IF NOT EXISTS lux_count bigint,
    ADD COLUMN IF NOT EXISTS etvoc_count bigint,
    ADD COLUMN IF NOT EXISTS soil_moisture_percent_count bigint,
    ADD COLUMN IF NOT EXISTS conductivity_us_cm_count bigint;
