# Client Contract

Clients should use server-provided metadata and metric names. They must not
classify devices from labels, MAC prefixes, collector process names, or the
presence of a recent metric.

## Device Classification

`GET /api/devices` and the `device` object in the latest endpoint expose:

| Field | Meaning |
| --- | --- |
| `ingest_source` | Telemetry transport and owning collector. |
| `sensor_type_code` | Stable decoder/model identifier. |
| `sensor_type` | Optional display metadata joined from `sensor_types`. |
| `sensor_category` | Client-facing category such as `plant` or `environment`. |
| `enabled` | Whether the device is enabled by its owning collector. |

For example:

```json
{
  "mac": "5c:85:7e:14:73:7d",
  "label": "Blueberry1",
  "location": "Greenhouse",
  "enabled": true,
  "ingest_source": "cisco_sensor_connect",
  "sensor_type_code": "xiaomi_flower_care",
  "sensor_type": {
    "code": "xiaomi_flower_care",
    "display_name": "Xiaomi Flower Care",
    "category": "plant",
    "vendor": "Xiaomi / HHCC",
    "model": "HHCCJCY01"
  },
  "sensor_category": "plant"
}
```

Use `enabled == true` for normal device lists. Use
`sensor_category == "plant"` for plant UI placement. Devices without `plant`
remain general sensor devices unless a client has another explicitly supported
category. If latest telemetry returns `404`, keep the configured device visible
and show that no reading is available.

## Metric Semantics

| Key | Meaning | Unit |
| --- | --- | --- |
| `temperature_c` | Temperature | C |
| `humidity_percent` | Air relative humidity | %RH |
| `battery_percent` | Device battery | % |
| `rssi_dbm` | Received signal strength | dBm |
| `pressure_hpa` | Air pressure | hPa |
| `co2_ppm` | Carbon dioxide | ppm |
| `lux` | Illuminance | lux |
| `etvoc` | Equivalent total volatile organic compounds | device-defined |
| `soil_moisture_percent` | Soil moisture | % |
| `conductivity_us_cm` | Soil conductivity | uS/cm |

Soil moisture and air humidity are different measurements and must never be
substituted for one another. The stable name is `conductivity_us_cm`; user
interfaces should label it `Conductivity` or `導電率`, not `Fertility`.

## Latest Snapshot

The latest endpoint returns all supported keys in `values`; an unavailable
metric is `null`. `value_timestamps` contains only metrics that have a non-null
stored value. Its timestamps may differ because some values, especially GATT
battery, are sampled less frequently.

```json
{
  "device": {
    "mac": "5c:85:7e:14:73:7d",
    "label": "Blueberry1",
    "enabled": true,
    "ingest_source": "cisco_sensor_connect",
    "sensor_type_code": "xiaomi_flower_care",
    "sensor_category": "plant"
  },
  "ts": "2026-06-28T03:18:00Z",
  "values": {
    "temperature_c": 24.4,
    "humidity_percent": null,
    "battery_percent": 100,
    "rssi_dbm": null,
    "pressure_hpa": null,
    "co2_ppm": null,
    "lux": 4307,
    "etvoc": null,
    "soil_moisture_percent": 26,
    "conductivity_us_cm": 279
  },
  "value_timestamps": {
    "temperature_c": "2026-06-28T03:18:00Z",
    "battery_percent": "2026-06-27T03:24:00Z",
    "lux": "2026-06-28T03:18:00Z",
    "soil_moisture_percent": "2026-06-28T03:18:00Z",
    "conductivity_us_cm": "2026-06-28T03:18:00Z"
  }
}
```

The top-level `ts` is the newest metric timestamp. A client may use an
individual `value_timestamps` entry to display metric age or de-emphasize stale
values. It should not assume every value was measured at top-level `ts`.

## Series

Series points retain their measurement timeline. The API does not carry an old
battery or other sparse metric into later timestamps. Valid ranges and metric
keys are defined by `docs/openapi.yaml`.

## Compatibility

The stable client contract is:

- classify with `sensor_category`;
- identify decoder/model with `sensor_type_code`;
- treat optional metadata as optional;
- treat unknown future metric keys as ignorable;
- use `value_timestamps` when metric age matters;
- do not depend on removed legacy classification fields.

Downstream application source, UI plans, and Grafana dashboards are maintained
outside this repository.
