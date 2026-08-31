package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/config"
)

type fakeCoolerMasterCalibrationController struct {
	*fakeController
	showTargets   []string
	setTargets    []string
	setRotations  []int
	cancelTargets []string
	active        bool
}

func (controller *fakeCoolerMasterCalibrationController) ShowOrientationCalibration(_ context.Context, target string) error {
	controller.showTargets = append(controller.showTargets, target)
	controller.active = true
	return nil
}

func (controller *fakeCoolerMasterCalibrationController) SetDisplayRotation(_ context.Context, target string, rotation int) error {
	controller.setTargets = append(controller.setTargets, target)
	controller.setRotations = append(controller.setRotations, rotation)
	controller.active = false
	return nil
}

func (controller *fakeCoolerMasterCalibrationController) CancelOrientationCalibration(_ context.Context, target string) error {
	controller.cancelTargets = append(controller.cancelTargets, target)
	controller.active = false
	return nil
}

func (controller *fakeCoolerMasterCalibrationController) OrientationCalibrationActive(string) bool {
	return controller.active
}

func newCoolerMasterCalibrationTestServer(t *testing.T) (*Server, *fakeCoolerMasterCalibrationController) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) error {
		cfg.Devices = []config.Device{
			{
				ID: "coolermaster-atmos-v2", Kind: "coolermaster", Enabled: true,
				Width: 480, Height: 480, Slot: 4, DisplayRotation: 330,
			},
			{ID: "waveshare-7", Kind: "waveshare", Enabled: true, Port: "COM7", Width: 1024, Height: 600},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controller := &fakeCoolerMasterCalibrationController{
		fakeController: &fakeController{enabled: true, interval: time.Second},
	}
	server, err := New(controller, store)
	if err != nil {
		t.Fatal(err)
	}
	return server, controller
}

func coolerMasterCalibrationRequest(server *Server, method, target, body string, authorized bool) *http.Request {
	request := httptest.NewRequest(method, "/api/coolermaster/calibration?target="+target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	if authorized {
		request.Header.Set("X-CaseScreen-Token", server.token)
	}
	return request
}

func TestCoolerRotationForEveryMarker(t *testing.T) {
	want := map[string]int{
		"1": 210, "2": 240, "3": 270, "4": 300, "5": 330, "6": 0,
		"7": 30, "8": 60, "9": 90, "10": 120, "11": 150, "12": 180,
	}
	for marker, rotation := range want {
		t.Run(marker, func(t *testing.T) {
			got, err := coolerRotationForMarker(marker)
			if err != nil || got != rotation {
				t.Fatalf("标识 %s 映射错误: rotation=%d err=%v", marker, got, err)
			}
		})
	}
	for _, marker := range []string{"", "0", "13", "A", "1.5"} {
		if _, err := coolerRotationForMarker(marker); err == nil {
			t.Fatalf("非法方向标识 %q 未被拒绝", marker)
		}
	}
}

func TestCoolerMasterCalibrationWorkflow(t *testing.T) {
	server, controller := newCoolerMasterCalibrationTestServer(t)
	target := "coolermaster-atmos-v2"

	// GET 是只读状态，与其它只读设置接口一致，不要求本地控制令牌。
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, http.MethodGet, target, "", false))
	if response.Code != http.StatusOK {
		t.Fatalf("读取方向状态失败: %d %s", response.Code, response.Body.String())
	}
	var status coolerMasterCalibrationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Target != target || status.Rotation != 330 || status.Calibrated || status.Profile != 0 {
		t.Fatalf("初始方向状态错误: %+v", status)
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, http.MethodPost, target, `{}`, true))
	if response.Code != http.StatusOK || len(controller.showTargets) != 1 || controller.showTargets[0] != target {
		t.Fatalf("显示校准图未调用正确设备: code=%d calls=%v body=%s", response.Code, controller.showTargets, response.Body.String())
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, http.MethodGet, target, "", false))
	if response.Code != http.StatusOK {
		t.Fatalf("读取活跃方向状态失败: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || !status.Active {
		t.Fatalf("服务端没有暴露正在显示的测试图: status=%+v err=%v", status, err)
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, http.MethodPut, target, `{"marker":"3"}`, true))
	if response.Code != http.StatusOK || len(controller.setTargets) != 1 || controller.setTargets[0] != target ||
		len(controller.setRotations) != 1 || controller.setRotations[0] != 270 {
		t.Fatalf("保存方向未调用正确设备: code=%d targets=%v rotations=%v body=%s", response.Code, controller.setTargets, controller.setRotations, response.Body.String())
	}
	configured := server.store.Get().Devices[0]
	if configured.DisplayRotation != 270 || !configured.OrientationCalibrated || configured.OrientationProfile != 1 {
		t.Fatalf("保存方向后配置不完整: %+v", configured)
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, http.MethodDelete, target, `{}`, true))
	if response.Code != http.StatusOK || len(controller.cancelTargets) != 1 || controller.cancelTargets[0] != target {
		t.Fatalf("取消校准未恢复正确设备: code=%d calls=%v body=%s", response.Code, controller.cancelTargets, response.Body.String())
	}
}

func TestPutThemeDuringCoolerCalibrationReturnsConflictWithoutPersisting(t *testing.T) {
	server, controller := newCoolerMasterCalibrationTestServer(t)
	target := "coolermaster-atmos-v2"
	controller.active = true
	manifest := map[string]any{
		"id": "must-not-persist", "name": "校准冲突主题", "width": 480, "height": 480,
		"layers": []map[string]any{{
			"id": "canvas", "type": "canvas", "bounds": map[string]any{"x": 0, "y": 0, "width": 1, "height": 1},
			"opacity": 1, "options": map[string]any{"color": "#102030ff"},
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	themePath, err := server.themePathFor(target)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/theme?target="+target, strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "方向测试图正在显示") {
		t.Fatalf("校准期间主题应用未明确冲突: %d %s", response.Code, response.Body.String())
	}
	if controller.applyCount != 0 || server.store.Get().DeviceThemePaths[target] != "" {
		t.Fatalf("冲突主题仍进入设备或配置: apply=%d path=%q", controller.applyCount, server.store.Get().DeviceThemePaths[target])
	}
	if _, err := os.Stat(themePath); !os.IsNotExist(err) {
		t.Fatalf("冲突主题仍写入文件: path=%s err=%v", themePath, err)
	}
}

func TestCoolerMasterCalibrationMutationsRequireAuthorization(t *testing.T) {
	for _, test := range []struct {
		method string
		body   string
	}{
		{method: http.MethodPost, body: `{}`},
		{method: http.MethodPut, body: `{"marker":"6"}`},
		{method: http.MethodDelete, body: `{}`},
	} {
		t.Run(test.method, func(t *testing.T) {
			server, controller := newCoolerMasterCalibrationTestServer(t)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, test.method, "coolermaster-atmos-v2", test.body, false))
			if response.Code != http.StatusForbidden {
				t.Fatalf("无令牌 %s 未被拒绝: %d %s", test.method, response.Code, response.Body.String())
			}
			if len(controller.showTargets)+len(controller.setTargets)+len(controller.cancelTargets) != 0 {
				t.Fatalf("无令牌 %s 仍调用了设备控制器", test.method)
			}
			configured := server.store.Get().Devices[0]
			if configured.DisplayRotation != 330 || configured.OrientationCalibrated || configured.OrientationProfile != 0 {
				t.Fatalf("无令牌 %s 改写了方向配置: %+v", test.method, configured)
			}
		})
	}
}

func TestCoolerMasterCalibrationRejectsNonCoolerTargets(t *testing.T) {
	for _, test := range []struct {
		method string
		body   string
	}{
		{method: http.MethodGet},
		{method: http.MethodPost, body: `{}`},
		{method: http.MethodPut, body: `{"marker":"6"}`},
		{method: http.MethodDelete, body: `{}`},
	} {
		t.Run(test.method, func(t *testing.T) {
			server, controller := newCoolerMasterCalibrationTestServer(t)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, test.method, "waveshare-7", test.body, true))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "不是酷冷至尊副屏") {
				t.Fatalf("%s 未拒绝非酷冷目标: %d %s", test.method, response.Code, response.Body.String())
			}
			if len(controller.showTargets)+len(controller.setTargets)+len(controller.cancelTargets) != 0 {
				t.Fatalf("%s 非酷冷目标仍调用了设备控制器", test.method)
			}
		})
	}
}

type concurrentCoolerCalibrationCall struct {
	target   string
	rotation int
}

type concurrentCoolerCalibrationController struct {
	*fakeController
	blockTarget   string
	blockRotation int
	started       chan concurrentCoolerCalibrationCall
	release       chan struct{}

	mu            sync.Mutex
	liveRotations map[string]int
}

func (controller *concurrentCoolerCalibrationController) ShowOrientationCalibration(context.Context, string) error {
	return nil
}

func (controller *concurrentCoolerCalibrationController) SetDisplayRotation(ctx context.Context, target string, rotation int) error {
	call := concurrentCoolerCalibrationCall{target: target, rotation: rotation}
	select {
	case controller.started <- call:
	case <-ctx.Done():
		return ctx.Err()
	}
	if target == controller.blockTarget && rotation == controller.blockRotation {
		select {
		case <-controller.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	controller.mu.Lock()
	controller.liveRotations[target] = rotation
	controller.mu.Unlock()
	return nil
}

func (controller *concurrentCoolerCalibrationController) CancelOrientationCalibration(context.Context, string) error {
	return nil
}

func (controller *concurrentCoolerCalibrationController) liveRotation(target string) int {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.liveRotations[target]
}

func runCoolerCalibrationSave(server *Server, target, marker string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		body := `{"marker":"` + marker + `"}`
		server.Handler().ServeHTTP(response, coolerMasterCalibrationRequest(server, http.MethodPut, target, body, true))
		done <- response
	}()
	return done
}

func waitCoolerCalibrationCall(t *testing.T, calls <-chan concurrentCoolerCalibrationCall) concurrentCoolerCalibrationCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("等待酷冷方向并发调用超时")
		return concurrentCoolerCalibrationCall{}
	}
}

func waitCoolerCalibrationResponse(t *testing.T, response <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case result := <-response:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("等待酷冷方向并发请求超时")
		return nil
	}
}

func assertCoolerCalibrationLockTableEmpty(t *testing.T, server *Server) {
	t.Helper()
	server.coolerCalibrationMu.Lock()
	defer server.coolerCalibrationMu.Unlock()
	if len(server.coolerCalibrationLocks) != 0 {
		t.Fatalf("酷冷方向目标锁未在最后一个引用离开后回收: %+v", server.coolerCalibrationLocks)
	}
}

func TestCoolerMasterCalibrationSerializesStoreAndLiveRotationPerTarget(t *testing.T) {
	server, baseController := newCoolerMasterCalibrationTestServer(t)
	const target = "coolermaster-atmos-v2"
	controller := &concurrentCoolerCalibrationController{
		fakeController: baseController.fakeController,
		blockTarget:    target, blockRotation: 30,
		started: make(chan concurrentCoolerCalibrationCall, 4),
		release: make(chan struct{}), liveRotations: make(map[string]int),
	}
	server.controller = controller

	firstResponse := runCoolerCalibrationSave(server, target, "7")
	if call := waitCoolerCalibrationCall(t, controller.started); call != (concurrentCoolerCalibrationCall{target: target, rotation: 30}) {
		t.Fatalf("第一个方向请求错误: %+v", call)
	}
	secondResponse := runCoolerCalibrationSave(server, target, "8")

	// 第一个请求卡在实时应用时，第二个请求不得先改配置或进入驱动。
	select {
	case call := <-controller.started:
		t.Fatalf("同一目标的第二个请求越过了未完成的事务: %+v", call)
	case <-time.After(120 * time.Millisecond):
	}
	configured := server.store.Get().Devices[0]
	if configured.DisplayRotation != 30 {
		t.Fatalf("第一个事务未完成时配置已被第二个请求越过: %+v", configured)
	}

	close(controller.release)
	if response := waitCoolerCalibrationResponse(t, firstResponse); response.Code != http.StatusOK {
		t.Fatalf("第一个并发方向请求失败: %d %s", response.Code, response.Body.String())
	}
	if call := waitCoolerCalibrationCall(t, controller.started); call != (concurrentCoolerCalibrationCall{target: target, rotation: 60}) {
		t.Fatalf("第二个方向请求没有按序执行: %+v", call)
	}
	if response := waitCoolerCalibrationResponse(t, secondResponse); response.Code != http.StatusOK {
		t.Fatalf("第二个并发方向请求失败: %d %s", response.Code, response.Body.String())
	}

	configured = server.store.Get().Devices[0]
	if configured.DisplayRotation != 60 || controller.liveRotation(target) != 60 {
		t.Fatalf("最终配置与运行中驱动方向不一致: config=%d live=%d", configured.DisplayRotation, controller.liveRotation(target))
	}
	assertCoolerCalibrationLockTableEmpty(t, server)
}

func TestCoolerMasterCalibrationDoesNotBlockDifferentTargets(t *testing.T) {
	server, baseController := newCoolerMasterCalibrationTestServer(t)
	const firstTarget = "coolermaster-atmos-v2"
	const secondTarget = "coolermaster-atmos-v2-secondary"
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.Devices = append(cfg.Devices, config.Device{
			ID: secondTarget, Kind: "coolermaster", Enabled: true,
			Width: 480, Height: 480, Slot: 3,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controller := &concurrentCoolerCalibrationController{
		fakeController: baseController.fakeController,
		blockTarget:    firstTarget, blockRotation: 30,
		started: make(chan concurrentCoolerCalibrationCall, 4),
		release: make(chan struct{}), liveRotations: make(map[string]int),
	}
	server.controller = controller

	firstResponse := runCoolerCalibrationSave(server, firstTarget, "7")
	if call := waitCoolerCalibrationCall(t, controller.started); call.target != firstTarget {
		t.Fatalf("首个目标调用错误: %+v", call)
	}
	secondResponse := runCoolerCalibrationSave(server, secondTarget, "8")
	if call := waitCoolerCalibrationCall(t, controller.started); call != (concurrentCoolerCalibrationCall{target: secondTarget, rotation: 60}) {
		t.Fatalf("不同目标被全局粗锁阻塞或调用错误: %+v", call)
	}
	if response := waitCoolerCalibrationResponse(t, secondResponse); response.Code != http.StatusOK {
		t.Fatalf("第二台设备的方向请求失败: %d %s", response.Code, response.Body.String())
	}
	if controller.liveRotation(secondTarget) != 60 {
		t.Fatalf("第二台设备未独立应用方向: %d", controller.liveRotation(secondTarget))
	}

	close(controller.release)
	if response := waitCoolerCalibrationResponse(t, firstResponse); response.Code != http.StatusOK {
		t.Fatalf("第一台设备的方向请求失败: %d %s", response.Code, response.Body.String())
	}
	assertCoolerCalibrationLockTableEmpty(t, server)
}
