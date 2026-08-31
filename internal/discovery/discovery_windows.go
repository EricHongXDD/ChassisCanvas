//go:build windows

package discovery

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const usbEnumRoot = `SYSTEM\CurrentControlSet\Enum\USB`

const serialCommRoot = `HARDWARE\DEVICEMAP\SERIALCOMM`

const usbDeviceCapabilityUniqueID = 0x10

type waveshareUSBParent struct {
	instance     string
	capabilities uint64
}

func findPlatformDevices() []Device {
	devices := make([]Device, 0, 2)
	devices = append(devices, waveshareDevices()...)
	if usbDevicePresent(`VID_2516&PID_0222`) {
		slot := 1
		devices = append(devices, Device{
			ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster", Enabled: true,
			Width: 480, Height: 480, Slot: &slot,
		})
	}
	sort.SliceStable(devices, func(left, right int) bool {
		leftRank := discoveryDeviceRank(devices[left])
		rightRank := discoveryDeviceRank(devices[right])
		if leftRank == rightRank {
			return devices[left].ID < devices[right].ID
		}
		return leftRank < rightRank
	})
	return devices
}

func discoveryDeviceRank(device Device) int {
	if device.Kind == "waveshare" {
		identity := strings.ToLower(device.ID + " " + device.Name)
		if strings.Contains(identity, "8.8") || (device.Height > device.Width && device.Height >= 1000) {
			return 1
		}
		return 0
	}
	if device.Kind == "coolermaster" {
		return 2
	}
	return 3
}

func wavesharePorts() []string {
	devices := waveshareDevices()
	ports := make([]string, 0, len(devices))
	for _, candidate := range devices {
		ports = append(ports, candidate.Port)
	}
	return ports
}

// waveshareDevices 把 Windows 枚举的串口端点还原为 USB 物理设备。
// 该系列的一部分固件会报告序列号（当前 7 英寸为 SYK）。
// 无序列号型号的 Container/Location/Instance 都会随 USB 插口变化，
// 因此绝不持久化为强身份，交给屏端握手分辨率唯一匹配。
func waveshareDevices() []Device {
	path := usbEnumRoot + `\VID_1D6B&PID_0104&MI_00`
	instances := registrySubkeys(path)
	present := presentSerialPorts()
	parentsByContainer := waveshareParentsByContainer()
	devices := make([]Device, 0, len(instances))
	seenPorts := make(map[string]bool, len(instances))
	for _, instance := range instances {
		instancePath := path + `\` + instance
		parameters := path + `\` + instance + `\Device Parameters`
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, parameters, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		port, _, valueErr := key.GetStringValue("PortName")
		_ = key.Close()
		port = strings.ToUpper(strings.TrimSpace(port))
		if valueErr != nil || !strings.HasPrefix(port, "COM") || !present[port] || seenPorts[port] {
			continue
		}
		seenPorts[port] = true
		containerID := registryStringValue(instancePath, "ContainerID")
		parent := parentsByContainer[normalizeContainerID(containerID)]
		stableID := waveshareStableID(parent.instance, parent.capabilities)
		devices = append(devices, Device{
			ID: waveshareDiscoveredID(stableID, port), Name: "微雪 USB Monitor（" + port + "）",
			Kind: "waveshare", Enabled: true, Port: port, StableID: stableID,
		})
	}
	sort.SliceStable(devices, func(left, right int) bool { return devices[left].Port < devices[right].Port })
	return devices
}

func waveshareParentsByContainer() map[string]waveshareUSBParent {
	path := usbEnumRoot + `\VID_1D6B&PID_0104`
	parents := make(map[string]waveshareUSBParent)
	for _, instance := range registrySubkeys(path) {
		instancePath := path + `\` + instance
		containerID := normalizeContainerID(registryStringValue(instancePath, "ContainerID"))
		if containerID != "" {
			parents[containerID] = waveshareUSBParent{
				instance:     strings.TrimSpace(instance),
				capabilities: registryIntegerValue(instancePath, "Capabilities"),
			}
		}
	}
	return parents
}

func registryIntegerValue(path, name string) uint64 {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return 0
	}
	defer key.Close()
	value, _, err := key.GetIntegerValue(name)
	if err != nil {
		return 0
	}
	return value
}

func registryStringValue(path, name string) string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, err := key.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func normalizeContainerID(value string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(value)), "{}")
}

func waveshareStableID(parentInstance string, capabilities uint64) string {
	parentInstance = strings.TrimSpace(parentInstance)
	// USB 规范中，无序列号设备的 Windows 父实例是 6&...&0&...
	// 这种值依赖连接拓扑，不能冒充设备序列号。
	if capabilities&usbDeviceCapabilityUniqueID != 0 && parentInstance != "" && !strings.Contains(parentInstance, "&") {
		return "usb-serial:1d6b:0104:" + strings.ToLower(parentInstance)
	}
	return ""
}

func waveshareDiscoveredID(stableID, port string) string {
	if stableID == "" {
		return "waveshare-" + strings.ToLower(strings.TrimSpace(port))
	}
	digest := sha256.Sum256([]byte(stableID))
	return fmt.Sprintf("waveshare-usb-%x", digest[:6])
}

// presentSerialPorts 只读取 Windows 当前挂载的串口映射。Enum\USB 会长期保留旧
// COM 号的非在线实例，直接枚举它们会产生不可连接、也没有主题绑定的假设备。
func presentSerialPorts() map[string]bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, serialCommRoot, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer key.Close()
	valueNames, err := key.ReadValueNames(-1)
	if err != nil {
		return nil
	}
	ports := make(map[string]bool, len(valueNames))
	for _, valueName := range valueNames {
		port, _, valueErr := key.GetStringValue(valueName)
		port = strings.ToUpper(strings.TrimSpace(port))
		if valueErr == nil && strings.HasPrefix(port, "COM") {
			ports[port] = true
		}
	}
	return ports
}

func filterPresentPorts(registered []string, present map[string]bool) []string {
	ports := make([]string, 0, len(registered))
	seen := make(map[string]bool, len(registered))
	for _, candidate := range registered {
		port := strings.ToUpper(strings.TrimSpace(candidate))
		if port == "" || !present[port] || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	sort.Strings(ports)
	return ports
}

func usbDevicePresent(identifier string) bool {
	return len(registrySubkeys(fmt.Sprintf(`%s\%s`, usbEnumRoot, identifier))) > 0
}

func registrySubkeys(path string) []string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer key.Close()
	names, err := key.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	return names
}
