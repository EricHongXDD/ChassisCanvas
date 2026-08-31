package service

import (
	"context"
	"errors"
	"time"

	"casescreen/internal/device"
)

type healthCheckTarget struct {
	id              string
	driver          device.HealthCheckDriver
	connectedAt     time.Time
	statusConnected bool
}

// runConnectionHealthChecks 用独立的低频循环验证物理 USB 在线状态。
// 静态驻留主题没有日常传输，若只检查内存中的 connected
// 标志，拔出后 /api/status 会永久粘在在线。
func (service *Service) runConnectionHealthChecks(ctx context.Context) {
	interval := service.healthCheckEvery
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			service.checkConnectedDevices(ctx)
		}
	}
}

func (service *Service) checkConnectedDevices(ctx context.Context) {
	service.mu.RLock()
	targets := make([]healthCheckTarget, 0, len(service.drivers))
	for _, driver := range service.drivers {
		checker, ok := driver.(device.HealthCheckDriver)
		if !ok {
			continue
		}
		id := driver.Info().ID
		status := service.statuses[id]
		if !status.Connected {
			continue
		}
		targets = append(targets, healthCheckTarget{
			id: id, driver: checker,
			connectedAt: status.LastConnectedAt, statusConnected: status.Connected,
		})
	}
	service.mu.RUnlock()

	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		timeout := service.healthCheckTimeout
		if timeout <= 0 {
			timeout = 1500 * time.Millisecond
		}
		checkContext, cancel := context.WithTimeout(ctx, timeout)
		// 健康探测与主题切换共用同一设备门闩。若探测先到，
		// 它会在测试图之前完成；若测试图先到，门闩内的权威
		// Active 复查会跳过可能刷新离线画面的驱动探测。
		unlockDevice, lockErr := service.lockDeviceTheme(checkContext, target.id)
		if lockErr != nil {
			cancel()
			continue
		}
		if service.hasOrientationCalibration(target.id) {
			unlockDevice()
			cancel()
			continue
		}
		err := target.driver.CheckConnection(checkContext)
		unlockDevice()
		cancel()
		if errors.Is(err, device.ErrHealthCheckBusy) {
			continue
		}
		now := time.Now()
		id := target.id
		invalidateHostFrames := false
		service.mu.Lock()
		status := service.statuses[id]
		// 不要让一个过期的探测结果覆盖刚刚完成的新会话。
		if status.Connected == target.statusConnected && status.LastConnectedAt.Equal(target.connectedAt) {
			status.LastConnectionCheck = now
			if err == nil {
				status.Info = target.driver.Info()
				status.LastVerifiedAt = now
				status.LastError = service.themeApplyErrors[id]
			} else if reporter, ok := target.driver.(device.ConnectionStateDriver); ok && reporter.Connected() {
				// 固件在忙碌或切换显示模式时可能返回过期/损坏 ACK。
				// 驱动仍持有可用 HID 会话时，本轮只是“未验证”，
				// 不是拔出证据；保持在线并留待下一轮健康检查。
				status.LastError = err.Error()
			} else {
				status = service.recordTransferDisconnectLocked(status, err, now)
				_, invalidateHostFrames = target.driver.(device.HostFrameDriver)
			}
			service.statuses[id] = status
		}
		service.mu.Unlock()
		if invalidateHostFrames {
			service.invalidateHostFrameDelivery(id)
		}
	}
}
