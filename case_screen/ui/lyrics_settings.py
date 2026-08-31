from __future__ import annotations

from PySide6.QtCore import Signal
from PySide6.QtWidgets import QCheckBox, QFileDialog, QFormLayout, QFrame, QHBoxLayout, QLabel, QLineEdit, QPushButton, QSpinBox, QVBoxLayout

from .api_client import CaseScreenApi, CaseScreenApiError


class LyricsSettings(QFrame):
    """歌词接入设置：自动媒体会话、固定提供方和本地 LRC 路径。"""

    message = Signal(str, bool)

    def __init__(self, api: CaseScreenApi | None = None) -> None:
        super().__init__()
        self.api = api or CaseScreenApi()
        self._build_ui()

    def _build_ui(self) -> None:
        root = QVBoxLayout(self)
        root.setContentsMargins(22, 18, 22, 22)
        title = QLabel("歌词接入")
        title.setObjectName("workbenchTitle")
        root.addWidget(title)
        root.addWidget(QLabel("歌词图层从这里读取设置；主题中的 LRC 歌词组件会自动使用当前配置。"))
        form = QFormLayout()
        self.enabled = QCheckBox("启用歌词同步")
        self.enabled.setChecked(True)
        form.addRow("运行状态", self.enabled)
        from PySide6.QtWidgets import QComboBox

        self.provider = QComboBox()
        self.provider.addItem("自动（严格使用播放器原生 ID）", "auto")
        self.provider.addItem("网易云", "netease")
        self.provider.addItem("QQ 音乐", "qq")
        self.provider.addItem("本地 LRC", "local")
        form.addRow("歌词来源", self.provider)
        path_row = QHBoxLayout()
        self.path = QLineEdit()
        self.path.setPlaceholderText("可选：固定本地 .lrc 文件")
        browse = QPushButton("选择文件")
        browse.clicked.connect(self._browse)
        path_row.addWidget(self.path, 1)
        path_row.addWidget(browse)
        form.addRow("本地 LRC", path_row)
        self.interval = QSpinBox()
        self.interval.setRange(250, 60000)
        self.interval.setSingleStep(250)
        self.interval.setValue(1000)
        form.addRow("媒体检测间隔", self.interval)
        root.addLayout(form)
        buttons = QHBoxLayout()
        load = QPushButton("读取当前设置")
        load.clicked.connect(self.load)
        save = QPushButton("保存歌词设置")
        save.clicked.connect(self.save)
        buttons.addWidget(load)
        buttons.addWidget(save)
        buttons.addStretch(1)
        root.addLayout(buttons)
        root.addStretch(1)
        self.hint = QLabel("本地服务未连接时，设置仍可保留在当前界面；连接服务后点击保存即可写入统一控制器。")
        self.hint.setObjectName("muted")
        root.addWidget(self.hint)

    def _browse(self) -> None:
        path, _ = QFileDialog.getOpenFileName(self, "选择 LRC 歌词", "", "LRC 歌词 (*.lrc)")
        if path:
            self.path.setText(path)
            self.provider.setCurrentIndex(self.provider.findData("local"))

    def load(self) -> None:
        try:
            if not self.api.token:
                self.api.connect()
            config = self.api.config()
            self.enabled.setChecked(bool(config.get("lyrics_enabled", True)))
            self.provider.setCurrentIndex(max(0, self.provider.findData(config.get("lyrics_provider", "auto"))))
            self.path.setText(str(config.get("lyrics_path", "")))
            self.interval.setValue(int(config.get("telemetry_interval_ms", 1000)))
            self.message.emit("已读取歌词设置。", True)
        except CaseScreenApiError as exc:
            self.message.emit(str(exc), False)

    def save(self) -> None:
        try:
            if not self.api.token:
                self.api.connect()
            config = self.api.config()
            config["lyrics_enabled"] = self.enabled.isChecked()
            config["lyrics_provider"] = self.provider.currentData() or "auto"
            config["lyrics_path"] = self.path.text().strip()
            config["telemetry_interval_ms"] = self.interval.value()
            self.api.save_config(config)
            self.message.emit("歌词设置已保存。", True)
        except CaseScreenApiError as exc:
            self.message.emit(str(exc), False)
