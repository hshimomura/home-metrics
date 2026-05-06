CREATE MATERIALIZED VIEW IF NOT EXISTS energy_1hour
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', ts) AS ts,
    source,
    device_key,
    metric,
    avg(value)::double precision AS value,
    min(value)::double precision AS min_value,
    max(value)::double precision AS max_value,
    count(*)::bigint AS samples
FROM energy_readings
GROUP BY 1, 2, 3, 4
WITH NO DATA;

CREATE MATERIALIZED VIEW IF NOT EXISTS energy_12hour
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('12 hours', ts) AS ts,
    source,
    device_key,
    metric,
    avg(value)::double precision AS value,
    min(value)::double precision AS min_value,
    max(value)::double precision AS max_value,
    count(*)::bigint AS samples
FROM energy_readings
GROUP BY 1, 2, 3, 4
WITH NO DATA;

CREATE MATERIALIZED VIEW IF NOT EXISTS energy_1day
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 day', ts) AS ts,
    source,
    device_key,
    metric,
    avg(value)::double precision AS value,
    min(value)::double precision AS min_value,
    max(value)::double precision AS max_value,
    count(*)::bigint AS samples
FROM energy_readings
GROUP BY 1, 2, 3, 4
WITH NO DATA;

CREATE INDEX IF NOT EXISTS energy_1hour_lookup_idx
    ON energy_1hour (source, device_key, metric, ts DESC);

CREATE INDEX IF NOT EXISTS energy_12hour_lookup_idx
    ON energy_12hour (source, device_key, metric, ts DESC);

CREATE INDEX IF NOT EXISTS energy_1day_lookup_idx
    ON energy_1day (source, device_key, metric, ts DESC);

SELECT add_continuous_aggregate_policy(
    'energy_1hour',
    start_offset => interval '14 days',
    end_offset => interval '5 minutes',
    schedule_interval => interval '15 minutes',
    if_not_exists => true
);

SELECT remove_continuous_aggregate_policy('energy_12hour', if_exists => true);

SELECT add_continuous_aggregate_policy(
    'energy_12hour',
    start_offset => interval '14 days',
    end_offset => interval '1 hour',
    schedule_interval => interval '1 hour',
    if_not_exists => true
);

SELECT remove_continuous_aggregate_policy('energy_1day', if_exists => true);

SELECT add_continuous_aggregate_policy(
    'energy_1day',
    start_offset => interval '14 days',
    end_offset => interval '6 hours',
    schedule_interval => interval '6 hours',
    if_not_exists => true
);

ALTER TABLE energy_readings SET (
    timescaledb.enable_columnstore,
    timescaledb.orderby = 'ts DESC',
    timescaledb.segmentby = 'source, device_key, metric'
);

SELECT set_chunk_time_interval('energy_readings', interval '1 day');

CALL remove_columnstore_policy('energy_readings', if_exists => true);

CALL add_columnstore_policy(
    'energy_readings',
    after => interval '2 days',
    if_not_exists => true
);

SELECT remove_retention_policy('energy_readings', if_exists => true);

SELECT add_retention_policy(
    'energy_readings',
    drop_after => interval '14 days',
    if_not_exists => true
);
