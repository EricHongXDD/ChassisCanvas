package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/device"
	"casescreen/internal/discovery"
	"casescreen/internal/service"
	"casescreen/internal/theme"
)

type fakeController struct {
	enabled         bool
	audioEnabled    bool
	lyricsEnabled   bool
	lyricsProvider  string
	lyricsPath      string
	interval        time.Duration
	renderInterval  time.Duration
	jpegQuality     int
	ffmpegPath      string
	appliedTheme    string
	appliedTarget   string
	appliedManifest theme.Manifest
	applyErr        error
	applyCount      int
	powerTarget     string
	poweredOff      bool
	powerResults    map[string]error
}

type blockingPowerController struct {
	fakeController

	mu           sync.Mutex
	calls        []bool
	callStarted  chan bool
	firstRelease chan struct{}
}

type blockingThemeController struct {
	fakeController

	mu            sync.Mutex
	calls         []string
	hardwareTheme string
	applyStarted  chan string
	firstRelease  chan struct{}
}

func (controller *blockingThemeController) ApplyThemeTo(
	_ context.Context, deviceID string, manifest theme.Manifest,
) map[string]error {
	controller.mu.Lock()
	controller.calls = append(controller.calls, manifest.ID)
	callIndex := len(controller.calls)
	// 模拟硬件命令已经生效，但 API 调用尚未返回。旧实现允许后一个
	// 请求此时覆盖硬件，再由前一个请求把旧 JSON 写回磁盘。
	controller.hardwareTheme = manifest.ID
	controller.mu.Unlock()
	controller.applyStarted <- manifest.ID
	if callIndex == 1 {
		<-controller.firstRelease
	}
	return map[string]error{deviceID: nil}
}

func (controller *blockingPowerController) SetPoweredOff(_ context.Context, target string, poweredOff bool) map[string]error {
	controller.mu.Lock()
	controller.calls = append(controller.calls, poweredOff)
	callIndex := len(controller.calls)
	controller.mu.Unlock()
	controller.callStarted <- poweredOff
	if callIndex == 1 {
		<-controller.firstRelease
	}
	return map[string]error{target: nil}
}

func TestDetachedOperationContextIgnoresClientButStopsWithService(t *testing.T) {
	serviceContext, stopService := context.WithCancel(context.Background())
	server := &Server{operationContext: serviceContext}
	requestContext, stopRequest := context.WithCancel(context.Background())
	operationContext, stopOperation := server.detachedOperationContext(requestContext, time.Second)
	defer stopOperation()

	stopRequest()
	select {
	case <-operationContext.Done():
		t.Fatalf("客户端断开不应取消已经接受的设备操作: %v", operationContext.Err())
	case <-time.After(20 * time.Millisecond):
	}
	stopService()
	select {
	case <-operationContext.Done():
		if !errors.Is(operationContext.Err(), context.Canceled) {
			t.Fatalf("服务退出返回了错误原因: %v", operationContext.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("服务退出没有取消长主题操作")
	}
}

func (controller *fakeController) Status() service.Status {
	return service.Status{
		TelemetryEnabled: controller.enabled, TelemetryInterval: controller.interval.Milliseconds(),
		Devices: []service.DeviceStatus{
			{Info: device.Info{ID: "waveshare-7", Resolution: [2]int{1024, 600}}, BrightnessSupported: true, DefaultBrightness: 255, CurrentBrightness: 255},
			{Info: device.Info{ID: "waveshare-8.8", Resolution: [2]int{480, 1920}}, BrightnessSupported: true, DefaultBrightness: 255, CurrentBrightness: 255},
			{Info: device.Info{ID: "coolermaster-atmos-v2", Resolution: [2]int{480, 480}}, BrightnessSupported: true, DefaultBrightness: 80, CurrentBrightness: 80},
		},
	}
}

func (controller *fakeController) SetTelemetryEnabled(enabled bool) {
	controller.enabled = enabled
}

func (controller *fakeController) SetAudioEnabled(enabled bool) {
	controller.audioEnabled = enabled
}

func (controller *fakeController) SetLyricsEnabled(enabled bool) {
	controller.lyricsEnabled = enabled
}

func (controller *fakeController) SetLyricsProviderPreference(provider string) {
	controller.lyricsProvider = provider
}

func (controller *fakeController) SetLyricsPath(path string) {
	controller.lyricsPath = path
}

func (controller *fakeController) SetInterval(interval time.Duration) {
	controller.interval = interval
}

func (controller *fakeController) SetRenderInterval(interval time.Duration) {
	controller.renderInterval = interval
}

func (controller *fakeController) SetJPEGQuality(quality int) {
	controller.jpegQuality = quality
}

func (controller *fakeController) SetFFmpegPath(path string) {
	controller.ffmpegPath = path
}

func (controller *fakeController) ApplyThemeTo(_ context.Context, deviceID string, manifest theme.Manifest) map[string]error {
	controller.applyCount++
	controller.appliedTheme = manifest.ID
	controller.appliedTarget = deviceID
	controller.appliedManifest = manifest
	return map[string]error{deviceID: controller.applyErr}
}

func (controller *fakeController) SetPoweredOff(_ context.Context, target string, poweredOff bool) map[string]error {
	controller.powerTarget = target
	controller.poweredOff = poweredOff
	if controller.powerResults != nil {
		return controller.powerResults
	}
	return map[string]error{target: nil}
}

func TestMutationRequiresToken(t *testing.T) {
	server, _ := newTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/telemetry", strings.NewReader(`{"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("无令牌请求状态码=%d", response.Code)
	}
}

func TestPowerSupportsSingleAndAllTargetsWithPerDeviceResults(t *testing.T) {
	server, controller := newTestServer(t)
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.Devices = []config.Device{
			{ID: "waveshare-7", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
			{ID: "waveshare-8.8", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 480, Height: 1920},
			{ID: "coolermaster-atmos-v2", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: 4},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controller.powerResults = map[string]error{
		"waveshare-7":           nil,
		"waveshare-8.8":         nil,
		"coolermaster-atmos-v2": errors.New("HID 超时"),
	}
	request := httptest.NewRequest(http.MethodPut, "/api/power", strings.NewReader(`{"target":"all","powered_off":true}`))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusMultiStatus {
		t.Fatalf("部分失败状态码=%d body=%s", response.Code, response.Body.String())
	}
	if controller.powerTarget != "all" || !controller.poweredOff {
		t.Fatalf("控制器未收到全部关屏: target=%q off=%v", controller.powerTarget, controller.poweredOff)
	}
	var body struct {
		Target     string            `json:"target"`
		PoweredOff bool              `json:"powered_off"`
		Results    map[string]string `json:"device_results"`
		Successes  int               `json:"successes"`
		Failures   int               `json:"failures"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Target != "all" || !body.PoweredOff || body.Successes != 2 || body.Failures != 1 ||
		body.Results["waveshare-7"] != "ok" || body.Results["coolermaster-atmos-v2"] != "HID 超时" {
		t.Fatalf("关屏结果不完整: %+v", body)
	}
	for _, configured := range server.store.Get().Devices {
		if !configured.PoweredOff {
			t.Fatalf("硬件部分失败后未保留 %s 的关屏意图", configured.ID)
		}
	}
	controller.powerResults = map[string]error{"waveshare-7": nil}
	request = httptest.NewRequest(http.MethodPut, "/api/power", strings.NewReader(`{"target":"waveshare-7","powered_off":false}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("单屏恢复失败: %d %s", response.Code, response.Body.String())
	}
	for _, configured := range server.store.Get().Devices {
		wantOff := configured.ID != "waveshare-7"
		if configured.PoweredOff != wantOff {
			t.Fatalf("单屏恢复误改其它设备: %+v", server.store.Get().Devices)
		}
	}
}

func TestPowerRequiresAuthorizationAndTarget(t *testing.T) {
	server, controller := newTestServer(t)
	request := httptest.NewRequest(http.MethodPut, "/api/power", strings.NewReader(`{"target":"waveshare-7","powered_off":false}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("无令牌关屏未被拒绝: %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/power", strings.NewReader(`{"target":"  ","powered_off":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || controller.powerTarget != "" {
		t.Fatalf("空目标处理错误: code=%d target=%q", response.Code, controller.powerTarget)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/power", strings.NewReader(`{"target":"waveshare-7"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "powered_off") {
		t.Fatalf("缺少 powered_off 未被拒绝: %d %s", response.Code, response.Body.String())
	}
}

func TestPutConfigDoesNotOverwriteNewerPowerIntentFromStaleSnapshot(t *testing.T) {
	server, controller := newTestServer(t)
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.Devices = []config.Device{{
			ID: "waveshare-7", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600,
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stale := server.store.Get()
	controller.powerResults = map[string]error{"waveshare-7": nil}
	powerRequest := httptest.NewRequest(http.MethodPut, "/api/power", strings.NewReader(`{"target":"waveshare-7","powered_off":true}`))
	powerRequest.Header.Set("Content-Type", "application/json")
	powerRequest.Header.Set("X-CaseScreen-Token", server.token)
	powerResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(powerResponse, powerRequest)
	if powerResponse.Code != http.StatusOK {
		t.Fatalf("关屏请求失败: %d %s", powerResponse.Code, powerResponse.Body.String())
	}

	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	configRequest := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(data))
	configRequest.Header.Set("Content-Type", "application/json")
	configRequest.Header.Set("X-CaseScreen-Token", server.token)
	configResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(configResponse, configRequest)
	if configResponse.Code != http.StatusOK {
		t.Fatalf("回写设置失败: %d %s", configResponse.Code, configResponse.Body.String())
	}
	configured := server.store.Get().Devices
	if len(configured) != 1 || !configured[0].PoweredOff {
		t.Fatalf("陈旧设置覆盖了最新关屏意图: %+v", configured)
	}
	var payload struct {
		Config config.Config `json:"config"`
	}
	if err := json.Unmarshal(configResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析设置响应失败: %v", err)
	}
	if len(payload.Config.Devices) != 1 || !payload.Config.Devices[0].PoweredOff {
		t.Fatalf("设置响应返回了陈旧关屏意图: %+v", payload.Config.Devices)
	}
}

func TestConcurrentOppositePowerRequestsKeepConfigAndRuntimeOrdered(t *testing.T) {
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) error {
		cfg.Devices = []config.Device{{
			ID: "waveshare-7", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600,
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controller := &blockingPowerController{
		fakeController: fakeController{enabled: true, interval: time.Second},
		callStarted:    make(chan bool, 2),
		firstRelease:   make(chan struct{}),
	}
	server, err := New(controller, store)
	if err != nil {
		t.Fatal(err)
	}

	requestPower := func(poweredOff bool) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			body := `{"target":"waveshare-7","powered_off":false}`
			if poweredOff {
				body = `{"target":"waveshare-7","powered_off":true}`
			}
			request := httptest.NewRequest(http.MethodPut, "/api/power", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CaseScreen-Token", server.token)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			done <- response
		}()
		return done
	}

	offDone := requestPower(true)
	select {
	case poweredOff := <-controller.callStarted:
		if !poweredOff {
			t.Fatal("首个运行时命令不是关屏")
		}
	case <-time.After(time.Second):
		t.Fatal("首个关屏请求未进入控制器")
	}
	onDone := requestPower(false)
	select {
	case poweredOff := <-controller.callStarted:
		close(controller.firstRelease)
		<-offDone
		<-onDone
		t.Fatalf("恢复请求越过了仍在执行的关屏事务: powered_off=%v", poweredOff)
	case <-time.After(30 * time.Millisecond):
	}
	close(controller.firstRelease)
	if response := <-offDone; response.Code != http.StatusOK {
		t.Fatalf("关屏请求失败: %d %s", response.Code, response.Body.String())
	}
	select {
	case poweredOff := <-controller.callStarted:
		if poweredOff {
			t.Fatal("第二个运行时命令不是恢复开屏")
		}
	case <-time.After(time.Second):
		t.Fatal("恢复请求未在关屏完成后进入控制器")
	}
	if response := <-onDone; response.Code != http.StatusOK {
		t.Fatalf("恢复请求失败: %d %s", response.Code, response.Body.String())
	}
	configured := store.Get().Devices
	if len(configured) != 1 || configured[0].PoweredOff {
		t.Fatalf("最终配置没有保留最后一次恢复意图: %+v", configured)
	}
	controller.mu.Lock()
	calls := append([]bool(nil), controller.calls...)
	controller.mu.Unlock()
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("运行时电源顺序错误: %v", calls)
	}
}

func TestDesktopSessionReturnsLocalToken(t *testing.T) {
	server, _ := newTestServer(t)
	server.SetApplicationVersion("test-version")
	server.SetProcessLifecycle(ProcessLifecycle{Mode: "desktop", ProcessID: 42, OwnerProcessID: 7, OwnerProcessStarted: 99}, func() {})
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("桌面会话状态码=%d", response.Code)
	}
	var body struct {
		Token              string `json:"token"`
		ProtocolVersion    int    `json:"protocol_version"`
		ApplicationVersion string `json:"application_version"`
		LaunchMode         string `json:"launch_mode"`
		ProcessID          int    `json:"process_id"`
		OwnerProcessID     int    `json:"owner_process_id"`
		OwnerStarted       uint64 `json:"owner_process_started"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Token == "" || body.Token != server.token {
		t.Fatalf("桌面会话令牌无效: %q", body.Token)
	}
	if body.ProtocolVersion != ProtocolVersion {
		t.Fatalf("协议版本 = %d, want %d", body.ProtocolVersion, ProtocolVersion)
	}
	if body.ApplicationVersion != "test-version" {
		t.Fatalf("应用版本 = %q, want test-version", body.ApplicationVersion)
	}
	if body.LaunchMode != "desktop" || body.ProcessID != 42 || body.OwnerProcessID != 7 || body.OwnerStarted != 99 {
		t.Fatalf("进程归属信息无效: %+v", body)
	}
}

func TestDesktopLifecycleShutdownRequiresManagedModeOrExplicitTakeover(t *testing.T) {
	server, _ := newTestServer(t)
	shutdown := make(chan struct{}, 1)
	server.SetProcessLifecycle(ProcessLifecycle{Mode: "desktop", ProcessID: 42}, func() { shutdown <- struct{}{} })
	request := httptest.NewRequest(http.MethodPost, "/api/lifecycle/shutdown", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("桌面托管退出状态码=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("桌面托管退出没有取消服务")
	}

	server.SetProcessLifecycle(ProcessLifecycle{Mode: "tray", ProcessID: 42}, func() { shutdown <- struct{}{} })
	request = httptest.NewRequest(http.MethodPost, "/api/lifecycle/shutdown", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("手动托盘服务退出状态码=%d，want 409", response.Code)
	}
	select {
	case <-shutdown:
		t.Fatal("桌面接口错误结束了手动托盘服务")
	default:
	}

	request = httptest.NewRequest(http.MethodPost, "/api/lifecycle/shutdown", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	request.Header.Set(DesktopTakeoverHeader, DesktopTakeoverValue)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("同版桌面接管托盘服务状态码=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("同版桌面接管没有优雅取消托盘服务")
	}
}

func TestDiscoveryReturnsConfigReadyDevices(t *testing.T) {
	server, _ := newTestServer(t)
	server.discover = func() []discovery.Device {
		return []discovery.Device{{
			ID: "waveshare-com3", Name: "微雪 USB Monitor（COM3）", Kind: "waveshare",
			Enabled: true, Port: "COM3", Width: 1024, Height: 600,
		}}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/discovery", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"port":"COM3"`) {
		t.Fatalf("设备扫描响应无效: %d %s", response.Code, response.Body.String())
	}
}

func TestDiscoveryPreservesConfiguredLogicalIDsAndSlots(t *testing.T) {
	server, _ := newTestServer(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: 4},
	}
	if err := server.store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	server.discover = func() []discovery.Device {
		slot := 1
		return []discovery.Device{
			{ID: "waveshare-com3", Name: "微雪 USB Monitor（COM3）", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
			{ID: "coolermaster-atmos-v2", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: &slot},
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/discovery", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"waveshare-7"`) ||
		!strings.Contains(response.Body.String(), `"slot":4`) || strings.Contains(response.Body.String(), `"id":"waveshare-com3"`) {
		t.Fatalf("手动扫描改变了逻辑 ID 或酷冷槽位: %d %s", response.Code, response.Body.String())
	}
}

func TestPutConfigUpdatesControllerAndDisk(t *testing.T) {
	server, controller := newTestServer(t)
	cfg := config.Default()
	cfg.TelemetryEnabled = false
	cfg.AudioEnabled = false
	cfg.LyricsEnabled = false
	cfg.FFmpegPath = ""
	cfg.TelemetryIntervalM = 1250
	body, _ := json.Marshal(cfg)
	request := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", response.Code, response.Body.String())
	}
	if controller.enabled || controller.interval != 1250*time.Millisecond {
		t.Fatalf("运行时配置未更新: %+v", controller)
	}
	if controller.audioEnabled || controller.lyricsEnabled {
		t.Fatalf("音频或歌词没有即时关闭: %+v", controller)
	}
	if controller.ffmpegPath != "" {
		t.Fatalf("清空 FFmpeg 路径没有传给运行时: %q", controller.ffmpegPath)
	}
}

func TestPutConfigFromOldClientPreservesWaveshareLibrary(t *testing.T) {
	server, _ := newTestServer(t)
	stored := server.store.Get()
	stored.WaveshareTemplates = map[string][]config.WaveshareTemplate{
		"waveshare-7": {{ID: "saved", Name: "已保存", ManifestPath: "saved.json"}},
	}
	stored.WavesharePlaylists = map[string]config.WavesharePlaylist{
		"waveshare-7": {Enabled: true, Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "saved", DurationSeconds: 10}}},
	}
	if err := server.store.Set(stored); err != nil {
		t.Fatal(err)
	}
	legacy := config.Default()
	body, _ := json.Marshal(legacy)
	request := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("旧客户端保存设置失败: %d %s", response.Code, response.Body.String())
	}
	actual := server.store.Get()
	if len(actual.WaveshareTemplates["waveshare-7"]) != 1 || !actual.WavesharePlaylists["waveshare-7"].Enabled {
		t.Fatalf("旧客户端意外清空微雪模板库: %+v", actual)
	}
}

func TestPutConfigFromStaleClientPreservesManagedThemeState(t *testing.T) {
	server, _ := newTestServer(t)
	initial := server.store.Get()
	initial.DeviceThemePaths = map[string]string{"waveshare-7": "old.json"}
	initial.WaveshareTemplates = map[string][]config.WaveshareTemplate{
		"waveshare-7": {{ID: "old", Name: "旧模板", ManifestPath: "old.json"}},
	}
	initial.WavesharePlaylists = map[string]config.WavesharePlaylist{
		"waveshare-7": {Enabled: true, Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "old", DurationSeconds: 10}}},
	}
	if err := server.store.Set(initial); err != nil {
		t.Fatal(err)
	}
	stale := server.store.Get()
	latest := server.store.Get()
	latest.DeviceThemePaths["waveshare-7"] = "latest.json"
	latest.WaveshareTemplates["waveshare-7"] = append(latest.WaveshareTemplates["waveshare-7"], config.WaveshareTemplate{
		ID: "latest", Name: "最新模板", ManifestPath: "latest.json",
	})
	latest.WavesharePlaylists["waveshare-7"] = config.WavesharePlaylist{
		Enabled: true, Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "latest", DurationSeconds: 20}},
	}
	if err := server.store.Set(latest); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(stale)
	request := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存过期设置快照失败: %d %s", response.Code, response.Body.String())
	}
	actual := server.store.Get()
	if actual.DeviceThemePaths["waveshare-7"] != "latest.json" || len(actual.WaveshareTemplates["waveshare-7"]) != 2 {
		t.Fatalf("过期设置快照覆盖了最新模板状态: %+v", actual)
	}
	playlist := actual.WavesharePlaylists["waveshare-7"]
	if len(playlist.Entries) != 1 || playlist.Entries[0].TemplateID != "latest" {
		t.Fatalf("过期设置快照覆盖了最新轮播: %+v", playlist)
	}
}

func TestPutThemeRejectsMissingTarget(t *testing.T) {
	server, controller := newTestServer(t)
	manifest := theme.Manifest{
		ID: "api-theme", Name: "API 主题", Width: 320, Height: 180,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	body, _ := json.Marshal(manifest)
	request := httptest.NewRequest(http.MethodPut, "/api/theme", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || controller.appliedTheme != "" {
		t.Fatalf("未指定副屏时不应应用主题: %d %q %s", response.Code, controller.appliedTheme, response.Body.String())
	}
}

func TestPutThemeCanTargetOneDevice(t *testing.T) {
	server, controller := newTestServer(t)
	manifest := theme.Manifest{
		ID: "target-theme", Name: "目标主题", Width: 320, Height: 180,
		Layers: []theme.Layer{{ID: "background", Type: theme.LayerBackgroundImage, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1}},
	}
	body, _ := json.Marshal(manifest)
	request := httptest.NewRequest(http.MethodPut, "/api/theme?target=coolermaster-atmos-v2", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || controller.appliedTarget != "coolermaster-atmos-v2" {
		t.Fatalf("目标主题没有按设备应用: %d %q %s", response.Code, controller.appliedTarget, response.Body.String())
	}
	if server.store.Get().DeviceThemePaths["coolermaster-atmos-v2"] == "" {
		t.Fatal("目标主题路径没有保存")
	}
	if controller.appliedManifest.Width != 480 || controller.appliedManifest.Height != 480 {
		t.Fatalf("主题没有强制使用设备分辨率：%dx%d", controller.appliedManifest.Width, controller.appliedManifest.Height)
	}
	canvas, picture := controller.appliedManifest.Layers[0], controller.appliedManifest.Layers[1]
	if canvas.Type != theme.LayerCanvas || canvas.Bounds != (theme.Rect{Width: 1, Height: 1}) {
		t.Fatalf("没有迁移为画布背景：%+v", canvas)
	}
	if picture.Type != theme.LayerImage || picture.Bounds != (theme.Rect{Width: 1, Height: 1}) {
		t.Fatalf("旧图片图层迁移错误：%+v", picture)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/theme?target=coolermaster-atmos-v2", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"target-theme"`) {
		t.Fatalf("设备主题读取失败: %d %s", response.Code, response.Body.String())
	}
}

func TestPutThemeSerializesApplyFileAndConfigPerTarget(t *testing.T) {
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	controller := &blockingThemeController{
		fakeController: fakeController{enabled: true, interval: time.Second},
		applyStarted:   make(chan string, 2),
		firstRelease:   make(chan struct{}),
	}
	server, err := New(controller, store)
	if err != nil {
		t.Fatal(err)
	}
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		return manifest, nil
	}
	requestBody := func(id string) string {
		body, marshalErr := json.Marshal(theme.Manifest{
			ID: id, Name: id, Layers: []theme.Layer{{
				ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			}},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return string(body)
	}
	firstBody := requestBody("theme-a")
	secondBody := requestBody("theme-b")

	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(firstResponse, authorizedRequest(
			server, http.MethodPut, "/api/theme?target=coolermaster-atmos-v2", firstBody,
		))
		close(firstDone)
	}()
	if started := <-controller.applyStarted; started != "theme-a" {
		t.Fatalf("第一个请求顺序错误: %s", started)
	}

	secondResponse := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(secondResponse, authorizedRequest(
			server, http.MethodPut, "/api/theme?target=coolermaster-atmos-v2", secondBody,
		))
		close(secondDone)
	}()
	select {
	case started := <-controller.applyStarted:
		t.Fatalf("第一个请求尚未完成时第二个请求已进入硬件应用: %s", started)
	case <-secondDone:
		t.Fatal("第一个请求尚未完成时第二个请求已返回")
	case <-time.After(40 * time.Millisecond):
	}
	close(controller.firstRelease)
	<-firstDone
	if started := <-controller.applyStarted; started != "theme-b" {
		t.Fatalf("第二个请求顺序错误: %s", started)
	}
	<-secondDone
	if firstResponse.Code != http.StatusOK || secondResponse.Code != http.StatusOK {
		t.Fatalf("并发主题响应失败: first=%d %s second=%d %s",
			firstResponse.Code, firstResponse.Body.String(), secondResponse.Code, secondResponse.Body.String())
	}
	controller.mu.Lock()
	calls := append([]string(nil), controller.calls...)
	hardwareTheme := controller.hardwareTheme
	controller.mu.Unlock()
	if len(calls) != 2 || calls[0] != "theme-a" || calls[1] != "theme-b" || hardwareTheme != "theme-b" {
		t.Fatalf("同目标事务顺序错误: calls=%v hardware=%s", calls, hardwareTheme)
	}
	path := store.Get().DeviceThemePaths["coolermaster-atmos-v2"]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted theme.Manifest
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("最终主题 JSON 损坏: %v", err)
	}
	if persisted.ID != "theme-b" {
		t.Fatalf("硬件与磁盘最终主题不一致: hardware=%s disk=%s", hardwareTheme, persisted.ID)
	}
}

func TestPutThemeDifferentTargetsRemainConcurrent(t *testing.T) {
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	controller := &blockingThemeController{
		fakeController: fakeController{enabled: true, interval: time.Second},
		applyStarted:   make(chan string, 2),
		firstRelease:   make(chan struct{}),
	}
	server, err := New(controller, store)
	if err != nil {
		t.Fatal(err)
	}
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		return manifest, nil
	}
	body := func(id string) string {
		data, marshalErr := json.Marshal(theme.Manifest{
			ID: id, Name: id, Layers: []theme.Layer{{
				ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			}},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return string(data)
	}
	firstBody, secondBody := body("cooler-theme"), body("portrait-theme")
	firstDone := make(chan struct{})
	go func() {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authorizedRequest(
			server, http.MethodPut, "/api/theme?target=coolermaster-atmos-v2", firstBody,
		))
		if response.Code != http.StatusOK {
			t.Errorf("第一目标响应失败: %d %s", response.Code, response.Body.String())
		}
		close(firstDone)
	}()
	if started := <-controller.applyStarted; started != "cooler-theme" {
		t.Fatalf("第一目标顺序错误: %s", started)
	}
	secondDone := make(chan struct{})
	go func() {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authorizedRequest(
			server, http.MethodPut, "/api/theme?target=waveshare-8.8", secondBody,
		))
		if response.Code != http.StatusOK {
			t.Errorf("第二目标响应失败: %d %s", response.Code, response.Body.String())
		}
		close(secondDone)
	}()
	select {
	case started := <-controller.applyStarted:
		if started != "portrait-theme" {
			t.Fatalf("第二目标顺序错误: %s", started)
		}
	case <-time.After(time.Second):
		t.Fatal("不同目标被同一把主题事务锁全局串行")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("第二目标未在第一目标阻塞期间独立完成")
	}
	close(controller.firstRelease)
	<-firstDone
}

func TestPutThemeAtomicReplaceFailureKeepsPreviousTheme(t *testing.T) {
	server, controller := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		return manifest, nil
	}
	put := func(id string) *httptest.ResponseRecorder {
		body, err := json.Marshal(theme.Manifest{
			ID: id, Name: id, Layers: []theme.Layer{{
				ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authorizedRequest(
			server, http.MethodPut, "/api/theme?target=coolermaster-atmos-v2", string(body),
		))
		return response
	}
	if response := put("theme-old"); response.Code != http.StatusOK {
		t.Fatalf("建立旧主题失败: %d %s", response.Code, response.Body.String())
	}
	path := server.store.Get().DeviceThemePaths["coolermaster-atmos-v2"]
	previous, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	server.replaceThemeFile = func(string, string) error { return errors.New("模拟原子替换失败") }
	response := put("theme-new")
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "模拟原子替换失败") {
		t.Fatalf("主题写失败响应无效: %d %s", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, previous) {
		t.Fatal("原子替换失败后旧主题文件被破坏")
	}
	if controller.appliedTheme != "theme-old" || controller.applyCount != 1 {
		t.Fatalf("写盘失败后仍改动了硬件主题: applied=%s count=%d", controller.appliedTheme, controller.applyCount)
	}
	temporaryFiles, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".theme-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryFiles) != 0 {
		t.Fatalf("原子替换失败遗留临时文件: %v", temporaryFiles)
	}
}

func TestPutThemeQueuesPersistedThemeWhenDeviceApplyFails(t *testing.T) {
	server, controller := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		return manifest, nil
	}
	controller.applyErr = errors.New("微雪屏幕未连接")
	manifest := theme.Manifest{
		ID: "queued-theme", Name: "待重连主题", Width: 1024, Height: 600,
		Layers: []theme.Layer{{
			ID: "canvas", Type: theme.LayerCanvas,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	body, _ := json.Marshal(manifest)
	request := httptest.NewRequest(http.MethodPut, "/api/theme?target=waveshare-7", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("设备离线但主题已保存时状态码=%d，want 202；body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Queued  bool              `json:"queued"`
		Warning string            `json:"warning"`
		Results map[string]string `json:"device_results"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Queued || !strings.Contains(result.Warning, "已保存") ||
		!strings.Contains(result.Results["waveshare-7"], "未连接") {
		t.Fatalf("排队响应无效: %+v", result)
	}
	themePath := server.store.Get().DeviceThemePaths["waveshare-7"]
	if themePath == "" {
		t.Fatal("设备应用失败后没有持久化待重连主题")
	}
	if _, err := os.Stat(themePath); err != nil {
		t.Fatalf("待重连主题清单不存在: %v", err)
	}
}

func TestUploadedImagePreviewIsServedAsPNG(t *testing.T) {
	server, _ := newTestServer(t)
	path := filepath.Join(t.TempDir(), "background.webp")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			imageData.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	if err := png.Encode(file, imageData); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	server.resources["uploaded"] = theme.Resource{ID: "uploaded", LocalPath: path}
	request := httptest.NewRequest(http.MethodGet, "/api/theme/resource?id=uploaded&preview=1", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("预览响应无效: %d %s", response.Code, response.Header().Get("Content-Type"))
	}
	decoded, _, err := image.Decode(bytes.NewReader(response.Body.Bytes()))
	if err != nil {
		t.Fatalf("预览不是有效图片: %v", err)
	}
	if decoded.Bounds().Dx() != 2 || decoded.Bounds().Dy() != 2 {
		t.Fatalf("预览尺寸错误: %v", decoded.Bounds())
	}
}

func newTestServer(t *testing.T) (*Server, *fakeController) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeController{enabled: true, interval: time.Second}
	server, err := New(controller, store)
	if err != nil {
		t.Fatal(err)
	}
	return server, controller
}
