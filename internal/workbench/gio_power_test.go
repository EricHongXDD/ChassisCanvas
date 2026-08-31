package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/service"
)

func TestTargetStatusCarriesBrightnessAndPowerState(t *testing.T) {
	model := &gioEditor{}
	model.updateTargetOptions(service.Status{Devices: []service.DeviceStatus{{
		Info:      device.Info{ID: "waveshare-7", Name: "微雪 7 英寸", Resolution: [2]int{1024, 600}},
		Connected: true, BrightnessSupported: true, DefaultBrightness: 255, CurrentBrightness: 0, PoweredOff: true,
	}}}, false)
	option := model.targetOption("waveshare-7")
	if option == nil || !option.brightnessSupported || option.defaultBrightness != 255 || option.currentBrightness != 0 || !option.poweredOff {
		t.Fatalf("界面丢失亮度/关屏状态: %+v", option)
	}
	if !model.targetPoweredOff("waveshare-7") {
		t.Fatal("目标按钮未识别已关屏")
	}
}

func TestPowerInvalidatesOlderTargetStatusCallback(t *testing.T) {
	oldStatusStarted := make(chan struct{})
	releaseOldStatus := make(chan struct{})
	var statusCalls atomic.Int32
	var poweredOff atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/status":
			call := statusCalls.Add(1)
			value := poweredOff.Load()
			if call == 1 {
				close(oldStatusStarted)
				<-releaseOldStatus
				value = false
			}
			brightness := 80
			if value {
				brightness = 0
			}
			_ = json.NewEncoder(writer).Encode(service.Status{Devices: []service.DeviceStatus{{
				Info:      device.Info{ID: "screen", Name: "测试副屏", Resolution: [2]int{80, 45}},
				Connected: true, BrightnessSupported: true, DefaultBrightness: 80,
				CurrentBrightness: brightness, PoweredOff: value,
			}}})
		case "PUT /api/power":
			poweredOff.Store(true)
			_ = json.NewEncoder(writer).Encode(PowerResult{
				Target: "screen", PoweredOff: true, Results: map[string]string{"screen": "ok"}, Successes: 1,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	api := NewAPI(server.URL)
	api.setToken("local-token")
	model := &gioEditor{
		api: api, uiUpdates: make(chan func(), 4), target: "screen",
		targetOptions: []targetOption{{
			id: "screen", label: "测试副屏", connected: true, brightnessSupported: true,
			defaultBrightness: 80, currentBrightness: 80,
		}},
	}
	model.refreshTargetStatusAsync()
	select {
	case <-oldStatusStarted:
	case <-time.After(time.Second):
		t.Fatal("旧状态请求未开始")
	}
	model.setPoweredOff("screen", true)
	applyNextUIUpdate(t, model)
	if option := model.targetOption("screen"); option == nil || !option.poweredOff {
		t.Fatalf("关屏结果未应用: %+v", option)
	}
	close(releaseOldStatus)
	applyNextUIUpdate(t, model)
	if option := model.targetOption("screen"); option == nil || !option.poweredOff || option.currentBrightness != 0 {
		t.Fatalf("旧状态回调覆盖了新关屏状态: %+v", option)
	}
}

func TestPowerClientErrorStillReconcilesAuthoritativeStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "PUT /api/power":
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"error":"响应在执行后丢失"}`))
		case "GET /api/status":
			_ = json.NewEncoder(writer).Encode(service.Status{Devices: []service.DeviceStatus{{
				Info:      device.Info{ID: "screen", Name: "测试副屏", Resolution: [2]int{80, 45}},
				Connected: true, BrightnessSupported: true, DefaultBrightness: 80,
				CurrentBrightness: 0, PoweredOff: true,
			}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	api := NewAPI(server.URL)
	api.setToken("local-token")
	model := &gioEditor{
		api: api, uiUpdates: make(chan func(), 2), target: "screen",
		targetOptions: []targetOption{{
			id: "screen", label: "测试副屏", connected: true, brightnessSupported: true,
			defaultBrightness: 80, currentBrightness: 80,
		}},
	}
	model.setPoweredOff("screen", true)
	applyNextUIUpdate(t, model)
	option := model.targetOption("screen")
	if option == nil || !option.poweredOff || option.currentBrightness != 0 {
		t.Fatalf("命令响应失败后没有按服务状态对账: %+v", option)
	}
	if !strings.Contains(model.message, "失败") || !strings.Contains(model.message, "响应在执行后丢失") {
		t.Fatalf("原始命令错误未保留: %q", model.message)
	}
}

func TestAllPowerTargetStateHandlesMixedScreens(t *testing.T) {
	model := &gioEditor{targetOptions: []targetOption{
		{id: "one", brightnessSupported: true, poweredOff: true},
		{id: "two", brightnessSupported: true, poweredOff: false},
		{id: "unsupported", brightnessSupported: false},
	}}
	allOff, supported := model.allPowerTargetsOff()
	if allOff || supported != 2 {
		t.Fatalf("混合状态应显示“关闭全部”: allOff=%v supported=%d", allOff, supported)
	}
	model.targetOptions[1].poweredOff = true
	allOff, supported = model.allPowerTargetsOff()
	if !allOff || supported != 2 {
		t.Fatalf("已全关屏应显示“恢复全部”: allOff=%v supported=%d", allOff, supported)
	}
}

func TestPowerResultMessageListsEveryDeviceInScreenOrder(t *testing.T) {
	model := &gioEditor{targetOptions: []targetOption{
		{id: "waveshare-7", label: "微雪 7 英寸"},
		{id: "coolermaster", label: "酷冷至尊冷龙影二代"},
	}}
	message := model.powerResultMessage(PowerResult{
		Target: "all", PoweredOff: true, Successes: 1, Failures: 1,
		Results: map[string]string{"coolermaster": "HID 超时", "waveshare-7": "ok"},
	})
	first := strings.Index(message, "微雪 7 英寸：成功")
	second := strings.Index(message, "酷冷至尊冷龙影二代：失败（HID 超时）")
	if !strings.Contains(message, "关闭全部副屏完成：1 成功，1 失败") || first < 0 || second < 0 || first > second {
		t.Fatalf("部分失败未逐台展示: %s", message)
	}
}

func TestPowerResultUpdatesOnlySuccessfulScreensLocally(t *testing.T) {
	model := &gioEditor{targetOptions: []targetOption{
		{id: "one", brightnessSupported: true, defaultBrightness: 80, currentBrightness: 80},
		{id: "two", brightnessSupported: true, defaultBrightness: 100, currentBrightness: 100},
	}}
	model.applyPowerResultLocally(PowerResult{
		PoweredOff: true, Results: map[string]string{"one": "ok", "two": "USB 已断开"},
	})
	if !model.targetOptions[0].poweredOff || model.targetOptions[0].currentBrightness != 0 {
		t.Fatalf("成功设备未更新: %+v", model.targetOptions[0])
	}
	if model.targetOptions[1].poweredOff || model.targetOptions[1].currentBrightness != 100 {
		t.Fatalf("失败设备不应伪装成功: %+v", model.targetOptions[1])
	}
}
