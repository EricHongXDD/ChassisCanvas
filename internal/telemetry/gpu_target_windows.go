//go:build windows

package telemetry

import (
	"fmt"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	directXAdaptersRegistryPath = `SOFTWARE\Microsoft\DirectX`
	telemetryDisplayAttached    = 0x00000001
	telemetryDisplayPrimary     = 0x00000004
	telemetryDisplayRemote      = 0x04000000
	discreteVRAMFloor           = 512 * 1024 * 1024
)

// gpuTelemetryTarget 是硬件监控目标，与 Windows 针对 CaseScreen 进程的
// GPU 调度首选项无关。LUID 用于把 GPU Engine 性能计数器限定到同一块显卡。
type gpuTelemetryTarget struct {
	model                string
	deviceID             string
	luid                 uint64
	dedicatedVideoMemory uint64
	active               bool
	primary              bool
}

// readPreferredGPUTelemetryTarget 从物理显示适配器中独立选择监控目标。
// DeviceKey 中的 Control\Video GUID 与 DirectX 适配器注册项一一对应，后者
// 提供性能计数器实例使用的 AdapterLuid，不依赖程序实际在哪块 GPU 上渲染。
func readPreferredGPUTelemetryTarget() gpuTelemetryTarget {
	return choosePreferredGPUTelemetryTarget(readGPUTelemetryTargets())
}

// readGPUTelemetryTargets 枚举物理监控目标。顺序稳定，便于 API 和组件设置保持选择。
func readGPUTelemetryTargets() []gpuTelemetryTarget {
	targets := make([]gpuTelemetryTarget, 0, 4)
	for index := uint32(0); index < 32; index++ {
		device := displayDevice{Size: uint32(unsafe.Sizeof(displayDevice{}))}
		found, _, _ := procEnumDisplayDevicesW.Call(
			0,
			uintptr(index),
			uintptr(unsafe.Pointer(&device)),
			0,
		)
		if found == 0 {
			break
		}

		model := normalizeHardwareName(windows.UTF16ToString(device.DeviceString[:]))
		deviceID := strings.TrimSpace(windows.UTF16ToString(device.DeviceID[:]))
		if !validTelemetryDisplayAdapter(model, deviceID, device.StateFlags) {
			continue
		}
		luid, dedicatedVideoMemory, ok := readDirectXAdapterIdentity(
			windows.UTF16ToString(device.DeviceKey[:]),
		)
		if !ok {
			continue
		}
		targets = append(targets, gpuTelemetryTarget{
			model:                model,
			deviceID:             deviceID,
			luid:                 luid,
			dedicatedVideoMemory: dedicatedVideoMemory,
			active:               device.StateFlags&telemetryDisplayAttached != 0,
			primary:              device.StateFlags&telemetryDisplayPrimary != 0,
		})
	}
	// EnumDisplayDevices 会为同一块显卡的多个输出端口重复返回同一 PCI DeviceID。
	// 组件设置只应列出物理显卡一次，并保留最活跃输出对应的 LUID。
	unique := make(map[string]gpuTelemetryTarget, len(targets))
	for _, target := range targets {
		key := strings.ToLower(strings.TrimSpace(target.deviceID))
		current, exists := unique[key]
		if !exists {
			unique[key] = target
			continue
		}
		active, primary := current.active || target.active, current.primary || target.primary
		memory := max(current.dedicatedVideoMemory, target.dedicatedVideoMemory)
		if betterGPUTelemetryTarget(target, current) {
			current = target
		}
		current.active, current.primary, current.dedicatedVideoMemory = active, primary, memory
		unique[key] = current
	}
	targets = targets[:0]
	for _, target := range unique {
		targets = append(targets, target)
	}
	sort.SliceStable(targets, func(left, right int) bool {
		return strings.ToLower(targets[left].deviceID) < strings.ToLower(targets[right].deviceID)
	})
	return targets
}

func resolveGPUTelemetryTarget(adapterID string) (gpuTelemetryTarget, []gpuTelemetryTarget, error) {
	targets := readGPUTelemetryTargets()
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" || strings.EqualFold(adapterID, AutomaticGPUAdapterID) {
		target := choosePreferredGPUTelemetryTarget(targets)
		if target.luid == 0 {
			return gpuTelemetryTarget{}, targets, fmt.Errorf("没有检测到可监控的物理 GPU")
		}
		return target, targets, nil
	}
	for _, target := range targets {
		if strings.EqualFold(target.deviceID, adapterID) {
			return target, targets, nil
		}
	}
	return gpuTelemetryTarget{}, targets, fmt.Errorf("没有找到监控 GPU: %s", adapterID)
}

func publicGPUAdapters(targets []gpuTelemetryTarget) []GPUAdapter {
	adapters := make([]GPUAdapter, 0, len(targets))
	for _, target := range targets {
		adapters = append(adapters, GPUAdapter{
			ID: target.deviceID, Name: target.model,
			DedicatedVideoMemory: target.dedicatedVideoMemory,
			Active:               target.active, Primary: target.primary,
		})
	}
	return adapters
}

func validTelemetryDisplayAdapter(model, deviceID string, flags uint32) bool {
	if model == "" || flags&displayMirroring != 0 || flags&telemetryDisplayRemote != 0 {
		return false
	}
	lowerID := strings.ToLower(strings.TrimSpace(deviceID))
	if lowerID != "" && !strings.Contains(lowerID, `pci\ven_`) {
		return false
	}
	return displayAdapterScore(strings.ToLower(model)) >= 0
}

func readDirectXAdapterIdentity(deviceKey string) (uint64, uint64, bool) {
	subKey := directXAdapterSubKey(deviceKey)
	if subKey == "" {
		return 0, 0, false
	}
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		directXAdaptersRegistryPath+`\`+subKey,
		registry.QUERY_VALUE,
	)
	if err != nil {
		return 0, 0, false
	}
	defer key.Close()

	luid, _, err := key.GetIntegerValue("AdapterLuid")
	if err != nil || luid == 0 {
		return 0, 0, false
	}
	dedicatedVideoMemory, _, _ := key.GetIntegerValue("DedicatedVideoMemory")
	return luid, dedicatedVideoMemory, true
}

func directXAdapterSubKey(deviceKey string) string {
	deviceKey = strings.TrimSpace(deviceKey)
	start := strings.Index(deviceKey, "{")
	if start < 0 {
		return ""
	}
	end := strings.Index(deviceKey[start:], "}")
	if end < 0 {
		return ""
	}
	return deviceKey[start : start+end+1]
}

func choosePreferredGPUTelemetryTarget(targets []gpuTelemetryTarget) gpuTelemetryTarget {
	var best gpuTelemetryTarget
	found := false
	for _, target := range targets {
		if target.model == "" || target.luid == 0 {
			continue
		}
		if !found || betterGPUTelemetryTarget(target, best) {
			best = target
			found = true
		}
	}
	return best
}

func betterGPUTelemetryTarget(candidate, current gpuTelemetryTarget) bool {
	candidateDiscrete := looksLikeDiscreteGPU(candidate)
	currentDiscrete := looksLikeDiscreteGPU(current)
	if candidateDiscrete != currentDiscrete {
		return candidateDiscrete
	}
	if candidate.dedicatedVideoMemory != current.dedicatedVideoMemory {
		return candidate.dedicatedVideoMemory > current.dedicatedVideoMemory
	}
	candidateScore := displayAdapterScore(strings.ToLower(candidate.model))
	currentScore := displayAdapterScore(strings.ToLower(current.model))
	if candidateScore != currentScore {
		return candidateScore > currentScore
	}
	if candidate.primary != current.primary {
		return candidate.primary
	}
	if candidate.active != current.active {
		return candidate.active
	}
	return strings.ToLower(candidate.deviceID) < strings.ToLower(current.deviceID)
}

func looksLikeDiscreteGPU(target gpuTelemetryTarget) bool {
	lower := strings.ToLower(target.model)
	if strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") ||
		strings.Contains(lower, "quadro") || strings.Contains(lower, " rtx") ||
		strings.Contains(lower, " gtx") || strings.Contains(lower, "radeon rx") ||
		strings.Contains(lower, "radeon pro") || strings.Contains(lower, "intel arc") {
		return true
	}
	if strings.Contains(lower, "integrated") || strings.Contains(lower, " iris") ||
		strings.Contains(lower, " uhd") || strings.Contains(lower, "radeon graphics") {
		return false
	}
	return target.dedicatedVideoMemory >= discreteVRAMFloor
}
