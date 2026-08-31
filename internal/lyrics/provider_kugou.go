package lyrics

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type KugouProvider struct {
	Client        *http.Client
	SearchBaseURL string
	LyricsBaseURL string
}

func (provider KugouProvider) ID() string { return "kugou" }

func (provider KugouProvider) Fetch(ctx context.Context, track NowPlaying) (Document, error) {
	searchBase := strings.TrimRight(provider.SearchBaseURL, "/")
	if searchBase == "" {
		searchBase = "https://songsearch.kugou.com"
	}
	lyricsBase := strings.TrimRight(provider.LyricsBaseURL, "/")
	if lyricsBase == "" {
		lyricsBase = "https://lyrics.kugou.com"
	}
	query := url.Values{
		"keyword": {strings.TrimSpace(track.Title + " " + track.Artist)},
		"page":    {"1"}, "pagesize": {"8"}, "userid": {"-1"},
		"platform": {"WebFilter"}, "filter": {"2"}, "iscorrection": {"1"},
	}
	searchData, err := httpGet(ctx, provider.Client, searchBase+"/song_search_v2?"+query.Encode(), "https://www.kugou.com/")
	if err != nil {
		return Document{}, err
	}
	var search struct {
		Data struct {
			Lists []struct {
				Hash   string `json:"FileHash"`
				Name   string `json:"SongName"`
				Singer string `json:"SingerName"`
			} `json:"lists"`
		} `json:"data"`
	}
	if err := json.Unmarshal(unwrapJSONP(searchData), &search); err != nil {
		return Document{}, err
	}
	bestScore := -1
	hash := ""
	for _, song := range search.Data.Lists {
		score := matchScore(track, song.Name, song.Singer)
		if score > bestScore {
			bestScore = score
			hash = song.Hash
		}
	}
	if hash == "" {
		return Document{}, errors.New("酷狗音乐未找到匹配歌曲")
	}
	candidateQuery := url.Values{
		"ver": {"1"}, "man": {"yes"}, "client": {"pc"},
		"keyword": {strings.TrimSpace(track.Title + " " + track.Artist)}, "hash": {hash},
	}
	candidateData, err := httpGet(ctx, provider.Client, lyricsBase+"/search?"+candidateQuery.Encode(), "https://www.kugou.com/")
	if err != nil {
		return Document{}, err
	}
	var candidates struct {
		Candidates []struct {
			ID        string `json:"id"`
			AccessKey string `json:"accesskey"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(unwrapJSONP(candidateData), &candidates); err != nil {
		return Document{}, err
	}
	if len(candidates.Candidates) == 0 {
		return Document{}, errors.New("酷狗音乐没有歌词候选")
	}
	downloadQuery := url.Values{
		"ver": {"1"}, "client": {"pc"}, "fmt": {"lrc"}, "charset": {"utf8"},
		"id": {candidates.Candidates[0].ID}, "accesskey": {candidates.Candidates[0].AccessKey},
	}
	downloadData, err := httpGet(ctx, provider.Client, lyricsBase+"/download?"+downloadQuery.Encode(), "https://www.kugou.com/")
	if err != nil {
		return Document{}, err
	}
	var download struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(unwrapJSONP(downloadData), &download); err != nil {
		return Document{}, err
	}
	decoded, err := base64.StdEncoding.DecodeString(download.Content)
	if err != nil {
		decoded = []byte(download.Content)
	}
	if len(bytes.TrimSpace(decoded)) == 0 {
		return Document{}, errors.New("酷狗音乐歌曲没有 LRC 歌词")
	}
	return ParseLRC(bytes.NewReader(decoded))
}
