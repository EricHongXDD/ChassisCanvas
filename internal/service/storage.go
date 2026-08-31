package service

import (
	"context"
	"fmt"
	"time"

	"casescreen/internal/device"
)

// RefreshDeviceStorage 立即向指定且已连接的设备查询一次本地存储容量。
// 该查询只更新驱动自己的容量缓存，不改变设备连接状态或最近传输错误。
func (service *Service) RefreshDeviceStorage(ctx context.Context, deviceID string) (device.StorageInfo, error) {
	requestStarted := time.Now()
	service.mu.RLock()
	var selected device.Driver
	for _, driver := range service.drivers {
		if driver.Info().ID == deviceID {
			selected = driver
			break
		}
	}
	status, statusFound := service.statuses[deviceID]
	timeout := service.storageRefreshTimeout
	service.mu.RUnlock()

	if selected == nil || !statusFound {
		return device.StorageInfo{}, fmt.Errorf("未找到设备 %q", deviceID)
	}
	if !status.Connected {
		return device.StorageInfo{}, fmt.Errorf("设备 %q 尚未连接", deviceID)
	}
	reporter, ok := selected.(device.StorageReporter)
	if !ok {
		return device.StorageInfo{}, fmt.Errorf("设备 %q 不支持容量查询", deviceID)
	}

	// 后台十秒轮询与界面按钮可能同时到达。先在服务层合并容量查询，避免按钮
	// 排在一个正在进行的串口查询后面时，自己的超时在拿到驱动锁之前就耗尽。
	service.storageRefreshMu.Lock()
	defer service.storageRefreshMu.Unlock()
	storage := reporter.StorageInfo()
	if storage.UpdatedAt.After(requestStarted) && storage.Error == "" {
		return storage, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	refreshContext, cancel := context.WithTimeout(ctx, timeout)
	err := reporter.RefreshStorage(refreshContext)
	cancel()
	storage = reporter.StorageInfo()
	if err != nil {
		return storage, err
	}
	return storage, nil
}

// runStorageRefresh 在独立协程中刷新设备存储容量，避免容量查询阻塞遥测调度。
func (service *Service) runStorageRefresh(ctx context.Context) {
	service.mu.RLock()
	interval := service.storageRefreshEvery
	service.mu.RUnlock()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			service.refreshStorage(ctx)
		}
	}
}

func (service *Service) refreshStorage(ctx context.Context) {
	// 与主动刷新共用单飞锁；一次只允许一个容量命令占用设备串口。
	service.storageRefreshMu.Lock()
	defer service.storageRefreshMu.Unlock()

	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	statuses := make(map[string]DeviceStatus, len(service.statuses))
	for id, status := range service.statuses {
		statuses[id] = status
	}
	timeout := service.storageRefreshTimeout
	service.mu.RUnlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	for _, driver := range drivers {
		if !statuses[driver.Info().ID].Connected {
			continue
		}
		reporter, ok := driver.(device.StorageReporter)
		if !ok {
			continue
		}
		storage := reporter.StorageInfo()
		// 成功回应过但明确没有容量字段，表示当前固件不提供这项可选能力。
		// 自动轮询不再反复占用串口；设备重连时驱动仍会重新探测一次。
		if !storage.Supported && storage.Error == "" && !storage.UpdatedAt.IsZero() {
			continue
		}
		refreshContext, cancel := context.WithTimeout(ctx, timeout)
		_ = reporter.RefreshStorage(refreshContext)
		cancel()
		// 容量查询是可选能力；失败信息只保存在驱动容量缓存中，
		// 不应覆盖主连接状态或最近一次传输错误。
	}
}
