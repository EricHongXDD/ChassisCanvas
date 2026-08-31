// Package edgerecognition 实现副屏本地采样、静音门控和低频识歌状态机。
//
// 原始 PCM 只在内存中流转；本包不会把录音写入 TF 卡或其他持久介质。
package edgerecognition

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	ProtocolVersion = 1
	SampleRateHz    = 16_000
	ChannelCount    = 1
	BytesPerSample  = 2
	AudioFormat     = "pcm_s16le"
)

var (
	ErrNoMatch          = errors.New("识歌服务没有命中歌曲")
	ErrInvalidResult    = errors.New("识歌服务返回的歌曲身份无效")
	ErrAudioUnavailable = errors.New("屏端录音设备不可用")
)

// Capture 是一次 16 kHz、单声道、S16_LE 原始采样。
// SampledAt 表示最后一个样本附近的本机时间，便于把识别偏移换算为当前进度。
type Capture struct {
	PCM       []byte
	SampledAt time.Time
	Duration  time.Duration
}

// Recorder 只负责产生短时内存采样，不得持久化 PCM。
type Recorder interface {
	Capture(context.Context, time.Duration) (Capture, error)
}

// Fingerprint 是屏端提取的紧凑声纹。Data 仍只在内存中流转；Algorithm 和
// Version 让中继拒绝不兼容格式，避免把不透明字节错误解释为另一种声纹。
type Fingerprint struct {
	Algorithm string
	Version   string
	Data      []byte
}

// FingerprintExtractor 可以由固件集成的 ARM32 提取器实现，也可以使用本包的
// stdin/stdout 外部命令适配器。没有提取器时才回退上传 PCM。
type FingerprintExtractor interface {
	Extract(context.Context, Capture) (Fingerprint, error)
}

// Submission 在声纹存在时不携带 PCM；PCM 只作为显式回退路径。
type Submission struct {
	SampledAt   time.Time
	Duration    time.Duration
	PCM         []byte
	Fingerprint *Fingerprint
}

// Result 是中继返回的稳定识别身份。
// StableID 是识歌目录中的稳定 ID；ISRC 可为空，但绝不能由标题猜测。
// Source/TrackID 只有 NativeIDVerified 为 true 时才能进入 QQ/网易云精确取词链路。
type Result struct {
	StableID         string
	ISRC             string
	Title            string
	Artist           string
	Album            string
	Source           string
	TrackID          string
	NativeIDVerified bool
	Duration         time.Duration
	PlaybackOffset   time.Duration
	Confidence       float64
	SampledAt        time.Time
}

// Response 允许中继在未命中或限流时建议下一次采样时间。
type Response struct {
	Matched    bool
	Result     Result
	RetryAfter time.Duration
}

// Provider 接收紧凑声纹或 PCM 回退并返回稳定歌曲身份。第三方识歌密钥应只
// 存在于中继，屏端只持有可撤销的中继令牌。
type Provider interface {
	Recognize(context.Context, Submission) (Response, error)
}

// RetryAfterError 让 HTTP 429/503 等响应把服务端退避建议传给状态机。
type RetryAfterError interface {
	error
	RetryAfter() time.Duration
}

func trimResult(result Result) Result {
	result.StableID = strings.TrimSpace(result.StableID)
	result.ISRC = strings.ToUpper(strings.TrimSpace(result.ISRC))
	result.Title = strings.TrimSpace(result.Title)
	result.Artist = strings.TrimSpace(result.Artist)
	result.Album = strings.TrimSpace(result.Album)
	result.Source = strings.ToLower(strings.TrimSpace(result.Source))
	result.TrackID = strings.TrimSpace(result.TrackID)
	return result
}
