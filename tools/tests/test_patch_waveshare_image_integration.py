from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


TOOLS_DIRECTORY = Path(__file__).resolve().parents[1]
if str(TOOLS_DIRECTORY) not in sys.path:
    sys.path.insert(0, str(TOOLS_DIRECTORY))

import patch_waveshare_image as patcher


def arm32_elf(*markers: bytes) -> bytes:
    data = bytearray(64)
    data[:4] = b"\x7fELF"
    data[4] = 1
    data[5] = 1
    data[18:20] = (40).to_bytes(2, "little")
    return bytes(data) + b"\x00".join(markers)


def lyric_theme_bytes() -> bytes:
    def qstring(value: str) -> bytes:
        encoded = value.encode("utf-16-be")
        return len(encoded).to_bytes(4, "big") + encoded

    def string_map(fields: dict[str, str]) -> bytes:
        result = len(fields).to_bytes(4, "big")
        for key, value in fields.items():
            result += qstring(key) + qstring(value)
        return result

    widgets = (
        {"type": "8", "system_data_name": "CaseScreen_LyricsCurrent"},
        {"type": "8", "system_data_name": "CaseScreen_LyricsNext"},
    )
    return b"".join(
        (
            patcher.WAVESHARE_THEME_HEADER,
            (0).to_bytes(4, "big"),
            string_map({"canvas_w": "1024", "canvas_h": "600"}),
            len(widgets).to_bytes(4, "big"),
            *(string_map(widget) for widget in widgets),
            b"\x00\x00\x00\x18ftypCaseScreen",
            b"\x00\x00\x00\x10moov",
            b"\x00\x00\x00\x10mdat",
        )
    )


@unittest.skipUnless(os.environ.get("CASESCREEN_MKE2FS"), "需要 CASESCREEN_MKE2FS 才运行真实 ext4 冒烟测试")
class DebugFsIntegrationTests(unittest.TestCase):
    def test_real_debugfs_patch_and_verify_on_small_ext4(self) -> None:
        mke2fs = Path(os.environ["CASESCREEN_MKE2FS"]).resolve()
        cygwin_bin = Path(os.environ["CASESCREEN_CYGWIN_BIN"]).resolve()
        self.assertTrue(mke2fs.is_file())
        self.assertTrue((cygwin_bin / "cygwin1.dll").is_file())

        work_base = patcher.ensure_safe_work_base(Path(tempfile.gettempdir()))
        root = Path(tempfile.mkdtemp(prefix="casescreen-small-ext4-", dir=work_base))
        try:
            filesystem = root / "fixture.ext4"
            with filesystem.open("wb") as stream:
                stream.truncate(32 * 1024 * 1024)
            environment = os.environ.copy()
            environment["PATH"] = str(cygwin_bin) + os.pathsep + environment.get("PATH", "")
            completed = subprocess.run(
                [
                    str(mke2fs),
                    "-q",
                    "-t",
                    "ext4",
                    "-F",
                    "-O",
                    "^64bit,^metadata_csum",
                    patcher.cygwin_argument(filesystem),
                ],
                cwd=mke2fs.parent,
                env=environment,
                text=True,
                encoding="utf-8",
                errors="replace",
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)

            tool_directory = root / "tools"
            debugfs_path, e2fsck_path = patcher.stage_e2fsprogs(
                patcher.DEFAULT_DEBUGFS,
                patcher.DEFAULT_E2FSCK,
                tool_directory,
            )
            staging = root / "payload"
            staging.mkdir()
            debugfs = patcher.DebugFs(debugfs_path, filesystem, tool_directory, staging)

            original_monitor = staging / "original-monitor"
            original_monitor.write_bytes(arm32_elf(b"original-monitor"))
            debugfs.apply(
                [
                    "mkdir /upper",
                    "mkdir /upper/data",
                    f"write {patcher.cygwin_argument(original_monitor)} {patcher.MONITOR_PATH}",
                    f"set_inode_field {patcher.MONITOR_PATH} mode 0100755",
                ]
            )

            edge = staging / "edge-new"
            launcher = staging / "launcher-new.sh"
            token = staging / "token-new"
            config = staging / "recognition.env"
            ca_bundle = staging / "ca-bundle.pem"
            theme = staging / "lyrics.Theme"
            time_sync_hook = staging / "90-casescreen-time"
            edge.write_bytes(
                arm32_elf(
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
            )
            theme_destination = patcher.standalone_theme_path("1024x600", overlay=True)
            launcher.write_bytes(patcher.make_launcher(theme_destination))
            time_sync_hook.write_bytes(patcher.make_time_sync_hook())
            token.write_bytes(("ab" * 32 + "\n").encode("ascii"))
            config.write_bytes(
                patcher.make_recognition_config(
                    "https://recognition.example.test/v1/recognize", "hw:0,0", "/usr/bin/arecord"
                )
            )
            ca_bundle.write_bytes(b"-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n")
            theme.write_bytes(lyric_theme_bytes())

            patcher.reject_unsupported_runtime(debugfs)
            extracted_original = patcher.collect_original_monitor(debugfs, staging)
            payloads = patcher.RuntimePayloads(
                edge=edge,
                launcher=launcher,
                original_monitor=extracted_original,
                config=config,
                token=token,
                ca_bundle=ca_bundle,
                time_sync_hook=time_sync_hook,
                theme=theme,
                theme_destination=theme_destination,
            )
            commands = patcher.prepare_runtime_directories(debugfs)
            commands.extend(patcher.prepare_standalone_theme_path(debugfs, "1024x600"))
            commands.extend(patcher.build_runtime_commands(payloads))
            debugfs.apply(commands)
            patcher.run_e2fsck(e2fsck_path, filesystem, tool_directory, "32MB ext4")
            patcher.verify_runtime(debugfs, payloads, "1024x600", staging)
        finally:
            shutil.rmtree(root)


if __name__ == "__main__":
    unittest.main()
