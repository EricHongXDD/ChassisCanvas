package lyrics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"

	"casescreen/internal/platformid"
)

type QQProvider struct {
	Client  *http.Client
	BaseURL string
	Session RequestAuthorizer
}

func (provider QQProvider) ID() string { return "qq" }

func (provider QQProvider) Fetch(ctx context.Context, track NowPlaying) (Document, error) {
	base := strings.TrimRight(provider.BaseURL, "/")
	if base == "" {
		base = "https://c.y.qq.com"
	}
	if sourceProviderID(track.Source) != provider.ID() {
		return Document{}, errors.New("QQ 音乐精确歌词需要 QQ 播放来源")
	}
	mid := track.SourceTrackID()
	if mid == "" {
		return Document{}, errors.New("QQ 音乐当前歌曲缺少来源 songmid，已停止按标题猜测")
	}
	if !platformid.QQ(mid) {
		return Document{}, errors.New("QQ 音乐歌曲 ID 无效")
	}
	lyricQuery := url.Values{"songmid": {mid}, "format": {"json"}, "nobase64": {"1"}, "g_tk": {"5381"}}
	lyricData, err := httpGetAuthorized(ctx, provider.Client, base+"/lyric/fcgi-bin/fcg_query_lyric_new.fcg?"+lyricQuery.Encode(), "https://y.qq.com/portal/player.html", provider.ID(), provider.Session)
	if err != nil {
		return Document{}, err
	}
	var lyric struct {
		Lyric string `json:"lyric"`
	}
	if err := json.Unmarshal(unwrapJSONP(lyricData), &lyric); err != nil {
		return Document{}, err
	}
	text := html.UnescapeString(lyric.Lyric)
	if !strings.Contains(text, "[") {
		if decoded, decodeErr := base64.StdEncoding.DecodeString(text); decodeErr == nil {
			text = string(decoded)
		}
	}
	if strings.TrimSpace(text) == "" {
		return Document{}, errors.New("QQ 音乐歌曲没有 LRC 歌词")
	}
	document, err := ParseLyricsText(text)
	if err != nil {
		return Document{}, err
	}
	if document.Metadata == nil {
		document.Metadata = make(map[string]string)
	}
	document.Metadata["track_id"] = mid
	document.Metadata["platform"] = provider.ID()
	return document, nil
}
