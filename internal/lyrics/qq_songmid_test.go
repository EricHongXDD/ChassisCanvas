//go:build windows

package lyrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQQSongMIDClientUsesExactSongIDAndCachesMapping(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v8/fcg-bin/fcg_play_single_song.fcg" {
			t.Fatalf("按 songId 换 MID 时发起了其他请求: %s", request.URL.String())
		}
		if got := request.URL.Query().Get("songid"); got != "97773" {
			t.Fatalf("QQ songId 请求错误: %q", got)
		}
		if request.URL.Query().Get("w") != "" || request.URL.Query().Get("key") != "" {
			t.Fatal("按原生 ID 换 MID 时携带了搜索词")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"code":0,"data":[{"id":97773,"mid":"0039MnYb0qxYhV","name":"晴天","singer":[{"name":"周杰伦"}],"album":{"name":"叶惠美"},"interval":269}]}`)
	}))
	defer server.Close()

	client := &qqSongMIDClient{BaseURL: server.URL, Client: server.Client()}
	for index := 0; index < 2; index++ {
		detail, err := client.ResolveSongMID(context.Background(), 97773)
		if err != nil {
			t.Fatalf("精确 songId 换 songmid 失败: %v", err)
		}
		if detail.SongID != 97773 || detail.SongMID != "0039MnYb0qxYhV" || detail.Duration != 269*time.Second {
			t.Fatalf("单曲详情错误: %+v", detail)
		}
	}
	if requests != 1 {
		t.Fatalf("稳定 songId 到 MID 映射未复用主机内存缓存: %d", requests)
	}
}

func TestQQSongMIDClientCacheIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		songID := request.URL.Query().Get("songid")
		_, _ = fmt.Fprintf(writer, `{"code":0,"data":[{"id":%s,"mid":"0039MnYb0qxYhV","name":"测试","singer":[{"name":"歌手"}],"album":{"name":"专辑"},"interval":200}]}`, songID)
	}))
	defer server.Close()
	client := &qqSongMIDClient{BaseURL: server.URL, Client: server.Client()}
	for songID := uint32(1); songID <= qqSongMIDCacheCapacity+1; songID++ {
		if _, err := client.ResolveSongMID(context.Background(), songID); err != nil {
			t.Fatalf("填充有界 QQ songmid 缓存失败: id=%d err=%v", songID, err)
		}
	}
	client.mu.RLock()
	defer client.mu.RUnlock()
	if len(client.cache) != qqSongMIDCacheCapacity || len(client.order) != qqSongMIDCacheCapacity {
		t.Fatalf("QQ songmid 缓存没有保持固定容量: cache=%d order=%d", len(client.cache), len(client.order))
	}
	if _, exists := client.cache[1]; exists {
		t.Fatal("QQ songmid 缓存没有淘汰最早项目")
	}
	if _, exists := client.cache[qqSongMIDCacheCapacity+1]; !exists {
		t.Fatal("QQ songmid 缓存没有保留最新项目")
	}
}

func TestQQSongMIDClientRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(writer, strings.Repeat("x", qqSongDetailMaximumBytes+1))
	}))
	defer server.Close()
	client := &qqSongMIDClient{BaseURL: server.URL, Client: server.Client()}
	if _, err := client.ResolveSongMID(context.Background(), 97773); err == nil ||
		!strings.Contains(err.Error(), "响应过大") {
		t.Fatalf("超大 QQ 单曲详情响应没有在解析前被拒绝: %v", err)
	}
}

func TestQQInjectorIdentityToLyricsUsesOnlyExactIDRequests(t *testing.T) {
	detailRequests, lyricRequests := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v8/fcg-bin/fcg_play_single_song.fcg":
			detailRequests++
			if request.URL.Query().Get("songid") != "97773" {
				t.Fatalf("单曲详情没有使用来源 songId: %s", request.URL.String())
			}
			_, _ = fmt.Fprint(writer, `{"code":0,"data":[{"id":97773,"mid":"0039MnYb0qxYhV","name":"晴天","singer":[{"name":"周杰伦"}],"album":{"name":"叶惠美"},"interval":269}]}`)
		case "/lyric/fcgi-bin/fcg_query_lyric_new.fcg":
			lyricRequests++
			if request.URL.Query().Get("songmid") != "0039MnYb0qxYhV" {
				t.Fatalf("歌词请求没有使用已验证 songmid: %s", request.URL.String())
			}
			_, _ = fmt.Fprint(writer, `{"lyric":"[00:00.00]第一句\n[00:02.00]第二句"}`)
		default:
			t.Fatalf("精确 QQ 链路发起了额外请求: %s", request.URL.String())
		}
	}))
	defer server.Close()

	track := enrichQQPlatformTrackID(context.Background(), qqInjectorCurrentTrack(), &qqSongMIDClient{
		BaseURL: server.URL, Client: server.Client(),
	})
	if track.SourceTrackID() != "0039MnYb0qxYhV" {
		t.Fatalf("来源 songId 没有升级为已验证 songmid: %+v", track)
	}
	document, err := (QQProvider{BaseURL: server.URL, Client: server.Client()}).Fetch(context.Background(), track)
	if err != nil {
		t.Fatalf("已验证 QQ songmid 获取歌词失败: %v", err)
	}
	if detailRequests != 1 || lyricRequests != 1 || len(document.Lines) != 2 ||
		document.Metadata["track_id"] != "0039MnYb0qxYhV" {
		t.Fatalf("QQ 精确链路结果错误: detail=%d lyric=%d document=%+v", detailRequests, lyricRequests, document)
	}
}

func TestParseQQSongDetailRejectsWrongOrAmbiguousIdentity(t *testing.T) {
	valid := `{"code":0,"data":[{"id":"97773","songmid":"0039MnYb0qxYhV","songname":"晴天","singer":[{"name":"周杰伦"}],"albumname":"叶惠美","interval":269}]}`
	if detail, err := parseQQSongDetail([]byte(valid), 97773); err != nil || detail.SongMID != "0039MnYb0qxYhV" {
		t.Fatalf("字符串 songId 的精确详情未解析: detail=%+v err=%v", detail, err)
	}
	for name, payload := range map[string]string{
		"wrong_id":          strings.Replace(valid, `"97773"`, `"97774"`, 1),
		"zero_id":           strings.Replace(valid, `"97773"`, `"0"`, 1),
		"bad_mid":           strings.Replace(valid, "0039MnYb0qxYhV", "bad-mid", 1),
		"no_duration":       strings.Replace(valid, `"interval":269`, `"interval":0`, 1),
		"long_duration":     strings.Replace(valid, `"interval":269`, `"interval":86401`, 1),
		"overflow_duration": strings.Replace(valid, `"interval":269`, `"interval":9223372036854775807`, 1),
		"long_title":        strings.Replace(valid, "晴天", strings.Repeat("题", qqSongDetailMaximumTextRunes+1), 1),
		"long_album":        strings.Replace(valid, "叶惠美", strings.Repeat("辑", qqSongDetailMaximumTextRunes+1), 1),
		"long_artist":       strings.Replace(valid, "周杰伦", strings.Repeat("人", qqSongDetailMaximumArtistRunes+1), 1),
		"many_artists": strings.Replace(valid, `[{"name":"周杰伦"}]`,
			`[`+strings.TrimSuffix(strings.Repeat(`{"name":"歌手"},`, qqSongDetailMaximumArtists+1), ",")+`]`, 1),
		"duplicate": `{"code":0,"data":[` + strings.TrimSuffix(strings.TrimPrefix(valid, `{"code":0,"data":[`), `]}`) + `,` + strings.TrimSuffix(strings.TrimPrefix(valid, `{"code":0,"data":[`), `]}`) + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseQQSongDetail([]byte(payload), 97773); err == nil {
				t.Fatal("非精确 QQ 单曲详情未被拒绝")
			}
		})
	}
}

type fixedQQSongMIDResolver struct {
	detail qqSongDetail
	err    error
	calls  int
}

func (resolver *fixedQQSongMIDResolver) ResolveSongMID(_ context.Context, _ uint32) (qqSongDetail, error) {
	resolver.calls++
	return resolver.detail, resolver.err
}

func qqInjectorCurrentTrack() NowPlaying {
	return NowPlaying{
		Source: "qq", QQSongID: 97773, Title: "晴天", Artist: "周杰伦", Album: "叶惠美",
		Duration: int64(269 * time.Second), Position: int64(42 * time.Second), PositionKnown: true, Playing: true,
	}
}

func qqExactDetail() qqSongDetail {
	return qqSongDetail{
		SongID: 97773, SongMID: "0039MnYb0qxYhV", Title: "晴天", Artist: "周杰伦",
		Album: "叶惠美", Duration: 269 * time.Second,
	}
}

func TestEnrichQQPlatformTrackIDRequiresExactSameTrackEvidence(t *testing.T) {
	resolver := &fixedQQSongMIDResolver{detail: qqExactDetail()}
	result := enrichQQPlatformTrackID(context.Background(), qqInjectorCurrentTrack(), resolver)
	if result.SourceTrackID() != "0039MnYb0qxYhV" || result.TrackIDOrigin != TrackIDOriginSource {
		t.Fatalf("严格 QQ 证据未转换为来源 songmid: %+v", result)
	}

	mutations := map[string]func(*qqSongDetail){
		"song_id":  func(detail *qqSongDetail) { detail.SongID++ },
		"title":    func(detail *qqSongDetail) { detail.Title = "同名其他歌" },
		"artist":   func(detail *qqSongDetail) { detail.Artist = "其他歌手" },
		"album":    func(detail *qqSongDetail) { detail.Album = "其他专辑" },
		"duration": func(detail *qqSongDetail) { detail.Duration += 3 * time.Second },
		"mid":      func(detail *qqSongDetail) { detail.SongMID = "bad-mid" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			detail := qqExactDetail()
			mutate(&detail)
			current := qqInjectorCurrentTrack()
			got := enrichQQPlatformTrackID(context.Background(), current, &fixedQQSongMIDResolver{detail: detail})
			if got.SourceTrackID() != "" || got.TrackID != "" || got.TrackIDOrigin != "" {
				t.Fatalf("不匹配的 QQ 详情被标记为来源 ID: %+v", got)
			}
		})
	}
}

func TestQQSongIDNeverLeavesHostStatusJSON(t *testing.T) {
	data, err := json.Marshal(qqInjectorCurrentTrack())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "97773") || strings.Contains(string(data), "qq_song") {
		t.Fatalf("QQ 数值 songId 泄漏到状态 JSON: %s", data)
	}
}
