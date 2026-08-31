package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolverKnownQQOrNeteaseSourceNeverFallsBackAcrossPlatforms(t *testing.T) {
	for _, source := range []string{"qq", "netease"} {
		t.Run(source, func(t *testing.T) {
			trackID := "186016"
			if source == "qq" {
				trackID = "0039MnYb0qxYhV"
			}
			qqCalls, neteaseCalls := 0, 0
			resolver := NewResolver("",
				exactTestProvider{id: "qq", calls: &qqCalls, err: errors.New("同平台没有结果")},
				exactTestProvider{id: "netease", calls: &neteaseCalls, err: errors.New("同平台没有结果")},
			)
			_, _, err := resolver.Resolve(context.Background(), NowPlaying{
				Source: source, TrackID: trackID, TrackIDOrigin: TrackIDOriginSource,
				Title: "同名歌曲", Artist: "目标歌手",
			})
			if err == nil {
				t.Fatal("同平台查询失败后不应跨平台返回歌词")
			}
			if source == "qq" && (qqCalls != 1 || neteaseCalls != 0) {
				t.Fatalf("QQ 来源发生跨平台查询: qq=%d netease=%d", qqCalls, neteaseCalls)
			}
			if source == "netease" && (neteaseCalls != 1 || qqCalls != 0) {
				t.Fatalf("网易云来源发生跨平台查询: qq=%d netease=%d", qqCalls, neteaseCalls)
			}
		})
	}
}

func TestResolverRejectsTrackIDWithoutSupportedSource(t *testing.T) {
	calls := 0
	resolver := NewResolver("", exactTestProvider{id: "qq", calls: &calls})
	_, _, err := resolver.Resolve(context.Background(), NowPlaying{
		Source: "unknown-player", TrackID: "0039MnYb0qxYhV", Title: "歌曲",
	})
	if err == nil || !strings.Contains(err.Error(), "无法验证") {
		t.Fatalf("未知来源的 ID 没有被拒绝: %v", err)
	}
	if calls != 0 {
		t.Fatalf("未知来源 ID 仍触发了平台查询: %d", calls)
	}
}

func TestPlatformIDValidatorsRejectMalformedOrCrossPlatformIDsBeforeHTTP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()

	for _, trackID := range []string{"186016", "short", "0039-MnYb0qxYhV", strings.Repeat("A", 33)} {
		_, err := (QQProvider{BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{
			Source: "qq", TrackID: trackID, TrackIDOrigin: TrackIDOriginSource, Title: "歌曲",
		})
		if err == nil || !strings.Contains(err.Error(), "ID 无效") {
			t.Fatalf("畸形 QQ songmid 没有拒绝: id=%q err=%v", trackID, err)
		}
	}
	for _, trackID := range []string{"0039MnYb0qxYhV", "+186016", "0186016", "0", "9223372036854775808", strings.Repeat("9", 20)} {
		_, err := (NeteaseProvider{BaseURL: server.URL}).Fetch(context.Background(), NowPlaying{
			Source: "netease", TrackID: trackID, TrackIDOrigin: TrackIDOriginSource, Title: "歌曲",
		})
		if err == nil || !strings.Contains(err.Error(), "ID 无效") {
			t.Fatalf("畸形网易云 ID 没有拒绝: id=%q err=%v", trackID, err)
		}
	}
	if calls != 0 {
		t.Fatalf("无效平台 ID 到达了 HTTP 层: %d", calls)
	}
	if parsed, ok := parseNeteaseSongID("9223372036854775807"); !ok || parsed <= 0 {
		t.Fatal("合法 int64 网易云 ID 被拒绝")
	}
}

func TestQQAndNeteaseMissingNativeIDNeverSearchByTitle(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	tests := []struct {
		name     string
		provider Provider
		track    NowPlaying
	}{
		{
			name:     "qq",
			provider: QQProvider{BaseURL: server.URL},
			track:    NowPlaying{Source: "qq", Title: "同名歌曲", Artist: "目标歌手"},
		},
		{
			name:     "netease",
			provider: NeteaseProvider{BaseURL: server.URL},
			track:    NowPlaying{Source: "netease", Title: "同名歌曲", Artist: "目标歌手"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.provider.Fetch(context.Background(), test.track)
			if err == nil || !strings.Contains(err.Error(), "停止按标题猜测") {
				t.Fatalf("缺少原生歌曲 ID 时没有拒绝标题搜索: %v", err)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("缺少原生歌曲 ID 时仍访问了网络: %d", requests)
	}
}

func TestHostProvidersMapHTTP200LoginOrVIPBodyToSentinel(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		track    NowPlaying
		body     string
	}{
		{
			name: "qq", track: NowPlaying{Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource, Title: "歌曲"},
			body: `{"code":1000,"message":"请登录 QQ 音乐后重试"}`,
		},
		{
			name: "netease", track: NowPlaying{Source: "netease", TrackID: "186016", TrackIDOrigin: TrackIDOriginSource, Title: "歌曲"},
			body: `{"code":200,"needVip":true,"message":"该歌曲需要会员权限"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			switch test.name {
			case "qq":
				test.provider = QQProvider{BaseURL: server.URL}
			case "netease":
				test.provider = NeteaseProvider{BaseURL: server.URL}
			}
			_, err := test.provider.Fetch(context.Background(), test.track)
			if !errors.Is(err, ErrPlatformLoginRequired) {
				t.Fatalf("HTTP 200 登录/VIP 错误没有保留 sentinel: %v", err)
			}
		})
	}
}

func TestResolverIgnoresCacheWithoutMatchingPlatformIdentity(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
	}{
		{name: "旧缓存没有身份"},
		{name: "缓存歌曲 ID 不同", metadata: map[string]string{"platform": "qq", "track_id": "0039MnYb0qxYhX"}},
		{name: "缓存平台不同", metadata: map[string]string{"platform": "netease", "track_id": "186016"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			track := NowPlaying{
				Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
				Title: "歌曲", Artist: "歌手",
			}
			resolver := NewResolver(directory, exactPlainProvider{document: Document{
				Metadata: map[string]string{"platform": "qq", "track_id": "0039MnYb0qxYhV"},
				Lines:    []Line{{Text: "新歌词"}},
			}})
			writeRawResolverCache(t, resolver, track, "qq", Document{
				Metadata: test.metadata,
				Lines:    []Line{{Text: "不应命中的旧歌词"}},
			})

			document, source, err := resolver.Resolve(context.Background(), track)
			if err != nil {
				t.Fatal(err)
			}
			if source != "qq" || document.Lines[0].Text != "新歌词" ||
				document.Metadata["track_id"] != track.SourceTrackID() {
				t.Fatalf("错误缓存没有触发同平台身份重建: source=%q document=%+v", source, document)
			}

			cached, source, err := resolver.Resolve(context.Background(), track)
			if err != nil || source != "cache" || cached.Metadata["track_id"] != track.SourceTrackID() {
				t.Fatalf("精确匹配当前原生 ID 的新缓存没有复用: source=%q err=%v document=%+v", source, err, cached)
			}
		})
	}
}

func TestResolverRejectsProviderResponseWithMismatchedIdentity(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
	}{
		{name: "响应歌曲 ID 不同", metadata: map[string]string{"platform": "qq", "track_id": "0039MnYb0qxYhX"}},
		{name: "响应平台不同", metadata: map[string]string{"platform": "netease", "track_id": "186016"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cacheDirectory := t.TempDir()
			track := NowPlaying{
				Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
				Title: "歌曲", Artist: "歌手",
			}
			resolver := NewResolver(cacheDirectory, exactPlainProvider{document: Document{
				Metadata: test.metadata,
				Lines:    []Line{{Text: "不应显示的歌词"}},
			}})

			document, source, err := resolver.Resolve(context.Background(), track)
			if err == nil || !strings.Contains(err.Error(), "身份与当前歌曲不一致") {
				t.Fatalf("身份不匹配的提供方响应没有失败关闭: source=%q document=%+v err=%v", source, document, err)
			}
			if source != "" || document.HasContent() {
				t.Fatalf("身份不匹配的歌词仍被返回: source=%q document=%+v", source, document)
			}
			entries, readErr := os.ReadDir(cacheDirectory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("身份不匹配的响应被错误缓存: %d", len(entries))
			}
		})
	}
}

func TestResolverUnknownSourceFailsClosedBeforeCacheOrProvider(t *testing.T) {
	directory := t.TempDir()
	calls := 0
	track := NowPlaying{Source: "unknown-player", Title: "同名歌曲", Artist: "歌手"}
	resolver := NewResolver(directory, exactTestProvider{id: "qq", calls: &calls})
	writeRawResolverCache(t, resolver, track, "", Document{Lines: []Line{{Text: "旧标题缓存"}}})

	_, _, err := resolver.Resolve(context.Background(), track)
	if err == nil || !strings.Contains(err.Error(), "停止按标题猜测") {
		t.Fatalf("auto 对未知来源没有失败关闭: %v", err)
	}
	if calls != 0 {
		t.Fatalf("未知来源仍进入了在线提供方: %d", calls)
	}
}

func TestResolverLocalIsExplicitDirectReadWithoutCache(t *testing.T) {
	filesDirectory := t.TempDir()
	firstPath := filepath.Join(filesDirectory, "first.lrc")
	secondPath := filepath.Join(filesDirectory, "second.lrc")
	if err := os.WriteFile(firstPath, []byte("[00:00.00]第一份歌词\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("[00:00.00]第二份歌词\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cacheDirectory := t.TempDir()
	resolver := NewResolver(cacheDirectory, &LocalFileProvider{Path: firstPath})
	track := NowPlaying{Source: "unknown-player", Title: "本地歌曲"}
	if _, _, err := resolver.Resolve(context.Background(), track); err == nil {
		t.Fatal("未显式选择 local 时不应读取本地 LRC")
	}

	resolver.SetPreferredProvider("local")
	first, source, err := resolver.Resolve(context.Background(), track)
	if err != nil || source != "local" || len(first.Lines) != 1 || first.Lines[0].Text != "第一份歌词" {
		t.Fatalf("首次本地直读失败: source=%q document=%+v err=%v", source, first, err)
	}
	resolver.SetLocalPath(secondPath)
	second, source, err := resolver.Resolve(context.Background(), track)
	if err != nil || source != "local" || len(second.Lines) != 1 || second.Lines[0].Text != "第二份歌词" {
		t.Fatalf("本地路径变化没有立即生效: source=%q document=%+v err=%v", source, second, err)
	}
	entries, err := os.ReadDir(cacheDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("本地 LRC 被错误写入歌曲缓存: %d", len(entries))
	}
}

func writeRawResolverCache(t *testing.T, resolver *Resolver, track NowPlaying, preferred string, document Document) {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(resolver.cachePath(track, preferred), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSameTrackWithoutIDDoesNotCarryIdentityAcrossMissingDiscriminators(t *testing.T) {
	previous := NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "同名歌曲", Artist: "目标歌手",
		Album: "录音室专辑", Duration: int64(200 * time.Second),
	}
	if sameTrackWithoutID(NowPlaying{Source: "qq", Title: "同名歌曲", Artist: "目标歌手"}, previous) {
		t.Fatal("当前歌曲缺少专辑和时长时仍沿用了上一首平台 ID")
	}
	current := previous
	current.TrackID = ""
	if !sameTrackWithoutID(current, previous) {
		t.Fatal("完整身份信息一致时没有保留同一首歌曲 ID")
	}
}

func TestNowPlayingKeyPreservesQQSongMIDCase(t *testing.T) {
	upper := NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "同名歌曲", Artist: "歌手",
	}
	lower := upper
	lower.TrackID = strings.ToLower(upper.TrackID)
	if upper.key() == lower.key() {
		t.Fatal("区分大小写的 QQ songmid 被合并为同一首歌")
	}
}

func TestNowPlayingKeyDoesNotTreatUnverifiedIDAsCurrentIdentity(t *testing.T) {
	verified := NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "同名歌曲", Artist: "歌手",
	}
	unverified := verified
	unverified.TrackIDOrigin = TrackIDOriginInferred
	if verified.key() == unverified.key() {
		t.Fatal("本轮未验证的 ID 仍被当作上一轮的精确歌曲身份")
	}
}
