package api

import (
	"reflect"
	"strconv"
	"testing"

	"casescreen/internal/theme"
)

func TestCompileWaveshareScalableTextComponentsFitTwentyFourPixels(t *testing.T) {
	testCases := []struct {
		name           string
		layerType      theme.LayerType
		options        map[string]any
		minimum        int
		dynamicMinimum int
	}{
		{name: "text", layerType: theme.LayerText, options: map[string]any{"text": "42%", "font_size": 220.0, "shadow": true}, minimum: 2},
		{name: "clock", layerType: theme.LayerClock, options: map[string]any{"format": "HH:mm:ss", "font_size": 220.0}, minimum: 1, dynamicMinimum: 1},
		{name: "lyrics", layerType: theme.LayerLyrics, options: map[string]any{"placeholder_current": "正在等待歌词", "placeholder_next": "播放歌曲后显示下一句", "font_size": 220.0, "next_font_size": 180.0}, minimum: 2, dynamicMinimum: 2},
		{name: "progress", layerType: theme.LayerProgressBar, options: map[string]any{"data_key": "Cpu_Freq", "font_size": 220.0, "show_label": true, "show_value": true}, minimum: 2, dynamicMinimum: 1},
		{name: "gauge", layerType: theme.LayerGauge, options: map[string]any{"data_key": "Cpu_Freq", "font_size": 220.0, "show_label": true, "show_value": true}, minimum: 2, dynamicMinimum: 1},
		{name: "gradient_gauge", layerType: theme.LayerGradientGauge, options: map[string]any{"data_key": "Gpu_Load", "font_size": 220.0, "show_label": true, "show_value": true}, minimum: 2, dynamicMinimum: 1},
		{name: "line_chart", layerType: theme.LayerLineChart, options: map[string]any{"data_key": "Cpu_Freq", "font_size": 220.0, "show_label": true}, minimum: 2, dynamicMinimum: 1},
		{name: "scalar_fallback", layerType: theme.LayerGauge, options: map[string]any{"data_key": "Cpu_Model", "font_size": 220.0, "show_label": true, "show_value": true}, minimum: 2, dynamicMinimum: 1},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			optionsBefore := make(map[string]any, len(testCase.options))
			for key, value := range testCase.options {
				optionsBefore[key] = value
			}
			layer := theme.Layer{
				ID: testCase.name, Type: testCase.layerType,
				Bounds:  theme.Rect{X: .2, Y: .2, Width: .2, Height: .2},
				Opacity: 1, ZIndex: 2, Options: testCase.options,
			}
			table := compileWaveshareTable(t, theme.Manifest{Width: 120, Height: 120, Layers: []theme.Layer{layer}})
			textWidgets := make([]waveshareThemeWidget, 0)
			for _, widget := range table.widgets {
				if widget.fields["type"] == "8" {
					textWidgets = append(textWidgets, widget)
				}
			}
			if len(textWidgets) < testCase.minimum {
				t.Fatalf("24×24 图层只生成 %d 个文字控件，期望至少 %d 个: %+v", len(textWidgets), testCase.minimum, textWidgets)
			}
			dynamicCount := 0
			for _, widget := range textWidgets {
				if widget.fields["system_data_flag"] == "1" {
					dynamicCount++
					if widget.fields["system_data_name"] == "" {
						t.Fatalf("窄尺寸降级丢失动态数据键: %+v", widget.fields)
					}
				}
				if widget.fields["text_customFont_flag"] != "1" || widget.fields["text_customFont_path"] == "" {
					t.Fatalf("窄尺寸降级丢失设备驻留字体: %+v", widget.fields)
				}
			}
			if dynamicCount < testCase.dynamicMinimum {
				t.Fatalf("24×24 图层动态控件数=%d，期望至少 %d", dynamicCount, testCase.dynamicMinimum)
			}
			assertWaveshareTextWidgetsFitRect(t, textWidgets, wavesharePixelRect{x: 24, y: 24, width: 24, height: 24})
			if !reflect.DeepEqual(testCase.options, optionsBefore) {
				t.Fatalf("编译过程修改了输入 Options: before=%+v after=%+v", optionsBefore, testCase.options)
			}
		})
	}
}

func TestCompileWaveshareTextUsesEllipsisInsideVeryNarrowLayer(t *testing.T) {
	layer := theme.Layer{
		ID: "narrow-text", Type: theme.LayerText,
		Bounds:  theme.Rect{X: .1, Y: .1, Width: .1, Height: .4},
		Opacity: 1, ZIndex: 1,
		Options: map[string]any{"text": "非常狭窄的文字组件", "font_size": 180.0},
	}
	widgets := compileWaveshareTable(t, theme.Manifest{Width: 180, Height: 120, Layers: []theme.Layer{layer}}).widgets
	textWidgets := make([]waveshareThemeWidget, 0, 1)
	for _, widget := range widgets {
		if widget.fields["type"] == "8" {
			textWidgets = append(textWidgets, widget)
		}
	}
	if len(textWidgets) != 1 {
		t.Fatalf("18 像素窄文字图层控件数=%d，期望 1", len(textWidgets))
	}
	if textWidgets[0].fields["text_str"] == "非常狭窄的文字组件" {
		t.Fatal("极窄图层没有对编译占位文案做省略降级")
	}
	assertWaveshareTextWidgetsFitRect(t, textWidgets, wavesharePixelRect{x: 18, y: 12, width: 18, height: 48})
}

func assertWaveshareTextWidgetsFitRect(t *testing.T, widgets []waveshareThemeWidget, bounds wavesharePixelRect) {
	t.Helper()
	for _, widget := range widgets {
		x, xErr := strconv.Atoi(widget.fields["x"])
		y, yErr := strconv.Atoi(widget.fields["y"])
		size, sizeErr := strconv.Atoi(widget.fields["text_size"])
		if xErr != nil || yErr != nil || sizeErr != nil {
			t.Fatalf("文字控件坐标或字号不是整数: %+v", widget.fields)
		}
		textWidth := waveshareEstimatedRenderedTextWidth(widget.fields["text_str"], size)
		textHeight := waveshareRenderedTextHeight(size)
		if x < bounds.x || y < bounds.y || x+textWidth > bounds.x+bounds.width || y+textHeight > bounds.y+bounds.height {
			t.Fatalf("文字控件越出图层范围 rect=%+v rendered=%dx%d fields=%+v", bounds, textWidth, textHeight, widget.fields)
		}
	}
}
