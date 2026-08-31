package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	"casescreen/internal/audio"
	"casescreen/internal/device"
	"casescreen/internal/lyrics"
	waveshareProtocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/render"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type fakeCollector struct{ count int }

func (collector *fakeCollector) Capture() (telemetry.Snapshot, error) {
	collector.count++
	return telemetry.Snapshot{CapturedAt: time.Unix(int64(collector.count), 0)}, nil
}

type fakeDriver struct {
	mu       sync.Mutex
	sends    int
	lastSend telemetry.Snapshot
	sendErr  error
	applyErr error
}

type reconnectDriver struct {
	mu       sync.Mutex
	connects int
	sends    int
}

type reconnectHostDriver struct {
	mu       sync.Mutex
	connects int
	frames   int
}

type boundedReconnectDriver struct {
	mu            sync.Mutex
	connects      int
	concurrent    int
	maxConcurrent int
}

func (*boundedReconnectDriver) Info() device.Info {
	return device.Info{ID: "bounded-reconnect", Capabilities: device.Capabilities{SmallTelemetry: true}}
}

func (driver *boundedReconnectDriver) Connect(ctx context.Context) error {
	driver.mu.Lock()
	driver.connects++
	call := driver.connects
	driver.concurrent++
	if driver.concurrent > driver.maxConcurrent {
		driver.maxConcurrent = driver.concurrent
	}
	driver.mu.Unlock()
	defer func() {
		driver.mu.Lock()
		driver.concurrent--
		driver.mu.Unlock()
	}()
	switch call {
	case 1:
		return errors.New("首次连接失败")
	case 2:
		<-ctx.Done()
		return ctx.Err()
	default:
		return nil
	}
}

func (*boundedReconnectDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*boundedReconnectDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (*boundedReconnectDriver) Close() error { return nil }

type connectionAwareDriver struct {
	connected bool
	sends     int
}

func (*connectionAwareDriver) Info() device.Info {
	return device.Info{ID: "connection-aware", Capabilities: device.Capabilities{SmallTelemetry: true}}
}
func (driver *connectionAwareDriver) Connect(context.Context) error {
	driver.connected = true
	return nil
}
func (*connectionAwareDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (driver *connectionAwareDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	driver.sends++
	return nil
}
func (*connectionAwareDriver) Close() error           { return nil }
func (driver *connectionAwareDriver) Connected() bool { return driver.connected }

type infoUpdatingDriver struct {
	connected bool
}

type themeDisconnectDriver struct {
	connected bool
	applyErr  error
}

type namedThemeDriver struct {
	id       string
	applyErr error
}

type orderedThemeDriver struct {
	mu         sync.Mutex
	applied    []string
	oldStarted chan struct{}
	releaseOld chan struct{}
	oldOnce    sync.Once
}

func (*orderedThemeDriver) Info() device.Info             { return device.Info{ID: "ordered-theme"} }
func (*orderedThemeDriver) Connect(context.Context) error { return nil }
func (driver *orderedThemeDriver) ApplyTheme(ctx context.Context, manifest theme.Manifest) error {
	driver.mu.Lock()
	driver.applied = append(driver.applied, manifest.ID)
	driver.mu.Unlock()
	if manifest.ID == "old-pending" {
		driver.oldOnce.Do(func() { close(driver.oldStarted) })
		select {
		case <-driver.releaseOld:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (*orderedThemeDriver) SendTelemetry(context.Context, telemetry.Snapshot) error { return nil }
func (*orderedThemeDriver) Close() error                                            { return nil }

type shutdownThemeDriver struct {
	mu               sync.Mutex
	active           bool
	closeDuringApply bool
	applyStarted     chan struct{}
	applyDone        chan struct{}
	startedOnce      sync.Once
	doneOnce         sync.Once
}

func (*shutdownThemeDriver) Info() device.Info             { return device.Info{ID: "shutdown-theme"} }
func (*shutdownThemeDriver) Connect(context.Context) error { return nil }
func (driver *shutdownThemeDriver) ApplyTheme(ctx context.Context, _ theme.Manifest) error {
	driver.mu.Lock()
	driver.active = true
	driver.mu.Unlock()
	driver.startedOnce.Do(func() { close(driver.applyStarted) })
	<-ctx.Done()
	driver.mu.Lock()
	driver.active = false
	driver.mu.Unlock()
	driver.doneOnce.Do(func() { close(driver.applyDone) })
	return ctx.Err()
}
func (*shutdownThemeDriver) SendTelemetry(context.Context, telemetry.Snapshot) error { return nil }
func (driver *shutdownThemeDriver) Close() error {
	driver.mu.Lock()
	driver.closeDuringApply = driver.active
	driver.mu.Unlock()
	return nil
}

func (driver *infoUpdatingDriver) Info() device.Info {
	resolution := [2]int{320, 240}
	if driver.connected {
		resolution = [2]int{1024, 600}
	}
	return device.Info{ID: "info-update", Resolution: resolution}
}
func (driver *infoUpdatingDriver) Connect(context.Context) error {
	driver.connected = true
	return nil
}
func (*infoUpdatingDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*infoUpdatingDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (*infoUpdatingDriver) Close() error { return nil }

func (*themeDisconnectDriver) Info() device.Info { return device.Info{ID: "theme-disconnect"} }
func (driver *themeDisconnectDriver) Connect(context.Context) error {
	driver.connected = true
	return nil
}
func (driver *themeDisconnectDriver) ApplyTheme(context.Context, theme.Manifest) error {
	if driver.applyErr != nil {
		driver.connected = false
		return driver.applyErr
	}
	return nil
}
func (*themeDisconnectDriver) SendTelemetry(context.Context, telemetry.Snapshot) error { return nil }
func (*themeDisconnectDriver) Close() error                                            { return nil }
func (driver *themeDisconnectDriver) Connected() bool                                  { return driver.connected }

func (driver *namedThemeDriver) Info() device.Info { return device.Info{ID: driver.id} }
func (*namedThemeDriver) Connect(context.Context) error {
	return nil
}
func (driver *namedThemeDriver) ApplyTheme(context.Context, theme.Manifest) error {
	return driver.applyErr
}
func (*namedThemeDriver) SendTelemetry(context.Context, telemetry.Snapshot) error { return nil }
func (*namedThemeDriver) Close() error                                            { return nil }

type statusSpectrum struct{}

func (statusSpectrum) Snapshot() audio.SpectrumFrame { return audio.SpectrumFrame{} }
func (statusSpectrum) Status() audio.EngineStatus {
	return audio.EngineStatus{Running: true, Endpoint: "测试扬声器", EndpointID: "endpoint-1", EndpointRole: "console", LastError: "音频诊断"}
}

type statusLyrics struct{}

func (statusLyrics) Snapshot() render.LyricState { return render.LyricState{} }
func (statusLyrics) Status() lyrics.EngineStatus {
	return lyrics.EngineStatus{
		Running: true, HasLyrics: true, Provider: "netease",
		Track: lyrics.NowPlaying{Title: "测试歌曲", Artist: "测试歌手", Playing: true},
	}
}

type overlaySpectrum struct{}

func (overlaySpectrum) Snapshot() audio.SpectrumFrame {
	return audio.SpectrumFrame{Bars: []float64{0, .25, .5, .75, 1}}
}

type spectrumTestProvider struct {
	frame audio.SpectrumFrame
}

func (provider *spectrumTestProvider) Snapshot() audio.SpectrumFrame {
	frame := provider.frame
	frame.Bars = append([]float64(nil), frame.Bars...)
	return frame
}

type spectrumTestDriver struct {
	fakeDriver
	spectrumSends int
	lastValues    map[string]string
	spectrumErr   error
}

type blockingLyricSnapshotProvider struct {
	mu      sync.Mutex
	state   render.LyricState
	block   bool
	started chan struct{}
	release chan struct{}
}

func (provider *blockingLyricSnapshotProvider) Snapshot() render.LyricState {
	provider.mu.Lock()
	state := provider.state
	block := provider.block
	provider.block = false
	provider.mu.Unlock()
	if block {
		close(provider.started)
		<-provider.release
	}
	return state
}

func (provider *blockingLyricSnapshotProvider) setState(state render.LyricState) {
	provider.mu.Lock()
	provider.state = state
	provider.mu.Unlock()
}

type orderedSmallPacketWrite struct {
	kind     string
	snapshot telemetry.Snapshot
	values   map[string]string
}

type orderedSmallPacketDriver struct {
	mu     sync.Mutex
	writes []orderedSmallPacketWrite
}

func (*orderedSmallPacketDriver) Info() device.Info {
	return device.Info{ID: "ordered-small-packet", Capabilities: device.Capabilities{
		DeliveryMode:   device.DeliveryStoredTheme,
		SmallTelemetry: true,
	}}
}

func (*orderedSmallPacketDriver) Connect(context.Context) error { return nil }
func (*orderedSmallPacketDriver) ApplyTheme(context.Context, theme.Manifest) error {
	return nil
}
func (*orderedSmallPacketDriver) Close() error { return nil }
func (driver *orderedSmallPacketDriver) SendTelemetry(_ context.Context, snapshot telemetry.Snapshot) error {
	driver.mu.Lock()
	driver.writes = append(driver.writes, orderedSmallPacketWrite{kind: "telemetry", snapshot: snapshot})
	driver.mu.Unlock()
	return nil
}
func (driver *orderedSmallPacketDriver) SendSpectrumValues(_ context.Context, values map[string]string) error {
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	driver.mu.Lock()
	driver.writes = append(driver.writes, orderedSmallPacketWrite{kind: "spectrum", values: copied})
	driver.mu.Unlock()
	return nil
}
func (driver *orderedSmallPacketDriver) snapshotWrites() []orderedSmallPacketWrite {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return append([]orderedSmallPacketWrite(nil), driver.writes...)
}

func (*spectrumTestDriver) Info() device.Info {
	return device.Info{ID: "fake", Capabilities: device.Capabilities{
		DeliveryMode:   device.DeliveryStoredTheme,
		SmallTelemetry: true,
	}}
}

type refreshDisconnectDriver struct {
	connected bool
	connects  int
	sendErr   error
}

func (*refreshDisconnectDriver) Info() device.Info {
	return device.Info{ID: "refresh-disconnect", Capabilities: device.Capabilities{SmallTelemetry: true}}
}
func (driver *refreshDisconnectDriver) Connect(context.Context) error {
	driver.connected = true
	driver.connects++
	return nil
}
func (*refreshDisconnectDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (driver *refreshDisconnectDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	if driver.sendErr != nil {
		driver.connected = false
		return driver.sendErr
	}
	return nil
}
func (*refreshDisconnectDriver) Close() error           { return nil }
func (driver *refreshDisconnectDriver) Connected() bool { return driver.connected }

func (driver *spectrumTestDriver) SendSpectrumValues(_ context.Context, values map[string]string) error {
	driver.spectrumSends++
	driver.lastValues = make(map[string]string, len(values))
	for key, value := range values {
		driver.lastValues[key] = value
	}
	return driver.spectrumErr
}

type overlayLyrics struct{}

func (overlayLyrics) Snapshot() render.LyricState {
	return render.LyricState{Current: "当前歌词", Next: "下一句歌词"}
}

func (driver *fakeDriver) Info() device.Info {
	return device.Info{ID: "fake", Capabilities: device.Capabilities{SmallTelemetry: true}}
}
func (driver *fakeDriver) Connect(context.Context) error { return nil }
func (driver *fakeDriver) ApplyTheme(context.Context, theme.Manifest) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.applyErr
}
func (driver *fakeDriver) Close() error { return nil }
func (driver *fakeDriver) SendTelemetry(_ context.Context, snapshot telemetry.Snapshot) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.sends++
	driver.lastSend = snapshot
	return driver.sendErr
}

func TestDisabledTelemetrySendsNothing(t *testing.T) {
	collector := &fakeCollector{}
	driver := &fakeDriver{}
	service := New(collector, 5*time.Millisecond)
	service.AddDriver(driver)
	service.SetTelemetryEnabled(false)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if collector.count != 0 || driver.sends != 0 {
		t.Fatalf("关闭遥测后仍发送: capture=%d send=%d", collector.count, driver.sends)
	}
}

func TestSmallTelemetryOnlySentWhenDeviceThemeHasTelemetryLayer(t *testing.T) {
	collector := &fakeCollector{}
	driver := &fakeDriver{}
	controller := New(collector, time.Second)
	controller.AddDriver(driver)
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{{Type: theme.LayerBackgroundImage, Opacity: 1}}}
	controller.tick(context.Background())
	if driver.sends != 0 {
		t.Fatalf("纯背景主题仍向微雪发送指标：%d", driver.sends)
	}
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{{Type: theme.LayerBackgroundImage, Opacity: 1}, {Type: theme.LayerTelemetry, Opacity: 1}}}
	controller.tick(context.Background())
	if driver.sends != 1 {
		t.Fatalf("添加遥测图层后没有发送指标：%d", driver.sends)
	}
}

func TestSmallDataIncludesAudioAndLyricsForWaveshareLayers(t *testing.T) {
	collector := &fakeCollector{}
	driver := &fakeDriver{}
	controller := New(collector, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(overlaySpectrum{})
	controller.SetAudioEnabled(true)
	controller.SetLyricsProvider(overlayLyrics{})
	controller.SetLyricsEnabled(true)
	controller.deviceThemes["fake"] = theme.Manifest{Layers: []theme.Layer{
		{Type: theme.LayerAudioBars, Opacity: 1},
		{Type: theme.LayerLyrics, Opacity: 1},
	}}
	controller.tick(context.Background())
	if driver.sends != 1 {
		t.Fatalf("音频/歌词图层没有触发小数据包：%d", driver.sends)
	}
	if driver.lastSend.AudioSpectrum != "0,25,50,75,100" {
		t.Fatalf("频谱没有编码到小数据包：%q", driver.lastSend.AudioSpectrum)
	}
	if driver.lastSend.LyricsCurrent != "当前歌词" || driver.lastSend.LyricsNext != "下一句歌词" {
		t.Fatalf("双行歌词没有写入专用字段：%+v", driver.lastSend)
	}
	if driver.lastSend.CPUModel != "" || driver.lastSend.GPUModel != "" {
		t.Fatalf("歌词污染了 CPU/GPU 型号字段：%+v", driver.lastSend)
	}
}

func TestWaveshareSpectrumLoopUsesLayerTransformAndConnectionState(t *testing.T) {
	bars := make([]float64, waveshareSpectrumBandCount)
	bars[1] = .25
	bars[2] = 1
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{
		CapturedAt: time.Now(), Bars: bars,
	}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	layer := theme.Layer{
		ID: "bars", Type: theme.LayerAudioBars, Opacity: 1,
		Bounds: theme.Rect{Width: 1, Height: 1},
		Options: map[string]any{
			"visible": true, "gain": 1.0, "sensitivity": 1.0,
			"min_amplitude": .1, "max_amplitude": .8, "smoothing": 0.0, "noise_gate": 0.0,
			"contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0, "count": 48,
		},
	}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}}
	controller.tick(context.Background())
	if driver.sends != 0 {
		t.Fatalf("仅含专用高频音频图层时仍发送每秒空包: %d", driver.sends)
	}
	runtimeByDevice := make(map[string]waveshareSpectrumRuntime)
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	if driver.spectrumSends != 1 {
		t.Fatalf("已连接的微雪音频图层没有收到频谱: %d", driver.spectrumSends)
	}
	// 屏端协议以 0..100 整数发送，.275 会按协议量化为 .28。
	want := []float64{theme.DefaultAudioIdleAmplitude, .28, .8}
	for index, expected := range want {
		got, _ := strconv.ParseFloat(driver.lastValues[theme.WaveshareAudioBandKey(layer, index)], 64)
		got /= 100
		if difference := got - expected; math.Abs(difference) > 1e-9 {
			t.Errorf("频带 %d: got=%v want=%v", index, got, expected)
		}
	}

	// 同一帧不会重复发；断线和不可见图层也不会发送。
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: false}
	provider.frame.CapturedAt = time.Now().Add(time.Millisecond)
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	manifest := controller.deviceThemes["fake"]
	manifest.Layers[0].Options["visible"] = false
	controller.deviceThemes["fake"] = manifest
	provider.frame.CapturedAt = time.Now().Add(2 * time.Millisecond)
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	if driver.spectrumSends != 2 {
		t.Fatalf("隐藏图层应只额外发送一组归零键: %d", driver.spectrumSends)
	}
}

func TestWaveshareSpectrumMergesOnlyFineAndKeepsLegacyIndependent(t *testing.T) {
	now := time.Now()
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: now, Bars: []float64{.25, .75}}}
	lyricsProvider := &eventLyrics{state: render.LyricState{
		Current: "第一句", Next: "第二句", Progress: .42,
		CurrentAt: 0, NextAt: 10 * time.Second, PositionKnown: true, Playing: true,
	}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.SetLyricsProvider(lyricsProvider)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: .4}, Opacity: 1},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Y: .5, Width: 1, Height: .4}, Opacity: 1,
			Options: map[string]any{"style": "player", "show_progress": true}},
	}}
	runtime := make(map[string]waveshareSpectrumRuntime)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	manifest := controller.deviceThemes["fake"]
	initialFine, _ := lyricProgressForManifest(manifest, lyricsProvider.state, true)
	if _, hasLegacy := driver.lastValues[telemetry.LyricsProgressLegacyKey]; hasLegacy {
		t.Fatalf("旧歌词键混入首个频谱包: %v", driver.lastValues)
	}
	if driver.spectrumSends != 1 || driver.lastValues[telemetry.LyricsProgressFineKey] != strconv.Itoa(initialFine) {
		t.Fatalf("首个频谱包没有合入短 Fine: sends=%d values=%v", driver.spectrumSends, driver.lastValues)
	}

	// 活跃频谱下独立路径只补旧兼容键，不重复发送 Fine。
	controller.sendLyricsProgress(context.Background())
	progressValues := driver.lastSend.Values()
	if driver.sends != 1 || driver.spectrumSends != 1 ||
		progressValues[telemetry.LyricsProgressLegacyKey] != "42" {
		t.Fatalf("活跃频谱没有独立发送旧兼容键: telemetry=%d spectrum=%d values=%v", driver.sends, driver.spectrumSends, progressValues)
	}
	if _, hasFine := progressValues[telemetry.LyricsProgressFineKey]; hasFine || len(progressValues) != 1 {
		t.Fatalf("旧兼容包重复携带 Fine: %v", progressValues)
	}

	// 同一整百分比内短 Fine 只随下一帧频谱发送，独立包数量不变。
	lyricsProvider.state.Progress = .4205
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	controller.sendLyricsProgress(context.Background())
	nextFine, _ := lyricProgressForManifest(manifest, lyricsProvider.state, true)
	if driver.spectrumSends != 2 || driver.sends != 1 ||
		driver.lastValues[telemetry.LyricsProgressFineKey] != strconv.Itoa(nextFine) {
		t.Fatalf("细进度没有仅随频谱更新: spectrum=%d telemetry=%d values=%v", driver.spectrumSends, driver.sends, driver.lastValues)
	}
	if _, hasLegacy := driver.lastValues[telemetry.LyricsProgressLegacyKey]; hasLegacy {
		t.Fatalf("第二帧频谱混入旧长键: %v", driver.lastValues)
	}

	lyricsProvider.state.Progress = .43
	provider.frame.CapturedAt = now.Add(2 * waveshareSpectrumInterval)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	controller.sendLyricsProgress(context.Background())
	nextFine, _ = lyricProgressForManifest(manifest, lyricsProvider.state, true)
	progressValues = driver.lastSend.Values()
	if driver.spectrumSends != 3 || driver.sends != 2 || progressValues[telemetry.LyricsProgressLegacyKey] != "43" {
		t.Fatalf("跨整百分比没有独立发送旧键: spectrum=%d telemetry=%d values=%v", driver.spectrumSends, driver.sends, progressValues)
	}
	if driver.lastValues[telemetry.LyricsProgressFineKey] != strconv.Itoa(nextFine) {
		t.Fatalf("跨整百分比的 Fine 没有留在频谱包: %v", driver.lastValues)
	}
	if _, hasLegacy := driver.lastValues[telemetry.LyricsProgressLegacyKey]; hasLegacy {
		t.Fatalf("跨整百分比时频谱包混入旧长键: %v", driver.lastValues)
	}

	lyricsProvider.state.Progress = .44
	lyricsProvider.state.Playing = false
	controller.sendLyricsEvent(context.Background())
	pauseFine, _ := lyricProgressForManifest(manifest, lyricsProvider.state, true)
	if driver.lastSend.LyricsPlaying || !driver.lastSend.LyricsProgressKnown || driver.lastSend.LyricsProgress != 44 ||
		!driver.lastSend.LyricsProgressFineKnown || driver.lastSend.LyricsProgressFine != pauseFine {
		t.Fatalf("暂停事件没有立即冻结最终位置: %+v", driver.lastSend)
	}
	provider.frame.CapturedAt = now.Add(3 * waveshareSpectrumInterval)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	_, hasLegacy := driver.lastValues[telemetry.LyricsProgressLegacyKey]
	_, hasFine := driver.lastValues[telemetry.LyricsProgressFineKey]
	if hasLegacy || hasFine {
		t.Fatalf("暂停后频谱仍携带周期歌词进度: %v", driver.lastValues)
	}
}

func TestWaveshareSpectrumZeroBoundaryKeepsLyricsPacketIndependent(t *testing.T) {
	now := time.Now()
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: now, Bars: []float64{.25, .75}}}
	lyricsProvider := &eventLyrics{state: render.LyricState{
		Current: "第一句", Next: "第二句", Progress: .42,
		PositionKnown: true, Playing: true,
	}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.SetLyricsProvider(lyricsProvider)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	barLayer := theme.Layer{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: .4}, Opacity: 1}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		barLayer,
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Y: .5, Width: 1, Height: .4}, Opacity: 1,
			Options: map[string]any{"style": "player", "show_progress": true}},
	}}
	runtime := make(map[string]waveshareSpectrumRuntime)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	if driver.spectrumSends != 1 {
		t.Fatalf("首个频谱包数量错误: %d", driver.spectrumSends)
	}

	// 采集帧刚好失效时，频带切换为静态基线；歌词仍使用独立短包。
	lyricsProvider.state.Progress = .43
	provider.frame.CapturedAt = time.Now().Add(-waveshareSpectrumStaleAfter - time.Second)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	controller.sendLyricsProgress(context.Background())
	if driver.spectrumSends != 2 || driver.sends != 1 {
		t.Fatalf("归零边界没有保持一帧频谱加一个歌词包: spectrum=%d telemetry=%d", driver.spectrumSends, driver.sends)
	}
	manifest := controller.deviceThemes["fake"]
	wantFine, _ := lyricProgressForManifest(manifest, lyricsProvider.state, true)
	progressValues := driver.lastSend.Values()
	if progressValues[telemetry.LyricsProgressLegacyKey] != "43" || len(progressValues) != 1 {
		t.Fatalf("归零边界的旧兼容独立包错误: %v", progressValues)
	}
	if driver.lastValues[telemetry.LyricsProgressFineKey] != strconv.Itoa(wantFine) {
		t.Fatalf("归零频谱包没有合入短 Fine: %v", driver.lastValues)
	}
	if _, hasLegacy := driver.lastValues[telemetry.LyricsProgressLegacyKey]; hasLegacy {
		t.Fatalf("归零频谱包混入旧长键: %v", driver.lastValues)
	}
	for _, key := range waveshareSpectrumKeys([]theme.Layer{barLayer}, &manifest) {
		if driver.lastValues[key] != "4" {
			t.Fatalf("过期频谱没有切换到低幅基线: key=%s value=%q", key, driver.lastValues[key])
		}
	}
}

func TestSmallPacketGatePreventsStaleSpectrumFineAfterPauseSeekAndLineChange(t *testing.T) {
	provider := &blockingLyricSnapshotProvider{
		state: render.LyricState{
			Current: "第一句", Next: "第二句", Progress: .92,
			CurrentAt: 0, NextAt: 10 * time.Second, PositionKnown: true, Playing: true,
		},
		block: true, started: make(chan struct{}), release: make(chan struct{}),
	}
	driver := &orderedSmallPacketDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(&spectrumTestProvider{frame: audio.SpectrumFrame{
		CapturedAt: time.Now(), Bars: []float64{.4, .8},
	}})
	controller.SetAudioEnabled(true)
	controller.SetLyricsProvider(provider)
	controller.statuses[driver.Info().ID] = DeviceStatus{Info: driver.Info(), Connected: true}
	controller.deviceThemes[driver.Info().ID] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: .4}, Opacity: 1},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Y: .5, Width: 1, Height: .4}, Opacity: 1,
			Options: map[string]any{"style": "player", "show_progress": true}},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runtime := make(map[string]waveshareSpectrumRuntime)
	spectrumDone := make(chan struct{})
	go func() {
		controller.sendWaveshareSpectrum(ctx, runtime)
		close(spectrumDone)
	}()
	select {
	case <-provider.started:
	case <-ctx.Done():
		t.Fatal("频谱发送没有进入阻塞歌词快照")
	}

	// 频谱已经读取旧快照但尚未写出；此时播放器暂停、拖动并切到
	// 下一句。事件发送必须等旧周期先完成，再用锁内新快照最终覆盖。
	provider.setState(render.LyricState{
		Current: "第二句", Next: "第三句", Progress: .08,
		CurrentAt: 10 * time.Second, NextAt: 15 * time.Second, PositionKnown: true, Playing: false,
	})
	eventStarted := make(chan struct{})
	eventDone := make(chan struct{})
	go func() {
		close(eventStarted)
		controller.sendLyricsEvent(ctx)
		close(eventDone)
	}()
	<-eventStarted
	select {
	case <-eventDone:
		t.Fatal("暂停事件越过了仍持有旧快照的频谱发送")
	case <-time.After(30 * time.Millisecond):
	}
	close(provider.release)
	select {
	case <-spectrumDone:
	case <-ctx.Done():
		t.Fatal("旧频谱发送没有完成")
	}
	select {
	case <-eventDone:
	case <-ctx.Done():
		t.Fatal("暂停/拖动/换句事件没有完成")
	}

	writes := driver.snapshotWrites()
	if len(writes) != 2 {
		t.Fatalf("并发场景包数量异常: %#v", writes)
	}
	if writes[0].kind != "spectrum" || writes[0].values[telemetry.LyricsProgressFineKey] == "" {
		t.Fatalf("首包不是携带旧 Fine 的频谱包: %#v", writes)
	}
	if _, hasLegacy := writes[0].values[telemetry.LyricsProgressLegacyKey]; hasLegacy {
		t.Fatalf("旧长键混入频谱包: %#v", writes)
	}
	last := writes[len(writes)-1]
	manifest := controller.deviceThemes[driver.Info().ID]
	wantFine, _ := lyricProgressForManifest(manifest, render.LyricState{
		Current: "第二句", Progress: .08, PositionKnown: true,
	}, true)
	if last.kind != "telemetry" || last.snapshot.LyricsCurrent != "第二句" ||
		last.snapshot.LyricsNext != "第三句" || last.snapshot.LyricsPlaying ||
		!last.snapshot.LyricsProgressKnown || last.snapshot.LyricsProgress != 8 ||
		!last.snapshot.LyricsProgressFineKnown || last.snapshot.LyricsProgressFine != wantFine {
		t.Fatalf("旧频谱 Fine 覆盖了暂停/拖动/换句状态: %#v", writes)
	}
}

func TestWaveshareSpectrumDropDoesNotQueueStaleFrame(t *testing.T) {
	now := time.Now()
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: now, Bars: []float64{.2}}}
	driver := &spectrumTestDriver{spectrumErr: device.ErrRealtimeFrameDropped}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	layer := theme.Layer{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		Options: map[string]any{
			"count": 6, "gain": 1.0, "sensitivity": 1.0,
			"min_amplitude": 0.0, "max_amplitude": 1.0, "smoothing": 0.0,
			"noise_gate": 0.0, "contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
		}}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}}
	runtimeByDevice := make(map[string]waveshareSpectrumRuntime)

	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	if _, exists := runtimeByDevice["fake"]; exists {
		t.Fatal("被驱动丢弃的频谱帧不应记作已发送")
	}
	driver.spectrumErr = nil
	provider.frame = audio.SpectrumFrame{CapturedAt: now.Add(time.Millisecond), Bars: []float64{.8}}
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	if driver.spectrumSends != 2 {
		t.Fatalf("带宽恢复后没有直接发送最新帧: %d", driver.spectrumSends)
	}
	if got := driver.lastValues[theme.WaveshareAudioBandKey(layer, 0)]; got != "80" {
		t.Fatalf("带宽恢复后发送了积压旧帧而不是最新帧: %q", got)
	}
	if runtimeByDevice["fake"].lastFrame != provider.frame.CapturedAt {
		t.Fatal("ACK 成功后没有记录最新帧时间")
	}
}

func TestWaveshareSpectrumUsesIndependentKeysAndOptionsPerLayer(t *testing.T) {
	bars := make([]float64, waveshareSpectrumBandCount)
	bars[1] = .25
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: time.Now(), Bars: bars}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	first := theme.Layer{ID: "bars-a", Type: theme.LayerAudioBars, Opacity: 1, Bounds: theme.Rect{Width: 1, Height: 1}, Options: map[string]any{
		"source": "system", "gain": 1.0, "sensitivity": 1.0, "min_amplitude": 0.0, "max_amplitude": 1.0, "smoothing": 0.0, "noise_gate": 0.0, "contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0, "count": 48,
	}}
	second := theme.Layer{ID: "ring-b", Type: theme.LayerAudioRing, Opacity: 1, Bounds: theme.Rect{Width: 1, Height: 1}, Options: map[string]any{
		"source": "system", "gain": 2.0, "sensitivity": 1.0, "min_amplitude": 0.0, "max_amplitude": 1.0, "smoothing": 0.0, "noise_gate": 0.0, "contrast": 1.0, "separation": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "mic", Type: theme.LayerAudioBars, Opacity: 1, Bounds: theme.Rect{Width: 1, Height: 1}, Options: map[string]any{"source": "device_mic", "gain": 20.0, "count": 48}},
		first, second,
	}}
	controller.sendWaveshareSpectrum(context.Background(), make(map[string]waveshareSpectrumRuntime))
	if driver.spectrumSends != 1 {
		t.Fatalf("多频谱图层应合并为一个小数据包: %d", driver.spectrumSends)
	}
	if got := driver.lastValues[theme.WaveshareAudioBandKey(first, 1)]; got != "25" {
		t.Fatalf("第一图层参数未独立生效: %q", got)
	}
	if got := driver.lastValues[theme.WaveshareAudioBandKey(second, 1)]; got != "50" {
		t.Fatalf("第二图层增益未独立生效: %q", got)
	}
	if _, exists := driver.lastValues[theme.WaveshareAudioBandKey(controller.deviceThemes["fake"].Layers[0], 1)]; !exists {
		t.Fatal("旧屏端麦克风图层应自动迁移到主机频谱")
	}
	if _, exists := driver.lastValues["Audio_01"]; exists {
		t.Fatal("频谱包不应继续发送未引用的 Audio_XX 兼容键")
	}
}

func TestWaveshareSpectrumSharesBaseKeysAndSeparatesPeakSettings(t *testing.T) {
	bars := make([]float64, waveshareSpectrumBandCount)
	for index := range bars {
		bars[index] = .5
	}
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: time.Now(), Bars: bars}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	defaultPeak := theme.Layer{ID: "bars-default-peak", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{
		"count": 6, "peak_hold": true,
	}}
	samePeak := theme.Layer{ID: "bars-same-peak", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{
		"count": 6, "peak_hold": true,
	}}
	customPeak := theme.Layer{ID: "bars-custom-peak", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{
		"count": 6, "peak_hold": true, "peak_hold_ms": 900.0, "peak_decay": 2.4,
	}}
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{defaultPeak, samePeak, customPeak}}
	controller.deviceThemes["fake"] = manifest
	runtimeByDevice := make(map[string]waveshareSpectrumRuntime)
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)

	indexes := theme.WaveshareAudioBandIndexes(defaultPeak, manifest.Width, manifest.Height)
	if got, want := len(driver.lastValues), len(indexes)*3; got != want {
		t.Fatalf("同变换的基础频带未共享，或不同峰值策略未分离: got=%d want=%d values=%v", got, want, driver.lastValues)
	}
	for _, index := range indexes {
		for _, key := range []string{
			theme.WaveshareAudioBandKey(defaultPeak, index),
			theme.WaveshareAudioPeakKey(defaultPeak, index),
			theme.WaveshareAudioPeakKey(customPeak, index),
		} {
			if _, exists := driver.lastValues[key]; !exists {
				t.Fatalf("频谱包缺少编译层引用键 %q: %v", key, driver.lastValues)
			}
		}
	}
	if got := len(runtimeByDevice["fake"].layers); got != 2 {
		t.Fatalf("相同峰值设置应共享运行态，不同设置应分离: %d", got)
	}
}

func TestWaveshareSpectrumDefaultBarsAndRingFitOneSmallPacket(t *testing.T) {
	bars := make([]float64, waveshareSpectrumBandCount)
	for index := range bars {
		bars[index] = 1
	}
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: time.Now(), Bars: bars}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: .3}, Opacity: 1},
		{ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{Y: .3, Width: 1, Height: .7}, Opacity: 1},
	}}
	controller.sendWaveshareSpectrum(context.Background(), make(map[string]waveshareSpectrumRuntime))
	if len(driver.lastValues) != theme.WaveshareAudioBandCount {
		t.Fatalf("默认条状和 48 段环状频谱应共享 48 个频带，实际 %d: %v", len(driver.lastValues), driver.lastValues)
	}
	for key := range driver.lastValues {
		if len(key) != 3 || key[0] != 'A' {
			t.Fatalf("默认频谱仍包含长键或兼容键: %q", key)
		}
	}
	packet := waveshareProtocol.SystemDataPacket(driver.lastValues)
	if len(packet) > 1002 || len(packet) < 800 {
		t.Fatalf("48 个短键频带的 QStringMap 命令包大小异常=%d，预算上限 1002 字节", len(packet))
	}
}

func TestWaveshareSpectrumFineMergeMeetsEightyFivePercentWireBudget(t *testing.T) {
	encode := func(values map[string]string) []byte {
		return waveshareProtocol.EncodePacket(waveshareProtocol.Packet{
			Type: waveshareProtocol.PacketTypeCommand, Command: waveshareProtocol.CommandSystemData,
			Payload: waveshareProtocol.EncodeQStringMap(values),
		})
	}
	typical := make(map[string]string, theme.WaveshareAudioBandCount+1)
	worst := make(map[string]string, theme.WaveshareAudioBandCount+1)
	for index := 0; index < theme.WaveshareAudioBandCount; index++ {
		typical[fmt.Sprintf("A%02d", index)] = "50"
		worst[fmt.Sprintf("A%02d", index)] = "100"
	}
	typical[telemetry.LyricsProgressFineKey] = "5000"
	worst[telemetry.LyricsProgressFineKey] = "5000"
	typicalPacket := encode(typical)
	worstPacket := encode(worst)
	finePacket := encode(map[string]string{telemetry.LyricsProgressFineKey: "5000"})
	legacyPacket := encode(map[string]string{telemetry.LyricsProgressLegacyKey: "50"})
	if len(typicalPacket) != 928 || len(worstPacket) != 1024 || len(finePacket) != 64 || len(legacyPacket) != 104 {
		t.Fatalf("真实命令包大小异常: typical=%d worst=%d fine=%d legacy=%d",
			len(typicalPacket), len(worstPacket), len(finePacket), len(legacyPacket))
	}
	const ackBytes = 38
	budgetBitsPerSecond := 115200.0 * .85
	typicalBitsPerSecond := float64((len(typicalPacket) + ackBytes) * 10 * 10)
	if typicalBitsPerSecond > budgetBitsPerSecond {
		t.Fatalf("典型48频带+Fine超过85%%线速预算: %.1f > %.1f bps", typicalBitsPerSecond, budgetBitsPerSecond)
	}
	worstInterval := time.Duration(math.Ceil(float64((len(worstPacket)+ackBytes)*10) / budgetBitsPerSecond * float64(time.Second)))
	if worstInterval <= 100*time.Millisecond || worstInterval > 110*time.Millisecond {
		t.Fatalf("三位数最坏包没有自适应降频: packet=%d interval=%v", len(worstPacket), worstInterval)
	}
	if _, exists := typical[telemetry.LyricsProgressLegacyKey]; exists {
		t.Fatal("旧长兼容键混入频谱包")
	}
}

func TestWaveshareSpectrumKeepsIdleBaselineWhenAudioUnavailableAndClearsWhenHidden(t *testing.T) {
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: time.Now(), Bars: []float64{1, 1}}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.SetAudioEnabled(true)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
		ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
	}}}
	runtimeByDevice := make(map[string]waveshareSpectrumRuntime)
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	controller.SetAudioEnabled(false)
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	if driver.spectrumSends != 2 || !allWaveshareSpectrumValuesEqual(driver.lastValues, "4") {
		t.Fatalf("关闭音频后没有保留低幅基线: sends=%d values=%v", driver.spectrumSends, driver.lastValues)
	}

	controller.SetAudioEnabled(true)
	provider.frame.CapturedAt = time.Now().Add(-waveshareSpectrumStaleAfter - time.Millisecond)
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	if !allWaveshareSpectrumValuesEqual(driver.lastValues, "4") {
		t.Fatalf("采集停滞后频谱没有保留低幅基线: %v", driver.lastValues)
	}

	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600}
	controller.sendWaveshareSpectrum(context.Background(), runtimeByDevice)
	if !allWaveshareSpectrumValuesZero(driver.lastValues) {
		t.Fatalf("频谱图层删除后没有清空旧基线: %v", driver.lastValues)
	}
}

func allWaveshareSpectrumValuesEqual(values map[string]string, expected string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if value != expected {
			return false
		}
	}
	return true
}

func allWaveshareSpectrumValuesZero(values map[string]string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if value != "0" {
			return false
		}
	}
	return true
}

func TestWaveshareSpectrumPeakHoldAndDecay(t *testing.T) {
	options := waveshareSpectrumOptions{peakHold: true, peakDuration: 500 * time.Millisecond, peakDecay: 1}
	start := time.Unix(30, 0)
	peaks, held := updateWaveshareSpectrumPeaks([]float64{.8}, waveshareSpectrumLayerRuntime{}, options, start)
	if len(peaks) != 1 || math.Abs(peaks[0]-.8) > 1e-9 {
		t.Fatalf("峰值未记录: %+v", peaks)
	}
	runtime := waveshareSpectrumLayerRuntime{peaks: peaks, heldUntil: held, updatedAt: start}
	peaks, held = updateWaveshareSpectrumPeaks([]float64{.2}, runtime, options, start.Add(300*time.Millisecond))
	if math.Abs(peaks[0]-.8) > 1e-9 {
		t.Fatalf("保持期内峰值提前回落: %v", peaks[0])
	}
	runtime = waveshareSpectrumLayerRuntime{peaks: peaks, heldUntil: held, updatedAt: start.Add(300 * time.Millisecond)}
	peaks, _ = updateWaveshareSpectrumPeaks([]float64{.2}, runtime, options, start.Add(700*time.Millisecond))
	if math.Abs(peaks[0]-.4) > 1e-9 {
		t.Fatalf("保持期后峰值未按速度回落: %v", peaks[0])
	}
}

func TestWaveshareFreshSilentFrameImmediatelyUsesIdleBaselineAndClearsPeakHold(t *testing.T) {
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: time.Now(), Bars: []float64{1, 1, 1, 1}}}
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.SetSpectrumProvider(provider)
	controller.statuses["fake"] = DeviceStatus{Info: driver.Info(), Connected: true}
	controller.deviceThemes["fake"] = theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
		ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		Options: map[string]any{"smoothing": .98, "noise_gate": .01, "peak_hold": true, "peak_hold_ms": 5000.0},
	}}}
	runtime := make(map[string]waveshareSpectrumRuntime)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	provider.frame = audio.SpectrumFrame{CapturedAt: time.Now().Add(time.Millisecond), Bars: []float64{0, 0, 0, 0}}
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	manifest := controller.deviceThemes["fake"]
	layer := manifest.Layers[0]
	for _, index := range theme.WaveshareAudioBandIndexes(layer, manifest.Width, manifest.Height) {
		if got := driver.lastValues[theme.WaveshareAudioBandKey(layer, index)]; got != "4" {
			t.Fatalf("新鲜静音帧没有保留低幅基线: key=%d value=%q", index, got)
		}
		if got := driver.lastValues[theme.WaveshareAudioPeakKey(layer, index)]; got != "0" {
			t.Fatalf("新鲜静音帧没有立即清除峰值: key=%d value=%q", index, got)
		}
	}
}

func TestWaveshareDeviceMicrophoneMigratesToHostSpectrum(t *testing.T) {
	manifest := theme.Manifest{Layers: []theme.Layer{{
		ID: "mic", Type: theme.LayerAudioRing, Opacity: 1, Options: map[string]any{"source": "device_mic"},
	}}}
	if layers := waveshareHostAudioLayers(&manifest); len(layers) != 1 {
		t.Fatalf("旧屏端麦克风图层没有迁移到 WASAPI: %+v", layers)
	}
}

func TestResampleWaveshareSpectrumCoversWholeFrequencyRange(t *testing.T) {
	output := resampleWaveshareSpectrum([]float64{0, 1}, waveshareSpectrumBandCount)
	if len(output) != waveshareSpectrumBandCount || output[0] != 0 || output[len(output)-1] != 1 {
		t.Fatalf("频谱重采样没有覆盖完整范围: %#v", output)
	}
	if output[24] <= .5 || output[24] >= .52 {
		t.Fatalf("频谱中点插值错误: %v", output[24])
	}
}

func (driver *reconnectDriver) Info() device.Info {
	return device.Info{ID: "reconnect", Capabilities: device.Capabilities{SmallTelemetry: true}}
}

func (driver *reconnectDriver) Connect(context.Context) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.connects++
	if driver.connects == 1 {
		return context.DeadlineExceeded
	}
	return nil
}

func (*reconnectDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }

func (driver *reconnectDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	driver.mu.Lock()
	driver.sends++
	driver.mu.Unlock()
	return nil
}
func (*reconnectDriver) Close() error { return nil }

func (driver *reconnectHostDriver) Info() device.Info {
	return device.Info{
		ID: "reconnect-host", Resolution: [2]int{80, 45},
		Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame, HostRenderedFrames: true},
	}
}

func (driver *reconnectHostDriver) Connect(context.Context) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.connects++
	if driver.connects == 1 {
		return context.DeadlineExceeded
	}
	return nil
}

func (*reconnectHostDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*reconnectHostDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (driver *reconnectHostDriver) SendJPEG(context.Context, []byte) error {
	driver.mu.Lock()
	driver.frames++
	driver.mu.Unlock()
	return nil
}
func (*reconnectHostDriver) Close() error { return nil }

func TestDisconnectedDriverReconnects(t *testing.T) {
	driver := &reconnectDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectEvery = 5 * time.Millisecond
	controller.AddDriver(driver)
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Millisecond)
	defer cancel()
	if err := controller.Run(ctx); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	connects := driver.connects
	driver.mu.Unlock()
	if connects < 2 {
		t.Fatalf("断线设备没有重连: connects=%d", connects)
	}
	status := controller.Status().Devices[0]
	if !status.Connected || status.LastError != "" {
		t.Fatalf("重连后的状态无效: %+v", status)
	}
}

func TestSlowReconnectDoesNotBlockServiceLoopAndRetriesAfterTimeout(t *testing.T) {
	driver := &boundedReconnectDriver{}
	collector := &hostFrameCountingCollector{}
	controller := New(collector, 2*time.Millisecond)
	controller.reconnectEvery = 2 * time.Millisecond
	controller.reconnectTimeout = 12 * time.Millisecond
	controller.AddDriver(driver)
	manifest := theme.Manifest{ID: "reconnect-loop", Name: "重连主循环", Width: 80, Height: 45, Layers: []theme.Layer{{
		ID: "metrics", Type: theme.LayerTelemetry, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
	}}}
	controller.activeTheme = &manifest

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Millisecond)
	defer cancel()
	if err := controller.Run(ctx); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	connects, maxConcurrent := driver.connects, driver.maxConcurrent
	driver.mu.Unlock()
	status := controller.Status().Devices[0]
	if connects < 3 || !status.Connected {
		t.Fatalf("超时后未继续重连并恢复: connects=%d status=%+v", connects, status)
	}
	if maxConcurrent != 1 {
		t.Fatalf("同一设备发生并发重连: %d", maxConcurrent)
	}
	if captures := collector.captures.Load(); captures < 8 {
		t.Fatalf("慢重连阻塞了服务主循环: captures=%d", captures)
	}
}

func TestReconnectUsesBoundedBackoffAndExposesDiagnostics(t *testing.T) {
	driver := &reconnectDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectEvery = 5 * time.Millisecond
	controller.AddDriver(driver)

	controller.mu.Lock()
	status := controller.statuses["reconnect"]
	status.RetryAt = time.Now().Add(time.Hour)
	controller.statuses["reconnect"] = status
	controller.mu.Unlock()
	controller.reconnectDisconnected(context.Background())
	if driver.connects != 0 {
		t.Fatalf("退避窗口内仍重复打开串口: %d", driver.connects)
	}

	controller.mu.Lock()
	status = controller.statuses["reconnect"]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses["reconnect"] = status
	controller.mu.Unlock()
	controller.reconnectDisconnected(context.Background())
	status = controller.Status().Devices[0]
	if driver.connects != 1 || status.ReconnectAttempts != 1 || status.RetryAt.IsZero() || status.LastDisconnectedAt.IsZero() || status.LastConnectionError == "" {
		t.Fatalf("失败重连的退避/诊断状态丢失: connects=%d status=%+v", driver.connects, status)
	}
	lastConnectionError := status.LastConnectionError
	controller.reconnectDisconnected(context.Background())
	if driver.connects != 1 {
		t.Fatalf("失败后立即无界重试: %d", driver.connects)
	}

	controller.mu.Lock()
	status = controller.statuses["reconnect"]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses["reconnect"] = status
	controller.mu.Unlock()
	controller.reconnectDisconnected(context.Background())
	status = controller.Status().Devices[0]
	if driver.connects != 2 || !status.Connected || status.ReconnectAttempts != 0 || !status.RetryAt.IsZero() || status.LastConnectedAt.IsZero() || status.LastConnectionError != lastConnectionError {
		t.Fatalf("重连成功后未复位退避: connects=%d status=%+v", driver.connects, status)
	}

	controller.reconnectEvery = 2 * time.Second
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for index, expected := range want {
		attempt := index + 1
		if index == len(want)-1 {
			attempt = 99
		}
		if got := controller.reconnectDelay(attempt); got != expected {
			t.Errorf("第 %d 次退避: got=%v want=%v", attempt, got, expected)
		}
	}
}

func TestTransferDisconnectAdvancesStrictReconnectBackoff(t *testing.T) {
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectEvery = 2 * time.Second
	now := time.Unix(100, 0)
	status := DeviceStatus{Connected: true}
	status = controller.recordTransferDisconnectLocked(status, errors.New("USB 断开"), now)
	if status.ReconnectAttempts != 1 || !status.RetryAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("传输断开没有进入第一级退避: %+v", status)
	}
	status = controller.recordReconnectFailureLocked(status, errors.New("首次重连失败"), now.Add(2*time.Second))
	if status.ReconnectAttempts != 2 || !status.RetryAt.Equal(now.Add(6*time.Second)) {
		t.Fatalf("首次重连失败后没有进入 4 秒退避: %+v", status)
	}
	for attempt, delay := range []time.Duration{8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		failedAt := status.RetryAt
		status = controller.recordReconnectFailureLocked(status, errors.New("继续失败"), failedAt)
		if got := status.RetryAt.Sub(failedAt); got != delay {
			t.Fatalf("第 %d 级退避错误: got=%v want=%v", attempt+3, got, delay)
		}
	}
}

func TestDisconnectedConnectionStateDriverDoesNotReceiveUnboundedTelemetryWrites(t *testing.T) {
	driver := &connectionAwareDriver{connected: false}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.deviceThemes["connection-aware"] = theme.Manifest{Layers: []theme.Layer{{
		ID: "metrics", Type: theme.LayerTelemetry, Opacity: 1,
	}}}
	for range 10 {
		controller.tick(context.Background())
	}
	if driver.sends != 0 {
		t.Fatalf("断线时仍向失效句柄反复试写: %d", driver.sends)
	}
}

func TestReconnectedSmallTelemetryDriverImmediatelyReceivesCurrentLyrics(t *testing.T) {
	driver := &reconnectDriver{}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.SetLyricsProvider(statusLyrics{})
	manifest := theme.Manifest{
		ID: "reconnect-lyrics", Name: "重连歌词", Width: 1024, Height: 600,
		Layers: []theme.Layer{{
			ID: "lyrics", Type: theme.LayerLyrics,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	controller.activeTheme = &manifest
	controller.connectAll(context.Background())
	controller.mu.Lock()
	reconnectStatus := controller.statuses["reconnect"]
	reconnectStatus.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses["reconnect"] = reconnectStatus
	controller.mu.Unlock()
	controller.reconnectDisconnected(context.Background())

	driver.mu.Lock()
	connects, sends := driver.connects, driver.sends
	driver.mu.Unlock()
	if connects != 2 || sends != 1 {
		t.Fatalf("小包设备重连后没有立即补发歌词: connects=%d sends=%d", connects, sends)
	}
}

func TestStatusIncludesAudioAndLyricsRuntime(t *testing.T) {
	controller := New(&fakeCollector{}, time.Second)
	controller.SetSpectrumProvider(statusSpectrum{})
	controller.SetLyricsProvider(statusLyrics{})
	status := controller.Status()
	if !status.Audio.Enabled || !status.Audio.Running || status.Audio.LastError != "音频诊断" {
		t.Fatalf("音频运行状态无效: %+v", status.Audio)
	}
	if status.Audio.Endpoint != "测试扬声器" || status.Audio.EndpointID != "endpoint-1" || status.Audio.EndpointRole != "console" {
		t.Fatalf("音频输出端点诊断缺失: %+v", status.Audio)
	}
	if !status.Lyrics.Enabled || !status.Lyrics.Running || !status.Lyrics.HasLyrics || status.Lyrics.Provider != "netease" {
		t.Fatalf("歌词运行状态无效: %+v", status.Lyrics)
	}
	if status.Lyrics.Track.Title != "测试歌曲" || status.Lyrics.Track.Artist != "测试歌手" {
		t.Fatalf("当前歌曲状态无效: %+v", status.Lyrics.Track)
	}
}

type detailedStatusSpectrum struct {
	frame   audio.SpectrumFrame
	running bool
}

func (provider detailedStatusSpectrum) Snapshot() audio.SpectrumFrame { return provider.frame }
func (provider detailedStatusSpectrum) Status() audio.EngineStatus {
	return audio.EngineStatus{Running: provider.running}
}

func TestStatusExposesBoundedRealtimeAudioFrameAndClearsWhenDisabled(t *testing.T) {
	bands := make([]float64, audioStatusBandLimit+8)
	for index := range bands {
		bands[index] = float64(index) / float64(len(bands)-1)
	}
	bands[2] = math.NaN()
	capturedAt := time.Now()
	controller := New(&fakeCollector{}, time.Second)
	controller.SetSpectrumProvider(detailedStatusSpectrum{running: true, frame: audio.SpectrumFrame{
		CapturedAt: capturedAt, Level: .25, Bars: bands,
	}})
	status := controller.Status().Audio
	if !status.CapturedAt.Equal(capturedAt) || status.Level != .25 || len(status.Bands) != audioStatusBandLimit {
		t.Fatalf("实时音频状态缺失或未限长: %+v", status)
	}
	if status.Bands[2] != 0 || status.Peak <= 0 || status.Peak > 1 {
		t.Fatalf("音频状态未正确归一化: peak=%v bands[2]=%v", status.Peak, status.Bands[2])
	}
	controller.SetAudioEnabled(false)
	status = controller.Status().Audio
	if status.Enabled || !status.CapturedAt.IsZero() || status.Level != 0 || status.Peak != 0 || len(status.Bands) != 0 {
		t.Fatalf("音频关闭后仍暴露旧频谱: %+v", status)
	}
}

func TestReconnectedHostDriverReceivesRecoveryFrame(t *testing.T) {
	driver := &reconnectHostDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectEvery = 30 * time.Millisecond
	controller.SetRenderInterval(20 * time.Millisecond)
	controller.AddDriver(driver)
	manifest := theme.Manifest{
		ID: "reconnect-frame", Name: "重连帧", Width: 80, Height: 45,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#102030ff"},
		}},
	}
	if err := controller.ApplyTheme(context.Background(), manifest)["reconnect-host"]; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := controller.Run(ctx); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.connects < 2 || driver.frames == 0 {
		t.Fatalf("主机帧设备重连后没有恢复画面: connects=%d frames=%d", driver.connects, driver.frames)
	}
}

func TestConnectRefreshesDeviceInfo(t *testing.T) {
	driver := &infoUpdatingDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	status := controller.Status().Devices[0]
	if !status.Connected || status.Info.Resolution != [2]int{1024, 600} {
		t.Fatalf("连接后没有刷新设备信息: %+v", status)
	}
}

func TestTransferTimeoutKeepsConnectionWhenDriverSessionIsUsable(t *testing.T) {
	driver := &connectionAwareDriver{connected: true}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	status := controller.statuses[driver.Info().ID]
	status.Connected = true
	status.LastConnectedAt = time.Now().Add(-time.Minute)
	controller.statuses[driver.Info().ID] = status

	controller.setTransferError(driver.Info().ID, context.DeadlineExceeded)
	status = controller.Status().Devices[0]
	if !status.Connected || status.LastError == "" || !status.LastDisconnectedAt.IsZero() || status.ReconnectAttempts != 0 {
		t.Fatalf("可用底层会话被调用超时误标为离线: %+v", status)
	}
}

func TestThemeFailureMarksDisconnectedDriverForReconnect(t *testing.T) {
	driver := &themeDisconnectDriver{applyErr: errors.New("主题传输断开")}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	manifest := theme.Manifest{
		ID: "disconnect", Name: "断线测试", Width: 80, Height: 45,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	if err := controller.ApplyTheme(context.Background(), manifest)["theme-disconnect"]; err == nil {
		t.Fatal("主题传输失败没有返回错误")
	}
	status := controller.Status().Devices[0]
	if status.Connected || status.LastError == "" {
		t.Fatalf("底层断开后仍被标记为在线: %+v", status)
	}
}

func TestApplyThemeToFailureKeepsAppliedSpectrumThemeAndError(t *testing.T) {
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	controller.SetSpectrumProvider(&spectrumTestProvider{frame: audio.SpectrumFrame{
		CapturedAt: time.Now(), Bars: []float64{1, .5, .25},
	}})

	oldManifest := spectrumApplyTheme("old-spectrum", 1)
	if err := controller.ApplyThemeTo(context.Background(), "fake", oldManifest)["fake"]; err != nil {
		t.Fatal(err)
	}
	applyErr := errors.New("模拟主题下发失败")
	driver.applyErr = applyErr
	newManifest := spectrumApplyTheme("new-spectrum", 2)
	if err := controller.ApplyThemeTo(context.Background(), "fake", newManifest)["fake"]; !errors.Is(err, applyErr) {
		t.Fatalf("主题下发失败未返回: %v", err)
	}

	if applied := controller.deviceThemes["fake"]; applied.ID != oldManifest.ID {
		t.Fatalf("失败主题被提交为已生效主题: got=%q want=%q", applied.ID, oldManifest.ID)
	}
	controller.sendWaveshareSpectrum(context.Background(), make(map[string]waveshareSpectrumRuntime))
	oldKey := theme.WaveshareAudioBandKey(oldManifest.Layers[1], 0)
	newKey := theme.WaveshareAudioBandKey(newManifest.Layers[1], 0)
	if _, exists := driver.lastValues[oldKey]; !exists {
		t.Fatalf("失败后没有继续使用已生效频谱键 %q: %#v", oldKey, driver.lastValues)
	}
	if _, exists := driver.lastValues[newKey]; exists {
		t.Fatalf("失败后仍下发未生效频谱键 %q: %#v", newKey, driver.lastValues)
	}
	status := controller.Status().Devices[0]
	if status.LastError != applyErr.Error() {
		t.Fatalf("频谱成功 ACK 掩盖了主题下发错误: %+v", status)
	}
}

func TestSuccessfulThemeReloadImmediatelyResendsCurrentState(t *testing.T) {
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	controller.lastSnapshot = telemetry.Snapshot{CapturedAt: time.Now().Add(-time.Second), CPUPercent: 37, MemoryPercent: 52}
	controller.SetSpectrumProvider(&spectrumTestProvider{frame: audio.SpectrumFrame{
		CapturedAt: time.Now(), Bars: []float64{.2, .8, .4},
	}})
	controller.SetLyricsProvider(overlayLyrics{})
	manifest := theme.Manifest{ID: "reload-state", Name: "重载状态", Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
		{ID: "metrics", Type: theme.LayerTelemetry, Bounds: theme.Rect{Width: 1, Height: .3}, Opacity: 1},
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{Y: .3, Width: 1, Height: .3}, Opacity: 1},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Y: .6, Width: 1, Height: .4}, Opacity: 1},
	}}
	if err := controller.ApplyThemeTo(context.Background(), "fake", manifest)["fake"]; err != nil {
		t.Fatal(err)
	}
	if driver.sends != 1 || driver.spectrumSends != 1 {
		t.Fatalf("主题成功后没有立即补发小包和频谱: telemetry=%d spectrum=%d", driver.sends, driver.spectrumSends)
	}
	if driver.lastSend.OmitHardwareMetrics || driver.lastSend.CPUPercent != 37 || driver.lastSend.LyricsCurrent != "当前歌词" {
		t.Fatalf("立即补发的当前状态不完整: %+v", driver.lastSend)
	}
	if allWaveshareSpectrumValuesZero(driver.lastValues) {
		t.Fatalf("新鲜频谱被错误清零: %v", driver.lastValues)
	}
}

func TestThemeReloadUsesIdleBaselineInsteadOfResendingStaleSpectrum(t *testing.T) {
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	controller.SetSpectrumProvider(&spectrumTestProvider{frame: audio.SpectrumFrame{
		CapturedAt: time.Now().Add(-waveshareSpectrumStaleAfter - time.Second), Bars: []float64{1, 1, 1},
	}})
	manifest := spectrumApplyTheme("stale-reload", 1)
	if err := controller.ApplyThemeTo(context.Background(), "fake", manifest)["fake"]; err != nil {
		t.Fatal(err)
	}
	if driver.spectrumSends != 1 || !allWaveshareSpectrumValuesEqual(driver.lastValues, "4") {
		t.Fatalf("主题重载没有用低幅基线取代旧频谱: sends=%d values=%v", driver.spectrumSends, driver.lastValues)
	}
}

func TestThemeReloadClearsLyricsEvenAfterPreviousClearWasAcknowledged(t *testing.T) {
	driver := &fakeDriver{}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	manifest := theme.Manifest{ID: "lyrics-off", Name: "歌词关闭", Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .1, Y: .2, Width: .8, Height: .5}, Opacity: 1},
	}}
	if err := controller.ApplyThemeTo(context.Background(), "fake", manifest)["fake"]; err != nil {
		t.Fatal(err)
	}
	if driver.sends != 1 || !driver.lastSend.LyricsClear {
		t.Fatalf("歌词关闭时重载 Theme 没有清除驻留占位: sends=%d packet=%+v", driver.sends, driver.lastSend)
	}
}

func TestReconnectRefreshFailureRemainsDisconnected(t *testing.T) {
	refreshErr := errors.New("补发当前状态时 USB 断开")
	driver := &refreshDisconnectDriver{connected: true, sendErr: refreshErr}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.statuses[driver.Info().ID] = DeviceStatus{Info: driver.Info(), Connected: true}
	manifest := theme.Manifest{ID: "pending-refresh", Name: "补发失败", Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
		{ID: "metrics", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: .1, Y: .2, Width: .8, Height: .5}, Opacity: 1},
	}}
	if err := controller.ApplyThemeTo(context.Background(), driver.Info().ID, manifest)[driver.Info().ID]; !errors.Is(err, refreshErr) {
		t.Fatalf("首次补发失败未进入 pending: %v", err)
	}
	controller.mu.Lock()
	status := controller.statuses[driver.Info().ID]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses[driver.Info().ID] = status
	controller.mu.Unlock()
	controller.reconnectDisconnected(context.Background())
	status = controller.Status().Devices[0]
	if status.Connected || driver.Connected() || status.ReconnectAttempts < 2 {
		t.Fatalf("pending Theme 补发失败被错误覆盖为在线: status=%+v connected=%v", status, driver.Connected())
	}
}

func TestThemeGenerationDropsOldSpectrumSmoothingRuntime(t *testing.T) {
	driver := &spectrumTestDriver{}
	provider := &spectrumTestProvider{frame: audio.SpectrumFrame{CapturedAt: time.Now(), Bars: []float64{1, 1, 1}}}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	controller.SetSpectrumProvider(provider)
	manifest := spectrumApplyTheme("generation-a", 1)
	manifest.Layers[1].Options["smoothing"] = .98
	if err := controller.ApplyThemeTo(context.Background(), "fake", manifest)["fake"]; err != nil {
		t.Fatal(err)
	}
	runtime := make(map[string]waveshareSpectrumRuntime)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	provider.frame = audio.SpectrumFrame{CapturedAt: time.Now().Add(20 * time.Millisecond), Bars: []float64{.1, .1, .1}}
	manifest.ID = "generation-b"
	if err := controller.ApplyThemeTo(context.Background(), "fake", manifest)["fake"]; err != nil {
		t.Fatal(err)
	}
	provider.frame.CapturedAt = time.Now().Add(40 * time.Millisecond)
	controller.sendWaveshareSpectrum(context.Background(), runtime)
	key := theme.WaveshareAudioBandKey(manifest.Layers[1], 0)
	value, err := strconv.Atoi(driver.lastValues[key])
	if err != nil {
		t.Fatalf("新主题频带值无效: %q", driver.lastValues[key])
	}
	expected := waveshareSpectrumPercent(transformWaveshareSpectrum(provider.frame.Bars, manifest.Layers[1], nil), 0)
	if value != expected {
		t.Fatalf("主题重载后继承了旧 smoothing/peak runtime: key=%s value=%d want=%d", key, value, expected)
	}
}

func TestApplyThemeFailureKeepsActiveTheme(t *testing.T) {
	driver := &spectrumTestDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	oldManifest := spectrumApplyTheme("old-global", 1)
	if err := controller.ApplyTheme(context.Background(), oldManifest)["fake"]; err != nil {
		t.Fatal(err)
	}

	applyErr := errors.New("模拟全局主题下发失败")
	driver.applyErr = applyErr
	newManifest := spectrumApplyTheme("new-global", 2)
	if err := controller.ApplyTheme(context.Background(), newManifest)["fake"]; !errors.Is(err, applyErr) {
		t.Fatalf("全局主题下发失败未返回: %v", err)
	}
	if controller.activeTheme == nil || controller.activeTheme.ID != oldManifest.ID {
		t.Fatalf("下发失败后 activeTheme 被替换: %+v", controller.activeTheme)
	}
	if controller.hostFrames["fake"] {
		t.Fatal("下发失败后 hostFrames 被未生效主题改写")
	}
}

func TestApplyThemePartialSuccessTracksEachAppliedDevice(t *testing.T) {
	succeeded := &namedThemeDriver{id: "succeeded"}
	failed := &namedThemeDriver{id: "failed"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(succeeded)
	controller.AddDriver(failed)
	oldManifest := spectrumApplyTheme("old-partial", 1)
	results := controller.ApplyTheme(context.Background(), oldManifest)
	if results[succeeded.id] != nil || results[failed.id] != nil {
		t.Fatalf("初始主题应用失败: %#v", results)
	}

	failed.applyErr = errors.New("模拟部分设备失败")
	newManifest := spectrumApplyTheme("new-partial", 2)
	results = controller.ApplyTheme(context.Background(), newManifest)
	if results[succeeded.id] != nil || !errors.Is(results[failed.id], failed.applyErr) {
		t.Fatalf("部分应用结果错误: %#v", results)
	}
	if controller.activeTheme == nil || controller.activeTheme.ID != oldManifest.ID {
		t.Fatalf("部分失败后全局主题被提前提交: %+v", controller.activeTheme)
	}
	if controller.deviceThemes[succeeded.id].ID != newManifest.ID {
		t.Fatalf("成功设备未记录实际生效主题: %+v", controller.deviceThemes[succeeded.id])
	}
	if _, exists := controller.deviceThemes[failed.id]; exists {
		t.Fatalf("失败设备被记录为已生效: %+v", controller.deviceThemes[failed.id])
	}
	if controller.pendingDeviceThemes[failed.id].ID != newManifest.ID {
		t.Fatalf("失败设备的待重试主题丢失: %+v", controller.pendingDeviceThemes[failed.id])
	}
}

func TestApplyThemeToFailureKeepsPreviousHostFrameMode(t *testing.T) {
	driver := &fakeHybridDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	oldManifest := theme.Manifest{
		ID: "old-static", Name: "old-static", Width: 160, Height: 90,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	if err := controller.ApplyThemeTo(context.Background(), "hybrid", oldManifest)["hybrid"]; err != nil {
		t.Fatal(err)
	}
	driver.liveErr = errors.New("模拟主机帧主题下发失败")
	newManifest := theme.Manifest{
		ID: "new-live", Name: "new-live", Width: 160, Height: 90,
		Layers: []theme.Layer{
			{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .1, Y: .6, Width: .8, Height: .3}, Opacity: 1},
		},
	}
	if err := controller.ApplyThemeTo(context.Background(), "hybrid", newManifest)["hybrid"]; err == nil {
		t.Fatal("主机帧主题下发失败未返回")
	}
	if controller.deviceThemes["hybrid"].ID != oldManifest.ID || controller.hostFrames["hybrid"] || controller.deviceRenderers["hybrid"] != nil {
		t.Fatalf("下发失败后主题模式被提前提交: theme=%q host=%v renderer=%v",
			controller.deviceThemes["hybrid"].ID, controller.hostFrames["hybrid"], controller.deviceRenderers["hybrid"] != nil)
	}
}

func TestPendingThemeCommitsOnlyAfterReconnectApplySucceeds(t *testing.T) {
	driver := &themeDisconnectDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	oldManifest := spectrumApplyTheme("old-reconnect", 1)
	if err := controller.ApplyThemeTo(context.Background(), "theme-disconnect", oldManifest)["theme-disconnect"]; err != nil {
		t.Fatal(err)
	}
	driver.applyErr = errors.New("模拟首次下发断开")
	newManifest := spectrumApplyTheme("new-reconnect", 2)
	if err := controller.ApplyThemeTo(context.Background(), "theme-disconnect", newManifest)["theme-disconnect"]; err == nil {
		t.Fatal("首次主题下发应失败")
	}
	if controller.deviceThemes["theme-disconnect"].ID != oldManifest.ID || controller.pendingDeviceThemes["theme-disconnect"].ID != newManifest.ID {
		t.Fatalf("失败主题没有与已生效主题隔离: applied=%q pending=%q",
			controller.deviceThemes["theme-disconnect"].ID, controller.pendingDeviceThemes["theme-disconnect"].ID)
	}
	controller.mu.Lock()
	reconnectStatus := controller.statuses["theme-disconnect"]
	reconnectStatus.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses["theme-disconnect"] = reconnectStatus
	controller.mu.Unlock()
	driver.applyErr = nil
	controller.reconnectDisconnected(context.Background())
	if controller.deviceThemes["theme-disconnect"].ID != newManifest.ID {
		t.Fatalf("重连下发成功后未提交 pending 主题: %q", controller.deviceThemes["theme-disconnect"].ID)
	}
	if _, exists := controller.pendingDeviceThemes["theme-disconnect"]; exists {
		t.Fatal("重连成功后 pending 主题未清理")
	}
	status := controller.Status().Devices[0]
	if !status.Connected || status.LastError != "" {
		t.Fatalf("重连应用主题成功后状态未恢复: %+v", status)
	}
}

func TestReconnectCannotOverwriteNewConcurrentTheme(t *testing.T) {
	driver := &orderedThemeDriver{oldStarted: make(chan struct{}), releaseOld: make(chan struct{})}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	oldManifest := spectrumApplyTheme("old-pending", 1)
	newManifest := spectrumApplyTheme("new-user-theme", 2)
	controller.rememberPendingTheme("ordered-theme", oldManifest)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reconnectDone := make(chan struct{})
	go func() {
		controller.reconnectDriver(ctx, driver)
		close(reconnectDone)
	}()
	select {
	case <-driver.oldStarted:
	case <-ctx.Done():
		t.Fatal("重连没有开始下发旧待重试主题")
	}

	userResult := make(chan error, 1)
	go func() {
		userResult <- controller.ApplyThemeTo(ctx, "ordered-theme", newManifest)["ordered-theme"]
	}()
	select {
	case err := <-userResult:
		t.Fatalf("新主题越过正在进行的重连操作提前完成: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(driver.releaseOld)
	select {
	case <-reconnectDone:
	case <-ctx.Done():
		t.Fatal("重连没有完成")
	}
	if err := <-userResult; err != nil {
		t.Fatalf("用户新主题应用失败: %v", err)
	}

	controller.mu.RLock()
	committed := controller.deviceThemes["ordered-theme"].ID
	_, pending := controller.pendingDeviceThemes["ordered-theme"]
	controller.mu.RUnlock()
	driver.mu.Lock()
	applied := append([]string(nil), driver.applied...)
	driver.mu.Unlock()
	if committed != newManifest.ID || pending {
		t.Fatalf("旧重连覆盖了用户新主题: committed=%q pending=%v", committed, pending)
	}
	if len(applied) != 2 || applied[0] != oldManifest.ID || applied[1] != newManifest.ID {
		t.Fatalf("同一设备主题操作没有严格串行: %#v", applied)
	}
}

func TestRunCancelsAndWaitsForExternalThemeBeforeClosingDriver(t *testing.T) {
	driver := &shutdownThemeDriver{applyStarted: make(chan struct{}), applyDone: make(chan struct{})}
	controller := New(&fakeCollector{}, time.Second)
	controller.healthCheckEvery = time.Hour
	controller.AddDriver(driver)
	runContext, stopRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- controller.Run(runContext) }()

	deadline := time.Now().Add(time.Second)
	for !controller.Status().Devices[0].Connected && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !controller.Status().Devices[0].Connected {
		stopRun()
		t.Fatal("测试驱动没有完成初始连接")
	}
	manifest := spectrumApplyTheme("shutdown-in-flight", 1)
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- controller.ApplyThemeTo(context.Background(), "shutdown-theme", manifest)["shutdown-theme"]
	}()
	select {
	case <-driver.applyStarted:
	case <-time.After(time.Second):
		stopRun()
		t.Fatal("外部主题操作没有启动")
	}
	stopRun()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("服务退出没有取消并等待主题操作")
	}
	select {
	case <-driver.applyDone:
	case <-time.After(time.Second):
		t.Fatal("主题操作在服务退出后仍未结束")
	}
	if err := <-applyDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("主题操作没有收到服务退出信号: %v", err)
	}
	driver.mu.Lock()
	closeDuringApply := driver.closeDuringApply
	driver.mu.Unlock()
	if closeDuringApply {
		t.Fatal("驱动在主题操作仍持有资源时被关闭")
	}
}

func spectrumApplyTheme(id string, sensitivity float64) theme.Manifest {
	return theme.Manifest{
		ID: id, Name: id, Width: 1024, Height: 600,
		Layers: []theme.Layer{
			{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .1, Y: .6, Width: .8, Height: .3}, Opacity: 1,
				Options: map[string]any{"sensitivity": sensitivity}},
		},
	}
}

func TestApplyThemeToTargetsOneDevice(t *testing.T) {
	driver := &fakeHybridDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	manifest := theme.Manifest{
		ID: "targeted", Name: "目标副屏主题", Width: 160, Height: 90,
		Layers: []theme.Layer{
			{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .6}, Opacity: .8},
		},
	}
	if err := controller.ApplyThemeTo(context.Background(), "hybrid", manifest)["hybrid"]; err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	liveApplies := driver.liveApplies
	driver.mu.Unlock()
	if liveApplies != 1 || !controller.hostFrames["hybrid"] || controller.deviceRenderers["hybrid"] == nil {
		t.Fatalf("目标主题没有只应用到选中设备: live=%d host=%v renderer=%v", liveApplies, controller.hostFrames["hybrid"], controller.deviceRenderers["hybrid"] != nil)
	}
	controller.closeRenderer()
}
