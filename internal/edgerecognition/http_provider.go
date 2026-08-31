package edgerecognition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maximumRecognitionResponse = 256 << 10

// HTTPConfig 配置自建或受信中继。屏端令牌应可在中继端单独撤销；第三方
// ACR/Shazam 等服务密钥不得放入本配置。
type HTTPConfig struct {
	Endpoint          string
	Token             string
	Client            *http.Client
	RequestTimeout    time.Duration
	AllowInsecureHTTP bool
	PreferredPlatform string
}

// HTTPProvider 实现 CaseScreen recognition-v1 JSON 协议。
type HTTPProvider struct {
	endpoint          string
	token             string
	client            *http.Client
	timeout           time.Duration
	preferredPlatform string
}

type recognitionRequest struct {
	ProtocolVersion   int                 `json:"protocol_version"`
	SampledAtUnixMS   int64               `json:"sampled_at_unix_ms"`
	PreferredPlatform string              `json:"preferred_platform"`
	AudioFormat       string              `json:"audio_format,omitempty"`
	SampleRateHz      int                 `json:"sample_rate_hz,omitempty"`
	Channels          int                 `json:"channels,omitempty"`
	SampleCount       int                 `json:"sample_count"`
	PCM               []byte              `json:"pcm_s16le_base64,omitempty"`
	DeviceFingerprint *fingerprintRequest `json:"device_fingerprint,omitempty"`
}

type fingerprintRequest struct {
	Algorithm string `json:"algorithm"`
	Version   string `json:"version"`
	Data      []byte `json:"data_base64"`
}

type recognitionResponse struct {
	ProtocolVersion int               `json:"protocol_version"`
	Matched         bool              `json:"matched"`
	RetryAfterMS    int64             `json:"retry_after_ms,omitempty"`
	Result          recognitionResult `json:"result"`
}

type recognitionResult struct {
	StableID         string  `json:"stable_id"`
	ISRC             string  `json:"isrc,omitempty"`
	Title            string  `json:"title"`
	Artist           string  `json:"artist"`
	Album            string  `json:"album,omitempty"`
	Source           string  `json:"source,omitempty"`
	TrackID          string  `json:"track_id,omitempty"`
	NativeIDVerified bool    `json:"native_id_verified"`
	DurationMS       int64   `json:"duration_ms,omitempty"`
	PlaybackOffsetMS int64   `json:"playback_offset_ms"`
	Confidence       float64 `json:"confidence"`
	SampledAtUnixMS  int64   `json:"sampled_at_unix_ms"`
}

type retryableHTTPError struct {
	status int
	after  time.Duration
}

func (err retryableHTTPError) Error() string {
	return fmt.Sprintf("识歌中继暂时不可用（HTTP %d）", err.status)
}

func (err retryableHTTPError) RetryAfter() time.Duration { return err.after }

// NewHTTPProvider 验证地址和令牌，并禁止跨地址重定向以避免泄露 Bearer 令牌。
func NewHTTPProvider(config HTTPConfig) (*HTTPProvider, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || strings.Contains(endpoint, "#") ||
		parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, errors.New("识歌中继地址无效")
	}
	if parsed.Scheme != "https" && !(config.AllowInsecureHTTP && parsed.Scheme == "http") {
		return nil, errors.New("识歌中继必须使用 HTTPS；本地调试 HTTP 需显式启用")
	}
	token := strings.TrimSpace(config.Token)
	if !validRecognitionDeviceToken(token) {
		return nil, errors.New("识歌中继令牌格式无效")
	}
	timeout := config.RequestTimeout
	if timeout == 0 {
		timeout = 12 * time.Second
	}
	if timeout < time.Second || timeout > 30*time.Second {
		return nil, errors.New("识歌中继请求超时必须在 1..30 秒")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = timeout
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	preferred, err := normalizePreferredPlatform(config.PreferredPlatform)
	if err != nil {
		return nil, err
	}
	return &HTTPProvider{
		endpoint: parsed.String(), token: token, client: &copyClient, timeout: timeout,
		preferredPlatform: preferred,
	}, nil
}

func validRecognitionDeviceToken(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !((character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func (provider *HTTPProvider) Recognize(ctx context.Context, submission Submission) (Response, error) {
	if provider == nil || provider.client == nil {
		return Response{}, errors.New("识歌 HTTP 提供方尚未初始化")
	}
	if ctx == nil {
		return Response{}, errors.New("识歌请求上下文不能为空")
	}
	if submission.SampledAt.IsZero() || submission.Duration <= 0 {
		return Response{}, errors.New("识歌采样时间无效")
	}
	payload := recognitionRequest{
		ProtocolVersion: ProtocolVersion, SampledAtUnixMS: submission.SampledAt.UnixMilli(),
		PreferredPlatform: provider.preferredPlatform,
	}
	if submission.Fingerprint != nil {
		fingerprint := submission.Fingerprint
		if !safeProtocolLabel(fingerprint.Algorithm, maximumFingerprintAlgorithmLength) ||
			!safeProtocolLabel(fingerprint.Version, maximumFingerprintVersionLength) ||
			len(fingerprint.Data) == 0 || len(fingerprint.Data) > maximumFingerprintBytes {
			return Response{}, errors.New("识歌声纹格式无效")
		}
		payload.SampleCount = int(submission.Duration.Seconds() * SampleRateHz)
		payload.DeviceFingerprint = &fingerprintRequest{
			Algorithm: fingerprint.Algorithm, Version: fingerprint.Version, Data: fingerprint.Data,
		}
	} else {
		if len(submission.PCM) == 0 || len(submission.PCM)%BytesPerSample != 0 {
			return Response{}, errors.New("识歌 PCM 回退格式无效")
		}
		payload.AudioFormat = AudioFormat
		payload.SampleRateHz = SampleRateHz
		payload.Channels = ChannelCount
		payload.SampleCount = len(submission.PCM) / BytesPerSample
		payload.PCM = submission.PCM
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, errors.New("编码识歌请求失败")
	}
	defer clear(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("创建识歌请求失败")
	}
	request.Header.Set("Authorization", "Bearer "+provider.token)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-CaseScreen-Protocol", "recognition-v1")
	response, err := provider.client.Do(request)
	if err != nil {
		return Response{}, errors.New("连接识歌中继失败")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		after := retryAfter(response.Header.Get("Retry-After"))
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable ||
			response.StatusCode == http.StatusGatewayTimeout {
			return Response{}, retryableHTTPError{status: response.StatusCode, after: after}
		}
		return Response{}, fmt.Errorf("识歌中继拒绝请求（HTTP %d）", response.StatusCode)
	}
	if response.Header.Get("X-CaseScreen-Protocol") != "recognition-v1" {
		return Response{}, errors.New("识歌中继响应协议标识无效")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Response{}, errors.New("识歌中继响应类型无效")
	}
	if response.ContentLength > maximumRecognitionResponse {
		return Response{}, errors.New("识歌中继响应超过大小上限")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumRecognitionResponse+1))
	if err != nil {
		return Response{}, errors.New("读取识歌中继响应失败")
	}
	defer clear(data)
	if len(data) > maximumRecognitionResponse {
		return Response{}, errors.New("识歌中继响应超过大小上限")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded recognitionResponse
	if err := decoder.Decode(&decoded); err != nil {
		return Response{}, errors.New("识歌中继响应不是有效 JSON")
	}
	if err := requireRecognitionResponseEOF(decoder); err != nil {
		return Response{}, errors.New("识歌中继响应包含多余内容")
	}
	if decoded.ProtocolVersion != ProtocolVersion {
		return Response{}, errors.New("识歌中继协议版本不兼容")
	}
	if decoded.RetryAfterMS < 0 || decoded.RetryAfterMS > int64((24*time.Hour)/time.Millisecond) {
		return Response{}, errors.New("识歌中继退避时间无效")
	}
	result := Result{
		StableID: decoded.Result.StableID, ISRC: decoded.Result.ISRC,
		Title: decoded.Result.Title, Artist: decoded.Result.Artist, Album: decoded.Result.Album,
		Source: decoded.Result.Source, TrackID: decoded.Result.TrackID,
		NativeIDVerified: decoded.Result.NativeIDVerified,
		Duration:         time.Duration(decoded.Result.DurationMS) * time.Millisecond,
		PlaybackOffset:   time.Duration(decoded.Result.PlaybackOffsetMS) * time.Millisecond,
		Confidence:       decoded.Result.Confidence,
	}
	if decoded.Result.SampledAtUnixMS != 0 {
		result.SampledAt = time.UnixMilli(decoded.Result.SampledAtUnixMS)
	}
	return Response{
		Matched: decoded.Matched, Result: result,
		RetryAfter: time.Duration(decoded.RetryAfterMS) * time.Millisecond,
	}, nil
}

func requireRecognitionResponseEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("存在第二个 JSON 值")
	}
	return err
}

func normalizePreferredPlatform(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", "auto":
		return "auto", nil
	case "qq", "qqmusic":
		return "qq", nil
	case "netease", "cloudmusic", "163":
		return "netease", nil
	default:
		return "", errors.New("识歌偏好平台必须是 auto、qq 或 netease")
	}
}

func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 || seconds > int((24*time.Hour)/time.Second) {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
