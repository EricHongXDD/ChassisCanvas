package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type staticHealthDriver struct {
	id                   string
	connected            bool
	checkErr             error
	keepConnectedOnError bool
	checks               int
	connects             int
	applies              int
}

// calibrationHealthDriver 模拟会被 param 探测刷新画面的方向校准设备。
// 阻塞通道用来验证健康探测与测试图发送共用同一设备门闩。
type calibrationHealthDriver struct {
	mu sync.Mutex

	id        string
	connected bool
	rotation  int
	checks    int
	shows     int
	applies   int
	events    []string

	checkStarted chan struct{}
	checkRelease chan struct{}
	showStarted  chan struct{}
	showRelease  chan struct{}
}

func (driver *calibrationHealthDriver) Info() device.Info {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return device.Info{
		ID: driver.id, Resolution: [2]int{480, 480}, DisplayRotation: driver.rotation,
		Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, StoresResources: true},
	}
}

func (driver *calibrationHealthDriver) Connect(context.Context) error {
	driver.mu.Lock()
	driver.connected = true
	driver.mu.Unlock()
	return nil
}

func (driver *calibrationHealthDriver) Connected() bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.connected
}

func (driver *calibrationHealthDriver) CheckConnection(ctx context.Context) error {
	driver.mu.Lock()
	driver.checks++
	driver.events = append(driver.events, "check")
	started := driver.checkStarted
	release := driver.checkRelease
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
	return nil
}

func (driver *calibrationHealthDriver) ApplyTheme(context.Context, theme.Manifest) error {
	driver.mu.Lock()
	driver.applies++
	driver.events = append(driver.events, "apply")
	driver.mu.Unlock()
	return nil
}

func (*calibrationHealthDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}

func (driver *calibrationHealthDriver) Close() error {
	driver.mu.Lock()
	driver.connected = false
	driver.mu.Unlock()
	return nil
}

func (driver *calibrationHealthDriver) SetDisplayRotation(rotation int) error {
	driver.mu.Lock()
	driver.rotation = rotation
	driver.mu.Unlock()
	return nil
}

func (driver *calibrationHealthDriver) DisplayRotation() int {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.rotation
}

func (driver *calibrationHealthDriver) ShowOrientationCalibration(ctx context.Context) error {
	driver.mu.Lock()
	driver.shows++
	driver.events = append(driver.events, "show")
	started := driver.showStarted
	release := driver.showRelease
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
	return nil
}

func (driver *calibrationHealthDriver) healthCounts() (checks, shows int) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.checks, driver.shows
}

func newCalibrationHealthController(t *testing.T, driver *calibrationHealthDriver) *Service {
	t.Helper()
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	if result := controller.ApplyThemeTo(context.Background(), driver.id, orientationTestManifest()); result[driver.id] != nil {
		t.Fatal(result[driver.id])
	}
	t.Cleanup(controller.cancelAllOrientationCalibrations)
	return controller
}

func (driver *staticHealthDriver) Info() device.Info {
	return device.Info{ID: driver.id, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme}}
}

func (driver *staticHealthDriver) Connect(context.Context) error {
	driver.connects++
	driver.connected = true
	return nil
}

func (driver *staticHealthDriver) CheckConnection(context.Context) error {
	driver.checks++
	if driver.checkErr != nil && !driver.keepConnectedOnError {
		driver.connected = false
	}
	return driver.checkErr
}

func (driver *staticHealthDriver) Connected() bool { return driver.connected }

func (driver *staticHealthDriver) ApplyTheme(context.Context, theme.Manifest) error {
	driver.applies++
	return nil
}

func (*staticHealthDriver) SendTelemetry(context.Context, telemetry.Snapshot) error { return nil }
func (driver *staticHealthDriver) Close() error {
	driver.connected = false
	return nil
}

func TestStaticThemeHealthFailureReconnectsAndReappliesTheme(t *testing.T) {
	driver := &staticHealthDriver{id: "static-health", connected: true, checkErr: errors.New("USB 已拔出")}
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectEvery = time.Millisecond
	controller.AddDriver(driver)
	manifest := theme.Manifest{
		ID: "static", Name: "静态背景", Width: 64, Height: 64,
		Layers: []theme.Layer{{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1}},
	}
	connectedAt := time.Now().Add(-time.Minute)
	status := controller.statuses[driver.id]
	status.Connected = true
	status.LastConnectedAt = connectedAt
	status.LastVerifiedAt = connectedAt
	controller.statuses[driver.id] = status
	controller.deviceThemes[driver.id] = manifest

	controller.checkConnectedDevices(context.Background())
	status = controller.Status().Devices[0]
	if status.Connected || status.LastDisconnectedAt.IsZero() || status.LastConnectionCheck.IsZero() || driver.checks != 1 {
		t.Fatalf("静态主题拔出后状态未刷新: status=%+v checks=%d", status, driver.checks)
	}

	driver.checkErr = nil
	controller.mu.Lock()
	status = controller.statuses[driver.id]
	status.RetryAt = time.Now().Add(-time.Millisecond)
	controller.statuses[driver.id] = status
	controller.mu.Unlock()
	controller.reconnectDisconnected(context.Background())
	status = controller.Status().Devices[0]
	if !status.Connected || driver.connects != 1 || driver.applies != 1 || !status.LastConnectedAt.After(connectedAt) || status.LastVerifiedAt.IsZero() {
		t.Fatalf("重插后没有重连并恢复主题: status=%+v connects=%d applies=%d", status, driver.connects, driver.applies)
	}
}

func TestBusyHealthCheckDoesNotChangeConnectedStatus(t *testing.T) {
	driver := &staticHealthDriver{id: "static-health", connected: true, checkErr: device.ErrHealthCheckBusy}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	connectedAt := time.Now().Add(-time.Minute)
	status := controller.statuses[driver.id]
	status.Connected = true
	status.LastConnectedAt = connectedAt
	controller.statuses[driver.id] = status
	controller.checkConnectedDevices(context.Background())
	status = controller.Status().Devices[0]
	if !status.Connected || !status.LastConnectionCheck.IsZero() || !status.LastDisconnectedAt.IsZero() {
		t.Fatalf("繁忙跳过被误判为断开: %+v", status)
	}
}

func TestIndeterminateHealthErrorKeepsDriverReportedOnline(t *testing.T) {
	driver := &staticHealthDriver{
		id: "static-health", connected: true,
		checkErr: errors.New("ACK 回包暂时无效"), keepConnectedOnError: true,
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	connectedAt := time.Now().Add(-time.Minute)
	status := controller.statuses[driver.id]
	status.Connected = true
	status.LastConnectedAt = connectedAt
	status.LastVerifiedAt = connectedAt
	controller.statuses[driver.id] = status

	controller.checkConnectedDevices(context.Background())
	status = controller.Status().Devices[0]
	if !status.Connected || status.LastError == "" || status.LastConnectionCheck.IsZero() ||
		!status.LastDisconnectedAt.IsZero() || status.ReconnectAttempts != 0 {
		t.Fatalf("驱动仍在线时单次健康检查失败造成闪断: %+v", status)
	}
	driver.checkErr = nil
	controller.checkConnectedDevices(context.Background())
	status = controller.Status().Devices[0]
	if !status.Connected || status.LastError != "" || !status.LastVerifiedAt.After(connectedAt) {
		t.Fatalf("下一轮健康检查成功后没有自动恢复: %+v", status)
	}
}

func TestOrientationCalibrationSkipsHealthCheckUntilCancel(t *testing.T) {
	driver := &calibrationHealthDriver{id: "cooler-health-calibration"}
	controller := newCalibrationHealthController(t, driver)
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.LastConnectionCheck = time.Time{}
	controller.statuses[driver.id] = status
	controller.mu.Unlock()
	if err := controller.ShowOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}

	statusBefore := controller.Status().Devices[0]
	controller.checkConnectedDevices(context.Background())
	statusDuring := controller.Status().Devices[0]
	checks, shows := driver.healthCounts()
	if checks != 0 || shows != 1 {
		t.Fatalf("方向测试期间仍调用健康探测: checks=%d shows=%d", checks, shows)
	}
	if !statusDuring.LastConnectionCheck.Equal(statusBefore.LastConnectionCheck) {
		t.Fatalf("跳过的校准健康探测仍更新检查时间: before=%v after=%v",
			statusBefore.LastConnectionCheck, statusDuring.LastConnectionCheck)
	}

	if err := controller.CancelOrientationCalibration(context.Background(), driver.id); err != nil {
		t.Fatal(err)
	}
	controller.checkConnectedDevices(context.Background())
	statusAfter := controller.Status().Devices[0]
	checks, shows = driver.healthCounts()
	if checks != 1 || shows != 1 {
		t.Fatalf("取消校准后健康探测未恢复: checks=%d shows=%d", checks, shows)
	}
	if statusAfter.LastConnectionCheck.IsZero() {
		t.Fatalf("取消校准后未记录健康探测: before=%v after=%v",
			statusDuring.LastConnectionCheck, statusAfter.LastConnectionCheck)
	}
}

func TestHealthCheckCompletesBeforeConcurrentOrientationShow(t *testing.T) {
	driver := &calibrationHealthDriver{
		id:           "cooler-health-show-order",
		checkStarted: make(chan struct{}, 1),
		checkRelease: make(chan struct{}),
		showStarted:  make(chan struct{}, 1),
	}
	controller := newCalibrationHealthController(t, driver)

	healthDone := make(chan struct{})
	go func() {
		controller.checkConnectedDevices(context.Background())
		close(healthDone)
	}()
	select {
	case <-driver.checkStarted:
	case <-time.After(time.Second):
		t.Fatal("健康探测未进入驱动调用")
	}

	showInvoked := make(chan struct{})
	showDone := make(chan error, 1)
	go func() {
		close(showInvoked)
		showDone <- controller.ShowOrientationCalibration(context.Background(), driver.id)
	}()
	<-showInvoked
	select {
	case <-driver.showStarted:
		t.Fatal("健康探测未释放设备门闩时已开始发送测试图")
	case <-time.After(30 * time.Millisecond):
	}

	close(driver.checkRelease)
	select {
	case <-healthDone:
	case <-time.After(time.Second):
		t.Fatal("健康探测未返回")
	}
	select {
	case <-driver.showStarted:
	case <-time.After(time.Second):
		t.Fatal("健康探测完成后测试图未开始发送")
	}
	select {
	case err := <-showDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("测试图发送未返回")
	}

	// Show 已在门闩内登记 Active，后续探测必须完全跳过。
	controller.checkConnectedDevices(context.Background())
	checks, shows := driver.healthCounts()
	if checks != 1 || shows != 1 || !controller.hasOrientationCalibration(driver.id) {
		t.Fatalf("并发顺序保护失效: checks=%d shows=%d active=%v",
			checks, shows, controller.hasOrientationCalibration(driver.id))
	}
}
