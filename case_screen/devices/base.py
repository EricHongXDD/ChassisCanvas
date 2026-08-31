from __future__ import annotations

from abc import ABC, abstractmethod

from PIL import Image

from ..models import DeviceInfo


class DeviceError(RuntimeError):
    """设备通信失败。"""


class UnsafeProtocolError(DeviceError):
    """协议尚未确认，拒绝向设备发送数据。"""


class DisplayAdapter(ABC):
    def __init__(self, info: DeviceInfo) -> None:
        self.info = info

    @abstractmethod
    def connect(self) -> None:
        raise NotImplementedError

    @abstractmethod
    def disconnect(self) -> None:
        raise NotImplementedError

    @abstractmethod
    def send_image(self, image: Image.Image) -> None:
        raise NotImplementedError

    @property
    @abstractmethod
    def connected(self) -> bool:
        raise NotImplementedError

