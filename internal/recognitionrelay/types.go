// Package recognitionrelay 实现 CaseScreen recognition-v1 自托管识歌中继。
//
// 屏端只持有可撤销设备令牌；第三方识歌凭据和 QQ/网易云原生 ID 映射只存在于服务端。
package recognitionrelay

import (
	"context"
	"errors"
	"time"

	"casescreen/internal/edgerecognition"
)

const (
	// RecognizePath 与屏端 HTTPProvider 使用的正式接口路径一致。
	RecognizePath = "/v1/recognize"
	// HealthPath 仅供同机反向代理执行存活检查，不包含运行配置。
	HealthPath = "/healthz"
)

var (
	// ErrNoMatch 表示提供方正常完成请求但没有识别结果。
	ErrNoMatch = edgerecognition.ErrNoMatch
	// ErrMappingConflict 表示静态映射与识别证据互相矛盾。
	ErrMappingConflict = errors.New("平台原生 ID 映射与识别证据冲突")
)

// Provider 只负责把 PCM 或兼容声纹转换为稳定的录音身份。
// Provider 返回的 Source、TrackID 和 NativeIDVerified 会被服务端无条件清除，
// 因此第三方元数据不能绕过显式 NativeMapper。
type Provider interface {
	Recognize(context.Context, edgerecognition.Submission) (edgerecognition.Response, error)
}

// NativeIdentity 是经显式映射验证的平台原生歌曲身份。
type NativeIdentity struct {
	Source  string
	TrackID string
}

// NativeMapper 只使用 stable_id/ISRC 等强证据解析平台原生 ID，不得按标题搜索。
type NativeMapper interface {
	Resolve(context.Context, edgerecognition.Result, string) (NativeIdentity, bool, error)
}

// TemporaryError 允许上游把退避时间安全地传给屏端，不包含第三方响应正文。
type TemporaryError interface {
	error
	RetryAfter() time.Duration
}

type temporaryError struct {
	message string
	after   time.Duration
}

func (err temporaryError) Error() string             { return err.message }
func (err temporaryError) RetryAfter() time.Duration { return err.after }
