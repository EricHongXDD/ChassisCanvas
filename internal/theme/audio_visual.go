package theme

import (
	"math"
	"sort"
)

const (
	// 默认响应针对系统回放做“快速起峰、干净落底”的视觉映射。
	// 它不会逐帧归一化，所以歌曲本身的音量差仍然保留。
	DefaultAudioGain         = 1.14
	DefaultAudioSensitivity  = 0.92
	DefaultAudioMinAmplitude = 0.0
	DefaultAudioMaxAmplitude = 1.0
	DefaultAudioSmoothing    = 0.20
	DefaultAudioNoiseGate    = 0.025
	DefaultAudioContrast     = 1.58
	DefaultAudioSeparation   = 0.56
	DefaultAudioBassBoost    = 1.22
	DefaultAudioFloorReduce  = 0.62
	// DefaultAudioIdleAmplitude 只用于频谱的可见静态基线，不参与 FFT
	// 变换或峰值计算。4% 足以在短环形柱上保留一个细刻度，又不会形成
	// 覆盖整个量程的固定柱框。
	DefaultAudioIdleAmplitude = 0.04
)

// AudioBandsWithIdleBaseline 把频谱的零值转成可见的静态基线。
//
// FFT 变换仍然保留全零语义，以便峰值保持和平滑状态能在静音时立即清空；
// 只有最终可见层使用此函数。这样主机合成帧、工作台预览和微雪小包
// 在无声时都只留一条低幅静态轮廓，不会把旧波峰当成当前声音。
func AudioBandsWithIdleBaseline(values []float64) []float64 {
	visible := append([]float64(nil), values...)
	for index, value := range visible {
		value = finiteAudioUnit(value)
		if value < DefaultAudioIdleAmplitude {
			value = DefaultAudioIdleAmplitude
		}
		visible[index] = value
	}
	return visible
}

// AudioVisualOptions 是主机合成、编辑器预览和微雪小包共用的频谱响应参数。
// Contrast 拉开全局峰谷，Separation 强化相邻频带差异；二者都不会凭空生成波形。
type AudioVisualOptions struct {
	Gain         float64
	Sensitivity  float64
	MinAmplitude float64
	MaxAmplitude float64
	Smoothing    float64
	NoiseGate    float64
	Contrast     float64
	Separation   float64
	BassBoost    float64
	FloorReduce  float64
}

// AudioVisualOptionsForLayer 读取并约束频谱响应参数。
func AudioVisualOptionsForLayer(layer Layer) AudioVisualOptions {
	minimum := clampAudioOption(OptionFloat(layer.Options, "min_amplitude", DefaultAudioMinAmplitude), 0, 1)
	maximum := clampAudioOption(OptionFloat(layer.Options, "max_amplitude", DefaultAudioMaxAmplitude), 0, 1)
	if maximum < minimum {
		minimum, maximum = maximum, minimum
	}
	return AudioVisualOptions{
		Gain:         clampAudioOption(OptionFloat(layer.Options, "gain", DefaultAudioGain), 0, 20),
		Sensitivity:  clampAudioOption(OptionFloat(layer.Options, "sensitivity", DefaultAudioSensitivity), .05, 8),
		MinAmplitude: minimum,
		MaxAmplitude: maximum,
		Smoothing:    clampAudioOption(OptionFloat(layer.Options, "smoothing", DefaultAudioSmoothing), 0, .98),
		NoiseGate:    clampAudioOption(OptionFloat(layer.Options, "noise_gate", DefaultAudioNoiseGate), 0, .25),
		Contrast:     clampAudioOption(OptionFloat(layer.Options, "contrast", DefaultAudioContrast), .1, 4),
		Separation:   clampAudioOption(OptionFloat(layer.Options, "separation", DefaultAudioSeparation), 0, 2),
		BassBoost:    clampAudioOption(OptionFloat(layer.Options, "bass_boost", DefaultAudioBassBoost), .25, 3),
		FloorReduce:  clampAudioOption(OptionFloat(layer.Options, "floor_reduction", DefaultAudioFloorReduce), 0, .95),
	}
}

// TransformAudioBands 将真实 FFT 频带变换为视觉频带。
// 输入完全静音时直接返回全零并绕过平滑，避免残留波峰；有声时只放大真实差异。
func TransformAudioBands(input []float64, count int, layer Layer, previous []float64) []float64 {
	if count <= 0 {
		return nil
	}
	options := AudioVisualOptionsForLayer(layer)
	values := ResampleAudioBands(input, count)
	output := make([]float64, count)
	active := false
	for _, sample := range values {
		if finiteAudioUnit(sample) > options.NoiseGate {
			active = true
			break
		}
	}
	if !active {
		return output
	}

	// 先移除门限，再从整组频带的低分位估计公共底座。只有大多数频带
	// 同时带有能量时才会产生非零底座，因此独立的真实尖峰不会被削掉。
	// 这解决了宽频音乐下所有柱子一起悬在高位的问题，又不会制造节拍。
	gated := make([]float64, count)
	for index, sample := range values {
		value := finiteAudioUnit(sample)
		if value <= options.NoiseGate {
			continue
		}
		if options.NoiseGate > 0 {
			value = (value - options.NoiseGate) / (1 - options.NoiseGate)
		}
		gated[index] = finiteAudioUnit(value)
	}
	floor := audioLowerQuantile(gated, .22) * options.FloorReduce

	mapped := make([]float64, count)
	for index, value := range gated {
		if value <= floor {
			continue
		}
		if floor > 0 && floor < 1 {
			value = (value - floor) / (1 - floor)
		}
		// 频带按低到高排列。低频增强只缩放真实能量，并在前 35% 频带
		// 平滑衰减到 1，不会给静音或空频带添加固定柱高。
		position := float64(index) / float64(max(1, count-1))
		bassWeight := 1 + (options.BassBoost-1)*math.Pow(math.Max(0, 1-position/.35), 2)
		value *= bassWeight
		value = math.Pow(value, 1/options.Sensitivity) * options.Gain
		if options.Gain > 1 || options.BassBoost > 1 {
			value = audioSoftShoulder(value, .82)
		} else {
			value = finiteAudioUnit(value)
		}
		if value > 0 {
			mapped[index] = options.MinAmplitude + value*(options.MaxAmplitude-options.MinAmplitude)
		}
	}

	// 先用局部反遮罩强化相邻频带，再围绕全局均值拉开峰谷。
	separated := make([]float64, count)
	mean := 0.0
	for index, value := range mapped {
		left, right := value, value
		if index > 0 {
			left = mapped[index-1]
		}
		if index+1 < len(mapped) {
			right = mapped[index+1]
		}
		local := (left + value*2 + right) / 4
		value += (value - local) * options.Separation
		if options.Separation > 0 {
			separated[index] = audioSoftShoulder(value, .84)
		} else {
			separated[index] = finiteAudioUnit(value)
		}
		mean += separated[index]
	}
	mean /= float64(max(1, len(separated)))
	for index, value := range separated {
		value = mean + (value-mean)*options.Contrast
		if options.Contrast > 1 {
			value = audioSoftShoulder(value, .84)
		} else {
			value = finiteAudioUnit(value)
		}
		if index < len(previous) {
			old := finiteAudioUnit(previous[index])
			// 起峰比回落更快，让鼓点有尖峰，尾部仍保持平滑而不闪烁。
			blend := options.Smoothing
			if value >= old {
				blend *= .32
			}
			value = old*blend + value*(1-blend)
		}
		output[index] = finiteAudioUnit(value)
	}
	return output
}

// audioSoftShoulder 在高能量端保留峰值之间的细微差异，避免多个相邻频带
// 同时硬裁成 1 而形成不自然的平顶；低于拐点的值完全不变。
func audioSoftShoulder(value, start float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	start = clampAudioOption(start, 0, .99)
	if value <= start {
		return value
	}
	span := 1 - start
	return finiteAudioUnit(start + span*(1-math.Exp(-(value-start)/span)))
}

// audioLowerQuantile 返回包含零值在内的低分位。稀疏频谱的分位为零，
// 因而不会错误削弱独立的低频鼓点或单音测试信号。
func audioLowerQuantile(values []float64, ratio float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	for index := range ordered {
		ordered[index] = finiteAudioUnit(ordered[index])
	}
	sort.Float64s(ordered)
	index := int(math.Round(clampAudioOption(ratio, 0, 1) * float64(len(ordered)-1)))
	return ordered[index]
}

// ResampleAudioBands 以线性插值把输入覆盖到固定数量的视觉频带。
func ResampleAudioBands(input []float64, count int) []float64 {
	if count <= 0 {
		return nil
	}
	output := make([]float64, count)
	if len(input) == 0 {
		return output
	}
	if len(input) == 1 || count == 1 {
		for index := range output {
			output[index] = finiteAudioUnit(input[0])
		}
		return output
	}
	for index := range output {
		position := float64(index) * float64(len(input)-1) / float64(count-1)
		left := int(math.Floor(position))
		right := min(len(input)-1, left+1)
		fraction := position - float64(left)
		output[index] = finiteAudioUnit(input[left])*(1-fraction) + finiteAudioUnit(input[right])*fraction
	}
	return output
}

func finiteAudioUnit(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Max(0, math.Min(1, value))
}
