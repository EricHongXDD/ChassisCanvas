from __future__ import annotations

import logging
import copy
from concurrent.futures import ThreadPoolExecutor

from PIL.ImageQt import ImageQt
from PySide6.QtCore import QObject, Qt, QTimer, Signal
from PySide6.QtGui import QImage, QPixmap
from PySide6.QtWidgets import (
    QFrame,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QScrollArea,
    QSlider,
    QTabWidget,
    QToolButton,
    QVBoxLayout,
    QWidget,
)

from ..devices.base import DeviceError
from ..devices.discovery import discover_devices
from ..devices.factory import create_adapter
from ..models import DeviceInfo, DeviceKind, ProtocolState
from ..rendering import render_dashboard
from ..telemetry import TelemetryCollector
from .api_client import CaseScreenApi, LocalServiceProcess
from .lyrics_settings import LyricsSettings
from .theme_editor import ThemeEditor

log = logging.getLogger(__name__)


class _Bridge(QObject):
    result = Signal(str, bool, str)


class DeviceCard(QFrame):
    send_requested = Signal(object)

    def __init__(self, info: DeviceInfo) -> None:
        super().__init__()
        self.info = info
        self.setObjectName("deviceCard")
        layout = QVBoxLayout(self)
        layout.setContentsMargins(18, 15, 18, 15)
        layout.setSpacing(7)

        title_row = QHBoxLayout()
        title = QLabel(info.name)
        title.setObjectName("cardTitle")
        title_row.addWidget(title, 1)
        service_offline = bool(info.metadata.get("service")) and not bool(info.metadata.get("connected"))
        state_text = "等待连接" if service_offline else ("协议就绪" if info.protocol_state is ProtocolState.READY else "待完成协议")
        self.state = QLabel(state_text)
        self.state.setObjectName("pendingChip" if service_offline or info.protocol_state is not ProtocolState.READY else "readyChip")
        title_row.addWidget(self.state)
        layout.addLayout(title_row)

        if info.metadata.get("service"):
            transport_text = "统一 Go 协议服务"
        else:
            transport_text = f"USB  {info.usb_id}    ·    {info.transport}"
        layout.addWidget(QLabel(transport_text))
        layout.addWidget(QLabel(f"画布  {info.resolution[0]} × {info.resolution[1]}"))
        self.action = QPushButton("发送当前监控画面")
        self.action.setEnabled(info.protocol_state is ProtocolState.READY)
        self.action.clicked.connect(lambda: self.send_requested.emit(self.info))
        layout.addWidget(self.action)

    def set_busy(self, busy: bool) -> None:
        self.action.setDisabled(busy or self.info.protocol_state is not ProtocolState.READY)
        if busy:
            self.action.setText("正在发送…")
        else:
            self.action.setText("发送当前监控画面")


class MainWindow(QMainWindow):
    def __init__(self) -> None:
        super().__init__()
        self.setWindowTitle("机箱副屏统一控制器")
        self.resize(1280, 780)
        self._collector = TelemetryCollector()
        self._snapshot = self._collector.capture()
        self._devices: list[DeviceInfo] = []
        self._cards: dict[str, DeviceCard] = {}
        self._executor = ThreadPoolExecutor(max_workers=3, thread_name_prefix="display")
        self._api = CaseScreenApi()
        self._service_process = LocalServiceProcess(self._api)
        self._service_available = self._service_process.ensure()
        self._bridge = _Bridge()
        self._bridge.result.connect(self._on_send_result)
        self._build_ui()
        self._apply_style()
        self._refresh_devices()

        self._timer = QTimer(self)
        self._timer.timeout.connect(self._update_telemetry)
        self._timer.start(1000)

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root = QHBoxLayout(central)
        root.setContentsMargins(0, 0, 0, 0)
        root.setSpacing(0)

        sidebar = QFrame()
        sidebar.setObjectName("sidebar")
        sidebar.setFixedWidth(230)
        side = QVBoxLayout(sidebar)
        side.setContentsMargins(24, 30, 24, 24)
        logo = QLabel("CASE\nSCREEN")
        logo.setObjectName("logo")
        side.addWidget(logo)
        side.addSpacing(30)
        nav = []
        for text in ("总览", "设备", "主题工作台", "歌词接入"):
            button = QToolButton()
            button.setText(text)
            button.setObjectName("navActive" if text == "总览" else "nav")
            button.setToolButtonStyle(Qt.ToolButtonStyle.ToolButtonTextOnly)
            nav.append(button)
            side.addWidget(button)
        side.addStretch(1)
        version = QLabel("统一协议层  v0.1.0")
        version.setObjectName("muted")
        side.addWidget(version)
        root.addWidget(sidebar)

        content = QWidget()
        content_layout = QVBoxLayout(content)
        content_layout.setContentsMargins(28, 24, 28, 24)
        content_layout.setSpacing(18)

        header = QHBoxLayout()
        heading = QVBoxLayout()
        title = QLabel("统一控制台")
        title.setObjectName("pageTitle")
        subtitle = QLabel("一个主题、一次采集，同时驱动三种 USB 机箱屏")
        subtitle.setObjectName("muted")
        heading.addWidget(title)
        heading.addWidget(subtitle)
        header.addLayout(heading)
        header.addStretch(1)
        refresh = QPushButton("重新扫描设备")
        refresh.clicked.connect(self._refresh_devices)
        header.addWidget(refresh)
        content_layout.addLayout(header)

        self.tabs = QTabWidget()
        self.tabs.setDocumentMode(True)
        overview = QWidget()
        overview_layout = QHBoxLayout(overview)
        overview_layout.setSpacing(18)
        left = QVBoxLayout()
        left.addWidget(QLabel("已连接设备"))
        self.device_container = QWidget()
        self.device_layout = QVBoxLayout(self.device_container)
        self.device_layout.setContentsMargins(0, 0, 0, 0)
        self.device_layout.setSpacing(12)
        self.device_layout.addStretch(1)
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setFrameShape(QFrame.Shape.NoFrame)
        scroll.setWidget(self.device_container)
        left.addWidget(scroll, 1)
        overview_layout.addLayout(left, 3)
        right = QVBoxLayout()
        right.addWidget(QLabel("统一主题预览"))
        self.preview = QLabel()
        self.preview.setObjectName("preview")
        self.preview.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self.preview.setMinimumSize(500, 294)
        right.addWidget(self.preview)
        control = QFrame()
        control.setObjectName("controlPanel")
        control_layout = QVBoxLayout(control)
        control_layout.addWidget(QLabel("全局亮度（协议完成后同步应用）"))
        brightness = QSlider(Qt.Orientation.Horizontal)
        brightness.setRange(10, 100)
        brightness.setValue(70)
        control_layout.addWidget(brightness)
        self.status = QLabel("正在扫描设备…")
        self.status.setObjectName("muted")
        control_layout.addWidget(self.status)
        right.addWidget(control)
        right.addStretch(1)
        overview_layout.addLayout(right, 4)
        self.tabs.addTab(overview, "总览")

        self.editor = ThemeEditor(self._api)
        self.editor.message.connect(self._on_editor_message)
        self.tabs.addTab(self.editor, "主题工作台")
        self.lyrics = LyricsSettings(self._api)
        self.lyrics.message.connect(self._on_editor_message)
        self.tabs.addTab(self.lyrics, "歌词接入")
        content_layout.addWidget(self.tabs, 1)
        root.addWidget(content, 1)
        nav[0].clicked.connect(lambda: self.tabs.setCurrentIndex(0))
        nav[1].clicked.connect(lambda: self.tabs.setCurrentIndex(0))
        nav[2].clicked.connect(lambda: self.tabs.setCurrentIndex(1))
        nav[3].clicked.connect(lambda: self.tabs.setCurrentIndex(2))

    def _apply_style(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow, QWidget { background: #0b0f19; color: #e9eefb; font-family: "Microsoft YaHei UI"; font-size: 13px; }
            #sidebar { background: #111727; border-right: 1px solid #222d43; }
            #logo { color: #5ddeff; font-size: 25px; font-weight: 800; letter-spacing: 2px; }
            #nav, #navActive { padding: 12px 14px; border-radius: 8px; color: #8f9cb4; }
            #navActive { background: #1a2940; color: #68ddff; font-weight: 700; }
            #pageTitle { font-size: 27px; font-weight: 800; }
            #muted { color: #8290a9; }
            #deviceCard, #controlPanel { background: #141c2c; border: 1px solid #25324a; border-radius: 12px; }
            #cardTitle { font-size: 15px; font-weight: 700; }
            #readyChip, #pendingChip { border-radius: 9px; padding: 4px 8px; font-size: 11px; }
            #readyChip { background: #143e3d; color: #74f1d1; }
            #pendingChip { background: #3a3020; color: #ffd47d; }
            #preview { background: #080c15; border: 1px solid #25324a; border-radius: 12px; padding: 8px; }
            #workbenchPane, #canvasPane { background: #101a25; border: 1px solid #25384a; border-radius: 10px; }
            #workbenchTitle { color: #7de7e5; font-size: 20px; font-weight: 800; }
            #editorStatus { color: #78e2d4; padding: 5px; }
            QTabWidget::pane { border: 0; }
            QTabBar::tab { background: #121b2a; color: #8f9cb4; padding: 10px 18px; margin-right: 4px; border-radius: 6px; }
            QTabBar::tab:selected { background: #1a3547; color: #7de7e5; }
            QTreeWidget, QListWidget, QLineEdit, QDoubleSpinBox, QComboBox, QSpinBox { background: #0c141e; color: #e9eefb; border: 1px solid #294258; border-radius: 5px; padding: 5px; }
            QListWidget::item { padding: 11px 8px; border-bottom: 1px solid #1d2d3b; }
            QListWidget::item:selected { background: #174552; color: #8efbf0; }
            QToolButton#nav, QToolButton#navActive { border: 0; text-align: left; padding: 12px 14px; border-radius: 8px; color: #8f9cb4; }
            QToolButton#navActive { background: #1a2940; color: #68ddff; font-weight: 700; }
            QPushButton { background: #1e3554; color: #dff8ff; border: 1px solid #315579; border-radius: 8px; padding: 9px 14px; }
            QPushButton:hover { background: #28486e; }
            QPushButton:disabled { background: #171e2b; color: #58657a; border-color: #273143; }
            QScrollArea { background: transparent; }
            """
        )

    def _clear_cards(self) -> None:
        while self.device_layout.count() > 1:
            item = self.device_layout.takeAt(0)
            if item.widget() is not None:
                item.widget().deleteLater()
        self._cards.clear()

    def _refresh_devices(self) -> None:
        self.status.setText("正在扫描 USB 设备…")
        self._devices = self._service_devices()
        if not self._devices:
            self._devices = discover_devices()
        self.editor.refresh_targets()
        self._clear_cards()
        for info in self._devices:
            card = DeviceCard(info)
            card.send_requested.connect(self._send_to_device)
            self.device_layout.insertWidget(self.device_layout.count() - 1, card)
            self._cards[info.key] = card
        ready = sum(item.protocol_state is ProtocolState.READY for item in self._devices)
        self.status.setText(f"发现 {len(self._devices)} 台屏幕，其中 {ready} 台由统一协议服务接管")
        self._update_preview()

    def _service_devices(self) -> list[DeviceInfo]:
        """优先使用统一 Go 服务的设备状态，Python 仅作为离线诊断回退。"""
        try:
            if not self._api.token:
                self._api.connect()
            status = self._api.status()
        except Exception:
            return []
        devices: list[DeviceInfo] = []
        kind_map = {
            "waveshare": DeviceKind.WAVESHARE,
            "coolermaster": DeviceKind.COOLER_MASTER,
        }
        for item in status.get("devices", []):
            info = item.get("info") or {}
            device_id = str(info.get("id", "")).strip()
            resolution = info.get("resolution") or [1024, 600]
            if not device_id or len(resolution) != 2:
                continue
            kind_text = device_id.split("-", 1)[0].lower()
            kind = kind_map.get(kind_text)
            if kind is None:
                # 旧服务可能返回已移除或无法识别的设备，不再为它创建控制卡片。
                continue
            devices.append(
                DeviceInfo(
                    key=device_id,
                    kind=kind,
                    name=str(info.get("name", device_id)),
                    vid=0,
                    pid=0,
                    transport="service",
                    resolution=(int(resolution[0]), int(resolution[1])),
                    protocol_state=ProtocolState.READY,
                    metadata={"service": True, "service_id": device_id, "connected": bool(item.get("connected"))},
                )
            )
        return devices

    def _update_telemetry(self) -> None:
        self._snapshot = self._collector.capture()
        self._update_preview()

    def _update_preview(self) -> None:
        image = render_dashboard(self._snapshot, (1024, 600))
        qimage = QImage(ImageQt(image))
        pixmap = QPixmap.fromImage(qimage)
        self.preview.setPixmap(
            pixmap.scaled(
                self.preview.size(),
                Qt.AspectRatioMode.KeepAspectRatio,
                Qt.TransformationMode.SmoothTransformation,
            )
        )

    def _send_to_device(self, info: DeviceInfo) -> None:
        card = self._cards.get(info.key)
        if card is not None:
            card.set_busy(True)
        snapshot = self._snapshot
        service_id = str(info.metadata.get("service_id", ""))
        manifest = copy.deepcopy(self.editor.manifest) if service_id else None

        def worker() -> None:
            try:
                if service_id and manifest is not None:
                    if not self._api.token:
                        self._api.connect()
                    result = self._api.apply_theme(manifest, service_id)
                    if int(result.get("successes", 0)) <= 0 and result.get("device_results"):
                        raise RuntimeError("；".join(f"{key}: {value}" for key, value in result["device_results"].items()))
                    self._bridge.result.emit(info.key, True, "主题已通过统一协议服务应用")
                    return
                image = render_dashboard(snapshot, info.resolution, info.name)
                adapter = create_adapter(info)
                adapter.connect()
                adapter.send_image(image)
                adapter.disconnect()
                self._bridge.result.emit(info.key, True, "画面发送成功")
            except Exception as exc:
                log.exception("发送画面失败")
                self._bridge.result.emit(info.key, False, str(exc))

        self._executor.submit(worker)

    def _on_send_result(self, key: str, ok: bool, message: str) -> None:
        card = self._cards.get(key)
        if card is not None:
            card.set_busy(False)
        self.status.setText(message)
        if not ok:
            QMessageBox.warning(self, "设备通信失败", message)

    def _on_editor_message(self, message: str, ok: bool) -> None:
        self.status.setText(message)
        self.editor.editor_status.setText(message)
        if not ok:
            QMessageBox.warning(self, "主题/歌词设置", message)

    def closeEvent(self, event) -> None:  # noqa: N802
        self._executor.shutdown(wait=False, cancel_futures=True)
        self._service_process.close()
        super().closeEvent(event)
