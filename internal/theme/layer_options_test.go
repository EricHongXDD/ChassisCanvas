package theme

import "testing"

func TestWaveshareAudioDefaultLayersShareShortKeys(t *testing.T) {
	bars := Layer{Type: LayerAudioBars}
	ring := Layer{Type: LayerAudioRing, Options: map[string]any{"source": "device_mic"}}
	if prefix := WaveshareAudioDataPrefix(bars); prefix != "A" {
		t.Fatalf("默认主机频谱命名空间不够短: %q", prefix)
	}
	if WaveshareAudioDataPrefix(ring) != WaveshareAudioDataPrefix(bars) {
		t.Fatal("旧 device_mic 图层没有与默认主机频谱共享数据键")
	}
	if got := WaveshareAudioBandKey(bars, 7); got != "A07" {
		t.Fatalf("默认频带键错误: %q", got)
	}
	if got := WaveshareAudioPeakKey(bars, 7); got != "P07" {
		t.Fatalf("默认峰值键不够短: %q", got)
	}
}

func TestWaveshareAudioNoiseGateGetsIndependentNamespace(t *testing.T) {
	first := Layer{Type: LayerAudioBars, Options: map[string]any{"noise_gate": .01}}
	second := Layer{Type: LayerAudioRing, Options: map[string]any{"noise_gate": .08}}
	if WaveshareAudioDataPrefix(first) == WaveshareAudioDataPrefix(second) {
		t.Fatal("不同静音门限的图层错误共享了数据键")
	}
}

func TestWaveshareAudioGeometryAndGlowDoNotIncreasePacketNamespaces(t *testing.T) {
	base := Layer{Type: LayerAudioBars}
	artistic := Layer{Type: LayerAudioRing, Options: map[string]any{
		"style": "waveform", "direction": "outward", "count": 96, "thickness": 2,
		"glow": true, "glow_radius": 12, "gradient_start": "FF3CAC", "gradient_end": "45E8FF",
	}}
	if got := WaveshareAudioDataPrefix(artistic); got != WaveshareAudioDataPrefix(base) || got != "A" {
		t.Fatalf("纯驻留视觉选项错误增加了小包命名空间: %q", got)
	}
}

func TestWaveshareAudioBassResponseGetsIndependentNamespace(t *testing.T) {
	base := Layer{Type: LayerAudioBars}
	custom := Layer{Type: LayerAudioBars, Options: map[string]any{"bass_boost": 1.8, "floor_reduction": .8}}
	if WaveshareAudioDataPrefix(base) == WaveshareAudioDataPrefix(custom) {
		t.Fatal("不同低频/底座响应错误共享了数据键")
	}
}

func TestWaveshareAudioPeakSettingsDoNotSplitBaseNamespace(t *testing.T) {
	withoutPeak := Layer{Type: LayerAudioBars}
	defaultPeak := Layer{Type: LayerAudioRing, Options: map[string]any{
		"peak_hold": true,
	}}
	customPeak := Layer{Type: LayerAudioBars, Options: map[string]any{
		"peak_hold": true, "peak_hold_ms": 900.0, "peak_decay": 2.4,
	}}
	for _, layer := range []Layer{defaultPeak, customPeak} {
		if got := WaveshareAudioDataPrefix(layer); got != WaveshareAudioDataPrefix(withoutPeak) || got != "A" {
			t.Fatalf("峰值设置错误拆分了基础频带命名空间: %q", got)
		}
		if got := WaveshareAudioBandKey(layer, 3); got != "A03" {
			t.Fatalf("峰值设置改变了基础频带键: %q", got)
		}
	}
	if got := WaveshareAudioPeakKey(defaultPeak, 3); got != "P03" {
		t.Fatalf("默认峰值组合没有使用短键: %q", got)
	}
	if WaveshareAudioPeakKey(customPeak, 3) == WaveshareAudioPeakKey(defaultPeak, 3) {
		t.Fatal("不同保持/回落设置错误共享峰值键")
	}
	sameCustomPeak := Layer{Type: LayerAudioRing, Options: map[string]any{
		"peak_hold": true, "peak_hold_ms": 900, "peak_decay": 2.4,
	}}
	if WaveshareAudioPeakKey(customPeak, 3) != WaveshareAudioPeakKey(sameCustomPeak, 3) {
		t.Fatal("相同变换与峰值设置没有共享峰值键")
	}
}

func TestWaveshareAudioPeakKeyIncludesTransformOptions(t *testing.T) {
	first := Layer{Type: LayerAudioBars, Options: map[string]any{
		"peak_hold": true, "noise_gate": .01,
	}}
	second := Layer{Type: LayerAudioRing, Options: map[string]any{
		"peak_hold": true, "noise_gate": .08,
	}}
	if WaveshareAudioPeakKey(first, 0) == WaveshareAudioPeakKey(second, 0) {
		t.Fatal("不同音频变换错误共享峰值键")
	}
}

func TestAssignWaveshareAudioPrefixesCompactsAndSharesTransforms(t *testing.T) {
	manifest := Manifest{Layers: []Layer{
		{ID: "bars", Type: LayerAudioBars, Options: map[string]any{"noise_gate": .025, "peak_hold": true}},
		{ID: "ring", Type: LayerAudioRing, Options: map[string]any{"noise_gate": .025, "peak_hold": true}},
		{ID: "second", Type: LayerAudioBars, Options: map[string]any{"noise_gate": .08, "peak_hold": true, "peak_hold_ms": 900}},
	}}
	assigned := AssignWaveshareAudioPrefixes(manifest)
	firstBand := WaveshareAudioBandKey(assigned.Layers[0], 7)
	sharedBand := WaveshareAudioBandKey(assigned.Layers[1], 7)
	secondBand := WaveshareAudioBandKey(assigned.Layers[2], 7)
	if firstBand != sharedBand || len(firstBand) != 3 {
		t.Fatalf("相同变换没有共享三字符短键: first=%q shared=%q", firstBand, sharedBand)
	}
	if secondBand == firstBand || len(secondBand) != 3 {
		t.Fatalf("不同变换的短键分配错误: first=%q second=%q", firstBand, secondBand)
	}
	firstPeak := WaveshareAudioPeakKey(assigned.Layers[0], 7)
	sharedPeak := WaveshareAudioPeakKey(assigned.Layers[1], 7)
	secondPeak := WaveshareAudioPeakKey(assigned.Layers[2], 7)
	if firstPeak != sharedPeak || len(firstPeak) != 4 || secondPeak == firstPeak || len(secondPeak) != 4 {
		t.Fatalf("峰值短键分配错误: first=%q shared=%q second=%q", firstPeak, sharedPeak, secondPeak)
	}
	// 输入清单的 options map 不能被原地污染，否则取消保存也会改变编辑器状态。
	if _, exists := manifest.Layers[0].Options[waveshareAudioDataPrefixOption]; exists {
		t.Fatal("短键分配原地修改了输入清单")
	}
}

func TestAssignWaveshareAudioPrefixesIsDeterministicAcrossLayerOrder(t *testing.T) {
	first := Layer{ID: "first", Type: LayerAudioBars, Options: map[string]any{"noise_gate": .01}}
	second := Layer{ID: "second", Type: LayerAudioRing, Options: map[string]any{"noise_gate": .08}}
	left := AssignWaveshareAudioPrefixes(Manifest{Layers: []Layer{first, second}})
	right := AssignWaveshareAudioPrefixes(Manifest{Layers: []Layer{second, first}})
	leftByID := map[string]string{}
	for _, layer := range left.Layers {
		leftByID[layer.ID] = WaveshareAudioDataPrefix(layer)
	}
	for _, layer := range right.Layers {
		if got := WaveshareAudioDataPrefix(layer); got != leftByID[layer.ID] {
			t.Fatalf("调整图层顺序后短键漂移: layer=%s got=%q want=%q", layer.ID, got, leftByID[layer.ID])
		}
	}
}

func TestWaveshareAudioDefaultBarsAndRingReferenceAllCompiledBands(t *testing.T) {
	bars := Layer{Type: LayerAudioBars, Bounds: Rect{Width: 1, Height: 1}}
	ring := Layer{Type: LayerAudioRing, Bounds: Rect{Width: 1, Height: 1}}
	set := make(map[int]bool)
	for _, layer := range []Layer{bars, ring} {
		for _, index := range WaveshareAudioBandIndexes(layer, 1024, 600) {
			set[index] = true
		}
	}
	if len(set) != WaveshareAudioBandCount {
		t.Fatalf("默认环状频谱应覆盖全部 %d 个编译频带，实际 %d: %v", WaveshareAudioBandCount, len(set), set)
	}
}

func TestWaveshareFourwayBarsIndexesMatchDelegatedRingCount(t *testing.T) {
	cases := []struct {
		name    string
		bounds  Rect
		options map[string]any
		want    int
	}{
		{name: "缺省爆发", bounds: Rect{Width: .5, Height: .5}, options: map[string]any{"style": "burst"}, want: DefaultAudioRingCount},
		{name: "窄边界四向", bounds: Rect{Width: 1.0 / 1024, Height: 1.0 / 600}, options: map[string]any{"direction": "fourway", "count": 24}, want: 24},
		{name: "环状下限", bounds: Rect{Width: 1.0 / 1024, Height: 1.0 / 600}, options: map[string]any{"style": "burst", "count": 6}, want: 12},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			layer := Layer{Type: LayerAudioBars, Bounds: test.bounds, Options: test.options}
			if got := len(WaveshareAudioBandIndexes(layer, 1024, 600)); got != test.want {
				t.Fatalf("委托环状编译的发包索引数错误: got=%d want=%d", got, test.want)
			}
		})
	}
}
