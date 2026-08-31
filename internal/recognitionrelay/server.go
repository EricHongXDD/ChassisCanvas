package recognitionrelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"casescreen/internal/edgerecognition"
	"casescreen/internal/platformid"
)

const (
	defaultMaximumRequestBytes = int64(2 << 20)
	defaultProviderTimeout     = 12 * time.Second
	defaultMinimumAudio        = 5 * time.Second
	defaultMaximumAudio        = 15 * time.Second
	defaultRequestsPerSecond   = 1.0
	defaultBurst               = 3
	defaultMaximumClients      = 4096
	defaultClientTTL           = 30 * time.Minute
	defaultMaximumConcurrent   = 4
	defaultMaximumPastAge      = 10 * time.Minute
	defaultMaximumFutureSkew   = 2 * time.Minute
	maximumFingerprintBytes    = 64 << 10
	maximumRetryAfter          = 24 * time.Hour
)

type contextKey int

const deviceIDKey contextKey = iota

// Config 为中继定义严格的资源、安全和时钟边界。
type Config struct {
	Tokens                  *TokenVerifier
	Provider                Provider
	Mapper                  NativeMapper
	MaximumRequestBytes     int64
	ProviderTimeout         time.Duration
	MinimumAudioDuration    time.Duration
	MaximumAudioDuration    time.Duration
	RequestsPerSecond       float64
	Burst                   int
	MaximumClients          int
	ClientTTL               time.Duration
	MaximumConcurrent       int
	UnauthRequestsPerSecond float64
	UnauthBurst             int
	MaximumPastAge          time.Duration
	MaximumFutureSkew       time.Duration
	Now                     func() time.Time
}

// Server 只暴露识歌和不含配置的健康检查，不代理桌面控制接口。
type Server struct {
	tokens              *TokenVerifier
	provider            Provider
	mapper              NativeMapper
	maximumRequestBytes int64
	providerTimeout     time.Duration
	minimumAudio        time.Duration
	maximumAudio        time.Duration
	maximumPastAge      time.Duration
	maximumFutureSkew   time.Duration
	providerGate        chan struct{}
	deviceLimiter       *rateLimiter
	unauthLimiter       *rateLimiter
	now                 func() time.Time
	handler             http.Handler
}

type providerOutcome struct {
	response edgerecognition.Response
	err      error
}

// New 验证配置并构造 recognition-v1 服务。
func New(config Config) (*Server, error) {
	if config.Tokens == nil || len(config.Tokens.records) == 0 {
		return nil, errors.New("识歌中继必须配置设备令牌摘要")
	}
	if config.Provider == nil {
		return nil, errors.New("识歌中继必须配置识歌 provider")
	}
	if config.MaximumRequestBytes == 0 {
		config.MaximumRequestBytes = defaultMaximumRequestBytes
	}
	if config.ProviderTimeout == 0 {
		config.ProviderTimeout = defaultProviderTimeout
	}
	if config.MinimumAudioDuration == 0 {
		config.MinimumAudioDuration = defaultMinimumAudio
	}
	if config.MaximumAudioDuration == 0 {
		config.MaximumAudioDuration = defaultMaximumAudio
	}
	if config.RequestsPerSecond == 0 {
		config.RequestsPerSecond = defaultRequestsPerSecond
	}
	if config.Burst == 0 {
		config.Burst = defaultBurst
	}
	if config.MaximumClients == 0 {
		config.MaximumClients = defaultMaximumClients
	}
	if config.ClientTTL == 0 {
		config.ClientTTL = defaultClientTTL
	}
	if config.MaximumConcurrent == 0 {
		config.MaximumConcurrent = defaultMaximumConcurrent
	}
	if config.UnauthRequestsPerSecond == 0 {
		config.UnauthRequestsPerSecond = 20
	}
	if config.UnauthBurst == 0 {
		config.UnauthBurst = 40
	}
	if config.MaximumPastAge == 0 {
		config.MaximumPastAge = defaultMaximumPastAge
	}
	if config.MaximumFutureSkew == 0 {
		config.MaximumFutureSkew = defaultMaximumFutureSkew
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaximumRequestBytes < 128<<10 || config.MaximumRequestBytes > 16<<20 {
		return nil, errors.New("识歌请求大小上限必须在 128 KiB..16 MiB")
	}
	if config.ProviderTimeout < time.Second || config.ProviderTimeout > 30*time.Second {
		return nil, errors.New("识歌 provider 超时必须在 1..30 秒")
	}
	if config.MinimumAudioDuration < time.Second || config.MinimumAudioDuration > 15*time.Second ||
		config.MaximumAudioDuration < config.MinimumAudioDuration || config.MaximumAudioDuration > 30*time.Second {
		return nil, errors.New("识歌采样时长范围无效")
	}
	if config.RequestsPerSecond <= 0 || config.RequestsPerSecond > 10 || config.UnauthRequestsPerSecond <= 0 || config.UnauthRequestsPerSecond > 1000 {
		return nil, errors.New("识歌限流速率无效")
	}
	if config.Burst < 1 || config.Burst > 100 || config.UnauthBurst < 1 || config.UnauthBurst > 10000 {
		return nil, errors.New("识歌限流突发值无效")
	}
	if config.MaximumClients < 1 || config.MaximumClients > 65536 || config.ClientTTL < time.Second || config.ClientTTL > 24*time.Hour {
		return nil, errors.New("识歌限流客户端配置无效")
	}
	if config.MaximumConcurrent < 1 || config.MaximumConcurrent > 128 {
		return nil, errors.New("识歌 provider 并发上限必须在 1..128")
	}
	if config.MaximumPastAge < time.Minute || config.MaximumPastAge > 24*time.Hour ||
		config.MaximumFutureSkew < 0 || config.MaximumFutureSkew > time.Hour {
		return nil, errors.New("识歌采样时间窗口无效")
	}

	server := &Server{
		tokens: config.Tokens, provider: config.Provider, mapper: config.Mapper,
		maximumRequestBytes: config.MaximumRequestBytes, providerTimeout: config.ProviderTimeout,
		minimumAudio: config.MinimumAudioDuration, maximumAudio: config.MaximumAudioDuration,
		maximumPastAge: config.MaximumPastAge, maximumFutureSkew: config.MaximumFutureSkew,
		providerGate:  make(chan struct{}, config.MaximumConcurrent),
		deviceLimiter: newRateLimiter(config.RequestsPerSecond, config.Burst, config.MaximumClients, config.ClientTTL),
		unauthLimiter: newRateLimiter(config.UnauthRequestsPerSecond, config.UnauthBurst, config.MaximumClients, config.ClientTTL),
		now:           config.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, server.health)
	protected := server.limitUnauthenticated(server.authenticate(server.limitDevice(http.HandlerFunc(server.recognize))))
	mux.Handle(RecognizePath, protected)
	server.handler = securityHeaders(mux)
	return server, nil
}

// Handler 供测试或嵌入现有进程使用。
func (server *Server) Handler() http.Handler { return server.handler }

// Serve 在反向代理后方的监听器上运行，并在上下文取消后有界关闭。
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil || listener == nil {
		return errors.New("识歌中继运行上下文或监听器不能为空")
	}
	httpServer := &http.Server{
		Handler: server.handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: server.providerTimeout + 5*time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	shutdownDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = httpServer.Shutdown(shutdownContext)
			cancel()
		case <-shutdownDone:
		}
	}()
	err := httpServer.Serve(listener)
	close(shutdownDone)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (server *Server) health(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "protocol": "recognition-v1"})
}

func (server *Server) limitUnauthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !server.unauthLimiter.allow(remoteClient(request.RemoteAddr), server.now()) {
			writer.Header().Set("Retry-After", strconv.Itoa(server.unauthLimiter.retryAfterSeconds()))
			writeError(writer, http.StatusTooManyRequests, "请求过于频繁")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		candidate, headerOK := bearerToken(request.Header.Get("Authorization"))
		deviceID, tokenOK := server.tokens.Verify(candidate)
		if !headerOK || !tokenOK {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="CaseScreen Recognition Relay"`)
			writeError(writer, http.StatusUnauthorized, "未授权")
			return
		}
		ctx := context.WithValue(request.Context(), deviceIDKey, deviceID)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (server *Server) limitDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		deviceID, _ := request.Context().Value(deviceIDKey).(string)
		if deviceID == "" || !server.deviceLimiter.allow(deviceID, server.now()) {
			writer.Header().Set("Retry-After", strconv.Itoa(server.deviceLimiter.retryAfterSeconds()))
			writeError(writer, http.StatusTooManyRequests, "设备请求过于频繁")
			return
		}
		next.ServeHTTP(writer, request)
	})
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

func (server *Server) recognize(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if request.Header.Get("X-CaseScreen-Protocol") != "recognition-v1" {
		writeError(writer, http.StatusBadRequest, "识歌协议标识无效")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || strings.TrimSpace(request.Header.Get("Content-Encoding")) != "" {
		writeError(writer, http.StatusUnsupportedMediaType, "仅接受未压缩的 application/json")
		return
	}
	if request.ContentLength > server.maximumRequestBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "识歌请求超过大小上限")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, server.maximumRequestBytes)
	var payload recognitionRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		var maximumError *http.MaxBytesError
		if errors.As(err, &maximumError) {
			writeError(writer, http.StatusRequestEntityTooLarge, "识歌请求超过大小上限")
			return
		}
		writeError(writer, http.StatusBadRequest, "识歌请求不是有效 JSON")
		return
	}
	defer clear(payload.PCM)
	if payload.DeviceFingerprint != nil {
		defer clear(payload.DeviceFingerprint.Data)
	}
	if err := requireJSONEOF(decoder); err != nil {
		writeError(writer, http.StatusBadRequest, "识歌请求包含多余内容")
		return
	}
	submission, preferred, err := server.validateRequest(payload)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), server.providerTimeout)
	defer cancel()
	response, err := server.callProvider(ctx, submission)
	if err != nil {
		server.writeProviderError(writer, err)
		return
	}
	if !response.Matched {
		writeRecognitionResponse(writer, edgerecognition.Response{Matched: false, RetryAfter: boundedRetry(response.RetryAfter)})
		return
	}
	result, err := sanitizeProviderResult(response.Result, submission.SampledAt)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "识歌提供方返回无效结果")
		return
	}
	if server.mapper != nil {
		identity, found, mapErr := server.mapper.Resolve(ctx, result, preferred)
		if mapErr != nil {
			writeError(writer, http.StatusBadGateway, "平台原生 ID 映射与识别证据不一致")
			return
		}
		if found {
			if err := validateNativeIdentity(identity, preferred); err != nil {
				writeError(writer, http.StatusBadGateway, "平台原生 ID 映射无效")
				return
			}
			result.Source = strings.ToLower(strings.TrimSpace(identity.Source))
			result.TrackID = strings.TrimSpace(identity.TrackID)
			result.NativeIDVerified = true
		}
	}
	writeRecognitionResponse(writer, edgerecognition.Response{
		Matched: true, Result: result, RetryAfter: boundedRetry(response.RetryAfter),
	})
}

func (server *Server) validateRequest(payload recognitionRequest) (edgerecognition.Submission, string, error) {
	if payload.ProtocolVersion != edgerecognition.ProtocolVersion {
		return edgerecognition.Submission{}, "", errors.New("识歌协议版本不兼容")
	}
	preferred := strings.ToLower(strings.TrimSpace(payload.PreferredPlatform))
	switch preferred {
	case "", "auto":
		preferred = "auto"
	case "qq", "netease":
	default:
		return edgerecognition.Submission{}, "", errors.New("平台偏好必须是 auto、qq 或 netease")
	}
	minimumSamples := int64(server.minimumAudio) * edgerecognition.SampleRateHz / int64(time.Second)
	maximumSamples := int64(server.maximumAudio) * edgerecognition.SampleRateHz / int64(time.Second)
	if int64(payload.SampleCount) < minimumSamples || int64(payload.SampleCount) > maximumSamples {
		return edgerecognition.Submission{}, "", errors.New("识歌 sample_count 无效")
	}
	duration := time.Duration(payload.SampleCount) * time.Second / edgerecognition.SampleRateHz
	if duration < server.minimumAudio || duration > server.maximumAudio {
		return edgerecognition.Submission{}, "", errors.New("识歌采样时长超出服务端允许范围")
	}
	sampledAt := time.UnixMilli(payload.SampledAtUnixMS)
	now := server.now()
	if sampledAt.Before(now.Add(-server.maximumPastAge)) || sampledAt.After(now.Add(server.maximumFutureSkew)) {
		return edgerecognition.Submission{}, "", errors.New("识歌采样时间超出允许窗口")
	}
	hasPCM := len(payload.PCM) > 0
	hasFingerprint := payload.DeviceFingerprint != nil
	if hasPCM == hasFingerprint {
		return edgerecognition.Submission{}, "", errors.New("必须且只能提交 PCM 或设备声纹之一")
	}
	submission := edgerecognition.Submission{SampledAt: sampledAt, Duration: duration}
	if hasPCM {
		if payload.AudioFormat != edgerecognition.AudioFormat || payload.SampleRateHz != edgerecognition.SampleRateHz ||
			payload.Channels != edgerecognition.ChannelCount || len(payload.PCM) != payload.SampleCount*edgerecognition.BytesPerSample {
			return edgerecognition.Submission{}, "", errors.New("PCM 必须是 16 kHz、单声道、S16_LE 且长度与 sample_count 一致")
		}
		submission.PCM = payload.PCM
		return submission, preferred, nil
	}
	fingerprint := payload.DeviceFingerprint
	if payload.AudioFormat != "" || payload.SampleRateHz != 0 || payload.Channels != 0 ||
		!safeIdentifier(fingerprint.Algorithm, 1, 64) || !safeIdentifier(fingerprint.Version, 1, 32) ||
		len(fingerprint.Data) == 0 || len(fingerprint.Data) > maximumFingerprintBytes {
		return edgerecognition.Submission{}, "", errors.New("设备声纹字段无效或格式不受支持")
	}
	submission.Fingerprint = &edgerecognition.Fingerprint{
		Algorithm: fingerprint.Algorithm, Version: fingerprint.Version, Data: fingerprint.Data,
	}
	return submission, preferred, nil
}

// callProvider 即使插件未正确响应 context，也会按服务端期限返回；仍在运行的
// 插件继续占用并发槽，避免超时请求无限创建后台 goroutine。
func (server *Server) callProvider(ctx context.Context, submission edgerecognition.Submission) (edgerecognition.Response, error) {
	select {
	case server.providerGate <- struct{}{}:
	case <-ctx.Done():
		return edgerecognition.Response{}, ctx.Err()
	}
	owned := cloneSubmission(submission)
	resultChannel := make(chan providerOutcome, 1)
	go func() {
		outcome := providerOutcome{}
		defer func() {
			if recovered := recover(); recovered != nil {
				outcome.response = edgerecognition.Response{}
				outcome.err = errors.New("识歌 provider 异常")
			}
			clearSubmission(&owned)
			<-server.providerGate
			resultChannel <- outcome
		}()
		outcome.response, outcome.err = server.provider.Recognize(ctx, owned)
	}()
	select {
	case outcome := <-resultChannel:
		return outcome.response, outcome.err
	case <-ctx.Done():
		return edgerecognition.Response{}, ctx.Err()
	}
}

func cloneSubmission(source edgerecognition.Submission) edgerecognition.Submission {
	cloned := source
	cloned.PCM = append([]byte(nil), source.PCM...)
	if source.Fingerprint != nil {
		fingerprint := *source.Fingerprint
		fingerprint.Data = append([]byte(nil), source.Fingerprint.Data...)
		cloned.Fingerprint = &fingerprint
	}
	return cloned
}

func clearSubmission(submission *edgerecognition.Submission) {
	if submission == nil {
		return
	}
	clear(submission.PCM)
	if submission.Fingerprint != nil {
		clear(submission.Fingerprint.Data)
	}
}

func (server *Server) writeProviderError(writer http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writer.Header().Set("Retry-After", "10")
		writeError(writer, http.StatusGatewayTimeout, "识歌提供方超时")
		return
	}
	var temporary TemporaryError
	if errors.As(err, &temporary) {
		after := boundedRetry(temporary.RetryAfter())
		if after > 0 {
			writer.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(after.Seconds())))))
		}
		writeError(writer, http.StatusServiceUnavailable, "识歌提供方暂时不可用")
		return
	}
	if errors.Is(err, ErrNoMatch) {
		writeRecognitionResponse(writer, edgerecognition.Response{Matched: false, RetryAfter: 10 * time.Second})
		return
	}
	writeError(writer, http.StatusBadGateway, "识歌提供方请求失败")
}

func sanitizeProviderResult(result edgerecognition.Result, sampledAt time.Time) (edgerecognition.Result, error) {
	// 第三方 provider 永远无权直接证明 QQ/网易云原生 ID。
	result.Source = ""
	result.TrackID = ""
	result.NativeIDVerified = false
	result.StableID = strings.TrimSpace(result.StableID)
	result.ISRC = normalizeISRC(result.ISRC)
	result.Title = truncateText(strings.TrimSpace(result.Title), 512)
	result.Artist = truncateText(strings.TrimSpace(result.Artist), 512)
	result.Album = truncateText(strings.TrimSpace(result.Album), 512)
	result.SampledAt = sampledAt
	if !validStableID(result.StableID) || result.Title == "" || result.Artist == "" ||
		math.IsNaN(result.Confidence) || math.IsInf(result.Confidence, 0) || result.Confidence < 0 || result.Confidence > 1 ||
		result.Duration < 0 || result.Duration > 24*time.Hour || result.PlaybackOffset < 0 || result.PlaybackOffset > 24*time.Hour {
		return edgerecognition.Result{}, errors.New("识歌结果字段无效")
	}
	if result.ISRC != "" && !validISRC(result.ISRC) {
		result.ISRC = ""
	}
	if result.Duration > 0 && result.PlaybackOffset > result.Duration {
		result.PlaybackOffset = result.Duration
	}
	return result, nil
}

func validateNativeIdentity(identity NativeIdentity, preferred string) error {
	identity.Source = strings.ToLower(strings.TrimSpace(identity.Source))
	identity.TrackID = strings.TrimSpace(identity.TrackID)
	if preferred != "auto" && preferred != identity.Source {
		return errors.New("平台映射不符合请求偏好")
	}
	switch identity.Source {
	case "qq":
		if !platformid.QQ(identity.TrackID) {
			return errors.New("QQ 原生 ID 无效")
		}
	case "netease":
		if !platformid.Netease(identity.TrackID) {
			return errors.New("网易云原生 ID 无效")
		}
	default:
		return errors.New("平台来源无效")
	}
	return nil
}

func writeRecognitionResponse(writer http.ResponseWriter, response edgerecognition.Response) {
	result := response.Result
	payload := recognitionResponse{
		ProtocolVersion: edgerecognition.ProtocolVersion, Matched: response.Matched,
		RetryAfterMS: boundedRetry(response.RetryAfter).Milliseconds(),
		Result: recognitionResult{
			StableID: result.StableID, ISRC: result.ISRC, Title: result.Title, Artist: result.Artist, Album: result.Album,
			Source: result.Source, TrackID: result.TrackID, NativeIDVerified: result.NativeIDVerified,
			DurationMS: result.Duration.Milliseconds(), PlaybackOffsetMS: result.PlaybackOffset.Milliseconds(),
			Confidence: result.Confidence,
		},
	}
	if !result.SampledAt.IsZero() {
		payload.Result.SampledAtUnixMS = result.SampledAt.UnixMilli()
	}
	writer.Header().Set("X-CaseScreen-Protocol", "recognition-v1")
	writeJSON(writer, http.StatusOK, payload)
}

func boundedRetry(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	if value > maximumRetryAfter {
		return maximumRetryAfter
	}
	return value
}

func requireJSONEOF(decoder *json.Decoder) error {
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

func remoteClient(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil && host != "" {
		return host
	}
	if address == "" {
		return "unknown"
	}
	return address
}

func truncateText(value string, maximumRunes int) string {
	if utf8.RuneCountInString(value) <= maximumRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maximumRunes])
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
