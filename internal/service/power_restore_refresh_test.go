package service

import (
	"context"
	"testing"
	"time"

	"casescreen/internal/device"
)

type smallPowerDriver struct{ powerTestDriver }

func (driver *smallPowerDriver) Info() device.Info {
	info := driver.powerTestDriver.Info()
	info.Capabilities = device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true}
	return info
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
