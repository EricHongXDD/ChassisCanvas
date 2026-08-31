package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

const waveshareStandaloneCanvasColor = "06151B"

// BuildWaveshareStandaloneLyricsTheme 生成可直接写入微雪 TF 卡的深色歌词主题。
// 背景必须由 FFmpeg 重新编码并替换，任何一步失败都会返回错误，绝不回退到
// 基础模板原有的视频。
func BuildWaveshareStandaloneLyricsTheme(ctx context.Context, ffmpegPath string, width, height int) ([]byte, error) {
	manifest, err := waveshareStandaloneLyricsManifest(width, height)
	if err != nil {
		return nil, err
	}
	ffmpegPath, err = resolveWaveshareThemeFFmpeg(ffmpegPath)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseTheme, err := resolveWaveshareStandaloneBaseTheme()
	if err != nil {
		return nil, err
	}

	directory, err := os.MkdirTemp("", "casescreen-waveshare-theme-")
	if err != nil {
		return nil, fmt.Errorf("创建微雪主题临时目录失败: %w", err)
	}
	defer os.RemoveAll(directory)
	scenePath := filepath.Join(directory, "dark-canvas.jpg")
	videoPath := filepath.Join(directory, "dark-canvas.mp4")
	if err := renderWaveshareStaticScene(manifest, scenePath); err != nil {
		return nil, fmt.Errorf("渲染微雪深色画布失败: %w", err)
	}
	if err := encodeWaveshareBackground(ctx, ffmpegPath, scenePath, videoPath, width, height, true); err != nil {
		return nil, fmt.Errorf("编码微雪深色背景失败: %w", err)
	}
	video, err := os.ReadFile(videoPath)
	if err != nil {
		return nil, fmt.Errorf("读取微雪深色背景失败: %w", err)
	}
	if err := validateWaveshareMP4(video); err != nil {
		return nil, fmt.Errorf("验证微雪深色背景失败: %w", err)
	}
	originalVideo, err := extractWaveshareEmbeddedVideo(baseTheme)
	if err != nil {
		return nil, fmt.Errorf("读取微雪基础模板背景失败: %w", err)
	}
	if sha256.Sum256(originalVideo) == sha256.Sum256(video) {
		return nil, errors.New("深色背景编码结果仍与基础模板视频相同")
	}

	withDarkVideo, err := replaceWaveshareEmbeddedVideoData(baseTheme, video)
	if err != nil {
		return nil, fmt.Errorf("替换微雪深色背景失败: %w", err)
	}
	compiled, err := compileWaveshareThemeWidgets(withDarkVideo, manifest)
	if err != nil {
		return nil, fmt.Errorf("编译微雪歌词控件失败: %w", err)
	}
	if err := validateWaveshareStandaloneLyricsTheme(compiled, width, height); err != nil {
		return nil, err
	}
	compiledVideo, err := extractWaveshareEmbeddedVideo(compiled)
	if err != nil {
		return nil, fmt.Errorf("读取编译后的微雪背景失败: %w", err)
	}
	if sha256.Sum256(compiledVideo) != sha256.Sum256(video) {
		return nil, errors.New("编译微雪控件时深色背景被意外改写")
	}
	return compiled, nil
}

func resolveWaveshareStandaloneBaseTheme() ([]byte, error) {
	candidates := []string{
		strings.TrimSpace(os.Getenv("CASESCREEN_WAVESHARE_BASE_THEME")),
		filepath.Join(".tmp", "waveshare", "070-astronaut.Theme"),
		filepath.Join("themes", "070-astronaut.Theme"),
		filepath.Join("themes", "waveshare", "070-astronaut.Theme"),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		data, err := os.ReadFile(candidate)
		if err == nil && waveshareThemeHasRequiredTemplates(data) {
			return data, nil
		}
	}
	return nil, errors.New("未找到可用的微雪基础 Theme；请通过 CASESCREEN_WAVESHARE_BASE_THEME 指定从你拥有的设备或原厂软件取得的 070-astronaut.Theme")
}

func waveshareStandaloneLyricsManifest(width, height int) (theme.Manifest, error) {
	if width <= 0 || height <= 0 {
		return theme.Manifest{}, errors.New("微雪歌词主题分辨率无效")
	}
	lyricsY, lyricsHeight := 0.61, 0.29
	fontSize, nextFontSize := max(18, min(52, height/14)), max(16, min(40, height/18))
	if height > width {
		lyricsY, lyricsHeight = 0.70, 0.20
		fontSize, nextFontSize = max(18, min(48, width/11)), max(16, min(36, width/14))
	}
	manifest := theme.Manifest{
		ID:     fmt.Sprintf("casescreen-standalone-lyrics-%dx%d", width, height),
		Name:   "CaseScreen 独立歌词",
		Width:  width,
		Height: height,
		Layers: []theme.Layer{
			{
				ID: "canvas", Type: theme.LayerCanvas,
				Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: -10000,
				Options: map[string]any{"color": waveshareStandaloneCanvasColor, "visible": true},
			},
			{
				ID: "lyrics", Type: theme.LayerLyrics,
				Bounds:  theme.Rect{X: 0.06, Y: lyricsY, Width: 0.88, Height: lyricsHeight},
				Opacity: 1, ZIndex: 10,
				Options: map[string]any{
					"visible":             true,
					"placeholder_current": "正在等待歌词",
					"placeholder_next":    "播放歌曲后显示下一句",
					"font_size":           fontSize,
					"next_font_size":      nextFontSize,
					"current_color":       "F5FBFF",
					"next_color":          "A8C4CA",
					"panel_color":         "081E29E6",
					"corner_radius":       12,
				},
			},
		},
	}
	if err := manifest.Validate(); err != nil {
		return theme.Manifest{}, fmt.Errorf("微雪歌词主题清单无效: %w", err)
	}
	return manifest, nil
}

func resolveWaveshareThemeFFmpeg(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		resolved, err := exec.LookPath("ffmpeg")
		if err != nil {
			return "", errors.New("未找到 FFmpeg；请通过 --ffmpeg 指定 ffmpeg.exe")
		}
		return resolved, nil
	}
	if strings.ContainsAny(value, `/\\`) || filepath.IsAbs(value) {
		info, err := os.Stat(value)
		if err != nil {
			return "", fmt.Errorf("FFmpeg 不存在: %w", err)
		}
		if info.IsDir() {
			return "", errors.New("FFmpeg 路径不能是目录")
		}
		return value, nil
	}
	resolved, err := exec.LookPath(value)
	if err != nil {
		return "", fmt.Errorf("未找到 FFmpeg %q: %w", value, err)
	}
	return resolved, nil
}

func validateWaveshareStandaloneLyricsTheme(data []byte, width, height int) error {
	table, err := parseWaveshareThemeWidgetTable(data)
	if err != nil {
		return fmt.Errorf("解析生成的微雪歌词主题失败: %w", err)
	}
	if table.canvas["canvas_w"] != fmt.Sprintf("%d", width) || table.canvas["canvas_h"] != fmt.Sprintf("%d", height) {
		return fmt.Errorf("生成的微雪歌词主题分辨率不匹配: %sx%s", table.canvas["canvas_w"], table.canvas["canvas_h"])
	}
	wanted := map[string]int{"CaseScreen_LyricsCurrent": 0, "CaseScreen_LyricsNext": 0}
	progressWidgets := 0
	visibilityWidgets := 0
	backgrounds := 0
	for _, widget := range table.widgets {
		if widget.fields["type"] == "100" {
			backgrounds++
			continue
		}
		key := widget.fields["system_data_name"]
		if widget.fields["type"] == "102" {
			switch key {
			case telemetry.LyricsProgressFineKey:
				if widget.fields["back_color"] != "r=0,g=0,b=0,a=0" || widget.fields["border_width"] != "0" {
					return fmt.Errorf("生成的微雪歌词进度控件会在零进度时残留底轨: %s", widget.fields["itemName"])
				}
				progressWidgets++
				continue
			case telemetry.LyricsVisibleKey:
				if widget.fields["back_color"] != "r=0,g=0,b=0,a=0" || widget.fields["border_width"] != "0" ||
					widget.fields["system_data_min_value"] != "0" || widget.fields["system_data_max_value"] != "1" {
					return fmt.Errorf("生成的微雪歌词可见性控件不是透明背景的 0/1 标量: %s", widget.fields["itemName"])
				}
				visibilityWidgets++
				continue
			}
		}
		if widget.fields["type"] != "8" {
			return fmt.Errorf("生成的微雪歌词主题包含非文字前景控件: type=%s", widget.fields["type"])
		}
		if _, ok := wanted[key]; !ok {
			return fmt.Errorf("生成的微雪歌词主题包含无关动态键: %s", key)
		}
		if widget.fields["text_str"] != "" {
			return fmt.Errorf("生成的微雪歌词文字带有初始占位内容: %s", widget.fields["itemName"])
		}
		wanted[key]++
	}
	// 粗体歌词会为当前句增加四个同键值的 1px 膨胀副本，仍然只消费原有两个动态键。
	if backgrounds != 1 || progressWidgets != 1 || visibilityWidgets < 1 || len(table.widgets) < 5 || len(table.widgets) > 64 {
		return fmt.Errorf("生成的微雪歌词主题控件数量异常: 背景=%d 进度=%d 可见性=%d 总数=%d", backgrounds, progressWidgets, visibilityWidgets, len(table.widgets))
	}
	for key, count := range wanted {
		if count < 1 {
			return fmt.Errorf("生成的微雪歌词主题缺少动态键 %s", key)
		}
	}
	return nil
}

func extractWaveshareEmbeddedVideo(themeData []byte) ([]byte, error) {
	ftyp := bytes.LastIndex(themeData, []byte("ftyp"))
	if ftyp < 4 {
		return nil, errors.New("微雪 .Theme 中未找到内嵌 MP4")
	}
	videoStart := ftyp - 4
	if videoStart < 3 || videoStart+4 > len(themeData) {
		return nil, errors.New("微雪 .Theme 内嵌视频位置无效")
	}
	videoLength := int(themeData[videoStart-3])<<16 | int(themeData[videoStart-2])<<8 | int(themeData[videoStart-1])
	if videoLength <= 0 || videoStart+videoLength > len(themeData) {
		return nil, errors.New("微雪 .Theme 内嵌视频长度无效")
	}
	video := append([]byte(nil), themeData[videoStart:videoStart+videoLength]...)
	if err := validateWaveshareMP4(video); err != nil {
		return nil, err
	}
	return video, nil
}

func replaceWaveshareEmbeddedVideoData(themeData, video []byte) ([]byte, error) {
	if len(video) > 0xFFFFFF {
		return nil, fmt.Errorf("内嵌视频过大（%d 字节，微雪主题包最多支持 16777215 字节）", len(video))
	}
	if err := validateWaveshareMP4(video); err != nil {
		return nil, err
	}
	oldVideo, err := extractWaveshareEmbeddedVideo(themeData)
	if err != nil {
		return nil, err
	}
	ftyp := bytes.LastIndex(themeData, []byte("ftyp"))
	videoStart := ftyp - 4
	length := []byte{byte(len(video) >> 16), byte(len(video) >> 8), byte(len(video))}
	result := make([]byte, 0, len(themeData)-len(oldVideo)+len(video))
	result = append(result, themeData[:videoStart-3]...)
	result = append(result, length...)
	result = append(result, video...)
	result = append(result, themeData[videoStart+len(oldVideo):]...)
	return result, nil
}

func validateWaveshareMP4(data []byte) error {
	if len(data) < 24 {
		return errors.New("背景文件不是有效的 MP4")
	}
	found := map[string]bool{}
	for cursor := 0; cursor < len(data); {
		if cursor+8 > len(data) {
			return errors.New("MP4 顶层 box 不完整")
		}
		size := uint64(binary.BigEndian.Uint32(data[cursor : cursor+4]))
		boxType := string(data[cursor+4 : cursor+8])
		headerSize := uint64(8)
		if size == 1 {
			if cursor+16 > len(data) {
				return errors.New("MP4 扩展 box 不完整")
			}
			size = binary.BigEndian.Uint64(data[cursor+8 : cursor+16])
			headerSize = 16
		} else if size == 0 {
			size = uint64(len(data) - cursor)
		}
		if size < headerSize || uint64(cursor)+size > uint64(len(data)) {
			return fmt.Errorf("MP4 box %q 长度无效", boxType)
		}
		found[boxType] = true
		cursor += int(size)
	}
	for _, boxType := range []string{"ftyp", "moov", "mdat"} {
		if !found[boxType] {
			return fmt.Errorf("MP4 缺少 %s box", boxType)
		}
	}
	return nil
}
