package service

import (
	"testing"
	"time"

	"casescreen/internal/telemetry"
)

type switchableGPUCollector struct {
	status   telemetry.GPUTargetStatus
	snapshot telemetry.Snapshot
	captures int
}

func (collector *switchableGPUCollector) Capture() (telemetry.Snapshot, error) {
	collector.captures++
	return collector.snapshot, nil
}

func (collector *switchableGPUCollector) SetGPUTarget(adapterID string) (telemetry.GPUTargetStatus, error) {
	collector.status.ConfiguredID = adapterID
	collector.status.EffectiveID = adapterID
	collector.status.EffectiveName = collector.snapshot.GPUModel
	return collector.status, nil
}

func (collector *switchableGPUCollector) GPUTargetStatus() telemetry.GPUTargetStatus {
	return collector.status
}

func TestSetGPUTargetClearsOldValuesAndCapturesImmediately(t *testing.T) {
	load, oldTemperature := 88.0, 91.0
	collector := &switchableGPUCollector{
		status:   telemetry.GPUTargetStatus{Adapters: []telemetry.GPUAdapter{{ID: `PCI\VEN_8086`, Name: "Intel(R) Graphics"}}},
		snapshot: telemetry.Snapshot{CapturedAt: time.Now(), GPUModel: "Intel(R) Graphics"},
	}
	controller := New(collector, time.Minute)
	controller.lastSnapshot = telemetry.Snapshot{
		GPUModel: "AMD Radeon RX 7900 XTX", GPUPercent: &load,
		GPUTemperature: &oldTemperature, GPUHotspotTemperature: &oldTemperature,
		GPUMemoryTemperature: &oldTemperature,
	}
	status, err := controller.SetGPUTarget(`PCI\VEN_8086`)
	if err != nil {
		t.Fatal(err)
	}
	if status.EffectiveName != "Intel(R) Graphics" || collector.captures != 1 {
		t.Fatalf("切换没有立即采样: status=%+v captures=%d", status, collector.captures)
	}
	snapshot := controller.Status().LastSnapshot
	if snapshot.GPUModel != "Intel(R) Graphics" || snapshot.GPUPercent != nil || snapshot.GPUTemperature != nil ||
		snapshot.GPUHotspotTemperature != nil || snapshot.GPUMemoryTemperature != nil {
		t.Fatalf("新目标仍残留旧显卡数据: %+v", snapshot)
	}
	select {
	case <-controller.telemetryWake:
	default:
		t.Fatal("切换后没有唤醒遥测发送")
	}
}
