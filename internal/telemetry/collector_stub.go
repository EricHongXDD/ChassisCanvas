//go:build !windows

package telemetry

import (
	"errors"
	"time"
)

type Collector struct{}

func NewCollector() *Collector { return &Collector{} }

func (collector *Collector) SetGPUTarget(string) (GPUTargetStatus, error) {
	return GPUTargetStatus{ConfiguredID: AutomaticGPUAdapterID, Automatic: true}, errors.New("当前系统不支持 GPU 监控目标选择")
}

func (collector *Collector) GPUTargetStatus() GPUTargetStatus {
	return GPUTargetStatus{ConfiguredID: AutomaticGPUAdapterID, Automatic: true}
}

func (collector *Collector) Capture() (Snapshot, error) {
	return Snapshot{CapturedAt: time.Now()}, nil
}
