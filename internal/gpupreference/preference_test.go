package gpupreference

import (
	"path/filepath"
	"testing"
)

func TestParsePreference(t *testing.T) {
	for _, value := range []Preference{SystemDefault, PowerSaving, HighPerformance} {
		if parsed, err := Parse(string(value)); err != nil || parsed != value {
			t.Fatalf("解析 %q = %q, %v", value, parsed, err)
		}
	}
	for _, value := range []string{"", "automatic", "integrated", "unknown"} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("应拒绝 GPU 首选项 %q", value)
		}
	}
}

func TestManagerTargetsOnlyTwoFixedExecutables(t *testing.T) {
	manager := NewManagerForExecutable(filepath.Join(t.TempDir(), "renamed-service.exe"))
	if len(manager.targets) != 2 {
		t.Fatalf("目标数量 = %d", len(manager.targets))
	}
	if manager.targets[0].Name != "CaseScreen.exe" || manager.targets[1].Name != "CaseScreenDesktop.exe" {
		t.Fatalf("目标程序错误: %+v", manager.targets)
	}
	if filepath.Dir(manager.targets[0].Path) != filepath.Dir(manager.targets[1].Path) {
		t.Fatalf("两个程序不在同一目录: %+v", manager.targets)
	}
}

func TestStatusReportsMixedApplications(t *testing.T) {
	status := statusFromApplications(true, []ApplicationStatus{
		{Name: "CaseScreen.exe", Preference: PowerSaving},
		{Name: "CaseScreenDesktop.exe", Preference: HighPerformance},
	})
	if status.Effective != Mixed || status.Consistent || len(status.RestartProcesses) != 2 || status.Notice == "" {
		t.Fatalf("混合状态错误: %+v", status)
	}
}

func TestFindAdapterMatchesStableIDCaseInsensitively(t *testing.T) {
	adapters := []AdapterSummary{
		{ID: `PCI\VEN_8086&DEV_7D67`, Name: "Intel(R) Graphics", Preference: PowerSaving},
		{ID: `PCI\VEN_1002&DEV_744C`, Name: "AMD Radeon RX 7900 XTX", Preference: HighPerformance},
	}
	adapter, ok := FindAdapter(adapters, ` pci\ven_1002&dev_744c `)
	if !ok || adapter.Name != "AMD Radeon RX 7900 XTX" || adapter.Preference != HighPerformance {
		t.Fatalf("稳定 ID 匹配失败: %+v, %v", adapter, ok)
	}
	if _, ok := FindAdapter(adapters, `PCI\VEN_FAKE`); ok {
		t.Fatal("不应接受未枚举的显卡 ID")
	}
}
