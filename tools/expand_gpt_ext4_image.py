from __future__ import annotations

import argparse
import hashlib
import os
import shutil
import struct
import subprocess
import tempfile
import zlib
from dataclasses import dataclass
from pathlib import Path


SECTOR_SIZE = 512
GPT_SIGNATURE = b"EFI PART"
GPT_HEADER_SIZE_MIN = 92


@dataclass(frozen=True)
class GptLayout:
    header_size: int
    first_usable_lba: int
    partition_entries_lba: int
    partition_entry_count: int
    partition_entry_size: int
    partition_entries: bytes
    disk_guid: bytes
    revision: int

    @property
    def partition_entries_sectors(self) -> int:
        return (len(self.partition_entries) + SECTOR_SIZE - 1) // SECTOR_SIZE


@dataclass(frozen=True)
class PartitionEntry:
    index: int
    first_lba: int
    last_lba: int
    name: str


def read_exact(stream, offset: int, size: int) -> bytes:
    stream.seek(offset)
    data = stream.read(size)
    if len(data) != size:
        raise ValueError(f"无法在偏移 {offset} 读取 {size} 字节")
    return data


def copy_range(source, destination, source_offset: int, size: int, chunk_size: int) -> None:
    source.seek(source_offset)
    remaining = size
    while remaining:
        chunk = source.read(min(chunk_size, remaining))
        if not chunk:
            raise ValueError("源文件在预期位置之前结束")
        destination.write(chunk)
        remaining -= len(chunk)


def parse_primary_gpt(stream) -> GptLayout:
    sector = read_exact(stream, SECTOR_SIZE, SECTOR_SIZE)
    if sector[:8] != GPT_SIGNATURE:
        raise ValueError("源镜像没有有效的主 GPT 头")

    revision, header_size = struct.unpack_from("<II", sector, 8)
    if not GPT_HEADER_SIZE_MIN <= header_size <= SECTOR_SIZE:
        raise ValueError(f"GPT 头大小异常: {header_size}")

    stored_header_crc = struct.unpack_from("<I", sector, 16)[0]
    header_for_crc = bytearray(sector[:header_size])
    struct.pack_into("<I", header_for_crc, 16, 0)
    actual_header_crc = zlib.crc32(header_for_crc) & 0xFFFFFFFF
    if actual_header_crc != stored_header_crc:
        raise ValueError(
            f"主 GPT 头 CRC 错误: 记录 {stored_header_crc:08x}, 计算 {actual_header_crc:08x}"
        )

    current_lba, backup_lba, first_usable_lba, _ = struct.unpack_from("<QQQQ", sector, 24)
    if current_lba != 1:
        raise ValueError(f"不支持的主 GPT 位置: LBA {current_lba}")

    partition_entries_lba = struct.unpack_from("<Q", sector, 72)[0]
    partition_entry_count, partition_entry_size, stored_entries_crc = struct.unpack_from(
        "<III", sector, 80
    )
    if partition_entry_size < 128 or partition_entry_size % 8:
        raise ValueError(f"GPT 分区项大小异常: {partition_entry_size}")

    entries_size = partition_entry_count * partition_entry_size
    entries = read_exact(stream, partition_entries_lba * SECTOR_SIZE, entries_size)
    actual_entries_crc = zlib.crc32(entries) & 0xFFFFFFFF
    if actual_entries_crc != stored_entries_crc:
        raise ValueError(
            f"GPT 分区项 CRC 错误: 记录 {stored_entries_crc:08x}, 计算 {actual_entries_crc:08x}"
        )

    source_sectors = stream.seek(0, 2) // SECTOR_SIZE
    if backup_lba != source_sectors - 1:
        raise ValueError(
            f"备份 GPT 位置与镜像尾部不一致: LBA {backup_lba}, 镜像末扇区 {source_sectors - 1}"
        )

    return GptLayout(
        header_size=header_size,
        first_usable_lba=first_usable_lba,
        partition_entries_lba=partition_entries_lba,
        partition_entry_count=partition_entry_count,
        partition_entry_size=partition_entry_size,
        partition_entries=entries,
        disk_guid=sector[56:72],
        revision=revision,
    )


def list_partitions(layout: GptLayout) -> list[PartitionEntry]:
    partitions: list[PartitionEntry] = []
    for index in range(layout.partition_entry_count):
        start = index * layout.partition_entry_size
        entry = layout.partition_entries[start : start + layout.partition_entry_size]
        if entry[:16] == b"\0" * 16:
            continue
        first_lba, last_lba = struct.unpack_from("<QQ", entry, 32)
        name = entry[56:128].decode("utf-16le", errors="replace").rstrip("\0")
        partitions.append(PartitionEntry(index + 1, first_lba, last_lba, name))
    return partitions


def make_gpt_header(
    layout: GptLayout,
    current_lba: int,
    backup_lba: int,
    last_usable_lba: int,
    entries_lba: int,
    entries_crc: int,
) -> bytes:
    sector = bytearray(SECTOR_SIZE)
    struct.pack_into(
        "<8sIIIIQQQQ16sQIII",
        sector,
        0,
        GPT_SIGNATURE,
        layout.revision,
        layout.header_size,
        0,
        0,
        current_lba,
        backup_lba,
        layout.first_usable_lba,
        last_usable_lba,
        layout.disk_guid,
        entries_lba,
        layout.partition_entry_count,
        layout.partition_entry_size,
        entries_crc,
    )
    header_crc = zlib.crc32(sector[: layout.header_size]) & 0xFFFFFFFF
    struct.pack_into("<I", sector, 16, header_crc)
    return bytes(sector)


def update_gpt(
    image: Path,
    layout: GptLayout,
    target_sectors: int,
    grow_partition_index: int,
) -> PartitionEntry:
    entries_sectors = layout.partition_entries_sectors
    backup_header_lba = target_sectors - 1
    backup_entries_lba = backup_header_lba - entries_sectors
    last_usable_lba = backup_entries_lba - 1
    if last_usable_lba <= layout.first_usable_lba:
        raise ValueError("目标镜像太小，无法容纳 GPT")

    entries = bytearray(layout.partition_entries)
    entry_offset = (grow_partition_index - 1) * layout.partition_entry_size
    entry = entries[entry_offset : entry_offset + layout.partition_entry_size]
    if len(entry) != layout.partition_entry_size or entry[:16] == b"\0" * 16:
        raise ValueError(f"找不到第 {grow_partition_index} 个 GPT 分区")
    first_lba = struct.unpack_from("<Q", entry, 32)[0]
    old_last_lba = struct.unpack_from("<Q", entry, 40)[0]
    if last_usable_lba <= old_last_lba:
        raise ValueError("目标镜像没有为分区提供更多空间")
    struct.pack_into("<Q", entries, entry_offset + 40, last_usable_lba)

    entries_crc = zlib.crc32(entries) & 0xFFFFFFFF
    primary_header = make_gpt_header(
        layout,
        current_lba=1,
        backup_lba=backup_header_lba,
        last_usable_lba=last_usable_lba,
        entries_lba=layout.partition_entries_lba,
        entries_crc=entries_crc,
    )
    backup_header = make_gpt_header(
        layout,
        current_lba=backup_header_lba,
        backup_lba=1,
        last_usable_lba=last_usable_lba,
        entries_lba=backup_entries_lba,
        entries_crc=entries_crc,
    )

    with image.open("r+b") as stream:
        mbr = bytearray(read_exact(stream, 0, SECTOR_SIZE))
        protective_offset = None
        for index in range(4):
            offset = 446 + index * 16
            if mbr[offset + 4] == 0xEE:
                protective_offset = offset
                break
        if protective_offset is None:
            raise ValueError("没有找到 GPT 保护性 MBR 分区项")
        struct.pack_into("<I", mbr, protective_offset + 12, min(target_sectors - 1, 0xFFFFFFFF))
        stream.seek(0)
        stream.write(mbr)

        stream.seek(SECTOR_SIZE)
        stream.write(primary_header)
        stream.seek(layout.partition_entries_lba * SECTOR_SIZE)
        stream.write(entries)

        stream.seek(backup_entries_lba * SECTOR_SIZE)
        stream.write(entries)
        padding = entries_sectors * SECTOR_SIZE - len(entries)
        if padding:
            stream.write(b"\0" * padding)
        stream.seek(backup_header_lba * SECTOR_SIZE)
        stream.write(backup_header)

    name = entry[56:128].decode("utf-16le", errors="replace").rstrip("\0")
    return PartitionEntry(grow_partition_index, first_lba, last_usable_lba, name)


def verify_gpt(image: Path, grow_partition_index: int) -> PartitionEntry:
    image_size = image.stat().st_size
    if image_size % SECTOR_SIZE:
        raise ValueError("目标镜像未按扇区对齐")
    target_sectors = image_size // SECTOR_SIZE

    with image.open("rb") as stream:
        layout = parse_primary_gpt(stream)
        partitions = list_partitions(layout)
        selected = next((item for item in partitions if item.index == grow_partition_index), None)
        if selected is None:
            raise ValueError(f"找不到第 {grow_partition_index} 个 GPT 分区")

        primary_header = read_exact(stream, SECTOR_SIZE, SECTOR_SIZE)
        backup_header_lba = struct.unpack_from("<Q", primary_header, 32)[0]
        backup_header = read_exact(stream, backup_header_lba * SECTOR_SIZE, SECTOR_SIZE)
        if backup_header[:8] != GPT_SIGNATURE:
            raise ValueError("备份 GPT 头签名错误")
        current_lba, backup_lba = struct.unpack_from("<QQ", backup_header, 24)
        if current_lba != target_sectors - 1 or backup_lba != 1:
            raise ValueError("备份 GPT 头位置字段错误")
        backup_header_size = struct.unpack_from("<I", backup_header, 12)[0]
        stored_crc = struct.unpack_from("<I", backup_header, 16)[0]
        header_for_crc = bytearray(backup_header[:backup_header_size])
        struct.pack_into("<I", header_for_crc, 16, 0)
        actual_crc = zlib.crc32(header_for_crc) & 0xFFFFFFFF
        if actual_crc != stored_crc:
            raise ValueError("备份 GPT 头 CRC 错误")

        backup_entries_lba = struct.unpack_from("<Q", backup_header, 72)[0]
        backup_entries = read_exact(
            stream,
            backup_entries_lba * SECTOR_SIZE,
            layout.partition_entry_count * layout.partition_entry_size,
        )
        stored_entries_crc = struct.unpack_from("<I", backup_header, 88)[0]
        if zlib.crc32(backup_entries) & 0xFFFFFFFF != stored_entries_crc:
            raise ValueError("备份 GPT 分区项 CRC 错误")
        if backup_entries != layout.partition_entries:
            raise ValueError("主、备 GPT 分区项不一致")
    return selected


def run_checked(command: list[str], allowed_returncodes: tuple[int, ...] = (0,)) -> None:
    print("  > " + " ".join(command))
    environment = os.environ.copy()
    executable = Path(command[0]).resolve()
    cygwin_bin = executable.parents[2] / "bin"
    if (cygwin_bin / "cygwin1.dll").is_file():
        environment["PATH"] = str(cygwin_bin) + os.pathsep + environment.get("PATH", "")
    completed = subprocess.run(
        command,
        text=True,
        encoding="utf-8",
        errors="replace",
        env=environment,
    )
    if completed.returncode not in allowed_returncodes:
        raise RuntimeError(f"命令失败，退出码 {completed.returncode}: {command[0]}")


def sha256_range(path: Path, offset: int, size: int, chunk_size: int) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        stream.seek(offset)
        remaining = size
        while remaining:
            chunk = stream.read(min(chunk_size, remaining))
            if not chunk:
                raise ValueError("计算 SHA-256 时文件提前结束")
            digest.update(chunk)
            remaining -= len(chunk)
    return digest.hexdigest()


def to_cygwin_path(path: Path) -> str:
    resolved = path.resolve()
    drive = resolved.drive
    if len(drive) != 2 or drive[1] != ":":
        raise ValueError(f"无法转换为 Cygwin 路径: {resolved}")
    relative = resolved.as_posix()[3:]
    return f"/cygdrive/{drive[0].lower()}/{relative}"


def expand_image(args: argparse.Namespace) -> None:
    source = args.source.resolve()
    target = args.target.resolve()
    e2fsck = args.e2fsck.resolve()
    resize2fs = args.resize2fs.resolve()
    target_bytes = args.target_bytes
    chunk_size = args.chunk_mib * 1024 * 1024

    if source == target:
        raise ValueError("目标路径不能与源镜像相同")
    if target.exists() and not args.overwrite:
        raise FileExistsError(f"目标文件已存在: {target}")
    if target_bytes % SECTOR_SIZE:
        raise ValueError("目标大小必须是 512 字节的整数倍")
    if target_bytes <= source.stat().st_size:
        raise ValueError("目标大小必须大于源镜像")
    for tool in (e2fsck, resize2fs):
        if not tool.is_file():
            raise FileNotFoundError(f"找不到工具: {tool}")

    target.parent.mkdir(parents=True, exist_ok=True)
    with source.open("rb") as source_stream:
        layout = parse_primary_gpt(source_stream)
        partitions = list_partitions(layout)
        selected = next(
            (item for item in partitions if item.index == args.partition_index), None
        )
        if selected is None:
            raise ValueError(f"找不到第 {args.partition_index} 个 GPT 分区")
        old_partition_size = (selected.last_lba - selected.first_lba + 1) * SECTOR_SIZE

        print(f"[复制] {source.name} -> {target.name}")
        with target.open("wb") as target_stream:
            source_stream.seek(0)
            shutil.copyfileobj(source_stream, target_stream, length=chunk_size)
            target_stream.truncate(target_bytes)

    target_sectors = target_bytes // SECTOR_SIZE
    entries_sectors = layout.partition_entries_sectors
    new_last_lba = target_sectors - entries_sectors - 2
    new_partition_size = (new_last_lba - selected.first_lba + 1) * SECTOR_SIZE

    temp_dir = args.work_dir.resolve() if args.work_dir else target.parent / ".resize-work"
    temp_dir.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        mode="w+b", prefix=f"{target.stem}-p{args.partition_index}-", suffix=".ext4", dir=temp_dir, delete=False
    ) as temp_stream:
        temp_path = Path(temp_stream.name)
        print(f"[提取] 分区 {args.partition_index}: {old_partition_size} 字节")
        with source.open("rb") as source_stream:
            copy_range(
                source_stream,
                temp_stream,
                selected.first_lba * SECTOR_SIZE,
                old_partition_size,
                chunk_size,
            )
        temp_stream.truncate(new_partition_size)

    try:
        cygwin_temp_path = to_cygwin_path(temp_path)
        print("[检查] 扩容前 ext4")
        run_checked([str(e2fsck), "-f", "-y", cygwin_temp_path], (0, 1))
        print(f"[扩容] ext4 -> {new_partition_size} 字节")
        run_checked([str(resize2fs), cygwin_temp_path])
        # resize2fs 会把普通文件截断到文件系统块边界，恢复末尾的分区对齐填充。
        with temp_path.open("r+b") as temp_stream:
            temp_stream.truncate(new_partition_size)
        print("[检查] 扩容后 ext4")
        run_checked([str(e2fsck), "-f", "-n", cygwin_temp_path])

        print("[回写] 扩容后的 ext4 分区")
        with temp_path.open("rb") as temp_stream, target.open("r+b") as target_stream:
            target_stream.seek(selected.first_lba * SECTOR_SIZE)
            copy_range(temp_stream, target_stream, 0, new_partition_size, chunk_size)

        print("[更新] 主、备 GPT 和保护性 MBR")
        update_gpt(target, layout, target_sectors, args.partition_index)
        grown = verify_gpt(target, args.partition_index)

        protected_start = layout.first_usable_lba * SECTOR_SIZE
        protected_size = (selected.first_lba - layout.first_usable_lba) * SECTOR_SIZE
        print("[校验] 未扩容分区的原始数据区")
        source_hash = sha256_range(source, protected_start, protected_size, chunk_size)
        target_hash = sha256_range(target, protected_start, protected_size, chunk_size)
        if source_hash != target_hash:
            raise ValueError("分区 6 之前的原始数据发生变化")

        print(
            f"[完成] 分区 {grown.index} {grown.name!r}: "
            f"LBA {grown.first_lba}-{grown.last_lba}; 保护区 SHA-256 {target_hash}"
        )
    except Exception:
        if target.exists():
            target.unlink()
        raise
    finally:
        if temp_path.exists():
            temp_path.unlink()
        try:
            temp_dir.rmdir()
        except OSError:
            pass


def main() -> int:
    parser = argparse.ArgumentParser(description="扩展 GPT 镜像的末分区及其 ext4 文件系统")
    parser.add_argument("source", type=Path, help="源 IMG（只读）")
    parser.add_argument("target", type=Path, help="输出 IMG")
    parser.add_argument("--target-bytes", type=int, default=7_800_000_000)
    parser.add_argument("--partition-index", type=int, default=6)
    parser.add_argument("--e2fsck", type=Path, required=True)
    parser.add_argument("--resize2fs", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path)
    parser.add_argument("--chunk-mib", type=int, default=16)
    parser.add_argument("--overwrite", action="store_true")
    args = parser.parse_args()
    expand_image(args)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
