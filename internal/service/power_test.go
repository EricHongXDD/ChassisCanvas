package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type powerTestDriver struct {
	id string

	mu              sync.Mutex
	connected       bool
	brightnessCalls []int
	brightnessErr   error
	brightnessStart chan struct{}
	brightnessWait  chan struct{}
	frames          int
	themeCalls      int
	themeErr        error
	applyStarted    chan struct{}
	applyRelease    chan struct{}
}

func (driver *powerTestDriver) Info() device.Info {
	return device.Info{
		ID: driver.id, Name: driver.id, Resolution: [2]int{80, 45},
		Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame, HostRenderedFrames: true},
	}
}

func (driver *powerTestDriver) Connect(context.Context) error {
	driver.mu.Lock()
	driver.connected = true
	driver.mu.Unlock()
	return nil
}

func (driver *powerTestDriver) Connected() bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.connected
}

func (driver *powerTestDriver) ApplyTheme(ctx context.Context, _ theme.Manifest) error {
	driver.mu.Lock()
	driver.themeCalls++
	started, release, err := driver.applyStarted, driver.applyRelease, driver.themeErr
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
	return err
}

func (*powerTestDriver) SendTelemetry(context.Context, telemetry.Snapshot) error { return nil }
func (*powerTestDriver) Close() error                                            { return nil }

func (driver *powerTestDriver) SendJPEG(context.Context, []byte) error {
	driver.mu.Lock()
	driver.frames++
	driver.mu.Unlock()
	return nil
}

func (driver *powerTestDriver) SetBrightness(ctx context.Context, value int) error {
	driver.mu.Lock()
	driver.brightnessCalls = append(driver.brightnessCalls, value)
	err := driver.brightnessErr
	started, wait := driver.brightnessStart, driver.brightnessWait
	if err != nil {
		driver.connected = false
	}
	driver.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (*powerTestDriver) DefaultBrightness() int { return 80 }

func (driver *powerTestDriver) snapshot() (calls []int, frames int) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return append([]int(nil), driver.brightnessCalls...), driver.frames
}

type unsupportedPowerDriver struct{ id string }

func (driver *unsupportedPowerDriver) Info() device.Info {
	return device.Info{ID: driver.id, Name: driver.id, Resolution: [2]int{40, 40}}
}
func (*unsupportedPowerDriver) Connect(context.Context) error                    { return nil }
func (*unsupportedPowerDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*unsupportedPowerDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (*unsupportedPowerDriver) Close() error { return nil }

func powerTestManifest(id string) theme.Manifest {
	return theme.Manifest{ID: id, Name: id, Width: 80, Height: 45, Layers: []theme.Layer{{
		ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		Options: map[string]any{"color": "101820"},
	}}}
}

func TestPowerStatusAndCurrentBrightnessZeroRemainExplicit(t *testing.T) {
	driver := &powerTestDriver{id: "screen"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())

	status := controller.Status().Devices[0]
	if !status.BrightnessSupported || status.DefaultBrightness != 80 || status.CurrentBrightness != 80 || status.PoweredOff {
		t.Fatalf("初始亮度能力错误: %+v", status)
	}
	if err := controller.SetPoweredOff(context.Background(), "screen", true)["screen"]; err != nil {
		t.Fatal(err)
	}
	status = controller.Status().Devices[0]
	if !status.PoweredOff || status.CurrentBrightness != 0 {
		t.Fatalf("关屏状态错误: %+v", status)
	}
	data, err := json.Marshal(controller.Status())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"current_brightness":0`) {
		t.Fatalf("关屏 JSON 丢失了 0 亮度: %s", data)
	}
}

func TestInitialPowerIntentReplaysOnceAfterStartupTheme(t *testing.T) {
	driver := &powerTestDriver{id: "startup-off"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	if err := controller.SetInitialPoweredOff(driver.id, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetInitialDeviceTheme(driver.id, powerTestManifest("startup-theme")); err != nil {
		t.Fatal(err)
	}
	controller.connectAll(context.Background())
	controller.applyInitialTheme(context.Background())
	if calls, _ := driver.snapshot(); len(calls) != 0 {
		t.Fatalf("启动主题阶段提前发送了亮度: %v", calls)
	}
	controller.applyInitialPower(context.Background())
	calls, _ := driver.snapshot()
	if len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("启动主题后未精确重放一次关屏: %v", calls)
	}
	status := controller.Status().Devices[0]
	if !status.PoweredOff || status.CurrentBrightness != 0 {
		t.Fatalf("启动关屏状态错误: %+v", status)
	}
}

func TestInitialExplicitOnRestoresDefaultBrightness(t *testing.T) {
	driver := &powerTestDriver{id: "startup-on"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	if err := controller.SetInitialPoweredOff(driver.id, false); err != nil {
		t.Fatal(err)
	}
	controller.connectAll(context.Background())
	controller.applyInitialTheme(context.Background())
	controller.applyInitialPower(context.Background())
	calls, _ := driver.snapshot()
	if len(calls) != 1 || calls[0] != 80 {
		t.Fatalf("显式开屏意图未恢复默认亮度: %v", calls)
	}
}

func TestAllPowerReturnsPerDevicePartialFailures(t *testing.T) {
	one := &powerTestDriver{id: "one"}
	two := &powerTestDriver{id: "two", brightnessErr: errors.New("串口超时")}
	unsupported := &unsupportedPowerDriver{id: "unsupported"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(one)
	controller.AddDriver(two)
	controller.AddDriver(unsupported)
	controller.connectAll(context.Background())

	results := controller.SetPoweredOff(context.Background(), "all", true)
	if results["one"] != nil || results["two"] == nil {
		t.Fatalf("全部关屏没有保留逐台结果: %+v", results)
	}
	if _, exists := results["unsupported"]; exists {
		t.Fatalf("全部关屏应只包含受支持设备: %+v", results)
	}
	statuses := controller.Status().Devices
	if len(statuses) != 3 {
		t.Fatalf("状态数量=%d", len(statuses))
	}
}

func TestAllPowerRegistersEveryIntentBeforeParallelUSBWait(t *testing.T) {
	blocked := &powerTestDriver{id: "blocked", brightnessStart: make(chan struct{}, 1), brightnessWait: make(chan struct{})}
	fast := &powerTestDriver{id: "fast"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(blocked)
	controller.AddDriver(fast)
	controller.connectAll(context.Background())
	done := make(chan map[string]error, 1)
	go func() { done <- controller.SetPoweredOff(context.Background(), "all", true) }()
	select {
	case <-blocked.brightnessStart:
	case <-time.After(time.Second):
		t.Fatal("阻塞设备未进入亮度命令")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		calls, _ := fast.snapshot()
		if len(calls) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if calls, _ := fast.snapshot(); len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("首台卡住阻断了其它副屏: %v", calls)
	}
	for _, status := range controller.Status().Devices {
		if !status.PoweredOff || status.CurrentBrightness != 0 {
			t.Fatalf("USB 等待前未为 %s 登记关屏意图: %+v", status.Info.ID, status)
		}
	}
	close(blocked.brightnessWait)
	results := <-done
	if results["blocked"] != nil || results["fast"] != nil {
		t.Fatalf("并行全部关屏失败: %+v", results)
	}
}

func TestPowerOffIntentSurvivesFailureAndReplaysAfterReconnect(t *testing.T) {
	driver := &powerTestDriver{id: "reconnect", brightnessErr: errors.New("USB 已断开")}
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectEvery = time.Millisecond
	controller.AddDriver(driver)
	controller.connectAll(context.Background())

	if err := controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id]; err == nil {
		t.Fatal("第一次关屏应失败")
	}
	status := controller.Status().Devices[0]
	if !status.PoweredOff || status.CurrentBrightness != 0 || status.Connected {
		t.Fatalf("断线后没有保留关屏意图: %+v", status)
	}
	driver.mu.Lock()
	driver.brightnessErr = nil
	driver.mu.Unlock()
	controller.mu.Lock()
	status = controller.statuses[driver.id]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses[driver.id] = status
	controller.mu.Unlock()
	controller.reconnectDisconnected(context.Background())

	calls, _ := driver.snapshot()
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 0 {
		t.Fatalf("重连没有重放关屏意图: %v", calls)
	}
	status = controller.Status().Devices[0]
	if !status.Connected || !status.PoweredOff || status.CurrentBrightness != 0 {
		t.Fatalf("重连后关屏状态错误: %+v", status)
	}
}

func TestApplyThemeWhileOffImmediatelyReappliesZero(t *testing.T) {
	driver := &powerTestDriver{id: "theme-off"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	if err := controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id]; err != nil {
		t.Fatal(err)
	}
	if err := controller.ApplyThemeTo(context.Background(), driver.id, powerTestManifest("new-theme"))[driver.id]; err != nil {
		t.Fatal(err)
	}
	calls, _ := driver.snapshot()
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 0 {
		t.Fatalf("主题下发后没有补发亮度 0: %v", calls)
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if !suspended {
		t.Fatal("关屏时主题应用错误恢复了主机帧")
	}
}

func TestApplyThemeWhileOnDoesNotResendConfirmedBrightness(t *testing.T) {
	driver := &powerTestDriver{id: "theme-on"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	if err := controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id]; err != nil {
		t.Fatal(err)
	}
	// 已确认的亮屏亮度不应在主题提交后重复发送；即使设备此时会
	// 忽略亮度命令，也不能把已成功的主题误报为保存失败。
	driver.mu.Lock()
	driver.brightnessErr = errors.New("主题提交后的冗余亮度命令不应发送")
	driver.mu.Unlock()
	if err := controller.ApplyThemeTo(context.Background(), driver.id, powerTestManifest("new-theme"))[driver.id]; err != nil {
		t.Fatal(err)
	}
	calls, _ := driver.snapshot()
	if len(calls) != 1 || calls[0] != 80 {
		t.Fatalf("主题下发后重复发送了已确认的亮屏亮度: %v", calls)
	}
}

func TestPowerUsesSameDeviceGateAsTheme(t *testing.T) {
	driver := &powerTestDriver{
		id: "serialized", applyStarted: make(chan struct{}, 1), applyRelease: make(chan struct{}),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	themeDone := make(chan error, 1)
	go func() {
		themeDone <- controller.ApplyThemeTo(context.Background(), driver.id, powerTestManifest("blocking"))[driver.id]
	}()
	select {
	case <-driver.applyStarted:
	case <-time.After(time.Second):
		t.Fatal("主题操作未进入驱动")
	}
	powerDone := make(chan error, 1)
	go func() { powerDone <- controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id] }()
	time.Sleep(20 * time.Millisecond)
	if calls, _ := driver.snapshot(); len(calls) != 0 {
		t.Fatalf("亮度命令越过主题门闩: %v", calls)
	}
	close(driver.applyRelease)
	if err := <-themeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-powerDone; err != nil {
		t.Fatal(err)
	}
	if calls, _ := driver.snapshot(); len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("串行关屏结果错误: %v", calls)
	}
}

func TestThemeTailAbsorbsSameOffIntentRegisteredDuringBrightnessSend(t *testing.T) {
	driver := &powerTestDriver{
		id: "same-off", applyStarted: make(chan struct{}, 1), applyRelease: make(chan struct{}),
		brightnessStart: make(chan struct{}, 2), brightnessWait: make(chan struct{}),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	themeDone := make(chan error, 1)
	go func() {
		themeDone <- controller.ApplyThemeTo(context.Background(), driver.id, powerTestManifest("same-off-theme"))[driver.id]
	}()
	select {
	case <-driver.applyStarted:
	case <-time.After(time.Second):
		t.Fatal("主题操作未进入驱动")
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id] }()
	waitPowerGeneration(t, controller, driver.id, 1)
	close(driver.applyRelease)
	select {
	case <-driver.brightnessStart:
	case <-time.After(time.Second):
		t.Fatal("主题收尾未开始补发亮度 0")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id] }()
	waitPowerGeneration(t, controller, driver.id, 2)
	close(driver.brightnessWait)
	if err := <-themeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if calls, _ := driver.snapshot(); len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("相同关屏意图被重复发送: %v", calls)
	}
}

func TestPowerIntentReversalDuringBrightnessSendAppliesLatestValue(t *testing.T) {
	driver := &powerTestDriver{
		id: "reverse", brightnessStart: make(chan struct{}, 2), brightnessWait: make(chan struct{}),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	offDone := make(chan error, 1)
	go func() { offDone <- controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id] }()
	select {
	case <-driver.brightnessStart:
	case <-time.After(time.Second):
		t.Fatal("关屏亮度命令未开始")
	}
	onDone := make(chan error, 1)
	go func() { onDone <- controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id] }()
	waitPowerGeneration(t, controller, driver.id, 2)
	close(driver.brightnessWait)
	if err := <-offDone; err != nil {
		t.Fatal(err)
	}
	if err := <-onDone; err != nil {
		t.Fatal(err)
	}
	if calls, _ := driver.snapshot(); len(calls) != 2 || calls[0] != 0 || calls[1] != 80 {
		t.Fatalf("发送中反转意图未按顺序落到最终值: %v", calls)
	}
	status := controller.Status().Devices[0]
	if status.PoweredOff || status.CurrentBrightness != 80 {
		t.Fatalf("恢复后的状态错误: %+v", status)
	}
}

func TestThemeFinishReplaysRestoreThatTimedOutWaitingForGate(t *testing.T) {
	driver := &powerTestDriver{
		id: "restore-timeout", applyStarted: make(chan struct{}, 1), applyRelease: make(chan struct{}),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	if err := controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.reconnectTimeout = 15 * time.Millisecond
	themeDone := make(chan error, 1)
	go func() {
		themeDone <- controller.ApplyThemeTo(context.Background(), driver.id, powerTestManifest("restore-timeout-theme"))[driver.id]
	}()
	select {
	case <-driver.applyStarted:
	case <-time.After(time.Second):
		t.Fatal("主题操作未进入驱动")
	}
	if err := controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id]; err == nil {
		t.Fatal("恢复请求等待主题门闩应超时")
	}
	close(driver.applyRelease)
	if err := <-themeDone; err != nil {
		t.Fatal(err)
	}
	if calls, _ := driver.snapshot(); len(calls) != 2 || calls[0] != 0 || calls[1] != 80 {
		t.Fatalf("主题收尾未精确重放最新恢复意图: %v", calls)
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if suspended {
		t.Fatal("恢复意图已生效但主机帧仍被暂停")
	}
}

func TestPendingPowerRetriesAfterThemeFailureAndGateTimeout(t *testing.T) {
	driver := &powerTestDriver{
		id: "pending-retry", themeErr: errors.New("主题本地准备失败"),
		applyStarted: make(chan struct{}, 1), applyRelease: make(chan struct{}),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectTimeout = 15 * time.Millisecond
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	themeDone := make(chan error, 1)
	go func() {
		themeDone <- controller.ApplyThemeTo(context.Background(), driver.id, powerTestManifest("failed-theme"))[driver.id]
	}()
	select {
	case <-driver.applyStarted:
	case <-time.After(time.Second):
		t.Fatal("失败主题未持有设备门闩")
	}
	if err := controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id]; err == nil {
		t.Fatal("关屏请求等待主题门闩应超时")
	}
	close(driver.applyRelease)
	if err := <-themeDone; err == nil {
		t.Fatal("测试主题应失败")
	}
	if calls, _ := driver.snapshot(); len(calls) != 0 {
		t.Fatalf("失败主题结束前意外发送亮度: %v", calls)
	}
	controller.replayPendingPowerAsync(context.Background())
	waitPowerBrightnessCalls(t, driver, 1)
	controller.reconnectWG.Wait()
	if calls, _ := driver.snapshot(); len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("pending 关屏没有被补偿重放: %v", calls)
	}
	status := controller.Status().Devices[0]
	if !status.Connected || !status.PoweredOff || status.CurrentBrightness != 0 {
		t.Fatalf("pending 重放后的状态错误: %+v", status)
	}
}

func TestInitialPowerUsesLatestIntentWhenCapturedTargetBecomesOn(t *testing.T) {
	driver := &powerTestDriver{id: "initial-latest"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	if err := controller.SetInitialPoweredOff(driver.id, true); err != nil {
		t.Fatal(err)
	}
	controller.connectAll(context.Background())
	controller.mu.Lock()
	controller.hostFrames[driver.id] = true
	controller.mu.Unlock()
	targets := controller.collectInitialPowerTargets()
	if len(targets) != 1 {
		t.Fatalf("启动关屏目标数=%d", len(targets))
	}
	if err := controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.applyInitialPowerTargets(context.Background(), targets)
	if calls, _ := driver.snapshot(); len(calls) != 1 || calls[0] != 80 {
		t.Fatalf("已完成的用户恢复被启动快照重复覆盖: %v", calls)
	}
	controller.mu.RLock()
	suspended := controller.hostFrameSuspended[driver.id]
	controller.mu.RUnlock()
	if suspended {
		t.Fatal("最新意图为开屏时启动重放仍留下 suspended")
	}
}

func TestPendingThemeReconnectWithExplicitOnReappliesBrightnessOnce(t *testing.T) {
	driver := &powerTestDriver{id: "pending-on"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	if err := controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.Connected = false
	status.RetryAt = time.Time{}
	controller.statuses[driver.id] = status
	controller.pendingDeviceThemes[driver.id] = powerTestManifest("pending-on-theme")
	controller.mu.Unlock()
	driver.mu.Lock()
	driver.connected = false
	driver.mu.Unlock()
	controller.reconnectDriver(context.Background(), driver)
	if calls, _ := driver.snapshot(); len(calls) != 2 || calls[0] != 80 || calls[1] != 80 {
		t.Fatalf("pending 主题重连重复恢复默认亮度: %v", calls)
	}
}

func TestReconnectThemeFailureKeepsLiveSessionAndAppliesPendingOffOnce(t *testing.T) {
	driver := &powerTestDriver{id: "reconnect-bad-theme", themeErr: errors.New("本地渲染资源永久失败")}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.Connected = false
	status.RetryAt = time.Time{}
	controller.statuses[driver.id] = status
	controller.pendingDeviceThemes[driver.id] = powerTestManifest("permanent-bad-theme")
	controller.mu.Unlock()
	driver.mu.Lock()
	driver.connected = false
	driver.mu.Unlock()
	controller.rememberPowerIntent(driver.id, driver, true, false)
	controller.suspendHostFrameDelivery(driver.id)

	controller.reconnectDriver(context.Background(), driver)
	if calls, _ := driver.snapshot(); len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("坏主题阻塞了关屏意图或造成重复: %v", calls)
	}
	status = controller.Status().Devices[0]
	if !status.Connected || !status.PoweredOff || status.CurrentBrightness != 0 || !strings.Contains(status.LastError, "本地渲染资源永久失败") {
		t.Fatalf("底层在线的主题错误状态不正确: %+v", status)
	}
	// 在线状态不会再被坏 pending 主题的重连循环抢占；pending 代次也
	// 已确认，因此下一节拍不能再次发送亮度 0。
	controller.reconnectDisconnectedAsync(context.Background())
	controller.replayPendingPowerAsync(context.Background())
	controller.reconnectWG.Wait()
	if calls, _ := driver.snapshot(); len(calls) != 1 {
		t.Fatalf("坏主题导致关屏意图被重复重放: %v", calls)
	}
}

func TestHostFramesPauseWhileOffAndResumeWithCurrentFrame(t *testing.T) {
	driver := &powerTestDriver{id: "host-power"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.startHostFrameWorkers()
	t.Cleanup(controller.stopAllHostFrameWorkers)
	controller.connectAll(context.Background())
	if err := controller.ApplyThemeTo(context.Background(), driver.id, powerTestManifest("host"))[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.renderHostFrames(context.Background())
	waitPowerFrames(t, driver, 1)

	if err := controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.SetTelemetryEnabled(false)
	controller.renderHostFrames(context.Background())
	time.Sleep(30 * time.Millisecond)
	if _, frames := driver.snapshot(); frames != 1 {
		t.Fatalf("关屏后仍发送 JPEG: %d", frames)
	}
	controller.mu.RLock()
	worker := controller.hostFrameWorkers[driver.id]
	controller.mu.RUnlock()
	if worker != nil {
		t.Fatal("关屏后主机帧 worker 仍存活")
	}

	if err := controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.renderHostFrames(context.Background())
	waitPowerFrames(t, driver, 2)
	calls, _ := driver.snapshot()
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 80 {
		t.Fatalf("恢复亮度命令错误: %v", calls)
	}
}

func waitPowerGeneration(t *testing.T, controller *Service, deviceID string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		controller.mu.RLock()
		generation := controller.powerIntentGeneration[deviceID]
		controller.mu.RUnlock()
		if generation >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("电源意图代次未推进")
}

func waitPowerBrightnessCalls(t *testing.T, driver *powerTestDriver, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		calls, _ := driver.snapshot()
		if len(calls) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	calls, _ := driver.snapshot()
	t.Fatalf("亮度调用数=%d, want >=%d: %v", len(calls), want, calls)
}

func waitPowerFrames(t *testing.T, driver *powerTestDriver, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, frames := driver.snapshot(); frames >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, frames := driver.snapshot()
	t.Fatalf("主机帧数=%d, want >=%d", frames, want)
}
