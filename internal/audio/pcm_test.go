package audio

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func TestDecodeFloat32Stereo(t *testing.T) {
	values := []float32{1, -1, 0.5, 0.5}
	data := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(data[index*4:], math.Float32bits(value))
	}
	decoded, err := DecodeInterleavedPCM(data, SampleFormat{Encoding: PCMFloat, Channels: 2, BitsPerSample: 32, BlockAlign: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || math.Abs(decoded[0]) > 1e-9 || math.Abs(decoded[1]-0.5) > 1e-9 {
		t.Fatalf("混音结果错误: %#v", decoded)
	}
}

func TestDecodeStereoLinearEqualWeightFoldDown(t *testing.T) {
	tests := []struct {
		name     string
		left     float32
		right    float32
		expected float64
	}{
		{name: "同相正信号幅值不变", left: 0.5, right: 0.5, expected: 0.5},
		{name: "同相负信号幅值不变", left: -0.25, right: -0.25, expected: -0.25},
		{name: "仅左声道", left: 1, right: 0, expected: 0.5},
		{name: "仅右声道", left: 0, right: -1, expected: -0.5},
		{name: "反相信号正确相消", left: 0.75, right: -0.75, expected: 0},
		{name: "反相信号交换声道后仍相消", left: -0.75, right: 0.75, expected: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := make([]byte, 8)
			binary.LittleEndian.PutUint32(data[0:4], math.Float32bits(test.left))
			binary.LittleEndian.PutUint32(data[4:8], math.Float32bits(test.right))
			decoded, err := DecodeInterleavedPCM(data, SampleFormat{
				Encoding: PCMFloat, Channels: 2, BitsPerSample: 32, BlockAlign: 8,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded) != 1 || math.Abs(decoded[0]-test.expected) > 1e-7 {
				t.Fatalf("线性下混结果错误: got=%#v want=%.9f", decoded, test.expected)
			}
		})
	}
}

func TestDecodeFloatTreatsNaNAndInfinityAsSilentChannels(t *testing.T) {
	values := []float32{
		float32(math.NaN()), 0.5,
		float32(math.Inf(1)), -0.5,
		float32(math.Inf(-1)), float32(math.NaN()),
	}
	data := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(data[index*4:], math.Float32bits(value))
	}
	decoded, err := DecodeInterleavedPCM(data, SampleFormat{
		Encoding: PCMFloat, Channels: 2, BitsPerSample: 32, BlockAlign: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.25, -0.25, 0}
	if len(decoded) != len(want) {
		t.Fatalf("非法浮点测试帧数错误: %#v", decoded)
	}
	for index := range want {
		if math.IsNaN(decoded[index]) || math.IsInf(decoded[index], 0) || math.Abs(decoded[index]-want[index]) > 1e-7 {
			t.Fatalf("第 %d 帧非法浮点处理错误: got=%.9f want=%.9f", index, decoded[index], want[index])
		}
	}
}

func TestDecodeStereoIndependentTonesDoNotCreateStrongFalseHarmonics(t *testing.T) {
	const (
		sampleRate  = 48000
		sampleCount = 4096
	)
	data := make([]byte, sampleCount*8)
	for index := 0; index < sampleCount; index++ {
		left := float32(0.7 * math.Sin(2*math.Pi*440*float64(index)/sampleRate))
		right := float32(0.7 * math.Sin(2*math.Pi*880*float64(index)/sampleRate))
		binary.LittleEndian.PutUint32(data[index*8:], math.Float32bits(left))
		binary.LittleEndian.PutUint32(data[index*8+4:], math.Float32bits(right))
	}
	decoded, err := DecodeInterleavedPCM(data, SampleFormat{
		Encoding: PCMFloat, Channels: 2, BitsPerSample: 32, BlockAlign: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	fundamental := math.Max(testToneMagnitude(decoded, sampleRate, 440), testToneMagnitude(decoded, sampleRate, 880))
	falseHarmonic := 0.0
	for _, frequency := range []float64{1320, 1760, 2200, 2640, 3080} {
		falseHarmonic = math.Max(falseHarmonic, testToneMagnitude(decoded, sampleRate, frequency))
	}
	if fundamental <= 0 || falseHarmonic >= fundamental*0.01 {
		t.Fatalf("线性下混产生强伪谐波: fundamental=%.9f false=%.9f", fundamental, falseHarmonic)
	}
}

func testToneMagnitude(samples []float64, sampleRate int, frequency float64) float64 {
	realPart := 0.0
	imaginaryPart := 0.0
	for index, sample := range samples {
		window := 0.5 - 0.5*math.Cos(2*math.Pi*float64(index)/float64(len(samples)-1))
		angle := -2 * math.Pi * frequency * float64(index) / float64(sampleRate)
		realPart += sample * window * math.Cos(angle)
		imaginaryPart += sample * window * math.Sin(angle)
	}
	return 4 * math.Hypot(realPart, imaginaryPart) / float64(len(samples))
}

func TestDecodeSigned24(t *testing.T) {
	data := []byte{0x00, 0x00, 0x80, 0xFF, 0xFF, 0x7F}
	decoded, err := DecodeInterleavedPCM(data, SampleFormat{Encoding: PCMSigned, Channels: 1, BitsPerSample: 24, BlockAlign: 3})
	if err != nil {
		t.Fatal(err)
	}
	if decoded[0] != -1 || decoded[1] < 0.999999 {
		t.Fatalf("24 位解码错误: %#v", decoded)
	}
}

func TestEngineBuildsSpectrum(t *testing.T) {
	engine := NewEngine(nil, 16)
	samples := make([]float64, 2048)
	for index := range samples {
		samples[index] = math.Sin(2 * math.Pi * 440 * float64(index) / 48000)
	}
	capturedAt := time.Unix(1, 0)
	if err := engine.consume(PCMFrame{CapturedAt: capturedAt, SampleRate: 48000, Samples: samples}); err != nil {
		t.Fatal(err)
	}
	frame := engine.snapshotAt(capturedAt)
	if len(frame.Bars) != 16 || frame.Level <= 0 {
		t.Fatalf("频谱未生成: %#v", frame)
	}
	frame.Bars[0] = 99
	if engine.snapshotAt(capturedAt).Bars[0] == 99 {
		t.Fatal("Snapshot 泄漏了内部切片")
	}
}
