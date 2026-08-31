package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"casescreen/internal/platformid"
)

type NeteaseProvider struct {
	Client  *http.Client
	BaseURL string
	Session RequestAuthorizer
}

func (provider NeteaseProvider) ID() string { return "netease" }

func (provider NeteaseProvider) Fetch(ctx context.Context, track NowPlaying) (Document, error) {
	base := strings.TrimRight(provider.BaseURL, "/")
	if base == "" {
		base = "https://music.163.com"
	}
	if sourceProviderID(track.Source) != provider.ID() {
		return Document{}, errors.New("网易云精确歌词需要网易云播放来源")
	}
	nativeID := track.SourceTrackID()
	if nativeID == "" {
		return Document{}, errors.New("网易云当前歌曲缺少来源数字 ID，已停止按标题猜测")
	}
	songID, ok := parseNeteaseSongID(nativeID)
	if !ok {
		return Document{}, errors.New("网易云歌曲 ID 无效")
	}
	lyricQuery := url.Values{"id": {strconv.FormatInt(songID, 10)}, "lv": {"-1"}, "kv": {"-1"}, "tv": {"-1"}}
	lyricData, err := httpGetAuthorized(ctx, provider.Client, base+"/api/song/lyric?"+lyricQuery.Encode(), "https://music.163.com/", provider.ID(), provider.Session)
	if err != nil {
		return Document{}, err
	}
	var lyric struct {
		LRC struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
	}
	if err := json.Unmarshal(unwrapJSONP(lyricData), &lyric); err != nil {
		return Document{}, err
	}
	if strings.TrimSpace(lyric.LRC.Lyric) == "" {
		return Document{}, errors.New("网易云歌曲没有 LRC 歌词")
	}
	document, err := ParseLyricsText(lyric.LRC.Lyric)
	if err != nil {
		return Document{}, err
	}
	if document.Metadata == nil {
		document.Metadata = make(map[string]string)
	}
	document.Metadata["track_id"] = strconv.FormatInt(songID, 10)
	document.Metadata["platform"] = provider.ID()
	return document, nil
}

func parseNeteaseSongID(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if !platformid.Netease(value) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed > 0
}
