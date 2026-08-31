package api

import (
	"bytes"
	"image"
	"strconv"
	"strings"
	"testing"

	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

func TestCompileWaveshareLyricsStylesHaveTruthfulResidentSemantics(t *testing.T) {
	tests := []struct {
		style            string
		wantNext         bool
		wantProgressKind string
		wantAccent       bool
	}{
		{style: theme.LyricsStylePlayer, wantNext: true, wantProgressKind: "continuous", wantAccent: true},
		{style: theme.LyricsStyleKaraoke, wantNext: true, wantProgressKind: "segments", wantAccent: true},
		{style: theme.LyricsStyleClassic, wantNext: true, wantProgressKind: "continuous", wantAccent: false},
		{style: theme.LyricsStyleMinimal, wantNext: false, wantProgressKind: "none", wantAccent: false},
	}
	for _, test := range tests {
		t.Run(test.style, func(t *testing.T) {
			layer := theme.Layer{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .08, Y: .55, Width: .84, Height: .30}, Opacity: 1, ZIndex: 5, Options: map[string]any{
				"style": test.style, "show_progress": true, "karaoke_highlight": true,
				"current_color": "F7FCFF", "accent_color": "55E5FF", "outline_width": 0,
				"shadow": false, "glow_radius": 0,
			}}
			table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
			current, nextCount, progressCount, visibilityCount := map[string]string(nil), 0, 0, 0
			foundProgressStart, foundProgressEnd := false, false
			for _, widget := range table.widgets {
				switch widget.fields["system_data_name"] {
				case "CaseScreen_LyricsCurrent":
					if widget.fields["text_str"] != "" {
						t.Fatalf("当前歌词控件仍带等待占位: %+v", widget.fields)
					}
					if widget.fields["itemName"] == "lyrics-current" {
						current = widget.fields
					}
				case "CaseScreen_LyricsNext":
					if widget.fields["text_str"] != "" {
						t.Fatalf("下一句控件仍带等待占位: %+v", widget.fields)
					}
					nextCount++
				case telemetry.LyricsProgressFineKey:
					if widget.fields["type"] != "102" || widget.fields["system_data_min_value"] == "" || widget.fields["system_data_max_value"] == "" {
						t.Fatalf("歌词进度不是高分辨率标量控件: %+v", widget.fields)
					}
					if widget.fields["back_color"] != "r=0,g=0,b=0,a=0" {
						t.Fatalf("零进度仍会显示固定底轨: %+v", widget.fields)
					}
					minimum, minErr := strconv.ParseFloat(widget.fields["system_data_min_value"], 64)
					maximum, maxErr := strconv.ParseFloat(widget.fields["system_data_max_value"], 64)
					if minErr != nil || maxErr != nil || minimum < 0 || maximum > telemetry.LyricsProgressFineMax || maximum <= minimum {
						t.Fatalf("歌词细进度分段量程错误: %+v", widget.fields)
					}
					foundProgressStart = foundProgressStart || minimum == 0
					foundProgressEnd = foundProgressEnd || maximum == telemetry.LyricsProgressFineMax
					progressCount++
				case telemetry.LyricsVisibleKey:
					visibilityCount++
					if widget.fields["type"] != "102" || widget.fields["back_color"] != "r=0,g=0,b=0,a=0" {
						t.Fatalf("歌词可见控件在 LV=0 时仍会留下背景: %+v", widget.fields)
					}
				}
			}
			if current == nil || (nextCount > 0) != test.wantNext {
				t.Fatalf("%s 动态文本布局错误: current=%v next=%d", test.style, current != nil, nextCount)
			}
			if progressCount > 0 && (!foundProgressStart || !foundProgressEnd) {
				t.Fatalf("%s 细进度控件没有覆盖完整 0..10000 量程", test.style)
			}
			if visibilityCount < 1 {
				t.Fatalf("%s 缺少歌词图层可见性控件", test.style)
			}
			accent := strings.Contains(current["front_color"], "r=85,g=229,b=255")
			if accent != test.wantAccent {
				t.Fatalf("%s 当前句颜色降级语义错误: accent=%v fields=%+v", test.style, accent, current)
			}
			switch test.wantProgressKind {
			case "none":
				if progressCount != 0 {
					t.Fatalf("%s 不应生成进度控件: %d", test.style, progressCount)
				}
			case "segments":
				if progressCount < 4 {
					t.Fatalf("%s 没有生成分段真实进度轨: %d", test.style, progressCount)
				}
			default:
				if progressCount != 1 {
					t.Fatalf("%s 应生成一个连续真实进度轨: %d", test.style, progressCount)
				}
			}
		})
	}
}

func TestWaveshareLyricsShellIsDynamicAndNeverBakesWaitingVisuals(t *testing.T) {
	transparent := theme.Manifest{Width: 200, Height: 100, Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .8}, Opacity: 1,
		Options: map[string]any{"style": "player", "panel_color": "00000000", "panel_border_color": "00000000", "accent_color": "55E5FF", "show_progress": true},
	}}}
	canvas := image.NewRGBA(image.Rect(0, 0, 200, 100))
	drawWaveshareComponentShells(canvas, transparent)
	for offset := 3; offset < len(canvas.Pix); offset += 4 {
		if canvas.Pix[offset] != 0 {
			t.Fatal("歌词背景仍烘焙了与真实进度无关的固定条")
		}
	}

	bordered := transparent
	bordered.Layers = append([]theme.Layer(nil), transparent.Layers...)
	bordered.Layers[0].Options = map[string]any{
		"style": "player", "panel_color": "FF0000FF", "panel_border_color": "00FF00FF", "corner_radius": 0, "show_progress": false,
	}
	canvas = image.NewRGBA(image.Rect(0, 0, 200, 100))
	drawWaveshareComponentShells(canvas, bordered)
	for offset := 3; offset < len(canvas.Pix); offset += 4 {
		if canvas.Pix[offset] != 0 {
			t.Fatal("歌词面板或描边仍被永久烘焙进背景")
		}
	}
	table := compileWaveshareTable(t, bordered)
	foundBorder, foundPanel := false, false
	for _, widget := range table.widgets {
		if widget.fields["system_data_name"] != telemetry.LyricsVisibleKey {
			continue
		}
		if widget.fields["type"] != "102" || widget.fields["back_color"] != "r=0,g=0,b=0,a=0" ||
			widget.fields["system_data_min_value"] != "0" || widget.fields["system_data_max_value"] != "1" {
			t.Fatalf("歌词可见性壳不是透明背景的 0/1 标量控件: %+v", widget.fields)
		}
		switch widget.fields["itemName"] {
		case "lyrics-visible-border":
			foundBorder = strings.Contains(widget.fields["front_color"], "r=0,g=255,b=0")
		case "lyrics-visible-panel":
			foundPanel = strings.Contains(widget.fields["front_color"], "r=255,g=0,b=0")
		}
	}
	if !foundBorder || !foundPanel {
		t.Fatalf("歌词动态面板控件不完整: border=%v panel=%v", foundBorder, foundPanel)
	}
}

func TestCompileWaveshareLyricsShowProgressFalseBindsNoProgressKey(t *testing.T) {
	layer := theme.Layer{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .1, Y: .5, Width: .8, Height: .3}, Opacity: 1,
		Options: map[string]any{"style": "player", "show_progress": false}}
	table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	for _, widget := range table.widgets {
		if widget.fields["system_data_name"] == telemetry.LyricsProgressFineKey ||
			widget.fields["system_data_name"] == telemetry.LyricsProgressLegacyKey {
			t.Fatalf("show_progress=false 仍绑定动态进度键: %+v", widget.fields)
		}
	}
}

func TestCompileWaveshareLyricsUsesResidentPlayerStyleEffects(t *testing.T) {
	layer := theme.Layer{
		ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .08, Y: .64, Width: .84, Height: .24}, Opacity: 1, ZIndex: 5,
		Options: map[string]any{
			"style": "player", "font_family": "rounded", "font_weight": "bold", "align": "center",
			"accent_color": "55E5FF", "outline_width": 2, "outline_color": "02070EE1",
			"shadow": true, "shadow_offset_x": 2, "shadow_offset_y": 3, "glow_radius": 4,
		},
	}
	table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	counts := map[string]int{}
	foundMainCurrent := false
	for _, widget := range table.widgets {
		key := widget.fields["system_data_name"]
		if key != "CaseScreen_LyricsCurrent" && key != "CaseScreen_LyricsNext" {
			continue
		}
		counts[key]++
		if widget.fields["type"] != "8" || widget.fields["system_data_flag"] != "1" {
			t.Fatalf("歌词效果不是屏端动态文字控件: %+v", widget.fields)
		}
		if widget.fields["text_customFont_path"] != "/usr/share/fonts/YuGothB.ttc" || widget.fields["text_IsBold"] != "1" {
			t.Fatalf("歌词字体样式没有驻留到 .Theme: %+v", widget.fields)
		}
		if widget.fields["itemName"] == "lyrics-current" {
			foundMainCurrent = true
			if !strings.Contains(widget.fields["front_color"], "r=85,g=229,b=255") {
				t.Fatalf("当前歌词没有使用播放器强调色: %+v", widget.fields)
			}
		}
	}
	if counts["CaseScreen_LyricsCurrent"] < 10 || counts["CaseScreen_LyricsNext"] < 4 || !foundMainCurrent {
		t.Fatalf("歌词描边/阴影/柔光控件不完整: counts=%v main=%v", counts, foundMainCurrent)
	}
}

func TestCompileWaveshareLegacyLyricsUsesSharedDefaultFontSizes(t *testing.T) {
	layer := theme.Layer{
		ID: "lyrics", Type: theme.LayerLyrics,
		Bounds: theme.Rect{X: .02, Y: .72, Width: .96, Height: .2}, Opacity: 1, ZIndex: 5,
		Options: map[string]any{
			"style": "player", "font_weight": "regular", "show_progress": false,
			"outline_width": 0, "shadow": false, "glow_radius": 0,
		},
	}
	table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	wanted := map[string]string{
		"lyrics-current": strconv.Itoa(theme.DefaultLyricsFontSize),
		"lyrics-next":    strconv.Itoa(theme.DefaultLyricsNextFontSize),
	}
	for _, widget := range table.widgets {
		if _, ok := wanted[widget.fields["itemName"]]; ok {
			if got := widget.fields["text_size"]; got != wanted[widget.fields["itemName"]] {
				t.Fatalf("%s 字号=%s，期望 %s", widget.fields["itemName"], got, wanted[widget.fields["itemName"]])
			}
			delete(wanted, widget.fields["itemName"])
		}
	}
	if len(wanted) != 0 {
		t.Fatalf("缺少默认字号歌词控件: %v", wanted)
	}
}

func TestWaveshareFontWeightsUseResidentFaceMatrix(t *testing.T) {
	tests := []struct {
		family, weight string
		path, name     string
		bold           string
	}{
		{"pangmen", "light", "/usr/share/fonts/Dengl.ttf", "DengXian Light", "0"},
		{"pangmen", "regular", "/usr/share/fonts/Deng.ttf", "DengXian", "0"},
		{"pangmen", "medium", "/usr/share/fonts/Dengb.ttf", "DengXian", "0"},
		{"pangmen", "bold", "/usr/share/fonts/Dengb.ttf", "DengXian", "1"},
		{"source-han", "light", "/usr/share/fonts/Source San Hans BLOD.ttf", "Source Han Sans CN", "0"},
		{"source-han", "regular", "/usr/share/fonts/Source San Hans BLOD.ttf", "Source Han Sans CN", "0"},
		{"source-han", "medium", "/usr/share/fonts/Source San Hans BLOD.ttf", "Source Han Sans CN", "0"},
		{"source-han", "bold", "/usr/share/fonts/Source San Hans BLOD.ttf", "Source Han Sans CN", "1"},
		{"microsoft-yahei", "light", "/usr/share/fonts/msyhl.ttc", "Microsoft YaHei Light", "0"},
		{"microsoft-yahei", "regular", "/usr/share/fonts/msyh.ttc", "Microsoft YaHei", "0"},
		{"microsoft-yahei", "medium", "/usr/share/fonts/msyhbd.ttc", "Microsoft YaHei", "0"},
		{"microsoft-yahei", "bold", "/usr/share/fonts/msyhbd.ttc", "Microsoft YaHei", "1"},
		{"simhei", "light", "/usr/share/fonts/simhei.ttf", "SimHei", "0"},
		{"simhei", "regular", "/usr/share/fonts/simhei.ttf", "SimHei", "0"},
		{"simhei", "medium", "/usr/share/fonts/simhei.ttf", "SimHei", "0"},
		{"simhei", "bold", "/usr/share/fonts/simhei.ttf", "SimHei", "1"},
		{"rounded", "light", "/usr/share/fonts/YuGothL.ttc", "Yu Gothic Light", "0"},
		{"rounded", "regular", "/usr/share/fonts/YuGothR.ttc", "Yu Gothic", "0"},
		{"rounded", "medium", "/usr/share/fonts/YuGothM.ttc", "Yu Gothic Medium", "0"},
		{"rounded", "bold", "/usr/share/fonts/YuGothB.ttc", "Yu Gothic", "1"},
		{"digital", "light", "/usr/share/fonts/LiquidCrystal-Light.otf", "Liquid Crystal", "0"},
		{"digital", "regular", "/usr/share/fonts/LiquidCrystal-Normal.otf", "Liquid Crystal", "0"},
		{"digital", "medium", "/usr/share/fonts/LiquidCrystal-Bold.otf", "Liquid Crystal", "0"},
		{"digital", "bold", "/usr/share/fonts/LiquidCrystal-ExBold.otf", "Liquid Crystal", "1"},
		{"monospace", "light", "/usr/share/fonts/CascadiaMono.ttf", "Cascadia Mono", "0"},
		{"monospace", "regular", "/usr/share/fonts/consola.ttf", "Consolas", "0"},
		{"monospace", "medium", "/usr/share/fonts/consolab.ttf", "Consolas", "0"},
		{"monospace", "bold", "/usr/share/fonts/consolab.ttf", "Consolas", "1"},
		{"serif", "light", "/usr/share/fonts/simsun.ttc", "SimSun", "0"},
		{"serif", "regular", "/usr/share/fonts/simsun.ttc", "SimSun", "0"},
		{"serif", "medium", "/usr/share/fonts/simsunb.ttf", "SimSun", "0"},
		{"serif", "bold", "/usr/share/fonts/simsunb.ttf", "SimSun", "1"},
	}
	for _, test := range tests {
		t.Run(test.family+"/"+test.weight, func(t *testing.T) {
			widget := waveshareThemeWidget{fields: map[string]string{}}
			waveshareApplyFontOptions(&widget, map[string]any{"font_family": test.family, "font_weight": test.weight})
			if got := widget.fields["text_customFont_path"]; got != test.path {
				t.Fatalf("字体路径=%q，期望 %q", got, test.path)
			}
			if got := widget.fields["text_family"]; got != test.name {
				t.Fatalf("字体族=%q，期望 %q", got, test.name)
			}
			if got := widget.fields["text_IsBold"]; got != test.bold {
				t.Fatalf("粗体标志=%q，期望 %q", got, test.bold)
			}
		})
	}
}

func TestWaveshareSourceHanAndMicrosoftYaHeiDifferAtEveryWeight(t *testing.T) {
	compile := func(family, weight string) ([]byte, map[string]string) {
		manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
			ID: "font-sample", Type: theme.LayerText,
			Bounds: theme.Rect{X: .1, Y: .1, Width: .6, Height: .2}, Opacity: 1, ZIndex: 2,
			Options: map[string]any{"text": "字体族 123", "font_family": family, "font_weight": weight, "font_size": 42.0},
		}}}
		compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
		if err != nil {
			t.Fatal(err)
		}
		table, err := parseWaveshareThemeWidgetTable(compiled)
		if err != nil {
			t.Fatal(err)
		}
		for _, widget := range table.widgets {
			if widget.fields["itemName"] == "font-sample" {
				return compiled, widget.fields
			}
		}
		t.Fatalf("缺少字体样例控件: family=%s weight=%s", family, weight)
		return nil, nil
	}

	for _, weight := range []string{"light", "regular", "medium", "bold"} {
		t.Run(weight, func(t *testing.T) {
			sourceData, sourceFields := compile("source-han", weight)
			yaheiData, yaheiFields := compile("microsoft-yahei", weight)
			if sourceFields["text_customFont_path"] != "/usr/share/fonts/Source San Hans BLOD.ttf" ||
				strings.Contains(strings.ToLower(sourceFields["text_family"]), "yahei") {
				t.Fatalf("Source Han %s 回退到了雅黑: %+v", weight, sourceFields)
			}
			if sourceFields["text_customFont_path"] == yaheiFields["text_customFont_path"] ||
				sourceFields["text_family"] == yaheiFields["text_family"] {
				t.Fatalf("Source Han/%s 与微软雅黑使用相同驻留字脸: source=%+v yahei=%+v", weight, sourceFields, yaheiFields)
			}
			if bytes.Equal(sourceData, yaheiData) {
				t.Fatalf("Source Han/%s 与微软雅黑编译出完全相同的 Theme", weight)
			}
		})
	}
}

func TestCompileWaveshareFourWeightsHaveDistinctThemeAndCacheFingerprint(t *testing.T) {
	for _, family := range []string{"pangmen", "simhei", "digital", "serif"} {
		t.Run(family, func(t *testing.T) {
			seenTheme := make(map[string]string)
			seenPath := make(map[string]string)
			for _, weight := range []string{"light", "regular", "medium", "bold"} {
				manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
					ID: "sample", Type: theme.LayerText,
					Bounds: theme.Rect{X: .1, Y: .1, Width: .5, Height: .2}, Opacity: 1, ZIndex: 2,
					Options: map[string]any{"text": "字重 123", "font_family": family, "font_weight": weight, "font_size": 42.0, "color": "FFFFFFFF"},
				}}}
				compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
				if err != nil {
					t.Fatal(err)
				}
				key := string(compiled)
				if previous, exists := seenTheme[key]; exists {
					t.Fatalf("微雪 Theme 字重 %s/%s 与 %s 完全相同", family, weight, previous)
				}
				seenTheme[key] = weight
				path := waveshareVersionedThemeRemotePath("/data/theme/1024x600/font-matrix.Theme", compiled)
				if previous, exists := seenPath[path]; exists {
					t.Fatalf("微雪缓存指纹 %s/%s 与 %s 相同", family, weight, previous)
				}
				seenPath[path] = weight
			}
		})
	}
}

func TestCompileWaveshareNonLyricsBoldAddsSharedKeyCopyAndChangesFingerprint(t *testing.T) {
	layer := theme.Layer{
		ID: "telemetry", Type: theme.LayerTelemetry,
		Bounds: theme.Rect{X: .05, Y: .05, Width: .4, Height: .25}, Opacity: 1, ZIndex: 3,
		Options: map[string]any{
			"fields": []any{"Cpu_Freq"}, "style": "cards", "font_family": "microsoft-yahei",
			"font_weight": "bold", "show_label": true, "show_value": true,
		},
	}
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}}
	table := compileWaveshareTable(t, manifest)
	boldCopies := 0
	for _, widget := range table.widgets {
		if !strings.Contains(widget.fields["itemName"], "-bold-") {
			continue
		}
		boldCopies++
		if widget.fields["system_data_flag"] == "1" && widget.fields["system_data_name"] != "Cpu使用率" {
			t.Fatalf("粗体动态副本没有共享原小数据键: %+v", widget.fields)
		}
	}
	if boldCopies < 2 {
		t.Fatalf("遥测标签和数值没有生成可见粗体副本: %d", boldCopies)
	}

	boldData, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Layers[0].Options["font_weight"] = "regular"
	regularData, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(boldData) == string(regularData) {
		t.Fatal("字重变化没有进入驻留 Theme 内容")
	}
	basePath := "/data/theme/1024x600/font-cache.Theme"
	if boldPath, regularPath := waveshareVersionedThemeRemotePath(basePath, boldData), waveshareVersionedThemeRemotePath(basePath, regularData); boldPath == regularPath {
		t.Fatalf("字重变化没有进入屏端缓存指纹: %q", boldPath)
	}
}

func TestCompileWaveshareLyricsBoldAddsSharedKeyEmphasisWidgets(t *testing.T) {
	layer := theme.Layer{
		ID: "lyrics", Type: theme.LayerLyrics,
		Bounds: theme.Rect{X: .05, Y: .65, Width: .9, Height: .25}, Opacity: 1, ZIndex: 5,
		Options: map[string]any{
			"style": "player", "font_weight": "bold", "show_progress": false,
			"outline_width": 0, "shadow": false, "glow_radius": 0, "karaoke_highlight": false,
		},
	}
	table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	mainX, mainY := 0, 0
	foundMain := false
	type point struct{ x, y int }
	positions := map[point]bool{}
	for _, widget := range table.widgets {
		name := widget.fields["itemName"]
		if name == "lyrics-current" {
			mainX, _ = strconv.Atoi(widget.fields["x"])
			mainY, _ = strconv.Atoi(widget.fields["y"])
			foundMain = true
			continue
		}
		if !strings.HasPrefix(name, "lyrics-current-bold-") {
			continue
		}
		if widget.fields["system_data_name"] != "CaseScreen_LyricsCurrent" || widget.fields["system_data_flag"] != "1" {
			t.Fatalf("加粗副本没有共享当前句小数据键: %+v", widget.fields)
		}
		x, _ := strconv.Atoi(widget.fields["x"])
		y, _ := strconv.Atoi(widget.fields["y"])
		positions[point{x, y}] = true
	}
	if !foundMain || len(positions) != 4 {
		t.Fatalf("粗体形态膨胀控件不完整: main=%v positions=%v", foundMain, positions)
	}
	for _, offset := range []point{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
		if !positions[point{mainX + offset.x, mainY + offset.y}] {
			t.Fatalf("缺少 1px 加粗位置 (%d,%d): %v", offset.x, offset.y, positions)
		}
	}

	layer.Options["font_weight"] = "regular"
	regular := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	for _, widget := range regular.widgets {
		if strings.HasPrefix(widget.fields["itemName"], "lyrics-current-bold-") {
			t.Fatalf("常规字重不应生成形态膨胀副本: %+v", widget.fields)
		}
	}
}

func TestCompileWaveshareLyricsFallsBackToResidentCJKBoldWithOutline(t *testing.T) {
	for _, unsupportedFamily := range []string{"digital", "monospace"} {
		t.Run(unsupportedFamily, func(t *testing.T) {
			layer := theme.Layer{
				ID: "lyrics", Type: theme.LayerLyrics,
				Bounds: theme.Rect{X: .08, Y: .62, Width: .84, Height: .26}, Opacity: 1, ZIndex: 5,
				// 数码和等宽字体可用于英文遥测，但不保证包含动态歌词的中日韩文字形。
				Options: map[string]any{"style": "player", "font_family": unsupportedFamily, "show_progress": true},
			}
			table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
			currentWidgets := 0
			outlineWidgets := 0
			for _, widget := range table.widgets {
				if widget.fields["system_data_name"] != "CaseScreen_LyricsCurrent" {
					continue
				}
				currentWidgets++
				if widget.fields["text_customFont_path"] != "/usr/share/fonts/Source San Hans BLOD.ttf" ||
					widget.fields["text_family"] != "Source Han Sans CN" || widget.fields["text_IsBold"] != "1" {
					t.Fatalf("歌词没有回退到屏内粗体 CJK 字体: %+v", widget.fields)
				}
				if strings.Contains(widget.fields["itemName"], "-outline-") {
					outlineWidgets++
				}
			}
			if currentWidgets == 0 || outlineWidgets < 4 {
				t.Fatalf("默认歌词缺少动态主文字或描边: current=%d outline=%d", currentWidgets, outlineWidgets)
			}
		})
	}
}

func TestCompileWaveshareSpectrumHonorsThinBarsAndBidirectionalRing(t *testing.T) {
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .05, Y: .58, Width: .55, Height: .28}, Opacity: 1, ZIndex: 2,
			Options: map[string]any{"count": 12, "gap": 6, "thickness": 2, "direction": "center"}},
		{ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: .64, Y: .08, Width: .30, Height: .50}, Opacity: 1, ZIndex: 3,
			Options: map[string]any{"count": 12, "thickness": 1, "direction": "bidirectional", "style": "spokes"}},
	}}
	table := compileWaveshareTable(t, manifest)
	bars := 0
	ringOut, ringIn := 0, 0
	for _, widget := range table.widgets {
		name := widget.fields["itemName"]
		if strings.HasPrefix(name, "bars-band-") {
			bars++
			thickness, err := strconv.Atoi(widget.fields["h"])
			if err != nil || thickness != 2 {
				t.Fatalf("条状频谱线宽没有写入 .Theme: %+v", widget.fields)
			}
		}
		if strings.HasPrefix(name, "ring-band-") {
			thickness, err := strconv.Atoi(widget.fields["h"])
			if err != nil || thickness != 1 {
				t.Fatalf("环状频谱线宽没有写入 .Theme: %+v", widget.fields)
			}
			if strings.HasSuffix(name, "-out") {
				ringOut++
			}
			if strings.HasSuffix(name, "-in") {
				ringIn++
			}
		}
	}
	if bars != 24 || ringOut != 12 || ringIn != 12 {
		t.Fatalf("频谱驻留控件数量错误: bars=%d ringOut=%d ringIn=%d", bars, ringOut, ringIn)
	}
}

func TestCompileWaveshareBarsFourwayBurstUsesResidentRadialWidgets(t *testing.T) {
	for _, options := range []map[string]any{
		{"style": "burst", "direction": "bottom", "count": 24, "thickness": 2},
		{"style": "equalizer", "direction": "fourway", "count": 24, "thickness": 2},
	} {
		layer := theme.Layer{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .25, Y: .15, Width: .5, Height: .7}, Opacity: 1, ZIndex: 2, Options: options}
		table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
		quadrants := [4]bool{}
		keys := map[string]bool{}
		widgets := 0
		for _, widget := range table.widgets {
			if !strings.HasPrefix(widget.fields["itemName"], "bars-band-") {
				continue
			}
			widgets++
			rotation, err := strconv.Atoi(widget.fields["rotate"])
			if err != nil {
				t.Fatalf("四向控件旋转角无效: %+v", widget.fields)
			}
			normalized := ((rotation % 360) + 360) % 360
			quadrants[min(3, normalized/90)] = true
			keys[widget.fields["system_data_name"]] = true
			assertWaveshareRotatedRectInCanvas(t, widget.fields, 1024, 600)
		}
		if widgets != 24 || len(keys) > 24 {
			t.Fatalf("四向爆发没有复用短频带键: widgets=%d keys=%d", widgets, len(keys))
		}
		for index, visible := range quadrants {
			if !visible {
				t.Fatalf("微雪四向爆发缺少第 %d 象限的原生控件", index+1)
			}
		}
	}
}

func TestCompileWaveshareWaveformHasNoFixedSineAndGlowReusesSmallPacketKeys(t *testing.T) {
	layer := theme.Layer{
		ID: "wave", Type: theme.LayerAudioBars,
		Bounds: theme.Rect{X: .08, Y: .6, Width: .84, Height: .22}, Opacity: 1, ZIndex: 2,
		Options: map[string]any{
			"style": "waveform", "direction": "bottom", "count": 8, "thickness": 2,
			"glow": true, "glow_radius": 6,
		},
	}
	table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	baseY := ""
	base, glows := 0, 0
	keys := make(map[string]bool)
	baseByKey := make(map[string]waveshareThemeWidget)
	for _, widget := range table.widgets {
		name := widget.fields["itemName"]
		if !strings.HasPrefix(name, "wave-band-") {
			continue
		}
		key := widget.fields["system_data_name"]
		keys[key] = true
		if strings.HasSuffix(name, "-glow") {
			glows++
			continue
		}
		base++
		baseByKey[key] = widget
		if baseY == "" {
			baseY = widget.fields["y"]
		} else if widget.fields["y"] != baseY {
			t.Fatalf("波形驻留控件仍含与声音无关的固定正弦偏移: first=%s current=%s", baseY, widget.fields["y"])
		}
	}
	if base != 8 || glows != 8 || len(keys) != 8 {
		t.Fatalf("柔光应只增加驻留控件、不增加小包键: base=%d glow=%d keys=%d", base, glows, len(keys))
	}
	for _, widget := range table.widgets {
		if !strings.HasSuffix(widget.fields["itemName"], "-glow") {
			continue
		}
		baseWidget := baseByKey[widget.fields["system_data_name"]]
		baseHeight, _ := strconv.Atoi(baseWidget.fields["h"])
		glowHeight, _ := strconv.Atoi(widget.fields["h"])
		if glowHeight <= baseHeight || widget.fields["back_color"] != "r=0,g=0,b=0,a=0" {
			t.Fatalf("驻留柔光没有包围细柱或仍有固定轨道: base=%+v glow=%+v", baseWidget.fields, widget.fields)
		}
	}
}

func TestCompileWaveshareFourwayPacketIndexesMatchResidentWidgets(t *testing.T) {
	cases := []struct {
		name    string
		bounds  theme.Rect
		options map[string]any
	}{
		{name: "缺省爆发", bounds: theme.Rect{X: .25, Y: .15, Width: .5, Height: .7}, options: map[string]any{"style": "burst"}},
		{name: "窄边界四向", bounds: theme.Rect{Width: 1.0 / 1024, Height: 1.0 / 600}, options: map[string]any{"direction": "fourway", "count": 24}},
		{name: "窄边界下限含峰值", bounds: theme.Rect{Width: 1.0 / 1024, Height: 1.0 / 600}, options: map[string]any{"style": "burst", "count": 6, "peak_hold": true}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			layer := theme.Layer{ID: "bars", Type: theme.LayerAudioBars, Bounds: test.bounds, Opacity: 1, ZIndex: 2, Options: test.options}
			table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
			compiled := make(map[string]bool)
			for _, widget := range table.widgets {
				if strings.HasPrefix(widget.fields["itemName"], "bars-band-") {
					compiled[widget.fields["system_data_name"]] = true
				}
			}
			expected := make(map[string]bool)
			for _, index := range theme.WaveshareAudioBandIndexes(layer, 1024, 600) {
				expected[theme.WaveshareAudioBandKey(layer, index)] = true
				if theme.OptionBool(layer.Options, "peak_hold", false) {
					expected[theme.WaveshareAudioPeakKey(layer, index)] = true
				}
			}
			if len(compiled) != len(expected) {
				t.Fatalf("编译引用键与发包键数量不一致: compiled=%d expected=%d\ncompiled=%v\nexpected=%v", len(compiled), len(expected), compiled, expected)
			}
			for key := range compiled {
				if !expected[key] {
					t.Fatalf("编译器引用了未发送的频带键 %q", key)
				}
			}
		})
	}
}

func TestCompileWaveshareDefaultRingUsesSharedCountAndPacketKeys(t *testing.T) {
	layer := theme.Layer{
		ID: "ring", Type: theme.LayerAudioRing,
		Bounds: theme.Rect{X: .2, Y: .1, Width: .6, Height: .8}, Opacity: 1, ZIndex: 2,
	}
	table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	compiled := make(map[string]bool)
	for _, widget := range table.widgets {
		if strings.HasPrefix(widget.fields["itemName"], "ring-band-") {
			compiled[widget.fields["system_data_name"]] = true
			if got := widget.fields["h"]; got != "1" {
				t.Fatalf("默认环状频谱没有编译为 1px 细柱: item=%s h=%s", widget.fields["itemName"], got)
			}
		}
	}
	expected := make(map[string]bool)
	for _, index := range theme.WaveshareAudioBandIndexes(layer, 1024, 600) {
		expected[theme.WaveshareAudioBandKey(layer, index)] = true
	}
	if len(compiled) != theme.DefaultAudioRingCount || len(compiled) != len(expected) {
		t.Fatalf("默认环状驻留控件与共享段数不一致: compiled=%d expected=%d default=%d", len(compiled), len(expected), theme.DefaultAudioRingCount)
	}
	for key := range compiled {
		if !expected[key] {
			t.Fatalf("默认环状编译器引用了未发送的键 %q", key)
		}
	}
}
