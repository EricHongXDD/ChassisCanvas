package theme

import "testing"

func TestForegroundCannotHideBackground(t *testing.T) {
	manifest := Manifest{
		ID: "demo", Name: "Demo", Width: 1024, Height: 600,
		Layers: []Layer{
			{ID: "bg", Type: LayerBackgroundImage, Bounds: Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "lyrics", Type: LayerLyrics, Bounds: Rect{Width: 1, Height: 1}, Opacity: 1},
		},
	}
	if err := manifest.Validate(); err == nil {
		t.Fatal("全屏不透明前景应被拒绝")
	}
	manifest.Layers[1].Bounds = Rect{X: 0.1, Y: 0.75, Width: 0.8, Height: 0.15}
	manifest.Layers[1].Opacity = 0.82
	if err := manifest.Validate(); err != nil {
		t.Fatalf("正常歌词图层被拒绝: %v", err)
	}
}

func TestWaveshareLyricsThemeRequiresCurrentCompilerRevision(t *testing.T) {
	manifest := Manifest{Layers: []Layer{{ID: "lyrics", Type: LayerLyrics}}}
	if !manifest.NeedsWaveshareCompilerUpgrade() {
		t.Fatal("未标版本的微雪歌词主题应要求重新编译")
	}
	manifest.WaveshareCompilerRevision = CurrentWaveshareCompilerRevision
	if manifest.NeedsWaveshareCompilerUpgrade() {
		t.Fatal("当前编译版本不应重复迁移")
	}
	manifest.WaveshareCompilerRevision = 0
	manifest.Layers = []Layer{{ID: "telemetry", Type: LayerTelemetry}}
	if manifest.NeedsWaveshareCompilerUpgrade() {
		t.Fatal("不含歌词的旧主题不需要可见性迁移")
	}
}

func TestManifestRejectsUnknownAndDuplicateEntries(t *testing.T) {
	base := Manifest{
		ID: "demo", Name: "Demo", Width: 320, Height: 180,
		Layers: []Layer{{
			ID: "background", Type: LayerBackgroundImage,
			Bounds: Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	unknown := base
	unknown.Layers = append(unknown.Layers, Layer{
		ID: "unknown", Type: "audio_bra", Bounds: Rect{Width: 0.5, Height: 0.5}, Opacity: 0.5,
	})
	if err := unknown.Validate(); err == nil {
		t.Fatal("应拒绝未知图层类型")
	}
	duplicateLayer := base
	duplicateLayer.Layers = append(duplicateLayer.Layers, Layer{
		ID: "background", Type: LayerLyrics, Bounds: Rect{Width: 0.5, Height: 0.2}, Opacity: 0.5,
	})
	if err := duplicateLayer.Validate(); err == nil {
		t.Fatal("应拒绝重复图层 ID")
	}
	duplicateResource := base
	duplicateResource.Resources = []Resource{
		{ID: "image", LocalPath: `C:\a.png`},
		{ID: "image", LocalPath: `C:\b.png`},
	}
	if err := duplicateResource.Validate(); err == nil {
		t.Fatal("应拒绝重复资源 ID")
	}
}

func TestNormalizeSceneMigratesLegacyBackgroundToIndependentImage(t *testing.T) {
	legacy := Manifest{ID: "legacy", Name: "旧主题", Width: 1024, Height: 600, Layers: []Layer{{
		ID: "background", Type: LayerBackgroundImage, Bounds: Rect{X: .2, Y: .1, Width: .4, Height: .5}, Opacity: .7, Options: map[string]any{"resource_id": "picture"},
	}}}
	normalized := NormalizeScene(legacy)
	if len(normalized.Layers) != 2 || normalized.Layers[0].Type != LayerCanvas || normalized.Layers[1].Type != LayerImage {
		t.Fatalf("旧主题没有迁移为画布和图片：%+v", normalized.Layers)
	}
	if normalized.Layers[1].Bounds != legacy.Layers[0].Bounds || normalized.Layers[1].Opacity != .7 {
		t.Fatalf("迁移改变了图片几何属性：%+v", normalized.Layers[1])
	}
	if err := normalized.Validate(); err != nil {
		t.Fatalf("迁移后的主题无效：%v", err)
	}
}

func TestImageLayerMayScaleBeyondCanvasButMustIntersect(t *testing.T) {
	manifest := Manifest{ID: "scale", Name: "缩放", Width: 100, Height: 100, Layers: []Layer{
		{ID: "canvas", Type: LayerCanvas, Bounds: Rect{Width: 1, Height: 1}, Opacity: 1},
		{ID: "picture", Type: LayerImage, Bounds: Rect{X: -.5, Y: -.5, Width: 2, Height: 2}, Opacity: 1},
	}}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("允许滚轮放大的图片被拒绝：%v", err)
	}
	manifest.Layers[1].Bounds.X = 2
	if err := manifest.Validate(); err == nil {
		t.Fatal("完全移出画布的图片应被拒绝")
	}
}

func TestExtendedWaveshareLayerTypesValidate(t *testing.T) {
	types := []LayerType{LayerText, LayerClock, LayerProgressBar, LayerGauge, LayerGradientGauge, LayerLineChart, LayerShape}
	for _, layerType := range types {
		manifest := Manifest{ID: "extended", Name: "微雪扩展组件", Width: 1024, Height: 600, Layers: []Layer{
			{ID: "canvas", Type: LayerCanvas, Bounds: Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: string(layerType), Type: layerType, Bounds: Rect{X: .1, Y: .1, Width: .3, Height: .2}, Opacity: 1},
		}}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("扩展组件 %s 不应被拒绝：%v", layerType, err)
		}
	}
}

func TestLayerVisibleDefaultsToTrueAndReadsExplicitValue(t *testing.T) {
	layer := Layer{Options: map[string]any{}}
	if !LayerVisible(layer) {
		t.Fatal("旧主题未声明 visible 时应默认显示")
	}
	layer.Options["visible"] = false
	if LayerVisible(layer) {
		t.Fatal("显式隐藏的图层仍被判定为可见")
	}
	layer.Options["visible"] = "true"
	if !LayerVisible(layer) {
		t.Fatal("文本形式的 visible 没有兼容")
	}
}

func TestNormalizeSceneMigratesDeviceMicrophoneToSystemAudio(t *testing.T) {
	manifest := Manifest{Layers: []Layer{{
		ID: "legacy-audio", Type: LayerAudioBars,
		Options: map[string]any{"source": "device_mic"},
	}}}
	normalized := NormalizeScene(manifest)
	for _, layer := range normalized.Layers {
		if layer.ID == "legacy-audio" {
			if got := layer.Options["source"]; got != "system" {
				t.Fatalf("旧屏端麦克风来源没有迁移到主机系统音频: %v", got)
			}
			return
		}
	}
	t.Fatal("场景归一化后音频图层丢失")
}

func TestNormalizeSceneUpgradesLegacyLyricsAndSpectrumVisualDefaults(t *testing.T) {
	manifest := Manifest{Layers: []Layer{
		{ID: "bars", Type: LayerAudioBars},
		{ID: "ring", Type: LayerAudioRing},
		{ID: "lyrics", Type: LayerLyrics},
	}}
	normalized := NormalizeScene(manifest)
	byID := make(map[string]Layer)
	for _, layer := range normalized.Layers {
		byID[layer.ID] = layer
	}
	if got := OptionFloat(byID["bars"].Options, "contrast", 0); got != DefaultAudioContrast {
		t.Fatalf("旧条状频谱没有获得新版峰谷增强: %v", got)
	}
	if got := OptionFloat(byID["bars"].Options, "thickness", 0); got != 3 {
		t.Fatalf("旧条状频谱没有可调细线默认值: %v", got)
	}
	if got := byID["bars"].Options["style"]; got != "neon" {
		t.Fatalf("旧条状频谱没有迁移为参考图的霓虹细柱: %v", got)
	}
	if got := byID["ring"].Options["direction"]; got != "outward" || byID["ring"].Options["style"] != "waveform" {
		t.Fatalf("旧圆环没有迁移为向外波形: %#v", byID["ring"].Options)
	}
	if got := OptionInt(byID["ring"].Options, "thickness", 0); got != 1 {
		t.Fatalf("旧圆环没有迁移为清晰的 1px 细线: %v", got)
	}
	if !OptionBool(byID["bars"].Options, "glow", false) || OptionFloat(byID["bars"].Options, "bass_boost", 0) != DefaultAudioBassBoost {
		t.Fatalf("旧频谱没有获得柔光和低频增强默认值: %#v", byID["bars"].Options)
	}
	lyrics := byID["lyrics"]
	if lyrics.Options["style"] != "player" || !OptionBool(lyrics.Options, "karaoke_highlight", false) ||
		OptionInt(lyrics.Options, "outline_width", 0) != 2 {
		t.Fatalf("旧歌词没有获得播放器样式默认值: %#v", lyrics.Options)
	}
	if got := OptionInt(lyrics.Options, "font_size", 0); got != DefaultLyricsFontSize {
		t.Fatalf("旧歌词当前句字号=%d，期望 %d", got, DefaultLyricsFontSize)
	}
	if got := OptionInt(lyrics.Options, "next_font_size", 0); got != DefaultLyricsNextFontSize {
		t.Fatalf("旧歌词下一句字号=%d，期望 %d", got, DefaultLyricsNextFontSize)
	}
}

func TestNormalizeScenePreservesExplicitLyricsFontSizes(t *testing.T) {
	manifest := Manifest{Layers: []Layer{{
		ID: "lyrics", Type: LayerLyrics,
		Options: map[string]any{"font_size": 46, "next_font_size": 27},
	}}}
	normalized := NormalizeScene(manifest)
	var lyrics Layer
	for _, layer := range normalized.Layers {
		if layer.Type == LayerLyrics {
			lyrics = layer
			break
		}
	}
	if lyrics.Type != LayerLyrics {
		t.Fatal("标准化后未找到歌词图层")
	}
	if got := OptionInt(lyrics.Options, "font_size", 0); got != 46 {
		t.Fatalf("显式当前句字号被默认值覆盖: %d", got)
	}
	if got := OptionInt(lyrics.Options, "next_font_size", 0); got != 27 {
		t.Fatalf("显式下一句字号被默认值覆盖: %d", got)
	}
}

func TestNormalizeScenePreservesExplicitSpectrumStyles(t *testing.T) {
	manifest := Manifest{Layers: []Layer{
		{ID: "bars", Type: LayerAudioBars, Options: map[string]any{
			"style": "blocks", "direction": "center", "count": 24, "thickness": 7, "glow": false,
		}},
		{ID: "ring", Type: LayerAudioRing, Options: map[string]any{
			"style": "dashed", "direction": "inward", "count": 36, "inner_radius": .44, "thickness": 5,
		}},
	}}
	normalized := NormalizeScene(manifest)
	byID := make(map[string]Layer)
	for _, layer := range normalized.Layers {
		byID[layer.ID] = layer
	}
	bars := byID["bars"]
	if bars.Options["style"] != "blocks" || bars.Options["direction"] != "center" ||
		OptionInt(bars.Options, "count", 0) != 24 || OptionInt(bars.Options, "thickness", 0) != 7 || OptionBool(bars.Options, "glow", true) {
		t.Fatalf("显式条状频谱样式被默认迁移覆盖: %#v", bars.Options)
	}
	ring := byID["ring"]
	if ring.Options["style"] != "dashed" || ring.Options["direction"] != "inward" ||
		OptionInt(ring.Options, "count", 0) != 36 || OptionFloat(ring.Options, "inner_radius", 0) != .44 || OptionInt(ring.Options, "thickness", 0) != 5 {
		t.Fatalf("显式环状频谱样式被默认迁移覆盖: %#v", ring.Options)
	}
}
