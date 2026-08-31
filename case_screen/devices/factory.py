from __future__ import annotations

from .base import DisplayAdapter
from .cooler_master import CoolerMasterAdapter
from .waveshare import WaveshareAdapter
from ..models import DeviceInfo, DeviceKind


def create_adapter(info: DeviceInfo) -> DisplayAdapter:
    if info.kind is DeviceKind.WAVESHARE:
        return WaveshareAdapter(info)
    if info.kind is DeviceKind.COOLER_MASTER:
        return CoolerMasterAdapter(info)
    raise ValueError(f"不支持的设备类型：{info.kind}")
