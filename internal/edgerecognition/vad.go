package edgerecognition

import (
	"errors"
	"math"
)

const minimumDBFS = -120.0

// VADConfig 使用短帧能量过滤静音和偶发碰撞声，避免把无声 PCM 上传到中继。
type VADConfig struct {
	FrameMilliseconds  int
	RMSGateDBFS        float64
	PeakGateDBFS       float64
	MinimumActiveRatio float64
}

// Activity 是一次采样的轻量诊断，不包含任何原始声音。
type Activity struct {
	RMSDBFS     float64
	PeakDBFS    float64
	ActiveRatio float64
	FrameCount  int
	Active      bool
}

// DefaultVADConfig 对机箱内普通 MEMS 麦克风保留较低门限，同时要求至少 12%
// 的帧持续有声，避免风扇底噪或单次敲击触发联网识歌。
func DefaultVADConfig() VADConfig {
	return VADConfig{
		FrameMilliseconds:  20,
		RMSGateDBFS:        -48,
		PeakGateDBFS:       -36,
		MinimumActiveRatio: 0.12,
	}
}

// AnalyzePCM 分析 16 kHz 单声道 S16_LE；每帧先去直流分量再计算能量。
func AnalyzePCM(pcm []byte, config VADConfig) (Activity, error) {
	config, err := normalizeVADConfig(config)
	if err != nil {
		return Activity{}, err
	}
	if len(pcm) == 0 || len(pcm)%BytesPerSample != 0 {
		return Activity{}, errors.New("PCM 必须是非空的 S16_LE 完整样本")
	}
	frameSamples := SampleRateHz * config.FrameMilliseconds / 1000
	if frameSamples <= 0 {
		return Activity{}, errors.New("VAD 帧长度无效")
	}
	totalSamples := len(pcm) / BytesPerSample
	frameCount := (totalSamples + frameSamples - 1) / frameSamples
	activeFrames := 0
	globalSquare := float64(0)
	globalPeak := float64(0)

	for frame := 0; frame < frameCount; frame++ {
		start := frame * frameSamples
		end := start + frameSamples
		if end > totalSamples {
			end = totalSamples
		}
		count := end - start
		if count <= 0 {
			continue
		}
		mean := float64(0)
		for sample := start; sample < end; sample++ {
			mean += float64(pcmSample(pcm, sample))
		}
		mean /= float64(count)

		frameSquare := float64(0)
		framePeak := float64(0)
		for sample := start; sample < end; sample++ {
			value := float64(pcmSample(pcm, sample)) - mean
			frameSquare += value * value
			absolute := math.Abs(value)
			if absolute > framePeak {
				framePeak = absolute
			}
		}
		globalSquare += frameSquare
		if framePeak > globalPeak {
			globalPeak = framePeak
		}
		frameRMS := math.Sqrt(frameSquare / float64(count))
		if amplitudeDBFS(frameRMS) >= config.RMSGateDBFS &&
			amplitudeDBFS(framePeak) >= config.PeakGateDBFS {
			activeFrames++
		}
	}

	activity := Activity{
		RMSDBFS:     amplitudeDBFS(math.Sqrt(globalSquare / float64(totalSamples))),
		PeakDBFS:    amplitudeDBFS(globalPeak),
		ActiveRatio: float64(activeFrames) / float64(frameCount),
		FrameCount:  frameCount,
	}
	activity.Active = activity.ActiveRatio >= config.MinimumActiveRatio
	return activity, nil
}

func normalizeVADConfig(config VADConfig) (VADConfig, error) {
	if config == (VADConfig{}) {
		config = DefaultVADConfig()
	}
	if config.FrameMilliseconds < 10 || config.FrameMilliseconds > 100 ||
		1000%config.FrameMilliseconds != 0 {
		return VADConfig{}, errors.New("VAD 帧长度必须是 10..100 毫秒且能整除 1 秒")
	}
	if config.RMSGateDBFS >= 0 || config.RMSGateDBFS < minimumDBFS ||
		config.PeakGateDBFS >= 0 || config.PeakGateDBFS < minimumDBFS {
		return VADConfig{}, errors.New("VAD 门限必须位于 -120..0 dBFS")
	}
	if config.MinimumActiveRatio <= 0 || config.MinimumActiveRatio > 1 {
		return VADConfig{}, errors.New("VAD 有声帧比例必须位于 0..1")
	}
	return config, nil
}

func pcmSample(pcm []byte, index int) int16 {
	offset := index * BytesPerSample
	return int16(uint16(pcm[offset]) | uint16(pcm[offset+1])<<8)
}

func amplitudeDBFS(amplitude float64) float64 {
	if amplitude <= 0 {
		return minimumDBFS
	}
	value := 20 * math.Log10(amplitude/32768)
	if value < minimumDBFS {
		return minimumDBFS
	}
	if value > 0 {
		return 0
	}
	return value
}
