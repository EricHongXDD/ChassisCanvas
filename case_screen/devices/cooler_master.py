from __future__ import annotations

from PIL import Image

from .base import DisplayAdapter, UnsafeProtocolError
from ..models import DeviceInfo


class CoolerMasterAdapter(DisplayAdapter):
    """协议抓取完成前只提供 HID 设备发现，禁止猜测性写入。"""

    @property
    def connected(self) -> bool:
        return False

    def connect(self) -> None:
        raise UnsafeProtocolError("酷冷 ATMOS V2-LCD HID 协议尚未完成抓取")

    def disconnect(self) -> None:
        return None

    def send_image(self, image: Image.Image) -> None:
        raise UnsafeProtocolError("酷冷 ATMOS V2-LCD HID 协议尚未完成抓取")

