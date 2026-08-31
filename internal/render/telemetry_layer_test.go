package render

import (
	"crypto/sha256"
	"image"
	"image/color"
	"testing"
	"time"

	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
)

func TestHostTelemetryPresetLayoutsArePixelDistinctAndClipped(t *testing.T) {
	renderer := newTelemetryLayerTestRenderer(t)
	bounds := theme.Rect{X: .05, Y: .1, Width: .9, Height: .8}
	common := map[string]any{
		"fields":       []any{"Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp"},
		"font_size":    22.0,
		"font_family":  "source-han",
		"font_weight":  "medium",
		"label_color":  "A8C4CAFF",
		"value_color":  "F5FBFFFF",
		"accent_color": "4CE0FFFF",
		"panel_color":  "04141BFF",
		"card_color":   "112C35FF",
		"card_shape":   "outline",
		"show_label":   true,
		"show_value":   true,
		"show_unit":    true,
	}
	frames := make(map[string]*image.RGBA)
	for _, style := range []string{"cards", "gauge", "digital", "minimal"} {
		options := cloneTelemetryOptions(common)
		options["style"] = style
		frames[style] = renderTelemetryLayerTestFrame(t, renderer, 320, 140, bounds, options, telemetryLayerTestSnapshot(42))
		assertTelemetryPixelsStayInside(t, frames[style], normalizedRect(bounds, 320, 140))
	}

	styles := []string{"cards", "gauge", "digital", "minimal"}
	area := normalizedRect(bounds, 320, 140).Dx() * normalizedRect(bounds, 320, 140).Dy()
	for left := 0; left < len(styles); left++ {
		for right := left + 1; right < len(styles); right++ {
			difference := telemetryFrameDifference(frames[styles[left]], frames[styles[right]], normalizedRect(bounds, 320, 140))
			// 同一配色下仍需要有可感知的布局差异，不能只是换了一个 style 字符串。
			if difference < area/100 {
				t.Fatalf("telemetry 布局 %s/%s 像素差异不足: %d/%d", styles[left], styles[right], difference, area)
			}
		}
	}
}

func TestHostTelemetryPresetPalettesAndEveryVisualOptionAffectPixels(t *testing.T) {
	renderer := newTelemetryLayerTestRenderer(t)
	bounds := theme.Rect{X: .06, Y: .08, Width: .88, Height: .84}
	base := map[string]any{
		"style":        "cards",
		"fields":       []any{"Cpu_Freq"},
		"font_size":    20.0,
		"font_family":  "source-han",
		"font_weight":  "medium",
		"label_color":  "A8C4CAFF",
		"value_color":  "F5FBFFFF",
		"accent_color": "4CE0FFFF",
		"panel_color":  "04141BFF",
		"card_color":   "112C35FF",
		"card_shape":   "outline",
		"show_label":   true,
		"show_value":   true,
		"show_unit":    true,
	}
	baseFrame := renderTelemetryLayerTestFrame(t, renderer, 240, 100, bounds, base, telemetryLayerTestSnapshot(42))
	baseDigest := sha256.Sum256(baseFrame.Pix)
	variants := []struct {
		name  string
		key   string
		value any
	}{
		{name: "标签颜色", key: "label_color", value: "FF3355FF"},
		{name: "数值颜色", key: "value_color", value: "33FF88FF"},
		{name: "强调颜色", key: "accent_color", value: "FFD400FF"},
		{name: "面板颜色", key: "panel_color", value: "3A0718FF"},
		{name: "卡片颜色", key: "card_color", value: "263A74FF"},
		{name: "卡片形状", key: "card_shape", value: "pill"},
		{name: "字号", key: "font_size", value: 34.0},
		{name: "隐藏标签", key: "show_label", value: false},
		{name: "隐藏数值", key: "show_value", value: false},
		{name: "隐藏单位", key: "show_unit", value: false},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			options := cloneTelemetryOptions(base)
			options[variant.key] = variant.value
			frame := renderTelemetryLayerTestFrame(t, renderer, 240, 100, bounds, options, telemetryLayerTestSnapshot(42))
			if digest := sha256.Sum256(frame.Pix); digest == baseDigest {
				t.Fatalf("%s 没有改变 telemetry 主机合成像素", variant.name)
			}
			assertTelemetryPixelsStayInside(t, frame, normalizedRect(bounds, 240, 100))
		})
	}

	presets := []struct {
		name    string
		options map[string]any
	}{
		{name: "glacier-cards", options: map[string]any{"style": "cards", "font_family": "source-han", "font_weight": "medium", "label_color": "A8C4CA", "value_color": "F5FBFF", "accent_color": "4CE0FF", "panel_color": "04141BDC", "card_color": "112C35D9", "card_shape": "rounded"}},
		{name: "amber-gauge", options: map[string]any{"style": "gauge", "font_family": "rounded", "font_weight": "bold", "label_color": "FFD59A", "value_color": "FFF7E8", "accent_color": "FFB45E", "panel_color": "211308DC", "card_color": "35200DD9", "card_shape": "pill"}},
		{name: "violet-digital", options: map[string]any{"style": "digital", "font_family": "digital", "font_weight": "bold", "label_color": "C6B7FF", "value_color": "FFFFFF", "accent_color": "8B5CF6", "panel_color": "100724E6", "card_color": "1D1045D9", "card_shape": "outline"}},
		{name: "mono-minimal", options: map[string]any{"style": "minimal", "font_family": "monospace", "font_weight": "regular", "label_color": "9FB5C0", "value_color": "FFFFFF", "accent_color": "FFFFFF", "panel_color": "00000000", "card_color": "00000000", "card_shape": "square"}},
	}
	seen := make(map[[32]byte]string)
	for _, preset := range presets {
		options := cloneTelemetryOptions(preset.options)
		options["fields"] = []any{"Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp"}
		options["font_size"] = 22.0
		frame := renderTelemetryLayerTestFrame(t, renderer, 320, 140, bounds, options, telemetryLayerTestSnapshot(42))
		digest := sha256.Sum256(frame.Pix)
		if previous, exists := seen[digest]; exists {
			t.Fatalf("telemetry 预设 %s 与 %s 像素完全相同", preset.name, previous)
		}
		seen[digest] = preset.name
	}
}

func TestHostTelemetryGaugeUsesCurrentMetricAndConfiguredRange(t *testing.T) {
	renderer := newTelemetryLayerTestRenderer(t)
	bounds := theme.Rect{X: .05, Y: .15, Width: .9, Height: .7}
	options := map[string]any{
		"style":        "gauge",
		"fields":       []any{"Cpu_Freq"},
		"show_label":   false,
		"show_value":   false,
		"panel_color":  "00000000",
		"card_color":   "00000000",
		"accent_color": "FF0000FF",
		"min_value":    20.0,
		"max_value":    80.0,
	}
	low := renderTelemetryLayerTestFrame(t, renderer, 240, 80, bounds, options, telemetryLayerTestSnapshot(30))
	high := renderTelemetryLayerTestFrame(t, renderer, 240, 80, bounds, options, telemetryLayerTestSnapshot(70))
	clip := normalizedRect(bounds, 240, 80)
	lowPixels := telemetryRedPixelCount(low, clip)
	highPixels := telemetryRedPixelCount(high, clip)
	if lowPixels == 0 || highPixels < lowPixels*2 {
		t.Fatalf("gauge 没有按实时数值及 min/max 增长: low=%d high=%d", lowPixels, highPixels)
	}
}

func TestHostTelemetryTinyBoundsNeverPanicOrDrawOutside(t *testing.T) {
	renderer := newTelemetryLayerTestRenderer(t)
	for _, size := range []image.Point{{X: 1, Y: 1}, {X: 2, Y: 3}, {X: 7, Y: 5}} {
		for _, style := range []string{"cards", "gauge", "digital", "minimal", "compact"} {
			for _, shape := range []string{"rounded", "square", "pill", "outline"} {
				name := style + "/" + shape + "/" + size.String()
				t.Run(name, func(t *testing.T) {
					bounds := theme.Rect{X: .5, Y: .5, Width: float64(size.X) / 64, Height: float64(size.Y) / 48}
					options := map[string]any{
						"style": style, "card_shape": shape, "fields": []any{"time", "Cpu_Freq", "Gpu_Load", "Memory_Load"},
						"font_size": 200.0, "show_label": true, "show_value": true, "show_unit": true,
						"panel_color": "151A20FF", "card_color": "27313BFF", "accent_color": "55E5FFFF",
					}
					frame := renderTelemetryLayerTestFrame(t, renderer, 64, 48, bounds, options, telemetryLayerTestSnapshot(42))
					assertTelemetryPixelsStayInside(t, frame, normalizedRect(bounds, 64, 48))
				})
			}
		}
	}
}

func TestHostTelemetryCompactAndLegacyColorRemainCompatible(t *testing.T) {
	renderer := newTelemetryLayerTestRenderer(t)
	bounds := theme.Rect{X: .08, Y: .1, Width: .84, Height: .8}
	base := map[string]any{
		"fields": []any{"Cpu_Freq", "Memory_Load"}, "font_size": 22.0,
		"panel_color": "00000000", "color": "FF3355FF",
	}
	digests := make([][32]byte, 0, 3)
	for _, style := range []string{"", "compact", "old-unknown-style"} {
		options := cloneTelemetryOptions(base)
		if style != "" {
			options["style"] = style
		}
		frame := renderTelemetryLayerTestFrame(t, renderer, 240, 100, bounds, options, telemetryLayerTestSnapshot(42))
		digests = append(digests, sha256.Sum256(frame.Pix))
		if telemetryRedPixelCount(frame, normalizedRect(bounds, 240, 100)) == 0 {
			t.Fatalf("旧 color 没有继续作用于 %q 紧凑布局", style)
		}
	}
	if digests[0] != digests[1] || digests[0] != digests[2] {
		t.Fatal("省略 style、compact 与未知旧 style 未走相同兼容路径")
	}

	withoutColor := cloneTelemetryOptions(base)
	delete(withoutColor, "color")
	explicitOldDefault := cloneTelemetryOptions(withoutColor)
	explicitOldDefault["color"] = "F5F8FFFF"
	implicitFrame := renderTelemetryLayerTestFrame(t, renderer, 240, 100, bounds, withoutColor, telemetryLayerTestSnapshot(42))
	explicitFrame := renderTelemetryLayerTestFrame(t, renderer, 240, 100, bounds, explicitOldDefault, telemetryLayerTestSnapshot(42))
	if sha256.Sum256(implicitFrame.Pix) != sha256.Sum256(explicitFrame.Pix) {
		t.Fatal("compact 旧主题的单色缺省值发生了变化")
	}
}

func TestTelemetryValueTextHonorsShowUnit(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "42%", want: "42"},
		{value: "45℃", want: "45"},
		{value: "18.6GB", want: "18.6"},
		{value: "-18dB", want: "-18"},
		{value: "15:04:05", want: "15:04:05"},
	} {
		if got := telemetryValueText(test.value, false); got != test.want {
			t.Errorf("telemetryValueText(%q, false)=%q, want %q", test.value, got, test.want)
		}
		if got := telemetryValueText(test.value, true); got != test.value {
			t.Errorf("telemetryValueText(%q, true)=%q, want %q", test.value, got, test.value)
		}
	}
}

func newTelemetryLayerTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	regular, err := opentype.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	renderer := &Renderer{
		fonts: map[string]*opentype.Font{
			"": regular,
		},
		faces:  make(map[faceKey]font.Face),
		audio:  make(map[string]audioVisualRuntime),
		charts: make(map[string]chartVisualRuntime),
	}
	t.Cleanup(func() {
		if err := renderer.Close(); err != nil {
			t.Errorf("关闭 telemetry 测试渲染器: %v", err)
		}
	})
	return renderer
}

func renderTelemetryLayerTestFrame(t *testing.T, renderer *Renderer, width, height int, bounds theme.Rect,
	options map[string]any, snapshot telemetry.Snapshot) *image.RGBA {
	t.Helper()
	renderer.manifest = theme.Manifest{ID: "telemetry-test", Name: "telemetry 测试", Width: width, Height: height, Layers: []theme.Layer{
		{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: 0, Options: map[string]any{"color": "transparent"}},
		{ID: "telemetry", Type: theme.LayerTelemetry, Bounds: bounds, Opacity: 1, ZIndex: 1, Options: cloneTelemetryOptions(options)},
	}}
	frame, err := renderer.Render(width, height, State{TelemetryEnabled: true, Telemetry: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func telemetryLayerTestSnapshot(cpu float64) telemetry.Snapshot {
	gpu, gpuTemperature := 63.0, 45.0
	return telemetry.Snapshot{
		CapturedAt: time.Date(2026, time.August, 30, 15, 4, 5, 0, time.Local),
		CPUPercent: cpu, MemoryPercent: 58, MemoryUsedGiB: 18.6, MemoryTotalGiB: 32,
		CPUModel: "AMD Ryzen", GPUModel: "GeForce RTX", GPUPercent: &gpu, GPUTemperature: &gpuTemperature,
		Volume: 72, CurrentDecibels: -18, AverageDecibels: -24, MaximumDecibels: -8,
	}
}

func cloneTelemetryOptions(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func assertTelemetryPixelsStayInside(t *testing.T, frame *image.RGBA, bounds image.Rectangle) {
	t.Helper()
	for y := frame.Bounds().Min.Y; y < frame.Bounds().Max.Y; y++ {
		for x := frame.Bounds().Min.X; x < frame.Bounds().Max.X; x++ {
			if image.Pt(x, y).In(bounds) {
				continue
			}
			if pixel := frame.RGBAAt(x, y); pixel.A != 0 {
				t.Fatalf("telemetry 绘制越界: point=(%d,%d) pixel=%v bounds=%v", x, y, pixel, bounds)
			}
		}
	}
}

func telemetryFrameDifference(left, right *image.RGBA, bounds image.Rectangle) int {
	bounds = bounds.Intersect(left.Bounds()).Intersect(right.Bounds())
	difference := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if left.RGBAAt(x, y) != right.RGBAAt(x, y) {
				difference++
			}
		}
	}
	return difference
}

func telemetryRedPixelCount(frame *image.RGBA, bounds image.Rectangle) int {
	bounds = bounds.Intersect(frame.Bounds())
	count := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := color.NRGBAModel.Convert(frame.RGBAAt(x, y)).(color.NRGBA)
			if pixel.A > 192 && pixel.R > 192 && pixel.G < 96 && pixel.B < 112 {
				count++
			}
		}
	}
	return count
}
