from __future__ import annotations

import argparse
import json
import struct
from pathlib import Path


def _read_u32(stream) -> int:
    data = stream.read(4)
    if len(data) != 4:
        raise ValueError("ASAR 头部不完整")
    return struct.unpack("<I", data)[0]


def _safe_output(root: Path, relative: Path) -> Path:
    output = (root / relative).resolve()
    try:
        output.relative_to(root)
    except ValueError as error:
        raise ValueError(f"ASAR 路径越界: {relative}") from error
    return output


def extract(source: Path, destination: Path) -> tuple[int, int]:
    source = source.resolve()
    destination.mkdir(parents=True, exist_ok=True)
    destination = destination.resolve()
    with source.open("rb") as stream:
        size_payload = _read_u32(stream)
        if size_payload != 4:
            raise ValueError(f"不支持的 ASAR size pickle: {size_payload}")
        header_size = _read_u32(stream)
        header_pickle = stream.read(header_size)
        if len(header_pickle) != header_size or header_size < 8:
            raise ValueError("ASAR JSON 头部不完整")
        json_payload_size, json_size = struct.unpack_from("<II", header_pickle)
        if json_payload_size + 4 > header_size or json_size > json_payload_size:
            raise ValueError("ASAR JSON 长度无效")
        header = json.loads(header_pickle[8 : 8 + json_size].decode("utf-8"))
        data_offset = 8 + header_size
        extracted = 0
        total_bytes = 0

        def walk(files: dict, parent: Path) -> None:
            nonlocal extracted, total_bytes
            for name, entry in files.items():
                relative = parent / name
                if "files" in entry:
                    _safe_output(destination, relative).mkdir(parents=True, exist_ok=True)
                    walk(entry["files"], relative)
                    continue
                if entry.get("unpacked"):
                    # Electron 把此类文件放在 app.asar.unpacked，当前安装包没有该目录。
                    continue
                if "link" in entry:
                    continue
                size = int(entry.get("size", 0))
                offset = int(entry.get("offset", 0))
                output = _safe_output(destination, relative)
                output.parent.mkdir(parents=True, exist_ok=True)
                stream.seek(data_offset + offset)
                remaining = size
                with output.open("wb") as target:
                    while remaining:
                        chunk = stream.read(min(1024 * 1024, remaining))
                        if not chunk:
                            raise ValueError(f"ASAR 文件数据不完整: {relative}")
                        target.write(chunk)
                        remaining -= len(chunk)
                extracted += 1
                total_bytes += size

        walk(header["files"], Path())
        return extracted, total_bytes


def main() -> None:
    parser = argparse.ArgumentParser(description="静态提取 Electron ASAR 文件")
    parser.add_argument("source", type=Path)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    count, total = extract(args.source, args.destination)
    print(f"已提取 {count} 个文件，共 {total} 字节")


if __name__ == "__main__":
    main()
