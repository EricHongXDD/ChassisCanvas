//go:build windows

package telemetry

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (value filetime) uint64() uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}

type memoryStatusEx struct {
	Length            uint32
	MemoryLoad        uint32
	TotalPhysical     uint64
	AvailablePhysical uint64
	TotalPageFile     uint64
	AvailablePageFile uint64
	TotalVirtual      uint64
	AvailableVirtual  uint64
	AvailableExtended uint64
}

// Collector 只使用 Win32 API，不引入常驻的第三方监控库。
type Collector struct {
	mu                     sync.Mutex
	previous               systemTimes
	initialized            bool
	cpuModel               string
	gpuModel               string
	gpu                    *windowsGPUSampler
	nvidia                 *nvidiaSMISampler
	amd                    *amdADLSampler
	target                 gpuTelemetryTarget
	configuredGPUAdapterID string
}

type systemTimes struct {
	idle   uint64
	kernel uint64
	user   uint64
}

func NewCollector() *Collector {
	// 遥测目标独立于 Windows 的逐程序 GPU 运算首选项；始终从物理适配器
	// 中选择独显，并把型号与性能计数器绑定到同一个 LUID。
	gpuTarget := readPreferredGPUTelemetryTarget()
	gpuModel := gpuTarget.model
	if gpuModel == "" {
		gpuModel = readPreferredDisplayAdapterModel()
	}
	collector := &Collector{
		cpuModel:               readCPUModel(),
		gpuModel:               gpuModel,
		gpu:                    newWindowsGPUSampler(gpuTarget),
		nvidia:                 newNvidiaSMISampler(gpuModel),
		amd:                    newAMDADLSampler(gpuModel),
		target:                 gpuTarget,
		configuredGPUAdapterID: AutomaticGPUAdapterID,
	}
	return collector
}

// SetGPUTarget 立即切换硬件监控来源；它不读取或修改 Windows 逐程序 GPU 首选项。
func (collector *Collector) SetGPUTarget(adapterID string) (GPUTargetStatus, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	configured := strings.TrimSpace(adapterID)
	if configured == "" {
		configured = AutomaticGPUAdapterID
	}
	target, targets, err := resolveGPUTelemetryTarget(configured)
	if err != nil {
		return collector.gpuTargetStatusLocked(configured, targets), err
	}
	collector.target = target
	collector.gpuModel = target.model
	collector.configuredGPUAdapterID = configured
	if collector.gpu == nil {
		collector.gpu = newWindowsGPUSampler(target)
	} else {
		collector.gpu.setTarget(target)
	}
	// 厂商采样器按需只初始化一次；切回同厂商时复用，避免 ADL 上下文泄漏。
	if looksLikeAMD(target.model) && collector.amd == nil {
		collector.amd = newAMDADLSampler(target.model)
	}
	if looksLikeNVIDIA(target.model) && collector.nvidia == nil {
		collector.nvidia = newNvidiaSMISampler(target.model)
	}
	return collector.gpuTargetStatusLocked(configured, targets), nil
}

func (collector *Collector) GPUTargetStatus() GPUTargetStatus {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.gpuTargetStatusLocked(collector.configuredGPUAdapterID, readGPUTelemetryTargets())
}

func (collector *Collector) gpuTargetStatusLocked(configured string, targets []gpuTelemetryTarget) GPUTargetStatus {
	if strings.TrimSpace(configured) == "" {
		configured = AutomaticGPUAdapterID
	}
	status := GPUTargetStatus{
		ConfiguredID:  configured,
		EffectiveID:   collector.target.deviceID,
		EffectiveName: collector.target.model,
		Automatic:     strings.EqualFold(configured, AutomaticGPUAdapterID),
		Adapters:      publicGPUAdapters(targets),
	}
	if !status.Automatic {
		for _, adapter := range status.Adapters {
			if strings.EqualFold(adapter.ID, configured) {
				status.ConfiguredID = adapter.ID
				status.ConfiguredName = adapter.Name
				break
			}
		}
	}
	return status
}

func (collector *Collector) Capture() (Snapshot, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()

	times, err := readSystemTimes()
	if err != nil {
		return Snapshot{}, err
	}
	cpu := 0.0
	if collector.initialized {
		idle := times.idle - collector.previous.idle
		kernel := times.kernel - collector.previous.kernel
		user := times.user - collector.previous.user
		total := kernel + user
		if total > 0 && total >= idle {
			cpu = float64(total-idle) * 100 / float64(total)
		}
	}
	collector.previous = times
	collector.initialized = true

	memory, err := readMemory()
	if err != nil {
		return Snapshot{}, err
	}
	const gib = 1024 * 1024 * 1024
	snapshot := Snapshot{
		CapturedAt:     time.Now(),
		CPUPercent:     cpu,
		MemoryPercent:  float64(memory.MemoryLoad),
		MemoryUsedGiB:  float64(memory.TotalPhysical-memory.AvailablePhysical) / gib,
		MemoryTotalGiB: float64(memory.TotalPhysical) / gib,
		CPUModel:       collector.cpuModel,
		GPUModel:       collector.gpuModel,
	}

	// GPU 是可选指标。性能计数器或显卡厂商工具失败时，仍返回已采集的 CPU/内存数据。
	// PDH 已按独显 LUID 过滤，可作为所有厂商的轻量采样和 AMD 驱动采样
	// 首次就绪前的回退值，不会混入集显或虚拟适配器。
	if collector.gpu != nil {
		if load, ok := collector.gpu.capture(); ok {
			snapshot.GPUPercent = &load
		}
	}
	if collector.nvidia != nil && looksLikeNVIDIA(snapshot.GPUModel) {
		info := collector.nvidia.cachedAndRefresh()
		if snapshot.GPUModel == "" && info.model != "" {
			snapshot.GPUModel = info.model
			collector.gpuModel = info.model
		}
		if info.temperature != nil {
			temperature := *info.temperature
			snapshot.GPUTemperature = &temperature
		}
	}
	if collector.amd != nil && looksLikeAMD(snapshot.GPUModel) {
		info := collector.amd.cachedAndRefresh()
		// AMD 的多个逻辑适配器索引可能返回不同瞬时活动率；占用率统一使用
		// 上面按物理 LUID 过滤的 PDH 结果，ADL 只补充三类温度。
		if info.temperature != nil {
			temperature := *info.temperature
			snapshot.GPUTemperature = &temperature
		}
		if info.hotspotTemperature != nil {
			temperature := *info.hotspotTemperature
			snapshot.GPUHotspotTemperature = &temperature
		}
		if info.memoryTemperature != nil {
			temperature := *info.memoryTemperature
			snapshot.GPUMemoryTemperature = &temperature
		}
	}
	return snapshot, nil
}

func readSystemTimes() (systemTimes, error) {
	var idle, kernel, user filetime
	ok, _, callErr := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ok == 0 {
		return systemTimes{}, callErr
	}
	return systemTimes{idle: idle.uint64(), kernel: kernel.uint64(), user: user.uint64()}, nil
}

func readMemory() (memoryStatusEx, error) {
	status := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	ok, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ok == 0 {
		if callErr == syscall.Errno(0) {
			callErr = errors.New("GlobalMemoryStatusEx 失败")
		}
		return memoryStatusEx{}, callErr
	}
	return status, nil
}
