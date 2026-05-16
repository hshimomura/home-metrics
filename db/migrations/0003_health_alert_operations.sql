ALTER TABLE health_alert_state
    ADD COLUMN IF NOT EXISTS acknowledged_at timestamptz,
    ADD COLUMN IF NOT EXISTS acknowledged_by text,
    ADD COLUMN IF NOT EXISTS muted_until timestamptz,
    ADD COLUMN IF NOT EXISTS muted_by text,
    ADD COLUMN IF NOT EXISTS muted_reason text,
    ADD COLUMN IF NOT EXISTS manually_resolved_at timestamptz,
    ADD COLUMN IF NOT EXISTS manually_resolved_by text;
