from __future__ import annotations

from typing import Mapping


def clamp(value: float, low: float, high: float) -> float:
    return max(low, min(high, value))


def move_bounds(bounds: Mapping[str, float], dx: float, dy: float) -> dict[str, float]:
    """按归一化坐标移动图层，位移不会受画布像素尺寸或缩放影响。"""
    width = float(bounds.get("width", 0.2))
    height = float(bounds.get("height", 0.2))
    return {
        "x": clamp(float(bounds.get("x", 0)) + dx, 0, 1 - width),
        "y": clamp(float(bounds.get("y", 0)) + dy, 0, 1 - height),
        "width": width,
        "height": height,
    }


def resize_bounds(bounds: Mapping[str, float], dx: float, dy: float) -> dict[str, float]:
    """从右下角调整图层大小，保持左上角不动并夹紧到画布。"""
    x = float(bounds.get("x", 0))
    y = float(bounds.get("y", 0))
    return {
        "x": x,
        "y": y,
        "width": clamp(float(bounds.get("width", 0.2)) + dx, 0.03, 1 - x),
        "height": clamp(float(bounds.get("height", 0.2)) + dy, 0.03, 1 - y),
    }
