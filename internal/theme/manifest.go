package theme

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

type LayerType string

const (
	LayerCanvas          LayerType = "canvas"
	LayerImage           LayerType = "image"
	LayerBackgroundImage LayerType = "background_image"
	LayerBackgroundVideo LayerType = "background_video"
	LayerTelemetry       LayerType = "telemetry"
	LayerAudioBars       LayerType = "audio_bars"
	LayerAudioRing       LayerType = "audio_ring"
	LayerLyrics          LayerType = "lyrics"
	LayerText            LayerType = "text"
	LayerClock           LayerType = "clock"
	LayerProgressBar     LayerType = "progress_bar"
	LayerGauge           LayerType = "gauge"
	LayerGradientGauge   LayerType = "gradient_gauge"
	LayerLineChart       LayerType = "line_chart"
	LayerShape           LayerType = "shape"
	// CurrentWaveshareCompilerRevision 标识驻留 Theme 的可见性编译契约。
	// 版本 1 起，歌词文字初始为空，面板、装饰和进度底轨都由 LV 控制。
	CurrentWaveshareCompilerRevision = 1
)

// Rect 使用 0..1 归一化坐标，同一主题可自适应不同分辨率。
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type Layer struct {
	ID       string         `json:"id"`
	Type     LayerType      `json:"type"`
	Bounds   Rect           `json:"bounds"`
	Opacity  float64        `json:"opacity"`
	Rotation float64        `json:"rotation,omitempty"`
	ZIndex   int            `json:"z_index"`
	Options  map[string]any `json:"options,omitempty"`
}

type Resource struct {
	ID         string `json:"id"`
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path,omitempty"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

type Manifest struct {
	ID                        string     `json:"id"`
	Name                      string     `json:"name"`
	Width                     int        `json:"width"`
	Height                    int        `json:"height"`
	WaveshareCompilerRevision int        `json:"waveshare_compiler_revision,omitempty"`
	Resources                 []Resource `json:"resources"`
	Layers                    []Layer    `json:"layers"`
}

// NeedsWaveshareCompilerUpgrade 只对含歌词的微雪驻留主题生效。主机合成
// 主题不依赖这个版本字段；调用方必须先确认目标设备确实是微雪。
func (manifest Manifest) NeedsWaveshareCompilerUpgrade() bool {
	if manifest.WaveshareCompilerRevision >= CurrentWaveshareCompilerRevision {
		return false
	}
	for _, layer := range manifest.Layers {
		if layer.Type == LayerLyrics {
			return true
		}
	}
	return false
}

func (manifest Manifest) Validate() error {
	if manifest.ID == "" || manifest.Name == "" {
		return errors.New("主题 ID 和名称不能为空")
	}
	if manifest.Width <= 0 || manifest.Height <= 0 {
		return errors.New("主题分辨率无效")
	}
	canvases := 0
	legacyBackgrounds := 0
	layerIDs := make(map[string]bool, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		if layerIDs[layer.ID] {
			return fmt.Errorf("图层 ID 重复: %q", layer.ID)
		}
		layerIDs[layer.ID] = true
		if err := validateLayer(layer); err != nil {
			return fmt.Errorf("图层 %q: %w", layer.ID, err)
		}
		if layer.Type == LayerCanvas {
			canvases++
		}
		if layer.Type == LayerBackgroundImage || layer.Type == LayerBackgroundVideo {
			legacyBackgrounds++
		}
	}
	if canvases > 1 {
		return errors.New("主题只能有一个画布背景图层")
	}
	// 继续接受旧 JSON，载入后由 NormalizeScene 自动迁移为画布背景和独立图片图层。
	if canvases == 0 && legacyBackgrounds != 1 {
		return errors.New("主题必须有一个画布背景图层")
	}
	resourceIDs := make(map[string]bool, len(manifest.Resources))
	for _, resource := range manifest.Resources {
		if resource.ID == "" || resource.LocalPath == "" {
			return errors.New("主题资源 ID 和本地路径不能为空")
		}
		if resourceIDs[resource.ID] {
			return fmt.Errorf("主题资源 ID 重复: %q", resource.ID)
		}
		resourceIDs[resource.ID] = true
	}
	return nil
}

func validateLayer(layer Layer) error {
	if layer.ID == "" {
		return errors.New("图层 ID 不能为空")
	}
	if layer.Opacity < 0 || layer.Opacity > 1 {
		return errors.New("透明度必须在 0..1 之间")
	}
	if math.IsNaN(layer.Rotation) || math.IsInf(layer.Rotation, 0) {
		return errors.New("旋转角度无效")
	}
	switch layer.Type {
	case LayerCanvas, LayerImage, LayerBackgroundImage, LayerBackgroundVideo, LayerTelemetry, LayerAudioBars, LayerAudioRing, LayerLyrics,
		LayerText, LayerClock, LayerProgressBar, LayerGauge, LayerGradientGauge, LayerLineChart, LayerShape:
	default:
		return fmt.Errorf("不支持的图层类型: %q", layer.Type)
	}
	bounds := layer.Bounds
	if layer.Type == LayerImage {
		if bounds.Width <= 0 || bounds.Height <= 0 || bounds.Width > 4 || bounds.Height > 4 || bounds.X >= 1 || bounds.Y >= 1 || bounds.X+bounds.Width <= 0 || bounds.Y+bounds.Height <= 0 {
			return errors.New("图片图层必须与画布相交且尺寸不能超过画布的 4 倍")
		}
	} else if bounds.X < 0 || bounds.Y < 0 || bounds.Width <= 0 || bounds.Height <= 0 || bounds.X+bounds.Width > 1 || bounds.Y+bounds.Height > 1 {
		return errors.New("归一化坐标超出画布")
	}
	if layer.Type != LayerCanvas && layer.Type != LayerImage && layer.Type != LayerBackgroundImage && layer.Type != LayerBackgroundVideo {
		coverage := bounds.Width * bounds.Height
		if coverage >= 0.92 && layer.Opacity >= 0.92 {
			return errors.New("前景控件不得以不透明方式覆盖整个背景")
		}
	}
	return nil
}

// NormalizeScene 把旧版“全屏背景图片”模型迁移为画布底色和独立图片图层。
// 新模型的画布只表示透明、黑色或白色，图片尺寸和旋转均由各自图层保存。
func NormalizeScene(manifest Manifest) Manifest {
	hasCanvas := false
	for index := range manifest.Layers {
		layer := &manifest.Layers[index]
		if layer.Type == LayerAudioBars || layer.Type == LayerAudioRing {
			if layer.Options == nil {
				layer.Options = map[string]any{}
			}
			// 屏端麦克风容易采到风扇与机箱底噪。旧清单重新保存时
			// 一律迁移为 Windows WASAPI 系统回放频谱。
			layer.Options["source"] = "system"
			setLayerOptionDefault(layer.Options, "gain", DefaultAudioGain)
			setLayerOptionDefault(layer.Options, "sensitivity", DefaultAudioSensitivity)
			setLayerOptionDefault(layer.Options, "min_amplitude", DefaultAudioMinAmplitude)
			setLayerOptionDefault(layer.Options, "max_amplitude", DefaultAudioMaxAmplitude)
			setLayerOptionDefault(layer.Options, "smoothing", DefaultAudioSmoothing)
			setLayerOptionDefault(layer.Options, "noise_gate", DefaultAudioNoiseGate)
			setLayerOptionDefault(layer.Options, "contrast", DefaultAudioContrast)
			setLayerOptionDefault(layer.Options, "separation", DefaultAudioSeparation)
			setLayerOptionDefault(layer.Options, "bass_boost", DefaultAudioBassBoost)
			setLayerOptionDefault(layer.Options, "floor_reduction", DefaultAudioFloorReduce)
			setLayerOptionDefault(layer.Options, "gradient_start", "4CE0FF")
			setLayerOptionDefault(layer.Options, "gradient_end", "8B5CF6")
			setLayerOptionDefault(layer.Options, "panel_color", "00000000")
			setLayerOptionDefault(layer.Options, "glow", true)
			setLayerOptionDefault(layer.Options, "glow_radius", 6)
			if layer.Type == LayerAudioBars {
				setLayerOptionDefault(layer.Options, "style", "neon")
				setLayerOptionDefault(layer.Options, "direction", "bottom")
				setLayerOptionDefault(layer.Options, "count", DefaultAudioBarsCount)
				setLayerOptionDefault(layer.Options, "gap", 5)
				setLayerOptionDefault(layer.Options, "bar_shape", "pill")
				setLayerOptionDefault(layer.Options, "thickness", 3)
			} else {
				setLayerOptionDefault(layer.Options, "style", "waveform")
				setLayerOptionDefault(layer.Options, "direction", "outward")
				setLayerOptionDefault(layer.Options, "count", DefaultAudioRingCount)
				setLayerOptionDefault(layer.Options, "inner_radius", .62)
				setLayerOptionDefault(layer.Options, "thickness", 1)
			}
		}
		if layer.Type == LayerLyrics {
			if layer.Options == nil {
				layer.Options = map[string]any{}
			}
			setLayerOptionDefault(layer.Options, "style", "player")
			lyricsStyle := LyricsStyle(*layer)
			playerStyle := lyricsStyle == LyricsStylePlayer || lyricsStyle == LyricsStyleKaraoke
			setLayerOptionDefault(layer.Options, "font_family", "source-han")
			setLayerOptionDefault(layer.Options, "font_weight", "bold")
			setLayerOptionDefault(layer.Options, "font_size", DefaultLyricsFontSize)
			setLayerOptionDefault(layer.Options, "next_font_size", DefaultLyricsNextFontSize)
			setLayerOptionDefault(layer.Options, "align", "center")
			setLayerOptionDefault(layer.Options, "current_color", "F7FCFF")
			setLayerOptionDefault(layer.Options, "next_color", "B8C8DAA4")
			setLayerOptionDefault(layer.Options, "accent_color", "55E5FF")
			setLayerOptionDefault(layer.Options, "outline_color", "02070EE1")
			setLayerOptionDefault(layer.Options, "outline_width", 2)
			setLayerOptionDefault(layer.Options, "shadow", true)
			setLayerOptionDefault(layer.Options, "shadow_color", "000000B4")
			setLayerOptionDefault(layer.Options, "shadow_offset_x", 2)
			setLayerOptionDefault(layer.Options, "shadow_offset_y", 3)
			setLayerOptionDefault(layer.Options, "shadow_blur", 3)
			setLayerOptionDefault(layer.Options, "glow_color", "53DAFF42")
			setLayerOptionDefault(layer.Options, "glow_radius", 4)
			setLayerOptionDefault(layer.Options, "karaoke_highlight", playerStyle)
			setLayerOptionDefault(layer.Options, "smooth_transition", playerStyle)
			setLayerOptionDefault(layer.Options, "show_progress", playerStyle)
		}
		if layer.Type == LayerCanvas {
			hasCanvas = true
			layer.Bounds = Rect{Width: 1, Height: 1}
			layer.Opacity = 1
			layer.Rotation = 0
			if layer.Options == nil {
				layer.Options = map[string]any{}
			}
			if _, ok := layer.Options["color"]; !ok {
				layer.Options["color"] = "transparent"
			}
		}
	}
	if !hasCanvas {
		canvasID := "canvas"
		for suffix := 2; ; suffix++ {
			available := true
			for _, layer := range manifest.Layers {
				if layer.ID == canvasID {
					available = false
					break
				}
			}
			if available {
				break
			}
			canvasID = fmt.Sprintf("canvas-%d", suffix)
		}
		canvas := Layer{ID: canvasID, Type: LayerCanvas, Bounds: Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: -10000, Options: map[string]any{"color": "transparent"}}
		manifest.Layers = append([]Layer{canvas}, manifest.Layers...)
	}
	for index := range manifest.Layers {
		if manifest.Layers[index].Type == LayerBackgroundImage {
			manifest.Layers[index].Type = LayerImage
			if manifest.Layers[index].Options == nil {
				manifest.Layers[index].Options = map[string]any{}
			}
			manifest.Layers[index].Options["migrated_legacy_background"] = true
			if manifest.Layers[index].ID == "background" {
				manifest.Layers[index].ID = "image"
			}
		}
	}
	return manifest
}

func setLayerOptionDefault(options map[string]any, key string, value any) {
	if _, exists := options[key]; !exists {
		options[key] = value
	}
}

// InspectResource 生成资源指纹，设备端已有相同指纹时可跳过上传。
func InspectResource(id, localPath, remotePath string) (Resource, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return Resource{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Resource{}, err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return Resource{}, err
	}
	return Resource{
		ID:         id,
		LocalPath:  filepath.Clean(localPath),
		RemotePath: remotePath,
		SHA256:     hex.EncodeToString(digest.Sum(nil)),
		Size:       info.Size(),
	}, nil
}
