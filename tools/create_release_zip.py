#!/usr/bin/env python3
"""创建保留 Linux 可执行权限的 CaseScreen 发行 ZIP。"""

from __future__ import annotations

import argparse
import stat
import time
import zipfile
from pathlib import Path


EXECUTABLE_PATHS = {
    "screen/casescreen-acrcloud-fingerprint",
    "screen/casescreen-edge",
    "server/casescreen-recognition-relay",
}


def executable_mode(relative_path: str) -> bool:
    return relative_path in EXECUTABLE_PATHS or relative_path.endswith(".sh")


def zip_datetime(path: Path) -> tuple[int, int, int, int, int, int]:
    parts = list(time.localtime(path.stat().st_mtime)[:6])
    parts[0] = min(2107, max(1980, parts[0]))
    return tuple(parts)  # type: ignore[return-value]


def build_archive(source: Path, target: Path) -> None:
    source = source.resolve(strict=True)
    target = target.resolve(strict=False)
    if not source.is_dir():
        raise ValueError(f"发行源不是目录：{source}")
    if source == target or source in target.parents:
        raise ValueError("ZIP 输出不能位于待打包目录内")
    if target.exists():
        raise FileExistsError(f"拒绝覆盖已有 ZIP：{target}")
    target.parent.mkdir(parents=True, exist_ok=True)

    files = sorted(path for path in source.rglob("*") if path.is_file())
    if not files:
        raise ValueError("发行目录为空")

    with zipfile.ZipFile(
        target,
        mode="x",
        compression=zipfile.ZIP_DEFLATED,
        compresslevel=9,
        allowZip64=True,
    ) as archive:
        for path in files:
            if path.is_symlink():
                raise ValueError(f"发行目录包含符号链接：{path}")
            relative = path.relative_to(source).as_posix()
            permissions = 0o755 if executable_mode(relative) else 0o644
            info = zipfile.ZipInfo(relative, date_time=zip_datetime(path))
            info.create_system = 3
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (stat.S_IFREG | permissions) << 16
            with path.open("rb") as source_stream, archive.open(
                info, mode="w", force_zip64=True
            ) as target_stream:
                while chunk := source_stream.read(1024 * 1024):
                    target_stream.write(chunk)
    verify_archive(target)


def verify_archive(target: Path) -> None:
    with zipfile.ZipFile(target, mode="r") as archive:
        if not archive.infolist():
            raise ValueError("ZIP 没有文件")
        for info in archive.infolist():
            expected = 0o755 if executable_mode(info.filename) else 0o644
            actual = (info.external_attr >> 16) & 0o777
            if info.create_system != 3 or actual != expected:
                raise ValueError(
                    f"ZIP Unix 权限无效：{info.filename}，"
                    f"create_system={info.create_system}，mode={actual:o}"
                )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--target", required=True, type=Path)
    return parser


def main() -> int:
    arguments = build_parser().parse_args()
    build_archive(arguments.source, arguments.target)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
