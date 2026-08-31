#!/usr/bin/env python3
"""只读解析磁盘镜像中的 Flattened Device Tree（DTB）。"""

from __future__ import annotations

import argparse
import re
import struct
from dataclasses import dataclass, field
from pathlib import Path


FDT_MAGIC = 0xD00DFEED
FDT_BEGIN_NODE = 1
FDT_END_NODE = 2
FDT_PROP = 3
FDT_NOP = 4
FDT_END = 9


@dataclass
class Node:
    path: str
    properties: dict[str, bytes] = field(default_factory=dict)


def align4(value: int) -> int:
    return (value + 3) & ~3


def read_cstring(data: bytes, offset: int) -> tuple[str, int]:
    end = data.find(b"\0", offset)
    if end < 0:
        raise ValueError("DTB 中存在未终止字符串")
    return data[offset:end].decode("utf-8", "replace"), align4(end + 1)


def parse_dtb(blob: bytes) -> tuple[dict[str, int], list[Node]]:
    if len(blob) < 40:
        raise ValueError("DTB 头部不完整")
    values = struct.unpack_from(">10I", blob, 0)
    names = (
        "magic",
        "totalsize",
        "off_dt_struct",
        "off_dt_strings",
        "off_mem_rsvmap",
        "version",
        "last_comp_version",
        "boot_cpuid_phys",
        "size_dt_strings",
        "size_dt_struct",
    )
    header = dict(zip(names, values))
    if header["magic"] != FDT_MAGIC:
        raise ValueError("指定偏移不是 DTB")
    if header["totalsize"] > len(blob):
        raise ValueError("DTB 数据不完整")

    strings_start = header["off_dt_strings"]
    strings_end = strings_start + header["size_dt_strings"]
    strings = blob[strings_start:strings_end]
    cursor = header["off_dt_struct"]
    struct_end = cursor + header["size_dt_struct"]
    stack: list[str] = []
    nodes: list[Node] = []

    while cursor + 4 <= struct_end:
        token = struct.unpack_from(">I", blob, cursor)[0]
        cursor += 4
        if token == FDT_BEGIN_NODE:
            name, cursor = read_cstring(blob, cursor)
            stack.append(name)
            path = "/" + "/".join(item for item in stack if item)
            nodes.append(Node(path or "/"))
        elif token == FDT_END_NODE:
            if not stack:
                raise ValueError("DTB 节点栈下溢")
            stack.pop()
        elif token == FDT_PROP:
            if cursor + 8 > struct_end or not nodes:
                raise ValueError("DTB 属性头部不完整")
            length, name_offset = struct.unpack_from(">II", blob, cursor)
            cursor += 8
            if name_offset >= len(strings):
                raise ValueError("DTB 属性名偏移越界")
            name_end = strings.find(b"\0", name_offset)
            if name_end < 0:
                raise ValueError("DTB 属性名未终止")
            prop_name = strings[name_offset:name_end].decode("utf-8", "replace")
            value = blob[cursor : cursor + length]
            cursor = align4(cursor + length)
            nodes[-1].properties[prop_name] = value
        elif token == FDT_NOP:
            continue
        elif token == FDT_END:
            break
        else:
            raise ValueError(f"未知 DTB token: {token}")
    return header, nodes


def printable_string_list(value: bytes) -> list[str] | None:
    if not value or value[-1:] != b"\0":
        return None
    parts = value[:-1].split(b"\0")
    if not all(part and all(byte in (9, 10, 13) or 32 <= byte <= 126 for byte in part) for part in parts):
        return None
    return [part.decode("utf-8", "replace") for part in parts]


def format_value(value: bytes) -> str:
    if not value:
        return "<空>"
    strings = printable_string_list(value)
    if strings is not None:
        return repr(strings[0]) if len(strings) == 1 else repr(strings)
    if len(value) % 4 == 0 and len(value) <= 64:
        cells = struct.unpack(f">{len(value) // 4}I", value)
        return "<" + " ".join(f"0x{cell:x}" for cell in cells) + ">"
    return value.hex()


def find_offsets(path: Path) -> list[int]:
    pattern = struct.pack(">I", FDT_MAGIC)
    offsets: list[int] = []
    with path.open("rb") as stream:
        absolute = 0
        tail = b""
        while True:
            chunk = stream.read(4 * 1024 * 1024)
            if not chunk:
                break
            data = tail + chunk
            base = absolute - len(tail)
            cursor = 0
            while True:
                found = data.find(pattern, cursor)
                if found < 0:
                    break
                offsets.append(base + found)
                cursor = found + 1
            tail = data[-3:]
            absolute += len(chunk)
    return offsets


def load_at(path: Path, offset: int) -> bytes:
    with path.open("rb") as stream:
        stream.seek(offset)
        header = stream.read(40)
        if len(header) < 40 or struct.unpack_from(">I", header, 0)[0] != FDT_MAGIC:
            raise ValueError(f"偏移 {offset} 处不存在完整 DTB 头部")
        total_size = struct.unpack_from(">I", header, 4)[0]
        if total_size < 40 or total_size > 64 * 1024 * 1024:
            raise ValueError(f"偏移 {offset} 处的 DTB 大小异常: {total_size}")
        stream.seek(offset)
        return stream.read(total_size)


def main() -> int:
    parser = argparse.ArgumentParser(description="只读解析镜像中的 DTB")
    parser.add_argument("image", type=Path)
    parser.add_argument("--scan", action="store_true", help="扫描并列出看起来有效的 DTB 偏移")
    parser.add_argument("--offset", type=int, help="要解析的 DTB 字节偏移")
    parser.add_argument("--match", help="仅输出路径、属性名或属性值匹配该正则的节点")
    parser.add_argument("--all-properties", action="store_true", help="匹配节点后输出它的所有属性")
    args = parser.parse_args()

    image = args.image.resolve()
    if args.scan:
        for offset in find_offsets(image):
            try:
                blob = load_at(image, offset)
                header, nodes = parse_dtb(blob)
            except ValueError:
                continue
            root = next((node for node in nodes if node.path == "/"), None)
            model = format_value(root.properties.get("model", b"")) if root else ""
            print(f"offset={offset} size={header['totalsize']} version={header['version']} model={model}")
        if args.offset is None:
            return 0

    if args.offset is None:
        parser.error("需要 --offset 或 --scan")
    blob = load_at(image, args.offset)
    header, nodes = parse_dtb(blob)
    print(
        f"offset={args.offset} size={header['totalsize']} "
        f"version={header['version']} nodes={len(nodes)}"
    )
    pattern = re.compile(args.match, re.IGNORECASE) if args.match else None
    for node in nodes:
        formatted = {name: format_value(value) for name, value in node.properties.items()}
        matched_names = {
            name
            for name, value in formatted.items()
            if pattern and (pattern.search(name) or pattern.search(value))
        }
        if pattern and not pattern.search(node.path) and not matched_names:
            continue
        print(node.path)
        chosen = formatted if args.all_properties or not pattern else {
            name: value for name, value in formatted.items() if name in matched_names
        }
        for name, value in chosen.items():
            print(f"  {name} = {value}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
