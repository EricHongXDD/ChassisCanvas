package service

import (
	"context"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type smallPowerDriver struct{ powerTestDriver }

func (driver *smallPowerDriver) Info() device.Info {
	info := driver.powerTestDriver.Info()
	info.Capabilities = device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true}
	return info
}

type droppedSmallPowerDriver struct {
	smallPowerDriver
	off     bool
	packets []telemetry.Snapshot
}

func (driver *droppedSmallPowerDriver) SetBrightness(ctx context.Context, value int) error {
	if err := driver.powerTestDriver.SetBrightness(ctx, value); err != nil {
		return err
	}
	driver.off = value == 0
	return nil
}

func (driver *droppedSmallPowerDriver) SendTelemetry(_ context.Context, snapshot telemetry.Snapshot) error {
	if driver.off {
		return device.ErrRealtimeFrameDropped
	}
	driver.packets = append(driver.packets, snapshot)
	return nil
}

func TestLyricsDisabledWhileOffClearOnlyAfterRestore(t *testing.T) {
	driver := &droppedSmallPowerDriver{smallPowerDriver: smallPowerDriver{powerTestDriver: powerTestDriver{id: "off-clear"}}}
	controller := New(&fakeCollector{}, time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	controller.SetLyricsProvider(&eventLyrics{})
	controller.SetLyricsEnabled(true)
	if err := controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.SetLyricsEnabled(false)
	manifest := powerTestManifest("off-lyrics")
	manifest.Layers = append(manifest.Layers, theme.Layer{
		ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .1, Y: .6, Width: .8, Height: .3}, Opacity: 1,
	})
	if err := controller.ApplyThemeTo(context.Background(), driver.id, manifest)[driver.id]; err != nil {
		t.Fatalf("息屏跳过数据导致主题保存失败: %v", err)
	}
	controller.tick(context.Background())
	if !controller.lyricsClearPending || len(driver.packets) != 0 {
		t.Fatal("息屏时把未发送的清空包记成已生效")
	}
	if err := controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id]; err != nil {
		t.Fatal(err)
	}
	controller.tick(context.Background())
	if controller.lyricsClearPending || len(driver.packets) == 0 || !driver.packets[len(driver.packets)-1].LyricsClear {
		t.Fatal("恢复后没有真正清除息屏期间关闭的歌词")
	}
}

func TestSmallPacketPowerRestoreSchedulesCurrentStateImmediately(t *testing.T) {
	driver := &smallPowerDriver{powerTestDriver: powerTestDriver{id: "small-power"}}
	controller := New(&fakeCollector{}, 60*time.Second)
	controller.AddDriver(driver)
	controller.connectAll(context.Background())
	if err := controller.SetPoweredOff(context.Background(), driver.id, true)[driver.id]; err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.telemetryWake:
		t.Fatal("息屏意图触发了显示刷新")
	default:
	}
	if err := controller.SetPoweredOff(context.Background(), driver.id, false)[driver.id]; err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.telemetryWake:
	default:
		t.Fatal("恢复后仍要等待 60 秒采集周期")
	}
	select {
	case <-controller.smallPacketWake:
	default:
		t.Fatal("恢复后没有立即调度当前歌词")
	}
}
