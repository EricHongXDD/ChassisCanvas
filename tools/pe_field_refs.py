from __future__ import annotations

import argparse
from pathlib import Path

import capstone
import pefile
from capstone.x86 import X86_OP_MEM, X86_REG_RIP

from pe_xrefs import containing_function, runtime_functions


def main() -> int:
    parser = argparse.ArgumentParser(description="查找 PE x64 指令对指定结构成员偏移的访问")
    parser.add_argument("path", type=Path)
    parser.add_argument("offset", type=lambda value: int(value, 0))
    parser.add_argument("--limit", type=int, default=200)
    args = parser.parse_args()

    pe = pefile.PE(str(args.path), fast_load=False)
    functions = sorted(runtime_functions(pe))
    decoder = capstone.Cs(capstone.CS_ARCH_X86, capstone.CS_MODE_64)
    decoder.detail = True
    decoder.skipdata = True
    found = 0
    for section in pe.sections:
        if not section.IMAGE_SCN_MEM_EXECUTE:
            continue
        address = pe.OPTIONAL_HEADER.ImageBase + section.VirtualAddress
        for instruction in decoder.disasm(section.get_data(), address):
            try:
                matched = any(
                    operand.type == X86_OP_MEM
                    and operand.mem.base != X86_REG_RIP
                    and operand.mem.disp == args.offset
                    for operand in instruction.operands
                )
            except capstone.CsError:
                continue
            if not matched:
                continue
            function = containing_function(functions, instruction.address)
            function_text = f"{function[0]:016x}-{function[1]:016x}" if function else "未知"
            print(f"{instruction.address:016x} {instruction.mnemonic:<8} {instruction.op_str} | 函数 {function_text}")
            found += 1
            if found >= args.limit:
                return 0
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
