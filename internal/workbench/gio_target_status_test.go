package workbench

import (
	"reflect"
	"strings"
	"testing"

	"casescreen/internal/device"
	"casescreen/internal/service"
	caseTheme "casescreen/internal/theme"
)

func TestTargetStatusRefreshUpdatesMetadataWithoutTouchingManifest(t *testing.T) {
	manifest := caseTheme.Manifest{
		ID: "editing", Width: 1024, Height: 600,
		Layers: []caseTheme.Layer{{ID: "custom-text", Type: caseTheme.LayerText, Bounds: caseTheme.Rect{X: .2, Y: .3, Width: .4, Height: .2}, Options: map[string]any{"text": "正在编辑"}}},
	}
	model := &gioEditor{
		target:        "desk-panel",
		manifest:      cloneManifest(manifest),
		targetOptions: []targetOption{{id: "desk-panel", label: "串口屏", resolution: [2]int{1024, 600}}},
	}
	status := service.Status{Devices: []service.DeviceStatus{{Info: device.Info{
		ID: "desk-panel", Name: "微雪 8.8 英寸", Resolution: [2]int{480, 1920},
		Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, StoresResources: true, SmallTelemetry: true},
	}}}}
	model.updateTargetOptions(status, true)
	if len(model.targetOptions) != 1 || model.targetOptions[0].label != "微雪 8.8 英寸" || model.targetOptions[0].resolution != [2]int{480, 1920} || !model.targetOptions[0].waveshare {
		t.Fatalf("目标元信息未更新：%+v", model.targetOptions)
	}
	if !reflect.DeepEqual(model.manifest, manifest) {
		t.Fatalf("后台状态刷新不应改写正在编辑的 manifest：got=%+v want=%+v", model.manifest, manifest)
	}
	if !strings.Contains(model.message, "480 × 1920") || !strings.Contains(model.message, "未被覆盖") {
		t.Fatalf("分辨率变更提示不清晰：%q", model.message)
	}
}

func TestCurrentTargetClickAdaptsCanvasAndPreservesLayers(t *testing.T) {
	model := &gioEditor{
		target: "desk-panel",
		targetOptions: []targetOption{{
			id: "desk-panel", label: "微雪 8.8 英寸", resolution: [2]int{480, 1920}, waveshare: true,
		}},
		manifest: caseTheme.Manifest{
			ID: "editing", Width: 1024, Height: 600,
			Layers: []caseTheme.Layer{{ID: "keep-me", Type: caseTheme.LayerText, Bounds: caseTheme.Rect{X: .1, Y: .2, Width: .3, Height: .1}, Options: map[string]any{"text": "保留我"}}},
		},
	}
	model.syncTargetButtons()
	model.targetButtons[0].action()
	if model.manifest.Width != 480 || model.manifest.Height != 1920 {
		t.Fatalf("明确点击当前目标后未适配真实分辨率：%d×%d", model.manifest.Width, model.manifest.Height)
	}
	kept := false
	for index := range model.manifest.Layers {
		if model.manifest.Layers[index].ID == "keep-me" && layerOptionString(&model.manifest.Layers[index], "text") == "保留我" {
			kept = true
			break
		}
	}
	if !kept {
		t.Fatalf("适配画布时丢失了用户图层：%+v", model.manifest.Layers)
	}
	if len(model.undoStack) != 1 {
		t.Fatalf("画布适配应可撤销：undo=%d", len(model.undoStack))
	}
}

func TestTargetButtonsStayMappedByIDWhenMetadataChanges(t *testing.T) {
	model := &gioEditor{targetOptions: []targetOption{
		{id: "screen-a", label: "A", resolution: [2]int{1024, 600}},
		{id: "screen-b", label: "B", resolution: [2]int{480, 480}},
	}}
	model.syncTargetButtons()
	buttonA, buttonB := model.targetButtons[0], model.targetButtons[1]

	model.targetOptions = []targetOption{
		{id: "screen-b", label: "B 新名称", resolution: [2]int{600, 600}},
		{id: "screen-a", label: "A 新名称", resolution: [2]int{480, 1920}, waveshare: true},
	}
	model.syncTargetButtons()
	if model.targetButtons[0] != buttonB || model.targetButtons[1] != buttonA {
		t.Fatal("设备顺序变化后 Clickable 没有按 ID 稳定复用")
	}
	if model.targetButtons[0].text != "B 新名称" || model.targetButtons[1].text != "A 新名称" {
		t.Fatalf("设备数量不变时按钮文字仍然陈旧：%q, %q", model.targetButtons[0].text, model.targetButtons[1].text)
	}
}

func TestTargetStatusPutsBothWaveshareScreensFirstAndHidesThermalright(t *testing.T) {
	model := &gioEditor{}
	status := service.Status{Devices: []service.DeviceStatus{
		{Info: device.Info{ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Resolution: [2]int{480, 480}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame}}},
		{Info: device.Info{ID: "thermalright-kv", Name: "利民 KV", Resolution: [2]int{1920, 468}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame}}},
		{Info: device.Info{ID: "waveshare-com4", Name: "微雪 8.8 英寸", Resolution: [2]int{480, 1920}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true}}},
		{Info: device.Info{ID: "waveshare-7", Name: "微雪 7 英寸", Resolution: [2]int{1024, 600}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true}}},
	}}

	model.updateTargetOptions(status, false)
	if len(model.targetOptions) != 3 {
		t.Fatalf("应隐藏利民目标：%+v", model.targetOptions)
	}
	want := []string{"waveshare-7", "waveshare-com4", "coolermaster-atmos-v2"}
	for index, id := range want {
		if model.targetOptions[index].id != id {
			t.Fatalf("目标顺序错误：got=%+v want=%v", model.targetOptions, want)
		}
	}
}

func TestTargetStatusPutsConnectedScreenBeforeOfflineScreen(t *testing.T) {
	model := &gioEditor{}
	status := service.Status{Devices: []service.DeviceStatus{
		{Info: device.Info{ID: "waveshare-7", Name: "微雪 7 英寸", Resolution: [2]int{1024, 600}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true}}, LastError: "COM3 未枚举"},
		{Info: device.Info{ID: "waveshare-com4", Name: "微雪 8.8 英寸", Resolution: [2]int{480, 1920}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true}}, Connected: true},
	}}

	model.updateTargetOptions(status, false)
	if len(model.targetOptions) != 2 || model.targetOptions[0].id != "waveshare-com4" || !model.targetOptions[0].connected {
		t.Fatalf("在线目标没有排在离线目标之前：%+v", model.targetOptions)
	}
	if model.targetOptions[1].lastError != "COM3 未枚举" {
		t.Fatalf("离线原因未保留：%+v", model.targetOptions[1])
	}
	if got := model.preferredTarget(""); got != "waveshare-com4" {
		t.Fatalf("首次进入没有优先选择唯一在线目标：%q", got)
	}
	if got := model.preferredTarget("waveshare-7"); got != "waveshare-com4" {
		t.Fatalf("陈旧离线目标不应压过在线目标：%q", got)
	}
	if got := model.preferredTarget("waveshare-com4"); got != "waveshare-com4" {
		t.Fatalf("当前在线目标应保持不变：%q", got)
	}
}

func TestLateDeviceConnectionAutoSelectsOnlineUntilUserChooses(t *testing.T) {
	model := &gioEditor{
		target: "waveshare-7",
		targetOptions: []targetOption{
			{id: "waveshare-com4", label: "微雪 8.8 英寸", connected: true},
			{id: "waveshare-7", label: "微雪 7 英寸", connected: false},
		},
	}
	if got := model.automaticConnectedTarget(); got != "waveshare-com4" {
		t.Fatalf("设备晚于首帧连接时没有自动选择在线目标：%q", got)
	}
	model.targetExplicit = true
	if got := model.automaticConnectedTarget(); got != "" {
		t.Fatalf("用户明确选择离线目标后不应自动跳走：%q", got)
	}
	model.targetExplicit = false
	model.target = "waveshare-com4"
	if got := model.automaticConnectedTarget(); got != "" {
		t.Fatalf("当前目标在线时不应重复切换：%q", got)
	}
}

func TestActiveCoolerCalibrationPreventsAutomaticTargetSwitch(t *testing.T) {
	model := &gioEditor{
		target: "coolermaster-atmos-v2",
		targetOptions: []targetOption{
			{id: "waveshare-7", label: "微雪 7 英寸", connected: true},
			{id: "coolermaster-atmos-v2", label: "酷冷至尊", connected: false, coolermaster: true},
		},
	}
	model.coolerCalibration.loadedTarget = model.target
	model.coolerCalibration.testVisible = true
	if got := model.automaticConnectedTarget(); got != "" {
		t.Fatalf("校准屏短暂离线时自动切换了目标: %q", got)
	}
}

func TestOfflineTargetSelectionExplainsQueuedApply(t *testing.T) {
	option := &targetOption{label: "微雪 7 英寸", lastError: "COM3 未枚举"}
	message := targetSelectionMessage(option, "在线")
	if !strings.Contains(message, "离线目标") || !strings.Contains(message, "重连后自动应用") || !strings.Contains(message, "COM3 未枚举") {
		t.Fatalf("离线目标提示不完整：%q", message)
	}
}
