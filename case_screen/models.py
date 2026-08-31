from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any


class DeviceKind(str, Enum):
    WAVESHARE = "waveshare"
    COOLER_MASTER = "cooler_master"


class ProtocolState(str, Enum):
    READY = "ready"
    NEEDS_CAPTURE = "needs_capture"
    UNAVAILABLE = "unavailable"


@dataclass(slots=True)
class DeviceInfo:
    key: str
    kind: DeviceKind
    name: str
    vid: int
    pid: int
    transport: str
    resolution: tuple[int, int]
    protocol_state: ProtocolState
    path: str | bytes | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def usb_id(self) -> str:
        return f"{self.vid:04X}:{self.pid:04X}"


@dataclass(slots=True)
class TelemetrySnapshot:
    captured_at: datetime
    cpu_percent: float
    memory_percent: float
    memory_used_gib: float
    memory_total_gib: float
    cpu_temperature: float | None = None
    gpu_percent: float | None = None
    gpu_temperature: float | None = None
