package workbench

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/service"
	caseTheme "casescreen/internal/theme"
	"gioui.org/f32"
	"gioui.org/io/input"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

func TestPlatformAuthNeedsLoginTextIncludesMembershipRequirement(t *testing.T) {
	if text := platformAuthStateText("not_configured"); text != "未配置；公开歌词无需登录" {
		t.Fatalf("not_configured 状态提示不完整: %q", text)
	}
	if text := platformAuthStateText("needs_login"); text != "需要登录或会员权限" {
		t.Fatalf("needs_login 状态提示不完整: %q", text)
	}
}

func TestRefreshPlatformAuthShowsLatestNeedsLoginState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/platform-auth" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[{"platform":"qq","state":"needs_login"},{"platform":"netease","state":"configured"}]}`))
	}))
	defer server.Close()
	model := &gioEditor{
		api: NewAPI(server.URL), uiUpdates: make(chan func(), 1),
		platformAuthStatuses: make(map[string]PlatformAuthStatus),
	}
	model.refreshPlatformAuth()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("刷新登录态没有返回")
	}
	if model.platformAuthLoading || model.platformAuthStatuses["qq"].State != "needs_login" ||
		model.platformAuthStatuses["netease"].State != "configured" {
		t.Fatalf("没有显示服务端最新登录态: loading=%v statuses=%+v", model.platformAuthLoading, model.platformAuthStatuses)
	}
}

func TestInspectorTabsSeparateWaveshareTemplateAndSettings(t *testing.T) {
	model := &gioEditor{target: "waveshare-7"}
	tabs := model.inspectorTabOptions()
	want := []optionChoice{{"layers", "图层"}, {"properties", "属性"}, {"presets", "预设"}, {"waveshare-library", "模板"}, {"waveshare-settings", "设置"}, {"program-settings", "程序"}}
	if !reflect.DeepEqual(tabs, want) {
		t.Fatalf("微雪页签没有按功能拆分：got=%+v want=%+v", tabs, want)
	}

	model.target = "thermalright-kv"
	model.inspectorTab = "waveshare-settings"
	model.normalizeInspectorTab()
	if model.inspectorTab != "layers" {
		t.Fatalf("切到非微雪目标后没有退出专属设置页：%q", model.inspectorTab)
	}
	if tabs = model.inspectorTabOptions(); len(tabs) != 4 || tabs[0].value != "layers" || tabs[1].value != "properties" || tabs[2].value != "presets" || tabs[3].value != "program-settings" {
		t.Fatalf("非微雪目标出现了专属页签：%+v", tabs)
	}
}

func TestWaveshareTemplateAndSettingsPagesKeepControlsSeparate(t *testing.T) {
	newModel := func() *gioEditor {
		model := &gioEditor{
			theme:            material.NewTheme(),
			target:           "waveshare-7",
			manifest:         defaultManifestFor(1024, 600),
			componentOptions: make(map[string]*widget.Clickable),
		}
		model.waveshareLibrary.init()
		model.waveshareLibrary.loadedTarget = model.target
		model.waveshareLibrary.templates = []config.WaveshareTemplate{{ID: "day", Name: "日间"}, {ID: "night", Name: "夜间"}}
		model.waveshareLibrary.applyServerPlaylist(model.target, config.WavesharePlaylist{Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "day"}}})
		return model
	}
	layoutPage := func(model *gioEditor, page layout.Widget) {
		var router input.Router
		var operations op.Ops
		gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(330, 1800)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
		page(gtx)
		router.Frame(&operations)
	}

	templateModel := newModel()
	layoutPage(templateModel, templateModel.layoutWaveshareLibrary)
	for _, key := range []string{"waveshare-template-save", "waveshare-activate-day", "waveshare-rename-day", "waveshare-delete-day"} {
		if templateModel.componentOptions[key] == nil {
			t.Fatalf("模板页缺少模板管理控件 %q", key)
		}
	}
	for _, key := range []string{"waveshare-storage-refresh", "waveshare-playlist-enabled", "waveshare-playlist-save", "waveshare-playlist-select-all"} {
		if templateModel.componentOptions[key] != nil {
			t.Fatalf("模板页混入了运行设置控件 %q", key)
		}
	}

	settingsModel := newModel()
	layoutPage(settingsModel, settingsModel.layoutWaveshareSettings)
	for _, key := range []string{"waveshare-storage-refresh", "waveshare-playlist-enabled", "waveshare-playlist-save", "waveshare-playlist-select-all", "waveshare-playlist-clear"} {
		if settingsModel.componentOptions[key] == nil {
			t.Fatalf("设置页缺少运行设置控件 %q", key)
		}
	}
	for _, key := range []string{"waveshare-template-save", "waveshare-activate-day", "waveshare-rename-day", "waveshare-delete-day"} {
		if settingsModel.componentOptions[key] != nil {
			t.Fatalf("设置页混入了模板创建或管理控件 %q", key)
		}
	}
	// 两个页面使用不同的 List 实例；改动模板页位置，设置页不应随之变化。
	templateModel.waveshareLibrary.list.Position.First = 3
	if settingsModel.waveshareLibrary.settingsList.Position.First == 3 {
		t.Fatal("模板页与设置页错误共享了滚动位置")
	}
}

func TestWaveshareSettingsHidePlaylistControlsUntilLoaded(t *testing.T) {
	model := &gioEditor{
		theme:            material.NewTheme(),
		target:           "waveshare-7",
		manifest:         defaultManifestFor(1024, 600),
		componentOptions: make(map[string]*widget.Clickable),
	}
	model.waveshareLibrary.reset(model.target)
	var router input.Router
	var operations op.Ops
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(330, 900)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	model.layoutWaveshareSettings(gtx)
	router.Frame(&operations)
	if model.componentOptions["waveshare-storage-refresh"] == nil {
		t.Fatal("轮播加载期间仍应显示独立的容量刷新控件")
	}
	for _, key := range []string{"waveshare-playlist-enabled", "waveshare-playlist-save", "waveshare-playlist-select-all", "waveshare-playlist-clear"} {
		if model.componentOptions[key] != nil {
			t.Fatalf("轮播初次加载完成前不应提供可编辑占位控件 %q", key)
		}
	}
}

func TestWavesharePlaylistBulkSelectionAndExplicitUpdateMode(t *testing.T) {
	model := &gioEditor{target: "waveshare-7"}
	model.waveshareLibrary.init()
	model.waveshareLibrary.templates = []config.WaveshareTemplate{{ID: "day", Name: "日间"}, {ID: "night", Name: "夜间"}}
	model.waveshareLibrary.playlist.Entries = []config.WavesharePlaylistEntry{{TemplateID: "night", DurationSeconds: 45}}
	model.waveshareLibrary.durationField("night", 45).set("45")
	model.selectAllWavesharePlaylistEntries()
	if got := model.waveshareLibrary.playlist.Entries; len(got) != 2 || got[0].TemplateID != "night" || got[1].TemplateID != "day" || got[0].DurationSeconds != 45 {
		t.Fatalf("全选没有保留原顺序与单模板设置：%+v", got)
	}
	model.clearWavesharePlaylistEntries()
	if len(model.waveshareLibrary.playlist.Entries) != 0 || len(model.waveshareLibrary.durationFields) != 0 {
		t.Fatalf("清空轮播选择不完整：entries=%+v fields=%d", model.waveshareLibrary.playlist.Entries, len(model.waveshareLibrary.durationFields))
	}

	model.waveshareLibrary.list.Position.First = 5
	model.waveshareLibrary.list.Position.Offset = 17
	model.selectWaveshareTemplateForUpdate("night")
	if model.waveshareLibrary.editingTemplateID != "night" || model.waveshareLibrary.templateName.editor.Text() != "夜间" {
		t.Fatalf("没有显式进入模板更新模式：id=%q name=%q", model.waveshareLibrary.editingTemplateID, model.waveshareLibrary.templateName.editor.Text())
	}
	if model.waveshareLibrary.list.Position != (layout.Position{}) {
		t.Fatalf("选择更新后没有返回创建区：%+v", model.waveshareLibrary.list.Position)
	}
	model.clearWaveshareTemplateUpdate()
	if model.waveshareLibrary.editingTemplateID != "" || model.waveshareLibrary.templateName.editor.Text() != "" {
		t.Fatal("没有退出模板更新模式")
	}
}

func TestWavesharePlaylistEnabledRequiresSelectedTemplate(t *testing.T) {
	state := waveshareLibraryState{playlist: config.WavesharePlaylist{Enabled: true, Mode: "sequence"}}
	state.init()
	if _, err := state.playlistSnapshot(); err == nil || !strings.Contains(err.Error(), "至少选择一个") {
		t.Fatalf("启用空轮播时没有明确拒绝：%v", err)
	}
}

func TestWaveshareTemplateCreateRejectsImplicitSameNameOverwrite(t *testing.T) {
	model := &gioEditor{api: NewAPI("http://127.0.0.1:1"), target: "waveshare-7", manifest: defaultManifestFor(1024, 600)}
	model.waveshareLibrary.init()
	model.waveshareLibrary.loadedTarget = model.target
	model.waveshareLibrary.templates = []config.WaveshareTemplate{{ID: "night", Name: "夜间"}}
	model.waveshareLibrary.templateName.set(" 夜间 ")
	model.saveCurrentWaveshareTemplate()
	if model.waveshareLibrary.operationRunning || !strings.Contains(model.message, "显示并载入编辑") {
		t.Fatalf("同名新建没有在发请求前阻止静默覆盖：running=%v message=%q", model.waveshareLibrary.operationRunning, model.message)
	}
}

func TestGioLayoutUsesFullWindowAndBuildsTargetSelector(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{
		theme:        material.NewTheme(),
		manifest:     defaultManifest(),
		target:       "waveshare-7",
		inspectorTab: "layers",
		targetOptions: []targetOption{
			{id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600}},
			{id: "thermalright-kv", label: "利民 KV", resolution: [2]int{1920, 468}},
			{id: "coolermaster-atmos-v2", label: "酷冷至尊冷龙影二代", resolution: [2]int{480, 480}},
		},
	}
	gtx := layout.Context{
		Ops:         &operations,
		Source:      router.Source(),
		Constraints: layout.Exact(image.Pt(1500, 900)),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
	}
	dimensions := model.layout(gtx)
	if dimensions.Size != image.Pt(1500, 900) {
		t.Fatalf("布局未填满窗口：got %v", dimensions.Size)
	}
	if len(model.targetButtons) != 3 {
		t.Fatalf("目标屏幕选择器按钮数量错误：got %d", len(model.targetButtons))
	}
	router.Frame(&operations)
}

func TestInspectorPropertyTabCanBeClicked(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{theme: material.NewTheme(), manifest: defaultManifest(), selected: 0, inspectorTab: "layers"}
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(1500, 900)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	frame := func() { gtx.Reset(); model.layout(gtx); router.Frame(gtx.Ops) }
	frame()
	router.Queue(
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Press, Position: f32.Pt(1285, 119)},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Release, Position: f32.Pt(1285, 119)},
	)
	frame()
	if model.inspectorTab != "properties" {
		t.Fatalf("点击属性页签没有切换：%q", model.inspectorTab)
	}
}

func TestPrimaryEditorButtonsKeepReceivingClicks(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{theme: material.NewTheme(), manifest: defaultManifest(), selected: 0, inspectorTab: "layers"}
	model.addLayer(caseTheme.LayerImage)
	model.selected = 0
	model.inspectorTab = "layers"
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(1500, 900)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	frame := func() { gtx.Reset(); model.layout(gtx); router.Frame(gtx.Ops) }
	click := func(x, y float32) {
		router.Queue(
			pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Press, Position: f32.Pt(x, y)},
			pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Release, Position: f32.Pt(x, y)},
		)
		frame()
	}
	frame()
	click(1330, 198)
	if model.selected != 1 {
		t.Fatalf("图层列表点击没有切换图片图层：%d hover=%v buttons=%d last=%v history=%+v", model.selected, model.layerButtons[1].click.Hovered(), len(model.layerButtons), model.lastPoint, model.layerButtons[1].click.History())
	}
	click(1285, 119)
	if model.inspectorTab != "properties" {
		t.Fatalf("属性页点击没有切换：%q", model.inspectorTab)
	}
	click(1400, 70)
	if !strings.Contains(model.message, "请先选择一台目标副屏") {
		t.Fatalf("应用当前画布按钮没有响应：%q", model.message)
	}
}

func TestTargetButtonClickSelectsDeviceAndLoadsTheme(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/theme" || request.URL.Query().Get("target") != "waveshare-7" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(writer).Encode(caseTheme.Manifest{ID: "waveshare-theme", Name: "微雪主题", Width: 1024, Height: 600})
	}))
	defer server.Close()

	var router input.Router
	var operations op.Ops
	model := &gioEditor{
		api:          NewAPI(server.URL),
		theme:        material.NewTheme(),
		manifest:     defaultManifest(),
		target:       "thermalright-kv",
		inspectorTab: "layers",
		targetOptions: []targetOption{
			{id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600}},
			{id: "thermalright-kv", label: "利民 KV", resolution: [2]int{1920, 468}},
		},
	}
	model.targetStatus.lastRefresh = time.Now()
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(1500, 900)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	frame := func() { gtx.Reset(); model.layout(gtx); router.Frame(gtx.Ops) }
	frame()
	router.Queue(
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Press, Position: f32.Pt(170, 40)},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Release, Position: f32.Pt(170, 40)},
	)
	frame()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("异步载入目标主题未返回")
	}
	if model.target != "waveshare-7" {
		t.Fatalf("目标按钮未切换设备：got %q, hover=(%v,%v), pressed=(%v,%v)", model.target, model.targetButtons[0].click.Hovered(), model.targetButtons[1].click.Hovered(), model.targetButtons[0].click.Pressed(), model.targetButtons[1].click.Pressed())
	}
	if model.manifest.ID != "waveshare-theme" {
		t.Fatalf("未载入目标副屏主题：got %q", model.manifest.ID)
	}
}

func TestTargetSelectionDoesNotBlockOnSlowThemeService(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/theme" || request.URL.Query().Get("target") != "waveshare-8.8" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		started <- struct{}{}
		<-release
		_ = json.NewEncoder(writer).Encode(caseTheme.Manifest{ID: "portrait", Width: 480, Height: 1920})
	}))
	defer server.Close()
	model := &gioEditor{
		api:      NewAPI(server.URL),
		target:   "waveshare-7",
		manifest: defaultManifestFor(1024, 600),
		targetOptions: []targetOption{
			{id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600}, waveshare: true},
			{id: "waveshare-8.8", label: "微雪 8.8 英寸", resolution: [2]int{480, 1920}, waveshare: true},
		},
	}
	startedAt := time.Now()
	model.selectTargetFromUI("waveshare-8.8")
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("目标点击被网络请求阻塞：%v", elapsed)
	}
	if model.target != "waveshare-7" || model.targetLoading != "waveshare-8.8" {
		t.Fatalf("慢请求期间不应提前覆盖当前画布：target=%q loading=%q", model.target, model.targetLoading)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("后台主题请求未启动")
	}
	close(release)
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("后台主题请求未完成")
	}
	if model.target != "waveshare-8.8" || model.manifest.ID != "portrait" || model.targetLoading != "" {
		t.Fatalf("异步载入结果没有切换到目标画布：target=%q theme=%q loading=%q", model.target, model.manifest.ID, model.targetLoading)
	}
}

func TestOldTargetLoadCallbackDoesNotOverwriteCurrentCanvas(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_ = json.NewEncoder(writer).Encode(caseTheme.Manifest{ID: "stale-theme", Width: 480, Height: 1920})
	}))
	defer server.Close()
	model := &gioEditor{
		api: NewAPI(server.URL), target: "waveshare-7",
		manifest: caseTheme.Manifest{ID: "editing", Width: 1024, Height: 600, Layers: defaultManifestFor(1024, 600).Layers},
		targetOptions: []targetOption{
			{id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600}, waveshare: true},
			{id: "waveshare-8.8", label: "微雪 8.8 英寸", resolution: [2]int{480, 1920}, waveshare: true},
		},
	}
	model.selectTargetFromUI("waveshare-8.8")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("旧目标请求未启动")
	}
	model.selectTargetFromUI("waveshare-7")
	messageAfterCancel := model.message
	close(release)
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("旧目标请求未完成")
	}
	if model.target != "waveshare-7" || model.manifest.ID != "editing" || model.message != messageAfterCancel {
		t.Fatalf("旧目标回调覆盖了当前画布：target=%q theme=%q message=%q", model.target, model.manifest.ID, model.message)
	}
}

func TestTargetButtonFrameUsesStableSnapshotWhenOptionsChange(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{
		theme: material.NewTheme(),
		targetOptions: []targetOption{
			{id: "screen-a", label: "A"},
			{id: "screen-b", label: "B"},
		},
	}
	frame := func() {
		operations.Reset()
		gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(320, 32)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
		views := model.targetButtonViews()
		views[0].action = func() { model.targetOptions = nil }
		model.layoutTargetButtonViews(gtx, views)
		router.Frame(&operations)
	}
	frame()
	router.Queue(
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Press, Position: f32.Pt(50, 16)},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Release, Position: f32.Pt(50, 16)},
	)
	frame()
	if len(model.targetOptions) != 0 {
		t.Fatal("测试动作没有在同一帧更新目标清单")
	}
}

func TestAddLayerAtUsesDropPointAndClampsToCanvas(t *testing.T) {
	model := &gioEditor{manifest: caseTheme.Manifest{Width: 1024, Height: 600}, selected: -1}
	model.addLayerAt(caseTheme.LayerTelemetry, .95, .95)
	if len(model.manifest.Layers) != 1 {
		t.Fatalf("未创建拖入的组件")
	}
	bounds := model.manifest.Layers[0].Bounds
	assertNear(t, bounds.X, .58)
	assertNear(t, bounds.Y, .80)
	assertNear(t, bounds.Width, .42)
	assertNear(t, bounds.Height, .20)
}

func TestAddLayerAtRejectsSecondCanvas(t *testing.T) {
	model := &gioEditor{manifest: defaultManifest()}
	before := len(model.manifest.Layers)
	model.addLayerAt(caseTheme.LayerCanvas, .5, .5)
	if len(model.manifest.Layers) != before {
		t.Fatalf("重复背景图层不应被创建")
	}
}

func TestHitLayerAndMoveSelectedPreferTopLayer(t *testing.T) {
	model := &gioEditor{manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{
		{ID: "bottom", Bounds: caseTheme.Rect{X: .1, Y: .1, Width: .6, Height: .6}, ZIndex: 1},
		{ID: "top", Bounds: caseTheme.Rect{X: .2, Y: .2, Width: .3, Height: .3}, ZIndex: 9},
	}}}
	selected := model.hitLayer(image.Pt(300, 300), image.Pt(1000, 1000))
	if selected != 1 {
		t.Fatalf("重叠图层未选中最上层：got %d", selected)
	}
	model.selected = selected
	model.componentLayerID = "top"
	model.moveSelectedBy(1, 1)
	bounds := model.manifest.Layers[selected].Bounds
	assertNear(t, bounds.X, .7)
	assertNear(t, bounds.Y, .7)
	if model.componentLayerID != "" {
		t.Fatalf("拖动后属性缓存未同步刷新")
	}
}

func TestComponentScaleAndLayerOrder(t *testing.T) {
	model := &gioEditor{manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{
		{ID: "canvas", Type: caseTheme.LayerCanvas, ZIndex: -100},
		{ID: "bars", Type: caseTheme.LayerAudioBars, Bounds: caseTheme.Rect{X: .2, Y: .2, Width: .2, Height: .2}, ZIndex: 10},
		{ID: "ring", Type: caseTheme.LayerAudioRing, Bounds: caseTheme.Rect{X: .3, Y: .3, Width: .2, Height: .2}, ZIndex: 20},
	}}, selected: 1}
	before := model.manifest.Layers[1].Bounds
	model.scaleSelected(-500, image.Pt(1000, 1000))
	if model.manifest.Layers[1].Bounds.Width <= before.Width || model.manifest.Layers[1].Bounds.Height <= before.Height {
		t.Fatalf("组件滚轮缩放没有生效：before=%+v after=%+v", before, model.manifest.Layers[1].Bounds)
	}
	model.moveSelectedLayer(1)
	if model.manifest.Layers[1].ZIndex != 20 || model.manifest.Layers[2].ZIndex != 10 {
		t.Fatalf("图层上移没有交换层级：%+v", model.manifest.Layers)
	}
	model.moveSelectedLayer(-1)
	if model.manifest.Layers[1].ZIndex != 10 || model.manifest.Layers[2].ZIndex != 20 {
		t.Fatalf("图层下移没有恢复层级：%+v", model.manifest.Layers)
	}
}

func TestHitLayerDoesNotTreatOutsidePanelAsCanvas(t *testing.T) {
	model := &gioEditor{manifest: defaultManifest(), selected: 0}
	if hit := model.hitLayer(image.Pt(500, 300), image.Pt(1000, 600)); hit != 0 {
		t.Fatalf("画布内部空白应选中画布背景：%d", hit)
	}
	if hit := model.hitLayer(image.Pt(1200, 300), image.Pt(1000, 600)); hit != -1 {
		t.Fatalf("画布外点击不应命中画布背景：%d", hit)
	}
}

func TestControlColorsProvideDistinctFeedback(t *testing.T) {
	foreground := color.NRGBA{R: 220, G: 230, B: 235, A: 255}
	normalBackground, normalBorder, _ := controlColors(foreground, false, false, false)
	activeBackground, activeBorder, _ := controlColors(foreground, true, false, false)
	hoverBackground, hoverBorder, _ := controlColors(foreground, false, true, false)
	pressedBackground, pressedBorder, pressedForeground := controlColors(foreground, false, true, true)
	if normalBackground == activeBackground || normalBorder == activeBorder {
		t.Fatalf("选中状态没有独立反馈")
	}
	if normalBackground == hoverBackground || normalBorder == hoverBorder {
		t.Fatalf("悬停状态没有独立反馈")
	}
	if hoverBackground == pressedBackground || hoverBorder == pressedBorder || pressedForeground == foreground {
		t.Fatalf("按下状态没有独立反馈")
	}
}

func TestPaletteSingleClickCreatesOneLayer(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{
		theme:         material.NewTheme(),
		manifest:      caseTheme.Manifest{ID: "click-test", Width: 1024, Height: 600},
		target:        "waveshare-7",
		inspectorTab:  "layers",
		targetOptions: []targetOption{{id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600}}},
	}
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(1500, 900)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	frame := func() { gtx.Reset(); model.layout(gtx); router.Frame(gtx.Ops) }
	frame()
	telemetryPoint := findPaletteButtonPoint(t, model, &router, frame, caseTheme.LayerTelemetry, 900)
	router.Queue(
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Press, Position: telemetryPoint},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Release, Position: telemetryPoint},
	)
	frame()
	if len(model.manifest.Layers) != 1 || model.manifest.Layers[0].Type != caseTheme.LayerTelemetry {
		t.Fatalf("组件按钮单击未创建唯一遥测图层：%+v", model.manifest.Layers)
	}
}

func TestPaletteDragCreatesLayerAndCanvasDragMovesIt(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{
		theme:         material.NewTheme(),
		manifest:      defaultManifest(),
		target:        "waveshare-7",
		inspectorTab:  "layers",
		targetOptions: []targetOption{{id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600}}},
	}
	gtx := layout.Context{
		Ops:         &operations,
		Source:      router.Source(),
		Constraints: layout.Exact(image.Pt(1500, 900)),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
	}
	frame := func() {
		gtx.Reset()
		model.layout(gtx)
		router.Frame(gtx.Ops)
	}
	frame()
	telemetryPoint := findPaletteButtonPoint(t, model, &router, frame, caseTheme.LayerTelemetry, 900)

	// 从左侧“遥测”按钮拖到画布中心。
	router.Queue(
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Press, Position: telemetryPoint},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Move, Position: telemetryPoint},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Release, Position: f32.Pt(698, 477)},
	)
	frame()
	frame()
	if len(model.manifest.Layers) != 2 {
		t.Fatalf("组件拖入画布未创建图层：got %d", len(model.manifest.Layers))
	}
	created := model.manifest.Layers[1]
	if created.Type != caseTheme.LayerTelemetry {
		t.Fatalf("拖入类型错误：got %s", created.Type)
	}

	beforeX, beforeY := created.Bounds.X, created.Bounds.Y
	router.Queue(
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Press, Position: f32.Pt(698, 477)},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Move, Position: f32.Pt(758, 517)},
		pointer.Event{Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Kind: pointer.Release, Position: f32.Pt(758, 517)},
	)
	frame()
	if model.manifest.Layers[1].Bounds.X <= beforeX || model.manifest.Layers[1].Bounds.Y <= beforeY {
		t.Fatalf("画布拖动未改变图层位置：before=(%.3f, %.3f), after=(%.3f, %.3f)", beforeX, beforeY, model.manifest.Layers[1].Bounds.X, model.manifest.Layers[1].Bounds.Y)
	}
}

func findPaletteButtonPoint(t *testing.T, model *gioEditor, router *input.Router, frame func(), layerType caseTheme.LayerType, height int) f32.Point {
	t.Helper()
	const x = float32(116)
	for y := 0; y < height; y += 4 {
		point := f32.Pt(x, float32(y))
		router.Queue(pointer.Event{Source: pointer.Mouse, Kind: pointer.Move, Position: point})
		frame()
		for index := range model.paletteButtons {
			button := &model.paletteButtons[index]
			if button.layerType == layerType && button.drag.Hovered() {
				return point
			}
		}
	}
	t.Fatalf("未在组件库中找到 %s 按钮", layerType)
	return f32.Point{}
}

func TestImportBackgroundPathUpdatesSelectedLayer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "背景.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 320, 160))); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	model := &gioEditor{manifest: defaultManifest(), selected: 0, imageCache: make(map[string]image.Image)}
	model.addLayer(caseTheme.LayerImage)
	model.importBackgroundPath(path)
	if len(model.manifest.Resources) != 1 {
		t.Fatalf("背景文件未加入主题资源：got %d", len(model.manifest.Resources))
	}
	layer := &model.manifest.Layers[1]
	if layer.Type != caseTheme.LayerImage {
		t.Fatalf("图片图层类型错误：got %s", layer.Type)
	}
	if actual := model.backgroundResourcePath(layer); actual != path {
		t.Fatalf("背景图层未绑定所选文件：got %q, want %q", actual, path)
	}
}

func TestImageImportKeepsOriginalPixelsAndLargeImageFitsProportionally(t *testing.T) {
	writePNG := func(name string, width, height int) string {
		path := filepath.Join(t.TempDir(), name)
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		return path
	}
	model := &gioEditor{manifest: defaultManifestFor(1024, 600), selected: 0, imageCache: make(map[string]image.Image)}
	model.addLayer(caseTheme.LayerImage)
	model.importBackgroundPath(writePNG("small.png", 320, 160))
	small := model.manifest.Layers[1].Bounds
	assertNear(t, small.Width*1024, 320)
	assertNear(t, small.Height*600, 160)
	assertNear(t, small.X+small.Width/2, .5)
	assertNear(t, small.Y+small.Height/2, .5)

	model.importBackgroundPath(writePNG("large.png", 2000, 1000))
	large := model.manifest.Layers[1].Bounds
	assertNear(t, large.Width*1024, 1024)
	assertNear(t, large.Height*600, 512)
	assertNear(t, large.Width/large.Height, (2000.0/1000.0)/(1024.0/600.0))
}

func TestImageWheelScaleRotationAndUndoRedo(t *testing.T) {
	model := &gioEditor{manifest: defaultManifestFor(1000, 500), selected: 0}
	model.addLayer(caseTheme.LayerImage)
	model.manifest.Layers[1].Bounds = caseTheme.Rect{X: .4, Y: .3, Width: .2, Height: .4}
	before := model.manifest.Layers[1].Bounds
	model.scaleSelected(-120, image.Pt(1000, 500))
	after := model.manifest.Layers[1].Bounds
	if after.Width <= before.Width || after.Height <= before.Height {
		t.Fatalf("滚轮向上没有放大图片：before=%+v after=%+v", before, after)
	}
	assertNear(t, after.Width/after.Height, before.Width/before.Height)
	model.manifest.Layers[1].Rotation = normalizeDegrees(225)
	assertNear(t, model.manifest.Layers[1].Rotation, -135)
	model.undo()
	if model.manifest.Layers[1].Bounds != before {
		t.Fatalf("Ctrl+Z 对应的撤销没有恢复缩放：%+v", model.manifest.Layers[1].Bounds)
	}
	model.redo()
	if model.manifest.Layers[1].Bounds != after {
		t.Fatalf("重做没有恢复缩放：%+v", model.manifest.Layers[1].Bounds)
	}
}

func TestDeleteRemovesImageButKeepsCanvas(t *testing.T) {
	model := &gioEditor{manifest: defaultManifest(), selected: 0}
	model.removeLayer()
	if len(model.manifest.Layers) != 1 {
		t.Fatal("画布背景不应被删除")
	}
	model.addLayer(caseTheme.LayerImage)
	model.removeLayer()
	if len(model.manifest.Layers) != 1 || model.manifest.Layers[0].Type != caseTheme.LayerCanvas {
		t.Fatalf("Delete 应只删除选中图片：%+v", model.manifest.Layers)
	}
	model.undo()
	if len(model.manifest.Layers) != 2 || model.manifest.Layers[1].Type != caseTheme.LayerImage {
		t.Fatalf("删除操作无法撤销：%+v", model.manifest.Layers)
	}
}

func TestWindowShortcutsUndoRedoAndDelete(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{theme: material.NewTheme(), manifest: defaultManifest(), selected: 0, inspectorTab: "layers"}
	model.addLayer(caseTheme.LayerImage)
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(1500, 900)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	frame := func() { gtx.Reset(); model.layout(gtx); router.Frame(gtx.Ops) }
	frame()
	router.Queue(key.Event{Name: "Z", Modifiers: key.ModCtrl, State: key.Press})
	frame()
	if len(model.manifest.Layers) != 1 {
		t.Fatalf("Ctrl+Z 没有撤销添加图片：%+v", model.manifest.Layers)
	}
	router.Queue(key.Event{Name: "Y", Modifiers: key.ModCtrl, State: key.Press})
	frame()
	if len(model.manifest.Layers) != 2 {
		t.Fatalf("Ctrl+Y 没有重做添加图片：%+v", model.manifest.Layers)
	}
	router.Queue(key.Event{Name: key.NameDeleteForward, State: key.Press})
	frame()
	if len(model.manifest.Layers) != 1 {
		t.Fatalf("Delete 没有删除选中图片：%+v", model.manifest.Layers)
	}
}

func TestDefaultManifestStartsWithBackgroundOnly(t *testing.T) {
	manifest := defaultManifestFor(480, 480)
	if manifest.Width != 480 || manifest.Height != 480 || len(manifest.Layers) != 1 {
		t.Fatalf("新主题没有按设备分辨率建立纯背景：%dx%d layers=%d", manifest.Width, manifest.Height, len(manifest.Layers))
	}
	if manifest.Layers[0].Type != caseTheme.LayerCanvas {
		t.Fatalf("新主题不应自动包含指标：%+v", manifest.Layers)
	}
}

func TestAdaptManifestToTargetMigratesBackgroundWithoutStretchingImage(t *testing.T) {
	model := &gioEditor{
		target: "thermalright-kv",
		targetOptions: []targetOption{{
			id: "thermalright-kv", label: "利民 KV", resolution: [2]int{1920, 468},
		}},
		manifest: caseTheme.Manifest{Width: 1024, Height: 600, Layers: []caseTheme.Layer{{
			ID: "background", Type: caseTheme.LayerBackgroundImage,
			Bounds: caseTheme.Rect{X: .2, Y: .2, Width: .5, Height: .5}, Opacity: .6,
		}}},
	}
	model.adaptManifestToTarget()
	canvas, picture := model.manifest.Layers[0], model.manifest.Layers[1]
	if model.manifest.Width != 1920 || model.manifest.Height != 468 {
		t.Fatalf("主题未使用目标屏分辨率：%dx%d", model.manifest.Width, model.manifest.Height)
	}
	if canvas.Type != caseTheme.LayerCanvas || canvas.Bounds != (caseTheme.Rect{Width: 1, Height: 1}) {
		t.Fatalf("没有建立独立画布背景：%+v", canvas)
	}
	if picture.Type != caseTheme.LayerImage || picture.Bounds != (caseTheme.Rect{X: .2, Y: .2, Width: .5, Height: .5}) || picture.Opacity != .6 {
		t.Fatalf("旧背景图片迁移时被拉伸：%+v", picture)
	}
}

func TestClientRejectsAllScreenThemeOperations(t *testing.T) {
	api := NewAPI("http://127.0.0.1:1")
	if _, err := api.Theme("all"); err == nil {
		t.Fatal("读取主题不应再接受全部副屏")
	}
	if _, err := api.ApplyTheme(defaultManifest(), "all"); err == nil {
		t.Fatal("应用主题不应再接受全部副屏")
	}
}

func TestLyricsOptionsBelongToSelectedLyricsLayer(t *testing.T) {
	model := &gioEditor{
		manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{
			{ID: "background", Type: caseTheme.LayerBackgroundImage, Options: map[string]any{}},
			{ID: "lyrics", Type: caseTheme.LayerLyrics, Options: map[string]any{}},
		}},
		selected:              1,
		lyricsDefaultProvider: "auto",
	}
	model.ensureLyricsFields(&model.manifest.Layers[1])
	model.lyricsProvider.set("local")
	model.lyricsPath.set(`D:\Music\当前歌曲.lrc`)
	if !model.updateSelectedLyricsOptions() {
		t.Fatalf("选中歌词图层后未保存属性")
	}
	lyrics := &model.manifest.Layers[1]
	if layerOptionString(lyrics, "provider") != "local" || layerOptionString(lyrics, "lrc_path") != `D:\Music\当前歌曲.lrc` {
		t.Fatalf("歌词属性未写入选中图层：%+v", lyrics.Options)
	}
	if len(model.manifest.Layers[0].Options) != 0 {
		t.Fatalf("歌词属性不应写入背景图层：%+v", model.manifest.Layers[0].Options)
	}
}

func TestLyricsSaveIsAsyncAndOldCallbackDoesNotOverwriteCurrentSelection(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*gioEditor)
		wantApplied bool
	}{
		{name: "当前图层保持不变", wantApplied: true},
		{name: "目标已切换", mutate: func(model *gioEditor) { model.target = "waveshare-8.8" }},
		{name: "图层已切换", mutate: func(model *gioEditor) { model.manifest.Layers[0].ID = "lyrics-b" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/config" {
					http.Error(writer, "unexpected request", http.StatusBadRequest)
					return
				}
				writer.Header().Set("Content-Type", "application/json; charset=utf-8")
				switch request.Method {
				case http.MethodGet:
					started <- struct{}{}
					<-release
					_, _ = writer.Write([]byte(`{}`))
				case http.MethodPut:
					_, _ = writer.Write([]byte(`{"restart_required":false}`))
				default:
					http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
				}
			}))
			model := &gioEditor{
				api:                   NewAPI(server.URL),
				target:                "waveshare-7",
				selected:              0,
				uiUpdates:             make(chan func(), 2),
				lyricsDefaultProvider: "auto",
				lyricsDefaultPath:     "旧路径",
				manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{{
					ID: "lyrics-a", Type: caseTheme.LayerLyrics, Options: map[string]any{},
				}}},
			}
			model.api.setToken("desktop-token")
			model.lyricsProvider.set("netease")
			model.lyricsPath.set(`D:\Music\当前歌曲.lrc`)
			startedAt := time.Now()
			model.saveLyrics()
			if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
				t.Fatalf("歌词保存阻塞了界面线程：%v", elapsed)
			}
			if !model.lyricsSaveRunning {
				t.Fatal("歌词保存期间未进入运行状态")
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("后台歌词保存请求未启动")
			}
			if test.mutate != nil {
				test.mutate(model)
				model.lyricsDefaultProvider = "qq"
				model.lyricsDefaultPath = "切换后路径"
				model.message = "切换后的状态"
			}
			close(release)
			select {
			case update := <-model.uiUpdates:
				update()
			case <-time.After(time.Second):
				t.Fatal("后台歌词保存请求未完成")
			}
			server.Close()
			if model.lyricsSaveRunning {
				t.Fatal("歌词保存完成后仍处于运行状态")
			}
			if test.wantApplied {
				if model.lyricsDefaultProvider != "netease" || model.lyricsDefaultPath != `D:\Music\当前歌曲.lrc` || model.message != "歌词图层设置已保存" {
					t.Fatalf("当前歌词图层没有接收保存结果：provider=%q path=%q message=%q", model.lyricsDefaultProvider, model.lyricsDefaultPath, model.message)
				}
			} else if model.lyricsDefaultProvider != "qq" || model.lyricsDefaultPath != "切换后路径" || model.message != "切换后的状态" {
				t.Fatalf("旧歌词回调污染了已切换界面：provider=%q path=%q message=%q", model.lyricsDefaultProvider, model.lyricsDefaultPath, model.message)
			}
		})
	}
}

func TestApplyThemeRunsAsynchronouslyAndUpdatesResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/api/theme" || request.URL.Query().Get("target") != "waveshare-7" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		time.Sleep(30 * time.Millisecond)
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(writer).Encode(map[string]any{"device_results": map[string]string{"waveshare-7": "ok"}})
	}))
	defer server.Close()

	model := &gioEditor{
		api:       NewAPI(server.URL),
		manifest:  defaultManifest(),
		target:    "waveshare-7",
		uiUpdates: make(chan func(), 1),
	}
	model.api.setToken("desktop-token")
	started := time.Now()
	model.applyTheme()
	if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
		t.Fatalf("应用主题阻塞了界面线程：耗时 %v", elapsed)
	}
	if !model.applyRunning {
		t.Fatalf("应用主题期间未显示运行状态")
	}
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("异步应用主题未返回")
	}
	if model.applyRunning || model.message != "主题已应用并记住：waveshare-7:ok" {
		t.Fatalf("应用结果状态错误：running=%v message=%q", model.applyRunning, model.message)
	}
}

func TestApplyThemeRefreshesWaveshareActiveTemplateState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "PUT /api/theme":
			_ = json.NewEncoder(writer).Encode(map[string]any{"device_results": map[string]string{"waveshare-7": "ok"}})
		case "GET /api/waveshare/templates":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"target": "waveshare-7", "active_template_id": "",
				"templates": []map[string]any{{"id": "night", "name": "夜间"}},
			})
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), target: "waveshare-7", manifest: defaultManifest(), uiUpdates: make(chan func(), 2)}
	model.api.setToken("desktop-token")
	model.waveshareLibrary.loadedTarget = "waveshare-7"
	model.waveshareLibrary.activeTemplateID = "night"
	model.applyTheme()
	applyNextUIUpdate(t, model)
	applyNextUIUpdate(t, model)
	if model.waveshareLibrary.activeTemplateID != "" {
		t.Fatalf("普通应用后仍错误标记旧模板为已激活：%q", model.waveshareLibrary.activeTemplateID)
	}
	if len(model.waveshareLibrary.templates) != 1 || model.waveshareLibrary.templates[0].ID != "night" {
		t.Fatalf("模板激活态刷新没有保留服务模板清单：%+v", model.waveshareLibrary.templates)
	}
	if model.message != "主题已应用并记住：waveshare-7:ok" {
		t.Fatalf("后台激活态刷新不应覆盖应用结果提示：%q", model.message)
	}
}

func TestWaveshareExtendedComponentsHaveEditableDefaults(t *testing.T) {
	model := &gioEditor{
		target:   "waveshare-7",
		manifest: defaultManifestFor(1024, 600),
	}
	types := []caseTheme.LayerType{
		caseTheme.LayerText, caseTheme.LayerClock, caseTheme.LayerProgressBar, caseTheme.LayerGauge,
		caseTheme.LayerGradientGauge, caseTheme.LayerLineChart, caseTheme.LayerShape,
	}
	for _, layerType := range types {
		model.addLayer(layerType)
		layer := model.manifest.Layers[model.selected]
		if layer.Type != layerType {
			t.Fatalf("新增微雪组件类型错误：got %s, want %s", layer.Type, layerType)
		}
		if !caseTheme.LayerVisible(layer) {
			t.Fatalf("新增组件 %s 不应默认隐藏", layerType)
		}
		if layer.Bounds.Width <= 0 || layer.Bounds.Height <= 0 {
			t.Fatalf("新增组件 %s 没有可编辑尺寸：%+v", layerType, layer.Bounds)
		}
		if layer.ID == "" {
			t.Fatalf("新增组件 %s 没有稳定 ID", layerType)
		}
	}
	ids := make(map[string]bool)
	for _, layer := range model.manifest.Layers {
		if ids[layer.ID] {
			t.Fatalf("连续添加组件产生重复 ID：%s", layer.ID)
		}
		ids[layer.ID] = true
	}

	assertOptions := func(layerType caseTheme.LayerType, keys ...string) {
		t.Helper()
		options := model.defaultWaveshareOptions(layerType)
		for _, key := range keys {
			if _, ok := options[key]; !ok {
				t.Fatalf("组件 %s 缺少可编辑选项 %q：%+v", layerType, key, options)
			}
		}
	}
	assertOptions(caseTheme.LayerText, "text", "font_family", "font_size", "font_weight", "color", "panel_color")
	assertOptions(caseTheme.LayerClock, "format", "font_family", "font_size", "color")
	assertOptions(caseTheme.LayerProgressBar, "data_key", "min_value", "max_value", "style", "color")
	assertOptions(caseTheme.LayerGauge, "data_key", "min_value", "max_value", "start_angle", "end_angle", "stroke_width")
	assertOptions(caseTheme.LayerGradientGauge, "gradient_start", "gradient_mid", "gradient_end")
	assertOptions(caseTheme.LayerLineChart, "data_key", "history_points", "min_value", "max_value", "style")
	assertOptions(caseTheme.LayerShape, "shape", "fill_color", "stroke_color", "stroke_width", "corner_radius")
}

func TestExtendedWaveshareComponentsDoNotChangeOtherTargets(t *testing.T) {
	model := &gioEditor{target: "thermalright-kv", manifest: defaultManifestFor(1920, 468)}
	before := len(model.manifest.Layers)
	model.addLayer(caseTheme.LayerGauge)
	if len(model.manifest.Layers) != before {
		t.Fatalf("利民目标不应添加微雪原生组件：%+v", model.manifest.Layers)
	}
	if !strings.Contains(model.message, "微雪") {
		t.Fatalf("拒绝时没有给出明确提示：%q", model.message)
	}
}

func TestWavesharePaletteExpansionIsTargetScoped(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{theme: material.NewTheme(), target: "waveshare-7", manifest: defaultManifestFor(1024, 600)}
	gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(210, 760)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	model.layoutPalette(gtx)
	if len(model.paletteButtons) != 13 {
		t.Fatalf("微雪组件库数量不完整：got %d, want 13", len(model.paletteButtons))
	}
	router.Frame(&operations)
	model.target = "coolermaster-atmos-v2"
	operations.Reset()
	gtx = layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(210, 760)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	model.layoutPalette(gtx)
	if len(model.paletteButtons) != 6 {
		t.Fatalf("酷冷组件库不应被微雪扩展改动：got %d, want 6", len(model.paletteButtons))
	}
}

func TestWavesharePixelGeometryRoundTripsExactly(t *testing.T) {
	model := &gioEditor{
		target:   "waveshare-7",
		manifest: defaultManifestFor(1024, 600),
	}
	model.addLayer(caseTheme.LayerTelemetry)
	layer := &model.manifest.Layers[model.selected]
	fields := model.componentFieldValues(layer)
	if got := []string{fields[0].label, fields[1].label, fields[2].label, fields[3].label}; !reflect.DeepEqual(got, []string{"X (px)", "Y (px)", "宽度 (px)", "高度 (px)"}) {
		t.Fatalf("微雪几何属性应使用像素单位：%v", got)
	}
	fields[2].apply(317)
	fields[3].apply(149)
	fields = model.componentFieldValues(layer)
	fields[0].apply(233)
	fields[1].apply(181)
	if rect := model.layerRect(*layer, image.Pt(1024, 600)); rect != image.Rect(233, 181, 550, 330) {
		t.Fatalf("像素属性到预览坐标未逐像素一致：%v", rect)
	}
	fields = model.componentFieldValues(layer)
	if fields[0].value != 233 || fields[1].value != 181 || fields[2].value != 317 || fields[3].value != 149 {
		t.Fatalf("像素坐标回读失真：%+v", fields[:4])
	}

	portrait := &gioEditor{target: "waveshare-8.8", manifest: defaultManifestFor(480, 1920)}
	portrait.addLayer(caseTheme.LayerGauge)
	portraitLayer := &portrait.manifest.Layers[portrait.selected]
	portraitFields := portrait.componentFieldValues(portraitLayer)
	portraitFields[2].apply(201)
	portraitFields[3].apply(407)
	portraitFields = portrait.componentFieldValues(portraitLayer)
	portraitFields[0].apply(117)
	portraitFields[1].apply(1301)
	if rect := portrait.layerRect(*portraitLayer, image.Pt(480, 1920)); rect != image.Rect(117, 1301, 318, 1708) {
		t.Fatalf("竖向 8.8 英寸坐标回路不一致：%v", rect)
	}
}

func TestWaveshareLayerVisibilityPreservesGeometryAndOpacity(t *testing.T) {
	model := &gioEditor{target: "waveshare-7", manifest: defaultManifestFor(1024, 600)}
	model.addLayer(caseTheme.LayerTelemetry)
	index := model.selected
	beforeBounds := model.manifest.Layers[index].Bounds
	model.manifest.Layers[index].Opacity = .63
	model.toggleLayerVisibility(index)
	if caseTheme.LayerVisible(model.manifest.Layers[index]) {
		t.Fatal("点击隐藏后图层仍可见")
	}
	if model.manifest.Layers[index].Bounds != beforeBounds || model.manifest.Layers[index].Opacity != .63 {
		t.Fatalf("隐藏图层不应改写坐标或透明度：%+v", model.manifest.Layers[index])
	}
	point := image.Pt(int((beforeBounds.X+beforeBounds.Width/2)*1024), int((beforeBounds.Y+beforeBounds.Height/2)*600))
	if hit := model.hitLayer(point, image.Pt(1024, 600)); hit == index {
		t.Fatalf("隐藏图层仍拦截了画布命中：%d", hit)
	}
	model.toggleLayerVisibility(index)
	if !caseTheme.LayerVisible(model.manifest.Layers[index]) || model.manifest.Layers[index].Opacity != .63 {
		t.Fatal("重新显示没有恢复原图层状态")
	}
}

func TestWaveshareAudioDefaultsExposeDynamicsControls(t *testing.T) {
	model := &gioEditor{target: "waveshare-7"}
	for _, layerType := range []caseTheme.LayerType{caseTheme.LayerAudioBars, caseTheme.LayerAudioRing} {
		options := model.defaultWaveshareOptions(layerType)
		for _, key := range []string{"source", "sensitivity", "gain", "min_amplitude", "max_amplitude", "smoothing", "noise_gate", "contrast", "separation", "bass_boost", "floor_reduction", "count", "thickness", "direction", "gradient_start", "gradient_end", "glow", "glow_radius"} {
			if _, ok := options[key]; !ok {
				t.Fatalf("%s 缺少音频动态控制 %q：%+v", layerType, key, options)
			}
		}
		if minimum, maximum := caseTheme.OptionFloat(options, "min_amplitude", -1), caseTheme.OptionFloat(options, "max_amplitude", -1); minimum < 0 || maximum > 1 || minimum > maximum {
			t.Fatalf("%s 默认幅度范围无效：%.2f..%.2f", layerType, minimum, maximum)
		}
	}
	lyrics := model.defaultWaveshareOptions(caseTheme.LayerLyrics)
	for _, key := range []string{"style", "font_family", "font_weight", "current_color", "next_color", "accent_color", "outline_width", "shadow", "glow_radius", "karaoke_highlight", "smooth_transition", "show_progress"} {
		if _, ok := lyrics[key]; !ok {
			t.Fatalf("歌词默认视觉属性缺少 %q：%+v", key, lyrics)
		}
	}
	if lyrics["style"] != "player" || lyrics["karaoke_highlight"] != true {
		t.Fatalf("旧歌词没有采用播放器视觉默认值：%+v", lyrics)
	}
}

func TestBarsPropertiesExposeFourwayBurstChoices(t *testing.T) {
	hasChoice := func(choices []optionChoice, value string) bool {
		for _, choice := range choices {
			if choice.value == value {
				return true
			}
		}
		return false
	}
	if !hasChoice(waveshareBarsStyleChoices, "burst") || !hasChoice(waveshareBarsStyleChoices, "waveform") || !hasChoice(waveshareBarsDirectionChoices, "fourway") {
		t.Fatalf("条状频谱属性没有暴露四向爆发入口：styles=%+v directions=%+v", waveshareBarsStyleChoices, waveshareBarsDirectionChoices)
	}
}

func TestWaveshareTextAndNumberOptionsWriteBackToManifest(t *testing.T) {
	var router input.Router
	var operations op.Ops
	model := &gioEditor{
		theme:         material.NewTheme(),
		target:        "waveshare-7",
		selected:      0,
		optionEditors: make(map[string]*textField),
		manifest: caseTheme.Manifest{Width: 1024, Height: 600, Layers: []caseTheme.Layer{{
			ID: "text-1", Type: caseTheme.LayerText, Opacity: 1,
			Bounds:  caseTheme.Rect{X: .1, Y: .1, Width: .4, Height: .2},
			Options: map[string]any{"text": "旧标题", "font_size": 20.0},
		}}},
	}
	layoutFrame := func(widget layout.Widget) {
		operations.Reset()
		gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(330, 40)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
		widget(gtx)
		router.Frame(&operations)
	}

	layer := &model.manifest.Layers[0]
	layoutFrame(func(gtx layout.Context) layout.Dimensions {
		return model.layoutTextOption(gtx, layer, "text", "文字", "自定义文本")
	})
	model.optionEditor(layer, "text", "自定义文本").editor.SetText("新标题")
	layoutFrame(func(gtx layout.Context) layout.Dimensions {
		return model.layoutTextOption(gtx, layer, "text", "文字", "自定义文本")
	})
	if got := layerOptionString(layer, "text"); got != "新标题" {
		t.Fatalf("文本属性没有写回 manifest：got %q", got)
	}

	layoutFrame(func(gtx layout.Context) layout.Dimensions {
		return model.layoutNumberOption(gtx, layer, "font_size", "字号", 20, 6, 200, false)
	})
	model.optionEditor(layer, "font_size", "20").editor.SetText("48")
	layoutFrame(func(gtx layout.Context) layout.Dimensions {
		return model.layoutNumberOption(gtx, layer, "font_size", "字号", 20, 6, 200, false)
	})
	if got := caseTheme.OptionFloat(layer.Options, "font_size", 0); got != 48 {
		t.Fatalf("数值属性没有写回 manifest：got %.1f", got)
	}
	if len(model.undoStack) != 2 {
		t.Fatalf("两次属性编辑都应可撤销：undo=%d", len(model.undoStack))
	}
}

func TestWaveshareExtendedPropertyPanelsAndPreviewsLayout(t *testing.T) {
	types := []caseTheme.LayerType{
		caseTheme.LayerTelemetry, caseTheme.LayerAudioBars, caseTheme.LayerAudioRing,
		caseTheme.LayerText, caseTheme.LayerClock, caseTheme.LayerProgressBar, caseTheme.LayerGauge,
		caseTheme.LayerGradientGauge, caseTheme.LayerLineChart, caseTheme.LayerShape,
	}
	for _, layerType := range types {
		var router input.Router
		var operations op.Ops
		model := &gioEditor{
			theme:         material.NewTheme(),
			target:        "waveshare-7",
			manifest:      defaultManifestFor(1024, 600),
			inspectorTab:  "properties",
			optionEditors: make(map[string]*textField),
		}
		model.propertyList.Axis = layout.Vertical
		model.addLayer(layerType)
		gtx := layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(330, 640)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
		model.propertyList.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
			return model.layoutComponentFields(gtx)
		})
		router.Frame(&operations)

		operations.Reset()
		gtx = layout.Context{Ops: &operations, Source: router.Source(), Constraints: layout.Exact(image.Pt(640, 375)), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
		model.drawCanvasBackground(gtx, image.Pt(640, 375))
		model.drawLayers(gtx, image.Pt(640, 375))
		router.Frame(&operations)
	}
}

func TestWaveshareTelemetryPreviewUsesCompilerGridAndValueSwitches(t *testing.T) {
	fields := waveshareTelemetryPreviewFields(map[string]any{
		"fields": []any{"Memory_Load", "Cpu_Freq", "Gpu_Load", "GPU_Temp", "Memory_Used"},
	})
	if got, want := fields, []string{"Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp", "Memory_Used"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("预览字段顺序应与编译器一致：got %v, want %v", got, want)
	}
	for _, test := range []struct {
		style   string
		count   int
		columns int
		rows    int
	}{
		{style: "dials", count: 5, columns: 4, rows: 2},
		{style: "compact", count: 7, columns: 6, rows: 2},
		{style: "tiles", count: 5, columns: 3, rows: 2},
		{style: "list", count: 5, columns: 1, rows: 5},
	} {
		columns, rows := waveshareTelemetryPreviewGrid(test.style, test.count)
		if columns != test.columns || rows != test.rows {
			t.Fatalf("%s 预览网格与编译器不一致：got %dx%d, want %dx%d", test.style, columns, rows, test.columns, test.rows)
		}
	}
	if got := waveshareTelemetryPreviewValue("45℃", false); got != "45" {
		t.Fatalf("关闭单位后预览仍显示单位：%q", got)
	}
	if got := waveshareTelemetryPreviewValue("18.6GB", true); got != "18.6GB" {
		t.Fatalf("开启单位后预览丢失单位：%q", got)
	}
	// 430×120、5 项的 digital 网格单元为约 107×60；标签占位后数值字号必须自动收敛。
	if got := waveshareFitTelemetryPreviewText("42%", 40, 95, 30); got > 25 {
		t.Fatalf("小尺寸 digital 数值字号未按高度自适应：got %.2f, want <= 25", got)
	}
	if got := waveshareFitTelemetryPreviewText("Intel Core Ultra 9 285K", 40, 95, 30); got >= 10 {
		t.Fatalf("长文本字号未按宽度自适应：got %.2f, want < 10", got)
	}
	digital := caseTheme.Layer{Options: map[string]any{"font_family": "digital", "data_key": "Cpu_Freq"}}
	labelLayer := waveshareTelemetryPreviewLabelLayer(digital)
	if got := layerOptionString(&labelLayer, "font_family"); got != "source-han" {
		t.Fatalf("数字遥测标签未切换到中文字库：%q", got)
	}
	if got := layerOptionString(&digital, "font_family"); got != "digital" {
		t.Fatalf("标签字体适配不应改变数值字体：%q", got)
	}
	if got := layerOptionString(&labelLayer, "data_key"); got != "Cpu_Freq" {
		t.Fatalf("标签字体适配不应丢失其他选项：%q", got)
	}
}

func TestWaveshareVideoLayerIsNormalizedAndUnique(t *testing.T) {
	model := &gioEditor{target: "waveshare-7", manifest: defaultManifestFor(1024, 600)}
	model.addLayerAt(caseTheme.LayerBackgroundVideo, .25, .75)
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		t.Fatal("添加视频后没有选中视频图层")
	}
	video := model.manifest.Layers[model.selected]
	if video.Bounds != (caseTheme.Rect{Width: 1, Height: 1}) || video.Opacity != 1 || video.Rotation != 0 || video.ZIndex != -9000 {
		t.Fatalf("微雪视频未固定为全屏最底层：%+v", video)
	}
	before := len(model.manifest.Layers)
	model.addLayerAt(caseTheme.LayerBackgroundVideo, .5, .5)
	if len(model.manifest.Layers) != before {
		t.Fatalf("微雪不应创建第二个可见视频图层：got %d, want %d", len(model.manifest.Layers), before)
	}
}

func TestCanvasResizeHandlesChangeWidthAndHeightIndependently(t *testing.T) {
	model := &gioEditor{
		target:   "waveshare-7",
		manifest: defaultManifestFor(1000, 500),
		selected: 1,
	}
	model.manifest.Layers = append(model.manifest.Layers, caseTheme.Layer{
		ID: "telemetry-1", Type: caseTheme.LayerTelemetry,
		Bounds: caseTheme.Rect{X: .2, Y: .2, Width: .4, Height: .3}, Opacity: 1,
		Options: map[string]any{"visible": true},
	})
	canvasSize := image.Pt(1000, 500)
	if handle := model.resizeHandleHit(image.Pt(600, 175), canvasSize); handle != "e" {
		t.Fatalf("没有命中右侧缩放手柄：got %q", handle)
	}
	model.beginHistoryGesture()
	model.beginResize("e")
	model.dragMode, model.dragging = "resize-e", true
	model.resizeSelectedTo(image.Pt(800, 175), canvasSize)
	layer := model.manifest.Layers[1]
	assertGeometryNear(t, layer.Bounds.X, .2)
	assertGeometryNear(t, layer.Bounds.Width, .6)
	assertGeometryNear(t, layer.Bounds.Height, .3)
	if len(model.undoStack) != 1 {
		t.Fatalf("一次缩放手势应只生成一个撤销快照：got %d", len(model.undoStack))
	}

	model.dragging, model.dragMode, model.resizeHandle, model.historyGesture = false, "", "", false
	model.beginHistoryGesture()
	model.beginResize("s")
	model.dragMode, model.dragging = "resize-s", true
	model.resizeSelectedTo(image.Pt(500, 350), canvasSize)
	layer = model.manifest.Layers[1]
	assertGeometryNear(t, layer.Bounds.Width, .6)
	assertGeometryNear(t, layer.Bounds.Height, .5)
	if len(model.undoStack) != 2 {
		t.Fatalf("第二次缩放手势没有独立撤销快照：got %d", len(model.undoStack))
	}
}

func TestPreviewAnimationIntervalOnlyRefreshesVisibleDynamicLayers(t *testing.T) {
	model := &gioEditor{manifest: defaultManifestFor(1024, 600)}
	if got := model.previewAnimationInterval(); got != 0 {
		t.Fatalf("静态画布不应持续刷新：%s", got)
	}
	model.manifest.Layers = append(model.manifest.Layers, caseTheme.Layer{Type: caseTheme.LayerClock, Opacity: 1, Options: map[string]any{"visible": true}})
	if got := model.previewAnimationInterval(); got != 250*time.Millisecond {
		t.Fatalf("时钟预览刷新周期不正确：%s", got)
	}
	model.manifest.Layers = append(model.manifest.Layers, caseTheme.Layer{Type: caseTheme.LayerAudioRing, Opacity: 1, Options: map[string]any{"visible": true}})
	if got := model.previewAnimationInterval(); got != audioPreviewRefreshInterval {
		t.Fatalf("频谱预览应与真实音频状态以 10 FPS 刷新：%s", got)
	}
	model.manifest.Layers[len(model.manifest.Layers)-1].Options["visible"] = false
	if got := model.previewAnimationInterval(); got != 250*time.Millisecond {
		t.Fatalf("隐藏频谱不应驱动动画刷新：%s", got)
	}
}

func TestWorkbenchRingDefaultCountMatchesSharedThemeDefault(t *testing.T) {
	model := &gioEditor{}
	options := model.defaultWaveshareOptions(caseTheme.LayerAudioRing)
	if got := caseTheme.OptionInt(options, "count", 0); got != caseTheme.DefaultAudioRingCount {
		t.Fatalf("工作台环状频谱缺省段数漂移: got=%d want=%d", got, caseTheme.DefaultAudioRingCount)
	}
	if got := caseTheme.OptionInt(options, "thickness", 0); got != 1 {
		t.Fatalf("工作台环状频谱恢复了过粗默认线宽: got=%d want=1", got)
	}
}

func TestAudioPreviewUsesFreshServiceBandsAndFallsBackToSimulation(t *testing.T) {
	now := time.Unix(100, 0)
	layer := caseTheme.Layer{Type: caseTheme.LayerAudioBars, Options: map[string]any{
		"noise_gate": 0.0, "gain": 1.0, "sensitivity": 1.0, "min_amplitude": 0.0, "max_amplitude": 1.0,
		"contrast": 1.0, "separation": 0.0, "smoothing": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
	}}
	model := &gioEditor{previewNow: now}
	model.setAudioPreviewStatus(service.AudioStatus{
		Enabled: true, Running: true, CapturedAt: now, Bands: []float64{0, .5, 1}, Level: .4, Peak: 1,
	})
	first := model.audioPreviewValues(layer, 3)
	if len(first) != 3 || first[0] != 0 || math.Abs(first[1]-.5) > 1e-9 || first[2] != 1 {
		t.Fatalf("预览没有使用服务实时频带：%v", first)
	}
	model.previewNow = now.Add(100 * time.Millisecond)
	second := model.audioPreviewValues(layer, 3)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同一真实音频帧不应生成动画波形：first=%v second=%v", first, second)
	}
	model.setAudioPreviewStatus(service.AudioStatus{Enabled: false, Running: false, Bands: []float64{1, 1, 1}})
	disabled := model.audioPreviewValues(layer, 3)
	if previewAudioBandsSilent(disabled) || disabled[0] == disabled[1] && disabled[1] == disabled[2] {
		t.Fatalf("音频关闭时没有显示高低分明的演示频谱：%v", disabled)
	}
	model.setAudioPreviewStatus(service.AudioStatus{Enabled: true, Running: true, CapturedAt: now, Bands: []float64{1, 1, 1}})
	model.previewNow = now.Add(audioPreviewStaleAfter + time.Millisecond)
	stale := model.audioPreviewValues(layer, 3)
	if previewAudioBandsSilent(stale) {
		t.Fatalf("服务频带过期时没有回退到演示频谱：%v", stale)
	}
	model.previewNow = model.previewNow.Add(audioPreviewRefreshInterval)
	animated := model.audioPreviewValues(layer, 3)
	if reflect.DeepEqual(stale, animated) {
		t.Fatalf("演示频谱没有随预览时钟变化：first=%v second=%v", stale, animated)
	}
}

func TestSimulatedAudioPreviewBandsStayBelowFullScaleAndShowShape(t *testing.T) {
	values := simulatedAudioPreviewBands(24, time.Unix(123, 0))
	minimum, maximum := 1.0, 0.0
	for _, value := range values {
		minimum = math.Min(minimum, value)
		maximum = math.Max(maximum, value)
	}
	if minimum <= 0 || maximum >= 1 || maximum-minimum < .25 {
		t.Fatalf("演示频谱没有形成清晰且非满柱的动态高度：min=%f max=%f values=%v", minimum, maximum, values)
	}
}

func TestAudioPreviewPeakHoldRemainsVisibleThenDecays(t *testing.T) {
	now := time.Unix(200, 0)
	layer := caseTheme.Layer{ID: "preview-peak", Type: caseTheme.LayerAudioBars, Options: map[string]any{
		"noise_gate": 0.0, "gain": 1.0, "sensitivity": 1.0, "min_amplitude": 0.0, "max_amplitude": 1.0,
		"contrast": 1.0, "separation": 0.0, "smoothing": 0.0, "bass_boost": 1.0, "floor_reduction": 0.0,
		"peak_hold": true, "peak_hold_ms": 200.0, "peak_decay": 1.0,
	}}
	model := &gioEditor{previewNow: now}
	model.setAudioPreviewStatus(service.AudioStatus{Enabled: true, Running: true, CapturedAt: now, Bands: []float64{1}})
	values, peaks := model.audioPreviewFrame(layer, 1)
	if values[0] != 1 || peaks[0] != 1 {
		t.Fatalf("首次高峰没有进入预览峰值状态: values=%v peaks=%v", values, peaks)
	}

	now = now.Add(100 * time.Millisecond)
	model.previewNow = now
	model.setAudioPreviewStatus(service.AudioStatus{Enabled: true, Running: true, CapturedAt: now, Bands: []float64{.2}})
	values, peaks = model.audioPreviewFrame(layer, 1)
	if math.Abs(values[0]-.2) > 1e-9 || peaks[0] != 1 {
		t.Fatalf("保持期内预览峰值没有留在高位: values=%v peaks=%v", values, peaks)
	}

	now = now.Add(200 * time.Millisecond)
	model.previewNow = now
	model.setAudioPreviewStatus(service.AudioStatus{Enabled: true, Running: true, CapturedAt: now, Bands: []float64{.2}})
	_, peaks = model.audioPreviewFrame(layer, 1)
	if math.Abs(peaks[0]-.8) > 1e-9 {
		t.Fatalf("保持结束后预览峰值没有按真实时间衰减: %v", peaks)
	}
	_, repeatedPeaks := model.audioPreviewFrame(layer, 1)
	if !reflect.DeepEqual(peaks, repeatedPeaks) {
		t.Fatalf("同一真实音频帧被重复衰减: first=%v repeated=%v", peaks, repeatedPeaks)
	}

	now = now.Add(100 * time.Millisecond)
	model.previewNow = now
	model.setAudioPreviewStatus(service.AudioStatus{Enabled: true, Running: true, CapturedAt: now, Bands: []float64{0}})
	values, peaks = model.audioPreviewFrame(layer, 1)
	if values[0] <= 0 || len(peaks) != 1 || math.Abs(peaks[0]-values[0]) > 1e-9 {
		t.Fatalf("静音回退演示时继承了真实峰值或没有可见形态: values=%v peaks=%v", values, peaks)
	}
}

func TestPreviewImageOpIsReusedAcrossFrames(t *testing.T) {
	const path = "memory-preview.png"
	layer := caseTheme.Layer{Type: caseTheme.LayerImage, Options: map[string]any{"resource_id": "preview"}}
	model := &gioEditor{
		manifest:   caseTheme.Manifest{Resources: []caseTheme.Resource{{ID: "preview", LocalPath: path}}},
		imageCache: map[string]image.Image{path: image.NewNRGBA(image.Rect(0, 0, 64, 64))},
	}
	first, ok := model.loadLayerImageOp(layer)
	if !ok {
		t.Fatal("首次创建预览 ImageOp 失败")
	}
	second, ok := model.loadLayerImageOp(layer)
	if !ok || first != second {
		t.Fatal("连续帧没有复用同一个预览 ImageOp")
	}
	if len(model.imageOps) != 1 {
		t.Fatalf("预览纹理缓存数量异常：%d", len(model.imageOps))
	}
}

func TestWaveshareTemplateRenameModeUsesDedicatedNameField(t *testing.T) {
	model := &gioEditor{target: "waveshare-7"}
	model.waveshareLibrary.init()
	model.waveshareLibrary.templates = []config.WaveshareTemplate{{ID: "day", Name: "日间主题"}}
	model.beginWaveshareTemplateRename("day")
	if model.waveshareLibrary.renamingTemplateID != "day" || model.waveshareLibrary.renameName.editor.Text() != "日间主题" {
		t.Fatalf("没有进入独立重命名模式：id=%q name=%q", model.waveshareLibrary.renamingTemplateID, model.waveshareLibrary.renameName.editor.Text())
	}
	if model.waveshareLibrary.editingTemplateID != "" || model.waveshareLibrary.templateName.editor.Text() != "" {
		t.Fatal("名称修改不应误进入画布重编译模式")
	}
	model.cancelWaveshareTemplateRename()
	if model.waveshareLibrary.renamingTemplateID != "" || model.waveshareLibrary.renameName.editor.Text() != "" {
		t.Fatal("取消重命名后仍残留编辑状态")
	}
}

func assertNear(t *testing.T, actual, expected float64) {
	t.Helper()
	if math.Abs(actual-expected) > 1e-9 {
		t.Fatalf("数值不匹配：got %.6f, want %.6f", actual, expected)
	}
}

func assertGeometryNear(t *testing.T, actual, expected float64) {
	t.Helper()
	if math.Abs(actual-expected) > 1e-5 {
		t.Fatalf("几何数值不匹配：got %.6f, want %.6f", actual, expected)
	}
}
