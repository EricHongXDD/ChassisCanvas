package workbench

import (
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	caseTheme "casescreen/internal/theme"
)

func TestLyricPreviewStylesHaveIndependentVisualModels(t *testing.T) {
	seen := make(map[lyricPreviewSpec]string)
	styles := []string{"player", "karaoke", "classic", "minimal"}
	models := make(map[string]lyricPreviewSpec, len(styles))
	for _, style := range styles {
		layer := caseTheme.Layer{Type: caseTheme.LayerLyrics, Options: map[string]any{
			"style": style, "align": "center", "karaoke_highlight": true, "show_progress": true,
		}}
		preview := buildLyricPreviewSpec(layer, image.Pt(1020, 120), 8, 3)
		if previous, duplicate := seen[preview]; duplicate {
			t.Fatalf("%s 与 %s 仍然共用相同歌词预览模型：%+v", style, previous, preview)
		}
		seen[preview] = style
		models[style] = preview
	}
	if models["player"].highlightStyle != "text" || models["player"].progressStyle != "bar" {
		t.Fatalf("播放器预览没有流光文字和连续进度条：%+v", models["player"])
	}
	if models["karaoke"].badge == "" || models["karaoke"].progressStyle != "segments" {
		t.Fatalf("卡拉 OK 预览没有演唱状态和分段进度：%+v", models["karaoke"])
	}
	if !models["classic"].divider || models["classic"].showHighlight {
		t.Fatalf("经典双行预览应使用分隔线并与编译器一致地关闭流光：%+v", models["classic"])
	}
	if models["minimal"].showNext || models["minimal"].showHighlight || models["minimal"].showProgress {
		t.Fatalf("极简预览不应残留下一句、流光或进度轨：%+v", models["minimal"])
	}
}

func TestLyricPreviewUsesSharedDefaultFontSizes(t *testing.T) {
	model := &gioEditor{manifest: caseTheme.Manifest{Width: 1024, Height: 600}}
	layer := caseTheme.Layer{Type: caseTheme.LayerLyrics, Bounds: caseTheme.Rect{Width: 1, Height: .2}}
	current, next := model.lyricPreviewFontSizes(layer, image.Pt(1024, 120))
	if got := int(current); got != caseTheme.DefaultLyricsFontSize {
		t.Fatalf("Gio 当前句默认字号=%d，期望 %d", got, caseTheme.DefaultLyricsFontSize)
	}
	if got := int(next); got != caseTheme.DefaultLyricsNextFontSize {
		t.Fatalf("Gio 下一句默认字号=%d，期望 %d", got, caseTheme.DefaultLyricsNextFontSize)
	}
}

func TestWaveshareLyricPreviewDegradesToDeliverableWholeLineHighlight(t *testing.T) {
	layer := caseTheme.Layer{Type: caseTheme.LayerLyrics, Options: map[string]any{
		"style": "karaoke", "karaoke_highlight": true, "show_progress": true,
	}}
	spec := buildLyricPreviewSpec(layer, image.Pt(720, 180), 8, 4)
	waveshare := &gioEditor{target: "waveshare-7", targetOptions: []targetOption{{id: "waveshare-7", waveshare: true}}}
	adapted, isWaveshare, wholeLine := waveshare.adaptLyricPreviewToTarget(spec)
	if !isWaveshare || !wholeLine || adapted.showHighlight || adapted.badge != "" || adapted.progressStyle != "segments" {
		t.Fatalf("微雪预览仍承诺逐字裁切或丢失可兑现进度轨：%+v waveshare=%v whole=%v", adapted, isWaveshare, wholeLine)
	}
	host := &gioEditor{target: "cooler", targetOptions: []targetOption{{id: "cooler", waveshare: false}}}
	hostSpec, isWaveshare, wholeLine := host.adaptLyricPreviewToTarget(spec)
	if isWaveshare || wholeLine || !hostSpec.showHighlight || hostSpec.badge == "" {
		t.Fatalf("主机合成预览被错误降级：%+v waveshare=%v whole=%v", hostSpec, isWaveshare, wholeLine)
	}
}

func TestLyricPreviewReadsTransitionAndShadowProperties(t *testing.T) {
	short := caseTheme.Layer{Options: map[string]any{"transition_fraction": .01}}
	long := caseTheme.Layer{Options: map[string]any{"transition_fraction": .35}}
	if lyricPreviewProgressRatio(short) >= lyricPreviewProgressRatio(long) {
		t.Fatalf("淡入比例没有改变歌词模拟进度：short=%v long=%v", lyricPreviewProgressRatio(short), lyricPreviewProgressRatio(long))
	}
	next := color.NRGBA{R: 10, G: 20, B: 30, A: 200}
	smooth := lyricPreviewTransitionColor(caseTheme.Layer{Options: map[string]any{"smooth_transition": true}}, next)
	direct := lyricPreviewTransitionColor(caseTheme.Layer{Options: map[string]any{"smooth_transition": false}}, next)
	if smooth.A >= direct.A || direct != next {
		t.Fatalf("平滑淡入没有反映到下一句透明层次：smooth=%+v direct=%+v", smooth, direct)
	}
	if got := lyricPreviewShadowBlur(caseTheme.Layer{Options: map[string]any{"shadow_blur": 2.0}}); got != 2 {
		t.Fatalf("阴影柔化属性未进入预览：%d", got)
	}
}

func TestPreviewFontChoicesMapToRealSingleFamilyNames(t *testing.T) {
	seen := make(map[string]string)
	for _, choice := range waveshareFontChoices {
		face := string(previewTypeface(choice.value))
		if face == "" || strings.Contains(face, ",") {
			t.Fatalf("%s 没有映射到 Gio 可识别的单一字体族：%q", choice.value, face)
		}
		if previous, duplicate := seen[face]; duplicate {
			t.Fatalf("%s 与 %s 的字体预览仍然完全相同：%q", choice.value, previous, face)
		}
		seen[face] = choice.value
	}
}

func TestEverySelectedComponentOptionRequestsPreviewFrame(t *testing.T) {
	layerTypes := []caseTheme.LayerType{
		caseTheme.LayerTelemetry, caseTheme.LayerAudioBars, caseTheme.LayerAudioRing, caseTheme.LayerLyrics,
		caseTheme.LayerText, caseTheme.LayerClock, caseTheme.LayerProgressBar, caseTheme.LayerGauge,
		caseTheme.LayerGradientGauge, caseTheme.LayerLineChart, caseTheme.LayerShape,
	}
	for _, layerType := range layerTypes {
		model := &gioEditor{selected: 0, manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{{ID: string(layerType), Type: layerType, Options: map[string]any{}}}}}
		model.setSelectedOption("preview_probe", string(layerType))
		if model.previewRevision != 1 {
			t.Fatalf("%s 属性修改没有请求下一帧：revision=%d", layerType, model.previewRevision)
		}
		if got := model.manifest.Layers[0].Options["preview_probe"]; got != string(layerType) {
			t.Fatalf("%s 属性没有写回主题模型：%v", layerType, got)
		}
	}
}

func TestPreviewRangePropertiesChangeProgressRatio(t *testing.T) {
	base := caseTheme.Layer{Options: map[string]any{"min_value": 0.0, "max_value": 100.0}}
	zoomed := caseTheme.Layer{Options: map[string]any{"min_value": 40.0, "max_value": 50.0}}
	if got := previewMetricRangeRatio(base, "Cpu_Freq", .42); got != .42 {
		t.Fatalf("默认范围比例错误：%v", got)
	}
	if got := previewMetricRangeRatio(zoomed, "Cpu_Freq", .42); got < .19 || got > .21 {
		t.Fatalf("自定义范围没有改变预览进度：%v", got)
	}
	invalid := caseTheme.Layer{Options: map[string]any{"min_value": 50.0, "max_value": 50.0}}
	if got := previewMetricRangeRatio(invalid, "Cpu_Freq", .42); got != 0 {
		t.Fatalf("无效范围应安全回落为空进度：%v", got)
	}
}

func TestAudioMainColorAndCenterStyleReachPreviewModel(t *testing.T) {
	for _, layerType := range []caseTheme.LayerType{caseTheme.LayerAudioBars, caseTheme.LayerAudioRing} {
		model := &gioEditor{selected: 0, optionEditors: map[string]*textField{}, manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{{
			ID: "audio", Type: layerType, Options: map[string]any{"color": "FFFFFF", "gradient_start": "4CE0FF"},
		}}}}
		model.setSelectedOption("color", "FF5C68")
		if got := model.manifest.Layers[0].Options["gradient_start"]; got != "FF5C68" {
			t.Fatalf("%s 主颜色没有同步到设备实际使用的渐变起点：%v", layerType, got)
		}
	}
	center := caseTheme.Layer{Type: caseTheme.LayerAudioBars, Options: map[string]any{"style": "center", "direction": "bottom"}}
	if got := waveshareBarsPreviewDirection(center); got != "center" {
		t.Fatalf("中心扩散样式没有覆盖普通方向：%q", got)
	}
	horizontal := caseTheme.Layer{Type: caseTheme.LayerAudioBars, Options: map[string]any{"style": "blocks", "direction": "horizontal"}}
	if got := waveshareBarsPreviewDirection(horizontal); got != "horizontal" {
		t.Fatalf("水平块状频谱方向丢失：%q", got)
	}
}

func TestBoundTextPreviewUsesTelemetrySample(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 34, 56, 0, time.Local)
	staticLayer := caseTheme.Layer{Options: map[string]any{"text": "固定标题"}}
	if got := previewTextComponentValue(staticLayer, now); got != "固定标题" {
		t.Fatalf("固定文本预览被意外替换：%q", got)
	}
	dynamicLayer := caseTheme.Layer{Options: map[string]any{"text": "自定义文本", "data_key": "GPU_Temp"}}
	if got := previewTextComponentValue(dynamicLayer, now); got != "45℃" {
		t.Fatalf("动态文本绑定没有显示遥测示例值：%q", got)
	}
}
