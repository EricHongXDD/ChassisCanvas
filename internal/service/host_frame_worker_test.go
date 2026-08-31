package service

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type slowHostFrameDriver struct {
	id string

	started   chan struct{}
	release   chan struct{}
	completed chan []byte
	startOnce sync.Once

	mu            sync.Mutex
	calls         [][]byte
	concurrent    int
	maxConcurrent int
	telemetry     atomic.Int32
	sendErr       error
}

func (driver *slowHostFrameDriver) Info() device.Info {
	return device.Info{
		ID: driver.id, Name: "慢主机帧测试", Resolution: [2]int{80, 45},
		Capabilities: device.Capabilities{
			DeliveryMode: device.DeliveryHostFrame, HostRenderedFrames: true, SmallTelemetry: true,
		},
	}
}

func (*slowHostFrameDriver) Connect(context.Context) error                    { return nil }
func (*slowHostFrameDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*slowHostFrameDriver) ApplyHostTheme(context.Context, theme.Manifest) error {
	return nil
}
func (*slowHostFrameDriver) UseHostFrames(manifest theme.Manifest) bool {
	return manifestHasLayer(manifest, theme.LayerTelemetry) || manifestHasLayer(manifest, theme.LayerAudioBars) ||
		manifestHasLayer(manifest, theme.LayerAudioRing) || manifestHasLayer(manifest, theme.LayerLyrics)
}
func (*slowHostFrameDriver) Close() error { return nil }
func (driver *slowHostFrameDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	driver.telemetry.Add(1)
	return nil
}

func (driver *slowHostFrameDriver) SendJPEG(ctx context.Context, data []byte) error {
	payload := append([]byte(nil), data...)
	driver.mu.Lock()
	driver.calls = append(driver.calls, payload)
	call := len(driver.calls)
	driver.concurrent++
	if driver.concurrent > driver.maxConcurrent {
		driver.maxConcurrent = driver.concurrent
	}
	driver.mu.Unlock()
	driver.startOnce.Do(func() { close(driver.started) })

	if call == 1 {
		select {
		case <-driver.release:
		case <-ctx.Done():
			driver.finishCall()
			return ctx.Err()
		}
	}
	driver.finishCall()
	if call == 1 && driver.sendErr != nil {
		return driver.sendErr
	}
	select {
	case driver.completed <- payload:
	default:
	}
	return nil
}

func (driver *slowHostFrameDriver) finishCall() {
	driver.mu.Lock()
	driver.concurrent--
	driver.mu.Unlock()
}

type hostFrameCountingCollector struct {
	captures atomic.Int32
}

type retryingHostFrameDriver struct {
	id string

	mu        sync.Mutex
	connected bool
	calls     int
	failures  int
}

func (driver *retryingHostFrameDriver) Info() device.Info {
	return device.Info{
		ID: driver.id, Name: "主机帧重试测试", Resolution: [2]int{80, 45},
		Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame, HostRenderedFrames: true},
	}
}

func (driver *retryingHostFrameDriver) Connect(context.Context) error {
	driver.mu.Lock()
	driver.connected = true
	driver.mu.Unlock()
	return nil
}

func (driver *retryingHostFrameDriver) Connected() bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.connected
}

func (*retryingHostFrameDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*retryingHostFrameDriver) ApplyHostTheme(context.Context, theme.Manifest) error {
	return nil
}
func (*retryingHostFrameDriver) UseHostFrames(theme.Manifest) bool { return true }
func (*retryingHostFrameDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}

func (driver *retryingHostFrameDriver) SendJPEG(context.Context, []byte) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.calls++
	if driver.calls <= driver.failures {
		return context.DeadlineExceeded
	}
	return nil
}

func (driver *retryingHostFrameDriver) Close() error {
	driver.mu.Lock()
	driver.connected = false
	driver.mu.Unlock()
	return nil
}

func (collector *hostFrameCountingCollector) Capture() (telemetry.Snapshot, error) {
	sequence := collector.captures.Add(1)
	return telemetry.Snapshot{CapturedAt: time.Unix(0, int64(sequence)), CPUPercent: float64(sequence)}, nil
}

func TestHostFrameWorkerRetriesTransientFirstFrameFailure(t *testing.T) {
	driver := &retryingHostFrameDriver{id: "retry-first-frame", failures: 1}
	controller := New(&fakeCollector{}, time.Hour)
	controller.hostFrameRetryDelay = 5 * time.Millisecond
	controller.renderInterval = time.Hour
	controller.AddDriver(driver)
	manifest := theme.Manifest{
		ID: "retry-static", Name: "静态实时主题", Width: 80, Height: 45,
		Layers: []theme.Layer{
			{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "#102030ff"}},
			{ID: "telemetry", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .8}, Opacity: 1},
		},
	}
	if err := controller.SetInitialTheme(manifest); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	waitForHostFrameCondition(t, func() bool {
		driver.mu.Lock()
		calls := driver.calls
		driver.mu.Unlock()
		return calls >= 2 && controller.HostFrameStats(driver.id).Sent == 1
	})
	status := controller.Status().Devices[0]
	if !status.Connected || status.LastError != "" {
		t.Fatalf("瞬时 ACK 超时重试成功后状态未恢复: %+v", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("停止主机帧重试测试超时")
	}
}

func TestHostFrameWorkerKeepsOnlyLatestFrameAndSerializesPerDevice(t *testing.T) {
	driver := &slowHostFrameDriver{
		id: "latest", started: make(chan struct{}), release: make(chan struct{}), completed: make(chan []byte, 4),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.Connected = true
	controller.statuses[driver.id] = status
	controller.hostFrames[driver.id] = true
	generation := controller.hostFrameGenerations[driver.id]
	controller.mu.Unlock()
	t.Cleanup(controller.stopAllHostFrameWorkers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !controller.enqueueHostFrame(ctx, driver.id, driver, generation, []byte{1}) {
		t.Fatal("首帧未入队")
	}
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("慢发送未开始")
	}
	for _, marker := range []byte{2, 3, 4} {
		if !controller.enqueueHostFrame(ctx, driver.id, driver, generation, []byte{marker}) {
			t.Fatalf("最新帧 %d 未入队", marker)
		}
	}
	close(driver.release)
	for range 2 {
		select {
		case <-driver.completed:
		case <-time.After(time.Second):
			t.Fatal("等待主机帧完成超时")
		}
	}
	waitForHostFrameCondition(t, func() bool { return controller.HostFrameStats(driver.id).Sent == 2 })

	driver.mu.Lock()
	calls := append([][]byte(nil), driver.calls...)
	maxConcurrent := driver.maxConcurrent
	driver.mu.Unlock()
	if len(calls) != 2 || !bytes.Equal(calls[0], []byte{1}) || !bytes.Equal(calls[1], []byte{4}) {
		t.Fatalf("容量 1 队列没有只保留最新帧: calls=%v", calls)
	}
	stats := controller.HostFrameStats(driver.id)
	if maxConcurrent != 1 || stats.Sent != 2 || stats.Dropped != 2 || stats.LastSendAt.IsZero() {
		t.Fatalf("主机帧串行化或统计无效: concurrent=%d stats=%+v", maxConcurrent, stats)
	}
}

func TestSlowHostFrameSendDoesNotBlockServiceLoop(t *testing.T) {
	driver := &slowHostFrameDriver{
		id: "slow-loop", started: make(chan struct{}), release: make(chan struct{}), completed: make(chan []byte, 4),
	}
	collector := &hostFrameCountingCollector{}
	controller := New(collector, 3*time.Millisecond)
	controller.SetRenderInterval(2 * time.Millisecond)
	controller.AddDriver(driver)
	manifest := theme.Manifest{
		ID: "slow-loop", Name: "慢发送不阻塞主循环", Width: 80, Height: 45,
		Layers: []theme.Layer{
			{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "#102030ff"}},
			{ID: "telemetry", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: .05, Y: .05, Width: .9, Height: .9}, Opacity: 1},
		},
	}
	if err := controller.ApplyTheme(context.Background(), manifest)[driver.id]; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("首帧未开始发送")
	}
	waitForHostFrameCondition(t, func() bool {
		return collector.captures.Load() >= 3 && driver.telemetry.Load() >= 3 && controller.HostFrameStats(driver.id).Dropped > 0
	})
	select {
	case err := <-done:
		t.Fatalf("慢 SendJPEG 期间服务提前退出: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后主机帧 worker 未安全退出")
	}
}

func TestHostFrameThemeSwitchCancelsInflightAndDropsQueuedOldFrame(t *testing.T) {
	driver := &slowHostFrameDriver{
		id: "theme-switch", started: make(chan struct{}), release: make(chan struct{}), completed: make(chan []byte, 4),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	dynamic := theme.Manifest{
		ID: "dynamic", Name: "实时主题", Width: 80, Height: 45,
		Layers: []theme.Layer{
			{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "#102030ff"}},
			{ID: "telemetry", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .8}, Opacity: .8},
		},
	}
	if err := controller.ApplyThemeTo(context.Background(), driver.id, dynamic)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.renderHostFrames(context.Background())
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("换主题前的首帧未开始")
	}
	controller.mu.RLock()
	generation := controller.hostFrameGenerations[driver.id]
	controller.mu.RUnlock()
	if !controller.enqueueHostFrame(context.Background(), driver.id, driver, generation, []byte{9}) {
		t.Fatal("换主题前的待发帧未入队")
	}
	static := theme.Manifest{
		ID: "static", Name: "离线主题", Width: 80, Height: 45,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#203040ff"},
		}},
	}
	if err := controller.ApplyThemeTo(context.Background(), driver.id, static)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.mu.RLock()
	active := controller.hostFrames[driver.id]
	worker := controller.hostFrameWorkers[driver.id]
	controller.mu.RUnlock()
	driver.mu.Lock()
	calls := len(driver.calls)
	driver.mu.Unlock()
	stats := controller.HostFrameStats(driver.id)
	if active || worker != nil || calls != 1 || stats.Sent != 0 || stats.Dropped != 2 {
		t.Fatalf("换主题后仍保留或补发旧帧: active=%v worker=%v calls=%d stats=%+v", active, worker != nil, calls, stats)
	}
}

func TestHostFrameDisconnectDropsPendingFrame(t *testing.T) {
	sendErr := errors.New("测试 USB 断开")
	driver := &slowHostFrameDriver{
		id: "disconnect", started: make(chan struct{}), release: make(chan struct{}), completed: make(chan []byte, 4), sendErr: sendErr,
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.Connected = true
	controller.statuses[driver.id] = status
	controller.hostFrames[driver.id] = true
	generation := controller.hostFrameGenerations[driver.id]
	controller.mu.Unlock()
	t.Cleanup(controller.stopAllHostFrameWorkers)

	if !controller.enqueueHostFrame(context.Background(), driver.id, driver, generation, []byte{1}) {
		t.Fatal("断线测试首帧未入队")
	}
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("断线测试首帧未开始")
	}
	if !controller.enqueueHostFrame(context.Background(), driver.id, driver, generation, []byte{2}) {
		t.Fatal("断线前待发帧未入队")
	}
	close(driver.release)
	waitForHostFrameCondition(t, func() bool {
		status := controller.Status().Devices[0]
		return !status.Connected && status.LastError == sendErr.Error() && controller.HostFrameStats(driver.id).Dropped == 2
	})
	driver.mu.Lock()
	calls := len(driver.calls)
	driver.mu.Unlock()
	if calls != 1 {
		t.Fatalf("断线后仍补发旧帧: calls=%d", calls)
	}
}

func waitForHostFrameCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("等待主机帧条件超时")
		case <-ticker.C:
		}
	}
}
