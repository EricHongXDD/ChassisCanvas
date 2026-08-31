from __future__ import annotations

import copy
import hashlib
import json
import os
from pathlib import Path
from typing import Any

from PySide6.QtCore import QMimeData, QPointF, QRectF, Qt, Signal
from PySide6.QtGui import QColor, QDrag, QImage, QPainter, QPen
from PySide6.QtWidgets import (
    QColorDialog,
    QComboBox,
    QFileDialog,
    QFormLayout,
    QFrame,
    QDoubleSpinBox,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QListWidget,
    QListWidgetItem,
    QPushButton,
    QSplitter,
    QSpinBox,
    QTreeWidget,
    QTreeWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .api_client import CaseScreenApi, CaseScreenApiError
from .geometry import clamp, move_bounds, resize_bounds


LAYER_LABELS = {
    "background_image": "背景图片",
    "background_video": "背景视频",
    "telemetry": "遥测",
    "audio_bars": "条状频谱",
    "audio_ring": "环状频谱",
    "lyrics": "LRC 歌词",
}
PALETTE = (
    ("background_image", "背景图片"),
    ("telemetry", "遥测数据"),
    ("audio_bars", "条状频谱"),
    ("audio_ring", "环状频谱"),
    ("lyrics", "LRC 歌词"),
)


def default_layer(layer_type: str, index: int) -> dict[str, Any]:
    defaults: dict[str, tuple[float, float, float, float]] = {
        "background_image": (0.0, 0.0, 1.0, 1.0),
        "background_video": (0.0, 0.0, 1.0, 1.0),
        "telemetry": (0.05, 0.08, 0.32, 0.22),
        "audio_bars": (0.08, 0.70, 0.84, 0.22),
        "audio_ring": (0.70, 0.08, 0.22, 0.34),
        "lyrics": (0.12, 0.80, 0.76, 0.13),
    }
    x, y, width, height = defaults.get(layer_type, defaults["telemetry"])
    return {
        "id": f"{layer_type}-{index + 1}",
        "type": layer_type,
        "bounds": {"x": x, "y": y, "width": width, "height": height},
        "opacity": 1.0 if layer_type.startswith("background") else 0.92,
        "z_index": index * 10,
        "options": {"color": "#46e5ffff"},
    }


def default_manifest() -> dict[str, Any]:
    layers = [default_layer("background_image", 0), default_layer("telemetry", 1), default_layer("audio_ring", 2), default_layer("lyrics", 3)]
    layers[0]["id"] = "background"
    layers[0]["options"] = {"color": "#0b1722ff"}
    layers[1]["id"] = "telemetry"
    layers[2]["id"] = "audio-ring"
    layers[2]["options"] = {"color": "#ff70cfff", "thickness": 4}
    layers[3]["id"] = "lyrics"
    layers[3]["options"] = {"color": "#ffffffff", "font_size": 24, "panel_color": "#00000070"}
    return {"id": "desktop-workbench", "name": "桌面工作台主题", "width": 1024, "height": 600, "resources": [], "layers": layers}


class ComponentPalette(QListWidget):
    """左侧可拖拽组件库；使用 MIME 类型而不是依赖像素坐标。"""

    componentDropped = Signal(str, QPointF)

    def __init__(self) -> None:
        super().__init__()
        self.setDragEnabled(True)
        self.setSelectionMode(QListWidget.SelectionMode.SingleSelection)
        self.setAlternatingRowColors(False)
        self.setMinimumWidth(170)
        for kind, label in PALETTE:
            item = QListWidgetItem(label)
            item.setData(Qt.ItemDataRole.UserRole, kind)
            item.setToolTip("拖到中央屏幕画布以添加组件")
            self.addItem(item)

    def startDrag(self, supported_actions: Qt.DropAction) -> None:  # noqa: N802
        item = self.currentItem()
        if item is None:
            return
        mime = QMimeData()
        mime.setData("application/x-casescreen-layer", str(item.data(Qt.ItemDataRole.UserRole)).encode("utf-8"))
        drag = QDrag(self)
        drag.setMimeData(mime)
        drag.exec(Qt.DropAction.CopyAction)


class ThemeCanvas(QWidget):
    layerSelected = Signal(int)
    layerChanged = Signal()
    layerCommitted = Signal()
    layerDropped = Signal(str, QPointF)

    def __init__(self) -> None:
        super().__init__()
        self.setAcceptDrops(True)
        self.setMinimumSize(600, 360)
        self.setMouseTracking(True)
        self.manifest: dict[str, Any] | None = None
        self.selected = -1
        self._dragging = False
        self._resizing = False
        self._start = QPointF()
        self._start_bounds: dict[str, float] = {}
        self._background: QImage | None = None
        self._background_scaled: QImage | None = None
        self._background_scale_key: tuple[int, int] | None = None

    def set_manifest(self, manifest: dict[str, Any]) -> None:
        self.manifest = manifest
        self._background = None
        self._background_scaled = None
        self._background_scale_key = None
        self.update()

    def set_background(self, path: str) -> bool:
        image = QImage(path)
        self._background = image if not image.isNull() else None
        self._background_scaled = None
        self._background_scale_key = None
        self.update()
        return self._background is not None

    def resizeEvent(self, event) -> None:  # noqa: N802
        self._background_scaled = None
        self._background_scale_key = None
        super().resizeEvent(event)

    def _canvas_rect(self) -> QRectF:
        margin = 18.0
        available = QRectF(margin, margin, max(1.0, self.width() - margin * 2), max(1.0, self.height() - margin * 2))
        ratio = (self.manifest or {}).get("width", 1024) / max(1, (self.manifest or {}).get("height", 600))
        if available.width() / max(1.0, available.height()) > ratio:
            height = available.height()
            width = height * ratio
            return QRectF(available.center().x() - width / 2, available.top(), width, height)
        width = available.width()
        height = width / max(0.01, ratio)
        return QRectF(available.left(), available.center().y() - height / 2, width, height)

    def _bounds_rect(self, layer: dict[str, Any]) -> QRectF:
        canvas = self._canvas_rect()
        bounds = layer.setdefault("bounds", {})
        return QRectF(canvas.left() + float(bounds.get("x", 0)) * canvas.width(), canvas.top() + float(bounds.get("y", 0)) * canvas.height(), float(bounds.get("width", 0.2)) * canvas.width(), float(bounds.get("height", 0.2)) * canvas.height())

    def _hit_test(self, point: QPointF) -> int:
        if not self.manifest:
            return -1
        layers = self.manifest.get("layers", [])
        order = sorted(range(len(layers)), key=lambda index: int(layers[index].get("z_index", 0)), reverse=True)
        for index in order:
            if self._bounds_rect(layers[index]).contains(point) and float(layers[index].get("opacity", 1)) > 0.01:
                return index
        return -1

    def paintEvent(self, event) -> None:  # noqa: N802
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor("#071018"))
        canvas = self._canvas_rect()
        painter.fillRect(canvas, QColor("#0d1922"))
        painter.setPen(QPen(QColor("#2d5362"), 1, Qt.PenStyle.DashLine))
        painter.drawRect(canvas)
        if not self.manifest:
            return
        layers = self.manifest.get("layers", [])
        for index in sorted(range(len(layers)), key=lambda item: int(layers[item].get("z_index", 0))):
            layer = layers[index]
            if float(layer.get("opacity", 1)) <= 0.01:
                continue
            rect = self._bounds_rect(layer)
            opacity = clamp(float(layer.get("opacity", 1)), 0, 1)
            painter.save()
            painter.setOpacity(opacity)
            layer_type = layer.get("type", "telemetry")
            options = layer.get("options") or {}
            if layer_type.startswith("background"):
                if self._background is not None and layer_type == "background_image":
                    scale_key = (max(1, round(rect.width())), max(1, round(rect.height())))
                    if self._background_scaled is None or self._background_scale_key != scale_key:
                        self._background_scaled = self._background.scaled(scale_key[0], scale_key[1], Qt.AspectRatioMode.IgnoreAspectRatio, Qt.TransformationMode.SmoothTransformation)
                        self._background_scale_key = scale_key
                    painter.drawImage(rect, self._background_scaled)
                else:
                    painter.fillRect(rect, QColor(str(options.get("color", "#0b1722"))[:7]))
                painter.setPen(QPen(QColor("#effaff"), 1))
                painter.drawText(rect.adjusted(12, 8, -8, -8), Qt.AlignmentFlag.AlignTop | Qt.AlignmentFlag.AlignLeft, "背景 · 屏端资源")
            elif layer_type == "telemetry":
                painter.setBrush(QColor("#08121ccc"))
                painter.setPen(QPen(QColor("#3e7280"), 1))
                painter.drawRoundedRect(rect, 8, 8)
                painter.setPen(QColor("#f4fbff"))
                painter.drawText(rect.adjusted(14, 10, -8, -8), Qt.AlignmentFlag.AlignTop | Qt.AlignmentFlag.AlignLeft, "CPU 42%\nMEM 38%")
                painter.setPen(QColor("#82e6de"))
                painter.drawText(rect.adjusted(14, 0, -8, -10), Qt.AlignmentFlag.AlignBottom | Qt.AlignmentFlag.AlignLeft, "实时遥测")
            elif layer_type == "audio_bars":
                painter.setPen(Qt.PenStyle.NoPen)
                painter.setBrush(QColor(str(options.get("color", "#46e5ff"))[:7]))
                count = 16
                gap = max(2.0, rect.width() * 0.012)
                bar_width = max(2.0, (rect.width() - gap * (count - 1)) / count)
                for bar in range(count):
                    height = rect.height() * (0.2 + 0.7 * ((bar * 17) % 11) / 10)
                    painter.drawRoundedRect(QRectF(rect.left() + bar * (bar_width + gap), rect.bottom() - height, bar_width, height), 2, 2)
            elif layer_type == "audio_ring":
                color = QColor(str(options.get("color", "#ff70cf"))[:7])
                painter.setBrush(Qt.BrushStyle.NoBrush)
                painter.setPen(QPen(color, max(2.0, float(options.get("thickness", 4)))))
                painter.drawEllipse(rect.center(), min(rect.width(), rect.height()) * 0.34, min(rect.width(), rect.height()) * 0.34)
                painter.setPen(QPen(QColor(color), 1, Qt.PenStyle.DotLine))
                painter.drawEllipse(rect.center(), min(rect.width(), rect.height()) * 0.46, min(rect.width(), rect.height()) * 0.46)
            elif layer_type == "lyrics":
                painter.setBrush(QColor("#071019b8"))
                painter.setPen(QPen(QColor("#486779"), 1))
                painter.drawRoundedRect(rect, 8, 8)
                painter.setPen(QColor("#ffffff"))
                painter.drawText(rect.adjusted(14, 0, -14, -4), Qt.AlignmentFlag.AlignVCenter, "LRC 歌词 · 当前句预览")
            painter.restore()
        if 0 <= self.selected < len(layers):
            selected = layers[self.selected]
            rect = self._bounds_rect(selected)
            painter.setPen(QPen(QColor("#62f6e4"), 2, Qt.PenStyle.DashLine))
            painter.setBrush(QColor("#62f6e4"))
            painter.drawRect(rect)
            painter.drawRect(QRectF(rect.right() - 9, rect.bottom() - 9, 9, 9))

    def dragEnterEvent(self, event) -> None:  # noqa: N802
        if event.mimeData().hasFormat("application/x-casescreen-layer"):
            event.acceptProposedAction()

    def dropEvent(self, event) -> None:  # noqa: N802
        if not event.mimeData().hasFormat("application/x-casescreen-layer"):
            return
        value = bytes(event.mimeData().data("application/x-casescreen-layer")).decode("utf-8")
        canvas = self._canvas_rect()
        position = QPointF(clamp((event.position().x() - canvas.left()) / canvas.width(), 0, 1), clamp((event.position().y() - canvas.top()) / canvas.height(), 0, 1))
        self.layerDropped.emit(value, position)
        event.acceptProposedAction()

    def mousePressEvent(self, event) -> None:  # noqa: N802
        if event.button() != Qt.MouseButton.LeftButton or not self.manifest:
            return
        point = event.position()
        hit = self._hit_test(point)
        if hit < 0:
            self.selected = -1
            self.layerSelected.emit(-1)
            self.update()
            return
        self.selected = hit
        self.layerSelected.emit(hit)
        rect = self._bounds_rect(self.manifest["layers"][hit])
        self._resizing = rect.adjusted(-12, -12, 12, 12).contains(point) and point.x() > rect.center().x() and point.y() > rect.center().y()
        self._dragging = not self._resizing
        self._start = point
        self._start_bounds = copy.deepcopy(self.manifest["layers"][hit].get("bounds", {}))
        self.grabMouse()
        self.update()

    def mouseMoveEvent(self, event) -> None:  # noqa: N802
        if not self.manifest or self.selected < 0 or (not self._dragging and not self._resizing):
            return
        canvas = self._canvas_rect()
        dx = (event.position().x() - self._start.x()) / max(1.0, canvas.width())
        dy = (event.position().y() - self._start.y()) / max(1.0, canvas.height())
        bounds = self.manifest["layers"][self.selected].setdefault("bounds", {})
        start = self._start_bounds
        next_bounds = resize_bounds(start, dx, dy) if self._resizing else move_bounds(start, dx, dy)
        bounds.update(next_bounds)
        self.layerChanged.emit()
        self.update()

    def mouseReleaseEvent(self, event) -> None:  # noqa: N802
        if self._dragging or self._resizing:
            self.releaseMouse()
            self.layerCommitted.emit()
        self._dragging = False
        self._resizing = False
        super().mouseReleaseEvent(event)


class ThemeEditor(QFrame):
    """工程化主题工作台：组件库、可拖动屏幕、属性编辑和 Photoshop 式图层。"""

    message = Signal(str, bool)

    def __init__(self, api: CaseScreenApi | None = None) -> None:
        super().__init__()
        self.api = api or CaseScreenApi()
        self.manifest = default_manifest()
        self.selected = 0
        self._property_visible = True
        self._build_ui()
        self._load_manifest(self.manifest)

    def _build_ui(self) -> None:
        root = QVBoxLayout(self)
        root.setContentsMargins(16, 14, 16, 16)
        root.setSpacing(10)
        toolbar = QHBoxLayout()
        title = QLabel("主题工程工作台")
        title.setObjectName("workbenchTitle")
        toolbar.addWidget(title)
        toolbar.addWidget(QLabel("目标副屏"))
        self.target = QComboBox()
        self.target.addItem("全部副屏（统一应用）", "all")
        toolbar.addWidget(self.target)
        toolbar.addStretch(1)
        self.template = QComboBox()
        self.template.addItems(["仪表盘模板", "频谱模板", "极简模板"])
        toolbar.addWidget(self.template)
        apply_template = QPushButton("套用模板")
        apply_template.clicked.connect(self._apply_template)
        toolbar.addWidget(apply_template)
        for text, callback in (("导入背景", self.import_background), ("读取当前", self.load_service_theme), ("新建", self._new_theme), ("打开 JSON", self._open_json), ("保存 JSON", self._save_json), ("应用到设备", self._apply_to_device)):
            button = QPushButton(text)
            button.clicked.connect(callback)
            toolbar.addWidget(button)
        root.addLayout(toolbar)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        splitter.setChildrenCollapsible(False)
        left = QFrame()
        left.setObjectName("workbenchPane")
        left_layout = QVBoxLayout(left)
        left_layout.addWidget(QLabel("组件库 · 拖到中央屏幕"))
        self.palette = ComponentPalette()
        left_layout.addWidget(self.palette, 1)
        left_layout.addWidget(QLabel("组件使用归一化坐标，拖动时不会因缩放产生跳变。"))
        splitter.addWidget(left)

        center = QFrame()
        center.setObjectName("canvasPane")
        center_layout = QVBoxLayout(center)
        canvas_header = QHBoxLayout()
        canvas_header.addWidget(QLabel("画布预览"))
        self.resolution = QLabel()
        self.resolution.setObjectName("muted")
        canvas_header.addWidget(self.resolution)
        canvas_header.addStretch(1)
        center_layout.addLayout(canvas_header)
        self.canvas = ThemeCanvas()
        center_layout.addWidget(self.canvas, 1)
        self.canvas_hint = QLabel("拖动组件移动；拖动右下角手柄调整大小。")
        self.canvas_hint.setObjectName("muted")
        center_layout.addWidget(self.canvas_hint)
        splitter.addWidget(center)

        right = QFrame()
        right.setObjectName("workbenchPane")
        right_layout = QVBoxLayout(right)
        self.property_header = QHBoxLayout()
        self.property_header.addWidget(QLabel("组件属性"))
        self.close_editor = QPushButton("关闭编辑")
        self.close_editor.clicked.connect(self._toggle_properties)
        self.property_header.addStretch(1)
        self.property_header.addWidget(self.close_editor)
        right_layout.addLayout(self.property_header)
        self.properties = QFrame()
        form = QFormLayout(self.properties)
        self.layer_id = QLineEdit()
        self.layer_type = QComboBox()
        self.layer_type.addItems(list(LAYER_LABELS))
        self.layer_x = self._number(0, 1)
        self.layer_y = self._number(0, 1)
        self.layer_w = self._number(0.01, 1)
        self.layer_h = self._number(0.01, 1)
        self.layer_opacity = self._number(0, 1)
        self.layer_color = QPushButton("选择颜色")
        self.layer_font_size = QSpinBox()
        self.layer_font_size.setRange(8, 200)
        self.layer_font_size.setValue(24)
        self.layer_thickness = self._number(1, 20)
        self.layer_thickness.setValue(4)
        self.layer_gap = self._number(0, 40)
        self.layer_gap.setValue(5)
        self.layer_panel_color = QLineEdit("#00000070")
        self.layer_panel_color.setPlaceholderText("例如 #00000070")
        form.addRow("图层 ID", self.layer_id)
        form.addRow("组件类型", self.layer_type)
        form.addRow("X", self.layer_x)
        form.addRow("Y", self.layer_y)
        form.addRow("宽度", self.layer_w)
        form.addRow("高度", self.layer_h)
        form.addRow("透明度", self.layer_opacity)
        form.addRow("颜色", self.layer_color)
        form.addRow("歌词字号", self.layer_font_size)
        form.addRow("环形粗细", self.layer_thickness)
        form.addRow("频谱间距", self.layer_gap)
        form.addRow("歌词面板色", self.layer_panel_color)
        right_layout.addWidget(self.properties)
        right_layout.addWidget(QLabel("图层 · 可见性与顺序"))
        self.layers = QTreeWidget()
        self.layers.setHeaderLabels(["名称", "层级"])
        self.layers.setRootIsDecorated(False)
        self.layers.itemSelectionChanged.connect(self._layer_selection_changed)
        self.layers.itemChanged.connect(self._layer_visibility_changed)
        right_layout.addWidget(self.layers, 1)
        layer_actions = QHBoxLayout()
        for text, callback in (("上移", lambda: self._move_layer(1)), ("下移", lambda: self._move_layer(-1)), ("复制", self._duplicate_layer), ("删除", self._delete_layer)):
            button = QPushButton(text)
            button.clicked.connect(callback)
            layer_actions.addWidget(button)
        right_layout.addLayout(layer_actions)
        splitter.addWidget(right)
        splitter.setSizes([190, 700, 340])
        root.addWidget(splitter, 1)
        self.editor_status = QLabel()
        self.editor_status.setObjectName("editorStatus")
        root.addWidget(self.editor_status)

        self.canvas.layerSelected.connect(self._select_layer)
        self.canvas.layerChanged.connect(self._canvas_changed)
        self.canvas.layerCommitted.connect(self._canvas_committed)
        self.canvas.layerDropped.connect(self._drop_component)
        for widget in (self.layer_id, self.layer_x, self.layer_y, self.layer_w, self.layer_h, self.layer_opacity):
            signal = widget.textChanged if isinstance(widget, QLineEdit) else widget.valueChanged
            signal.connect(self._property_changed)
        for widget in (self.layer_font_size, self.layer_thickness, self.layer_gap, self.layer_panel_color):
            signal = widget.textChanged if isinstance(widget, QLineEdit) else widget.valueChanged
            signal.connect(self._property_changed)
        self.layer_type.currentTextChanged.connect(self._property_changed)
        self.layer_color.clicked.connect(self._choose_color)

    @staticmethod
    def _number(minimum: float, maximum: float) -> QDoubleSpinBox:
        widget = QDoubleSpinBox()
        widget.setRange(minimum, maximum)
        widget.setDecimals(3)
        widget.setSingleStep(0.01)
        return widget

    def _load_manifest(self, manifest: dict[str, Any]) -> None:
        self.manifest = copy.deepcopy(manifest)
        self.manifest.setdefault("resources", [])
        self.manifest.setdefault("layers", [])
        self.canvas.set_manifest(self.manifest)
        background = next((layer for layer in self.manifest["layers"] if layer.get("type") == "background_image"), None)
        resource_id = (background or {}).get("options", {}).get("resource_id")
        resource = next((item for item in self.manifest["resources"] if item.get("id") == resource_id), None)
        if resource and resource.get("local_path") and Path(str(resource["local_path"])).exists():
            self.canvas.set_background(str(resource["local_path"]))
        self.resolution.setText(f"{self.manifest.get('width', 0)} × {self.manifest.get('height', 0)}")
        self._render_layers()
        self._select_layer(min(self.selected, len(self.manifest["layers"]) - 1))

    def refresh_targets(self) -> None:
        """从统一服务读取三种设备，避免桌面端重复实现 USB 枚举和协议。"""
        try:
            if not self.api.token:
                self.api.connect()
            status = self.api.status()
        except CaseScreenApiError:
            return
        current = self.target.currentData() or "all"
        self.target.blockSignals(True)
        self.target.clear()
        self.target.addItem("全部副屏（统一应用）", "all")
        for device in status.get("devices", []):
            info = device.get("info") or {}
            device_id = str(info.get("id", "")).strip()
            if not device_id:
                continue
            name = str(info.get("name", device_id))
            resolution = info.get("resolution") or []
            resolution_text = f" · {resolution[0]} × {resolution[1]}" if len(resolution) == 2 else ""
            state = "在线" if device.get("connected") else "离线"
            self.target.addItem(f"{name}{resolution_text} · {state}", device_id)
        index = self.target.findData(current)
        self.target.setCurrentIndex(max(0, index))
        self.target.blockSignals(False)

    def _render_layers(self) -> None:
        self.layers.blockSignals(True)
        self.layers.clear()
        for index, layer in enumerate(sorted(self.manifest.get("layers", []), key=lambda value: int(value.get("z_index", 0)), reverse=True)):
            item = QTreeWidgetItem([f"{LAYER_LABELS.get(layer.get('type'), layer.get('type'))} · {layer.get('id', '')}", str(layer.get("z_index", 0))])
            item.setData(0, Qt.ItemDataRole.UserRole, self.manifest["layers"].index(layer))
            item.setFlags(item.flags() | Qt.ItemFlag.ItemIsUserCheckable | Qt.ItemFlag.ItemIsSelectable | Qt.ItemFlag.ItemIsEnabled)
            item.setCheckState(0, Qt.CheckState.Unchecked if float(layer.get("opacity", 1)) <= 0.01 else Qt.CheckState.Checked)
            self.layers.addTopLevelItem(item)
            if item.data(0, Qt.ItemDataRole.UserRole) == self.selected:
                item.setSelected(True)
        self.layers.blockSignals(False)

    def _select_layer(self, index: int) -> None:
        if not self.manifest.get("layers"):
            self.selected = -1
            return
        self.selected = max(0, min(index, len(self.manifest["layers"]) - 1))
        self.canvas.selected = self.selected
        layer = self.manifest["layers"][self.selected]
        bounds = layer.setdefault("bounds", {})
        self.layer_id.blockSignals(True)
        self.layer_type.blockSignals(True)
        self.layer_x.blockSignals(True)
        self.layer_y.blockSignals(True)
        self.layer_w.blockSignals(True)
        self.layer_h.blockSignals(True)
        self.layer_opacity.blockSignals(True)
        self.layer_font_size.blockSignals(True)
        self.layer_thickness.blockSignals(True)
        self.layer_gap.blockSignals(True)
        self.layer_panel_color.blockSignals(True)
        self.layer_id.setText(str(layer.get("id", "")))
        self.layer_type.setCurrentText(str(layer.get("type", "telemetry")))
        self.layer_x.setValue(float(bounds.get("x", 0)))
        self.layer_y.setValue(float(bounds.get("y", 0)))
        self.layer_w.setValue(float(bounds.get("width", 0.2)))
        self.layer_h.setValue(float(bounds.get("height", 0.2)))
        self.layer_opacity.setValue(float(layer.get("opacity", 1)))
        options = layer.get("options") or {}
        self.layer_font_size.setValue(int(float(options.get("font_size", 24))))
        self.layer_thickness.setValue(float(options.get("thickness", 4)))
        self.layer_gap.setValue(float(options.get("gap", 5)))
        self.layer_panel_color.setText(str(options.get("panel_color", "#00000070")))
        self.layer_id.blockSignals(False)
        self.layer_type.blockSignals(False)
        self.layer_x.blockSignals(False)
        self.layer_y.blockSignals(False)
        self.layer_w.blockSignals(False)
        self.layer_h.blockSignals(False)
        self.layer_opacity.blockSignals(False)
        self.layer_font_size.blockSignals(False)
        self.layer_thickness.blockSignals(False)
        self.layer_gap.blockSignals(False)
        self.layer_panel_color.blockSignals(False)
        self._sync_option_controls(str(layer.get("type", "telemetry")))
        self.canvas.update()

    def _sync_option_controls(self, layer_type: str) -> None:
        """只启用当前组件有意义的参数，避免右侧面板变成无关字段堆叠。"""
        self.layer_font_size.setEnabled(layer_type == "lyrics")
        self.layer_panel_color.setEnabled(layer_type == "lyrics")
        self.layer_thickness.setEnabled(layer_type == "audio_ring")
        self.layer_gap.setEnabled(layer_type == "audio_bars")

    def _layer_selection_changed(self) -> None:
        items = self.layers.selectedItems()
        if items:
            self._select_layer(int(items[0].data(0, Qt.ItemDataRole.UserRole)))

    def _layer_visibility_changed(self, item: QTreeWidgetItem, column: int) -> None:
        if column != 0 or self.layers.signalsBlocked():
            return
        index = int(item.data(0, Qt.ItemDataRole.UserRole))
        layer = self.manifest["layers"][index]
        if layer.get("type", "").startswith("background") and item.checkState(0) == Qt.CheckState.Unchecked:
            item.setCheckState(0, Qt.CheckState.Checked)
            self.message.emit("背景图层不能隐藏；请切换背景资源。", False)
            return
        if item.checkState(0) == Qt.CheckState.Unchecked:
            layer["_opacity_before_hidden"] = layer.get("opacity", 1)
            layer["opacity"] = 0
        else:
            layer["opacity"] = layer.pop("_opacity_before_hidden", 1)
        self.canvas.update()

    def _property_changed(self, *_args) -> None:
        if self.selected < 0 or self.selected >= len(self.manifest.get("layers", [])):
            return
        layer = self.manifest["layers"][self.selected]
        layer["id"] = self.layer_id.text().strip() or f"layer-{self.selected + 1}"
        selected_type = self.layer_type.currentText()
        if selected_type.startswith("background"):
            another_background = any(
                index != self.selected and candidate.get("type", "").startswith("background")
                for index, candidate in enumerate(self.manifest["layers"])
            )
            if another_background:
                self.layer_type.blockSignals(True)
                self.layer_type.setCurrentText(str(layer.get("type", "telemetry")))
                self.layer_type.blockSignals(False)
                self.message.emit("主题只能有一个背景图层；请先删除或改造现有背景。", False)
                return
        layer["type"] = selected_type
        bounds = layer.setdefault("bounds", {})
        bounds["x"] = self.layer_x.value()
        bounds["y"] = self.layer_y.value()
        bounds["width"] = min(self.layer_w.value(), 1 - bounds["x"])
        bounds["height"] = min(self.layer_h.value(), 1 - bounds["y"])
        layer["opacity"] = self.layer_opacity.value()
        options = layer.setdefault("options", {})
        if selected_type == "lyrics":
            options["font_size"] = self.layer_font_size.value()
            options["panel_color"] = self.layer_panel_color.text().strip() or "#00000070"
        elif selected_type == "audio_ring":
            options["thickness"] = self.layer_thickness.value()
        elif selected_type == "audio_bars":
            options["gap"] = self.layer_gap.value()
        self._sync_option_controls(selected_type)
        self._render_layers()
        self.canvas.update()

    def _canvas_changed(self) -> None:
        # 拖动期间只让画布重绘；右侧控件和图层树在松开鼠标时一次性同步。
        self.canvas.update()

    def _canvas_committed(self) -> None:
        self._select_layer(self.canvas.selected)
        self._render_layers()

    def _drop_component(self, layer_type: str, position: QPointF) -> None:
        if layer_type.startswith("background") and any(layer.get("type", "").startswith("background") for layer in self.manifest["layers"]):
            layer_type = "telemetry"
        layer = default_layer(layer_type, len(self.manifest["layers"]))
        bounds = layer["bounds"]
        bounds.update(move_bounds(bounds, position.x() - bounds["width"] / 2, position.y() - bounds["height"] / 2))
        self.manifest["layers"].append(layer)
        self.selected = len(self.manifest["layers"]) - 1
        self._render_layers()
        self._select_layer(self.selected)
        self.canvas.update()

    def _choose_color(self) -> None:
        if self.selected < 0:
            return
        current = QColor(str((self.manifest["layers"][self.selected].get("options") or {}).get("color", "#46e5ff"))[:7])
        color = QColorDialog.getColor(current, self, "选择组件颜色")
        if color.isValid():
            self.manifest["layers"][self.selected].setdefault("options", {})["color"] = color.name() + "ff"
            self.canvas.update()

    def _toggle_properties(self) -> None:
        self._property_visible = not self._property_visible
        self.properties.setVisible(self._property_visible)
        self.close_editor.setText("关闭编辑" if self._property_visible else "打开编辑")

    def _move_layer(self, direction: int) -> None:
        if self.selected < 0:
            return
        layers = self.manifest["layers"]
        target = self.selected + direction
        if not 0 <= target < len(layers):
            return
        layers[self.selected], layers[target] = layers[target], layers[self.selected]
        for index, layer in enumerate(layers):
            layer["z_index"] = index * 10
        self.selected = target
        self._render_layers()
        self._select_layer(self.selected)
        self.canvas.update()

    def _duplicate_layer(self) -> None:
        if self.selected < 0:
            return
        duplicate = copy.deepcopy(self.manifest["layers"][self.selected])
        duplicate["id"] = f"{duplicate.get('id', 'layer')}-copy"
        bounds = duplicate.setdefault("bounds", {})
        bounds.update(move_bounds(bounds, 0.03, 0.03))
        self.manifest["layers"].insert(self.selected + 1, duplicate)
        self.selected += 1
        self._move_layer(0)

    def _delete_layer(self) -> None:
        if self.selected < 0:
            return
        if self.manifest["layers"][self.selected].get("type", "").startswith("background"):
            self.message.emit("主题必须保留一个背景图层。", False)
            return
        self.manifest["layers"].pop(self.selected)
        self.selected = max(0, self.selected - 1)
        self._render_layers()
        self._select_layer(self.selected)
        self.canvas.update()

    def _apply_template(self) -> None:
        choice = self.template.currentIndex()
        manifest = copy.deepcopy(default_manifest())
        if choice == 1:
            manifest["layers"] = [default_layer("background_image", 0), default_layer("audio_bars", 1), default_layer("audio_ring", 2)]
            manifest["layers"][0]["id"] = "background"
        elif choice == 2:
            manifest["layers"] = [default_layer("background_image", 0), default_layer("telemetry", 1)]
            manifest["layers"][0]["id"] = "background"
        self._load_manifest(manifest)
        self.message.emit("已套用模板，可继续拖动组件调整布局。", True)

    def _new_theme(self) -> None:
        self._load_manifest(default_manifest())
        self.message.emit("已建立新的主题草稿。", True)

    def _open_json(self) -> None:
        path, _ = QFileDialog.getOpenFileName(self, "打开主题 JSON", "", "主题 JSON (*.json)")
        if not path:
            return
        try:
            with open(path, "r", encoding="utf-8") as handle:
                self._load_manifest(json.load(handle))
            self.message.emit(f"已载入 {Path(path).name}。", True)
        except (OSError, json.JSONDecodeError) as exc:
            self.message.emit(f"读取主题失败：{exc}", False)

    def _save_json(self) -> None:
        path, _ = QFileDialog.getSaveFileName(self, "保存主题 JSON", "desktop-theme.json", "主题 JSON (*.json)")
        if not path:
            return
        try:
            with open(path, "w", encoding="utf-8") as handle:
                json.dump(self.manifest, handle, ensure_ascii=False, indent=2)
                handle.write("\n")
            self.message.emit(f"主题已保存：{path}", True)
        except OSError as exc:
            self.message.emit(f"保存主题失败：{exc}", False)

    def import_background(self) -> None:
        path, _ = QFileDialog.getOpenFileName(self, "导入背景图片/视频", "", "图片或视频 (*.png *.jpg *.jpeg *.webp *.bmp *.mp4)")
        if not path:
            return
        digest_builder = hashlib.sha256()
        with open(path, "rb") as resource_file:
            for chunk in iter(lambda: resource_file.read(1024 * 1024), b""):
                digest_builder.update(chunk)
        digest = digest_builder.hexdigest()
        resource_id = f"background-{digest[:12]}"
        self.manifest["resources"] = [item for item in self.manifest.get("resources", []) if item.get("id") != resource_id]
        self.manifest["resources"].append({"id": resource_id, "local_path": os.path.abspath(path), "sha256": digest, "size": os.path.getsize(path)})
        background = next((item for item in self.manifest["layers"] if item.get("type", "").startswith("background")), None)
        if background is None:
            background = default_layer("background_image", 0)
            background["id"] = "background"
            self.manifest["layers"].insert(0, background)
        background["type"] = "background_video" if Path(path).suffix.lower() == ".mp4" else "background_image"
        background.setdefault("options", {})["resource_id"] = resource_id
        if background["type"] == "background_image" and not self.canvas.set_background(path):
            self.message.emit("图片预览失败，请更换为 JPG、PNG 或 WebP 格式。", False)
            return
        if background["type"] == "background_video":
            self.canvas.set_background("")
        self._render_layers()
        self.message.emit(f"已导入背景：{Path(path).name}", True)

    def _apply_to_device(self) -> None:
        try:
            if not self.api.token:
                self.api.connect()
            result = self.api.apply_theme(self.manifest, self.target.currentData() or "all")
            self.message.emit(f"主题已应用；成功设备数 {result.get('successes', 0)}。", True)
        except CaseScreenApiError as exc:
            self.message.emit(str(exc), False)

    def load_service_theme(self) -> None:
        try:
            if not self.api.token:
                self.api.connect()
            self._load_manifest(self.api.theme(self.target.currentData() or "all"))
            self.message.emit("已读取服务中的当前主题。", True)
        except CaseScreenApiError as exc:
            self.message.emit(str(exc), False)
