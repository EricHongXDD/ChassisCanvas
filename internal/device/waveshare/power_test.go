package waveshare

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/telemetry"
)

// powerFirmwarePort 模拟 ACK 已到达但启动线程尚未接受第一次零亮度的固件。
type powerFirmwarePort struct {
	memoryPort
	physicalBrightness int
	ignoreFirstOff     bool
	brightnessWrites   []int
	commands           []protocol.Command
}

func (port *powerFirmwarePort) Write(data []byte) (int, error) {
	packet, err := protocol.DecodePacket(data)
	if err != nil {
		return 0, err
	}
	port.commands = append(port.commands, packet.Command)
	if packet.Command == protocol.CommandSetBrightness {
		value, err := strconv.Atoi(string(packet.Payload))
		if err != nil {
			return 0, err
		}
		port.brightnessWrites = append(port.brightnessWrites, value)
		if value == 0 && port.ignoreFirstOff {
			port.ignoreFirstOff = false
		} else {
			port.physicalBrightness = value
		}
	}
	return port.memoryPort.Write(data)
}

func newPowerFirmwareDriver(t *testing.T) (*Driver, *powerFirmwarePort, *scriptedGate) {
	t.Helper()
	port := &powerFirmwarePort{physicalBrightness: 255, ignoreFirstOff: true}
	gate := &scriptedGate{themes: map[string]string{}, commandErrors: map[protocol.Command]error{}}
	driver := New("wave-power", "微雪", [2]int{1024, 600}, port, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return driver, port, gate
}

func TestPoweredOffSurvivesStartupAckAndSilentFirmwareWake(t *testing.T) {
	driver, port, _ := newPowerFirmwareDriver(t)
	if err := driver.SetBrightness(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if port.physicalBrightness != 0 || !reflect.DeepEqual(port.brightnessWrites, []int{0, 0}) {
		t.Fatalf("启动 ACK 后没有稳定息屏: brightness=%d writes=%v", port.physicalBrightness, port.brightnessWrites)
	}
	// USB 没有断开，但屏端启动/主题线程晚到的操作重新打开了背光。
	port.physicalBrightness = 255
	if err := driver.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if port.physicalBrightness != 0 || !driver.Connected() {
		t.Fatal("在线设备的意外亮屏没有自动纠正")
	}
	if err := driver.SetBrightness(context.Background(), 255); err != nil {
		t.Fatal(err)
	}
	before := len(port.brightnessWrites)
	if err := driver.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if port.physicalBrightness != 255 || len(port.brightnessWrites) != before {
		t.Fatal("健康检查覆盖了用户最新的恢复意图")
	}
}

func TestPoweredOffSkipsDynamicPacketsAndResendsSameValuesOnRestore(t *testing.T) {
	driver, port, _ := newPowerFirmwareDriver(t)
	snapshot := telemetry.Snapshot{OmitClock: true, OmitHardwareMetrics: true, LyricsClear: true}
	values := map[string]string{"Audio_00": "42"}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendSpectrumValues(context.Background(), values); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetBrightness(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	before := len(port.commands)
	if err := driver.SendTelemetry(context.Background(), snapshot); !errors.Is(err, device.ErrRealtimeFrameDropped) {
		t.Fatalf("跳过的遥测被当作已确认: %v", err)
	}
	if err := driver.SendSpectrumValues(context.Background(), values); !errors.Is(err, device.ErrRealtimeFrameDropped) {
		t.Fatalf("跳过的频谱被当作已确认: %v", err)
	}
	if len(port.commands) != before {
		t.Fatal("息屏期间仍向屏端发送显示更新")
	}
	if err := driver.SetBrightness(context.Background(), 255); err != nil {
		t.Fatal(err)
	}
	before = len(port.commands)
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendSpectrumValues(context.Background(), values); err != nil {
		t.Fatal(err)
	}
	if len(port.commands) != before+2 {
		t.Fatal("恢复后相同的当前内容被旧去重缓存吞掉")
	}
}

func TestPoweredOffRetainedAcrossDriverReconnect(t *testing.T) {
	driver, _, gate := newPowerFirmwareDriver(t)
	if err := driver.SetBrightness(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	port := &powerFirmwarePort{physicalBrightness: 255}
	driver.Port, driver.AckGate = port, gate
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if port.physicalBrightness != 0 {
		t.Fatal("新会话沿用了硬件开屏状态")
	}
}

func TestPoweredOffHealthFailureIsReportedAndDisconnects(t *testing.T) {
	driver, _, gate := newPowerFirmwareDriver(t)
	if err := driver.SetBrightness(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	want := errors.New("息屏命令 ACK 丢失")
	gate.commandErrors[protocol.CommandSetBrightness] = want
	if err := driver.CheckConnection(context.Background()); !errors.Is(err, want) {
		t.Fatalf("息屏校正错误没有传播: %v", err)
	}
	if driver.Connected() {
		t.Fatal("校正失败仍宣称会话可用")
	}
}

func TestPoweredOffSettleHonorsCancellation(t *testing.T) {
	driver, port, gate := newPowerFirmwareDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate.waitHook = func(command protocol.Command) {
		if command == protocol.CommandSetBrightness {
			cancel()
		}
	}
	if err := driver.SetBrightness(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待显示线程时没有响应取消: %v", err)
	}
	if len(port.brightnessWrites) != 1 {
		t.Fatal("取消后仍继续发送亮度命令")
	}
}
