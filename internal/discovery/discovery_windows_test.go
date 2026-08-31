//go:build windows

package discovery

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFilterPresentPortsExcludesStaleRegistryInstances(t *testing.T) {
	registered := []string{"COM4", "com3", " COM3 ", "COM8"}
	present := map[string]bool{"COM3": true, "COM8": true}
	if got, want := filterPresentPorts(registered, present), []string{"COM3", "COM8"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("应排除 Windows Enum\\USB 中残留的非在线 COM 实例: got=%v want=%v", got, want)
	}
}

func TestFilterPresentPortsFailsClosedWithoutCurrentPortMap(t *testing.T) {
	if got := filterPresentPorts([]string{"COM3", "COM4"}, nil); len(got) != 0 {
		t.Fatalf("无法确认当前串口时不应把历史实例当成在线设备: %v", got)
	}
}

func TestWaveshareStableIDUsesVerifiedUSBSerial(t *testing.T) {
	got := waveshareStableID("SYK", usbDeviceCapabilityUniqueID)
	if got != "usb-serial:1d6b:0104:syk" {
		t.Fatalf("USB 序列号应是最高优先级的稳定身份: %q", got)
	}
	if id := waveshareDiscoveredID(got, "COM4"); id == "waveshare-com4" || id != waveshareDiscoveredID(got, "COM9") {
		t.Fatalf("新设备 ID 应来自物理身份而不是 COM 号: %q", id)
	}
}

func TestWaveshareStableIDRejectsPortDependentIdentityWithoutSerial(t *testing.T) {
	if got := waveshareStableID("6&37524538&0&3", 0); got != "" {
		t.Fatalf("无序列号的拓扑父实例不应持久成稳定身份: %q", got)
	}
	if got := waveshareStableID("PLAIN-BUT-NOT-UNIQUE", 0); got != "" {
		t.Fatalf("父实例没有 UniqueID 能力时，不得只因尾段形式持久化: %q", got)
	}
	if got := waveshareStableID("", usbDeviceCapabilityUniqueID); got != "" {
		t.Fatalf("系统未提供可用指纹时应留空，交给握手分辨率回退: %q", got)
	}
}

func TestDiscoveryDeviceRankPutsSupportedScreensInBackendOrder(t *testing.T) {
	wave7 := Device{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Width: 1024, Height: 600}
	wave88 := Device{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Width: 480, Height: 1920}
	cooler := Device{ID: "coolermaster-atmos-v2", Kind: "coolermaster", Width: 480, Height: 480}
	if !(discoveryDeviceRank(wave7) < discoveryDeviceRank(wave88) && discoveryDeviceRank(wave88) < discoveryDeviceRank(cooler)) {
		t.Fatalf("设备发现排序应为微雪 7、微雪 8.8、酷冷: %d %d %d",
			discoveryDeviceRank(wave7), discoveryDeviceRank(wave88), discoveryDeviceRank(cooler))
	}
}

func TestHardwareDiscoverySmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_DISCOVERY_SMOKE") != "1" {
		t.Skip("设置 CASESCREEN_DISCOVERY_SMOKE=1 后执行真机设备发现测试")
	}
	devices := Find()
	if len(devices) < 3 {
		t.Fatalf("没有发现三块受支持的已连接副屏: %+v", devices)
	}
	found := make(map[string]int)
	waveshareSerials := 0
	for _, device := range devices {
		found[device.Kind]++
		if device.Kind == "waveshare" {
			if strings.HasPrefix(device.StableID, "usb-container:") {
				t.Fatalf("无序列号屏的 Container ID 不得暴露为强身份: %+v", device)
			}
			if strings.HasPrefix(device.StableID, "usb-serial:") {
				waveshareSerials++
			}
		}
		if device.Kind == "thermalright" {
			t.Fatalf("设备发现不应再返回已移除的利民设备: %+v", devices)
		}
	}
	if found["waveshare"] < 2 || found["coolermaster"] < 1 {
		t.Fatalf("没有发现两块微雪和一块酷冷至尊: %+v", devices)
	}
	if waveshareSerials != 1 {
		t.Fatalf("当前两块微雪应只有 7 英寸提供可验证 USB 序列号: %+v", devices)
	}
}
