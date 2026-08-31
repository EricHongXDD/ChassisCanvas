package telemetry

// AutomaticGPUAdapterID 表示由 CaseScreen 自动挑选最合适的物理 GPU。
const AutomaticGPUAdapterID = "auto"

// GPUAdapter 描述一块可用于硬件监控的物理显示适配器。
// 它只决定遥测来源，不影响 CaseScreen 进程参与运算的显卡。
type GPUAdapter struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	DedicatedVideoMemory uint64 `json:"dedicated_video_memory,omitempty"`
	Active               bool   `json:"active"`
	Primary              bool   `json:"primary"`
}

// GPUTargetStatus 是监控 GPU 的持久化选择和当前实际生效目标。
type GPUTargetStatus struct {
	ConfiguredID   string       `json:"configured_adapter_id"`
	ConfiguredName string       `json:"configured_adapter_name,omitempty"`
	EffectiveID    string       `json:"effective_adapter_id,omitempty"`
	EffectiveName  string       `json:"effective_adapter_name,omitempty"`
	Automatic      bool         `json:"automatic"`
	Adapters       []GPUAdapter `json:"adapters"`
}
