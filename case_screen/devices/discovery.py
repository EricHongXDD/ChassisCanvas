from __future__ import annotations

import logging
from collections.abc import Iterable

from ..models import DeviceInfo, DeviceKind, ProtocolState

log = logging.getLogger(__name__)

WAVESHARE_USB_ID = (0x1D6B, 0x0104)
COOLER_MASTER_USB_ID = (0x2516, 0x0222)


def _discover_serial() -> Iterable[DeviceInfo]:
    try:
        from serial.tools import list_ports
    except ImportError:
        return []

    found: list[DeviceInfo] = []
    for port in list_ports.comports():
        if (port.vid, port.pid) != WAVESHARE_USB_ID:
            continue
        found.append(
            DeviceInfo(
                key=f"waveshare:{port.device}",
                kind=DeviceKind.WAVESHARE,
                name="微雪 7 英寸 USB Monitor",
                vid=port.vid or WAVESHARE_USB_ID[0],
                pid=port.pid or WAVESHARE_USB_ID[1],
                transport=f"CDC ACM / {port.device}",
                resolution=(1024, 600),
                protocol_state=ProtocolState.NEEDS_CAPTURE,
                path=port.device,
                metadata={"description": port.description, "hwid": port.hwid},
            )
        )
    return found


def _discover_cooler_master() -> Iterable[DeviceInfo]:
    try:
        import hid
    except ImportError:
        return []

    try:
        interfaces = hid.enumerate(*COOLER_MASTER_USB_ID)
    except Exception as exc:
        log.warning("枚举酷冷屏幕失败：%s", exc)
        return []
    if not interfaces:
        return []

    paths = [item.get("path") for item in interfaces]
    interface_summary = [
        {
            "interface_number": item.get("interface_number"),
            "usage_page": item.get("usage_page"),
            "usage": item.get("usage"),
            "product_string": item.get("product_string"),
        }
        for item in interfaces
    ]
    return [
        DeviceInfo(
            key="cooler-master:atmos-v2-lcd",
            kind=DeviceKind.COOLER_MASTER,
            name="酷冷至尊 ATMOS V2-LCD",
            vid=COOLER_MASTER_USB_ID[0],
            pid=COOLER_MASTER_USB_ID[1],
            transport=f"HID（{len(interfaces)} 个接口）",
            resolution=(480, 480),
            protocol_state=ProtocolState.NEEDS_CAPTURE,
            path=paths[0] if paths else None,
            metadata={"interfaces": interface_summary, "paths": paths},
        )
    ]


def discover_devices() -> list[DeviceInfo]:
    devices: list[DeviceInfo] = []
    devices.extend(_discover_serial())
    devices.extend(_discover_cooler_master())
    return devices
