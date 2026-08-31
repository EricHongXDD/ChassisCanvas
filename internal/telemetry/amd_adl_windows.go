//go:build windows

package telemetry

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	amdADLRefreshInterval = 2 * time.Second
	amdADLRetryInterval   = 10 * time.Second

	amdADLStatusOK        = 0
	amdADLPMLogMaxSensors = 256

	amdADLSensorTemperatureEdge    = 8
	amdADLSensorTemperatureMemory  = 9
	amdADLSensorTemperatureHotspot = 27
)

type amdADLSingleSensorData struct {
	supported int32
	value     int32
}

type amdADLPMLogDataOutput struct {
	size    int32
	sensors [amdADLPMLogMaxSensors]amdADLSingleSensorData
}

// amdTemperatureInfo 保留驱动实际提供的三类温度。temperature 是通用显卡温度，
// 优先使用 edge，驱动不提供时才依次回退到 hotspot 和 memory。
type amdTemperatureInfo struct {
	temperature        *float64
	edgeTemperature    *float64
	hotspotTemperature *float64
	memoryTemperature  *float64
}

type amdADLQuerier interface {
	adapterCount() int32
	queryPMLog(adapter int32, output *amdADLPMLogDataOutput) int32
}

// windowsAMDADLNative 持有 ADL2 上下文，整个服务进程内复用，避免反复初始化驱动。
type windowsAMDADLNative struct {
	dll          *syscall.LazyDLL
	context      uintptr
	adapterTotal int32
	query        *syscall.LazyProc
}

// amdADLSampler 只在后台低频查询 ADL；Capture 路径只取缓存，不会被驱动调用阻塞。
type amdADLSampler struct {
	mu              sync.Mutex
	native          amdADLQuerier
	refreshInterval time.Duration
	retryInterval   time.Duration
	now             func() time.Time
	lastAttempt     time.Time
	lastFailed      bool
	refreshing      bool
	info            amdTemperatureInfo
}

func newAMDADLSampler(displayModel string) *amdADLSampler {
	if !looksLikeAMD(displayModel) {
		return nil
	}
	native, err := newWindowsAMDADLNative()
	if err != nil {
		return nil
	}
	return newAMDADLSamplerWithNative(displayModel, native)
}

func newAMDADLSamplerWithNative(displayModel string, native amdADLQuerier) *amdADLSampler {
	if !looksLikeAMD(displayModel) || native == nil || native.adapterCount() <= 0 {
		return nil
	}
	return &amdADLSampler{
		native:          native,
		refreshInterval: amdADLRefreshInterval,
		retryInterval:   amdADLRetryInterval,
		now:             time.Now,
	}
}

func looksLikeAMD(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(lower, "amd") ||
		strings.Contains(lower, "radeon") ||
		strings.Contains(lower, "firepro") ||
		strings.Contains(lower, "advanced micro devices")
}

func newWindowsAMDADLNative() (*windowsAMDADLNative, error) {
	adl := syscall.NewLazyDLL("atiadlxx.dll")
	create := adl.NewProc("ADL2_Main_Control_Create")
	destroy := adl.NewProc("ADL2_Main_Control_Destroy")
	countAdapters := adl.NewProc("ADL2_Adapter_NumberOfAdapters_Get")
	queryPMLog := adl.NewProc("ADL2_New_QueryPMLogData_Get")
	malloc := syscall.NewLazyDLL("ucrtbase.dll").NewProc("malloc")

	for name, proc := range map[string]*syscall.LazyProc{
		"ADL2_Main_Control_Create":          create,
		"ADL2_Main_Control_Destroy":         destroy,
		"ADL2_Adapter_NumberOfAdapters_Get": countAdapters,
		"ADL2_New_QueryPMLogData_Get":       queryPMLog,
		"malloc":                            malloc,
	} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("无法加载 %s: %w", name, err)
		}
	}

	var context uintptr
	result, _, _ := create.Call(malloc.Addr(), 1, uintptr(unsafe.Pointer(&context)))
	if amdADLStatus(result) != amdADLStatusOK || context == 0 {
		return nil, errors.New("AMD ADL2 上下文初始化失败")
	}

	var adapterTotal int32
	result, _, _ = countAdapters.Call(context, uintptr(unsafe.Pointer(&adapterTotal)))
	if amdADLStatus(result) != amdADLStatusOK || adapterTotal <= 0 {
		// 初始化未完成时立即释放；正常运行的上下文由进程退出时统一回收。
		destroy.Call(context)
		return nil, errors.New("AMD ADL2 未返回有效显卡适配器")
	}

	return &windowsAMDADLNative{
		dll:          adl,
		context:      context,
		adapterTotal: adapterTotal,
		query:        queryPMLog,
	}, nil
}

func amdADLStatus(value uintptr) int32 {
	return int32(value)
}

func (native *windowsAMDADLNative) adapterCount() int32 {
	return native.adapterTotal
}

func (native *windowsAMDADLNative) queryPMLog(adapter int32, output *amdADLPMLogDataOutput) int32 {
	result, _, _ := native.query.Call(
		native.context,
		uintptr(adapter),
		uintptr(unsafe.Pointer(output)),
	)
	return amdADLStatus(result)
}

func (sampler *amdADLSampler) cachedAndRefresh() amdTemperatureInfo {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()

	now := time.Now()
	if sampler.now != nil {
		now = sampler.now()
	}
	interval := sampler.refreshInterval
	if sampler.lastFailed {
		interval = sampler.retryInterval
	}
	if !sampler.refreshing && (sampler.lastAttempt.IsZero() || now.Sub(sampler.lastAttempt) >= interval) {
		sampler.refreshing = true
		sampler.lastAttempt = now
		go sampler.refresh()
	}
	return cloneAMDTemperatureInfo(sampler.info)
}

func (sampler *amdADLSampler) refresh() {
	info, err := collectAMDTemperatures(sampler.native)

	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	sampler.refreshing = false
	sampler.lastFailed = err != nil
	if err == nil {
		sampler.info = info
	}
}

func collectAMDTemperatures(native amdADLQuerier) (amdTemperatureInfo, error) {
	if native == nil || native.adapterCount() <= 0 {
		return amdTemperatureInfo{}, errors.New("AMD ADL2 查询器不可用")
	}

	var edge, hotspot, memory *float64
	for adapter := int32(0); adapter < native.adapterCount(); adapter++ {
		output := amdADLPMLogDataOutput{size: int32(unsafe.Sizeof(amdADLPMLogDataOutput{}))}
		if native.queryPMLog(adapter, &output) != amdADLStatusOK {
			continue
		}
		edge = maxValidAMDTemperature(edge, output.sensors[amdADLSensorTemperatureEdge])
		hotspot = maxValidAMDTemperature(hotspot, output.sensors[amdADLSensorTemperatureHotspot])
		memory = maxValidAMDTemperature(memory, output.sensors[amdADLSensorTemperatureMemory])
	}

	primary := edge
	if primary == nil {
		primary = hotspot
	}
	if primary == nil {
		primary = memory
	}
	if primary == nil {
		return amdTemperatureInfo{}, errors.New("AMD ADL2 未返回有效温度")
	}
	return amdTemperatureInfo{
		temperature:        cloneFloat64(primary),
		edgeTemperature:    cloneFloat64(edge),
		hotspotTemperature: cloneFloat64(hotspot),
		memoryTemperature:  cloneFloat64(memory),
	}, nil
}

func maxValidAMDTemperature(current *float64, sensor amdADLSingleSensorData) *float64 {
	if sensor.supported == 0 {
		return current
	}
	value := float64(sensor.value)
	if value < -50 || value > 200 {
		return current
	}
	if current == nil || value > *current {
		return &value
	}
	return current
}

func cloneAMDTemperatureInfo(info amdTemperatureInfo) amdTemperatureInfo {
	return amdTemperatureInfo{
		temperature:        cloneFloat64(info.temperature),
		edgeTemperature:    cloneFloat64(info.edgeTemperature),
		hotspotTemperature: cloneFloat64(info.hotspotTemperature),
		memoryTemperature:  cloneFloat64(info.memoryTemperature),
	}
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
