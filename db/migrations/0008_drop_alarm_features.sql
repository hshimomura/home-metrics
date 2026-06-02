DROP TABLE IF EXISTS health_notification_events;
DROP TABLE IF EXISTS admin_notification_channels;
DROP TABLE IF EXISTS health_alert_state;
DROP TABLE IF EXISTS notification_events;
DROP TABLE IF EXISTS alert_rule_state;
DROP TABLE IF EXISTS alert_rules;
DROP TABLE IF EXISTS ios_devices;
DROP TABLE IF EXISTS app_users;

ALTER TABLE devices
    DROP COLUMN IF EXISTS maintenance_mode,
    DROP COLUMN IF EXISTS maintenance_reason,
    DROP COLUMN IF EXISTS maintenance_since;
