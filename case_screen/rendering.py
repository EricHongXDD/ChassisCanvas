from __future__ import annotations

from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

from .models import TelemetrySnapshot

FONT_PATHS = (
    Path("C:/Windows/Fonts/msyh.ttc"),
    Path("C:/Windows/Fonts/segoeui.ttf"),
)


def _font(size: int, bold: bool = False) -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    paths = (
        Path("C:/Windows/Fonts/msyhbd.ttc"),
        Path("C:/Windows/Fonts/seguisb.ttf"),
    ) if bold else FONT_PATHS
    for path in paths:
        if path.exists():
            return ImageFont.truetype(str(path), size=size)
    return ImageFont.load_default()


def _panel(draw: ImageDraw.ImageDraw, box: tuple[int, int, int, int], radius: int) -> None:
    draw.rounded_rectangle(box, radius=radius, fill=(25, 31, 47), outline=(45, 57, 82), width=2)


def _metric(
    draw: ImageDraw.ImageDraw,
    box: tuple[int, int, int, int],
    label: str,
    value: str,
    percent: float,
    accent: tuple[int, int, int],
    scale: float,
) -> None:
    x1, y1, x2, y2 = box
    _panel(draw, box, max(10, int(18 * scale)))
    padding = max(10, int(20 * scale))
    draw.text((x1 + padding, y1 + padding), label, font=_font(max(12, int(20 * scale))), fill=(151, 165, 190))
    draw.text((x1 + padding, y1 + int(48 * scale)), value, font=_font(max(20, int(42 * scale)), True), fill=(242, 246, 255))
    bar_y = y2 - max(14, int(24 * scale))
    bar_left = x1 + padding
    bar_right = x2 - padding
    draw.rounded_rectangle((bar_left, bar_y, bar_right, bar_y + max(5, int(8 * scale))), radius=8, fill=(48, 58, 78))
    progress = bar_left + int((bar_right - bar_left) * max(0, min(100, percent)) / 100)
    if progress > bar_left:
        draw.rounded_rectangle((bar_left, bar_y, progress, bar_y + max(5, int(8 * scale))), radius=8, fill=accent)


def render_dashboard(
    snapshot: TelemetrySnapshot,
    size: tuple[int, int],
    device_name: str = "统一监控",
) -> Image.Image:
    width, height = size
    image = Image.new("RGB", size, (10, 14, 25))
    draw = ImageDraw.Draw(image)
    scale = min(width / 1024, height / 600)
    margin = max(12, int(28 * scale))

    # 背景光晕只用于统一视觉，不依赖设备特殊能力。
    draw.ellipse(
        (int(width * 0.68), -int(height * 0.35), int(width * 1.15), int(height * 0.55)),
        fill=(20, 33, 63),
    )
    title_size = max(18, int(31 * scale))
    draw.text((margin, margin), "CASE SCREEN", font=_font(title_size, True), fill=(93, 222, 255))
    device_font = _font(max(11, int(17 * scale)))
    device_box = draw.textbbox((0, 0), device_name, font=device_font)
    draw.text((width - margin - (device_box[2] - device_box[0]), margin + int(8 * scale)), device_name, font=device_font, fill=(155, 169, 194))

    time_text = snapshot.captured_at.strftime("%H:%M:%S")
    date_text = snapshot.captured_at.strftime("%Y / %m / %d")
    time_font = _font(max(28, int(65 * scale)), True)
    draw.text((margin, int(height * 0.18)), time_text, font=time_font, fill=(245, 248, 255))
    draw.text((margin + int(4 * scale), int(height * 0.18) + int(76 * scale)), date_text, font=_font(max(12, int(20 * scale))), fill=(121, 136, 163))

    top = int(height * 0.49)
    bottom = height - margin
    gap = max(8, int(14 * scale))
    panel_width = (width - margin * 2 - gap * 2) // 3
    boxes = [
        (margin + index * (panel_width + gap), top, margin + index * (panel_width + gap) + panel_width, bottom)
        for index in range(3)
    ]
    cpu_temp = "--°C" if snapshot.cpu_temperature is None else f"{snapshot.cpu_temperature:.0f}°C"
    _metric(draw, boxes[0], "CPU 占用", f"{snapshot.cpu_percent:.0f}%", snapshot.cpu_percent, (57, 208, 255), scale)
    _metric(draw, boxes[1], "CPU 温度", cpu_temp, snapshot.cpu_temperature or 0, (255, 103, 132), scale)
    _metric(draw, boxes[2], "内存占用", f"{snapshot.memory_percent:.0f}%", snapshot.memory_percent, (142, 116, 255), scale)
    return image

