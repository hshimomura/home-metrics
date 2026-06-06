ALTER TABLE sensor_minute
    ADD COLUMN IF NOT EXISTS soil_moisture_percent double precision,
    ADD COLUMN IF NOT EXISTS conductivity_us_cm double precision;

ALTER TABLE sensor_1hour
    ADD COLUMN IF NOT EXISTS soil_moisture_percent double precision,
    ADD COLUMN IF NOT EXISTS conductivity_us_cm double precision;

ALTER TABLE sensor_12hour
    ADD COLUMN IF NOT EXISTS soil_moisture_percent double precision,
    ADD COLUMN IF NOT EXISTS conductivity_us_cm double precision;

ALTER TABLE sensor_1day
    ADD COLUMN IF NOT EXISTS soil_moisture_percent double precision,
    ADD COLUMN IF NOT EXISTS conductivity_us_cm double precision;
