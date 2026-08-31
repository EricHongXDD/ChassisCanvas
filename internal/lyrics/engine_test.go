package lyrics

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixedNowPlayingSource struct {
	track NowPlaying
}

type sequenceNowPlayingSource struct {
	tracks []NowPlaying
	index  int
}

func (source *sequenceNowPlayingSource) Current(context.Context) (NowPlaying, error) {
	if len(source.tracks) == 0 {
		return NowPlaying{}, nil
	}
	index := source.index
	if index >= len(source.tracks) {
		index = len(source.tracks) - 1
	} else {
		source.index++
	}
	return source.tracks[index], nil
}

type countingNowPlayingSource struct {
	calls atomic.Int32
}

type closeTrackingNowPlayingSource struct {
	closed atomic.Int32
}

func (*closeTrackingNowPlayingSource) Current(context.Context) (NowPlaying, error) {
	return NowPlaying{}, nil
}

func (source *closeTrackingNowPlayingSource) Close() error {
	source.closed.Add(1)
	return nil
}

type mutableNowPlayingSource struct {
	mu    sync.RWMutex
	track NowPlaying
	err   error
}

func (source *mutableNowPlayingSource) Current(context.Context) (NowPlaying, error) {
	source.mu.RLock()
	defer source.mu.RUnlock()
	return source.track, source.err
}

func (source *mutableNowPlayingSource) Set(track NowPlaying, err error) {
	source.mu.Lock()
	source.track = track
	source.err = err
	source.mu.Unlock()
}

func (source *countingNowPlayingSource) Current(context.Context) (NowPlaying, error) {
	source.calls.Add(1)
	return NowPlaying{}, nil
}

func (source fixedNowPlayingSource) Current(context.Context) (NowPlaying, error) {
	return source.track, nil
}

func TestDisablingLyricsReleasesNativeSource(t *testing.T) {
	source := &closeTrackingNowPlayingSource{}
	engine := NewEngine(source, nil)
	engine.SetEnabled(false)
	if source.closed.Load() != 1 {
		t.Fatalf("关闭歌词没有释放本机播放源: %d", source.closed.Load())
	}
	engine.SetEnabled(false)
	if source.closed.Load() != 1 {
		t.Fatalf("重复关闭歌词重复释放播放源: %d", source.closed.Load())
	}
	engine.SetEnabled(true)
	engine.SetEnabled(false)
	if source.closed.Load() != 2 {
		t.Fatalf("重新启用后关闭没有再次释放播放源: %d", source.closed.Load())
	}
}

type retryProvider struct {
	calls int
}

type resolvedIDProvider struct {
	calls int
}

type timelineProvider struct{}

type blockingProvider struct {
	started  chan struct{}
	canceled chan struct{}
}

func (*blockingProvider) ID() string { return "local" }

func (provider *blockingProvider) Fetch(ctx context.Context, _ NowPlaying) (Document, error) {
	close(provider.started)
	<-ctx.Done()
	close(provider.canceled)
	return Document{}, ctx.Err()
}

func (*retryProvider) ID() string { return "local" }

func (provider *retryProvider) Fetch(context.Context, NowPlaying) (Document, error) {
	provider.calls++
	if provider.calls == 1 {
		return Document{}, errors.New("临时歌词错误")
	}
	return Document{Lines: []Line{{At: 0, Text: "测试歌词"}}}, nil
}

func explicitLocalTestResolver(provider Provider) *Resolver {
	resolver := NewResolver("", provider)
	resolver.SetPreferredProvider("local")
	return resolver
}

func (*resolvedIDProvider) ID() string { return "qq" }

func (*timelineProvider) ID() string { return "local" }

func (*timelineProvider) Fetch(context.Context, NowPlaying) (Document, error) {
	return Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 5 * time.Second, Text: "第二句"},
		{At: 10 * time.Second, Text: "第三句"},
	}}, nil
}

func (provider *resolvedIDProvider) Fetch(context.Context, NowPlaying) (Document, error) {
	provider.calls++
	return Document{
		Lines:    []Line{{At: 0, Text: "测试歌词"}},
		Metadata: map[string]string{"platform": "qq", "track_id": "0039MnYb0qxYhV"},
	}, nil
}

func TestEngineDoesNotCarrySourcePlatformIDIntoPollWithoutNativeID(t *testing.T) {
	provider := &resolvedIDProvider{}
	track := NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "晴天", Artist: "周杰伦", Album: "叶惠美",
		Duration: int64(269 * time.Second), Playing: true,
	}
	withoutID := track
	withoutID.TrackID = ""
	withoutID.TrackIDOrigin = ""
	engine := NewEngine(&sequenceNowPlayingSource{tracks: []NowPlaying{track, withoutID}}, NewResolver("", provider))

	engine.refresh(context.Background())
	if got := engine.Status().Track.TrackID; got != "0039MnYb0qxYhV" {
		t.Fatalf("首次轮询没有保存 QQ songmid: %q", got)
	}
	engine.refresh(context.Background())
	status := engine.Status()
	if status.Track.TrackID != "" || status.Track.SourceTrackID() != "" {
		t.Fatalf("当前轮询没有原生 ID 时仍沿用了上一轮身份: %+v", status.Track)
	}
	if provider.calls != 1 {
		t.Fatalf("缺少原生 ID 时仍进入了平台提供方: %d", provider.calls)
	}
	if status.HasLyrics || engine.Snapshot().Current != "" ||
		!strings.Contains(status.LastError, "缺少来源原生 ID") {
		t.Fatalf("缺少原生 ID 时没有清空联网歌词并失败关闭: status=%+v state=%+v", status, engine.Snapshot())
	}
}

func TestEngineClearsOnlineLyricsWhenCurrentIDLosesSourceEvidence(t *testing.T) {
	provider := &resolvedIDProvider{}
	track := NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "晴天", Artist: "周杰伦", Album: "叶惠美",
		Duration: int64(269 * time.Second), Playing: true,
	}
	unverified := track
	unverified.TrackIDOrigin = TrackIDOriginInferred
	engine := NewEngine(&sequenceNowPlayingSource{tracks: []NowPlaying{track, unverified}}, NewResolver("", provider))

	engine.refresh(context.Background())
	engine.refresh(context.Background())
	status := engine.Status()
	if status.Track.SourceTrackID() != "" || status.HasLyrics || engine.Snapshot().Current != "" ||
		!strings.Contains(status.LastError, "缺少来源原生 ID") {
		t.Fatalf("原生 ID 的来源证据丢失后仍保留联网歌词: status=%+v state=%+v", status, engine.Snapshot())
	}
	if provider.calls != 1 {
		t.Fatalf("未验证 ID 仍进入了平台提供方: %d", provider.calls)
	}
}

func TestEngineKeepsResolveErrorAndRetriesSameTrack(t *testing.T) {
	provider := &retryProvider{}
	track := NowPlaying{Title: "测试歌曲", Artist: "测试歌手", Playing: true}
	engine := NewEngine(fixedNowPlayingSource{track: track}, explicitLocalTestResolver(provider))
	engine.retryDelay = time.Hour
	engine.refresh(context.Background())
	first := engine.Status()
	if first.LastError == "" || first.HasLyrics {
		t.Fatalf("第一次失败状态无效: %+v", first)
	}

	engine.refresh(context.Background())
	second := engine.Status()
	if second.LastError != first.LastError || provider.calls != 1 {
		t.Fatalf("退避期间错误未保留: status=%+v calls=%d", second, provider.calls)
	}

	engine.retryDelay = 0
	engine.refresh(context.Background())
	third := engine.Status()
	if third.LastError != "" || !third.HasLyrics || provider.calls != 2 {
		t.Fatalf("同曲重试没有恢复: status=%+v calls=%d", third, provider.calls)
	}
}

func TestSnapshotClockFreezesOnPauseAndContinuesAfterResume(t *testing.T) {
	engine := NewEngine(nil, nil)
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.Local)
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 5 * time.Second, Text: "第二句"},
		{At: 10 * time.Second, Text: "第三句"},
	}}
	engine.status.Track = NowPlaying{Playing: true, PositionKnown: true}
	engine.anchorPosition = 4 * time.Second
	engine.anchorTime = base

	playing := engine.SnapshotAt(base.Add(2 * time.Second))
	if playing.Current != "第二句" || playing.Next != "第三句" ||
		playing.CurrentAt != 5*time.Second || playing.NextAt != 10*time.Second ||
		playing.Position != 6*time.Second || !playing.Playing {
		t.Fatalf("播放时歌词时间轴没有推进: %+v", playing)
	}

	// 收到暂停包时，主机把当前位置重新锚定为 6 秒。无论之后经过多久，
	// SnapshotAt 都必须保持在同一句，直到收到恢复播放的状态。
	engine.status.Track.Playing = false
	engine.anchorPosition = 6 * time.Second
	engine.anchorTime = base.Add(2 * time.Second)
	paused := engine.SnapshotAt(base.Add(time.Minute))
	if paused.Current != "第二句" || paused.Next != "第三句" || paused.CurrentAt != 5*time.Second ||
		paused.Position != 6*time.Second || paused.Playing {
		t.Fatalf("暂停后歌词仍然滚动: %+v", paused)
	}

	engine.status.Track.Playing = true
	engine.anchorPosition = 6 * time.Second
	engine.anchorTime = base.Add(time.Minute)
	resumed := engine.SnapshotAt(base.Add(time.Minute + 5*time.Second))
	if resumed.Current != "第三句" || resumed.CurrentAt != 10*time.Second ||
		resumed.Position != 11*time.Second || !resumed.Playing {
		t.Fatalf("恢复播放后歌词时钟没有继续: %+v", resumed)
	}
}

func TestPlaybackPositionProjectsFromSourceLastUpdatedTime(t *testing.T) {
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	track := NowPlaying{
		Position: int64(10 * time.Second), Duration: int64(30 * time.Second),
		PositionKnown: true, Playing: true, PositionUpdatedAt: base,
	}
	if got := playbackPositionAt(track, base.Add(2500*time.Millisecond)); got != 12500*time.Millisecond {
		t.Fatalf("没有从 SMTC LastUpdatedTime 投影当前位置: %s", got)
	}
	track.Playing = false
	if got := playbackPositionAt(track, base.Add(2500*time.Millisecond)); got != 10*time.Second {
		t.Fatalf("暂停状态错误投影了旧位置: %s", got)
	}
	track.Playing = true
	track.Position = int64(29 * time.Second)
	if got := playbackPositionAt(track, base.Add(5*time.Second)); got != 30*time.Second {
		t.Fatalf("投影位置没有按歌曲时长钳制: %s", got)
	}
	track.PositionUpdatedAt = base.Add(time.Second)
	track.Position = -1
	if got := playbackPositionAt(track, base); got != 0 {
		t.Fatalf("负位置或未来更新时间没有失败关闭: %s", got)
	}
}

func TestSnapshotFailsClosedWhenPositionUnknown(t *testing.T) {
	base := time.Now()
	engine := NewEngine(nil, nil)
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 5 * time.Second, Text: "第二句"},
	}}
	engine.anchorPosition = 6 * time.Second
	engine.anchorTime = base
	engine.status.Track = NowPlaying{Playing: true, PositionKnown: false}
	state := engine.SnapshotAt(base.Add(time.Minute))
	if state.Current != "" || state.Next != "" || state.Position != 6*time.Second || state.PositionKnown {
		t.Fatalf("未知位置仍猜测并滚动歌词: %+v", state)
	}
}

func TestEngineFailsClosedAcrossKnownUnknownKnownPosition(t *testing.T) {
	known := NowPlaying{
		Title: "位置证据测试", Artist: "测试歌手", Playing: true,
		Position: int64(6 * time.Second), PositionKnown: true,
	}
	unknown := known
	unknown.Position = 0
	unknown.PositionKnown = false
	recovered := known
	recovered.Position = int64(11 * time.Second)
	engine := NewEngine(
		&sequenceNowPlayingSource{tracks: []NowPlaying{known, unknown, recovered}},
		explicitLocalTestResolver(&timelineProvider{}),
	)

	engine.refresh(context.Background())
	if state := engine.Snapshot(); state.Current != "第二句" || !state.PositionKnown {
		t.Fatalf("首次已知位置显示错误: %+v", state)
	}
	engine.refresh(context.Background())
	if state := engine.Snapshot(); state.Current != "" || state.Next != "" || state.PositionKnown ||
		state.Position < 6*time.Second || state.Position >= 7*time.Second {
		t.Fatalf("位置证据丢失后没有在最后可信锚点失败关闭: %+v", state)
	}
	engine.refresh(context.Background())
	if state := engine.Snapshot(); state.Current != "第三句" || !state.PositionKnown {
		t.Fatalf("位置证据恢复后没有重新同步: %+v", state)
	}
}

func TestSyncSnapshotReturnsMatchingTrackAndLyricState(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	engine := &Engine{
		status: EngineStatus{Track: NowPlaying{
			Source: "netease", TrackID: "186016", TrackIDOrigin: TrackIDOriginSource,
			Title: "测试歌曲", PositionKnown: true, Playing: false,
		}},
		document:       Document{Lines: []Line{{At: 0, Text: "第一句"}, {At: 5 * time.Second, Text: "第二句"}}},
		anchorPosition: 2 * time.Second,
		anchorTime:     base,
	}
	state, status := engine.SyncSnapshot()
	if state.Current != "第一句" || state.Position != 2*time.Second || state.Playing ||
		status.Track.SourceTrackID() != "186016" || status.Track.Title != "测试歌曲" {
		t.Fatalf("原子歌词快照不匹配: state=%+v status=%+v", state, status)
	}
}

func TestEngineHotEnableStopsPolling(t *testing.T) {
	source := &countingNowPlayingSource{}
	engine := NewEngine(source, NewResolver(""))
	engine.interval = 5 * time.Millisecond
	engine.SetEnabled(false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	time.Sleep(15 * time.Millisecond)
	if calls := source.calls.Load(); calls != 0 {
		t.Fatalf("歌词关闭时仍在轮询: %d", calls)
	}

	engine.SetEnabled(true)
	deadline := time.Now().Add(time.Second)
	for source.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if source.calls.Load() == 0 {
		t.Fatal("歌词启用后没有立即查询媒体会话")
	}

	engine.SetEnabled(false)
	time.Sleep(10 * time.Millisecond)
	calls := source.calls.Load()
	time.Sleep(20 * time.Millisecond)
	if source.calls.Load() != calls {
		t.Fatalf("歌词关闭后仍在轮询: before=%d after=%d", calls, source.calls.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("歌词引擎没有退出")
	}
}

func TestDisableCancelsInFlightLyricsRequest(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	track := NowPlaying{Title: "测试歌曲", Artist: "测试歌手", Playing: true}
	engine := NewEngine(fixedNowPlayingSource{track: track}, explicitLocalTestResolver(provider))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("歌词请求没有启动")
	}
	engine.SetEnabled(false)
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("关闭歌词后没有取消在线请求")
	}
	deadline := time.Now().Add(time.Second)
	for engine.Status().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := engine.Status()
	if status.Running || status.Track.Valid() || status.LastError != "" {
		t.Fatalf("关闭歌词后的状态无效: %+v", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("歌词引擎没有退出")
	}
}

func TestSlowLyricsRequestDoesNotBlockTrackChangeOrPause(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	track := NowPlaying{
		Title: "新歌", Artist: "歌手", Album: "专辑", Duration: int64(3 * time.Minute),
		Position: int64(2 * time.Second), PositionKnown: true, Playing: true,
	}
	source := &mutableNowPlayingSource{track: track}
	engine := NewEngine(source, explicitLocalTestResolver(provider))
	engine.interval = 5 * time.Millisecond
	engine.trackKey = (NowPlaying{Title: "旧歌", Artist: "旧歌手"}).key()
	engine.document = Document{Lines: []Line{{At: 0, Text: "旧歌词"}}}
	engine.status = EngineStatus{
		Running: true, HasLyrics: true, LyricMode: "timed", Scrollable: true,
		Track: NowPlaying{Title: "旧歌", Artist: "旧歌手", Playing: true, PositionKnown: true},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("新歌取词请求没有启动")
	}
	if status := engine.Status(); status.Track.Title != "新歌" || status.HasLyrics {
		cancel()
		t.Fatalf("慢请求期间没有立即发布新歌状态: %+v", status)
	}
	if snapshot := engine.Snapshot(); snapshot.Current != "" {
		cancel()
		t.Fatalf("慢请求期间仍显示上一首歌词: %+v", snapshot)
	}
	// 消费切歌通知；暂停即使没有改变文字，也必须发送一次 playing=false
	// 小状态包，供协议消费者冻结歌词时间轴。
	select {
	case <-engine.Updates():
	default:
	}

	paused := track
	paused.Playing = false
	source.Set(paused, nil)
	deadline := time.Now().Add(time.Second)
	for engine.Status().Track.Playing && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-engine.Updates():
	case <-time.After(time.Second):
		cancel()
		t.Fatal("暂停状态没有唤醒歌词小包分发")
	}
	if state, status := engine.SyncSnapshot(); status.Track.Playing || state.Playing || state.Current != "" {
		cancel()
		t.Fatalf("取词未返回时暂停没有立即冻结屏端状态: state=%+v status=%+v", state, status)
	}

	cancel()
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("退出时没有取消慢取词请求")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("歌词引擎没有退出")
	}
}

func TestPlaybackSourceErrorFreezesExistingLyricClock(t *testing.T) {
	engine := NewEngine(&mutableNowPlayingSource{err: errors.New("媒体会话暂时不可用")}, NewResolver(""))
	base := time.Now().Add(-2 * time.Second)
	engine.document = Document{Lines: []Line{{At: 0, Text: "第一句"}, {At: 5 * time.Second, Text: "第二句"}}}
	engine.status = EngineStatus{Running: true, HasLyrics: true, Track: NowPlaying{
		Title: "歌曲", PositionKnown: true, Playing: true,
	}}
	engine.anchorPosition = 2 * time.Second
	engine.anchorTime = base
	engine.refresh(context.Background())

	first, status := engine.SyncSnapshot()
	if status.Track.Playing || first.Playing || status.LastError == "" {
		t.Fatalf("媒体会话错误后没有冻结歌词时钟: state=%+v status=%+v", first, status)
	}
	time.Sleep(10 * time.Millisecond)
	second := engine.Snapshot()
	if second.Position != first.Position || second.Current != first.Current {
		t.Fatalf("媒体会话错误后旧歌词仍在滚动: first=%+v second=%+v", first, second)
	}
}

func TestTimedLyricLineChangeNotifiesWithoutTelemetryTick(t *testing.T) {
	previous := NowPlaying{
		Source: "netease", TrackID: "186016", TrackIDOrigin: TrackIDOriginSource,
		Title: "测试歌曲", Artist: "测试歌手", Position: int64(4 * time.Second),
		PositionKnown: true, Playing: true,
	}
	current := previous
	current.Position = int64(6 * time.Second)
	engine := NewEngine(fixedNowPlayingSource{track: current}, NewResolver(""))
	base := time.Now()
	engine.mu.Lock()
	engine.trackKey = previous.key()
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 5 * time.Second, Text: "第二句"},
		{At: 10 * time.Second, Text: "第三句"},
	}}
	engine.status = EngineStatus{
		Running: true, HasLyrics: true, LyricMode: "timed", Scrollable: true, Track: previous,
	}
	engine.anchorPosition = 4 * time.Second
	engine.anchorTime = base
	engine.markDeliveryChangedLocked(base)
	engine.mu.Unlock()

	engine.poll(context.Background(), context.Background())
	select {
	case <-engine.Updates():
	case <-time.After(time.Second):
		t.Fatal("歌词切句没有独立唤醒分发服务")
	}
	state := engine.Snapshot()
	if state.Current != "第二句" || state.Next != "第三句" || state.CurrentAt != 5*time.Second {
		t.Fatalf("切句后的歌词状态错误: %+v", state)
	}
}

func TestDeliveryClockWakesAtSubsecondLineWithoutSMTCPoll(t *testing.T) {
	engine := NewEngine(nil, nil)
	startedAt := time.Now()
	engine.mu.Lock()
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 80 * time.Millisecond, Text: "第二句"},
		{At: 160 * time.Millisecond, Text: "第三句"},
	}}
	engine.status = EngineStatus{
		Running: true, HasLyrics: true, LyricMode: "timed", Scrollable: true,
		Track: NowPlaying{Title: "短句歌曲", PositionKnown: true, Playing: true},
	}
	engine.anchorTime = startedAt
	engine.markDeliveryChangedLocked(startedAt)
	engine.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.runDeliveryClock(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-engine.Updates():
		elapsed := time.Since(startedAt)
		if elapsed >= 500*time.Millisecond {
			t.Fatalf("80ms 切句仍表现为一秒轮询: elapsed=%s", elapsed)
		}
		state := engine.Snapshot()
		if state.Current != "第二句" || state.Next != "第三句" || state.CurrentAt != 80*time.Millisecond {
			t.Fatalf("本地截止时间唤醒后的歌词错误: %+v", state)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("短于一秒的歌词切句没有由本地截止时间唤醒")
	}
}

func TestDeliveryClockStopsOnPauseAndResumesFromFrozenAnchor(t *testing.T) {
	engine := NewEngine(nil, nil)
	startedAt := time.Now()
	engine.mu.Lock()
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 120 * time.Millisecond, Text: "第二句"},
	}}
	engine.status = EngineStatus{
		Running: true, HasLyrics: true, LyricMode: "timed", Scrollable: true,
		Track: NowPlaying{Title: "暂停测试", PositionKnown: true, Playing: true},
	}
	engine.anchorTime = startedAt
	engine.markDeliveryChangedLocked(startedAt)
	engine.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.runDeliveryClock(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(30 * time.Millisecond)
	pausedAt := time.Now()
	engine.mu.Lock()
	engine.anchorPosition += pausedAt.Sub(engine.anchorTime)
	engine.anchorTime = pausedAt
	engine.status.Track.Playing = false
	engine.markDeliveryChangedLocked(pausedAt)
	engine.mu.Unlock()
	engine.rescheduleDelivery()
	select {
	case <-engine.Updates():
		t.Fatal("暂停后仍在下一句截止时间产生切句通知")
	case <-time.After(140 * time.Millisecond):
	}

	resumedAt := time.Now()
	engine.mu.Lock()
	engine.anchorTime = resumedAt
	engine.status.Track.Playing = true
	engine.markDeliveryChangedLocked(resumedAt)
	engine.mu.Unlock()
	engine.rescheduleDelivery()
	select {
	case <-engine.Updates():
		state := engine.Snapshot()
		if state.Current != "第二句" {
			t.Fatalf("恢复后没有从冻结位置继续: %+v", state)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("恢复后没有重新安排下一句截止时间")
	}
}

func TestDeliveryClockDoesNotRunForPlainPausedOrUnknownPosition(t *testing.T) {
	base := time.Now()
	tests := []struct {
		name     string
		document Document
		track    NowPlaying
	}{
		{name: "纯文本", document: Document{PlainText: "整首歌词"}, track: NowPlaying{Playing: true, PositionKnown: true}},
		{name: "暂停", document: Document{Lines: []Line{{At: 0, Text: "第一句"}, {At: time.Second, Text: "第二句"}}}, track: NowPlaying{Playing: false, PositionKnown: true}},
		{name: "未知位置", document: Document{Lines: []Line{{At: 0, Text: "第一句"}, {At: time.Second, Text: "第二句"}}}, track: NowPlaying{Playing: true, PositionKnown: false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := NewEngine(nil, nil)
			engine.document = test.document
			engine.status.Track = test.track
			engine.anchorTime = base
			if delay, ok := engine.nextDeliveryDelay(base); ok || delay != 0 {
				t.Fatalf("不应启动歌词截止时间计时器: delay=%s ok=%v", delay, ok)
			}
		})
	}
}

func TestDeliveryClockCoalescesDuplicateTimestamps(t *testing.T) {
	engine := NewEngine(nil, nil)
	startedAt := time.Now()
	engine.mu.Lock()
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 80 * time.Millisecond, Text: "同刻第一行"},
		{At: 80 * time.Millisecond, Text: "同刻最终行"},
		{At: 160 * time.Millisecond, Text: "下一时间戳"},
	}}
	engine.status.Track = NowPlaying{Title: "重复时间戳", PositionKnown: true, Playing: true}
	engine.anchorTime = startedAt
	engine.markDeliveryChangedLocked(startedAt)
	engine.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.runDeliveryClock(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-engine.Updates():
		if state := engine.Snapshot(); state.Current != "同刻最终行" || state.Next != "下一时间戳" {
			t.Fatalf("相同时间戳没有合并到最终显示行: %+v", state)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("相同时间戳的切句没有唤醒")
	}
	select {
	case <-engine.Updates():
		t.Fatal("相同时间戳错误产生了第二个即时事件")
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-engine.Updates():
		if state := engine.Snapshot(); state.Current != "下一时间戳" {
			t.Fatalf("后续时间戳显示错误: %+v", state)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("重复时间戳之后的下一句没有按期唤醒")
	}
}

func TestDeliveryClockReschedulesOnSeekAndTrackSwitch(t *testing.T) {
	engine := NewEngine(nil, nil)
	startedAt := time.Now()
	engine.mu.Lock()
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "第一句"},
		{At: 500 * time.Millisecond, Text: "第二句"},
		{At: time.Second, Text: "第三句"},
	}}
	engine.status.Track = NowPlaying{Title: "拖动测试", PositionKnown: true, Playing: true}
	engine.anchorTime = startedAt
	engine.markDeliveryChangedLocked(startedAt)
	engine.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.runDeliveryClock(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// 向前拖到 900ms，下一截止点必须改为约 100ms，而不是原来的 500ms。
	time.Sleep(30 * time.Millisecond)
	seekAt := time.Now()
	engine.mu.Lock()
	engine.anchorPosition = 900 * time.Millisecond
	engine.anchorTime = seekAt
	changed := engine.markDeliveryChangedLocked(seekAt)
	engine.mu.Unlock()
	if changed {
		engine.notifyUpdate()
	}
	engine.rescheduleDelivery()
	select {
	case <-engine.Updates():
	case <-time.After(time.Second):
		t.Fatal("向前拖动后的当前句事件缺失")
	}
	select {
	case <-engine.Updates():
		if state := engine.Snapshot(); state.Current != "第三句" {
			t.Fatalf("向前拖动后的截止时间错误: %+v", state)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("向前拖动没有重排到更早的下一句截止点")
	}

	// 再向后拖动，让旧歌曲重新武装一个约 400ms 的截止点。
	backwardAt := time.Now()
	engine.mu.Lock()
	engine.anchorPosition = 100 * time.Millisecond
	engine.anchorTime = backwardAt
	changed = engine.markDeliveryChangedLocked(backwardAt)
	engine.mu.Unlock()
	if changed {
		engine.notifyUpdate()
	}
	engine.rescheduleDelivery()
	select {
	case <-engine.Updates():
	case <-time.After(time.Second):
		t.Fatal("向后拖动后的当前句事件缺失")
	}

	// 在旧截止点触发前切歌；新歌下一句在 800ms，前 550ms 内不应有
	// 任何事件，否则说明旧歌曲约 400ms 的计时器没有被取消。
	time.Sleep(30 * time.Millisecond)
	switchAt := time.Now()
	engine.mu.Lock()
	engine.document = Document{Lines: []Line{
		{At: 0, Text: "新歌第一句"},
		{At: 800 * time.Millisecond, Text: "新歌第二句"},
	}}
	engine.status.Track.Title = "切换后的歌曲"
	engine.anchorPosition = 0
	engine.anchorTime = switchAt
	changed = engine.markDeliveryChangedLocked(switchAt)
	engine.mu.Unlock()
	if changed {
		engine.notifyUpdate()
	}
	engine.rescheduleDelivery()
	select {
	case <-engine.Updates():
	case <-time.After(time.Second):
		t.Fatal("切歌后的立即显示事件缺失")
	}
	select {
	case <-engine.Updates():
		t.Fatal("切歌后旧歌曲的截止时间仍然触发")
	case <-time.After(550 * time.Millisecond):
	}
}
