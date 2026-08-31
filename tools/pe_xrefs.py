from __future__ import annotations

import argparse
import re
from dataclasses import dataclass
from pathlib import Path

import capstone
import pefile
from capstone.x86 import X86_OP_IMM, X86_OP_MEM, X86_REG_RIP


ASCII = re.compile(rb"[\x20-\x7e]{4,}")
UTF16_LE = re.compile(rb"(?:[\x20-\x7e]\x00){4,}")


@dataclass(frozen=True)
class TextValue:
    address: int
    value: str


def find_text_values(pe: pefile.PE, pattern: re.Pattern[str]) -> dict[int, TextValue]:
    values: dict[int, TextValue] = {}
    data = pe.__data__
    for regex, encoding in ((ASCII, "ascii"), (UTF16_LE, "utf-16-le")):
        for match in regex.finditer(data):
            value = match.group().decode(encoding, errors="replace")
            if not pattern.search(value):
                continue
            try:
                rva = pe.get_rva_from_offset(match.start())
            except pefile.PEFormatError:
                continue
            if rva is None:
                continue
            address = pe.OPTIONAL_HEADER.ImageBase + rva
            values[address] = TextValue(address=address, value=value)
    return values


def runtime_functions(pe: pefile.PE) -> list[tuple[int, int]]:
    functions = []
    for entry in getattr(pe, "DIRECTORY_ENTRY_EXCEPTION", []):
        begin = pe.OPTIONAL_HEADER.ImageBase + entry.struct.BeginAddress
        end = pe.OPTIONAL_HEADER.ImageBase + entry.struct.EndAddress
        functions.append((begin, end))
    return functions


def containing_function(functions: list[tuple[int, int]], address: int) -> tuple[int, int] | None:
    left = 0
    right = len(functions)
    while left < right:
        middle = (left + right) // 2
        if functions[middle][0] <= address:
            left = middle + 1
        else:
            right = middle
    if left:
        candidate = functions[left - 1]
        if candidate[0] <= address < candidate[1]:
            return candidate
    return None


def read_virtual(pe: pefile.PE, address: int, size: int) -> bytes:
    rva = address - pe.OPTIONAL_HEADER.ImageBase
    return pe.get_data(rva, size)


def instruction_targets(instruction) -> list[int]:
    targets = []
    try:
        operands = instruction.operands
    except capstone.CsError:
        return targets
    for operand in operands:
        if operand.type == X86_OP_MEM and operand.mem.base == X86_REG_RIP:
            targets.append(instruction.address + instruction.size + operand.mem.disp)
        elif operand.type == X86_OP_IMM:
            targets.append(operand.imm)
    return targets


def main() -> None:
    parser = argparse.ArgumentParser(description="定位 PE 代码对指定字符串的引用")
    parser.add_argument("path", type=Path)
    parser.add_argument("--match", required=True, help="不区分大小写的字符串正则")
    parser.add_argument("--limit", type=int, default=100)
    parser.add_argument("--show-function", action="store_true")
    args = parser.parse_args()

    pe = pefile.PE(str(args.path), fast_load=False)
    pattern = re.compile(args.match, re.IGNORECASE)
    text_values = find_text_values(pe, pattern)
    if not text_values:
        raise SystemExit("没有找到匹配字符串")
    annotations_lookup = find_text_values(pe, re.compile(".")) if args.show_function else text_values
    functions = sorted(runtime_functions(pe))
    decoder = capstone.Cs(capstone.CS_ARCH_X86, capstone.CS_MODE_64)
    decoder.detail = True
    decoder.skipdata = True
    found = 0
    shown_functions: set[tuple[int, int]] = set()
    for section in pe.sections:
        if not section.IMAGE_SCN_MEM_EXECUTE:
            continue
        address = pe.OPTIONAL_HEADER.ImageBase + section.VirtualAddress
        for instruction in decoder.disasm(section.get_data(), address):
            matches = [text_values[target] for target in instruction_targets(instruction) if target in text_values]
            if not matches:
                continue
            function = containing_function(functions, instruction.address)
            function_text = (
                f"函数 {function[0]:016x}-{function[1]:016x}"
                if function
                else "函数范围未知"
            )
            for value in matches:
                print(
                    f"引用 {instruction.address:016x}: {instruction.mnemonic} {instruction.op_str} | "
                    f"{function_text} | {value.address:016x} {value.value}"
                )
                found += 1
                if found >= args.limit:
                    return
            if args.show_function and function and function not in shown_functions:
                shown_functions.add(function)
                print(f"--- {function_text} ---")
                for item in decoder.disasm(read_virtual(pe, function[0], function[1] - function[0]), function[0]):
                    annotations = [
                        annotations_lookup[target].value
                        for target in instruction_targets(item)
                        if target in annotations_lookup
                    ]
                    suffix = f" ; {' | '.join(annotations)}" if annotations else ""
                    print(f"{item.address:016x}  {item.mnemonic:<8} {item.op_str}{suffix}")


if __name__ == "__main__":
    main()
