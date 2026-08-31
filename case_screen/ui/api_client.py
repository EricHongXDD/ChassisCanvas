from __future__ import annotations

import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


class CaseScreenApiError(RuntimeError):
    """本地 Go 服务返回的可读错误。"""


class CaseScreenApi:
    """桌面工作台使用的本机 API 客户端，不把设备协议复制到 Python。"""

    def __init__(self, base_url: str = "http://127.0.0.1:19321") -> None:
        self.base_url = base_url.rstrip("/")
        self.token = ""

    def _request(self, path: str, method: str = "GET", body: Any | None = None, *, auth: bool = False) -> Any:
        headers = {"Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = "application/json; charset=utf-8"
            payload = json.dumps(body, ensure_ascii=False).encode("utf-8")
        else:
            payload = None
        if auth and self.token:
            headers["X-CaseScreen-Token"] = self.token
        request = urllib.request.Request(self.base_url + path, data=payload, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=4) as response:
                raw = response.read().decode("utf-8")
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")
            raise CaseScreenApiError(detail or f"HTTP {exc.code}") from exc
        except (urllib.error.URLError, TimeoutError) as exc:
            raise CaseScreenApiError(f"无法连接 CaseScreen 服务：{exc}") from exc
        try:
            return json.loads(raw) if raw else {}
        except json.JSONDecodeError as exc:
            raise CaseScreenApiError("服务返回了无效 JSON") from exc

    def connect(self) -> dict[str, Any]:
        session = self._request("/api/session")
        self.token = str(session.get("token", ""))
        if not self.token:
            raise CaseScreenApiError("服务未返回桌面客户端令牌")
        return session

    def status(self) -> dict[str, Any]:
        return self._request("/api/status")

    def config(self) -> dict[str, Any]:
        return self._request("/api/config")

    def save_config(self, config: dict[str, Any]) -> dict[str, Any]:
        return self._request("/api/config", "PUT", config, auth=True)

    def theme(self, target: str = "all") -> dict[str, Any]:
        suffix = "" if target in ("", "all") else f"?target={urllib.parse.quote(target)}"
        return self._request("/api/theme" + suffix)

    def apply_theme(self, manifest: dict[str, Any], target: str = "all") -> dict[str, Any]:
        suffix = "" if target in ("", "all") else f"?target={urllib.parse.quote(target)}"
        return self._request("/api/theme" + suffix, "PUT", manifest, auth=True)


class LocalServiceProcess:
    """桌面端启动时复用现有服务；找不到 Go 可执行文件时仍允许离线编辑主题。"""

    def __init__(self, api: CaseScreenApi) -> None:
        self.api = api
        self.process: subprocess.Popen | None = None

    def ensure(self) -> bool:
        try:
            self.api.connect()
            return True
        except CaseScreenApiError:
            pass
        candidates = []
        executable_directory = os.path.dirname(sys.executable)
        candidates.extend(
            [
                os.path.join(executable_directory, "CaseScreen.exe"),
                os.path.join(os.getcwd(), "CaseScreen.exe"),
                os.path.join(os.getcwd(), "artifacts", "CaseScreen.exe"),
                os.path.abspath(os.path.join(os.getcwd(), "..", "CaseScreen.exe")),
            ]
        )
        executable = next((path for path in candidates if os.path.isfile(path)), None)
        if not executable:
            return False
        creation_flags = getattr(subprocess, "CREATE_NO_WINDOW", 0)
        try:
            self.process = subprocess.Popen([executable, "-headless"], creationflags=creation_flags)
        except OSError:
            return False
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            try:
                self.api.connect()
                return True
            except CaseScreenApiError:
                time.sleep(0.1)
        return False

    def close(self) -> None:
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.process.kill()
