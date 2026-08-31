package edgesetup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CommandClock 使用 BusyBox date 设置系统时间；RTCPath 为空或设备不存在时
// 仍然成功，因此可用于没有 RTC 或 RTC 不带后备电池的屏幕。
type CommandClock struct {
	DateBinary    string
	HWClockBinary string
	RTCPath       string
	Now           func() time.Time
}

func (clock CommandClock) Status(_ context.Context) ClockStatus {
	now := time.Now()
	if clock.Now != nil {
		now = clock.Now()
	}
	ready := !now.Before(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) &&
		now.Before(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	status := ClockStatus{Ready: ready, Current: now}
	if !ready {
		status.Reason = "系统时间不可信；平台 HTTPS 保持禁用"
	}
	return status
}

func (clock CommandClock) SetUnix(ctx context.Context, unixSeconds int64) error {
	dateBinary := strings.TrimSpace(clock.DateBinary)
	if dateBinary == "" {
		dateBinary = "/bin/date"
	}
	command := exec.CommandContext(ctx, dateBinary, "-s", "@"+strconv.FormatInt(unixSeconds, 10))
	if err := command.Run(); err != nil {
		return errors.New("无法设置系统时间")
	}
	rtcPath := strings.TrimSpace(clock.RTCPath)
	hwclockBinary := strings.TrimSpace(clock.HWClockBinary)
	if rtcPath == "" || hwclockBinary == "" {
		return nil
	}
	if _, err := os.Stat(rtcPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return errors.New("无法检查 RTC")
	}
	if err := exec.CommandContext(ctx, hwclockBinary, "-w", "-u", "-f", rtcPath).Run(); err != nil {
		return errors.New("系统时间已设置，但无法写入 RTC")
	}
	return nil
}
