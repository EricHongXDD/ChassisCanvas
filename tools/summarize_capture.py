from __future__ import annotations

import argparse
import json
from collections import Counter, defaultdict
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser(description="汇总 Windows I/O 抓取结果")
    parser.add_argument("capture", type=Path)
    args = parser.parse_args()

    lengths: Counter[tuple[str, str, int]] = Counter()
    samples: dict[tuple[str, str, int], list[str]] = defaultdict(list)
    with args.capture.open("r", encoding="utf-8") as stream:
        for line in stream:
            item = json.loads(line)
            key = (item.get("api", "?"), item.get("handle", "?"), item.get("length", 0))
            lengths[key] += 1
            data = item.get("data", "")
            if data and len(samples[key]) < 3 and data not in samples[key]:
                samples[key].append(data)

    for key, count in lengths.most_common():
        print(f"{count:6d}  API={key[0]:22s} HANDLE={key[1]:>8s} LENGTH={key[2]}")
        for sample in samples[key]:
            first_line = sample.splitlines()[0] if sample else ""
            print(f"         {first_line}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

