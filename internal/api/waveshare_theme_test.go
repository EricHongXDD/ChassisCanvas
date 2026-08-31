package api

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"casescreen/internal/theme"
)

func TestReplaceWaveshareEmbeddedVideo(t *testing.T) {
	directory := t.TempDir()
	oldVideo := []byte{0, 0, 0, 32, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	newVideo := append([]byte(nil), oldVideo...)
	newVideo = append(newVideo, 1, 2, 3, 4, 5)
	oldTheme := append([]byte("header"), byte(len(oldVideo)>>16), byte(len(oldVideo)>>8), byte(len(oldVideo)))
	oldTheme = append(oldTheme, oldVideo...)
	oldPath := filepath.Join(directory, "old.Theme")
	videoPath := filepath.Join(directory, "new.mp4")
	outputPath := filepath.Join(directory, "new.Theme")
	if err := os.WriteFile(oldPath, oldTheme, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(videoPath, newVideo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceWaveshareEmbeddedVideo(oldPath, videoPath, outputPath); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(result[len("header")])<<16 | int(result[len("header")+1])<<8 | int(result[len("header")+2]); got != len(newVideo) {
		t.Fatalf("内嵌视频长度未更新: %d", got)
	}
	if string(result[len(result)-len(newVideo):]) != string(newVideo) {
		t.Fatal("内嵌视频内容未替换")
	}
}

func TestEncodeWaveshareBackgroundUsesExactTargetResolution(t *testing.T) {
	ffmpegPath := filepath.Join("..", "..", "artifacts", "CaseScreen-windows-x64", "ffmpeg.exe")
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skip("发行运行时中没有 FFmpeg")
	}
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.jpg")
	outputPath := filepath.Join(directory, "output.mp4")
	file, err := os.Create(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 320, 320))
	for y := 0; y < 320; y++ {
		for x := 0; x < 320; x++ {
			imageData.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	if err := jpeg.Encode(file, imageData, &jpeg.Options{Quality: 85}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := encodeWaveshareBackground(context.Background(), ffmpegPath, sourcePath, outputPath, 1024, 600, true); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command(ffmpegPath, "-hide_banner", "-i", outputPath, "-f", "null", "-")
	probeOutput, _ := probe.CombinedOutput()
	if !strings.Contains(string(probeOutput), "1024x600") {
		t.Fatalf("生成视频不是目标分辨率：%s", probeOutput)
	}
}

func TestEncodeWaveshareBackgroundPreservesVideoDuration(t *testing.T) {
	ffmpegPath := filepath.Join("..", "..", "artifacts", "CaseScreen-windows-x64", "ffmpeg.exe")
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skip("发行运行时中没有 FFmpeg")
	}
	directory := t.TempDir()
	posterPath := filepath.Join(directory, "poster.jpg")
	poster, err := os.Create(posterPath)
	if err != nil {
		t.Fatal(err)
	}
	posterImage := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			posterImage.SetRGBA(x, y, color.RGBA{R: 30, G: 90, B: 160, A: 255})
		}
	}
	if err := jpeg.Encode(poster, posterImage, &jpeg.Options{Quality: 85}); err != nil {
		_ = poster.Close()
		t.Fatal(err)
	}
	if err := poster.Close(); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(directory, "source.mp4")
	create := exec.Command(ffmpegPath, "-y", "-loop", "1", "-i", posterPath, "-t", "4", "-r", "5", "-an", "-c:v", "mpeg4", sourcePath)
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("创建视频样本失败: %v\n%s", err, output)
	}
	outputPath := filepath.Join(directory, "output.mp4")
	if err := encodeWaveshareBackground(context.Background(), ffmpegPath, sourcePath, outputPath, 320, 180, false); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command(ffmpegPath, "-hide_banner", "-i", outputPath, "-f", "null", "-")
	probeOutput, _ := probe.CombinedOutput()
	match := regexp.MustCompile(`Duration: \d{2}:\d{2}:([0-9.]+)`).FindStringSubmatch(string(probeOutput))
	if len(match) != 2 {
		t.Fatalf("无法读取生成视频时长: %s", probeOutput)
	}
	seconds, err := strconv.ParseFloat(match[1], 64)
	if err != nil || seconds < 3.5 {
		t.Fatalf("真实视频被截短: duration=%q err=%v", match[1], err)
	}
}

func TestDrawWaveshareComponentShellsUsesLayerBounds(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 200, 120))
	manifest := theme.Manifest{Width: 200, Height: 120, Layers: []theme.Layer{
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .5, Y: .1, Width: .45, Height: .3}, Opacity: 1, ZIndex: 2},
		{ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: .05, Y: .5, Width: .35, Height: .45}, Opacity: 1, ZIndex: 3},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .5, Y: .55, Width: .45, Height: .3}, Opacity: 1, ZIndex: 4},
	}}
	drawWaveshareComponentShells(canvas, manifest)
	if got := color.NRGBAModel.Convert(canvas.At(110, 15)).(color.NRGBA); got.A == 0 || got.G <= got.R {
		t.Fatalf("条状频谱没有绘入指定区域: %+v", got)
	}
	if got := color.NRGBAModel.Convert(canvas.At(10, 65)).(color.NRGBA); got.A == 0 {
		t.Fatal("环状频谱面板没有绘入指定区域")
	}
	if got := color.NRGBAModel.Convert(canvas.At(110, 70)).(color.NRGBA); got.A != 0 {
		t.Fatalf("歌词面板仍被永久烘焙，无法在等待时隐藏: %+v", got)
	}
	if got := color.NRGBAModel.Convert(canvas.At(50, 20)).(color.NRGBA); got.A != 0 {
		t.Fatalf("组件外区域被意外覆盖: %+v", got)
	}
}

func TestDrawWaveshareComponentShellRotatesAroundLayerCenter(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 200, 120))
	manifest := theme.Manifest{Width: 200, Height: 120, Layers: []theme.Layer{{
		ID: "shape", Type: theme.LayerShape,
		Bounds:  theme.Rect{X: .25, Y: 1.0 / 3.0, Width: .4, Height: 1.0 / 6.0},
		Opacity: 1, Rotation: 90, ZIndex: 1,
		Options: map[string]any{"shape": "rect", "fill_color": "FF0000FF", "stroke_width": 0},
	}}}
	drawWaveshareComponentShells(canvas, manifest)
	if got := color.NRGBAModel.Convert(canvas.At(95, 20)).(color.NRGBA); got.A == 0 || got.R == 0 {
		t.Fatalf("旋转后的组件壳没有出现在中心旋转位置: %+v", got)
	}
	if got := color.NRGBAModel.Convert(canvas.At(60, 45)).(color.NRGBA); got.A != 0 {
		t.Fatalf("组件壳仍停留在未旋转位置: %+v", got)
	}
}

func TestWaveshareVersionedThemeRemotePathBreaksScreenCache(t *testing.T) {
	original := "/data/theme/1024x600/casescreen-070-astronaut.Theme"
	first := waveshareVersionedThemeRemotePath(original, []byte("first"))
	second := waveshareVersionedThemeRemotePath(first, []byte("second"))
	if first == original || second == original || first == second {
		t.Fatalf("不同主题内容没有生成独立路径: first=%q second=%q", first, second)
	}
	if !strings.HasPrefix(first, "/data/theme/1024x600/casescreen-070-astronaut-v") || !strings.HasSuffix(first, ".Theme") {
		t.Fatalf("主题指纹路径格式错误: %q", first)
	}
	if got := waveshareThemeTemplateRemotePath(first); got != original {
		t.Fatalf("指纹路径没有还原为稳定模板路径: got=%q want=%q", got, original)
	}
	if got := waveshareVersionedThemeRemotePath(first, []byte("first")); got != first {
		t.Fatalf("相同内容没有复用同一路径: got=%q want=%q", got, first)
	}
}

func TestWaveshareBackgroundVideoRejectsUnsupportedDeviceGeometry(t *testing.T) {
	manifest := theme.Manifest{
		Resources: []theme.Resource{{ID: "video", LocalPath: "demo.mp4"}},
		Layers: []theme.Layer{{
			ID: "video", Type: theme.LayerBackgroundVideo, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: -9000,
			Options: map[string]any{"resource_id": "video", "visible": true},
		}},
	}
	layer, resource, found, err := waveshareBackgroundVideo(manifest)
	if err != nil || !found || layer.ID != "video" || resource.ID != "video" {
		t.Fatalf("合法全屏视频未被选中：found=%t layer=%+v resource=%+v err=%v", found, layer, resource, err)
	}

	manifest.Layers[0].Bounds.Width = .8
	if _, _, _, err := waveshareBackgroundVideo(manifest); err == nil || !strings.Contains(err.Error(), "全屏") {
		t.Fatalf("非全屏视频未被拒绝：%v", err)
	}

	manifest.Layers[0].Bounds.Width = 1
	manifest.Layers = append(manifest.Layers, theme.Layer{
		ID: "video-2", Type: theme.LayerBackgroundVideo, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: -8999,
		Options: map[string]any{"resource_id": "video", "visible": true},
	})
	if _, _, _, err := waveshareBackgroundVideo(manifest); err == nil || !strings.Contains(err.Error(), "一个可见视频") {
		t.Fatalf("多个可见视频未被拒绝：%v", err)
	}
}

func TestWaveshareBackgroundSelectionSkipsHiddenAndTransparentLayers(t *testing.T) {
	manifest := theme.Manifest{
		Resources: []theme.Resource{
			{ID: "hidden", LocalPath: "hidden.jpg"},
			{ID: "transparent", LocalPath: "transparent.jpg"},
			{ID: "visible", LocalPath: "visible.jpg"},
		},
		Layers: []theme.Layer{
			{ID: "hidden", Type: theme.LayerBackgroundImage, Opacity: 1, Options: map[string]any{"resource_id": "hidden", "visible": false}},
			{ID: "transparent", Type: theme.LayerBackgroundImage, Opacity: 0, Options: map[string]any{"resource_id": "transparent", "visible": true}},
			{ID: "visible", Type: theme.LayerBackgroundImage, Opacity: 1, Options: map[string]any{"resource_id": "visible", "visible": true}},
		},
	}
	resource, found := waveshareBackgroundResource(manifest)
	if !found || resource.ID != "visible" {
		t.Fatalf("背景资源选择未跳过隐藏/全透明图层：found=%t resource=%+v", found, resource)
	}
}
