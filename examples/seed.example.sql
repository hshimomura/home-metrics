INSERT INTO energy_devices (source, device_key, label, location)
VALUES
    ('nature_remo', 'remo-e', 'Nature Remo E', 'Home'),
    ('echonet', 'echonet-device', 'ECHONET Lite device', 'Home'),
    ('apcupsd', 'ups', 'APC UPS', 'Home')
ON CONFLICT (source, device_key) DO UPDATE SET
    label = EXCLUDED.label,
    location = EXCLUDED.location,
    updated_at = now();
