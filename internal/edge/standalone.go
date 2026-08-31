package edge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"casescreen/internal/edgelyrics"
	"casescreen/internal/edgesync"
	"casescreen/internal/lyrics"
	"casescreen/internal/platformid"
)

const defaultStandaloneIdentityRetry = 30 * time.Second

// PlatformAuthSession 同时供同平台身份确认与精确歌词请求注入登录态。
// 实现只能向平台 HTTPS 白名单主机发送会话，不能记录或回显凭据。
type PlatformAuthSession interface {
	Authorize(context.Context, string, *http.Request) error
}

// StandaloneOptions 配置不依赖电脑 EdgeSync 的实验性屏端元数据与歌词链路。
// Source 通常是 BlueZ AVRCP；Platform 可为 auto、qq 或 netease。AVRCP 本身
// 不携带平台原生歌曲 ID，因此该模式不能替代来源 companion 的精确 ID 路径。
type StandaloneOptions struct {
	Source           lyrics.NowPlayingSource
	Platform         string
	IdentityCacheDir string
	LyricsCacheDir   string
	HTTPClient       *http.Client
	Session          PlatformAuthSession
	IdentityRetry    time.Duration
}

// NewStandalonePipeline 创建“屏端元数据 -> 严格同平台推断 -> TF 歌词缓存”的
// 实验链路。它不读取麦克风，也不需要 Windows EdgeSync，但不能保证推断出的 ID
// 就是播放来源内部的原生歌曲 ID。
func NewStandalonePipeline(options StandaloneOptions) (MetadataClient, CoreFactory, error) {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	identityProviders := []lyrics.Provider{
		lyrics.QQProvider{Client: client, Session: options.Session},
		lyrics.NeteaseProvider{Client: client, Session: options.Session},
	}
	exactProviders := []edgelyrics.ExactProvider{
		edgelyrics.QQExactProvider{Client: client},
		edgelyrics.NeteaseExactProvider{Client: client},
	}
	return newStandalonePipeline(options, identityProviders, exactProviders)
}

func newStandalonePipeline(options StandaloneOptions, identityProviders []lyrics.Provider, exactProviders []edgelyrics.ExactProvider) (MetadataClient, CoreFactory, error) {
	if options.Source == nil {
		return nil, nil, errors.New("屏端独立元数据源不能为空")
	}
	platform := strings.ToLower(strings.TrimSpace(options.Platform))
	if platform == "" {
		platform = "auto"
	}
	if platform != "auto" && platform != "qq" && platform != "netease" {
		return nil, nil, errors.New("屏端独立平台必须是 auto、qq 或 netease")
	}
	retry := options.IdentityRetry
	if retry == 0 {
		retry = defaultStandaloneIdentityRetry
	}
	if retry < time.Second || retry > 24*time.Hour {
		return nil, nil, errors.New("屏端歌曲身份重试间隔必须在 1 秒..24 小时之间")
	}
	if strings.TrimSpace(options.IdentityCacheDir) == "" || strings.TrimSpace(options.LyricsCacheDir) == "" {
		return nil, nil, errors.New("屏端身份缓存和歌词缓存目录不能为空")
	}

	prefetched := &standalonePrefetch{}
	metadata := &standaloneMetadataClient{
		source:     options.Source,
		resolver:   lyrics.NewResolver(options.IdentityCacheDir, identityProviders...),
		prefetched: prefetched,
		platform:   platform,
		retryAfter: retry,
		now:        time.Now,
	}
	factory := func() LyricsEngine {
		providers := make([]edgelyrics.ExactProvider, 0, len(exactProviders))
		for _, provider := range exactProviders {
			providers = append(providers, standaloneExactProvider{
				source: standalonePlatform(provider.ID()), prefetched: prefetched, fallback: provider,
			})
		}
		return edgelyrics.NewWithExactProviders(options.LyricsCacheDir, options.Session, providers...)
	}
	return metadata, factory, nil
}

type standaloneMetadataClient struct {
	source     lyrics.NowPlayingSource
	resolver   *lyrics.Resolver
	prefetched *standalonePrefetch
	platform   string
	retryAfter time.Duration
	now        func() time.Time

	mu              sync.Mutex
	lastSignature   string
	lastTrackID     string
	lastMode        edgesync.LyricMode
	lastFailureTime time.Time
}

func (client *standaloneMetadataClient) Current(ctx context.Context) (edgesync.Envelope, error) {
	track, err := client.source.Current(ctx)
	if err != nil {
		return edgesync.Envelope{}, err
	}
	now := client.now()
	track = sanitizeStandaloneTrack(track)
	if !track.Valid() {
		client.clearIdentity()
		return standaloneEnvelope(track, "", edgesync.LyricModeUnknown, now), nil
	}

	platform := standalonePlatform(track.Source)
	if client.platform != "auto" {
		platform = client.platform
	}
	if platform != "" {
		track.Source = platform
	}
	if validStandaloneTrackID(platform, track.TrackID) {
		return standaloneEnvelope(track, track.TrackID, edgesync.LyricModeUnknown, now), nil
	}
	track.TrackID = ""
	signature := standaloneSignature(track, platform)

	client.mu.Lock()
	if signature == client.lastSignature && client.lastTrackID != "" {
		trackID, mode := client.lastTrackID, client.lastMode
		client.mu.Unlock()
		return standaloneEnvelope(track, trackID, mode, now), nil
	}
	if signature == client.lastSignature && !client.lastFailureTime.IsZero() &&
		now.Sub(client.lastFailureTime) < client.retryAfter {
		client.mu.Unlock()
		return standaloneEnvelope(track, "", edgesync.LyricModeUnknown, now), nil
	}
	if signature != client.lastSignature {
		client.lastSignature = signature
		client.lastTrackID = ""
		client.lastMode = edgesync.LyricModeUnknown
		client.lastFailureTime = time.Time{}
	}
	client.mu.Unlock()

	// AVRCP 不定义平台歌曲 ID。为了避免同名现场版/录音室版误配，只有
	// 标题、歌手、专辑和时长四项证据齐全时才允许同平台补全 ID。
	if platform == "" || strings.TrimSpace(track.Artist) == "" ||
		strings.TrimSpace(track.Album) == "" || track.Duration <= 0 {
		client.recordIdentityFailure(signature, now)
		return standaloneEnvelope(track, "", edgesync.LyricModeUnknown, now), nil
	}
	document, _, resolveErr := client.resolver.Resolve(ctx, track)
	if resolveErr != nil {
		client.recordIdentityFailure(signature, now)
		return standaloneEnvelope(track, "", edgesync.LyricModeUnknown, now), nil
	}
	resolvedPlatform := standalonePlatform(document.Metadata["platform"])
	trackID := strings.TrimSpace(document.Metadata["track_id"])
	if resolvedPlatform != platform || !validStandaloneTrackID(platform, trackID) {
		client.recordIdentityFailure(signature, now)
		return standaloneEnvelope(track, "", edgesync.LyricModeUnknown, now), nil
	}
	mode, result := standaloneResult(document)
	client.prefetched.put(platform, trackID, result)
	client.mu.Lock()
	client.lastSignature = signature
	client.lastTrackID = trackID
	client.lastMode = mode
	client.lastFailureTime = time.Time{}
	client.mu.Unlock()
	return standaloneEnvelope(track, trackID, mode, now), nil
}

func (client *standaloneMetadataClient) clearIdentity() {
	client.mu.Lock()
	client.lastSignature = ""
	client.lastTrackID = ""
	client.lastMode = edgesync.LyricModeUnknown
	client.lastFailureTime = time.Time{}
	client.mu.Unlock()
}

func (client *standaloneMetadataClient) recordIdentityFailure(signature string, now time.Time) {
	client.mu.Lock()
	client.lastSignature = signature
	client.lastTrackID = ""
	client.lastMode = edgesync.LyricModeUnknown
	client.lastFailureTime = now
	client.mu.Unlock()
}

func standaloneEnvelope(track lyrics.NowPlaying, trackID string, mode edgesync.LyricMode, now time.Time) edgesync.Envelope {
	track.TrackID = strings.TrimSpace(trackID)
	return edgesync.Envelope{
		ProtocolVersion: edgesync.ProtocolVersion,
		SampledAtUnixMS: now.UnixMilli(),
		Available:       track.Valid(),
		Metadata: edgesync.Metadata{
			Source: standalonePlatform(track.Source), TrackID: track.TrackID, LyricMode: mode,
		},
		Track: track,
	}
}

func sanitizeStandaloneTrack(track lyrics.NowPlaying) lyrics.NowPlaying {
	track.Source = strings.TrimSpace(track.Source)
	track.TrackID = strings.TrimSpace(track.TrackID)
	track.Title = strings.TrimSpace(track.Title)
	track.Artist = strings.TrimSpace(track.Artist)
	track.Album = strings.TrimSpace(track.Album)
	if track.Position < 0 {
		track.Position = 0
	}
	if track.Duration < 0 {
		track.Duration = 0
	}
	return track
}

func standaloneSignature(track lyrics.NowPlaying, platform string) string {
	return platform + "\n" + standaloneIdentityText(track.Title) + "\n" +
		standaloneIdentityText(track.Artist) + "\n" + standaloneIdentityText(track.Album) + "\n" +
		time.Duration(track.Duration).String()
}

func standaloneIdentityText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}

func standalonePlatform(value string) string {
	normalized := standaloneIdentityText(value)
	switch {
	case strings.Contains(normalized, "qq"), strings.Contains(normalized, "腾讯"):
		return "qq"
	case strings.Contains(normalized, "netease"), strings.Contains(normalized, "cloudmusic"),
		strings.Contains(normalized, "网易"), normalized == "163":
		return "netease"
	default:
		return ""
	}
}

func validStandaloneTrackID(platform, trackID string) bool {
	switch platform {
	case "qq":
		return platformid.QQ(strings.TrimSpace(trackID))
	case "netease":
		return platformid.Netease(strings.TrimSpace(trackID))
	default:
		return false
	}
}

func standaloneResult(document lyrics.Document) (edgesync.LyricMode, edgelyrics.Result) {
	if document.Scrollable() {
		return edgesync.LyricModeTimed, edgelyrics.Result{
			Type: edgelyrics.LyricsSynchronized, Document: document,
		}
	}
	if strings.TrimSpace(document.PlainText) != "" {
		return edgesync.LyricModePlain, edgelyrics.Result{
			Type: edgelyrics.LyricsPlainText, PlainText: document.PlainText,
		}
	}
	return edgesync.LyricModeUnknown, edgelyrics.Result{}
}

type standalonePrefetch struct {
	mu       sync.RWMutex
	platform string
	trackID  string
	result   edgelyrics.Result
}

func (prefetch *standalonePrefetch) put(platform, trackID string, result edgelyrics.Result) {
	prefetch.mu.Lock()
	prefetch.platform = platform
	prefetch.trackID = trackID
	prefetch.result = result
	prefetch.mu.Unlock()
}

func (prefetch *standalonePrefetch) get(platform, trackID string) (edgelyrics.Result, bool) {
	prefetch.mu.RLock()
	defer prefetch.mu.RUnlock()
	if prefetch.platform != platform || prefetch.trackID != trackID {
		return edgelyrics.Result{}, false
	}
	return prefetch.result, true
}

type standaloneExactProvider struct {
	source     string
	prefetched *standalonePrefetch
	fallback   edgelyrics.ExactProvider
}

func (provider standaloneExactProvider) ID() string { return provider.source }

func (provider standaloneExactProvider) FetchExact(ctx context.Context, metadata edgelyrics.Metadata, session edgelyrics.AuthSession) (edgelyrics.Result, error) {
	if provider.prefetched != nil {
		if result, ok := provider.prefetched.get(provider.source, strings.TrimSpace(metadata.TrackID)); ok {
			return result, nil
		}
	}
	if provider.fallback == nil {
		return edgelyrics.Result{}, errors.New("屏端同平台精确歌词提供方未配置")
	}
	return provider.fallback.FetchExact(ctx, metadata, session)
}
