package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	localapi "casescreen/internal/api"
	caseTheme "casescreen/internal/theme"
)

func TestTelemetryGPUClientUsesDedicatedEndpoint(t *testing.T) {
	type observedSave struct {
		token     string
		adapterID string
	}
	saves := make(chan observedSave, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"token": "telemetry-gpu-token", "protocol_version": localapi.ProtocolVersion,
		})
	})
	mux.HandleFunc("GET /api/telemetry-gpu", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(telemetryGPUTestStatus(automaticTelemetryGPUOption, true))
	})
	mux.HandleFunc("PUT /api/telemetry-gpu", func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			AdapterID string `json:"adapter_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		saves <- observedSave{token: request.Header.Get("X-CaseScreen-Token"), adapterID: body.AdapterID}
		_ = json.NewEncoder(writer).Encode(telemetryGPUTestStatus(body.AdapterID, false))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewAPI(server.URL)
	status, err := client.TelemetryGPU()
	if err != nil {
		t.Fatalf("读取监控显卡: %v", err)
	}
	if !status.Automatic || status.EffectiveAdapterName != "AMD Radeon RX 7900 XTX" || len(status.Adapters) != 2 {
		t.Fatalf("监控显卡状态不完整: %+v", status)
	}
	saved, err := client.SaveTelemetryGPUAdapter(` PCI\VEN_1002&DEV_744C `)
	if err != nil {
		t.Fatalf("保存监控显卡: %v", err)
	}
	select {
	case observed := <-saves:
		if observed.token != "telemetry-gpu-token" || observed.adapterID != `PCI\VEN_1002&DEV_744C` {
			t.Fatalf("监控 GPU 请求错误: %+v", observed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待监控 GPU 保存请求超时")
	}
	if saved.Automatic || saved.ConfiguredAdapterID != `PCI\VEN_1002&DEV_744C` {
		t.Fatalf("保存后状态错误: %+v", saved)
	}
}

func TestTelemetryGPUSettingsLoadsAndSavesAsynchronously(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"token": "async-token", "protocol_version": localapi.ProtocolVersion,
		})
	})
	mux.HandleFunc("GET /api/telemetry-gpu", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(telemetryGPUTestStatus(automaticTelemetryGPUOption, true))
	})
	mux.HandleFunc("PUT /api/telemetry-gpu", func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			AdapterID string `json:"adapter_id"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(writer).Encode(telemetryGPUTestStatus(body.AdapterID, false))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), uiUpdates: make(chan func(), 2)}
	model.loadTelemetryGPU(false)
	applyNextTelemetryGPUUpdate(t, model)
	if !model.telemetryGPUSettings.loaded || model.telemetryGPUSettings.draft != automaticTelemetryGPUOption {
		t.Fatalf("全局监控 GPU 未异步载入: %+v", model.telemetryGPUSettings)
	}

	model.saveTelemetryGPUSelection(`PCI\VEN_8086&DEV_7D67`)
	if !model.telemetryGPUSettings.saving || model.message != "正在切换全局 GPU 遥测来源…" {
		t.Fatalf("选择后没有立即反馈: saving=%v message=%q", model.telemetryGPUSettings.saving, model.message)
	}
	applyNextTelemetryGPUUpdate(t, model)
	if model.telemetryGPUSettings.saving || model.telemetryGPUSettings.draft != `PCI\VEN_8086&DEV_7D67` ||
		model.telemetryGPUSettings.status.EffectiveAdapterName != "Intel(R) Graphics" {
		t.Fatalf("全局监控 GPU 未切换: %+v", model.telemetryGPUSettings)
	}
	if model.gpuSettings.loaded || model.gpuSettings.draft != "" {
		t.Fatalf("监控 GPU 不应改动程序运算 GPU: %+v", model.gpuSettings)
	}
}

func TestTelemetryGPUOptionsAndGPUMetricDetection(t *testing.T) {
	status := telemetryGPUTestStatus(automaticTelemetryGPUOption, true)
	options := telemetryGPUAdapterOptions(status)
	if len(options) != 3 || options[0].id != automaticTelemetryGPUOption || options[1].label != "Intel(R) Graphics" {
		t.Fatalf("监控显卡选项错误: %+v", options)
	}
	if !validTelemetryGPUSelection(status, automaticTelemetryGPUOption) ||
		!validTelemetryGPUSelection(status, `pci\ven_1002&dev_744c`) ||
		validTelemetryGPUSelection(status, `PCI\VEN_FAKE`) {
		t.Fatal("监控显卡选项校验错误")
	}
	for _, metric := range []string{"Gpu_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp", "Gpu_Model"} {
		if !isGPUTelemetryMetric(metric) {
			t.Errorf("GPU 指标 %q 未识别", metric)
		}
	}
	if isGPUTelemetryMetric("Cpu_Freq") {
		t.Fatal("CPU 指标不应显示监控 GPU 选择器")
	}
	layer := &caseTheme.Layer{Options: map[string]any{"data_key": "GPU_Temp"}}
	if !layerUsesSingleGPUTelemetryMetric(layer, "Cpu_Freq") {
		t.Fatal("绑定 GPU 指标的进度条/仪表/折线应显示全局 GPU 选择")
	}
	textLayer := &caseTheme.Layer{Type: caseTheme.LayerText, Options: map[string]any{"data_key": "Gpu_Model"}}
	if !layerUsesSingleGPUTelemetryMetric(textLayer, "") {
		t.Fatal("绑定 GPU 指标的动态文本也应显示全局 GPU 选择")
	}
	for _, layerType := range []caseTheme.LayerType{
		caseTheme.LayerTelemetry, caseTheme.LayerText, caseTheme.LayerProgressBar,
		caseTheme.LayerGauge, caseTheme.LayerGradientGauge, caseTheme.LayerLineChart,
	} {
		if !layerSupportsGPUTelemetrySelection(layerType) {
			t.Errorf("可绑定 GPU 指标的组件没有统一选择器入口: %s", layerType)
		}
	}
	for _, layerType := range []caseTheme.LayerType{caseTheme.LayerClock, caseTheme.LayerShape, caseTheme.LayerAudioBars} {
		if layerSupportsGPUTelemetrySelection(layerType) {
			t.Errorf("不绑定 GPU 指标的组件不应显示监控显卡选择器: %s", layerType)
		}
	}
	state := telemetryGPUSettingsState{loaded: true, lastError: "枚举失败"}
	if state.editable() {
		t.Fatal("显卡枚举失败后不应继续使用过期列表保存")
	}
}

func applyNextTelemetryGPUUpdate(t *testing.T, model *gioEditor) {
	t.Helper()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(2 * time.Second):
		t.Fatal("等待监控 GPU 界面更新超时")
	}
}

func telemetryGPUTestStatus(configured string, automatic bool) TelemetryGPUStatus {
	status := TelemetryGPUStatus{
		ConfiguredAdapterID: configured,
		Automatic:           automatic,
		Adapters: []TelemetryGPUAdapter{
			{ID: `PCI\VEN_8086&DEV_7D67`, Name: "Intel(R) Graphics"},
			{ID: `PCI\VEN_1002&DEV_744C`, Name: "AMD Radeon RX 7900 XTX", Active: true, Primary: true},
		},
	}
	if automatic || strings.EqualFold(configured, automaticTelemetryGPUOption) {
		status.ConfiguredAdapterID = automaticTelemetryGPUOption
		status.EffectiveAdapterID = `PCI\VEN_1002&DEV_744C`
		status.EffectiveAdapterName = "AMD Radeon RX 7900 XTX"
		return status
	}
	status.EffectiveAdapterID = configured
	if strings.EqualFold(configured, `PCI\VEN_8086&DEV_7D67`) {
		status.ConfiguredAdapterName = "Intel(R) Graphics"
		status.EffectiveAdapterName = "Intel(R) Graphics"
	} else {
		status.ConfiguredAdapterName = "AMD Radeon RX 7900 XTX"
		status.EffectiveAdapterName = "AMD Radeon RX 7900 XTX"
	}
	return status
}
