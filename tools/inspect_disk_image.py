#!/usr/bin/env python3
"""只读检查原始磁盘镜像的分区表和常见文件系统签名。"""

from __future__ import annotations

import json
import struct
import sys
import uuid
from pathlib import Path


SECTOR_SIZE = 512


def decode_partition_type(value: int) -> str:
    names = {
        0x00: "empty",
        0x05: "extended",
        0x06: "FAT16",
        0x07: "NTFS/exFAT",
        0x0B: "FAT32",
        0x0C: "FAT32 LBA",
        0x0E: "FAT16 LBA",
        0x0F: "extended LBA",
        0x82: "Linux swap",
        0x83: "Linux filesystem",
        0xEE: "GPT protective",
    }
    return names.get(value, f"unknown-0x{value:02x}")


def filesystem_at(handle, offset: int) -> dict[str, object]:
    handle.seek(offset)
    head = handle.read(4096)
    result: dict[str, object] = {"offset": offset, "kind": "unknown"}

    if len(head) >= 90 and head[3:11] == b"NTFS    ":
        result["kind"] = "ntfs"
    elif len(head) >= 90 and head[3:11] == b"EXFAT   ":
        result["kind"] = "exfat"
    elif len(head) >= 90 and head[82:90] == b"FAT32   ":
        result["kind"] = "fat32"
    elif len(head) >= 62 and head[54:62] == b"FAT16   ":
        result["kind"] = "fat16"

    handle.seek(offset + 1024)
    superblock = handle.read(1024)
    if len(superblock) >= 0x68 and superblock[0x38:0x3A] == b"\x53\xef":
        log_block_size = struct.unpack_from("<I", superblock, 0x18)[0]
        block_size = 1024 << log_block_size
        blocks_lo = struct.unpack_from("<I", superblock, 0x04)[0]
        feature_incompat = struct.unpack_from("<I", superblock, 0x60)[0]
        blocks_hi = struct.unpack_from("<I", superblock, 0x150)[0] if len(superblock) >= 0x154 else 0
        blocks = blocks_lo | (blocks_hi << 32 if feature_incompat & 0x80 else 0)
        volume_name = superblock[0x78:0x88].split(b"\0", 1)[0].decode("utf-8", "replace")
        result.update(
            {
                "kind": "ext2/3/4",
                "block_size": block_size,
                "blocks": blocks,
                "filesystem_bytes": blocks * block_size,
                "volume_name": volume_name,
                "feature_incompat": f"0x{feature_incompat:08x}",
            }
        )
    return result


def guid_from_gpt(raw: bytes) -> str:
    if raw == b"\0" * 16:
        return "00000000-0000-0000-0000-000000000000"
    return str(uuid.UUID(bytes_le=raw))


def parse_gpt(handle, image_sectors: int) -> dict[str, object] | None:
    handle.seek(SECTOR_SIZE)
    header = handle.read(SECTOR_SIZE)
    if len(header) < 92 or header[:8] != b"EFI PART":
        return None

    revision, header_size, header_crc = struct.unpack_from("<III", header, 8)
    current_lba, backup_lba, first_usable, last_usable = struct.unpack_from("<QQQQ", header, 24)
    disk_guid = guid_from_gpt(header[56:72])
    entries_lba, entry_count, entry_size, entries_crc = struct.unpack_from("<QIII", header, 72)
    gpt: dict[str, object] = {
        "revision": f"0x{revision:08x}",
        "header_size": header_size,
        "header_crc32": f"0x{header_crc:08x}",
        "current_lba": current_lba,
        "backup_lba": backup_lba,
        "backup_lba_inside_image": backup_lba < image_sectors,
        "first_usable_lba": first_usable,
        "last_usable_lba": last_usable,
        "disk_guid": disk_guid,
        "entries_lba": entries_lba,
        "entry_count": entry_count,
        "entry_size": entry_size,
        "entries_crc32": f"0x{entries_crc:08x}",
        "partitions": [],
    }
    if entry_size < 128 or entry_count > 4096:
        return gpt

    handle.seek(entries_lba * SECTOR_SIZE)
    entries = handle.read(entry_count * entry_size)
    partitions = []
    for index in range(entry_count):
        entry = entries[index * entry_size : (index + 1) * entry_size]
        if len(entry) < 128 or entry[:16] == b"\0" * 16:
            continue
        first_lba, last_lba, attributes = struct.unpack_from("<QQQ", entry, 32)
        name = entry[56:128].decode("utf-16le", "replace").rstrip("\0")
        partition = {
            "index": index + 1,
            "type_guid": guid_from_gpt(entry[:16]),
            "unique_guid": guid_from_gpt(entry[16:32]),
            "first_lba": first_lba,
            "last_lba": last_lba,
            "sectors": last_lba - first_lba + 1,
            "start_bytes": first_lba * SECTOR_SIZE,
            "size_bytes": (last_lba - first_lba + 1) * SECTOR_SIZE,
            "attributes": f"0x{attributes:016x}",
            "name": name,
        }
        if first_lba < image_sectors:
            partition["filesystem"] = filesystem_at(handle, first_lba * SECTOR_SIZE)
        else:
            partition["filesystem"] = {"kind": "outside-image"}
        partitions.append(partition)
    gpt["partitions"] = partitions
    return gpt


def inspect(path: Path) -> dict[str, object]:
    size = path.stat().st_size
    result: dict[str, object] = {
        "path": str(path),
        "bytes": size,
        "sectors": size // SECTOR_SIZE,
        "sector_remainder": size % SECTOR_SIZE,
        "partitions": [],
    }
    with path.open("rb") as handle:
        mbr = handle.read(SECTOR_SIZE)
        result["mbr_signature"] = mbr[510:512].hex() if len(mbr) == SECTOR_SIZE else ""
        partitions = []
        if len(mbr) == SECTOR_SIZE and mbr[510:512] == b"\x55\xaa":
            for index in range(4):
                entry = mbr[446 + index * 16 : 462 + index * 16]
                boot, part_type, start_lba, sectors = struct.unpack_from("<B3xB3xII", entry)
                if part_type == 0 or sectors == 0:
                    continue
                partition = {
                    "index": index + 1,
                    "bootable": boot == 0x80,
                    "type": f"0x{part_type:02x}",
                    "type_name": decode_partition_type(part_type),
                    "start_lba": start_lba,
                    "sectors": sectors,
                    "start_bytes": start_lba * SECTOR_SIZE,
                    "size_bytes": sectors * SECTOR_SIZE,
                    "end_lba_inclusive": start_lba + sectors - 1,
                }
                partition["filesystem"] = filesystem_at(handle, start_lba * SECTOR_SIZE)
                partitions.append(partition)
        result["partitions"] = partitions
        result["gpt"] = parse_gpt(handle, size // SECTOR_SIZE)
    return result


def main() -> int:
    if len(sys.argv) < 2:
        print("用法: inspect_disk_image.py <镜像> [...]", file=sys.stderr)
        return 2
    for raw_path in sys.argv[1:]:
        print(json.dumps(inspect(Path(raw_path)), ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
