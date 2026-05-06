#!/usr/bin/env python3
import argparse
import json
import signal
import socket
import struct
import time
from datetime import datetime, timezone


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


SOL_HCI = 0
HCI_FILTER = 2
HCI_EVENT_PKT = 0x04
HCI_COMMAND_PKT = 0x01
EVT_CMD_COMPLETE = 0x0E
EVT_LE_META_EVENT = 0x3E
OGF_LE_CTL = 0x08
OCF_LE_SET_SCAN_PARAMETERS = 0x000B
OCF_LE_SET_SCAN_ENABLE = 0x000C


def hci_opcode(ogf, ocf):
    return (ogf << 10) | ocf


def mac_from_le(raw):
    return ":".join(f"{byte:02x}" for byte in raw[::-1])


def signed_byte(value):
    return value - 256 if value > 127 else value


def ad_structures(raw):
    pos = 0
    result = []
    while pos < len(raw):
        length = raw[pos]
        pos += 1
        if length == 0:
            break
        if pos + length > len(raw):
            result.append({"type": "malformed", "data": raw[pos - 1 :].hex()})
            break
        ad_type = raw[pos]
        ad_data = raw[pos + 1 : pos + length]
        result.append({"type": f"0x{ad_type:02x}", "data": ad_data.hex()})
        pos += length
    return result


def send_hci_command(sock, opcode, params=b""):
    packet = struct.pack("<BHB", HCI_COMMAND_PKT, opcode, len(params)) + params
    sock.send(packet)


def set_event_filter(sock):
    def event_bit(event_code):
        if event_code < 32:
            return 1 << event_code, 0
        return 0, 1 << (event_code - 32)

    type_mask = 1 << HCI_EVENT_PKT
    cmd_complete1, cmd_complete2 = event_bit(EVT_CMD_COMPLETE)
    le_meta1, le_meta2 = event_bit(EVT_LE_META_EVENT)
    event_mask1 = cmd_complete1 | le_meta1
    event_mask2 = cmd_complete2 | le_meta2
    opcode = 0
    flt = struct.pack("<LLLH", type_mask, event_mask1, event_mask2, opcode)
    sock.setsockopt(SOL_HCI, HCI_FILTER, flt)


def enable_scan(sock):
    scan_type = 0x01
    interval = 0x0010
    window = 0x0010
    own_address_type = 0x00
    filter_policy = 0x00
    params = struct.pack("<BHHBB", scan_type, interval, window, own_address_type, filter_policy)
    send_hci_command(sock, hci_opcode(OGF_LE_CTL, OCF_LE_SET_SCAN_PARAMETERS), params)
    time.sleep(0.1)
    send_hci_command(sock, hci_opcode(OGF_LE_CTL, OCF_LE_SET_SCAN_ENABLE), b"\x01\x00")


def disable_scan(sock):
    send_hci_command(sock, hci_opcode(OGF_LE_CTL, OCF_LE_SET_SCAN_ENABLE), b"\x00\x00")


def parse_le_advertising_reports(payload):
    if not payload or payload[0] != 0x02:
        return []
    count = payload[1]
    pos = 2
    reports = []
    for _ in range(count):
        if pos + 10 > len(payload):
            break
        event_type = payload[pos]
        address_type = payload[pos + 1]
        address = mac_from_le(payload[pos + 2 : pos + 8])
        data_len = payload[pos + 8]
        pos += 9
        if pos + data_len + 1 > len(payload):
            break
        data = payload[pos : pos + data_len]
        pos += data_len
        rssi = signed_byte(payload[pos])
        pos += 1
        reports.append(
            {
                "address": address,
                "address_type": address_type,
                "event_type": event_type,
                "rssi": rssi,
                "data_hex": data.hex(),
                "ad": ad_structures(data),
            }
        )
    return reports


def main():
    parser = argparse.ArgumentParser(description="Scan BLE advertisements via raw Linux HCI.")
    parser.add_argument("--hci", type=int, default=0, help="HCI device index, default: 0")
    parser.add_argument("--seconds", type=int, default=60, help="Scan duration")
    parser.add_argument("--all", action="store_true", help="Print all advertisements, not only targets")
    parser.add_argument("--targets", default="/etc/home-metrics/sensors.json", help="Sensor definition JSON path")
    parser.add_argument("--jsonl", default="home-metrics-ble-scan.jsonl", help="Output JSON Lines path")
    args = parser.parse_args()

    targets = load_targets(args.targets)
    targets_by_mac = {entry["mac"].lower(): {"key": key, **entry} for key, entry in targets.items()}
    stop = False

    def handle_stop(_signum, _frame):
        nonlocal stop
        stop = True

    signal.signal(signal.SIGINT, handle_stop)
    signal.signal(signal.SIGTERM, handle_stop)

    seen_targets = set()
    counts = {}

    sock = socket.socket(socket.AF_BLUETOOTH, socket.SOCK_RAW, socket.BTPROTO_HCI)
    sock.bind((args.hci,))
    sock.settimeout(1.0)
    try:
        set_event_filter(sock)
    except OSError:
        pass

    started = time.time()
    try:
        enable_scan(sock)
        with open(args.jsonl, "a", encoding="utf-8") as output:
            while not stop and time.time() - started < args.seconds:
                try:
                    packet = sock.recv(4096)
                except TimeoutError:
                    continue
                if len(packet) < 4 or packet[0] != HCI_EVENT_PKT or packet[1] != EVT_LE_META_EVENT:
                    continue
                for report in parse_le_advertising_reports(packet[3:]):
                    mac = report["address"].lower()
                    target = targets_by_mac.get(mac)
                    counts[mac] = counts.get(mac, 0) + 1
                    if not target and not args.all:
                        continue
                    record = {
                        "timestamp": datetime.now(timezone.utc).isoformat(),
                        "target": target,
                        **report,
                    }
                    output.write(json.dumps(record, ensure_ascii=False) + "\n")
                    output.flush()
                    name = target["label"] if target else "unknown"
                    print(
                        f"{record['timestamp']} {name} {record['address']} "
                        f"rssi={record['rssi']} data={record['data_hex']}",
                        flush=True,
                    )
                    if target:
                        seen_targets.add(mac)
    finally:
        try:
            disable_scan(sock)
        finally:
            sock.close()

    print("\nTarget summary:")
    for key, entry in targets.items():
        mac = entry["mac"].lower()
        status = "FOUND" if mac in seen_targets else "not seen"
        print(f"- {key} ({entry['label']}) {entry['mac']}: {status}, packets={counts.get(mac, 0)}")


if __name__ == "__main__":
    main()
