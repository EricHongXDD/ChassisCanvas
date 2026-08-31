package gpupreference

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Preference string

const (
	SystemDefault   Preference = "system"
	PowerSaving     Preference = "power_saving"
	HighPerformance Preference = "high_performance"
	Unknown         Preference = "unknown"
	Mixed           Preference = "mixed"
)

const Notice = "下拉框显示 Windows 实际检测到的显卡名称；保存时会把所选显卡映射为 Windows 支持的逐程序 GPU 调度首选项并持久写入当前用户注册表。保存后需重启 CaseScreen.exe 和 CaseScreenDesktop.exe，最终调度仍由 Windows 决定。"

var ErrAdapterNotFound = errors.New("所选显卡当前未检测到")

var processNames = []string{"CaseScreen.exe", "CaseScreenDesktop.exe"}

type ApplicationStatus struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Preference Preference `json:"preference"`
}

type AdapterSummary struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Preference Preference `json:"preference"`
	Active     bool       `json:"active"`
	Primary    bool       `json:"primary"`
}

type Status struct {
	Supported            bool                `json:"supported"`
	Effective            Preference          `json:"effective"`
	Consistent           bool                `json:"consistent"`
	Applications         []ApplicationStatus `json:"applications"`
	Adapters             []AdapterSummary    `json:"adapters,omitempty"`
	PowerSavingLabel     string              `json:"power_saving_label"`
	HighPerformanceLabel string              `json:"high_performance_label"`
	RestartProcesses     []string            `json:"restart_processes"`
	Notice               string              `json:"notice"`
}

type ApplyResult struct {
	Status  Status
	Adapter AdapterSummary
	Changed bool
}

type Manager struct {
	mu      sync.Mutex
	targets []ApplicationStatus
}

func Parse(value string) (Preference, error) {
	preference := Preference(strings.ToLower(strings.TrimSpace(value)))
	switch preference {
	case SystemDefault, PowerSaving, HighPerformance:
		return preference, nil
	default:
		return "", fmt.Errorf("GPU 首选项不受支持: %q", value)
	}
}

func NewManager() *Manager {
	executable, err := os.Executable()
	if err != nil {
		executable = "."
	}
	return NewManagerForExecutable(executable)
}

// NewManagerForExecutable 只从当前程序目录推导两个固定的正式 EXE，
// 不接受来自 API 的任意路径，避免本机设置接口变成通用注册表写入器。
func NewManagerForExecutable(executable string) *Manager {
	root := filepath.Dir(executable)
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	targets := make([]ApplicationStatus, 0, len(processNames))
	for _, name := range processNames {
		targets = append(targets, ApplicationStatus{Name: name, Path: filepath.Clean(filepath.Join(root, name))})
	}
	return &Manager{targets: targets}
}

func (manager *Manager) Status() (Status, error) {
	if manager == nil {
		return Status{}, errors.New("GPU 首选项管理器未初始化")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return platformStatus(manager.targets)
}

func (manager *Manager) Apply(value Preference) (ApplyResult, error) {
	if manager == nil {
		return ApplyResult{}, errors.New("GPU 首选项管理器未初始化")
	}
	preference, err := Parse(string(value))
	if err != nil {
		return ApplyResult{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return platformApply(manager.targets, preference)
}

// ApplyAdapter 只接受本机枚举结果中的稳定适配器 ID。Windows 的逐程序
// 注册表契约只能表达节能/高性能调度类别，因此由后端完成 ID 到类别的映射，
// API 不允许调用方直接伪造类别或注册表路径。
func (manager *Manager) ApplyAdapter(adapterID string) (ApplyResult, error) {
	if manager == nil {
		return ApplyResult{}, errors.New("GPU 首选项管理器未初始化")
	}
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return ApplyResult{}, errors.New("请选择检测到的显卡")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	status, err := platformStatus(manager.targets)
	if err != nil {
		return ApplyResult{}, err
	}
	adapter, ok := FindAdapter(status.Adapters, adapterID)
	if !ok {
		return ApplyResult{}, fmt.Errorf("%w: %q", ErrAdapterNotFound, adapterID)
	}
	preference, err := Parse(string(adapter.Preference))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("显卡 %q 缺少有效的 Windows 调度映射: %w", adapter.Name, err)
	}
	result, err := platformApply(manager.targets, preference)
	if err != nil {
		return ApplyResult{}, err
	}
	result.Adapter = adapter
	return result, nil
}

// FindAdapter 使用不区分大小写的设备 ID 匹配当前枚举结果。
func FindAdapter(adapters []AdapterSummary, adapterID string) (AdapterSummary, bool) {
	adapterID = strings.TrimSpace(adapterID)
	for _, adapter := range adapters {
		if strings.EqualFold(strings.TrimSpace(adapter.ID), adapterID) {
			return adapter, true
		}
	}
	return AdapterSummary{}, false
}

func statusFromApplications(supported bool, applications []ApplicationStatus) Status {
	status := Status{
		Supported:            supported,
		Effective:            SystemDefault,
		Consistent:           true,
		Applications:         applications,
		PowerSavingLabel:     "",
		HighPerformanceLabel: "",
		RestartProcesses:     append([]string(nil), processNames...),
		Notice:               Notice,
	}
	if len(applications) == 0 {
		return status
	}
	status.Effective = applications[0].Preference
	for _, application := range applications[1:] {
		if application.Preference != status.Effective {
			status.Effective = Mixed
			status.Consistent = false
			break
		}
	}
	return status
}
