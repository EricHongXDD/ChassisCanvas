from __future__ import annotations

import argparse
import re
from pathlib import Path

import capstone
import pefile
from capstone.x86 import X86_INS_CALL, X86_INS_JMP, X86_OP_IMM

from pe_xrefs import (
    containing_function,
    find_text_values,
    instruction_targets,
    read_virtual,
    runtime_functions,
)


def parse_address(value: str) -> int:
    return int(value, 0)


def decode_function(pe: pefile.PE, address: int):
    functions = sorted(runtime_functions(pe))
    bounds = containing_function(functions, address)
    if bounds is None:
        raise SystemExit(f"找不到包含 {address:#x} 的 x64 运行时函数")
    decoder = capstone.Cs(capstone.CS_ARCH_X86, capstone.CS_MODE_64)
    decoder.detail = True
    decoder.skipdata = True
    instructions = list(decoder.disasm(read_virtual(pe, bounds[0], bounds[1] - bounds[0]), bounds[0]))
    return functions, bounds, instructions


def branch_target(instruction) -> int | None:
    if instruction.id not in (X86_INS_CALL, X86_INS_JMP):
        return None
    try:
        operands = instruction.operands
    except capstone.CsError:
        return None
    if operands and operands[0].type == X86_OP_IMM:
        return operands[0].imm
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description="按虚拟地址反汇编 PE x64 函数并标注字符串和分支目标")
    parser.add_argument("path", type=Path)
    parser.add_argument("address", type=parse_address, help="函数内虚拟地址，例如 0x1402b0100")
    parser.add_argument("--calls", action="store_true", help="列出直接调用的内部函数范围")
    parser.add_argument("--xrefs", action="store_true", help="列出所有直接调用当前函数的指令")
    parser.add_argument("--summary-only", action="store_true", help="只显示函数范围及直接调用摘要")
    parser.add_argument("--strings-only", action="store_true", help="只显示引用字符串的指令")
    parser.add_argument("--start", type=parse_address, help="仅显示此虚拟地址及之后的指令")
    parser.add_argument("--stop", type=parse_address, help="仅显示此虚拟地址之前的指令")
    args = parser.parse_args()

    pe = pefile.PE(str(args.path), fast_load=False)
    functions, bounds, instructions = decode_function(pe, args.address)
    text_values = find_text_values(pe, re.compile("."))
    print(f"函数 {bounds[0]:016x}-{bounds[1]:016x}，长度 {bounds[1] - bounds[0]:#x}")
    called: set[tuple[int, int]] = set()
    for instruction in instructions:
        annotations = [
            f"{target:016x}={text_values[target].value}"
            for target in instruction_targets(instruction)
            if target in text_values
        ]
        target = branch_target(instruction)
        target_text = ""
        if target is not None:
            called_bounds = containing_function(functions, target)
            if called_bounds is not None:
                called.add(called_bounds)
                target_text = f" -> {target:016x} [{called_bounds[0]:016x}-{called_bounds[1]:016x}]"
            else:
                target_text = f" -> {target:016x}"
        suffix = ""
        if annotations:
            suffix = " ; " + " | ".join(annotations)
        in_selected_range = (
            (args.start is None or instruction.address >= args.start)
            and (args.stop is None or instruction.address < args.stop)
        )
        if in_selected_range and not args.summary_only and (not args.strings_only or annotations):
            print(f"{instruction.address:016x}  {instruction.mnemonic:<8} {instruction.op_str}{target_text}{suffix}")

    if args.calls:
        print("--- 直接内部调用 ---")
        for begin, end in sorted(called):
            print(f"{begin:016x}-{end:016x} 长度 {end - begin:#x}")

    if args.xrefs:
        print("--- 直接调用引用 ---")
        decoder = capstone.Cs(capstone.CS_ARCH_X86, capstone.CS_MODE_64)
        decoder.detail = True
        for section in pe.sections:
            if not section.IMAGE_SCN_MEM_EXECUTE:
                continue
            section_address = pe.OPTIONAL_HEADER.ImageBase + section.VirtualAddress
            for instruction in decoder.disasm(section.get_data(), section_address):
                target = branch_target(instruction)
                if target is None or not bounds[0] <= target < bounds[1]:
                    continue
                caller = containing_function(functions, instruction.address)
                caller_text = f"{caller[0]:016x}-{caller[1]:016x}" if caller else "未知"
                print(f"{instruction.address:016x} {instruction.mnemonic} {instruction.op_str} | 调用者 {caller_text}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
