package theme

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	WaveshareAudioBandCount   = 48
	DefaultAudioBarsCount     = 48
	DefaultAudioRingCount     = 48
	DefaultLyricsFontSize     = 34
	DefaultLyricsNextFontSize = 20

	// 这两个字段只写入 CaseScreen 自己保存的清单。屏端不会读取它们；
	// 编译器和运行时据此使用同一组短键，避免每个频带重复携带长哈希。
	waveshareAudioDataPrefixOption = "_waveshare_audio_data_prefix"
	waveshareAudioPeakPrefixOption = "_waveshare_audio_peak_prefix"
)

// LayerVisible 返回图层的显示状态。旧主题没有 visible 字段时默认显示。
func LayerVisible(layer Layer) bool {
	return OptionBool(layer.Options, "visible", true)
}

// OptionBool 兼容 JSON 布尔值与常见文本形式，便于读取旧版主题。
func OptionBool(options map[string]any, key string, fallback bool) bool {
	if options == nil {
		return fallback
	}
	switch value := options[key].(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err == nil {
			return parsed
		}
	case float64:
		return value != 0
	case int:
		return value != 0
	case json.Number:
		parsed, err := value.Float64()
		if err == nil {
			return parsed != 0
		}
	}
	return fallback
}

// OptionFloat 兼容 JSON 数字、Go 整数和文本数值。
func OptionFloat(options map[string]any, key string, fallback float64) float64 {
	if options == nil {
		return fallback
	}
	switch value := options[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		if parsed, err := value.Float64(); err == nil {
			return parsed
		}
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			return parsed
		}
	}
	return fallback
}

// OptionInt 兼容 JSON 数字、Go 整数和文本整数。
func OptionInt(options map[string]any, key string, fallback int) int {
	if options == nil {
		return fallback
	}
	switch value := options[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(math.Round(value))
	case float32:
		return int(math.Round(float64(value)))
	case json.Number:
		if parsed, err := value.Float64(); err == nil {
			return int(math.Round(parsed))
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return fallback
}

// WaveshareAudioDataPrefix 按音频变换参数生成屏端数据命名空间。
// 默认参数使用单字符 A，使通常的条状与环状频谱共享 A00..A47；
// 峰值保持属性不影响基础频带键，避免只改尾迹样式就重复传输同一份 FFT 数据。
func WaveshareAudioDataPrefix(layer Layer) string {
	if prefix, ok := waveshareAssignedAudioPrefix(layer.Options, waveshareAudioDataPrefixOption, false); ok {
		return prefix
	}
	identity, usesDefaults := waveshareAudioTransformIdentity(layer)
	if usesDefaults {
		return "A"
	}
	return "A" + waveshareAudioIdentityHash(identity)
}

// WaveshareAudioBandKey 返回单个实时频带的键名。
func WaveshareAudioBandKey(layer Layer, index int) string {
	prefix := WaveshareAudioDataPrefix(layer)
	if len(prefix) == 1 {
		return fmt.Sprintf("%s%02d", prefix, max(0, index))
	}
	return fmt.Sprintf("%s_%02d", prefix, max(0, index))
}

// WaveshareAudioPeakKey 返回峰值保持图层的频带键名。
// 它单独按音频变换、保持时间和回落速度命名：相同设置共享一组键，
// 默认变换 + 450ms + 1.8 使用 P00..P47 短键。
func WaveshareAudioPeakKey(layer Layer, index int) string {
	prefix := waveshareAudioPeakPrefix(layer)
	if len(prefix) <= 2 {
		return fmt.Sprintf("%s%02d", prefix, max(0, index))
	}
	return fmt.Sprintf("%s_%02d", prefix, max(0, index))
}

func waveshareAudioPeakPrefix(layer Layer) string {
	if prefix, ok := waveshareAssignedAudioPrefix(layer.Options, waveshareAudioPeakPrefixOption, true); ok {
		return prefix
	}
	identity, usesDefaults := waveshareAudioPeakIdentity(layer)
	if usesDefaults {
		return "P"
	}
	return "P" + waveshareAudioIdentityHash(identity)
}

func waveshareAudioPeakIdentity(layer Layer) (string, bool) {
	transformIdentity, usesDefaultTransform := waveshareAudioTransformIdentity(layer)
	peakDuration := clampAudioOption(OptionFloat(layer.Options, "peak_hold_ms", 450), 0, 5000)
	peakDecay := clampAudioOption(OptionFloat(layer.Options, "peak_decay", 1.8), .05, 10)
	identity := strings.Join([]string{
		"v1", transformIdentity, formatAudioOption(peakDuration), formatAudioOption(peakDecay),
	}, "|")
	return identity, usesDefaultTransform && peakDuration == 450 && peakDecay == 1.8
}

// AssignWaveshareAudioPrefixes 为同一主题中的不同音频变换分配确定性的短键。
// 旧清单没有这些内部字段时仍沿用长哈希键，因此升级软件不会让已经驻留在
// TF 卡中的旧 .Theme 失配；主题下一次应用时会重新编译并自动切换到短键。
func AssignWaveshareAudioPrefixes(manifest Manifest) Manifest {
	manifest.Layers = append([]Layer(nil), manifest.Layers...)
	dataIdentities := make(map[string]bool)
	peakIdentities := make(map[string]bool)
	for _, layer := range manifest.Layers {
		if layer.Type != LayerAudioBars && layer.Type != LayerAudioRing {
			continue
		}
		identity, _ := waveshareAudioTransformIdentity(layer)
		dataIdentities[identity] = true
		if OptionBool(layer.Options, "peak_hold", false) {
			peakIdentity, _ := waveshareAudioPeakIdentity(layer)
			peakIdentities[peakIdentity] = true
		}
	}
	dataPrefixes := waveshareCompactPrefixMap(dataIdentities, false)
	peakPrefixes := waveshareCompactPrefixMap(peakIdentities, true)
	for index := range manifest.Layers {
		layer := &manifest.Layers[index]
		if layer.Type != LayerAudioBars && layer.Type != LayerAudioRing {
			continue
		}
		options := make(map[string]any, len(layer.Options)+2)
		for key, value := range layer.Options {
			options[key] = value
		}
		identity, _ := waveshareAudioTransformIdentity(*layer)
		if prefix := dataPrefixes[identity]; prefix != "" {
			options[waveshareAudioDataPrefixOption] = prefix
		}
		if OptionBool(layer.Options, "peak_hold", false) {
			peakIdentity, _ := waveshareAudioPeakIdentity(*layer)
			if prefix := peakPrefixes[peakIdentity]; prefix != "" {
				options[waveshareAudioPeakPrefixOption] = prefix
			}
		} else {
			delete(options, waveshareAudioPeakPrefixOption)
		}
		layer.Options = options
	}
	return manifest
}

func waveshareCompactPrefixMap(identities map[string]bool, peak bool) map[string]string {
	ordered := make([]string, 0, len(identities))
	for identity := range identities {
		ordered = append(ordered, identity)
	}
	sort.Strings(ordered)
	result := make(map[string]string, len(ordered))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for index, identity := range ordered {
		if index >= len(alphabet) {
			break
		}
		prefix := string(alphabet[index])
		if peak {
			prefix = "P" + prefix
		}
		result[identity] = prefix
	}
	return result
}

func waveshareAssignedAudioPrefix(options map[string]any, key string, peak bool) (string, bool) {
	if options == nil {
		return "", false
	}
	prefix, ok := options[key].(string)
	prefix = strings.TrimSpace(prefix)
	if !ok {
		return "", false
	}
	if peak {
		if len(prefix) != 2 || prefix[0] != 'P' {
			return "", false
		}
		return prefix, strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", rune(prefix[1]))
	}
	return prefix, len(prefix) == 1 && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", rune(prefix[0]))
}

func waveshareAudioTransformIdentity(layer Layer) (string, bool) {
	source := strings.ToLower(optionString(layer.Options, "source", "system"))
	// 旧版曾允许屏端麦克风。重新编译时统一迁移到 Windows 系统回放，
	// 避免机箱内底噪和风扇声造成静音时仍有波峰。
	if source == "device_mic" {
		source = "system"
	}
	visual := AudioVisualOptionsForLayer(layer)
	usesDefaults := source == "system" && visual.Gain == DefaultAudioGain && visual.Sensitivity == DefaultAudioSensitivity &&
		visual.MinAmplitude == DefaultAudioMinAmplitude && visual.MaxAmplitude == DefaultAudioMaxAmplitude &&
		visual.Smoothing == DefaultAudioSmoothing && visual.NoiseGate == DefaultAudioNoiseGate &&
		visual.Contrast == DefaultAudioContrast && visual.Separation == DefaultAudioSeparation &&
		visual.BassBoost == DefaultAudioBassBoost && visual.FloorReduce == DefaultAudioFloorReduce
	identity := strings.Join([]string{
		"v5", source,
		formatAudioOption(visual.Gain), formatAudioOption(visual.Sensitivity),
		formatAudioOption(visual.MinAmplitude), formatAudioOption(visual.MaxAmplitude),
		formatAudioOption(visual.Smoothing), formatAudioOption(visual.NoiseGate),
		formatAudioOption(visual.Contrast), formatAudioOption(visual.Separation),
		formatAudioOption(visual.BassBoost), formatAudioOption(visual.FloorReduce),
	}, "|")
	return identity, usesDefaults
}

func waveshareAudioIdentityHash(identity string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(identity))
	return fmt.Sprintf("%08x", hash.Sum32())
}

// WaveshareAudioBandIndexes 返回该驻留控件真正引用的频带。
// 主机只发送编译后控件真实引用的键；默认 48 段圆环会覆盖全部频带，
// 较少段数的条状/环状图层则不会固定传满 48 个值。
func WaveshareAudioBandIndexes(layer Layer, canvasWidth, canvasHeight int) []int {
	if canvasWidth <= 0 {
		canvasWidth = 1024
	}
	if canvasHeight <= 0 {
		canvasHeight = 600
	}
	x := max(0, min(canvasWidth-1, int(math.Round(layer.Bounds.X*float64(canvasWidth)))))
	y := max(0, min(canvasHeight-1, int(math.Round(layer.Bounds.Y*float64(canvasHeight)))))
	pixelWidth := max(1, min(canvasWidth-x, int(math.Round(layer.Bounds.Width*float64(canvasWidth)))))
	pixelHeight := max(1, min(canvasHeight-y, int(math.Round(layer.Bounds.Height*float64(canvasHeight)))))
	indexes := make([]int, 0, WaveshareAudioBandCount)
	seen := make(map[int]bool, WaveshareAudioBandCount)
	appendBand := func(index, count int) {
		band := index * WaveshareAudioBandCount / max(1, count)
		band = max(0, min(WaveshareAudioBandCount-1, band))
		if !seen[band] {
			seen[band] = true
			indexes = append(indexes, band)
		}
	}
	switch layer.Type {
	case LayerAudioBars:
		// 四向条状频谱在微雪编译器中委托给环状驻留控件。这里必须
		// 使用同一套 12..144 数量规则，并且不能按窄 bounds 再裁减，
		// 否则编译出的控件会引用主机没有发送的频带键。
		if AudioBarsUseRadialLayout(layer) {
			count := WaveshareAudioRingCount(layer)
			for index := 0; index < count; index++ {
				appendBand(index, count)
			}
			break
		}
		count := max(6, min(96, OptionInt(layer.Options, "count", DefaultAudioBarsCount)))
		available := pixelWidth
		if strings.EqualFold(optionString(layer.Options, "direction", "bottom"), "horizontal") {
			available = pixelHeight
		}
		count = max(1, min(count, available))
		for index := 0; index < count; index++ {
			appendBand(index, count)
		}
	case LayerAudioRing:
		count := WaveshareAudioRingCount(layer)
		dashed := strings.EqualFold(optionString(layer.Options, "style", "radial"), "dashed")
		for index := 0; index < count; index++ {
			if dashed && index%2 != 0 {
				continue
			}
			appendBand(index, count)
		}
	}
	return indexes
}

// WaveshareAudioRingCount 返回环状驻留控件与主机环状渲染共用的段数。
// 缺省值集中在这里，避免编译、预览与小包索引各自漂移。
func WaveshareAudioRingCount(layer Layer) int {
	return max(12, min(144, OptionInt(layer.Options, "count", DefaultAudioRingCount)))
}

// AudioBarsUseRadialLayout 表示条状频谱采用四向/爆发式径向布局。
// 微雪驻留控件、主机渲染和工作台必须用同一段数规则。
func AudioBarsUseRadialLayout(layer Layer) bool {
	style := strings.ToLower(optionString(layer.Options, "style", "equalizer"))
	direction := strings.ToLower(optionString(layer.Options, "direction", "bottom"))
	return style == "burst" || direction == "fourway"
}

func optionString(options map[string]any, key, fallback string) string {
	if options != nil {
		if value, ok := options[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return fallback
}

func clampAudioOption(value, minimum, maximum float64) float64 {
	return math.Max(minimum, math.Min(maximum, value))
}

func formatAudioOption(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
