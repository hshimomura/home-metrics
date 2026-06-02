#!/usr/bin/env python3
import argparse
import csv
import json
import struct
from collections import defaultdict


TARGET_LABELS = {
    "green": "Greenhouse",
    "desk": "Desk",
    "living": "Living",
    "loft": "Loft",
    "bed": "Bed",
    "dc": "DC",
}


def s16le(lo, hi):
    value = lo | (hi << 8)
    return value - 0x10000 if value & 0x8000 else value


def decode_service_data(hex_payload):
    data = bytes.fromhex(hex_payload)
    decoded = {
        "payload_kind": "unknown",
        "temperature_c": None,
        "humidity_percent": None,
        "battery_percent": None,
        "pressure_hpa": None,
        "co2_ppm": None,
        "lux": None,
        "etvoc": None,
        "gas_subtype": None,
        "gas_value": None,
        "notes": [],
    }

    if len(data) >= 5 and data[:3] == b"\x02\x80\x02":
        decoded["payload_kind"] = "static_or_status"
        battery_candidates = [value for value in data[3:5] if value <= 100]
        if battery_candidates:
            decoded["battery_percent"] = max(battery_candidates)
        if len(data) > 5:
            try:
                decoded["notes"].append(f"ascii_tail={data[5:].decode('ascii')}")
            except UnicodeDecodeError:
                pass

    temp_marker = b"\x03\x13"
    temp_at = data.find(temp_marker)
    if temp_at >= 0 and temp_at + 4 <= len(data):
        decoded["payload_kind"] = "sensor_values"
        raw_temp = s16le(data[temp_at + 2], data[temp_at + 3])
        decoded["temperature_c"] = round(raw_temp / 256.0, 2)

    humidity_marker = b"\x02\x12"
    humidity_at = data.find(humidity_marker)
    if humidity_at >= 0 and humidity_at + 3 <= len(data):
        decoded["payload_kind"] = "sensor_values"
        decoded["humidity_percent"] = data[humidity_at + 2]

    if data.startswith(b"\x03\x02\x0a") and len(data) >= 6:
        decoded["payload_kind"] = "short_sensor_values"
        if humidity_at >= 0 and humidity_at + 3 <= len(data):
            decoded["humidity_percent"] = data[humidity_at + 2]
        decoded["notes"].append("short HibouCO2 frame; temperature not present")

    # HibouCO2 appears as segmented FE6A service data in this BlueZ capture.
    # The public HibouAir examples expose ALS/light, pressure, temperature,
    # humidity, VOC and CO2 from advertising frames; these markers are inferred
    # from the local FE6A payload shape.
    if data.startswith(b"\x03\x05\x17") and len(data) >= 24:
        decoded["payload_kind"] = "hibou_air_values"
        decoded["pressure_hpa"] = round(struct.unpack("<f", data[3:7])[0], 2)

        gas_at = data.find(b"\x04\x1f")
        if gas_at >= 0 and gas_at + 5 <= len(data):
            subtype = data[gas_at + 2]
            value = int.from_bytes(data[gas_at + 3 : gas_at + 5], "little")
            decoded["gas_subtype"] = subtype
            decoded["gas_value"] = value
            if subtype == 0x07:
                decoded["co2_ppm"] = value
            elif subtype == 0x08:
                decoded["etvoc"] = value
            else:
                decoded["notes"].append(f"unmapped_hibou_gas_subtype=0x{subtype:02x}")

        lux_at = data.find(b"\x03\x20")
        if lux_at >= 0 and lux_at + 4 <= len(data):
            decoded["lux"] = int.from_bytes(data[lux_at + 2 : lux_at + 4], "little")

    return decoded


def service_hex(record):
    for value in (record.get("service_data") or {}).values():
        if isinstance(value, dict) and value.get("hex"):
            return value["hex"]
    return ""


def main():
    parser = argparse.ArgumentParser(description="Decode BLE service data captured by tools/scan_bluez.py.")
    parser.add_argument("input", nargs="?", default="home-metrics-bluez-scan.jsonl")
    parser.add_argument("--csv", default="home-metrics-decoded.csv")
    args = parser.parse_args()

    rows = []
    latest = {}
    state = defaultdict(
        lambda: {
            "temperature_c": None,
            "humidity_percent": None,
            "battery_percent": None,
            "pressure_hpa": None,
            "co2_ppm": None,
            "lux": None,
            "etvoc": None,
            "gas_subtype": None,
            "gas_value": None,
            "timestamp": None,
            "label": None,
            "address": None,
            "name": None,
            "rssi": None,
            "payload_hex": None,
        }
    )
    battery_by_key = {}
    samples_by_key = defaultdict(list)

    with open(args.input, encoding="utf-8") as source:
        for line in source:
            record = json.loads(line)
            target = record.get("target") or {}
            key = target.get("key") or record["address"]
            label = target.get("label") or TARGET_LABELS.get(key) or record.get("alias") or key
            payload = service_hex(record)
            if not payload:
                continue

            decoded = decode_service_data(payload)
            if decoded["battery_percent"] is not None:
                battery_by_key[key] = decoded["battery_percent"]

            battery = decoded["battery_percent"]
            if battery is None:
                battery = battery_by_key.get(key)

            row = {
                "timestamp": record["timestamp"],
                "key": key,
                "label": label,
                "address": record["address"],
                "name": record.get("name"),
                "rssi": record.get("rssi"),
                "payload_hex": payload,
                "payload_kind": decoded["payload_kind"],
                "temperature_c": decoded["temperature_c"],
                "humidity_percent": decoded["humidity_percent"],
                "battery_percent": battery,
                "pressure_hpa": decoded["pressure_hpa"],
                "co2_ppm": decoded["co2_ppm"],
                "lux": decoded["lux"],
                "etvoc": decoded["etvoc"],
                "gas_subtype": decoded["gas_subtype"],
                "gas_value": decoded["gas_value"],
                "notes": ";".join(decoded["notes"]),
            }
            rows.append(row)
            current = state[key]
            current["timestamp"] = row["timestamp"]
            current["label"] = row["label"]
            current["address"] = row["address"]
            current["name"] = row["name"]
            current["rssi"] = row["rssi"]
            current["payload_hex"] = row["payload_hex"]
            if decoded["temperature_c"] is not None:
                current["temperature_c"] = decoded["temperature_c"]
            if decoded["humidity_percent"] is not None and decoded["humidity_percent"] <= 100:
                current["humidity_percent"] = decoded["humidity_percent"]
            if battery is not None:
                current["battery_percent"] = battery
            for field in ("pressure_hpa", "co2_ppm", "lux", "etvoc", "gas_subtype", "gas_value"):
                if decoded[field] is not None:
                    current[field] = decoded[field]
            if decoded["temperature_c"] is not None or decoded["humidity_percent"] is not None:
                latest[key] = row
            elif key not in latest:
                latest[key] = row
            if len(samples_by_key[key]) < 6:
                samples_by_key[key].append(row)

    fieldnames = [
        "timestamp",
        "key",
        "label",
        "address",
        "name",
        "rssi",
        "payload_hex",
        "payload_kind",
        "temperature_c",
        "humidity_percent",
        "battery_percent",
        "pressure_hpa",
        "co2_ppm",
        "lux",
        "etvoc",
        "gas_subtype",
        "gas_value",
        "notes",
    ]
    with open(args.csv, "w", encoding="utf-8", newline="") as dest:
        writer = csv.DictWriter(dest, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(rows)

    print(f"Wrote {len(rows)} decoded rows to {args.csv}")
    print()
    print("Latest combined sensor values:")
    for key in sorted(state):
        row = state[key]
        print(
            f"- {key:6} {row['label']:10} temp={row['temperature_c']} C "
            f"humidity={row['humidity_percent']}% battery={row['battery_percent']}% "
            f"pressure={row['pressure_hpa']} hPa co2={row['co2_ppm']} ppm "
            f"lux={row['lux']} etvoc={row['etvoc']} "
            f"rssi={row['rssi']} payload={row['payload_hex']}"
        )

    print()
    print("Early samples:")
    for key in sorted(samples_by_key):
        print(f"[{key}]")
        for row in samples_by_key[key]:
            print(
                f"  {row['payload_kind']:16} temp={row['temperature_c']} "
                f"humidity={row['humidity_percent']} battery={row['battery_percent']} "
                f"{row['payload_hex']}"
            )


if __name__ == "__main__":
    main()
