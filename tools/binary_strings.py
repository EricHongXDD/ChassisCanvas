from __future__ import annotations

import argparse
import re
from pathlib import Path


ASCII = re.compile(rb"[\x20-\x7e]{4,}")
UTF16_LE = re.compile(rb"(?:[\x20-\x7e]\x00){4,}")


def strings(path: Path):
    data = path.read_bytes()
    for match in ASCII.finditer(data):
        yield match.start(), match.group().decode("ascii", errors="replace")
    for match in UTF16_LE.finditer(data):
        yield match.start(), match.group().decode("utf-16-le", errors="replace")


def main() -> None:
    parser = argparse.ArgumentParser(description="提取 PE/二进制中的 ASCII 与 UTF-16LE 字符串")
    parser.add_argument("path", type=Path)
    parser.add_argument("--match", default="", help="不区分大小写的正则过滤")
    parser.add_argument("--limit", type=int, default=500)
    args = parser.parse_args()
    pattern = re.compile(args.match, re.IGNORECASE) if args.match else None
    count = 0
    for offset, value in sorted(strings(args.path), key=lambda item: item[0]):
        if pattern and not pattern.search(value):
            continue
        print(f"{offset:08x} {value}")
        count += 1
        if count >= args.limit:
            break


if __name__ == "__main__":
    main()
