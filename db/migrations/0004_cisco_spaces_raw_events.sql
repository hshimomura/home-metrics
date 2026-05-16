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

SELECT add_retention_policy(
    'cisco_spaces_raw_events',
    drop_after => interval '14 days',
    if_not_exists => true
);

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
