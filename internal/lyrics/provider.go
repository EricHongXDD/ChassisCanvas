package lyrics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"casescreen/internal/platformid"
)

type NowPlaying struct {
	Source  string `json:"source,omitempty"`
	TrackID string `json:"track_id,omitempty"`
	// TrackIDOrigin 只在进程内部标记歌曲 ID 的证据来源，不进入任何同步包。
	// 由标题、歌手、专辑和时长匹配得到的 ID 必须标为 inferred；准确同步
	// 会拒绝这种 ID，只接受播放器/平台适配器直接提供的原生 ID。
	TrackIDOrigin TrackIDOrigin `json:"-"`
	// QQSongID 是 QQ 当前播放对象的非零数值 songId。Windows 端只从
	// CaseScreen 内置只读进程适配器取得；它只在主机内用于
	// 按精确 songId 换取 songmid，不进入状态 JSON 或副屏数据包。
	QQSongID      uint32 `json:"-"`
	Title         string `json:"title,omitempty"`
	Artist        string `json:"artist,omitempty"`
	Album         string `json:"album,omitempty"`
	Position      int64  `json:"position_ns,omitempty"`
	Duration      int64  `json:"duration_ns,omitempty"`
	PositionKnown bool   `json:"position_known"`
	Playing       bool   `json:"playing"`
	// PositionUpdatedAt 是播放器说明 Position 所对应的采样时刻，只在
	// 主机进程内用于把旧 SMTC 时间轴投影到本轮读取时刻。
	PositionUpdatedAt time.Time `json:"-"`
}

type TrackIDOrigin string

const (
	TrackIDOriginSource   TrackIDOrigin = "source"
	TrackIDOriginInferred TrackIDOrigin = "inferred"
)

// SourceTrackID 返回可用于准确同平台取词的歌曲 ID。只有播放器或经过
// 严格交叉验证的平台适配器显式标记为 source 的 ID 才可信；空标记和
// inferred 一律失败关闭。
func (track NowPlaying) SourceTrackID() string {
	if track.TrackIDOrigin != TrackIDOriginSource {
		return ""
	}
	return strings.TrimSpace(track.TrackID)
}

func (track NowPlaying) Valid() bool {
	return strings.TrimSpace(track.Title) != ""
}

func (track NowPlaying) key() string {
	// 只把本轮仍有来源证据的 ID 纳入身份。QQ songmid 还区分
	// 大小写，不能像标题一样归一化。
	return normalizeText(track.Source) + "\n" + track.SourceTrackID() + "\n" +
		normalizeText(track.Title) + "\n" + normalizeText(track.Artist)
}

type NowPlayingSource interface {
	Current(context.Context) (NowPlaying, error)
}

type Provider interface {
	ID() string
	Fetch(context.Context, NowPlaying) (Document, error)
}

// resultSourceProvider 允许组合提供方报告实际命中的后端，避免把兼容源误报成原服务。
type resultSourceProvider interface {
	ResultSource(Document) string
}

type Resolver struct {
	providers []Provider
	cacheDir  string
	mu        sync.RWMutex
	preferred string
}

func NewResolver(cacheDir string, providers ...Provider) *Resolver {
	return &Resolver{providers: append([]Provider(nil), providers...), cacheDir: filepath.Clean(cacheDir)}
}

// SetPreferredProvider 让桌面端可以固定歌词来源；传入 auto 或空字符串时恢复自动排序。
func (resolver *Resolver) SetPreferredProvider(providerID string) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.preferred = normalizeText(providerID)
}

// SetLocalPath 更新本地 LRC 提供方，不需要重启歌词引擎。
func (resolver *Resolver) SetLocalPath(path string) {
	for _, provider := range resolver.providers {
		if local, ok := provider.(*LocalFileProvider); ok {
			local.mu.Lock()
			local.Path = strings.TrimSpace(path)
			local.mu.Unlock()
		}
	}
}

func (resolver *Resolver) Resolve(ctx context.Context, track NowPlaying) (Document, string, error) {
	if !track.Valid() {
		return Document{}, "", errors.New("当前歌曲标题为空")
	}
	resolver.mu.RLock()
	preferred := resolver.preferred
	resolver.mu.RUnlock()
	if preferred == "local" {
		// 本地 LRC 是用户显式选择的文件，不属于歌曲缓存。每次都直接读取，
		// 这样切换路径后下一次解析会立即看到新文件，也不会误用旧歌词。
		return resolver.resolveLocal(ctx, track)
	}
	lockedProvider := sourceProviderID(track.Source)
	if lockedProvider == "" {
		return Document{}, "", errors.New("当前播放器来源无法验证为 QQ 音乐或网易云音乐，已停止按标题猜测")
	}
	trackID := track.SourceTrackID()
	if trackID == "" {
		if lockedProvider == "qq" {
			return Document{}, "", errors.New("QQ 当前歌曲缺少来源原生 ID（未通过内置适配器验证）；请确认使用受支持的 Windows QQ 音乐版本并开启系统媒体会话，已停止按标题猜测")
		}
		return Document{}, "", fmt.Errorf("%s 当前歌曲缺少来源原生 ID，已停止按标题猜测", lockedProvider)
	}
	if !validPlatformTrackID(lockedProvider, trackID) {
		return Document{}, "", fmt.Errorf("%s 当前歌曲的来源原生 ID 无效", lockedProvider)
	}
	// 已知播放器平台时，缓存与查询都锁定同一平台；用户首选项不能把
	// QQ songmid/网易云歌曲身份改交给另一家平台解释。
	cacheScope := lockedProvider
	if document, ok := resolver.loadCache(track, cacheScope); ok {
		return document, "cache", nil
	}
	providers := append([]Provider(nil), resolver.providers...)
	filtered := providers[:0]
	for _, provider := range providers {
		if normalizeText(provider.ID()) == lockedProvider {
			filtered = append(filtered, provider)
		}
	}
	providers = filtered
	sort.SliceStable(providers, func(left, right int) bool {
		if preferred != "" && preferred != "auto" {
			leftPreferred := normalizeText(providers[left].ID()) == preferred
			rightPreferred := normalizeText(providers[right].ID()) == preferred
			if leftPreferred != rightPreferred {
				return leftPreferred
			}
		}
		return providerPriority(providers[left].ID(), track.Source) < providerPriority(providers[right].ID(), track.Source)
	})
	var failures []string
	for _, provider := range providers {
		document, err := provider.Fetch(ctx, track)
		if err != nil {
			failures = append(failures, provider.ID()+": "+err.Error())
			continue
		}
		if !document.HasContent() {
			failures = append(failures, provider.ID()+": 没有歌词")
			continue
		}
		// 提供方响应必须带回与当前播放器完全一致的平台和原生 ID。
		// 身份不一致的歌词既不能显示也不能缓存，避免同名歌曲串词。
		if !cachedDocumentHasExactIdentity(document, lockedProvider, trackID) {
			failures = append(failures, provider.ID()+": 返回歌词身份与当前歌曲不一致")
			continue
		}
		resolver.saveCache(track, cacheScope, document)
		resolvedSource := provider.ID()
		if reporter, ok := provider.(resultSourceProvider); ok {
			if reported := strings.TrimSpace(reporter.ResultSource(document)); reported != "" {
				resolvedSource = reported
			}
		}
		return document, resolvedSource, nil
	}
	if len(failures) == 0 {
		return Document{}, "", errors.New("没有配置歌词提供方")
	}
	return Document{}, "", fmt.Errorf("歌词解析失败: %s", strings.Join(failures, "; "))
}

func (resolver *Resolver) resolveLocal(ctx context.Context, track NowPlaying) (Document, string, error) {
	var failures []string
	for _, provider := range resolver.providers {
		if normalizeText(provider.ID()) != "local" {
			continue
		}
		document, err := provider.Fetch(ctx, track)
		if err != nil {
			failures = append(failures, provider.ID()+": "+err.Error())
			continue
		}
		if !document.HasContent() {
			failures = append(failures, provider.ID()+": 没有歌词")
			continue
		}
		return document, provider.ID(), nil
	}
	if len(failures) == 0 {
		return Document{}, "", errors.New("没有配置本地 LRC 提供方")
	}
	return Document{}, "", fmt.Errorf("歌词解析失败: %s", strings.Join(failures, "; "))
}

func cachedDocumentHasExactIdentity(document Document, platform, sourceTrackID string) bool {
	if document.Metadata == nil {
		return false
	}
	cachedPlatform := sourceProviderID(document.Metadata["platform"])
	if cachedPlatform == "" {
		cachedPlatform = normalizeText(document.Metadata["platform"])
	}
	trackID := strings.TrimSpace(document.Metadata["track_id"])
	sourceTrackID = strings.TrimSpace(sourceTrackID)
	return cachedPlatform == normalizeText(platform) && trackID == sourceTrackID &&
		validPlatformTrackID(cachedPlatform, trackID)
}

func providerPriority(providerID, source string) int {
	providerID = normalizeText(providerID)
	source = normalizeText(source)
	if source != "" && (strings.Contains(source, providerID) || strings.Contains(providerID, source)) {
		return 0
	}
	return 1
}

func (resolver *Resolver) cachePath(track NowPlaying, preferred string) string {
	// 播放器来源和用户指定的歌词源都属于解析语义。若只按歌名/歌手缓存，
	// 同一首歌从网易切换到 QQ 时会继续命中旧歌词，来源按钮看似生效却不发请求。
	key := track.key() + "\nsource=" + normalizeText(track.Source) + "\npreferred=" + normalizeText(preferred)
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(resolver.cacheDir, hex.EncodeToString(sum[:])+".json")
}

func (resolver *Resolver) loadCache(track NowPlaying, preferred string) (Document, bool) {
	if resolver.cacheDir == "" || resolver.cacheDir == "." {
		return Document{}, false
	}
	platform := sourceProviderID(track.Source)
	trackID := track.SourceTrackID()
	if platform == "" || normalizeText(preferred) != platform ||
		!validPlatformTrackID(platform, trackID) {
		return Document{}, false
	}
	data, err := os.ReadFile(resolver.cachePath(track, preferred))
	if err != nil {
		return Document{}, false
	}
	var document Document
	if json.Unmarshal(data, &document) != nil || !document.HasContent() ||
		!cachedDocumentHasExactIdentity(document, platform, trackID) {
		return Document{}, false
	}
	return document, true
}

// sourceProviderID 只把明确识别出的 QQ 音乐和网易云音乐来源映射到平台。
// 未知播放器即使携带某个 ID，也不能猜测该 ID 属于哪一家平台。
func sourceProviderID(source string) string {
	source = normalizeText(source)
	switch {
	case strings.Contains(source, "qq") || strings.Contains(source, "腾讯"):
		return "qq"
	case strings.Contains(source, "netease") || strings.Contains(source, "cloudmusic") || strings.Contains(source, "网易") || source == "163":
		return "netease"
	default:
		return ""
	}
}

func validPlatformTrackID(platform, trackID string) bool {
	switch normalizeText(platform) {
	case "qq":
		return platformid.QQ(strings.TrimSpace(trackID))
	case "netease":
		_, ok := parseNeteaseSongID(trackID)
		return ok
	default:
		return false
	}
}

func (resolver *Resolver) saveCache(track NowPlaying, preferred string, document Document) {
	if resolver.cacheDir == "" || resolver.cacheDir == "." {
		return
	}
	platform := sourceProviderID(track.Source)
	trackID := track.SourceTrackID()
	if platform == "" || normalizeText(preferred) != platform ||
		!validPlatformTrackID(platform, trackID) ||
		!cachedDocumentHasExactIdentity(document, platform, trackID) {
		return
	}
	if err := os.MkdirAll(resolver.cacheDir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(document)
	if err != nil {
		return
	}
	temporary, err := os.CreateTemp(resolver.cacheDir, ".lyrics-*.tmp")
	if err != nil {
		return
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err = temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return
	}
	if temporary.Close() != nil {
		return
	}
	_ = os.Rename(name, resolver.cachePath(track, preferred))
}

func normalizeText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}

func matchScore(track NowPlaying, title, artist string) int {
	wantedTitle := normalizeText(track.Title)
	candidateTitle := normalizeText(title)
	if wantedTitle == "" || candidateTitle == "" {
		return -1
	}
	score := 0
	switch {
	case wantedTitle == candidateTitle:
		score += 8
	case strings.Contains(wantedTitle, candidateTitle) || strings.Contains(candidateTitle, wantedTitle):
		score += 4
	default:
		return -1
	}
	wantedArtist := normalizeText(track.Artist)
	candidateArtist := normalizeText(artist)
	if wantedArtist != "" && candidateArtist != "" {
		switch {
		case wantedArtist == candidateArtist:
			score += 5
		case strings.Contains(wantedArtist, candidateArtist) || strings.Contains(candidateArtist, wantedArtist):
			score += 3
		}
	}
	return score
}
