package coolermaster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/coolermaster"
)

const (
	healthLockWait               = 250 * time.Millisecond
	recentRealtimeIOHealthWindow = 2 * time.Second
)

// CheckConnection 读取一次 param 状态，确认离线槽主题正在使用的 HID
// 句柄仍然对应真实设备。探测不会切换实时/离线显示模式。
func (driver *Driver) CheckConnection(ctx context.Context) error {
	if !driver.lockHealthCheck(ctx) {
		return device.ErrHealthCheckBusy
	}
	defer driver.mu.Unlock()
	if !driver.connected || driver.Transport == nil {
		return errors.New("酷冷至尊屏幕未连接")
	}
	// 某些酷冷固件在 realtimeDisplay 持续接收 JPEG 时不回
	// param。最近一帧已完整写入说明当前 HID 会话仍可用，
	// 此时再插入 param 只会与实时帧争用并造成假断线。
	if driver.realtime && !driver.lastSuccessfulRealtimeIO.IsZero() &&
		time.Since(driver.lastSuccessfulRealtimeIO) <= recentRealtimeIOHealthWindow {
		return nil
	}
	report, err := protocol.BuildParamRequest()
	if err != nil {
		return err
	}
	// 先清掉可能滞留的实时 JPEG ACK，再读取本次 param 状态。
	driver.drainResponsesLocked(ctx)
	if err := driver.writeReportLocked(ctx, report); err != nil {
		if healthCheckInterrupted(ctx, err) {
			return device.ErrHealthCheckBusy
		}
		driver.disconnectOnFatalTransportErrorLocked(err)
		return fmt.Errorf("写入酷冷至尊健康检查: %w", err)
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		if healthCheckInterrupted(ctx, err) {
			return device.ErrHealthCheckBusy
		}
		return err
	}
	response, err := driver.readResponseWithTimeoutLocked(ctx, time.Second)
	if err != nil {
		if healthCheckInterrupted(ctx, err) {
			// ATMOS V2 的 param 在部分显示状态下不回包，但 HID
			// 写入已经成功。把无响应当作本轮不可验证，保留句柄让
			// 下一轮继续探测；真正拔出时 hidapi 写入或读取会返回
			// 非超时错误，再由下面的路径释放坏会话。
			return device.ErrHealthCheckBusy
		}
		driver.disconnectOnFatalTransportErrorLocked(err)
		return fmt.Errorf("读取酷冷至尊健康检查: %w", err)
	}
	if err := protocol.ValidateStatus(response); err != nil {
		// 一条损坏或过期回包不足以证明 HID 已断开。
		// 保留句柄，下一轮会重新探测，避免 UI 瞬间闪离线。
		return fmt.Errorf("验证酷冷至尊健康检查: %w", err)
	}
	return nil
}

func healthCheckInterrupted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
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
