package audio

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

type cancellableSource struct {
	started chan struct{}
	stopped chan struct{}
	once    sync.Once
}

type restartingSource struct {
	mu        sync.Mutex
	attempts  int
	restarted chan struct{}
}

type endpointChangingSource struct {
	mu        sync.Mutex
	attempts  int
	restarted chan struct{}
}

func (source *restartingSource) Capture(ctx context.Context, _ func(PCMFrame) error) error {
	source.mu.Lock()
	source.attempts++
	attempt := source.attempts
	source.mu.Unlock()
	if attempt == 1 {
		return errors.New("模拟音频采集异常")
	}
	if attempt == 2 {
		close(source.restarted)
	}
	<-ctx.Done()
	return nil
}

func (source *cancellableSource) Capture(ctx context.Context, _ func(PCMFrame) error) error {
	source.once.Do(func() { close(source.started) })
	<-ctx.Done()
	select {
	case <-source.stopped:
	default:
		close(source.stopped)
	}
	return nil
}

func (source *endpointChangingSource) Capture(ctx context.Context, _ func(PCMFrame) error) error {
	source.mu.Lock()
	source.attempts++
	attempt := source.attempts
	source.mu.Unlock()
	if attempt == 1 {
		return errDefaultOutputEndpointChanged
	}
	if attempt == 2 {
		close(source.restarted)
	}
	<-ctx.Done()
	return nil
}

func TestEngineDisableCancelsCapture(t *testing.T) {
	source := &cancellableSource{started: make(chan struct{}), stopped: make(chan struct{})}
	engine := NewEngine(source, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()

	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("音频采集没有启动")
	}
	initial := engine.Snapshot()
	if initial.CapturedAt.IsZero() || len(initial.Bars) != 32 || maximumSpectrumBar(initial.Bars) != 0 {
		t.Fatalf("空闲驱动启动后未发布清零帧: %#v", initial)
	}
	engine.SetEnabled(false)
	select {
	case <-source.stopped:
	case <-time.After(time.Second):
		t.Fatal("关闭音频后没有取消采集")
	}
	deadline := time.Now().Add(time.Second)
	for engine.Status().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if engine.Status().Running {
		t.Fatal("关闭音频后状态仍为运行中")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("音频引擎没有退出")
	}
}

func TestEngineRestartsSourceAfterCaptureError(t *testing.T) {
	source := &restartingSource{restarted: make(chan struct{})}
	engine := NewEngine(source, 32)
	engine.retryDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()

	select {
	case <-source.restarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("音频采集错误后 Engine 没有重启音频源")
	}
	if status := engine.Status(); !status.Running || status.LastError != "" {
		cancel()
		t.Fatalf("重连后的音频状态错误: %+v", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("音频引擎没有退出")
	}
}

func TestEngineImmediatelyReconnectsAfterDefaultEndpointChange(t *testing.T) {
	source := &endpointChangingSource{restarted: make(chan struct{})}
	engine := NewEngine(source, 32)
	// 保留正式的慢错误重试间隔，证明端点切换走的是立即重连。
	engine.retryDelay = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	select {
	case <-source.restarted:
	case <-time.After(250 * time.Millisecond):
		cancel()
		t.Fatal("默认输出端点切换后没有立即重建回环采集")
	}
	if status := engine.Status(); !status.Running || status.LastError != "" {
		cancel()
		t.Fatalf("正常端点切换被长期标记为异常: %+v", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("音频引擎没有退出")
	}
}

func TestEngineSnapshotClearsWhenDriverStopsSendingSilentPackets(t *testing.T) {
	engine := NewEngine(nil, 16)
	firstAt := time.Unix(20, 0)
	samples := make([]float64, 4096)
	for index := range samples {
		samples[index] = 0.7 * math.Sin(2*math.Pi*440*float64(index)/48000)
	}
	if err := engine.consume(PCMFrame{CapturedAt: firstAt, SampleRate: 48000, Samples: samples}); err != nil {
		t.Fatal(err)
	}
	active := engine.snapshotAt(firstAt.Add(40 * time.Millisecond))
	stale := engine.snapshotAt(firstAt.Add(500 * time.Millisecond))
	if maximumSpectrumBar(active.Bars) < 0.5 {
		t.Fatalf("测试音未生成有效频谱: %#v", active)
	}
	if stale.Level != 0 || maximumSpectrumBar(stale.Bars) != 0 {
		t.Fatalf("WASAPI 无新包时旧频谱没有立即清零: active=%.4f stale=%.4f", maximumSpectrumBar(active.Bars), maximumSpectrumBar(stale.Bars))
	}
	if !stale.CapturedAt.Equal(firstAt.Add(500 * time.Millisecond)) {
		t.Fatalf("衰减帧未推进时间戳: %v", stale.CapturedAt)
	}
}

func TestEngineDoesNotRestoreOldPeakWhenPacketsResumeAfterSilence(t *testing.T) {
	engine := NewEngine(nil, 16)
	firstAt := time.Unix(30, 0)
	samples := make([]float64, 4096)
	for index := range samples {
		samples[index] = 0.7 * math.Sin(2*math.Pi*440*float64(index)/48000)
	}
	if err := engine.consume(PCMFrame{CapturedAt: firstAt, SampleRate: 48000, Samples: samples}); err != nil {
		t.Fatal(err)
	}
	if err := engine.consume(PCMFrame{CapturedAt: firstAt.Add(time.Second), SampleRate: 48000, Samples: make([]float64, 4096)}); err != nil {
		t.Fatal(err)
	}
	frame := engine.snapshotAt(firstAt.Add(time.Second))
	if maximumSpectrumBar(frame.Bars) >= 0.01 {
		t.Fatalf("恢复静音包后旧波峰回弹: %.6f", maximumSpectrumBar(frame.Bars))
	}
}

func TestEngineResetsOldWindowBeforeShortPacketAfterCaptureGap(t *testing.T) {
	engine := NewEngine(nil, 16)
	firstAt := time.Unix(40, 0)
	tone := make([]float64, 4096)
	for index := range tone {
		tone[index] = 0.7 * math.Sin(2*math.Pi*440*float64(index)/48000)
	}
	if err := engine.consume(PCMFrame{CapturedAt: firstAt, SampleRate: 48000, Samples: tone}); err != nil {
		t.Fatal(err)
	}
	resumeAt := firstAt.Add(time.Second)
	if err := engine.consume(PCMFrame{CapturedAt: resumeAt, SampleRate: 48000, Samples: make([]float64, 480)}); err != nil {
		t.Fatal(err)
	}
	frame := engine.snapshotAt(resumeAt)
	if frame.Level != 0 || maximumSpectrumBar(frame.Bars) != 0 {
		t.Fatalf("停包后首个短静音包恢复了旧波峰: level=%.6f peak=%.6f", frame.Level, maximumSpectrumBar(frame.Bars))
	}
	if err := engine.consume(PCMFrame{CapturedAt: resumeAt.Add(10 * time.Millisecond), SampleRate: 48000, Samples: make([]float64, 544)}); err != nil {
		t.Fatal(err)
	}
	frame = engine.snapshotAt(resumeAt.Add(10 * time.Millisecond))
	if frame.Level != 0 || maximumSpectrumBar(frame.Bars) != 0 {
		t.Fatalf("停包后的短静音窗口仍混入旧样本: level=%.6f peak=%.6f", frame.Level, maximumSpectrumBar(frame.Bars))
	}
}

func TestEngineResetsAnalyzerOnReportedDiscontinuity(t *testing.T) {
	engine := NewEngine(nil, 16)
	firstAt := time.Unix(50, 0)
	tone := make([]float64, 4096)
	for index := range tone {
		tone[index] = 0.7 * math.Sin(2*math.Pi*440*float64(index)/48000)
	}
	if err := engine.consume(PCMFrame{CapturedAt: firstAt, SampleRate: 48000, Samples: tone}); err != nil {
		t.Fatal(err)
	}
	discontinuousAt := firstAt.Add(40 * time.Millisecond)
	if err := engine.consume(PCMFrame{
		CapturedAt: discontinuousAt, SampleRate: 48000, Samples: make([]float64, 480), Discontinuity: true,
	}); err != nil {
		t.Fatal(err)
	}
	frame := engine.snapshotAt(discontinuousAt)
	if frame.Level != 0 || maximumSpectrumBar(frame.Bars) != 0 {
		t.Fatalf("驱动报告断流后仍保留旧波峰: level=%.6f peak=%.6f", frame.Level, maximumSpectrumBar(frame.Bars))
	}
}
