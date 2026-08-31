from __future__ import annotations

import argparse
import re
import struct
from dataclasses import dataclass
from pathlib import Path, PurePosixPath


SECTOR_SIZE = 512
EXT4_EXTENTS_FL = 0x00080000


def human_size(size: int) -> str:
    units = ("B", "KiB", "MiB", "GiB")
    value = float(size)
    for unit in units:
        if value < 1024 or unit == units[-1]:
            return f"{value:.2f} {unit}"
        value /= 1024
    return f"{size} B"


@dataclass(frozen=True)
class Inode:
    number: int
    mode: int
    size: int
    flags: int
    block_data: bytes

    @property
    def kind(self) -> str:
        file_type = self.mode & 0xF000
        return {0x4000: "目录", 0x8000: "文件", 0xA000: "链接"}.get(file_type, "其他")


@dataclass(frozen=True)
class DirectoryEntry:
    inode: int
    name: str
    file_type: int


class Ext4Reader:
    def __init__(self, image: Path, partition_lba: int) -> None:
        self.stream = image.open("rb")
        self.base = partition_lba * SECTOR_SIZE
        superblock = self.read_at(1024, 1024)
        if superblock[56:58] != b"\x53\xef":
            raise ValueError("指定分区不是 ext 文件系统")
        self.block_size = 1024 << struct.unpack_from("<I", superblock, 24)[0]
        self.blocks_per_group = struct.unpack_from("<I", superblock, 32)[0]
        self.inodes_per_group = struct.unpack_from("<I", superblock, 40)[0]
        self.inode_size = struct.unpack_from("<H", superblock, 88)[0]
        self.incompat = struct.unpack_from("<I", superblock, 96)[0]
        desc_size = struct.unpack_from("<H", superblock, 0xFE)[0]
        self.desc_size = max(32, desc_size if self.incompat & 0x80 else 32)
        self.group_desc_offset = self.block_size if self.block_size > 1024 else 2 * self.block_size

    def close(self) -> None:
        self.stream.close()

    def read_at(self, relative_offset: int, size: int) -> bytes:
        self.stream.seek(self.base + relative_offset)
        return self.stream.read(size)

    def read_block(self, block: int) -> bytes:
        return self.read_at(block * self.block_size, self.block_size)

    def inode_table_block(self, group: int) -> int:
        descriptor = self.read_at(self.group_desc_offset + group * self.desc_size, self.desc_size)
        table_lo = struct.unpack_from("<I", descriptor, 8)[0]
        table_hi = struct.unpack_from("<I", descriptor, 0x28)[0] if self.desc_size >= 64 else 0
        return table_lo | (table_hi << 32)

    def read_inode(self, number: int) -> Inode:
        if number <= 0:
            raise ValueError(f"无效 inode: {number}")
        group, index = divmod(number - 1, self.inodes_per_group)
        inode_offset = self.inode_table_block(group) * self.block_size + index * self.inode_size
        raw = self.read_at(inode_offset, self.inode_size)
        mode = struct.unpack_from("<H", raw, 0)[0]
        size_lo = struct.unpack_from("<I", raw, 4)[0]
        flags = struct.unpack_from("<I", raw, 32)[0]
        size_hi = struct.unpack_from("<I", raw, 108)[0] if len(raw) >= 112 else 0
        return Inode(number, mode, size_lo | (size_hi << 32), flags, raw[40:100])

    def _extent_ranges_from_node(self, node: bytes) -> list[tuple[int, int, int]]:
        magic, entries, _maximum, depth = struct.unpack_from("<HHHH", node, 0)
        if magic != 0xF30A:
            raise ValueError("无效的 ext4 extent 节点")
        ranges: list[tuple[int, int, int]] = []
        for index in range(entries):
            entry = node[12 + index * 12 : 24 + index * 12]
            logical = struct.unpack_from("<I", entry, 0)[0]
            if depth == 0:
                length, start_hi, start_lo = struct.unpack_from("<HHI", entry, 4)
                length &= 0x7FFF
                ranges.append((logical, start_lo | (start_hi << 32), length))
            else:
                leaf_lo, leaf_hi = struct.unpack_from("<IH", entry, 4)
                leaf = leaf_lo | (leaf_hi << 32)
                ranges.extend(self._extent_ranges_from_node(self.read_block(leaf)))
        return ranges

    def block_ranges(self, inode: Inode) -> list[tuple[int, int, int]]:
        if inode.flags & EXT4_EXTENTS_FL:
            return sorted(self._extent_ranges_from_node(inode.block_data))

        # 兼容没有启用 extent 的小文件和目录。
        direct = struct.unpack_from("<12I", inode.block_data, 0)
        ranges = [(index, block, 1) for index, block in enumerate(direct) if block]
        indirect = struct.unpack_from("<I", inode.block_data, 48)[0]
        if indirect:
            pointers = struct.unpack(f"<{self.block_size // 4}I", self.read_block(indirect))
            ranges.extend((12 + index, block, 1) for index, block in enumerate(pointers) if block)
        return ranges

    def read_inode_data(self, inode: Inode, limit: int | None = None) -> bytes:
        wanted = inode.size if limit is None else min(inode.size, limit)
        output = bytearray()
        logical_cursor = 0
        for logical, physical, length in self.block_ranges(inode):
            if len(output) >= wanted:
                break
            if logical > logical_cursor:
                output.extend(b"\0" * min((logical - logical_cursor) * self.block_size, wanted - len(output)))
            for block_offset in range(length):
                if len(output) >= wanted:
                    break
                block = self.read_block(physical + block_offset)
                output.extend(block[: wanted - len(output)])
            logical_cursor = logical + length
        return bytes(output[:wanted])

    def list_directory(self, inode_number: int) -> list[DirectoryEntry]:
        inode = self.read_inode(inode_number)
        if inode.kind != "目录":
            raise NotADirectoryError(inode_number)
        entries: list[DirectoryEntry] = []
        for _logical, physical, length in self.block_ranges(inode):
            for block_offset in range(length):
                block = self.read_block(physical + block_offset)
                cursor = 0
                while cursor + 8 <= len(block):
                    entry_inode, record_length = struct.unpack_from("<IH", block, cursor)
                    name_length = block[cursor + 6]
                    file_type = block[cursor + 7]
                    if record_length < 8 or record_length % 4 or cursor + record_length > len(block):
                        break
                    if entry_inode and name_length <= record_length - 8:
                        raw_name = block[cursor + 8 : cursor + 8 + name_length]
                        name = raw_name.decode("utf-8", errors="replace")
                        if name not in (".", ".."):
                            entries.append(DirectoryEntry(entry_inode, name, file_type))
                    cursor += record_length
        unique = {(entry.inode, entry.name): entry for entry in entries}
        return sorted(unique.values(), key=lambda item: item.name.casefold())

    def resolve(self, path: PurePosixPath) -> int:
        inode_number = 2
        for component in path.parts:
            if component in ("/", "", "."):
                continue
            entries = self.list_directory(inode_number)
            match = next((entry for entry in entries if entry.name == component), None)
            if match is None:
                raise FileNotFoundError(str(path))
            inode_number = match.inode
        return inode_number


def print_tree(
    reader: Ext4Reader,
    inode_number: int,
    display_path: PurePosixPath,
    depth: int,
    max_entries: int,
) -> int:
    printed = 0

    def visit(current_inode: int, current_path: PurePosixPath, remaining_depth: int) -> None:
        nonlocal printed
        if remaining_depth < 0 or printed >= max_entries:
            return
        for entry in reader.list_directory(current_inode):
            if printed >= max_entries:
                return
            inode = reader.read_inode(entry.inode)
            child_path = current_path / entry.name
            print(f"{inode.kind:4} {human_size(inode.size):>10}  {child_path}")
            printed += 1
            if inode.kind == "目录" and remaining_depth > 0:
                visit(entry.inode, child_path, remaining_depth - 1)

    visit(inode_number, display_path, depth)
    return printed


def main() -> int:
    parser = argparse.ArgumentParser(description="只读列出磁盘镜像内 ext4 分区目录")
    parser.add_argument("image", type=Path)
    parser.add_argument("--partition-lba", type=int, required=True)
    parser.add_argument("--path", default="/")
    parser.add_argument("--depth", type=int, default=1)
    parser.add_argument("--max-entries", type=int, default=300)
    parser.add_argument("--text", action="store_true", help="将文件预览按 UTF-8 文本输出")
    parser.add_argument("--strings", action="store_true", help="提取文件中的可打印 ASCII 字符串")
    parser.add_argument("--contains", action="append", default=[], help="只显示包含指定文本的字符串")
    parser.add_argument("--max-bytes", type=int, default=4096, help="文件预览的最大字节数")
    parser.add_argument("--output", type=Path, help="将单个文件完整提取到指定路径")
    args = parser.parse_args()

    reader = Ext4Reader(args.image.resolve(), args.partition_lba)
    try:
        path = PurePosixPath(args.path)
        inode_number = reader.resolve(path)
        inode = reader.read_inode(inode_number)
        print(
            f"路径: {path}，inode={inode_number}，类型={inode.kind}，"
            f"大小={human_size(inode.size)}"
        )
        if inode.kind == "目录":
            count = print_tree(reader, inode_number, path, args.depth, args.max_entries)
            if count >= args.max_entries:
                print(f"输出已限制为 {args.max_entries} 项")
        else:
            if args.output is not None:
                output_path = args.output.resolve()
                output_path.parent.mkdir(parents=True, exist_ok=True)
                output_path.write_bytes(reader.read_inode_data(inode))
                print(f"已提取到: {output_path}（{human_size(inode.size)}）")
            else:
                preview = reader.read_inode_data(inode, max(1, args.max_bytes))
                if args.strings:
                    strings = [item.decode("ascii") for item in re.findall(rb"[\x20-\x7e]{4,}", preview)]
                    filters = [item.casefold() for item in args.contains]
                    for item in strings:
                        if not filters or any(pattern in item.casefold() for pattern in filters):
                            print(item)
                elif args.text:
                    print(preview.decode("utf-8", errors="replace"))
                else:
                    print(preview.hex(" "))
    finally:
        reader.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
