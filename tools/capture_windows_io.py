from __future__ import annotations

import argparse
import json
import time
from datetime import datetime
from pathlib import Path

import frida


HOOK_SOURCE = r"""
const maxBytes = __MAX_BYTES__;
const trackedOnly = __TRACKED_ONLY__;
const trackedHandles = new Set(__INITIAL_HANDLES__);
const pendingReads = new Map();
const probeHandleText = __PROBE_HANDLE__;

function isInterestingPath(path) {
    const upper = path.toUpperCase();
    return upper.startsWith("\\\\.\\COM") ||
        upper.includes("\\\\?\\HID#") ||
        upper.includes("USB#VID_");
}

function shouldCapture(handle) {
    return !trackedOnly || trackedHandles.has(handle);
}

function readBytes(pointer, requested) {
    const size = Math.min(requested, maxBytes);
    if (pointer.isNull() || size <= 0) return "";
    try {
        return hexdump(pointer, { offset: 0, length: size, header: false, ansi: false });
    } catch (_) {
        return "";
    }
}

function sendRead(api, handle, pointer, actual, ok) {
    if (!shouldCapture(handle) || actual <= 0) return;
    send({
        api: api,
        handle: handle,
        length: actual,
        ok: ok,
        data: readBytes(pointer, actual)
    });
}

function hookExport(moduleName, exportName, callbacks) {
    const address = Module.findGlobalExportByName(exportName);
    if (address === null) return;
    Interceptor.attach(address, callbacks);
}

if (probeHandleText !== "") {
    setImmediate(function () {
        try {
            const handle = ptr(probeHandleText);
            const dcb = Memory.alloc(28);
            dcb.writeU32(28);
            const getCommStateAddress = Module.findGlobalExportByName("GetCommState");
            const getCommTimeoutsAddress = Module.findGlobalExportByName("GetCommTimeouts");
            if (getCommStateAddress !== null) {
                const getCommState = new NativeFunction(getCommStateAddress, "int", ["pointer", "pointer"]);
                const ok = getCommState(handle, dcb) !== 0;
                send({
                    api: "GetCommStateProbe",
                    handle: probeHandleText,
                    ok: ok,
                    baud_rate: dcb.add(4).readU32(),
                    flags: dcb.add(8).readU32(),
                    byte_size: dcb.add(18).readU8(),
                    parity: dcb.add(19).readU8(),
                    stop_bits: dcb.add(20).readU8()
                });
            }
            if (getCommTimeoutsAddress !== null) {
                const timeouts = Memory.alloc(20);
                const getCommTimeouts = new NativeFunction(getCommTimeoutsAddress, "int", ["pointer", "pointer"]);
                const ok = getCommTimeouts(handle, timeouts) !== 0;
                send({
                    api: "GetCommTimeoutsProbe",
                    handle: probeHandleText,
                    ok: ok,
                    read_interval: timeouts.readU32(),
                    read_multiplier: timeouts.add(4).readU32(),
                    read_constant: timeouts.add(8).readU32(),
                    write_multiplier: timeouts.add(12).readU32(),
                    write_constant: timeouts.add(16).readU32()
                });
            }
        } catch (error) {
            send({api: "SerialProbeError", message: String(error)});
        }
    });
}

hookExport("KernelBase.dll", "CreateFileW", {
    onEnter(args) {
        try {
            this.path = args[0].readUtf16String();
        } catch (_) {
            this.path = "";
        }
    },
    onLeave(retval) {
        if (this.path && isInterestingPath(this.path)) {
            trackedHandles.add(retval.toString());
            send({
                api: "CreateFileW",
                path: this.path,
                handle: retval.toString()
            });
        }
    }
});

hookExport("KernelBase.dll", "WriteFile", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.capture = shouldCapture(this.handle);
        if (!this.capture) return;
        this.length = args[2].toUInt32();
        this.data = readBytes(args[1], this.length);
    },
    onLeave(retval) {
        if (!this.capture) return;
        send({
            api: "WriteFile",
            handle: this.handle,
            length: this.length,
            ok: !retval.isNull() && retval.toInt32() !== 0,
            data: this.data
        });
    }
});

hookExport("KernelBase.dll", "WriteFileEx", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.capture = shouldCapture(this.handle);
        if (!this.capture) return;
        this.length = args[2].toUInt32();
        this.data = readBytes(args[1], this.length);
    },
    onLeave(retval) {
        if (!this.capture) return;
        send({
            api: "WriteFileEx",
            handle: this.handle,
            length: this.length,
            ok: !retval.isNull() && retval.toInt32() !== 0,
            data: this.data
        });
    }
});

// QSerialPort 在 Windows 上通常使用重叠 ReadFile + IOCP。
hookExport("KernelBase.dll", "ReadFile", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.capture = shouldCapture(this.handle);
        if (!this.capture) return;
        this.buffer = args[1];
        this.requested = args[2].toUInt32();
        this.bytesRead = args[3];
        this.overlapped = args[4];
    },
    onLeave(retval) {
        if (!this.capture) return;
        const ok = !retval.isNull() && retval.toInt32() !== 0;
        if (ok && !this.bytesRead.isNull()) {
            const actual = this.bytesRead.readU32();
            sendRead("ReadFile", this.handle, this.buffer, actual, true);
            return;
        }
        if (!this.overlapped.isNull()) {
            pendingReads.set(this.overlapped.toString(), {
                handle: this.handle,
                buffer: this.buffer,
                requested: this.requested
            });
        }
    }
});

hookExport("KernelBase.dll", "GetOverlappedResult", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.overlapped = args[1];
        this.bytesRead = args[2];
    },
    onLeave(retval) {
        if (retval.isNull() || retval.toInt32() === 0 || this.overlapped.isNull()) return;
        const key = this.overlapped.toString();
        const pending = pendingReads.get(key);
        if (pending === undefined || this.bytesRead.isNull()) return;
        pendingReads.delete(key);
        sendRead("GetOverlappedResult", pending.handle, pending.buffer, this.bytesRead.readU32(), true);
    }
});

hookExport("KernelBase.dll", "GetQueuedCompletionStatus", {
    onEnter(args) {
        this.bytesRead = args[1];
        this.overlappedOut = args[3];
    },
    onLeave(retval) {
        if (this.bytesRead.isNull() || this.overlappedOut.isNull()) return;
        const overlapped = this.overlappedOut.readPointer();
        if (overlapped.isNull()) return;
        const key = overlapped.toString();
        const pending = pendingReads.get(key);
        if (pending === undefined) return;
        pendingReads.delete(key);
        sendRead("GetQueuedCompletionStatus", pending.handle, pending.buffer, this.bytesRead.readU32(), retval.toInt32() !== 0);
    }
});

// Qt 的串口后端可能直接调用 ntdll，绕过 KernelBase 的 WriteFile。
hookExport("ntdll.dll", "NtWriteFile", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.capture = shouldCapture(this.handle);
        if (!this.capture) return;
        this.length = args[6].toUInt32();
        this.data = readBytes(args[5], this.length);
    },
    onLeave(retval) {
        if (!this.capture) return;
        send({
            api: "NtWriteFile",
            handle: this.handle,
            length: this.length,
            status: retval.toInt32(),
            data: this.data
        });
    }
});

hookExport("ntdll.dll", "NtReadFile", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.capture = shouldCapture(this.handle);
        if (!this.capture) return;
        this.ioStatus = args[4];
        this.buffer = args[5];
        this.requested = args[6].toUInt32();
    },
    onLeave(retval) {
        if (!this.capture || this.ioStatus.isNull()) return;
        const status = retval.toInt32();
        if (status === 0) {
            const actual = Process.pointerSize === 8
                ? this.ioStatus.add(Process.pointerSize).readU64().toNumber()
                : this.ioStatus.add(Process.pointerSize).readU32();
            sendRead("NtReadFile", this.handle, this.buffer, actual, true);
        }
    }
});

hookExport("hid.dll", "HidD_SetOutputReport", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.capture = shouldCapture(this.handle);
        if (!this.capture) return;
        this.length = args[2].toUInt32();
        this.data = readBytes(args[1], this.length);
    },
    onLeave(retval) {
        if (!this.capture) return;
        send({
            api: "HidD_SetOutputReport",
            handle: this.handle,
            length: this.length,
            ok: retval.toInt32() !== 0,
            data: this.data
        });
    }
});

hookExport("hid.dll", "HidD_SetFeature", {
    onEnter(args) {
        this.handle = args[0].toString();
        this.capture = shouldCapture(this.handle);
        if (!this.capture) return;
        this.length = args[2].toUInt32();
        this.data = readBytes(args[1], this.length);
    },
    onLeave(retval) {
        if (!this.capture) return;
        send({
            api: "HidD_SetFeature",
            handle: this.handle,
            length: this.length,
            ok: retval.toInt32() !== 0,
            data: this.data
        });
    }
});
"""


def _select_process(name: str | None, pid: int | None) -> int:
    if pid is not None:
        return pid
    assert name is not None
    matches = [process for process in frida.get_local_device().enumerate_processes() if process.name.lower() == name.lower()]
    if not matches:
        raise SystemExit(f"未找到进程：{name}")
    if len(matches) > 1:
        raise SystemExit(f"发现多个同名进程，请使用 --pid：{[item.pid for item in matches]}")
    return matches[0].pid


def main() -> int:
    parser = argparse.ArgumentParser(description="抓取原厂软件向串口或 HID 写入的报文")
    target = parser.add_mutually_exclusive_group(required=True)
    target.add_argument("--name", help="进程名，包含扩展名")
    target.add_argument("--pid", type=int, help="进程 PID")
    target.add_argument("--spawn", type=Path, help="在进程启动前注入抓取器")
    parser.add_argument("--seconds", type=float, default=15.0, help="抓取时长")
    parser.add_argument("--output", type=Path, required=True, help="JSONL 输出路径")
    parser.add_argument("--tracked-only", action="store_true", help="只记录本次捕获到的串口/HID句柄")
    parser.add_argument("--handle", action="append", default=[], help="额外记录的句柄，例如 0xe88")
    parser.add_argument("--max-bytes", type=int, default=8192, help="每次写入最多保留的字节数")
    parser.add_argument("--probe-dcb", action="store_true", help="读取第一个 --handle 当前串口参数")
    args = parser.parse_args()

    device = frida.get_local_device()
    spawned = args.spawn is not None
    if spawned:
        executable = args.spawn.resolve()
        selected_pid = device.spawn([str(executable)], cwd=str(executable.parent))
    else:
        selected_pid = _select_process(args.name, args.pid)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    output = args.output.open("w", encoding="utf-8", newline="\n")

    def on_message(message, _data) -> None:
        if message.get("type") != "send":
            payload = {"api": "frida", "message": message}
        else:
            payload = message["payload"]
        payload["captured_at"] = datetime.now().isoformat(timespec="milliseconds")
        output.write(json.dumps(payload, ensure_ascii=False) + "\n")
        output.flush()

    session = device.attach(selected_pid)
    hook_source = HOOK_SOURCE.replace("__MAX_BYTES__", str(max(1, args.max_bytes)))
    hook_source = hook_source.replace("__TRACKED_ONLY__", "true" if args.tracked_only else "false")
    hook_source = hook_source.replace("__INITIAL_HANDLES__", json.dumps(args.handle))
    probe_handle = args.handle[0] if args.probe_dcb and args.handle else ""
    hook_source = hook_source.replace("__PROBE_HANDLE__", json.dumps(probe_handle))
    script = session.create_script(hook_source)
    script.on("message", on_message)
    script.load()
    if spawned:
        device.resume(selected_pid)
    print(f"已附加 PID {selected_pid}，抓取 {args.seconds:g} 秒…")
    try:
        time.sleep(args.seconds)
    finally:
        script.unload()
        session.detach()
        output.close()
    print(args.output.resolve())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
