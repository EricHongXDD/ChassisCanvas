from __future__ import annotations

import argparse
import ctypes
from pathlib import Path


class HIDDeviceInfo(ctypes.Structure):
    pass


HIDDeviceInfoPointer = ctypes.POINTER(HIDDeviceInfo)
HIDDeviceInfo._fields_ = [
    ("path", ctypes.c_char_p),
    ("vendor_id", ctypes.c_ushort),
    ("product_id", ctypes.c_ushort),
    ("serial_number", ctypes.c_wchar_p),
    ("release_number", ctypes.c_ushort),
    ("manufacturer_string", ctypes.c_wchar_p),
    ("product_string", ctypes.c_wchar_p),
    ("usage_page", ctypes.c_ushort),
    ("usage", ctypes.c_ushort),
    ("interface_number", ctypes.c_int),
    ("next", HIDDeviceInfoPointer),
]


def parse_hex(value: str) -> int:
    return int(value, 0)


def main() -> int:
    parser = argparse.ArgumentParser(description="使用随原厂程序附带的 hidapi 枚举 HID 接口")
    parser.add_argument("dll", type=Path)
    parser.add_argument("--vid", type=parse_hex, default=0)
    parser.add_argument("--pid", type=parse_hex, default=0)
    parser.add_argument("--probe", action="store_true", help="只读打开后立即关闭每个枚举到的接口")
    args = parser.parse_args()

    library = ctypes.WinDLL(str(args.dll.resolve()))
    library.hid_enumerate.argtypes = [ctypes.c_ushort, ctypes.c_ushort]
    library.hid_enumerate.restype = HIDDeviceInfoPointer
    library.hid_free_enumeration.argtypes = [HIDDeviceInfoPointer]
    library.hid_open_path.argtypes = [ctypes.c_char_p]
    library.hid_open_path.restype = ctypes.c_void_p
    library.hid_close.argtypes = [ctypes.c_void_p]
    head = library.hid_enumerate(args.vid, args.pid)
    current = head
    try:
        while current:
            item = current.contents
            print(
                f"{item.vendor_id:04X}:{item.product_id:04X} "
                f"interface={item.interface_number} usage={item.usage_page:04X}:{item.usage:04X} "
                f"product={item.product_string!r} serial={item.serial_number!r}"
            )
            print(f"  path={item.path.decode('utf-8', 'replace') if item.path else ''}")
            if args.probe and item.path:
                handle = library.hid_open_path(item.path)
                print(f"  open={'ok' if handle else 'failed'}")
                if handle:
                    library.hid_close(handle)
            current = item.next
    finally:
        if head:
            library.hid_free_enumeration(head)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
