package audio

import (
	"context"
	"errors"
	"time"
)

var errDefaultOutputEndpointChanged = errors.New("默认系统播放设备已变更")

// PCMFrame 是由系统音频采集器输出的单声道浮点样本。
type PCMFrame struct {
	CapturedAt time.Time
	SampleRate int
	Samples    []float64
	// Discontinuity 表示本帧前的样本或采样时间轴不连续，分析器必须丢弃旧窗口。
	Discontinuity bool
}

// Source 屏蔽 WASAPI 等平台细节。emit 必须在 Capture 返回前复制需要保留的数据。
type Source interface {
	Capture(context.Context, func(PCMFrame) error) error
}

// SourceStatus 说明回环采集实际跟随的系统输出。
// 它只用于诊断，不会改变或接管 Windows 的音频路由。
type SourceStatus struct {
	Endpoint     string
	EndpointID   string
	EndpointRole string
}

type sourceStatusProvider interface {
	SourceStatus() SourceStatus
}

type PCMEncoding uint8

const (
	PCMUnsigned PCMEncoding = iota + 1
	PCMSigned
	PCMFloat
)

// SampleFormat 描述小端、交错排列的 PCM 帧。
type SampleFormat struct {
	Encoding      PCMEncoding
	Channels      int
	BitsPerSample int
	BlockAlign    int
}

func (format SampleFormat) Validate() error {
	if format.Channels <= 0 || format.Channels > 32 {
		return errors.New("音频声道数无效")
	}
	bytesPerSample := (format.BitsPerSample + 7) / 8
	if bytesPerSample <= 0 || format.BlockAlign < bytesPerSample*format.Channels {
		return errors.New("音频块对齐无效")
	}
	switch format.Encoding {
	case PCMUnsigned:
		if format.BitsPerSample != 8 {
			return errors.New("无符号 PCM 仅支持 8 位")
		}
	case PCMSigned:
		if format.BitsPerSample != 16 && format.BitsPerSample != 24 && format.BitsPerSample != 32 {
			return errors.New("有符号 PCM 仅支持 16/24/32 位")
		}
	case PCMFloat:
		if format.BitsPerSample != 32 && format.BitsPerSample != 64 {
			return errors.New("浮点 PCM 仅支持 32/64 位")
		}
	default:
		return errors.New("音频编码无效")
	}
	return nil
}
