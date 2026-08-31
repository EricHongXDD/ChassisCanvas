from __future__ import annotations

import argparse
import tempfile
from pathlib import Path

from expand_gpt_ext4_image import (
    SECTOR_SIZE,
    copy_range,
    list_partitions,
    parse_primary_gpt,
    run_checked,
    sha256_range,
    to_cygwin_path,
    verify_gpt,
)


def verify(args: argparse.Namespace) -> None:
    image = args.image.resolve()
    source = args.source.resolve() if args.source else None
    e2fsck = args.e2fsck.resolve()
    work_dir = args.work_dir.resolve()
    chunk_size = args.chunk_mib * 1024 * 1024

    work_dir.mkdir(parents=True, exist_ok=True)
    with image.open("rb") as stream:
        layout = parse_primary_gpt(stream)
        partitions = list_partitions(layout)
    partition = next(
        (item for item in partitions if item.index == args.partition_index), None
    )
    if partition is None:
        raise ValueError(f"找不到第 {args.partition_index} 个 GPT 分区")
    verified_partition = verify_gpt(image, args.partition_index)
    if verified_partition != partition:
        raise ValueError("GPT 二次解析结果不一致")

    partition_size = (partition.last_lba - partition.first_lba + 1) * SECTOR_SIZE
    with tempfile.NamedTemporaryFile(
        mode="w+b",
        prefix=f"verify-{image.stem}-p{partition.index}-",
        suffix=".ext4",
        dir=work_dir,
        delete=False,
    ) as temp_stream:
        temp_path = Path(temp_stream.name)
        print(f"[提取] {image.name} 分区 {partition.index}: {partition_size} 字节")
        with image.open("rb") as image_stream:
            copy_range(
                image_stream,
                temp_stream,
                partition.first_lba * SECTOR_SIZE,
                partition_size,
                chunk_size,
            )

    try:
        print("[检查] 最终镜像中的 ext4（只读）")
        run_checked([str(e2fsck), "-f", "-n", to_cygwin_path(temp_path)])

        if source:
            protected_start = layout.first_usable_lba * SECTOR_SIZE
            protected_size = (partition.first_lba - layout.first_usable_lba) * SECTOR_SIZE
            source_hash = sha256_range(source, protected_start, protected_size, chunk_size)
            image_hash = sha256_range(image, protected_start, protected_size, chunk_size)
            if source_hash != image_hash:
                raise ValueError("未扩容分区的原始数据区与源镜像不同")
            print(f"[通过] 未扩容分区数据 SHA-256: {image_hash}")

        image_sha256 = sha256_range(image, 0, image.stat().st_size, chunk_size)
        print(f"[通过] GPT 主备表、ext4 与数据区校验")
        print(f"[SHA-256] {image_sha256}  {image.name}")
    finally:
        if temp_path.exists():
            temp_path.unlink()
        try:
            work_dir.rmdir()
        except OSError:
            pass


def main() -> int:
    parser = argparse.ArgumentParser(description="独立校验扩容后的 GPT/ext4 镜像")
    parser.add_argument("image", type=Path)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--partition-index", type=int, default=6)
    parser.add_argument("--e2fsck", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--chunk-mib", type=int, default=16)
    args = parser.parse_args()
    verify(args)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
