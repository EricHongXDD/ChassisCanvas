from __future__ import annotations

import argparse
import struct
from pathlib import Path

import pefile

from pe_xrefs import containing_function, runtime_functions


def main() -> int:
    parser = argparse.ArgumentParser(description="定位 MSVC x64 RTTI 类型对应的虚函数表")
    parser.add_argument("path", type=Path)
    parser.add_argument("type_name", help="例如 .?AVLCDATMOSV2PixelSSRTransporter@@")
    parser.add_argument("--limit", type=int, default=100)
    args = parser.parse_args()

    pe = pefile.PE(str(args.path), fast_load=False)
    data = pe.__data__
    image_base = pe.OPTIONAL_HEADER.ImageBase
    functions = sorted(runtime_functions(pe))
    executable_ranges = [
        (image_base + section.VirtualAddress, image_base + section.VirtualAddress + section.Misc_VirtualSize)
        for section in pe.sections
        if section.IMAGE_SCN_MEM_EXECUTE
    ]

    name = args.type_name.encode("ascii") + b"\0"
    name_offset = data.find(name)
    if name_offset < 0:
        raise SystemExit("没有找到 RTTI 类型名称")
    name_rva = pe.get_rva_from_offset(name_offset)
    type_descriptor_rva = name_rva - 16
    print(f"类型描述符 VA={image_base + type_descriptor_rva:#x} RVA={type_descriptor_rva:#x}")

    type_reference = struct.pack("<I", type_descriptor_rva)
    search_offset = 0
    found = 0
    while found < args.limit:
        match_offset = data.find(type_reference, search_offset)
        if match_offset < 0:
            break
        search_offset = match_offset + 1
        locator_offset = match_offset - 12
        if locator_offset < 0 or locator_offset + 24 > len(data):
            continue
        signature, object_offset, constructor_offset, type_rva, class_rva, self_rva = struct.unpack_from(
            "<IIIIII", data, locator_offset
        )
        try:
            locator_rva = pe.get_rva_from_offset(locator_offset)
        except pefile.PEFormatError:
            continue
        if signature != 1 or type_rva != type_descriptor_rva or self_rva != locator_rva:
            continue
        locator_va = image_base + locator_rva
        print(
            f"完整对象定位器 VA={locator_va:#x} offset={object_offset:#x} "
            f"constructor={constructor_offset:#x} classRVA={class_rva:#x}"
        )
        pointer = struct.pack("<Q", locator_va)
        pointer_search = 0
        while True:
            pointer_offset = data.find(pointer, pointer_search)
            if pointer_offset < 0:
                break
            pointer_search = pointer_offset + 1
            try:
                pointer_rva = pe.get_rva_from_offset(pointer_offset)
            except pefile.PEFormatError:
                continue
            vtable_va = image_base + pointer_rva + 8
            entries = []
            entry_offset = pointer_offset + 8
            while entry_offset + 8 <= len(data):
                target = struct.unpack_from("<Q", data, entry_offset)[0]
                if not any(start <= target < end for start, end in executable_ranges):
                    break
                entries.append(target)
                entry_offset += 8
            if not entries:
                continue
            print(f"虚函数表 VA={vtable_va:#x}，条目={len(entries)}")
            for index, target in enumerate(entries):
                bounds = containing_function(functions, target)
                bounds_text = f" [{bounds[0]:#x}-{bounds[1]:#x}]" if bounds else ""
                print(f"  [{index:02d}] {target:#x}{bounds_text}")
            found += 1
            if found >= args.limit:
                break
    if found == 0:
        raise SystemExit("没有找到对应虚函数表")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
