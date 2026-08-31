package workbench

import (
	"encoding/json"
	"image"
	"image/color"
	"reflect"
	"testing"

	caseTheme "casescreen/internal/theme"
)

func TestLyricPreviewRegressionStylesStayVisuallyDistinct(t *testing.T) {
	styles := []string{"player", "karaoke", "classic", "minimal"}
	seen := make(map[lyricPreviewSpec]string, len(styles))
	result := make(map[string]lyricPreviewSpec, len(styles))
	for _, style := range styles {
		layer := caseTheme.Layer{Type: caseTheme.LayerLyrics, Options: map[string]any{
			"style": style, "align": "center", "karaoke_highlight": true, "show_progress": true,
		}}
		spec := buildLyricPreviewSpec(layer, image.Pt(720, 180), 8, 4)
		if previous, duplicate := seen[spec]; duplicate {
			t.Fatalf("歌词样式 %s 与 %s 的预览状态和布局完全相同：%+v", style, previous, spec)
		}
		seen[spec] = style
		result[style] = spec
	}

	if got := result["player"]; got.highlightStyle != "text" || got.progressStyle != "bar" || !got.showNext {
		t.Fatalf("播放器样式缺少流光文字、连续进度条或下一句：%+v", got)
	}
	if got := result["karaoke"]; got.badge == "" || got.highlightStyle != "karaoke" || got.progressStyle != "segments" {
		t.Fatalf("卡拉 OK 样式缺少演唱标识、逐字高亮或分段进度：%+v", got)
	}
	if got := result["classic"]; !got.divider || got.showHighlight || got.progressStyle != "line" {
		t.Fatalf("经典双行样式应显示分隔线和细线进度，并与编译器一致地关闭流光：%+v", got)
	}
	if got := result["minimal"]; got.showNext || got.showHighlight || got.showProgress {
		t.Fatalf("极简样式仍显示下一句、高亮或进度：%+v", got)
	}
}

func TestLyricPreviewRegressionTransitionAndShadowOptionsDriveHelpers(t *testing.T) {
	lowFraction := caseTheme.Layer{Options: map[string]any{"transition_fraction": .01}}
	highFraction := caseTheme.Layer{Options: map[string]any{"transition_fraction": .35}}
	if low, high := lyricPreviewProgressRatio(lowFraction), lyricPreviewProgressRatio(highFraction); !(low < high) {
		t.Fatalf("transition_fraction 未改变预览进度：low=%v high=%v", low, high)
	}

	next := color.NRGBA{R: 42, G: 96, B: 180, A: 210}
	smooth := lyricPreviewTransitionColor(caseTheme.Layer{Options: map[string]any{"smooth_transition": true}}, next)
	direct := lyricPreviewTransitionColor(caseTheme.Layer{Options: map[string]any{"smooth_transition": false}}, next)
	if smooth.A >= direct.A || direct != next {
		t.Fatalf("smooth_transition 未改变下一句淡入透明度：smooth=%+v direct=%+v", smooth, direct)
	}

	soft := lyricPreviewShadowBlur(caseTheme.Layer{Options: map[string]any{"shadow_blur": 1.0}})
	blurred := lyricPreviewShadowBlur(caseTheme.Layer{Options: map[string]any{"shadow_blur": 4.0}})
	if soft != 1 || blurred != 4 || soft == blurred {
		t.Fatalf("shadow_blur 未改变阴影柔化半径：soft=%d blurred=%d", soft, blurred)
	}
}

func TestLyricVisualPresetsUpdateOnlySelectedLayerAndSurviveJSONRoundTrip(t *testing.T) {
	presets := waveshareVisualPresetsForLayerType(caseTheme.LayerLyrics)
	if len(presets) != 4 {
		t.Fatalf("歌词视觉预设数量应为 4，实际为 %d", len(presets))
	}
	wantIDs := map[string]bool{
		"aurora-player": false, "magenta-karaoke": false, "amber-classic": false, "clean-minimal": false,
	}

	for _, preset := range presets {
		preset := preset
		t.Run(preset.id, func(t *testing.T) {
			if _, known := wantIDs[preset.id]; !known {
				t.Fatalf("出现未纳入回归保护的歌词预设：%q", preset.id)
			}
			wantIDs[preset.id] = true

			decoy := caseTheme.Layer{
				ID: "lyrics-decoy", Type: caseTheme.LayerLyrics,
				Bounds:  caseTheme.Rect{X: .04, Y: .06, Width: .35, Height: .18},
				Opacity: .66, Rotation: -3, ZIndex: 2,
				Options: map[string]any{
					"provider": "qq", "lrc_path": `D:\Music\decoy.lrc`, "visible": true,
					"style": "classic", "current_color": "ABCDEF",
				},
			}
			selected := caseTheme.Layer{
				ID: "lyrics-selected", Type: caseTheme.LayerLyrics,
				Bounds:  caseTheme.Rect{X: .17, Y: .63, Width: .68, Height: .22},
				Opacity: .73, Rotation: 7, ZIndex: 11,
				Options: map[string]any{
					"provider": "netease", "lrc_path": `D:\Music\当前歌曲.lrc`, "visible": false,
					"style": "classic", "font_family": "monospace", "current_color": "102030",
				},
			}
			model := &gioEditor{
				selected: 1,
				manifest: caseTheme.Manifest{ID: "preset-roundtrip", Name: "歌词预设回归", Width: 1024, Height: 600, Layers: []caseTheme.Layer{decoy, selected}},
			}
			decoyBefore := cloneManifest(caseTheme.Manifest{Layers: []caseTheme.Layer{decoy}}).Layers[0]
			geometryBefore := cloneManifest(caseTheme.Manifest{Layers: []caseTheme.Layer{selected}}).Layers[0]

			model.setWaveshareVisualPreset(&model.manifest.Layers[model.selected], preset)
			got := model.manifest.Layers[model.selected]
			if !waveshareVisualPresetMatches(&got, preset) {
				t.Fatalf("选中歌词图层未完整应用预设 %q：%v", preset.id, got.Options)
			}
			if reflect.DeepEqual(got.Options, geometryBefore.Options) {
				t.Fatalf("预设 %q 没有修改选中歌词图层", preset.id)
			}
			assertLyricPresetProtectedState(t, got, geometryBefore)
			if !reflect.DeepEqual(model.manifest.Layers[0], decoyBefore) {
				t.Fatalf("预设 %q 错误修改了未选中的歌词图层：got=%+v want=%+v", preset.id, model.manifest.Layers[0], decoyBefore)
			}

			encoded, err := json.Marshal(model.manifest)
			if err != nil {
				t.Fatalf("编码应用预设后的主题：%v", err)
			}
			var decoded caseTheme.Manifest
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("解码应用预设后的主题：%v", err)
			}
			roundTripped := decoded.Layers[model.selected]
			if !waveshareVisualPresetMatches(&roundTripped, preset) {
				t.Fatalf("JSON 往返后丢失歌词预设 %q：%v", preset.id, roundTripped.Options)
			}
			assertLyricPresetProtectedState(t, roundTripped, geometryBefore)
			if !reflect.DeepEqual(decoded.Layers[0], decoyBefore) {
				t.Fatalf("JSON 往返后未选中歌词图层发生变化：got=%+v want=%+v", decoded.Layers[0], decoyBefore)
			}
		})
	}

	for id, seen := range wantIDs {
		if !seen {
			t.Errorf("缺少歌词视觉预设 %q", id)
		}
	}
}

func assertLyricPresetProtectedState(t *testing.T, got, before caseTheme.Layer) {
	t.Helper()
	if got.ID != before.ID || got.Type != before.Type || got.Bounds != before.Bounds || got.Opacity != before.Opacity || got.Rotation != before.Rotation || got.ZIndex != before.ZIndex {
		t.Fatalf("歌词视觉预设改变了图层身份或几何：got=%+v want=%+v", got, before)
	}
	for _, key := range []string{"provider", "lrc_path", "visible"} {
		if !reflect.DeepEqual(got.Options[key], before.Options[key]) {
			t.Fatalf("歌词视觉预设不应改变 %s：got=%v want=%v", key, got.Options[key], before.Options[key])
		}
	}
}
