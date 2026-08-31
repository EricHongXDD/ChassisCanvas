package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"

	"casescreen/internal/audio"
	"casescreen/internal/render"
	"casescreen/internal/theme"
)

func main() {
	output := filepath.Join("output", "visual-preview-1.0.png")
	if len(os.Args) > 1 {
		output = os.Args[1]
	}
	manifest := previewManifest()
	renderer, err := render.New(manifest)
	if err != nil {
		panic(err)
	}
	defer renderer.Close()
	bands := []float64{
		.18, .32, .86, 1.0, .72, .38, .18, .08, .05, .11, .22, .46,
		.64, .39, .18, .08, .05, .09, .15, .28, .47, .31, .12, .06,
		.04, .05, .12, .24, .78, .92, .64, .26, .08, .05, .07, .13,
		.24, .58, .74, .41, .16, .07, .05, .06, .12, .35, .24, .08,
	}
	frame, err := renderer.Render(1024, 600, render.State{
		Spectrum: audio.SpectrumFrame{Bars: bands, Level: .72},
		Lyrics: render.LyricState{
			Current: "穿过星海，让旋律继续发光", Next: "Every beat becomes a little universe",
			Progress: .58, PositionKnown: true, Playing: true,
		},
	})
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		panic(err)
	}
	file, err := os.Create(output)
	if err != nil {
		panic(err)
	}
	if err := png.Encode(file, frame); err != nil {
		_ = file.Close()
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
	fmt.Println(output)
}

func previewManifest() theme.Manifest {
	commonAudio := map[string]any{
		"source": "system", "gain": 1.2, "sensitivity": .9, "min_amplitude": 0.0, "max_amplitude": 1.0,
		"smoothing": 0.0, "noise_gate": .014, "contrast": 2.2, "separation": .76,
		"bass_boost": 1.45, "floor_reduction": .66, "glow": true, "glow_radius": 7.0,
	}
	barsOptions := cloneOptions(commonAudio)
	for key, value := range map[string]any{
		"style": "neon", "bar_shape": "capsule", "direction": "bottom", "count": 48, "gap": 3, "thickness": 3,
		"gradient_start": "FF54C8", "gradient_end": "8B5CF6", "panel_color": "00000000",
	} {
		barsOptions[key] = value
	}
	ringOptions := cloneOptions(commonAudio)
	for key, value := range map[string]any{
		"style": "waveform", "direction": "outward", "count": 48, "thickness": 1, "inner_radius": .62,
		"gradient_start": "55E5FF", "gradient_end": "FF54C8", "panel_color": "00000000",
	} {
		ringOptions[key] = value
	}
	return theme.Manifest{
		ID: "visual-preview-1.0", Name: "歌词与频谱视觉验收", Width: 1024, Height: 600,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: -100, Options: map[string]any{"color": "050A14"}},
			{ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: .66, Y: .07, Width: .29, Height: .50}, Opacity: .96, ZIndex: 10, Options: ringOptions},
			{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .05, Y: .67, Width: .90, Height: .25}, Opacity: .94, ZIndex: 20, Options: barsOptions},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .07, Y: .24, Width: .58, Height: .29}, Opacity: 1, ZIndex: 30, Options: map[string]any{
				"style": "player", "font_family": "source-han", "font_weight": "bold", "align": "center",
				"font_size": 38.0, "next_font_size": 20.0,
				"current_color": "F7FCFF", "next_color": "B8C8DAA4", "accent_color": "55E5FF",
				"panel_color": "030A12B5", "panel_border_color": "66E3FF50", "corner_radius": 18,
				"outline_color": "02070EF0", "outline_width": 2, "shadow": true, "shadow_color": "000000C8",
				"shadow_offset_x": 2, "shadow_offset_y": 4, "shadow_blur": 3, "glow_color": "53DAFF55", "glow_radius": 5,
				"karaoke_highlight": true, "smooth_transition": true, "show_progress": true, "progress_thickness": 3,
			}},
		},
	}
}

func cloneOptions(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
