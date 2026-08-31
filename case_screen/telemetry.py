from __future__ import annotations

from datetime import datetime

import psutil

from .models import TelemetrySnapshot


class TelemetryCollector:
    def __init__(self) -> None:
        psutil.cpu_percent(interval=None)

    def capture(self) -> TelemetrySnapshot:
        memory = psutil.virtual_memory()
        gib = 1024**3
        return TelemetrySnapshot(
            captured_at=datetime.now(),
            cpu_percent=psutil.cpu_percent(interval=None),
            memory_percent=memory.percent,
            memory_used_gib=(memory.total - memory.available) / gib,
            memory_total_gib=memory.total / gib,
        )

