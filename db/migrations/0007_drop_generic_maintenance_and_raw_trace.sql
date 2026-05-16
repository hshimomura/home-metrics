DROP TABLE IF EXISTS health_maintenance_targets;

DROP TABLE IF EXISTS cisco_spaces_processing_events;

DROP INDEX IF EXISTS cisco_spaces_raw_events_status_received_idx;

ALTER TABLE cisco_spaces_raw_events
    DROP COLUMN IF EXISTS process_status,
    DROP COLUMN IF EXISTS processed_at,
    DROP COLUMN IF EXISTS process_error,
    DROP COLUMN IF EXISTS processor_version;
