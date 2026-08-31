from __future__ import annotations

import unittest
from datetime import datetime

from case_screen.models import TelemetrySnapshot
from case_screen.rendering import render_dashboard


class RenderingTests(unittest.TestCase):
    def test_all_target_sizes_render(self) -> None:
        snapshot = TelemetrySnapshot(
            captured_at=datetime(2026, 8, 23, 12, 34, 56),
            cpu_percent=42,
            memory_percent=57,
            memory_used_gib=27.4,
            memory_total_gib=48,
        )
        for size in ((1024, 600), (1920, 468), (480, 480)):
            with self.subTest(size=size):
                self.assertEqual(render_dashboard(snapshot, size).size, size)


if __name__ == "__main__":
    unittest.main()

