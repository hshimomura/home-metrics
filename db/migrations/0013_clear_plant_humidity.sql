UPDATE sensor_minute sm
SET humidity_percent = NULL
FROM devices d
WHERE d.mac = sm.mac
  AND d.sensor_category = 'plant'
  AND sm.humidity_percent IS NOT NULL;

UPDATE sensor_1hour s
SET humidity_percent = NULL
FROM devices d
WHERE d.mac = s.mac
  AND d.sensor_category = 'plant'
  AND s.humidity_percent IS NOT NULL;

UPDATE sensor_12hour s
SET humidity_percent = NULL
FROM devices d
WHERE d.mac = s.mac
  AND d.sensor_category = 'plant'
  AND s.humidity_percent IS NOT NULL;

UPDATE sensor_1day s
SET humidity_percent = NULL
FROM devices d
WHERE d.mac = s.mac
  AND d.sensor_category = 'plant'
  AND s.humidity_percent IS NOT NULL;
