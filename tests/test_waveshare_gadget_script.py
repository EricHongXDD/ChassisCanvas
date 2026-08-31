"""微雪 ACM + RNDIS 脚本的静态安全检查。"""

from __future__ import annotations

import shutil
import subprocess
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "firmware" / "waveshare" / "serial_uart_casescreen.sh"
README = ROOT / "firmware" / "waveshare" / "README.md"


class WaveshareGadgetScriptTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.script = SCRIPT.read_text(encoding="utf-8")
        cls.readme = README.read_text(encoding="utf-8")

    def test_script_is_strict_posix_shell(self) -> None:
        self.assertTrue(self.script.startswith("#!/bin/sh\n"))
        self.assertIn("set -eu", self.script)
        self.assertNotIn("#!/bin/bash", self.script)

    def test_preserves_original_acm_identity_and_serial_port(self) -> None:
        for expected in (
            'GADGET="$GADGET_ROOT/ACM"',
            'ACM_FUNCTION="$GADGET/functions/acm.usb0"',
            'RNDIS_FUNCTION="$GADGET/functions/rndis.usb0"',
            'write_value "0x1d6b" "$GADGET/idVendor"',
            'write_value "0x0104" "$GADGET/idProduct"',
            'write_value "0123456789ABCDEF"',
            "/dev/ttyGS0",
        ):
            self.assertIn(expected, self.script)

    def test_acm_is_linked_before_rndis(self) -> None:
        acm = 'ensure_link "$ACM_FUNCTION" "$ACM_LINK"'
        rndis = 'ensure_link "$RNDIS_FUNCTION" "$RNDIS_LINK"'
        start = self.script.index("start_composite()")
        start_body = self.script[start : self.script.index("restore_acm_only()")]
        self.assertLess(start_body.index(acm), start_body.index(rndis))

    def test_has_idempotency_and_failure_rollback(self) -> None:
        for expected in (
            "is_composite_bound",
            "ROLLBACK_REQUIRED=1",
            "rollback_to_acm",
            "trap 'on_exit' 0",
            "restore-acm)",
            "bind_target_gadget \"$ROLLBACK_UDC\"",
        ):
            self.assertIn(expected, self.script)

        # 只能清空目标 ACM gadget 的 UDC，不能遍历解绑其他 gadget。
        self.assertIn('write_value "" "$GADGET/UDC"', self.script)
        self.assertNotRegex(self.script, r"usb_gadget/\*.*UDC")

    def test_uses_fixed_point_to_point_addresses(self) -> None:
        for expected in (
            'USB_DEVICE_ADDRESS="192.168.77.2"',
            'USB_HOST_ADDRESS="192.168.77.1"',
            'USB_NETMASK="255.255.255.0"',
            'USB_PREFIX="24"',
            "/sbin/ifconfig",
            "/sbin/ip -4 addr show",
        ):
            self.assertIn(expected, self.script)

    def test_dhcp_is_private_nonfatal_and_has_static_fallback(self) -> None:
        for expected in (
            'DNSMASQ="/usr/sbin/dnsmasq"',
            'DNSMASQ_PID="/var/run/casescreen-usb-dnsmasq.pid"',
            "--conf-file=/dev/null",
            "--bind-interfaces",
            "--dhcp-lease-max=1",
            "Windows RNDIS 网卡手动设置",
            "网关和 DNS 留空",
        ):
            self.assertIn(expected, self.script)

        # 陈旧 PID 文件不得导致脚本停止无关进程。
        self.assertIn('grep -qs "$DNSMASQ_PID" "/proc/$dnsmasq_pid/cmdline"', self.script)
        self.assertIn("拒绝停止", self.script)

        # DHCP 尝试发生在回滚标记清除后，所以 DHCP 失败不会破坏已工作的 COM/RNDIS。
        clear_rollback = self.script.index("ROLLBACK_REQUIRED=0", self.script.index("start_composite()"))
        dhcp_attempt = self.script.index('start_private_dnsmasq "$usb_interface"', clear_rollback)
        self.assertLess(clear_rollback, dhcp_attempt)

    def test_forbids_destructive_or_unscoped_commands(self) -> None:
        lowered = self.script.lower()
        for forbidden in (
            "rm -rf",
            "killall",
            "pkill",
            "mkfs",
            "parted",
            "sgdisk",
            "dd if=",
            ".img",
            "microphone",
            "麦克风",
            "录音",
            "声纹",
        ):
            self.assertNotIn(forbidden, lowered)

    def test_documentation_states_real_limitations_and_recovery(self) -> None:
        for expected in (
            "仓库中的原始 IMG 没有被修改",
            "不能把 DHCP 自动成功当作既定事实",
            "192.168.77.1/24",
            "网关：留空",
            "/data/serial_uart.sh restore-acm",
            "/upper/data/serial_uart.sh",
            "127.0.0.1:19321",
        ):
            self.assertIn(expected, self.readme)

    @unittest.skipIf(sys.platform == "win32" or shutil.which("sh") is None, "native POSIX sh is unavailable")
    def test_shell_syntax(self) -> None:
        completed = subprocess.run(
            ["sh", "-n", str(SCRIPT)],
            check=False,
            capture_output=True,
            text=True,
            encoding="utf-8",
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)


if __name__ == "__main__":
    unittest.main()
