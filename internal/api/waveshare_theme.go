package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	caseRender "casescreen/internal/render"
	"casescreen/internal/theme"
)

// prepareWaveshareTheme 将可视化编辑器选择的背景图片写入微雪 .Theme 包。
// 微雪主题包的尾部保存了一个带三字节长度前缀的 MP4，屏端只会播放这个内嵌视频。
func (server *Server) prepareWaveshareTheme(ctx context.Context, manifest theme.Manifest) (theme.Manifest, error) {
	themeIndex := -1
	for index, resource := range manifest.Resources {
		if strings.EqualFold(filepath.Ext(resource.RemotePath), ".Theme") {
			themeIndex = index
			break
		}
	}
	if themeIndex < 0 {
		return manifest, nil
	}
	// 新编译的驻留主题使用每主题确定性的短频谱键。旧主题只有在用户
	// 再次应用时才迁移，因此升级过程中主机数据不会与 TF 卡资源失配。
	manifest = theme.AssignWaveshareAudioPrefixes(manifest)
	if err := os.MkdirAll(filepath.Join(server.dataDirectory, "theme-assets"), 0o700); err != nil {
		return manifest, fmt.Errorf("创建微雪主题缓存目录失败: %w", err)
	}
	originalThemePath, err := server.ensureWaveshareThemeSource(manifest.Resources[themeIndex])
	if err != nil {
		return manifest, fmt.Errorf("准备微雪原始主题模板失败: %w", err)
	}
	workingThemePath := originalThemePath
	ffmpegPath := server.findFFmpegPath()
	if ffmpegPath == "" {
		return manifest, errors.New("微雪屏需要 FFmpeg 将画布合成为驻留视频；请在设备与运行时配置中填写 ffmpeg.exe 路径")
	}
	ffmpegSource := ""
	videoOverlay := ""
	videoFit := "stretch"
	stillImage := true
	videoLayer, background, hasVideo, videoErr := waveshareBackgroundVideo(manifest)
	if videoErr != nil {
		return manifest, videoErr
	}
	if hasVideo && background.LocalPath != "" && waveshareVideoPath(background.LocalPath) {
		ffmpegSource, stillImage = background.LocalPath, false
		videoFit = waveshareOptionString(videoLayer.Options, "fit", "cover")
		videoOverlay = filepath.Join(server.dataDirectory, "theme-assets", "waveshare-overlay-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".png")
		if err := renderWaveshareStaticOverlay(manifest, videoOverlay); err != nil {
			return manifest, fmt.Errorf("合成微雪视频叠加层失败: %w", err)
		}
		defer os.Remove(videoOverlay)
	} else {
		ffmpegSource = filepath.Join(server.dataDirectory, "theme-assets", "waveshare-scene-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".jpg")
		if err := renderWaveshareStaticScene(manifest, ffmpegSource); err != nil {
			return manifest, fmt.Errorf("合成微雪画布失败: %w", err)
		}
		defer os.Remove(ffmpegSource)
	}
	videoPath := filepath.Join(server.dataDirectory, "theme-assets", "waveshare-background-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".mp4")
	if err := encodeWaveshareBackgroundWithOptions(ctx, ffmpegPath, ffmpegSource, videoOverlay, videoPath, manifest.Width, manifest.Height, stillImage, videoFit); err != nil {
		return manifest, fmt.Errorf("生成微雪背景视频失败: %w", err)
	}
	defer os.Remove(videoPath)
	workingThemePath = filepath.Join(server.dataDirectory, "theme-assets", "waveshare-video-theme-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".Theme")
	if err := replaceWaveshareEmbeddedVideo(originalThemePath, videoPath, workingThemePath); err != nil {
		return manifest, fmt.Errorf("更新微雪 .Theme 背景失败: %w", err)
	}
	defer os.Remove(workingThemePath)

	themeData, err := os.ReadFile(workingThemePath)
	if err != nil {
		return manifest, fmt.Errorf("读取微雪 .Theme: %w", err)
	}
	filteredTheme, err := compileWaveshareThemeWidgets(themeData, manifest)
	if err != nil {
		return manifest, fmt.Errorf("编译微雪 .Theme 控件失败: %w", err)
	}
	digest := sha256.Sum256(filteredTheme)
	patchedPath := filepath.Join(server.dataDirectory, "theme-assets", fmt.Sprintf("waveshare-theme-%x.Theme", digest[:12]))
	if err := os.WriteFile(patchedPath, filteredTheme, 0o600); err != nil {
		return manifest, fmt.Errorf("写入微雪 .Theme: %w", err)
	}
	// 屏端 PCMonitorLinux 会按主题路径缓存已经解析的控件树。覆盖同名文件后即使
	// 背景资源变化，旧控件仍可能留在内存中；内容指纹路径会强制它重建主题。
	remotePath := waveshareVersionedThemeRemotePath(manifest.Resources[themeIndex].RemotePath, filteredTheme)
	patched, err := theme.InspectResource(manifest.Resources[themeIndex].ID, patchedPath, remotePath)
	if err != nil {
		return manifest, fmt.Errorf("读取更新后的微雪主题失败: %w", err)
	}
	manifest.Resources[themeIndex] = patched
	manifest.WaveshareCompilerRevision = theme.CurrentWaveshareCompilerRevision
	return manifest, nil
}

// ensureWaveshareBaseThemeResource 只为微雪目标补齐屏端主题包。可视化编辑器新建
// 主题时通常只有图片资源；没有 .Theme 时屏端无法创建任何原生控件。
func (server *Server) ensureWaveshareBaseThemeResource(manifest theme.Manifest) (theme.Manifest, error) {
	remotePath := fmt.Sprintf("/data/theme/%dx%d/casescreen-base.Theme", manifest.Width, manifest.Height)
	for index := range manifest.Resources {
		resource := &manifest.Resources[index]
		if strings.EqualFold(filepath.Ext(resource.RemotePath), ".Theme") {
			return manifest, nil
		}
		if strings.EqualFold(filepath.Ext(resource.LocalPath), ".Theme") {
			resource.RemotePath = remotePath
			return manifest, nil
		}
	}
	directory := filepath.Join(server.dataDirectory, "theme-assets")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return manifest, fmt.Errorf("创建微雪主题缓存目录失败: %w", err)
	}
	baseTheme, err := server.loadWaveshareBaseTheme()
	if err != nil {
		return manifest, err
	}
	digest := sha256.Sum256(baseTheme)
	localPath := filepath.Join(directory, fmt.Sprintf("waveshare-embedded-base-%x.Theme", digest[:8]))
	if _, err := os.Stat(localPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(localPath, baseTheme, 0o600); err != nil {
			return manifest, fmt.Errorf("缓存微雪主题模板失败: %w", err)
		}
	} else if err != nil {
		return manifest, fmt.Errorf("读取微雪主题模板缓存失败: %w", err)
	}
	resourceID := "waveshare-base-theme"
	for suffix := 2; ; suffix++ {
		available := true
		for _, resource := range manifest.Resources {
			if resource.ID == resourceID {
				available = false
				break
			}
		}
		if available {
			break
		}
		resourceID = fmt.Sprintf("waveshare-base-theme-%d", suffix)
	}
	resource, err := theme.InspectResource(resourceID, localPath, remotePath)
	if err != nil {
		return manifest, fmt.Errorf("读取微雪主题模板失败: %w", err)
	}
	manifest.Resources = append(manifest.Resources, resource)
	return manifest, nil
}

// ensureWaveshareThemeSource 保留一份不可变的原始 .Theme，避免纯背景主题应用后
// 把原厂控件永久过滤掉。旧版本已经把资源覆盖成单控件主题时，优先从旧软件目录
// 恢复。公开源码与 GitHub 发行包不分发厂商主题，必须使用用户自己设备或原厂软件中的模板。
func (server *Server) ensureWaveshareThemeSource(resource theme.Resource) (string, error) {
	cacheDirectory := filepath.Join(server.dataDirectory, "theme-assets")
	if err := os.MkdirAll(cacheDirectory, 0o700); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(waveshareThemeTemplateRemotePath(resource.RemotePath)))
	basePath := filepath.Join(cacheDirectory, fmt.Sprintf("waveshare-base-%x.Theme", digest[:8]))
	if cached, err := os.ReadFile(basePath); err == nil && waveshareThemeHasRequiredTemplates(cached) {
		return basePath, nil
	}

	sourceData, sourceErr := os.ReadFile(resource.LocalPath)
	if sourceErr == nil && waveshareThemeHasRequiredTemplates(sourceData) {
		if err := os.WriteFile(basePath, sourceData, 0o600); err != nil {
			return "", err
		}
		return basePath, nil
	}

	// 当前资源已经是被过滤的旧主题，尝试寻找用户原厂软件中的同分辨率模板。
	for _, candidate := range server.legacyWaveshareThemeCandidates() {
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		if waveshareThemeHasRequiredTemplates(data) {
			if err := os.WriteFile(basePath, data, 0o600); err != nil {
				return "", err
			}
			return basePath, nil
		}
	}
	return "", errors.New("未找到可用的微雪基础 Theme；请从你拥有的微雪原厂软件或设备导出 070-astronaut.Theme，并放入程序 themes 目录后重试")
}

func (server *Server) loadWaveshareBaseTheme() ([]byte, error) {
	for _, candidate := range server.legacyWaveshareThemeCandidates() {
		data, err := os.ReadFile(candidate)
		if err == nil && waveshareThemeHasRequiredTemplates(data) {
			return data, nil
		}
	}
	return nil, errors.New("未找到可用的微雪基础 Theme；请从你拥有的微雪原厂软件或设备导出 070-astronaut.Theme，并放入程序 themes 目录后重试")
}

func waveshareThemeHasRequiredTemplates(data []byte) bool {
	table, err := parseWaveshareThemeWidgetTable(data)
	if err != nil {
		return false
	}
	// 106 等新控件由编译器构造；这里校验所有需要克隆字段布局的基础模板。
	required := map[string]bool{"8": false, "100": false, "101": false, "102": false, "108": false}
	for _, widget := range table.widgets {
		if _, ok := required[widget.fields["type"]]; ok {
			required[widget.fields["type"]] = true
		}
	}
	for _, found := range required {
		if !found {
			return false
		}
	}
	return true
}

func waveshareVersionedThemeRemotePath(remotePath string, data []byte) string {
	stablePath := waveshareThemeTemplateRemotePath(remotePath)
	extension := path.Ext(stablePath)
	if !strings.EqualFold(extension, ".Theme") {
		return stablePath
	}
	digest := sha256.Sum256(data)
	return strings.TrimSuffix(stablePath, extension) + fmt.Sprintf("-v%x", digest[:6]) + extension
}

func waveshareThemeTemplateRemotePath(remotePath string) string {
	remotePath = filepath.ToSlash(strings.TrimSpace(remotePath))
	extension := path.Ext(remotePath)
	base := strings.TrimSuffix(remotePath, extension)
	marker := strings.LastIndex(base, "-v")
	if marker < 0 {
		return remotePath
	}
	contentTag := base[marker+2:]
	if len(contentTag) != 12 || !isWaveshareHexTag(contentTag) {
		return remotePath
	}
	return base[:marker] + extension
}

func isWaveshareHexTag(value string) bool {
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func (server *Server) legacyWaveshareThemeCandidates() []string {
	candidates := make([]string, 0, 10)
	if configured := strings.TrimSpace(os.Getenv("CASESCREEN_WAVESHARE_BASE_THEME")); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates,
		filepath.Join(server.dataDirectory, "theme-assets", "070-astronaut.Theme"),
		filepath.Join(server.dataDirectory, "070-astronaut.Theme"),
		filepath.Join(".tmp", "waveshare", "070-astronaut.Theme"),
	)
	if executable, err := os.Executable(); err == nil {
		executableDirectory := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(executableDirectory, "themes", "070-astronaut.Theme"),
			filepath.Join(executableDirectory, "themes", "waveshare", "070-astronaut.Theme"),
			filepath.Join(executableDirectory, ".tmp", "waveshare", "070-astronaut.Theme"),
		)
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(workingDirectory, ".tmp", "waveshare", "070-astronaut.Theme"),
			filepath.Join(workingDirectory, "themes", "070-astronaut.Theme"),
		)
	}
	return candidates
}

func waveshareThemeWidgetCount(data []byte) (uint32, error) {
	if len(data) < len(waveshareThemeHeader)+4 || string(data[:len(waveshareThemeHeader)]) != waveshareThemeHeader {
		return 0, errors.New("微雪主题头无效")
	}
	cursor := len(waveshareThemeHeader)
	if err := skipWaveshareVariantMap(data, &cursor); err != nil {
		return 0, err
	}
	if _, _, err := readWaveshareStringMap(data, &cursor); err != nil {
		return 0, err
	}
	return readWaveshareUint32(data, &cursor)
}

func waveshareWidgetFilter(manifest theme.Manifest) func(map[string]string) bool {
	telemetryFields := make(map[string]bool)
	hasAudio := false
	for _, layer := range manifest.Layers {
		switch layer.Type {
		case theme.LayerTelemetry:
			fields := waveshareTelemetryFields(layer.Options)
			for _, field := range fields {
				telemetryFields[field] = true
			}
		case theme.LayerAudioBars, theme.LayerAudioRing:
			hasAudio = true
		}
	}
	return func(fields map[string]string) bool {
		switch fields["type"] {
		case "100": // 内嵌背景视频。
			return true
		case "108": // 原生音频频谱。
			return hasAudio
		default:
			field := waveshareWidgetTelemetryField(fields)
			return field != "" && telemetryFields[field]
		}
	}
}

func waveshareTelemetryFields(options map[string]any) []string {
	if options != nil {
		switch values := options["fields"].(type) {
		case []string:
			if len(values) > 0 {
				return values
			}
		case []any:
			result := make([]string, 0, len(values))
			for _, value := range values {
				if text, ok := value.(string); ok && text != "" {
					result = append(result, text)
				}
			}
			if len(result) > 0 {
				return result
			}
		}
	}
	return []string{"time", "Cpu_Freq", "Memory_Load"}
}

func waveshareWidgetTelemetryField(fields map[string]string) string {
	// 原宇航员主题的一些静态标题复用了错误的数据名，标题文字需要优先判断。
	switch strings.TrimSpace(fields["text_str"]) {
	case "CPU":
		return "Cpu_Freq"
	case "GPU":
		return "Gpu_Load"
	case "RAM":
		return "Memory_Load"
	case "Used":
		return "Memory_Used"
	case "Free":
		return "Memory_Available"
	case "C：", "D：":
		return ""
	}
	switch strings.TrimSpace(fields["system_data_name"]) {
	case "时间", "日期", "星期":
		return "time"
	case "Cpu使用率":
		return "Cpu_Freq"
	case "Cpu型号":
		return "Cpu_Model"
	case "显卡利用率":
		return "Gpu_Load"
	case "显卡温度":
		return "GPU_Temp"
	case "显卡热点温度":
		return "GPU_Hotspot_Temp"
	case "显存温度":
		return "GPU_Memory_Temp"
	case "内存利用率":
		return "Memory_Load"
	case "已用内存":
		return "Memory_Used"
	case "可用内存":
		return "Memory_Available"
	default:
		return ""
	}
}

func normalizeWaveshareImage(sourcePath, outputPath string) error {
	input, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer input.Close()
	decoded, _, err := image.Decode(input)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(output, decoded, &jpeg.Options{Quality: 95}); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func renderWaveshareStaticScene(manifest theme.Manifest, outputPath string) error {
	manifest = theme.NormalizeScene(manifest)
	fullScene := manifest
	staticLayers := make([]theme.Layer, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		switch layer.Type {
		case theme.LayerCanvas, theme.LayerImage, theme.LayerBackgroundImage:
			staticLayers = append(staticLayers, layer)
		}
	}
	manifest.Layers = staticLayers
	renderer, err := caseRender.New(manifest)
	if err != nil {
		return err
	}
	defer renderer.Close()
	frame, err := renderer.Render(manifest.Width, manifest.Height, caseRender.State{})
	if err != nil {
		return err
	}
	drawWaveshareComponentShells(frame, fullScene)
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(output, frame, &jpeg.Options{Quality: 95}); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

// renderWaveshareStaticOverlay 为全屏驻留视频生成透明叠加层。微雪固件只有一个
// 背景视频控件，图片和各组件的面板外壳必须先合成进视频，才能与编辑器层级一致。
func renderWaveshareStaticOverlay(manifest theme.Manifest, outputPath string) error {
	manifest = theme.NormalizeScene(manifest)
	fullScene := manifest
	staticLayers := make([]theme.Layer, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		switch layer.Type {
		case theme.LayerCanvas:
			layer.Opacity = 1
			layer.Options = map[string]any{"color": "transparent", "visible": true}
			staticLayers = append(staticLayers, layer)
		case theme.LayerImage, theme.LayerBackgroundImage:
			staticLayers = append(staticLayers, layer)
		}
	}
	manifest.Layers = staticLayers
	renderer, err := caseRender.New(manifest)
	if err != nil {
		return err
	}
	defer renderer.Close()
	frame, err := renderer.Render(manifest.Width, manifest.Height, caseRender.State{})
	if err != nil {
		return err
	}
	drawWaveshareComponentShells(frame, fullScene)
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := png.Encode(output, frame); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func waveshareBackgroundResource(manifest theme.Manifest) (theme.Resource, bool) {
	for _, layer := range manifest.Layers {
		if layer.Type != theme.LayerBackgroundImage && layer.Type != theme.LayerBackgroundVideo {
			continue
		}
		if !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		resourceID, _ := layer.Options["resource_id"].(string)
		if resourceID == "" {
			return theme.Resource{}, false
		}
		for _, resource := range manifest.Resources {
			if resource.ID == resourceID {
				return resource, true
			}
		}
	}
	return theme.Resource{}, false
}

func waveshareBackgroundVideo(manifest theme.Manifest) (theme.Layer, theme.Resource, bool, error) {
	var selected theme.Layer
	visible := 0
	for _, layer := range manifest.Layers {
		if layer.Type != theme.LayerBackgroundVideo || !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		visible++
		selected = layer
	}
	if visible == 0 {
		return theme.Layer{}, theme.Resource{}, false, nil
	}
	if visible > 1 {
		return theme.Layer{}, theme.Resource{}, false, errors.New("微雪固件只支持一个可见视频背景，请隐藏或删除多余视频图层")
	}
	if math.Abs(selected.Bounds.X) > 1e-9 || math.Abs(selected.Bounds.Y) > 1e-9 || math.Abs(selected.Bounds.Width-1) > 1e-9 || math.Abs(selected.Bounds.Height-1) > 1e-9 || math.Abs(selected.Opacity-1) > 1e-9 || math.Abs(selected.Rotation) > 1e-9 {
		return theme.Layer{}, theme.Resource{}, false, errors.New("微雪视频必须是全屏、不透明且不旋转的最底层背景；请在可视化编辑器中重新选择该视频图层")
	}
	for _, layer := range manifest.Layers {
		if layer.ID == selected.ID || layer.Type == theme.LayerCanvas || !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		if layer.ZIndex <= selected.ZIndex {
			return theme.Layer{}, theme.Resource{}, false, errors.New("微雪视频必须位于所有图片和组件之下；请将视频图层移到最底层")
		}
	}
	resourceID, _ := selected.Options["resource_id"].(string)
	if strings.TrimSpace(resourceID) == "" {
		return theme.Layer{}, theme.Resource{}, false, errors.New("微雪视频图层尚未选择视频文件")
	}
	for _, resource := range manifest.Resources {
		if resource.ID != resourceID {
			continue
		}
		if !waveshareVideoPath(resource.LocalPath) {
			return theme.Layer{}, theme.Resource{}, false, fmt.Errorf("微雪视频格式不受支持: %s", filepath.Ext(resource.LocalPath))
		}
		return selected, resource, true, nil
	}
	return theme.Layer{}, theme.Resource{}, false, fmt.Errorf("微雪视频资源不存在: %s", resourceID)
}

func waveshareVideoPath(value string) bool {
	switch strings.ToLower(filepath.Ext(value)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".gif":
		return true
	default:
		return false
	}
}

func (server *Server) findFFmpegPath() string {
	if configured := strings.TrimSpace(server.store.Get().FFmpegPath); configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured
		}
	}
	if executable, err := os.Executable(); err == nil {
		executableDirectory := filepath.Dir(executable)
		for _, candidate := range []string{
			filepath.Join(executableDirectory, "ffmpeg.exe"),
			filepath.Join(executableDirectory, ".tmp", "ffmpeg-runtime", "ffmpeg.exe"),
			filepath.Join(filepath.Dir(executableDirectory), ".tmp", "ffmpeg-runtime", "ffmpeg.exe"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		for _, candidate := range []string{
			filepath.Join(workingDirectory, "ffmpeg.exe"),
			filepath.Join(workingDirectory, ".tmp", "ffmpeg-runtime", "ffmpeg.exe"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if candidate, err := exec.LookPath("ffmpeg"); err == nil {
		return candidate
	}
	return ""
}

func encodeWaveshareBackground(ctx context.Context, ffmpegPath, sourcePath, outputPath string, width, height int, stillImage bool) error {
	return encodeWaveshareBackgroundWithOptions(ctx, ffmpegPath, sourcePath, "", outputPath, width, height, stillImage, "stretch")
}

func encodeWaveshareBackgroundWithOptions(ctx context.Context, ffmpegPath, sourcePath, overlayPath, outputPath string, width, height int, stillImage bool, fit string) error {
	if width <= 0 {
		width = 1024
	}
	if height <= 0 {
		height = 600
	}
	filter := waveshareVideoScaleFilter(width, height, fit)
	var lastOutput []byte
	var lastErr error
	// 发行版 FFmpeg 使用 Windows Media Foundation 提供 H.264 编码器，未必带 libx264。
	for _, encoder := range []string{"h264_mf", "mpeg4"} {
		_ = os.Remove(outputPath)
		arguments := []string{"-y"}
		if stillImage {
			arguments = append(arguments, "-loop", "1")
		}
		arguments = append(arguments, "-i", sourcePath)
		if overlayPath != "" {
			arguments = append(arguments, "-loop", "1", "-i", overlayPath)
			filterGraph := fmt.Sprintf("[0:v]%s,format=rgba[base];[1:v]scale=%d:%d:flags=lanczos,format=rgba[overlay];[base][overlay]overlay=0:0:format=auto,format=yuv420p[out]", filter, width, height)
			arguments = append(arguments, "-filter_complex", filterGraph, "-map", "[out]")
		} else {
			arguments = append(arguments, "-vf", filter+",format=yuv420p")
		}
		// 静态图只需一小段可循环视频；真实视频必须保留完整时长，不能像旧实现
		// 那样无提示截成 2 秒。统一限制码率，最终仍由 Theme 的 24 位长度字段
		// 检查 16 MiB 上限，超限时向用户明确报错。
		if stillImage {
			arguments = append(arguments, "-t", "2")
		}
		arguments = append(arguments, "-r", "10", "-an", "-c:v", encoder, "-b:v", "1200k", "-movflags", "+faststart", outputPath)
		command := exec.CommandContext(ctx, ffmpegPath, arguments...)
		output, err := command.CombinedOutput()
		if err == nil {
			return nil
		}
		lastOutput, lastErr = output, err
	}
	message := strings.TrimSpace(string(lastOutput))
	if message == "" {
		return lastErr
	}
	return fmt.Errorf("%w: %s", lastErr, message)
}

func waveshareVideoScaleFilter(width, height int, fit string) string {
	switch strings.ToLower(strings.TrimSpace(fit)) {
	case "contain":
		return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1", width, height, width, height)
	case "cover":
		return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase:flags=lanczos,crop=%d:%d,setsar=1", width, height, width, height)
	default:
		return fmt.Sprintf("scale=%d:%d:flags=lanczos,setsar=1", width, height)
	}
}

func replaceWaveshareEmbeddedVideo(themePath, videoPath, outputPath string) error {
	themeBytes, err := os.ReadFile(themePath)
	if err != nil {
		return err
	}
	videoBytes, err := os.ReadFile(videoPath)
	if err != nil {
		return err
	}
	if len(videoBytes) > 0xFFFFFF {
		return fmt.Errorf("内嵌视频过大（%d 字节，微雪主题包最多支持 16777215 字节）", len(videoBytes))
	}
	ftyp := bytes.LastIndex(videoBytes, []byte("ftyp"))
	if ftyp < 4 || int(binary.BigEndian.Uint32(videoBytes[ftyp-4:ftyp])) > len(videoBytes)-ftyp+4 {
		return errors.New("背景文件不是有效的 MP4")
	}
	videoStart := bytes.LastIndex(themeBytes, []byte("ftyp"))
	if videoStart < 4 {
		return errors.New("微雪 .Theme 中未找到内嵌 MP4")
	}
	videoStart -= 4
	if videoStart < 3 || videoStart+4 > len(themeBytes) {
		return errors.New("微雪 .Theme 内嵌视频位置无效")
	}
	oldLength := int(themeBytes[videoStart-3])<<16 | int(themeBytes[videoStart-2])<<8 | int(themeBytes[videoStart-1])
	if oldLength <= 0 || videoStart+oldLength > len(themeBytes) {
		return errors.New("微雪 .Theme 内嵌视频长度无效")
	}
	length := []byte{byte(len(videoBytes) >> 16), byte(len(videoBytes) >> 8), byte(len(videoBytes))}
	result := make([]byte, 0, len(themeBytes)-oldLength+len(videoBytes))
	result = append(result, themeBytes[:videoStart-3]...)
	result = append(result, length...)
	result = append(result, videoBytes...)
	result = append(result, themeBytes[videoStart+oldLength:]...)
	if err := os.WriteFile(outputPath, result, 0o600); err != nil {
		return err
	}
	return nil
}
