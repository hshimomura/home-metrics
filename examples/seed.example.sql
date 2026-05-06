INSERT INTO devices (mac, label, sensor_category, location)
VALUES
    ('aa:bb:cc:dd:ee:01', 'Living room', 'Minew', 'Living room'),
    ('aa:bb:cc:dd:ee:02', 'Bedroom CO2', 'HibouCO2', 'Bedroom')
ON CONFLICT (mac) DO UPDATE SET
    label = EXCLUDED.label,
    sensor_category = EXCLUDED.sensor_category,
    location = EXCLUDED.location,
    updated_at = now();

INSERT INTO energy_devices (source, device_key, label, location)
VALUES
    ('nature_remo', 'remo-e', 'Nature Remo E', 'Home'),
    ('echonet', 'echonet-device', 'ECHONET Lite device', 'Home'),
    ('apcupsd', 'ups', 'APC UPS', 'Home')
ON CONFLICT (source, device_key) DO UPDATE SET
    label = EXCLUDED.label,
    location = EXCLUDED.location,
    updated_at = now();
