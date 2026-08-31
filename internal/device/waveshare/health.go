package waveshare

import (
	"context"
	"errors"
	"fmt"
	"time"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/waveshare"
)

const healthLockWait = 250 * time.Millisecond

// CheckConnection 通过无副作用的 find_device 握手确认屏幕仍然在线。
// 驻留静态主题可能很长时间没有遥测发送，不能只依赖旧串口句柄。
func (driver *Driver) CheckConnection(ctx context.Context) error {
	if !driver.lockHealthCheck(ctx) {
		return device.ErrHealthCheckBusy
	}
	defer driver.mu.Unlock()
	if !driver.connected || driver.Port == nil || driver.AckGate == nil {
		return errors.New("微雪屏幕未连接")
	}
	response, err := driver.sendAndWaitLocked(ctx, protocol.CommandFindDevice, nil)
	if err != nil {
		return fmt.Errorf("微雪设备健康检查: %w", err)
	}
	driver.updateDeviceInfo(response.Payload)
	return nil
}

func (driver *Driver) lockHealthCheck(ctx context.Context) bool {
	if driver.mu.TryLock() {
		return true
	}
	timer := time.NewTimer(healthLockWait)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ticker.C:
			if driver.mu.TryLock() {
				return true
			}
		}
	}
}
