package waveshare

import (
	"context"
	"fmt"

	protocol "casescreen/internal/protocol/waveshare"
)

// 调用者持有 driver.mu，确保息屏校正不会与用户恢复命令交错。
func (driver *Driver) poweredOffLocked() bool {
	return driver.brightnessKnown && driver.brightness == 0
}

func (driver *Driver) enforcePoweredOffLocked(ctx context.Context) error {
	if !driver.poweredOffLocked() {
		return nil
	}
	if _, err := driver.sendAndWaitLocked(ctx, protocol.CommandSetBrightness, []byte("0")); err != nil {
		return fmt.Errorf("维持微雪屏幕息屏: %w", err)
	}
	return nil
}
