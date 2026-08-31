//go:build windows

package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"casescreen/internal/platformid"
)

const (
	qqSongDetailMaximumBytes          = 64 << 10
	qqSongDetailMaximumTextRunes      = 512
	qqSongDetailMaximumArtists        = 16
	qqSongDetailMaximumArtistRunes    = 256
	qqSongDetailMaximumIntervalSecond = 24 * 60 * 60
	qqSongDurationSlack               = 2 * time.Second
	qqSongMIDCacheCapacity            = 512
)

type qqSongDetail struct {
	SongID   uint32
	SongMID  string
	Title    string
	Artist   string
	Album    string
	Duration time.Duration
}

type qqSongMIDResolver interface {
	ResolveSongMID(context.Context, uint32) (qqSongDetail, error)
}

// qqSongMIDClient 只调用 QQ 单曲详情接口。请求键是当前播放
// 对象携带的数值 songId，不发起标题或歌手搜索。
type qqSongMIDClient struct {
	Client  *http.Client
	BaseURL string

	mu    sync.RWMutex
	cache map[uint32]qqSongDetail
	order []uint32
}

func (client *qqSongMIDClient) ResolveSongMID(ctx context.Context, songID uint32) (qqSongDetail, error) {
	if songID == 0 {
		return qqSongDetail{}, errors.New("QQ 音乐 songId 不能为 0")
	}
	client.mu.RLock()
	cached, ok := client.cache[songID]
	client.mu.RUnlock()
	if ok {
		return cached, nil
	}
	detail, err := client.fetch(ctx, songID)
	if err != nil {
		return qqSongDetail{}, err
	}
	client.mu.Lock()
	if client.cache == nil {
		client.cache = make(map[uint32]qqSongDetail)
	}
	// 一首歌只需精确换取一次 songmid；固定容量避免播放器长期运行后
	// 缓存随听歌数量无界增长。FIFO 足够，因为换歌频率远低于媒体轮询。
	if existing, exists := client.cache[songID]; exists {
		detail = existing
	} else {
		if len(client.order) >= qqSongMIDCacheCapacity {
			delete(client.cache, client.order[0])
			copy(client.order, client.order[1:])
			client.order = client.order[:len(client.order)-1]
		}
		client.cache[songID] = detail
		client.order = append(client.order, songID)
	}
	client.mu.Unlock()
	return detail, nil
}

func (client *qqSongMIDClient) fetch(ctx context.Context, songID uint32) (qqSongDetail, error) {
	base := strings.TrimRight(client.BaseURL, "/")
	if base == "" {
		base = "https://c.y.qq.com"
	}
	query := url.Values{
		"songid":      {strconv.FormatUint(uint64(songID), 10)},
		"format":      {"json"},
		"outCharset":  {"utf8"},
		"platform":    {"yqq"},
		"needNewCode": {"0"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/v8/fcg-bin/fcg_play_single_song.fcg?"+query.Encode(), nil)
	if err != nil {
		return qqSongDetail{}, err
	}
	request.Header.Set("Referer", "https://y.qq.com/")
	request.Header.Set("User-Agent", "CaseScreen/QQNativeID")
	httpClient := client.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Second}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return qqSongDetail{}, fmt.Errorf("QQ 音乐按 songId 读取单曲详情: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return qqSongDetail{}, fmt.Errorf("QQ 音乐单曲详情 HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, qqSongDetailMaximumBytes+1))
	if err != nil {
		return qqSongDetail{}, err
	}
	if len(data) > qqSongDetailMaximumBytes {
		return qqSongDetail{}, errors.New("QQ 音乐单曲详情响应过大")
	}
	return parseQQSongDetail(data, songID)
}

type qqSongDetailResponse struct {
	Code int                   `json:"code"`
	Data []qqSongDetailPayload `json:"data"`
}

type qqSongDetailPayload struct {
	ID       json.RawMessage `json:"id"`
	MID      string          `json:"mid"`
	SongMID  string          `json:"songmid"`
	Name     string          `json:"name"`
	SongName string          `json:"songname"`
	Singer   []struct {
		Name string `json:"name"`
	} `json:"singer"`
	Album struct {
		Name string `json:"name"`
	} `json:"album"`
	AlbumName string `json:"albumname"`
	Interval  int64  `json:"interval"`
}

func parseQQSongDetail(data []byte, wantedSongID uint32) (qqSongDetail, error) {
	var response qqSongDetailResponse
	if err := json.Unmarshal(unwrapJSONP(data), &response); err != nil {
		return qqSongDetail{}, fmt.Errorf("解析 QQ 音乐单曲详情: %w", err)
	}
	if response.Code != 0 {
		return qqSongDetail{}, fmt.Errorf("QQ 音乐单曲详情返回错误码 %d", response.Code)
	}
	var matched *qqSongDetail
	for _, payload := range response.Data {
		songID, ok := parseQQDetailSongID(payload.ID)
		if !ok || songID != wantedSongID {
			continue
		}
		mid := strings.TrimSpace(payload.MID)
		if mid == "" {
			mid = strings.TrimSpace(payload.SongMID)
		}
		if !platformid.QQ(mid) || payload.Interval <= 0 ||
			payload.Interval > qqSongDetailMaximumIntervalSecond {
			return qqSongDetail{}, errors.New("QQ 音乐单曲详情缺少有效 songmid 或时长")
		}
		title := strings.TrimSpace(payload.Name)
		if title == "" {
			title = strings.TrimSpace(payload.SongName)
		}
		if err := validateQQSongDetailText("歌曲标题", title, qqSongDetailMaximumTextRunes, true); err != nil {
			return qqSongDetail{}, err
		}
		if len(payload.Singer) > qqSongDetailMaximumArtists {
			return qqSongDetail{}, errors.New("QQ 音乐单曲详情歌手数量超过限制")
		}
		artists := make([]string, 0, len(payload.Singer))
		for _, singer := range payload.Singer {
			name := strings.TrimSpace(singer.Name)
			if err := validateQQSongDetailText("歌手", name, qqSongDetailMaximumArtistRunes, false); err != nil {
				return qqSongDetail{}, err
			}
			if name != "" {
				artists = append(artists, name)
			}
		}
		album := strings.TrimSpace(payload.Album.Name)
		if album == "" {
			album = strings.TrimSpace(payload.AlbumName)
		}
		if err := validateQQSongDetailText("专辑", album, qqSongDetailMaximumTextRunes, true); err != nil {
			return qqSongDetail{}, err
		}
		candidate := qqSongDetail{
			SongID: songID, SongMID: mid, Title: title,
			Artist: strings.Join(artists, "/"), Album: album,
			Duration: time.Duration(payload.Interval) * time.Second,
		}
		if matched != nil {
			return qqSongDetail{}, errors.New("QQ 音乐单曲详情返回了重复 songId")
		}
		matched = &candidate
	}
	if matched == nil {
		return qqSongDetail{}, errors.New("QQ 音乐单曲详情未返回请求的 songId")
	}
	return *matched, nil
}

func validateQQSongDetailText(field, value string, maximumRunes int, required bool) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumRunes {
		return fmt.Errorf("QQ 音乐单曲详情%s超过长度限制或不是有效 UTF-8", field)
	}
	if required && value == "" {
		return fmt.Errorf("QQ 音乐单曲详情%s为空", field)
	}
	return nil
}

func parseQQDetailSongID(raw json.RawMessage) (uint32, bool) {
	value := strings.TrimSpace(string(raw))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		var decoded string
		if json.Unmarshal(raw, &decoded) != nil {
			return 0, false
		}
		value = decoded
	}
	if value == "" || len(value) > 10 || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	return uint32(parsed), err == nil && parsed > 0
}

func enrichQQPlatformTrackID(ctx context.Context, track NowPlaying, resolver qqSongMIDResolver) NowPlaying {
	if sourceProviderID(track.Source) != "qq" || track.SourceTrackID() != "" ||
		track.QQSongID == 0 || resolver == nil || ctx == nil || ctx.Err() != nil {
		return track
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	detail, err := resolver.ResolveSongMID(ctx, track.QQSongID)
	if err != nil || !qqSongDetailMatchesCurrent(detail, track) {
		return track
	}
	track.TrackID = detail.SongMID
	track.TrackIDOrigin = TrackIDOriginSource
	return track
}

// qqSongDetailMatchesCurrent 要求 songId 单曲详情和同一轮 SMTC
// 当前播放对象的标题、全部歌手、专辑和时长全部一致。
func qqSongDetailMatchesCurrent(detail qqSongDetail, current NowPlaying) bool {
	if detail.SongID == 0 || detail.SongID != current.QQSongID || !platformid.QQ(detail.SongMID) ||
		normalizeText(detail.Title) == "" || normalizeText(detail.Title) != normalizeText(current.Title) ||
		normalizeText(detail.Artist) == "" || normalizeText(detail.Artist) != normalizeText(current.Artist) ||
		normalizeText(detail.Album) == "" || normalizeText(current.Album) == "" ||
		normalizeText(detail.Album) != normalizeText(current.Album) || detail.Duration <= 0 || current.Duration <= 0 {
		return false
	}
	difference := detail.Duration - time.Duration(current.Duration)
	if difference < 0 {
		difference = -difference
	}
	return difference <= qqSongDurationSlack
}
