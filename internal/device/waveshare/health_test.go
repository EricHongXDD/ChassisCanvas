package waveshare

import (
	"context"
	"errors"
	"testing"
	"time"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/waveshare"
)

func TestHealthCheckConfirmsPhysicalWaveshareSession(t *testing.T) {
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave-health", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(gate.commands)
	if err := driver.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gate.commands) != before+1 || gate.commands[len(gate.commands)-1] != protocol.CommandFindDevice {
		t.Fatalf("健康检查没有使用 find_device: %v", gate.commands)
	}
	if !driver.Connected() {
		t.Fatal("健康检查成功后设备被误标记为断开")
	}
}

func TestHealthCheckDisconnectsFailedWaveshareSession(t *testing.T) {
	gate := &scriptedGate{themes: map[string]string{}, commandErrors: make(map[protocol.Command]error)}
	port := &memoryPort{}
	driver := New("wave-health", "微雪", [2]int{1024, 600}, port, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.commandErrors[protocol.CommandFindDevice] = errors.New("USB 已拔出")
	if err := driver.CheckConnection(context.Background()); err == nil {
		t.Fatal("拔出后健康检查错误地成功")
	}
	if driver.Connected() || !port.closed || driver.Port != nil {
		t.Fatalf("失败会话没有释放: connected=%v closed=%v port=%v", driver.Connected(), port.closed, driver.Port)
	}
}

func TestHealthCheckBusyDoesNotDisconnectWaveshare(t *testing.T) {
	driver := New("wave-health", "微雪", [2]int{1024, 600}, &memoryPort{}, &scriptedGate{themes: map[string]string{}})
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	err := driver.CheckConnection(ctx)
	cancel()
	driver.mu.Unlock()
	if !errors.Is(err, device.ErrHealthCheckBusy) || !driver.Connected() {
		t.Fatalf("设备繁忙被误判为断开: err=%v connected=%v", err, driver.Connected())
	}
}
