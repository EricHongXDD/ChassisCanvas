package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type runtimeResetDriver struct {
	mu       sync.Mutex
	id       string
	connects int
	closes   int
}

type blockingResetDriver struct {
	id           string
	closeStarted chan struct{}
	releaseClose chan struct{}
	closed       chan struct{}
	block        bool
}

func (driver *blockingResetDriver) Info() device.Info                                { return device.Info{ID: driver.id} }
func (*blockingResetDriver) Connect(context.Context) error                           { return nil }
func (*blockingResetDriver) ApplyTheme(context.Context, theme.Manifest) error        { return nil }
func (*blockingResetDriver) SendTelemetry(context.Context, telemetry.Snapshot) error { return nil }
func (driver *blockingResetDriver) Close() error {
	if driver.block {
		close(driver.closeStarted)
		<-driver.releaseClose
	}
	close(driver.closed)
	return nil
}

func (driver *runtimeResetDriver) Info() device.Info {
	return device.Info{ID: driver.id, Name: driver.id}
}

func (driver *runtimeResetDriver) Connect(context.Context) error {
	driver.mu.Lock()
	driver.connects++
	driver.mu.Unlock()
	return nil
}

func (*runtimeResetDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*runtimeResetDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (driver *runtimeResetDriver) Close() error {
	driver.mu.Lock()
	driver.closes++
	driver.mu.Unlock()
	return nil
}

func TestDriverAddedWhileRunningReconnectsAndRejectsDuplicateID(t *testing.T) {
	controller := New(&fakeCollector{}, time.Millisecond)
	controller.reconnectEvery = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	waitForHostFrameCondition(t, func() bool {
		controller.themeOperationMu.Lock()
		started := controller.themeOperationContext != nil
		controller.themeOperationMu.Unlock()
		return started
	})

	driver := &runtimeResetDriver{id: "runtime-hotplug"}
	if !controller.AddDriverIfAbsent(driver) {
		cancel()
		t.Fatal("运行期新驱动被拒绝")
	}
	if controller.AddDriverIfAbsent(&runtimeResetDriver{id: "runtime-hotplug"}) {
		cancel()
		t.Fatal("重复设备 ID 被注册了两次")
	}
	waitForHostFrameCondition(t, func() bool {
		devices := controller.Status().Devices
		return len(devices) == 1 && devices[0].Info.ID == "runtime-hotplug" && devices[0].Connected
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("动态驱动服务退出超时")
	}
	if controller.AddDriverIfAbsent(&runtimeResetDriver{id: "after-close"}) {
		t.Fatal("服务关闭后仍接受新驱动")
	}
}

func TestResetDeviceConnectionClearsBackoffAndReconnects(t *testing.T) {
	driver := &runtimeResetDriver{id: "endpoint-change"}
	controller := New(&fakeCollector{}, time.Second)
	controller.reconnectEvery = time.Millisecond
	controller.AddDriver(driver)
	controller.mu.Lock()
	status := controller.statuses[driver.id]
	status.Connected = true
	status.ReconnectAttempts = 5
	status.RetryAt = time.Now().Add(time.Hour)
	controller.statuses[driver.id] = status
	controller.mu.Unlock()

	if err := controller.ResetDeviceConnection(context.Background(), driver.id, "COM 已变更"); err != nil {
		t.Fatal(err)
	}
	status = controller.Status().Devices[0]
	if status.Connected || status.ReconnectAttempts != 0 || !status.RetryAt.IsZero() || status.LastError != "COM 已变更" {
		t.Fatalf("端点更新后没有清除旧退避: %+v", status)
	}
	controller.reconnectDisconnected(context.Background())
	status = controller.Status().Devices[0]
	driver.mu.Lock()
	connects, closes := driver.connects, driver.closes
	driver.mu.Unlock()
	if !status.Connected || connects != 1 || closes != 1 {
		t.Fatalf("端点更新后没有立即重连: status=%+v connects=%d closes=%d", status, connects, closes)
	}
}

func TestResetDeviceConnectionsAtomicallyCommitsEndpointsOnlyAfterEveryClose(t *testing.T) {
	first := &blockingResetDriver{
		id: "a-loser", block: true, closeStarted: make(chan struct{}), releaseClose: make(chan struct{}), closed: make(chan struct{}),
	}
	second := &blockingResetDriver{id: "b-owner", closed: make(chan struct{})}
	controller := New(nil, time.Second)
	controller.AddDriver(first)
	controller.AddDriver(second)
	committed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- controller.ResetDeviceConnectionsAtomically(context.Background(), []ConnectionReset{
			{DeviceID: second.id, Reason: "owner move"},
			{DeviceID: first.id, Reason: "loser move"},
		}, func() { close(committed) })
	}()
	<-first.closeStarted
	select {
	case <-committed:
		t.Fatal("旧 loser 会话尚未关闭时就提交了新端点")
	default:
	}
	close(first.releaseClose)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.closed:
	default:
		t.Fatal("loser 未关闭")
	}
	select {
	case <-second.closed:
	default:
		t.Fatal("owner 未关闭")
	}
	select {
	case <-committed:
	default:
		t.Fatal("全部旧会话关闭后仍未提交端点")
	}
}
