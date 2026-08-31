//go:build windows

package telemetry

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	pdhFormatDouble = 0x00000200
	pdhMoreData     = 0x800007D2
)

var (
	pdhDLL                    = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW         = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData   = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetCounterArrayW   = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery         = pdhDLL.NewProc("PdhCloseQuery")
)

// windowsGPUSampler 直接读取 Windows GPU Engine 性能计数器，不启动外部采集进程。
type windowsGPUSampler struct {
	mu        sync.Mutex
	query     uintptr
	counter   uintptr
	primed    bool
	nextRetry time.Time

	// targetLUID 只限定硬件遥测来源，不表示 CaseScreen 进程运行在哪块 GPU。
	targetLUID     uint64
	targetResolved bool
}

type gpuCounterSample struct {
	name  string
	value float64
}

func newWindowsGPUSampler(target gpuTelemetryTarget) *windowsGPUSampler {
	return &windowsGPUSampler{
		targetLUID:     target.luid,
		targetResolved: target.luid != 0,
	}
}

// setTarget 原位切换 PDH 的物理 LUID。关闭旧查询后重新建立采样基线，
// 避免动态切换时遗留查询句柄或把前一块显卡的速率样本带到新目标。
func (sampler *windowsGPUSampler) setTarget(target gpuTelemetryTarget) {
	if sampler == nil {
		return
	}
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	if sampler.targetLUID == target.luid && sampler.targetResolved == (target.luid != 0) && sampler.query != 0 {
		return
	}
	sampler.reset(time.Time{})
	sampler.targetLUID = target.luid
	sampler.targetResolved = target.luid != 0
	if !sampler.targetResolved {
		return
	}
	// 先收集一次建立速率计数器基线。随后 telemetryWake 触发的 Capture
	// 就能读取新显卡的第二个样本，不必等待最长 60 秒的常规间隔。
	if err := sampler.open(); err != nil {
		sampler.nextRetry = time.Now().Add(30 * time.Second)
		return
	}
	if status, _, _ := procPdhCollectQueryData.Call(sampler.query); status != 0 {
		sampler.reset(time.Now().Add(30 * time.Second))
		return
	}
	sampler.primed = true
}

func (sampler *windowsGPUSampler) capture() (float64, bool) {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	if !sampler.targetResolved {
		// 不知道物理适配器身份时宁可不发布，也不能重新聚合核显或虚拟显卡。
		return 0, false
	}

	now := time.Now()
	if sampler.query == 0 {
		if now.Before(sampler.nextRetry) {
			return 0, false
		}
		if err := sampler.open(); err != nil {
			sampler.nextRetry = now.Add(30 * time.Second)
			return 0, false
		}
	}
	if status, _, _ := procPdhCollectQueryData.Call(sampler.query); status != 0 {
		sampler.reset(now.Add(30 * time.Second))
		return 0, false
	}
	// 速率型性能计数器至少需要两个样本，首帧只用于建立基线。
	if !sampler.primed {
		sampler.primed = true
		return 0, false
	}
	samples, ok := sampler.readCounterArray()
	if !ok {
		return 0, false
	}
	return aggregateGPUEngineSamplesForLUID(samples, sampler.targetLUID), true
}

func (sampler *windowsGPUSampler) open() error {
	path, err := windows.UTF16PtrFromString(`\GPU Engine(*)\Utilization Percentage`)
	if err != nil {
		return err
	}
	var query uintptr
	if status, _, callErr := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&query))); status != 0 {
		return callErr
	}
	var counter uintptr
	if status, _, callErr := procPdhAddEnglishCounterW.Call(
		query,
		uintptr(unsafe.Pointer(path)),
		0,
		uintptr(unsafe.Pointer(&counter)),
	); status != 0 {
		_, _, _ = procPdhCloseQuery.Call(query)
		return callErr
	}
	sampler.query = query
	sampler.counter = counter
	sampler.primed = false
	return nil
}

func (sampler *windowsGPUSampler) reset(nextRetry time.Time) {
	if sampler.query != 0 {
		_, _, _ = procPdhCloseQuery.Call(sampler.query)
	}
	sampler.query = 0
	sampler.counter = 0
	sampler.primed = false
	sampler.nextRetry = nextRetry
}

func (sampler *windowsGPUSampler) readCounterArray() ([]gpuCounterSample, bool) {
	var size, count uint32
	status, _, _ := procPdhGetCounterArrayW.Call(
		sampler.counter,
		pdhFormatDouble,
		uintptr(unsafe.Pointer(&size)),
		uintptr(unsafe.Pointer(&count)),
		0,
	)
	if status != pdhMoreData || size == 0 {
		return nil, false
	}
	buffer := make([]byte, size)
	status, _, _ = procPdhGetCounterArrayW.Call(
		sampler.counter,
		pdhFormatDouble,
		uintptr(unsafe.Pointer(&size)),
		uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&buffer[0])),
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
	bufferStart := uintptr(unsafe.Pointer(&buffer[0]))
	bufferEnd := bufferStart + uintptr(len(buffer))
	samples := make([]gpuCounterSample, 0, count)
	for index := 0; index < int(count); index++ {
		offset := index * itemSize
		if offset+itemSize > len(buffer) {
			break
		}
		namePointer := *(*uintptr)(unsafe.Pointer(&buffer[offset]))
		counterStatus := *(*uint32)(unsafe.Pointer(&buffer[offset+pointerSize]))
		value := *(*float64)(unsafe.Pointer(&buffer[offset+doubleOffset]))
		if namePointer < bufferStart || namePointer >= bufferEnd || counterStatus > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		nameOffset := int(namePointer - bufferStart)
		name := utf16StringFromBytes(buffer[nameOffset:])
		if name == "" {
			continue
		}
		samples = append(samples, gpuCounterSample{name: name, value: value})
	}
	return samples, len(samples) > 0
}

func utf16StringFromBytes(data []byte) string {
	units := make([]uint16, 0, len(data)/2)
	for offset := 0; offset+1 < len(data); offset += 2 {
		unit := binary.LittleEndian.Uint16(data[offset : offset+2])
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return string(utf16.Decode(units))
}

// aggregateGPUEngineSamples 先合并同一物理引擎的各进程占用，再取最繁忙引擎。
// 这与 Windows 任务管理器的总体 GPU 百分比定义一致，并避免把并行引擎直接相加到 100% 以上。
func aggregateGPUEngineSamples(samples []gpuCounterSample) float64 {
	return aggregateFilteredGPUEngineSamples(samples, 0, false)
}

// aggregateGPUEngineSamplesForLUID 只合并目标物理适配器的引擎。
// 即使该适配器当前完全空闲，也返回 0；调用方据此发布有效的 0%，而不是
// 因为别的适配器有活动而误报它的占用率。
func aggregateGPUEngineSamplesForLUID(samples []gpuCounterSample, targetLUID uint64) float64 {
	return aggregateFilteredGPUEngineSamples(samples, targetLUID, true)
}

func aggregateFilteredGPUEngineSamples(samples []gpuCounterSample, targetLUID uint64, filter bool) float64 {
	engines := make(map[string]float64)
	for _, sample := range samples {
		key := ""
		if filter {
			var ok bool
			key, ok = gpuEngineKeyForLUID(sample.name, targetLUID)
			if !ok {
				continue
			}
		} else {
			key = gpuEngineKey(sample.name)
		}
		if key == "" || sample.value <= 0 || math.IsNaN(sample.value) || math.IsInf(sample.value, 0) {
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

func gpuEngineKeyForLUID(name string, targetLUID uint64) (string, bool) {
	luid, ok := gpuEngineLUID(name)
	if !ok || luid != targetLUID {
		return "", false
	}
	lower := strings.ToLower(name)
	luidStart := strings.Index(lower, "luid_")
	if luidStart < 0 {
		return "", false
	}
	physicalOffset := strings.Index(lower[luidStart:], "_phys_")
	if physicalOffset < 0 {
		return "", false
	}
	physicalStart := luidStart + physicalOffset
	// 数值化后的 LUID 消除实例名大小写和前导零差异。
	return strconv.FormatUint(luid, 16) + lower[physicalStart:], true
}

// gpuEngineLUID 解析 PDH 实例中的两个十六进制 LUID 分量。
// Windows 可能改变字母大小写或省略前导零，因此按数值比较。
func gpuEngineLUID(name string) (uint64, bool) {
	lower := strings.ToLower(name)
	start := strings.Index(lower, "luid_")
	if start < 0 {
		return 0, false
	}
	parts := strings.SplitN(lower[start+len("luid_"):], "_", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "phys_") {
		return 0, false
	}
	high, ok := parseGPUCounterHex32(parts[0])
	if !ok {
		return 0, false
	}
	low, ok := parseGPUCounterHex32(parts[1])
	if !ok {
		return 0, false
	}
	return uint64(high)<<32 | uint64(low), true
}

func parseGPUCounterHex32(value string) (uint32, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "0x")
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(parsed), true
}

func gpuEngineKey(name string) string {
	lower := strings.ToLower(name)
	start := strings.Index(lower, "luid_")
	if start < 0 {
		return ""
	}
	return lower[start:]
}
