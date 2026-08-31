package edgelyrics

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"casescreen/internal/lyrics"
	"casescreen/internal/platformid"
	"casescreen/internal/platformstatus"
)

const maximumLyricsResponse = 4 << 20

type loginStateReporter interface {
	MarkNeedsLogin(string) error
}

var (
	lrcMetadataLine = regexp.MustCompile(`(?i)^\[(ti|ar|al|by|offset|re|ve):.*\]$`)
)

// QQExactProvider 使用 QQ songmid 直接查询歌词，不执行关键词搜索。
type QQExactProvider struct {
	Client  *http.Client
	BaseURL string
}

func (QQExactProvider) ID() string { return "qq" }

func (provider QQExactProvider) FetchExact(ctx context.Context, metadata Metadata, session AuthSession) (Result, error) {
	trackID := strings.TrimSpace(metadata.TrackID)
	if trackID == "" {
		return Result{}, ErrMissingTrackID
	}
	if !platformid.QQ(trackID) {
		return Result{}, errors.New("QQ 音乐歌曲 ID 无效")
	}
	base := strings.TrimRight(provider.BaseURL, "/")
	if base == "" {
		base = "https://c.y.qq.com"
	}
	query := url.Values{
		"songmid":  {trackID},
		"format":   {"json"},
		"nobase64": {"1"},
		"g_tk":     {"5381"},
	}
	data, err := exactGet(ctx, provider.Client,
		base+"/lyric/fcgi-bin/fcg_query_lyric_new.fcg?"+query.Encode(),
		"https://y.qq.com/portal/player.html", "qq", session)
	if err != nil {
		return Result{}, err
	}
	var response struct {
		Code  int    `json:"code"`
		Lyric string `json:"lyric"`
	}
	if err := json.Unmarshal(unwrapJSONP(data), &response); err != nil {
		return Result{}, err
	}
	text := html.UnescapeString(response.Lyric)
	if !strings.Contains(text, "[") {
		if decoded, decodeErr := base64.StdEncoding.DecodeString(text); decodeErr == nil {
			text = string(decoded)
		}
	}
	if strings.TrimSpace(text) == "" {
		if response.Code != 0 {
			return Result{}, fmt.Errorf("QQ 音乐精确歌词接口返回代码 %d", response.Code)
		}
		return Result{}, errors.New("QQ 音乐歌曲没有歌词")
	}
	return ParseLyricsText(text)
}

// NeteaseExactProvider 使用网易云数字歌曲 ID 直接查询歌词，不执行关键词搜索。
type NeteaseExactProvider struct {
	Client  *http.Client
	BaseURL string
}

func (NeteaseExactProvider) ID() string { return "netease" }

func (provider NeteaseExactProvider) FetchExact(ctx context.Context, metadata Metadata, session AuthSession) (Result, error) {
	trackID := strings.TrimSpace(metadata.TrackID)
	if trackID == "" {
		return Result{}, ErrMissingTrackID
	}
	if !platformid.Netease(trackID) {
		return Result{}, errors.New("网易云歌曲 ID 无效")
	}
	if parsed, parseErr := strconv.ParseInt(trackID, 10, 64); parseErr != nil || parsed <= 0 {
		return Result{}, errors.New("网易云歌曲 ID 无效")
	}
	base := strings.TrimRight(provider.BaseURL, "/")
	if base == "" {
		base = "https://music.163.com"
	}
	query := url.Values{"id": {trackID}, "lv": {"-1"}, "kv": {"-1"}, "tv": {"-1"}}
	data, err := exactGet(ctx, provider.Client,
		base+"/api/song/lyric?"+query.Encode(),
		"https://music.163.com/", "netease", session)
	if err != nil {
		return Result{}, err
	}
	var response struct {
		Code        int  `json:"code"`
		NoLyric     bool `json:"nolyric"`
		Uncollected bool `json:"uncollected"`
		LRC         struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
	}
	if err := json.Unmarshal(unwrapJSONP(data), &response); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(response.LRC.Lyric) == "" {
		switch {
		case response.NoLyric:
			return Result{}, errors.New("网易云标记该歌曲为纯音乐")
		case response.Uncollected:
			return Result{}, errors.New("网易云尚未收录该歌曲歌词")
		case response.Code != 0 && response.Code != 200:
			return Result{}, fmt.Errorf("网易云精确歌词接口返回代码 %d", response.Code)
		default:
			return Result{}, errors.New("网易云歌曲没有歌词")
		}
	}
	return ParseLyricsText(response.LRC.Lyric)
}

// ParseLyricsText 自动区分带时间戳 LRC 与无时间戳纯文本。
func ParseLyricsText(text string) (Result, error) {
	document, err := lyrics.ParseLRC(bytes.NewBufferString(text))
	if err != nil {
		return Result{}, err
	}
	if len(document.Lines) > 0 {
		return Result{Type: LyricsSynchronized, Document: document}, nil
	}
	plain := normalizePlainText(text)
	if plain == "" {
		return Result{}, errors.New("歌词内容为空")
	}
	return Result{Type: LyricsPlainText, PlainText: plain}, nil
}

func normalizePlainText(text string) string {
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if lrcMetadataLine.MatchString(line) {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func exactGet(ctx context.Context, client *http.Client, rawURL, referer, platform string, session AuthSession) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (Linux; ARMv7) CaseScreen-Edge/0.1")
	request.Header.Set("Accept", "application/json,text/plain,*/*")
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	if session != nil {
		if err := session.Authorize(ctx, platform, request); err != nil {
			return nil, fmt.Errorf("%s 会话注入失败: %w", platform, err)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		reportNeedsLogin(session, platform)
		return nil, fmt.Errorf("%w: %s HTTP %s", ErrLoginRequired, platform, response.Status)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumLyricsResponse+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximumLyricsResponse {
		return nil, errors.New("歌词响应超过 4 MiB")
	}
	// QQ/网易部分接口即使登录态失效或需要会员也会返回 HTTP 200，
	// 必须读取结构化错误字段，才能让主机中继准确返回 needs_login。
	if platformstatus.RequiresLogin(unwrapJSONP(data)) {
		reportNeedsLogin(session, platform)
		return nil, fmt.Errorf("%w: %s 平台返回登录或会员限制", ErrLoginRequired, platform)
	}
	return data, nil
}

func reportNeedsLogin(session AuthSession, platform string) {
	if reporter, ok := session.(loginStateReporter); ok {
		// 状态回报失败不能掩盖平台已经返回的登录/VIP错误。
		_ = reporter.MarkNeedsLogin(platform)
	}
}

func unwrapJSONP(data []byte) []byte {
	start := -1
	end := -1
	for index, value := range data {
		if value == '{' && start < 0 {
			start = index
		}
		if value == '}' {
			end = index + 1
		}
	}
	if start >= 0 && end > start {
		return data[start:end]
	}
	return data
}
