//go:build windows

package gpupreference

import "testing"

func TestPreferenceFromRaw(t *testing.T) {
	tests := []struct {
		raw    string
		exists bool
		want   Preference
	}{
		{want: SystemDefault},
		{raw: "GpuPreference=0;", exists: true, want: SystemDefault},
		{raw: "GpuPreference=1;", exists: true, want: PowerSaving},
		{raw: "SwapEffectUpgradeEnable=1;GpuPreference=2;", exists: true, want: HighPerformance},
		{raw: "GpuPreference=9;", exists: true, want: Unknown},
		{raw: "GpuPreference=1;GpuPreference=2;", exists: true, want: Unknown},
	}
	for _, test := range tests {
		if got := preferenceFromRaw(test.raw, test.exists); got != test.want {
			t.Fatalf("preferenceFromRaw(%q, %v) = %q, want %q", test.raw, test.exists, got, test.want)
		}
	}
}

func TestRegistryValuePreservesUnrelatedDirectXOptions(t *testing.T) {
	existing := registryValue{exists: true, raw: "SwapEffectUpgradeEnable=1;GpuPreference=2;VRROptimizeEnable=1;"}
	got := registryValueForPreference(existing, PowerSaving)
	want := registryValue{exists: true, raw: "SwapEffectUpgradeEnable=1;VRROptimizeEnable=1;GpuPreference=1;"}
	if got != want {
		t.Fatalf("节能值 = %+v, want %+v", got, want)
	}
	got = registryValueForPreference(existing, SystemDefault)
	want = registryValue{exists: true, raw: "SwapEffectUpgradeEnable=1;VRROptimizeEnable=1;"}
	if got != want {
		t.Fatalf("恢复系统默认值 = %+v, want %+v", got, want)
	}
	if got := registryValueForPreference(registryValue{exists: true, raw: "GpuPreference=2;"}, SystemDefault); got.exists {
		t.Fatalf("只有 GPU 字段时应删除注册表值: %+v", got)
	}
}
