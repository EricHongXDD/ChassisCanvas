package workbench

import (
	"image"
	"reflect"
	"strconv"
	"strings"
	"testing"

	caseTheme "casescreen/internal/theme"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

func TestWaveshareVisualPresetsCoverComponentLibrary(t *testing.T) {
	layerTypes := []caseTheme.LayerType{
		caseTheme.LayerTelemetry, caseTheme.LayerAudioBars, caseTheme.LayerAudioRing, caseTheme.LayerLyrics,
		caseTheme.LayerText, caseTheme.LayerClock, caseTheme.LayerProgressBar,
		caseTheme.LayerGauge, caseTheme.LayerGradientGauge, caseTheme.LayerLineChart, caseTheme.LayerShape,
	}
	styles := map[caseTheme.LayerType]map[string]bool{
		caseTheme.LayerTelemetry:     {"cards": true, "list": true, "compact": true, "gauge": true, "dials": true, "tiles": true, "digital": true, "minimal": true},
		caseTheme.LayerAudioBars:     {"equalizer": true, "waveform": true, "mirror": true, "center": true, "blocks": true, "dots": true, "neon": true, "outline": true, "burst": true},
		caseTheme.LayerAudioRing:     {"waveform": true, "radial": true, "donut": true, "dots": true, "dashed": true, "halo": true, "spokes": true, "pulse": true},
		caseTheme.LayerLyrics:        {"player": true, "karaoke": true, "classic": true, "minimal": true},
		caseTheme.LayerProgressBar:   {"rounded": true, "square": true, "segmented": true, "dots": true, "neon": true, "outline": true},
		caseTheme.LayerGauge:         {"arc": true, "dial": true, "semicircle": true, "ring": true, "ticks": true, "minimal": true},
		caseTheme.LayerGradientGauge: {"arc": true, "dial": true, "semicircle": true, "ring": true, "ticks": true, "minimal": true},
		caseTheme.LayerLineChart:     {"line": true, "area": true, "step": true, "spline": true, "dots": true, "neon": true},
	}
	fonts := map[string]bool{"source-han": true, "pangmen": true, "microsoft-yahei": true, "simhei": true, "digital": true, "monospace": true, "rounded": true, "serif": true}
	shapes := map[string]bool{"rect": true, "rounded_rect": true, "circle": true, "ellipse": true, "line": true, "pill": true}

	for _, layerType := range layerTypes {
		presets := waveshareVisualPresetsForLayerType(layerType)
		if len(presets) < 3 {
			t.Fatalf("%s 的视觉预设不足三种：%d", layerType, len(presets))
		}
		seen := make(map[string]bool)
		for _, preset := range presets {
			if preset.id == "" || preset.label == "" || seen[preset.id] {
				t.Fatalf("%s 存在空值或重复预设：%q / %q", layerType, preset.id, preset.label)
			}
			seen[preset.id] = true
			if len(preset.options) < 3 {
				t.Fatalf("%s/%s 没有同时改变多项外观：%v", layerType, preset.id, preset.options)
			}
			for key, value := range preset.options {
				if !waveshareVisualOptionKeys[layerType][key] {
					t.Fatalf("%s/%s 包含非外观或编译器不支持的字段 %q", layerType, preset.id, key)
				}
				switch key {
				case "style":
					if !styles[layerType][value.(string)] {
						t.Fatalf("%s/%s 使用编译器未支持的样式 %q", layerType, preset.id, value)
					}
				case "font_family":
					if !fonts[value.(string)] {
						t.Fatalf("%s/%s 使用未知字体 %q", layerType, preset.id, value)
					}
				case "shape":
					if !shapes[value.(string)] {
						t.Fatalf("%s/%s 使用未知形状 %q", layerType, preset.id, value)
					}
				}
				if key == "color" || strings.HasSuffix(key, "_color") || strings.HasPrefix(key, "gradient_") {
					textValue := strings.TrimPrefix(value.(string), "#")
					if (len(textValue) != 6 && len(textValue) != 8) || !isHexPresetColor(textValue) {
						t.Fatalf("%s/%s 的颜色无效：%s=%v", layerType, preset.id, key, value)
					}
				}
			}
		}
	}
}

func isHexPresetColor(value string) bool {
	_, err := strconv.ParseUint(value, 16, 32)
	return err == nil
}

func TestUniversalPresetCatalogHasRequestedCountsAndDistinctStylesAndPalettes(t *testing.T) {
	expected := map[caseTheme.LayerType]int{
		caseTheme.LayerTelemetry: 4,
		caseTheme.LayerAudioBars: 7,
		caseTheme.LayerAudioRing: 6,
		caseTheme.LayerLyrics:    4,
	}
	colorKeys := []string{
		"color", "label_color", "value_color", "current_color", "next_color", "accent_color",
		"gradient_start", "gradient_end", "panel_color", "card_color", "peak_color",
	}
	for layerType, wanted := range expected {
		presets := waveshareVisualPresetsForLayerType(layerType)
		if len(presets) != wanted {
			t.Fatalf("%s 预设数量错误：got=%d want=%d", layerType, len(presets), wanted)
		}
		styles := make(map[string]string, len(presets))
		palettes := make(map[string]string, len(presets))
		for _, preset := range presets {
			style, _ := preset.options["style"].(string)
			if style == "" {
				t.Fatalf("%s/%s 缺少可辨识的 style", layerType, preset.id)
			}
			if previous := styles[style]; previous != "" {
				t.Fatalf("%s/%s 与 %s 使用相同样式 %q", layerType, preset.id, previous, style)
			}
			styles[style] = preset.id

			paletteParts := make([]string, 0, len(colorKeys))
			for _, key := range colorKeys {
				if value, ok := preset.options[key].(string); ok {
					paletteParts = append(paletteParts, key+"="+value)
				}
			}
			if len(paletteParts) < 3 {
				t.Fatalf("%s/%s 的配色字段不足：%v", layerType, preset.id, preset.options)
			}
			palette := strings.Join(paletteParts, "|")
			if previous := palettes[palette]; previous != "" {
				t.Fatalf("%s/%s 与 %s 使用相同配色", layerType, preset.id, previous)
			}
			palettes[palette] = preset.id
		}
	}
}

func TestApplyWaveshareVisualPresetPreservesDataGeometryAndVisibility(t *testing.T) {
	layerTypes := []caseTheme.LayerType{
		caseTheme.LayerTelemetry, caseTheme.LayerAudioBars, caseTheme.LayerAudioRing, caseTheme.LayerLyrics,
		caseTheme.LayerText, caseTheme.LayerClock, caseTheme.LayerProgressBar,
		caseTheme.LayerGauge, caseTheme.LayerGradientGauge, caseTheme.LayerLineChart, caseTheme.LayerShape,
	}
	protected := map[string]any{
		"visible": false, "fields": []string{"GPU_Temp", "time"}, "data_key": "Memory_Load",
		"text": "保留内容", "format": "yyyy-MM-dd", "source": "system", "min_value": -20.0, "max_value": 180.0,
		"show_label": false, "show_value": false, "show_unit": false, "show_grid": false, "peak_hold": true,
		"min_amplitude": .12, "max_amplitude": .88,
		"peak_hold_ms": 820.0, "peak_decay": 2.4,
		"orientation": "vertical", "segments": 7, "bar_length": 29,
		"start_angle": -117.0, "end_angle": 123.0, "font_size": 57.0, "align": "right",
		"corner_radius": 23.0, "stroke_width": 5.0, "shadow": true,
	}
	for _, layerType := range layerTypes {
		options := make(map[string]any, len(protected))
		for key, value := range protected {
			options[key] = value
		}
		layer := caseTheme.Layer{
			ID: "keep", Type: layerType, Bounds: caseTheme.Rect{X: .13, Y: .17, Width: .42, Height: .31},
			Opacity: .73, Rotation: 19, ZIndex: 7, Options: options,
		}
		bounds, opacity, rotation, zIndex := layer.Bounds, layer.Opacity, layer.Rotation, layer.ZIndex
		changed := applyWaveshareVisualPreset(&layer, waveshareVisualPresetsForLayerType(layerType)[0])
		if changed < 2 {
			t.Fatalf("%s 的预设没有一次改变多项外观：%d", layerType, changed)
		}
		if !reflect.DeepEqual(layer.Bounds, bounds) || layer.Opacity != opacity || layer.Rotation != rotation || layer.ZIndex != zIndex {
			t.Fatalf("%s 的预设改变了图层几何或透明度：%+v", layerType, layer)
		}
		for key, wanted := range protected {
			if waveshareVisualOptionAllowed(&layer, key) {
				continue
			}
			if !reflect.DeepEqual(layer.Options[key], wanted) {
				t.Errorf("%s 的预设不应改变 %s：got=%v want=%v", layerType, key, layer.Options[key], wanted)
			}
		}
	}
}

func TestAudioVisualPresetsNeverChangeSharedDynamics(t *testing.T) {
	dynamics := []string{"gain", "sensitivity", "smoothing", "noise_gate", "contrast", "separation", "bass_boost", "floor_reduction"}
	for _, layerType := range []caseTheme.LayerType{caseTheme.LayerAudioBars, caseTheme.LayerAudioRing} {
		for _, key := range dynamics {
			if waveshareVisualOptionKeys[layerType][key] {
				t.Fatalf("%s 的视觉白名单仍包含共享动态字段 %q", layerType, key)
			}
		}
		for _, preset := range waveshareVisualPresetsForLayerType(layerType) {
			for _, key := range dynamics {
				if _, exists := preset.options[key]; exists {
					t.Fatalf("%s/%s 仍会改写共享动态字段 %q", layerType, preset.id, key)
				}
			}
		}
	}
}

func TestAudioVisualPresetsAreSelfContainedAndOrderIndependent(t *testing.T) {
	required := map[caseTheme.LayerType][]string{
		caseTheme.LayerAudioBars: {
			"style", "bar_shape", "direction", "count", "gap", "thickness", "glow", "glow_radius",
			"color", "gradient_start", "gradient_end", "panel_color", "peak_color",
		},
		caseTheme.LayerAudioRing: {
			"style", "direction", "count", "inner_radius", "thickness", "glow", "glow_radius",
			"color", "gradient_start", "gradient_end", "panel_color", "peak_color",
		},
	}
	for layerType, keys := range required {
		presets := waveshareVisualPresetsForLayerType(layerType)
		for index, preset := range presets {
			for _, key := range keys {
				if _, exists := preset.options[key]; !exists {
					t.Fatalf("%s/%s 缺少自包含视觉字段 %q", layerType, preset.id, key)
				}
			}

			previous := presets[(index+len(presets)-1)%len(presets)]
			chained := caseTheme.Layer{Type: layerType, Options: map[string]any{"source": "system", "gain": 2.7}}
			fresh := caseTheme.Layer{Type: layerType, Options: map[string]any{"source": "system", "gain": 2.7}}
			applyWaveshareVisualPreset(&chained, previous)
			applyWaveshareVisualPreset(&chained, preset)
			applyWaveshareVisualPreset(&fresh, preset)
			for _, key := range keys {
				if !reflect.DeepEqual(chained.Options[key], fresh.Options[key]) {
					t.Fatalf("%s/%s 仍受前一预设 %s 的 %q 影响：chained=%v fresh=%v", layerType, preset.id, previous.id, key, chained.Options[key], fresh.Options[key])
				}
			}
			if chained.Options["gain"] != 2.7 || chained.Options["source"] != "system" {
				t.Fatalf("%s/%s 改写了共享音频输入参数：%v", layerType, preset.id, chained.Options)
			}
		}
	}
}

func TestUniversalPresetAppliesOrAddsAsOneUndoStep(t *testing.T) {
	preset := waveshareVisualPresetsForLayerType(caseTheme.LayerAudioBars)[0]
	existing := &gioEditor{
		manifest: caseTheme.Manifest{Width: 1024, Height: 600, Layers: []caseTheme.Layer{{
			ID: "bars", Type: caseTheme.LayerAudioBars, Bounds: caseTheme.Rect{X: .1, Y: .1, Width: .4, Height: .2}, Opacity: 1,
			Options: map[string]any{"style": "equalizer", "gain": 2.7, "source": "system", "min_amplitude": .1, "max_amplitude": .9},
		}}},
		selected: 0,
	}
	existing.applyUniversalVisualPreset(caseTheme.LayerAudioBars, preset)
	if len(existing.undoStack) != 1 || len(existing.manifest.Layers) != 1 {
		t.Fatalf("同类图层套用预设应只产生一次撤销：undo=%d layers=%d", len(existing.undoStack), len(existing.manifest.Layers))
	}
	if existing.inspectorTab != "properties" || !strings.Contains(existing.message, "当前条状频谱") {
		t.Fatalf("套用后没有进入属性页或提示不明确：tab=%q message=%q", existing.inspectorTab, existing.message)
	}
	if existing.manifest.Layers[0].Options["gain"] != 2.7 || existing.manifest.Layers[0].Options["source"] != "system" || existing.manifest.Layers[0].Options["min_amplitude"] != .1 || existing.manifest.Layers[0].Options["max_amplitude"] != .9 {
		t.Fatalf("视觉预设改写了共享音频参数：%v", existing.manifest.Layers[0].Options)
	}
	existing.undo()
	if existing.manifest.Layers[0].Options["style"] != "equalizer" {
		t.Fatalf("一次撤销没有恢复原样式：%v", existing.manifest.Layers[0].Options)
	}

	added := &gioEditor{target: "coolermaster-atmos-v2", manifest: defaultManifestFor(1920, 480), selected: 0}
	before := len(added.manifest.Layers)
	added.applyUniversalVisualPreset(caseTheme.LayerAudioBars, preset)
	if len(added.undoStack) != 1 || len(added.manifest.Layers) != before+1 {
		t.Fatalf("新建并套用预设应只产生一次撤销：undo=%d layers=%d", len(added.undoStack), len(added.manifest.Layers))
	}
	created := added.manifest.Layers[added.selected]
	if created.Type != caseTheme.LayerAudioBars || !waveshareVisualPresetMatches(&created, preset) || added.inspectorTab != "properties" || !strings.Contains(added.message, "画布中心新建条状频谱") {
		t.Fatalf("新建预设组件状态不完整：layer=%+v tab=%q message=%q", created, added.inspectorTab, added.message)
	}
	added.undo()
	if len(added.manifest.Layers) != before {
		t.Fatalf("一次撤销没有同时移除新增预设组件：%+v", added.manifest.Layers)
	}
}

func TestUniversalPresetPageListsAllFourCategoriesAndGenericTelemetryProperties(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{
		theme: material.NewTheme(), target: "coolermaster-atmos-v2", manifest: caseTheme.Manifest{Width: 1920, Height: 480, Layers: []caseTheme.Layer{{
			ID: "telemetry", Type: caseTheme.LayerTelemetry, Bounds: caseTheme.Rect{Width: .4, Height: .2}, Opacity: 1, Options: map[string]any{"style": "cards"},
		}}}, selected: 0,
	}
	model.presetList.Axis = layout.Vertical
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(520, 2400)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	model.layoutUniversalVisualPresets(gtx)
	router.Frame(&operations)
	for _, section := range universalVisualPresetSections {
		for _, preset := range waveshareVisualPresetsForLayerType(section.layerType) {
			key := "universal-preset-" + string(section.layerType) + "-" + preset.id
			if model.componentOptions[key] == nil {
				t.Fatalf("通用预设页缺少 %s/%s", section.layerType, preset.id)
			}
		}
	}
	if len(universalVisualPresetSections) != 4 {
		t.Fatalf("通用预设页分类数量错误：%d", len(universalVisualPresetSections))
	}

	operations.Reset()
	gtx = layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(520, 1600)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	model.layoutTelemetryProperties(gtx, &model.manifest.Layers[0])
	first := waveshareVisualPresetsForLayerType(caseTheme.LayerTelemetry)[0]
	if model.componentOptions["telemetry-visual-preset-"+first.id] == nil {
		t.Fatal("非微雪遥测属性页顶部没有现有视觉预设")
	}
}

func TestWaveshareLegacyDeviceMicrophonePresetUsesHostSpectrumStyles(t *testing.T) {
	layer := caseTheme.Layer{Type: caseTheme.LayerAudioBars, Options: map[string]any{
		"source": "device_mic", "style": "equalizer", "bar_shape": "rounded", "count": 19, "gain": 2.7,
	}}
	var preset waveshareVisualPreset
	for _, candidate := range waveshareVisualPresetsForLayerType(caseTheme.LayerAudioBars) {
		if candidate.id == "magenta-mirror" {
			preset = candidate
			break
		}
	}
	if preset.id == "" {
		t.Fatal("缺少桃紫镜像预设")
	}
	if changed := applyWaveshareVisualPreset(&layer, preset); changed != len(preset.options) {
		t.Fatalf("旧麦克风图层应按主机频谱应用完整视觉预设：got=%d options=%v", changed, layer.Options)
	}
	if layer.Options["style"] != "mirror" || layer.Options["bar_shape"] != "capsule" || layer.Options["direction"] != "bottom" || layer.Options["count"] != 32 {
		t.Fatalf("旧麦克风图层没有按主机频谱应用样式：%v", layer.Options)
	}
	if layer.Options["source"] != "device_mic" || layer.Options["gain"] != 2.7 {
		t.Fatalf("完整视觉预设不应改写旧图层的输入源或动态参数：%v", layer.Options)
	}
	if !waveshareVisualPresetMatches(&layer, preset) {
		t.Fatal("旧麦克风图层套用完整样式后应识别为当前预设")
	}
}

func TestApplyWaveshareVisualPresetRejectsUnsafeFields(t *testing.T) {
	layer := caseTheme.Layer{Type: caseTheme.LayerProgressBar, Options: map[string]any{
		"visible": false, "data_key": "Gpu_Load", "orientation": "vertical", "font_size": 31.0,
	}}
	unsafe := waveshareVisualPreset{id: "unsafe", label: "不安全", options: map[string]any{
		"style": "neon", "color": "4CE0FF", "visible": true, "data_key": "Cpu_Freq",
		"orientation": "horizontal", "font_size": 99.0,
	}}
	if changed := applyWaveshareVisualPreset(&layer, unsafe); changed != 2 {
		t.Fatalf("白名单之外的字段不应应用：changed=%d options=%v", changed, layer.Options)
	}
	if layer.Options["visible"] != false || layer.Options["data_key"] != "Gpu_Load" || layer.Options["orientation"] != "vertical" || layer.Options["font_size"] != 31.0 {
		t.Fatalf("不安全字段被预设改写：%v", layer.Options)
	}
}

func TestSetWaveshareVisualPresetCreatesUndoBeforeApplying(t *testing.T) {
	layer := caseTheme.Layer{ID: "clock-1", Type: caseTheme.LayerClock, Options: map[string]any{
		"font_family": "source-han", "font_weight": "regular", "color": "FFFFFF", "panel_color": "00000000",
	}}
	model := &gioEditor{manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{layer}}, selected: 0}
	preset := waveshareVisualPresetsForLayerType(caseTheme.LayerClock)[0]
	model.setWaveshareVisualPreset(&model.manifest.Layers[0], preset)
	if len(model.undoStack) != 1 {
		t.Fatalf("套用预设应生成一次撤销记录：%d", len(model.undoStack))
	}
	model.undo()
	restored := model.manifest.Layers[0].Options
	if restored["font_family"] != "source-han" || restored["font_weight"] != "regular" || restored["color"] != "FFFFFF" || restored["panel_color"] != "00000000" {
		t.Fatalf("撤销没有恢复套用前的外观：%v", restored)
	}
}
