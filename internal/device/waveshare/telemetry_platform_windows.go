//go:build windows

package waveshare

import (
	"math"
	"strings"
	"sync"
	"time"
	"unsafe"

	"casescreen/internal/telemetry"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	pdhFormatDouble = 0x00000200
	pdhMoreData     = 0x800007D2
)

var (
	pdhDLL                      = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW           = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW   = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData     = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetCounterArrayW     = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery           = pdhDLL.NewProc("PdhCloseQuery")
	defaultWaveshareGPUPlatform = &waveshareGPUSampler{}
)

type waveshareGPUSampler struct {
	mu        sync.Mutex
	query     uintptr
	counter   uintptr
	primed    bool
	model     string
	nextRetry time.Time
}

type gpuCounterSample struct {
	name  string
	value float64
}

func enrichWavesharePlatformTelemetry(snapshot telemetry.Snapshot) telemetry.Snapshot {
	// 上游已经选定型号时，GPUPercent 为空表示目标显卡采样尚未就绪；这里
	// 不能再用聚合所有 LUID 的旧回退值，否则会把集显占用率写进独显主题。
	if snapshot.GPUModel != "" {
		return snapshot
	}
	load, model, ok := defaultWaveshareGPUPlatform.capture()
	if snapshot.GPUPercent == nil && ok {
		snapshot.GPUPercent = &load
	}
	if snapshot.GPUModel == "" && model != "" {
		snapshot.GPUModel = model
	}
	return snapshot
}

func (sampler *waveshareGPUSampler) capture() (float64, string, bool) {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	if sampler.model == "" {
		sampler.model = readDisplayAdapterModel()
	}
	if sampler.query == 0 {
		if time.Now().Before(sampler.nextRetry) || sampler.open() != nil {
			sampler.nextRetry = time.Now().Add(30 * time.Second)
			return 0, sampler.model, false
		}
	}
	if status, _, _ := procPdhCollectQueryData.Call(sampler.query); status != 0 {
		_, _, _ = procPdhCloseQuery.Call(sampler.query)
		sampler.nextRetry = time.Now().Add(30 * time.Second)
		sampler.query = 0
		sampler.counter = 0
		sampler.primed = false
		return 0, sampler.model, false
	}
	if !sampler.primed {
		sampler.primed = true
		return 0, sampler.model, false
	}
	samples, ok := sampler.readCounterArray()
	if !ok {
		return 0, sampler.model, false
	}
	return aggregateGPUEngineSamples(samples), sampler.model, true
}

func (sampler *waveshareGPUSampler) open() error {
	path, err := windows.UTF16PtrFromString(`\GPU Engine(*)\Utilization Percentage`)
	if err != nil {
		return err
	}
	var query uintptr
	if status, _, callErr := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&query))); status != 0 {
		return callErr
	}
	var counter uintptr
	if status, _, callErr := procPdhAddEnglishCounterW.Call(query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter))); status != 0 {
		_, _, _ = procPdhCloseQuery.Call(query)
		return callErr
	}
	sampler.query = query
	sampler.counter = counter
	sampler.primed = false
	return nil
}

func (sampler *waveshareGPUSampler) readCounterArray() ([]gpuCounterSample, bool) {
	var size, count uint32
	status, _, _ := procPdhGetCounterArrayW.Call(
		sampler.counter, pdhFormatDouble,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0,
	)
	if status != pdhMoreData || size == 0 {
		return nil, false
	}
	buffer := make([]byte, size)
	status, _, _ = procPdhGetCounterArrayW.Call(
		sampler.counter, pdhFormatDouble,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buffer[0])),
	)
	if status != 0 || count == 0 {
		return nil, false
	}
	pointerSize := int(unsafe.Sizeof(uintptr(0)))
	itemSize := 16
	doubleOffset := 8
	if pointerSize == 8 {
		itemSize = 24
		doubleOffset = 16
	}
	samples := make([]gpuCounterSample, 0, count)
	for index := 0; index < int(count); index++ {
		offset := index * itemSize
		if offset+itemSize > len(buffer) {
			break
		}
		namePointer := *(*uintptr)(unsafe.Pointer(&buffer[offset]))
		counterStatus := *(*uint32)(unsafe.Pointer(&buffer[offset+pointerSize]))
		value := *(*float64)(unsafe.Pointer(&buffer[offset+doubleOffset]))
		if namePointer == 0 || counterStatus > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		name := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(namePointer)))
		samples = append(samples, gpuCounterSample{name: name, value: value})
	}
	return samples, len(samples) > 0
}

func aggregateGPUEngineSamples(samples []gpuCounterSample) float64 {
	engines := make(map[string]float64)
	for _, sample := range samples {
		key := gpuEngineKey(sample.name)
		if key == "" || sample.value <= 0 {
			continue
		}
		engines[key] += sample.value
	}
	maximum := 0.0
	for _, value := range engines {
		maximum = math.Max(maximum, value)
	}
	return math.Max(0, math.Min(100, maximum))
}

func gpuEngineKey(name string) string {
	start := strings.Index(strings.ToLower(name), "luid_")
	if start < 0 {
		return ""
	}
	return name[start:]
}

func readDisplayAdapterModel() string {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Video`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	defer root.Close()
	identifiers, _ := root.ReadSubKeyNames(-1)
	for _, identifier := range identifiers {
		adapterRoot, openErr := registry.OpenKey(root, identifier, registry.ENUMERATE_SUB_KEYS)
		if openErr != nil {
			continue
		}
		entries, _ := adapterRoot.ReadSubKeyNames(-1)
		for _, entry := range entries {
			adapter, entryErr := registry.OpenKey(adapterRoot, entry, registry.QUERY_VALUE)
			if entryErr != nil {
				continue
			}
			name, _, _ := adapter.GetStringValue("DriverDesc")
			adapter.Close()
			lower := strings.ToLower(name)
			if name != "" && !strings.Contains(lower, "virtual") && !strings.Contains(lower, "remote") && !strings.Contains(lower, "basic display") {
				adapterRoot.Close()
				return name
			}
		}
		adapterRoot.Close()
	}
	return ""
}
