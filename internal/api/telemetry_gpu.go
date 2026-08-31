package api

import (
	"net/http"
	"strings"

	"casescreen/internal/config"
	"casescreen/internal/telemetry"
)

type telemetryGPUManager interface {
	SetGPUTarget(string) (telemetry.GPUTargetStatus, error)
	GPUTargetStatus() telemetry.GPUTargetStatus
}

type TelemetryGPURequest struct {
	AdapterID string `json:"adapter_id"`
}

func (server *Server) getTelemetryGPU(writer http.ResponseWriter, _ *http.Request) {
	if server.telemetryGPUs == nil {
		writeError(writer, http.StatusNotImplemented, "当前遥测采集器不支持选择 GPU")
		return
	}
	status := server.telemetryGPUs.GPUTargetStatus()
	cfg := server.store.Get()
	applyConfiguredTelemetryGPU(&status, cfg.TelemetryGPUAdapterID, cfg.TelemetryGPUAdapterName)
	writeJSON(writer, http.StatusOK, status)
}

func (server *Server) putTelemetryGPU(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	if server.telemetryGPUs == nil {
		writeError(writer, http.StatusNotImplemented, "当前遥测采集器不支持选择 GPU")
		return
	}
	var body TelemetryGPURequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	adapterID := normalizeTelemetryGPUAdapterID(body.AdapterID)
	server.settingsMu.Lock()
	defer server.settingsMu.Unlock()
	previous := normalizeTelemetryGPUAdapterID(server.store.Get().TelemetryGPUAdapterID)
	status, err := server.telemetryGPUs.SetGPUTarget(adapterID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err := server.store.Update(func(cfg *config.Config) error {
		if status.Automatic {
			cfg.TelemetryGPUAdapterID = ""
			cfg.TelemetryGPUAdapterName = ""
		} else {
			cfg.TelemetryGPUAdapterID = status.ConfiguredID
			cfg.TelemetryGPUAdapterName = status.ConfiguredName
		}
		return nil
	}); err != nil {
		if _, rollbackErr := server.telemetryGPUs.SetGPUTarget(previous); rollbackErr != nil {
			writeError(writer, http.StatusInternalServerError, err.Error()+"；回滚监控 GPU 失败: "+rollbackErr.Error())
			return
		}
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	applyConfiguredTelemetryGPU(&status, status.ConfiguredID, status.ConfiguredName)
	writeJSON(writer, http.StatusOK, status)
}

func normalizeTelemetryGPUAdapterID(adapterID string) string {
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" || strings.EqualFold(adapterID, telemetry.AutomaticGPUAdapterID) {
		return telemetry.AutomaticGPUAdapterID
	}
	return adapterID
}

func applyConfiguredTelemetryGPU(status *telemetry.GPUTargetStatus, configured, savedName string) {
	if status == nil {
		return
	}
	configured = normalizeTelemetryGPUAdapterID(configured)
	status.ConfiguredID = configured
	status.ConfiguredName = strings.TrimSpace(savedName)
	status.Automatic = strings.EqualFold(configured, telemetry.AutomaticGPUAdapterID)
	if status.Automatic {
		status.ConfiguredID = telemetry.AutomaticGPUAdapterID
		status.ConfiguredName = ""
		return
	}
	for _, adapter := range status.Adapters {
		if strings.EqualFold(adapter.ID, configured) {
			status.ConfiguredID = adapter.ID
			status.ConfiguredName = adapter.Name
			return
		}
	}
}
