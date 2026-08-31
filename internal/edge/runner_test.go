package edge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"casescreen/internal/edgedisplay"
	"casescreen/internal/edgelyrics"
	"casescreen/internal/edgesync"
	"casescreen/internal/lyrics"
)

type fakeMetadataClient struct {
	mu       sync.Mutex
	envelope edgesync.Envelope
	err      error
	calls    int
}

func (client *fakeMetadataClient) Current(context.Context) (edgesync.Envelope, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.calls++
	return client.envelope, client.err
}

type fakeLyricsEngine struct {
	mu            sync.Mutex
	state         edgelyrics.State
	updates       []edgelyrics.Metadata
	firstError    error
	errorReturned bool
}

func (engine *fakeLyricsEngine) Update(_ context.Context, metadata edgelyrics.Metadata) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.updates = append(engine.updates, metadata)
	engine.state.Track = metadata
	engine.state.Position = metadata.Position
	if engine.firstError != nil && !engine.errorReturned {
		engine.errorReturned = true
		engine.state.LastError = engine.firstError.Error()
		return engine.firstError
	}
	return nil
}

func (engine *fakeLyricsEngine) Snapshot() edgelyrics.State {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.state
}

func (engine *fakeLyricsEngine) setState(state edgelyrics.State) {
	engine.mu.Lock()
	engine.state = state
	engine.mu.Unlock()
}

type fakeDisplay struct {
	mu      sync.Mutex
	updates []edgedisplay.LyricsUpdate
	err     error
}

func (display *fakeDisplay) SendLyricsUpdate(_ context.Context, update edgedisplay.LyricsUpdate) error {
	display.mu.Lock()
	defer display.mu.Unlock()
	display.updates = append(display.updates, update)
	return display.err
}

func validTestConfig() Config {
	return Config{
		PollInterval: time.Second, RenderInterval: 500 * time.Millisecond,
		SyncTimeout: time.Second, LyricsRetryMin: 5 * time.Second,
		LyricsRetryMax: time.Minute, ErrorRepeatAfter: 30 * time.Second,
	}
}

func TestMetadataFromEnvelopeUsesPlatformIDAndLocalReceiveAnchor(t *testing.T) {
	receivedAt := time.Date(2026, 8, 26, 20, 30, 0, 0, time.UTC)
	metadata, mode := MetadataFromEnvelope(edgesync.Envelope{
		Available: true,
		Metadata:  edgesync.Metadata{Source: " qq ", TrackID: " mid-001 ", LyricMode: edgesync.LyricModeTimed},
		Track: lyrics.NowPlaying{
			Source: "ignored", TrackID: "ignored", Title: "歌名", Artist: "歌手", Album: "专辑",
			Position: int64(12 * time.Second), Duration: int64(180 * time.Second),
			PositionKnown: true, Playing: true,
		},
	}, receivedAt)
	if metadata.Source != "qq" || metadata.TrackID != "mid-001" || metadata.Title != "歌名" {
		t.Fatalf("平台歌曲 ID 映射错误: %#v", metadata)
	}
	if metadata.Position != int64(12*time.Second) || !metadata.PositionKnown || !metadata.Playing {
		t.Fatalf("播放时间轴映射错误: %#v", metadata)
	}
	if metadata.CapturedAt != receivedAt || mode != edgesync.LyricModeTimed {
		t.Fatalf("本地锚点或歌词模式错误: captured=%v mode=%q", metadata.CapturedAt, mode)
	}
}

func TestMetadataFromUnavailableEnvelopeClearsStaleTrack(t *testing.T) {
	metadata, mode := MetadataFromEnvelope(edgesync.Envelope{
		Available: false,
		Metadata:  edgesync.Metadata{Source: "qq", TrackID: "stale"},
		Track:     lyrics.NowPlaying{Title: "旧歌曲", Playing: true},
	}, time.Now())
	if metadata != (edgelyrics.Metadata{}) || mode != edgesync.LyricModeUnknown {
		t.Fatalf("不可用快照不应保留旧歌: metadata=%#v mode=%q", metadata, mode)
	}
}

func TestRenderThrottleSkipsIdenticalPausedPacket(t *testing.T) {
	core := &fakeLyricsEngine{state: edgelyrics.State{
		Track:    edgelyrics.Metadata{Source: "qq", TrackID: "mid", Playing: false},
		Position: 3, LyricsType: edgelyrics.LyricsSynchronized,
		Current: lyrics.Line{Text: "当前句"}, Next: lyrics.Line{Text: "下一句"},
	}}
	display := &fakeDisplay{}
	runner, err := NewRunner(validTestConfig(), &fakeMetadataClient{}, func() LyricsEngine { return core }, display)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.RenderOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.RenderOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(display.updates) != 1 {
		t.Fatalf("暂停且状态不变时重复发送了小包: %d", len(display.updates))
	}
	state := core.Snapshot()
	state.Position++
	core.setState(state)
	if err := runner.RenderOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(display.updates) != 2 {
		t.Fatalf("播放位置变化后没有发送新包: %d", len(display.updates))
	}
}

func TestRenderIntervalMustStayWithinHardwareCadence(t *testing.T) {
	for _, interval := range []time.Duration{249 * time.Millisecond, 1001 * time.Millisecond} {
		config := validTestConfig()
		config.RenderInterval = interval
		if _, err := NewRunner(config, &fakeMetadataClient{}, func() LyricsEngine { return &fakeLyricsEngine{} }, &fakeDisplay{}); err == nil {
			t.Fatalf("应拒绝显示刷新间隔 %v", interval)
		}
	}
}

func TestRepeatedSyncErrorIsDeduplicated(t *testing.T) {
	now := time.Date(2026, 8, 26, 21, 0, 0, 0, time.UTC)
	client := &fakeMetadataClient{err: errors.New("网络断开")}
	var messages []string
	config := validTestConfig()
	config.ErrorSink = func(scope, message string) { messages = append(messages, scope+":"+message) }
	runner, err := newRunnerWithClock(config, client, func() LyricsEngine { return &fakeLyricsEngine{} }, &fakeDisplay{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_ = runner.PollOnce(context.Background())
	_ = runner.PollOnce(context.Background())
	if len(messages) != 1 {
		t.Fatalf("相同同步错误没有去重: %#v", messages)
	}
	now = now.Add(config.ErrorRepeatAfter)
	_ = runner.PollOnce(context.Background())
	if len(messages) != 2 {
		t.Fatalf("超过重复报告窗口后没有重新报告: %#v", messages)
	}
}

func TestSyncFailureKeepsCachedTimelineVisible(t *testing.T) {
	core := &fakeLyricsEngine{state: edgelyrics.State{
		Track:    edgelyrics.Metadata{Source: "netease", TrackID: "186016", Playing: true},
		Position: int64(9 * time.Second), LyricsType: edgelyrics.LyricsSynchronized,
		Current: lyrics.Line{Text: "缓存当前句"}, Next: lyrics.Line{Text: "缓存下一句"},
	}}
	display := &fakeDisplay{}
	runner, err := NewRunner(validTestConfig(), &fakeMetadataClient{err: errors.New("离线")}, func() LyricsEngine { return core }, display)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PollOnce(context.Background()); err == nil {
		t.Fatal("模拟断网应返回错误")
	}
	if len(core.updates) != 0 {
		t.Fatal("同步失败不应向 Core 写入空元数据并清除缓存")
	}
	if err := runner.RenderOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := display.updates[0]; got.Current != "缓存当前句" || got.Next != "缓存下一句" {
		t.Fatalf("断网后没有继续显示本地时间轴: %#v", got)
	}
}

func TestFailedExactLookupRetriesOnlyAfterBackoff(t *testing.T) {
	now := time.Date(2026, 8, 26, 22, 0, 0, 0, time.UTC)
	envelope := edgesync.Envelope{
		Available: true,
		Metadata:  edgesync.Metadata{Source: "qq", TrackID: "retry-mid", LyricMode: edgesync.LyricModeTimed},
		Track:     lyrics.NowPlaying{Title: "歌曲", PositionKnown: true, Playing: true},
	}
	client := &fakeMetadataClient{envelope: envelope}
	failed := &fakeLyricsEngine{firstError: errors.New("网络离线")}
	succeeded := &fakeLyricsEngine{state: edgelyrics.State{
		LyricsType: edgelyrics.LyricsSynchronized, Current: lyrics.Line{Text: "已恢复"},
	}}
	factoryCalls := 0
	factory := func() LyricsEngine {
		factoryCalls++
		if factoryCalls == 1 {
			return failed
		}
		return succeeded
	}
	runner, err := newRunnerWithClock(validTestConfig(), client, factory, &fakeDisplay{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PollOnce(context.Background()); err == nil {
		t.Fatal("首次精确查询应失败")
	}
	if err := runner.PollOnce(context.Background()); err != nil {
		t.Fatalf("退避前普通进度包不应重复联网: %v", err)
	}
	if factoryCalls != 1 {
		t.Fatalf("退避前错误重建 Core: %d", factoryCalls)
	}
	now = now.Add(validTestConfig().LyricsRetryMin)
	if err := runner.PollOnce(context.Background()); err != nil {
		t.Fatalf("退避到期后重试应成功: %v", err)
	}
	if factoryCalls != 2 {
		t.Fatalf("退避到期后没有新建 Core 重试: %d", factoryCalls)
	}
}

func TestMissingTrackIDHidesLyricsAndNeverRetriesByTitle(t *testing.T) {
	core := &fakeLyricsEngine{firstError: edgelyrics.ErrMissingTrackID}
	factoryCalls := 0
	runner, err := NewRunner(validTestConfig(), &fakeMetadataClient{envelope: edgesync.Envelope{
		Available: true, Metadata: edgesync.Metadata{Source: "qq"},
		Track: lyrics.NowPlaying{Title: "只有歌名", Artist: "歌手"},
	}}, func() LyricsEngine {
		factoryCalls++
		return core
	}, &fakeDisplay{})
	if err != nil {
		t.Fatal(err)
	}
	_ = runner.PollOnce(context.Background())
	_ = runner.PollOnce(context.Background())
	if factoryCalls != 1 {
		t.Fatalf("缺少 track_id 时不应创建新核心按标题重试: %d", factoryCalls)
	}
	update := DisplayUpdate(core.Snapshot(), edgesync.LyricModeUnknown, "")
	if update.Current != "" || update.Next != "" {
		t.Fatalf("缺少 ID 时歌词图层没有隐藏: %#v", update)
	}
}

func TestPlainLyricsAlwaysUseFixedNotice(t *testing.T) {
	update := DisplayUpdate(edgelyrics.State{
		Track:      edgelyrics.Metadata{Source: "netease", TrackID: "123"},
		LyricsType: edgelyrics.LyricsPlainText, PlainText: "整份歌词不能进入高频包",
		PlainPreview: "整份歌词不能进入高频包", Notice: edgelyrics.PlainLyricsNotice,
	}, edgesync.LyricModePlain, "")
	if update.Current != edgedisplay.PlainLyricsNotice || update.Next != "" || update.LyricMode != edgedisplay.LyricModePlain {
		t.Fatalf("纯文本歌词没有固定降级: %#v", update)
	}
}

func TestStandaloneLoginErrorDoesNotOccupyLyricsLayer(t *testing.T) {
	update := DisplayUpdateWithLoginNotice(edgelyrics.State{
		Track:     edgelyrics.Metadata{Source: "qq", TrackID: "0039MnYb0qxYhV"},
		LastError: edgelyrics.ErrLoginRequired.Error(),
	}, edgesync.LyricModeUnknown, "", StandalonePlatformLoginNotice)
	if update.Current != "" || update.Next != "" {
		t.Fatalf("独立模式登录错误仍占用歌词图层: %#v", update)
	}
}

func TestStandaloneIdleNoticeStaysHiddenAndDoesNotOverrideRealLyrics(t *testing.T) {
	idle := "未发现 Wi-Fi 接口；详情已写入 TF 诊断报告"
	update := DisplayUpdateWithNotices(edgelyrics.State{}, edgesync.LyricModeUnknown, "", StandalonePlatformLoginNotice, idle)
	if update.Current != "" || update.Next != "" {
		t.Fatalf("空闲诊断仍占用歌词图层: %#v", update)
	}
	update = DisplayUpdateWithNotices(edgelyrics.State{
		Track:      edgelyrics.Metadata{Source: "qq", TrackID: "0039MnYb0qxYhV"},
		LyricsType: edgelyrics.LyricsSynchronized,
		Current:    lyrics.Line{Text: "当前歌词"},
	}, edgesync.LyricModeTimed, "", StandalonePlatformLoginNotice, idle)
	if update.Current != "当前歌词" {
		t.Fatalf("空闲提示覆盖了真实歌词: %#v", update)
	}
}
