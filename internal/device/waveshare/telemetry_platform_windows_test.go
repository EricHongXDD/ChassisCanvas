//go:build windows

package waveshare

import (
	"os"
	"testing"
	"time"
)

func TestAggregateGPUEngineSamplesSumsProcessesPerEngine(t *testing.T) {
	samples := []gpuCounterSample{
		{name: "pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D", value: 22},
		{name: "pid_2_luid_0x0_0x1_phys_0_eng_0_engtype_3D", value: 31},
		{name: "pid_1_luid_0x0_0x1_phys_0_eng_1_engtype_Copy", value: 19},
		{name: "_Total", value: 99},
	}
	if value := aggregateGPUEngineSamples(samples); value != 53 {
		t.Fatalf("GPU 引擎聚合错误: %v", value)
	}
}

func TestAggregateGPUEngineSamplesClampsAtHundred(t *testing.T) {
	samples := []gpuCounterSample{
		{name: "pid_1_luid_a_phys_0_eng_0_engtype_3D", value: 80},
		{name: "pid_2_luid_a_phys_0_eng_0_engtype_3D", value: 50},
	}
	if value := aggregateGPUEngineSamples(samples); value != 100 {
		t.Fatalf("GPU 百分比没有限制在 100: %v", value)
	}
}

func TestWaveshareGPUSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_GPU_SMOKE") != "1" {
		t.Skip("设置 CASESCREEN_GPU_SMOKE=1 后执行真机 GPU 计数器采集")
	}
	sampler := &waveshareGPUSampler{}
	_, model, _ := sampler.capture()
	time.Sleep(1100 * time.Millisecond)
	load, model, ok := sampler.capture()
	if model == "" {
		t.Fatal("没有读取到物理显示适配器名称")
	}
	if !ok {
		t.Fatal("没有读取到 GPU Engine 性能计数器")
	}
	t.Logf("GPU %s，利用率 %.1f%%", model, load)
}
