CREATE TABLE IF NOT EXISTS collector_status (
    collector_name text NOT NULL,
    target_type text NOT NULL,
    target_key text NOT NULL DEFAULT 'default',
    last_attempt_at timestamptz,
    last_success_at timestamptz,
    last_data_at timestamptz,
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
