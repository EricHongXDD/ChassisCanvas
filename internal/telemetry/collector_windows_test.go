//go:build windows

package telemetry

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestChoosePreferredDisplayAdapter(t *testing.T) {
	models := []string{
		"Microsoft Basic Display Adapter",
		"Intel(R) UHD Graphics 770",
		" NVIDIA   GeForce RTX 4070 ",
		"NVIDIA GeForce RTX 4070",
	}
	if got := choosePreferredDisplayAdapter(models); got != "NVIDIA GeForce RTX 4070" {
		t.Fatalf("选择的显卡型号不正确: %q", got)
	}
}

func TestChoosePreferredDisplayAdapterFallsBackToIntegratedGPU(t *testing.T) {
	models := []string{"Remote Display Adapter", "Intel(R) Iris(R) Xe Graphics"}
	if got := choosePreferredDisplayAdapter(models); got != "Intel(R) Iris(R) Xe Graphics" {
		t.Fatalf("集成显卡回退不正确: %q", got)
	}
}

func TestAggregateGPUEngineSamples(t *testing.T) {
	samples := []gpuCounterSample{
		{name: "pid_100_luid_0x00000000_0x00001234_phys_0_eng_0_engtype_3D", value: 25},
		{name: "pid_200_luid_0x00000000_0x00001234_phys_0_eng_0_engtype_3D", value: 35},
		{name: "pid_100_luid_0x00000000_0x00001234_phys_0_eng_1_engtype_Copy", value: 12},
		{name: "无效实例", value: 99},
	}
	if got := aggregateGPUEngineSamples(samples); math.Abs(got-60) > 0.001 {
		t.Fatalf("GPU 引擎聚合结果不正确: %.3f", got)
	}
}

func TestAggregateGPUEngineSamplesClampsToHundred(t *testing.T) {
	samples := []gpuCounterSample{
		{name: "pid_1_luid_a_phys_0_eng_0_engtype_3D", value: 80},
		{name: "pid_2_luid_a_phys_0_eng_0_engtype_3D", value: 50},
	}
	if got := aggregateGPUEngineSamples(samples); got != 100 {
		t.Fatalf("GPU 百分比应限制到 100，实际为 %.3f", got)
	}
}

func TestParseNvidiaSMIOutput(t *testing.T) {
	info, err := parseNvidiaSMIOutput([]byte("NVIDIA GeForce RTX 4070, 54\r\nNVIDIA GeForce RTX 4060, 61\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if info.model != "NVIDIA GeForce RTX 4070" {
		t.Fatalf("显卡型号解析错误: %q", info.model)
	}
	if info.temperature == nil || *info.temperature != 61 {
		t.Fatalf("应返回多显卡中的最高温度，实际为 %v", info.temperature)
	}
}

func TestParseNvidiaSMIOutputRejectsUnavailableTemperature(t *testing.T) {
	if _, err := parseNvidiaSMIOutput([]byte("NVIDIA GeForce RTX 4070, N/A\n")); err == nil {
		t.Fatal("无效温度不应被接受")
	}
}

func TestNvidiaRefreshDoesNotBlockCapture(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	sampler := &nvidiaSMISampler{
		executable:      "fake-nvidia-smi.exe",
		refreshInterval: time.Hour,
		retryInterval:   time.Hour,
		timeout:         time.Second,
		info:            nvidiaInfo{model: "NVIDIA GeForce RTX 4070"},
		run: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			close(started)
			select {
			case <-release:
				return []byte("NVIDIA GeForce RTX 4070, 52\n"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}

	begin := time.Now()
	info := sampler.cachedAndRefresh()
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("读取缓存被外部查询阻塞: %v", elapsed)
	}
	if info.model != "NVIDIA GeForce RTX 4070" || info.temperature != nil {
		t.Fatalf("首次读取应立即返回已有缓存: %+v", info)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("后台温度查询没有启动")
	}
	close(release)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		sampler.mu.Lock()
		refreshed := !sampler.refreshing && sampler.info.temperature != nil
		sampler.mu.Unlock()
		if refreshed {
			return
		}
	}
	t.Fatal("后台温度查询结果未写入缓存")
}
