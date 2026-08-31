//go:build windows

package gpupreference

import (
	"os"
	"testing"
)

func TestPhysicalAdapterSummariesFilterVirtualDevices(t *testing.T) {
	adapters := physicalAdapterSummaries([]displayAdapterCandidate{
		{name: "Intel(R) Graphics", deviceID: `PCI\VEN_8086&DEV_7D67`, flags: displayDeviceAttachedToDesktop | displayDevicePrimaryDevice},
		{name: "AMD Radeon RX 7900 XTX", deviceID: `PCI\VEN_1002&DEV_744C`, flags: displayDeviceAttachedToDesktop},
		{name: "GameViewer Virtual Display", deviceID: `ROOT\DISPLAY\0000`, flags: displayDeviceAttachedToDesktop},
		{name: "Remote Display Adapter", deviceID: `PCI\VEN_FAKE`, flags: displayDeviceRemote},
	})
	if len(adapters) != 2 || adapters[0].Name != "Intel(R) Graphics" || adapters[1].Name != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("实体显卡过滤错误: %+v", adapters)
	}
	if adapters[0].ID != `PCI\VEN_8086&DEV_7D67` || adapters[0].Preference != PowerSaving || adapters[1].ID != `PCI\VEN_1002&DEV_744C` || adapters[1].Preference != HighPerformance {
		t.Fatalf("显卡 ID 或 Windows 类别映射错误: %+v", adapters)
	}
	power, performance := adapterLabels(adapters)
	if power != "Intel(R) Graphics" || performance != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("显卡标签错误: %q / %q", power, performance)
	}
}

func TestGPUPreferenceStatusLive(t *testing.T) {
	if os.Getenv("CASESCREEN_GPU_PREFERENCE_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_GPU_PREFERENCE_LIVE=1 后执行只读显卡枚举")
	}
	status, err := NewManager().Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Supported || len(status.Applications) != 2 {
		t.Fatalf("Windows GPU 状态无效: %+v", status)
	}
	if len(status.Adapters) == 0 {
		t.Fatal("没有枚举到实体显示适配器")
	}
	t.Logf("实体显卡=%+v，标签=%q / %q", status.Adapters, status.PowerSavingLabel, status.HighPerformanceLabel)
}
