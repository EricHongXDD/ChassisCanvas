from __future__ import annotations

from PIL import Image

from .base import DisplayAdapter, UnsafeProtocolError
from ..models import DeviceInfo


class WaveshareAdapter(DisplayAdapter):
    """协议抓取完成前只提供安全的设备占位实现。"""

    @property
    def connected(self) -> bool:
        return False

    def connect(self) -> None:
        raise UnsafeProtocolError("微雪 V2.22 串口协议尚未完成抓取")

    def disconnect(self) -> None:
        return None

    def send_image(self, image: Image.Image) -> None:
        raise UnsafeProtocolError("微雪 V2.22 串口协议尚未完成抓取")

