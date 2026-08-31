// Package edgelyrics 实现可在副屏 Linux 系统中独立运行的歌词时间轴核心。
//
// 本包只消费播放器元数据，不访问麦克风、不录音，也不依赖 Windows 媒体接口。
package edgelyrics

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"casescreen/internal/lyrics"
)

const (
	defaultRequestTimeout = 12 * time.Second

	// PlainLyricsNotice 是无时间戳歌词必须显示给用户的明确提示。
	PlainLyricsNotice = "该首歌的歌词暂不支持滚动"
)

var (
	ErrMissingTrackID      = errors.New("播放器元数据缺少平台歌曲 ID")
	ErrUnsupportedPlatform = errors.New("播放器平台暂不支持精确歌词查询")
	ErrLoginRequired       = errors.New("平台歌词需要有效的登录/VIP 会话")
)

// Metadata 是播放器传给副屏的最小歌曲状态。
// TrackID 必须是 Source 平台自己的歌曲 ID，不能用歌名或其他平台 ID 代替。
type Metadata struct {
	Source        string    `json:"source,omitempty"`
	TrackID       string    `json:"track_id"`
	Title         string    `json:"title,omitempty"`
	Artist        string    `json:"artist,omitempty"`
	Album         string    `json:"album,omitempty"`
	Position      int64     `json:"position_ns,omitempty"`
	Duration      int64     `json:"duration_ns,omitempty"`
	PositionKnown bool      `json:"position_known"`
	Playing       bool      `json:"playing"`
	CapturedAt    time.Time `json:"captured_at"`
}

// Valid 表示元数据包含平台和该平台的精确歌曲 ID。
func (metadata Metadata) Valid() bool {
	return canonicalSource(metadata.Source) != "" && strings.TrimSpace(metadata.TrackID) != ""
}

// State 是屏端渲染循环可直接读取的歌词状态。
type State struct {
	Track        Metadata    `json:"track"`
	Position     int64       `json:"position_ns"`
	LyricsType   LyricsType  `json:"lyrics_type,omitempty"`
	Current      lyrics.Line `json:"current"`
	Next         lyrics.Line `json:"next"`
	Progress     float64     `json:"progress"`
	HasCurrent   bool        `json:"has_current"`
	HasLyrics    bool        `json:"has_lyrics"`
	PlainText    string      `json:"plain_text,omitempty"`
	PlainPreview string      `json:"plain_preview,omitempty"`
	Notice       string      `json:"notice,omitempty"`
	Provider     string      `json:"provider,omitempty"`
	LastError    string      `json:"last_error,omitempty"`
	LastUpdated  time.Time   `json:"last_updated"`
}

// Core 在平台歌曲 ID 变化时精确查询一次歌词，播放过程中只查询本地时间轴。
type Core struct {
	resolver *exactResolver
	now      func() time.Time

	// updateMu 保证同一平台歌曲 ID 不会因并发元数据包而重复发起歌词请求。
	updateMu sync.Mutex
	mu       sync.RWMutex

	identity       string
	track          Metadata
	result         Result
	provider       string
	lastError      string
	lastUpdated    time.Time
	anchorPosition time.Duration
	anchorTime     time.Time
}

// New 创建默认使用 QQ 音乐和网易云音乐精确 ID 接口的屏端歌词核心。
func New(cacheDir string, client *http.Client) *Core {
	return NewWithAuth(cacheDir, client, nil)
}

// NewWithAuth 创建可注入平台登录/VIP会话的歌词核心。
// AuthSession 只在构造 HTTP 请求时使用，不会写入歌词缓存。
func NewWithAuth(cacheDir string, client *http.Client, session AuthSession) *Core {
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	return NewWithExactProviders(cacheDir, session,
		QQExactProvider{Client: client},
		NeteaseExactProvider{Client: client},
	)
}

// NewWithExactProviders 允许固件测试或替换平台精确查询实现。
func NewWithExactProviders(cacheDir string, session AuthSession, providers ...ExactProvider) *Core {
	return newWithClock(cacheDir, session, time.Now, providers...)
}

func newWithClock(cacheDir string, session AuthSession, now func() time.Time, providers ...ExactProvider) *Core {
	if now == nil {
		now = time.Now
	}
	return &Core{
		resolver: newExactResolver(cacheDir, session, providers...),
		now:      now,
	}
}

// Update 接收最新播放器元数据。
//
// 同一平台歌曲 ID 只更新播放锚点，不访问网络。仅 source 或 track_id 变化时才会
// 精确查询歌词；查询失败后，同一歌曲的后续进度包也不会自动重试。
func (core *Core) Update(ctx context.Context, metadata Metadata) error {
	if core == nil || core.resolver == nil {
		return errors.New("歌词核心尚未初始化")
	}
	core.updateMu.Lock()
	defer core.updateMu.Unlock()

	now := core.now()
	metadata = sanitizeMetadata(metadata)
	if metadataEmpty(metadata) {
		core.clear(metadata, now)
		return nil
	}

	identity := trackIdentity(metadata)
	core.mu.Lock()
	if identity == core.identity {
		position := core.positionLocked(now)
		if metadata.PositionKnown {
			position = positionAt(metadata, now)
		}
		core.track = metadata
		core.anchorPosition = position
		core.anchorTime = now
		core.lastUpdated = now
		core.mu.Unlock()
		return nil
	}

	position := time.Duration(0)
	if metadata.PositionKnown {
		position = positionAt(metadata, now)
	}
	// 切歌后立即丢弃旧歌词，避免联网期间把上一首歌继续显示在屏幕上。
	core.identity = identity
	core.track = metadata
	core.result = Result{}
	core.provider = ""
	core.lastError = ""
	core.lastUpdated = now
	core.anchorPosition = position
	core.anchorTime = now
	core.mu.Unlock()

	if strings.TrimSpace(metadata.TrackID) == "" {
		core.setError(ErrMissingTrackID)
		return ErrMissingTrackID
	}
	platform := canonicalSource(metadata.Source)
	if platform != "qq" && platform != "netease" {
		core.setError(ErrUnsupportedPlatform)
		return ErrUnsupportedPlatform
	}

	result, provider, err := core.resolver.Resolve(ctx, metadata)
	core.mu.Lock()
	defer core.mu.Unlock()
	if err != nil {
		core.lastError = err.Error()
		return err
	}
	core.result = result
	core.provider = provider
	return nil
}

// Snapshot 返回当前时刻应显示的当前句、下一句或纯文本歌词。
func (core *Core) Snapshot() State {
	if core == nil {
		return State{}
	}
	return core.SnapshotAt(core.now())
}

// SnapshotAt 允许渲染循环传入同一帧时间，也便于固件离线测试。
func (core *Core) SnapshotAt(now time.Time) State {
	if core == nil {
		return State{}
	}
	core.mu.RLock()
	track := core.track
	result := core.result
	provider := core.provider
	lastError := core.lastError
	lastUpdated := core.lastUpdated
	position := core.positionLocked(now)
	core.mu.RUnlock()

	state := State{
		Track:       track,
		Position:    int64(position),
		LyricsType:  result.Type,
		Provider:    provider,
		LastError:   lastError,
		LastUpdated: lastUpdated,
	}
	switch result.Type {
	case LyricsSynchronized:
		state.HasLyrics = len(result.Document.Lines) > 0
		current, next, progress, ok := result.Document.Current(position)
		if ok {
			state.Current = current
			state.Next = next
			state.Progress = progress
			state.HasCurrent = true
		} else if len(result.Document.Lines) > 0 {
			// 第一条时间戳之前没有“当前句”，但提前提供下一句便于 UI 预告。
			state.Next = result.Document.Lines[0]
		}
	case LyricsPlainText:
		state.HasLyrics = strings.TrimSpace(result.PlainText) != ""
		state.PlainText = result.PlainText
		state.PlainPreview = firstParagraph(result.PlainText)
		state.Notice = PlainLyricsNotice
	}
	return state
}

func (core *Core) clear(metadata Metadata, now time.Time) {
	core.mu.Lock()
	defer core.mu.Unlock()
	core.identity = ""
	core.track = metadata
	core.result = Result{}
	core.provider = ""
	core.lastError = ""
	core.lastUpdated = now
	core.anchorPosition = 0
	core.anchorTime = time.Time{}
}

func (core *Core) setError(err error) {
	core.mu.Lock()
	defer core.mu.Unlock()
	core.lastError = err.Error()
}

// positionLocked 要求调用者已经持有 core.mu 的读锁或写锁。
func (core *Core) positionLocked(now time.Time) time.Duration {
	position := core.anchorPosition
	if core.track.Playing && !core.anchorTime.IsZero() && now.After(core.anchorTime) {
		position += now.Sub(core.anchorTime)
	}
	return clampPosition(position, time.Duration(core.track.Duration))
}

func sanitizeMetadata(metadata Metadata) Metadata {
	metadata.Source = strings.TrimSpace(metadata.Source)
	metadata.TrackID = strings.TrimSpace(metadata.TrackID)
	metadata.Title = strings.TrimSpace(metadata.Title)
	metadata.Artist = strings.TrimSpace(metadata.Artist)
	metadata.Album = strings.TrimSpace(metadata.Album)
	if metadata.Position < 0 {
		metadata.Position = 0
	}
	if metadata.Duration < 0 {
		metadata.Duration = 0
	}
	return metadata
}

func metadataEmpty(metadata Metadata) bool {
	return metadata.Source == "" && metadata.TrackID == "" && metadata.Title == "" && metadata.Artist == ""
}

func positionAt(metadata Metadata, receivedAt time.Time) time.Duration {
	if !metadata.PositionKnown {
		return 0
	}
	position := time.Duration(metadata.Position)
	if metadata.Playing && !metadata.CapturedAt.IsZero() && receivedAt.After(metadata.CapturedAt) {
		position += receivedAt.Sub(metadata.CapturedAt)
	}
	return clampPosition(position, time.Duration(metadata.Duration))
}

func clampPosition(position, duration time.Duration) time.Duration {
	if position < 0 {
		return 0
	}
	if duration > 0 && position > duration {
		return duration
	}
	return position
}

func trackIdentity(metadata Metadata) string {
	platform := canonicalSource(metadata.Source)
	if metadata.TrackID != "" {
		return platform + "\nid=" + metadata.TrackID
	}
	// 缺少 ID 时仍区分歌曲，避免下一首缺失 ID 的歌曲继承上一首的错误状态。
	return platform + "\nmissing-id\n" + identityText(metadata.Title) + "\n" + identityText(metadata.Artist)
}

func identityText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}

func canonicalSource(source string) string {
	normalized := identityText(source)
	switch {
	case strings.Contains(normalized, "qq") || strings.Contains(normalized, "腾讯"):
		return "qq"
	case strings.Contains(normalized, "netease"), strings.Contains(normalized, "cloudmusic"),
		normalized == "163", strings.Contains(normalized, "music163com"), strings.Contains(normalized, "网易"):
		return "netease"
	default:
		return normalized
	}
}
