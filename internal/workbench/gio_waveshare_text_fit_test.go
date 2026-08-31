package workbench

import (
	"image"
	"image/color"
	"testing"
	"time"

	caseTheme "casescreen/internal/theme"
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

func assertPreviewTextPlacementWithin(t *testing.T, name string, placement wavesharePreviewTextPlacement, bounds image.Rectangle) {
	t.Helper()
	if placement.size <= 0 {
		t.Fatalf("%s 字号必须大于 0：%.2f", name, placement.size)
	}
	if placement.point.X < bounds.Min.X || placement.point.Y < bounds.Min.Y ||
		placement.point.X+placement.extent.X > bounds.Max.X || placement.point.Y+placement.extent.Y > bounds.Max.Y {
		t.Fatalf("%s 文字布局越界：point=%v extent=%v bounds=%v", name, placement.point, placement.extent, bounds)
	}
}

func TestWavesharePreviewTextLayoutFitsSmallComponentAreas(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		preferred unit.Sp
		bounds    image.Rectangle
		align     string
		vertical  string
	}{
		{name: "自定义文本", value: "自定义文本", preferred: 32, bounds: image.Rect(2, 2, 22, 22), align: "center", vertical: "center"},
		{name: "液晶时钟", value: "15:04:05", preferred: 44, bounds: image.Rect(2, 2, 22, 22), align: "center", vertical: "center"},
		{name: "歌词当前行", value: "示例歌曲 · 流光", preferred: 20, bounds: image.Rect(2, 2, 22, 11), align: "left", vertical: "center"},
		{name: "歌词下一行", value: "让时间继续向前", preferred: 14, bounds: image.Rect(2, 11, 22, 19), align: "left", vertical: "center"},
		{name: "进度标签", value: "CPU 使用率", preferred: 12, bounds: image.Rect(1, 1, 11, 10), align: "left", vertical: "center"},
		{name: "进度数值", value: "42%", preferred: 12, bounds: image.Rect(12, 1, 23, 10), align: "right", vertical: "center"},
		{name: "仪表数值", value: "63%", preferred: 22, bounds: image.Rect(2, 8, 22, 16), align: "center", vertical: "center"},
		{name: "仪表标签", value: "GPU 使用率", preferred: 11, bounds: image.Rect(2, 16, 22, 22), align: "center", vertical: "center"},
		{name: "折线标题", value: "CPU 使用率  42%", preferred: 7, bounds: image.Rect(5, 5, 19, 9), align: "left", vertical: "top"},
		{name: "通用遥测", value: "CPU 42%   GPU 63%   RAM 58%", preferred: 12, bounds: image.Rect(2, 2, 22, 12), align: "left", vertical: "center"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			placement := waveshareFitPreviewTextLayout(test.value, test.preferred, test.bounds, test.align, test.vertical)
			assertPreviewTextPlacementWithin(t, test.name, placement, test.bounds)
			if placement.size > test.preferred {
				t.Fatalf("%s 自适应字号不应超过用户设置：got %.2f, preferred %.2f", test.name, placement.size, test.preferred)
			}
		})
	}
}

func TestWavesharePreviewTextSizeRespondsToWidthAndHeight(t *testing.T) {
	wide := waveshareFitPreviewTextSize("15:04:05", 44, 240, 80)
	narrow := waveshareFitPreviewTextSize("15:04:05", 44, 20, 80)
	short := waveshareFitPreviewTextSize("15:04:05", 44, 240, 8)
	if narrow >= wide {
		t.Fatalf("宽度缩小时字号未收敛：wide %.2f, narrow %.2f", wide, narrow)
	}
	if short >= wide {
		t.Fatalf("高度缩小时字号未收敛：wide %.2f, short %.2f", wide, short)
	}
}

func TestWaveshareClockPreviewKeepsFullHHmmssAt127By43(t *testing.T) {
	model := &gioEditor{theme: material.NewTheme()}
	model.theme.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()), text.NoSystemFonts())
	layer := caseTheme.Layer{Type: caseTheme.LayerClock, Options: map[string]any{
		"format": "HH:mm:ss", "font_size": 44.0, "font_family": "digital", "font_weight": "bold", "align": "center",
	}}
	value := formatClockPreview("HH:mm:ss", time.Date(2026, 8, 26, 18, 59, 7, 0, time.Local))
	if value != "18:59:07" {
		t.Fatalf("时钟测试值必须保留完整 HH:mm:ss：%q", value)
	}

	content := image.Rect(4, 4, 123, 39)
	preferred := unit.Sp(caseTheme.OptionFloat(layer.Options, "font_size", 44))
	placement := waveshareFitClockPreviewTextLayout(value, preferred, content, "center", "center")
	general := waveshareFitPreviewTextLayout(value, preferred, content, "center", "center")
	if placement.size >= general.size {
		t.Fatalf("时钟布局必须为真实字体回退预留宽度：clock %.2f, general %.2f", placement.size, general.size)
	}
	if placement.size <= .5 {
		t.Fatalf("127×43 足以显示完整时间，不应直接降到最小字号：%.2f", placement.size)
	}
	assertPreviewTextPlacementWithin(t, "127×43 完整时钟", placement, content)

	var operations op.Ops
	textMaterialMacro := op.Record(&operations)
	paint.ColorOp{Color: color.NRGBA{A: 255}}.Add(&operations)
	textMaterial := textMaterialMacro.Stop()
	labelGTX := layout.Context{
		Ops:         &operations,
		Constraints: layout.Constraints{Max: content.Size()},
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
	}
	label := material.Label(model.theme, placement.size, value)
	label.Font.Typeface = font.Typeface("Liquid Crystal, Cascadia Mono, monospace")
	label.Font.Weight = font.Bold
	dimensions, info := (widget.Label{MaxLines: 1}).LayoutDetailed(labelGTX, model.theme.Shaper, label.Font, placement.size, value, textMaterial)
	if info.Truncated != 0 {
		t.Fatalf("127×43 时钟不应触发 Gio 单行省略：值 %q，字号 %.2f，省略字符数 %d", value, placement.size, info.Truncated)
	}
	if dimensions.Size.X > content.Dx() || dimensions.Size.Y > content.Dy() {
		t.Fatalf("127×43 时钟必须通过缩小字号完整显示 %q：字号 %.2f，实测 %v，可用 %v", value, placement.size, dimensions.Size, content.Size())
	}
}

func TestTextBearingComponentPreviewsRenderAtTwentyFourPixels(t *testing.T) {
	model := &gioEditor{
		theme:      material.NewTheme(),
		target:     "waveshare-7",
		manifest:   defaultManifestFor(1024, 600),
		previewNow: time.Date(2026, 8, 26, 15, 4, 5, 0, time.Local),
	}
	layers := []caseTheme.Layer{
		{ID: "text", Type: caseTheme.LayerText, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"text": "自定义文本", "font_size": 96.0, "font_family": "source-han", "font_weight": "bold", "align": "center", "shadow": true}},
		{ID: "clock", Type: caseTheme.LayerClock, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"format": "HH:mm:ss", "font_size": 96.0, "font_family": "digital", "align": "center"}},
		{ID: "progress", Type: caseTheme.LayerProgressBar, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"data_key": "Cpu_Freq", "show_label": true, "show_value": true}},
		{ID: "gauge", Type: caseTheme.LayerGauge, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"data_key": "Cpu_Freq", "font_size": 96.0, "show_label": true, "show_value": true}},
		{ID: "gradient", Type: caseTheme.LayerGradientGauge, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"data_key": "Gpu_Load", "font_size": 96.0, "show_label": true, "show_value": true}},
		{ID: "line", Type: caseTheme.LayerLineChart, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"data_key": "Cpu_Freq", "show_label": true}},
		{ID: "lyrics", Type: caseTheme.LayerLyrics, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"font_size": 96.0, "next_font_size": 72.0, "font_family": "source-han"}},
	}
	for _, layer := range layers {
		t.Run(string(layer.Type), func(t *testing.T) {
			var operations op.Ops
			gtx := layout.Context{Ops: &operations, Constraints: layout.Exact(image.Pt(24, 24)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
			model.drawLayerPreview(gtx, image.Rect(0, 0, 24, 24), layer, color.NRGBA{R: 76, G: 224, B: 255, A: 255})
		})
	}
	for _, style := range []string{"cards", "digital", "gauge", "dials", "minimal"} {
		t.Run("waveshare-telemetry-"+style, func(t *testing.T) {
			var operations op.Ops
			gtx := layout.Context{Ops: &operations, Constraints: layout.Exact(image.Pt(24, 24)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
			layer := caseTheme.Layer{
				ID: "telemetry", Type: caseTheme.LayerTelemetry,
				Bounds:  caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600},
				Opacity: 1,
				Options: map[string]any{"style": style, "fields": []string{"Cpu_Freq", "Gpu_Load", "Memory_Load"}, "font_size": 96.0, "show_label": true, "show_value": true},
			}
			model.drawTelemetryPreview(gtx, image.Pt(24, 24), layer, color.NRGBA{R: 76, G: 224, B: 255, A: 255})
		})
	}

	// 非微雪目标会走通用遥测预览，也必须能在同样的小画布上完成布局。
	model.target = "thermalright-kv"
	for _, style := range []string{"cards", "compact", "gauge", "digital", "minimal"} {
		t.Run("generic-telemetry-"+style, func(t *testing.T) {
			var operations op.Ops
			gtx := layout.Context{Ops: &operations, Constraints: layout.Exact(image.Pt(24, 24)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
			model.drawTelemetryPreview(gtx, image.Pt(24, 24), caseTheme.Layer{Type: caseTheme.LayerTelemetry, Bounds: caseTheme.Rect{Width: 24.0 / 1024, Height: 24.0 / 600}, Opacity: 1, Options: map[string]any{"style": style}}, color.NRGBA{R: 76, G: 224, B: 255, A: 255})
		})
	}
}
