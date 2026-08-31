package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/lyrics"
	waveshareprotocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/render"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type failingCollector struct{ count int }

type blockingMetricsCollector struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (collector *blockingMetricsCollector) Capture() (telemetry.Snapshot, error) {
	collector.once.Do(func() { close(collector.started) })
	<-collector.release
	return telemetry.Snapshot{CapturedAt: time.Now(), CPUPercent: 50}, nil
}

type eventLyrics struct {
	state   render.LyricState
	updates chan struct{}
}

func (provider *eventLyrics) Snapshot() render.LyricState { return provider.state }
func (provider *eventLyrics) Updates() <-chan struct{}    { return provider.updates }

type lyricEventDriver struct {
	sends chan telemetry.Snapshot
}

type lyricProgressDriver struct {
	mu        sync.Mutex
	snapshots []telemetry.Snapshot
}

// lyricProgressSpectrumDriver 模拟同时显示频谱与歌词的微雪屏。
// 它只用来证明歌词保持独立 20Hz 小包，不再向频谱包让出节拍。
type lyricProgressSpectrumDriver struct {
	lyricProgressDriver
}

func (*lyricProgressSpectrumDriver) SendSpectrumValues(context.Context, map[string]string) error {
	return nil
}

func (*lyricProgressDriver) Info() device.Info {
	return device.Info{ID: "lyric-progress", Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true}}
}
func (*lyricProgressDriver) Connect(context.Context) error                    { return nil }
func (*lyricProgressDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*lyricProgressDriver) Close() error                                     { return nil }
func (driver *lyricProgressDriver) SendTelemetry(_ context.Context, snapshot telemetry.Snapshot) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.snapshots = append(driver.snapshots, snapshot)
	return nil
}
func (driver *lyricProgressDriver) values() []map[string]string {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	result := make([]map[string]string, len(driver.snapshots))
	for index, snapshot := range driver.snapshots {
		result[index] = snapshot.Values()
	}
	return result
}

func (*lyricEventDriver) Info() device.Info {
	return device.Info{ID: "lyric-event", Capabilities: device.Capabilities{SmallTelemetry: true}}
}
func (*lyricEventDriver) Connect(context.Context) error                    { return nil }
func (*lyricEventDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*lyricEventDriver) Close() error                                     { return nil }
func (driver *lyricEventDriver) SendTelemetry(ctx context.Context, snapshot telemetry.Snapshot) error {
	select {
	case driver.sends <- snapshot:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type identifiedLyrics struct{}

func (identifiedLyrics) Snapshot() render.LyricState {
	return render.LyricState{
		Current: "第一句", Next: "第二句",
		CurrentAt: 40 * time.Second, NextAt: 45 * time.Second,
		Position: 42 * time.Second, PositionKnown: true, Playing: true,
	}
}

func (identifiedLyrics) Status() lyrics.EngineStatus {
	return lyrics.EngineStatus{
		Running: true, HasLyrics: true, Scrollable: true, LyricMode: "timed",
		Track: lyrics.NowPlaying{
			Source: "netease", TrackID: "186016", Title: "晴天", Artist: "周杰伦",
			Position: int64(42 * time.Second), PositionKnown: true, Playing: true,
		},
	}
}

func (collector *failingCollector) Capture() (telemetry.Snapshot, error) {
	collector.count++
	return telemetry.Snapshot{}, errors.New("模拟采集失败")
}

func TestLyricEventBypassesSlowMetricsAndSendsCompactVisibilityState(t *testing.T) {
	collector := &blockingMetricsCollector{started: make(chan struct{}), release: make(chan struct{})}
	driver := &lyricEventDriver{sends: make(chan telemetry.Snapshot, 4)}
	provider := &eventLyrics{
		state:   render.LyricState{Current: "第一句", Next: "第二句"},
		updates: make(chan struct{}, 1),
	}
	controller := New(collector, 5*time.Millisecond)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(provider)
	controller.deviceThemes["lyric-event"] = theme.Manifest{Layers: []theme.Layer{
		{ID: "metrics", Type: theme.LayerTelemetry, Opacity: 1},
		{ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	defer func() {
		select {
		case <-collector.release:
		default:
			close(collector.release)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("服务退出失败: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("服务没有退出")
		}
	}()

	// SetLyricsProvider 的首次唤醒先发送初始双行文本。
	select {
	case <-driver.sends:
	case <-time.After(time.Second):
		t.Fatal("没有发送初始歌词事件")
	}
	select {
	case <-collector.started:
	case <-time.After(time.Second):
		t.Fatal("普通硬件采集没有进入阻塞测试点")
	}

	provider.state = render.LyricState{
		Current: "第二句", Next: "第三句", CurrentAt: 5 * time.Second, NextAt: 10 * time.Second,
		PositionKnown: true, Playing: true,
	}
	provider.updates <- struct{}{}
	select {
	case snapshot := <-driver.sends:
		values := snapshot.Values()
		if len(values) != 6 || values["CaseScreen_LyricsCurrent"] != "第二句" ||
			values["CaseScreen_LyricsNext"] != "第三句" ||
			values["CaseScreen_LyricsCurrentAtMs"] != "5000" ||
			values["CaseScreen_LyricsNextAtMs"] != "10000" ||
			values["CaseScreen_LyricsPlaying"] != "true" || values[telemetry.LyricsVisibleKey] != "1" {
			t.Fatalf("歌词事件不是紧凑状态小包: %#v", values)
		}
		if !snapshot.OmitHardwareMetrics || !snapshot.OmitClock {
			t.Fatalf("歌词事件意外附带普通遥测: %+v", snapshot)
		}
		payloadBytes := len(waveshareprotocol.EncodeQStringMap(values))
		t.Logf("普通双行歌词紧凑 QStringMap=%d bytes", payloadBytes)
		if payloadBytes > 512 {
			t.Fatalf("普通双行歌词状态包不够小: %d bytes", payloadBytes)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("歌词事件被慢硬件采集阻塞")
	}
}

func TestWaveshareLyricProgressUsesFineKeyKeepsLegacyAndStopsWhenPaused(t *testing.T) {
	driver := &lyricProgressDriver{}
	provider := &eventLyrics{state: render.LyricState{
		Current: "第一句", Next: "第二句", Progress: .42,
		CurrentAt: 0, NextAt: 10 * time.Second, PositionKnown: true, Playing: true,
	}}
	controller := New(&fakeCollector{}, 60*time.Second)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(provider)
	manifest := theme.Manifest{Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1,
		Options: map[string]any{"style": "player", "show_progress": true},
	}}}
	controller.deviceThemes["lyric-progress"] = manifest

	controller.sendLyricsEvent(context.Background())
	values := driver.values()
	fine42, _ := lyricProgressForManifest(manifest, provider.state, true)
	if len(values) != 1 || values[0][telemetry.LyricsProgressLegacyKey] != "42" ||
		values[0][telemetry.LyricsProgressFineKey] != strconv.Itoa(fine42) {
		t.Fatalf("首次换句包没有携带新旧进度: %#v", values)
	}
	controller.sendLyricsProgress(context.Background())
	if values = driver.values(); len(values) != 1 {
		t.Fatalf("相同可见进度仍被重复发送: %#v", values)
	}

	// 同一整百分比内跨过一个可见步进时只发送细进度键。
	provider.state.Progress = .421
	controller.sendLyricsProgress(context.Background())
	values = driver.values()
	if len(values) != 2 || len(values[1]) != 1 || values[1][telemetry.LyricsProgressFineKey] != "4210" {
		t.Fatalf("同一整百分比内不是细进度单键小包: %#v", values)
	}
	payloadBytes := len(waveshareprotocol.EncodeQStringMap(values[1]))
	if payloadBytes > 128 {
		t.Fatalf("细进度单键包过大: %d bytes", payloadBytes)
	}

	// 跨整百分比时 Fine 与旧长键分包；旧键不会搭便车进入高频路径。
	provider.state.Progress = .43
	controller.sendLyricsProgress(context.Background())
	values = driver.values()
	if len(values) != 4 || len(values[2]) != 1 || values[2][telemetry.LyricsProgressFineKey] != "4300" ||
		len(values[3]) != 1 || values[3][telemetry.LyricsProgressLegacyKey] != "43" {
		t.Fatalf("跨整百分比没有拆分 Fine 与旧兼容键: %#v", values)
	}

	provider.state.Progress = .431
	provider.state.Playing = false
	controller.sendLyricsEvent(context.Background())
	values = driver.values()
	if len(values) != 5 || values[4]["CaseScreen_LyricsPlaying"] != "false" ||
		values[4][telemetry.LyricsProgressLegacyKey] != "43" || values[4][telemetry.LyricsProgressFineKey] != "4310" {
		t.Fatalf("暂停事件没有冻结最终歌词进度: %#v", values)
	}
	controller.sendLyricsProgress(context.Background())
	if values = driver.values(); len(values) != 5 {
		t.Fatalf("暂停后仍发送周期进度小包: %#v", values)
	}
}

func TestWaveshareLyricProgressUsesTwentyHertzCadence(t *testing.T) {
	if waveshareLyricProgressInterval != time.Second/20 {
		t.Fatalf("歌词进度节拍=%v，期望 20Hz", waveshareLyricProgressInterval)
	}
}

func TestWaveshareLyricProgressWithActiveSpectrumOnlySendsLegacyFallback(t *testing.T) {
	driver := &lyricProgressSpectrumDriver{}
	provider := &eventLyrics{state: render.LyricState{
		Current: "第一句", Next: "第二句", Progress: .42,
		PositionKnown: true, Playing: true,
	}}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(provider)
	controller.deviceThemes["lyric-progress"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "spectrum", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: .4}, Opacity: 1},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Y: .5, Width: 1, Height: .4}, Opacity: 1,
			Options: map[string]any{"style": "player", "show_progress": true}},
	}}

	controller.sendLyricsProgress(context.Background())
	values := driver.values()
	if len(values) != 1 || len(values[0]) != 1 || values[0][telemetry.LyricsProgressLegacyKey] != "42" {
		t.Fatalf("活跃频谱下首包没有仅发送旧兼容键: %#v", values)
	}
	controller.sendLyricsProgress(context.Background())
	if values = driver.values(); len(values) != 1 {
		t.Fatalf("相同可见进度在频谱间隔被重复发送: %#v", values)
	}
	provider.state.Progress = .4205
	controller.sendLyricsProgress(context.Background())
	if values = driver.values(); len(values) != 1 {
		t.Fatalf("同一整百分比内重复发送了独立 Fine: %#v", values)
	}

	provider.state.Progress = .44
	controller.sendLyricsProgress(context.Background())
	if values = driver.values(); len(values) != 2 || len(values[1]) != 1 ||
		values[1][telemetry.LyricsProgressLegacyKey] != "44" {
		t.Fatalf("跨整百分比没有独立发送旧兼容键: %#v", values)
	}
}

func TestWaveshareLyricProgressFallbackDeduplicatesByVisibleTrackStep(t *testing.T) {
	driver := &lyricProgressDriver{}
	provider := &eventLyrics{state: render.LyricState{
		Current: "第一句", Next: "第二句", Progress: .421,
		PositionKnown: true, Playing: true,
	}}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(provider)
	// 12 像素图层扣除两侧 padding 后只有 10 个可见步进。
	controller.deviceThemes["lyric-progress"] = theme.Manifest{Width: 12, Height: 40, Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		Options: map[string]any{"style": "player", "show_progress": true},
	}}}

	controller.sendLyricsProgress(context.Background())
	values := driver.values()
	if len(values) != 2 || len(values[0]) != 1 || values[0][telemetry.LyricsProgressFineKey] != "4000" ||
		len(values[1]) != 1 || values[1][telemetry.LyricsProgressLegacyKey] != "40" {
		t.Fatalf("窄轨道没有按可见步进量化: %#v", values)
	}
	provider.state.Progress = .429
	controller.sendLyricsProgress(context.Background())
	if values = driver.values(); len(values) != 2 {
		t.Fatalf("像素未变化仍重复发送进度: %#v", values)
	}
	provider.state.Progress = .46
	controller.sendLyricsProgress(context.Background())
	values = driver.values()
	if len(values) != 4 || len(values[2]) != 1 || values[2][telemetry.LyricsProgressFineKey] != "5000" ||
		len(values[3]) != 1 || values[3][telemetry.LyricsProgressLegacyKey] != "50" {
		t.Fatalf("跨过下一可见步进后没有发送: %#v", values)
	}
	provider.state.Playing = false
	provider.state.Progress = .8
	controller.sendLyricsProgress(context.Background())
	if values = driver.values(); len(values) != 4 {
		t.Fatalf("暂停后仍发送周期进度: %#v", values)
	}
}

func TestLyricDisplayLinesHideUnavailableStatesAndKeepPlainLyrics(t *testing.T) {
	testCases := []struct {
		name        string
		state       render.LyricState
		status      lyrics.EngineStatus
		timed       bool
		wantCurrent string
		wantNext    string
	}{
		{name: "等待播放器"},
		{name: "等待歌曲ID", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "晴天"}, LastError: "QQ 当前歌曲缺少来源原生 ID"}},
		{name: "无歌词", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "纯音乐"}, LastError: "该 ID 暂无歌词"}},
		{name: "QQ没有LRC歌词", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Source: "qq", TrackID: "003", Title: "纯音乐"}, LastError: "QQ 音乐歌曲没有 LRC 歌词"}},
		{name: "网易云没有LRC歌词", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Source: "netease", TrackID: "186016", Title: "纯音乐"}, LastError: "网易云歌曲没有 LRC 歌词"}},
		{name: "歌词内容为空", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Source: "qq", TrackID: "004", Title: "纯音乐"}, LastError: "歌词内容为空"}},
		{name: "没有匹配同步歌词", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "纯音乐"}, LastError: "没有与当前歌曲匹配的同步歌词"}},
		{name: "不可滚动", state: render.LyricState{Current: lyrics.UnsynchronizedLyricsMessage},
			status:      lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "测试歌"}, HasLyrics: true},
			wantCurrent: lyrics.UnsynchronizedLyricsMessage, wantNext: "请在原音乐播放器查看完整歌词"},
		{name: "需要登录或会员", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "受限歌曲"}, LastError: lyrics.ErrPlatformLoginRequired.Error()}},
		{name: "网络暂时异常", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "晴天"}, LastError: "请求歌词超时"}},
		{name: "等待播放器时间轴", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "晴天"}, HasLyrics: true, Scrollable: true}},
		{name: "取词中", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "晴天"}}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot := telemetry.Snapshot{}
			populateLyricPacket(&snapshot, testCase.state, testCase.status, testCase.timed)
			if snapshot.LyricsCurrent != testCase.wantCurrent || snapshot.LyricsNext != testCase.wantNext {
				t.Fatalf("歌词可见内容错误: current=%q next=%q", snapshot.LyricsCurrent, snapshot.LyricsNext)
			}
			if testCase.wantCurrent == "" {
				values := snapshot.Values()
				if !snapshot.LyricsClear || values[telemetry.LyricsVisibleKey] != "0" || values["CaseScreen_LyricsCurrent"] != "" || values["CaseScreen_LyricsNext"] != "" {
					t.Fatalf("不可用歌词没有隐藏整个驻留图层: snapshot=%+v values=%#v", snapshot, values)
				}
			} else if snapshot.LyricsClear || !snapshot.LyricsVisibleKnown || !snapshot.LyricsVisible {
				t.Fatalf("真实或纯文本歌词被错误隐藏: %+v", snapshot)
			}
		})
	}
}

func TestPausedLyricsRemainVisibleAndFreezeState(t *testing.T) {
	state := render.LyricState{
		Current: "暂停在这一句", Next: "下一句", Progress: .42,
		CurrentAt: 10 * time.Second, NextAt: 15 * time.Second, PositionKnown: true, Playing: false,
	}
	status := lyrics.EngineStatus{HasLyrics: true, Scrollable: true, Track: lyrics.NowPlaying{Title: "测试歌", PositionKnown: true, Playing: false}}
	snapshot := telemetry.Snapshot{}
	populateLyricPacket(&snapshot, state, status, true)
	values := snapshot.Values()
	if snapshot.LyricsClear || !snapshot.LyricsVisible || !snapshot.LyricsVisibleKnown ||
		values[telemetry.LyricsVisibleKey] != "1" || values["CaseScreen_LyricsCurrent"] != state.Current ||
		values["CaseScreen_LyricsPlaying"] != "false" {
		t.Fatalf("暂停歌词没有保持可见和冻结状态: snapshot=%+v values=%#v", snapshot, values)
	}
}

func TestWaveshareLyricUnavailableStatesResetPreviousProgress(t *testing.T) {
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Width: 1, Height: .4}, Opacity: 1,
		Options: map[string]any{"style": "player", "show_progress": true},
	}}}
	testCases := []struct {
		name   string
		state  render.LyricState
		status lyrics.EngineStatus
	}{
		{name: "取词中", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "晴天"}}},
		{name: "无歌词", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "纯音乐"}, LastError: "该 ID 暂无歌词"}},
		{name: "纯文本不可滚动", state: render.LyricState{Current: lyrics.UnsynchronizedLyricsMessage},
			status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "测试歌"}, HasLyrics: true}},
		{name: "时间轴丢失", status: lyrics.EngineStatus{Track: lyrics.NowPlaying{Title: "晴天"}, HasLyrics: true, Scrollable: true}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot := telemetry.Snapshot{}
			populateLyricPacket(&snapshot, testCase.state, testCase.status, testCase.status.Scrollable)
			if !applyLyricProgressForManifest(&snapshot, manifest, testCase.state, testCase.status.Scrollable) {
				t.Fatal("开启进度轨的主题没有生成状态归零键")
			}
			values := snapshot.Values()
			if !snapshot.LyricsProgressKnown || snapshot.LyricsProgress != 0 ||
				!snapshot.LyricsProgressFineKnown || snapshot.LyricsProgressFine != 0 ||
				values[telemetry.LyricsProgressLegacyKey] != "0" || values[telemetry.LyricsProgressFineKey] != "0" {
				t.Fatalf("无可滚动歌词时残留上一句进度: snapshot=%+v values=%#v", snapshot, values)
			}
		})
	}
}

func TestLyricsProgressDisabledOrMinimalSendsNoPeriodicPacket(t *testing.T) {
	for _, options := range []map[string]any{
		{"style": "player", "show_progress": false},
		{"style": "minimal", "show_progress": true},
	} {
		driver := &lyricProgressDriver{}
		provider := &eventLyrics{state: render.LyricState{Current: "当前句", Progress: .5, PositionKnown: true, Playing: true}}
		controller := New(&fakeCollector{}, time.Second)
		controller.AddDriver(driver)
		controller.SetLyricsProvider(provider)
		controller.deviceThemes["lyric-progress"] = theme.Manifest{Layers: []theme.Layer{{ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1, Options: options}}}
		controller.sendLyricsProgress(context.Background())
		if values := driver.values(); len(values) != 0 {
			t.Fatalf("不需要进度轨的主题仍发送周期小包: options=%v values=%#v", options, values)
		}
	}
}

func TestDisabledHardwareTelemetryStillSendsVisibleLyrics(t *testing.T) {
	collector := &fakeCollector{}
	driver := &fakeDriver{}
	controller := New(collector, time.Second)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(overlayLyrics{})
	controller.SetTelemetryEnabled(false)
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1,
		Options: map[string]any{"visible": true},
	}}}

	controller.tick(context.Background())
	if collector.count != 0 {
		t.Fatalf("关闭硬件遥测后仍调用采集器: %d", collector.count)
	}
	if driver.sends != 1 {
		t.Fatalf("关闭硬件遥测后可见歌词没有发送: %d", driver.sends)
	}
	if driver.lastSend.LyricsCurrent != "当前歌词" || driver.lastSend.LyricsNext != "下一句歌词" {
		t.Fatalf("歌词专用字段无效: %+v", driver.lastSend)
	}
	if driver.lastSend.CPUModel != "" || driver.lastSend.GPUModel != "" {
		t.Fatalf("歌词不应覆盖硬件型号: %+v", driver.lastSend)
	}
	if !driver.lastSend.OmitHardwareMetrics {
		t.Fatal("仅歌词包必须标记为不含硬件实测数据")
	}
	if _, exists := driver.lastSend.Values()["Cpu_Freq"]; exists {
		t.Fatal("仅歌词包不应编码 CPU=0 的伪遥测")
	}
	if _, exists := driver.lastSend.Values()["time"]; exists {
		t.Fatal("仅歌词包不应附带每秒变化的时钟字段")
	}
}

func TestLyricsLayerSendsOnlyBoundDisplayTextKeys(t *testing.T) {
	driver := &fakeDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(identifiedLyrics{})
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1,
	}}}

	controller.tick(context.Background())
	values := driver.lastSend.Values()
	if values["CaseScreen_LyricsCurrent"] != "第一句" || values["CaseScreen_LyricsNext"] != "第二句" {
		t.Fatalf("歌词显示文字小包错误: %#v", values)
	}
	for _, key := range []string{
		"source", "track_id", "position", "position_known", "playing", "lyric_mode",
		"CaseScreen_LyricsPositionMs",
	} {
		if _, exists := values[key]; exists {
			t.Errorf("原厂 Theme 未绑定的字段不应下发到屏端: %s=%q", key, values[key])
		}
	}
	if values["CaseScreen_LyricsCurrentAtMs"] != "40000" ||
		values["CaseScreen_LyricsNextAtMs"] != "45000" ||
		values["CaseScreen_LyricsPlaying"] != "true" {
		t.Fatalf("歌词协议时间戳/状态错误: %#v", values)
	}
}

func TestLyricsSmallPacketRejectsInferredIDAndBoundsText(t *testing.T) {
	driver := &fakeDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	longLine := strings.Repeat("歌", maximumSmallPacketLyricRunes+20)
	controller.SetLyricsProvider(fakeTimedLyrics{
		state: render.LyricState{
			Current: longLine, Next: longLine, CurrentAt: 0, NextAt: 3 * time.Second,
			Position: 0, PositionKnown: true, Playing: false,
		},
		status: lyrics.EngineStatus{
			Running: true, HasLyrics: true, Scrollable: true, LyricMode: "timed",
			Track: lyrics.NowPlaying{
				Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: lyrics.TrackIDOriginInferred,
				PositionKnown: true, Playing: false,
			},
		},
	})
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1,
	}}}

	controller.tick(context.Background())
	values := driver.lastSend.Values()
	if _, exists := values["track_id"]; exists {
		t.Fatalf("主机歌曲 ID 不应进入副屏小包: %#v", values)
	}
	if got := []rune(values["CaseScreen_LyricsCurrent"]); len(got) != maximumSmallPacketLyricRunes || got[len(got)-1] != '…' {
		t.Fatalf("歌词行没有按小包预算截断: runes=%d value=%q", len(got), values["CaseScreen_LyricsCurrent"])
	}
	if values["CaseScreen_LyricsCurrentAtMs"] != "0" ||
		values["CaseScreen_LyricsNextAtMs"] != "3000" || values["CaseScreen_LyricsPlaying"] != "false" {
		t.Fatalf("歌词五字段协议内容错误: %#v", values)
	}
	if payload := waveshareprotocol.EncodeQStringMap(values); len(payload) > 4<<10 {
		t.Fatalf("歌词与时钟包超过 4 KiB 预算: %d bytes", len(payload))
	}
}

type fakeTimedLyrics struct {
	state  render.LyricState
	status lyrics.EngineStatus
}

func (provider fakeTimedLyrics) Snapshot() render.LyricState { return provider.state }
func (provider fakeTimedLyrics) Status() lyrics.EngineStatus { return provider.status }

func TestCollectionFailureKeepsLyricsWithoutSendingZeroMetrics(t *testing.T) {
	collector := &failingCollector{}
	driver := &fakeDriver{}
	controller := New(collector, time.Second)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(overlayLyrics{})
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{
		{ID: "metrics", Type: theme.LayerTelemetry, Opacity: 1},
		{ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1},
	}}

	controller.tick(context.Background())
	if collector.count != 1 || driver.sends != 1 {
		t.Fatalf("采集失败时应保留歌词小包: capture=%d send=%d", collector.count, driver.sends)
	}
	if driver.lastSend.LyricsCurrent != "当前歌词" || !driver.lastSend.OmitHardwareMetrics {
		t.Fatalf("采集失败后的歌词包状态错误: %+v", driver.lastSend)
	}
	values := driver.lastSend.Values()
	for _, key := range []string{"Cpu_Freq", "Memory_Load", "Memory_Used", "Memory_Available", "RAM"} {
		if _, exists := values[key]; exists {
			t.Errorf("采集失败时不应下发 %s=%q", key, values[key])
		}
	}
}

func TestCollectionFailureSendsOnlyRealClockFields(t *testing.T) {
	collector := &failingCollector{}
	driver := &fakeDriver{}
	controller := New(collector, time.Second)
	controller.AddDriver(driver)
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{
		{ID: "metrics", Type: theme.LayerTelemetry, Opacity: 1},
	}}

	controller.tick(context.Background())
	if collector.count != 1 || driver.sends != 1 {
		t.Fatalf("采集失败时应保留真实时钟小包: capture=%d send=%d", collector.count, driver.sends)
	}
	values := driver.lastSend.Values()
	if !driver.lastSend.OmitHardwareMetrics || values["time"] == "" {
		t.Fatalf("采集失败时的小包状态错误: snapshot=%+v values=%#v", driver.lastSend, values)
	}
	for _, key := range []string{"Cpu_Freq", "Memory_Load", "Memory_Used", "Memory_Available", "RAM"} {
		if _, exists := values[key]; exists {
			t.Errorf("采集失败时不应下发 %s=%q", key, values[key])
		}
	}
}

func TestManifestNeedsSmallTelemetryRecognizesDynamicWidgets(t *testing.T) {
	tests := []struct {
		name  string
		layer theme.Layer
		want  bool
	}{
		{name: "遥测面板", layer: theme.Layer{Type: theme.LayerTelemetry, Opacity: 1}, want: true},
		{name: "时钟", layer: theme.Layer{Type: theme.LayerClock, Opacity: 1}, want: true},
		{name: "绑定数据的文本", layer: theme.Layer{Type: theme.LayerText, Opacity: 1, Options: map[string]any{"data_key": "Cpu_Freq"}}, want: true},
		{name: "静态文本", layer: theme.Layer{Type: theme.LayerText, Opacity: 1, Options: map[string]any{"data_key": "  "}}, want: false},
		{name: "进度条", layer: theme.Layer{Type: theme.LayerProgressBar, Opacity: 1}, want: true},
		{name: "仪表", layer: theme.Layer{Type: theme.LayerGauge, Opacity: 1}, want: true},
		{name: "渐变仪表", layer: theme.Layer{Type: theme.LayerGradientGauge, Opacity: 1}, want: true},
		{name: "折线图", layer: theme.Layer{Type: theme.LayerLineChart, Opacity: 1}, want: true},
		{name: "隐藏控件", layer: theme.Layer{Type: theme.LayerClock, Opacity: 1, Options: map[string]any{"visible": false}}, want: false},
		{name: "全透明控件", layer: theme.Layer{Type: theme.LayerClock, Opacity: 0, Options: map[string]any{"visible": true}}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := theme.Manifest{Layers: []theme.Layer{test.layer}}
			if got := manifestNeedsSmallTelemetry(manifest); got != test.want {
				t.Fatalf("动态小数据判定错误: got=%v want=%v layer=%+v", got, test.want, test.layer)
			}
		})
	}
}

func TestManifestHasLayerRequiresVisibleNonTransparentLayer(t *testing.T) {
	manifest := theme.Manifest{Layers: []theme.Layer{
		{Type: theme.LayerLyrics, Opacity: 1, Options: map[string]any{"visible": false}},
		{Type: theme.LayerAudioBars, Opacity: 0, Options: map[string]any{"visible": true}},
	}}
	if manifestHasLayer(manifest, theme.LayerLyrics) || manifestHasLayer(manifest, theme.LayerAudioBars) {
		t.Fatal("隐藏或全透明图层仍被当作运行中图层")
	}
}

func TestDisabledAudioAndLyricsClearsLyricsOnceThenStops(t *testing.T) {
	driver := &fakeDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(overlaySpectrum{})
	controller.SetAudioEnabled(false)
	controller.SetLyricsProvider(overlayLyrics{})
	controller.SetLyricsEnabled(false)
	// 设置页可能重复保存同一个关闭值；在清空包成功前不能撤销 pending。
	controller.SetLyricsEnabled(false)
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{
		{Type: theme.LayerAudioBars, Opacity: 1},
		{Type: theme.LayerLyrics, Opacity: 1},
	}}

	controller.tick(context.Background())
	if driver.sends != 1 {
		t.Fatalf("关闭歌词后必须发送一次显式清空包: %d", driver.sends)
	}
	if !driver.lastSend.LyricsClear {
		t.Fatalf("关闭歌词后发送的不是显式清空包: %+v", driver.lastSend)
	}
	values := driver.lastSend.Values()
	current, hasCurrent := values["CaseScreen_LyricsCurrent"]
	next, hasNext := values["CaseScreen_LyricsNext"]
	if !hasCurrent || current != "" || !hasNext || next != "" {
		t.Fatalf("显式清空包字段错误: %#v", values)
	}
	if values["CaseScreen_LyricsCurrentAtMs"] != "" || values["CaseScreen_LyricsNextAtMs"] != "" ||
		values["CaseScreen_LyricsPlaying"] != "false" {
		t.Fatalf("清空包没有复位歌词时间戳/播放状态: %#v", values)
	}
	controller.mu.RLock()
	pending := controller.lyricsClearPending
	controller.mu.RUnlock()
	if pending {
		t.Fatal("显式清空成功后 lyricsClearPending 没有复位")
	}

	controller.tick(context.Background())
	if driver.sends != 1 {
		t.Fatalf("清空成功后仍重复发送小数据包: %d", driver.sends)
	}
}

func TestFailedLyricClearRemainsPendingUntilRetrySucceeds(t *testing.T) {
	driver := &fakeDriver{sendErr: errors.New("模拟清空失败")}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(overlayLyrics{})
	controller.SetLyricsEnabled(false)
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{
		{Type: theme.LayerLyrics, Opacity: 1},
	}}

	controller.tick(context.Background())
	controller.mu.RLock()
	pending := controller.lyricsClearPending
	controller.mu.RUnlock()
	if !pending {
		t.Fatal("清空失败后不应撤销待重试状态")
	}

	driver.mu.Lock()
	driver.sendErr = nil
	driver.mu.Unlock()
	controller.tick(context.Background())
	controller.mu.RLock()
	pending = controller.lyricsClearPending
	controller.mu.RUnlock()
	if pending || driver.sends != 2 {
		t.Fatalf("清空重试成功后状态错误: pending=%v sends=%d", pending, driver.sends)
	}
}
