package audio

import (
	"math"
	"testing"
	"time"
)

func TestAnalyzerFindsSineBand(t *testing.T) {
	const sampleRate = 48000
	const frequency = 1000.0
	analyzer, err := NewAnalyzer(sampleRate, 24)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]float64, 4096)
	for index := range samples {
		samples[index] = 0.7 * math.Sin(2*math.Pi*frequency*float64(index)/sampleRate)
	}
	frame, err := analyzer.Analyze(samples, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	peak := 0
	for index := range frame.Bars {
		if frame.Bars[index] > frame.Bars[peak] {
			peak = index
		}
	}
	if peak < 10 || peak > 15 {
		t.Fatalf("1kHz 峰值落在意外频带: %d", peak)
	}
	if frame.Level < 0.45 || frame.Level > 0.55 {
		t.Fatalf("RMS 电平异常: %.3f", frame.Level)
	}
}

func TestAnalyzerPreservesInputVolumeDifference(t *testing.T) {
	const sampleRate = 48000
	const frequency = 1000.0
	analyzer, err := NewAnalyzer(sampleRate, 24)
	if err != nil {
		t.Fatal(err)
	}
	maximumBar := func(amplitude float64, second int64) float64 {
		samples := make([]float64, 4096)
		for index := range samples {
			samples[index] = amplitude * math.Sin(2*math.Pi*frequency*float64(index)/sampleRate)
		}
		frame, analyzeErr := analyzer.Analyze(samples, time.Unix(second, 0))
		if analyzeErr != nil {
			t.Fatal(analyzeErr)
		}
		maximum := 0.0
		for _, value := range frame.Bars {
			maximum = math.Max(maximum, value)
		}
		return maximum
	}

	silence := maximumBar(0, 1)
	low := maximumBar(0.005, 2)
	high := maximumBar(0.5, 3)
	if silence != 0 {
		t.Fatalf("静音频谱应为 0，实际 %.6f", silence)
	}
	if !(silence < low && low < high) {
		t.Fatalf("频谱没有保留音量级差: silence=%.4f low=%.4f high=%.4f", silence, low, high)
	}
	if high-low < 0.2 {
		t.Fatalf("高低音量的可视差异过小: low=%.4f high=%.4f", low, high)
	}
}

func TestSpectrumAmplitudeUsesFixedDBFSRange(t *testing.T) {
	for _, test := range []struct {
		name      string
		decibels  float64
		expected  float64
		tolerance float64
	}{
		{name: "噪声底", decibels: spectrumNoiseFloorDB, expected: 0, tolerance: 1e-12},
		{name: "动态范围中点", decibels: (spectrumNoiseFloorDB + spectrumCeilingDB) / 2, expected: .5, tolerance: 1e-12},
		{name: "显示上限", decibels: spectrumCeilingDB, expected: 1, tolerance: 1e-12},
	} {
		t.Run(test.name, func(t *testing.T) {
			amplitude := math.Pow(10, test.decibels/20)
			if got := spectrumAmplitudeToUnit(amplitude); math.Abs(got-test.expected) > test.tolerance {
				t.Fatalf("%.0f dBFS 映射异常: got=%.12f want=%.12f", test.decibels, got, test.expected)
			}
		})
	}
}

func TestAnalyzerKeepsBassHitSharpAndAboveQuieterTreble(t *testing.T) {
	const sampleRate = 48000
	analyzer, err := NewAnalyzer(sampleRate, 48)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]float64, 4096)
	for index := range samples {
		position := float64(index) / sampleRate
		samples[index] = .72*math.Sin(2*math.Pi*72*position) + .16*math.Sin(2*math.Pi*4200*position)
	}
	frame, err := analyzer.Analyze(samples, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	lowPeak, lowIndex := 0.0, 0
	highPeak := 0.0
	for index, value := range frame.Bars {
		if index < len(frame.Bars)/3 && value > lowPeak {
			lowPeak, lowIndex = value, index
		}
		if index >= len(frame.Bars)/2 {
			highPeak = math.Max(highPeak, value)
		}
	}
	if lowIndex >= len(frame.Bars)/3 || lowPeak < highPeak+.12 {
		t.Fatalf("低频鼓点没有形成独立高峰: low[%d]=%.3f high=%.3f bars=%v", lowIndex, lowPeak, highPeak, frame.Bars)
	}
}

func TestAnalyzerFreshSilenceImmediatelyClearsPreviousSpectrum(t *testing.T) {
	const sampleRate = 48000
	analyzer, err := NewAnalyzer(sampleRate, 24)
	if err != nil {
		t.Fatal(err)
	}
	tone := make([]float64, 4096)
	for index := range tone {
		tone[index] = 0.7 * math.Sin(2*math.Pi*1000*float64(index)/sampleRate)
	}
	firstAt := time.Unix(10, 0)
	first, err := analyzer.Analyze(tone, firstAt)
	if err != nil {
		t.Fatal(err)
	}
	peakBefore := maximumSpectrumBar(first.Bars)
	silence, err := analyzer.Analyze(make([]float64, len(tone)), firstAt.Add(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	peakAfter := maximumSpectrumBar(silence.Bars)
	if peakBefore < 0.5 {
		t.Fatalf("测试音未产生有效频谱: %.4f", peakBefore)
	}
	if silence.Level != 0 || peakAfter != 0 {
		t.Fatalf("静音间隔后仍保留旧波峰: before=%.4f after=%.4f", peakBefore, peakAfter)
	}
}

func TestAnalyzerTreatsLoopbackFloorNoiseAsSilence(t *testing.T) {
	analyzer, err := NewAnalyzer(48000, 24)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]float64, 4096)
	for index := range samples {
		samples[index] = 0.00003 * math.Sin(2*math.Pi*1000*float64(index)/48000)
	}
	frame, err := analyzer.Analyze(samples, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if frame.Level != 0 || maximumSpectrumBar(frame.Bars) != 0 {
		t.Fatalf("回环底噪应输出全零频谱: level=%.9f peak=%.9f", frame.Level, maximumSpectrumBar(frame.Bars))
	}
}

func maximumSpectrumBar(bars []float64) float64 {
	maximum := 0.0
	for _, value := range bars {
		maximum = math.Max(maximum, value)
	}
	return maximum
}
