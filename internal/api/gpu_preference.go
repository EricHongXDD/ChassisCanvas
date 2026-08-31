package api

import (
	"errors"
	"net/http"
	"strings"

	"casescreen/internal/config"
	"casescreen/internal/gpupreference"
)

type gpuPreferenceManager interface {
	Status() (gpupreference.Status, error)
	Apply(gpupreference.Preference) (gpupreference.ApplyResult, error)
	ApplyAdapter(string) (gpupreference.ApplyResult, error)
}

// GPUPreferenceStatus 是桌面工作台可直接反序列化的 GPU 设置状态。
// Effective 表示注册表中两个正式程序的当前提示；Configured 表示 CaseScreen 保存的选择。
type GPUPreferenceStatus struct {
	Configured            gpupreference.Preference          `json:"configured"`
	ConfiguredAdapterID   string                            `json:"configured_adapter_id,omitempty"`
	ConfiguredAdapterName string                            `json:"configured_adapter_name,omitempty"`
	Supported             bool                              `json:"supported"`
	Effective             gpupreference.Preference          `json:"effective"`
	Consistent            bool                              `json:"consistent"`
	Applications          []gpupreference.ApplicationStatus `json:"applications"`
	Adapters              []gpupreference.AdapterSummary    `json:"adapters,omitempty"`
	PowerSavingLabel      string                            `json:"power_saving_label"`
	HighPerformanceLabel  string                            `json:"high_performance_label"`
	RestartRequired       bool                              `json:"restart_required"`
	RestartProcesses      []string                          `json:"restart_processes"`
	Notice                string                            `json:"notice"`
}

type GPUPreferenceRequest struct {
	Preference string `json:"preference,omitempty"`
	AdapterID  string `json:"adapter_id,omitempty"`
}

func newGPUPreferenceManager() gpuPreferenceManager {
	return gpupreference.NewManager()
}

func (server *Server) getGPUPreference(writer http.ResponseWriter, _ *http.Request) {
	status, err := server.gpuPreferences.Status()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "读取 Windows GPU 首选项失败: "+err.Error())
		return
	}
	cfg := server.store.Get()
	configured, err := gpupreference.Parse(cfg.GPUPreference)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "已保存的 GPU 首选项无效: "+err.Error())
		return
	}
	adapterID, adapterName := configuredGPUAdapter(status.Adapters, configured, cfg.GPUAdapterID, cfg.GPUAdapterName)
	writeJSON(writer, http.StatusOK, makeGPUPreferenceResponse(configured, adapterID, adapterName, status, false))
}

func (server *Server) putGPUPreference(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	var body GPUPreferenceRequest
	if err := decodeJSONLimit(writer, request, &body, 4096); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.settingsMu.Lock()
	defer server.settingsMu.Unlock()
	previous := server.store.Get()
	preference := gpupreference.Preference("")
	selectedAdapterID := ""
	selectedAdapterName := ""
	var result gpupreference.ApplyResult
	var err error
	if adapterID := strings.TrimSpace(body.AdapterID); strings.EqualFold(adapterID, string(gpupreference.SystemDefault)) {
		preference = gpupreference.SystemDefault
		result, err = server.gpuPreferences.Apply(preference)
	} else if adapterID != "" {
		result, err = server.gpuPreferences.ApplyAdapter(adapterID)
		if err == nil {
			preference = result.Adapter.Preference
			selectedAdapterID = result.Adapter.ID
			selectedAdapterName = result.Adapter.Name
		}
	} else {
		preference, err = gpupreference.Parse(body.Preference)
		if err == nil {
			result, err = server.gpuPreferences.Apply(preference)
		}
	}
	if err != nil {
		statusCode := http.StatusInternalServerError
		if strings.TrimSpace(body.AdapterID) == "" {
			if _, parseErr := gpupreference.Parse(body.Preference); parseErr != nil {
				statusCode = http.StatusBadRequest
			}
		} else if errors.Is(err, gpupreference.ErrAdapterNotFound) {
			statusCode = http.StatusBadRequest
		}
		writeError(writer, statusCode, "应用 Windows GPU 首选项失败: "+err.Error())
		return
	}
	selectionChanged := previous.GPUAdapterID != selectedAdapterID || previous.GPUAdapterName != selectedAdapterName
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.GPUPreference = string(preference)
		cfg.GPUAdapterID = selectedAdapterID
		cfg.GPUAdapterName = selectedAdapterName
		return nil
	}); err != nil {
		previousPreference, parseErr := gpupreference.Parse(previous.GPUPreference)
		if parseErr == nil {
			_, parseErr = server.gpuPreferences.Apply(previousPreference)
		}
		if parseErr != nil {
			writeError(writer, http.StatusInternalServerError, err.Error()+"；回滚 Windows GPU 首选项失败: "+parseErr.Error())
			return
		}
		writeError(writer, http.StatusInternalServerError, "保存 GPU 首选项失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, makeGPUPreferenceResponse(preference, selectedAdapterID, selectedAdapterName, result.Status, result.Changed || selectionChanged))
}

func makeGPUPreferenceResponse(configured gpupreference.Preference, adapterID, adapterName string, status gpupreference.Status, restartRequired bool) GPUPreferenceStatus {
	return GPUPreferenceStatus{
		Configured: configured, ConfiguredAdapterID: adapterID, ConfiguredAdapterName: adapterName, Supported: status.Supported,
		Effective: status.Effective, Consistent: status.Consistent,
		Applications: status.Applications, Adapters: status.Adapters,
		PowerSavingLabel: status.PowerSavingLabel, HighPerformanceLabel: status.HighPerformanceLabel,
		RestartRequired:  restartRequired,
		RestartProcesses: status.RestartProcesses, Notice: status.Notice,
	}
}

func configuredGPUAdapter(adapters []gpupreference.AdapterSummary, configured gpupreference.Preference, adapterID, adapterName string) (string, string) {
	if configured == gpupreference.SystemDefault {
		return "", ""
	}
	if adapter, ok := gpupreference.FindAdapter(adapters, adapterID); ok {
		return adapter.ID, adapter.Name
	}
	if strings.TrimSpace(adapterID) != "" {
		return strings.TrimSpace(adapterID), strings.TrimSpace(adapterName)
	}
	for _, adapter := range adapters {
		if adapter.Preference == configured {
			return adapter.ID, adapter.Name
		}
	}
	return "", ""
}
