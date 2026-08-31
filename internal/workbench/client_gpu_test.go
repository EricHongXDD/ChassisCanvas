package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	localapi "casescreen/internal/api"
	"casescreen/internal/gpupreference"
)

func TestGPUPreferenceClientReadsAndSavesDedicatedSetting(t *testing.T) {
	type observedSave struct {
		token     string
		adapterID string
		decodeErr error
	}
	saves := make(chan observedSave, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"token": "gpu-test-token", "protocol_version": localapi.ProtocolVersion,
		})
	})
	mux.HandleFunc("GET /api/gpu-preference", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"configured": "power_saving", "effective": "power_saving", "supported": true, "consistent": true,
			"configured_adapter_id": `PCI\VEN_8086&DEV_7D67`, "configured_adapter_name": "Intel(R) Graphics",
			"adapters": []map[string]any{
				{"id": `PCI\VEN_8086&DEV_7D67`, "name": "Intel(R) Graphics", "preference": "power_saving"},
				{"id": `PCI\VEN_1002&DEV_744C`, "name": "AMD Radeon RX 7900 XTX", "preference": "high_performance", "active": true, "primary": true},
			},
		})
	})
	mux.HandleFunc("PUT /api/gpu-preference", func(writer http.ResponseWriter, request *http.Request) {
		var body localapi.GPUPreferenceRequest
		decodeErr := json.NewDecoder(request.Body).Decode(&body)
		saves <- observedSave{token: request.Header.Get("X-CaseScreen-Token"), adapterID: body.AdapterID, decodeErr: decodeErr}
		if decodeErr != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"configured": "high_performance", "effective": "high_performance", "supported": true, "consistent": true,
			"configured_adapter_id": body.AdapterID, "configured_adapter_name": "AMD Radeon RX 7900 XTX",
			"restart_required": true, "restart_processes": []string{"CaseScreen.exe", "CaseScreenDesktop.exe"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewAPI(server.URL)
	status, err := client.GPUPreference()
	if err != nil {
		t.Fatalf("读取 GPU 设置: %v", err)
	}
	if string(status.Configured) != "power_saving" || len(status.Adapters) != 2 {
		t.Fatalf("GPU 设置状态不完整: %+v", status)
	}
	savedStatus, err := client.SaveGPUAdapter(` PCI\VEN_1002&DEV_744C `)
	if err != nil {
		t.Fatalf("保存 GPU 设置: %v", err)
	}
	var observed observedSave
	select {
	case observed = <-saves:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 GPU 设置请求超时")
	}
	if observed.decodeErr != nil || observed.token != "gpu-test-token" || observed.adapterID != `PCI\VEN_1002&DEV_744C` || savedStatus.ConfiguredAdapterName != "AMD Radeon RX 7900 XTX" || !savedStatus.RestartRequired {
		t.Fatalf("GPU 设置保存结果错误: observed=%+v status=%+v", observed, savedStatus)
	}
}

func TestGPUSettingsStateLoadsAsynchronously(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/gpu-preference" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"configured": "high_performance", "effective": "high_performance", "supported": true, "consistent": true,
			"configured_adapter_id": `PCI\VEN_1002&DEV_744C`, "configured_adapter_name": "AMD Radeon RX 7900 XTX",
			"adapters": []map[string]any{
				{"id": `PCI\VEN_8086&DEV_7D67`, "name": "Intel(R) Graphics", "preference": "power_saving"},
				{"id": `PCI\VEN_1002&DEV_744C`, "name": "AMD Radeon RX 7900 XTX", "preference": "high_performance"},
			},
		})
	}))
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), uiUpdates: make(chan func(), 1)}
	model.loadGPUPreference(false)
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(2 * time.Second):
		t.Fatal("等待 GPU 设置异步读取超时")
	}
	if !model.gpuSettings.loaded || model.gpuSettings.draft != `PCI\VEN_1002&DEV_744C` {
		t.Fatalf("GPU 设置未载入工作台状态: %+v", model.gpuSettings)
	}
	options := gpuAdapterOptions(model.gpuSettings.status)
	if len(options) != 3 || options[0].label != "系统自动" || options[1].label != "Intel(R) Graphics" || options[2].label != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("显卡下拉选项错误: %+v", options)
	}
	if got := selectedGPUAdapterLabel(model.gpuSettings.status, model.gpuSettings.draft); got != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("当前显卡显示错误: %q", got)
	}
}

func TestGPUAdapterOptionsValidateSystemAndDetectedAdapters(t *testing.T) {
	status := localapi.GPUPreferenceStatus{Adapters: []gpupreference.AdapterSummary{
		{ID: `PCI\VEN_8086&DEV_7D67`, Name: "Intel(R) Graphics", Preference: gpupreference.PowerSaving},
		{ID: `PCI\VEN_1002&DEV_744C`, Name: "AMD Radeon RX 7900 XTX", Preference: gpupreference.HighPerformance},
	}}
	if !validGPUAdapterSelection(status, systemAutomaticGPUOption) || !validGPUAdapterSelection(status, `pci\ven_8086&dev_7d67`) {
		t.Fatal("系统自动和枚举显卡都应可选")
	}
	if validGPUAdapterSelection(status, `PCI\VEN_FAKE`) {
		t.Fatal("未检测到的显卡不应可保存")
	}
}

func TestConfiguredGPUSelectionInfersLegacyPreference(t *testing.T) {
	status := localapi.GPUPreferenceStatus{
		Configured: gpupreference.PowerSaving,
		Adapters: []gpupreference.AdapterSummary{
			{ID: `PCI\VEN_8086&DEV_7D67`, Name: "Intel(R) Graphics", Preference: gpupreference.PowerSaving},
			{ID: `PCI\VEN_1002&DEV_744C`, Name: "AMD Radeon RX 7900 XTX", Preference: gpupreference.HighPerformance},
		},
	}
	if got := configuredGPUSelection(status); got != `PCI\VEN_8086&DEV_7D67` {
		t.Fatalf("旧版首选项未映射到真实显卡: %q", got)
	}
	status.Configured = gpupreference.SystemDefault
	if got := configuredGPUSelection(status); got != systemAutomaticGPUOption {
		t.Fatalf("系统自动选择错误: %q", got)
	}
}

func TestGPUSettingsStateDisablesEditingAfterRefreshFailure(t *testing.T) {
	state := gpuSettingsState{
		loaded: true,
		status: localapi.GPUPreferenceStatus{Supported: true},
	}
	if !state.editable() {
		t.Fatal("成功读取且受支持时应允许编辑 GPU 设置")
	}
	state.lastError = "temporary failure"
	if state.editable() {
		t.Fatal("刷新失败后不应允许用过期状态保存 GPU 设置")
	}
	state.lastError = ""
	state.status.Supported = false
	if state.editable() {
		t.Fatal("不受支持的平台不应允许编辑 GPU 设置")
	}
}
