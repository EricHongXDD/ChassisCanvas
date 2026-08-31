//go:build windows

package telemetry

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	cpuRegistryPath   = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	videoRegistryPath = `SYSTEM\CurrentControlSet\Control\Video`
	displayMirroring  = 0x00000008
)

var (
	user32DLL               = windows.NewLazySystemDLL("user32.dll")
	procEnumDisplayDevicesW = user32DLL.NewProc("EnumDisplayDevicesW")
)

type displayDevice struct {
	Size         uint32
	DeviceName   [32]uint16
	DeviceString [128]uint16
	StateFlags   uint32
	DeviceID     [128]uint16
	DeviceKey    [128]uint16
}

// readCPUModel 从 Windows 硬件注册表读取静态型号；该函数只在创建采集器时执行一次。
func readCPUModel() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, cpuRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		return normalizeHardwareName(os.Getenv("PROCESSOR_IDENTIFIER"))
	}
	defer key.Close()
	model, _, err := key.GetStringValue("ProcessorNameString")
	if err != nil {
		return normalizeHardwareName(os.Getenv("PROCESSOR_IDENTIFIER"))
	}
	return normalizeHardwareName(model)
}

// readPreferredDisplayAdapterModel 收集显示适配器并优先选择独立显卡。
// Control\Video 比每秒启动 WMI 查询更轻量，且型号属于无需反复刷新的静态信息。
func readPreferredDisplayAdapterModel() string {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, videoRegistryPath, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return choosePreferredDisplayAdapter(enumerateDisplayDeviceModels())
	}
	defer root.Close()

	identifiers, _ := root.ReadSubKeyNames(-1)
	models := make([]string, 0, len(identifiers))
	for _, identifier := range identifiers {
		adapterRoot, openErr := registry.OpenKey(root, identifier, registry.ENUMERATE_SUB_KEYS)
		if openErr != nil {
			continue
		}
		entries, _ := adapterRoot.ReadSubKeyNames(-1)
		for _, entry := range entries {
			adapter, entryErr := registry.OpenKey(adapterRoot, entry, registry.QUERY_VALUE)
			if entryErr != nil {
				continue
			}
			name, _, _ := adapter.GetStringValue("DriverDesc")
			if name == "" {
				name, _, _ = adapter.GetStringValue("HardwareInformation.AdapterString")
			}
			adapter.Close()
			if name = normalizeHardwareName(name); name != "" {
				models = append(models, name)
			}
		}
		adapterRoot.Close()
	}
	// EnumDisplayDevices 是注册表布局异常或权限受限时的 Win32 回退来源。
	models = append(models, enumerateDisplayDeviceModels()...)
	return choosePreferredDisplayAdapter(models)
}

func enumerateDisplayDeviceModels() []string {
	models := make([]string, 0, 4)
	for index := uint32(0); index < 32; index++ {
		device := displayDevice{Size: uint32(unsafe.Sizeof(displayDevice{}))}
		ok, _, _ := procEnumDisplayDevicesW.Call(
			0,
			uintptr(index),
			uintptr(unsafe.Pointer(&device)),
			0,
		)
		if ok == 0 {
			break
		}
		if device.StateFlags&displayMirroring != 0 {
			continue
		}
		if name := normalizeHardwareName(windows.UTF16ToString(device.DeviceString[:])); name != "" {
			models = append(models, name)
		}
	}
	return models
}

func choosePreferredDisplayAdapter(models []string) string {
	bestName := ""
	bestScore := -1
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = normalizeHardwareName(model)
		key := strings.ToLower(model)
		if model == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		score := displayAdapterScore(key)
		if score > bestScore {
			bestScore = score
			bestName = model
		}
	}
	return bestName
}

func displayAdapterScore(lowerName string) int {
	for _, marker := range []string{
		"microsoft basic", "remote display", "virtual display", "virtual adapter",
		"indirect display", "parsec", "vnc", "mirror driver",
	} {
		if strings.Contains(lowerName, marker) {
			return -1
		}
	}
	switch {
	case strings.Contains(lowerName, "nvidia") || strings.Contains(lowerName, "geforce") || strings.Contains(lowerName, "quadro"):
		return 100
	case strings.Contains(lowerName, "amd") || strings.Contains(lowerName, "radeon"):
		return 90
	case strings.Contains(lowerName, "intel") && strings.Contains(lowerName, "arc"):
		return 85
	case strings.Contains(lowerName, "intel"):
		return 70
	default:
		return 10
	}
}

func normalizeHardwareName(value string) string {
	return strings.Join(strings.Fields(strings.Trim(value, "\x00\ufeff \t\r\n")), " ")
}
