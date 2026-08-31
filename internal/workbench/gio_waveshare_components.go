package workbench

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	caseTheme "casescreen/internal/theme"
	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

var waveshareMetricChoices = []optionChoice{
	{"Cpu_Freq", "CPU 使用率"}, {"Gpu_Load", "GPU 使用率"}, {"Memory_Load", "内存使用率"},
	{"GPU_Temp", "GPU 温度"}, {"Memory_Used", "已用内存"}, {"Memory_Available", "可用内存"},
	{"GPU_Hotspot_Temp", "GPU 热点温度"}, {"GPU_Memory_Temp", "显存温度"},
	{"time", "时间"}, {"date", "日期"}, {"week", "星期"},
	{"Cpu_Model", "CPU 型号"}, {"Gpu_Model", "GPU 型号"},
}

var waveshareFontChoices = []optionChoice{
	{"pangmen", "粗黑标题"}, {"source-han", "思源黑体"}, {"microsoft-yahei", "微软雅黑"},
	{"simhei", "黑体"}, {"digital", "液晶数字（数字/英文专用）"}, {"monospace", "等宽（数字/英文专用）"},
	{"rounded", "圆体"}, {"serif", "宋体"},
}

var waveshareLyricsFontChoices = []optionChoice{
	{"pangmen", "粗黑标题"}, {"source-han", "思源黑体"}, {"microsoft-yahei", "微软雅黑"},
	{"simhei", "黑体"}, {"rounded", "圆体"}, {"serif", "宋体"},
}

var waveshareColorChoices = []optionChoice{
	{"4CE0FF", "青"}, {"7CF29A", "绿"}, {"FFB45E", "橙"}, {"FF7DD2", "粉"},
	{"8B5CF6", "紫"}, {"4F7CFF", "蓝"}, {"FFF275", "黄"}, {"FF5C68", "红"},
	{"FFFFFF", "白"}, {"9FB5C0", "灰"},
}

var waveshareBarsStyleChoices = []optionChoice{
	{"equalizer", "均衡器"}, {"waveform", "连续波形"}, {"mirror", "镜像"}, {"center", "中心扩散"},
	{"blocks", "分段"}, {"dots", "点阵"}, {"neon", "霓虹"}, {"outline", "轮廓"}, {"burst", "四向爆发"},
}

var waveshareBarsDirectionChoices = []optionChoice{
	{"bottom", "从下向上"}, {"top", "从上向下"}, {"center", "中心对称"},
	{"horizontal", "水平"}, {"fourway", "四向发散"},
}

var genericGaugeStyleChoices = []optionChoice{
	{"arc", "圆弧"}, {"dial", "指针"}, {"semicircle", "半圆"},
	{"ring", "圆环"}, {"ticks", "刻度"}, {"minimal", "极简"},
}

var nativeWaveshareGaugeStyleChoices = []optionChoice{
	{"arc", "圆弧"}, {"dial", "密刻度（无指针）"}, {"semicircle", "半圆"},
	{"ring", "圆环"}, {"ticks", "稀刻度"}, {"minimal", "极简"},
}

var genericLineChartStyleChoices = []optionChoice{
	{"line", "折线"}, {"area", "面积"}, {"step", "阶梯"},
	{"spline", "平滑"}, {"dots", "散点"}, {"neon", "霓虹"},
}

var nativeWaveshareLineChartStyleChoices = []optionChoice{
	{"line", "折线"}, {"area", "面积"}, {"step", "阶梯→折线"},
	{"spline", "平滑→折线"}, {"dots", "散点→折线"}, {"neon", "霓虹面积"},
}

func gaugeStyleChoices(waveshare bool) []optionChoice {
	if waveshare {
		return nativeWaveshareGaugeStyleChoices
	}
	return genericGaugeStyleChoices
}

func lineChartStyleChoices(waveshare bool) []optionChoice {
	if waveshare {
		return nativeWaveshareLineChartStyleChoices
	}
	return genericLineChartStyleChoices
}

func (model *gioEditor) isWaveshareTarget() bool {
	if option := model.targetOption(model.target); option != nil && option.waveshare {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model.target)), "waveshare")
}

func isWaveshareExtendedLayer(layerType caseTheme.LayerType) bool {
	switch layerType {
	case caseTheme.LayerText, caseTheme.LayerClock, caseTheme.LayerProgressBar, caseTheme.LayerGauge,
		caseTheme.LayerGradientGauge, caseTheme.LayerLineChart, caseTheme.LayerShape:
		return true
	default:
		return false
	}
}

func (model *gioEditor) uniqueLayerID(layerType caseTheme.LayerType) string {
	base := strings.ReplaceAll(string(layerType), "_", "-")
	if base == "" {
		base = "layer"
	}
	used := make(map[string]bool, len(model.manifest.Layers))
	for _, layer := range model.manifest.Layers {
		used[layer.ID] = true
	}
	for suffix := 1; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if !used[candidate] {
			return candidate
		}
	}
}

func (model *gioEditor) selectLayer(index int) {
	if index < 0 || index >= len(model.manifest.Layers) {
		return
	}
	model.selected = index
	model.lastScaleUndo = time.Time{}
	model.componentLayerID = ""
	model.lyricsLayerID = ""
}

func (model *gioEditor) toggleLayerVisibility(index int) {
	if index < 0 || index >= len(model.manifest.Layers) {
		return
	}
	layer := &model.manifest.Layers[index]
	if model.isWaveshareTarget() && layer.Type == caseTheme.LayerBackgroundVideo && !caseTheme.LayerVisible(*layer) {
		for otherIndex, other := range model.manifest.Layers {
			if otherIndex != index && other.Type == caseTheme.LayerBackgroundVideo && caseTheme.LayerVisible(other) && other.Opacity > 0 {
				model.message = "微雪只支持一个可见视频背景，请先隐藏当前视频"
				return
			}
		}
	}
	model.pushUndo()
	if layer.Options == nil {
		layer.Options = make(map[string]any)
	}
	layer.Options["visible"] = !caseTheme.LayerVisible(*layer)
	model.message = layerLabel(layer.Type) + map[bool]string{true: "已显示", false: "已隐藏"}[caseTheme.LayerVisible(*layer)]
}

func (model *gioEditor) layoutSelectedVisibility(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	visible := caseTheme.LayerVisible(*layer)
	label := "○ 已隐藏（不在屏幕上渲染）"
	if visible {
		label = "◉ 已显示（点击隐藏）"
	}
	return model.buttonState(gtx, model.optionButton("selected-visible-"+layer.ID), label, func() {
		model.toggleLayerVisibility(model.selected)
	}, visible)
}

func (model *gioEditor) componentFieldValues(layer *caseTheme.Layer) []componentFieldValue {
	if !model.isWaveshareTarget() {
		return []componentFieldValue{
			{"X", layer.Bounds.X, func(v float64) { layer.Bounds.X = clamp01(v) }},
			{"Y", layer.Bounds.Y, func(v float64) { layer.Bounds.Y = clamp01(v) }},
			{"宽度", layer.Bounds.Width, func(v float64) { layer.Bounds.Width = clamp01(v) }},
			{"高度", layer.Bounds.Height, func(v float64) { layer.Bounds.Height = clamp01(v) }},
			{"透明度", layer.Opacity, func(v float64) { layer.Opacity = clamp01(v) }},
			{"旋转角度", layer.Rotation, func(v float64) { layer.Rotation = normalizeDegrees(v) }},
			{"层级", float64(layer.ZIndex), func(v float64) { layer.ZIndex = int(math.Round(v)) }},
		}
	}
	if layer.Type == caseTheme.LayerBackgroundVideo {
		// 微雪固件只提供一个全屏背景视频控件；几何、透明度和旋转不能独立表达。
		return nil
	}
	canvasWidth, canvasHeight := float64(max(1, model.manifest.Width)), float64(max(1, model.manifest.Height))
	pixelX := math.Round(layer.Bounds.X * canvasWidth)
	pixelY := math.Round(layer.Bounds.Y * canvasHeight)
	pixelWidth := math.Round(layer.Bounds.Width * canvasWidth)
	pixelHeight := math.Round(layer.Bounds.Height * canvasHeight)
	isImage := layer.Type == caseTheme.LayerImage || layer.Type == caseTheme.LayerBackgroundImage
	clampGeometry := func() {
		if isImage {
			layer.Bounds.Width = math.Max(1/canvasWidth, math.Min(4, layer.Bounds.Width))
			layer.Bounds.Height = math.Max(1/canvasHeight, math.Min(4, layer.Bounds.Height))
			layer.Bounds.X = math.Max(-layer.Bounds.Width+1/canvasWidth, math.Min(1-1/canvasWidth, layer.Bounds.X))
			layer.Bounds.Y = math.Max(-layer.Bounds.Height+1/canvasHeight, math.Min(1-1/canvasHeight, layer.Bounds.Y))
			return
		}
		layer.Bounds.Width = math.Max(1/canvasWidth, math.Min(1, layer.Bounds.Width))
		layer.Bounds.Height = math.Max(1/canvasHeight, math.Min(1, layer.Bounds.Height))
		layer.Bounds.X = math.Max(0, math.Min(1-layer.Bounds.Width, layer.Bounds.X))
		layer.Bounds.Y = math.Max(0, math.Min(1-layer.Bounds.Height, layer.Bounds.Y))
	}
	return []componentFieldValue{
		{"X (px)", pixelX, func(v float64) { layer.Bounds.X = math.Round(v) / canvasWidth; clampGeometry() }},
		{"Y (px)", pixelY, func(v float64) { layer.Bounds.Y = math.Round(v) / canvasHeight; clampGeometry() }},
		{"宽度 (px)", pixelWidth, func(v float64) { layer.Bounds.Width = math.Round(math.Max(1, v)) / canvasWidth; clampGeometry() }},
		{"高度 (px)", pixelHeight, func(v float64) { layer.Bounds.Height = math.Round(math.Max(1, v)) / canvasHeight; clampGeometry() }},
		{"透明度", layer.Opacity, func(v float64) { layer.Opacity = clamp01(v) }},
		{"旋转角度", layer.Rotation, func(v float64) { layer.Rotation = normalizeDegrees(v) }},
		{"层级", float64(layer.ZIndex), func(v float64) { layer.ZIndex = int(math.Round(v)) }},
	}
}

func (model *gioEditor) defaultWaveshareOptions(layerType caseTheme.LayerType) map[string]any {
	if !model.isWaveshareTarget() {
		switch layerType {
		case caseTheme.LayerTelemetry:
			return map[string]any{"visible": true, "style": "cards", "fields": []string{"time", "Cpu_Freq", "Memory_Load"}}
		case caseTheme.LayerAudioBars:
			return map[string]any{
				"visible": true, "source": "system", "style": "neon", "count": 48, "gap": 3, "thickness": 3, "bar_shape": "capsule", "direction": "bottom",
				"color": "FF54C8", "gradient_start": "FF54C8", "gradient_end": "8B5CF6", "panel_color": "00000000", "glow": true, "glow_radius": 6,
				"gain": caseTheme.DefaultAudioGain, "sensitivity": caseTheme.DefaultAudioSensitivity,
				"min_amplitude": caseTheme.DefaultAudioMinAmplitude, "max_amplitude": caseTheme.DefaultAudioMaxAmplitude,
				"smoothing": caseTheme.DefaultAudioSmoothing, "noise_gate": caseTheme.DefaultAudioNoiseGate,
				"contrast": caseTheme.DefaultAudioContrast, "separation": caseTheme.DefaultAudioSeparation, "bass_boost": caseTheme.DefaultAudioBassBoost, "floor_reduction": caseTheme.DefaultAudioFloorReduce,
			}
		case caseTheme.LayerAudioRing:
			return map[string]any{
				"visible": true, "source": "system", "style": "waveform", "count": 48, "thickness": 1, "color": "55E5FF", "inner_radius": .62, "direction": "outward",
				"gradient_start": "55E5FF", "gradient_end": "FF54C8", "panel_color": "00000000", "glow": true, "glow_radius": 6,
				"gain": caseTheme.DefaultAudioGain, "sensitivity": caseTheme.DefaultAudioSensitivity,
				"min_amplitude": caseTheme.DefaultAudioMinAmplitude, "max_amplitude": caseTheme.DefaultAudioMaxAmplitude,
				"smoothing": caseTheme.DefaultAudioSmoothing, "noise_gate": caseTheme.DefaultAudioNoiseGate,
				"contrast": caseTheme.DefaultAudioContrast, "separation": caseTheme.DefaultAudioSeparation, "bass_boost": caseTheme.DefaultAudioBassBoost, "floor_reduction": caseTheme.DefaultAudioFloorReduce,
			}
		case caseTheme.LayerLyrics:
			return defaultLyricsVisualOptions()
		}
	}
	common := map[string]any{"visible": true}
	switch layerType {
	case caseTheme.LayerTelemetry:
		return mergeOptions(common, map[string]any{
			"style": "cards", "fields": []string{"Cpu_Freq", "Gpu_Load", "Memory_Load", "time"},
			"font_family": "source-han", "font_size": 22.0, "font_weight": "medium", "show_label": true, "show_value": true, "show_unit": true,
			"label_color": "A8C4CA", "value_color": "F5FBFF", "accent_color": "4CE0FF", "panel_color": "04141BDC", "corner_radius": 10.0, "stroke_width": 2.0,
		})
	case caseTheme.LayerAudioBars:
		return mergeOptions(common, map[string]any{
			"source": "system", "style": "neon", "count": 48, "gap": 3, "thickness": 3, "bar_shape": "capsule", "direction": "bottom",
			"color": "FF54C8", "gradient_start": "FF54C8", "gradient_end": "8B5CF6", "panel_color": "00000000", "glow": true, "glow_radius": 6.0,
			"sensitivity": caseTheme.DefaultAudioSensitivity, "gain": caseTheme.DefaultAudioGain,
			"min_amplitude": caseTheme.DefaultAudioMinAmplitude, "max_amplitude": caseTheme.DefaultAudioMaxAmplitude,
			"smoothing": caseTheme.DefaultAudioSmoothing, "noise_gate": caseTheme.DefaultAudioNoiseGate,
			"contrast": caseTheme.DefaultAudioContrast, "separation": caseTheme.DefaultAudioSeparation, "bass_boost": caseTheme.DefaultAudioBassBoost, "floor_reduction": caseTheme.DefaultAudioFloorReduce,
			"peak_hold": false, "peak_hold_ms": 450.0, "peak_decay": 1.8, "peak_color": "FFF275",
		})
	case caseTheme.LayerAudioRing:
		return mergeOptions(common, map[string]any{
			"source": "system", "style": "waveform", "count": caseTheme.DefaultAudioRingCount, "thickness": 1, "inner_radius": .62, "direction": "outward",
			"color": "55E5FF", "gradient_start": "55E5FF", "gradient_end": "FF54C8", "panel_color": "00000000", "glow": true, "glow_radius": 6.0,
			"sensitivity": caseTheme.DefaultAudioSensitivity, "gain": caseTheme.DefaultAudioGain,
			"min_amplitude": caseTheme.DefaultAudioMinAmplitude, "max_amplitude": caseTheme.DefaultAudioMaxAmplitude,
			"smoothing": caseTheme.DefaultAudioSmoothing, "noise_gate": caseTheme.DefaultAudioNoiseGate,
			"contrast": caseTheme.DefaultAudioContrast, "separation": caseTheme.DefaultAudioSeparation, "bass_boost": caseTheme.DefaultAudioBassBoost, "floor_reduction": caseTheme.DefaultAudioFloorReduce,
			"peak_hold": false, "peak_hold_ms": 450.0, "peak_decay": 1.8, "peak_color": "FFF275",
		})
	case caseTheme.LayerLyrics:
		return mergeOptions(common, defaultLyricsVisualOptions())
	case caseTheme.LayerText:
		return mergeOptions(common, map[string]any{"text": "自定义文本", "font_family": "source-han", "font_size": 32.0, "font_weight": "medium", "align": "center", "color": "FFFFFF", "panel_color": "00000000", "corner_radius": 8.0, "shadow": false})
	case caseTheme.LayerClock:
		return mergeOptions(common, map[string]any{"format": "HH:mm:ss", "font_family": "digital", "font_size": 44.0, "font_weight": "bold", "align": "center", "color": "4CE0FF", "panel_color": "04141BBC", "corner_radius": 10.0, "show_seconds": true})
	case caseTheme.LayerProgressBar:
		return mergeOptions(common, map[string]any{"data_key": "Cpu_Freq", "style": "rounded", "orientation": "horizontal", "min_value": 0.0, "max_value": 100.0, "color": "4CE0FF", "secondary_color": "183740", "panel_color": "04141BBC", "corner_radius": 8.0, "show_label": true, "show_value": true})
	case caseTheme.LayerGauge:
		return mergeOptions(common, map[string]any{"data_key": "Cpu_Freq", "style": "arc", "min_value": 0.0, "max_value": 100.0, "start_angle": -140.0, "end_angle": 140.0, "stroke_width": 8.0, "color": "4CE0FF", "secondary_color": "183740", "panel_color": "04141BBC", "show_label": true, "show_value": true, "font_size": 22.0})
	case caseTheme.LayerGradientGauge:
		return mergeOptions(common, map[string]any{"data_key": "Gpu_Load", "style": "arc", "min_value": 0.0, "max_value": 100.0, "start_angle": -140.0, "end_angle": 140.0, "stroke_width": 9.0, "gradient_start": "4CE0FF", "gradient_mid": "FFF275", "gradient_end": "FF5C68", "secondary_color": "183740", "panel_color": "04141BBC", "show_label": true, "show_value": true, "font_size": 22.0})
	case caseTheme.LayerLineChart:
		return mergeOptions(common, map[string]any{"data_key": "Cpu_Freq", "style": "area", "history_points": 48, "min_value": 0.0, "max_value": 100.0, "stroke_width": 3.0, "color": "4CE0FF", "fill_color": "4CE0FF38", "grid_color": "52727E66", "panel_color": "04141BBC", "show_grid": true, "show_label": true})
	case caseTheme.LayerShape:
		return mergeOptions(common, map[string]any{"shape": "rounded_rect", "fill_color": "0B2029CC", "stroke_color": "4CE0FF", "stroke_width": 2.0, "corner_radius": 14.0})
	default:
		return common
	}
}

func defaultLyricsVisualOptions() map[string]any {
	return map[string]any{
		"visible": true, "style": "player", "font_family": "source-han", "font_weight": "bold", "align": "center",
		"font_size": caseTheme.DefaultLyricsFontSize, "next_font_size": caseTheme.DefaultLyricsNextFontSize,
		"current_color": "F7FCFF", "next_color": "B8C8DAA4", "accent_color": "55E5FF",
		"panel_color": "030A1292", "panel_border_color": "66E3FF3E", "corner_radius": 14.0,
		"outline_color": "02070EE1", "outline_width": 2.0,
		"shadow": true, "shadow_color": "000000B4", "shadow_offset_x": 2.0, "shadow_offset_y": 3.0, "shadow_blur": 3.0,
		"glow_color": "53DAFF42", "glow_radius": 4.0,
		"karaoke_highlight": true, "smooth_transition": true, "transition_fraction": .08, "show_progress": true, "progress_thickness": 3.0,
	}
}

func mergeOptions(base, extra map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func (model *gioEditor) layoutChoiceGrid(gtx layout.Context, prefix string, choices []optionChoice, current string, apply func(string), columns int) layout.Dimensions {
	columns = max(1, columns)
	rows := (len(choices) + columns - 1) / columns
	rowChildren := make([]layout.FlexChild, 0, rows)
	for row := 0; row < rows; row++ {
		row := row
		rowChildren = append(rowChildren, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			children := make([]layout.FlexChild, 0, columns)
			for column := 0; column < columns; column++ {
				index := row*columns + column
				if index >= len(choices) {
					children = append(children, layout.Flexed(1, func(layout.Context) layout.Dimensions { return layout.Dimensions{} }))
					continue
				}
				choice := choices[index]
				children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return model.buttonState(gtx, model.optionButton(prefix+"-"+choice.value), choice.label, func() {
							if current != choice.value {
								model.pushUndo()
								apply(choice.value)
							}
						}, current == choice.value)
					})
				}))
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rowChildren...)
}

func (model *gioEditor) layoutToggleGrid(gtx layout.Context, prefix string, choices []optionChoice, selected map[string]bool, apply func(string), columns int) layout.Dimensions {
	columns = max(1, columns)
	rows := (len(choices) + columns - 1) / columns
	rowChildren := make([]layout.FlexChild, 0, rows)
	for row := 0; row < rows; row++ {
		row := row
		rowChildren = append(rowChildren, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			children := make([]layout.FlexChild, 0, columns)
			for column := 0; column < columns; column++ {
				index := row*columns + column
				if index >= len(choices) {
					children = append(children, layout.Flexed(1, func(layout.Context) layout.Dimensions { return layout.Dimensions{} }))
					continue
				}
				choice := choices[index]
				children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					label := choice.label
					if selected[choice.value] {
						label = "✓ " + label
					}
					return layout.Inset{Right: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return model.buttonState(gtx, model.optionButton(prefix+"-"+choice.value), label, func() {
							model.pushUndo()
							apply(choice.value)
						}, selected[choice.value])
					})
				}))
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rowChildren...)
}

func (model *gioEditor) optionEditor(layer *caseTheme.Layer, key, fallback string) *textField {
	if model.optionEditors == nil {
		model.optionEditors = make(map[string]*textField)
	}
	id := layer.ID + "\x00" + key
	field := model.optionEditors[id]
	if field == nil {
		field = &textField{}
		field.editor.SingleLine = true
		value := layerOptionString(layer, key)
		if value == "" {
			value = fallback
		}
		field.set(value)
		model.optionEditors[id] = field
	}
	return field
}

func (model *gioEditor) layoutTextOption(gtx layout.Context, layer *caseTheme.Layer, key, label, fallback string) layout.Dimensions {
	field := model.optionEditor(layer, key, fallback)
	current := layerOptionString(layer, key)
	if current == "" {
		current = fallback
	}
	if !gtx.Focused(&field.editor) && current != field.last {
		field.set(current)
	}
	dimensions := layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(92), gtx.Metric.Dp(92)
			return material.Body2(model.theme, label).Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return model.layoutTextField(gtx, field, label) }),
	)
	value := strings.TrimSpace(field.editor.Text())
	if value != field.last {
		model.pushUndo()
		model.setSelectedOption(key, value)
		field.last = value
	}
	return dimensions
}

func (model *gioEditor) layoutNumberOption(gtx layout.Context, layer *caseTheme.Layer, key, label string, fallback, minimum, maximum float64, integer bool) layout.Dimensions {
	currentValue := caseTheme.OptionFloat(layer.Options, key, fallback)
	format := strconv.FormatFloat(currentValue, 'f', -1, 64)
	field := model.optionEditor(layer, key, format)
	if !gtx.Focused(&field.editor) && format != field.last {
		field.set(format)
	}
	dimensions := layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(92), gtx.Metric.Dp(92)
			return material.Body2(model.theme, label).Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return model.layoutTextField(gtx, field, label) }),
	)
	textValue := strings.TrimSpace(field.editor.Text())
	if textValue != field.last {
		if parsed, err := strconv.ParseFloat(textValue, 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			parsed = math.Max(minimum, math.Min(maximum, parsed))
			model.pushUndo()
			if integer {
				model.setSelectedOption(key, int(math.Round(parsed)))
			} else {
				model.setSelectedOption(key, parsed)
			}
		}
		field.last = textValue
	}
	return dimensions
}

func (model *gioEditor) layoutBoolOption(gtx layout.Context, layer *caseTheme.Layer, key, label string, fallback bool) layout.Dimensions {
	current := caseTheme.OptionBool(layer.Options, key, fallback)
	textValue := "○ " + label
	if current {
		textValue = "✓ " + label
	}
	return model.buttonState(gtx, model.optionButton(layer.ID+"-bool-"+key), textValue, func() {
		model.pushUndo()
		model.setSelectedOption(key, !current)
	}, current)
}

func (model *gioEditor) layoutColorOption(gtx layout.Context, layer *caseTheme.Layer, key, label, fallback string) layout.Dimensions {
	current := colorOptionValue(layer, key, fallback)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutTextOption(gtx, layer, key, label+" HEX", current)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutChoiceGrid(gtx, layer.ID+"-color-"+key, waveshareColorChoices, current, func(value string) {
					model.setSelectedOption(key, value)
				}, 5)
			})
		}),
	)
}

func sectionTitle(model *gioEditor, gtx layout.Context, textValue string) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		label := material.Body2(model.theme, textValue)
		label.Color = color.NRGBA{R: 113, G: 224, B: 220, A: 255}
		return label.Layout(gtx)
	})
}

// layoutDeviceCapabilityNote 明确提示屏端原生控件的能力边界，避免属性面板
// 展示主机合成效果、实际写入微雪后却悄悄降级。
func (model *gioEditor) layoutDeviceCapabilityNote(gtx layout.Context, textValue string) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		label := material.Body2(model.theme, textValue)
		label.Color = color.NRGBA{R: 164, G: 181, B: 190, A: 255}
		return label.Layout(gtx)
	})
}

func (model *gioEditor) layoutWaveshareTelemetryProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	selected := make(map[string]bool)
	for _, value := range optionStrings(layer.Options, "fields") {
		selected[value] = true
	}
	if len(selected) == 0 {
		selected["Cpu_Freq"], selected["Gpu_Load"], selected["Memory_Load"], selected["time"] = true, true, true, true
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "布局样式（8 种）") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-telemetry-style", []optionChoice{
				{"cards", "卡片"}, {"list", "列表"}, {"compact", "紧凑"}, {"gauge", "水平条"},
				{"dials", "双仪表"}, {"tiles", "瓦片"}, {"digital", "数字仪表"}, {"minimal", "极简"},
			}, defaultOption(layer, "style", "cards"), func(value string) { model.setSelectedOption("style", value) }, 4)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "数据字段（可多选、可任意组合）")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutToggleGrid(gtx, layer.ID+"-telemetry-field", waveshareMetricChoices, selected, model.toggleSelectedField, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "字体与文字") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-telemetry-font", waveshareFontChoices, defaultOption(layer, "font_family", "source-han"), func(value string) { model.setSelectedOption("font_family", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-telemetry-weight", []optionChoice{{"light", "细体"}, {"regular", "常规"}, {"medium", "中粗"}, {"bold", "粗体"}}, defaultOption(layer, "font_weight", "medium"), func(value string) { model.setSelectedOption("font_weight", value) }, 4)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "font_size", "字号 (px)", 22, 8, 160, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_label", "显示指标名称", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_value", "显示当前数值", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_unit", "显示单位", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "label_color", "标签颜色", "A8C4CA")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "value_color", "数值颜色", "F5FBFF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "accent_color", "强调颜色", "4CE0FF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "面板颜色", "04141BDC")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "形状与轮廓") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-telemetry-card-shape", []optionChoice{{"rounded", "圆角"}, {"square", "直角"}, {"pill", "胶囊"}, {"outline", "描边"}}, defaultOption(layer, "card_shape", "rounded"), func(value string) { model.setSelectedOption("card_shape", value) }, 4)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "corner_radius", "圆角 (px)", 10, 0, 100, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "stroke_width", "描边 (px)", 2, 0, 20, false)
		}),
	)
}

func (model *gioEditor) layoutAudioDynamicsProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "音频响应") }),
		layout.Rigid(model.layoutAudioPreviewSource),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "sensitivity", "灵敏度", caseTheme.DefaultAudioSensitivity, .05, 8, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "gain", "增强倍数", caseTheme.DefaultAudioGain, .1, 20, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "min_amplitude", "最小幅度", caseTheme.DefaultAudioMinAmplitude, 0, 1, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "max_amplitude", "最大幅度", 1, 0, 1, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "smoothing", "平滑度", caseTheme.DefaultAudioSmoothing, 0, .98, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "noise_gate", "静音门限", caseTheme.DefaultAudioNoiseGate, 0, .25, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "contrast", "峰谷对比", caseTheme.DefaultAudioContrast, .1, 4, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "separation", "邻频分离", caseTheme.DefaultAudioSeparation, 0, 2, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "bass_boost", "低频冲击", caseTheme.DefaultAudioBassBoost, .25, 3, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "floor_reduction", "公共底座抑制", caseTheme.DefaultAudioFloorReduce, 0, .95, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "peak_hold", "显示峰值保持", false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "peak_hold_ms", "峰值保持 (ms)", 450, 0, 5000, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "peak_decay", "峰值回落速度", 1.8, .05, 10, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "peak_color", "峰值颜色", "FFF275")
		}),
	)
}

func (model *gioEditor) layoutAudioPreviewSource(gtx layout.Context) layout.Dimensions {
	status := model.audioPreview.status
	now := model.previewMoment()
	fresh := status.Enabled && status.Running && !status.CapturedAt.IsZero() && !previewAudioBandsSilent(status.Bands) &&
		(!now.After(status.CapturedAt) || now.Sub(status.CapturedAt) <= audioPreviewStaleAfter)
	text := "预览来源：模拟频谱（等待 Windows Console 默认输出）"
	if !status.Enabled {
		text = "预览来源：模拟频谱（音频采集已关闭）"
	} else if fresh {
		endpoint := status.Endpoint
		if endpoint == "" {
			endpoint = "Windows Console 默认输出"
		}
		text = "实时来源：" + endpoint
	} else if status.Endpoint != "" {
		text = "预览来源：模拟频谱（" + status.Endpoint + " 暂无有效音频）"
	}
	label := material.Label(model.theme, unit.Sp(11), text)
	label.Color = color.NRGBA{R: 126, G: 151, B: 161, A: 255}
	return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(4)}.Layout(gtx, label.Layout)
}

func (model *gioEditor) layoutWaveshareBarsProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "频谱样式（9 种）") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-bars-style", waveshareBarsStyleChoices, defaultOption(layer, "style", "equalizer"), func(value string) { model.setSelectedOption("style", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-bars-shape", []optionChoice{{"square", "直角"}, {"rounded", "圆角"}, {"capsule", "胶囊"}, {"line", "细线"}, {"dots", "圆点"}}, defaultOption(layer, "bar_shape", "rounded"), func(value string) { model.setSelectedOption("bar_shape", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-bars-direction", waveshareBarsDirectionChoices, defaultOption(layer, "direction", "bottom"), func(value string) { model.setSelectedOption("direction", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "count", "频带数", 24, 6, 96, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "gap", "间隔 (px)", 3, 0, 24, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "thickness", "柱宽 (px)", 5, 1, 60, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "glow", "霓虹柔光", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "glow_radius", "柔光半径 (px)", 6, 0, 16, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutAudioDynamicsProperties(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "颜色与渐变") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "color", "主颜色", "4CE0FF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "gradient_start", "渐变起点", "4CE0FF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "gradient_end", "渐变终点", "8B5CF6")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "面板颜色", "05151BD2")
		}),
	)
}

func (model *gioEditor) layoutWaveshareRingProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "环状样式（8 种）") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-ring-style", []optionChoice{
				{"waveform", "连续波环"}, {"radial", "径向"}, {"donut", "光环"}, {"dots", "圆点"}, {"dashed", "刻度"},
				{"halo", "双环"}, {"spokes", "辐射"}, {"pulse", "呼吸"},
			}, defaultOption(layer, "style", "waveform"), func(value string) { model.setSelectedOption("style", value) }, 4)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-ring-direction", []optionChoice{{"outward", "向外"}, {"inward", "向内"}, {"bidirectional", "双向发散"}}, defaultOption(layer, "direction", "bidirectional"), func(value string) { model.setSelectedOption("direction", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "count", "频带数", 48, 12, 144, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "inner_radius", "内径比例", .52, .08, .9, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "thickness", "线宽 (px)", 1, 1, 30, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "glow", "霓虹柔光", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "glow_radius", "柔光半径 (px)", 6, 0, 16, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutAudioDynamicsProperties(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "颜色与渐变") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "color", "主颜色", "FF7DD2")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "gradient_start", "渐变起点", "FF7DD2")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "gradient_end", "渐变终点", "8B5CF6")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "面板颜色", "061218DC")
		}),
	)
}

func (model *gioEditor) layoutFontProperties(gtx layout.Context, layer *caseTheme.Layer, fallbackSize float64) layout.Dimensions {
	fontChoices := waveshareFontChoices
	if layer.Type == caseTheme.LayerLyrics {
		// 动态歌词始终可能出现中日韩文。不向歌词面板提供缺少 CJK
		// 字形的数码/等宽字体，避免预览可选、屏端却必须安全回退的误导。
		fontChoices = waveshareLyricsFontChoices
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-font-family", fontChoices, defaultOption(layer, "font_family", "source-han"), func(value string) { model.setSelectedOption("font_family", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if layer.Type != caseTheme.LayerLyrics {
				return layout.Dimensions{}
			}
			return model.layoutDeviceCapabilityNote(gtx, "歌词字体只显示覆盖中日韩文的字族；旧主题中的数码/等宽选项会安全回退为思源黑体。")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-font-weight", []optionChoice{{"light", "细体"}, {"regular", "常规"}, {"medium", "中粗"}, {"bold", "粗体"}}, defaultOption(layer, "font_weight", "medium"), func(value string) { model.setSelectedOption("font_weight", value) }, 4)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "font_size", "字号 (px)", fallbackSize, 8, 220, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-font-align", []optionChoice{{"left", "左对齐"}, {"center", "居中"}, {"right", "右对齐"}}, defaultOption(layer, "align", "center"), func(value string) { model.setSelectedOption("align", value) }, 3)
		}),
	)
}

func (model *gioEditor) layoutDataKeyProperties(gtx layout.Context, layer *caseTheme.Layer, fallback string) layout.Dimensions {
	return model.layoutChoiceGrid(gtx, layer.ID+"-data-key", waveshareMetricChoices, defaultOption(layer, "data_key", fallback), func(value string) {
		model.setSelectedOption("data_key", value)
	}, 3)
}

func (model *gioEditor) layoutOptionalDataKeyProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	choices := make([]optionChoice, 0, len(waveshareMetricChoices)+1)
	choices = append(choices, optionChoice{"", "不绑定（固定文本）"})
	choices = append(choices, waveshareMetricChoices...)
	return model.layoutChoiceGrid(gtx, layer.ID+"-optional-data-key", choices, layerOptionString(layer, "data_key"), func(value string) {
		model.setSelectedOption("data_key", value)
	}, 3)
}

func (model *gioEditor) layoutTextProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutTextOption(gtx, layer, "text", "文本内容", "自定义文本")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "可选的实时数据绑定")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutOptionalDataKeyProperties(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "字体与排版") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutFontProperties(gtx, layer, 32) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "shadow", "文字阴影", false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "color", "文字颜色", "FFFFFF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "背景颜色", "00000000")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "corner_radius", "圆角 (px)", 8, 0, 100, false)
		}),
	)
}

func (model *gioEditor) layoutClockProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "时间 / 日期格式") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-clock-format", []optionChoice{
				{"HH:mm:ss", "15:04:05"}, {"HH:mm", "15:04"}, {"yyyy-MM-dd", "2026-08-26"},
				{"MM-dd HH:mm", "08-26 15:04"}, {"yyyy/MM/dd HH:mm", "2026/08/26 15:04"}, {"week HH:mm", "周三 15:04"},
			}, defaultOption(layer, "format", "HH:mm:ss"), func(value string) { model.setSelectedOption("format", value) }, 2)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutTextOption(gtx, layer, "format", "自定义格式", defaultOption(layer, "format", "HH:mm:ss"))
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "字体与外观") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutFontProperties(gtx, layer, 44) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "color", "文字颜色", "4CE0FF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "背景颜色", "04141BBC")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "corner_radius", "圆角 (px)", 10, 0, 100, false)
		}),
	)
}

func (model *gioEditor) layoutValueRangeProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "min_value", "最小值", 0, -100000, 100000, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "max_value", "最大值", 100, -100000, 100000, false)
		}),
	)
}

func (model *gioEditor) layoutProgressProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "绑定的遥测指标") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutDataKeyProperties(gtx, layer, "Cpu_Freq")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "进度条样式") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-progress-style", []optionChoice{{"rounded", "圆角"}, {"square", "直角"}, {"segmented", "分段"}, {"dots", "圆点"}, {"neon", "霓虹"}, {"outline", "轮廓"}}, defaultOption(layer, "style", "rounded"), func(value string) { model.setSelectedOption("style", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-progress-orientation", []optionChoice{{"horizontal", "水平"}, {"vertical", "垂直"}, {"reverse", "反向"}}, defaultOption(layer, "orientation", "horizontal"), func(value string) { model.setSelectedOption("orientation", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutValueRangeProperties(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_label", "显示指标名称", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_value", "显示当前数值", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "color", "填充颜色", "4CE0FF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "secondary_color", "轨道颜色", "183740")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "面板颜色", "04141BBC")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "corner_radius", "圆角 (px)", 8, 0, 100, false)
		}),
	)
}

func (model *gioEditor) layoutGaugeProperties(gtx layout.Context, layer *caseTheme.Layer, gradient bool) layout.Dimensions {
	waveshare := model.isWaveshareTarget()
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "绑定的遥测指标") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutDataKeyProperties(gtx, layer, "Cpu_Freq")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "仪表样式") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-gauge-style", gaugeStyleChoices(waveshare), defaultOption(layer, "style", "arc"), func(value string) { model.setSelectedOption("style", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !waveshare || !strings.EqualFold(defaultOption(layer, "style", "arc"), "dial") {
				return layout.Dimensions{}
			}
			return model.layoutDeviceCapabilityNote(gtx, "微雪原生圆弧控件没有指针：此样式会保存为更密的刻度圆弧。")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutValueRangeProperties(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "start_angle", "起始角度", -140, -360, 360, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "end_angle", "结束角度", 140, -360, 360, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "stroke_width", "环宽 (px)", 8, 1, 50, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "font_size", "数值字号", 22, 8, 160, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_label", "显示指标名称", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_value", "显示当前数值", true)
		}),
	}
	if gradient {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutColorOption(gtx, layer, "gradient_start", "低值颜色", "4CE0FF")
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutColorOption(gtx, layer, "gradient_mid", "中值颜色", "FFF275")
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutColorOption(gtx, layer, "gradient_end", "高值颜色", "FF5C68")
			}),
		)
	} else {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "color", "仪表颜色", "4CE0FF")
		}))
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "secondary_color", "轨道颜色", "183740")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "面板颜色", "04141BBC")
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layoutLineChartProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	waveshare := model.isWaveshareTarget()
	style := strings.ToLower(defaultOption(layer, "style", "area"))
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "绑定的遥测指标") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutDataKeyProperties(gtx, layer, "Cpu_Freq")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "曲线样式") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-chart-style", lineChartStyleChoices(waveshare), defaultOption(layer, "style", "area"), func(value string) { model.setSelectedOption("style", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutValueRangeProperties(gtx, layer) }),
	}
	if waveshare {
		note := "微雪屏端的历史点数和线宽由固件决定。"
		if style == "step" || style == "spline" || style == "dots" {
			note += " 当前样式会按原生折线显示。"
		}
		if style == "spline" || style == "dots" || style == "neon" {
			note += " 该原生样式固定不显示网格。"
		}
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutDeviceCapabilityNote(gtx, note)
		}))
	} else {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutNumberOption(gtx, layer, "history_points", "历史点数", 48, 8, 240, true)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutNumberOption(gtx, layer, "stroke_width", "线宽 (px)", 3, 1, 20, false)
			}),
		)
	}
	if !waveshare || (style != "spline" && style != "dots" && style != "neon") {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_grid", "显示网格", true)
		}))
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "show_label", "显示指标与当前值", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "color", "曲线颜色", "4CE0FF")
		}),
	)
	if !waveshare || style == "area" || style == "neon" {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "fill_color", "面积颜色", "4CE0FF38")
		}))
	}
	if !waveshare || (style != "spline" && style != "dots" && style != "neon") {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "grid_color", "网格颜色", "52727E66")
		}))
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "面板颜色", "04141BBC")
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layoutShapeProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "形状") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-shape", []optionChoice{{"rect", "矩形"}, {"rounded_rect", "圆角矩形"}, {"circle", "正圆"}, {"ellipse", "椭圆"}, {"line", "直线"}, {"pill", "胶囊"}}, defaultOption(layer, "shape", "rounded_rect"), func(value string) { model.setSelectedOption("shape", value) }, 3)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "fill_color", "填充颜色", "0B2029CC")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "stroke_color", "描边颜色", "4CE0FF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "stroke_width", "描边 (px)", 2, 0, 40, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "corner_radius", "圆角 (px)", 14, 0, 200, false)
		}),
	)
}

func (model *gioEditor) previewScale(layer caseTheme.Layer, size image.Point) float64 {
	physicalWidth := layer.Bounds.Width * float64(max(1, model.manifest.Width))
	physicalHeight := layer.Bounds.Height * float64(max(1, model.manifest.Height))
	if physicalWidth <= 0 || physicalHeight <= 0 {
		return 1
	}
	return minFloat(float64(size.X)/physicalWidth, float64(size.Y)/physicalHeight)
}

func (model *gioEditor) previewFontSize(layer caseTheme.Layer, size image.Point, fallback float64) unit.Sp {
	value := caseTheme.OptionFloat(layer.Options, "font_size", fallback) * model.previewScale(layer, size)
	return unit.Sp(math.Max(7, math.Min(72, value)))
}

func (model *gioEditor) previewPanel(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	panel := previewColor(layer, "panel_color", fallback)
	radius := int(math.Round(caseTheme.OptionFloat(layer.Options, "corner_radius", 8) * model.previewScale(layer, size)))
	radius = max(0, min(min(size.X, size.Y)/2, radius))
	paint.FillShape(gtx.Ops, panel, clip.UniformRRect(image.Rectangle{Max: size}, radius).Op(gtx.Ops))
}

func (model *gioEditor) drawWaveshareTelemetryPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 220})
	fields := waveshareTelemetryPreviewFields(layer.Options)
	labelColor := previewColor(layer, "label_color", color.NRGBA{R: 168, G: 196, B: 202, A: 255})
	valueColor := previewColor(layer, "value_color", color.NRGBA{R: 245, G: 251, B: 255, A: 255})
	accent := previewColor(layer, "accent_color", fallback)
	showLabel := caseTheme.OptionBool(layer.Options, "show_label", true)
	showValue := caseTheme.OptionBool(layer.Options, "show_value", true)
	showUnit := caseTheme.OptionBool(layer.Options, "show_unit", true)
	fontSize := model.previewFontSize(layer, size, 22)
	style := strings.ToLower(defaultOption(&layer, "style", "cards"))
	labelLayer := waveshareTelemetryPreviewLabelLayer(layer)
	columns, rows := waveshareTelemetryPreviewGrid(style, len(fields))
	cellWidth, cellHeight := max(1, size.X/columns), max(1, size.Y/max(1, rows))
	if style == "cards" || style == "tiles" || style == "digital" {
		model.drawWaveshareTelemetryCards(gtx, size, layer, len(fields), columns, rows)
	}
	for index, key := range fields {
		column, row := index%columns, index/columns
		x, y := column*cellWidth, row*cellHeight
		valueText, ratio := sampleMetricValueAt(key, model.previewMoment())
		valueText = waveshareTelemetryPreviewValue(valueText, showUnit)
		if style == "gauge" {
			padding := max(1, min(5, min(cellWidth, cellHeight)/8))
			labelWidth, valueWidth := 0, 0
			if showLabel {
				labelWidth = min(max(1, cellWidth/4), max(1, cellWidth-padding*2))
				labelBounds := image.Rect(x+padding, y+padding, min(x+cellWidth-padding, x+padding+labelWidth), max(y+padding+1, y+cellHeight-padding))
				placement := waveshareFitPreviewTextLayout(metricLabel(key), fontSize, labelBounds, "left", "center")
				model.previewStyledTextAt(gtx, placement.point, metricLabel(key), placement.size, labelColor, labelLayer)
			}
			if showValue {
				valueWidth = min(max(1, cellWidth/4), max(1, cellWidth-padding*2-labelWidth))
			}
			barX := x + padding + labelWidth + min(padding, max(0, cellWidth-labelWidth-valueWidth-padding*2))
			barRight := max(barX+1, x+cellWidth-padding-valueWidth)
			barHeight := max(1, min(cellHeight/3, 18))
			barY := y + (cellHeight-barHeight)/2
			track := image.Rect(barX, barY, barRight, barY+barHeight)
			paint.FillShape(gtx.Ops, color.NRGBA{R: 28, G: 57, B: 65, A: 255}, clip.UniformRRect(track, min(4, barHeight/2)).Op(gtx.Ops))
			fill := track
			fill.Max.X = fill.Min.X + int(float64(fill.Dx())*ratio)
			paint.FillShape(gtx.Ops, accent, clip.UniformRRect(fill, min(4, barHeight/2)).Op(gtx.Ops))
			if showValue {
				valueBounds := image.Rect(max(x+padding, x+cellWidth-padding-valueWidth), y+padding, max(x+padding+1, x+cellWidth-padding), max(y+padding+1, y+cellHeight-padding))
				placement := waveshareFitPreviewTextLayout(valueText, fontSize, valueBounds, "right", "center")
				model.previewStyledTextAt(gtx, placement.point, valueText, placement.size, valueColor, layer)
			}
			continue
		}
		if style == "dials" {
			center := image.Pt(x+cellWidth/2, y+cellHeight/2)
			radius := max(1, (max(2, min(cellWidth, cellHeight)-8))/2)
			model.drawPreviewArc(gtx, center, radius, -140, 140, max(2, radius/10), color.NRGBA{R: 28, G: 57, B: 65, A: 255}, 1)
			model.drawPreviewArc(gtx, center, radius, -140, -140+280*ratio, max(2, radius/10), accent, 1)
			if showValue {
				valueBounds := image.Rect(x+1, y+cellHeight/3, x+cellWidth-1, y+max(cellHeight/3+1, cellHeight*2/3))
				placement := waveshareFitPreviewTextLayout(valueText, fontSize, valueBounds, "center", "center")
				model.previewStyledTextAt(gtx, placement.point, valueText, placement.size, valueColor, layer)
			}
			if showLabel {
				labelBounds := image.Rect(x+1, y+1, x+cellWidth-1, y+max(2, cellHeight/3))
				placement := waveshareFitPreviewTextLayout(metricLabel(key), unit.Sp(maxFloat(3, float64(fontSize)-4)), labelBounds, "center", "top")
				model.previewStyledTextAt(gtx, placement.point, metricLabel(key), placement.size, labelColor, labelLayer)
			}
			continue
		}
		if style == "minimal" {
			if showValue {
				valueBounds := image.Rect(x+2, y+1, max(x+3, x+cellWidth-2), max(y+2, y+cellHeight-1))
				placement := waveshareFitPreviewTextLayout(valueText, fontSize, valueBounds, "left", "center")
				model.previewStyledTextAt(gtx, placement.point, valueText, placement.size, valueColor, layer)
			}
			continue
		}
		labelText := metricLabel(key)
		labelPreferred := maxFloat(9, math.Min(float64(fontSize)-5, float64(cellHeight)/4))
		labelSize := waveshareFitTelemetryPreviewText(labelText, labelPreferred, cellWidth-12, max(8, cellHeight/3))
		valuePreferred := maxFloat(12, math.Min(float64(fontSize), float64(cellHeight)/2))
		if style == "digital" {
			valuePreferred = maxFloat(valuePreferred, math.Min(96, float64(cellHeight)*2/3))
		}
		if showLabel {
			model.previewStyledTextAt(gtx, image.Pt(x+6, y+4), labelText, labelSize, labelColor, labelLayer)
		}
		if showValue {
			valueTop := y + 4
			valueHeight := max(6, cellHeight-8)
			if showLabel {
				labelHeight := int(math.Ceil(float64(labelSize) * 1.22))
				valueTop = y + 4 + labelHeight + 2
				valueHeight = max(6, y+cellHeight-4-valueTop)
			}
			valueSize := waveshareFitTelemetryPreviewText(valueText, valuePreferred, cellWidth-12, valueHeight)
			if !showLabel {
				lineHeight := int(math.Ceil(float64(valueSize) * 1.22))
				valueTop = y + max(4, (cellHeight-lineHeight)/2)
			}
			model.previewStyledTextAt(gtx, image.Pt(x+6, valueTop), valueText, valueSize, valueColor, layer)
		}
	}
}

// waveshareTelemetryPreviewLabelLayer 让液晶数字字体只作用于数值；中文标签使用屏端同款中文字库。
func waveshareTelemetryPreviewLabelLayer(layer caseTheme.Layer) caseTheme.Layer {
	if !strings.EqualFold(layerOptionString(&layer, "font_family"), "digital") {
		return layer
	}
	options := make(map[string]any, len(layer.Options)+1)
	for key, value := range layer.Options {
		options[key] = value
	}
	options["font_family"] = "source-han"
	layer.Options = options
	return layer
}

// wavesharePreviewTextPlacement 记录单行文字经过宽高拟合后的字号、起点与估算占用。
// Gio 的标签最终仍使用真实字体渲染；这里采用略保守的字宽和行高，避免缩小组件时出现明显裁切。
type wavesharePreviewTextPlacement struct {
	point  image.Point
	size   unit.Sp
	extent image.Point
}

func wavesharePreviewTextWidthUnits(value string) float64 {
	units := 0.0
	for _, current := range strings.TrimSpace(value) {
		switch {
		case current == ' ' || current == '\t':
			units += .38
		case current <= 0x7f && strings.ContainsRune(".,:;!|'`", current):
			units += .42
		case current <= 0x7f:
			units += .68
		default:
			units += 1.05
		}
	}
	return maxFloat(1, units)
}

// waveshareFitPreviewTextSize 同时受可用宽高约束。极小组件允许字号低于常规 UI 下限，
// 否则 24×24 画布中的中文标签只能被硬裁切；正常尺寸仍以用户设置的字号为上限。
func waveshareFitPreviewTextSize(value string, preferred unit.Sp, maxWidth, maxHeight int) unit.Sp {
	if preferred <= 0 {
		preferred = 12
	}
	widthLimit := float64(max(1, maxWidth)-1) / wavesharePreviewTextWidthUnits(value)
	heightLimit := float64(max(1, maxHeight)-1) / 1.28
	fitted := minFloat(float64(preferred), minFloat(widthLimit, heightLimit))
	fitted = math.Floor(maxFloat(.5, fitted)*4) / 4
	return unit.Sp(maxFloat(.5, fitted))
}

// waveshareFitPreviewTextLayout 在给定矩形内完成字号拟合和水平、垂直对齐。
// align/vertical 支持 left|center|right 与 top|center|bottom。
func waveshareFitPreviewTextLayout(value string, preferred unit.Sp, bounds image.Rectangle, align, vertical string) wavesharePreviewTextPlacement {
	if bounds.Max.X <= bounds.Min.X {
		bounds.Max.X = bounds.Min.X + 1
	}
	if bounds.Max.Y <= bounds.Min.Y {
		bounds.Max.Y = bounds.Min.Y + 1
	}
	fitted := waveshareFitPreviewTextSize(value, preferred, bounds.Dx(), bounds.Dy())
	extent := image.Pt(
		max(1, int(math.Ceil(wavesharePreviewTextWidthUnits(value)*float64(fitted)))),
		max(1, int(math.Ceil(float64(fitted)*1.28))),
	)
	point := bounds.Min
	switch strings.ToLower(strings.TrimSpace(align)) {
	case "center":
		point.X += max(0, (bounds.Dx()-extent.X)/2)
	case "right":
		point.X += max(0, bounds.Dx()-extent.X)
	}
	switch strings.ToLower(strings.TrimSpace(vertical)) {
	case "center":
		point.Y += max(0, (bounds.Dy()-extent.Y)/2)
	case "bottom":
		point.Y += max(0, bounds.Dy()-extent.Y)
	}
	return wavesharePreviewTextPlacement{point: point, size: fitted, extent: extent}
}

// waveshareFitClockPreviewTextLayout 为单行时钟额外预留少量字形宽度。
// Gio 会按系统实际回退字体排版，液晶数字/粗体的真实 advance 可能略大于通用字符估算；
// 先缩小字号可保留完整 HH:mm:ss，只有缩到通用最小字号仍放不下时才由 Label 省略。
func waveshareFitClockPreviewTextLayout(value string, preferred unit.Sp, bounds image.Rectangle, align, vertical string) wavesharePreviewTextPlacement {
	width := bounds.Dx()
	if width <= 2 {
		return waveshareFitPreviewTextLayout(value, preferred, bounds, align, vertical)
	}
	reserve := max(1, int(math.Ceil(float64(width)*.1)))
	reserve = min(reserve, width-1)
	safeBounds := bounds
	safeBounds.Min.X += reserve / 2
	safeBounds.Max.X -= reserve - reserve/2
	return waveshareFitPreviewTextLayout(value, preferred, safeBounds, align, vertical)
}

// waveshareFitTelemetryPreviewText 复用通用拟合规则，遥测单元格缩到极小时也不会强制保留大字号。
func waveshareFitTelemetryPreviewText(value string, preferred float64, maxWidth, maxHeight int) unit.Sp {
	return waveshareFitPreviewTextSize(value, unit.Sp(preferred), maxWidth, maxHeight)
}

var waveshareTelemetryPreviewOrder = []string{
	"Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp", "Memory_Used", "Memory_Available",
	"time", "date", "week", "Cpu_Model", "Gpu_Model", "vol", "cur_dB", "avg_dB", "max_dB",
}

func waveshareTelemetryPreviewFields(options map[string]any) []string {
	requested := optionStrings(options, "fields")
	if len(requested) == 0 {
		requested = []string{"time", "Cpu_Freq", "Memory_Load"}
	}
	selected := make(map[string]bool, len(requested))
	for _, key := range requested {
		selected[key] = true
	}
	fields := make([]string, 0, len(selected))
	for _, key := range waveshareTelemetryPreviewOrder {
		if selected[key] {
			fields = append(fields, key)
		}
	}
	if len(fields) == 0 {
		return []string{"Cpu_Freq", "Memory_Load"}
	}
	return fields
}

func waveshareTelemetryPreviewGrid(style string, count int) (columns, rows int) {
	count = max(1, count)
	columns = count
	switch strings.ToLower(style) {
	case "list", "minimal", "gauge":
		columns = 1
	case "tiles":
		columns = min(3, count)
	case "compact":
		columns = min(6, count)
	default: // cards、digital、dials
		columns = min(4, count)
	}
	return columns, (count + columns - 1) / columns
}

func waveshareTelemetryPreviewValue(value string, showUnit bool) string {
	if showUnit {
		return value
	}
	value = strings.TrimSpace(value)
	for _, suffix := range []string{"GB", "℃", "%"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return strings.TrimSpace(value)
}

func (model *gioEditor) drawWaveshareTelemetryCards(gtx layout.Context, size image.Point, layer caseTheme.Layer, count, columns, rows int) {
	gap := max(1, int(math.Round(4*model.previewScale(layer, size))))
	cardWidth := max(12, (size.X-gap*(columns-1))/columns)
	cardHeight := max(12, (size.Y-gap*(rows-1))/rows)
	cardFill := previewColor(layer, "card_color", color.NRGBA{R: 17, G: 44, B: 53, A: 217})
	accent := previewColor(layer, "accent_color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	corner := max(0, int(math.Round(caseTheme.OptionFloat(layer.Options, "corner_radius", 10)*model.previewScale(layer, size))))
	strokeWidth := max(1, int(math.Round(caseTheme.OptionFloat(layer.Options, "stroke_width", 2)*model.previewScale(layer, size))))
	shape := strings.ToLower(defaultOption(&layer, "card_shape", "rounded"))
	for index := 0; index < count; index++ {
		column, row := index%columns, index/columns
		x, y := column*(cardWidth+gap), row*(cardHeight+gap)
		card := image.Rect(x, y, min(size.X, x+cardWidth), min(size.Y, y+cardHeight))
		switch shape {
		case "square":
			paint.FillShape(gtx.Ops, cardFill, clip.Rect(card).Op())
		case "pill":
			paint.FillShape(gtx.Ops, cardFill, clip.UniformRRect(card, card.Dy()/2).Op(gtx.Ops))
		case "outline":
			paint.FillShape(gtx.Ops, accent, clip.UniformRRect(card, corner).Op(gtx.Ops))
			inner := image.Rect(card.Min.X+strokeWidth, card.Min.Y+strokeWidth, card.Max.X-strokeWidth, card.Max.Y-strokeWidth)
			if !inner.Empty() {
				paint.FillShape(gtx.Ops, cardFill, clip.UniformRRect(inner, max(0, corner-strokeWidth)).Op(gtx.Ops))
			}
		default:
			paint.FillShape(gtx.Ops, cardFill, clip.UniformRRect(card, corner).Op(gtx.Ops))
		}
	}
}

// waveshareNativeLinearPreviewLayout 与 compileWaveshareHostAudioBars 的
// wavesharePackedLinearLayout 保持同一套约束。尤其是水平频谱必须按图层高度
// 容纳用户请求的频带数，不能拿竖向柱宽反推后悄悄减少频带。
func waveshareNativeLinearPreviewLayout(length, requestedCount, requestedGap int) (count, itemSize, gap int) {
	length = max(1, length)
	count = max(1, min(max(1, requestedCount), length))
	gap = max(0, requestedGap)
	if count > 1 {
		gap = min(gap, max(0, (length-count)/(count-1)))
	} else {
		gap = 0
	}
	itemSize = max(1, (length-gap*(count-1))/count)
	return count, itemSize, gap
}

func (model *gioEditor) drawWaveshareBarsPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	style := strings.ToLower(defaultOption(&layer, "style", "equalizer"))
	direction := waveshareBarsPreviewDirection(layer)
	if direction == "fourway" || style == "burst" {
		options := make(map[string]any, len(layer.Options)+3)
		for key, value := range layer.Options {
			options[key] = value
		}
		options["style"] = "spokes"
		options["direction"] = "outward"
		if _, ok := options["inner_radius"]; !ok {
			options["inner_radius"] = .46
		}
		layer.Options = options
		model.drawWaveshareRingPreview(gtx, size, layer, fallback)
		return
	}
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 5, G: 21, B: 27, A: 210})
	requestedCount := max(6, min(96, previewInt(layer, "count", 24)))
	requestedGap := max(0, min(24, previewInt(layer, "gap", 3)))
	axisLength := size.X
	if strings.EqualFold(layerOptionString(&layer, "direction"), "horizontal") {
		axisLength = size.Y
	}
	count, barSlot, gap := waveshareNativeLinearPreviewLayout(axisLength, requestedCount, requestedGap)
	barWidth := max(1, min(barSlot, previewInt(layer, "thickness", max(1, int(math.Round(float64(barSlot)*.52))))))
	values, peaks := model.audioPreviewFrame(layer, count)
	values = model.visibleAudioPreviewValues(values)
	start := previewColor(layer, "gradient_start", previewColor(layer, "color", fallback))
	end := previewColor(layer, "gradient_end", start)
	if (style == "wave" || style == "waveform") && !model.isWaveshareTarget() {
		// 主机合成目标能绘制连续折线；微雪屏端只能用驻留进度柱近似，
		// 其预览继续走下方离散柱路径，避免画布与实体屏表现不一致。
		model.drawWaveshareWaveformPreview(gtx, size, layer, values, start, end, barWidth)
		return
	}
	peakEnabled := caseTheme.OptionBool(layer.Options, "peak_hold", false)
	peakColor := previewColor(layer, "peak_color", color.NRGBA{R: 255, G: 242, B: 117, A: 150})
	peakColor.A = uint8(float64(peakColor.A) * .58)
	shape := strings.ToLower(defaultOption(&layer, "bar_shape", "rounded"))
	glowEnabled := caseTheme.OptionBool(layer.Options, "glow", style == "neon")
	glowRadius := max(0, min(16, previewInt(layer, "glow_radius", 6)))
	paintBar := func(bar image.Rectangle, fill color.NRGBA, paintStyle string) {
		if glowEnabled && glowRadius > 0 {
			model.paintPreviewBarGlow(gtx, bar.Intersect(image.Rect(0, 0, size.X, size.Y)), fill, glowRadius)
		}
		model.paintPreviewBar(gtx, bar, fill, shape, paintStyle, direction == "horizontal")
	}
	for index, value := range values {
		foreground := interpolateColor(start, end, float64(index)/float64(max(1, count-1)))
		peakValue := 0.0
		if peakEnabled && index < len(peaks) {
			peakValue = clamp01(peaks[index])
		}
		if direction == "horizontal" {
			y := index*(barSlot+gap) + (barSlot-barWidth)/2
			if value > 0 {
				length := max(1, int(float64(size.X-8)*value))
				bar := image.Rect(4, y, min(size.X, 4+length), min(size.Y, y+barWidth))
				paintBar(bar, foreground, style)
			}
			if peakValue > 0 {
				peakX := min(size.X-2, 4+int(float64(size.X-8)*peakValue))
				markerWidth := max(1, min(3, barWidth/2+1))
				model.paintPreviewPeakMarker(gtx, image.Rect(max(0, peakX-markerWidth/2), y, min(size.X, peakX+(markerWidth+1)/2), min(size.Y, y+barWidth)), peakColor)
			}
			continue
		}
		x := index*(barSlot+gap) + (barSlot-barWidth)/2
		if value > 0 {
			height := max(1, int(float64(size.Y-8)*value))
			var bar image.Rectangle
			switch direction {
			case "top":
				bar = image.Rect(x, 4, min(size.X, x+barWidth), min(size.Y, 4+height))
			case "center":
				bar = image.Rect(x, max(0, size.Y/2-height/2), min(size.X, x+barWidth), min(size.Y, size.Y/2+height/2))
			default:
				bar = image.Rect(x, max(0, size.Y-height-4), min(size.X, x+barWidth), size.Y-4)
			}
			paintBar(bar, foreground, style)
			if style == "mirror" && direction != "center" {
				reflection := bar
				reflection.Min.Y, reflection.Max.Y = 4, min(size.Y, 4+max(1, height/3))
				foreground.A /= 3
				paintBar(reflection, foreground, style)
			}
		}
		if peakValue > 0 {
			peakHeight := max(1, int(float64(size.Y-8)*peakValue))
			markerHeight := max(1, min(3, barWidth/2+1))
			paintMarker := func(y int) {
				model.paintPreviewPeakMarker(gtx, image.Rect(x, max(0, y-markerHeight/2), min(size.X, x+barWidth), min(size.Y, y+(markerHeight+1)/2)), peakColor)
			}
			switch direction {
			case "top":
				paintMarker(min(size.Y-1, 4+peakHeight))
			case "center":
				paintMarker(max(0, size.Y/2-peakHeight/2))
				paintMarker(min(size.Y-1, size.Y/2+peakHeight/2))
			default:
				paintMarker(max(0, size.Y-4-peakHeight))
			}
		}
	}
}

func waveshareBarsPreviewDirection(layer caseTheme.Layer) string {
	direction := strings.ToLower(defaultOption(&layer, "direction", "bottom"))
	if strings.EqualFold(defaultOption(&layer, "style", "equalizer"), "center") {
		// 与微雪编译器一致：中心扩散样式本身就决定双向绘制，
		// 不应再要求用户额外把方向切换为“中心对称”。
		return "center"
	}
	return direction
}

// drawWaveshareWaveformPreview 用真实频带端点连成连续谱线；中心模式绘制
// 上下两条对称轮廓，接近音乐播放器的双向波形，而不是规则正弦动画。
func (model *gioEditor) drawWaveshareWaveformPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, values []float64, start, end color.NRGBA, thickness int) {
	values = model.visibleAudioPreviewValues(values)
	if len(values) < 2 {
		return
	}
	active := false
	for _, value := range values {
		if value > 0 {
			active = true
			break
		}
	}
	if !active {
		return
	}
	direction := strings.ToLower(defaultOption(&layer, "direction", "bottom"))
	glowEnabled := caseTheme.OptionBool(layer.Options, "glow", true)
	glowRadius := max(0, min(16, previewInt(layer, "glow_radius", 6)))
	thickness = max(1, min(12, thickness))
	point := func(index int, value float64, mirror bool) image.Point {
		if direction == "horizontal" {
			y := 4 + index*max(1, size.Y-8)/max(1, len(values)-1)
			x := 4 + int(math.Round(value*float64(max(1, size.X-8))))
			if mirror {
				x = size.X - x
			}
			return image.Pt(x, y)
		}
		x := 4 + index*max(1, size.X-8)/max(1, len(values)-1)
		height := int(math.Round(value * float64(max(1, size.Y-8))))
		switch direction {
		case "top":
			return image.Pt(x, 4+height)
		case "center":
			offset := max(1, height/2)
			if mirror {
				return image.Pt(x, size.Y/2+offset)
			}
			return image.Pt(x, size.Y/2-offset)
		default:
			return image.Pt(x, size.Y-4-height)
		}
	}
	drawPath := func(mirror bool) {
		previous := point(0, values[0], mirror)
		for index := 1; index < len(values); index++ {
			current := point(index, values[index], mirror)
			fill := interpolateColor(start, end, float64(index)/float64(max(1, len(values)-1)))
			if glowEnabled && glowRadius > 0 {
				model.previewSpectrumGlowLine(gtx, previous.X, previous.Y, current.X, current.Y, thickness, glowRadius, fill)
			}
			model.previewSpectrumLine(gtx, previous.X, previous.Y, current.X, current.Y, thickness, fill, true)
			previous = current
		}
	}
	drawPath(false)
	if direction == "center" || direction == "horizontal" {
		drawPath(true)
	}
}

func (model *gioEditor) paintPreviewBarGlow(gtx layout.Context, bar image.Rectangle, fill color.NRGBA, radius int) {
	if bar.Empty() || radius <= 0 {
		return
	}
	for _, scale := range []int{radius, max(1, radius/2)} {
		glow := image.Rect(bar.Min.X-scale, bar.Min.Y-scale, bar.Max.X+scale, bar.Max.Y+scale)
		glowColor := fill
		alpha := .15
		if scale == radius {
			alpha = .08
		}
		glowColor.A = uint8(float64(glowColor.A) * alpha)
		paint.FillShape(gtx.Ops, glowColor, clip.UniformRRect(glow, min(glow.Dx(), glow.Dy())/2).Op(gtx.Ops))
	}
}

func (model *gioEditor) paintPreviewPeakMarker(gtx layout.Context, marker image.Rectangle, fill color.NRGBA) {
	if marker.Empty() {
		return
	}
	radius := min(2, min(marker.Dx(), marker.Dy())/2)
	paint.FillShape(gtx.Ops, fill, clip.UniformRRect(marker, radius).Op(gtx.Ops))
}

func (model *gioEditor) paintPreviewBar(gtx layout.Context, bar image.Rectangle, fill color.NRGBA, shape, style string, horizontal bool) {
	if bar.Empty() {
		return
	}
	if style == "blocks" {
		axisLength := bar.Dy()
		if horizontal {
			axisLength = bar.Dx()
		}
		segments := max(2, axisLength/6)
		for segment := 0; segment < segments; segment++ {
			if horizontal {
				x0 := bar.Min.X + segment*bar.Dx()/segments
				x1 := bar.Min.X + (segment+1)*bar.Dx()/segments - 1
				if x1 > x0 {
					paint.FillShape(gtx.Ops, fill, clip.Rect{Min: image.Pt(x0, bar.Min.Y), Max: image.Pt(x1, bar.Max.Y)}.Op())
				}
			} else {
				y0 := bar.Max.Y - (segment+1)*bar.Dy()/segments
				y1 := bar.Max.Y - segment*bar.Dy()/segments - 1
				if y1 > y0 {
					paint.FillShape(gtx.Ops, fill, clip.Rect{Min: image.Pt(bar.Min.X, y0), Max: image.Pt(bar.Max.X, y1)}.Op())
				}
			}
		}
		return
	}
	if shape == "dots" || style == "dots" {
		radius := max(1, min(bar.Dx(), bar.Dy())/2)
		if horizontal {
			for x := bar.Min.X + radius; x <= bar.Max.X; x += max(2, radius*3) {
				paint.FillShape(gtx.Ops, fill, clip.Ellipse(image.Rect(x-radius, (bar.Min.Y+bar.Max.Y)/2-radius, x+radius, (bar.Min.Y+bar.Max.Y)/2+radius)).Op(gtx.Ops))
			}
		} else {
			for y := bar.Max.Y - radius; y >= bar.Min.Y; y -= max(2, radius*3) {
				paint.FillShape(gtx.Ops, fill, clip.Ellipse(image.Rect((bar.Min.X+bar.Max.X)/2-radius, y-radius, (bar.Min.X+bar.Max.X)/2+radius, y+radius)).Op(gtx.Ops))
			}
		}
		return
	}
	radius := 0
	if shape == "rounded" {
		radius = min(4, min(bar.Dx(), bar.Dy())/3)
	}
	if shape == "capsule" {
		radius = min(bar.Dx(), bar.Dy()) / 2
	}
	if shape == "line" {
		if horizontal {
			bar.Max.Y = min(bar.Max.Y, bar.Min.Y+max(1, bar.Dy()/3))
		} else {
			bar.Max.X = min(bar.Max.X, bar.Min.X+max(1, bar.Dx()/3))
		}
	}
	paint.FillShape(gtx.Ops, fill, clip.UniformRRect(bar, radius).Op(gtx.Ops))
	if style == "outline" {
		paint.FillShape(gtx.Ops, color.NRGBA{R: 5, G: 21, B: 27, A: 180}, clip.UniformRRect(image.Rect(bar.Min.X+1, bar.Min.Y+1, max(bar.Min.X+1, bar.Max.X-1), max(bar.Min.Y+1, bar.Max.Y-1)), max(0, radius-1)).Op(gtx.Ops))
	}
}

type waveshareRingPreviewGeometry struct {
	outerRadius  int
	innerRadius  int
	columnLength int
	columnWidth  int
}

// waveshareNativeRingPreviewGeometry 先在设备像素空间复刻编译器的半径、柱长和
// 线宽约束，再缩放到编辑器画布。这样 8 种样式在缩放预览时仍与写入 .Theme
// 的原生 102 号进度控件保持相同几何语义。
func waveshareNativeRingPreviewGeometry(size image.Point, layer caseTheme.Layer, scale float64) waveshareRingPreviewGeometry {
	if scale <= 0 {
		scale = 1
	}
	physicalWidth := max(1, int(math.Round(float64(size.X)/scale)))
	physicalHeight := max(1, int(math.Round(float64(size.Y)/scale)))
	physicalOuter := max(12, min(physicalWidth, physicalHeight)/2-2)
	physicalLength := max(8, min(64, previewInt(layer, "bar_length", max(10, physicalOuter/3))))
	innerValue := float64(physicalOuter - physicalLength)
	if layer.Options != nil {
		if _, exists := layer.Options["inner_radius"]; exists {
			innerValue = previewFloat(layer, "inner_radius", innerValue)
		}
	}
	physicalInner := int(math.Round(innerValue))
	if innerValue > 0 && innerValue <= 1 {
		physicalInner = int(math.Round(float64(physicalOuter) * innerValue))
	}
	physicalInner = max(4, min(physicalOuter-4, physicalInner))
	physicalLength = max(4, physicalOuter-physicalInner)
	physicalWidthValue := max(1, min(16, previewInt(layer, "thickness", 1)))
	style := strings.ToLower(defaultOption(&layer, "style", "radial"))
	if style == "wave" {
		style = "waveform"
	}
	switch style {
	case "donut":
		physicalLength = max(4, min(physicalLength, physicalWidthValue*2))
		physicalWidthValue = max(physicalWidthValue, min(18, physicalLength))
	case "dots":
		physicalLength = physicalWidthValue
	case "dashed":
		physicalLength = max(4, min(physicalLength, physicalWidthValue*3))
	case "spokes":
		physicalLength = max(physicalLength, min(72, physicalOuter/2))
	case "pulse":
		physicalWidthValue = max(physicalWidthValue, min(20, physicalWidthValue*2))
	}
	outer := max(2, int(math.Round(float64(physicalOuter)*scale)))
	length := max(1, int(math.Round(float64(physicalLength)*scale)))
	width := max(1, int(math.Round(float64(physicalWidthValue)*scale)))
	inner := max(1, int(math.Round(float64(physicalInner)*scale)))
	return waveshareRingPreviewGeometry{outerRadius: outer, innerRadius: inner, columnLength: length, columnWidth: width}
}

func (model *gioEditor) drawWaveshareRingPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	if !model.isWaveshareTarget() {
		model.drawCompositeRingPreview(gtx, size, layer, fallback)
		return
	}
	model.drawNativeWaveshareRingPreview(gtx, size, layer, fallback)
}

func (model *gioEditor) drawNativeWaveshareRingPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 6, G: 18, B: 24, A: 220})
	center := image.Pt(size.X/2, size.Y/2)
	geometry := waveshareNativeRingPreviewGeometry(size, layer, model.previewScale(layer, size))
	count := caseTheme.WaveshareAudioRingCount(layer)
	values, peaks := model.audioPreviewFrame(layer, count)
	values = model.visibleAudioPreviewValues(values)
	style := strings.ToLower(defaultOption(&layer, "style", "radial"))
	if style == "wave" {
		style = "waveform"
	}
	direction := strings.ToLower(defaultOption(&layer, "direction", "bidirectional"))
	startAngle := previewFloat(layer, "start_angle", -90)
	start := previewColor(layer, "gradient_start", previewColor(layer, "color", fallback))
	end := previewColor(layer, "gradient_end", start)
	peakEnabled := caseTheme.OptionBool(layer.Options, "peak_hold", false)
	peakColor := previewColor(layer, "peak_color", color.NRGBA{R: 255, G: 242, B: 117, A: 150})
	peakColor.A = uint8(float64(peakColor.A) * .58)
	defaultGlow := style == "waveform" || style == "halo" || style == "pulse"
	glowEnabled := caseTheme.OptionBool(layer.Options, "glow", defaultGlow)
	glowRadius := max(0, int(math.Round(float64(max(0, min(16, previewInt(layer, "glow_radius", 6))))*model.previewScale(layer, size))))
	glowColor := previewColor(layer, "glow_color", start)
	// 连续波环在缩小预览中使用平头，避免 1–2 px 的相邻短柱叠成一圈珠子；
	// 圆点、光环或用户明确选择圆头时仍保留圆角语义。
	rounded := style == "donut" || style == "dots" || strings.EqualFold(defaultOption(&layer, "bar_shape", "square"), "rounded") ||
		strings.EqualFold(defaultOption(&layer, "bar_shape", "square"), "capsule")
	if style == "dashed" {
		rounded = false
	}

	paintBand := func(angle float64, startRadius, endRadius, width int, value, peak float64, bandStart, bandEnd color.NRGBA) {
		if value > 0 && glowEnabled && glowRadius > 0 {
			activeEnd := startRadius + int(math.Round(float64(endRadius-startRadius)*clamp01(value)))
			model.paintNativeRingGlow(gtx, center, angle, startRadius, activeEnd, width, glowRadius, glowColor)
		}
		if peakEnabled && peak > 0 {
			peakEnd := startRadius + int(math.Round(float64(endRadius-startRadius)*clamp01(peak)))
			model.paintNativeRingStroke(gtx, center, angle, startRadius, peakEnd, width, peakColor, rounded)
		}
		if value <= 0 {
			return
		}
		activeEnd := startRadius + int(math.Round(float64(endRadius-startRadius)*clamp01(value)))
		if style == "dashed" {
			bandEnd = bandStart
		}
		model.paintNativeRingGradient(gtx, center, angle, startRadius, activeEnd, width, bandStart, bandEnd, rounded)
	}

	for index, value := range values {
		if style == "dashed" && index%2 != 0 {
			continue
		}
		peak := 0.0
		if index < len(peaks) {
			peak = peaks[index]
		}
		angle := (startAngle + float64(index)*360/float64(max(1, count))) * math.Pi / 180
		switch direction {
		case "inward":
			paintBand(angle, geometry.outerRadius, geometry.outerRadius-geometry.columnLength, geometry.columnWidth, value, peak, start, end)
		case "bidirectional":
			middle := (geometry.innerRadius + geometry.outerRadius) / 2
			paintBand(angle, middle, geometry.outerRadius, geometry.columnWidth, value, peak, start, end)
			paintBand(angle, middle, geometry.innerRadius, geometry.columnWidth, value, peak, start, end)
		default:
			paintBand(angle, geometry.innerRadius, geometry.innerRadius+geometry.columnLength, geometry.columnWidth, value, peak, start, end)
		}
		if style == "halo" {
			haloRadius := max(2, geometry.innerRadius-geometry.columnWidth*2)
			haloWidth := max(1, geometry.columnWidth/2)
			haloLength := max(2, geometry.columnLength/2)
			paintBand(angle, haloRadius, haloRadius+haloLength, haloWidth, value, peak, end, start)
		}
	}
}

// paintNativeRingGlow 用两层低透明度轮廓近似屏端柔光。
// 单根宽线配圆头会在 48 段短柱的末端叠成一圈大色块，
// 视觉上比实体屏粗很多。柔光用平头，主频带仍保留用户选择的端点形状。
func (model *gioEditor) paintNativeRingGlow(gtx layout.Context, center image.Point, angle float64, startRadius, endRadius, width, radius int, fill color.NRGBA) {
	x0 := center.X + int(math.Round(math.Cos(angle)*float64(startRadius)))
	y0 := center.Y + int(math.Round(math.Sin(angle)*float64(startRadius)))
	x1 := center.X + int(math.Round(math.Cos(angle)*float64(endRadius)))
	y1 := center.Y + int(math.Round(math.Sin(angle)*float64(endRadius)))
	model.previewSpectrumGlowLine(gtx, x0, y0, x1, y1, width, radius, fill)
}

type spectrumPreviewGlowPass struct {
	width int
	alpha float64
}

func spectrumPreviewGlowPasses(width, radius int) []spectrumPreviewGlowPass {
	width = max(1, width)
	radius = max(0, radius)
	if radius == 0 {
		return nil
	}
	return []spectrumPreviewGlowPass{
		{width: width + radius*2, alpha: .045},
		{width: width + radius, alpha: .09},
	}
}

func (model *gioEditor) paintNativeRingStroke(gtx layout.Context, center image.Point, angle float64, startRadius, endRadius, width int, fill color.NRGBA, rounded bool) {
	x0 := center.X + int(math.Round(math.Cos(angle)*float64(startRadius)))
	y0 := center.Y + int(math.Round(math.Sin(angle)*float64(startRadius)))
	x1 := center.X + int(math.Round(math.Cos(angle)*float64(endRadius)))
	y1 := center.Y + int(math.Round(math.Sin(angle)*float64(endRadius)))
	model.previewSpectrumLine(gtx, x0, y0, x1, y1, width, fill, rounded)
}

func (model *gioEditor) paintNativeRingGradient(gtx layout.Context, center image.Point, angle float64, startRadius, endRadius, width int, start, end color.NRGBA, rounded bool) {
	length := abs(endRadius - startRadius)
	steps := max(1, min(8, length))
	for index := 0; index < steps; index++ {
		from := startRadius + int(math.Round(float64(endRadius-startRadius)*float64(index)/float64(steps)))
		to := startRadius + int(math.Round(float64(endRadius-startRadius)*float64(index+1)/float64(steps)))
		fill := interpolateColor(start, end, (float64(index)+.5)/float64(steps))
		// 渐变分段是编辑器的近似手段，不是多个屏端控件。
		// 内部接缝使用平头，否则每一段都会叠出粗大圆点。
		model.paintNativeRingStroke(gtx, center, angle, from, to, width, fill, false)
	}
	if rounded {
		model.paintNativeRingCap(gtx, center, angle, startRadius, width, start)
		model.paintNativeRingCap(gtx, center, angle, endRadius, width, end)
	}
}

func (model *gioEditor) paintNativeRingCap(gtx layout.Context, center image.Point, angle float64, radius, width int, fill color.NRGBA) {
	point := image.Pt(
		center.X+int(math.Round(math.Cos(angle)*float64(radius))),
		center.Y+int(math.Round(math.Sin(angle)*float64(radius))),
	)
	paint.FillShape(gtx.Ops, fill, clip.Ellipse(spectrumPreviewCapRect(point, width)).Op(gtx.Ops))
}

func (model *gioEditor) drawCompositeRingPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 6, G: 18, B: 24, A: 220})
	center := image.Pt(size.X/2, size.Y/2)
	outer := max(10, min(size.X, size.Y)/2-5)
	inner := max(4, int(float64(outer)*math.Max(.08, math.Min(.9, previewFloat(layer, "inner_radius", .52)))))
	previewScale := model.previewScale(layer, size)
	thickness := scaledSpectrumPreviewWidth(previewInt(layer, "thickness", 1), previewScale, 30)
	count := caseTheme.WaveshareAudioRingCount(layer)
	values, peaks := model.audioPreviewFrame(layer, count)
	values = model.visibleAudioPreviewValues(values)
	start := previewColor(layer, "gradient_start", previewColor(layer, "color", fallback))
	end := previewColor(layer, "gradient_end", start)
	peakEnabled := caseTheme.OptionBool(layer.Options, "peak_hold", false)
	peakColor := previewColor(layer, "peak_color", color.NRGBA{R: 255, G: 242, B: 117, A: 150})
	peakColor.A = uint8(float64(peakColor.A) * .58)
	style := strings.ToLower(defaultOption(&layer, "style", "radial"))
	direction := strings.ToLower(defaultOption(&layer, "direction", "bidirectional"))
	if style == "wave" || style == "waveform" {
		model.drawWaveshareRingWaveformPreview(gtx, center, inner, outer, layer, values, start, end, thickness, previewScale)
		return
	}
	glowEnabled := caseTheme.OptionBool(layer.Options, "glow", style == "halo" || style == "pulse" || style == "neon")
	glowRadius := scaledSpectrumPreviewWidthAllowZero(previewInt(layer, "glow_radius", 6), previewScale, 16)
	for index, value := range values {
		peakValue := 0.0
		if peakEnabled && index < len(peaks) {
			peakValue = clamp01(peaks[index])
		}
		if value <= 0 && peakValue <= 0 {
			continue
		}
		angle := -math.Pi/2 + 2*math.Pi*float64(index)/float64(count)
		if style == "dashed" && index%2 != 0 {
			continue
		}
		fill := interpolateColor(start, end, float64(index)/float64(max(1, count-1)))
		if value > 0 {
			length := max(2, int(float64(outer-inner)*value))
			startRadius, endRadius := inner, inner+length
			switch direction {
			case "inward":
				startRadius, endRadius = outer, outer-length
			case "bidirectional":
				middle := (outer + inner) / 2
				startRadius, endRadius = middle-length/2, middle+length/2
			}
			x0 := center.X + int(math.Round(math.Cos(angle)*float64(startRadius)))
			y0 := center.Y + int(math.Round(math.Sin(angle)*float64(startRadius)))
			x1 := center.X + int(math.Round(math.Cos(angle)*float64(endRadius)))
			y1 := center.Y + int(math.Round(math.Sin(angle)*float64(endRadius)))
			lineThickness := thickness
			if style == "pulse" {
				lineThickness = max(1, int(math.Round(float64(thickness)*(.45+value*.85))))
			}
			if style == "dots" {
				radius := max(1, lineThickness/2)
				soft := fill
				soft.A = uint8(float64(soft.A) * .16)
				paint.FillShape(gtx.Ops, soft, clip.Ellipse(image.Rect(x1-radius-1, y1-radius-1, x1+radius+2, y1+radius+2)).Op(gtx.Ops))
				paint.FillShape(gtx.Ops, fill, clip.Ellipse(image.Rect(x1-radius, y1-radius, x1+radius+1, y1+radius+1)).Op(gtx.Ops))
			} else {
				if glowEnabled && glowRadius > 0 {
					model.previewSpectrumGlowLine(gtx, x0, y0, x1, y1, lineThickness, glowRadius, fill)
				}
				model.previewSpectrumLine(gtx, x0, y0, x1, y1, lineThickness, fill, true)
				if style == "halo" {
					haloStart := max(2, inner-thickness*2)
					haloEnd := haloStart + int(math.Round(float64(length)*.35))
					hx0 := center.X + int(math.Round(math.Cos(angle)*float64(haloStart)))
					hy0 := center.Y + int(math.Round(math.Sin(angle)*float64(haloStart)))
					hx1 := center.X + int(math.Round(math.Cos(angle)*float64(haloEnd)))
					hy1 := center.Y + int(math.Round(math.Sin(angle)*float64(haloEnd)))
					haloColor := fill
					haloColor.A = uint8(float64(haloColor.A) * .62)
					model.previewSpectrumLine(gtx, hx0, hy0, hx1, hy1, max(1, lineThickness/2), haloColor, true)
				}
			}
		}
		if peakValue > 0 {
			peakLength := max(2, int(float64(outer-inner)*peakValue))
			peakRadii := []int{inner + peakLength}
			switch direction {
			case "inward":
				peakRadii = []int{outer - peakLength}
			case "bidirectional":
				middle := (outer + inner) / 2
				peakRadii = []int{middle - peakLength/2, middle + peakLength/2}
			}
			radius := max(1, thickness/2+1)
			for _, peakRadius := range peakRadii {
				peakX := center.X + int(math.Round(math.Cos(angle)*float64(peakRadius)))
				peakY := center.Y + int(math.Round(math.Sin(angle)*float64(peakRadius)))
				paint.FillShape(gtx.Ops, peakColor, clip.Ellipse(image.Rect(peakX-radius, peakY-radius, peakX+radius+1, peakY+radius+1)).Op(gtx.Ops))
			}
		}
	}
}

// drawWaveshareRingWaveformPreview 把频带端点连成闭合锯齿轮廓；双向模式
// 同时绘制内外两圈，形成参考图中的向内、向外发散效果。
func (model *gioEditor) drawWaveshareRingWaveformPreview(gtx layout.Context, center image.Point, inner, outer int, layer caseTheme.Layer, values []float64, start, end color.NRGBA, thickness int, previewScale float64) {
	values = model.visibleAudioPreviewValues(values)
	if len(values) < 3 {
		return
	}
	active := false
	for _, value := range values {
		if value > 0 {
			active = true
			break
		}
	}
	if !active {
		return
	}
	direction := strings.ToLower(defaultOption(&layer, "direction", "outward"))
	glowEnabled := caseTheme.OptionBool(layer.Options, "glow", true)
	glowRadius := scaledSpectrumPreviewWidthAllowZero(previewInt(layer, "glow_radius", 6), previewScale, 16)
	thickness = max(1, min(12, thickness))
	point := func(index int, value float64, innerPath bool) image.Point {
		length := float64(max(1, outer-inner)) * value
		radius := float64(inner) + length
		switch direction {
		case "inward":
			radius = float64(outer) - length
		case "bidirectional":
			middle := float64(inner+outer) / 2
			if innerPath {
				radius = middle - length/2
			} else {
				radius = middle + length/2
			}
		}
		angle := -math.Pi/2 + 2*math.Pi*float64(index%len(values))/float64(len(values))
		return image.Pt(center.X+int(math.Round(math.Cos(angle)*radius)), center.Y+int(math.Round(math.Sin(angle)*radius)))
	}
	drawPath := func(innerPath bool) {
		previous := point(0, values[0], innerPath)
		for index := 1; index <= len(values); index++ {
			current := point(index, values[index%len(values)], innerPath)
			fill := interpolateColor(start, end, float64(index%len(values))/float64(max(1, len(values)-1)))
			if glowEnabled && glowRadius > 0 {
				model.previewSpectrumGlowLine(gtx, previous.X, previous.Y, current.X, current.Y, thickness, glowRadius, fill)
			}
			model.previewSpectrumLine(gtx, previous.X, previous.Y, current.X, current.Y, thickness, fill, false)
			previous = current
		}
	}
	drawPath(false)
	if direction == "bidirectional" {
		drawPath(true)
	}
}

func scaledSpectrumPreviewWidth(value int, scale float64, maximum int) int {
	return max(1, min(maximum, int(math.Round(float64(max(1, value))*math.Max(0, scale)))))
}

func scaledSpectrumPreviewWidthAllowZero(value int, scale float64, maximum int) int {
	if value <= 0 || scale <= 0 {
		return 0
	}
	return max(1, min(maximum, int(math.Round(float64(value)*scale))))
}

func (model *gioEditor) previewSpectrumGlowLine(gtx layout.Context, x0, y0, x1, y1, width, radius int, fill color.NRGBA) {
	for _, pass := range spectrumPreviewGlowPasses(width, radius) {
		glow := fill
		glow.A = uint8(math.Round(float64(glow.A) * pass.alpha))
		if glow.A == 0 {
			continue
		}
		// clip.Stroke 自带半个线宽的端点延伸，即使不手动画圆头也会
		// 在相邻短线段上叠成大色块。柔光因此改用真正平头的四边形。
		model.previewSpectrumFlatLine(gtx, x0, y0, x1, y1, pass.width, glow)
	}
}

func (model *gioEditor) previewSpectrumFlatLine(gtx layout.Context, x0, y0, x1, y1, width int, fill color.NRGBA) {
	dx, dy := float64(x1-x0), float64(y1-y0)
	length := math.Hypot(dx, dy)
	if length <= 0 || fill.A == 0 {
		return
	}
	half := float64(max(1, width)) / 2
	nx, ny := -dy/length*half, dx/length*half
	var path clip.Path
	path.Begin(gtx.Ops)
	path.MoveTo(f32.Pt(float32(float64(x0)+nx), float32(float64(y0)+ny)))
	path.LineTo(f32.Pt(float32(float64(x1)+nx), float32(float64(y1)+ny)))
	path.LineTo(f32.Pt(float32(float64(x1)-nx), float32(float64(y1)-ny)))
	path.LineTo(f32.Pt(float32(float64(x0)-nx), float32(float64(y0)-ny)))
	path.Close()
	paint.FillShape(gtx.Ops, fill, clip.Outline{Path: path.End()}.Op())
}

// previewSpectrumLine 的圆头直径严格等于线宽。通用 previewLine
// 为了兼容其他控件会多补一像素，在细小环形频带上会显得端点过粗。
func (model *gioEditor) previewSpectrumLine(gtx layout.Context, x0, y0, x1, y1, width int, fill color.NRGBA, rounded bool) {
	width = max(1, width)
	var path clip.Path
	path.Begin(gtx.Ops)
	path.MoveTo(f32.Pt(float32(x0), float32(y0)))
	path.LineTo(f32.Pt(float32(x1), float32(y1)))
	paint.FillShape(gtx.Ops, fill, clip.Stroke{Path: path.End(), Width: float32(width)}.Op())
	if !rounded || width <= 1 {
		return
	}
	for _, point := range []image.Point{{X: x0, Y: y0}, {X: x1, Y: y1}} {
		cap := spectrumPreviewCapRect(point, width)
		paint.FillShape(gtx.Ops, fill, clip.Ellipse(cap).Op(gtx.Ops))
	}
}

func spectrumPreviewCapRect(point image.Point, width int) image.Rectangle {
	width = max(1, width)
	minimum := image.Pt(point.X-width/2, point.Y-width/2)
	return image.Rectangle{Min: minimum, Max: minimum.Add(image.Pt(width, width))}
}

func (model *gioEditor) alignedTextPoint(containerWidth, originX, y int, value string, fontSize unit.Sp, align string) image.Point {
	estimatedWidth := int(math.Round(float64(utf8.RuneCountInString(value)) * float64(fontSize) * .58))
	x := originX + 5
	switch strings.ToLower(align) {
	case "center":
		x = originX + (containerWidth-estimatedWidth)/2
	case "right":
		x = originX + containerWidth - estimatedWidth - 5
	}
	return image.Pt(max(originX, x), y)
}

func (model *gioEditor) drawTextComponentPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer) {
	model.previewPanel(gtx, size, layer, color.NRGBA{})
	textValue := previewTextComponentValue(layer, model.previewMoment())
	preferred := model.previewFontSize(layer, size, 32)
	foreground := previewColor(layer, "color", color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	shadowEnabled := caseTheme.OptionBool(layer.Options, "shadow", false)
	shadowReserve := 0
	if shadowEnabled {
		shadowReserve = 2
	}
	padding := max(1, min(5, min(size.X, size.Y)/10))
	content := image.Rect(padding, padding, max(padding+1, size.X-padding-shadowReserve), max(padding+1, size.Y-padding-shadowReserve))
	placement := waveshareFitPreviewTextLayout(textValue, preferred, content, defaultOption(&layer, "align", "center"), "center")
	if shadowEnabled {
		shadow := foreground
		shadow.R, shadow.G, shadow.B, shadow.A = 0, 0, 0, foreground.A/2
		model.previewStyledTextAt(gtx, placement.point.Add(image.Pt(2, 2)), textValue, placement.size, shadow, layer)
	}
	model.previewStyledTextAt(gtx, placement.point, textValue, placement.size, foreground, layer)
}

func previewTextComponentValue(layer caseTheme.Layer, now time.Time) string {
	textValue := defaultOption(&layer, "text", "自定义文本")
	if dataKey := layerOptionString(&layer, "data_key"); dataKey != "" {
		// 动态文字在设备上会被 system_data_name 的实时值替换；预览也显示
		// 同一个示例值，避免绑定数据后画布仍停留在“自定义文本”。
		textValue, _ = sampleMetricValueAt(dataKey, now)
	}
	return textValue
}

func formatClockPreview(format string, now time.Time) string {
	replacer := strings.NewReplacer(
		"yyyy", "2006", "MM", "01", "dd", "02", "HH", "15", "mm", "04", "ss", "05",
	)
	if strings.Contains(format, "week") {
		week := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[now.Weekday()]
		format = strings.ReplaceAll(format, "week", week)
	}
	return now.Format(replacer.Replace(format))
}

func (model *gioEditor) drawClockPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer) {
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	value := formatClockPreview(defaultOption(&layer, "format", "HH:mm:ss"), model.previewMoment())
	preferred := model.previewFontSize(layer, size, 44)
	foreground := previewColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	padding := max(1, min(5, min(size.X, size.Y)/10))
	content := image.Rect(padding, padding, max(padding+1, size.X-padding), max(padding+1, size.Y-padding))
	placement := waveshareFitClockPreviewTextLayout(value, preferred, content, defaultOption(&layer, "align", "center"), "center")
	model.previewStyledTextAt(gtx, placement.point, value, placement.size, foreground, layer)
}

func (model *gioEditor) drawProgressBarPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer) {
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	key := defaultOption(&layer, "data_key", "Cpu_Freq")
	valueText, ratio := sampleMetricValueAt(key, model.previewMoment())
	ratio = previewMetricRangeRatio(layer, key, ratio)
	trackColor := previewColor(layer, "secondary_color", color.NRGBA{R: 24, G: 55, B: 64, A: 255})
	fillColor := previewColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	showLabel := caseTheme.OptionBool(layer.Options, "show_label", true)
	showValue := caseTheme.OptionBool(layer.Options, "show_value", true)
	orientation := strings.ToLower(defaultOption(&layer, "orientation", "horizontal"))
	style := strings.ToLower(defaultOption(&layer, "style", "rounded"))
	margin := max(4, min(size.X, size.Y)/10)
	bar := image.Rect(margin, size.Y/2, size.X-margin, max(size.Y/2+4, size.Y-margin))
	if orientation == "vertical" {
		bar = image.Rect(size.X/3, margin, max(size.X/3+5, size.X*2/3), size.Y-margin)
	}
	radius := max(0, int(caseTheme.OptionFloat(layer.Options, "corner_radius", 8)*model.previewScale(layer, size)))
	radius = min(radius, min(bar.Dx(), bar.Dy())/2)
	if style == "square" || style == "segmented" {
		radius = 0
	}
	trackFill := trackColor
	if style == "outline" {
		trackFill = previewColor(layer, "panel_color", color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	}
	paint.FillShape(gtx.Ops, trackFill, clip.UniformRRect(bar, radius).Op(gtx.Ops))
	fill := bar
	if orientation == "vertical" {
		fill.Min.Y = fill.Max.Y - int(float64(fill.Dy())*ratio)
	} else if orientation == "reverse" {
		fill.Min.X = fill.Max.X - int(float64(fill.Dx())*ratio)
	} else {
		fill.Max.X = fill.Min.X + int(float64(fill.Dx())*ratio)
	}
	segments := max(1, min(16, previewInt(layer, "segments", 10)))
	if style == "segmented" {
		for index := 0; index < segments; index++ {
			segment := image.Rect(bar.Min.X+index*bar.Dx()/segments, bar.Min.Y, bar.Min.X+(index+1)*bar.Dx()/segments-2, bar.Max.Y)
			if orientation == "vertical" {
				segment = image.Rect(bar.Min.X, bar.Min.Y+index*bar.Dy()/segments, bar.Max.X, bar.Min.Y+(index+1)*bar.Dy()/segments-2)
			}
			active := segment.Intersect(fill)
			if !active.Empty() {
				paint.FillShape(gtx.Ops, fillColor, clip.UniformRRect(active, 2).Op(gtx.Ops))
			}
		}
	} else if style == "dots" {
		for index := 0; index < segments; index++ {
			x := bar.Min.X + (index*2+1)*bar.Dx()/(segments*2)
			y := (bar.Min.Y + bar.Max.Y) / 2
			radius := max(1, bar.Dy()/3)
			active := x >= fill.Min.X && x < fill.Max.X
			if orientation == "vertical" {
				x = (bar.Min.X + bar.Max.X) / 2
				y = bar.Min.Y + (index*2+1)*bar.Dy()/(segments*2)
				radius = max(1, bar.Dx()/3)
				active = y >= fill.Min.Y && y < fill.Max.Y
			}
			if active {
				paint.FillShape(gtx.Ops, fillColor, clip.Ellipse(image.Rect(x-radius, y-radius, x+radius, y+radius)).Op(gtx.Ops))
			}
		}
	} else {
		paint.FillShape(gtx.Ops, fillColor, clip.UniformRRect(fill, radius).Op(gtx.Ops))
	}
	if style == "neon" && !fill.Empty() {
		highlight := fill
		highlight.Max.Y = highlight.Min.Y + max(1, highlight.Dy()/2)
		paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 255, B: 255, A: 72}, clip.UniformRRect(highlight, radius).Op(gtx.Ops))
	}
	if style == "neon" || style == "outline" {
		borderWidth := 1
		if style == "outline" {
			borderWidth = 2
		}
		paintPreviewRectBorder(gtx, bar, borderWidth, fillColor)
	}
	preferred := unit.Sp(maxFloat(7, float64(min(size.X, size.Y))*.15))
	textTop := max(1, margin/2)
	textBottom := max(textTop+1, bar.Min.Y-max(1, margin/2))
	if orientation == "vertical" || textBottom > size.Y-1 {
		textBottom = min(size.Y-1, max(textTop+1, size.Y/3))
	}
	textBounds := image.Rect(max(1, margin/2), textTop, max(2, size.X-max(1, margin/2)), textBottom)
	labelText := metricLabel(key)
	if showLabel && showValue {
		gap := max(1, min(4, textBounds.Dx()/16))
		middle := textBounds.Min.X + max(1, (textBounds.Dx()-gap)/2)
		labelBounds := image.Rect(textBounds.Min.X, textBounds.Min.Y, middle, textBounds.Max.Y)
		valueBounds := image.Rect(min(textBounds.Max.X-1, middle+gap), textBounds.Min.Y, textBounds.Max.X, textBounds.Max.Y)
		labelPlacement := waveshareFitPreviewTextLayout(labelText, preferred, labelBounds, "left", "center")
		valuePlacement := waveshareFitPreviewTextLayout(valueText, preferred, valueBounds, "right", "center")
		model.previewStyledTextAt(gtx, labelPlacement.point, labelText, labelPlacement.size, color.NRGBA{R: 185, G: 210, B: 216, A: 255}, layer)
		model.previewStyledTextAt(gtx, valuePlacement.point, valueText, valuePlacement.size, fillColor, layer)
	} else if showLabel {
		placement := waveshareFitPreviewTextLayout(labelText, preferred, textBounds, "left", "center")
		model.previewStyledTextAt(gtx, placement.point, labelText, placement.size, color.NRGBA{R: 185, G: 210, B: 216, A: 255}, layer)
	} else if showValue {
		placement := waveshareFitPreviewTextLayout(valueText, preferred, textBounds, "right", "center")
		model.previewStyledTextAt(gtx, placement.point, valueText, placement.size, fillColor, layer)
	}
}

// paintPreviewRectBorder 用四条窄矩形模拟固件进度控件的描边字段。
func paintPreviewRectBorder(gtx layout.Context, rectangle image.Rectangle, width int, fill color.NRGBA) {
	width = max(1, min(width, min(rectangle.Dx(), rectangle.Dy())/2))
	for _, edge := range []image.Rectangle{
		image.Rect(rectangle.Min.X, rectangle.Min.Y, rectangle.Max.X, rectangle.Min.Y+width),
		image.Rect(rectangle.Min.X, rectangle.Max.Y-width, rectangle.Max.X, rectangle.Max.Y),
		image.Rect(rectangle.Min.X, rectangle.Min.Y+width, rectangle.Min.X+width, rectangle.Max.Y-width),
		image.Rect(rectangle.Max.X-width, rectangle.Min.Y+width, rectangle.Max.X, rectangle.Max.Y-width),
	} {
		if !edge.Empty() {
			paint.FillShape(gtx.Ops, fill, clip.Rect(edge).Op())
		}
	}
}

func (model *gioEditor) drawPreviewArc(gtx layout.Context, center image.Point, radius int, startDegrees, endDegrees float64, width int, fill color.NRGBA, progress float64) {
	progress = clamp01(progress)
	endDegrees = startDegrees + (endDegrees-startDegrees)*progress
	steps := max(8, int(math.Abs(endDegrees-startDegrees)/3))
	lastX, lastY := 0, 0
	for index := 0; index <= steps; index++ {
		ratio := float64(index) / float64(max(1, steps))
		angle := (startDegrees + (endDegrees-startDegrees)*ratio - 90) * math.Pi / 180
		x := center.X + int(math.Round(math.Cos(angle)*float64(radius)))
		y := center.Y + int(math.Round(math.Sin(angle)*float64(radius)))
		if index > 0 {
			model.previewLine(gtx, lastX, lastY, x, y, width, fill)
		}
		lastX, lastY = x, y
	}
}

func (model *gioEditor) drawGaugePreview(gtx layout.Context, size image.Point, layer caseTheme.Layer) {
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	key := defaultOption(&layer, "data_key", "Cpu_Freq")
	valueText, ratio := sampleMetricValueAt(key, model.previewMoment())
	ratio = previewMetricRangeRatio(layer, key, ratio)
	center := image.Pt(size.X/2, size.Y/2)
	radius := max(8, min(size.X, size.Y)/2-max(5, min(size.X, size.Y)/12))
	stroke := max(1, int(caseTheme.OptionFloat(layer.Options, "stroke_width", 8)*model.previewScale(layer, size)))
	startAngle := caseTheme.OptionFloat(layer.Options, "start_angle", -140)
	endAngle := caseTheme.OptionFloat(layer.Options, "end_angle", 140)
	style := strings.ToLower(defaultOption(&layer, "style", "arc"))
	if layer.Type == caseTheme.LayerGradientGauge {
		switch style {
		case "semicircle":
			startAngle, endAngle = 180, 360
		case "ring":
			startAngle, endAngle = -180, 180
		case "minimal":
			stroke = max(1, stroke/2)
		}
	} else {
		switch style {
		case "semicircle":
			startAngle, endAngle = 180, 360
		case "ring", "minimal":
			startAngle, endAngle = -180, 180
			if style == "minimal" {
				stroke = max(1, stroke/2)
			}
		}
	}
	track := previewColor(layer, "secondary_color", color.NRGBA{R: 24, G: 55, B: 64, A: 255})
	model.drawPreviewArc(gtx, center, radius, startAngle, endAngle, stroke, track, 1)
	if layer.Type == caseTheme.LayerGradientGauge {
		startColor := previewColor(layer, "gradient_start", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
		middleColor := previewColor(layer, "gradient_mid", color.NRGBA{R: 255, G: 242, B: 117, A: 255})
		endColor := previewColor(layer, "gradient_end", color.NRGBA{R: 255, G: 92, B: 104, A: 255})
		segments := 32
		for index := 0; index < int(float64(segments)*ratio); index++ {
			segmentStart := startAngle + (endAngle-startAngle)*float64(index)/float64(segments)
			segmentEnd := startAngle + (endAngle-startAngle)*float64(index+1)/float64(segments)
			position := float64(index) / float64(segments-1)
			fill := interpolateColor(startColor, middleColor, position*2)
			if position > .5 {
				fill = interpolateColor(middleColor, endColor, (position-.5)*2)
			}
			model.drawPreviewArc(gtx, center, radius, segmentStart, segmentEnd, stroke, fill, 1)
		}
	} else {
		foreground := previewColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
		model.drawPreviewArc(gtx, center, radius, startAngle, endAngle, stroke, foreground, ratio)
	}
	if style == "dial" || style == "ticks" {
		markCount := 10
		if model.isWaveshareTarget() {
			if style == "dial" {
				markCount = 18
			} else {
				markCount = 7
			}
		}
		for index := 0; index <= markCount; index++ {
			angle := (startAngle + (endAngle-startAngle)*float64(index)/float64(markCount) - 90) * math.Pi / 180
			x0 := center.X + int(math.Cos(angle)*float64(radius-stroke*2))
			y0 := center.Y + int(math.Sin(angle)*float64(radius-stroke*2))
			x1 := center.X + int(math.Cos(angle)*float64(radius+stroke/2))
			y1 := center.Y + int(math.Sin(angle)*float64(radius+stroke/2))
			model.previewLine(gtx, x0, y0, x1, y1, max(1, stroke/3), track)
		}
	}
	if style == "dial" && !model.isWaveshareTarget() {
		angle := (startAngle + (endAngle-startAngle)*ratio - 90) * math.Pi / 180
		x := center.X + int(math.Cos(angle)*float64(radius-stroke*2))
		y := center.Y + int(math.Sin(angle)*float64(radius-stroke*2))
		model.previewLine(gtx, center.X, center.Y, x, y, max(2, stroke/3), previewColor(layer, "color", color.NRGBA{R: 255, G: 255, B: 255, A: 255}))
	}
	preferred := model.previewFontSize(layer, size, 22)
	textColor := previewColor(layer, "color", color.NRGBA{R: 245, G: 251, B: 255, A: 255})
	if layer.Type == caseTheme.LayerGradientGauge {
		textColor = previewColor(layer, "color", previewColor(layer, "gradient_start", color.NRGBA{R: 76, G: 224, B: 255, A: 255}))
	}
	showValue := caseTheme.OptionBool(layer.Options, "show_value", true)
	showLabel := caseTheme.OptionBool(layer.Options, "show_label", true)
	padding := max(1, min(5, min(size.X, size.Y)/12))
	content := image.Rect(padding, padding, max(padding+1, size.X-padding), max(padding+1, size.Y-padding))
	if showValue && showLabel {
		labelTop := max(content.Min.Y+1, min(content.Max.Y-1, size.Y*2/3))
		valueBounds := image.Rect(content.Min.X, max(content.Min.Y, size.Y/3), content.Max.X, labelTop)
		labelBounds := image.Rect(content.Min.X, labelTop, content.Max.X, content.Max.Y)
		valuePlacement := waveshareFitPreviewTextLayout(valueText, preferred, valueBounds, "center", "center")
		labelText := metricLabel(key)
		labelPlacement := waveshareFitPreviewTextLayout(labelText, unit.Sp(maxFloat(3, float64(preferred)*.48)), labelBounds, "center", "center")
		model.previewStyledTextAt(gtx, valuePlacement.point, valueText, valuePlacement.size, textColor, layer)
		model.previewStyledTextAt(gtx, labelPlacement.point, labelText, labelPlacement.size, textColor, layer)
	} else if showValue {
		placement := waveshareFitPreviewTextLayout(valueText, preferred, content, "center", "center")
		model.previewStyledTextAt(gtx, placement.point, valueText, placement.size, textColor, layer)
	} else if showLabel {
		labelText := metricLabel(key)
		placement := waveshareFitPreviewTextLayout(labelText, unit.Sp(maxFloat(3, float64(preferred)*.48)), content, "center", "center")
		model.previewStyledTextAt(gtx, placement.point, labelText, placement.size, textColor, layer)
	}
}

func lineChartPreviewStyle(style string, waveshare bool) (renderStyle string, forceNoGrid, fixedGeometry bool) {
	style = strings.ToLower(strings.TrimSpace(style))
	if style == "" {
		style = "area"
	}
	if !waveshare {
		return style, false, false
	}
	switch style {
	case "step", "spline", "dots":
		return "line", style == "spline" || style == "dots", true
	case "neon":
		return "area", true, true
	default:
		return style, false, true
	}
}

func previewCatmullRomPoint(first, second, third, fourth image.Point, ratio float64) image.Point {
	squared, cubed := ratio*ratio, ratio*ratio*ratio
	coordinate := func(p0, p1, p2, p3 int) int {
		value := .5 * (2*float64(p1) + (-float64(p0)+float64(p2))*ratio +
			(2*float64(p0)-5*float64(p1)+4*float64(p2)-float64(p3))*squared +
			(-float64(p0)+3*float64(p1)-3*float64(p2)+float64(p3))*cubed)
		return int(math.Round(value))
	}
	return image.Pt(coordinate(first.X, second.X, third.X, fourth.X), coordinate(first.Y, second.Y, third.Y, fourth.Y))
}

func (model *gioEditor) drawLineChartPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer) {
	model.previewPanel(gtx, size, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	requestedStyle := strings.ToLower(defaultOption(&layer, "style", "area"))
	renderStyle, forceNoGrid, fixedGeometry := lineChartPreviewStyle(requestedStyle, model.isWaveshareTarget())
	margin := max(5, min(size.X, size.Y)/12)
	chart := image.Rect(margin, margin, size.X-margin, size.Y-margin)
	if chart.Empty() {
		return
	}
	showGrid := caseTheme.OptionBool(layer.Options, "show_grid", true) && !forceNoGrid
	if showGrid {
		grid := previewColor(layer, "grid_color", color.NRGBA{R: 82, G: 114, B: 126, A: 100})
		for index := 1; index < 4; index++ {
			y := chart.Min.Y + index*chart.Dy()/4
			model.previewLine(gtx, chart.Min.X, y, chart.Max.X, y, 1, grid)
		}
		for index := 1; index < 6; index++ {
			x := chart.Min.X + index*chart.Dx()/6
			model.previewLine(gtx, x, chart.Min.Y, x, chart.Max.Y, 1, grid)
		}
	}
	points := max(8, min(96, previewInt(layer, "history_points", 48)))
	stroke := max(1, int(caseTheme.OptionFloat(layer.Options, "stroke_width", 3)*model.previewScale(layer, size)))
	if fixedGeometry {
		points = 48
		stroke = max(1, int(math.Round(2*model.previewScale(layer, size))))
	}
	lineColor := previewColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	fillColor := previewColor(layer, "fill_color", color.NRGBA{R: 76, G: 224, B: 255, A: 56})
	phase := float64(model.previewMoment().UnixNano()%int64(6*time.Second)) / float64(6*time.Second)
	samples := make([]image.Point, 0, points)
	for index := 0; index < points; index++ {
		ratio := float64(index) / float64(points-1)
		rawValue := 48 + 24*math.Sin(ratio*math.Pi*4+phase*math.Pi*2) + 12*math.Sin(ratio*math.Pi*13)
		value := previewRangeRatio(layer, rawValue)
		x := chart.Min.X + int(ratio*float64(chart.Dx()))
		y := chart.Max.Y - int(clamp01(value)*float64(chart.Dy()))
		samples = append(samples, image.Pt(x, y))
		if renderStyle == "area" || renderStyle == "neon" {
			paint.FillShape(gtx.Ops, fillColor, clip.Rect{Min: image.Pt(x, y), Max: image.Pt(min(chart.Max.X, x+max(1, chart.Dx()/points+1)), chart.Max.Y)}.Op())
		}
	}
	switch renderStyle {
	case "dots":
		for _, point := range samples {
			paint.FillShape(gtx.Ops, lineColor, clip.Ellipse(image.Rect(point.X-stroke, point.Y-stroke, point.X+stroke+1, point.Y+stroke+1)).Op(gtx.Ops))
		}
	case "step":
		for index := 1; index < len(samples); index++ {
			previous, current := samples[index-1], samples[index]
			model.previewLine(gtx, previous.X, previous.Y, current.X, previous.Y, stroke, lineColor)
			model.previewLine(gtx, current.X, previous.Y, current.X, current.Y, stroke, lineColor)
		}
	case "spline":
		for index := 0; index+1 < len(samples); index++ {
			first := samples[max(0, index-1)]
			second, third := samples[index], samples[index+1]
			fourth := samples[min(len(samples)-1, index+2)]
			previous := second
			for subdivision := 1; subdivision <= 4; subdivision++ {
				current := previewCatmullRomPoint(first, second, third, fourth, float64(subdivision)/4)
				current.X = max(chart.Min.X, min(chart.Max.X, current.X))
				current.Y = max(chart.Min.Y, min(chart.Max.Y, current.Y))
				model.previewLine(gtx, previous.X, previous.Y, current.X, current.Y, stroke, lineColor)
				previous = current
			}
		}
	default:
		for index := 1; index < len(samples); index++ {
			previous, current := samples[index-1], samples[index]
			if renderStyle == "neon" {
				glow := lineColor
				glow.A /= 3
				model.previewLine(gtx, previous.X, previous.Y, current.X, current.Y, stroke+3, glow)
			}
			model.previewLine(gtx, previous.X, previous.Y, current.X, current.Y, stroke, lineColor)
		}
	}
	if caseTheme.OptionBool(layer.Options, "show_label", true) {
		key := defaultOption(&layer, "data_key", "Cpu_Freq")
		value, _ := sampleMetricValueAt(key, model.previewMoment())
		labelValue := metricLabel(key) + "  " + value
		preferred := unit.Sp(max(7, min(size.X, size.Y)/10))
		labelHeight := max(2, min(chart.Dy(), max(4, chart.Dy()/4)))
		labelBounds := image.Rect(min(chart.Max.X-1, chart.Min.X+2), min(chart.Max.Y-1, chart.Min.Y+1), max(chart.Min.X+3, chart.Max.X-2), min(chart.Max.Y, chart.Min.Y+1+labelHeight))
		placement := waveshareFitPreviewTextLayout(labelValue, preferred, labelBounds, "left", "top")
		model.previewStyledTextAt(gtx, placement.point, labelValue, placement.size, lineColor, layer)
	}
}

func (model *gioEditor) drawShapePreview(gtx layout.Context, size image.Point, layer caseTheme.Layer) {
	fill := previewColor(layer, "fill_color", color.NRGBA{R: 11, G: 32, B: 41, A: 204})
	stroke := previewColor(layer, "stroke_color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	strokeWidth := max(0, int(caseTheme.OptionFloat(layer.Options, "stroke_width", 2)*model.previewScale(layer, size)))
	shape := strings.ToLower(defaultOption(&layer, "shape", "rounded_rect"))
	rect := image.Rectangle{Max: size}
	if shape == "line" {
		model.previewLine(gtx, 2, size.Y/2, max(2, size.X-2), size.Y/2, max(1, strokeWidth), stroke)
		return
	}
	if shape == "circle" {
		diameter := min(size.X, size.Y)
		rect = image.Rect((size.X-diameter)/2, (size.Y-diameter)/2, (size.X+diameter)/2, (size.Y+diameter)/2)
	}
	if shape == "circle" || shape == "ellipse" {
		paint.FillShape(gtx.Ops, stroke, clip.Ellipse(rect).Op(gtx.Ops))
		inset := min(strokeWidth, min(rect.Dx(), rect.Dy())/2)
		inside := image.Rect(rect.Min.X+inset, rect.Min.Y+inset, rect.Max.X-inset, rect.Max.Y-inset)
		if !inside.Empty() {
			paint.FillShape(gtx.Ops, fill, clip.Ellipse(inside).Op(gtx.Ops))
		}
		return
	}
	radius := 0
	if shape == "rounded_rect" {
		radius = int(caseTheme.OptionFloat(layer.Options, "corner_radius", 14) * model.previewScale(layer, size))
	}
	if shape == "pill" {
		radius = min(size.X, size.Y) / 2
	}
	radius = max(0, min(radius, min(size.X, size.Y)/2))
	paint.FillShape(gtx.Ops, stroke, clip.UniformRRect(rect, radius).Op(gtx.Ops))
	inset := min(strokeWidth, min(size.X, size.Y)/2)
	inside := image.Rect(inset, inset, size.X-inset, size.Y-inset)
	if !inside.Empty() {
		paint.FillShape(gtx.Ops, fill, clip.UniformRRect(inside, max(0, radius-inset)).Op(gtx.Ops))
	}
}

func sampleMetricValue(key string) (string, float64) {
	return sampleMetricValueAt(key, time.Now())
}

// previewMetricRangeRatio 将固定演示值放入用户配置的最小值/最大值范围中，
// 使进度条和仪表的范围属性在画布上可见，而不是永远沿用 0..100 的比例。
func previewMetricRangeRatio(layer caseTheme.Layer, key string, fallbackRatio float64) float64 {
	raw := fallbackRatio * 100
	switch key {
	case "Memory_Used":
		raw = 18.6
	case "Memory_Available":
		raw = 13.4
	case "GPU_Temp":
		raw = 45
	case "GPU_Hotspot_Temp":
		raw = 54
	case "GPU_Memory_Temp":
		raw = 56
	case "cur_dB":
		raw = -18
	case "avg_dB":
		raw = -24
	case "max_dB":
		raw = -8
	}
	return previewRangeRatio(layer, raw)
}

func previewRangeRatio(layer caseTheme.Layer, raw float64) float64 {
	minimum := caseTheme.OptionFloat(layer.Options, "min_value", 0)
	maximum := caseTheme.OptionFloat(layer.Options, "max_value", 100)
	if math.IsNaN(minimum) || math.IsNaN(maximum) || math.IsInf(minimum, 0) || math.IsInf(maximum, 0) || maximum <= minimum {
		return 0
	}
	return clamp01((raw - minimum) / (maximum - minimum))
}

func sampleMetricValueAt(key string, now time.Time) (string, float64) {
	switch key {
	case "Gpu_Load":
		return "63%", .63
	case "Memory_Load":
		return "58%", .58
	case "GPU_Temp":
		return "45℃", .45
	case "GPU_Hotspot_Temp":
		return "54℃", .54
	case "GPU_Memory_Temp":
		return "56℃", .56
	case "Memory_Used":
		return "18.6GB", .58
	case "Memory_Available":
		return "13.4GB", .42
	case "time":
		return now.Format("15:04:05"), .72
	case "date":
		return now.Format("2006-01-02"), .72
	case "week":
		return "周三", .50
	case "Cpu_Model":
		return "AMD Ryzen", .72
	case "Gpu_Model":
		return "GeForce RTX", .72
	case "vol":
		return "72%", .72
	case "cur_dB":
		return "-18dB", .62
	case "avg_dB":
		return "-24dB", .52
	case "max_dB":
		return "-8dB", .82
	default:
		return "42%", .42
	}
}

func metricLabel(key string) string {
	for _, choice := range waveshareMetricChoices {
		if choice.value == key {
			return choice.label
		}
	}
	return key
}

func interpolateColor(start, end color.NRGBA, ratio float64) color.NRGBA {
	ratio = clamp01(ratio)
	value := func(a, b uint8) uint8 { return uint8(math.Round(float64(a) + (float64(b)-float64(a))*ratio)) }
	return color.NRGBA{R: value(start.R, end.R), G: value(start.G, end.G), B: value(start.B, end.B), A: value(start.A, end.A)}
}

func (model *gioEditor) previewStyledTextAt(gtx layout.Context, point image.Point, value string, size unit.Sp, foreground color.NRGBA, layer caseTheme.Layer) {
	for _, pass := range previewFontPasses(layerOptionString(&layer, "font_weight")) {
		paint := foreground
		paint.A = uint8(math.Round(float64(paint.A) * pass.alpha))
		model.previewFontLabelAt(gtx, point.Add(pass.offset), value, size, paint, layer)
	}
}

type previewFontPass struct {
	offset image.Point
	alpha  float64
}

// previewFontPasses 让四档字重即使在系统只有单字重时也保持可见差异。
// light 降低主笔画覆盖率，medium/bold 使用有界 1px 膨胀；不会改变字号与排版。
func previewFontPasses(weight string) []previewFontPass {
	switch normalizedPreviewFontWeight(weight) {
	case "light":
		return []previewFontPass{{alpha: .68}}
	case "medium":
		return []previewFontPass{{offset: image.Pt(1, 0), alpha: .42}, {alpha: 1}}
	case "bold":
		return []previewFontPass{{offset: image.Pt(1, 0), alpha: .78}, {offset: image.Pt(0, 1), alpha: .46}, {alpha: 1}}
	default:
		return []previewFontPass{{alpha: 1}}
	}
}

func normalizedPreviewFontWeight(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "light", "thin", "100", "200", "300":
		return "light"
	case "regular", "normal", "400":
		return "regular"
	case "bold", "700", "800", "900":
		return "bold"
	default:
		return "medium"
	}
}

// previewFontLabelAt 负责单次字体绘制；previewStyledTextAt 在它之上模拟设备端
// 的中粗/粗体膨胀，因此即使系统只安装了某个字体族的一张字脸，字重按钮也不会
// 看起来完全一样。
func (model *gioEditor) previewFontLabelAt(gtx layout.Context, point image.Point, value string, size unit.Sp, foreground color.NRGBA, layer caseTheme.Layer) {
	stack := opOffset(point, gtx)
	defer stack()
	label := material.Label(model.theme, size, value)
	label.Color = foreground
	label.MaxLines = 1
	label.Font.Typeface = previewTypeface(layerOptionString(&layer, "font_family"))
	switch normalizedPreviewFontWeight(layerOptionString(&layer, "font_weight")) {
	case "bold":
		label.Font.Weight = font.Bold
	case "medium":
		label.Font.Weight = font.Medium
	case "light":
		label.Font.Weight = font.Light
	}
	if strings.EqualFold(layerOptionString(&layer, "font_style"), "italic") {
		label.Font.Style = font.Italic
	}
	label.Layout(gtx)
}

// previewTypeface 使用 Gio 字体匹配器可以识别的“单一真实字体族名”。旧值采用
// CSS 逗号回退列表，Gio 会把整串当成一个不存在的字体名，因而所有按钮看起来相同。
// 字体缺失时系统匹配器仍会按字形回退，不影响中文显示。
func previewTypeface(family string) font.Typeface {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case "pangmen":
		return workbenchTypefacePangMen
	case "source-han":
		return workbenchTypefaceSourceHan
	case "microsoft-yahei":
		return workbenchTypefaceMicrosoftYaHei
	case "simhei":
		return workbenchTypefaceSimHei
	case "digital":
		return workbenchTypefaceDigital
	case "monospace":
		return workbenchTypefaceMonospace
	case "rounded":
		return workbenchTypefaceRounded
	case "serif":
		return workbenchTypefaceSerif
	default:
		return ""
	}
}

// opOffset 将局部偏移封装成可延迟恢复的函数，避免各预览组件遗漏 Pop。
func opOffset(point image.Point, gtx layout.Context) func() {
	stack := op.Offset(point).Push(gtx.Ops)
	return stack.Pop
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
