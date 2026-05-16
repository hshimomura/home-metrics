CREATE TABLE IF NOT EXISTS health_maintenance_targets (
    alert_key text PRIMARY KEY,
    target_kind text NOT NULL,
    target_label text NOT NULL DEFAULT '',
    reason text,
    started_at timestamptz NOT NULL DEFAULT now(),
    ends_at timestamptz,
    created_by text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at IS NULL OR ends_at > started_at)
);

CREATE INDEX IF NOT EXISTS health_maintenance_targets_active_idx
    ON health_maintenance_targets (ends_at, updated_at DESC);

INSERT INTO health_maintenance_targets (
    alert_key,
    target_kind,
    target_label,
    reason,
    started_at,
    created_by,
    updated_at
)
SELECT
    'metric:sensor_minute:' || replace(mac, ':', '_') || ':data',
    'sensor',
    COALESCE(NULLIF(label, ''), mac),
    maintenance_reason,
    COALESCE(maintenance_since, now()),
    'migration',
    now()
FROM devices
WHERE COALESCE(maintenance_mode, false)
ON CONFLICT (alert_key) DO UPDATE SET
    target_kind = EXCLUDED.target_kind,
    target_label = EXCLUDED.target_label,
    reason = COALESCE(health_maintenance_targets.reason, EXCLUDED.reason),
    started_at = LEAST(health_maintenance_targets.started_at, EXCLUDED.started_at),
    updated_at = now();
