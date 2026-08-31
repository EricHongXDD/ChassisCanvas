package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"casescreen/internal/config"
	"casescreen/internal/gpupreference"
)

type fakeGPUPreferenceManager struct {
	status          gpupreference.Status
	applyResult     gpupreference.ApplyResult
	statusErr       error
	applyErr        error
	applied         []gpupreference.Preference
	appliedAdapters []string
}

func (manager *fakeGPUPreferenceManager) Status() (gpupreference.Status, error) {
	return manager.status, manager.statusErr
}

func (manager *fakeGPUPreferenceManager) Apply(preference gpupreference.Preference) (gpupreference.ApplyResult, error) {
	manager.applied = append(manager.applied, preference)
	if manager.applyErr != nil {
		return gpupreference.ApplyResult{}, manager.applyErr
	}
	result := manager.applyResult
	if len(result.Status.Applications) == 0 {
		result.Status = testGPUPreferenceStatus(preference)
	}
	return result, nil
}

func (manager *fakeGPUPreferenceManager) ApplyAdapter(adapterID string) (gpupreference.ApplyResult, error) {
	manager.appliedAdapters = append(manager.appliedAdapters, adapterID)
	if manager.applyErr != nil {
		return gpupreference.ApplyResult{}, manager.applyErr
	}
	adapters := testGPUPreferenceStatus(gpupreference.SystemDefault).Adapters
	adapter, ok := gpupreference.FindAdapter(adapters, adapterID)
	if !ok {
		return gpupreference.ApplyResult{}, gpupreference.ErrAdapterNotFound
	}
	result := manager.applyResult
	if len(result.Status.Applications) == 0 {
		result.Status = testGPUPreferenceStatus(adapter.Preference)
	}
	result.Adapter = adapter
	return result, nil
}

func testGPUPreferenceStatus(preference gpupreference.Preference) gpupreference.Status {
	return gpupreference.Status{
		Supported: true, Effective: preference, Consistent: true,
		Applications: []gpupreference.ApplicationStatus{
			{Name: "CaseScreen.exe", Path: `C:\CaseScreen\CaseScreen.exe`, Preference: preference},
			{Name: "CaseScreenDesktop.exe", Path: `C:\CaseScreen\CaseScreenDesktop.exe`, Preference: preference},
		},
		Adapters: []gpupreference.AdapterSummary{
			{ID: `PCI\VEN_8086&DEV_7D67`, Name: "Intel(R) Graphics", Preference: gpupreference.PowerSaving, Active: true, Primary: true},
			{ID: `PCI\VEN_1002&DEV_744C`, Name: "AMD Radeon RX 7900 XTX", Preference: gpupreference.HighPerformance, Active: true},
		},
		RestartProcesses:     []string{"CaseScreen.exe", "CaseScreenDesktop.exe"},
		PowerSavingLabel:     "Intel(R) Graphics",
		HighPerformanceLabel: "AMD Radeon RX 7900 XTX",
		Notice:               gpupreference.Notice,
	}
}

func TestGetGPUPreferenceReportsConfiguredAndActualState(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeGPUPreferenceManager{status: testGPUPreferenceStatus(gpupreference.PowerSaving)}
	server.gpuPreferences = manager
	request := httptest.NewRequest(http.MethodGet, "/api/gpu-preference", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("读取状态失败: %d %s", response.Code, response.Body.String())
	}
	var body GPUPreferenceStatus
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Configured != gpupreference.SystemDefault || body.Effective != gpupreference.PowerSaving || !body.Supported || !body.Consistent {
		t.Fatalf("GPU 状态响应错误: %+v", body)
	}
	if body.RestartRequired || len(body.RestartProcesses) != 2 || !strings.Contains(body.Notice, "显卡名称") {
		t.Fatalf("GPU 生效说明不完整: %+v", body)
	}
	if !strings.Contains(body.PowerSavingLabel, "Intel") || !strings.Contains(body.HighPerformanceLabel, "AMD") {
		t.Fatalf("实体显卡标签没有传给客户端: %+v", body)
	}
}

func TestGetGPUPreferenceInfersAdapterForLegacyConfig(t *testing.T) {
	server, _ := newTestServer(t)
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.GPUPreference = string(gpupreference.HighPerformance)
		cfg.GPUAdapterID = ""
		cfg.GPUAdapterName = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	server.gpuPreferences = &fakeGPUPreferenceManager{status: testGPUPreferenceStatus(gpupreference.HighPerformance)}
	request := httptest.NewRequest(http.MethodGet, "/api/gpu-preference", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("读取旧版显卡设置失败: %d %s", response.Code, response.Body.String())
	}
	var body GPUPreferenceStatus
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ConfiguredAdapterID != `PCI\VEN_1002&DEV_744C` || body.ConfiguredAdapterName != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("旧版枚举未映射到检测显卡: %+v", body)
	}
}

func TestPutGPUPreferenceAppliesBothProcessesAndPersists(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeGPUPreferenceManager{applyResult: gpupreference.ApplyResult{
		Status: testGPUPreferenceStatus(gpupreference.HighPerformance), Changed: true,
	}}
	server.gpuPreferences = manager
	request := httptest.NewRequest(http.MethodPut, "/api/gpu-preference", strings.NewReader(`{"adapter_id":"PCI\\VEN_1002&DEV_744C"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存状态失败: %d %s", response.Code, response.Body.String())
	}
	if len(manager.appliedAdapters) != 1 || manager.appliedAdapters[0] != `PCI\VEN_1002&DEV_744C` {
		t.Fatalf("没有按真实显卡 ID 应用首选项: %+v", manager.appliedAdapters)
	}
	cfg := server.store.Get()
	if cfg.GPUPreference != string(gpupreference.HighPerformance) || cfg.GPUAdapterID != `PCI\VEN_1002&DEV_744C` || cfg.GPUAdapterName != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("显卡选择未完整持久化: %+v", cfg)
	}
	var body GPUPreferenceStatus
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.RestartRequired || body.Effective != gpupreference.HighPerformance || body.ConfiguredAdapterName != "AMD Radeon RX 7900 XTX" || len(body.Applications) != 2 {
		t.Fatalf("保存响应错误: %+v", body)
	}
}

func TestGPUPreferenceSelectionSurvivesStoreReopen(t *testing.T) {
	server, controller := newTestServer(t)
	server.gpuPreferences = &fakeGPUPreferenceManager{applyResult: gpupreference.ApplyResult{
		Status: testGPUPreferenceStatus(gpupreference.PowerSaving), Changed: true,
	}}
	request := httptest.NewRequest(http.MethodPut, "/api/gpu-preference", strings.NewReader(`{"adapter_id":"PCI\\VEN_8086&DEV_7D67"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存显卡选择失败: %d %s", response.Code, response.Body.String())
	}

	reopened, err := config.Open(server.store.Path())
	if err != nil {
		t.Fatalf("重开配置失败: %v", err)
	}
	restarted, err := New(controller, reopened)
	if err != nil {
		t.Fatalf("重建 API 服务失败: %v", err)
	}
	restarted.gpuPreferences = &fakeGPUPreferenceManager{status: testGPUPreferenceStatus(gpupreference.PowerSaving)}
	request = httptest.NewRequest(http.MethodGet, "/api/gpu-preference", nil)
	response = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("重启后读取显卡选择失败: %d %s", response.Code, response.Body.String())
	}
	var body GPUPreferenceStatus
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ConfiguredAdapterID != `PCI\VEN_8086&DEV_7D67` || body.ConfiguredAdapterName != "Intel(R) Graphics" || body.Configured != gpupreference.PowerSaving {
		t.Fatalf("重启后显卡选择丢失: %+v", body)
	}
}

func TestPutGPUPreferenceSystemAutomaticClearsStoredAdapter(t *testing.T) {
	server, _ := newTestServer(t)
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.GPUPreference = string(gpupreference.HighPerformance)
		cfg.GPUAdapterID = `PCI\VEN_1002&DEV_744C`
		cfg.GPUAdapterName = "AMD Radeon RX 7900 XTX"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeGPUPreferenceManager{applyResult: gpupreference.ApplyResult{Status: testGPUPreferenceStatus(gpupreference.SystemDefault)}}
	server.gpuPreferences = manager
	request := httptest.NewRequest(http.MethodPut, "/api/gpu-preference", strings.NewReader(`{"adapter_id":"system"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存系统自动失败: %d %s", response.Code, response.Body.String())
	}
	if len(manager.applied) != 1 || manager.applied[0] != gpupreference.SystemDefault {
		t.Fatalf("系统自动未写入两个程序: %+v", manager.applied)
	}
	cfg := server.store.Get()
	if cfg.GPUPreference != string(gpupreference.SystemDefault) || cfg.GPUAdapterID != "" || cfg.GPUAdapterName != "" {
		t.Fatalf("系统自动未清理适配器选择: %+v", cfg)
	}
}

func TestPutGPUPreferenceRejectsUnknownAdapter(t *testing.T) {
	server, _ := newTestServer(t)
	server.gpuPreferences = &fakeGPUPreferenceManager{}
	request := httptest.NewRequest(http.MethodPut, "/api/gpu-preference", strings.NewReader(`{"adapter_id":"PCI\\VEN_FAKE"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未知显卡状态码 = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestPutGPUPreferenceRejectsRegistryFailureWithoutChangingConfig(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeGPUPreferenceManager{applyErr: errors.New("拒绝访问")}
	server.gpuPreferences = manager
	request := httptest.NewRequest(http.MethodPut, "/api/gpu-preference", strings.NewReader(`{"preference":"power_saving"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("注册表失败状态码 = %d, body=%s", response.Code, response.Body.String())
	}
	if got := server.store.Get().GPUPreference; got != string(gpupreference.SystemDefault) {
		t.Fatalf("失败后不应改变配置: %q", got)
	}
}

func TestPutConfigAppliesChangedGPUPreferenceAndRequiresRestart(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeGPUPreferenceManager{}
	server.gpuPreferences = manager
	cfg := server.store.Get()
	cfg.GPUPreference = string(gpupreference.PowerSaving)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存配置失败: %d %s", response.Code, response.Body.String())
	}
	if len(manager.applied) != 1 || manager.applied[0] != gpupreference.PowerSaving {
		t.Fatalf("配置保存没有应用 GPU 首选项: %+v", manager.applied)
	}
	var body struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.RestartRequired {
		t.Fatal("GPU 首选项变化必须提示重启")
	}
}

func TestPutConfigAppliesAndCanonicalizesDetectedAdapter(t *testing.T) {
	server, _ := newTestServer(t)
	manager := &fakeGPUPreferenceManager{applyResult: gpupreference.ApplyResult{
		Status: testGPUPreferenceStatus(gpupreference.HighPerformance), Changed: true,
	}}
	server.gpuPreferences = manager
	cfg := server.store.Get()
	cfg.GPUPreference = string(gpupreference.HighPerformance)
	cfg.GPUAdapterID = `pci\ven_1002&dev_744c`
	cfg.GPUAdapterName = "过期名称"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("按显卡保存配置失败: %d %s", response.Code, response.Body.String())
	}
	if len(manager.appliedAdapters) != 1 || manager.appliedAdapters[0] != `pci\ven_1002&dev_744c` {
		t.Fatalf("配置 API 未按适配器 ID 应用: %+v", manager.appliedAdapters)
	}
	saved := server.store.Get()
	if saved.GPUAdapterID != `PCI\VEN_1002&DEV_744C` || saved.GPUAdapterName != "AMD Radeon RX 7900 XTX" || saved.GPUPreference != string(gpupreference.HighPerformance) {
		t.Fatalf("配置 API 未保存规范化显卡选择: %+v", saved)
	}
}

func TestPutGPUPreferenceRequiresTokenAndKnownValue(t *testing.T) {
	server, _ := newTestServer(t)
	server.gpuPreferences = &fakeGPUPreferenceManager{}
	request := httptest.NewRequest(http.MethodPut, "/api/gpu-preference", strings.NewReader(`{"preference":"power_saving"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("无令牌状态码 = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/gpu-preference", strings.NewReader(`{"preference":"force_discrete"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("无效值状态码 = %d, body=%s", response.Code, response.Body.String())
	}
}
