package edgesync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"casescreen/internal/lyrics"
	"casescreen/internal/platformid"
)

const (
	// ProtocolVersion 仅在响应结构发生不兼容变化时递增。
	ProtocolVersion = 1
	// NowPlayingPath 是副屏唯一需要访问的只读接口。
	NowPlayingPath = "/v1/now-playing"
	// LyricsPath 只在切歌且屏端缓存未命中时访问。
	LyricsPath = "/v1/lyrics"

	defaultRequestsPerSecond = 4
	defaultBurst             = 8
	defaultMaxClients        = 256
	defaultClientTTL         = 10 * time.Minute
	defaultSourceTimeout     = 750 * time.Millisecond
	defaultLyricsTimeout     = 15 * time.Second
	responseWriteGrace       = 5 * time.Second
	maximumLyricsRelayBytes  = 4 << 20
)

// ErrPlatformLoginRequired 允许精确歌词源请求 UI 提示用户更新平台登录态。
var ErrPlatformLoginRequired = errors.New("平台歌词需要有效的登录/VIP 会话")

// Config 只包含独立同步通道的安全和资源限制，不会改变桌面控制 API。
type Config struct {
	Token             string
	RequestsPerSecond float64
	Burst             int
	MaxClients        int
	ClientTTL         time.Duration
	SourceTimeout     time.Duration
	LyricsTimeout     time.Duration
}

// Source 提供当前播放器元数据。实现必须在 ctx 取消后尽快返回。
type Source interface {
	Current(context.Context) (lyrics.NowPlaying, error)
}

// SourceFunc 便于调用方把 service.Status().Lyrics.Track 接入只读通道。
type SourceFunc func(context.Context) (lyrics.NowPlaying, error)

func (function SourceFunc) Current(ctx context.Context) (lyrics.NowPlaying, error) {
	return function(ctx)
}

// LyricMode 描述副屏能否使用时间轴滚动歌词，不包含任何歌词正文。
type LyricMode string

const (
	LyricModeUnknown LyricMode = "unknown"
	LyricModeTimed   LyricMode = "timed"
	LyricModePlain   LyricMode = "plain"
)

// Metadata 是新协议使用的轻量扩展元数据。Source 是播放器平台，TrackID 是
// 该平台内的歌曲标识；两者让副屏可以自行向同一平台请求歌词。
type Metadata struct {
	Source    string    `json:"source,omitempty"`
	TrackID   string    `json:"track_id,omitempty"`
	LyricMode LyricMode `json:"lyric_mode"`
	// 以下字段只描述屏端声纹识别证据。StableID/ISRC 不能冒充 QQ/网易云
	// 原生 TrackID；只有识歌中继显式验证的 Source+TrackID 才进入精确取词。
	StableID                   string  `json:"stable_id,omitempty"`
	ISRC                       string  `json:"isrc,omitempty"`
	Confidence                 float64 `json:"confidence,omitempty"`
	RecognitionSampledAtUnixMS int64   `json:"recognition_sampled_at_unix_ms,omitempty"`
}

// MetadataSource 在旧 Source 基础上提供平台歌曲 ID 和歌词滚动能力。
type MetadataSource interface {
	CurrentMetadata(context.Context) (lyrics.NowPlaying, Metadata, error)
}

// MetadataSourceFunc 便于调用方逐步接入扩展元数据，不影响旧 SourceFunc。
type MetadataSourceFunc func(context.Context) (lyrics.NowPlaying, Metadata, error)

func (function MetadataSourceFunc) CurrentMetadata(ctx context.Context) (lyrics.NowPlaying, Metadata, error) {
	return function(ctx)
}

// LyricsResult 是切歌时一次性返回并保存到 TF 的歌词，不属于高频元数据包。
type LyricsResult struct {
	Mode      LyricMode       `json:"mode"`
	Document  lyrics.Document `json:"document,omitempty"`
	PlainText string          `json:"plain_text,omitempty"`
}

// LyricsSource 必须使用来源平台自己的精确歌曲 ID，不得按歌名搜索或跨平台回退。
type LyricsSource interface {
	ExactLyrics(context.Context, Metadata) (LyricsResult, error)
}

type LyricsSourceFunc func(context.Context, Metadata) (LyricsResult, error)

func (function LyricsSourceFunc) ExactLyrics(ctx context.Context, metadata Metadata) (LyricsResult, error) {
	return function(ctx, metadata)
}

// Envelope 是副屏轮询时得到的完整协议响应。Track 保留旧版结构；新客户端
// 可读取 Metadata 和 AuthRequired，而旧客户端会安全忽略这些新增字段。
type Envelope struct {
	ProtocolVersion int               `json:"protocol_version"`
	SampledAtUnixMS int64             `json:"sampled_at_unix_ms"`
	Available       bool              `json:"available"`
	AuthRequired    bool              `json:"auth_required"`
	Metadata        Metadata          `json:"metadata"`
	Track           lyrics.NowPlaying `json:"track"`
}

// Snapshot 是 Envelope 的兼容名称，已有调用方无需修改。
type Snapshot = Envelope

type sourceResult struct {
	track    lyrics.NowPlaying
	metadata Metadata
	err      error
}

// Server 是独立的只读 HTTP 服务。它不注册或代理桌面端的任何控制路由。
type Server struct {
	source        MetadataSource
	tokenDigest   [sha256.Size]byte
	tokenLength   int
	sourceTimeout time.Duration
	sourceGate    chan struct{}
	lyricsTimeout time.Duration
	lyricsGate    chan struct{}
	lyricsSource  LyricsSource
	limiter       *rateLimiter
	handler       http.Handler
}

func New(config Config, source Source) (*Server, error) {
	if source == nil {
		return nil, errors.New("边缘同步元数据源不能为空")
	}
	if function, ok := source.(SourceFunc); ok && function == nil {
		return nil, errors.New("边缘同步元数据源不能为空")
	}
	return newServer(config, legacySourceAdapter{source: source})
}

// NewWithMetadata 创建支持平台歌曲 ID 和歌词模式的新协议服务。
func NewWithMetadata(config Config, source MetadataSource) (*Server, error) {
	if source == nil {
		return nil, errors.New("边缘同步扩展元数据源不能为空")
	}
	if function, ok := source.(MetadataSourceFunc); ok && function == nil {
		return nil, errors.New("边缘同步扩展元数据源不能为空")
	}
	return newServer(config, source)
}

// NewWithMetadataAndLyrics 增加切歌时的一次性精确歌词接口；当前歌曲接口仍保持小包。
func NewWithMetadataAndLyrics(config Config, source MetadataSource, lyricsSource LyricsSource) (*Server, error) {
	if lyricsSource == nil {
		return nil, errors.New("边缘同步精确歌词源不能为空")
	}
	if function, ok := lyricsSource.(LyricsSourceFunc); ok && function == nil {
		return nil, errors.New("边缘同步精确歌词源不能为空")
	}
	server, err := NewWithMetadata(config, source)
	if err != nil {
		return nil, err
	}
	server.lyricsSource = lyricsSource
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+NowPlayingPath, server.nowPlaying)
	mux.HandleFunc("GET "+LyricsPath, server.exactLyrics)
	server.handler = securityHeaders(server.limitRequests(server.authenticate(mux)))
	return server, nil
}

type legacySourceAdapter struct {
	source Source
}

func (adapter legacySourceAdapter) CurrentMetadata(ctx context.Context) (lyrics.NowPlaying, Metadata, error) {
	track, err := adapter.source.Current(ctx)
	return track, Metadata{}, err
}

func newServer(config Config, source MetadataSource) (*Server, error) {
	digest, tokenLength, err := tokenDigest(config.Token)
	if err != nil {
		return nil, err
	}
	if err := normalizeConfig(&config); err != nil {
		return nil, err
	}
	server := &Server{
		source: source, tokenDigest: digest, tokenLength: tokenLength,
		sourceTimeout: config.SourceTimeout, sourceGate: make(chan struct{}, 1),
		lyricsTimeout: config.LyricsTimeout, lyricsGate: make(chan struct{}, 2),
		limiter: newRateLimiter(config.RequestsPerSecond, config.Burst, config.MaxClients, config.ClientTTL),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+NowPlayingPath, server.nowPlaying)
	server.handler = securityHeaders(server.limitRequests(server.authenticate(mux)))
	return server, nil
}

func normalizeConfig(config *Config) error {
	if config.RequestsPerSecond == 0 {
		config.RequestsPerSecond = defaultRequestsPerSecond
	}
	if config.Burst == 0 {
		config.Burst = defaultBurst
	}
	if config.MaxClients == 0 {
		config.MaxClients = defaultMaxClients
	}
	if config.ClientTTL == 0 {
		config.ClientTTL = defaultClientTTL
	}
	if config.SourceTimeout == 0 {
		config.SourceTimeout = defaultSourceTimeout
	}
	if config.LyricsTimeout == 0 {
		config.LyricsTimeout = defaultLyricsTimeout
	}
	if config.RequestsPerSecond <= 0 || config.RequestsPerSecond > 100 {
		return errors.New("边缘同步每秒请求数必须大于 0 且不超过 100")
	}
	if config.Burst < 1 || config.Burst > 1000 {
		return errors.New("边缘同步突发请求数必须在 1..1000 之间")
	}
	if config.MaxClients < 1 || config.MaxClients > 4096 {
		return errors.New("边缘同步客户端上限必须在 1..4096 之间")
	}
	if config.ClientTTL < time.Second || config.ClientTTL > 24*time.Hour {
		return errors.New("边缘同步客户端记录时长必须在 1 秒..24 小时之间")
	}
	if config.SourceTimeout <= 0 || config.SourceTimeout > 10*time.Second {
		return errors.New("边缘同步元数据读取超时必须在 0..10 秒之间")
	}
	if config.LyricsTimeout <= 0 || config.LyricsTimeout > 30*time.Second {
		return errors.New("边缘同步歌词读取超时必须在 0..30 秒之间")
	}
	return nil
}

// Handler 供测试或嵌入已有进程使用；它仍然只包含独立同步路由。
func (server *Server) Handler() http.Handler {
	return server.handler
}

// Serve 在调用方提供的独立监听器上运行，并在上下文取消时有界关闭。
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil {
		return errors.New("边缘同步运行上下文不能为空")
	}
	if listener == nil {
		return errors.New("边缘同步监听器不能为空")
	}
	httpServer := &http.Server{
		Handler: server.handler, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: server.responseWriteTimeout(),
		IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	shutdownDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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

// responseWriteTimeout 必须覆盖歌词提供方的完整超时窗口；否则 handler 仍在等待
// 同平台歌词时，net/http 会先截断屏端连接并造成无意义的重复请求。
func (server *Server) responseWriteTimeout() time.Duration {
	return server.lyricsTimeout + responseWriteGrace
}

func (server *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		candidate, ok := bearerToken(request.Header.Get("Authorization"))
		if !ok || !secureTokenEqual(candidate, server.tokenDigest, server.tokenLength) {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="CaseScreen Edge Sync"`)
			writeError(writer, http.StatusUnauthorized, "未授权")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) limitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !server.limiter.allow(remoteClient(request.RemoteAddr), time.Now()) {
			writer.Header().Set("Retry-After", strconv.Itoa(server.limiter.retryAfterSeconds()))
			writeError(writer, http.StatusTooManyRequests, "请求过于频繁")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func remoteClient(remoteAddress string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err == nil && host != "" {
		return host
	}
	if remoteAddress == "" {
		return "unknown"
	}
	return remoteAddress
}

func (server *Server) nowPlaying(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	track, metadata, err := server.readMetadata(request.Context())
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		// 不把播放器、系统或路径错误透传给局域网客户端。
		writeError(writer, status, "播放器元数据暂不可用")
		return
	}
	track, metadata = sanitizeMetadata(track, metadata)
	snapshot := Snapshot{
		ProtocolVersion: ProtocolVersion,
		SampledAtUnixMS: time.Now().UnixMilli(),
		Available:       track.Valid() || metadata.TrackID != "",
		AuthRequired:    true,
		Metadata:        metadata,
		Track:           track,
	}
	if request.URL.Query().Get("compact") == "1" {
		// 精简模式只保留滚动时间轴所需的播放锚点。平台与歌曲 ID 已在
		// Metadata 中，不能再在 Track 中重复，也不发送每秒不变的文本。
		snapshot.Track = lyrics.NowPlaying{
			Position: track.Position, PositionKnown: track.PositionKnown, Playing: track.Playing,
		}
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (server *Server) exactLyrics(writer http.ResponseWriter, request *http.Request) {
	if server.lyricsSource == nil {
		writeError(writer, http.StatusNotFound, "精确歌词接口未启用")
		return
	}
	metadata, err := exactLyricsMetadata(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	result, err := server.readLyrics(request.Context(), metadata)
	if err != nil {
		if errors.Is(err, ErrPlatformLoginRequired) {
			writeJSON(writer, http.StatusFailedDependency, map[string]any{
				"error": "该平台歌词需要登录或有效会员权限", "needs_login": true,
			})
			return
		}
		// 平台原始错误、URL 和登录态细节不能返回给网络客户端。
		writeError(writer, http.StatusNotFound, "该歌曲歌词暂不可用")
		return
	}
	result, err = sanitizeLyricsResult(result)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	payload := map[string]any{
		"protocol_version": ProtocolVersion,
		"source":           metadata.Source,
		"track_id":         metadata.TrackID,
		"lyrics":           result,
	}
	data, err := json.Marshal(payload)
	if err != nil || len(data) > maximumLyricsRelayBytes {
		writeError(writer, http.StatusInternalServerError, "精确歌词响应过大")
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(append(data, '\n'))
}

func exactLyricsMetadata(request *http.Request) (Metadata, error) {
	return validateExactLyricsMetadata(request.URL.Query().Get("source"), request.URL.Query().Get("track_id"))
}

func validateExactLyricsMetadata(source, trackID string) (Metadata, error) {
	metadata := Metadata{Source: strings.ToLower(strings.TrimSpace(source)), TrackID: strings.TrimSpace(trackID)}
	switch metadata.Source {
	case "qq":
		if !platformid.QQ(metadata.TrackID) {
			return Metadata{}, errors.New("QQ 音乐歌曲 ID 无效")
		}
	case "netease":
		if !platformid.Netease(metadata.TrackID) {
			return Metadata{}, errors.New("网易云歌曲 ID 无效")
		}
	default:
		return Metadata{}, errors.New("精确歌词仅支持 QQ 音乐和网易云音乐")
	}
	return metadata, nil
}

func (server *Server) readLyrics(parent context.Context, metadata Metadata) (LyricsResult, error) {
	ctx, cancel := context.WithTimeout(parent, server.lyricsTimeout)
	defer cancel()
	select {
	case server.lyricsGate <- struct{}{}:
		defer func() { <-server.lyricsGate }()
	case <-ctx.Done():
		return LyricsResult{}, ctx.Err()
	}
	return server.lyricsSource.ExactLyrics(ctx, metadata)
}

func sanitizeLyricsResult(result LyricsResult) (LyricsResult, error) {
	result.Document.Metadata = nil
	result.Document.Title = truncateText(strings.TrimSpace(result.Document.Title), 512)
	result.Document.Artist = truncateText(strings.TrimSpace(result.Document.Artist), 512)
	result.Document.Album = truncateText(strings.TrimSpace(result.Document.Album), 512)
	if len(result.Document.Lines) > 10000 {
		return LyricsResult{}, errors.New("歌词时间轴过长")
	}
	for index := range result.Document.Lines {
		result.Document.Lines[index].Text = truncateText(strings.TrimSpace(result.Document.Lines[index].Text), 4096)
		if result.Document.Lines[index].At < 0 {
			result.Document.Lines[index].At = 0
		}
	}
	plain := strings.TrimSpace(result.PlainText)
	if plain == "" {
		plain = strings.TrimSpace(result.Document.PlainText)
	}
	result.Document.PlainText = ""
	if len(result.Document.Lines) > 0 {
		result.Mode = LyricModeTimed
		result.PlainText = ""
		return result, nil
	}
	if plain != "" {
		if len(plain) > maximumLyricsRelayBytes {
			return LyricsResult{}, errors.New("纯文本歌词过长")
		}
		result.Mode = LyricModePlain
		result.Document = lyrics.Document{}
		result.PlainText = plain
		return result, nil
	}
	return LyricsResult{}, errors.New("平台没有返回可显示的歌词")
}

func (server *Server) readMetadata(parent context.Context) (lyrics.NowPlaying, Metadata, error) {
	ctx, cancel := context.WithTimeout(parent, server.sourceTimeout)
	defer cancel()
	select {
	case server.sourceGate <- struct{}{}:
	case <-ctx.Done():
		return lyrics.NowPlaying{}, Metadata{}, ctx.Err()
	}
	resultChannel := make(chan sourceResult, 1)
	go func() {
		result := sourceResult{}
		defer func() {
			if recovered := recover(); recovered != nil {
				result.err = fmt.Errorf("元数据源异常: %v", recovered)
			}
			resultChannel <- result
			<-server.sourceGate
		}()
		result.track, result.metadata, result.err = server.source.CurrentMetadata(ctx)
	}()
	select {
	case result := <-resultChannel:
		return result.track, result.metadata, result.err
	case <-ctx.Done():
		return lyrics.NowPlaying{}, Metadata{}, ctx.Err()
	}
}

func sanitizeMetadata(track lyrics.NowPlaying, metadata Metadata) (lyrics.NowPlaying, Metadata) {
	track = sanitizeTrack(track)
	metadata.Source = truncateText(strings.ToLower(strings.TrimSpace(metadata.Source)), 64)
	metadata.TrackID = truncateText(strings.TrimSpace(metadata.TrackID), 128)
	if metadata.Source == "" {
		metadata.Source = track.Source
	} else {
		// 扩展源提供的平台标识更具体，同时更新旧 Track 字段保持两代客户端一致。
		track.Source = metadata.Source
	}
	if metadata.TrackID == "" {
		metadata.TrackID = track.TrackID
	} else {
		track.TrackID = metadata.TrackID
	}
	if metadata.TrackID != "" {
		if _, err := validateExactLyricsMetadata(metadata.Source, metadata.TrackID); err != nil {
			// 高频包宁可暂时缺少 ID，也不能把另一平台或畸形 ID 交给屏端解释。
			metadata.TrackID = ""
			track.TrackID = ""
		}
	}
	switch metadata.LyricMode {
	case LyricModeTimed, LyricModePlain, LyricModeUnknown:
	case "":
		metadata.LyricMode = LyricModeUnknown
	default:
		metadata.LyricMode = LyricModeUnknown
	}
	return track, metadata
}

func sanitizeTrack(track lyrics.NowPlaying) lyrics.NowPlaying {
	track.Source = truncateText(strings.TrimSpace(track.Source), 64)
	track.TrackID = truncateText(strings.TrimSpace(track.TrackID), 128)
	track.Title = truncateText(strings.TrimSpace(track.Title), 512)
	track.Artist = truncateText(strings.TrimSpace(track.Artist), 512)
	track.Album = truncateText(strings.TrimSpace(track.Album), 512)
	if track.Position < 0 {
		track.Position = 0
	}
	if track.Duration < 0 {
		track.Duration = 0
	}
	if track.Duration > 0 && track.Position > track.Duration {
		track.Position = track.Duration
	}
	return track
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
