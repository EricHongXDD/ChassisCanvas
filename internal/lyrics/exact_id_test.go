package lyrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testRequestAuthorizer struct {
	platforms []string
}

func (authorizer *testRequestAuthorizer) Authorize(_ context.Context, platform string, request *http.Request) error {
	authorizer.platforms = append(authorizer.platforms, platform)
	request.Header.Set("X-Test-Platform-Session", platform)
	return nil
}

func TestQQProviderUsesExactSongMIDWithoutSearch(t *testing.T) {
	requests := 0
	authorizer := &testRequestAuthorizer{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/lyric/fcgi-bin/fcg_query_lyric_new.fcg" {
			t.Fatalf("提供歌曲 ID 后仍然发起搜索: %s", request.URL.String())
		}
		if got := request.URL.Query().Get("songmid"); got != "0039MnYb0qxYhV" {
			t.Fatalf("QQ songmid 错误: %q", got)
		}
		if request.Header.Get("X-Test-Platform-Session") != "qq" {
			t.Fatal("QQ 精确歌词请求没有使用平台登录态")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"lyric":"[00:00.00]精确 QQ 歌词"}`))
	}))
	defer server.Close()

	document, err := (QQProvider{BaseURL: server.URL, Session: authorizer}).Fetch(context.Background(), NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "同名歌曲", Artist: "歌手",
	})
	if err != nil {
		t.Fatalf("按 QQ songmid 获取歌词失败: %v", err)
	}
	if requests != 1 || !document.Scrollable() || document.Lines[0].Text != "精确 QQ 歌词" {
		t.Fatalf("QQ 精确歌词结果错误: requests=%d document=%+v", requests, document)
	}
	if document.Metadata["track_id"] != "0039MnYb0qxYhV" || document.Metadata["platform"] != "qq" {
		t.Fatalf("QQ 歌曲身份没有写入文档: %+v", document.Metadata)
	}
	if len(authorizer.platforms) != 1 || authorizer.platforms[0] != "qq" {
		t.Fatalf("登录态平台路由错误: %#v", authorizer.platforms)
	}
}

func TestNeteaseProviderUsesExactSongIDWithoutSearch(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/song/lyric" {
			t.Fatalf("提供歌曲 ID 后仍然发起搜索: %s", request.URL.String())
		}
		if got := request.URL.Query().Get("id"); got != "347230" {
			t.Fatalf("网易云歌曲 ID 错误: %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"lrc":{"lyric":"[00:01.00]精确网易云歌词"}}`))
	}))
	defer server.Close()

	document, err := (NeteaseProvider{BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{
		Source: "netease", TrackID: "347230", TrackIDOrigin: TrackIDOriginSource,
		Title: "同名歌曲", Artist: "歌手",
	})
	if err != nil {
		t.Fatalf("按网易云歌曲 ID 获取歌词失败: %v", err)
	}
	if requests != 1 || !document.Scrollable() || document.Lines[0].Text != "精确网易云歌词" {
		t.Fatalf("网易云精确歌词结果错误: requests=%d document=%+v", requests, document)
	}
}

type exactTestProvider struct {
	id    string
	calls *int
	err   error
}

func (provider exactTestProvider) ID() string { return provider.id }

func (provider exactTestProvider) Fetch(context.Context, NowPlaying) (Document, error) {
	(*provider.calls)++
	if provider.err != nil {
		return Document{}, provider.err
	}
	return Document{Lines: []Line{{Text: provider.id}}}, nil
}

func TestResolverDoesNotCrossPlatformsWhenExactIDIsKnown(t *testing.T) {
	qqCalls, neteaseCalls := 0, 0
	resolver := NewResolver(t.TempDir(),
		exactTestProvider{id: "netease", calls: &neteaseCalls},
		exactTestProvider{id: "qq", calls: &qqCalls, err: errors.New("该 ID 暂无歌词")},
	)
	_, _, err := resolver.Resolve(context.Background(), NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "同名歌曲", Artist: "歌手",
	})
	if err == nil || !strings.Contains(err.Error(), "该 ID 暂无歌词") {
		t.Fatalf("同平台精确失败没有返回真实错误: %v", err)
	}
	if qqCalls != 1 || neteaseCalls != 0 {
		t.Fatalf("精确 ID 被错误跨平台查询: qq=%d netease=%d", qqCalls, neteaseCalls)
	}
}

func TestPlainLyricsExposeNonScrollableMessage(t *testing.T) {
	document, err := ParseLyricsText("[ti:纯文本歌曲]\n第一段歌词\n第二段歌词")
	if err != nil {
		t.Fatalf("解析纯文本歌词失败: %v", err)
	}
	if document.Scrollable() || document.Mode() != "plain" || document.PlainText != "第一段歌词\n第二段歌词" {
		t.Fatalf("纯文本歌词能力错误: %+v", document)
	}
	resolver := NewResolver(t.TempDir(), exactPlainProvider{id: "local", document: document})
	resolver.SetPreferredProvider("local")
	engine := NewEngine(fixedNowPlayingSource{track: NowPlaying{Source: "local-player", Title: "纯文本歌曲"}}, resolver)
	engine.refresh(context.Background())
	if state := engine.Snapshot(); state.Current != UnsynchronizedLyricsMessage {
		t.Fatalf("未显示不可滚动提示: %+v", state)
	}
	status := engine.Status()
	if !status.HasLyrics || status.Scrollable || status.LyricMode != "plain" {
		t.Fatalf("纯文本歌词状态错误: %+v", status)
	}
}

func TestResolverRejectsUnverifiedPlatformTrackIDBeforeProvider(t *testing.T) {
	for _, origin := range []TrackIDOrigin{"", TrackIDOriginInferred} {
		t.Run(string(origin), func(t *testing.T) {
			calls := 0
			resolver := NewResolver(t.TempDir(), exactTestProvider{id: "qq", calls: &calls})
			_, _, err := resolver.Resolve(context.Background(), NowPlaying{
				Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: origin,
				Title: "歌曲", Artist: "歌手",
			})
			if err == nil || !strings.Contains(err.Error(), "缺少来源原生 ID") {
				t.Fatalf("未验证 ID 没有失败关闭: %v", err)
			}
			if calls != 0 {
				t.Fatalf("未验证 ID 仍进入了平台提供方: %d", calls)
			}
		})
	}
}

type exactPlainProvider struct {
	id       string
	document Document
}

func (provider exactPlainProvider) ID() string {
	if provider.id != "" {
		return provider.id
	}
	return "qq"
}
func (provider exactPlainProvider) Fetch(context.Context, NowPlaying) (Document, error) {
	return provider.document, nil
}
