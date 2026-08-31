package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"casescreen/internal/config"
	"casescreen/internal/telemetry"
)

type fakeTelemetryGPUManager struct {
	status telemetry.GPUTargetStatus
	setErr error
	setIDs []string
}

func (manager *fakeTelemetryGPUManager) GPUTargetStatus() telemetry.GPUTargetStatus {
	return manager.status
}

func (manager *fakeTelemetryGPUManager) SetGPUTarget(adapterID string) (telemetry.GPUTargetStatus, error) {
	manager.setIDs = append(manager.setIDs, adapterID)
	if manager.setErr != nil {
		return manager.status, manager.setErr
	}
	manager.status.ConfiguredID = adapterID
	manager.status.Automatic = strings.EqualFold(adapterID, telemetry.AutomaticGPUAdapterID)
	manager.status.ConfiguredName = ""
	if manager.status.Automatic {
		manager.status.EffectiveID = manager.status.Adapters[1].ID
		manager.status.EffectiveName = manager.status.Adapters[1].Name
	} else {
		for _, adapter := range manager.status.Adapters {
			if strings.EqualFold(adapter.ID, adapterID) {
				manager.status.ConfiguredID = adapter.ID
				manager.status.ConfiguredName = adapter.Name
				manager.status.EffectiveID = adapter.ID
				manager.status.EffectiveName = adapter.Name
				return manager.status, nil
			}
		}
		return manager.status, errors.New("没有找到监控 GPU")
	}
	return manager.status, nil
}

func testTelemetryGPUStatus() telemetry.GPUTargetStatus {
	return telemetry.GPUTargetStatus{
		ConfiguredID: telemetry.AutomaticGPUAdapterID,
		EffectiveID:  `PCI\VEN_1002&DEV_744C`, EffectiveName: "AMD Radeon RX 7900 XTX",
		Automatic: true,
		Adapters: []telemetry.GPUAdapter{
			{ID: `PCI\VEN_8086&DEV_7D67`, Name: "Intel(R) Graphics", Active: true, Primary: true},
			{ID: `PCI\VEN_1002&DEV_744C`, Name: "AMD Radeon RX 7900 XTX", DedicatedVideoMemory: 24 << 30},
		},
	}
}

func TestTelemetryGPUGetUsesDedicatedConfigAndStableSchema(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeTelemetryGPUManager{status: testTelemetryGPUStatus()}
	server.telemetryGPUs = manager
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.TelemetryGPUAdapterID = `PCI\VEN_8086&DEV_7D67`
		cfg.TelemetryGPUAdapterName = "Intel(R) Graphics"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/telemetry-gpu", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("读取监控 GPU 失败: %d %s", response.Code, response.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"configured_adapter_id", "configured_adapter_name", "effective_adapter_id", "effective_adapter_name", "automatic", "adapters"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("响应缺少字段 %q: %s", field, response.Body.String())
		}
	}
	var status telemetry.GPUTargetStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ConfiguredName != "Intel(R) Graphics" || status.Automatic || status.EffectiveName != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("保存选择与生效目标报告错误: %+v", status)
	}
}

func TestTelemetryGPUPutSwitchesImmediatelyAndPersistsIndependently(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeTelemetryGPUManager{status: testTelemetryGPUStatus()}
	server.telemetryGPUs = manager
	request := httptest.NewRequest(http.MethodPut, "/api/telemetry-gpu", strings.NewReader(`{"adapter_id":"PCI\\VEN_8086&DEV_7D67"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("切换监控 GPU 失败: %d %s", response.Code, response.Body.String())
	}
	if len(manager.setIDs) != 1 || manager.setIDs[0] != `PCI\VEN_8086&DEV_7D67` {
		t.Fatalf("没有立即切换采集器: %v", manager.setIDs)
	}
	cfg := server.store.Get()
	if cfg.TelemetryGPUAdapterID != `PCI\VEN_8086&DEV_7D67` || cfg.TelemetryGPUAdapterName != "Intel(R) Graphics" {
		t.Fatalf("监控 GPU 没有持久化: %+v", cfg)
	}
	if cfg.GPUAdapterID != "" || cfg.GPUAdapterName != "" {
		t.Fatalf("监控 GPU 污染了进程运算显卡设置: %+v", cfg)
	}
}

func TestTelemetryGPUPutAutoClearsPhysicalIdentity(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeTelemetryGPUManager{status: testTelemetryGPUStatus()}
	server.telemetryGPUs = manager
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.TelemetryGPUAdapterID = `PCI\VEN_8086&DEV_7D67`
		cfg.TelemetryGPUAdapterName = "Intel(R) Graphics"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/telemetry-gpu", strings.NewReader(`{"adapter_id":"auto"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("恢复自动选择失败: %d %s", response.Code, response.Body.String())
	}
	cfg := server.store.Get()
	if cfg.TelemetryGPUAdapterID != "" || cfg.TelemetryGPUAdapterName != "" {
		t.Fatalf("自动选择仍保存物理身份: %+v", cfg)
	}
}

func TestTelemetryGPUPutRejectsFailureWithoutChangingConfig(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeTelemetryGPUManager{status: testTelemetryGPUStatus(), setErr: errors.New("没有找到监控 GPU")}
	server.telemetryGPUs = manager
	request := httptest.NewRequest(http.MethodPut, "/api/telemetry-gpu", strings.NewReader(`{"adapter_id":"PCI\\VEN_FAKE"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未知 GPU 状态码=%d: %s", response.Code, response.Body.String())
	}
	if cfg := server.store.Get(); cfg.TelemetryGPUAdapterID != "" || cfg.TelemetryGPUAdapterName != "" {
		t.Fatalf("失败请求改变了配置: %+v", cfg)
	}
}
