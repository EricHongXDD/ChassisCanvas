package audio

import (
	"errors"
	"math"
	"math/cmplx"
	"time"
)

const (
	// 固定 dBFS 范围保留输入音量差异，避免每帧按自身峰值拉满。
	spectrumNoiseFloorDB = -68.0
	spectrumCeilingDB    = -8.0
	// 低于 -80 dBFS 的回环底噪按静音处理。真机空闲端点的峰值约为
	// 0.00003，不应让屏幕误以为主机正在播放。
	spectrumSilenceGate = 0.0001
	// 平滑系数以 20ms 为基准。按真实时间换算后，即使 WASAPI
	// 在静音期间暂停产生数据包，恢复时也不会让旧波峰突然回弹。
	spectrumSmoothingInterval = 20 * time.Millisecond
)

// SpectrumFrame 是设备无关的音频图层数据，Bars 可同时驱动条形和环形控件。
type SpectrumFrame struct {
	CapturedAt time.Time `json:"captured_at"`
	Level      float64   `json:"level"`
	Bars       []float64 `json:"bars"`
}

// Analyzer 对 PCM 样本执行 Hann 窗和 FFT，再压缩到对数频带。
type Analyzer struct {
	SampleRate int
	BandCount  int
	Attack     float64
	Decay      float64
	previous   []float64
	previousAt time.Time
}

func NewAnalyzer(sampleRate, bandCount int) (*Analyzer, error) {
	if sampleRate < 8000 {
		return nil, errors.New("音频采样率过低")
	}
	if bandCount < 4 || bandCount > 128 {
		return nil, errors.New("频带数必须介于 4 和 128 之间")
	}
	return &Analyzer{
		SampleRate: sampleRate,
		BandCount:  bandCount,
		Attack:     0.88,
		Decay:      0.72,
		previous:   make([]float64, bandCount),
	}, nil
}

// Analyze 接收 -1..1 的单声道 PCM 样本。
func (analyzer *Analyzer) Analyze(samples []float64, capturedAt time.Time) (SpectrumFrame, error) {
	size := floorPowerOfTwo(len(samples))
	if size < 64 {
		return SpectrumFrame{}, errors.New("PCM 样本数不足")
	}
	samples = samples[len(samples)-size:]
	values := make([]complex128, size)
	levelSum := 0.0
	for index, sample := range samples {
		if sample > 1 {
			sample = 1
		} else if sample < -1 {
			sample = -1
		}
		levelSum += sample * sample
		window := 0.5 - 0.5*math.Cos(2*math.Pi*float64(index)/float64(size-1))
		values[index] = complex(sample*window, 0)
	}
	level := math.Sqrt(levelSum / float64(size))
	bandValues := make([]float64, analyzer.BandCount)
	if level >= spectrumSilenceGate {
		fft(values)
		// 32Hz 起点保留鼓点和贝斯的首个尖峰；18kHz 上限避免把
		// Nyquist 边缘的无效噪声占成多个可见频带。
		minimumFrequency := 32.0
		maximumFrequency := math.Min(18000, float64(analyzer.SampleRate)/2*.94)
		frequencyRatio := math.Pow(maximumFrequency/minimumFrequency, 1/float64(analyzer.BandCount))
		for band := 0; band < analyzer.BandCount; band++ {
			low := minimumFrequency * math.Pow(frequencyRatio, float64(band))
			high := minimumFrequency * math.Pow(frequencyRatio, float64(band+1))
			start := maxInt(1, int(math.Floor(low*float64(size)/float64(analyzer.SampleRate))))
			end := minInt(size/2, int(math.Ceil(high*float64(size)/float64(analyzer.SampleRate))))
			if end <= start {
				end = minInt(size/2, start+1)
			}
			power := 0.0
			peak := 0.0
			for bin := start; bin < end; bin++ {
				// Hann 窗的相干增益约为 0.5；单边谱还需乘 2，因此使用 4/N。
				magnitude := 4 * cmplx.Abs(values[bin]) / float64(size)
				power += magnitude * magnitude
				peak = math.Max(peak, magnitude)
			}
			amplitude := peak
			if end > start {
				rms := math.Sqrt(power / float64(end-start))
				// 宽高频带不能用纯 RMS 稀释窄谱尖峰；峰值与 RMS 混合既
				// 保留鼓点/单音的高低落差，也不会把单个噪声 bin 完全拉满。
				amplitude = math.Sqrt(peak*peak*.72 + rms*rms*.28)
			}
			bandValues[band] = spectrumAmplitudeToUnit(amplitude)
		}
	} else {
		// 静音是明确的零帧，不能再用上一帧的衰减值继续驱动副屏。
		// 否则峰值保持与图层平滑会把空闲端点误画成持续波形。
		clear(analyzer.previous)
		analyzer.previousAt = capturedAt
		return SpectrumFrame{CapturedAt: capturedAt, Bars: bandValues}, nil
	}
	steps := analyzer.smoothingSteps(capturedAt)
	attack := 1 - math.Pow(1-analyzer.Attack, steps)
	decay := math.Pow(analyzer.Decay, steps)
	for index, value := range bandValues {
		previous := analyzer.previous[index]
		if value >= previous {
			value = previous + (value-previous)*attack
		} else {
			value = math.Max(value, previous*decay)
		}
		bandValues[index] = clamp01(value)
		analyzer.previous[index] = bandValues[index]
	}
	analyzer.previousAt = capturedAt

	return SpectrumFrame{
		CapturedAt: capturedAt,
		Level:      clamp01(level),
		Bars:       bandValues,
	}, nil
}

func (analyzer *Analyzer) smoothingSteps(capturedAt time.Time) float64 {
	if analyzer.previousAt.IsZero() || !capturedAt.After(analyzer.previousAt) {
		return 1
	}
	steps := capturedAt.Sub(analyzer.previousAt).Seconds() / spectrumSmoothingInterval.Seconds()
	// 很短的 WASAPI 包间隔不应因为调度抖动让攻击/衰减失效。
	return math.Max(1, math.Min(250, steps))
}

func spectrumAmplitudeToUnit(amplitude float64) float64 {
	if amplitude <= 0 {
		return 0
	}
	decibels := 20 * math.Log10(amplitude)
	return clamp01((decibels - spectrumNoiseFloorDB) / (spectrumCeilingDB - spectrumNoiseFloorDB))
}

func fft(values []complex128) {
	length := len(values)
	for left, right := 1, 0; left < length; left++ {
		bit := length >> 1
		for ; right&bit != 0; bit >>= 1 {
			right &^= bit
		}
		right |= bit
		if left < right {
			values[left], values[right] = values[right], values[left]
		}
	}
	for block := 2; block <= length; block <<= 1 {
		angle := -2 * math.Pi / float64(block)
		root := cmplx.Rect(1, angle)
		for start := 0; start < length; start += block {
			factor := complex(1, 0)
			for offset := 0; offset < block/2; offset++ {
				even := values[start+offset]
				odd := values[start+offset+block/2] * factor
				values[start+offset] = even + odd
				values[start+offset+block/2] = even - odd
				factor *= root
			}
		}
	}
}

func floorPowerOfTwo(value int) int {
	result := 1
	for result <= value/2 {
		result <<= 1
	}
	return result
}

func clamp01(value float64) float64 {
	return math.Max(0, math.Min(1, value))
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
