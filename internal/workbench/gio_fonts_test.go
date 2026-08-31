package workbench

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"gioui.org/font"
	"gioui.org/io/system"
	"gioui.org/text"
	"golang.org/x/image/math/fixed"
)

func TestWorkbenchFontCollectionContainsEveryLogicalFamilyAndWeight(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("逻辑预览字体使用 Windows 字体白名单")
	}
	wanted := map[font.Typeface]map[font.Weight]bool{
		workbenchTypefacePangMen: {}, workbenchTypefaceSourceHan: {},
		workbenchTypefaceMicrosoftYaHei: {}, workbenchTypefaceSimHei: {},
		workbenchTypefaceDigital: {}, workbenchTypefaceMonospace: {},
		workbenchTypefaceRounded: {}, workbenchTypefaceSerif: {},
	}
	for _, face := range workbenchFontCollection() {
		if weights, ok := wanted[face.Font.Typeface]; ok {
			weights[face.Font.Weight] = true
		}
	}
	for typeface, found := range wanted {
		for _, weight := range []font.Weight{font.Light, font.Normal, font.Medium, font.Bold} {
			if !found[weight] {
				t.Errorf("工作台缺少逻辑字脸 %q/%d", typeface, weight)
			}
		}
	}
}

func TestWorkbenchSourceHanNeverSharesYaHeiBottomFace(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("逻辑预览字体使用 Windows 字体白名单")
	}
	type key struct {
		typeface font.Typeface
		weight   font.Weight
	}
	faces := make(map[key]font.Face)
	for _, candidate := range workbenchFontCollection() {
		faces[key{candidate.Font.Typeface, candidate.Font.Weight}] = candidate.Face
	}
	for _, weight := range []font.Weight{font.Light, font.Normal, font.Medium, font.Bold} {
		source := faces[key{workbenchTypefaceSourceHan, weight}]
		yahei := faces[key{workbenchTypefaceMicrosoftYaHei, weight}]
		if source == nil || yahei == nil {
			t.Fatalf("字体集合缺少 Source Han/微软雅黑字重 %d", weight)
		}
		if source == yahei {
			t.Fatalf("Source Han 与微软雅黑的底层 face 在字重 %d 下完全相同", weight)
		}
	}
}

func TestLyricsFontChoicesExcludeNonCJKFamilies(t *testing.T) {
	allowed := make(map[string]bool, len(waveshareLyricsFontChoices))
	for _, choice := range waveshareLyricsFontChoices {
		allowed[choice.value] = true
	}
	for _, unsupported := range []string{"digital", "monospace"} {
		if allowed[unsupported] {
			t.Fatalf("歌词字体选择器仍暴露不含完整中日韩字形的字体族 %q", unsupported)
		}
	}
	for _, required := range []string{"pangmen", "source-han", "microsoft-yahei", "simhei", "rounded", "serif"} {
		if !allowed[required] {
			t.Fatalf("歌词字体选择器缺少中日韩安全字族 %q", required)
		}
	}
}

func TestPreviewFontPassesKeepAllFourWeightsDistinct(t *testing.T) {
	seen := make(map[string]string)
	for _, weight := range []string{"light", "regular", "medium", "bold"} {
		passes := previewFontPasses(weight)
		signature := ""
		for _, pass := range passes {
			signature += fmt.Sprintf("%d,%d,%.2f;", pass.offset.X, pass.offset.Y, pass.alpha)
		}
		if previous, exists := seen[signature]; exists {
			t.Fatalf("Gio 预览字重 %s 与 %s 绘制契约相同", weight, previous)
		}
		seen[signature] = weight
	}
	if passes := previewFontPasses("light"); len(passes) != 1 || passes[0].alpha >= 1 {
		t.Fatalf("light 没有生成细化绘制：%+v", passes)
	}
}

func TestWorkbenchThemeUsesReadyBoundedShaper(t *testing.T) {
	started := time.Now()
	theme := newWorkbenchTheme()
	if theme == nil || theme.Shaper == nil {
		t.Fatal("工作台主题没有初始化字体整形器")
	}
	theme.Shaper.LayoutString(text.Parameters{
		Font:      font.Font{Weight: font.Bold},
		Alignment: text.Start,
		PxPerEm:   fixed.I(18),
		MaxLines:  1,
		MinWidth:  0,
		MaxWidth:  480,
		Locale:    system.Locale{Language: "zh-CN", Direction: system.LTR},
	}, "机箱副屏 123")
	glyphs := 0
	for glyph, ok := theme.Shaper.NextGlyph(); ok; glyph, ok = theme.Shaper.NextGlyph() {
		_ = glyph
		glyphs++
	}
	if glyphs == 0 {
		t.Fatal("工作台字体整形器没有输出中文界面字形")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("有界字体初始化过慢：%v", elapsed)
	}
}
