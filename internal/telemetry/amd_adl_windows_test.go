//go:build windows

package telemetry

import (
	"sync"
	"testing"
	"time"
)

type fakeAMDADLResponse struct {
	status int32
	output amdADLPMLogDataOutput
}

type fakeAMDADLQuerier struct {
	mu        sync.Mutex
	count     int32
	responses map[int32]fakeAMDADLResponse
	queries   int
	started   chan struct{}
	release   chan struct{}
}

func (fake *fakeAMDADLQuerier) adapterCount() int32 {
	return fake.count
}

func (fake *fakeAMDADLQuerier) queryPMLog(adapter int32, output *amdADLPMLogDataOutput) int32 {
	fake.mu.Lock()
	fake.queries++
	started := fake.started
	release := fake.release
	response, exists := fake.responses[adapter]
	fake.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if !exists {
		return -1
	}
	*output = response.output
	return response.status
}

func (fake *fakeAMDADLQuerier) queryCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.queries
}

func amdADLTestOutput(edge, hotspot, memory *int32) amdADLPMLogDataOutput {
	output := amdADLPMLogDataOutput{}
	set := func(sensor int, value *int32) {
		if value != nil {
			output.sensors[sensor] = amdADLSingleSensorData{supported: 1, value: *value}
		}
	}
	set(amdADLSensorTemperatureEdge, edge)
	set(amdADLSensorTemperatureHotspot, hotspot)
	set(amdADLSensorTemperatureMemory, memory)
	return output
}

func int32Pointer(value int32) *int32 {
	return &value
}

func TestLooksLikeAMD(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "AMD Radeon RX 7900 XTX", want: true},
		{model: "Radeon Graphics", want: true},
		{model: "AMD FirePro W7100", want: true},
		{model: "Advanced Micro Devices display adapter", want: true},
		{model: "NVIDIA GeForce RTX 4090", want: false},
		{model: "Intel Arc A770", want: false},
		{model: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			if got := looksLikeAMD(test.model); got != test.want {
				t.Fatalf("looksLikeAMD(%q) = %v, want %v", test.model, got, test.want)
			}
		})
	}
}

func TestNewAMDADLSamplerWithNativeGatesModelAndInitialization(t *testing.T) {
	valid := &fakeAMDADLQuerier{count: 1}
	if sampler := newAMDADLSamplerWithNative("Intel Arc A770", valid); sampler != nil {
		t.Fatal("非 AMD 型号不应创建 ADL 采样器")
	}
	if sampler := newAMDADLSamplerWithNative("AMD Radeon RX 7900 XTX", nil); sampler != nil {
		t.Fatal("初始化失败时不应创建 ADL 采样器")
	}
	if sampler := newAMDADLSamplerWithNative("AMD Radeon RX 7900 XTX", &fakeAMDADLQuerier{}); sampler != nil {
		t.Fatal("没有适配器时不应创建 ADL 采样器")
	}
	if sampler := newAMDADLSamplerWithNative("AMD Radeon RX 7900 XTX", valid); sampler == nil {
		t.Fatal("有效 AMD 型号与 ADL 查询器应创建采样器")
	}
}

func TestCollectAMDTemperaturesUsesHighestValidDuplicateAdapterValues(t *testing.T) {
	fake := &fakeAMDADLQuerier{
		count: 4,
		responses: map[int32]fakeAMDADLResponse{
			0: {status: 0, output: amdADLTestOutput(int32Pointer(44), int32Pointer(53), int32Pointer(56))},
			1: {status: 0, output: amdADLTestOutput(int32Pointer(45), int32Pointer(54), int32Pointer(55))},
			2: {status: 0, output: amdADLTestOutput(int32Pointer(230), int32Pointer(-80), int32Pointer(57))},
			3: {status: -8, output: amdADLTestOutput(int32Pointer(99), int32Pointer(99), int32Pointer(99))},
		},
	}

	info, err := collectAMDTemperatures(fake)
	if err != nil {
		t.Fatalf("collectAMDTemperatures() error = %v", err)
	}
	assertAMDTemperature(t, "temperature", info.temperature, 45)
	assertAMDTemperature(t, "edge", info.edgeTemperature, 45)
	assertAMDTemperature(t, "hotspot", info.hotspotTemperature, 54)
	assertAMDTemperature(t, "memory", info.memoryTemperature, 57)
}

func TestCollectAMDTemperaturesPrimaryFallback(t *testing.T) {
	tests := []struct {
		name    string
		output  amdADLPMLogDataOutput
		primary float64
	}{
		{name: "edge", output: amdADLTestOutput(int32Pointer(44), int32Pointer(54), int32Pointer(56)), primary: 44},
		{name: "hotspot", output: amdADLTestOutput(nil, int32Pointer(54), int32Pointer(56)), primary: 54},
		{name: "memory", output: amdADLTestOutput(nil, nil, int32Pointer(56)), primary: 56},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeAMDADLQuerier{
				count: 1,
				responses: map[int32]fakeAMDADLResponse{
					0: {status: 0, output: test.output},
				},
			}
			info, err := collectAMDTemperatures(fake)
			if err != nil {
				t.Fatalf("collectAMDTemperatures() error = %v", err)
			}
			assertAMDTemperature(t, "primary", info.temperature, test.primary)
		})
	}
}

func TestCollectAMDTemperaturesRejectsUnsupportedAndInvalidValues(t *testing.T) {
	output := amdADLTestOutput(int32Pointer(44), nil, nil)
	output.sensors[amdADLSensorTemperatureEdge].supported = 0
	fake := &fakeAMDADLQuerier{
		count: 1,
		responses: map[int32]fakeAMDADLResponse{
			0: {status: 0, output: output},
		},
	}
	if _, err := collectAMDTemperatures(fake); err == nil {
		t.Fatal("不支持或超出合理范围的温度应被忽略")
	}
}

func TestAMDADLSamplerCachedAndRefreshIsNonBlockingAndCoalescesConcurrentCalls(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake := &fakeAMDADLQuerier{
		count:   1,
		started: started,
		release: release,
		responses: map[int32]fakeAMDADLResponse{
			0: {status: 0, output: amdADLTestOutput(int32Pointer(44), int32Pointer(54), int32Pointer(56))},
		},
	}
	sampler := newAMDADLSamplerWithNative("AMD Radeon RX 7900 XTX", fake)

	begin := time.Now()
	for index := 0; index < 32; index++ {
		if info := sampler.cachedAndRefresh(); info.temperature != nil {
			t.Fatal("后台首次刷新完成前不应凭空产生温度")
		}
	}
	if elapsed := time.Since(begin); elapsed > 200*time.Millisecond {
		t.Fatalf("读缓存被驱动查询阻塞: %v", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("后台刷新未启动")
	}
	if got := fake.queryCount(); got != 1 {
		t.Fatalf("并发读取应合并为一次查询，实际 %d", got)
	}
	close(release)
	waitForAMDRefresh(t, sampler)
	info := sampler.cachedAndRefresh()
	assertAMDTemperature(t, "cached primary", info.temperature, 44)
	assertAMDTemperature(t, "cached hotspot", info.hotspotTemperature, 54)
}

func TestAMDADLSamplerFailureUsesRetryInterval(t *testing.T) {
	fake := &fakeAMDADLQuerier{
		count: 1,
		responses: map[int32]fakeAMDADLResponse{
			0: {status: -8},
		},
	}
	sampler := newAMDADLSamplerWithNative("AMD Radeon RX 7900 XTX", fake)
	clock := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	sampler.now = func() time.Time { return clock }
	sampler.refreshInterval = 2 * time.Second
	sampler.retryInterval = 10 * time.Second

	sampler.cachedAndRefresh()
	waitForAMDRefresh(t, sampler)
	if got := fake.queryCount(); got != 1 {
		t.Fatalf("首次查询次数 = %d, want 1", got)
	}
	clock = clock.Add(9 * time.Second)
	sampler.cachedAndRefresh()
	if got := fake.queryCount(); got != 1 {
		t.Fatalf("失败后重试间隔内不应再查询，实际 %d", got)
	}
	clock = clock.Add(time.Second)
	sampler.cachedAndRefresh()
	waitForAMDRefresh(t, sampler)
	if got := fake.queryCount(); got != 2 {
		t.Fatalf("重试间隔后查询次数 = %d, want 2", got)
	}
}

func waitForAMDRefresh(t *testing.T, sampler *amdADLSampler) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sampler.mu.Lock()
		refreshing := sampler.refreshing
		sampler.mu.Unlock()
		if !refreshing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("等待 AMD ADL 后台刷新超时")
}

func assertAMDTemperature(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s temperature = %v, want %.1f", name, got, want)
	}
}
