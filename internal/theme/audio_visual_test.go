package theme

import (
	"math"
	"testing"
)

func TestTransformAudioBandsSilenceBypassesSmoothing(t *testing.T) {
	layer := Layer{Type: LayerAudioBars, Options: map[string]any{"smoothing": .98, "noise_gate": .01}}
	got := TransformAudioBands([]float64{0, .001, 0}, 12, layer, []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	for index, value := range got {
		if value != 0 {
			t.Fatalf("静音频带 %d 残留 %.6f", index, value)
		}
	}
}

func TestAudioBandsWithIdleBaselineOnlyChangesVisibleCopy(t *testing.T) {
	input := []float64{0, .02, .4, 2, -1}
	visible := AudioBandsWithIdleBaseline(input)
	want := []float64{DefaultAudioIdleAmplitude, DefaultAudioIdleAmplitude, .4, 1, DefaultAudioIdleAmplitude}
	if len(visible) != len(want) {
		t.Fatalf("静态基线长度错误: got=%v want=%v", visible, want)
	}
	for index := range want {
		if math.Abs(visible[index]-want[index]) > 1e-9 {
			t.Fatalf("静态基线错误: index=%d got=%v want=%v", index, visible[index], want[index])
		}
	}
	if input[0] != 0 || input[1] != .02 || input[3] != 2 || input[4] != -1 {
		t.Fatalf("可见基线不应改写 FFT 原值: %v", input)
	}
}

func TestTransformAudioBandsIncreasesRealPeakValleyDifference(t *testing.T) {
	input := []float64{.12, .14, .62, .16, .13}
	plain := Layer{Type: LayerAudioBars, Options: map[string]any{
		"gain": 1.0, "sensitivity": 1.0, "min_amplitude": 0.0, "max_amplitude": 1.0,
		"smoothing": 0.0, "noise_gate": 0.0, "contrast": 1.0, "separation": 0.0,
		"bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	vivid := plain
	vivid.Options = map[string]any{
		"gain": 1.0, "sensitivity": 1.0, "min_amplitude": 0.0, "max_amplitude": 1.0,
		"smoothing": 0.0, "noise_gate": 0.0, "contrast": 1.6, "separation": .8,
		"bass_boost": 1.0, "floor_reduction": 0.0,
	}
	baseline := TransformAudioBands(input, len(input), plain, nil)
	enhanced := TransformAudioBands(input, len(input), vivid, nil)
	baselineGap := baseline[2] - math.Max(baseline[1], baseline[3])
	enhancedGap := enhanced[2] - math.Max(enhanced[1], enhanced[3])
	if enhancedGap <= baselineGap+.08 {
		t.Fatalf("峰谷差没有明显拉开: baseline=%.3f enhanced=%.3f", baselineGap, enhancedGap)
	}
	if enhanced[2] <= 0 || enhanced[1] < 0 || enhanced[3] < 0 {
		t.Fatalf("增强结果越界: %#v", enhanced)
	}
}

func TestTransformAudioBandsRemovesBroadFloorButKeepsIrregularPeaks(t *testing.T) {
	input := []float64{.42, .44, .46, .88, .43, .45, .72, .41, .47, .44, .65, .43}
	flat := Layer{Type: LayerAudioBars, Options: map[string]any{
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	clean := flat
	clean.Options = map[string]any{
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": .75,
	}
	baseline := TransformAudioBands(input, len(input), flat, nil)
	result := TransformAudioBands(input, len(input), clean, nil)
	if result[0] >= baseline[0]*.8 || result[3]-result[0] <= baseline[3]-baseline[0]+.1 {
		t.Fatalf("公共频谱底座没有被压低或尖峰未保留: baseline=%v result=%v", baseline, result)
	}
	if result[3] <= result[6] || result[6] <= result[1] {
		t.Fatalf("真实非规则峰值次序被破坏: %v", result)
	}
}

func TestTransformAudioBandsBassBoostOnlyScalesExistingLowEnergy(t *testing.T) {
	neutral := Layer{Type: LayerAudioBars, Options: map[string]any{
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	boosted := neutral
	boosted.Options = map[string]any{
		"gain": 1.0, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.8, "floor_reduction": 0.0,
	}
	input := []float64{.25, 0, 0, 0, .25, 0, 0, 0}
	base := TransformAudioBands(input, len(input), neutral, nil)
	result := TransformAudioBands(input, len(input), boosted, nil)
	if result[0] <= base[0] || math.Abs(result[4]-base[4]) > .02 {
		t.Fatalf("低频增强的频率范围异常: base=%v boosted=%v", base, result)
	}
	for _, index := range []int{1, 2, 3, 5, 6, 7} {
		if result[index] != 0 {
			t.Fatalf("低频增强给空频带制造了能量: index=%d values=%v", index, result)
		}
	}
}

func TestTransformAudioBandsSoftShoulderAvoidsAdjacentFlatTops(t *testing.T) {
	layer := Layer{Type: LayerAudioBars, Options: map[string]any{
		"gain": 1.2, "sensitivity": 1.0, "noise_gate": 0.0, "smoothing": 0.0,
		"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	values := TransformAudioBands([]float64{.72, .86, 1.0}, 3, layer, nil)
	if !(values[0] < values[1] && values[1] < values[2] && values[2] <= 1) {
		t.Fatalf("高能量频带被硬裁成平顶: %v", values)
	}
}

func TestTransformAudioBandsDoesNotInventEnergyInZeroBands(t *testing.T) {
	layer := Layer{Type: LayerAudioRing, Options: map[string]any{
		"gain": 2.0, "sensitivity": 2.0, "noise_gate": 0.0, "contrast": 2.0, "separation": 1.0, "smoothing": 0.0,
	}}
	got := TransformAudioBands(make([]float64, 8), 16, layer, nil)
	for _, value := range got {
		if value != 0 {
			t.Fatalf("零输入产生了虚假能量: %#v", got)
		}
	}
}
