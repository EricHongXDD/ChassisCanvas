package render

import (
	"bytes"
	"crypto/sha256"
	"image"
	"testing"
	"time"

	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

func TestHostComponentLayersProducePixelsInsideTheirBounds(t *testing.T) {
	tests := []struct {
		name      string
		layerType theme.LayerType
		options   map[string]any
	}{
		{name: "静态文字", layerType: theme.LayerText, options: map[string]any{
			"text": "CASE SCREEN", "font_size": 26, "color": "FF3040", "panel_color": "00000000",
		}},
		{name: "时钟", layerType: theme.LayerClock, options: map[string]any{
			"format": "HH:mm:ss", "font_size": 26, "color": "FF3040", "panel_color": "00000000",
		}},
		{name: "进度条", layerType: theme.LayerProgressBar, options: map[string]any{
			"data_key": "Cpu_Freq", "style": "rounded", "min_value": 0, "max_value": 100,
			"color": "FF3040", "secondary_color": "214050", "panel_color": "00000000",
			"show_label": false, "show_value": false,
		}},
		{name: "圆形仪表", layerType: theme.LayerGauge, options: map[string]any{
			"data_key": "Cpu_Freq", "min_value": 0, "max_value": 100, "stroke_width": 6,
			"color": "FF3040", "secondary_color": "214050", "panel_color": "00000000",
			"show_label": false, "show_value": false,
		}},
		{name: "渐变仪表", layerType: theme.LayerGradientGauge, options: map[string]any{
			"data_key": "Gpu_Load", "min_value": 0, "max_value": 100, "stroke_width": 6,
			"gradient_start": "20E0FF", "gradient_mid": "FFF050", "gradient_end": "FF3040",
			"secondary_color": "214050", "panel_color": "00000000", "show_label": false, "show_value": false,
		}},
		{name: "折线图", layerType: theme.LayerLineChart, options: map[string]any{
			"data_key": "Cpu_Freq", "style": "line", "min_value": 0, "max_value": 100,
			"history_points": 16, "stroke_width": 2, "color": "FF3040", "panel_color": "00000000",
			"show_grid": false, "show_label": false,
		}},
		{name: "形状", layerType: theme.LayerShape, options: map[string]any{
			"shape": "rounded_rect", "fill_color": "A02030", "stroke_color": "FF3040",
			"stroke_width": 3, "corner_radius": 12,
		}},
	}
	fixed := time.Date(2026, 8, 29, 12, 34, 56, 0, time.UTC)
	gpu := 63.0
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			bounds := theme.Rect{X: .1, Y: .1, Width: .8, Height: .7}
			manifest := componentTestManifest(testCase.layerType, bounds, testCase.options)
			renderer, err := New(manifest)
			if err != nil {
				t.Fatal(err)
			}
			defer renderer.Close()
			frame, err := renderer.Render(200, 100, State{TelemetryEnabled: true, Telemetry: telemetry.Snapshot{
				CapturedAt: fixed, CPUPercent: 42, MemoryPercent: 58, GPUPercent: &gpu,
			}})
			if err != nil {
				t.Fatal(err)
			}
			layerBounds := normalizedRect(bounds, 200, 100)
			if pixels := opaquePixelCount(frame, layerBounds); pixels < 5 {
				t.Fatalf("%s 没有合成实际像素: %d", testCase.layerType, pixels)
			}
			if pixels := opaquePixelCount(frame, image.Rect(0, 0, 8, 8)); pixels != 0 {
				t.Fatalf("%s 的绘制越过图层边界: %d", testCase.layerType, pixels)
			}
		})
	}
}

func TestClockUsesCapturedAtDeterministically(t *testing.T) {
	if got := formatComponentClock("yyyy-MM-dd week HH:mm:ss", time.Date(2026, 8, 29, 7, 8, 9, 0, time.UTC)); got != "2026-08-29 周六 07:08:09" {
		t.Fatalf("时钟格式转换错误: %q", got)
	}
	manifest := componentTestManifest(theme.LayerClock, theme.Rect{X: .05, Y: .1, Width: .9, Height: .7}, map[string]any{
		"format": "HH:mm:ss", "font_family": "monospace", "font_size": 34,
		"color": "FFFFFF", "panel_color": "00000000",
	})
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	renderAt := func(moment time.Time) []byte {
		frame, renderErr := renderer.Render(240, 80, State{Telemetry: telemetry.Snapshot{CapturedAt: moment}})
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		digest := sha256.Sum256(frame.Pix)
		return digest[:]
	}
	firstMoment := time.Date(2026, 8, 29, 7, 8, 9, 0, time.UTC)
	first := renderAt(firstMoment)
	if second := renderAt(firstMoment); !bytes.Equal(first, second) {
		t.Fatal("相同 CapturedAt 渲染出的时钟像素不稳定")
	}
	if changed := renderAt(firstMoment.Add(5 * time.Hour)); bytes.Equal(first, changed) {
		t.Fatal("时钟没有使用遥测采样时间")
	}
}

func TestProgressBarUsesTelemetryValueAndConfiguredRange(t *testing.T) {
	manifest := componentTestManifest(theme.LayerProgressBar, theme.Rect{X: .05, Y: .1, Width: .9, Height: .8}, map[string]any{
		"data_key": "Cpu_Freq", "style": "square", "orientation": "horizontal",
		"min_value": 0, "max_value": 100, "color": "FF0000", "secondary_color": "00000000",
		"panel_color": "00000000", "show_label": false, "show_value": false,
	})
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	render := func(value float64) int {
		frame, renderErr := renderer.Render(200, 80, State{TelemetryEnabled: true, Telemetry: telemetry.Snapshot{CPUPercent: value}})
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return opaquePixelCount(frame, frame.Bounds())
	}
	low, high := render(20), render(80)
	if low == 0 || high < low*3 {
		t.Fatalf("进度条没有按真实 CPU 值改变填充量: low=%d high=%d", low, high)
	}
}

func TestGaugeKindsUseTelemetryValue(t *testing.T) {
	for _, layerType := range []theme.LayerType{theme.LayerGauge, theme.LayerGradientGauge} {
		t.Run(string(layerType), func(t *testing.T) {
			dataKey := "Cpu_Freq"
			options := map[string]any{
				"data_key": dataKey, "min_value": 0, "max_value": 100, "stroke_width": 7,
				"color": "FF2040", "gradient_start": "20E0FF", "gradient_mid": "FFF050", "gradient_end": "FF2040",
				"secondary_color": "00000000", "panel_color": "00000000", "show_label": false, "show_value": false,
			}
			manifest := componentTestManifest(layerType, theme.Rect{X: .1, Y: .05, Width: .8, Height: .85}, options)
			renderer, err := New(manifest)
			if err != nil {
				t.Fatal(err)
			}
			defer renderer.Close()
			render := func(value float64) int {
				frame, renderErr := renderer.Render(160, 120, State{TelemetryEnabled: true, Telemetry: telemetry.Snapshot{CPUPercent: value}})
				if renderErr != nil {
					t.Fatal(renderErr)
				}
				return opaquePixelCount(frame, frame.Bounds())
			}
			if empty, active := render(0), render(75); empty != 0 || active < 20 {
				t.Fatalf("%s 没有按遥测值绘制有效圆弧: zero=%d active=%d", layerType, empty, active)
			}
		})
	}
}

func TestStaticTextDoesNotChangeWithTelemetry(t *testing.T) {
	manifest := componentTestManifest(theme.LayerText, theme.Rect{X: .05, Y: .1, Width: .9, Height: .7}, map[string]any{
		"text": "FIXED TITLE", "data_key": "GPU_Temp", "font_size": 30,
		"color": "FFFFFF", "panel_color": "00000000",
	})
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	firstTemperature, secondTemperature := 20.0, 90.0
	render := func(value *float64) [32]byte {
		frame, renderErr := renderer.Render(240, 80, State{TelemetryEnabled: true, Telemetry: telemetry.Snapshot{GPUTemperature: value}})
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return sha256.Sum256(frame.Pix)
	}
	if first, second := render(&firstTemperature), render(&secondTemperature); first != second {
		t.Fatal("静态文字被 data_key 或遥测值意外替换")
	}
}

func TestComponentMetricsExposeEveryAvailableGPUTemperature(t *testing.T) {
	edge, hotspot, memory := 44.0, 54.0, 56.0
	state := State{TelemetryEnabled: true, Telemetry: telemetry.Snapshot{
		GPUTemperature:        &edge,
		GPUHotspotTemperature: &hotspot,
		GPUMemoryTemperature:  &memory,
	}}
	for key, want := range map[string]string{
		"GPU_Temp": "44℃", "GPU_Hotspot_Temp": "54℃", "GPU_Memory_Temp": "56℃",
	} {
		if got := resolveComponentMetric(state, theme.Layer{}, key).text; got != want {
			t.Errorf("%s 主机合成值=%q，期望 %q", key, got, want)
		}
	}
}

func TestComponentMetricsDoNotInventMissingSelectedGPUValues(t *testing.T) {
	state := State{TelemetryEnabled: true, Telemetry: telemetry.Snapshot{CapturedAt: time.Now(), GPUModel: "Intel(R) Graphics"}}
	for _, key := range []string{"Gpu_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp"} {
		if got := resolveComponentMetric(state, theme.Layer{}, key); got.text != "--" || got.raw != 0 {
			t.Errorf("%s 缺失时不应使用演示值: %+v", key, got)
		}
	}
}

func TestLineChartAdvancesOnlyForNewTelemetrySamples(t *testing.T) {
	manifest := componentTestManifest(theme.LayerLineChart, theme.Rect{X: .05, Y: .05, Width: .9, Height: .8}, map[string]any{
		"data_key": "Cpu_Freq", "style": "line", "history_points": 8, "min_value": 0, "max_value": 100,
		"stroke_width": 2, "color": "FFFFFF", "panel_color": "00000000", "show_grid": false, "show_label": false,
	})
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	start := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	render := func(moment time.Time, value float64) *image.RGBA {
		frame, renderErr := renderer.Render(200, 100, State{TelemetryEnabled: true, Telemetry: telemetry.Snapshot{
			CapturedAt: moment, CPUPercent: value,
		}})
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return frame
	}
	first := render(start, 20)
	duplicate := render(start, 80)
	if got := len(renderer.charts["component\x00Cpu_Freq"].values); got != 1 {
		t.Fatalf("相同遥测时间戳重复推进历史: %d", got)
	}
	if sha256.Sum256(first.Pix) != sha256.Sum256(duplicate.Pix) {
		t.Fatal("相同遥测时间戳改变了折线历史")
	}
	changed := render(start.Add(time.Second), 80)
	if got := len(renderer.charts["component\x00Cpu_Freq"].values); got != 2 {
		t.Fatalf("新遥测样本没有推进历史: %d", got)
	}
	if sha256.Sum256(first.Pix) == sha256.Sum256(changed.Pix) {
		t.Fatal("折线图没有显示新遥测值")
	}
}

func TestShapeVariantsRenderActualPixels(t *testing.T) {
	for _, shape := range []string{"rect", "rounded_rect", "circle", "ellipse", "line", "pill"} {
		t.Run(shape, func(t *testing.T) {
			manifest := componentTestManifest(theme.LayerShape, theme.Rect{X: .1, Y: .1, Width: .8, Height: .7}, map[string]any{
				"shape": shape, "fill_color": "305070", "stroke_color": "F0A020", "stroke_width": 3, "corner_radius": 12,
			})
			renderer, err := New(manifest)
			if err != nil {
				t.Fatal(err)
			}
			frame, renderErr := renderer.Render(120, 80, State{})
			_ = renderer.Close()
			if renderErr != nil {
				t.Fatal(renderErr)
			}
			if pixels := opaquePixelCount(frame, frame.Bounds()); pixels < 5 {
				t.Fatalf("形状 %s 没有绘制像素: %d", shape, pixels)
			}
		})
	}
}

func componentTestManifest(layerType theme.LayerType, bounds theme.Rect, options map[string]any) theme.Manifest {
	return theme.Manifest{
		ID: "component-test", Name: "主机组件像素测试", Width: 200, Height: 100,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "transparent"}},
			{ID: "component", Type: layerType, Bounds: bounds, Opacity: 1, ZIndex: 1, Options: options},
		},
	}
}

func opaquePixelCount(frame *image.RGBA, bounds image.Rectangle) int {
	count := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if frame.RGBAAt(x, y).A != 0 {
				count++
			}
		}
	}
	return count
}
