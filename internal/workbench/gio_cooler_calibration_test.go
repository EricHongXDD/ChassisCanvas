package workbench

import (
	"encoding/json"
	"image"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/service"
)

func TestCoolerTargetMetadataAndRecognition(t *testing.T) {
	model := &gioEditor{}
	model.updateTargetOptions(service.Status{Devices: []service.DeviceStatus{{
		Info: device.Info{
			ID:              "coolermaster-atmos-v2",
			Name:            "酷冷至尊冷龙影二代",
			Resolution:      [2]int{480, 480},
			DisplayShape:    "circle_in_rounded_hex",
			DisplayRotation: 90,
			Capabilities:    device.Capabilities{DeliveryMode: device.DeliveryHostFrame},
		},
		Connected: true,
	}}}, false)

	if len(model.targetOptions) != 1 {
		t.Fatalf("酷冷目标没有进入工作台：%+v", model.targetOptions)
	}
	option := model.targetOptions[0]
	if !option.coolermaster || option.displayShape != "circle_in_rounded_hex" || option.displayRotation != 90 {
		t.Fatalf("酷冷屏幕外形或方向元数据丢失：%+v", option)
	}
	model.target = option.id
	if !model.isCoolerMasterTarget() {
		t.Fatal("已标记的酷冷目标没有被识别")
	}

	shapeOnly := &gioEditor{
		target:        "usb-display",
		targetOptions: []targetOption{{id: "usb-display", displayShape: "circle_in_rounded_hex"}},
	}
	if !shapeOnly.isCoolerMasterTarget() {
		t.Fatal("应能从圆形 LCD + 圆角六边形外壳元数据识别酷冷目标")
	}
	if (&gioEditor{target: "waveshare-7"}).isCoolerMasterTarget() {
		t.Fatal("微雪目标不应误判为酷冷")
	}
}

func TestCoolerDirectionTabIsTargetSpecific(t *testing.T) {
	model := &gioEditor{
		target:        "coolermaster-atmos-v2",
		targetOptions: []targetOption{{id: "coolermaster-atmos-v2", coolermaster: true}},
	}
	want := []optionChoice{
		{value: "layers", label: "图层"},
		{value: "properties", label: "属性"},
		{value: "presets", label: "预设"},
		{value: "cooler-orientation", label: "方向"},
		{value: "program-settings", label: "程序"},
	}
	if got := model.inspectorTabOptions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("酷冷专属方向页签位置错误：got=%+v want=%+v", got, want)
	}

	model.target = "waveshare-7"
	model.targetOptions = []targetOption{{id: "waveshare-7", waveshare: true}}
	model.inspectorTab = "cooler-orientation"
	model.normalizeInspectorTab()
	if model.inspectorTab != "layers" {
		t.Fatalf("离开酷冷目标后没有退出方向页：%q", model.inspectorTab)
	}
	for _, tab := range model.inspectorTabOptions() {
		if tab.value == "cooler-orientation" {
			t.Fatal("酷冷方向页泄漏到微雪目标")
		}
	}
}

func TestCoolerCanvasGeometryUsesWideShellAndCircularScreen(t *testing.T) {
	geometry := coolerCanvasGeometry(image.Pt(1000, 800), 12)
	if geometry.shell.X <= geometry.shell.Y {
		t.Fatalf("官方 85×79.2 mm 外壳应宽于高：got=%v", geometry.shell)
	}
	if geometry.screen.X != geometry.screen.Y {
		t.Fatalf("有效 LCD 应为正圆：got=%v", geometry.screen)
	}
	if geometry.screen.X >= geometry.shell.Y || geometry.screen.Y >= geometry.shell.Y {
		t.Fatalf("圆形 LCD 应完整落在六边形外壳内：shell=%v screen=%v", geometry.shell, geometry.screen)
	}
	if got := float64(geometry.shell.X) / float64(geometry.shell.Y); got < 1.06 || got > 1.09 {
		t.Fatalf("外壳宽高比应接近 85/79.2：got=%.4f", got)
	}
	if got := float64(geometry.screen.X) / float64(geometry.shell.Y); got < .66 || got > .69 {
		t.Fatalf("LCD 直径与外壳高度比应接近 53.3/79.2：got=%.4f", got)
	}
}

func TestCoolerMarkerMappingMatchesTwelveClockCalibration(t *testing.T) {
	for marker := 1; marker <= 12; marker++ {
		rotation := (marker - 6) * 30
		if got := coolerMarkerForRotation(rotation); got != stringMarker(marker) {
			t.Fatalf("方向数字 %d 与补偿角度 %d° 不可逆：got=%s", marker, rotation, got)
		}
	}
	if normalizeCoolerRotation(-30) != 330 || normalizeCoolerRotation(390) != 30 {
		t.Fatal("酷冷方向角度没有归一化到 0..359")
	}
	state := &coolerCalibrationState{loaded: true, lastError: "连接超时"}
	if got := coolerCalibrationStatusText(state); !strings.Contains(got, "操作失败") || !strings.Contains(got, "保留上次") {
		t.Fatalf("异常状态没有给出可操作的说明：%q", got)
	}
}

func stringMarker(marker int) string {
	const markers = "123456789"
	if marker >= 1 && marker <= 9 {
		return string(markers[marker-1])
	}
	if marker == 10 {
		return "10"
	}
	if marker == 11 {
		return "11"
	}
	return "12"
}

func TestCoolerCalibrationAsyncStateShowSaveAndCancel(t *testing.T) {
	const target = "coolermaster-atmos-v2"
	deleted := make(chan string, 4)
	var deleteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/theme" && request.Method == http.MethodGet && request.URL.Query().Get("target") == "waveshare-7" {
			response.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = response.Write([]byte("{}"))
			return
		}
		if request.URL.Path != "/api/coolermaster/calibration" || request.URL.Query().Get("target") != target {
			http.Error(response, "unexpected calibration path", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(CoolerCalibrationStatus{Target: target, Rotation: 90, Calibrated: true, Profile: 1})
		case http.MethodPost:
			response.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			var body struct {
				Marker string `json:"marker"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || (body.Marker != "10" && body.Marker != "11") {
				http.Error(response, "invalid marker", http.StatusBadRequest)
				return
			}
			if body.Marker == "11" {
				response.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(response).Encode(CoolerCalibrationStatus{
					Target: target, Rotation: 150, Calibrated: true, Profile: 1, SelectedMark: "11",
					Queued: true, Warning: "设备暂未确认恢复主题",
				})
				return
			}
			_ = json.NewEncoder(response).Encode(CoolerCalibrationStatus{Target: target, Rotation: 120, Calibrated: true, Profile: 1, SelectedMark: "10"})
		case http.MethodDelete:
			deleted <- request.URL.Query().Get("target")
			if deleteCalls.Add(1) == 1 {
				response.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(response).Encode(CoolerCalibrationStatus{
					Target: target, Rotation: 150, Calibrated: true, Profile: 1,
					Queued: true, Warning: "设备暂未确认恢复原画面",
				})
				return
			}
			_ = json.NewEncoder(response).Encode(CoolerCalibrationStatus{Target: target, Rotation: 120, Calibrated: true, Profile: 1})
		default:
			http.Error(response, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("test-token")
	model := &gioEditor{
		api:           api,
		target:        target,
		targetOptions: []targetOption{{id: target, coolermaster: true, connected: true}},
		uiUpdates:     make(chan func(), 8),
	}
	model.coolerCalibration.init()
	model.coolerCalibration.reset(target)

	model.loadCoolerCalibration(false)
	applyCoolerAsyncUpdate(t, model)
	if !model.coolerCalibration.loaded || model.coolerCalibration.status.Rotation != 90 || model.targetOptions[0].displayRotation != 90 {
		t.Fatalf("已保存方向没有加载到页面：state=%+v target=%+v", model.coolerCalibration, model.targetOptions[0])
	}

	model.showCoolerCalibration()
	applyCoolerAsyncUpdate(t, model)
	if !model.coolerCalibration.testVisible || model.coolerCalibration.selectedMark != "" {
		t.Fatalf("显示测试图后状态不清晰：%+v", model.coolerCalibration)
	}

	model.coolerCalibration.selectedMark = "10"
	model.saveCoolerCalibration()
	applyCoolerAsyncUpdate(t, model)
	if model.coolerCalibration.testVisible || model.coolerCalibration.selectedMark != "" || model.coolerCalibration.status.Rotation != 120 {
		t.Fatalf("确认后没有保存方向或退出测试图：%+v", model.coolerCalibration)
	}

	// 202/Warning 只表示设置已接收，不能假定物理屏已恢复主题。
	model.showCoolerCalibration()
	applyCoolerAsyncUpdate(t, model)
	model.coolerCalibration.selectedMark = "11"
	model.saveCoolerCalibration()
	applyCoolerAsyncUpdate(t, model)
	if !model.coolerCalibration.testVisible || model.coolerCalibration.selectedMark != "11" || !strings.Contains(model.message, "仍可能显示") {
		t.Fatalf("202 保存后隐藏了测试图恢复入口：state=%+v message=%q", model.coolerCalibration, model.message)
	}

	model.cancelCoolerCalibration()
	applyCoolerAsyncUpdate(t, model)
	if !model.coolerCalibration.testVisible || !strings.Contains(model.message, "仍可能显示") {
		t.Fatalf("202 取消后过早隐藏了再次取消入口：state=%+v message=%q", model.coolerCalibration, model.message)
	}
	select {
	case <-deleted:
	case <-time.After(2 * time.Second):
		t.Fatal("第一次显式取消没有请求服务恢复原画面")
	}
	model.cancelCoolerCalibration()
	applyCoolerAsyncUpdate(t, model)
	if model.coolerCalibration.testVisible || model.coolerCalibration.selectedMark != "" {
		t.Fatalf("取消确认执行后没有恢复原画面：%+v", model.coolerCalibration)
	}
	select {
	case <-deleted:
	case <-time.After(2 * time.Second):
		t.Fatal("重试取消没有请求服务恢复原画面")
	}

	// 切换目标时也必须先退出旧酷冷的测试图，
	// 但恢复请求不应阻塞新目标的主题载入。
	model.showCoolerCalibration()
	applyCoolerAsyncUpdate(t, model)
	model.targetOptions = append(model.targetOptions, targetOption{
		id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600}, waveshare: true, connected: true,
	})
	model.selectTargetFromStatus("waveshare-7")
	if model.coolerCalibration.testVisible {
		t.Fatal("开始切换目标后仍把酷冷测试图标记为正在显示")
	}
	select {
	case got := <-deleted:
		if got != target {
			t.Fatalf("切换目标时取消了错误的设备：%q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("切换目标时没有异步取消旧酷冷测试图")
	}
	applyCoolerAsyncUpdate(t, model)
	if model.target != "waveshare-7" || model.coolerCalibration.loadedTarget != "waveshare-7" {
		t.Fatalf("新目标载入后校准状态没有切换：target=%q state=%+v", model.target, model.coolerCalibration)
	}
}

func TestCoolerCalibrationLoadRestoresServerActiveState(t *testing.T) {
	const target = "coolermaster-atmos-v2"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(response).Encode(CoolerCalibrationStatus{
			Target: target, Rotation: 90, Calibrated: true, Profile: 1, Active: true,
		})
	}))
	defer server.Close()
	model := &gioEditor{
		api:           NewAPI(server.URL),
		target:        target,
		targetOptions: []targetOption{{id: target, coolermaster: true, connected: true}},
		uiUpdates:     make(chan func(), 2),
	}
	model.coolerCalibration.init()
	model.coolerCalibration.reset(target)
	model.loadCoolerCalibration(false)
	applyCoolerAsyncUpdate(t, model)
	if !model.coolerCalibration.loaded || !model.coolerCalibration.testVisible {
		t.Fatalf("工作台重载后没有恢复服务端活跃校准状态: %+v", model.coolerCalibration)
	}
}

func TestPendingCoolerShowCancelsAfterTargetSwitch(t *testing.T) {
	const (
		oldTarget = "coolermaster-atmos-v2"
		newTarget = "waveshare-7"
	)
	postStarted := make(chan struct{}, 1)
	releasePost := make(chan struct{})
	deleted := make(chan string, 1)
	defer func() {
		select {
		case <-releasePost:
		default:
			close(releasePost)
		}
	}()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/theme" && request.Method == http.MethodGet && request.URL.Query().Get("target") == newTarget:
			response.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = response.Write([]byte("{}"))
		case request.URL.Path == "/api/coolermaster/calibration" && request.URL.Query().Get("target") == oldTarget && request.Method == http.MethodPost:
			postStarted <- struct{}{}
			<-releasePost
			response.WriteHeader(http.StatusNoContent)
		case request.URL.Path == "/api/coolermaster/calibration" && request.URL.Query().Get("target") == oldTarget && request.Method == http.MethodDelete:
			deleted <- oldTarget
			response.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(response).Encode(CoolerCalibrationStatus{Target: oldTarget})
		default:
			http.Error(response, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("test-token")
	model := &gioEditor{
		api:    api,
		target: oldTarget,
		targetOptions: []targetOption{
			{id: oldTarget, label: "酷冷至尊冷头", resolution: [2]int{480, 480}, coolermaster: true, connected: true},
			{id: newTarget, label: "微雪 7 英寸", resolution: [2]int{1024, 600}, waveshare: true, connected: true},
		},
		uiUpdates: make(chan func(), 8),
	}
	model.coolerCalibration.init()
	model.coolerCalibration.reset(oldTarget)

	model.showCoolerCalibration()
	select {
	case <-postStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("延迟测试的 POST 没有开始")
	}
	if len(model.coolerCalibration.pendingShows) != 1 {
		t.Fatalf("没有跟踪正在进行的测试图请求：%+v", model.coolerCalibration.pendingShows)
	}

	model.selectTargetFromStatus(newTarget)
	stale := false
	for _, pending := range model.coolerCalibration.pendingShows {
		stale = stale || pending.target == oldTarget && pending.stale
	}
	if !stale {
		t.Fatalf("切换目标时没有把旧 POST 标记为过期：%+v", model.coolerCalibration.pendingShows)
	}
	select {
	case <-deleted:
		t.Fatal("POST 尚未成功时不应抢先 DELETE，否则后到的 POST 会重新显示测试图")
	default:
	}
	close(releasePost)

	deleteObserved := false
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for !deleteObserved || model.target != newTarget || len(model.coolerCalibration.pendingShows) != 0 {
		select {
		case update := <-model.uiUpdates:
			update()
		case got := <-deleted:
			if got != oldTarget {
				t.Fatalf("过期 POST 成功后恢复了错误的目标：%q", got)
			}
			deleteObserved = true
		case <-deadline.C:
			t.Fatalf("过期 POST 成功后没有补发 DELETE：target=%q pending=%+v", model.target, model.coolerCalibration.pendingShows)
		}
	}
}

func applyCoolerAsyncUpdate(t *testing.T, model *gioEditor) {
	t.Helper()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(2 * time.Second):
		t.Fatal("酷冷方向请求没有返回 UI 更新")
	}
}
