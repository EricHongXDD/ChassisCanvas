package lyrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const (
	defaultSogouLegacyBaseURL = "http://mp3.sogou.com"
	defaultLRCAPIBaseURL      = "https://api.lrc.cx"
	defaultLRCLIBBaseURL      = "https://lrclib.net"
	defaultSogouLegacyTimeout = 2 * time.Second
	defaultSogouFallbackTime  = 4 * time.Second
	defaultSogouTotalTimeout  = 10 * time.Second
)

// SogouProvider 把搜狗播放器识别结果转换为同步歌词。
// 搜狗音乐盒旧服务已经长期不可用，因此先短时探测旧接口，再依次使用两个公开 LRC 服务。
// 实际命中的来源会写入 Document.Metadata，并通过 ResultSource 对外如实报告。
type SogouProvider struct {
	Client          *http.Client
	BaseURL         string
	LRCAPIBaseURL   string
	LRCLIBBaseURL   string
	LegacyTimeout   time.Duration
	FallbackTimeout time.Duration
	TotalTimeout    time.Duration
}

func (provider SogouProvider) ID() string   { return "sogou" }
func (provider SogouProvider) Name() string { return "搜狗音乐" }

func (provider SogouProvider) Fetch(ctx context.Context, track NowPlaying) (Document, error) {
	keyword := strings.TrimSpace(strings.Join([]string{track.Title, track.Artist}, " "))
	if keyword == "" {
		return Document{}, errors.New("搜狗音乐查询关键词为空")
	}
	boundedContext, stop := context.WithTimeout(ctx, durationOrDefault(provider.TotalTimeout, defaultSogouTotalTimeout))
	defer stop()
	ctx = boundedContext
	legacyBase, lrcAPIBase, lrclibBase := provider.endpoints()
	var failures []string
	if legacyBase != "" {
		legacyContext, cancel := context.WithTimeout(ctx, durationOrDefault(provider.LegacyTimeout, defaultSogouLegacyTimeout))
		document, err := provider.fetchLegacy(legacyContext, legacyBase, keyword, track)
		cancel()
		if err == nil {
			return markSogouLyricsSource(document, "sogou"), nil
		}
		failures = append(failures, "旧搜狗接口: "+err.Error())
	}
	if lrcAPIBase != "" {
		fallbackContext, cancel := context.WithTimeout(ctx, durationOrDefault(provider.FallbackTimeout, defaultSogouFallbackTime))
		document, err := provider.fetchLRCAPI(fallbackContext, lrcAPIBase, track)
		cancel()
		if err == nil {
			return markSogouLyricsSource(document, "lrc.cx"), nil
		}
		failures = append(failures, "LrcAPI: "+err.Error())
	}
	if lrclibBase != "" {
		fallbackContext, cancel := context.WithTimeout(ctx, durationOrDefault(provider.FallbackTimeout, defaultSogouFallbackTime))
		document, err := provider.fetchLRCLIB(fallbackContext, lrclibBase, track)
		cancel()
		if err == nil {
			return markSogouLyricsSource(document, "lrclib"), nil
		}
		failures = append(failures, "LRCLIB: "+err.Error())
	}
	if len(failures) == 0 {
		return Document{}, errors.New("搜狗音乐没有配置可用的歌词端点")
	}
	return Document{}, fmt.Errorf("搜狗音乐歌词不可用: %s", strings.Join(failures, "; "))
}

// ResultSource 避免把公开兼容源误报成已经停止服务的搜狗官方歌词接口。
func (provider SogouProvider) ResultSource(document Document) string {
	if document.Metadata == nil {
		return provider.ID()
	}
	source := strings.TrimSpace(document.Metadata["casescreen_source"])
	if source == "" || source == provider.ID() {
		return provider.ID()
	}
	return provider.ID() + "/" + source
}

func (provider SogouProvider) endpoints() (string, string, string) {
	legacy := strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
	lrcAPI := strings.TrimRight(strings.TrimSpace(provider.LRCAPIBaseURL), "/")
	lrclib := strings.TrimRight(strings.TrimSpace(provider.LRCLIBBaseURL), "/")
	// 任一地址由测试或私有部署显式提供时，不再偷偷访问公网默认端点。
	if legacy != "" || lrcAPI != "" || lrclib != "" {
		return legacy, lrcAPI, lrclib
	}
	return defaultSogouLegacyBaseURL, defaultLRCAPIBaseURL, defaultLRCLIBBaseURL
}

func (provider SogouProvider) fetchLegacy(ctx context.Context, base, keyword string, track NowPlaying) (Document, error) {
	query := url.Values{
		"query":        {keyword},
		"jsoncallback": {"CaseScreen"},
	}
	data, err := httpGet(ctx, provider.Client, base+"/api/lrc?"+query.Encode(), base+"/")
	if err != nil {
		return Document{}, fmt.Errorf("歌词请求失败: %w", err)
	}
	lyric, err := parseSogouLyricsResponse(data)
	if err != nil {
		return Document{}, err
	}
	return parseSogouDocument(lyric, track)
}

func (provider SogouProvider) fetchLRCAPI(ctx context.Context, base string, track NowPlaying) (Document, error) {
	query := url.Values{"title": {strings.TrimSpace(track.Title)}, "artist": {strings.TrimSpace(track.Artist)}}
	if album := strings.TrimSpace(track.Album); album != "" {
		query.Set("album", album)
	}
	data, err := httpGet(ctx, provider.Client, base+"/lyrics?"+query.Encode(), base+"/")
	if err != nil {
		return Document{}, fmt.Errorf("歌词请求失败: %w", err)
	}
	decoded, err := decodeSogouResponse(data)
	if err != nil {
		return Document{}, fmt.Errorf("歌词响应编码无效: %w", err)
	}
	return parseSogouDocument(html.UnescapeString(string(decoded)), track)
}

type lrclibLyrics struct {
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

func (provider SogouProvider) fetchLRCLIB(ctx context.Context, base string, track NowPlaying) (Document, error) {
	query := url.Values{"track_name": {strings.TrimSpace(track.Title)}}
	if artist := strings.TrimSpace(track.Artist); artist != "" {
		query.Set("artist_name", artist)
	}
	data, err := httpGet(ctx, provider.Client, base+"/api/search?"+query.Encode(), base+"/")
	if err != nil {
		return Document{}, fmt.Errorf("歌词搜索失败: %w", err)
	}
	var candidates []lrclibLyrics
	if err := json.Unmarshal(data, &candidates); err != nil {
		return Document{}, fmt.Errorf("歌词搜索响应无法解析: %w", err)
	}
	bestIndex, bestScore := -1, -1
	for index, candidate := range candidates {
		if candidate.Instrumental || !looksLikeLRC(candidate.SyncedLyrics) {
			continue
		}
		score := matchScore(track, candidate.TrackName, candidate.ArtistName)
		if score < 0 {
			continue
		}
		if wantedAlbum, candidateAlbum := normalizeText(track.Album), normalizeText(candidate.AlbumName); wantedAlbum != "" && candidateAlbum != "" && wantedAlbum == candidateAlbum {
			score += 2
		}
		if track.Duration > 0 && candidate.Duration > 0 {
			difference := absDurationSeconds(float64(track.Duration)/float64(time.Second) - candidate.Duration)
			switch {
			case difference <= 2:
				score += 4
			case difference <= 8:
				score += 2
			case difference > 45:
				score -= 4
			}
		}
		if score > bestScore {
			bestIndex, bestScore = index, score
		}
	}
	minimumScore := 8
	if strings.TrimSpace(track.Artist) != "" {
		minimumScore = 11
	}
	if bestIndex < 0 || bestScore < minimumScore {
		return Document{}, errors.New("没有与当前歌曲匹配的同步歌词")
	}
	candidate := candidates[bestIndex]
	document, err := parseSogouDocument(candidate.SyncedLyrics, track)
	if err != nil {
		return Document{}, err
	}
	if document.Title == "" {
		document.Title = strings.TrimSpace(candidate.TrackName)
	}
	if document.Artist == "" {
		document.Artist = strings.TrimSpace(candidate.ArtistName)
	}
	if document.Album == "" {
		document.Album = strings.TrimSpace(candidate.AlbumName)
	}
	return document, nil
}

func parseSogouDocument(lyric string, track NowPlaying) (Document, error) {
	document, err := ParseLRC(strings.NewReader(strings.TrimSpace(lyric)))
	if err != nil {
		return Document{}, fmt.Errorf("搜狗音乐 LRC 解析失败: %w", err)
	}
	if len(document.Lines) == 0 {
		return Document{}, errors.New("搜狗音乐歌曲没有同步 LRC 歌词")
	}
	if document.Title != "" {
		minimumScore := 8
		if strings.TrimSpace(track.Artist) != "" && document.Artist != "" {
			minimumScore = 11
		}
		if matchScore(track, document.Title, document.Artist) < minimumScore {
			return Document{}, errors.New("搜狗音乐返回的歌词与当前歌曲不匹配")
		}
	}
	return document, nil
}

func markSogouLyricsSource(document Document, source string) Document {
	if document.Metadata == nil {
		document.Metadata = make(map[string]string)
	}
	document.Metadata["casescreen_source"] = source
	return document
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func absDurationSeconds(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func parseSogouLyricsResponse(data []byte) (string, error) {
	decoded, err := decodeSogouResponse(data)
	if err != nil {
		return "", fmt.Errorf("搜狗音乐歌词响应编码无效: %w", err)
	}
	decoded = bytes.TrimSpace(bytes.TrimPrefix(decoded, []byte("\xef\xbb\xbf")))
	if len(decoded) == 0 {
		return "", errors.New("搜狗音乐歌词响应为空")
	}

	// 部署过渡期的端点曾直接返回 LRC，因此保留无 JSON 包装的兼容路径。
	if looksLikeBareLRC(decoded) {
		return html.UnescapeString(string(decoded)), nil
	}
	var payload any
	if err := json.Unmarshal(unwrapJSONP(decoded), &payload); err != nil {
		return "", fmt.Errorf("搜狗音乐歌词响应无法解析: %w", err)
	}
	lyric := html.UnescapeString(strings.TrimSpace(findSogouLyric(payload, 0)))
	if lyric == "" {
		return "", errors.New("搜狗音乐歌曲没有 LRC 歌词")
	}
	if !looksLikeLRC(lyric) {
		return "", errors.New("搜狗音乐返回的内容不是同步 LRC 歌词")
	}
	return lyric, nil
}

func decodeSogouResponse(data []byte) ([]byte, error) {
	if utf8.Valid(data) {
		return data, nil
	}
	reader := transform.NewReader(bytes.NewReader(data), simplifiedchinese.GB18030.NewDecoder())
	decoded, err := io.ReadAll(io.LimitReader(reader, maximumLyricsResponse*3+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) > maximumLyricsResponse*3 {
		return nil, errors.New("解码后的歌词响应过大")
	}
	return decoded, nil
}

func findSogouLyric(value any, depth int) string {
	if depth > 12 {
		return ""
	}
	switch typed := value.(type) {
	case string:
		if looksLikeLRC(typed) {
			return typed
		}
	case map[string]any:
		for _, key := range []string{"lrc", "lyric", "lyrics", "content"} {
			for candidateKey, candidateValue := range typed {
				if !strings.EqualFold(candidateKey, key) {
					continue
				}
				if lyric := findSogouLyric(candidateValue, depth+1); lyric != "" {
					return lyric
				}
			}
		}
		for _, key := range []string{"data", "result", "song", "detail"} {
			for candidateKey, candidateValue := range typed {
				if strings.EqualFold(candidateKey, key) {
					if lyric := findSogouLyric(candidateValue, depth+1); lyric != "" {
						return lyric
					}
				}
			}
		}
	case []any:
		for _, candidate := range typed {
			if lyric := findSogouLyric(candidate, depth+1); lyric != "" {
				return lyric
			}
		}
	}
	return ""
}

func looksLikeLRC(value string) bool {
	return timestampPattern.MatchString(value)
}

func looksLikeBareLRC(value []byte) bool {
	if len(value) < 2 || value[0] != '[' || value[1] == '"' || value[1] == '{' {
		return false
	}
	return looksLikeLRC(string(value))
}
