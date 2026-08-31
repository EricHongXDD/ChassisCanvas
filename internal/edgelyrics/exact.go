package edgelyrics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"casescreen/internal/lyrics"
)

// LyricsType 区分可滚动 LRC 和没有时间戳的纯文本歌词。
type LyricsType string

const (
	LyricsSynchronized LyricsType = "synchronized_lrc"
	LyricsPlainText    LyricsType = "plain_text"
)

// Result 是平台精确 ID 接口返回并可持久化缓存的歌词结果。
type Result struct {
	Type      LyricsType      `json:"type"`
	Document  lyrics.Document `json:"document,omitempty"`
	PlainText string          `json:"plain_text,omitempty"`
}

// AuthSession 为已登录的平台会话提供请求级认证。
//
// 实现方应从系统凭据库或短期令牌存储中读取 Cookie/令牌；本接口不接收也不保存
// 明文密码，歌词缓存也不会序列化会话对象或请求头。
type AuthSession interface {
	Authorize(ctx context.Context, platform string, request *http.Request) error
}

// ExactProvider 必须使用 Metadata.TrackID 调用对应平台的精确歌词接口。
// 实现不得把 TrackID 当作关键词，也不得退化为仅按歌名搜索。
type ExactProvider interface {
	ID() string
	FetchExact(ctx context.Context, metadata Metadata, session AuthSession) (Result, error)
}

type exactResolver struct {
	providers []ExactProvider
	cacheDir  string
	session   AuthSession
}

type cacheEntry struct {
	Version  int    `json:"version"`
	Platform string `json:"platform"`
	TrackID  string `json:"track_id"`
	Result   Result `json:"result"`
}

func newExactResolver(cacheDir string, session AuthSession, providers ...ExactProvider) *exactResolver {
	return &exactResolver{
		providers: append([]ExactProvider(nil), providers...),
		cacheDir:  filepath.Clean(cacheDir),
		session:   session,
	}
}

func (resolver *exactResolver) Resolve(ctx context.Context, metadata Metadata) (Result, string, error) {
	platform := canonicalSource(metadata.Source)
	if result, ok := resolver.loadCache(platform, metadata.TrackID); ok {
		return result, "cache", nil
	}

	query := metadata
	query.Source = platform
	var failures []string
	foundProvider := false
	loginRequired := false
	for _, provider := range resolver.providers {
		if canonicalSource(provider.ID()) != platform {
			continue
		}
		foundProvider = true
		result, err := provider.FetchExact(ctx, query, resolver.session)
		if err != nil {
			loginRequired = loginRequired || errors.Is(err, ErrLoginRequired)
			failures = append(failures, provider.ID()+": "+err.Error())
			continue
		}
		result, err = normalizeResult(result)
		if err != nil {
			failures = append(failures, provider.ID()+": "+err.Error())
			continue
		}
		resolver.saveCache(platform, metadata.TrackID, result)
		return result, provider.ID(), nil
	}
	if !foundProvider {
		return Result{}, "", fmt.Errorf("%w: %s", ErrUnsupportedPlatform, platform)
	}
	if loginRequired {
		return Result{}, "", fmt.Errorf("%w: %s", ErrLoginRequired, strings.Join(failures, "; "))
	}
	return Result{}, "", fmt.Errorf("歌词精确查询失败: %s", strings.Join(failures, "; "))
}

func normalizeResult(result Result) (Result, error) {
	if len(result.Document.Lines) > 0 {
		result.Type = LyricsSynchronized
		result.PlainText = ""
		return result, nil
	}
	plain := normalizePlainText(result.PlainText)
	if plain != "" {
		result.Type = LyricsPlainText
		result.Document = lyrics.Document{}
		result.PlainText = plain
		return result, nil
	}
	return Result{}, errors.New("平台没有返回可显示的歌词")
}

func (resolver *exactResolver) cachePath(platform, trackID string) string {
	sum := sha256.Sum256([]byte("v1\nplatform=" + platform + "\ntrack_id=" + trackID))
	return filepath.Join(resolver.cacheDir, hex.EncodeToString(sum[:])+".json")
}

func (resolver *exactResolver) loadCache(platform, trackID string) (Result, bool) {
	if resolver.cacheDir == "" || resolver.cacheDir == "." {
		return Result{}, false
	}
	data, err := os.ReadFile(resolver.cachePath(platform, trackID))
	if err != nil {
		return Result{}, false
	}
	var entry cacheEntry
	if json.Unmarshal(data, &entry) != nil || entry.Version != 1 ||
		entry.Platform != platform || entry.TrackID != trackID {
		return Result{}, false
	}
	result, err := normalizeResult(entry.Result)
	return result, err == nil
}

func (resolver *exactResolver) saveCache(platform, trackID string, result Result) {
	if resolver.cacheDir == "" || resolver.cacheDir == "." {
		return
	}
	if err := os.MkdirAll(resolver.cacheDir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(cacheEntry{Version: 1, Platform: platform, TrackID: trackID, Result: result})
	if err != nil {
		return
	}
	temporary, err := os.CreateTemp(resolver.cacheDir, ".exact-lyrics-*.tmp")
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
	_ = os.Rename(name, resolver.cachePath(platform, trackID))
}

func firstParagraph(text string) string {
	lines := strings.Split(normalizePlainText(text), "\n")
	var paragraph []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if len(paragraph) > 0 {
				break
			}
			continue
		}
		paragraph = append(paragraph, line)
	}
	return strings.Join(paragraph, "\n")
}
