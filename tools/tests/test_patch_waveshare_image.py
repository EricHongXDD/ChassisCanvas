from __future__ import annotations

import argparse
import contextlib
import io
import ssl
import sys
import tempfile
import unittest
from pathlib import Path


TOOLS_DIRECTORY = Path(__file__).resolve().parents[1]
if str(TOOLS_DIRECTORY) not in sys.path:
    sys.path.insert(0, str(TOOLS_DIRECTORY))

import patch_waveshare_image as patcher
from expand_gpt_ext4_image import PartitionEntry


def lyric_theme_bytes(
    *,
    include_current: bool = True,
    include_next: bool = True,
    include_mdat: bool = True,
) -> bytes:
    def qstring(value: str) -> bytes:
        encoded = value.encode("utf-16-be")
        return len(encoded).to_bytes(4, "big") + encoded

    def string_map(fields: dict[str, str]) -> bytes:
        result = len(fields).to_bytes(4, "big")
        for key, value in fields.items():
            result += qstring(key) + qstring(value)
        return result

    widgets = []
    if include_current:
        widgets.append({"type": "8", "system_data_name": "CaseScreen_LyricsCurrent"})
    if include_next:
        widgets.append({"type": "8", "system_data_name": "CaseScreen_LyricsNext"})
    parts = [
        patcher.WAVESHARE_THEME_HEADER,
        (0).to_bytes(4, "big"),
        string_map({"canvas_w": "1024", "canvas_h": "600"}),
        len(widgets).to_bytes(4, "big"),
        *(string_map(widget) for widget in widgets),
        b"\x00\x00\x00\x18ftypCaseScreen",
        b"\x00\x00\x00\x10moov",
    ]
    if include_mdat:
        parts.append(b"\x00\x00\x00\x10mdat")
    return b"".join(parts)


def arm32_elf(*markers: bytes) -> bytes:
    data = bytearray(64)
    data[:4] = b"\x7fELF"
    data[4] = 1
    data[5] = 1
    data[18:20] = (40).to_bytes(2, "little")
    return bytes(data) + b"\x00".join(markers)


def recognition_markers() -> tuple[bytes, ...]:
    return (
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


def trusted_test_certificate() -> bytes:
    certificates = ssl.create_default_context().get_ca_certs(binary_form=True)
    if not certificates:
        raise unittest.SkipTest("当前 Python 运行时没有可用于测试的系统根证书")
    return ssl.DER_cert_to_PEM_cert(certificates[0]).encode("ascii")


def required_parser_arguments() -> list[str]:
    return [
        "input.img",
        "output.img",
        "--runtime-mode",
        "standalone-recognition",
        "--edge-binary",
        "casescreen-edge",
        "--recognition-relay-url",
        "https://recognition.example.test/v1/recognize",
        "--device-token-file",
        "recognition.token",
        "--ca-bundle",
        "ca-bundle.pem",
        "--standalone-theme",
        "lyrics.Theme",
        "--theme-resolution",
        "480x1920",
    ]


class MemoryDebugFs:
    def __init__(self, files: dict[str, bytes], stats: dict[str, patcher.FsStat]):
        self.files = files
        self.stats = stats

    def stat(self, path: str) -> patcher.FsStat | None:
        return self.stats.get(path)

    def dump(self, source: str, destination: Path, *, required: bool = True) -> bool:
        if source not in self.files:
            if required:
                raise AssertionError(f"missing fake payload: {source}")
            return False
        destination.write_bytes(self.files[source])
        return True


class ThemeValidationTests(unittest.TestCase):
    def test_theme_paths_are_resolution_scoped(self) -> None:
        self.assertEqual(
            patcher.standalone_theme_path("1024x600", overlay=True),
            "/upper/data/theme/1024x600/casescreen-standalone.Theme",
        )
        self.assertEqual(
            patcher.standalone_theme_path("480x1920", overlay=False),
            "/data/theme/480x1920/casescreen-standalone.Theme",
        )
        with self.assertRaisesRegex(ValueError, "主题分辨率"):
            patcher.standalone_theme_path("800x480", overlay=True)

    def test_theme_validation_accepts_both_lyric_keys_and_media_atoms(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            theme = Path(temporary) / "lyrics.Theme"
            expected = lyric_theme_bytes()
            theme.write_bytes(expected)
            self.assertEqual(patcher.validate_standalone_theme(theme), expected)

    def test_theme_validation_rejects_missing_keys_atoms_and_oversize(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            variants = {
                "current.Theme": lyric_theme_bytes(include_current=False),
                "next.Theme": lyric_theme_bytes(include_next=False),
                "mdat.Theme": lyric_theme_bytes(include_mdat=False),
            }
            for name, data in variants.items():
                with self.subTest(name=name):
                    path = root / name
                    path.write_bytes(data)
                    with self.assertRaisesRegex(ValueError, "缺少必需内容"):
                        patcher.validate_standalone_theme(path)
            oversized = root / "oversized.Theme"
            with oversized.open("wb") as stream:
                stream.truncate(16 * 1024 * 1024 + 1)
            with self.assertRaisesRegex(ValueError, "大小异常"):
                patcher.validate_standalone_theme(oversized)

    def test_edge_binary_must_be_arm32_and_support_recognition_flags(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            edge = Path(temporary) / "casescreen-edge"
            markers = recognition_markers()
            expected = arm32_elf(*markers)
            edge.write_bytes(expected)
            self.assertEqual(patcher.validate_edge_binary(edge), expected)
            edge.write_bytes(arm32_elf(b"metadata-mode"))
            with self.assertRaisesRegex(ValueError, "不支持"):
                patcher.validate_edge_binary(edge)

    def test_fingerprint_extractor_must_be_arm32(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            extractor = Path(temporary) / "extractor"
            expected = arm32_elf(b"private-fingerprint")
            extractor.write_bytes(expected)
            self.assertEqual(patcher.validate_fingerprint_extractor(extractor), expected)
            extractor.write_bytes(b"#!/bin/sh\n")
            with self.assertRaisesRegex(ValueError, "ELF"):
                patcher.validate_fingerprint_extractor(extractor)

    def test_fingerprint_library_must_be_arm32_dynamic_elf(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            library = Path(temporary) / "libacrcloud_extr_tool.so"
            expected = bytearray(arm32_elf(b"create_fingerprint", b"acr_free"))
            expected[16:18] = (3).to_bytes(2, "little")
            library.write_bytes(expected)
            self.assertEqual(patcher.validate_fingerprint_library(library), bytes(expected))
            expected[16:18] = (2).to_bytes(2, "little")
            library.write_bytes(expected)
            with self.assertRaisesRegex(ValueError, "ET_DYN"):
                patcher.validate_fingerprint_library(library)
            missing_symbol = bytearray(arm32_elf(b"create_fingerprint"))
            missing_symbol[16:18] = (3).to_bytes(2, "little")
            library.write_bytes(missing_symbol)
            with self.assertRaisesRegex(ValueError, "acr_free"):
                patcher.validate_fingerprint_library(library)

    def test_acrcloud_wrapper_requires_its_same_directory_library(self) -> None:
        wrapper = arm32_elf(b"libacrcloud_extr_tool.so")
        library = bytearray(arm32_elf(b"create_fingerprint", b"acr_free"))
        library[16:18] = (3).to_bytes(2, "little")
        with self.assertRaisesRegex(ValueError, "必须同时注入"):
            patcher.validate_fingerprint_payload_pair(wrapper, None)
        patcher.validate_fingerprint_payload_pair(wrapper, bytes(library))
        patcher.validate_fingerprint_payload_pair(arm32_elf(b"self-contained"), None)

    def test_token_and_ca_validation_are_strict(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            token = root / "token"
            token.write_text("ab" * 32 + "\n", encoding="ascii")
            self.assertEqual(patcher.validate_device_token(token), ("ab" * 32 + "\n").encode("ascii"))
            token.write_text("AB" * 32 + "\n", encoding="ascii")
            self.assertEqual(patcher.validate_device_token(token), ("AB" * 32 + "\n").encode("ascii"))
            token.write_text("shared-secret", encoding="ascii")
            with self.assertRaisesRegex(ValueError, "64 位十六进制"):
                patcher.validate_device_token(token)
            ca = root / "ca.pem"
            ca.write_bytes(trusted_test_certificate())
            self.assertIn(b"BEGIN CERTIFICATE", patcher.validate_ca_bundle(ca))
            ca.write_bytes(b"-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n")
            with self.assertRaisesRegex(ValueError, "X.509"):
                patcher.validate_ca_bundle(ca)

    def test_theme_rejects_device_microphone_widgets(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            theme = Path(temporary) / "microphone.Theme"
            data = lyric_theme_bytes()
            # 以真实主题编码把第一个文本控件的类型 8 改成 108；长度字段会随之增加。
            type8 = "8".encode("utf-16-be")
            type108 = "108".encode("utf-16-be")
            marker = len(type8).to_bytes(4, "big") + type8
            replacement = len(type108).to_bytes(4, "big") + type108
            data = data.replace(marker, replacement, 1)
            theme.write_bytes(data)
            with self.assertRaisesRegex(ValueError, "麦克风控件"):
                patcher.validate_standalone_theme(theme)


class ImageMutationTests(unittest.TestCase):
    def test_clean_original_runtime_is_accepted(self) -> None:
        patcher.reject_unsupported_runtime(MemoryDebugFs({}, {}))

    def test_old_edge_runtime_is_rejected_without_deleting_it(self) -> None:
        path = "/upper/data/casescreen-edge"
        debugfs = MemoryDebugFs({}, {path: patcher.FsStat("regular", 0o755)})
        with self.assertRaisesRegex(ValueError, "干净的原厂扩容镜像"):
            patcher.reject_unsupported_runtime(debugfs)

    def test_optional_theme_produces_no_mutation_commands(self) -> None:
        debugfs = MemoryDebugFs({}, {})
        self.assertEqual(patcher.prepare_standalone_theme_path(debugfs, None), [])
        self.assertEqual(patcher.build_theme_commands(None, None), [])

    def test_theme_commands_touch_only_owned_theme_path(self) -> None:
        # 命令构造只接受 ASCII Cygwin 路径；无需为这一纯函数测试创建真实文件。
        theme = Path("C:/casescreen-test/lyrics.Theme")
        destination = patcher.standalone_theme_path("1024x600", overlay=True)
        commands = patcher.build_theme_commands(theme, destination)
        joined = "\n".join(commands)
        self.assertIn(destination, joined)
        self.assertIn("mode 0100644", joined)
        for forbidden in (
            "serial_uart.sh",
            "PCMonitorLinux",
            "casescreen-edge",
            "sync.token",
            "source-push.key",
            "config.txt",
            "ThemeMessage",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, joined)

    def test_theme_payload_pair_must_be_consistent(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            theme = Path(temporary) / "lyrics.Theme"
            theme.write_bytes(lyric_theme_bytes())
            with self.assertRaisesRegex(ValueError, "同时提供"):
                patcher.build_theme_commands(theme, None)
            with self.assertRaisesRegex(ValueError, "同时提供"):
                patcher.build_theme_commands(None, "/upper/data/theme/x.Theme")

    def test_theme_directory_preparation_owns_only_casescreen_file(self) -> None:
        existing = patcher.standalone_theme_path("480x1920", overlay=True)
        debugfs = MemoryDebugFs(
            {},
            {
                "/upper/data/theme": patcher.FsStat("directory", 0o777),
                existing: patcher.FsStat("regular", 0o644),
            },
        )
        commands = patcher.prepare_standalone_theme_path(debugfs, "1024x600")
        joined = "\n".join(commands)
        self.assertIn("mkdir /upper/data/theme/1024x600", joined)
        self.assertIn("mode 040755", joined)
        self.assertIn(f"rm {existing}", joined)
        self.assertNotIn("config.txt", joined)
        self.assertNotIn("ThemeMessage", joined)

    def test_verify_checks_exact_theme_content_directory_and_file_modes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            theme = root / "lyrics.Theme"
            theme.write_bytes(lyric_theme_bytes())
            destination = patcher.standalone_theme_path("1024x600", overlay=True)
            files = {destination: theme.read_bytes()}
            stats = {
                "/upper/data/theme": patcher.FsStat("directory", 0o755),
                "/upper/data/theme/1024x600": patcher.FsStat("directory", 0o755),
                destination: patcher.FsStat("regular", 0o644),
            }
            debugfs = MemoryDebugFs(files, stats)
            patcher.verify_standalone_theme(
                debugfs,
                theme,
                destination,
                "1024x600",
                root,
            )

            stats[destination] = patcher.FsStat("regular", 0o600)
            with self.assertRaisesRegex(ValueError, "权限错误"):
                patcher.verify_standalone_theme(
                    debugfs,
                    theme,
                    destination,
                    "1024x600",
                    root,
                )
            stats[destination] = patcher.FsStat("regular", 0o644)
            files[destination] += b"corrupt"
            with self.assertRaisesRegex(ValueError, "内容校验失败"):
                patcher.verify_standalone_theme(
                    debugfs,
                    theme,
                    destination,
                    "1024x600",
                    root,
                )

    def test_launcher_uses_allowlisted_config_and_factory_fallback(self) -> None:
        launcher = patcher.make_launcher(
            patcher.standalone_theme_path("1024x600", overlay=True)
        ).decode("utf-8")
        self.assertNotIn(". $CONFIG", launcher)
        self.assertNotIn("source ", launcher)
        self.assertIn("while IFS='=' read -r key value", launcher)
        self.assertIn('if [ -r "$CONFIG" ]', launcher)
        self.assertIn("-metadata-mode recognition", launcher)
        self.assertIn("-recognition-relay-url", launcher)
        self.assertIn("-recognition-config", launcher)
        self.assertIn("-recognition-chunk", launcher)
        self.assertIn("-recognition-preferred-platform", launcher)
        self.assertIn("-recognition-fingerprint-extractor", launcher)
        self.assertIn('run_edge -- "$@"', launcher)
        self.assertIn("-qq-cookie-file", launcher)
        self.assertIn("-netease-cookie-file", launcher)
        self.assertIn("exec \"$ORIGINAL\"", launcher)
        self.assertIn("CASESCREEN_LYRICS_CACHE_MAX_FILES", launcher)
        self.assertIn('CACHE_MAX_FILES" -le 256', launcher)
        self.assertIn('CACHE_MAX_KIB" -le 32768', launcher)
        self.assertIn("NTP_VALID_MARKER=/var/state/dnsmasqsec", launcher)
        self.assertIn("TIME_SYNC_MARKER=/run/casescreen/time-synchronized", launcher)
        self.assertIn('if [ ! -s "$NTP_VALID_MARKER" ]', launcher)
        self.assertNotIn("pgrep ntpd", launcher)
        # 缺失 CA/token/arecord 等必须交给 edge 动态门禁落盘明确 reasons，
        # launcher 不能提前绕过硬件报告。
        self.assertNotIn('[ -x "$ARECORD" ]', launcher)
        self.assertNotIn('[ -r "$TOKEN_FILE" ]', launcher)
        self.assertNotIn('[ -r "$CA" ]', launcher)
        for removed in ("source-push", "192.168.77.1"):
            self.assertNotIn(removed, launcher)

    def test_launcher_waits_for_time_only_with_independent_default_route(self) -> None:
        launcher = patcher.make_launcher(
            patcher.standalone_theme_path("1024x600", overlay=True)
        ).decode("utf-8")
        self.assertIn("TIME_SYNC_WAIT_SECONDS=60", launcher)
        self.assertIn("has_independent_default_route()", launcher)
        self.assertIn('[ "$destination" = "00000000" ] || continue', launcher)
        self.assertIn('[ -e "/sys/class/net/$interface/device" ] || continue', launcher)
        self.assertIn('[ "$OPERSTATE" = "up" ] || continue', launcher)
        self.assertIn('while [ "$TIME_SYNC_WAITED" -lt "$TIME_SYNC_WAIT_SECONDS" ]', launcher)
        self.assertIn("sleep 1 || break", launcher)
        self.assertIn("wait_for_time_marker_if_network_candidate", launcher)

    def test_launcher_skips_time_wait_without_safe_route_candidate(self) -> None:
        launcher = patcher.make_launcher(
            patcher.standalone_theme_path("1024x600", overlay=True)
        ).decode("utf-8")
        guard = launcher.index("has_independent_default_route || return 0")
        sleep = launcher.index("sleep 1 || break", guard)
        self.assertLess(guard, sleep)
        # 名称和 driver 都要拒绝 RNDIS/USB gadget；无 device 节点的虚拟接口也不会通过。
        self.assertIn("[Uu][Ss][Bb]*", launcher)
        self.assertIn("[Rr][Nn][Dd][Ii][Ss]*", launcher)
        self.assertIn("[Gg][Aa][Dd][Gg][Ee][Tt]*", launcher)
        self.assertIn("*[Gg]_[Ee][Tt][Hh][Ee][Rr]*", launcher)
        self.assertIn("*[Uu]_[Ee][Tt][Hh][Ee][Rr]*", launcher)
        self.assertIn("*[Uu][Ss][Bb]_[Ff]*", launcher)

    def test_runtime_config_contains_no_token_or_third_party_secret(self) -> None:
        config = patcher.make_recognition_config(
            "https://recognition.example.test/v1/recognize", "hw:0,0", "/usr/bin/arecord"
        ).decode("utf-8")
        self.assertIn("CASESCREEN_RECOGNITION_TOKEN_FILE=/data/casescreen/recognition.token", config)
        self.assertNotIn("a" * 64, config)
        self.assertNotIn("qqmusic_key", config)
        self.assertIn("CASESCREEN_QQ_COOKIE_FILE=", config)
        self.assertIn("CASESCREEN_NETEASE_COOKIE_FILE=", config)
        self.assertIn("CASESCREEN_RECOGNITION_CHUNK=1s", config)
        self.assertIn("CASESCREEN_RECOGNITION_CAPTURE=10s", config)
        self.assertIn("CASESCREEN_RECOGNITION_FINGERPRINT_EXTRACTOR=", config)
        compact = patcher.make_recognition_config(
            "https://recognition.example.test/v1/recognize",
            "plughw:audiocodec",
            "/usr/bin/arecord",
            "chromaprint-private",
            "1",
        ).decode("utf-8")
        self.assertIn(
            "CASESCREEN_RECOGNITION_FINGERPRINT_EXTRACTOR=/data/casescreen/fingerprint-extractor",
            compact,
        )

    def test_usb_gadget_bridge_config_and_launcher_are_explicit(self) -> None:
        config = patcher.make_recognition_config(
            "https://recognition.example.test/v1/recognize",
            "plughw:audiocodec",
            "/usr/bin/arecord",
            network_mode="usb-gadget-bridge",
        ).decode("utf-8")
        self.assertIn("CASESCREEN_RECOGNITION_NETWORK_MODE=usb-gadget-bridge", config)
        self.assertIn("CASESCREEN_RECOGNITION_NETWORK_INTERFACE=usb0", config)
        self.assertIn("CASESCREEN_RECOGNITION_NETWORK_GATEWAY=192.168.77.1", config)

        launcher = patcher.make_launcher(
            patcher.standalone_theme_path("1024x600", overlay=True),
            "usb-gadget-bridge",
        ).decode("utf-8")
        self.assertIn('[ "$interface" = "usb0" ]', launcher)
        self.assertIn('[ "$gateway" = "014DA8C0" ]', launcher)
        self.assertNotIn('[ -e "/sys/class/net/$interface/device" ]', launcher)

        serial_launcher = patcher.make_serial_bridge_launcher().decode("utf-8")
        self.assertEqual(
            serial_launcher,
            "#!/bin/sh\nexec /data/casescreen/usb-gadget-bridge.sh bridge-client\n",
        )
        gadget = patcher.load_usb_gadget_bridge_script().decode("utf-8")
        for marker in ("functions/acm.usb0", "functions/rndis.usb0", "bridge-client", "udhcpc"):
            self.assertIn(marker, gadget)
        self.assertIn("bridge_client_ready", gadget)
        self.assertIn('grep -qx "interface=$usb_interface gateway=$USB_HOST_ADDRESS"', gadget)
        self.assertIn("复用现有 DHCP 租约", gadget)
        self.assertIn("专用桥失败后恢复 ACM-only 也失败", gadget)

    def test_runtime_commands_preserve_original_and_private_permissions(self) -> None:
        files = {
            name: Path(f"C:/casescreen-test/{name}")
            for name in ("edge", "launcher", "original", "config", "token", "ca", "time", "theme")
        }
        payloads = patcher.RuntimePayloads(
            edge=files["edge"], launcher=files["launcher"], original_monitor=files["original"],
            config=files["config"], token=files["token"], ca_bundle=files["ca"],
            time_sync_hook=files["time"],
            theme=files["theme"],
            theme_destination=patcher.standalone_theme_path("1024x600", overlay=True),
        )
        joined = "\n".join(patcher.build_runtime_commands(payloads))
        self.assertIn(f"rm {patcher.MONITOR_PATH}", joined)
        self.assertIn(patcher.ORIGINAL_MONITOR_PATH, joined)
        self.assertIn(f"set_inode_field {patcher.TOKEN_PATH} mode 0100600", joined)
        self.assertIn(f"set_inode_field {patcher.CONFIG_PATH} mode 0100600", joined)
        self.assertNotIn("serial_uart.sh", joined)

        payloads = patcher.RuntimePayloads(
            edge=files["edge"], launcher=files["launcher"], original_monitor=files["original"],
            config=files["config"], token=files["token"], ca_bundle=files["ca"],
            time_sync_hook=files["time"],
            theme=files["theme"],
            theme_destination=patcher.standalone_theme_path("1024x600", overlay=True),
            fingerprint_extractor=files["edge"],
            fingerprint_library=files["ca"],
        )
        with_extractor = "\n".join(patcher.build_runtime_commands(payloads))
        self.assertIn(patcher.EXTRACTOR_PATH, with_extractor)
        self.assertIn(f"set_inode_field {patcher.EXTRACTOR_PATH} mode 0100755", with_extractor)
        self.assertIn(patcher.FINGERPRINT_LIBRARY_PATH, with_extractor)
        self.assertIn(f"set_inode_field {patcher.FINGERPRINT_LIBRARY_PATH} mode 0100644", with_extractor)

        payloads = patcher.RuntimePayloads(
            edge=files["edge"], launcher=files["launcher"], original_monitor=files["original"],
            config=files["config"], token=files["token"], ca_bundle=files["ca"],
            time_sync_hook=files["time"], theme=files["theme"],
            theme_destination=patcher.standalone_theme_path("1024x600", overlay=True),
            serial_launcher=files["launcher"], original_serial_script=files["original"],
            usb_gadget_bridge=files["edge"],
        )
        bridge_commands = "\n".join(patcher.build_runtime_commands(payloads))
        self.assertIn(f"rm {patcher.SERIAL_SCRIPT_PATH}", bridge_commands)
        self.assertIn(patcher.ORIGINAL_SERIAL_SCRIPT_PATH, bridge_commands)
        self.assertIn(patcher.USB_GADGET_BRIDGE_PATH, bridge_commands)

    def test_time_sync_hook_requires_successful_stratum_event(self) -> None:
        hook = patcher.make_time_sync_hook().decode("utf-8")
        self.assertIn('[ "$ACTION" = "stratum" ] || exit 0', hook)
        self.assertIn('[ "$stratum" -ge 1 ]', hook)
        self.assertIn('[ "$stratum" -lt 16 ]', hook)
        self.assertIn("MARKER=/run/casescreen/time-synchronized", hook)
        self.assertIn('mv -f "$TEMP" "$MARKER"', hook)
        for forbidden in ("periodic", "pgrep", "pidof", "touch"):
            self.assertNotIn(forbidden, hook)


class ParserAndSafetyTests(unittest.TestCase):
    @staticmethod
    def patch_arguments(source: Path, target: Path) -> argparse.Namespace:
        return argparse.Namespace(
            source=source,
            target=target,
            debugfs=Path("missing-debugfs"),
            e2fsck=Path("missing-e2fsck"),
            edge_binary=Path("missing-edge"),
            device_token_file=Path("missing-token"),
            ca_bundle=Path("missing-ca"),
            standalone_theme=Path("missing-theme"),
            theme_resolution="1024x600",
            runtime_mode="standalone-recognition",
            recognition_relay_url="https://recognition.example.test/v1/recognize",
            recognition_pcm="plughw:audiocodec",
            recognition_arecord="/usr/bin/arecord",
            fingerprint_extractor=None,
            fingerprint_algorithm=None,
            fingerprint_version=None,
            chunk_mib=16,
            partition_index=6,
            work_dir=None,
        )

    def test_parser_requires_explicit_complete_standalone_runtime(self) -> None:
        parser = patcher.build_parser()
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                parser.parse_args(["input.img", "output.img"])
        args = parser.parse_args(required_parser_arguments())
        self.assertEqual(args.source, Path("input.img"))
        self.assertEqual(args.target, Path("output.img"))
        self.assertEqual(args.runtime_mode, "standalone-recognition")
        self.assertEqual(args.standalone_theme, Path("lyrics.Theme"))
        self.assertEqual(args.theme_resolution, "480x1920")
        self.assertEqual(args.recognition_pcm, "plughw:audiocodec")
        self.assertEqual(args.network_mode, "physical")

        bridge_args = parser.parse_args(required_parser_arguments() + [
            "--network-mode", "usb-gadget-bridge",
        ])
        self.assertEqual(bridge_args.network_mode, "usb-gadget-bridge")

    def test_parser_rejects_removed_runtime_arguments(self) -> None:
        parser = patcher.build_parser()
        removed_arguments = (
            ("--runtime-mode", "relay"),
            ("--token-file", "token"),
            ("--serial-script", "serial.sh"),
            ("--setup-binary", "setup"),
            ("--source-key-file", "source.key"),
        )
        for option, value in removed_arguments:
            with self.subTest(option=option):
                arguments = required_parser_arguments()
                if option == "--runtime-mode":
                    index = arguments.index("standalone-recognition")
                    arguments[index] = value
                else:
                    arguments.extend((option, value))
                with contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit):
                        parser.parse_args(arguments)

    def test_relay_and_pcm_validation_fail_closed(self) -> None:
        with self.assertRaisesRegex(ValueError, "HTTPS"):
            patcher.validate_relay_url("http://192.168.1.2/recognize")
        rejected_urls = (
            "https://recognition.example.test/v1/recognize?api_key=secret",
            "https://recognition.example.test/v1/recognize?",
            "https://recognition.example.test/v1/recognize#token",
            "https://recognition.example.test/v1/recognize#",
            "https://user:password@recognition.example.test/v1/recognize",
        )
        for url in rejected_urls:
            with self.subTest(url=url):
                with self.assertRaisesRegex(ValueError, "查询参数"):
                    patcher.validate_relay_url(url)
        args = patcher.build_parser().parse_args(required_parser_arguments())
        args.recognition_pcm = "hw:0,0;reboot"
        with self.assertRaisesRegex(ValueError, "PCM"):
            patcher.validate_runtime_options(args)

    def test_fingerprint_options_are_all_or_none(self) -> None:
        args = patcher.build_parser().parse_args(required_parser_arguments())
        self.assertEqual(patcher.validate_fingerprint_options(args), (None, None, "", ""))
        args.fingerprint_library = Path("library.so")
        with self.assertRaisesRegex(ValueError, "只能与完整"):
            patcher.validate_fingerprint_options(args)
        args.fingerprint_library = None
        args.fingerprint_extractor = Path("extractor")
        with self.assertRaisesRegex(ValueError, "必须同时提供"):
            patcher.validate_fingerprint_options(args)
        args.fingerprint_algorithm = "private-v1"
        args.fingerprint_version = "1"
        path, library, algorithm, version = patcher.validate_fingerprint_options(args)
        self.assertTrue(path.is_absolute())
        self.assertIsNone(library)
        self.assertEqual((algorithm, version), ("private-v1", "1"))
        args.fingerprint_library = Path("library.so")
        _, library, _, _ = patcher.validate_fingerprint_options(args)
        self.assertIsNotNone(library)
        self.assertTrue(library.is_absolute())
        args.fingerprint_version = "v" * 33
        with self.assertRaisesRegex(ValueError, "1..32"):
            patcher.validate_fingerprint_options(args)

    def test_layout_is_strictly_waveshare_udisk_partition_six(self) -> None:
        partitions = [
            PartitionEntry(index, index * 100, index * 100 + 49, name)
            for index, name in enumerate(patcher.EXPECTED_PARTITIONS, start=1)
        ]
        self.assertEqual(patcher.validate_waveshare_layout(partitions, 6), partitions[-1])
        wrong = [*partitions[:-1], PartitionEntry(6, 600, 649, "DATA")]
        with self.assertRaisesRegex(ValueError, "布局"):
            patcher.validate_waveshare_layout(wrong, 6)
        overlapping = [*partitions]
        overlapping[1] = PartitionEntry(2, 140, 249, patcher.EXPECTED_PARTITIONS[1])
        with self.assertRaisesRegex(ValueError, "重叠"):
            patcher.validate_waveshare_layout(overlapping, 6)

    def test_patch_refuses_in_place_or_existing_output_before_external_tools(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.img"
            source.write_bytes(b"input-must-remain")
            with self.assertRaisesRegex(ValueError, "不能与输入镜像相同"):
                patcher.patch_image(self.patch_arguments(source, source))
            self.assertEqual(source.read_bytes(), b"input-must-remain")

            target = root / "target.img"
            target.write_bytes(b"existing-output")
            with self.assertRaisesRegex(FileExistsError, "拒绝覆盖"):
                patcher.patch_image(self.patch_arguments(source, target))
            self.assertEqual(source.read_bytes(), b"input-must-remain")
            self.assertEqual(target.read_bytes(), b"existing-output")

    def test_non_ascii_work_path_is_rejected_before_creation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "中文工作区"
            with self.assertRaisesRegex(ValueError, "中文或空格"):
                patcher.ensure_safe_work_base(path)
            self.assertFalse(path.exists())

    def test_atomic_commit_refuses_existing_target(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            partial = root / "partial"
            target = root / "target"
            partial.write_bytes(b"new")
            target.write_bytes(b"old")
            with self.assertRaises(FileExistsError):
                patcher.atomic_commit_no_replace(partial, target)
            self.assertEqual(target.read_bytes(), b"old")
            self.assertEqual(partial.read_bytes(), b"new")

    def test_partition_comparison_detects_changes_outside_udisk(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.img"
            target = root / "target.img"
            source.write_bytes(bytes(range(256)) * 16)
            target.write_bytes(source.read_bytes())
            partition = PartitionEntry(6, 2, 3, "UDISK")
            patcher.compare_outside_partition(source, target, partition, 128)
            data = bytearray(target.read_bytes())
            data[0] ^= 0xFF
            target.write_bytes(data)
            with self.assertRaisesRegex(ValueError, "之前"):
                patcher.compare_outside_partition(source, target, partition, 128)


if __name__ == "__main__":
    unittest.main()
