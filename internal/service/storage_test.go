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

type storageTestDriver struct {
	mu         sync.Mutex
	id         string
	connected  bool
	refreshes  int
	refreshErr error
	storage    device.StorageInfo
}

type blockingStorageDriver struct {
	storageTestDriver
	started chan struct{}
	release chan struct{}
}

func (driver *blockingStorageDriver) RefreshStorage(ctx context.Context) error {
	select {
	case driver.started <- struct{}{}:
	default:
	}
	select {
	case <-driver.release:
		return driver.storageTestDriver.RefreshStorage(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (driver *storageTestDriver) Info() device.Info {
	id := driver.id
	if id == "" {
		id = "storage-test"
	}
	return device.Info{ID: id, Name: "容量测试屏"}
}

func (driver *storageTestDriver) Connect(context.Context) error {
	driver.mu.Lock()
	driver.connected = true
	driver.mu.Unlock()
	return nil
}

func (*storageTestDriver) ApplyTheme(context.Context, theme.Manifest) error { return nil }
func (*storageTestDriver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}
func (*storageTestDriver) Close() error { return nil }

func (driver *storageTestDriver) RefreshStorage(context.Context) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.refreshes++
	if driver.refreshErr != nil {
		driver.storage.Error = driver.refreshErr.Error()
		return driver.refreshErr
	}
	driver.storage = device.StorageInfo{
		Supported: true, TotalBytes: 8 << 30, AvailableBytes: 6 << 30,
		UsedBytes: 2 << 30, UpdatedAt: time.Now(),
	}
	return nil
}

func (driver *storageTestDriver) StorageInfo() device.StorageInfo {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.storage
}

func TestStatusReadsStorageReporterCache(t *testing.T) {
	driver := &storageTestDriver{storage: device.StorageInfo{
		Supported: true, TotalBytes: 8 << 30, AvailableBytes: 5 << 30, UsedBytes: 3 << 30,
	}}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	status := controller.Status().Devices[0]
	if status.Storage == nil || !status.Storage.Supported || status.Storage.AvailableBytes != 5<<30 {
		t.Fatalf("设备状态没有读取容量缓存: %+v", status)
	}

	plain := New(&fakeCollector{}, time.Second)
	plain.AddDriver(&fakeDriver{})
	if plain.Status().Devices[0].Storage != nil {
		t.Fatal("不支持容量查询的设备不应暴露 storage")
	}
}

func TestRefreshDeviceStorageQueriesSelectedConnectedReporter(t *testing.T) {
	selected := &storageTestDriver{id: "waveshare-7"}
	other := &storageTestDriver{id: "waveshare-8.8"}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(selected)
	controller.AddDriver(other)
	controller.mu.Lock()
	status := controller.statuses["waveshare-7"]
	status.Connected = true
	controller.statuses["waveshare-7"] = status
	otherStatus := controller.statuses["waveshare-8.8"]
	otherStatus.Connected = true
	controller.statuses["waveshare-8.8"] = otherStatus
	controller.mu.Unlock()

	storage, err := controller.RefreshDeviceStorage(context.Background(), "waveshare-7")
	if err != nil {
		t.Fatalf("主动刷新容量失败：%v", err)
	}
	if !storage.Supported || storage.TotalBytes != 8<<30 || storage.AvailableBytes != 6<<30 {
		t.Fatalf("主动刷新返回错误容量：%+v", storage)
	}
	selected.mu.Lock()
	selectedRefreshes := selected.refreshes
	selected.mu.Unlock()
	other.mu.Lock()
	otherRefreshes := other.refreshes
	other.mu.Unlock()
	if selectedRefreshes != 1 || otherRefreshes != 0 {
		t.Fatalf("主动刷新触碰了错误设备：selected=%d other=%d", selectedRefreshes, otherRefreshes)
	}

	// 状态 API 与后台轮询读取同一个驱动缓存，因此主动刷新结果会立即可见。
	status = controller.Status().Devices[0]
	if status.Storage == nil || status.Storage.UpdatedAt.IsZero() || status.Storage.UsedBytes != 2<<30 {
		t.Fatalf("状态没有读取最近主动刷新结果：%+v", status.Storage)
	}
}

func TestRefreshDeviceStorageCoalescesWithBackgroundQuery(t *testing.T) {
	driver := &blockingStorageDriver{
		storageTestDriver: storageTestDriver{id: "waveshare-7"},
		started:           make(chan struct{}, 1),
		release:           make(chan struct{}),
	}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.mu.Lock()
	status := controller.statuses["waveshare-7"]
	status.Connected = true
	controller.statuses["waveshare-7"] = status
	controller.mu.Unlock()

	backgroundDone := make(chan struct{})
	go func() {
		controller.refreshStorage(context.Background())
		close(backgroundDone)
	}()
	<-driver.started

	type refreshResult struct {
		storage device.StorageInfo
		err     error
	}
	activeStarted := make(chan struct{})
	result := make(chan refreshResult, 1)
	go func() {
		close(activeStarted)
		storage, err := controller.RefreshDeviceStorage(context.Background(), "waveshare-7")
		result <- refreshResult{storage: storage, err: err}
	}()
	<-activeStarted
	time.Sleep(5 * time.Millisecond)
	close(driver.release)
	<-backgroundDone
	active := <-result
	if active.err != nil || !active.storage.Supported || active.storage.UpdatedAt.IsZero() {
		t.Fatalf("主动刷新没有复用刚完成的后台查询：storage=%+v err=%v", active.storage, active.err)
	}
	driver.mu.Lock()
	refreshes := driver.refreshes
	driver.mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("并发容量请求没有合并，实际访问设备 %d 次", refreshes)
	}
}

func TestRefreshDeviceStorageRejectsUnknownDisconnectedAndUnsupportedDevices(t *testing.T) {
	controller := New(&fakeCollector{}, time.Second)
	storageDriver := &storageTestDriver{id: "waveshare-7"}
	controller.AddDriver(storageDriver)
	controller.AddDriver(&fakeDriver{})

	if _, err := controller.RefreshDeviceStorage(context.Background(), "missing"); err == nil {
		t.Fatal("不存在的设备应拒绝主动刷新")
	}
	if _, err := controller.RefreshDeviceStorage(context.Background(), "waveshare-7"); err == nil {
		t.Fatal("未连接设备应拒绝主动刷新")
	}
	controller.mu.Lock()
	plainStatus := controller.statuses["fake"]
	plainStatus.Connected = true
	controller.statuses["fake"] = plainStatus
	controller.mu.Unlock()
	if _, err := controller.RefreshDeviceStorage(context.Background(), "fake"); err == nil {
		t.Fatal("没有 StorageReporter 的设备应拒绝主动刷新")
	}
	storageDriver.mu.Lock()
	refreshes := storageDriver.refreshes
	storageDriver.mu.Unlock()
	if refreshes != 0 {
		t.Fatalf("失败分支不应访问容量驱动，实际刷新 %d 次", refreshes)
	}
}

func TestRunRefreshesStorageWithoutChangingConnectionError(t *testing.T) {
	driver := &storageTestDriver{refreshErr: errors.New("容量查询失败")}
	controller := New(&fakeCollector{}, time.Second)
	controller.storageRefreshEvery = 5 * time.Millisecond
	controller.storageRefreshTimeout = 20 * time.Millisecond
	controller.AddDriver(driver)
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Millisecond)
	defer cancel()
	if err := controller.Run(ctx); err != nil {
		t.Fatal(err)
	}

	driver.mu.Lock()
	refreshes := driver.refreshes
	driver.mu.Unlock()
	if refreshes == 0 {
		t.Fatal("Run 没有按周期后台刷新容量")
	}
	status := controller.Status().Devices[0]
	if !status.Connected || status.LastError != "" {
		t.Fatalf("容量查询失败污染了连接状态: %+v", status)
	}
	if status.Storage == nil || status.Storage.Error != "容量查询失败" {
		t.Fatalf("容量查询错误没有留在容量缓存中: %+v", status.Storage)
	}
}

func TestRunSkipsKnownUnsupportedStorageCapability(t *testing.T) {
	driver := &storageTestDriver{storage: device.StorageInfo{
		Supported: false,
		UpdatedAt: time.Now(),
	}}
	controller := New(&fakeCollector{}, time.Second)
	controller.storageRefreshEvery = 5 * time.Millisecond
	controller.storageRefreshTimeout = 20 * time.Millisecond
	controller.AddDriver(driver)
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Millisecond)
	defer cancel()
	if err := controller.Run(ctx); err != nil {
		t.Fatal(err)
	}

	driver.mu.Lock()
	refreshes := driver.refreshes
	driver.mu.Unlock()
	if refreshes != 0 {
		t.Fatalf("已确认不支持容量的固件不应继续自动轮询，实际刷新 %d 次", refreshes)
	}
	status := controller.Status().Devices[0]
	if !status.Connected || status.LastError != "" {
		t.Fatalf("跳过可选容量查询不应影响连接状态: %+v", status)
	}
	if status.Storage == nil || status.Storage.Supported || status.Storage.Error != "" {
		t.Fatalf("不支持容量的状态应保持干净: %+v", status.Storage)
	}
}
