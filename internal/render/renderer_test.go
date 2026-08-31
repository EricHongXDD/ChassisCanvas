package render

import (
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"casescreen/internal/audio"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
)

func TestHostTelemetryUsesSelectedFontFamilyAndAllFourWeights(t *testing.T) {
	regularFont, err := opentype.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	monoFont, err := opentype.Parse(gomono.TTF)
	if err != nil {
		t.Fatal(err)
	}
	renderFrame := func(family, weight string) [32]byte {
		manifest := theme.Manifest{Width: 320, Height: 120, Layers: []theme.Layer{{
			ID: "telemetry", Type: theme.LayerTelemetry,
			Bounds: theme.Rect{X: .05, Y: .08, Width: .9, Height: .8}, Opacity: 1,
			Options: map[string]any{
				"fields": []any{"Cpu_Freq"}, "font_size": 38.0, "font_family": family,
				"font_weight": weight, "panel_color": "00000000", "color": "FFFFFFFF",
			},
		}}}
		renderer := &Renderer{
			manifest: manifest,
			fonts: map[string]*opentype.Font{
				"": regularFont, "logical-regular": regularFont, "logical-mono": monoFont,
			},
			faces: make(map[faceKey]font.Face), audio: make(map[string]audioVisualRuntime), charts: make(map[string]chartVisualRuntime),
		}
		frame, renderErr := renderer.Render(320, 120, State{Telemetry: telemetry.Snapshot{CapturedAt: time.Unix(1, 0), CPUPercent: 42}})
		if closeErr := renderer.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return sha256.Sum256(frame.Pix)
	}
	regular := renderFrame("logical-regular", "regular")
	mono := renderFrame("logical-mono", "regular")
	if regular == mono {
		t.Fatal("遥测 font_family 没有改变主机合成字体")
	}
	for _, family := range []string{"logical-regular", "logical-mono"} {
		seen := make(map[[32]byte]string)
		for _, weight := range []string{"light", "regular", "medium", "bold"} {
			digest := renderFrame(family, weight)
			if previous, exists := seen[digest]; exists {
				t.Fatalf("主机合成字重 %s/%s 与 %s 像素完全相同", family, weight, previous)
			}
			seen[digest] = weight
		}
	}
}

func TestSystemFontMatrixContainsEveryLogicalFamilyAndWeight(t *testing.T) {
	directory := t.TempDir()
	for name, data := range map[string][]byte{
		"msyhl.ttc": goregular.TTF, "msyh.ttc": goregular.TTF, "msyhbd.ttc": gobold.TTF,
		"simsun.ttc": gomono.TTF, "simsunb.ttf": gomono.TTF,
		"consola.ttf": gomono.TTF, "consolab.ttf": gobold.TTF,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fonts := loadSystemFontsFromDirectory(directory)
	for _, family := range []string{"pangmen", "source-han", "microsoft-yahei", "simhei", "digital", "monospace", "rounded", "serif"} {
		for _, weight := range []string{"light", "regular", "medium", "bold"} {
			if fonts[systemFontResourceID(family, weight)] == nil {
				t.Errorf("缺少 Windows 字脸映射 %s/%s", family, weight)
			}
		}
	}
}

func TestHostLyricsStylesRenderDistinctTruthfulLayouts(t *testing.T) {
	hashes := make(map[[32]byte]string)
	for _, style := range []string{theme.LyricsStylePlayer, theme.LyricsStyleKaraoke, theme.LyricsStyleClassic, theme.LyricsStyleMinimal} {
		manifest := theme.Manifest{ID: "lyrics-" + style, Name: style, Width: 480, Height: 180, Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "transparent"}},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .04, Y: .08, Width: .92, Height: .84}, Opacity: 1,
				Options: map[string]any{
					"style": style, "font_size": 32.0, "next_font_size": 18.0,
					"panel_color": "07111FE0", "panel_border_color": "55E5FF50", "corner_radius": 10,
					"current_color": "F7FCFF", "next_color": "B8C8DAA4", "accent_color": "55E5FF",
					"outline_width": 0, "shadow": false, "glow_radius": 0,
					"karaoke_highlight": true, "show_progress": true,
				},
			},
		}}
		renderer, err := New(manifest)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := renderer.Render(480, 180, State{Lyrics: LyricState{
			Current: "FLOWING LIGHT", Next: "NEXT VERSE", Progress: .58, PositionKnown: true, Playing: true,
		}})
		_ = renderer.Close()
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(frame.Pix)
		if previous, exists := hashes[digest]; exists {
			t.Fatalf("歌词样式 %s 与 %s 实际合成画面相同", style, previous)
		}
		hashes[digest] = style
	}
}

func TestHostLyricsDisabledOrMinimalProgressLeavesNoBottomDecoration(t *testing.T) {
	render := func(style string, showProgress bool) *image.RGBA {
		manifest := theme.Manifest{ID: "progress-" + style, Name: style, Width: 320, Height: 120, Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "transparent"}},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .05, Y: .05, Width: .90, Height: .90}, Opacity: 1,
				Options: map[string]any{
					"style": style, "font_size": 28.0, "next_font_size": 16.0,
					"panel_color": "00000000", "panel_border_color": "00000000",
					"outline_width": 0, "shadow": false, "glow_radius": 0,
					"karaoke_highlight": false, "show_progress": showProgress, "progress_thickness": 4,
				},
			},
		}}
		renderer, err := New(manifest)
		if err != nil {
			t.Fatal(err)
		}
		defer renderer.Close()
		frame, err := renderer.Render(320, 120, State{Lyrics: LyricState{Current: "CURRENT", Next: "NEXT", Progress: .5, PositionKnown: true}})
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	bottomPixels := func(frame *image.RGBA) int {
		count := 0
		for y := 106; y < 115; y++ {
			for x := 0; x < 320; x++ {
				if frame.RGBAAt(x, y).A != 0 {
					count++
				}
			}
		}
		return count
	}
	if count := bottomPixels(render(theme.LyricsStylePlayer, false)); count != 0 {
		t.Fatalf("show_progress=false 仍绘制底部进度装饰: %d", count)
	}
	if count := bottomPixels(render(theme.LyricsStyleMinimal, true)); count != 0 {
		t.Fatalf("minimal 继承旧 show_progress=true 后仍绘制进度装饰: %d", count)
	}
	if count := bottomPixels(render(theme.LyricsStylePlayer, true)); count == 0 {
		t.Fatal("启用进度轨后没有绘制真实进度")
	}
}

func TestRendererComposesTransparentLayers(t *testing.T) {
	directory := t.TempDir()
	backgroundPath := filepath.Join(directory, "background.png")
	background := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			background.SetRGBA(x, y, color.RGBA{R: 18, G: uint8(35 + x/8), B: uint8(70 + y/4), A: 255})
		}
	}
	file, err := os.Create(backgroundPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, background); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	resource, err := theme.InspectResource("background", backgroundPath, "")
	if err != nil {
		t.Fatal(err)
	}
	manifest := theme.Manifest{
		ID: "layer-test", Name: "图层测试", Width: 320, Height: 180,
		Resources: []theme.Resource{resource},
		Layers: []theme.Layer{
			{ID: "bg", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: 0, Options: map[string]any{"resource_id": "background"}},
			{ID: "telemetry", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: 0.03, Y: 0.05, Width: 0.32, Height: 0.28}, Opacity: 0.86, ZIndex: 1},
			{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: 0.05, Y: 0.72, Width: 0.42, Height: 0.20}, Opacity: 0.85, ZIndex: 2},
			{ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: 0.67, Y: 0.08, Width: 0.28, Height: 0.50}, Opacity: 0.84, ZIndex: 2},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: 0.26, Y: 0.47, Width: 0.48, Height: 0.19}, Opacity: 0.80, ZIndex: 3},
		},
	}
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	bars := make([]float64, 24)
	for index := range bars {
		bars[index] = 0.15 + 0.85*math.Abs(math.Sin(float64(index)*0.53))
	}
	canvas, err := renderer.Render(320, 180, State{
		Telemetry:        telemetry.Snapshot{CapturedAt: time.Unix(1, 0), CPUPercent: 42, MemoryPercent: 64},
		TelemetryEnabled: true,
		Spectrum:         audio.SpectrumFrame{Bars: bars, Level: 0.6},
		Lyrics:           LyricState{Current: "此刻正在播放", Next: "下一句歌词", Progress: 0.42},
	})
	if err != nil {
		t.Fatal(err)
	}
	if canvas.Bounds() != image.Rect(0, 0, 320, 180) {
		t.Fatalf("画布大小错误: %v", canvas.Bounds())
	}
	backgroundPixel := color.RGBAModel.Convert(background.At(160, 20)).(color.RGBA)
	canvasPixel := canvas.RGBAAt(160, 20)
	if canvasPixel != backgroundPixel {
		t.Fatalf("图层外背景被改变: got=%v want=%v", canvasPixel, backgroundPixel)
	}
	changed := 0
	for y := 130; y < 166; y++ {
		for x := 18; x < 148; x++ {
			if canvas.RGBAAt(x, y) != background.RGBAAt(x, y) {
				changed++
			}
		}
	}
	if changed < 100 {
		t.Fatalf("条状频谱区域变化不足: %d", changed)
	}
	withoutTelemetry, err := renderer.Render(320, 180, State{
		Spectrum: audio.SpectrumFrame{Bars: bars, Level: 0.6},
		Lyrics:   LyricState{Current: "此刻正在播放", Next: "下一句歌词", Progress: 0.42},
	})
	if err != nil {
		t.Fatal(err)
	}
	telemetryChanges := 0
	for y := 9; y < 50; y++ {
		for x := 10; x < 112; x++ {
			if withoutTelemetry.RGBAAt(x, y) != background.RGBAAt(x, y) {
				telemetryChanges++
			}
		}
	}
	if telemetryChanges < 100 {
		t.Fatalf("遥测关闭时未绘制可见占位组件: %d", telemetryChanges)
	}
}

func TestSpectrumRenderersKeepOnlyLowStaticBaselineForSilence(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 160, 100))
	barsLayer := theme.Layer{Type: theme.LayerAudioBars, Opacity: 1, Options: map[string]any{"count": 18}}
	ringLayer := theme.Layer{Type: theme.LayerAudioRing, Opacity: 1}
	drawBars(canvas, image.Rect(0, 0, 80, 100), barsLayer, nil)
	drawRing(canvas, image.Rect(80, 0, 160, 100), ringLayer, make([]float64, 24))
	barPixels, ringPixels := 0, 0
	for y := 0; y < canvas.Bounds().Dy(); y++ {
		for x := 0; x < canvas.Bounds().Dx(); x++ {
			if canvas.RGBAAt(x, y).A == 0 {
				continue
			}
			if x < 80 {
				barPixels++
				if y < 90 {
					t.Fatalf("静音柱状频谱伪造了高波峰: x=%d y=%d", x, y)
				}
				continue
			}
			ringPixels++
			distance := math.Hypot(float64(x-120), float64(y-50))
			if distance < 15 || distance > 28 {
				t.Fatalf("静音环形频谱不是窄幅基线: x=%d y=%d r=%.2f", x, y, distance)
			}
		}
	}
	if barPixels == 0 || ringPixels == 0 {
		t.Fatalf("静音时频谱完全消失: bars=%d ring=%d", barPixels, ringPixels)
	}

	canvas = image.NewRGBA(image.Rect(0, 0, 160, 100))
	drawBars(canvas, image.Rect(0, 0, 80, 100), barsLayer, []float64{0, .5, 1})
	drawRing(canvas, image.Rect(80, 0, 160, 100), ringLayer, []float64{0, .5, 1})
	visible := false
	for offset := 3; offset < len(canvas.Pix); offset += 4 {
		if canvas.Pix[offset] != 0 {
			visible = true
			break
		}
	}
	if !visible {
		t.Fatal("有声频带没有绘制")
	}
}

func TestVisualBarsRasterHasNoFullHeightTrackAndUsesFourPercentIdleBaseline(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 120, 100))
	layer := theme.Layer{Type: theme.LayerAudioBars, Opacity: 1, Options: map[string]any{
		"gap": 4, "thickness": 4, "bar_shape": "square", "glow": false,
		"gradient_start": "35CFFF", "gradient_end": "35CFFF",
	}}
	drawVisualBarsPrepared(canvas, canvas.Bounds(), layer, []float64{0, .5, 0, .9}, nil)
	cell, gap := packedVisualLayout(canvas.Bounds().Dx(), 4, 4)
	wantHeights := []int{4, 50, 4, 90}
	for index, want := range wantHeights {
		x := index*(cell+gap) + (cell-4)/2 + 1
		minimumY, maximumY := canvas.Bounds().Dy(), -1
		for y := 0; y < canvas.Bounds().Dy(); y++ {
			if canvas.RGBAAt(x, y).A == 0 {
				continue
			}
			minimumY, maximumY = min(minimumY, y), max(maximumY, y)
		}
		if maximumY < 0 {
			t.Fatalf("第 %d 个频带完全消失", index)
		}
		height := maximumY - minimumY + 1
		if height != want {
			t.Fatalf("第 %d 个频带高度错误: got=%d want=%d y=[%d,%d]", index, height, want, minimumY, maximumY)
		}
		if minimumY == 0 {
			t.Fatalf("第 %d 个频带出现了整高背景格", index)
		}
	}
}

func TestHostBarsPaintOnlyLowEnergyHeight(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 120, 100))
	layer := theme.Layer{Type: theme.LayerAudioBars, Opacity: 1, Options: map[string]any{
		"count": 6, "gap": 4, "thickness": 4, "bar_shape": "square",
		"noise_gate": 0.0, "gain": 1.0, "sensitivity": 1.0, "min_amplitude": 0.0, "max_amplitude": 1.0,
		"smoothing": 0.0, "contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	drawBars(canvas, canvas.Bounds(), layer, []float64{.1, .1, .1, .1, .1, .1})
	visible := 0
	for y := 0; y < canvas.Bounds().Dy(); y++ {
		for x := 0; x < canvas.Bounds().Dx(); x++ {
			if canvas.RGBAAt(x, y).A == 0 {
				continue
			}
			visible++
			if y < 89 {
				t.Fatalf("低能量频带在真实高度以上仍绘制像素: (%d,%d)", x, y)
			}
		}
	}
	if visible == 0 {
		t.Fatal("低能量频带没有绘制底部真实高度")
	}
}

func TestHostRingDefaultCountMatchesSharedThemeDefault(t *testing.T) {
	layer := theme.Layer{Type: theme.LayerAudioRing}
	if got := visualAudioBandCount(layer, image.Rect(0, 0, 320, 180)); got != theme.DefaultAudioRingCount {
		t.Fatalf("主机环状频谱缺省段数漂移: got=%d want=%d", got, theme.DefaultAudioRingCount)
	}
}

func TestHostFourWayBarsCountMatchesResidentThemeRules(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options map[string]any
		want    int
	}{
		{name: "缺省爆发", options: map[string]any{"style": "burst"}, want: theme.DefaultAudioRingCount},
		{name: "四向数量下限", options: map[string]any{"direction": "fourway", "count": 6}, want: 12},
		{name: "显式数量", options: map[string]any{"style": "burst", "count": 24}, want: 24},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			layer := theme.Layer{Type: theme.LayerAudioBars, Options: testCase.options}
			if got := visualAudioBandCount(layer, image.Rect(0, 0, 1, 1)); got != testCase.want {
				t.Fatalf("主机四向频谱段数与驻留主题不一致: got=%d want=%d", got, testCase.want)
			}
		})
	}
}

func TestVisualBarsHonorThinThicknessAndKeepLargePeakValleyGap(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 180, 100))
	layer := theme.Layer{Type: theme.LayerAudioBars, Opacity: 1, Options: map[string]any{
		"count": 6, "gap": 8, "thickness": 2, "direction": "bottom", "bar_shape": "square",
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.6, "separation": .8, "gradient_start": "40E5FF", "gradient_end": "8B5CF6",
	}}
	drawBars(canvas, canvas.Bounds(), layer, []float64{.12, .18, .92, .16, .68, .10})
	// 每个频带都位于较宽槽位中央，但真正发光柱宽必须保持 2px。
	maximumRun := 0
	for y := 0; y < canvas.Bounds().Dy(); y++ {
		run := 0
		for x := 0; x < canvas.Bounds().Dx(); x++ {
			if canvas.RGBAAt(x, y).A > 0 {
				run++
				maximumRun = maxInt(maximumRun, run)
			} else {
				run = 0
			}
		}
	}
	if maximumRun > 2 {
		t.Fatalf("细柱设置没有生效，连续像素宽度=%d", maximumRun)
	}
	columnHeight := func(centerX int) int {
		visible := 0
		for y := 0; y < canvas.Bounds().Dy(); y++ {
			if canvas.RGBAAt(centerX, y).A > 0 {
				visible++
			}
		}
		return visible
	}
	// 槽位宽约 23px，第三柱峰值必须明显高于相邻第二柱。
	if peak, valley := columnHeight(72), columnHeight(41); peak-valley < 45 {
		t.Fatalf("峰谷视觉落差不足: peak=%d valley=%d", peak, valley)
	}
}

func TestVisualWaveformConnectsActualBandsWithoutFillingArea(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 240, 100))
	layer := theme.Layer{Type: theme.LayerAudioBars, Opacity: 1, Options: map[string]any{
		"style": "waveform", "direction": "bottom", "count": 8, "thickness": 2, "glow": false,
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	drawBars(canvas, canvas.Bounds(), layer, []float64{.1, .9, .2, .75, .15, .55, .25, .8})
	visible := 0
	topQuarter := false
	for y := 0; y < canvas.Bounds().Dy(); y++ {
		for x := 0; x < canvas.Bounds().Dx(); x++ {
			if canvas.RGBAAt(x, y).A == 0 {
				continue
			}
			visible++
			if y < 25 {
				topQuarter = true
			}
		}
	}
	if visible < 250 || visible > 1800 || !topQuarter {
		t.Fatalf("连续波形没有形成细窄且有高低落差的谱线: pixels=%d top=%v", visible, topQuarter)
	}

	silent := image.NewRGBA(canvas.Bounds())
	drawBars(silent, silent.Bounds(), layer, make([]float64, 8))
	silentPixels := 0
	for y := 0; y < silent.Bounds().Dy(); y++ {
		for x := 0; x < silent.Bounds().Dx(); x++ {
			if silent.RGBAAt(x, y).A == 0 {
				continue
			}
			silentPixels++
			if y < 80 {
				t.Fatalf("静音基线不应伪造正弦波峰: x=%d y=%d", x, y)
			}
		}
	}
	if silentPixels == 0 {
		t.Fatal("连续波形在静音时没有保留低幅静态基线")
	}
}

func TestVisualRingWaveformIsClosedIrregularAndKeepsCenterClear(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 220, 220))
	layer := theme.Layer{Type: theme.LayerAudioRing, Opacity: 1, Options: map[string]any{
		"style": "waveform", "direction": "outward", "count": 12, "inner_radius": .62, "thickness": 2, "glow": false,
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	drawRing(canvas, canvas.Bounds(), layer, []float64{.15, .92, .24, .68, .12, .83, .31, .74, .18, .61, .27, .88})
	quadrants := [4]bool{}
	for y := 0; y < 220; y++ {
		for x := 0; x < 220; x++ {
			if canvas.RGBAAt(x, y).A == 0 {
				continue
			}
			quadrant := 0
			if x >= 110 {
				quadrant++
			}
			if y >= 110 {
				quadrant += 2
			}
			quadrants[quadrant] = true
		}
	}
	for index, visible := range quadrants {
		if !visible {
			t.Fatalf("闭合圆环波形缺少第 %d 象限", index+1)
		}
	}
	if canvas.RGBAAt(110, 110).A != 0 {
		t.Fatal("圆环波形错误填充了中心区域")
	}
}

func TestVisualSpectrumGlowIsOptionalAndDoesNotChangeBandData(t *testing.T) {
	draw := func(glow bool) int {
		canvas := image.NewRGBA(image.Rect(0, 0, 200, 100))
		layer := theme.Layer{Type: theme.LayerAudioBars, Opacity: 1, Options: map[string]any{
			"style": "neon", "direction": "bottom", "count": 8, "thickness": 2, "glow": glow, "glow_radius": 8,
			"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
			"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
		}}
		drawBars(canvas, canvas.Bounds(), layer, []float64{.2, .9, .3, .7, .15, .8, .25, .6})
		visible := 0
		for offset := 3; offset < len(canvas.Pix); offset += 4 {
			if canvas.Pix[offset] != 0 {
				visible++
			}
		}
		return visible
	}
	withoutGlow, withGlow := draw(false), draw(true)
	if withoutGlow == 0 || withGlow <= withoutGlow*2 {
		t.Fatalf("柔光开关没有显著改变轮廓范围: off=%d on=%d", withoutGlow, withGlow)
	}
}

func TestVisualBarsFourwayBurstReachesEveryQuadrant(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 180, 180))
	layer := theme.Layer{Type: theme.LayerAudioBars, Opacity: 1, Options: map[string]any{
		"style": "burst", "direction": "fourway", "count": 24, "thickness": 2, "inner_radius": .1,
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0, "contrast": 1.0, "separation": 0.0,
	}}
	drawBars(canvas, canvas.Bounds(), layer, []float64{.25, .9, .35, .75, .22, .82, .4, .95})
	quadrants := [4]bool{}
	for y := 0; y < 180; y++ {
		for x := 0; x < 180; x++ {
			if canvas.RGBAAt(x, y).A == 0 || (x >= 75 && x <= 105 && y >= 75 && y <= 105) {
				continue
			}
			index := 0
			if x >= 90 {
				index++
			}
			if y >= 90 {
				index += 2
			}
			quadrants[index] = true
		}
	}
	for index, visible := range quadrants {
		if !visible {
			t.Fatalf("四向爆发在第 %d 象限没有可见频带", index+1)
		}
	}
}

func TestHostRendererPeakHoldPersistsOnSoundAndClearsOnSilence(t *testing.T) {
	layer := theme.Layer{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .05, Y: .05, Width: .9, Height: .9}, Opacity: 1, Options: map[string]any{
		"count": 6, "gap": 8, "thickness": 4, "bar_shape": "square", "peak_hold": true, "peak_hold_ms": 1000.0,
		"peak_color": "FFEF55", "gradient_start": "35CFFF", "gradient_end": "35CFFF",
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0, "contrast": 1.0, "separation": 0.0,
	}}
	renderer, err := New(theme.Manifest{ID: "peak-host", Name: "主机峰值", Width: 180, Height: 100, Layers: []theme.Layer{
		{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "transparent"}},
		layer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	start := time.Unix(10, 0)
	if _, err := renderer.Render(180, 100, State{Spectrum: audio.SpectrumFrame{CapturedAt: start, Bars: []float64{.9, .9, .9, .9, .9, .9}}}); err != nil {
		t.Fatal(err)
	}
	second, err := renderer.Render(180, 100, State{Spectrum: audio.SpectrumFrame{CapturedAt: start.Add(100 * time.Millisecond), Bars: []float64{.2, .2, .2, .2, .2, .2}}})
	if err != nil {
		t.Fatal(err)
	}
	peakPixels := 0
	for y := 5; y < 20; y++ {
		for x := 0; x < 180; x++ {
			pixel := second.RGBAAt(x, y)
			if pixel.R > 180 && pixel.G > 150 && pixel.B < 160 && pixel.A > 0 {
				peakPixels++
			}
		}
	}
	if peakPixels == 0 {
		t.Fatal("主机合成帧没有保留可见的峰值标记")
	}
	silent, err := renderer.Render(180, 100, State{Spectrum: audio.SpectrumFrame{CapturedAt: start.Add(200 * time.Millisecond), Bars: make([]float64, 6)}})
	if err != nil {
		t.Fatal(err)
	}
	baselinePixels, stalePeakPixels := 0, 0
	baselineMinY := silent.Bounds().Dy()
	for y := 0; y < silent.Bounds().Dy(); y++ {
		for x := 0; x < silent.Bounds().Dx(); x++ {
			pixel := silent.RGBAAt(x, y)
			if pixel.A == 0 {
				continue
			}
			baselinePixels++
			baselineMinY = min(baselineMinY, y)
			if pixel.R > 180 && pixel.G > 150 && pixel.B < 160 {
				stalePeakPixels++
			}
			if y < 90 {
				t.Fatalf("静音帧仍残留高幅频谱或峰值: x=%d y=%d pixel=%v", x, y, pixel)
			}
		}
	}
	if baselinePixels == 0 || stalePeakPixels != 0 {
		t.Fatalf("全零输入应清除峰值并保留静态基线: baseline=%d stalePeak=%d", baselinePixels, stalePeakPixels)
	}
	if baselineMinY < 90 || baselineMinY > 92 {
		t.Fatalf("静音基线不是约 4%% 的短柱: minY=%d", baselineMinY)
	}
}

func TestVisualRingSilenceKeepsStaticBaselineWithoutOldPeaks(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 220, 220))
	layer := theme.Layer{Type: theme.LayerAudioRing, Opacity: 1, Options: map[string]any{
		"style": "waveform", "direction": "outward", "count": 24, "inner_radius": .62, "thickness": 1, "glow": false,
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	drawRing(canvas, canvas.Bounds(), layer, make([]float64, 24))
	quadrants := [4]bool{}
	visible := 0
	for y := 0; y < canvas.Bounds().Dy(); y++ {
		for x := 0; x < canvas.Bounds().Dx(); x++ {
			if canvas.RGBAAt(x, y).A == 0 {
				continue
			}
			visible++
			quadrant := 0
			if x >= 110 {
				quadrant++
			}
			if y >= 110 {
				quadrant += 2
			}
			quadrants[quadrant] = true
			if x >= 85 && x <= 135 && y >= 85 && y <= 135 {
				t.Fatalf("静态圆环不应侵入中心区域: x=%d y=%d", x, y)
			}
		}
	}
	if visible == 0 {
		t.Fatal("环形频谱在静音时完全消失")
	}
	for index, present := range quadrants {
		if !present {
			t.Fatalf("静态圆环第 %d 象限缺失", index+1)
		}
	}
}

func TestVisualRingBidirectionalExtendsOnBothSidesOfMiddleRadius(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 200, 200))
	layer := theme.Layer{Type: theme.LayerAudioRing, Opacity: 1, Options: map[string]any{
		"count": 12, "thickness": 1, "inner_radius": .42, "direction": "bidirectional", "style": "radial",
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0, "contrast": 1.0, "separation": 0.0,
	}}
	drawRing(canvas, canvas.Bounds(), layer, []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	center := image.Pt(100, 100)
	// 顶部频带从中间半径同时向圆心和外缘延伸。
	innerPixel := canvas.RGBAAt(center.X, 46)
	outerPixel := canvas.RGBAAt(center.X, 12)
	if innerPixel.A == 0 || outerPixel.A == 0 {
		t.Fatalf("双向圆环没有同时向内外发散: inner=%v outer=%v", innerPixel, outerPixel)
	}
}

func TestPlayerLyricsRenderOutlineShadowAndProgressHighlight(t *testing.T) {
	manifest := theme.Manifest{ID: "lyrics-player", Name: "播放器歌词", Width: 480, Height: 180, Layers: []theme.Layer{
		{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: 0, Options: map[string]any{"color": "07111F"}},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .05, Y: .15, Width: .9, Height: .7}, Opacity: 1, ZIndex: 1, Options: map[string]any{
			"style": "player", "font_size": 34.0, "next_font_size": 18.0, "align": "center",
			"current_color": "F7FCFF", "next_color": "A8B8C8A0", "accent_color": "35E8FF",
			"outline_color": "000000FF", "outline_width": 2, "shadow": true, "shadow_offset_x": 3, "shadow_offset_y": 4,
			"shadow_blur": 2, "glow_radius": 3, "karaoke_highlight": true, "show_progress": true,
		}},
	}}
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	frame, err := renderer.Render(480, 180, State{Lyrics: LyricState{Current: "FLOWING LIGHT", Next: "NEXT VERSE", Progress: .55, PositionKnown: true, Playing: true}})
	if err != nil {
		t.Fatal(err)
	}
	accentPixels, darkEffectPixels := 0, 0
	for y := 20; y < 155; y++ {
		for x := 20; x < 460; x++ {
			pixel := frame.RGBAAt(x, y)
			if pixel.B > 210 && pixel.G > 170 && pixel.R < 130 {
				accentPixels++
			}
			if pixel.R < 5 && pixel.G < 8 && pixel.B < 14 {
				darkEffectPixels++
			}
		}
	}
	if accentPixels < 80 || darkEffectPixels < 40 {
		t.Fatalf("播放器歌词效果不足: accent=%d outline/shadow=%d", accentPixels, darkEffectPixels)
	}
}

func TestLyricsLayerIsEmptyWhileWaitingAndRemainsVisibleWhenPaused(t *testing.T) {
	manifest := theme.Manifest{ID: "lyrics-visibility", Name: "歌词可见性", Width: 320, Height: 120, Layers: []theme.Layer{
		{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "transparent"}},
		{
			ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .05, Y: .1, Width: .9, Height: .8}, Opacity: 1,
			Options: map[string]any{
				"style": "player", "panel_color": "102030E0", "panel_border_color": "55E5FFFF",
				"show_progress": true, "progress_track_color": "FFFFFF80",
			},
		},
	}}
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()

	waiting, err := renderer.Render(320, 120, State{Lyrics: LyricState{
		Current: "   ", Next: "不应单独显示的下一句", Progress: .8, PositionKnown: true, Playing: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for index, value := range waiting.Pix {
		if value != 0 {
			t.Fatalf("等待歌词时图层仍有可见像素: offset=%d value=%d", index, value)
		}
	}

	state := LyricState{Current: "暂停在这一句", Next: "下一句", Progress: .42, PositionKnown: true}
	state.Playing = true
	playing, err := renderer.Render(320, 120, State{Lyrics: state})
	if err != nil {
		t.Fatal(err)
	}
	state.Playing = false
	paused, err := renderer.Render(320, 120, State{Lyrics: state})
	if err != nil {
		t.Fatal(err)
	}
	playingDigest, pausedDigest := sha256.Sum256(playing.Pix), sha256.Sum256(paused.Pix)
	if playingDigest != pausedDigest {
		t.Fatal("暂停改变了已经显示的歌词画面")
	}
	if playingDigest == sha256.Sum256(waiting.Pix) {
		t.Fatal("暂停且已有当前句时歌词图层被隐藏")
	}
}

func TestPlayerLyricsEffectsDoNotClipAtSmallLayerEdges(t *testing.T) {
	lyrics := theme.Layer{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .08, Y: .2, Width: .84, Height: .6}, Opacity: 1, ZIndex: 1, Options: map[string]any{
		"style": "player", "font_size": 28.0, "next_font_size": 18.0, "panel_color": "00000000", "panel_border_color": "00000000",
		"outline_width": 5, "glow_radius": 8, "shadow": true, "shadow_blur": 7, "shadow_offset_y": 6,
		"karaoke_highlight": true, "show_progress": false,
	}}
	renderer, err := New(theme.Manifest{ID: "lyrics-small", Name: "小高度歌词", Width: 240, Height: 40, Layers: []theme.Layer{
		{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "transparent"}},
		lyrics,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	frame, err := renderer.Render(240, 40, State{Lyrics: LyricState{Current: "SMALL PLAYER", Progress: .6, PositionKnown: true}})
	if err != nil {
		t.Fatal(err)
	}
	bounds := normalizedRect(lyrics.Bounds, 240, 40)
	for x := bounds.Min.X; x < bounds.Max.X; x++ {
		top, bottom := frame.RGBAAt(x, bounds.Min.Y), frame.RGBAAt(x, bounds.Max.Y-1)
		if top.A != 0 || bottom.A != 0 {
			t.Fatalf("小高度歌词的描边或柔光触及裁切边缘：x=%d top=%v bottom=%v bounds=%v", x, top, bottom, bounds)
		}
	}
}

func TestTinyHostRenderedTextLayersStayInsideTheirBounds(t *testing.T) {
	manifest := theme.Manifest{
		ID: "tiny-text", Name: "极小文字图层", Width: 100, Height: 50,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: 0, Options: map[string]any{"color": "white"}},
			{ID: "telemetry", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: .05, Y: .1, Width: .24, Height: .48}, Opacity: 1, ZIndex: 1, Options: map[string]any{
				"fields": []any{"time", "Cpu_Freq", "Memory_Load", "Gpu_Load", "GPU_Temp"}, "font_size": 100, "panel_color": "00000000",
			}},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .55, Y: .1, Width: .24, Height: .48}, Opacity: 1, ZIndex: 1, Options: map[string]any{
				"font_size": 100, "panel_color": "00000000",
			}},
		},
	}
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	gpuPercent, gpuTemperature := 63.0, 45.0
	frame, err := renderer.Render(100, 50, State{
		Telemetry: telemetry.Snapshot{CapturedAt: time.Unix(1, 0), CPUPercent: 42, MemoryPercent: 64, GPUPercent: &gpuPercent, GPUTemperature: &gpuTemperature},
		Lyrics:    LyricState{Current: "这是一句很长的当前歌词", Next: "下一句同样很长"},
	})
	if err != nil {
		t.Fatal(err)
	}
	telemetryBounds := normalizedRect(manifest.Layers[1].Bounds, 100, 50)
	lyricsBounds := normalizedRect(manifest.Layers[2].Bounds, 100, 50)
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	insideChanges := 0
	for y := 0; y < frame.Bounds().Dy(); y++ {
		for x := 0; x < frame.Bounds().Dx(); x++ {
			point := image.Pt(x, y)
			inside := point.In(telemetryBounds) || point.In(lyricsBounds)
			changed := frame.RGBAAt(x, y) != white
			if !inside && changed {
				t.Fatalf("极小文字图层绘制越界：point=%v pixel=%v", point, frame.RGBAAt(x, y))
			}
			if inside && changed {
				insideChanges++
			}
		}
	}
	if insideChanges == 0 {
		t.Fatal("极小文字图层虽然未越界，但没有保留任何可见内容")
	}
}

func TestCanvasColorAndRotatedImageLayerKeepIndependentBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "picture.png")
	picture := image.NewRGBA(image.Rect(0, 0, 20, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 20; x++ {
			picture.SetRGBA(x, y, color.RGBA{R: 240, G: 30, B: 20, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, picture); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	resource, err := theme.InspectResource("picture", path, "")
	if err != nil {
		t.Fatal(err)
	}
	manifest := theme.Manifest{ID: "scene", Name: "独立图片", Width: 100, Height: 100, Resources: []theme.Resource{resource}, Layers: []theme.Layer{
		{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: -100, Options: map[string]any{"color": "white"}},
		{ID: "picture", Type: theme.LayerImage, Bounds: theme.Rect{X: .25, Y: .4, Width: .5, Height: .2}, Opacity: 1, Rotation: 90, ZIndex: 1, Options: map[string]any{"resource_id": "picture"}},
	}}
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	frame, err := renderer.Render(100, 100, State{})
	if err != nil {
		t.Fatal(err)
	}
	if pixel := frame.RGBAAt(5, 5); pixel.R < 245 || pixel.G < 245 || pixel.B < 245 {
		t.Fatalf("图片之外没有保留白色画布：%v", pixel)
	}
	if pixel := frame.RGBAAt(50, 50); pixel.R < 200 || pixel.G > 80 {
		t.Fatalf("旋转图片没有合成到图层中心：%v", pixel)
	}
	if pixel := frame.RGBAAt(25, 50); pixel.R < 245 || pixel.G < 245 || pixel.B < 245 {
		t.Fatalf("图片被错误拉伸为背景：%v", pixel)
	}
}

func TestRendererSkipsHiddenAndFullyTransparentStaticLayers(t *testing.T) {
	directory := t.TempDir()
	writeImage := func(name string, fill color.RGBA) theme.Resource {
		t.Helper()
		path := filepath.Join(directory, name+".png")
		frame := image.NewRGBA(image.Rect(0, 0, 8, 8))
		for y := 0; y < frame.Bounds().Dy(); y++ {
			for x := 0; x < frame.Bounds().Dx(); x++ {
				frame.SetRGBA(x, y, fill)
			}
		}
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, frame); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		resource, err := theme.InspectResource(name, path, "")
		if err != nil {
			t.Fatal(err)
		}
		return resource
	}
	hidden := writeImage("hidden", color.RGBA{R: 255, A: 255})
	transparent := writeImage("transparent", color.RGBA{B: 255, A: 255})
	manifest := theme.Manifest{
		ID: "visibility", Name: "可见度", Width: 40, Height: 40,
		Resources: []theme.Resource{hidden, transparent},
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: 0, Options: map[string]any{"color": "white"}},
			{ID: "hidden", Type: theme.LayerImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: 1, Options: map[string]any{"resource_id": hidden.ID, "visible": false}},
			{ID: "transparent", Type: theme.LayerImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 0, ZIndex: 2, Options: map[string]any{"resource_id": transparent.ID}},
		},
	}
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	frame, err := renderer.Render(40, 40, State{})
	if err != nil {
		t.Fatal(err)
	}
	if got := frame.RGBAAt(20, 20); got.R < 245 || got.G < 245 || got.B < 245 {
		t.Fatalf("隐藏或全透明图片仍被烘焙：%+v", got)
	}
}

func TestDynamicLayersPreserveVideoBackground(t *testing.T) {
	manifest := theme.Manifest{
		ID: "video-overlay", Name: "视频浮层", Width: 240, Height: 135,
		Layers: []theme.Layer{
			{
				ID: "video", Type: theme.LayerBackgroundVideo,
				Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			},
			{
				ID: "lyrics", Type: theme.LayerLyrics,
				Bounds: theme.Rect{X: 0.2, Y: 0.7, Width: 0.6, Height: 0.2}, Opacity: 0.78,
			},
		},
	}
	renderer, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.Close()
	videoFrame := image.NewRGBA(image.Rect(0, 0, 240, 135))
	for y := 0; y < 135; y++ {
		for x := 0; x < 240; x++ {
			videoFrame.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	frame, err := renderer.Render(240, 135, State{
		VideoFrame: videoFrame,
		Lyrics: LyricState{
			Current: "歌词浮在视频上方", Next: "背景仍然可见", Progress: 0.35,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range []image.Point{{X: 8, Y: 8}, {X: 120, Y: 30}, {X: 225, Y: 120}} {
		if got, want := frame.RGBAAt(point.X, point.Y), videoFrame.RGBAAt(point.X, point.Y); got != want {
			t.Fatalf("浮层外视频背景被改变: point=%v got=%v want=%v", point, got, want)
		}
	}
	changed := 0
	for y := 95; y < 122; y++ {
		for x := 48; x < 192; x++ {
			if frame.RGBAAt(x, y) != videoFrame.RGBAAt(x, y) {
				changed++
			}
		}
	}
	if changed < 100 {
		t.Fatalf("歌词浮层区域变化不足: %d", changed)
	}
}
