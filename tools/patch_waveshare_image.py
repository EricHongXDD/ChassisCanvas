from __future__ import annotations

import argparse
import hashlib
import os
import re
import shlex
import shutil
import ssl
import subprocess
import sys
import tempfile
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Optional
from urllib.parse import urlsplit

from expand_gpt_ext4_image import (
    SECTOR_SIZE,
    PartitionEntry,
    copy_range,
    list_partitions,
    parse_primary_gpt,
    sha256_range,
    to_cygwin_path,
    verify_gpt,
)


REPOSITORY_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_DEBUGFS = REPOSITORY_ROOT / ".tools" / "android-e2fsprogs" / "debugfs.exe"
DEFAULT_E2FSCK = REPOSITORY_ROOT / ".tools" / "android-e2fsprogs" / "e2fsck.exe"

THEME_RESOLUTIONS = ("1024x600", "480x1920")
STANDALONE_THEME_NAME = "casescreen-standalone.Theme"
RUNTIME_MODE = "standalone-recognition"
NETWORK_MODES = ("physical", "usb-gadget-bridge")
USB_GADGET_BRIDGE_MODE = "usb-gadget-bridge"
MONITOR_PATH = "/upper/data/PCMonitorLinux"
ORIGINAL_MONITOR_PATH = "/upper/data/PCMonitorLinux.original"
SERIAL_SCRIPT_PATH = "/upper/data/serial_uart.sh"
ORIGINAL_SERIAL_SCRIPT_PATH = "/upper/data/serial_uart.sh.original"
EDGE_PATH = "/upper/data/casescreen-edge"
CASESCREEN_DIRECTORY = "/upper/data/casescreen"
USB_GADGET_BRIDGE_PATH = f"{CASESCREEN_DIRECTORY}/usb-gadget-bridge.sh"
EXTRACTOR_PATH = f"{CASESCREEN_DIRECTORY}/fingerprint-extractor"
FINGERPRINT_LIBRARY_PATH = f"{CASESCREEN_DIRECTORY}/libacrcloud_extr_tool.so"
USB_GADGET_BRIDGE_SOURCE = REPOSITORY_ROOT / "firmware" / "waveshare" / "serial_uart_casescreen.sh"
CONFIG_PATH = f"{CASESCREEN_DIRECTORY}/recognition.env"
TOKEN_PATH = f"{CASESCREEN_DIRECTORY}/recognition.token"
CA_PATH = f"{CASESCREEN_DIRECTORY}/ca-bundle.pem"
TIME_SYNC_HOOK_PATH = "/upper/etc/hotplug.d/ntp/90-casescreen-time"
DIAGNOSTICS_DIRECTORY = f"{CASESCREEN_DIRECTORY}/diagnostics"
LYRICS_CACHE_DIRECTORY = f"{CASESCREEN_DIRECTORY}/lyrics-cache"
PLATFORM_AUTH_DIRECTORY = f"{CASESCREEN_DIRECTORY}/platform-auth"
IMPORT_DIRECTORY = f"{CASESCREEN_DIRECTORY}/import"
HARDWARE_REPORT_PATH = f"{DIAGNOSTICS_DIRECTORY}/latest.json"
MAX_LYRICS_CACHE_FILES = 256
MAX_LYRICS_CACHE_KIB = 32 * 1024
EXPECTED_PARTITIONS = (
    "boot-resource",
    "env",
    "env-redund",
    "boot",
    "rootfs",
    "UDISK",
)
SAFE_CYGWIN_PATH = re.compile(r"^/[A-Za-z0-9_./-]+$")
SAFE_PCM_DEVICE = re.compile(r"^[A-Za-z0-9_:,.-]{1,128}$")
SAFE_DEVICE_PATH = re.compile(r"^/[A-Za-z0-9_./-]{1,255}$")
SAFE_FINGERPRINT_ALGORITHM = re.compile(r"^[A-Za-z0-9._-]{1,64}$")
SAFE_FINGERPRINT_VERSION = re.compile(r"^[A-Za-z0-9._-]{1,32}$")
WAVESHARE_THEME_HEADER = b"Author:SYKVersion: V2.0.0Description: NULL"

# 输入必须是干净原厂镜像；旧 relay/RNDIS 实验链和已经注入的新链都不能作为二次输入。
# 补丁器不会猜测性删除或覆盖用户数据。
UNSUPPORTED_RUNTIME_PATHS = (
    "/upper/data/casescreen-edge",
    "/upper/data/casescreen-edge-setup",
    "/upper/data/casescreen-setup.qml",
    "/upper/data/serial_uart.sh.acm-only",
    ORIGINAL_SERIAL_SCRIPT_PATH,
    "/upper/data/PCMonitorLinux.original",
    "/upper/data/casescreen/sync.token",
    "/upper/data/casescreen/source-push.key",
    "/upper/data/casescreen/ca-bundle.pem",
    "/upper/data/casescreen/setup.completed",
    "/upper/data/casescreen/platform-auth",
    "/upper/data/casescreen/identity-cache",
    "/upper/data/casescreen/import",
    "/upper/data/casescreen/diagnostics",
    CONFIG_PATH,
    TOKEN_PATH,
    LYRICS_CACHE_DIRECTORY,
    EXTRACTOR_PATH,
    FINGERPRINT_LIBRARY_PATH,
    TIME_SYNC_HOOK_PATH,
    USB_GADGET_BRIDGE_PATH,
)


@dataclass(frozen=True)
class FsStat:
    kind: str
    permissions: int


class ExternalToolError(RuntimeError):
    pass


@dataclass(frozen=True)
class RuntimePayloads:
    edge: Path
    launcher: Path
    original_monitor: Path
    config: Path
    token: Path
    ca_bundle: Path
    time_sync_hook: Path
    theme: Path
    theme_destination: str
    fingerprint_extractor: Optional[Path] = None
    fingerprint_library: Optional[Path] = None
    serial_launcher: Optional[Path] = None
    original_serial_script: Optional[Path] = None
    usb_gadget_bridge: Optional[Path] = None


def read_limited(path: Path, maximum_size: int, description: str) -> bytes:
    size = path.stat().st_size
    if size > maximum_size:
        raise ValueError(f"{description}大小异常: {size} 字节")
    return path.read_bytes()


def standalone_theme_path(resolution: str, *, overlay: bool) -> str:
    if resolution not in THEME_RESOLUTIONS:
        raise ValueError("主题分辨率必须是 1024x600 或 480x1920")
    prefix = "/upper" if overlay else ""
    return f"{prefix}/data/theme/{resolution}/{STANDALONE_THEME_NAME}"


def validate_standalone_theme(path: Path) -> bytes:
    """验证主机首次应用的驻留歌词主题，不接受原始无歌词主题。"""
    data = read_limited(path, 16 * 1024 * 1024, "驻留歌词主题")
    widgets = parse_waveshare_widgets(data)
    widget_values = {value for widget in widgets for value in widget.values()}
    missing_keys = [
        marker
        for marker in ("CaseScreen_LyricsCurrent", "CaseScreen_LyricsNext")
        if marker not in widget_values
    ]
    missing_atoms = [atom.decode("ascii") for atom in (b"ftyp", b"moov", b"mdat") if atom not in data]
    if missing_keys or missing_atoms:
        detail = ", ".join(missing_keys + missing_atoms)
        raise ValueError(f"驻留歌词主题缺少必需内容: {detail}")
    widget_types = [widget.get("type", "") for widget in widgets]
    microphone_types = sorted(widget_type for widget_type in widget_types if widget_type in {"107", "108"})
    if microphone_types:
        raise ValueError(
            "驻留歌词主题包含会争用 ALSA 采集设备的原厂麦克风控件: "
            + ", ".join(microphone_types)
        )
    return data


def _read_be_u32(data: bytes, cursor: int) -> tuple[int, int]:
    if cursor < 0 or cursor + 4 > len(data):
        raise ValueError("微雪主题数据越界")
    return int.from_bytes(data[cursor : cursor + 4], "big"), cursor + 4


def _read_qstring(data: bytes, cursor: int) -> tuple[str, int]:
    length, cursor = _read_be_u32(data, cursor)
    if length == 0xFFFFFFFF:
        return "", cursor
    if length % 2 or cursor + length > len(data):
        raise ValueError("微雪主题 QString 长度无效")
    try:
        return data[cursor : cursor + length].decode("utf-16-be"), cursor + length
    except UnicodeDecodeError as error:
        raise ValueError("微雪主题 QString 编码无效") from error


def _read_string_map(data: bytes, cursor: int) -> tuple[dict[str, str], int]:
    count, cursor = _read_be_u32(data, cursor)
    if count > 100_000:
        raise ValueError("微雪主题字段数量异常")
    fields: dict[str, str] = {}
    for _ in range(count):
        key, cursor = _read_qstring(data, cursor)
        value, cursor = _read_qstring(data, cursor)
        fields[key] = value
    return fields, cursor


def _skip_variant_map(data: bytes, cursor: int) -> int:
    count, cursor = _read_be_u32(data, cursor)
    if count > 100_000:
        raise ValueError("微雪主题设置数量异常")
    fixed_sizes = {1: 1, 2: 4, 3: 4, 4: 8, 5: 8, 6: 8}
    for _ in range(count):
        _, cursor = _read_qstring(data, cursor)
        variant_type, cursor = _read_be_u32(data, cursor)
        if cursor >= len(data):
            raise ValueError("微雪主题 QVariant 数据越界")
        cursor += 1
        if variant_type == 10:
            _, cursor = _read_qstring(data, cursor)
            continue
        size = fixed_sizes.get(variant_type)
        if size is None or cursor + size > len(data):
            raise ValueError(f"微雪主题含不支持的 QVariant 类型: {variant_type}")
        cursor += size
    return cursor


def parse_waveshare_widgets(data: bytes) -> list[dict[str, str]]:
    if not data.startswith(WAVESHARE_THEME_HEADER):
        raise ValueError("微雪主题头无效")
    cursor = _skip_variant_map(data, len(WAVESHARE_THEME_HEADER))
    _, cursor = _read_string_map(data, cursor)
    count, cursor = _read_be_u32(data, cursor)
    if count > 100_000:
        raise ValueError("微雪主题控件数量异常")
    result: list[dict[str, str]] = []
    for _ in range(count):
        fields, cursor = _read_string_map(data, cursor)
        result.append(fields)
    return result


def parse_waveshare_widget_types(data: bytes) -> list[str]:
    return [widget.get("type", "") for widget in parse_waveshare_widgets(data)]


def validate_edge_binary(path: Path) -> bytes:
    """只接受 32 位小端 ARM ELF，且必须包含新识歌模式的参数标记。"""
    data = read_limited(path, 64 * 1024 * 1024, "casescreen-edge")
    validate_arm32_elf(data, "casescreen-edge")
    missing = [
        marker.decode("ascii")
        for marker in (
            b"metadata-mode",
            b"recognition-relay-url",
            b"recognition-token-file",
            b"recognition-pcm",
            b"recognition-arecord",
            b"recognition-chunk",
            b"recognition-config",
            b"recognition-preferred-platform",
            b"recognition-resume-active",
            b"recognition-fingerprint-extractor",
            b"recognition-fingerprint-algorithm",
            b"recognition-fingerprint-version",
        )
        if marker not in data
    ]
    if missing:
        raise ValueError(f"casescreen-edge 不支持 standalone-recognition 参数: {', '.join(missing)}")
    return data


def validate_arm32_elf(data: bytes, description: str) -> None:
    if len(data) < 52 or data[:4] != b"\x7fELF":
        raise ValueError(f"{description} 不是 ELF 可执行文件")
    if data[4] != 1 or data[5] != 1:
        raise ValueError(f"{description} 必须是 32 位小端 ELF")
    machine = int.from_bytes(data[18:20], "little")
    if machine != 40:
        raise ValueError(f"{description} 必须面向 ARM32（e_machine=40），实际 {machine}")


def validate_fingerprint_extractor(path: Path) -> bytes:
    data = read_limited(path, 32 * 1024 * 1024, "声纹提取器")
    validate_arm32_elf(data, "声纹提取器")
    return data


def validate_fingerprint_library(path: Path) -> bytes:
    """验证用户自行取得的 ARM32 共享库，不对其许可或真机 ABI 兼容性作假设。"""
    data = read_limited(path, 32 * 1024 * 1024, "声纹共享库")
    validate_arm32_elf(data, "声纹共享库")
    elf_type = int.from_bytes(data[16:18], "little")
    if elf_type != 3:
        raise ValueError(f"声纹共享库必须是 ELF ET_DYN（3），实际 {elf_type}")
    missing = [symbol for symbol in (b"create_fingerprint", b"acr_free") if symbol not in data]
    if missing:
        names = ", ".join(symbol.decode("ascii") for symbol in missing)
        raise ValueError(f"声纹共享库缺少适配器必需接口: {names}")
    return data


def validate_fingerprint_payload_pair(
    extractor: Optional[bytes], library: Optional[bytes]
) -> None:
    """拒绝生成已知会在首次识歌时失败的 ACRCloud 适配器组合。"""
    if extractor is None:
        if library is not None:
            raise ValueError("声纹共享库不能脱离声纹提取器注入")
        return
    if b"libacrcloud_extr_tool.so" in extractor and library is None:
        raise ValueError("ACRCloud 声纹适配器必须同时注入 libacrcloud_extr_tool.so")


def validate_original_monitor(data: bytes) -> None:
    if len(data) < 52 or data[:4] != b"\x7fELF" or data[4] != 1 or data[5] != 1:
        raise ValueError("镜像内 PCMonitorLinux 不是预期的 32 位小端 ELF")
    if int.from_bytes(data[18:20], "little") != 40:
        raise ValueError("镜像内 PCMonitorLinux 不是 ARM32 程序")


def validate_device_token(path: Path) -> bytes:
    data = read_limited(path, 256, "设备令牌")
    text = data.decode("ascii", errors="strict")
    text = text.removesuffix("\n").removesuffix("\r")
    if not re.fullmatch(r"[0-9a-fA-F]{64}", text):
        raise ValueError("设备令牌必须是单行 64 位十六进制随机值")
    # Bearer 令牌按字节比较；不得静默改变十六进制字母大小写，否则会与
    # relay 端已经生成的 SHA-256 摘要不一致。
    return (text + "\n").encode("ascii")


def validate_ca_bundle(path: Path) -> bytes:
    data = read_limited(path, 4 * 1024 * 1024, "CA 根证书包")
    if b"-----BEGIN CERTIFICATE-----" not in data or b"-----END CERTIFICATE-----" not in data:
        raise ValueError("CA 根证书包不是 PEM 证书集合")
    try:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.load_verify_locations(cafile=str(path))
    except (OSError, ssl.SSLError) as error:
        raise ValueError("CA 根证书包不含可解析的 X.509 PEM 证书") from error
    return data if data.endswith(b"\n") else data + b"\n"


def validate_relay_url(value: str) -> str:
    value = value.strip()
    if len(value) > 2048 or any(ord(character) < 0x20 for character in value):
        raise ValueError("识歌 relay URL 长度或字符无效")
    parsed = urlsplit(value)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or "?" in value
        or "#" in value
    ):
        raise ValueError("standalone-recognition 只接受不含账号、查询参数与片段的 HTTPS relay URL")
    return value


def validate_runtime_options(args: argparse.Namespace) -> tuple[str, str, str, str]:
    if args.runtime_mode != RUNTIME_MODE:
        raise ValueError(f"--runtime-mode 必须明确指定 {RUNTIME_MODE}")
    relay_url = validate_relay_url(args.recognition_relay_url)
    pcm_device = args.recognition_pcm.strip()
    if not SAFE_PCM_DEVICE.fullmatch(pcm_device):
        raise ValueError("--recognition-pcm（PCM 设备）含无效字符")
    arecord_path = args.recognition_arecord.strip()
    if not SAFE_DEVICE_PATH.fullmatch(arecord_path):
        raise ValueError("--recognition-arecord 必须是安全的屏端绝对路径")
    network_mode = str(getattr(args, "network_mode", "physical") or "").strip()
    if network_mode not in NETWORK_MODES:
        raise ValueError("--network-mode 必须是 physical 或 usb-gadget-bridge")
    return relay_url, pcm_device, arecord_path, network_mode


def validate_fingerprint_options(
    args: argparse.Namespace,
) -> tuple[Optional[Path], Optional[Path], str, str]:
    raw_path = getattr(args, "fingerprint_extractor", None)
    raw_library = getattr(args, "fingerprint_library", None)
    algorithm = str(getattr(args, "fingerprint_algorithm", "") or "").strip()
    version = str(getattr(args, "fingerprint_version", "") or "").strip()
    provided = (raw_path is not None, algorithm != "", version != "")
    if any(provided) and not all(provided):
        raise ValueError(
            "--fingerprint-extractor、--fingerprint-algorithm 与 --fingerprint-version 必须同时提供"
        )
    if raw_path is None:
        if raw_library is not None:
            raise ValueError("--fingerprint-library 只能与完整的声纹提取器配置一起提供")
        return None, None, "", ""
    if not SAFE_FINGERPRINT_ALGORITHM.fullmatch(algorithm):
        raise ValueError("声纹 algorithm 必须是 1..64 位安全标识")
    if not SAFE_FINGERPRINT_VERSION.fullmatch(version):
        raise ValueError("声纹 version 必须是 1..32 位安全标识")
    library = Path(raw_library).resolve() if raw_library is not None else None
    return Path(raw_path).resolve(), library, algorithm, version


def make_recognition_config(
    relay_url: str,
    pcm_device: str,
    arecord_path: str,
    fingerprint_algorithm: str = "",
    fingerprint_version: str = "",
    network_mode: str = "physical",
) -> bytes:
    """配置只包含 relay 地址、设备令牌路径和非秘密运行参数。"""
    if network_mode not in NETWORK_MODES:
        raise ValueError("识歌网络模式无效")
    values = {
        "CASESCREEN_RECOGNITION_RELAY_URL": relay_url,
        "CASESCREEN_RECOGNITION_TOKEN_FILE": "/data/casescreen/recognition.token",
        "CASESCREEN_RECOGNITION_PCM": pcm_device,
        "CASESCREEN_RECOGNITION_ARECORD": arecord_path,
        "CASESCREEN_RECOGNITION_CHUNK": "1s",
        "CASESCREEN_RECOGNITION_CAPTURE": "10s",
        "CASESCREEN_RECOGNITION_SEARCH_INTERVAL": "2s",
        "CASESCREEN_RECOGNITION_STEADY_INTERVAL": "15s",
        "CASESCREEN_RECOGNITION_TIMEOUT": "12s",
        "CASESCREEN_RECOGNITION_MIN_CONFIDENCE": "0.78",
        "CASESCREEN_RECOGNITION_CONFIRMATIONS": "2",
        "CASESCREEN_RECOGNITION_PAUSE_SILENT": "2",
        "CASESCREEN_RECOGNITION_RESUME_ACTIVE": "2",
        "CASESCREEN_RECOGNITION_PREFERRED_PLATFORM": "auto",
        "CASESCREEN_RECOGNITION_NETWORK_MODE": network_mode,
        "CASESCREEN_RECOGNITION_NETWORK_INTERFACE": (
            "usb0" if network_mode == USB_GADGET_BRIDGE_MODE else ""
        ),
        "CASESCREEN_RECOGNITION_NETWORK_GATEWAY": (
            "192.168.77.1" if network_mode == USB_GADGET_BRIDGE_MODE else ""
        ),
        "CASESCREEN_RECOGNITION_FINGERPRINT_EXTRACTOR": (
            "/data/casescreen/fingerprint-extractor" if fingerprint_algorithm else ""
        ),
        "CASESCREEN_RECOGNITION_FINGERPRINT_ALGORITHM": fingerprint_algorithm,
        "CASESCREEN_RECOGNITION_FINGERPRINT_VERSION": fingerprint_version,
        "CASESCREEN_QQ_COOKIE_FILE": "",
        "CASESCREEN_NETEASE_COOKIE_FILE": "",
        "CASESCREEN_LYRICS_CACHE_MAX_FILES": str(MAX_LYRICS_CACHE_FILES),
        "CASESCREEN_LYRICS_CACHE_MAX_KIB": str(MAX_LYRICS_CACHE_KIB),
    }
    for key, value in values.items():
        if "\n" in value or "\r" in value or "\x00" in value:
            raise ValueError(f"识歌配置 {key} 含无效字符")
    return ("\n".join(f"{key}={value}" for key, value in values.items()) + "\n").encode("utf-8")


def make_launcher(theme_path: str, network_mode: str = "physical") -> bytes:
    theme_path = theme_path.removeprefix("/upper")
    if not theme_path.startswith("/data/theme/") or not theme_path.endswith(".Theme"):
        raise ValueError("launcher 主题路径无效")
    if network_mode not in NETWORK_MODES:
        raise ValueError("launcher 识歌网络模式无效")
    if network_mode == USB_GADGET_BRIDGE_MODE:
        route_guard = """# 专用桥模式只接受补丁器固定创建的 usb0 和 192.168.77.1 网关。
# 这是无需电脑的外置 USB Host/Wi-Fi 桥，不会把任意 RNDIS 路由误认成板载网络。
has_independent_default_route() {
    [ -r /proc/net/route ] || return 1
    while read -r interface destination gateway flags rest; do
        [ "$interface" = "usb0" ] || continue
        [ "$destination" = "00000000" ] || continue
        [ "$gateway" = "014DA8C0" ] || continue
        case "$flags" in ''|*[!0-9A-Fa-f]*) continue ;; esac
        LAST_FLAG=${flags#${flags%?}}
        case "$LAST_FLAG" in 2|3|6|7|A|B|E|F|a|b|e|f) ;; *) continue ;; esac
        [ -r /sys/class/net/usb0/operstate ] || continue
        IFS= read -r OPERSTATE < /sys/class/net/usb0/operstate || continue
        [ "$OPERSTATE" = "up" ] || continue
        return 0
    done < /proc/net/route
    return 1
}
"""
    else:
        route_guard = """# 只有默认路由真正落在已 up 的物理网卡上时才值得等待 NTP。
# USB Wi-Fi/普通 USB 以太网卡可以算独立网络，但 RNDIS/USB gadget
# 仍依赖另一台 USB Host，bridge/VLAN/TUN 等无 device 节点的虚拟接口也不算。
has_independent_default_route() {
    [ -r /proc/net/route ] || return 1
    while read -r interface destination gateway flags rest; do
        [ "$destination" = "00000000" ] || continue
        case "$flags" in ''|*[!0-9A-Fa-f]*) continue ;; esac
        LAST_FLAG=${flags#${flags%?}}
        case "$LAST_FLAG" in 2|3|6|7|A|B|E|F|a|b|e|f) ;; *) continue ;; esac
        case "$interface" in
            ''|lo|[Uu][Ss][Bb]*|[Rr][Nn][Dd][Ii][Ss]*|[Gg][Aa][Dd][Gg][Ee][Tt]*|*[!A-Za-z0-9_.:-]*) continue ;;
        esac
        [ -e "/sys/class/net/$interface/device" ] || continue
        IFS= read -r OPERSTATE < "/sys/class/net/$interface/operstate" || continue
        [ "$OPERSTATE" = "up" ] || continue
        DRIVER_LINK=$(readlink "/sys/class/net/$interface/device/driver" 2>/dev/null)
        DRIVER=${DRIVER_LINK##*/}
        case "$DRIVER" in
            *[Rr][Nn][Dd][Ii][Ss]*|*[Gg]_[Ee][Tt][Hh][Ee][Rr]*|*[Uu]_[Ee][Tt][Hh][Ee][Rr]*|*[Uu][Ss][Bb]_[Ff]*) continue ;;
        esac
        return 0
    done < /proc/net/route
    return 1
}
"""
    # 不 source 配置文件，避免 TF 上的字符串被 shell 当作代码执行；只读取固定白名单键。
    script = f"""#!/bin/sh
ORIGINAL=/data/PCMonitorLinux.original
EDGE=/data/casescreen-edge
CONFIG=/data/casescreen/recognition.env
CA=/data/casescreen/ca-bundle.pem
CACHE=/data/casescreen/lyrics-cache
REPORT=/data/casescreen/diagnostics/latest.json
NTP_VALID_MARKER=/var/state/dnsmasqsec
TIME_SYNC_MARKER=/run/casescreen/time-synchronized
TIME_SYNC_WAIT_SECONDS=60
RELAY_URL=
TOKEN_FILE=/data/casescreen/recognition.token
PCM_DEVICE=plughw:audiocodec
ARECORD=/usr/bin/arecord
CHUNK=1s
CAPTURE=10s
SEARCH_INTERVAL=2s
STEADY_INTERVAL=15s
RECOGNITION_TIMEOUT=12s
MIN_CONFIDENCE=0.78
CONFIRMATIONS=2
PAUSE_SILENT=2
RESUME_ACTIVE=2
PREFERRED_PLATFORM=auto
FINGERPRINT_EXTRACTOR=
FINGERPRINT_ALGORITHM=
FINGERPRINT_VERSION=
QQ_COOKIE_FILE=
NETEASE_COOKIE_FILE=
CACHE_MAX_FILES={MAX_LYRICS_CACHE_FILES}
CACHE_MAX_KIB={MAX_LYRICS_CACHE_KIB}

fallback() {{
    MESSAGE=$1
    shift
    printf '%s\\n' "CaseScreen standalone-recognition: $MESSAGE；恢复原厂程序" >&2
    exec "$ORIGINAL" "$@"
}}

[ -x "$ORIGINAL" ] || exit 127
[ -x "$EDGE" ] || fallback "识歌程序不可执行" "$@"

{route_guard}

# 原镜像的 /var 指向易失性 /tmp；原厂 NTP hotplug 只会在 ACTION=stratum
# 时创建 dnsmasqsec。/run 在该镜像中并非预置 tmpfs，所以每次 launcher
# 启动都先依据这个易失性来源清理或重建 marker，禁止沿用上次开机的文件。
prepare_time_marker() {{
    if [ ! -s "$NTP_VALID_MARKER" ]; then
        rm -f "$TIME_SYNC_MARKER"
        return 0
    fi
    TIME_SYNC_DIRECTORY=${{TIME_SYNC_MARKER%/*}}
    TIME_SYNC_TEMP="$TIME_SYNC_MARKER.tmp.$$"
    umask 077
    mkdir -p "$TIME_SYNC_DIRECTORY" || return 1
    printf '%s\\n' 'busybox-ntpd-stratum' > "$TIME_SYNC_TEMP" || {{ rm -f "$TIME_SYNC_TEMP"; return 1; }}
    chmod 0600 "$TIME_SYNC_TEMP" || {{ rm -f "$TIME_SYNC_TEMP"; return 1; }}
    mv -f "$TIME_SYNC_TEMP" "$TIME_SYNC_MARKER" || {{ rm -f "$TIME_SYNC_TEMP"; return 1; }}
}}

wait_for_time_marker_if_network_candidate() {{
    prepare_time_marker || printf '%s\\n' 'CaseScreen standalone-recognition: 无法更新可信时间 marker' >&2
    [ -s "$TIME_SYNC_MARKER" ] && return 0
    # 当前没有独立默认路由时不空等 60 秒，立即交由 edge 记录硬件原因并回退。
    has_independent_default_route || return 0
    TIME_SYNC_WAITED=0
    while [ "$TIME_SYNC_WAITED" -lt "$TIME_SYNC_WAIT_SECONDS" ] && [ ! -s "$TIME_SYNC_MARKER" ]; do
        sleep 1 || break
        # hotplug hook 会直接创建 marker；同时利用原厂易失性 marker 作为冗余通道。
        prepare_time_marker || printf '%s\\n' 'CaseScreen standalone-recognition: 无法更新可信时间 marker' >&2
        TIME_SYNC_WAITED=$((TIME_SYNC_WAITED + 1))
    done
}}

wait_for_time_marker_if_network_candidate

if [ -r "$CONFIG" ]; then
    while IFS='=' read -r key value; do
        case "$key" in
            CASESCREEN_RECOGNITION_RELAY_URL) RELAY_URL="$value" ;;
            CASESCREEN_RECOGNITION_TOKEN_FILE) TOKEN_FILE="$value" ;;
            CASESCREEN_RECOGNITION_PCM) PCM_DEVICE="$value" ;;
            CASESCREEN_RECOGNITION_ARECORD) ARECORD="$value" ;;
            CASESCREEN_RECOGNITION_CHUNK) CHUNK="$value" ;;
            CASESCREEN_RECOGNITION_CAPTURE) CAPTURE="$value" ;;
            CASESCREEN_RECOGNITION_SEARCH_INTERVAL) SEARCH_INTERVAL="$value" ;;
            CASESCREEN_RECOGNITION_STEADY_INTERVAL) STEADY_INTERVAL="$value" ;;
            CASESCREEN_RECOGNITION_TIMEOUT) RECOGNITION_TIMEOUT="$value" ;;
            CASESCREEN_RECOGNITION_MIN_CONFIDENCE) MIN_CONFIDENCE="$value" ;;
            CASESCREEN_RECOGNITION_CONFIRMATIONS) CONFIRMATIONS="$value" ;;
            CASESCREEN_RECOGNITION_PAUSE_SILENT) PAUSE_SILENT="$value" ;;
            CASESCREEN_RECOGNITION_RESUME_ACTIVE) RESUME_ACTIVE="$value" ;;
            CASESCREEN_RECOGNITION_PREFERRED_PLATFORM) PREFERRED_PLATFORM="$value" ;;
            CASESCREEN_RECOGNITION_FINGERPRINT_EXTRACTOR) FINGERPRINT_EXTRACTOR="$value" ;;
            CASESCREEN_RECOGNITION_FINGERPRINT_ALGORITHM) FINGERPRINT_ALGORITHM="$value" ;;
            CASESCREEN_RECOGNITION_FINGERPRINT_VERSION) FINGERPRINT_VERSION="$value" ;;
            CASESCREEN_QQ_COOKIE_FILE) QQ_COOKIE_FILE="$value" ;;
            CASESCREEN_NETEASE_COOKIE_FILE) NETEASE_COOKIE_FILE="$value" ;;
            CASESCREEN_LYRICS_CACHE_MAX_FILES) CACHE_MAX_FILES="$value" ;;
            CASESCREEN_LYRICS_CACHE_MAX_KIB) CACHE_MAX_KIB="$value" ;;
        esac
    done < "$CONFIG"
fi

# 配置允许把缓存上限调低，但不能绕过镜像规定的 256 文件 / 32 MiB 硬上限。
case "$CACHE_MAX_FILES" in ''|*[!0-9]*) CACHE_MAX_FILES={MAX_LYRICS_CACHE_FILES} ;; esac
case "$CACHE_MAX_KIB" in ''|*[!0-9]*) CACHE_MAX_KIB={MAX_LYRICS_CACHE_KIB} ;; esac
[ "$CACHE_MAX_FILES" -ge 1 ] 2>/dev/null && [ "$CACHE_MAX_FILES" -le {MAX_LYRICS_CACHE_FILES} ] 2>/dev/null || CACHE_MAX_FILES={MAX_LYRICS_CACHE_FILES}
[ "$CACHE_MAX_KIB" -ge 1 ] 2>/dev/null && [ "$CACHE_MAX_KIB" -le {MAX_LYRICS_CACHE_KIB} ] 2>/dev/null || CACHE_MAX_KIB={MAX_LYRICS_CACHE_KIB}

prune_cache() {{
    [ -d "$CACHE" ] || return 0
    while :; do
        CACHE_COUNT=$(find "$CACHE" -maxdepth 1 -type f -name '*.json' 2>/dev/null | wc -l)
        CACHE_SIZE=$(du -sk "$CACHE" 2>/dev/null | cut -f1)
        [ -n "$CACHE_SIZE" ] || CACHE_SIZE=0
        [ "$CACHE_COUNT" -le "$CACHE_MAX_FILES" ] 2>/dev/null && [ "$CACHE_SIZE" -le "$CACHE_MAX_KIB" ] 2>/dev/null && break
        OLDEST=$(ls -1tr "$CACHE"/*.json 2>/dev/null | head -n 1)
        case "$OLDEST" in "$CACHE"/*.json) rm -f "$OLDEST" ;; *) break ;; esac
    done
}}

prune_cache
(
    while :; do
        sleep 600
        prune_cache
    done
) &
JANITOR_PID=$!

run_edge() {{
    "$EDGE" \
        -metadata-mode recognition \
        -lyrics-mode direct \
        -recognition-config "$CONFIG" \
        -recognition-relay-url "$RELAY_URL" \
        -recognition-token-file "$TOKEN_FILE" \
        -recognition-pcm "$PCM_DEVICE" \
        -recognition-arecord "$ARECORD" \
        -recognition-chunk "$CHUNK" \
        -recognition-capture "$CAPTURE" \
        -recognition-search-interval "$SEARCH_INTERVAL" \
        -recognition-steady-interval "$STEADY_INTERVAL" \
        -recognition-timeout "$RECOGNITION_TIMEOUT" \
        -recognition-min-confidence "$MIN_CONFIDENCE" \
        -recognition-confirmations "$CONFIRMATIONS" \
        -recognition-pause-silent "$PAUSE_SILENT" \
        -recognition-resume-active "$RESUME_ACTIVE" \
        -recognition-preferred-platform "$PREFERRED_PLATFORM" \
        -ca-bundle "$CA" \
        -lyrics-cache "$CACHE" \
        -auth-dir /data/casescreen/platform-auth \
        -qq-cookie-file "$QQ_COOKIE_FILE" \
        -netease-cookie-file "$NETEASE_COOKIE_FILE" \
        -hardware-report "$REPORT" \
        -startup-theme {shlex.quote(theme_path)} \
        -program "$ORIGINAL" \
        "$@"
}}

if [ -n "$FINGERPRINT_EXTRACTOR" ]; then
    run_edge \
        -recognition-fingerprint-extractor "$FINGERPRINT_EXTRACTOR" \
        -recognition-fingerprint-algorithm "$FINGERPRINT_ALGORITHM" \
        -recognition-fingerprint-version "$FINGERPRINT_VERSION" \
        -- "$@"
else
    printf '%s\\n' "CaseScreen standalone-recognition: 未配置声纹提取器，将发送 10 秒 PCM JSON（约 427 KiB/次）" >&2
    run_edge -- "$@"
fi
STATUS=$?
kill "$JANITOR_PID" 2>/dev/null || true
wait "$JANITOR_PID" 2>/dev/null || true
printf '%s\\n' "CaseScreen standalone-recognition 退出码 $STATUS；恢复原厂程序" >&2
exec "$ORIGINAL" "$@"
"""
    return script.encode("utf-8")


def make_time_sync_hook() -> bytes:
    """仅把 BusyBox ntpd 的有效 stratum 事件转换为 CaseScreen 可信时间标记。"""
    return b"""#!/bin/sh
[ "$ACTION" = "stratum" ] || exit 0
case "$stratum" in
    ''|*[!0-9]*) exit 0 ;;
esac
[ "$stratum" -ge 1 ] 2>/dev/null && [ "$stratum" -lt 16 ] 2>/dev/null || exit 0

MARKER=/run/casescreen/time-synchronized
DIRECTORY=${MARKER%/*}
TEMP="$MARKER.tmp.$$"
umask 077
mkdir -p "$DIRECTORY" || exit 1
printf '%s\\n' 'busybox-ntpd-stratum' > "$TEMP" || { rm -f "$TEMP"; exit 1; }
chmod 0600 "$TEMP" || { rm -f "$TEMP"; exit 1; }
mv -f "$TEMP" "$MARKER" || { rm -f "$TEMP"; exit 1; }
"""


def make_serial_bridge_launcher() -> bytes:
    """让原厂 qt_app 在显示程序前先建立 ACM+RNDIS 专用桥链路。"""
    return b"#!/bin/sh\nexec /data/casescreen/usb-gadget-bridge.sh bridge-client\n"


def load_usb_gadget_bridge_script(path: Path = USB_GADGET_BRIDGE_SOURCE) -> bytes:
    data = read_limited(path, 128 * 1024, "USB Gadget 网络桥脚本")
    required = (
        b"#!/bin/sh",
        b"functions/acm.usb0",
        b"functions/rndis.usb0",
        b"bridge-client",
        b"192.168.77.1",
        b"192.168.77.2",
    )
    missing = [marker.decode("ascii") for marker in required if marker not in data]
    if missing or b"\r\n" in data:
        raise ValueError("USB Gadget 网络桥脚本无效或不是 LF/UTF-8 文本: " + ", ".join(missing))
    return data if data.endswith(b"\n") else data + b"\n"


def ensure_safe_work_base(path: Path) -> Path:
    resolved = path.resolve()
    text = str(resolved)
    if any(ord(character) > 127 or character.isspace() for character in text):
        raise ValueError(
            "旧版 Cygwin e2fsprogs 的工作目录不能包含中文或空格；"
            "请使用 --work-dir C:\\casescreen-work 之类的 ASCII 路径"
        )
    resolved.mkdir(parents=True, exist_ok=True)
    return resolved


def cygwin_argument(path: Path) -> str:
    converted = to_cygwin_path(path)
    if not SAFE_CYGWIN_PATH.fullmatch(converted):
        raise ValueError(f"debugfs 本地路径含不安全字符: {path}")
    return converted


def ext_path_argument(path: str) -> str:
    if not SAFE_CYGWIN_PATH.fullmatch(path):
        raise ValueError(f"debugfs 镜像内路径含不安全字符: {path}")
    return path


def run_external(
    command: list[str],
    *,
    tool_directory: Path,
    allowed_returncodes: tuple[int, ...] = (0,),
) -> subprocess.CompletedProcess[str]:
    environment = os.environ.copy()
    environment["PATH"] = str(tool_directory) + os.pathsep + environment.get("PATH", "")
    environment["CYGWIN"] = "nodosfilewarning"
    completed = subprocess.run(
        command,
        cwd=tool_directory,
        env=environment,
        text=True,
        encoding="utf-8",
        errors="replace",
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if completed.returncode not in allowed_returncodes:
        detail = (completed.stdout + "\n" + completed.stderr).strip()
        if len(detail) > 4000:
            detail = detail[-4000:]
        raise ExternalToolError(
            f"外部工具失败（退出码 {completed.returncode}）: {Path(command[0]).name}\n{detail}"
        )
    return completed


def stage_e2fsprogs(debugfs: Path, e2fsck: Path, destination: Path) -> tuple[Path, Path]:
    for tool in (debugfs, e2fsck):
        if not tool.is_file():
            raise FileNotFoundError(f"找不到 e2fsprogs 工具: {tool}")
    destination.mkdir(parents=True, exist_ok=False)
    copied: dict[str, Path] = {}
    for source_directory in {debugfs.parent.resolve(), e2fsck.parent.resolve()}:
        for source in source_directory.iterdir():
            if not source.is_file() or source.suffix.lower() not in (".exe", ".dll"):
                continue
            target = destination / source.name
            if target.name.lower() in copied:
                if source.read_bytes() != copied[target.name.lower()].read_bytes():
                    raise ValueError(f"e2fsprogs 依赖文件冲突: {source.name}")
                continue
            shutil.copy2(source, target)
            copied[target.name.lower()] = target
    staged_debugfs = destination / debugfs.name
    staged_e2fsck = destination / e2fsck.name
    if not staged_debugfs.is_file() or not staged_e2fsck.is_file():
        raise RuntimeError("复制 e2fsprogs 工具失败")
    return staged_debugfs, staged_e2fsck


class DebugFs:
    def __init__(self, executable: Path, filesystem: Path, tool_directory: Path, command_directory: Path):
        self.executable = executable
        self.filesystem = filesystem
        self.tool_directory = tool_directory
        self.command_directory = command_directory

    def _run(self, arguments: list[str]) -> subprocess.CompletedProcess[str]:
        return run_external(
            [str(self.executable), *arguments, cygwin_argument(self.filesystem)],
            tool_directory=self.tool_directory,
        )

    def stat(self, path: str) -> Optional[FsStat]:
        path = ext_path_argument(path)
        completed = self._run(["-R", f"stat {path}"])
        output = completed.stdout + "\n" + completed.stderr
        match = re.search(r"Type:\s+(\S+)\s+Mode:\s+0*([0-7]+)", output)
        if match:
            return FsStat(match.group(1).lower(), int(match.group(2), 8))
        missing_markers = ("File not found", "not found by ext2_lookup")
        if any(marker in output for marker in missing_markers):
            return None
        raise ExternalToolError(f"debugfs 无法解析 {path} 的 stat 输出\n{output.strip()[-2000:]}")

    def dump(self, source: str, destination: Path, *, required: bool = True) -> bool:
        source = ext_path_argument(source)
        if destination.exists():
            destination.unlink()
        completed = self._run(["-R", f"dump -p {source} {cygwin_argument(destination)}"])
        if destination.is_file():
            return True
        output = completed.stdout + "\n" + completed.stderr
        if not required and ("File not found" in output or "not found by ext2_lookup" in output):
            return False
        raise ExternalToolError(f"debugfs 无法导出 {source}\n{output.strip()[-2000:]}")

    def apply(self, commands: Iterable[str]) -> None:
        lines = list(commands)
        for line in lines:
            if "\r" in line or "\n" in line:
                raise ValueError("debugfs 命令不能包含换行符")
        fatal_markers = (
            "Command not found",
            "Filesystem not open",
            "while opening",
            "Could not allocate",
            "Ext2 file already exists",
            "File not found by ext2_lookup",
        )
        for index, line in enumerate(lines):
            if line.startswith("write "):
                parts = line.split(" ")
                if len(parts) != 3:
                    raise ValueError("debugfs write 命令格式无效")
                local_path, destination = parts[1], ext_path_argument(parts[2])
                parent, separator, basename = destination.rpartition("/")
                if separator == "" or not basename:
                    raise ValueError("debugfs write 目标路径无效")
                parent = parent or "/"
                # 旧版 debugfs 直接写绝对目标时可能不创建目录项，因此先切换目录再写文件名。
                command_file = self.command_directory / f"debugfs-write-{index}.txt"
                with command_file.open("w", encoding="ascii", newline="\n") as stream:
                    stream.write(f"cd {parent}\nwrite {local_path} {basename}\n")
                completed = self._run(["-w", "-f", cygwin_argument(command_file)])
            else:
                completed = self._run(["-w", "-R", line])
            output = completed.stdout + "\n" + completed.stderr
            if any(marker in output for marker in fatal_markers):
                raise ExternalToolError(
                    f"debugfs 补丁命令失败: {line.split(' ', 1)[0]}\n{output.strip()[-4000:]}"
                )


def require_kind(debugfs: DebugFs, path: str, kind: str) -> FsStat:
    stat = debugfs.stat(path)
    if stat is None:
        raise ValueError(f"镜像内缺少必需路径: {path}")
    if stat.kind != kind:
        raise ValueError(f"镜像内路径类型错误: {path} 实际为 {stat.kind}")
    return stat


def reject_unsupported_runtime(debugfs: DebugFs) -> None:
    found = [path for path in UNSUPPORTED_RUNTIME_PATHS if debugfs.stat(path) is not None]
    if found:
        raise ValueError(
            "输入镜像包含已取消的 CaseScreen 屏端运行链，拒绝继续复制；"
            f"请改用干净的原厂扩容镜像。发现: {', '.join(found)}"
        )


def collect_original_monitor(debugfs: DebugFs, staging: Path) -> Path:
    stat = require_kind(debugfs, MONITOR_PATH, "regular")
    if stat.permissions & 0o111 == 0:
        raise ValueError("镜像内原厂 PCMonitorLinux 不可执行")
    destination = staging / "PCMonitorLinux.original"
    debugfs.dump(MONITOR_PATH, destination)
    validate_original_monitor(destination.read_bytes())
    os.chmod(destination, 0o755)
    return destination


def collect_original_serial_script(debugfs: DebugFs, staging: Path) -> Path:
    stat = require_kind(debugfs, SERIAL_SCRIPT_PATH, "regular")
    if stat.permissions & 0o111 == 0:
        raise ValueError("镜像内原厂 serial_uart.sh 不可执行")
    destination = staging / "serial_uart.sh.original"
    debugfs.dump(SERIAL_SCRIPT_PATH, destination)
    data = read_limited(destination, 128 * 1024, "原厂 serial_uart.sh")
    if not data.startswith(b"#!/bin/sh") or b"functions/acm.usb0" not in data or b"UDC" not in data:
        raise ValueError("镜像内原厂 serial_uart.sh 内容不符合预期")
    os.chmod(destination, 0o755)
    return destination


def prepare_runtime_directories(debugfs: DebugFs) -> list[str]:
    commands: list[str] = []
    directories = (
        ("/upper/etc", 0o755),
        ("/upper/etc/hotplug.d", 0o755),
        ("/upper/etc/hotplug.d/ntp", 0o755),
        (CASESCREEN_DIRECTORY, 0o700),
        (DIAGNOSTICS_DIRECTORY, 0o700),
        (LYRICS_CACHE_DIRECTORY, 0o700),
        (PLATFORM_AUTH_DIRECTORY, 0o700),
        (IMPORT_DIRECTORY, 0o700),
    )
    require_kind(debugfs, "/upper/data", "directory")
    for path, permissions in directories:
        stat = debugfs.stat(path)
        if stat is None:
            commands.append(f"mkdir {path}")
        elif stat.kind != "directory":
            raise ValueError(f"镜像内运行目录路径类型错误: {path}")
        commands.extend(
            (
                f"set_inode_field {path} mode 040{permissions:03o}",
                f"set_inode_field {path} uid 0",
                f"set_inode_field {path} gid 0",
            )
        )
    return commands


def payload_write_commands(local: Path, destination: str, permissions: int) -> list[str]:
    destination = ext_path_argument(destination)
    return [
        f"write {cygwin_argument(local)} {destination}",
        f"set_inode_field {destination} mode 0100{permissions:03o}",
        f"set_inode_field {destination} uid 0",
        f"set_inode_field {destination} gid 0",
    ]


def build_runtime_commands(payloads: RuntimePayloads) -> list[str]:
    bridge_payloads = (
        payloads.serial_launcher,
        payloads.original_serial_script,
        payloads.usb_gadget_bridge,
    )
    if any(item is not None for item in bridge_payloads) and not all(
        item is not None for item in bridge_payloads
    ):
        raise ValueError("USB Gadget 网络桥的启动器、原厂备份与实现脚本必须同时提供")
    commands = [f"rm {MONITOR_PATH}"]
    commands.extend(payload_write_commands(payloads.original_monitor, ORIGINAL_MONITOR_PATH, 0o755))
    commands.extend(payload_write_commands(payloads.launcher, MONITOR_PATH, 0o755))
    commands.extend(payload_write_commands(payloads.edge, EDGE_PATH, 0o755))
    commands.extend(payload_write_commands(payloads.config, CONFIG_PATH, 0o600))
    commands.extend(payload_write_commands(payloads.token, TOKEN_PATH, 0o600))
    commands.extend(payload_write_commands(payloads.ca_bundle, CA_PATH, 0o644))
    commands.extend(payload_write_commands(payloads.time_sync_hook, TIME_SYNC_HOOK_PATH, 0o755))
    if payloads.serial_launcher is not None:
        commands.append(f"rm {SERIAL_SCRIPT_PATH}")
        commands.extend(
            payload_write_commands(payloads.original_serial_script, ORIGINAL_SERIAL_SCRIPT_PATH, 0o755)
        )
        commands.extend(payload_write_commands(payloads.serial_launcher, SERIAL_SCRIPT_PATH, 0o755))
        commands.extend(payload_write_commands(payloads.usb_gadget_bridge, USB_GADGET_BRIDGE_PATH, 0o755))
    if payloads.fingerprint_extractor is not None:
        commands.extend(payload_write_commands(payloads.fingerprint_extractor, EXTRACTOR_PATH, 0o755))
    if payloads.fingerprint_library is not None:
        commands.extend(payload_write_commands(payloads.fingerprint_library, FINGERPRINT_LIBRARY_PATH, 0o644))
    commands.extend(build_theme_commands(payloads.theme, payloads.theme_destination))
    return commands


def verify_runtime(debugfs: DebugFs, payloads: RuntimePayloads, resolution: str, staging: Path) -> None:
    expected_directories = {
        CASESCREEN_DIRECTORY: 0o700,
        DIAGNOSTICS_DIRECTORY: 0o700,
        LYRICS_CACHE_DIRECTORY: 0o700,
        PLATFORM_AUTH_DIRECTORY: 0o700,
        IMPORT_DIRECTORY: 0o700,
        "/upper/etc/hotplug.d/ntp": 0o755,
    }
    for path, permissions in expected_directories.items():
        stat = require_kind(debugfs, path, "directory")
        if stat.permissions != permissions:
            raise ValueError(f"{path} 权限错误：实际 {stat.permissions:04o}，预期 {permissions:04o}")
    expected_files = (
        (MONITOR_PATH, payloads.launcher, 0o755),
        (ORIGINAL_MONITOR_PATH, payloads.original_monitor, 0o755),
        (EDGE_PATH, payloads.edge, 0o755),
        (CONFIG_PATH, payloads.config, 0o600),
        (TOKEN_PATH, payloads.token, 0o600),
        (CA_PATH, payloads.ca_bundle, 0o644),
        (TIME_SYNC_HOOK_PATH, payloads.time_sync_hook, 0o755),
    )
    for destination, local, permissions in expected_files:
        assert_payload(debugfs, destination, local.read_bytes(), permissions, staging)
    bridge_payloads = (
        payloads.serial_launcher,
        payloads.original_serial_script,
        payloads.usb_gadget_bridge,
    )
    if any(item is not None for item in bridge_payloads) and not all(
        item is not None for item in bridge_payloads
    ):
        raise ValueError("USB Gadget 网络桥校验载荷不完整")
    if payloads.serial_launcher is not None:
        assert_payload(debugfs, SERIAL_SCRIPT_PATH, payloads.serial_launcher.read_bytes(), 0o755, staging)
        assert_payload(
            debugfs,
            ORIGINAL_SERIAL_SCRIPT_PATH,
            payloads.original_serial_script.read_bytes(),
            0o755,
            staging,
        )
        assert_payload(
            debugfs,
            USB_GADGET_BRIDGE_PATH,
            payloads.usb_gadget_bridge.read_bytes(),
            0o755,
            staging,
        )
    else:
        for path in (ORIGINAL_SERIAL_SCRIPT_PATH, USB_GADGET_BRIDGE_PATH):
            if debugfs.stat(path) is not None:
                raise ValueError(f"物理网络模式不应注入 USB Gadget 网络桥文件: {path}")
    if payloads.fingerprint_extractor is not None:
        assert_payload(debugfs, EXTRACTOR_PATH, payloads.fingerprint_extractor.read_bytes(), 0o755, staging)
    elif debugfs.stat(EXTRACTOR_PATH) is not None:
        raise ValueError("未配置声纹提取器时镜像内不应出现提取器文件")
    if payloads.fingerprint_library is not None:
        assert_payload(
            debugfs,
            FINGERPRINT_LIBRARY_PATH,
            payloads.fingerprint_library.read_bytes(),
            0o644,
            staging,
        )
    elif debugfs.stat(FINGERPRINT_LIBRARY_PATH) is not None:
        raise ValueError("未配置声纹共享库时镜像内不应出现共享库文件")
    verify_standalone_theme(debugfs, payloads.theme, payloads.theme_destination, resolution, staging)


def prepare_standalone_theme_path(debugfs: DebugFs, resolution: Optional[str]) -> list[str]:
    """准备驻留主题目录，并只清理补丁器拥有的同名旧主题。"""
    if resolution is None:
        return []
    if resolution not in THEME_RESOLUTIONS:
        raise ValueError("主题分辨率必须是 1024x600 或 480x1920")

    commands: list[str] = []
    for path in ("/upper/data/theme", f"/upper/data/theme/{resolution}"):
        stat = debugfs.stat(path)
        if stat is None:
            commands.append(f"mkdir {path}")
        elif stat.kind != "directory":
            raise ValueError(f"镜像内主题路径不是目录: {path}")
        commands.extend(
            (
                f"set_inode_field {path} mode 040755",
                f"set_inode_field {path} uid 0",
                f"set_inode_field {path} gid 0",
            )
        )
    for candidate in THEME_RESOLUTIONS:
        path = standalone_theme_path(candidate, overlay=True)
        stat = debugfs.stat(path)
        if stat is None:
            continue
        if stat.kind != "regular":
            raise ValueError(f"镜像内驻留主题路径不是普通文件: {path}")
        commands.append(f"rm {path}")
    return commands


def build_theme_commands(theme: Optional[Path], destination: Optional[str]) -> list[str]:
    if (theme is None) != (destination is None):
        raise ValueError("驻留歌词主题与镜像目标路径必须同时提供")
    if theme is None or destination is None:
        return []
    destination = ext_path_argument(destination)
    return [
        f"write {cygwin_argument(theme)} {destination}",
        f"set_inode_field {destination} mode 0100644",
        f"set_inode_field {destination} uid 0",
        f"set_inode_field {destination} gid 0",
    ]


def assert_payload(debugfs: DebugFs, path: str, expected: bytes, permissions: int, staging: Path) -> None:
    stat = require_kind(debugfs, path, "regular")
    if stat.permissions != permissions:
        raise ValueError(
            f"{path} 权限错误：实际 {stat.permissions:04o}，预期 {permissions:04o}"
        )
    destination = staging / ("verify-" + hashlib.sha256(path.encode("ascii")).hexdigest()[:16])
    debugfs.dump(path, destination)
    actual = destination.read_bytes()
    if actual != expected:
        raise ValueError(f"镜像内文件内容校验失败: {path}")


def verify_standalone_theme(
    debugfs: DebugFs,
    theme: Optional[Path],
    destination: Optional[str],
    resolution: Optional[str],
    staging: Path,
) -> None:
    if (theme is None) != (destination is None) or (theme is None) != (resolution is None):
        raise ValueError("驻留歌词主题、目标路径与分辨率必须同时校验")
    if theme is None or destination is None or resolution is None:
        return
    for path in ("/upper/data/theme", f"/upper/data/theme/{resolution}"):
        stat = require_kind(debugfs, path, "directory")
        if stat.permissions != 0o755:
            raise ValueError(f"{path} 权限错误：实际 {stat.permissions:04o}，预期 0755")
    assert_payload(debugfs, destination, theme.read_bytes(), 0o644, staging)


def run_e2fsck(e2fsck: Path, partition: Path, tool_directory: Path, label: str) -> None:
    print(f"[检查] {label}")
    run_external(
        [str(e2fsck), "-f", "-n", cygwin_argument(partition)],
        tool_directory=tool_directory,
    )


def validate_waveshare_layout(partitions: list[PartitionEntry], partition_index: int) -> PartitionEntry:
    names = tuple(partition.name for partition in partitions)
    if names != EXPECTED_PARTITIONS:
        raise ValueError(f"不是预期的微雪 7/8.8 英寸 GPT 布局: {names!r}")
    selected = next((entry for entry in partitions if entry.index == partition_index), None)
    if selected is None or selected.name != "UDISK" or selected.index != 6:
        raise ValueError("只允许补丁第 6 个 UDISK 分区")
    ordered = sorted(partitions, key=lambda entry: entry.first_lba)
    for left, right in zip(ordered, ordered[1:]):
        if left.first_lba > left.last_lba or left.last_lba >= right.first_lba:
            raise ValueError("GPT 分区范围异常或重叠")
    if selected != ordered[-1]:
        raise ValueError("UDISK 不是最后一个 GPT 分区")
    return selected


def copy_whole_file(source: Path, destination: Path, chunk_size: int) -> None:
    with source.open("rb") as source_stream, destination.open("xb") as destination_stream:
        shutil.copyfileobj(source_stream, destination_stream, length=chunk_size)
        destination_stream.flush()
        os.fsync(destination_stream.fileno())


def write_payload(path: Path, data: bytes, permissions: int) -> None:
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(path, permissions)


def compare_outside_partition(
    source: Path,
    target: Path,
    partition: PartitionEntry,
    chunk_size: int,
) -> None:
    if source.stat().st_size != target.stat().st_size:
        raise ValueError("输出镜像大小与输入不同")
    partition_start = partition.first_lba * SECTOR_SIZE
    partition_end = (partition.last_lba + 1) * SECTOR_SIZE
    prefix_source = sha256_range(source, 0, partition_start, chunk_size)
    prefix_target = sha256_range(target, 0, partition_start, chunk_size)
    if prefix_source != prefix_target:
        raise ValueError("UDISK 之前的数据发生变化")
    suffix_size = source.stat().st_size - partition_end
    suffix_source = sha256_range(source, partition_end, suffix_size, chunk_size)
    suffix_target = sha256_range(target, partition_end, suffix_size, chunk_size)
    if suffix_source != suffix_target:
        raise ValueError("UDISK 之后的 GPT/数据发生变化")


def atomic_commit_no_replace(partial: Path, target: Path) -> None:
    if target.exists():
        raise FileExistsError(f"目标文件已存在: {target}")
    if os.name == "nt":
        # Windows os.rename 不会覆盖已存在目标，可防止并发覆盖。
        os.rename(partial, target)
        return
    os.link(partial, target)
    partial.unlink()


def reserve_partial_path(target: Path) -> Path:
    return target.parent / f".{target.name}.partial-{uuid.uuid4().hex}"


def check_available_space(source: Path, target_parent: Path, work_base: Path, partition_size: int) -> None:
    margin = 128 * 1024 * 1024
    target_volume = target_parent.drive.lower() or target_parent.anchor.lower()
    work_volume = work_base.drive.lower() or work_base.anchor.lower()
    if target_volume == work_volume:
        required = source.stat().st_size + partition_size + margin
        if shutil.disk_usage(target_parent).free < required:
            raise OSError(f"磁盘空间不足：输出镜像与临时分区共需 {required} 字节")
    else:
        target_required = source.stat().st_size + margin
        work_required = partition_size + margin
        if shutil.disk_usage(target_parent).free < target_required:
            raise OSError(f"输出盘空间不足，至少需要 {target_required} 字节")
        if shutil.disk_usage(work_base).free < work_required:
            raise OSError(f"工作盘空间不足，至少需要 {work_required} 字节")


def patch_image(args: argparse.Namespace) -> None:
    source = args.source.resolve()
    target = args.target.resolve()
    debugfs_source = args.debugfs.resolve()
    e2fsck_source = args.e2fsck.resolve()
    edge_binary = args.edge_binary.resolve()
    device_token_file = args.device_token_file.resolve()
    ca_bundle = args.ca_bundle.resolve()
    standalone_theme = args.standalone_theme.resolve()
    theme_resolution = args.theme_resolution
    chunk_size = args.chunk_mib * 1024 * 1024
    relay_url, pcm_device, arecord_path, network_mode = validate_runtime_options(args)
    (
        fingerprint_extractor,
        fingerprint_library,
        fingerprint_algorithm,
        fingerprint_version,
    ) = validate_fingerprint_options(args)

    if not source.is_file():
        raise FileNotFoundError(f"找不到输入镜像: {source}")
    source_stat = source.stat()
    source_identity = source_stat.st_size, source_stat.st_mtime_ns, source_stat.st_ino
    if source == target:
        raise ValueError("输出镜像不能与输入镜像相同")
    if target.exists():
        raise FileExistsError(f"拒绝覆盖已存在的输出: {target}")
    if args.chunk_mib < 1 or args.chunk_mib > 256:
        raise ValueError("--chunk-mib 必须在 1..256 之间")
    if args.partition_index != 6:
        raise ValueError("此工具只允许补丁第 6 个 UDISK 分区")
    required_inputs = (
        (edge_binary, "casescreen-edge ARM32 程序"),
        (device_token_file, "设备令牌"),
        (ca_bundle, "CA 根证书包"),
        (standalone_theme, "驻留歌词主题"),
    )
    for path, description in required_inputs:
        if not path.is_file():
            raise FileNotFoundError(f"找不到{description}: {path}")
    if fingerprint_extractor is not None and not fingerprint_extractor.is_file():
        raise FileNotFoundError(f"找不到 ARM32 声纹提取器: {fingerprint_extractor}")
    if fingerprint_library is not None and not fingerprint_library.is_file():
        raise FileNotFoundError(f"找不到 ARM32 声纹共享库: {fingerprint_library}")
    if theme_resolution not in THEME_RESOLUTIONS:
        raise ValueError("--theme-resolution 必须是 1024x600 或 480x1920")

    edge_bytes = validate_edge_binary(edge_binary)
    fingerprint_bytes = (
        validate_fingerprint_extractor(fingerprint_extractor)
        if fingerprint_extractor is not None
        else None
    )
    fingerprint_library_bytes = (
        validate_fingerprint_library(fingerprint_library)
        if fingerprint_library is not None
        else None
    )
    validate_fingerprint_payload_pair(fingerprint_bytes, fingerprint_library_bytes)
    token_bytes = validate_device_token(device_token_file)
    ca_bytes = validate_ca_bundle(ca_bundle)
    standalone_theme_bytes = validate_standalone_theme(standalone_theme)

    target.parent.mkdir(parents=True, exist_ok=True)
    work_base = ensure_safe_work_base(args.work_dir if args.work_dir else Path(tempfile.gettempdir()))
    with source.open("rb") as stream:
        layout = parse_primary_gpt(stream)
        partitions = list_partitions(layout)
    selected = validate_waveshare_layout(partitions, args.partition_index)
    if verify_gpt(source, args.partition_index) != selected:
        raise ValueError("输入镜像 GPT 二次校验结果不一致")
    partition_size = (selected.last_lba - selected.first_lba + 1) * SECTOR_SIZE
    if (selected.last_lba + 1) * SECTOR_SIZE > source.stat().st_size:
        raise ValueError("UDISK 分区超出镜像大小")
    check_available_space(source, target.parent, work_base, partition_size)

    work_root = Path(tempfile.mkdtemp(prefix="casescreen-standalone-recognition-", dir=work_base))
    partial_output = reserve_partial_path(target)
    image_committed = False
    try:
        tool_directory = work_root / "e2fsprogs"
        debugfs, e2fsck = stage_e2fsprogs(debugfs_source, e2fsck_source, tool_directory)
        staging = work_root / "payload"
        staging.mkdir()
        payload_edge = staging / "casescreen-edge"
        payload_token = staging / "recognition.token"
        payload_ca = staging / "ca-bundle.pem"
        payload_config = staging / "recognition.env"
        payload_launcher = staging / "PCMonitorLinux"
        payload_time_sync_hook = staging / "90-casescreen-time"
        payload_theme = staging / STANDALONE_THEME_NAME
        payload_extractor: Optional[Path] = None
        payload_fingerprint_library: Optional[Path] = None
        payload_serial_launcher: Optional[Path] = None
        payload_usb_gadget_bridge: Optional[Path] = None
        theme_destination = standalone_theme_path(theme_resolution, overlay=True)
        write_payload(payload_edge, edge_bytes, 0o755)
        write_payload(payload_token, token_bytes, 0o600)
        write_payload(payload_ca, ca_bytes, 0o644)
        write_payload(
            payload_config,
            make_recognition_config(
                relay_url,
                pcm_device,
                arecord_path,
                fingerprint_algorithm,
                fingerprint_version,
                network_mode,
            ),
            0o600,
        )
        write_payload(payload_launcher, make_launcher(theme_destination, network_mode), 0o755)
        write_payload(payload_time_sync_hook, make_time_sync_hook(), 0o755)
        write_payload(payload_theme, standalone_theme_bytes, 0o644)
        if fingerprint_bytes is not None:
            payload_extractor = staging / "fingerprint-extractor"
            write_payload(payload_extractor, fingerprint_bytes, 0o755)
        if fingerprint_library_bytes is not None:
            payload_fingerprint_library = staging / "libacrcloud_extr_tool.so"
            write_payload(payload_fingerprint_library, fingerprint_library_bytes, 0o644)
        if network_mode == USB_GADGET_BRIDGE_MODE:
            payload_serial_launcher = staging / "serial_uart.sh"
            payload_usb_gadget_bridge = staging / "usb-gadget-bridge.sh"
            write_payload(payload_serial_launcher, make_serial_bridge_launcher(), 0o755)
            write_payload(payload_usb_gadget_bridge, load_usb_gadget_bridge_script(), 0o755)

        partition = work_root / "udisk-p6.ext4"
        print(f"[提取] 第 6 分区 UDISK：{partition_size} 字节")
        with source.open("rb") as image_stream, partition.open("xb") as partition_stream:
            copy_range(
                image_stream,
                partition_stream,
                selected.first_lba * SECTOR_SIZE,
                partition_size,
                chunk_size,
            )
            partition_stream.flush()
            os.fsync(partition_stream.fileno())
        run_e2fsck(e2fsck, partition, tool_directory, "写入前 ext4（只读）")

        fs = DebugFs(debugfs, partition, tool_directory, staging)
        reject_unsupported_runtime(fs)
        original_monitor = collect_original_monitor(fs, staging)
        original_serial_script = (
            collect_original_serial_script(fs, staging)
            if network_mode == USB_GADGET_BRIDGE_MODE
            else None
        )
        payloads = RuntimePayloads(
            edge=payload_edge,
            launcher=payload_launcher,
            original_monitor=original_monitor,
            config=payload_config,
            token=payload_token,
            ca_bundle=payload_ca,
            time_sync_hook=payload_time_sync_hook,
            theme=payload_theme,
            theme_destination=theme_destination,
            fingerprint_extractor=payload_extractor,
            fingerprint_library=payload_fingerprint_library,
            serial_launcher=payload_serial_launcher,
            original_serial_script=original_serial_script,
            usb_gadget_bridge=payload_usb_gadget_bridge,
        )
        commands = prepare_runtime_directories(fs)
        commands.extend(prepare_standalone_theme_path(fs, theme_resolution))
        commands.extend(build_runtime_commands(payloads))
        fs.apply(commands)
        run_e2fsck(e2fsck, partition, tool_directory, "写入后 ext4（只读）")
        verify_runtime(fs, payloads, theme_resolution, staging)
        expected_partition_sha256 = sha256_range(partition, 0, partition_size, chunk_size)

        print(f"[复制] {source.name} -> 临时输出")
        copy_whole_file(source, partial_output, chunk_size)
        print("[回写] standalone-recognition 屏端运行链")
        with partition.open("rb") as partition_stream, partial_output.open("r+b") as image_stream:
            image_stream.seek(selected.first_lba * SECTOR_SIZE)
            copy_range(partition_stream, image_stream, 0, partition_size, chunk_size)
            image_stream.flush()
            os.fsync(image_stream.fileno())
        partition.unlink()

        if verify_gpt(partial_output, args.partition_index) != selected:
            raise ValueError("输出镜像 GPT 校验结果不一致")
        compare_outside_partition(source, partial_output, selected, chunk_size)

        final_partition = work_root / "final-udisk-p6.ext4"
        print("[复验] 从新整盘镜像重新提取 UDISK")
        with partial_output.open("rb") as image_stream, final_partition.open("xb") as partition_stream:
            copy_range(
                image_stream,
                partition_stream,
                selected.first_lba * SECTOR_SIZE,
                partition_size,
                chunk_size,
            )
            partition_stream.flush()
            os.fsync(partition_stream.fileno())
        if sha256_range(final_partition, 0, partition_size, chunk_size) != expected_partition_sha256:
            raise ValueError("输出镜像 UDISK 分区 SHA-256 不一致")
        run_e2fsck(e2fsck, final_partition, tool_directory, "最终镜像 ext4（只读）")
        final_fs = DebugFs(debugfs, final_partition, tool_directory, staging)
        verify_runtime(final_fs, payloads, theme_resolution, staging)
        final_partition.unlink()

        current_source_stat = source.stat()
        current_source_identity = (
            current_source_stat.st_size,
            current_source_stat.st_mtime_ns,
            current_source_stat.st_ino,
        )
        if current_source_identity != source_identity:
            raise ValueError("补丁期间输入镜像发生变化，已拒绝提交输出")
        image_sha256 = sha256_range(partial_output, 0, partial_output.stat().st_size, chunk_size)
        atomic_commit_no_replace(partial_output, target)
        image_committed = True
        print(f"[完成] {target}")
        print(f"[SHA-256] {image_sha256}  {target.name}")
        print(
            "[模式] standalone-recognition；"
            f"network={network_mode}；原厂程序保留为 /data/PCMonitorLinux.original"
        )
        print("[令牌] 镜像只保存可撤销的每屏设备令牌，不包含第三方识歌服务主密钥")
        if payload_extractor is None:
            print("[带宽警告] 未注入 ARM32 声纹提取器，将发送 10 秒 PCM JSON（约 427 KiB/次）")
        else:
            print(f"[声纹] 已注入 ARM32 提取器：{fingerprint_algorithm}/{fingerprint_version}")
            if payload_fingerprint_library is not None:
                print("[声纹库] 已注入用户提供的 ARM32 共享库；许可与账户条款仍由部署者负责")
    except BaseException:
        if partial_output.exists():
            partial_output.unlink()
        raise
    finally:
        if work_root.exists():
            shutil.rmtree(work_root)
        if not image_committed and partial_output.exists():
            partial_output.unlink()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "验证已扩容的微雪 7/8.8 英寸整盘 IMG，并注入明确的 standalone-recognition 运行链；"
            "保留原厂显示程序作为失败回退，不修改输入、不覆盖输出。"
        )
    )
    parser.add_argument("source", type=Path, help="已扩容的 7/8.8 英寸整盘 IMG（只读）")
    parser.add_argument("target", type=Path, help="新输出 IMG（必须不存在）")
    parser.add_argument("--partition-index", type=int, default=6, help="固定为第 6 个 UDISK 分区")
    parser.add_argument("--debugfs", type=Path, default=DEFAULT_DEBUGFS)
    parser.add_argument("--e2fsck", type=Path, default=DEFAULT_E2FSCK)
    parser.add_argument(
        "--runtime-mode",
        required=True,
        choices=(RUNTIME_MODE,),
        help="必须明确指定 standalone-recognition，旧主机 relay/source-push 模式不再受支持",
    )
    parser.add_argument(
        "--network-mode",
        choices=NETWORK_MODES,
        default="physical",
        help=(
            "physical 要求板载/独立物理网卡；usb-gadget-bridge 会把原 ACM 扩展为 ACM+RNDIS，"
            "接无需电脑的专用 USB Host/Wi-Fi 桥"
        ),
    )
    parser.add_argument("--edge-binary", required=True, type=Path, help="当前源码构建的 Linux ARM32 casescreen-edge")
    parser.add_argument(
        "--recognition-relay-url",
        required=True,
        help="公开识歌 relay 的 HTTPS URL；不得包含第三方主密钥或 URL 用户信息",
    )
    parser.add_argument(
        "--device-token-file",
        required=True,
        type=Path,
        help="每块屏独有、可撤销的 64 位十六进制设备令牌文件",
    )
    parser.add_argument("--ca-bundle", required=True, type=Path, help="屏端 HTTPS 使用的 PEM CA 根证书包")
    parser.add_argument(
        "--recognition-pcm",
        default="plughw:audiocodec",
        help="屏端 ALSA PCM 采集设备；默认经 plug 把 audiocodec 的 3 声道采集转换为单声道",
    )
    parser.add_argument(
        "--recognition-arecord",
        default="/usr/bin/arecord",
        help="屏端 arecord 绝对路径",
    )
    parser.add_argument(
        "--fingerprint-extractor",
        type=Path,
        help=(
            "可选 ARM32 声纹提取器；stdin 接收 16kHz/单声道/S16_LE 窗口，stdout 最大 64KiB。"
            "未提供时明确使用约 427KiB/次的 PCM JSON"
        ),
    )
    parser.add_argument(
        "--fingerprint-algorithm",
        help="与 --fingerprint-extractor 同时提供的 relay 声纹算法 ID",
    )
    parser.add_argument(
        "--fingerprint-version",
        help="与 --fingerprint-extractor 同时提供的声纹格式版本",
    )
    parser.add_argument(
        "--fingerprint-library",
        type=Path,
        help=(
            "可选、由部署者合法取得的 ARM32 声纹共享库；与提取器写入同一目录。"
            "本工具不提供或授权第三方 SDK"
        ),
    )
    parser.add_argument(
        "--standalone-theme",
        required=True,
        type=Path,
        help="含双行歌词键的驻留 .Theme；standalone-recognition 启动后在屏内直接激活",
    )
    parser.add_argument(
        "--theme-resolution",
        required=True,
        choices=THEME_RESOLUTIONS,
        help="主题写入的分辨率目录",
    )
    parser.add_argument(
        "--work-dir",
        type=Path,
        help="临时 ext4 工作目录（必须是无空格、无中文的 Windows 路径）",
    )
    parser.add_argument("--chunk-mib", type=int, default=16)
    return parser


def main() -> int:
    # Windows 终端可能继承旧代码页，强制使用 UTF-8 避免中文进度乱码。
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    args = build_parser().parse_args()
    patch_image(args)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
