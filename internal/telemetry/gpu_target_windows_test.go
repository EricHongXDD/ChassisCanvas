//go:build windows

package telemetry

import (
	"math"
	"os"
	"testing"
	"time"
)

func TestChoosePreferredGPUTelemetryTargetPrefersDiscreteGPU(t *testing.T) {
	target := choosePreferredGPUTelemetryTarget([]gpuTelemetryTarget{
		{
			model:                "Intel(R) Graphics",
			deviceID:             `PCI\VEN_8086&DEV_7D67`,
			luid:                 0x166b5,
			dedicatedVideoMemory: 128 * 1024 * 1024,
			active:               true,
			primary:              true,
		},
		{
			model:                "AMD Radeon RX 7900 XTX",
			deviceID:             `PCI\VEN_1002&DEV_744C`,
			luid:                 0x1334f,
			dedicatedVideoMemory: 24 * 1024 * 1024 * 1024,
		},
	})
	if target.model != "AMD Radeon RX 7900 XTX" || target.luid != 0x1334f {
		t.Fatalf("没有选择独立显卡: %+v", target)
	}
}

func TestChoosePreferredGPUTelemetryTargetFallsBackToIntegratedGPU(t *testing.T) {
	target := choosePreferredGPUTelemetryTarget([]gpuTelemetryTarget{
		{model: "Intel(R) Graphics", deviceID: `PCI\VEN_8086&DEV_7D67`, luid: 0x166b5},
	})
	if target.model != "Intel(R) Graphics" || target.luid != 0x166b5 {
		t.Fatalf("单显卡回退错误: %+v", target)
	}
}

func TestPublicGPUAdaptersKeepsPhysicalIdentity(t *testing.T) {
	adapters := publicGPUAdapters([]gpuTelemetryTarget{
		{model: "AMD Radeon RX 7900 XTX", deviceID: `PCI\VEN_1002&DEV_744C`, luid: 1},
		{model: "Intel(R) Graphics", deviceID: `PCI\VEN_8086&DEV_7D67`, luid: 2, primary: true},
	})
	if len(adapters) != 2 || adapters[0].ID == adapters[1].ID || adapters[1].Name != "Intel(R) Graphics" {
		t.Fatalf("物理显卡公开列表错误: %+v", adapters)
	}
}

func TestDirectXAdapterSubKey(t *testing.T) {
	deviceKey := `\Registry\Machine\System\CurrentControlSet\Control\Video\{6A3CE5BD-A0B9-11F1-974D-AE3504F8D095}\0000`
	if got := directXAdapterSubKey(deviceKey); got != `{6A3CE5BD-A0B9-11F1-974D-AE3504F8D095}` {
		t.Fatalf("DirectX 适配器子键解析错误: %q", got)
	}
	if got := directXAdapterSubKey(`无 GUID`); got != "" {
		t.Fatalf("无效 DeviceKey 不应返回子键: %q", got)
	}
}

func TestGPUEngineLUIDAcceptsCaseAndLeadingZeroVariants(t *testing.T) {
	for _, name := range []string{
		"pid_1_luid_0x00000000_0x0001334f_phys_0_eng_0_engtype_3D",
		"PID_2_LUID_0X0_0X1334F_PHYS_0_ENG_1_ENGTYPE_COPY",
	} {
		luid, ok := gpuEngineLUID(name)
		if !ok || luid != 0x1334f {
			t.Fatalf("LUID 解析错误: %q => %#x, %v", name, luid, ok)
		}
	}
}

func TestGPUEngineLUIDRejectsMalformedInstance(t *testing.T) {
	for _, name := range []string{
		"pid_1_phys_0_eng_0",
		"pid_1_luid_0x0_phys_0_eng_0",
		"pid_1_luid_0xGG_0x1_phys_0_eng_0",
		"pid_1_luid_0x0_0x1_notphys_0_eng_0",
	} {
		if luid, ok := gpuEngineLUID(name); ok {
			t.Fatalf("无效实例被接受: %q => %#x", name, luid)
		}
	}
}

func TestAggregateGPUEngineSamplesForLUIDIgnoresBusyIntegratedGPU(t *testing.T) {
	samples := []gpuCounterSample{
		{name: "pid_1_luid_0x0_0x1334f_phys_0_eng_0_engtype_3D", value: 7},
		{name: "pid_2_luid_0x00000000_0x0001334F_phys_0_eng_0_engtype_3D", value: 5},
		{name: "pid_3_luid_0x0_0x1334f_phys_0_eng_1_engtype_Copy", value: 4},
		{name: "pid_4_luid_0x0_0x166b5_phys_0_eng_0_engtype_3D", value: 80},
	}
	if got := aggregateGPUEngineSamplesForLUID(samples, 0x1334f); math.Abs(got-12) > 0.001 {
		t.Fatalf("独显利用率聚合错误: %.3f", got)
	}
}

func TestAggregateGPUEngineSamplesForLUIDReturnsZeroWhenTargetIdle(t *testing.T) {
	samples := []gpuCounterSample{
		{name: "pid_1_luid_0x0_0x1334f_phys_0_eng_0_engtype_3D", value: 0},
		{name: "pid_2_luid_0x0_0x166b5_phys_0_eng_0_engtype_3D", value: 75},
	}
	if got := aggregateGPUEngineSamplesForLUID(samples, 0x1334f); got != 0 {
		t.Fatalf("空闲独显应返回 0，实际为 %.3f", got)
	}
}

func TestGPUTelemetryTargetLive(t *testing.T) {
	if os.Getenv("CASESCREEN_GPU_TARGET_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_GPU_TARGET_LIVE=1 后执行只读独显目标枚举")
	}
	target := readPreferredGPUTelemetryTarget()
	if target.model == "" || target.luid == 0 {
		t.Fatalf("没有解析到遥测 GPU 目标: %+v", target)
	}
	t.Logf("遥测 GPU=%s，DeviceID=%s，LUID=%#x，专用显存=%d", target.model, target.deviceID, target.luid, target.dedicatedVideoMemory)
}

func TestGPUTelemetrySamplerLive(t *testing.T) {
	if os.Getenv("CASESCREEN_GPU_TARGET_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_GPU_TARGET_LIVE=1 后执行只读独显利用率采集")
	}
	target := readPreferredGPUTelemetryTarget()
	sampler := newWindowsGPUSampler(target)
	if _, ok := sampler.capture(); ok {
		t.Fatal("速率计数器首帧只应建立基线")
	}
	time.Sleep(1100 * time.Millisecond)
	load, ok := sampler.capture()
	if !ok || load < 0 || load > 100 {
		t.Fatalf("独显利用率无效: %.3f, %v", load, ok)
	}
	t.Logf("%s（LUID=%#x）利用率=%.3f%%", target.model, target.luid, load)
}

func TestCollectorGPUTargetSwitchLive(t *testing.T) {
	if os.Getenv("CASESCREEN_GPU_TARGET_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_GPU_TARGET_LIVE=1 后执行只读 GPU 热切换")
	}
	targets := readGPUTelemetryTargets()
	if len(targets) < 2 {
		t.Skip("当前机器少于两块物理 GPU")
	}
	collector := NewCollector()
	for _, target := range targets {
		status, err := collector.SetGPUTarget(target.deviceID)
		if err != nil {
			t.Fatalf("切换到 %s: %v", target.model, err)
		}
		if status.Automatic || status.EffectiveID != target.deviceID || status.EffectiveName != target.model {
			t.Fatalf("切换状态错误: %+v", status)
		}
		snapshot, err := collector.Capture()
		if err != nil {
			t.Fatalf("采集 %s: %v", target.model, err)
		}
		if snapshot.GPUModel != target.model {
			t.Fatalf("型号没有随目标切换: got %q want %q", snapshot.GPUModel, target.model)
		}
		if snapshot.GPUPercent != nil && (*snapshot.GPUPercent < 0 || *snapshot.GPUPercent > 100) {
			t.Fatalf("%s 利用率无效: %.3f", target.model, *snapshot.GPUPercent)
		}
		t.Logf("目标=%s，DeviceID=%s，利用率=%v", target.model, target.deviceID, snapshot.GPUPercent)
	}
}
