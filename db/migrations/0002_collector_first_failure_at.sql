ALTER TABLE collector_status
    ADD COLUMN IF NOT EXISTS first_failure_at timestamptz;
