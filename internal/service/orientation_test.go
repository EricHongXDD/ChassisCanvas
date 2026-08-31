package service

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type fakeTransformingHostDriver struct {
	id             string
	transformKey   string
	transformColor color.RGBA
	frameSignal    chan struct{}

	mu             sync.Mutex
	payload        []byte
	transformCalls int
}

func (driver *fakeTransformingHostDriver) Info() device.Info {
	return device.Info{
		ID: driver.id, Name: "方向变换测试", Resolution: [2]int{80, 80},
		Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame, HostRenderedFrames: true},
	}
}

func (*fakeTransformingHostDriver) Connect(context.Context) error                    { return nil }
func (*fakeTransformingHostDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*fakeTransformingHostDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (*fakeTransformingHostDriver) Close() error { return nil }

func (driver *fakeTransformingHostDriver) TransformHostFrame(source image.Image) image.Image {
	driver.mu.Lock()
	driver.transformCalls++
	driver.mu.Unlock()
	transformed := image.NewRGBA(source.Bounds())
	for y := source.Bounds().Min.Y; y < source.Bounds().Max.Y; y++ {
		for x := source.Bounds().Min.X; x < source.Bounds().Max.X; x++ {
			transformed.SetRGBA(x, y, driver.transformColor)
		}
	}
	return transformed
}

func (driver *fakeTransformingHostDriver) HostFrameTransformKey() string {
	return driver.transformKey
}

func (driver *fakeTransformingHostDriver) SendJPEG(_ context.Context, payload []byte) error {
	driver.mu.Lock()
	driver.payload = append([]byte(nil), payload...)
	driver.mu.Unlock()
	select {
	case driver.frameSignal <- struct{}{}:
	default:
	}
	return nil
}

func (driver *fakeTransformingHostDriver) encodedFrame(t *testing.T) image.Image {
	t.Helper()
	select {
	case <-driver.frameSignal:
	case <-time.After(time.Second):
		t.Fatalf("等待设备 %s 的主机帧超时", driver.id)
	}
	driver.mu.Lock()
	payload := append([]byte(nil), driver.payload...)
	driver.mu.Unlock()
	frame, err := jpeg.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("设备 %s 收到的不是有效 JPEG: %v", driver.id, err)
	}
	return frame
}

func TestHostFrameTransformRunsBeforeJPEGAndKeySeparatesCache(t *testing.T) {
	red := &fakeTransformingHostDriver{
		id: "rotation-red", transformKey: "rotation=30", transformColor: color.RGBA{R: 240, G: 10, B: 10, A: 255},
		frameSignal: make(chan struct{}, 1),
	}
	blue := &fakeTransformingHostDriver{
		id: "rotation-blue", transformKey: "rotation=150", transformColor: color.RGBA{R: 10, G: 10, B: 240, A: 255},
		frameSignal: make(chan struct{}, 1),
	}
	controller := New(&fakeCollector{}, time.Minute)
	t.Cleanup(func() {
		controller.stopAllHostFrameWorkers()
		controller.closeRenderer()
	})
	controller.AddDriver(red)
	controller.AddDriver(blue)
	controller.connectAll(context.Background())
	manifest := theme.Manifest{
		ID: "transform-cache", Name: "方向变换缓存测试", Width: 80, Height: 80,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#203040ff"},
		}},
	}
	results := controller.ApplyTheme(context.Background(), manifest)
	if results[red.id] != nil || results[blue.id] != nil {
		t.Fatalf("应用主机帧测试主题失败: %+v", results)
	}
	controller.renderHostFrames(context.Background())
	redFrame := red.encodedFrame(t)
	blueFrame := blue.encodedFrame(t)

	redPixel := color.RGBAModel.Convert(redFrame.At(40, 40)).(color.RGBA)
	bluePixel := color.RGBAModel.Convert(blueFrame.At(40, 40)).(color.RGBA)
	if redPixel.R <= redPixel.B || bluePixel.B <= bluePixel.R {
		t.Fatalf("方向变换未在 JPEG 编码前生效或缓存串用: red=%+v blue=%+v", redPixel, bluePixel)
	}
	red.mu.Lock()
	redCalls := red.transformCalls
	red.mu.Unlock()
	blue.mu.Lock()
	blueCalls := blue.transformCalls
	blue.mu.Unlock()
	if redCalls != 1 || blueCalls != 1 {
		t.Fatalf("不同 transform key 未独立生成编码帧: red=%d blue=%d", redCalls, blueCalls)
	}
}

type fakeOrientationDriver struct {
	mu                 sync.Mutex
	id                 string
	rotation           int
	showCalls          int
	showFailures       int
	smallTelemetry     bool
	sendErr            error
	sendStarted        chan struct{}
	sendRelease        chan struct{}
	setCalls           int
	applyCalls         int
	applyFailures      int
	applySignal        chan struct{}
	infoSignal         chan struct{}
	brightnessCalls    int
	brightnessFailures int
	brightnessValues   []int
	appliedThemes      []string
	events             []string
}

// fakeOrientationHostDriver 同时覆盖真实酷冷使用的 HostFrameDriver 路径。
// 它能观察普通 JPEG 是否在测试模式中被错误重启，并可阻塞 Show 制造并发窗口。
type fakeOrientationHostDriver struct {
	*fakeOrientationDriver
	frameSignal chan struct{}
	showStarted chan struct{}
	showRelease chan struct{}
	connected   bool
	connects    int
	frames      int
}

func (driver *fakeOrientationHostDriver) Info() device.Info {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return device.Info{
		ID: driver.id, Resolution: [2]int{480, 480}, DisplayRotation: driver.rotation,
		Capabilities: device.Capabilities{
			DeliveryMode: device.DeliveryHostFrame, HostRenderedFrames: true,
			SmallTelemetry: driver.smallTelemetry,
		},
	}
}

func (driver *fakeOrientationHostDriver) Connect(context.Context) error {
	driver.mu.Lock()
	driver.connects++
	driver.connected = true
	driver.mu.Unlock()
	return nil
}

func (driver *fakeOrientationHostDriver) Connected() bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.connected
}

func (driver *fakeOrientationHostDriver) ShowOrientationCalibration(ctx context.Context) error {
	driver.mu.Lock()
	driver.showCalls++
	driver.events = append(driver.events, "show")
	shouldFail := driver.showFailures > 0
	if shouldFail {
		driver.showFailures--
	}
	started := driver.showStarted
	release := driver.showRelease
	driver.mu.Unlock()
	if shouldFail {
		return errors.New("模拟显示测试图失败")
	}
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (driver *fakeOrientationHostDriver) SendJPEG(context.Context, []byte) error {
	driver.mu.Lock()
	driver.frames++
	signal := driver.frameSignal
	driver.mu.Unlock()
	if signal != nil {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
	return nil
}

func (driver *fakeOrientationDriver) Info() device.Info {
	driver.mu.Lock()
	info := device.Info{
		ID: driver.id, Resolution: [2]int{480, 480}, DisplayRotation: driver.rotation,
		Capabilities: device.Capabilities{SmallTelemetry: driver.smallTelemetry},
	}
	signal := driver.infoSignal
	driver.mu.Unlock()
	if signal != nil {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
	return info
}
func (*fakeOrientationDriver) Connect(context.Context) error { return nil }
func (driver *fakeOrientationDriver) ApplyTheme(_ context.Context, manifest theme.Manifest) error {
	driver.mu.Lock()
	driver.applyCalls++
	driver.appliedThemes = append(driver.appliedThemes, manifest.ID)
	driver.events = append(driver.events, "apply")
	shouldFail := driver.applyFailures > 0
	if shouldFail {
		driver.applyFailures--
	}
	signal := driver.applySignal
	driver.mu.Unlock()
	if signal != nil {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
	if shouldFail {
		return errors.New("模拟恢复主题失败")
	}
	return nil
}
func (driver *fakeOrientationDriver) SendTelemetry(ctx context.Context, _ telemetry.Snapshot) error {
	driver.mu.Lock()
	sendErr := driver.sendErr
	started := driver.sendStarted
	release := driver.sendRelease
	driver.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return sendErr
}
func (driver *fakeOrientationDriver) Close() error {
	driver.mu.Lock()
	driver.events = append(driver.events, "close")
	driver.mu.Unlock()
	return nil
}
func (driver *fakeOrientationDriver) SetDisplayRotation(rotation int) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.rotation = rotation
	driver.setCalls++
	return nil
}
func (driver *fakeOrientationDriver) DisplayRotation() int {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.rotation
}
func (driver *fakeOrientationDriver) ShowOrientationCalibration(context.Context) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.showCalls++
	driver.events = append(driver.events, "show")
	if driver.showFailures > 0 {
		driver.showFailures--
		return errors.New("模拟显示测试图失败")
	}
	return nil
}

func (driver *fakeOrientationDriver) SetBrightness(_ context.Context, value int) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.brightnessCalls++
	driver.brightnessValues = append(driver.brightnessValues, value)
	if driver.brightnessFailures > 0 {
		driver.brightnessFailures--
		return errors.New("模拟恢复亮度失败")
	}
	return nil
}

func (*fakeOrientationDriver) DefaultBrightness() int { return 80 }

type fakeOrientationKeepaliveDriver struct {
	*fakeOrientationDriver
	interval         time.Duration
	keepaliveStarted chan struct{}
	keepaliveRelease chan struct{}
	keepaliveErr     error
	connected        bool
	keepaliveCalls   int
}

func (driver *fakeOrientationKeepaliveDriver) Connect(context.Context) error {
	driver.mu.Lock()
	driver.connected = true
	driver.mu.Unlock()
	return nil
}

func (driver *fakeOrientationKeepaliveDriver) Connected() bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.connected
}

func (driver *fakeOrientationKeepaliveDriver) OrientationCalibrationKeepaliveInterval() time.Duration {
	return driver.interval
}

func (driver *fakeOrientationKeepaliveDriver) SendOrientationCalibrationKeepalive(ctx context.Context) error {
	driver.mu.Lock()
	if !driver.connected {
		driver.mu.Unlock()
		return errors.New("模拟方向测试图保活设备未连接")
	}
	driver.keepaliveCalls++
	driver.events = append(driver.events, "keepalive")
	started := driver.keepaliveStarted
	release := driver.keepaliveRelease
	keepaliveErr := driver.keepaliveErr
	if keepaliveErr != nil {
		driver.connected = false
	}
	driver.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return keepaliveErr
}

func (driver *fakeOrientationKeepaliveDriver) keepaliveCount() int {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.keepaliveCalls
}

func orientationTestManifest() theme.Manifest {
	return theme.Manifest{
		ID: "orientation-theme", Name: "方向恢复测试", Width: 480, Height: 480,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#102030ff"},
		}},
	}
}

func TestOrientationCalibrationRejectsShowWithoutRestorableTheme(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-orientation"}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)

	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err == nil {
		t.Fatal("没有可恢复主题时仍允许显示测试图")
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	driver.mu.Lock()
	showCalls := driver.showCalls
	driver.mu.Unlock()
	if showCalls != 0 || suspended {
		t.Fatalf("拒绝无主题校准后状态错误: calls=%d suspended=%v", showCalls, suspended)
	}
}

func TestOrientationCalibrationShowSetAndCancelWithTheme(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-orientation"}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}

	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	driver.mu.Lock()
	showCalls := driver.showCalls
	driver.mu.Unlock()
	if showCalls != 1 || !suspended {
		t.Fatalf("显示校准图后未暂停普通主机帧: calls=%d suspended=%v", showCalls, suspended)
	}

	if err := controller.SetDisplayRotation(context.Background(), driver.id, 120); err != nil {
		t.Fatal(err)
	}
	controller.mu.RLock()
	suspended = controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	driver.mu.Lock()
	setCalls := driver.setCalls
	driver.mu.Unlock()
	if setCalls != 1 || driver.DisplayRotation() != 120 || suspended || controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("保存方向后状态错误: calls=%d rotation=%d suspended=%v session=%v", setCalls, driver.DisplayRotation(), suspended, controller.hasOrientationCalibration(driver.id))
	}

	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	controller.mu.RLock()
	suspended = controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	driver.mu.Lock()
	showCalls = driver.showCalls
	applyCalls := driver.applyCalls
	driver.mu.Unlock()
	if suspended || showCalls != 2 || applyCalls != 3 || controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("取消校准后未完整恢复主题: suspended=%v show=%d apply=%d session=%v", suspended, showCalls, applyCalls, controller.hasOrientationCalibration(driver.id))
	}
}

func TestOrientationCalibrationStaysVisibleUntilExplicitAction(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-persistent-test", applySignal: make(chan struct{}, 4)}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	select {
	case <-driver.applySignal:
	default:
		t.Fatal("没有收到初始主题提交信号")
	}

	if delay := controller.orientationCalibrationDelay(); delay != 0 {
		t.Fatalf("正式方向测试仍配置了自动恢复倒计时: %v", delay)
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.applySignal:
		t.Fatal("未点击确认或恢复时，方向测试图被主题自动覆盖")
	case <-time.After(50 * time.Millisecond):
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if !controller.hasOrientationCalibration(driver.id) || !suspended {
		t.Fatalf("方向测试未持续驻留: session=%v suspended=%v", controller.hasOrientationCalibration(driver.id), suspended)
	}

	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.applySignal:
	case <-time.After(time.Second):
		t.Fatal("显式恢复没有重新应用当前主题")
	}
	if controller.hasOrientationCalibration(driver.id) {
		t.Fatal("显式恢复成功后校准会话仍然存在")
	}
}

func TestOrientationCalibrationKeepaliveStartsOnlyOnceForRepeatedShow(t *testing.T) {
	driver := &fakeOrientationKeepaliveDriver{
		fakeOrientationDriver: &fakeOrientationDriver{id: "cooler-keepalive-single"},
		interval:              120 * time.Millisecond,
		keepaliveStarted:      make(chan struct{}, 4),
		keepaliveRelease:      make(chan struct{}, 4),
		connected:             true,
	}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.keepaliveStarted:
	case <-time.After(time.Second):
		t.Fatal("方向测试图保活没有启动")
	}
	driver.keepaliveRelease <- struct{}{}
	// 若重复 Show 错误启动了第二个循环，它已经在同一设备门闩后等待，
	// 第一个保活释放后会立即出现第二次调用，而不会等待完整周期。
	select {
	case <-driver.keepaliveStarted:
		t.Fatal("重复显示方向测试图启动了重复保活循环")
	case <-time.After(50 * time.Millisecond):
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationCalibrationKeepaliveStopsAfterCancelConfirmAndClose(t *testing.T) {
	driver := &fakeOrientationKeepaliveDriver{
		fakeOrientationDriver: &fakeOrientationDriver{id: "cooler-keepalive-stop"},
		interval:              3 * time.Millisecond,
		connected:             true,
	}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}

	waitKeepaliveAfter := func(before int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for driver.keepaliveCount() <= before {
			if time.Now().After(deadline) {
				t.Fatalf("等待方向测试图保活超时: before=%d current=%d", before, driver.keepaliveCount())
			}
			time.Sleep(time.Millisecond)
		}
	}
	assertStopped := func(label string) {
		t.Helper()
		before := driver.keepaliveCount()
		time.Sleep(20 * time.Millisecond)
		if after := driver.keepaliveCount(); after != before {
			t.Fatalf("%s 后保活仍在运行: before=%d after=%d", label, before, after)
		}
	}

	before := driver.keepaliveCount()
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	waitKeepaliveAfter(before)
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	assertStopped("取消")

	before = driver.keepaliveCount()
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	waitKeepaliveAfter(before)
	if err := controller.SetDisplayRotation(context.Background(), driver.id, 90); err != nil {
		t.Fatal(err)
	}
	assertStopped("确认方向")

	before = driver.keepaliveCount()
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	waitKeepaliveAfter(before)
	controller.cancelAllOrientationCalibrations()
	assertStopped("关闭服务")
}

func TestOrientationCalibrationKeepaliveSerializesBeforeThemeRestore(t *testing.T) {
	driver := &fakeOrientationKeepaliveDriver{
		fakeOrientationDriver: &fakeOrientationDriver{id: "cooler-keepalive-restore"},
		interval:              2 * time.Millisecond,
		keepaliveStarted:      make(chan struct{}, 4),
		keepaliveRelease:      make(chan struct{}, 4),
		connected:             true,
	}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.keepaliveStarted:
	case <-time.After(time.Second):
		t.Fatal("方向测试图保活没有进入设备调用")
	}
	driver.mu.Lock()
	applyBefore := driver.applyCalls
	driver.mu.Unlock()
	restoreDone := make(chan error, 1)
	go func() { restoreDone <- controller.CancelOrientationCalibration(context.Background(), driver.id) }()
	select {
	case err := <-restoreDone:
		t.Fatalf("保活仍持有设备门闩时恢复提前完成: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	driver.mu.Lock()
	applyWhileBlocked := driver.applyCalls
	driver.mu.Unlock()
	if applyWhileBlocked != applyBefore {
		t.Fatalf("保活与主题恢复发生交叉: before=%d blocked=%d", applyBefore, applyWhileBlocked)
	}
	driver.keepaliveRelease <- struct{}{}
	select {
	case err := <-restoreDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("释放保活后主题恢复仍未完成")
	}
	driver.mu.Lock()
	events := append([]string(nil), driver.events...)
	driver.mu.Unlock()
	if len(events) < 4 || events[len(events)-2] != "keepalive" || events[len(events)-1] != "apply" {
		t.Fatalf("设备门闩没有保证保活先结束、主题最后恢复: %v", events)
	}
	before := driver.keepaliveCount()
	time.Sleep(15 * time.Millisecond)
	if after := driver.keepaliveCount(); after != before {
		t.Fatalf("主题恢复后仍补发迟到保活: before=%d after=%d", before, after)
	}
}

func TestOrientationCalibrationKeepaliveFailureKeepsSessionAndSchedulesReconnect(t *testing.T) {
	driver := &fakeOrientationKeepaliveDriver{
		fakeOrientationDriver: &fakeOrientationDriver{id: "cooler-keepalive-error"},
		interval:              2 * time.Millisecond,
		keepaliveStarted:      make(chan struct{}, 4),
		keepaliveErr:          errors.New("模拟保活 USB 断开"),
		connected:             true,
	}
	controller := New(&fakeCollector{}, time.Minute)
	controller.reconnectEvery = time.Millisecond
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	driver.mu.Lock()
	applyBefore := driver.applyCalls
	driver.mu.Unlock()
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.keepaliveStarted:
	case <-time.After(time.Second):
		t.Fatal("失败保活没有执行")
	}
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.RLock()
		status := controller.statuses[driver.id]
		controller.mu.RUnlock()
		if !status.Connected && !status.RetryAt.IsZero() && status.LastError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("保活断开没有进入既有重连状态: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
	driver.mu.Lock()
	applyAfter := driver.applyCalls
	driver.mu.Unlock()
	if applyAfter != applyBefore || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("保活失败覆盖了普通主题或丢失会话: apply=%d->%d session=%v",
			applyBefore, applyAfter, controller.hasOrientationCalibration(driver.id))
	}
}

func TestOrientationCalibrationKeepaliveFailureReconnectsAndResumesSameSession(t *testing.T) {
	driver := &fakeOrientationKeepaliveDriver{
		fakeOrientationDriver: &fakeOrientationDriver{id: "cooler-keepalive-reconnect"},
		interval:              10 * time.Millisecond,
		keepaliveStarted:      make(chan struct{}, 4),
		keepaliveErr:          errors.New("模拟保活 USB 断开"),
	}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	controller.orientationCalibrationMu.Lock()
	sessionBefore := controller.orientationCalibrations[driver.id]
	controller.orientationCalibrationMu.Unlock()
	if sessionBefore == nil {
		t.Fatal("显示测试图后没有登记校准会话")
	}

	select {
	case <-driver.keepaliveStarted:
	case <-time.After(time.Second):
		t.Fatal("失败保活没有执行")
	}
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.RLock()
		status := controller.statuses[driver.id]
		controller.mu.RUnlock()
		if !status.Connected && !status.RetryAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("保活失败没有进入重连状态: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}

	driver.mu.Lock()
	applyBefore := driver.applyCalls
	showBefore := driver.showCalls
	driver.keepaliveErr = nil
	driver.mu.Unlock()
	keepaliveBefore := driver.keepaliveCount()
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses[driver.id] = status
	controller.mu.Unlock()

	controller.reconnectDriver(context.Background(), driver)
	driver.mu.Lock()
	applyAfter := driver.applyCalls
	showAfter := driver.showCalls
	driver.mu.Unlock()
	controller.mu.RLock()
	status = controller.statuses[driver.id]
	controller.mu.RUnlock()
	controller.orientationCalibrationMu.Lock()
	sessionAfter := controller.orientationCalibrations[driver.id]
	controller.orientationCalibrationMu.Unlock()
	if !status.Connected || applyAfter != applyBefore || showAfter != showBefore+1 || sessionAfter != sessionBefore {
		t.Fatalf(
			"保活断开重连没有只重发测试图并保留同一会话: status=%+v apply=%d/%d show=%d/%d same_session=%v",
			status, applyBefore, applyAfter, showBefore, showAfter, sessionAfter == sessionBefore,
		)
	}

	deadline = time.Now().Add(time.Second)
	for driver.keepaliveCount() <= keepaliveBefore {
		if time.Now().After(deadline) {
			t.Fatalf("重连成功后原保活循环没有继续: before=%d current=%d", keepaliveBefore, driver.keepaliveCount())
		}
		time.Sleep(time.Millisecond)
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationCalibrationShowFailureRollsBackOnlyNewSession(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-show-failure", showFailures: 1}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err == nil {
		t.Fatal("首次测试图发送失败未返回错误")
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if controller.hasOrientationCalibration(driver.id) || suspended {
		t.Fatalf("首次显示失败没有回滚瞬态模式: session=%v suspended=%v", controller.hasOrientationCalibration(driver.id), suspended)
	}

	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.showFailures = 1
	driver.mu.Unlock()
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err == nil {
		t.Fatal("重复测试图发送失败未返回错误")
	}
	controller.mu.RLock()
	suspended = controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if !controller.hasOrientationCalibration(driver.id) || !suspended {
		t.Fatalf("重复显示失败丢失原校准会话: session=%v suspended=%v", controller.hasOrientationCalibration(driver.id), suspended)
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationCalibrationBlocksRealHostFramesAndFailedRestore(t *testing.T) {
	base := &fakeOrientationDriver{id: "cooler-host-persistent"}
	driver := &fakeOrientationHostDriver{fakeOrientationDriver: base, frameSignal: make(chan struct{}, 8)}
	controller := New(&fakeCollector{}, time.Minute)
	controller.SetRenderInterval(time.Millisecond)
	controller.AddDriver(driver)
	controller.startHostFrameWorkers()
	controller.connectAll(context.Background())
	t.Cleanup(func() {
		controller.cancelAllOrientationCalibrations()
		controller.stopAllHostFrameWorkers()
		controller.closeRenderer()
	})
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	controller.renderHostFrames(context.Background())
	select {
	case <-driver.frameSignal:
	case <-time.After(time.Second):
		t.Fatal("校准前没有收到基线主机帧")
	}

	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		controller.mu.Lock()
		controller.renderDirty = true
		controller.mu.Unlock()
		controller.renderHostFrames(context.Background())
	}
	select {
	case <-driver.frameSignal:
		t.Fatal("方向测试期间仍发送了普通主机 JPEG")
	case <-time.After(30 * time.Millisecond):
	}
	driver.mu.Lock()
	driver.applyFailures = 1
	driver.mu.Unlock()
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err == nil {
		t.Fatal("模拟主题恢复失败未返回错误")
	}
	controller.mu.Lock()
	controller.renderDirty = true
	controller.mu.Unlock()
	controller.renderHostFrames(context.Background())
	select {
	case <-driver.frameSignal:
		t.Fatal("恢复失败后旧主机帧覆盖了仍在显示的测试图")
	case <-time.After(30 * time.Millisecond):
	}
	if !controller.hasOrientationCalibration(driver.id) {
		t.Fatal("恢复失败后校准会话被删除")
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationCalibrationFinalGateBlocksClearedSuspension(t *testing.T) {
	base := &fakeOrientationDriver{id: "cooler-final-gate"}
	driver := &fakeOrientationHostDriver{fakeOrientationDriver: base, frameSignal: make(chan struct{}, 8)}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.startHostFrameWorkers()
	controller.connectAll(context.Background())
	t.Cleanup(func() {
		controller.cancelAllOrientationCalibrations()
		controller.stopAllHostFrameWorkers()
		controller.closeRenderer()
	})
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}

	// 模拟未来某条通用收尾路径误清 suspended。最终发送门禁
	// 必须仍以权威校准会话为准，不能只依赖这一个布尔值。
	controller.mu.Lock()
	delete(controller.hostFrameSuspended, driver.id)
	generation := controller.hostFrameGenerations[driver.id]
	controller.renderDirty = false
	controller.mu.Unlock()
	select {
	case <-controller.renderWake:
	default:
	}
	if controller.hostFrameMaySend(driver.id, generation) {
		t.Fatal("校准会话活跃时最终发送门禁仍放行")
	}
	controller.requestHostFrameRetry(driver.id, generation)
	controller.mu.RLock()
	retryDirty := controller.renderDirty
	controller.mu.RUnlock()
	if retryDirty {
		t.Fatal("校准会话活跃时仍请求普通帧重试")
	}
	select {
	case <-controller.renderWake:
		t.Fatal("校准会话活跃时重试唤醒了渲染器")
	default:
	}
	if controller.enqueueHostFrame(context.Background(), driver.id, driver, generation, []byte{1}) {
		t.Fatal("人为清除 suspended 后普通帧仍能入队")
	}
	controller.mu.Lock()
	controller.renderDirty = true
	controller.mu.Unlock()
	controller.renderHostFrames(context.Background())
	select {
	case <-driver.frameSignal:
		t.Fatal("人为清除 suspended 后校准会话被普通 JPEG 覆盖")
	case <-time.After(30 * time.Millisecond):
	}

	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if controller.hasOrientationCalibration(driver.id) || suspended {
		t.Fatalf("校准结束后未恢复主机帧: session=%v suspended=%v",
			controller.hasOrientationCalibration(driver.id), suspended)
	}
	controller.renderHostFrames(context.Background())
	select {
	case <-driver.frameSignal:
	case <-time.After(time.Second):
		t.Fatal("校准结束后普通主机帧未恢复")
	}
}

func TestOrientationRestoreKeepsHostFramesSuspendedUntilSessionFinishes(t *testing.T) {
	base := &fakeOrientationDriver{id: "cooler-restore-commit-gate", smallTelemetry: true}
	driver := &fakeOrientationHostDriver{fakeOrientationDriver: base, frameSignal: make(chan struct{}, 8)}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.startHostFrameWorkers()
	controller.connectAll(context.Background())
	t.Cleanup(func() {
		controller.cancelAllOrientationCalibrations()
		controller.stopAllHostFrameWorkers()
		controller.closeRenderer()
	})
	manifest := orientationTestManifest()
	manifest.Layers = append(manifest.Layers, theme.Layer{
		ID: "metrics", Type: theme.LayerTelemetry,
		Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .3}, Opacity: 1,
	})
	if result := controller.ApplyThemeTo(context.Background(), driver.id, manifest); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}

	// 把主题提交后的小状态刷新稳定阻塞，放大“画面已提交、
	// 校准会话尚未结束”的竞态窗口。
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	driver.mu.Lock()
	driver.sendStarted = started
	driver.sendRelease = release
	driver.mu.Unlock()
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- controller.CancelOrientationCalibration(context.Background(), driver.id)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("方向恢复未进入主题提交后的状态刷新")
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if !controller.hasOrientationCalibration(driver.id) || !suspended {
		t.Fatalf("主题提交后提前解除校准隔离: session=%v suspended=%v",
			controller.hasOrientationCalibration(driver.id), suspended)
	}
	controller.mu.Lock()
	controller.renderDirty = true
	controller.mu.Unlock()
	controller.renderHostFrames(context.Background())
	select {
	case <-driver.frameSignal:
		t.Fatal("恢复操作尚未结束时普通 JPEG 已提前发送")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-restoreDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("等待方向恢复完成超时")
	}
	controller.mu.RLock()
	suspended = controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if controller.hasOrientationCalibration(driver.id) || suspended {
		t.Fatalf("方向恢复完成后主机帧仍被暂停: session=%v suspended=%v",
			controller.hasOrientationCalibration(driver.id), suspended)
	}
	controller.renderHostFrames(context.Background())
	select {
	case <-driver.frameSignal:
	case <-time.After(time.Second):
		t.Fatal("方向恢复完成后未发送当前主题帧")
	}
}

func TestOrientationCalibrationRegistersBeforeBlockingDeviceShow(t *testing.T) {
	base := &fakeOrientationDriver{id: "cooler-show-race"}
	driver := &fakeOrientationHostDriver{
		fakeOrientationDriver: base,
		showStarted:           make(chan struct{}, 1),
		showRelease:           make(chan struct{}),
	}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	showDone := make(chan error, 1)
	go func() { showDone <- controller.ShowOrientationCalibration(context.Background(), driver.id) }()
	select {
	case <-driver.showStarted:
	case <-time.After(time.Second):
		t.Fatal("方向测试没有进入设备发送阶段")
	}
	controller.resumeHostFrameDelivery(driver.id)
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if !suspended || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("Show 尚未返回时并发恢复清除了测试保护: suspended=%v session=%v", suspended, controller.hasOrientationCalibration(driver.id))
	}
	close(driver.showRelease)
	if err := <-showDone; err != nil {
		t.Fatal(err)
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationCalibrationReconnectResendsTestInsteadOfTheme(t *testing.T) {
	base := &fakeOrientationDriver{id: "cooler-calibration-reconnect"}
	driver := &fakeOrientationHostDriver{fakeOrientationDriver: base}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	applyBefore := driver.applyCalls
	showBefore := driver.showCalls
	driver.connected = false
	driver.mu.Unlock()
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.Connected = false
	status.RetryAt = time.Time{}
	controller.statuses[driver.id] = status
	controller.mu.Unlock()

	controller.reconnectDriver(context.Background(), driver)
	driver.mu.Lock()
	applyAfter := driver.applyCalls
	showAfter := driver.showCalls
	driver.mu.Unlock()
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	connected := controller.statuses[driver.id].Connected
	controller.mu.RUnlock()
	if applyAfter != applyBefore || showAfter != showBefore+1 || !suspended || !connected || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("重连没有保持测试模式: apply=%d/%d show=%d/%d suspended=%v connected=%v session=%v",
			applyBefore, applyAfter, showBefore, showAfter, suspended, connected, controller.hasOrientationCalibration(driver.id))
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationCalibrationReconnectRetriesFailedTestImage(t *testing.T) {
	base := &fakeOrientationDriver{id: "cooler-calibration-retry"}
	driver := &fakeOrientationHostDriver{fakeOrientationDriver: base}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	applyBefore := driver.applyCalls
	driver.showFailures = 1
	driver.connected = false
	driver.mu.Unlock()
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.Connected = false
	status.RetryAt = time.Time{}
	controller.statuses[driver.id] = status
	controller.mu.Unlock()

	controller.reconnectDriver(context.Background(), driver)
	status = controller.Status().Devices[0]
	if status.Connected || status.RetryAt.IsZero() || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("重发测试图失败后没有保留会话并安排重试: %+v session=%v", status, controller.hasOrientationCalibration(driver.id))
	}
	controller.mu.Lock()
	status = controller.statuses[driver.id]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses[driver.id] = status
	controller.mu.Unlock()
	controller.reconnectDriver(context.Background(), driver)
	driver.mu.Lock()
	applyAfter := driver.applyCalls
	showCalls := driver.showCalls
	driver.mu.Unlock()
	status = controller.Status().Devices[0]
	if !status.Connected || applyAfter != applyBefore || showCalls != 3 || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("测试图重试没有恢复校准模式: status=%+v apply=%d/%d show=%d session=%v",
			status, applyBefore, applyAfter, showCalls, controller.hasOrientationCalibration(driver.id))
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationCalibrationEndsAfterVisualRestoreWhenStateRefreshFails(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-refresh-failure", smallTelemetry: true}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	manifest := orientationTestManifest()
	manifest.Layers = append(manifest.Layers, theme.Layer{
		ID: "metrics", Type: theme.LayerTelemetry,
		Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .3}, Opacity: 1,
	})
	if result := controller.ApplyThemeTo(context.Background(), driver.id, manifest); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.sendErr = errors.New("模拟主题提交后的状态补发失败")
	driver.mu.Unlock()
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatalf("画面已经恢复后仍把校准保持为活跃: %v", err)
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	_, pending := controller.pendingDeviceThemes[driver.id]
	controller.mu.RUnlock()
	if controller.hasOrientationCalibration(driver.id) || suspended || !pending {
		t.Fatalf("状态补发失败后的视觉/重试状态不一致: session=%v suspended=%v pending=%v",
			controller.hasOrientationCalibration(driver.id), suspended, pending)
	}
}

func TestOrientationCalibrationReassertsTestImageWhenThemeRestoreFinishingFails(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-power-restore-failure"}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}

	// 模拟主题已经写入设备，但主题切换后的亮度重放失败。此时不能让
	// 测试图悄悄消失；服务应重新下发测试图并保留可重试的会话。
	controller.mu.Lock()
	controller.powerIntents[driver.id] = false
	controller.powerIntentGeneration[driver.id] = 1
	controller.powerAppliedGeneration[driver.id] = 0
	controller.mu.Unlock()
	driver.mu.Lock()
	driver.brightnessFailures = 1
	driver.mu.Unlock()

	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err == nil {
		t.Fatal("亮度收尾失败时取消测试未返回错误")
	}
	driver.mu.Lock()
	showCalls := driver.showCalls
	applyCalls := driver.applyCalls
	brightnessCalls := driver.brightnessCalls
	driver.mu.Unlock()
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if showCalls != 2 || applyCalls != 2 || brightnessCalls != 1 || !suspended || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("恢复收尾失败后没有重新保持测试图: show=%d apply=%d brightness=%d suspended=%v session=%v",
			showCalls, applyCalls, brightnessCalls, suspended, controller.hasOrientationCalibration(driver.id))
	}

	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatalf("第二次恢复未成功: %v", err)
	}
	driver.mu.Lock()
	showCalls = driver.showCalls
	brightnessCalls = driver.brightnessCalls
	driver.mu.Unlock()
	controller.mu.RLock()
	suspended = controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if showCalls != 2 || brightnessCalls != 2 || suspended || controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("重试恢复后的状态错误: show=%d brightness=%d suspended=%v session=%v",
			showCalls, brightnessCalls, suspended, controller.hasOrientationCalibration(driver.id))
	}
}

func TestOrientationCalibrationSchedulesReconnectWhenTestImageReassertionFails(t *testing.T) {
	base := &fakeOrientationDriver{id: "cooler-test-reassert-retry"}
	driver := &fakeOrientationHostDriver{fakeOrientationDriver: base}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	t.Cleanup(func() {
		controller.cancelAllOrientationCalibrations()
		controller.stopAllHostFrameWorkers()
		controller.closeRenderer()
	})
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.powerIntents[driver.id] = false
	controller.powerIntentGeneration[driver.id] = 1
	controller.powerAppliedGeneration[driver.id] = 0
	controller.mu.Unlock()
	driver.mu.Lock()
	driver.brightnessFailures = 1
	driver.showFailures = 1
	applyBefore := driver.applyCalls
	driver.mu.Unlock()

	err := controller.CancelOrientationCalibration(context.Background(), driver.id)
	if err == nil || !strings.Contains(err.Error(), "模拟恢复亮度失败") || !strings.Contains(err.Error(), "模拟显示测试图失败") {
		t.Fatalf("没有同时保留主题恢复与测试图重发错误: %v", err)
	}
	driver.mu.Lock()
	applyAfterCancel := driver.applyCalls
	showAfterCancel := driver.showCalls
	driverReportedConnected := driver.connected
	driver.mu.Unlock()
	controller.mu.RLock()
	status := controller.statuses[driver.id]
	_, pending := controller.pendingDeviceThemes[driver.id]
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if applyAfterCancel != applyBefore+1 || showAfterCancel != 2 || !driverReportedConnected || status.Connected ||
		status.RetryAt.IsZero() || !pending || !suspended || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("测试图重发失败后没有进入可重试状态: status=%+v apply=%d/%d show=%d driver_connected=%v pending=%v suspended=%v session=%v",
			status, applyBefore, applyAfterCancel, showAfterCancel, driverReportedConnected, pending, suspended, controller.hasOrientationCalibration(driver.id))
	}

	controller.mu.Lock()
	status = controller.statuses[driver.id]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses[driver.id] = status
	controller.mu.Unlock()
	controller.reconnectDriver(context.Background(), driver)
	driver.mu.Lock()
	applyAfter := driver.applyCalls
	showCalls := driver.showCalls
	driver.mu.Unlock()
	status = controller.Status().Devices[0]
	if !status.Connected || applyAfter != applyAfterCancel || showCalls != showAfterCancel+1 || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("重连没有只重发测试图: status=%+v apply=%d/%d show=%d/%d session=%v",
			status, applyAfterCancel, applyAfter, showAfterCancel, showCalls, controller.hasOrientationCalibration(driver.id))
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
}

func TestSetDisplayRotationFailureWithoutCalibrationDoesNotShowTestImage(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-rotation-without-calibration"}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	driver.mu.Lock()
	driver.applyFailures = 1
	driver.mu.Unlock()
	if err := controller.SetDisplayRotation(context.Background(), driver.id, 90); err == nil {
		t.Fatal("模拟非校准方向应用失败未返回错误")
	}
	driver.mu.Lock()
	showCalls := driver.showCalls
	driver.mu.Unlock()
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if showCalls != 0 || suspended || controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("非校准失败凭空创建了测试模式: show=%d suspended=%v session=%v",
			showCalls, suspended, controller.hasOrientationCalibration(driver.id))
	}
}

func TestOrientationCalibrationFailedCancelAndSetKeepRetryableSession(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-retry"}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}

	driver.mu.Lock()
	driver.applyFailures = 1
	driver.mu.Unlock()
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err == nil {
		t.Fatal("模拟恢复失败时 Cancel 未返回错误")
	}
	if !controller.hasOrientationCalibration(driver.id) {
		t.Fatal("Cancel 恢复失败后校准会话被提前删除")
	}

	driver.mu.Lock()
	driver.applyFailures = 1
	driver.mu.Unlock()
	if err := controller.SetDisplayRotation(context.Background(), driver.id, 90); err == nil {
		t.Fatal("模拟恢复失败时 SetDisplayRotation 未返回错误")
	}
	if !controller.hasOrientationCalibration(driver.id) || driver.DisplayRotation() != 90 {
		t.Fatalf("SetDisplayRotation 恢复失败后不可重试: session=%v rotation=%d", controller.hasOrientationCalibration(driver.id), driver.DisplayRotation())
	}

	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatalf("第二次恢复未成功: %v", err)
	}
	if controller.hasOrientationCalibration(driver.id) {
		t.Fatal("恢复成功后校准会话仍然存在")
	}
}

func TestOrientationCalibrationRejectsOrdinaryThemeUntilExplicitRestore(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-ordinary-theme"}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	manifest := orientationTestManifest()
	if result := controller.ApplyThemeTo(context.Background(), driver.id, manifest); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	applyCallsBefore := driver.applyCalls
	driver.mu.Unlock()
	if result := controller.ApplyThemeTo(context.Background(), driver.id, manifest); result[driver.id] == nil {
		t.Fatal("方向测试期间普通主题仍可覆盖测试图")
	}
	if !controller.hasOrientationCalibration(driver.id) {
		t.Fatal("拒绝普通主题后校准会话被提前删除")
	}
	driver.mu.Lock()
	applyCallsAfter := driver.applyCalls
	driver.mu.Unlock()
	if applyCallsAfter != applyCallsBefore {
		t.Fatalf("被拒绝的普通主题仍到达驱动: before=%d after=%d", applyCallsBefore, applyCallsAfter)
	}
	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	if result := controller.ApplyThemeTo(context.Background(), driver.id, manifest); result[driver.id] != nil {
		t.Fatalf("显式恢复后普通主题仍被拒绝: %v", result[driver.id])
	}
}

func TestSetDisplayRotationRereadsThemeAfterDeviceGate(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-latest-theme"}
	controller := New(&fakeCollector{}, time.Minute)
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	oldTheme := orientationTestManifest()
	oldTheme.ID = "old-theme"
	newTheme := orientationTestManifest()
	newTheme.ID = "new-theme"
	if result := controller.ApplyThemeTo(context.Background(), driver.id, oldTheme); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}

	// 占住设备门闩，使 SetDisplayRotation 完成驱动查找后等待。
	// 在释放前模拟先到达的普通主题提交，恢复必须使用新主题。
	unlockDevice, err := controller.lockDeviceTheme(context.Background(), driver.id)
	if err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.infoSignal = make(chan struct{}, 1)
	infoSignal := driver.infoSignal
	driver.mu.Unlock()
	setDone := make(chan error, 1)
	go func() {
		setDone <- controller.SetDisplayRotation(context.Background(), driver.id, 60)
	}()
	select {
	case <-infoSignal:
	case <-time.After(time.Second):
		unlockDevice()
		t.Fatal("SetDisplayRotation 未进入设备门闩等待")
	}
	controller.mu.Lock()
	controller.deviceThemes[driver.id] = copyManifest(newTheme)
	controller.mu.Unlock()
	unlockDevice()
	select {
	case err := <-setDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetDisplayRotation 等待新主题恢复超时")
	}

	driver.mu.Lock()
	applied := append([]string(nil), driver.appliedThemes...)
	driver.mu.Unlock()
	if len(applied) < 2 || applied[len(applied)-1] != newTheme.ID {
		t.Fatalf("方向确认用旧快照覆盖了并发新主题: %v", applied)
	}
}

func TestOrientationCalibrationAutomaticRestoreRetriesWithBoundedBackoff(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-auto-retry", applySignal: make(chan struct{}, 8)}
	controller := New(&fakeCollector{}, time.Minute)
	controller.orientationCalibrationTimeout = 5 * time.Millisecond
	controller.orientationRestoreAttemptTimeout = 100 * time.Millisecond
	controller.orientationRestoreRetryInitial = 5 * time.Millisecond
	controller.orientationRestoreRetryMaximum = 10 * time.Millisecond
	controller.AddDriver(driver)
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	// 清掉初始主题的信号，后面只计自动恢复尝试。
	select {
	case <-driver.applySignal:
	default:
		t.Fatal("未收到初始主题信号")
	}
	driver.mu.Lock()
	driver.applyFailures = 2
	driver.mu.Unlock()
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-driver.applySignal:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("等待第 %d 次自动恢复超时", attempt+1)
		}
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for controller.hasOrientationCalibration(driver.id) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if controller.hasOrientationCalibration(driver.id) {
		t.Fatal("自动恢复重试成功后会话未结束")
	}
	if got := boundedOrientationBackoff(8*time.Millisecond, 10*time.Millisecond); got != 10*time.Millisecond {
		t.Fatalf("退避未在上限截断: %v", got)
	}
}

func TestOrientationCalibrationShutdownRestoresBeforeDriverClose(t *testing.T) {
	driver := &fakeOrientationDriver{id: "cooler-shutdown", applySignal: make(chan struct{}, 8)}
	controller := New(&fakeCollector{}, 10*time.Millisecond)
	controller.orientationShutdownTimeout = 500 * time.Millisecond
	controller.AddDriver(driver)
	if err := controller.SetInitialDeviceTheme(driver.id, orientationTestManifest()); err != nil {
		t.Fatal(err)
	}

	runContext, stopRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- controller.Run(runContext) }()
	select {
	case <-driver.applySignal:
	case <-time.After(time.Second):
		stopRun()
		t.Fatal("等待启动主题超时")
	}
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		stopRun()
		t.Fatal(err)
	}
	stopRun()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务退出前恢复超时")
	}

	driver.mu.Lock()
	events := append([]string(nil), driver.events...)
	driver.mu.Unlock()
	if len(events) < 4 || events[0] != "apply" || events[1] != "show" || events[len(events)-2] != "apply" || events[len(events)-1] != "close" {
		t.Fatalf("退出顺序错误，应在关驱动前恢复主题: %v", events)
	}
}
