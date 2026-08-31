package render

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"casescreen/internal/audio"
	"casescreen/internal/theme"
)

func TestBundledTemplatesRenderDynamicLayers(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "themes", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	bars := make([]float64, 32)
	for index := range bars {
		bars[index] = 0.1 + 0.9*math.Abs(math.Sin(float64(index)*0.41))
	}
	state := State{
		TelemetryEnabled: true,
		Spectrum:         audio.SpectrumFrame{Level: 0.7, Bars: bars},
		Lyrics:           LyricState{Current: "正在播放的歌词", Next: "下一句歌词", Progress: 0.45},
	}
	for _, path := range paths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var manifest theme.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			renderer, err := New(manifest)
			if err != nil {
				t.Fatal(err)
			}
			defer renderer.Close()
			frame, err := renderer.Render(manifest.Width, manifest.Height, state)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Bounds().Dx() != manifest.Width || frame.Bounds().Dy() != manifest.Height {
				t.Fatalf("模板渲染尺寸错误: %v", frame.Bounds())
			}
		})
	}
}
