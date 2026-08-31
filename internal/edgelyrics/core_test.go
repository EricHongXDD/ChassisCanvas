package edgelyrics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingExactProvider struct {
	id       string
	result   Result
	err      error
	mu       *sync.Mutex
	calls    *map[string]int
	order    *[]string
	trackIDs *[]string
}

func (provider recordingExactProvider) ID() string { return provider.id }

func (provider recordingExactProvider) FetchExact(_ context.Context, metadata Metadata, _ AuthSession) (Result, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	(*provider.calls)[provider.id]++
	*provider.order = append(*provider.order, provider.id)
	if provider.trackIDs != nil {
		*provider.trackIDs = append(*provider.trackIDs, metadata.TrackID)
	}
	return provider.result, provider.err
}

func synchronizedResult() Result {
	result, err := ParseLyricsText("[00:00.00]第一句\n[00:04.00]第二句")
	if err != nil {
		panic(err)
	}
	return result
}

func TestCoreUsesExactPlatformIDOnlyOnTrackChange(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	trackIDs := []string{}
	providers := []ExactProvider{
		recordingExactProvider{id: "netease", result: synchronizedResult(), mu: mutex, calls: &calls, order: &order, trackIDs: &trackIDs},
		recordingExactProvider{id: "qq", result: synchronizedResult(), mu: mutex, calls: &calls, order: &order, trackIDs: &trackIDs},
	}
	core := newWithClock(t.TempDir(), nil, clock, providers...)

	track := Metadata{
		Source: "QQMusic", TrackID: "qq-mid-001", Title: "测试歌曲", Artist: "测试歌手",
		Position: int64(time.Second), Duration: int64(10 * time.Second), PositionKnown: true,
		Playing: true, CapturedAt: now.Add(-500 * time.Millisecond),
	}
	if err := core.Update(context.Background(), track); err != nil {
		t.Fatalf("首次精确解析失败: %v", err)
	}
	if len(order) != 1 || order[0] != "qq" || len(trackIDs) != 1 || trackIDs[0] != "qq-mid-001" {
		t.Fatalf("没有使用 QQ 平台歌曲 ID 精确查询: order=%#v ids=%#v", order, trackIDs)
	}

	state := core.SnapshotAt(now.Add(500 * time.Millisecond))
	if state.Current.Text != "第一句" || state.Next.Text != "第二句" {
		t.Fatalf("时间轴内容错误: current=%q next=%q", state.Current.Text, state.Next.Text)
	}
	if state.Position != int64(2*time.Second) || state.Progress != 0.5 {
		t.Fatalf("播放位置或进度错误: position=%v progress=%v", time.Duration(state.Position), state.Progress)
	}

	// 标题等展示信息变化不构成切歌，同一平台歌曲 ID 不能重新联网。
	now = now.Add(time.Second)
	track.Title = "同一首歌的新展示标题"
	track.Position = int64(3 * time.Second)
	track.CapturedAt = now
	if err := core.Update(context.Background(), track); err != nil {
		t.Fatalf("同曲目更新失败: %v", err)
	}
	if calls["qq"] != 1 || calls["netease"] != 0 {
		t.Fatalf("同一平台歌曲 ID 发生了重复请求: %#v", calls)
	}

	track.TrackID = "qq-mid-002"
	if err := core.Update(context.Background(), track); err != nil {
		t.Fatalf("切歌解析失败: %v", err)
	}
	if calls["qq"] != 2 || trackIDs[1] != "qq-mid-002" {
		t.Fatalf("歌曲 ID 变化后没有精确查询: calls=%#v ids=%#v", calls, trackIDs)
	}
}

func TestCoreNeverUsesForeignPlatformWithSameID(t *testing.T) {
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	core := newWithClock(t.TempDir(), nil, time.Now,
		recordingExactProvider{id: "netease", result: synchronizedResult(), mu: mutex, calls: &calls, order: &order},
		recordingExactProvider{id: "qq", err: errors.New("QQ 会话失效"), mu: mutex, calls: &calls, order: &order},
	)

	err := core.Update(context.Background(), Metadata{Source: "腾讯 QQ 音乐", TrackID: "qq-only-mid", Title: "歌曲"})
	if err == nil {
		t.Fatal("同平台精确查询失败应返回错误")
	}
	if len(order) != 1 || order[0] != "qq" || calls["netease"] != 0 {
		t.Fatalf("把 QQ songmid 错误地交给了其他平台: order=%#v calls=%#v", order, calls)
	}
}

func TestMissingTrackIDIsRejectedWithoutNetwork(t *testing.T) {
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	core := newWithClock(t.TempDir(), nil, time.Now,
		recordingExactProvider{id: "qq", result: synchronizedResult(), mu: mutex, calls: &calls, order: &order},
	)
	err := core.Update(context.Background(), Metadata{Source: "qq", Title: "只有歌名"})
	if !errors.Is(err, ErrMissingTrackID) {
		t.Fatalf("缺少平台歌曲 ID 的错误不明确: %v", err)
	}
	if calls["qq"] != 0 {
		t.Fatalf("缺少 ID 时仍进行了歌名查询: %#v", calls)
	}
	if state := core.Snapshot(); state.LastError != ErrMissingTrackID.Error() || state.HasLyrics {
		t.Fatalf("缺少 ID 的状态错误: %#v", state)
	}
}

func TestPlainLyricsExposeNoticeFullTextAndPreview(t *testing.T) {
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	plain, err := ParseLyricsText("[ti:无时间戳歌词]\n第一段第一行\n第一段第二行\n\n第二段")
	if err != nil {
		t.Fatalf("解析纯文本失败: %v", err)
	}
	core := newWithClock(t.TempDir(), nil, time.Now,
		recordingExactProvider{id: "netease", result: plain, mu: mutex, calls: &calls, order: &order},
	)
	track := Metadata{Source: "网易云音乐", TrackID: "123456", Title: "纯文本歌曲"}
	if err := core.Update(context.Background(), track); err != nil {
		t.Fatalf("加载纯文本歌词失败: %v", err)
	}
	state := core.Snapshot()
	if state.LyricsType != LyricsPlainText || !state.HasLyrics || state.HasCurrent {
		t.Fatalf("纯文本类型状态错误: %#v", state)
	}
	if state.Notice != "该首歌的歌词暂不支持滚动" {
		t.Fatalf("缺少明确的不可滚动提示: %q", state.Notice)
	}
	if state.PlainText != "第一段第一行\n第一段第二行\n\n第二段" {
		t.Fatalf("纯文本全文错误: %q", state.PlainText)
	}
	if state.PlainPreview != "第一段第一行\n第一段第二行" {
		t.Fatalf("纯文本首段错误: %q", state.PlainPreview)
	}
}

func TestFailedExactTrackIsNotRetriedByProgressPackets(t *testing.T) {
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	core := newWithClock(t.TempDir(), nil, time.Now,
		recordingExactProvider{id: "netease", err: errors.New("网络离线"), mu: mutex, calls: &calls, order: &order},
	)
	track := Metadata{Source: "netease", TrackID: "98765", Title: "歌曲", PositionKnown: true}
	if err := core.Update(context.Background(), track); err == nil {
		t.Fatal("首次联网失败应返回错误")
	}
	track.Position = int64(5 * time.Second)
	if err := core.Update(context.Background(), track); err != nil {
		t.Fatalf("同曲目状态更新不应重复返回查询错误: %v", err)
	}
	if calls["netease"] != 1 {
		t.Fatalf("失败曲目被自动重试: %#v", calls)
	}
	if state := core.Snapshot(); state.LastError == "" || state.HasLyrics {
		t.Fatalf("失败状态没有被保留: %#v", state)
	}
}

func TestExactCacheUsesPlatformAndTrackIDAcrossRestart(t *testing.T) {
	cacheDir := t.TempDir()
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	provider := recordingExactProvider{id: "qq", result: synchronizedResult(), mu: mutex, calls: &calls, order: &order}
	track := Metadata{Source: "qq", TrackID: "cache-mid", Title: "缓存歌曲"}

	first := newWithClock(cacheDir, nil, time.Now, provider)
	if err := first.Update(context.Background(), track); err != nil {
		t.Fatalf("写入缓存失败: %v", err)
	}
	second := newWithClock(cacheDir, nil, time.Now, provider)
	if err := second.Update(context.Background(), track); err != nil {
		t.Fatalf("读取缓存失败: %v", err)
	}
	if calls["qq"] != 1 {
		t.Fatalf("重启后没有按平台 ID 命中本地缓存: calls=%#v", calls)
	}
	if state := second.Snapshot(); state.Provider != "cache" || state.Current.Text != "第一句" {
		t.Fatalf("缓存状态错误: %#v", state)
	}
}

func TestUnknownPositionPreservesExistingAnchor(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	core := newWithClock(t.TempDir(), nil, func() time.Time { return now },
		recordingExactProvider{id: "qq", result: synchronizedResult(), mu: mutex, calls: &calls, order: &order},
	)
	track := Metadata{
		Source: "qq", TrackID: "position-mid", Position: int64(time.Second), PositionKnown: true,
		Playing: true, CapturedAt: now,
	}
	if err := core.Update(context.Background(), track); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	now = now.Add(time.Second)
	track.Position = 0
	track.PositionKnown = false
	track.CapturedAt = time.Time{}
	if err := core.Update(context.Background(), track); err != nil {
		t.Fatalf("未知位置更新失败: %v", err)
	}
	state := core.SnapshotAt(now.Add(time.Second))
	if state.Position != int64(3*time.Second) {
		t.Fatalf("未知位置覆盖了既有播放锚点: %v", time.Duration(state.Position))
	}
}

type headerAuthSession struct {
	platform string
	called   int
}

func (session *headerAuthSession) Authorize(_ context.Context, platform string, request *http.Request) error {
	session.platform = platform
	session.called++
	request.Header.Set("Authorization", "Bearer short-lived-session-token")
	return nil
}

func TestQQExactProviderUsesSongMIDAndInjectedSession(t *testing.T) {
	var requestError string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/lyric/fcgi-bin/fcg_query_lyric_new.fcg" {
			requestError = "请求了非精确歌词路径: " + request.URL.Path
		}
		if request.URL.Query().Get("songmid") != "0039MnYb0qxYhV" || request.URL.Query().Get("w") != "" {
			requestError = "没有仅使用 songmid 精确查询: " + request.URL.RawQuery
		}
		if request.Header.Get("Authorization") != "Bearer short-lived-session-token" {
			requestError = "认证会话没有注入请求"
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"code":0,"lyric":"[00:00.00]精确歌词\n[00:03.00]下一句"}`))
	}))
	defer server.Close()

	session := &headerAuthSession{}
	provider := QQExactProvider{Client: server.Client(), BaseURL: server.URL}
	result, err := provider.FetchExact(context.Background(), Metadata{TrackID: "0039MnYb0qxYhV"}, session)
	if err != nil {
		t.Fatalf("QQ 精确查询失败: %v", err)
	}
	if requestError != "" {
		t.Fatal(requestError)
	}
	if session.called != 1 || session.platform != "qq" {
		t.Fatalf("认证会话调用错误: called=%d platform=%q", session.called, session.platform)
	}
	if result.Type != LyricsSynchronized || len(result.Document.Lines) != 2 {
		t.Fatalf("QQ 同步歌词解析错误: %#v", result)
	}
}

func TestNeteaseExactProviderUsesNumericIDAndReturnsPlainText(t *testing.T) {
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rawQuery = request.URL.RawQuery
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"code":200,"lrc":{"lyric":"第一段\n\n第二段"}}`))
	}))
	defer server.Close()

	provider := NeteaseExactProvider{Client: server.Client(), BaseURL: server.URL}
	result, err := provider.FetchExact(context.Background(), Metadata{TrackID: "123456789"}, nil)
	if err != nil {
		t.Fatalf("网易云精确查询失败: %v", err)
	}
	if !strings.Contains(rawQuery, "id=123456789") || strings.Contains(rawQuery, "s=") {
		t.Fatalf("网易云没有使用数字 ID 精确查询: %q", rawQuery)
	}
	if result.Type != LyricsPlainText || result.PlainText != "第一段\n\n第二段" {
		t.Fatalf("网易云纯文本解析错误: %#v", result)
	}
}

func TestMetadataJSONContainsPlatformIDAndPositionKnown(t *testing.T) {
	data, err := json.Marshal(Metadata{Source: "qq", TrackID: "mid", PositionKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"track_id":"mid"`) || !strings.Contains(text, `"position_known":true`) {
		t.Fatalf("元数据 JSON 缺少硬要求字段: %s", text)
	}
}
