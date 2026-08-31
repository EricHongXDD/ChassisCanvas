package edgerecognition

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestAnalyzePCMRejectsSilenceAndDCBias(t *testing.T) {
	pcm := make([]byte, SampleRateHz*BytesPerSample)
	for index := 0; index < SampleRateHz; index++ {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(int16(1200)))
	}
	activity, err := AnalyzePCM(pcm, DefaultVADConfig())
	if err != nil {
		t.Fatal(err)
	}
	if activity.Active || activity.ActiveRatio != 0 {
		t.Fatalf("直流偏置不应被当作声音: %#v", activity)
	}
}

func TestAnalyzePCMDetectsSustainedMusicLikeSignal(t *testing.T) {
	pcm := sinePCM(SampleRateHz, 440, 9000)
	activity, err := AnalyzePCM(pcm, DefaultVADConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !activity.Active || activity.ActiveRatio < 0.95 || activity.RMSDBFS > -8 || activity.RMSDBFS < -15 {
		t.Fatalf("持续音频应通过 VAD: %#v", activity)
	}
}

func TestAnalyzePCMRejectsSingleShortClick(t *testing.T) {
	pcm := make([]byte, SampleRateHz*BytesPerSample)
	for index := 0; index < 40; index++ {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(int16(20_000)))
	}
	activity, err := AnalyzePCM(pcm, DefaultVADConfig())
	if err != nil {
		t.Fatal(err)
	}
	if activity.Active {
		t.Fatalf("单次短碰撞不应触发识歌: %#v", activity)
	}
}

func sinePCM(samples int, frequency, amplitude float64) []byte {
	pcm := make([]byte, samples*BytesPerSample)
	for index := 0; index < samples; index++ {
		value := int16(math.Sin(2*math.Pi*frequency*float64(index)/SampleRateHz) * amplitude)
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(value))
	}
	return pcm
}
