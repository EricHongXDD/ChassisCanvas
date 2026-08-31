package workbench

import (
	"math"
	"reflect"

	caseTheme "casescreen/internal/theme"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

type waveshareVisualPreset struct {
	id      string
	label   string
	options map[string]any
}

type universalVisualPresetSection struct {
	layerType caseTheme.LayerType
	label     string
}

var universalVisualPresetSections = []universalVisualPresetSection{
	{layerType: caseTheme.LayerTelemetry, label: "遥测"},
	{layerType: caseTheme.LayerAudioBars, label: "条状频谱"},
	{layerType: caseTheme.LayerAudioRing, label: "环状频谱"},
	{layerType: caseTheme.LayerLyrics, label: "歌词"},
}

// waveshareVisualOptionKeys 是预设能够改写的严格白名单。
// 它只包含微雪编译器真实读取的外观字段，刻意排除数据绑定、图层几何和可见性。
var waveshareVisualOptionKeys = map[caseTheme.LayerType]map[string]bool{
	caseTheme.LayerTelemetry: {
		"style": true, "font_family": true, "font_weight": true,
		"label_color": true, "value_color": true, "accent_color": true, "panel_color": true,
		"card_color": true, "card_shape": true,
	},
	caseTheme.LayerAudioBars: {
		"style": true, "bar_shape": true, "direction": true, "count": true, "gap": true, "thickness": true,
		"glow": true, "glow_radius": true,
		"color": true, "gradient_start": true, "gradient_end": true, "panel_color": true, "peak_color": true,
	},
	caseTheme.LayerAudioRing: {
		"style": true, "direction": true, "count": true, "inner_radius": true, "thickness": true,
		"glow": true, "glow_radius": true,
		"color": true, "gradient_start": true, "gradient_end": true, "panel_color": true, "peak_color": true,
	},
	caseTheme.LayerLyrics: {
		"style": true, "font_family": true, "font_weight": true, "align": true,
		"current_color": true, "next_color": true, "accent_color": true,
		"panel_color": true, "panel_border_color": true,
		"outline_color": true, "outline_width": true,
		"shadow": true, "shadow_color": true, "glow_color": true, "glow_radius": true,
		"karaoke_highlight": true, "smooth_transition": true, "show_progress": true,
	},
	caseTheme.LayerText: {
		"font_family": true, "font_weight": true, "color": true, "panel_color": true,
	},
	caseTheme.LayerClock: {
		"font_family": true, "font_weight": true, "color": true, "panel_color": true,
	},
	caseTheme.LayerProgressBar: {
		"style": true, "color": true, "secondary_color": true,
		"panel_color": true, "font_family": true, "font_weight": true,
	},
	caseTheme.LayerGauge: {
		"style": true, "font_family": true, "font_weight": true, "color": true,
		"secondary_color": true, "panel_color": true,
	},
	caseTheme.LayerGradientGauge: {
		"style": true, "font_family": true, "font_weight": true,
		"gradient_start": true, "gradient_mid": true, "gradient_end": true,
		"panel_color": true,
	},
	caseTheme.LayerLineChart: {
		"style": true, "color": true, "fill_color": true, "grid_color": true,
		"panel_color": true, "font_family": true, "font_weight": true,
	},
	caseTheme.LayerShape: {
		"shape": true, "fill_color": true, "stroke_color": true,
	},
}

var waveshareVisualPresets = map[caseTheme.LayerType][]waveshareVisualPreset{
	caseTheme.LayerTelemetry: {
		{id: "glacier-cards", label: "冰川卡片", options: map[string]any{
			"style": "cards", "font_family": "source-han", "font_weight": "medium",
			"label_color": "A8C4CA", "value_color": "F5FBFF", "accent_color": "4CE0FF",
			"panel_color": "04141BDC", "card_color": "112C35D9", "card_shape": "rounded",
		}},
		{id: "amber-gauge", label: "琥珀横条", options: map[string]any{
			"style": "gauge", "font_family": "rounded", "font_weight": "bold",
			"label_color": "FFD59A", "value_color": "FFF7E8", "accent_color": "FFB45E",
			"panel_color": "211308DC", "card_color": "35200DD9", "card_shape": "pill",
		}},
		{id: "violet-digital", label: "紫电数码", options: map[string]any{
			"style": "digital", "font_family": "digital", "font_weight": "bold",
			"label_color": "C6B7FF", "value_color": "FFFFFF", "accent_color": "8B5CF6",
			"panel_color": "100724E6", "card_color": "1D1045D9", "card_shape": "outline",
		}},
		{id: "mono-minimal", label: "极简白字", options: map[string]any{
			"style": "minimal", "font_family": "monospace", "font_weight": "regular",
			"label_color": "9FB5C0", "value_color": "FFFFFF", "accent_color": "FFFFFF",
			"panel_color": "00000000", "card_color": "00000000", "card_shape": "square",
		}},
	},
	caseTheme.LayerAudioBars: {
		{id: "pink-neon-player", label: "粉紫播放器", options: map[string]any{
			"style": "neon", "bar_shape": "capsule", "direction": "bottom", "count": 48, "gap": 3.0, "thickness": 3.0,
			"glow": true, "glow_radius": 7.0,
			"color": "FF54C8", "gradient_start": "FF54C8", "gradient_end": "8B5CF6", "panel_color": "00000000", "peak_color": "FFFFFF",
		}},
		{id: "pink-center-wave", label: "桃紫双向波", options: map[string]any{
			"style": "waveform", "bar_shape": "line", "direction": "center", "count": 48, "gap": 2.0, "thickness": 2.0,
			"glow": true, "glow_radius": 6.0,
			"color": "FF48C4", "gradient_start": "FF48C4", "gradient_end": "C45CFF", "panel_color": "00000000", "peak_color": "FFFFFF",
		}},
		{id: "cyan-equalizer", label: "冰蓝均衡", options: map[string]any{
			"style": "equalizer", "bar_shape": "rounded", "direction": "bottom", "count": 36, "gap": 3.0, "thickness": 5.0,
			"glow": false, "glow_radius": 4.0,
			"color": "4CE0FF", "gradient_start": "4CE0FF", "gradient_end": "4F7CFF", "panel_color": "05151BD2", "peak_color": "FFF275",
		}},
		{id: "magenta-mirror", label: "桃紫镜像", options: map[string]any{
			"style": "mirror", "bar_shape": "capsule", "direction": "bottom", "count": 32, "gap": 3.0, "thickness": 5.0,
			"glow": true, "glow_radius": 5.0,
			"color": "FF7DD2", "gradient_start": "FF7DD2", "gradient_end": "8B5CF6", "panel_color": "180A20D8", "peak_color": "FFFFFF",
		}},
		{id: "amber-blocks", label: "琥珀像素", options: map[string]any{
			"style": "blocks", "bar_shape": "square", "direction": "bottom", "count": 24, "gap": 4.0, "thickness": 8.0,
			"glow": false, "glow_radius": 4.0,
			"color": "FFB45E", "gradient_start": "FFB45E", "gradient_end": "FFF275", "panel_color": "1D1308D8", "peak_color": "FFFFFF",
		}},
		{id: "green-dots", label: "薄荷点阵", options: map[string]any{
			"style": "dots", "bar_shape": "dots", "direction": "bottom", "count": 40, "gap": 3.0, "thickness": 5.0,
			"glow": false, "glow_radius": 4.0,
			"color": "7CF29A", "gradient_start": "7CF29A", "gradient_end": "4CE0FF", "panel_color": "041710D2", "peak_color": "FFF275",
		}},
		{id: "violet-burst", label: "星云爆发", options: map[string]any{
			"style": "burst", "bar_shape": "line", "direction": "fourway", "count": 48, "gap": 2.0, "thickness": 2.0,
			"glow": true, "glow_radius": 6.0,
			"color": "55E5FF", "gradient_start": "55E5FF", "gradient_end": "FF7DD2", "panel_color": "10091AD2", "peak_color": "FFF275",
		}},
	},
	caseTheme.LayerAudioRing: {
		{id: "cyan-waveform", label: "青紫连续波环", options: map[string]any{
			"style": "waveform", "direction": "outward", "count": 48, "inner_radius": .62, "thickness": 1.0,
			"glow": true, "glow_radius": 7.0,
			"color": "55E5FF", "gradient_start": "55E5FF", "gradient_end": "FF54C8", "panel_color": "00000000", "peak_color": "FFFFFF",
		}},
		{id: "mint-dots", label: "薄荷点阵环", options: map[string]any{
			"style": "dots", "direction": "outward", "count": 36, "inner_radius": .58, "thickness": 5.0,
			"glow": false, "glow_radius": 4.0,
			"color": "7CF29A", "gradient_start": "7CF29A", "gradient_end": "4CE0FF", "panel_color": "041710D2", "peak_color": "FFF275",
		}},
		{id: "violet-halo", label: "紫焰双环", options: map[string]any{
			"style": "halo", "direction": "bidirectional", "count": 48, "inner_radius": .55, "thickness": 3.0,
			"glow": true, "glow_radius": 8.0,
			"color": "FF7DD2", "gradient_start": "FF7DD2", "gradient_end": "8B5CF6", "panel_color": "12081CDC", "peak_color": "FFF275",
		}},
		{id: "cyan-radial", label: "青蓝辐射", options: map[string]any{
			"style": "radial", "direction": "outward", "count": 48, "inner_radius": .52, "thickness": 3.0,
			"glow": false, "glow_radius": 4.0,
			"color": "4CE0FF", "gradient_start": "4CE0FF", "gradient_end": "4F7CFF", "panel_color": "061218DC", "peak_color": "FFFFFF",
		}},
		{id: "pink-pulse", label: "桃红脉冲", options: map[string]any{
			"style": "pulse", "direction": "outward", "count": 40, "inner_radius": .50, "thickness": 4.0,
			"glow": true, "glow_radius": 8.0,
			"color": "FF5C68", "gradient_start": "FF5C68", "gradient_end": "FF7DD2", "panel_color": "20090FD8", "peak_color": "FFF275",
		}},
		{id: "amber-dashed", label: "金色刻度", options: map[string]any{
			"style": "dashed", "direction": "outward", "count": 48, "inner_radius": .55, "thickness": 4.0,
			"glow": false, "glow_radius": 4.0,
			"color": "FFF275", "gradient_start": "FFF275", "gradient_end": "FFB45E", "panel_color": "171204D2", "peak_color": "FFFFFF",
		}},
	},
	caseTheme.LayerLyrics: {
		{id: "aurora-player", label: "极光播放器", options: map[string]any{
			"style": "player", "font_family": "source-han", "font_weight": "bold", "align": "center",
			"current_color": "F7FCFF", "next_color": "B8C8DAA4", "accent_color": "55E5FF",
			"panel_color": "030A1292", "panel_border_color": "66E3FF3E",
			"outline_color": "02070EE1", "outline_width": 2.0,
			"shadow": true, "shadow_color": "000000B4", "glow_color": "53DAFF42", "glow_radius": 4.0,
			"karaoke_highlight": true, "smooth_transition": true, "show_progress": true,
		}},
		{id: "magenta-karaoke", label: "桃紫卡拉 OK", options: map[string]any{
			"style": "karaoke", "font_family": "rounded", "font_weight": "bold", "align": "center",
			"current_color": "FFF7FD", "next_color": "E2C5DFA8", "accent_color": "FF70CF",
			"panel_color": "18091FA0", "panel_border_color": "FF70CF48",
			"outline_color": "140516E8", "outline_width": 2.0,
			"shadow": true, "shadow_color": "000000C0", "glow_color": "FF70CF46", "glow_radius": 5.0,
			"karaoke_highlight": true, "smooth_transition": true, "show_progress": true,
		}},
		{id: "amber-classic", label: "暖金经典双行", options: map[string]any{
			"style": "classic", "font_family": "serif", "font_weight": "bold", "align": "left",
			"current_color": "FFF8E9", "next_color": "D9C8A3A8", "accent_color": "FFCA6A",
			"panel_color": "1E130998", "panel_border_color": "FFCA6A42",
			"outline_color": "130C05E8", "outline_width": 2.0,
			"shadow": true, "shadow_color": "000000C0", "glow_color": "FFCA6A36", "glow_radius": 3.0,
			"karaoke_highlight": false, "smooth_transition": false, "show_progress": true,
		}},
		{id: "clean-minimal", label: "纯净极简", options: map[string]any{
			"style": "minimal", "font_family": "source-han", "font_weight": "medium", "align": "center",
			"current_color": "FFFFFF", "next_color": "FFFFFF8F", "accent_color": "FFFFFF",
			"panel_color": "00000000", "panel_border_color": "00000000",
			"outline_color": "00000000", "outline_width": 0.0,
			"shadow": true, "shadow_color": "000000A0", "glow_color": "00000000", "glow_radius": 0.0,
			"karaoke_highlight": false, "smooth_transition": true, "show_progress": false,
		}},
	},
	caseTheme.LayerText: {
		{id: "clean-title", label: "清爽标题", options: map[string]any{
			"font_family": "source-han", "font_weight": "bold", "color": "FFFFFF", "panel_color": "00000000",
		}},
		{id: "cyan-neon", label: "赛博霓虹", options: map[string]any{
			"font_family": "digital", "font_weight": "bold", "color": "4CE0FF", "panel_color": "04141BE6",
		}},
		{id: "warning-plate", label: "告警铭牌", options: map[string]any{
			"font_family": "simhei", "font_weight": "bold", "color": "FFF275", "panel_color": "351014EA",
		}},
		{id: "serif-caption", label: "雅致宋体", options: map[string]any{
			"font_family": "serif", "font_weight": "regular", "color": "FFE3C0", "panel_color": "24180ACC",
		}},
	},
	caseTheme.LayerClock: {
		{id: "lcd-cyan", label: "液晶冰蓝", options: map[string]any{
			"font_family": "digital", "font_weight": "bold", "color": "4CE0FF", "panel_color": "04141BE0",
		}},
		{id: "mono-minimal", label: "极简白钟", options: map[string]any{
			"font_family": "monospace", "font_weight": "medium", "color": "FFFFFF", "panel_color": "00000000",
		}},
		{id: "amber-rounded", label: "暖金圆体", options: map[string]any{
			"font_family": "rounded", "font_weight": "bold", "color": "FFF275", "panel_color": "251607DE",
		}},
		{id: "violet-panel", label: "紫夜时钟", options: map[string]any{
			"font_family": "source-han", "font_weight": "bold", "color": "FF7DD2", "panel_color": "160A2BE8",
		}},
	},
	caseTheme.LayerProgressBar: {
		{id: "cyan-pill", label: "冰蓝胶囊", options: map[string]any{
			"style": "rounded", "color": "4CE0FF", "secondary_color": "183740",
			"panel_color": "04141BBC", "font_family": "source-han", "font_weight": "medium",
		}},
		{id: "pink-neon", label: "桃红霓虹", options: map[string]any{
			"style": "neon", "color": "FF7DD2", "secondary_color": "381633",
			"panel_color": "180A20D8", "font_family": "digital", "font_weight": "bold",
		}},
		{id: "amber-segments", label: "琥珀分段", options: map[string]any{
			"style": "segmented", "color": "FFB45E", "secondary_color": "3A2A16",
			"panel_color": "1D1308D8", "font_family": "monospace", "font_weight": "bold",
		}},
		{id: "green-dots", label: "薄荷点列", options: map[string]any{
			"style": "dots", "color": "7CF29A", "secondary_color": "193A26",
			"panel_color": "06170DD2", "font_family": "rounded", "font_weight": "medium",
		}},
	},
	caseTheme.LayerGauge: {
		{id: "cyan-arc", label: "冰蓝圆弧", options: map[string]any{
			"style": "arc", "font_family": "source-han", "font_weight": "bold", "color": "4CE0FF",
			"secondary_color": "183740", "panel_color": "04141BBC",
		}},
		{id: "amber-ticks", label: "琥珀刻度", options: map[string]any{
			"style": "ticks", "font_family": "monospace", "font_weight": "bold", "color": "FFB45E",
			"secondary_color": "3B2A17", "panel_color": "1D1308D8",
		}},
		{id: "violet-ring", label: "紫电圆环", options: map[string]any{
			"style": "ring", "font_family": "digital", "font_weight": "bold", "color": "8B5CF6",
			"secondary_color": "2B1F4D", "panel_color": "110929E2",
		}},
		{id: "white-minimal", label: "极简白环", options: map[string]any{
			"style": "minimal", "font_family": "monospace", "font_weight": "regular", "color": "FFFFFF",
			"secondary_color": "FFFFFF26", "panel_color": "00000000",
		}},
	},
	caseTheme.LayerGradientGauge: {
		{id: "thermal-arc", label: "温度渐变", options: map[string]any{
			"style": "arc", "font_family": "source-han", "font_weight": "bold",
			"gradient_start": "4CE0FF", "gradient_mid": "FFF275", "gradient_end": "FF5C68", "panel_color": "04141BBC",
		}},
		{id: "aurora-ticks", label: "极光刻度", options: map[string]any{
			"style": "ticks", "font_family": "digital", "font_weight": "bold",
			"gradient_start": "7CF29A", "gradient_mid": "4CE0FF", "gradient_end": "8B5CF6", "panel_color": "071825E0",
		}},
		{id: "sunset-semi", label: "日落半圆", options: map[string]any{
			"style": "semicircle", "font_family": "rounded", "font_weight": "bold",
			"gradient_start": "FFF275", "gradient_mid": "FFB45E", "gradient_end": "FF5C68", "panel_color": "241008DE",
		}},
		{id: "violet-minimal", label: "紫白极简", options: map[string]any{
			"style": "minimal", "font_family": "monospace", "font_weight": "medium",
			"gradient_start": "FFFFFF", "gradient_mid": "C6B7FF", "gradient_end": "8B5CF6", "panel_color": "00000000",
		}},
	},
	caseTheme.LayerLineChart: {
		{id: "cyan-area", label: "冰蓝面积", options: map[string]any{
			"style": "area", "color": "4CE0FF", "fill_color": "4CE0FF38", "grid_color": "52727E66",
			"panel_color": "04141BBC", "font_family": "source-han", "font_weight": "medium",
		}},
		{id: "green-line", label: "薄荷折线", options: map[string]any{
			"style": "line", "color": "7CF29A", "fill_color": "7CF29A24", "grid_color": "42735A55",
			"panel_color": "06170DD2", "font_family": "monospace", "font_weight": "bold",
		}},
		{id: "pink-neon", label: "桃紫霓虹", options: map[string]any{
			"style": "neon", "color": "FF7DD2", "fill_color": "8B5CF666", "grid_color": "00000000",
			"panel_color": "160A25E0", "font_family": "digital", "font_weight": "bold",
		}},
		{id: "amber-spline", label: "暖金平滑", options: map[string]any{
			"style": "spline", "color": "FFF275", "fill_color": "FFB45E28", "grid_color": "00000000",
			"panel_color": "1C1408D8", "font_family": "rounded", "font_weight": "medium",
		}},
	},
	caseTheme.LayerShape: {
		{id: "cyan-panel", label: "冰蓝面板", options: map[string]any{
			"shape": "rounded_rect", "fill_color": "0B2029CC", "stroke_color": "4CE0FF",
		}},
		{id: "violet-pill", label: "紫电胶囊", options: map[string]any{
			"shape": "pill", "fill_color": "241044DD", "stroke_color": "FF7DD2",
		}},
		{id: "amber-outline", label: "琥珀边框", options: map[string]any{
			"shape": "rect", "fill_color": "1D130840", "stroke_color": "FFB45E",
		}},
		{id: "green-orbit", label: "薄荷圆环", options: map[string]any{
			"shape": "ellipse", "fill_color": "071D1238", "stroke_color": "7CF29A",
		}},
	},
}

func waveshareVisualPresetsForLayerType(layerType caseTheme.LayerType) []waveshareVisualPreset {
	return waveshareVisualPresets[layerType]
}

func wavesharePresetNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	default:
		return 0, false
	}
}

func wavesharePresetValuesEqual(current, wanted any) bool {
	currentNumber, currentOK := wavesharePresetNumber(current)
	wantedNumber, wantedOK := wavesharePresetNumber(wanted)
	if currentOK && wantedOK {
		return math.Abs(currentNumber-wantedNumber) < 1e-9
	}
	return reflect.DeepEqual(current, wanted)
}

func waveshareVisualOptionAllowed(layer *caseTheme.Layer, key string) bool {
	return layer != nil && waveshareVisualOptionKeys[layer.Type][key]
}

func waveshareVisualPresetMatches(layer *caseTheme.Layer, preset waveshareVisualPreset) bool {
	if layer == nil || len(preset.options) == 0 {
		return false
	}
	compared := 0
	for key, wanted := range preset.options {
		if !waveshareVisualOptionAllowed(layer, key) {
			continue
		}
		compared++
		if !wavesharePresetValuesEqual(layer.Options[key], wanted) {
			return false
		}
	}
	return compared > 0
}

// applyWaveshareVisualPreset 返回实际改变的字段数，调用方可据此避免空的历史记录。
func applyWaveshareVisualPreset(layer *caseTheme.Layer, preset waveshareVisualPreset) int {
	if layer == nil {
		return 0
	}
	if len(waveshareVisualOptionKeys[layer.Type]) == 0 {
		return 0
	}
	changed := 0
	if layer.Options == nil {
		layer.Options = make(map[string]any)
	}
	for key, value := range preset.options {
		if !waveshareVisualOptionAllowed(layer, key) || wavesharePresetValuesEqual(layer.Options[key], value) {
			continue
		}
		layer.Options[key] = value
		changed++
	}
	return changed
}

func (model *gioEditor) currentWaveshareVisualPreset(layer *caseTheme.Layer) string {
	for _, preset := range waveshareVisualPresetsForLayerType(layer.Type) {
		if waveshareVisualPresetMatches(layer, preset) {
			return preset.id
		}
	}
	return ""
}

func (model *gioEditor) setWaveshareVisualPreset(layer *caseTheme.Layer, preset waveshareVisualPreset) {
	if waveshareVisualPresetMatches(layer, preset) {
		return
	}
	model.pushUndo()
	if model.applyWaveshareVisualPresetAfterUndo(layer, preset) == 0 {
		// 理论上只会发生在无有效字段的防御路径；不要留下空的撤销记录。
		model.undoStack = model.undoStack[:len(model.undoStack)-1]
		return
	}
	model.message = "已套用视觉预设：" + preset.label
}

// applyWaveshareVisualPresetAfterUndo 只执行变更，不创建历史记录。属性页和
// 通用预设页可分别在自己的操作边界内调用，保证一次点击只产生一次撤销。
func (model *gioEditor) applyWaveshareVisualPresetAfterUndo(layer *caseTheme.Layer, preset waveshareVisualPreset) int {
	changed := applyWaveshareVisualPreset(layer, preset)
	if changed == 0 {
		return 0
	}
	// 数字和颜色输入框由 optionEditors 缓存；清掉对应缓存可让预设立即显示在属性区。
	for key := range preset.options {
		if waveshareVisualOptionAllowed(layer, key) {
			delete(model.optionEditors, layer.ID+"\x00"+key)
		}
	}
	model.componentLayerID = ""
	model.requestPreviewRefresh()
	return changed
}

// applyUniversalVisualPreset 在选中同类图层时直接改样式，否则在画布中心
// 新建组件。新建与套用共享 addLayerAt 创建的同一条历史记录。
func (model *gioEditor) applyUniversalVisualPreset(layerType caseTheme.LayerType, preset waveshareVisualPreset) {
	if model.selected >= 0 && model.selected < len(model.manifest.Layers) && model.manifest.Layers[model.selected].Type == layerType {
		layer := &model.manifest.Layers[model.selected]
		if !waveshareVisualPresetMatches(layer, preset) {
			model.pushUndo()
			model.applyWaveshareVisualPresetAfterUndo(layer, preset)
		}
		model.inspectorTab = "properties"
		model.message = "已为当前" + layerLabel(layerType) + "套用预设：" + preset.label
		return
	}

	before := len(model.manifest.Layers)
	model.addLayerAt(layerType, .5, .5)
	if len(model.manifest.Layers) != before+1 || model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := &model.manifest.Layers[model.selected]
	model.applyWaveshareVisualPresetAfterUndo(layer, preset)
	model.inspectorTab = "properties"
	model.message = "已在画布中心新建" + layerLabel(layerType) + "并套用预设：" + preset.label
}

func (model *gioEditor) currentUniversalVisualPreset(layerType caseTheme.LayerType) string {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return ""
	}
	layer := &model.manifest.Layers[model.selected]
	if layer.Type != layerType {
		return ""
	}
	return model.currentWaveshareVisualPreset(layer)
}

func (model *gioEditor) layoutUniversalVisualPresetGrid(gtx layout.Context, layerType caseTheme.LayerType, presets []waveshareVisualPreset) layout.Dimensions {
	const columns = 2
	current := model.currentUniversalVisualPreset(layerType)
	rows := (len(presets) + columns - 1) / columns
	children := make([]layout.FlexChild, 0, rows)
	for row := 0; row < rows; row++ {
		row := row
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			buttons := make([]layout.FlexChild, 0, columns)
			for column := 0; column < columns; column++ {
				index := row*columns + column
				if index >= len(presets) {
					buttons = append(buttons, layout.Flexed(1, func(layout.Context) layout.Dimensions { return layout.Dimensions{} }))
					continue
				}
				preset := presets[index]
				buttons = append(buttons, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						id := "universal-preset-" + string(layerType) + "-" + preset.id
						return model.buttonState(gtx, model.optionButton(id), preset.label, func() {
							model.applyUniversalVisualPreset(layerType, preset)
						}, current == preset.id)
					})
				}))
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, buttons...)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layoutUniversalVisualPresets(gtx layout.Context) layout.Dimensions {
	return model.presetList.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, "选择预设后会修改当前同类图层；未选中同类图层时会在画布中心新建组件。")
				label.Color = model.theme.Palette.Fg
				return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, label.Layout)
			}),
		}
		for _, section := range universalVisualPresetSections {
			section := section
			children = append(children,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, section.label) }),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return model.layoutUniversalVisualPresetGrid(gtx, section.layerType, waveshareVisualPresetsForLayerType(section.layerType))
				}),
			)
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (model *gioEditor) layoutWaveshareVisualPresets(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	presets := waveshareVisualPresetsForLayerType(layer.Type)
	if len(presets) == 0 {
		return layout.Dimensions{}
	}
	choices := make([]optionChoice, 0, len(presets))
	byID := make(map[string]waveshareVisualPreset, len(presets))
	for _, preset := range presets {
		choices = append(choices, optionChoice{value: preset.id, label: preset.label})
		byID[preset.id] = preset
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "一键视觉预设")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := material.Body2(model.theme, "仅替换样式、字体和配色；数据、位置尺寸与显示状态保持不变。")
			label.Color = model.theme.Palette.Fg
			return layout.Inset{Bottom: unit.Dp(5)}.Layout(gtx, label.Layout)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-visual-preset", choices, model.currentWaveshareVisualPreset(layer), func(value string) {
				preset := byID[value]
				model.applyWaveshareVisualPresetAfterUndo(layer, preset)
				model.message = "已套用视觉预设：" + preset.label
			}, 3)
		}),
	)
}
