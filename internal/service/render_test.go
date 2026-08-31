package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"casescreen/internal/audio"
	"casescreen/internal/device"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type fakeHostDriver struct {
	mu             sync.Mutex
	frames         int
	telemetrySends int
	payloads       [][]byte
	frameSignal    chan struct{}
}

type fakeHybridDriver struct {
	mu             sync.Mutex
	offlineApplies int
	liveApplies    int
	frames         int
	offlineErr     error
	liveErr        error
}

func (driver *fakeHybridDriver) Info() device.Info {
	return device.Info{
		ID: "hybrid", Name: "混合模式测试", Resolution: [2]int{160, 90},
		Capabilities: device.Capabilities{
			DeliveryMode: device.DeliveryHostFrame, StoresResources: true, HostRenderedFrames: true,
		},
	}
}

func (*fakeHybridDriver) Connect(context.Context) error { return nil }
func (*fakeHybridDriver) Close() error                  { return nil }
func (*fakeHybridDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (driver *fakeHybridDriver) UseHostFrames(manifest theme.Manifest) bool {
	for _, layer := range manifest.Layers {
		if layer.Type == theme.LayerAudioBars || layer.Type == theme.LayerAudioRing || layer.Type == theme.LayerLyrics || layer.Type == theme.LayerTelemetry {
			return true
		}
	}
	return false
}
func (driver *fakeHybridDriver) ApplyTheme(context.Context, theme.Manifest) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.offlineApplies++
	return driver.offlineErr
}
func (driver *fakeHybridDriver) ApplyHostTheme(context.Context, theme.Manifest) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.liveApplies++
	return driver.liveErr
}
func (driver *fakeHybridDriver) SendJPEG(context.Context, []byte) error {
	driver.mu.Lock()
	driver.frames++
	driver.mu.Unlock()
	return nil
}

func TestEncodeJPEGWithinLimitReducesQuality(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < canvas.Bounds().Dy(); y++ {
		for x := 0; x < canvas.Bounds().Dx(); x++ {
			value := uint8((x*73 + y*151 + x*y*17) & 0xff)
			canvas.SetRGBA(x, y, color.RGBA{R: value, G: value ^ uint8(x), B: value ^ uint8(y), A: 255})
		}
	}
	var high bytes.Buffer
	if err := jpeg.Encode(&high, canvas, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	limit := high.Len() * 3 / 4
	data, usedQuality, err := encodeJPEGWithinLimit(canvas, 92, limit)
	if err != nil {
		t.Fatal(err)
	}
	if usedQuality >= 92 {
		t.Fatalf("期望降低 JPEG 质量，实际仍为 %d", usedQuality)
	}
	if len(data) > limit {
		t.Fatalf("JPEG 大小 %d 超过限制 %d", len(data), limit)
	}
}

func (driver *fakeHostDriver) Info() device.Info {
	return device.Info{
		ID: "host", Name: "主机帧测试", Resolution: [2]int{160, 90},
		Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame, HostRenderedFrames: true, SmallTelemetry: true},
	}
}

func (driver *fakeHostDriver) Connect(context.Context) error                    { return nil }
func (driver *fakeHostDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (driver *fakeHostDriver) Close() error                                     { return nil }
func (driver *fakeHostDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	driver.mu.Lock()
	driver.telemetrySends++
	driver.mu.Unlock()
	return nil
}
func (driver *fakeHostDriver) SendJPEG(_ context.Context, data []byte) error {
	driver.mu.Lock()
	if len(data) > 0 {
		driver.frames++
		driver.payloads = append(driver.payloads, append([]byte(nil), data...))
	}
	driver.mu.Unlock()
	if driver.frameSignal != nil {
		select {
		case driver.frameSignal <- struct{}{}:
		default:
		}
	}
	return nil
}

type advancingPeakSpectrum struct {
	calls int
	base  time.Time
}

func (provider *advancingPeakSpectrum) Snapshot() audio.SpectrumFrame {
	provider.calls++
	value := .1
	if provider.calls == 1 {
		value = 1
	}
	return audio.SpectrumFrame{
		CapturedAt: provider.base.Add(time.Duration(provider.calls) * time.Second),
		Level:      value,
		Bars:       []float64{value, value, value, value, value, value},
	}
}

func TestHostFramesAdvancePeakDecayForEqualLowBands(t *testing.T) {
	driver := &fakeHostDriver{frameSignal: make(chan struct{}, 4)}
	controller := New(&fakeCollector{}, time.Minute)
	t.Cleanup(controller.stopAllHostFrameWorkers)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	manifest := theme.Manifest{ID: "peak-time", Name: "峰值时间状态", Width: 160, Height: 90, Layers: []theme.Layer{
		{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "#081828ff"}},
		{
			ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .05, Y: .1, Width: .9, Height: .8}, Opacity: 1,
			Options: map[string]any{
				"count": 6, "smoothing": 0.0, "noise_gate": 0.0,
				"gain": 1.0, "sensitivity": 1.0, "contrast": 1.0, "separation": 0.0,
				"peak_hold": true, "peak_hold_ms": 0.0, "peak_decay": .25,
			},
		},
	}}
	if err := controller.ApplyTheme(context.Background(), manifest)["host"]; err != nil {
		t.Fatal(err)
	}
	controller.SetSpectrumProvider(&advancingPeakSpectrum{base: time.Unix(100, 0)})
	for range 4 {
		controller.renderHostFrames(context.Background())
		select {
		case <-driver.frameSignal:
		case <-time.After(time.Second):
			t.Fatal("等待异步主机帧发送超时")
		}
	}
	driver.mu.Lock()
	payloads := append([][]byte(nil), driver.payloads...)
	driver.mu.Unlock()
	if len(payloads) != 4 {
		t.Fatalf("相同低频带冻结了峰值时间状态: frames=%d status=%+v renderer=%v host=%v", len(payloads), controller.Status().Devices, controller.renderer != nil, controller.hostFrames["host"])
	}
	for index := 1; index < len(payloads); index++ {
		if bytes.Equal(payloads[index-1], payloads[index]) {
			t.Fatalf("第 %d 帧峰值没有随 Spectrum.CapturedAt 衰减", index+1)
		}
	}
}

func TestHostFramesContinueWhenTelemetryDisabled(t *testing.T) {
	directory := t.TempDir()
	backgroundPath := filepath.Join(directory, "background.png")
	background := image.NewRGBA(image.Rect(0, 0, 160, 90))
	for y := 0; y < 90; y++ {
		for x := 0; x < 160; x++ {
			background.SetRGBA(x, y, color.RGBA{R: uint8(20 + x/3), G: 40, B: uint8(80 + y), A: 255})
		}
	}
	file, err := os.Create(backgroundPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, background); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	resource, err := theme.InspectResource("background", backgroundPath, "")
	if err != nil {
		t.Fatal(err)
	}
	manifest := theme.Manifest{
		ID: "host-test", Name: "主机帧测试", Width: 160, Height: 90,
		Resources: []theme.Resource{resource},
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"resource_id": "background"},
		}},
	}
	collector := &fakeCollector{}
	driver := &fakeHostDriver{}
	controller := New(collector, 5*time.Millisecond)
	controller.AddDriver(driver)
	controller.SetTelemetryEnabled(false)
	controller.SetRenderInterval(20 * time.Millisecond)
	results := controller.ApplyTheme(context.Background(), manifest)
	if results["host"] != nil {
		t.Fatal(results["host"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Millisecond)
	defer cancel()
	if err := controller.Run(ctx); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if collector.count != 0 || driver.telemetrySends != 0 {
		t.Fatalf("关闭遥测后仍采集或发送小包: capture=%d send=%d", collector.count, driver.telemetrySends)
	}
	if driver.frames == 0 {
		t.Fatal("关闭遥测不应停止主机帧图层渲染")
	}
	if driver.frames > 1 {
		t.Fatalf("静态画面没有变化却重复发送了 %d 帧", driver.frames)
	}
}

func TestHybridDriverKeepsStaticThemeOffline(t *testing.T) {
	driver := &fakeHybridDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	manifest := theme.Manifest{
		ID: "offline", Name: "离线主题", Width: 160, Height: 90,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#081828ff"},
		}},
	}
	if err := controller.ApplyTheme(context.Background(), manifest)["hybrid"]; err != nil {
		t.Fatal(err)
	}
	if controller.renderer != nil || controller.hostFrames["hybrid"] {
		t.Fatal("纯背景主题不应启用主机实时渲染")
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.offlineApplies != 1 || driver.liveApplies != 0 || driver.frames != 0 {
		t.Fatalf("离线主题调度无效: %+v", driver)
	}
}

func TestHybridDriverUsesHostFramesForDynamicTheme(t *testing.T) {
	driver := &fakeHybridDriver{}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	manifest := theme.Manifest{
		ID: "dynamic", Name: "动态主题", Width: 160, Height: 90,
		Layers: []theme.Layer{
			{
				ID: "background", Type: theme.LayerBackgroundImage,
				Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
				Options: map[string]any{"color": "#081828ff"},
			},
			{
				ID: "bars", Type: theme.LayerAudioBars,
				Bounds: theme.Rect{X: 0.1, Y: 0.65, Width: 0.8, Height: 0.25}, Opacity: 0.8,
			},
		},
	}
	if err := controller.ApplyTheme(context.Background(), manifest)["hybrid"]; err != nil {
		t.Fatal(err)
	}
	if controller.renderer == nil || !controller.hostFrames["hybrid"] {
		t.Fatal("动态主题没有启用主机实时渲染")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer cancel()
	if err := controller.Run(ctx); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.offlineApplies != 0 || driver.liveApplies != 1 || driver.frames == 0 {
		t.Fatalf("动态主题调度无效: %+v", driver)
	}
}
