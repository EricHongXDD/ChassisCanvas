from __future__ import annotations

import argparse
import json
from datetime import datetime
from pathlib import Path

from .devices.discovery import discover_devices
from .models import TelemetrySnapshot
from .rendering import render_dashboard


def _json_safe(value):
    if isinstance(value, bytes):
        return value.hex()
    if isinstance(value, dict):
        return {key: _json_safe(item) for key, item in value.items()}
    if isinstance(value, list):
        return [_json_safe(item) for item in value]
    return value


def scan() -> int:
    payload = []
    for info in discover_devices():
        payload.append(
            {
                "key": info.key,
                "name": info.name,
                "kind": info.kind.value,
                "usb_id": info.usb_id,
                "transport": info.transport,
                "resolution": info.resolution,
                "protocol_state": info.protocol_state.value,
                "metadata": _json_safe(info.metadata),
            }
        )
    print(json.dumps(payload, ensure_ascii=False, indent=2))
    return 0


def render(path: Path) -> int:
    snapshot = TelemetrySnapshot(
        captured_at=datetime.now(),
        cpu_percent=42,
        memory_percent=57,
        memory_used_gib=27.4,
        memory_total_gib=48,
    )
    path.parent.mkdir(parents=True, exist_ok=True)
    render_dashboard(snapshot, (1024, 600)).save(path)
    print(path.resolve())
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="机箱副屏开发诊断工具")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("scan", help="扫描微雪与酷冷至尊 USB 屏幕")
    render_parser = sub.add_parser("render", help="渲染主题预览")
    render_parser.add_argument("path", type=Path)
    args = parser.parse_args()
    if args.command == "scan":
        return scan()
    return render(args.path)


if __name__ == "__main__":
    raise SystemExit(main())
