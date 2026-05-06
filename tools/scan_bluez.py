#!/usr/bin/env python3
import argparse
import json
import time
from datetime import datetime, timezone

import dbus


def load_targets(path):
    if not path:
        return {}
    try:
        with open(path, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except FileNotFoundError:
        return {}
    devices = payload.get("devices", [])
    targets = {}
    for index, device in enumerate(devices, 1):
        if device.get("enabled") is False:
            continue
        mac = str(device.get("mac", "")).strip().lower()
        if not mac:
            continue
        key = str(device.get("key") or device.get("label") or f"sensor-{index}").strip().lower().replace(" ", "-")
        targets[key] = {
            "mac": mac,
            "label": str(device.get("label") or mac),
        }
    return targets


def convert(value):
    if isinstance(value, dbus.String):
        return str(value)
    if isinstance(value, dbus.Boolean):
        return bool(value)
    if isinstance(value, (dbus.Int16, dbus.Int32, dbus.Int64, dbus.UInt16, dbus.UInt32, dbus.UInt64)):
        return int(value)
    if isinstance(value, dbus.Byte):
        return int(value)
    if isinstance(value, dbus.Double):
        return float(value)
    if isinstance(value, dbus.Array):
        values = [convert(item) for item in value]
        if all(isinstance(item, int) and 0 <= item <= 255 for item in values):
            return {"hex": bytes(values).hex(), "bytes": values}
        return values
    if isinstance(value, dbus.Dictionary):
        return {str(convert(key)): convert(item) for key, item in value.items()}
    if isinstance(value, dbus.ObjectPath):
        return str(value)
    return value


def device_records(bus, targets_by_mac):
    manager = dbus.Interface(bus.get_object("org.bluez", "/"), "org.freedesktop.DBus.ObjectManager")
    objects = manager.GetManagedObjects()
    for path, interfaces in objects.items():
        device = interfaces.get("org.bluez.Device1")
        if not device:
            continue
        address = str(device.get("Address", "")).lower()
        target = targets_by_mac.get(address)
        yield {
            "path": str(path),
            "address": address,
            "target": target,
            "name": convert(device.get("Name", "")),
            "alias": convert(device.get("Alias", "")),
            "rssi": convert(device.get("RSSI")),
            "tx_power": convert(device.get("TxPower")),
            "uuids": convert(device.get("UUIDs", [])),
            "manufacturer_data": convert(device.get("ManufacturerData", {})),
            "service_data": convert(device.get("ServiceData", {})),
            "services_resolved": convert(device.get("ServicesResolved", False)),
            "paired": convert(device.get("Paired", False)),
            "connected": convert(device.get("Connected", False)),
        }


def main():
    parser = argparse.ArgumentParser(description="Scan BLE devices through BlueZ D-Bus.")
    parser.add_argument("--adapter", default="/org/bluez/hci0")
    parser.add_argument("--seconds", type=int, default=60)
    parser.add_argument("--all", action="store_true", help="Print all discovered devices")
    parser.add_argument("--targets", default="/etc/home-metrics/sensors.json", help="Sensor definition JSON path")
    parser.add_argument("--jsonl", default="home-metrics-bluez-scan.jsonl")
    args = parser.parse_args()

    targets = load_targets(args.targets)
    targets_by_mac = {entry["mac"].lower(): {"key": key, **entry} for key, entry in targets.items()}
    bus = dbus.SystemBus()
    adapter_obj = bus.get_object("org.bluez", args.adapter)
    adapter = dbus.Interface(adapter_obj, "org.bluez.Adapter1")

    seen_records = {}
    seen_targets = set()
    started = time.time()

    try:
        adapter.SetDiscoveryFilter({"Transport": dbus.String("le")})
    except dbus.exceptions.DBusException as exc:
        print(f"warning: SetDiscoveryFilter failed: {exc}")

    adapter.StartDiscovery()
    print(f"Started LE discovery for {args.seconds}s on {args.adapter}")
    try:
        with open(args.jsonl, "a", encoding="utf-8") as output:
            while time.time() - started < args.seconds:
                for record in device_records(bus, targets_by_mac):
                    if not record["target"] and not args.all:
                        continue
                    key = record["address"]
                    previous = seen_records.get(key)
                    comparable = json.dumps(record, sort_keys=True, ensure_ascii=False)
                    if previous == comparable:
                        continue
                    seen_records[key] = comparable
                    if record["target"]:
                        seen_targets.add(key)
                    record["timestamp"] = datetime.now(timezone.utc).isoformat()
                    output.write(json.dumps(record, ensure_ascii=False) + "\n")
                    output.flush()
                    label = record["target"]["label"] if record["target"] else record["alias"] or "unknown"
                    print(
                        f"{record['timestamp']} {label} {record['address']} "
                        f"rssi={record['rssi']} name={record['name']!r}",
                        flush=True,
                    )
                time.sleep(1)
    finally:
        adapter.StopDiscovery()

    print("\nTarget summary:")
    for key, entry in targets.items():
        mac = entry["mac"].lower()
        status = "FOUND" if mac in seen_targets else "not seen"
        print(f"- {key} ({entry['label']}) {entry['mac']}: {status}")


if __name__ == "__main__":
    main()
