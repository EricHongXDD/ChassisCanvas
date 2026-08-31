#!/usr/bin/env python3
"""把微雪原始 GPT 镜像扩展到指定容量，并扩展最后的 UDISK 分区。

脚本分为 prepare/finalize 两步。中间的 ext4 文件系统扩容交给官方
e2fsprogs 完成，避免在 Python 中自行改写复杂的 ext4 元数据。
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import struct
import sys
import zlib
from pathlib import Path


SECTOR_SIZE = 512
COPY_CHUNK = 4 * 1024 * 1024
DEFAULT_8GB_SECTORS = 15_523_840


def crc32(data: bytes | bytearray) -> int:
    return zlib.crc32(data) & 0xFFFFFFFF


def read_exact(handle, offset: int, length: int) -> bytes:
    handle.seek(offset)
    data = handle.read(length)
    if len(data) != length:
        raise ValueError(f"镜像数据不足：offset={offset}, 需要={length}, 实际={len(data)}")
    return data


def parse_header(sector: bytes, expected_lba: int) -> dict[str, int]:
    if len(sector) != SECTOR_SIZE or sector[:8] != b"EFI PART":
        raise ValueError(f"LBA {expected_lba} 不是有效 GPT 头")
    header_size = struct.unpack_from("<I", sector, 12)[0]
    if not 92 <= header_size <= SECTOR_SIZE:
        raise ValueError(f"GPT 头长度异常：{header_size}")
    stored_crc = struct.unpack_from("<I", sector, 16)[0]
    crc_buffer = bytearray(sector[:header_size])
    struct.pack_into("<I", crc_buffer, 16, 0)
    calculated_crc = crc32(crc_buffer)
    if stored_crc != calculated_crc:
        raise ValueError(f"GPT 头 CRC 错误：记录={stored_crc:08x}，计算={calculated_crc:08x}")
    values = struct.unpack_from("<QQQQ16sQIII", sector, 24)
    current_lba, backup_lba, first_usable, last_usable = values[:4]
    if current_lba != expected_lba:
        raise ValueError(f"GPT 当前 LBA 异常：记录={current_lba}，实际={expected_lba}")
    return {
        "header_size": header_size,
        "header_crc": stored_crc,
        "current_lba": current_lba,
        "backup_lba": backup_lba,
        "first_usable_lba": first_usable,
        "last_usable_lba": last_usable,
        "entries_lba": values[5],
        "entry_count": values[6],
        "entry_size": values[7],
        "entries_crc": values[8],
    }


def parse_layout(path: Path) -> dict[str, object]:
    image_bytes = path.stat().st_size
    if image_bytes % SECTOR_SIZE:
        raise ValueError("镜像长度不是 512 字节的整数倍")
    image_sectors = image_bytes // SECTOR_SIZE
    with path.open("rb") as handle:
        protective_mbr = read_exact(handle, 0, SECTOR_SIZE)
        if protective_mbr[510:512] != b"\x55\xaa" or protective_mbr[450] != 0xEE:
            raise ValueError("镜像没有有效的 GPT 保护性 MBR")

        primary_sector = read_exact(handle, SECTOR_SIZE, SECTOR_SIZE)
        primary = parse_header(primary_sector, 1)
        if primary["backup_lba"] != image_sectors - 1:
            raise ValueError("GPT 备份头不在镜像最后一个扇区")
        entries_length = primary["entry_count"] * primary["entry_size"]
        entries = read_exact(handle, primary["entries_lba"] * SECTOR_SIZE, entries_length)
        if crc32(entries) != primary["entries_crc"]:
            raise ValueError("GPT 主分区项数组 CRC 错误")

        backup_sector = read_exact(handle, primary["backup_lba"] * SECTOR_SIZE, SECTOR_SIZE)
        backup = parse_header(backup_sector, primary["backup_lba"])
        backup_entries = read_exact(handle, backup["entries_lba"] * SECTOR_SIZE, entries_length)
        if crc32(backup_entries) != backup["entries_crc"] or backup_entries != entries:
            raise ValueError("GPT 备份分区项数组与主表不一致")

    partitions: list[dict[str, object]] = []
    udisk: dict[str, object] | None = None
    for index in range(primary["entry_count"]):
        offset = index * primary["entry_size"]
        entry = entries[offset : offset + primary["entry_size"]]
        if entry[:16] == b"\0" * 16:
            continue
        first_lba, last_lba = struct.unpack_from("<QQ", entry, 32)
        name = entry[56:128].decode("utf-16le", "replace").rstrip("\0")
        item: dict[str, object] = {
            "index": index + 1,
            "entry_offset": offset,
            "first_lba": first_lba,
            "last_lba": last_lba,
            "sectors": last_lba - first_lba + 1,
            "name": name,
        }
        partitions.append(item)
        if name == "UDISK":
            udisk = item
    if udisk is None:
        raise ValueError("找不到名为 UDISK 的分区")
    if udisk is not partitions[-1]:
        raise ValueError("UDISK 不是最后一个有效分区，拒绝自动扩容")

    return {
        "image_bytes": image_bytes,
        "image_sectors": image_sectors,
        "protective_mbr": protective_mbr,
        "primary_sector": primary_sector,
        "primary": primary,
        "backup_sector": backup_sector,
        "backup": backup,
        "entries": entries,
        "partitions": partitions,
        "udisk": udisk,
    }


def copy_n(source, destination, length: int) -> None:
    remaining = length
    while remaining:
        data = source.read(min(COPY_CHUNK, remaining))
        if not data:
            raise ValueError("复制过程中源文件提前结束")
        destination.write(data)
        remaining -= len(data)


def prepare(source: Path, output: Path, work_partition: Path, manifest: Path, target_sectors: int) -> None:
    layout = parse_layout(source)
    old_sectors = int(layout["image_sectors"])
    if target_sectors <= old_sectors:
        raise ValueError("目标容量必须大于原镜像容量")

    primary = layout["primary"]
    udisk = layout["udisk"]
    trailing_gap = int(primary["last_usable_lba"]) - int(udisk["last_lba"])
    last_usable_to_end = old_sectors - 1 - int(primary["last_usable_lba"])
    new_last_usable = target_sectors - 1 - last_usable_to_end
    new_udisk_last = new_last_usable - trailing_gap
    new_udisk_sectors = new_udisk_last - int(udisk["first_lba"]) + 1
    if new_udisk_sectors <= int(udisk["sectors"]):
        raise ValueError("计算得到的 UDISK 新容量没有增加")

    output.parent.mkdir(parents=True, exist_ok=True)
    work_partition.parent.mkdir(parents=True, exist_ok=True)
    manifest.parent.mkdir(parents=True, exist_ok=True)

    with source.open("rb") as source_handle, output.open("wb") as output_handle:
        shutil.copyfileobj(source_handle, output_handle, COPY_CHUNK)
        output_handle.truncate(target_sectors * SECTOR_SIZE)
        # 原备份 GPT 现在位于扩展后的 UDISK 内部，先清零，防止留下伪分区表签名。
        old_udisk_end = (int(udisk["last_lba"]) + 1) * SECTOR_SIZE
        output_handle.seek(old_udisk_end)
        output_handle.write(b"\0" * (int(layout["image_bytes"]) - old_udisk_end))

    old_udisk_bytes = int(udisk["sectors"]) * SECTOR_SIZE
    with source.open("rb") as source_handle, work_partition.open("wb") as partition_handle:
        source_handle.seek(int(udisk["first_lba"]) * SECTOR_SIZE)
        copy_n(source_handle, partition_handle, old_udisk_bytes)
        partition_handle.truncate(new_udisk_sectors * SECTOR_SIZE)

    manifest_data = {
        "source": str(source.resolve()),
        "output": str(output.resolve()),
        "work_partition": str(work_partition.resolve()),
        "target_sectors": target_sectors,
        "target_bytes": target_sectors * SECTOR_SIZE,
        "old_image_sectors": old_sectors,
        "old_udisk_first_lba": int(udisk["first_lba"]),
        "old_udisk_last_lba": int(udisk["last_lba"]),
        "old_udisk_sectors": int(udisk["sectors"]),
        "new_udisk_last_lba": new_udisk_last,
        "new_udisk_sectors": new_udisk_sectors,
        "new_last_usable_lba": new_last_usable,
        "trailing_gap_sectors": trailing_gap,
        "last_usable_to_end_sectors": last_usable_to_end,
    }
    manifest.write_text(json.dumps(manifest_data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(manifest_data, ensure_ascii=False, indent=2))


def update_header(
    original_sector: bytes,
    *,
    current_lba: int,
    backup_lba: int,
    last_usable_lba: int,
    entries_lba: int,
    entries_crc: int,
) -> bytes:
    sector = bytearray(original_sector)
    struct.pack_into("<Q", sector, 24, current_lba)
    struct.pack_into("<Q", sector, 32, backup_lba)
    struct.pack_into("<Q", sector, 48, last_usable_lba)
    struct.pack_into("<Q", sector, 72, entries_lba)
    struct.pack_into("<I", sector, 88, entries_crc)
    struct.pack_into("<I", sector, 16, 0)
    header_size = struct.unpack_from("<I", sector, 12)[0]
    struct.pack_into("<I", sector, 16, crc32(sector[:header_size]))
    return bytes(sector)


def finalize(manifest: Path) -> None:
    data = json.loads(manifest.read_text(encoding="utf-8"))
    source = Path(data["source"])
    output = Path(data["output"])
    work_partition = Path(data["work_partition"])
    layout = parse_layout(source)
    primary = layout["primary"]
    backup = layout["backup"]
    udisk = layout["udisk"]

    new_partition_bytes = int(data["new_udisk_sectors"]) * SECTOR_SIZE
    if work_partition.stat().st_size != new_partition_bytes:
        raise ValueError("中间 UDISK 文件长度与清单不一致")
    if output.stat().st_size != int(data["target_bytes"]):
        raise ValueError("输出镜像长度与清单不一致")

    old_partition_bytes = int(data["old_udisk_sectors"]) * SECTOR_SIZE
    partition_offset = int(data["old_udisk_first_lba"]) * SECTOR_SIZE
    changed_chunks = 0
    written_bytes = 0
    with source.open("rb") as source_handle, work_partition.open("rb") as partition_handle, output.open("r+b") as output_handle:
        source_handle.seek(partition_offset)
        position = 0
        while position < new_partition_bytes:
            length = min(COPY_CHUNK, new_partition_bytes - position)
            new_data = partition_handle.read(length)
            if len(new_data) != length:
                raise ValueError("读取中间 UDISK 文件时提前结束")
            should_write = False
            if position < old_partition_bytes:
                compare_length = min(length, old_partition_bytes - position)
                old_data = source_handle.read(compare_length)
                should_write = new_data[:compare_length] != old_data
                if compare_length < length and any(new_data[compare_length:]):
                    should_write = True
            elif any(new_data):
                should_write = True
            if should_write:
                output_handle.seek(partition_offset + position)
                output_handle.write(new_data)
                changed_chunks += 1
                written_bytes += length
            position += length

        entries = bytearray(layout["entries"])
        struct.pack_into("<Q", entries, int(udisk["entry_offset"]) + 40, int(data["new_udisk_last_lba"]))
        entries_crc = crc32(entries)
        target_sectors = int(data["target_sectors"])
        backup_entries_distance = int(layout["image_sectors"]) - int(backup["entries_lba"])
        new_backup_entries_lba = target_sectors - backup_entries_distance
        primary_sector = update_header(
            layout["primary_sector"],
            current_lba=1,
            backup_lba=target_sectors - 1,
            last_usable_lba=int(data["new_last_usable_lba"]),
            entries_lba=int(primary["entries_lba"]),
            entries_crc=entries_crc,
        )
        backup_sector = update_header(
            layout["backup_sector"],
            current_lba=target_sectors - 1,
            backup_lba=1,
            last_usable_lba=int(data["new_last_usable_lba"]),
            entries_lba=new_backup_entries_lba,
            entries_crc=entries_crc,
        )
        output_handle.seek(SECTOR_SIZE)
        output_handle.write(primary_sector)
        output_handle.seek(int(primary["entries_lba"]) * SECTOR_SIZE)
        output_handle.write(entries)
        output_handle.seek(new_backup_entries_lba * SECTOR_SIZE)
        output_handle.write(entries)
        output_handle.seek((target_sectors - 1) * SECTOR_SIZE)
        output_handle.write(backup_sector)
        output_handle.flush()
        os.fsync(output_handle.fileno())

    expanded = parse_layout(output)
    expanded_udisk = expanded["udisk"]
    result = {
        "output": str(output),
        "bytes": output.stat().st_size,
        "sectors": expanded["image_sectors"],
        "udisk_first_lba": expanded_udisk["first_lba"],
        "udisk_last_lba": expanded_udisk["last_lba"],
        "udisk_sectors": expanded_udisk["sectors"],
        "changed_chunks": changed_chunks,
        "written_bytes": written_bytes,
        "gpt_crc_valid": True,
    }
    print(json.dumps(result, ensure_ascii=False, indent=2))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    prepare_parser = subparsers.add_parser("prepare", help="创建扩容镜像骨架和 UDISK 中间文件")
    prepare_parser.add_argument("source", type=Path)
    prepare_parser.add_argument("output", type=Path)
    prepare_parser.add_argument("work_partition", type=Path)
    prepare_parser.add_argument("manifest", type=Path)
    prepare_parser.add_argument("--target-sectors", type=int, default=DEFAULT_8GB_SECTORS)
    finalize_parser = subparsers.add_parser("finalize", help="回填扩容后的 UDISK 并重建 GPT 主备表")
    finalize_parser.add_argument("manifest", type=Path)
    args = parser.parse_args()
    if args.command == "prepare":
        prepare(args.source, args.output, args.work_partition, args.manifest, args.target_sectors)
    else:
        finalize(args.manifest)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, json.JSONDecodeError) as error:
        print(f"错误：{error}", file=sys.stderr)
        raise SystemExit(1)
