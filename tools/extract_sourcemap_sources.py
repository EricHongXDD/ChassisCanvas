from __future__ import annotations

import argparse
import json
import re
from pathlib import Path


def _safe_name(source: str) -> Path:
    normalized = source.replace("webpack://", "").replace("\\", "/")
    parts = []
    for part in normalized.split("/"):
        if part in {"", ".", ".."}:
            continue
        sanitized = re.sub(r'[<>:"|?*^]', "_", part).rstrip(". ")
        parts.append(sanitized or "unnamed")
    return Path(*parts) if parts else Path("unknown.js")


def extract(source_map: Path, destination: Path) -> int:
    data = json.loads(source_map.read_text(encoding="utf-8"))
    sources = data.get("sources", [])
    contents = data.get("sourcesContent", [])
    destination.mkdir(parents=True, exist_ok=True)
    destination = destination.resolve()
    written = 0
    for index, source in enumerate(sources):
        if index >= len(contents) or contents[index] is None:
            continue
        relative = _safe_name(str(source))
        output = (destination / relative).resolve()
        try:
            output.relative_to(destination)
        except ValueError as error:
            raise ValueError(f"source map 路径越界: {source}") from error
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(str(contents[index]), encoding="utf-8", newline="\n")
        written += 1
    return written


def main() -> None:
    parser = argparse.ArgumentParser(description="从 source map 恢复内嵌源文件")
    parser.add_argument("source_map", type=Path)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    count = extract(args.source_map, args.destination)
    print(f"已恢复 {count} 个源文件")


if __name__ == "__main__":
    main()
