package lyrics

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func TestNeteaseProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/search/get/web", "/api/cloudsearch/pc":
			fmt.Fprint(writer, `{"result":{"songs":[{"id":42,"name":"测试歌曲","duration":200000,"album":{"name":"测试专辑"},"artists":[{"name":"测试歌手"}]}]}}`)
		case "/api/song/lyric":
			fmt.Fprint(writer, `{"lrc":{"lyric":"[00:00.00]第一句\n[00:03.00]第二句"}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	document, err := (NeteaseProvider{BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{
		Source: "netease", TrackID: "42", TrackIDOrigin: TrackIDOriginSource,
		Title: "测试歌曲", Artist: "测试歌手", Album: "测试专辑", Duration: int64(200 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Lines) != 2 || document.Lines[1].Text != "第二句" {
		t.Fatalf("网易云歌词解析错误: %#v", document)
	}
}

func TestQQProviderAcceptsJSONP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/soso/fcgi-bin/client_search_cp":
			fmt.Fprint(writer, `MusicJsonCallback({"data":{"song":{"list":[{"songmid":"0039MnYb0qxYhV","songname":"测试歌曲","albumname":"测试专辑","interval":200,"singer":[{"name":"测试歌手"}]}]}}})`)
		case "/lyric/fcgi-bin/fcg_query_lyric_new.fcg":
			fmt.Fprint(writer, `{"lyric":"[00:00.00]QQ第一句\n[00:02.00]QQ第二句"}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	document, err := (QQProvider{BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "测试歌曲", Artist: "测试歌手", Album: "测试专辑", Duration: int64(200 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Lines) != 2 || document.Lines[0].Text != "QQ第一句" {
		t.Fatalf("QQ 歌词解析错误: %#v", document)
	}
}

func TestKugouProvider(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("[00:00.00]酷狗第一句\n[00:04.00]酷狗第二句"))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/song_search_v2":
			fmt.Fprint(writer, `{"data":{"lists":[{"FileHash":"hash-1","SongName":"测试歌曲","SingerName":"测试歌手"}]}}`)
		case "/search":
			fmt.Fprint(writer, `{"candidates":[{"id":"lyric-1","accesskey":"key-1"}]}`)
		case "/download":
			fmt.Fprintf(writer, `{"content":%q}`, encoded)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	document, err := (KugouProvider{SearchBaseURL: server.URL, LyricsBaseURL: server.URL}).Fetch(context.Background(), NowPlaying{Title: "测试歌曲", Artist: "测试歌手"})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Lines) != 2 || document.Lines[1].Text != "酷狗第二句" {
		t.Fatalf("酷狗歌词解析错误: %#v", document)
	}
}

func TestSogouProviderAcceptsJSONP(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/lrc" {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Query().Get("query") != "测试歌曲 测试歌手" {
			t.Errorf("搜狗查询词错误: %q", request.URL.Query().Get("query"))
		}
		if request.URL.Query().Get("jsoncallback") != "CaseScreen" {
			t.Errorf("搜狗 JSONP 回调错误: %q", request.URL.Query().Get("jsoncallback"))
		}
		if request.Referer() != server.URL+"/" {
			t.Errorf("搜狗 Referer 错误: %q", request.Referer())
		}
		writer.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(writer, `CaseScreen({"lrc":"[ti:测试歌曲]\n[ar:测试歌手]\n[00:00.00]搜狗第一句\n[00:03.00]搜狗第二句"})`)
	}))
	defer server.Close()

	provider := SogouProvider{BaseURL: server.URL}
	if provider.ID() != "sogou" || provider.Name() != "搜狗音乐" {
		t.Fatalf("搜狗提供方标识错误: id=%q name=%q", provider.ID(), provider.Name())
	}
	document, err := provider.Fetch(context.Background(), NowPlaying{Title: "测试歌曲", Artist: "测试歌手"})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Lines) != 2 || document.Lines[1].Text != "搜狗第二句" || document.Lines[1].At != 3*time.Second {
		t.Fatalf("搜狗歌词解析错误: %#v", document)
	}
}

func TestSogouProviderAcceptsNestedGB18030Response(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		encoded, _, err := transform.Bytes(simplifiedchinese.GB18030.NewEncoder(), []byte(`{"data":{"lyrics":"[00:01.25]编码测试"}}`))
		if err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json; charset=gb18030")
		_, _ = writer.Write(encoded)
	}))
	defer server.Close()

	document, err := (SogouProvider{BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{Title: "编码测试"})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Lines) != 1 || document.Lines[0].Text != "编码测试" || document.Lines[0].At != 1250*time.Millisecond {
		t.Fatalf("搜狗 GB18030 歌词解析错误: %#v", document)
	}
}

func TestSogouProviderRejectsEmptyMalformedAndMismatchedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "空结果", body: `{}`},
		{name: "失效页面", body: `<!doctype html><title>service unavailable</title>`},
		{name: "非同步歌词", body: `{"lrc":"只有纯文本"}`},
		{name: "歌曲不匹配", body: `{"lrc":"[ti:另一首歌]\n[ar:另一歌手]\n[00:00.00]错误候选"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				fmt.Fprint(writer, test.body)
			}))
			defer server.Close()
			_, err := (SogouProvider{BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{Title: "测试歌曲", Artist: "测试歌手"})
			if err == nil || !strings.Contains(err.Error(), "搜狗音乐") {
				t.Fatalf("无效响应应安全失败: %v", err)
			}
		})
	}
}

func TestSogouProviderHonorsHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	client := &http.Client{Timeout: 20 * time.Millisecond}
	_, err := (SogouProvider{Client: client, BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{Title: "测试歌曲"})
	if err == nil || !strings.Contains(err.Error(), "搜狗音乐歌词不可用") || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("超时应安全返回错误: %v", err)
	}
}

func TestSogouProviderFallsBackToLRCAPIAndReportsActualSource(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "旧服务已停止", http.StatusBadGateway)
	}))
	defer legacy.Close()
	lrcAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/lyrics" {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Query().Get("title") != "测试歌曲" || request.URL.Query().Get("artist") != "测试歌手" {
			t.Errorf("LrcAPI 查询参数错误: %s", request.URL.RawQuery)
		}
		fmt.Fprint(writer, "[00:00.00]兼容源第一句\n[00:03.00]兼容源第二句")
	}))
	defer lrcAPI.Close()

	provider := SogouProvider{BaseURL: legacy.URL, LRCAPIBaseURL: lrcAPI.URL}
	document, err := provider.Fetch(context.Background(), NowPlaying{Title: "测试歌曲", Artist: "测试歌手"})
	if err != nil {
		t.Fatal(err)
	}
	source := provider.ResultSource(document)
	if len(document.Lines) != 2 || document.Lines[1].Text != "兼容源第二句" {
		t.Fatalf("LrcAPI 兜底歌词错误: %#v", document)
	}
	if source != "sogou/lrc.cx" || document.Metadata["casescreen_source"] != "lrc.cx" {
		t.Fatalf("实际歌词来源被误报: source=%q metadata=%#v", source, document.Metadata)
	}
}

func TestSogouProviderFallsBackToBestLRCLIBCandidate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/lrc", "/lyrics":
			http.Error(writer, "不可用", http.StatusBadGateway)
		case "/api/search":
			if request.URL.Query().Get("track_name") != "测试歌曲" || request.URL.Query().Get("artist_name") != "测试歌手" {
				t.Errorf("LRCLIB 查询参数错误: %s", request.URL.RawQuery)
			}
			fmt.Fprint(writer, `[
				{"trackName":"测试歌曲","artistName":"测试歌手","albumName":"现场版","duration":299,"syncedLyrics":"[00:00.00]错误时长候选"},
				{"trackName":"测试歌曲","artistName":"测试歌手","albumName":"测试专辑","duration":270,"syncedLyrics":"[00:00.00]准确候选\n[00:02.00]第二句"},
				{"trackName":"另一首歌","artistName":"另一歌手","duration":270,"syncedLyrics":"[00:00.00]错误歌曲"}
			]`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	provider := SogouProvider{BaseURL: server.URL, LRCAPIBaseURL: server.URL, LRCLIBBaseURL: server.URL}
	document, err := provider.Fetch(context.Background(), NowPlaying{
		Title: "测试歌曲", Artist: "测试歌手", Album: "测试专辑", Duration: int64(270 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Lines) != 2 || document.Lines[0].Text != "准确候选" || provider.ResultSource(document) != "sogou/lrclib" {
		t.Fatalf("LRCLIB 候选或来源错误: source=%q document=%#v", provider.ResultSource(document), document)
	}
}

func TestSogouProviderBoundsEveryNetworkFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	provider := SogouProvider{
		BaseURL: server.URL, LRCAPIBaseURL: server.URL, LRCLIBBaseURL: server.URL,
		LegacyTimeout: 20 * time.Millisecond, FallbackTimeout: 20 * time.Millisecond,
	}
	started := time.Now()
	_, err := provider.Fetch(context.Background(), NowPlaying{Title: "测试歌曲", Artist: "测试歌手"})
	if err == nil || !strings.Contains(err.Error(), "旧搜狗接口") || !strings.Contains(err.Error(), "LrcAPI") || !strings.Contains(err.Error(), "LRCLIB") {
		t.Fatalf("全部端点超时后应返回完整降级信息: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("兼容源超时没有被限制: %v", elapsed)
	}
}

func TestSogouProviderBoundsWholeFallbackChain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	provider := SogouProvider{
		BaseURL: server.URL, LRCAPIBaseURL: server.URL, LRCLIBBaseURL: server.URL,
		LegacyTimeout: time.Second, FallbackTimeout: time.Second, TotalTimeout: 45 * time.Millisecond,
	}
	started := time.Now()
	_, err := provider.Fetch(context.Background(), NowPlaying{Title: "测试歌曲", Artist: "测试歌手"})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("整条兼容链超时应返回明确错误: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("整条兼容链超过总时限: %v", elapsed)
	}
}

type countingProvider struct {
	count atomic.Int32
}

func (provider *countingProvider) ID() string { return "qq" }
func (provider *countingProvider) Fetch(_ context.Context, track NowPlaying) (Document, error) {
	provider.count.Add(1)
	return Document{
		Metadata: map[string]string{"platform": "qq", "track_id": track.SourceTrackID()},
		Lines:    []Line{{Text: "缓存歌词"}},
	}, nil
}

func TestResolverCachesByTrack(t *testing.T) {
	provider := &countingProvider{}
	resolver := NewResolver(t.TempDir(), provider)
	track := NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "同一首歌", Artist: "同一歌手",
	}
	if _, source, err := resolver.Resolve(context.Background(), track); err != nil || source != "qq" {
		t.Fatalf("首次解析失败: source=%s err=%v", source, err)
	}
	if _, source, err := resolver.Resolve(context.Background(), track); err != nil || source != "cache" {
		t.Fatalf("缓存解析失败: source=%s err=%v", source, err)
	}
	if provider.count.Load() != 1 {
		t.Fatalf("同一歌曲重复请求: %d", provider.count.Load())
	}
}

type namedCountingProvider struct {
	id    string
	text  string
	count atomic.Int32
}

func (provider *namedCountingProvider) ID() string { return provider.id }

func (provider *namedCountingProvider) Fetch(_ context.Context, track NowPlaying) (Document, error) {
	provider.count.Add(1)
	return Document{
		Metadata: map[string]string{"platform": provider.id, "track_id": track.SourceTrackID()},
		Lines:    []Line{{At: 0, Text: provider.text}},
	}, nil
}

func TestResolverPreferredProviderCannotOverridePlayerPlatform(t *testing.T) {
	netease := &namedCountingProvider{id: "netease", text: "网易歌词"}
	qq := &namedCountingProvider{id: "qq", text: "QQ 歌词"}
	resolver := NewResolver(t.TempDir(), netease, qq)
	track := NowPlaying{
		Source: "netease", TrackID: "186016", TrackIDOrigin: TrackIDOriginSource,
		Title: "同一首歌", Artist: "同一歌手",
	}

	resolver.SetPreferredProvider("qq")
	document, source, err := resolver.Resolve(context.Background(), track)
	if err != nil || source != "netease" || document.Lines[0].Text != "网易歌词" {
		t.Fatalf("首选项错误覆盖播放器平台: source=%q document=%+v err=%v", source, document, err)
	}
	if netease.count.Load() != 1 || qq.count.Load() != 0 {
		t.Fatalf("发生跨平台请求: netease=%d qq=%d", netease.count.Load(), qq.count.Load())
	}
	if _, source, err = resolver.Resolve(context.Background(), track); err != nil || source != "cache" || netease.count.Load() != 1 {
		t.Fatalf("同平台缓存没有复用: source=%q netease=%d err=%v", source, netease.count.Load(), err)
	}
}

func TestResolverCacheIsolatedByPlayerSource(t *testing.T) {
	netease := &namedCountingProvider{id: "netease", text: "网易歌词"}
	qq := &namedCountingProvider{id: "qq", text: "QQ 歌词"}
	resolver := NewResolver(t.TempDir(), netease, qq)
	base := NowPlaying{Title: "同一首歌", Artist: "同一歌手"}

	neteaseTrack := base
	neteaseTrack.Source = "netease"
	neteaseTrack.TrackID = "186016"
	neteaseTrack.TrackIDOrigin = TrackIDOriginSource
	if document, source, err := resolver.Resolve(context.Background(), neteaseTrack); err != nil || source != "netease" || document.Lines[0].Text != "网易歌词" {
		t.Fatalf("网易播放器解析失败: source=%q document=%+v err=%v", source, document, err)
	}
	qqTrack := base
	qqTrack.Source = "qq"
	qqTrack.TrackID = "0039MnYb0qxYhV"
	qqTrack.TrackIDOrigin = TrackIDOriginSource
	if document, source, err := resolver.Resolve(context.Background(), qqTrack); err != nil || source != "qq" || document.Lines[0].Text != "QQ 歌词" {
		t.Fatalf("播放器来源切换后错误复用缓存: source=%q document=%+v err=%v", source, document, err)
	}
}

func TestResolverPreferredProviderConcurrentAccess(t *testing.T) {
	providers := []*namedCountingProvider{
		{id: "netease", text: "网易歌词"},
		{id: "qq", text: "QQ 歌词"},
	}
	resolver := NewResolver("", providers[0], providers[1])
	done := make(chan struct{}, 64)
	for index := 0; index < cap(done); index++ {
		index := index
		go func() {
			providerID := providers[index%len(providers)].id
			resolver.SetPreferredProvider(providerID)
			trackID := "186016"
			if providerID == "qq" {
				trackID = "0039MnYb0qxYhV"
			}
			_, _, _ = resolver.Resolve(context.Background(), NowPlaying{
				Source: providerID, TrackID: trackID, TrackIDOrigin: TrackIDOriginSource,
				Title: fmt.Sprintf("并发歌曲-%d", index), Artist: "测试歌手",
			})
			done <- struct{}{}
		}()
	}
	for index := 0; index < cap(done); index++ {
		<-done
	}
}

func TestOfficialLyricsEndpointsSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_LYRICS_SMOKE") != "1" {
		t.Skip("设置 CASESCREEN_LYRICS_SMOKE=1 后检查三家在线歌词接口")
	}
	tests := []struct {
		provider Provider
		track    NowPlaying
	}{
		{provider: NeteaseProvider{}, track: NowPlaying{Source: "netease", TrackID: "186016", TrackIDOrigin: TrackIDOriginSource, Title: "晴天", Artist: "周杰伦"}},
		{provider: QQProvider{}, track: NowPlaying{Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource, Title: "晴天", Artist: "周杰伦"}},
		{provider: KugouProvider{}, track: NowPlaying{Title: "晴天", Artist: "周杰伦"}},
	}
	for _, test := range tests {
		t.Run(test.provider.ID(), func(t *testing.T) {
			document, err := test.provider.Fetch(context.Background(), test.track)
			if err != nil {
				t.Fatal(err)
			}
			if len(document.Lines) < 3 {
				t.Fatalf("同步歌词行数过少: %d", len(document.Lines))
			}
			t.Logf("获得 %d 行同步歌词", len(document.Lines))
		})
	}
}
