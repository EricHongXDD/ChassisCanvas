//go:build windows

package gpupreference

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	displayDeviceAttachedToDesktop = 0x00000001
	displayDevicePrimaryDevice     = 0x00000004
	displayDeviceMirroringDriver   = 0x00000008
	displayDeviceRemote            = 0x04000000
)

var (
	user32DLL              = windows.NewLazySystemDLL("user32.dll")
	enumDisplayDevicesProc = user32DLL.NewProc("EnumDisplayDevicesW")
)

type windowsDisplayDevice struct {
	Size         uint32
	DeviceName   [32]uint16
	DeviceString [128]uint16
	StateFlags   uint32
	DeviceID     [128]uint16
	DeviceKey    [128]uint16
}

type displayAdapterCandidate struct {
	name     string
	deviceID string
	flags    uint32
}

func enrichAdapterStatus(status Status) Status {
	status.Adapters = enumeratePhysicalDisplayAdapters()
	status.PowerSavingLabel, status.HighPerformanceLabel = adapterLabels(status.Adapters)
	return status
}

func enumeratePhysicalDisplayAdapters() []AdapterSummary {
	candidates := make([]displayAdapterCandidate, 0, 4)
	for index := uint32(0); index < 32; index++ {
		device := windowsDisplayDevice{Size: uint32(unsafe.Sizeof(windowsDisplayDevice{}))}
		found, _, _ := enumDisplayDevicesProc.Call(0, uintptr(index), uintptr(unsafe.Pointer(&device)), 0)
		if found == 0 {
			break
		}
		candidates = append(candidates, displayAdapterCandidate{
			name:     windows.UTF16ToString(device.DeviceString[:]),
			deviceID: windows.UTF16ToString(device.DeviceID[:]),
			flags:    device.StateFlags,
		})
	}
	return physicalAdapterSummaries(candidates)
}

func physicalAdapterSummaries(candidates []displayAdapterCandidate) []AdapterSummary {
	result := make([]AdapterSummary, 0, len(candidates))
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		name := strings.TrimSpace(candidate.name)
		deviceID := strings.ToLower(strings.TrimSpace(candidate.deviceID))
		lowerName := strings.ToLower(name)
		if name == "" || candidate.flags&displayDeviceMirroringDriver != 0 || candidate.flags&displayDeviceRemote != 0 {
			continue
		}
		if deviceID != "" && !strings.Contains(deviceID, `pci\ven_`) {
			continue
		}
		if containsAny(lowerName, "virtual", "gameviewer", "remote", "indirect", "mirror", "parsec", "spacedesk", "basic render") {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		stableID := strings.TrimSpace(candidate.deviceID)
		if stableID == "" {
			stableID = "name:" + strings.ToLower(name)
		}
		result = append(result, AdapterSummary{
			ID:      stableID,
			Name:    name,
			Active:  candidate.flags&displayDeviceAttachedToDesktop != 0,
			Primary: candidate.flags&displayDevicePrimaryDevice != 0,
		})
	}
	return assignAdapterPreferences(result)
}

func adapterLabels(adapters []AdapterSummary) (string, string) {
	powerName, performanceName := "", ""
	for _, adapter := range adapters {
		if powerName == "" && adapter.Preference == PowerSaving {
			powerName = adapter.Name
		}
		if performanceName == "" && adapter.Preference == HighPerformance {
			performanceName = adapter.Name
		}
	}
	return powerName, performanceName
}

// assignAdapterPreferences 把实际枚举到的适配器映射到 Windows 注册表支持的
// 两个调度类别。UI 始终显示真实名称；类别只作为隐藏的持久化实现细节。
func assignAdapterPreferences(adapters []AdapterSummary) []AdapterSummary {
	if len(adapters) == 0 {
		return adapters
	}
	powerIndex, performanceIndex := -1, -1
	for index := range adapters {
		lower := strings.ToLower(adapters[index].Name)
		if powerIndex < 0 && adapterLooksPowerSaving(lower) {
			powerIndex = index
		}
		if performanceIndex < 0 && adapterLooksHighPerformance(lower) {
			performanceIndex = index
		}
	}
	if len(adapters) == 1 {
		if powerIndex == 0 && performanceIndex < 0 {
			adapters[0].Preference = PowerSaving
		} else {
			adapters[0].Preference = HighPerformance
		}
		return adapters
	}
	if powerIndex < 0 {
		powerIndex = 0
	}
	if performanceIndex < 0 || performanceIndex == powerIndex {
		performanceIndex = len(adapters) - 1
		if performanceIndex == powerIndex {
			performanceIndex = 0
		}
	}
	for index := range adapters {
		lower := strings.ToLower(adapters[index].Name)
		switch {
		case index == powerIndex:
			adapters[index].Preference = PowerSaving
		case index == performanceIndex || adapterLooksHighPerformance(lower):
			adapters[index].Preference = HighPerformance
		case adapterLooksPowerSaving(lower):
			adapters[index].Preference = PowerSaving
		default:
			adapters[index].Preference = HighPerformance
		}
	}
	return adapters
}

func adapterLooksPowerSaving(lowerName string) bool {
	return (strings.Contains(lowerName, "intel") && !strings.Contains(lowerName, " arc")) ||
		containsAny(lowerName, "integrated", " iris", " uhd", "apu", "radeon graphics")
}

func adapterLooksHighPerformance(lowerName string) bool {
	return containsAny(lowerName, "nvidia", "geforce", " rtx", " gtx", "quadro", "tesla", "radeon rx", "radeon pro", "intel arc")
}

func containsAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
