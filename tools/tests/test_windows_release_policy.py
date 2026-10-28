from __future__ import annotations

import json
import re
import struct
import unittest
from pathlib import Path


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]


class WindowsReleasePolicyTests(unittest.TestCase):
    def test_windows_build_pins_amd64_target_and_checks_pe_machine(self) -> None:
        script = (REPOSITORY_ROOT / "tools" / "build_windows.ps1").read_text(
            encoding="utf-8"
        )
        self.assertRegex(script, r'\$env:GOOS\s*=\s*"windows"')
        self.assertRegex(script, r'\$env:GOARCH\s*=\s*"amd64"')
        self.assertRegex(script, r'\$env:GOAMD64\s*=\s*"v1"')
        self.assertIn(
            "[BitConverter]::ToUInt16($peBytes, $peHeaderOffset + 4)", script
        )
        self.assertIn("$machine -ne [uint16]0x8664", script)
        self.assertIn("Executable is not an AMD64 PE binary", script)

    def test_runtime_files_reject_reparse_sources_and_non_amd64_pe(self) -> None:
        script = (REPOSITORY_ROOT / "tools" / "build_windows.ps1").read_text(
            encoding="utf-8"
        )
        runtime_match = re.search(
            r"\$runtimeFiles\s*=\s*@\((.*?)\)\s*foreach\s*\(\$runtimeFile.*?\n\}",
            script,
            re.DOTALL,
        )
        self.assertIsNotNone(runtime_match)
        runtime_block = runtime_match.group(0)
        for runtime_file in {
            "hidapi.dll",
            "ffmpeg.exe",
            "avcodec-58.dll",
            "avdevice-58.dll",
            "avfilter-7.dll",
            "avformat-58.dll",
            "avutil-56.dll",
            "swresample-3.dll",
            "swscale-5.dll",
        }:
            self.assertIn(f'"{runtime_file}"', runtime_block)

        source_check = "Assert-PlainFile -Path $runtimeSource"
        copy_source = "Copy-Item -LiteralPath $runtimeSource"
        self.assertIn(source_check, runtime_block)
        self.assertIn(copy_source, runtime_block)
        self.assertLess(runtime_block.index(source_check), runtime_block.index(copy_source))
        self.assertIn(
            "Assert-WindowsAMD64PEFile -Path $runtimeDestination", runtime_block
        )

        for marker in {
            "function Assert-WindowsAMD64PEFile",
            "$peBytes[0] -ne 0x4d",
            "$peBytes[1] -ne 0x5a",
            "$peBytes[$peHeaderOffset] -ne 0x50",
            "[BitConverter]::ToUInt16($peBytes, $peHeaderOffset + 4)",
            "$machine -ne [uint16]0x8664",
        }:
            self.assertIn(marker, script)

    def test_application_icon_assets_and_embedding_gates(self) -> None:
        png_path = REPOSITORY_ROOT / "assets" / "casescreen-icon.png"
        ico_path = REPOSITORY_ROOT / "assets" / "casescreen-icon.ico"
        png = png_path.read_bytes()
        self.assertEqual(png[:8], b"\x89PNG\r\n\x1a\n")
        width, height = struct.unpack_from(">II", png, 16)
        self.assertEqual((width, height), (256, 256))
        self.assertEqual(png[24:26], bytes((8, 6)), "PNG 必须是 8 位 RGBA")

        ico = ico_path.read_bytes()
        reserved, kind, count = struct.unpack_from("<HHH", ico, 0)
        self.assertEqual((reserved, kind), (0, 1))
        self.assertGreater(count, 0)
        sizes: set[int] = set()
        for index in range(count):
            offset = 6 + 16 * index
            width_byte, height_byte = ico[offset : offset + 2]
            size, data_offset = struct.unpack_from("<II", ico, offset + 8)
            width = width_byte or 256
            height = height_byte or 256
            self.assertEqual(width, height)
            self.assertGreater(size, 0)
            self.assertLessEqual(data_offset + size, len(ico))
            sizes.add(width)
        self.assertTrue({16, 20, 24, 32, 40, 48, 64, 128, 256}.issubset(sizes))

        tray_source = (
            REPOSITORY_ROOT / "internal" / "trayapp" / "tray.go"
        ).read_text(encoding="utf-8")
        self.assertIn('appassets "casescreen/assets"', tray_source)
        self.assertIn("appassets.IconPNG()", tray_source)

        build_script = (
            REPOSITORY_ROOT / "tools" / "build_windows.ps1"
        ).read_text(encoding="utf-8")
        for marker in {
            "assets\\casescreen-icon.png",
            "assets\\casescreen-icon.ico",
            "Set-CaseScreenExecutableIcon",
            "Assert-CaseScreenExecutableIcon",
        }:
            self.assertIn(marker, build_script)

        resource_script = (
            REPOSITORY_ROOT / "tools" / "windows_icon_resource.ps1"
        ).read_text(encoding="utf-8")
        for marker in {"[IntPtr]14", "[IntPtr]32512", "BeginUpdateResource"}:
            self.assertIn(marker, resource_script)

    def test_both_executables_receive_exact_1_0_version_resources(self) -> None:
        build_script = (
            REPOSITORY_ROOT / "tools" / "build_windows.ps1"
        ).read_text(encoding="utf-8")
        for marker in {
            'Description = "CaseScreen 后台服务"',
            'OriginalFilename = "CaseScreen.exe"',
            'Description = "CaseScreen 主题工作台"',
            'OriginalFilename = "CaseScreenDesktop.exe"',
            'ProductName "CaseScreen"',
            "Set-CaseScreenExecutableVersionInfo",
            "Assert-CaseScreenExecutableVersionInfo",
            "$numericVersionParts",
        }:
            self.assertIn(marker, build_script)

        resource_script = (
            REPOSITORY_ROOT / "tools" / "windows_icon_resource.ps1"
        ).read_text(encoding="utf-8")
        for marker in {
            "VS_VERSION_INFO",
            "FixedFileInfo",
            "FileDescription",
            "FileVersion",
            "OriginalFilename",
            "ProductName",
            "ProductVersion",
            "[IntPtr]16",
            "[Diagnostics.FileVersionInfo]::GetVersionInfo",
        }:
            self.assertIn(marker, resource_script)

    def test_build_manifest_copies_only_host_documents(self) -> None:
        script = (REPOSITORY_ROOT / "tools" / "build_windows.ps1").read_text(
            encoding="utf-8"
        )
        literal_sources = {
            value.replace("\\", "/").lower()
            for value in re.findall(
                r'Copy-Item\s+-LiteralPath\s+"([^"]+)"', script
            )
        }
        for required in {
            "readme.md",
            "examples/device-config.json",
            "docs/windows-production.md",
            "docs/windows-verification.md",
            "docs/host-lyrics.md",
            "desktop/readme.md",
        }:
            self.assertIn(required, literal_sources)
        for experimental in {
            "docs/architecture.md",
            "docs/verification.md",
            "docs/standalone-lyrics.md",
            "docs/standalone-recognition.md",
            "docs/recognition-relay.md",
        }:
            self.assertNotIn(experimental, literal_sources)

        for gate_marker in {
            "casescreen/internal/edgebridge",
            "casescreen/internal/edgerecognition",
            "casescreen/internal/edgeprobe",
            "casescreen/internal/edgelyrics",
            "casescreen/internal/recognitionrelay",
            "casescreen/internal/device/thermalright",
            "casescreen/internal/usb/libusb",
            "casescreen-recognition-relay",
            "recognition.env",
            "standalone-recognition.md",
            "casescreen/internal/gpupreference",
            "internal\\workbench\\gio_gpu_settings.go",
            "SaveGPUAdapter(adapterID)",
            "Windows GPU UI 恢复了旧提示或旧三按钮实现",
            "$archiveStagingPath",
            "临时发行压缩包缺少必要文件",
            "正式文档缺少精确的发布版本标题或字段",
        }:
            self.assertIn(gate_marker, script)
        for host_marker in {
            "audio.NewEngine(audio.NewLoopback()",
            "lyrics.NewNowPlayingSource()",
            "lyrics.NeteaseProvider{Session: platformSessions}",
            "lyrics.QQProvider{Session: platformSessions}",
            "server.SetApplicationVersion(version)",
        }:
            self.assertIn(host_marker, script)
        for user_boundary_marker in {
            "$formalUISources",
            "$formalReleaseDocuments",
            "$forbiddenUserFacingMarkers",
            "无需购买、连接或安装任何额外设备或辅助程序",
        }:
            self.assertIn(user_boundary_marker, script)

        runtime_files = re.search(
            r"\$runtimeFiles\s*=\s*@\((.*?)\)\s*foreach\s*\(\$runtimeFile",
            script,
            re.DOTALL,
        )
        self.assertIsNotNone(runtime_files)
        self.assertNotIn("libusb-1.0.dll", runtime_files.group(1).lower())

    def test_default_example_has_no_screen_experiment_configuration(self) -> None:
        example_path = REPOSITORY_ROOT / "examples" / "device-config.json"
        config = json.loads(example_path.read_text(encoding="utf-8"))
        self.assertEqual(config["listen_address"], "127.0.0.1:19321")
        self.assertTrue(config["audio_enabled"])
        self.assertTrue(config["lyrics_enabled"])
        self.assertEqual(config["lyrics_provider"], "auto")
        self.assertEqual(config["gpu_preference"], "system")
        self.assertEqual(
            [device["id"] for device in config["devices"]],
            ["waveshare-7", "waveshare-8.8", "coolermaster-atmos-v2"],
        )
        self.assertEqual(
            [device["kind"] for device in config["devices"]],
            ["waveshare", "waveshare", "coolermaster"],
        )

        encoded = json.dumps(config, ensure_ascii=False).lower()
        for forbidden in {
            "recognition",
            "acrcloud",
            "microphone",
            "edge_sync",
            "source_push",
            "screen_network",
            "screen_cookie",
            "relay_token",
            "thermalright",
        }:
            self.assertNotIn(forbidden, encoded)

    def test_windows_entry_does_not_import_edge_experiments(self) -> None:
        source = (REPOSITORY_ROOT / "cmd" / "casescreen" / "main.go").read_text(
            encoding="utf-8"
        )
        for required in {
            '"casescreen/internal/audio"',
            '"casescreen/internal/lyrics"',
            '"casescreen/internal/platformauth"',
            '"casescreen/internal/service"',
        }:
            self.assertIn(required, source)
        self.assertIn("audio.NewEngine(audio.NewLoopback()", source)
        self.assertIn("lyrics.NeteaseProvider{Session: platformSessions}", source)
        self.assertIn("lyrics.QQProvider{Session: platformSessions}", source)
        for forbidden in {
            "internal/playerhost",
            "internal/edgebridge",
            "internal/edgerecognition",
            "internal/edgeprobe",
            "internal/edgelyrics",
            "internal/edgesync",
            "internal/edgepush",
            "internal/device/thermalright",
            "internal/usb/libusb",
            "case \"thermalright\"",
            "lyrics.KugouProvider",
            "lyrics.SogouProvider",
        }:
            self.assertNotIn(forbidden, source)

    def test_desktop_requires_exact_service_application_version(self) -> None:
        desktop = (
            REPOSITORY_ROOT / "cmd" / "casescreen-desktop" / "main.go"
        ).read_text(encoding="utf-8")
        service = (REPOSITORY_ROOT / "cmd" / "casescreen" / "main.go").read_text(
            encoding="utf-8"
        )
        api = (REPOSITORY_ROOT / "internal" / "api" / "server.go").read_text(
            encoding="utf-8"
        )
        service_version = re.search(r'const version = "([^"]+)"', service)
        desktop_version = re.search(r'const desktopVersion = "([^"]+)"', desktop)
        self.assertIsNotNone(service_version)
        self.assertIsNotNone(desktop_version)
        self.assertEqual(service_version.group(1), desktop_version.group(1))
        self.assertEqual(service_version.group(1), "1.0.1")
        self.assertIn('json:"application_version"', desktop)
        self.assertIn("body.ApplicationVersion != desktopVersion", desktop)
        self.assertIn("workbench.NewAPI(serviceURL, desktopVersion)", desktop)
        self.assertRegex(
            api, r'"application_version":\s+server\.applicationVersion'
        )

    def test_formal_release_uses_1_0_1_names_everywhere(self) -> None:
        script = (REPOSITORY_ROOT / "tools" / "build_windows.ps1").read_text(
            encoding="utf-8"
        )
        default_version = re.search(
            r'\[string\]\$Version\s*=\s*"([^"]+)"', script
        )
        self.assertIsNotNone(default_version)
        self.assertEqual(default_version.group(1), "1.0.1")
        self.assertIn('"artifacts\\CaseScreen-windows-x64-{0}" -f $Version', script)
        self.assertIn(
            '"artifacts\\CaseScreen-windows-x64-{0}.zip" -f $Version', script
        )

        readme = (REPOSITORY_ROOT / "README.md").read_text(encoding="utf-8")
        verification = (
            REPOSITORY_ROOT / "docs" / "windows-verification.md"
        ).read_text(encoding="utf-8")
        self.assertRegex(readme, r"(?m)^> \*\*正式发布版本：\*\* `1\.0\.1`$")
        self.assertRegex(verification, r"(?m)^# Windows 正式版 1\.0\.1 验证$")
        self.assertNotIn("beta.17", readme)
        self.assertNotIn("beta.17", verification)

        self.assertIn("$requiredReleaseDocumentPatterns", script)
        self.assertIn("[Regex]::IsMatch", script)
        self.assertNotIn("$versionedText.Contains($Version)", script)

        preview = (
            REPOSITORY_ROOT / "cmd" / "casescreen-visual-preview" / "main.go"
        ).read_text(encoding="utf-8")
        self.assertIn('"visual-preview-1.0.png"', preview)
        self.assertIn('ID: "visual-preview-1.0"', preview)
        self.assertNotIn("beta17", preview)

    def test_gpu_preference_is_a_bounded_windows_program_setting(self) -> None:
        config_source = (
            REPOSITORY_ROOT / "internal" / "config" / "config.go"
        ).read_text(encoding="utf-8")
        server_source = (
            REPOSITORY_ROOT / "internal" / "api" / "server.go"
        ).read_text(encoding="utf-8")
        manager_source = (
            REPOSITORY_ROOT / "internal" / "gpupreference" / "preference.go"
        ).read_text(encoding="utf-8")
        windows_source = (
            REPOSITORY_ROOT
            / "internal"
            / "gpupreference"
            / "preference_windows.go"
        ).read_text(encoding="utf-8")
        ui_source = (
            REPOSITORY_ROOT / "internal" / "workbench" / "gio_gpu_settings.go"
        ).read_text(encoding="utf-8")
        desktop_source = (
            REPOSITORY_ROOT / "cmd" / "casescreen-desktop" / "main.go"
        ).read_text(encoding="utf-8")
        startup_source = (
            REPOSITORY_ROOT / "cmd" / "casescreen-desktop" / "gpu_startup_windows.go"
        ).read_text(encoding="utf-8")

        self.assertIn('json:"gpu_preference"', config_source)
        self.assertIn('mux.HandleFunc("GET /api/gpu-preference"', server_source)
        self.assertIn('mux.HandleFunc("PUT /api/gpu-preference"', server_source)
        for marker in {
            '"CaseScreen.exe"',
            '"CaseScreenDesktop.exe"',
            'Preference = "system"',
            'Preference = "power_saving"',
            'Preference = "high_performance"',
        }:
            self.assertIn(marker, manager_source)
        self.assertIn("Software\\Microsoft\\DirectX\\UserGpuPreferences", windows_source)
        self.assertIn("GpuPreference=1", windows_source)
        self.assertIn("GpuPreference=2", windows_source)
        self.assertIn("synchronizeStartupGPUPreference(executable)", desktop_source)
        self.assertLess(
            desktop_source.index("synchronizeStartupGPUPreference(executable)"),
            desktop_source.index("ensureService(root, serviceURL)"),
        )
        self.assertIn("config.Open(\"\")", startup_source)
        self.assertIn("gpupreference.NewManagerForExecutable(executable)", startup_source)
        for marker in {
            "程序与显卡",
            "检测到的显卡",
            "系统自动",
            "status.Adapters",
            "state.dropdownOpen",
            "SaveGPUAdapter(adapterID)",
            "保存 GPU 设置",
            "重启",
        }:
            self.assertIn(marker, ui_source)
        for retired_marker in {"通常", "PowerSavingLabel", "HighPerformanceLabel"}:
            self.assertNotIn(retired_marker, ui_source)

    def test_windows_audio_source_is_output_loopback_only(self) -> None:
        source = (
            REPOSITORY_ROOT / "internal" / "audio" / "loopback_windows.go"
        ).read_text(encoding="utf-8")
        self.assertIn("GetDefaultAudioEndpoint(wca.ERender", source)
        self.assertIn("wca.AUDCLNT_STREAMFLAGS_LOOPBACK", source)
        self.assertNotIn("GetDefaultAudioEndpoint(wca.ECapture", source)

    def test_readme_and_desktop_copy_state_host_only_boundary(self) -> None:
        readme = (REPOSITORY_ROOT / "README.md").read_text(encoding="utf-8")
        desktop = (REPOSITORY_ROOT / "desktop" / "README.md").read_text(
            encoding="utf-8"
        )
        for required in {
            "无需购买、连接或安装任何额外设备或辅助程序",
            "只访问播放端点",
            "不打开录音或输入端点",
            "WASAPI",
            "Windows 主机歌词同步",
            "Windows 正式版验证",
        }:
            self.assertIn(required, readme)
        for required in {
            "Windows 主机",
            "不发送到副屏",
            "副屏只通过原有 USB 连接接收显示数据",
            "无需额外设备、网络设置或辅助程序",
        }:
            self.assertIn(required, desktop)

        formal_lyrics = (
            REPOSITORY_ROOT / "docs" / "host-lyrics.md"
        ).read_text(encoding="utf-8")
        for required in {
            "CaseScreen_LyricsCurrent",
            "CaseScreen_LyricsNext",
            "CaseScreen_LyricsCurrentAtMs",
            "CaseScreen_LyricsNextAtMs",
            "CaseScreen_LyricsPlaying",
            "LV",
            "主机换行计时器",
            "不提供在屏内另行部署歌词解析服务的设置入口",
            "DPAPI",
            "not_configured",
            "needs_login",
        }:
            self.assertIn(required, formal_lyrics)

    def test_windows_1_0_lyrics_event_uses_exact_six_field_contract(self) -> None:
        long_fields = {
            "CaseScreen_LyricsCurrent",
            "CaseScreen_LyricsNext",
            "CaseScreen_LyricsCurrentAtMs",
            "CaseScreen_LyricsNextAtMs",
            "CaseScreen_LyricsPlaying",
        }
        base_fields = long_fields | {"LV"}
        self.assertEqual(len(base_fields), 6)

        telemetry = (
            REPOSITORY_ROOT / "internal" / "telemetry" / "snapshot.go"
        ).read_text(encoding="utf-8")
        emitted_literal_fields = set(
            re.findall(r'values\["(CaseScreen_Lyrics[^"]+)"\]', telemetry)
        )
        # 兼容进度键即使以后改成字面量写法，也不属于六字段状态事件。
        emitted_long_fields = emitted_literal_fields - {
            "CaseScreen_LyricsProgress"
        }
        self.assertEqual(emitted_long_fields, long_fields)
        self.assertRegex(telemetry, r'LyricsVisibleKey\s*=\s*"LV"')
        self.assertIn("values[LyricsVisibleKey]", telemetry)
        self.assertRegex(telemetry, r'LyricsProgressFineKey\s*=\s*"LFP"')
        self.assertRegex(
            telemetry,
            r'LyricsProgressLegacyKey\s*=\s*"CaseScreen_LyricsProgress"',
        )
        self.assertNotIn("LFP", base_fields)
        self.assertNotIn("CaseScreen_LyricsProgress", base_fields)

        contract_documents = {
            "README.md": REPOSITORY_ROOT / "README.md",
            "windows-production.md": (
                REPOSITORY_ROOT / "docs" / "windows-production.md"
            ),
            "windows-verification.md": (
                REPOSITORY_ROOT / "docs" / "windows-verification.md"
            ),
            "host-lyrics.md": REPOSITORY_ROOT / "docs" / "host-lyrics.md",
        }
        for name, path in contract_documents.items():
            text = path.read_text(encoding="utf-8")
            for field in base_fields:
                with self.subTest(document=name, field=field):
                    self.assertIn(field, text)

        host_lyrics = contract_documents["host-lyrics.md"].read_text(
            encoding="utf-8"
        )
        self.assertIn("六字段歌词状态", host_lyrics)
        verification = contract_documents["windows-verification.md"].read_text(
            encoding="utf-8"
        )
        self.assertNotIn("五个键", verification)
        self.assertIn("六个键", verification)

    def test_formal_user_surfaces_offer_no_recognition_or_bridge_entry(self) -> None:
        paths = [
            REPOSITORY_ROOT / "README.md",
            REPOSITORY_ROOT / "desktop" / "README.md",
            REPOSITORY_ROOT / "docs" / "windows-production.md",
            REPOSITORY_ROOT / "docs" / "windows-verification.md",
            REPOSITORY_ROOT / "docs" / "host-lyrics.md",
        ]
        forbidden = {
            "识歌",
            "麦克风",
            "microphone",
            "声纹",
            "acrcloud",
            "独立桥",
            "外部桥",
            "网桥",
            "companion",
            "edgesync",
            "source-push",
            "bluez",
            "rndis",
            "recognition-relay",
            "cmd/casescreen-edge",
            "thermalright",
            "利民",
        }
        for path in paths:
            text = path.read_text(encoding="utf-8").lower()
            for marker in forbidden:
                self.assertNotIn(marker, text, f"{path.name}: {marker}")

        ui = (
            REPOSITORY_ROOT / "internal" / "workbench" / "platform_login.go"
        ).read_text(encoding="utf-8")
        self.assertIn("无需购买、连接或安装任何额外设备或辅助程序", ui)
        self.assertIn("副屏只接收显示小包", ui)
        for marker in {"识歌", "麦克风", "microphone", "独立桥", "外部桥", "companion", "网桥"}:
            self.assertNotIn(marker, ui)

        service = (REPOSITORY_ROOT / "cmd" / "casescreen" / "main.go").read_text(
            encoding="utf-8"
        )
        api = (REPOSITORY_ROOT / "internal" / "api" / "server.go").read_text(
            encoding="utf-8"
        )
        self.assertIn("lyrics.NewNowPlayingSource()", service)
        self.assertNotIn("playerhost", service)
        self.assertNotIn("/api/player-host/netease", api)
        index = (REPOSITORY_ROOT / "internal" / "api" / "index.go").read_text(
            encoding="utf-8"
        )
        self.assertNotIn("thermalright", index.lower())
        self.assertNotIn("利民", index)

        dialog = (
            REPOSITORY_ROOT / "internal" / "workbench" / "file_dialog_windows.go"
        ).read_text(encoding="utf-8")
        self.assertIn("导入后由主机加密复用", dialog)
        self.assertNotIn("一次性导入载体", dialog)

    def test_netease_validation_is_host_read_only_memory(self) -> None:
        verification = (
            REPOSITORY_ROOT / "docs" / "windows-verification.md"
        ).read_text(encoding="utf-8")
        for required in {
            "CASESCREEN_NETEASE_MEMORY_LIVE",
            "TestNeteaseProcessMemoryNativeIDLive",
            "PROCESS_VM_READ",
            "无需退出、重启",
            "与同一轮 GSMTC 严格一致",
        }:
            self.assertIn(required, verification)
        for forbidden in {
            "CASESCREEN_NETEASE_DISPOSABLE_WINDOWS",
            "remote-debugging-pipe",
            "player-host/netease",
            "私有管道",
        }:
            self.assertNotIn(forbidden, verification)

    def test_research_documents_are_explicitly_non_production(self) -> None:
        documents = {
            "standalone-lyrics.md": "历史方案比较",
            "standalone-recognition.md": "实验/研究文档",
            "recognition-relay.md": "实验/研究文档",
            "verification.md": "历史版本与屏端实验",
        }
        for name, marker in documents.items():
            text = (REPOSITORY_ROOT / "docs" / name).read_text(encoding="utf-8")
            self.assertIn(marker, text, name)
            self.assertIn("不进入 Windows", text, name)

        architecture = (REPOSITORY_ROOT / "docs" / "architecture.md").read_text(
            encoding="utf-8"
        )
        for required in {
            "主机内置只读适配器",
            "CaseScreen_LyricsCurrent",
            "WASAPI",
            "USB 副屏发送小型状态包",
        }:
            self.assertIn(required, architecture)

    def test_release_transaction_guards_paths_and_verified_artifacts(self) -> None:
        script = (REPOSITORY_ROOT / "tools" / "build_windows.ps1").read_text(
            encoding="utf-8"
        )
        for required in {
            "Test-PathsOverlap",
            "transactionTargets",
            "outputBackupCreated",
            "archiveBackupCreated",
            "stagedArchiveHash",
            "initialArchiveHash",
            "Ensure-PlainDirectory -Path $artifactsRoot",
            "Get-PackageRelativePath",
        }:
            self.assertIn(required, script)
        self.assertNotIn("[System.IO.Path]::GetRelativePath", script)


if __name__ == "__main__":
    unittest.main()
