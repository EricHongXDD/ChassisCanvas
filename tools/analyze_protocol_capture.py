from __future__ import annotations

import argparse
import hashlib
import json
import re
import struct
from collections import Counter
from dataclasses import dataclass
from pathlib import Path


HEX_BYTE = re.compile(r"^[0-9a-fA-F]{2}$")


@dataclass(frozen=True)
class Record:
    api: str
    handle: str
    requested_length: int
    captured_at: str
    data: bytes


def decode_hexdump(value: str) -> bytes:
    output = bytearray()
    for line in value.splitlines():
        columns = line.split()
        if not columns:
            continue
        # Frida hexdump 每行第一列是地址，后面最多 16 个十六进制字节。
        for token in columns[1:17]:
            if not HEX_BYTE.fullmatch(token):
                break
            output.append(int(token, 16))
    return bytes(output)


def printable(data: bytes) -> str:
    return "".join(chr(value) if 32 <= value <= 126 else "." for value in data)


def read_records(path: Path, marker: bytes) -> list[Record]:
    records: list[Record] = []
    with path.open("r", encoding="utf-8") as stream:
        for line in stream:
            item = json.loads(line)
            data = decode_hexdump(item.get("data", ""))
            if marker not in data:
                continue
            records.append(
                Record(
                    api=item.get("api", "?"),
                    handle=item.get("handle", "?"),
                    requested_length=int(item.get("length", 0)),
                    captured_at=item.get("captured_at", "?"),
                    data=data,
                )
            )
    return records


def decode_qstring_map(data: bytes) -> dict[str, str]:
    if len(data) < 4:
        raise ValueError("QMap 数据太短")
    count = struct.unpack_from(">I", data, 0)[0]
    cursor = 4
    values: dict[str, str] = {}
    for _ in range(count):
        fields: list[str] = []
        for _field in range(2):
            if cursor + 4 > len(data):
                raise ValueError("QString 长度不完整")
            length = struct.unpack_from(">I", data, cursor)[0]
            cursor += 4
            if length % 2 or cursor + length > len(data):
                raise ValueError("QString 长度无效")
            fields.append(data[cursor : cursor + length].decode("utf-16be"))
            cursor += length
        values[fields[0]] = fields[1]
    if cursor != len(data):
        raise ValueError("QMap 尾部存在多余数据")
    return values


def main() -> int:
    parser = argparse.ArgumentParser(description="提取抓包中含指定标记的真实报文")
    parser.add_argument("capture", type=Path)
    parser.add_argument("--marker", default="AA55")
    parser.add_argument("--context", type=int, default=192)
    parser.add_argument("--limit", type=int, default=100)
    parser.add_argument("--show-command-maps", action="store_true")
    args = parser.parse_args()

    marker = args.marker.encode("ascii")
    records = read_records(args.capture, marker)
    print(f"文件: {args.capture.resolve()}")
    print(f"命中记录: {len(records)}")

    handles = Counter((record.api, record.handle) for record in records)
    for (api, handle), count in handles.most_common():
        print(f"  {count:5d}  {api:18s} handle={handle}")

    unique: dict[str, tuple[Record, int]] = {}
    for record in records:
        digest = hashlib.sha256(record.data).hexdigest()
        if digest in unique:
            previous, count = unique[digest]
            unique[digest] = (previous, count + 1)
        else:
            unique[digest] = (record, 1)

    print(f"唯一报文: {len(unique)}")
    ordered = sorted(unique.values(), key=lambda item: item[0].captured_at)
    if args.show_command_maps:
        print("\n可解码命令映射:")
        shown_commands: set[int] = set()
        for record, _count in ordered:
            if len(record.data) < 38 or not record.data.startswith(b"AA551234 FIXEDCMDHEAD "):
                continue
            command = struct.unpack_from("<I", record.data, 26)[0]
            payload_length = struct.unpack_from("<I", record.data, 30)[0]
            if command in shown_commands or payload_length == 0 or 38 + payload_length > len(record.data):
                continue
            try:
                values = decode_qstring_map(record.data[38 : 38 + payload_length])
            except (UnicodeDecodeError, ValueError):
                continue
            shown_commands.add(command)
            print(f"  command={command}: {json.dumps(values, ensure_ascii=False, sort_keys=True)}")

    for index, (record, count) in enumerate(ordered[: max(1, args.limit)], 1):
        marker_offset = record.data.find(marker)
        start = max(0, marker_offset - 16)
        end = min(len(record.data), marker_offset + max(len(marker), args.context))
        preview = record.data[start:end]
        print()
        print(
            f"[{index}] count={count} time={record.captured_at} api={record.api} "
            f"handle={record.handle} requested={record.requested_length} captured={len(record.data)} "
            f"marker_offset={marker_offset}"
        )
        print(preview.hex(" "))
        print(printable(preview))

    if len(ordered) > args.limit:
        print(f"\n其余 {len(ordered) - args.limit} 个唯一报文未输出")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
