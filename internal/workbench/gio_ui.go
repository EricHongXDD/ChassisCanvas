package workbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	caseTheme "casescreen/internal/theme"
	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/gesture"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/transfer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// Run 启动 Gio 纯 Go Windows 工作台。Windows 上 app.Main 不会随最后一个
// 窗口关闭而返回，因此必须由 DestroyEvent 回调通知入口程序完成后台清理并退出。
func Run(api *API, closed ...func(error)) {
	var onClosed func(error)
	if len(closed) > 0 {
		onClosed = closed[0]
	}
	runGioWorkbench(api, DesktopRunOptions{Closed: onClosed})
}

// RunDesktop 启动支持系统托盘的 Gio 工作台。
func RunDesktop(api *API, options DesktopRunOptions) {
	runGioWorkbench(api, options)
}

func runGioWorkbench(api *API, options DesktopRunOptions) {
	model := newGioEditor(api)
	window := new(app.Window)
	model.window = window
	window.Option(app.Title("机箱副屏主题工作台"), app.Size(unit.Dp(1500), unit.Dp(900)))
	if options.Ready != nil {
		controller := newDesktopWindowController(model)
		if err := options.Ready(controller); err != nil {
			if options.Closed != nil {
				options.Closed(err)
			}
			return
		}
	}
	go func() {
		err := model.loop(window)
		if err != nil {
			model.message = "窗口已关闭：" + err.Error()
		}
		if options.Closed != nil {
			options.Closed(err)
		}
	}()
	app.Main()
}

type gioEditor struct {
	api        *API
	window     *app.Window
	uiUpdates  chan func()
	activity   windowActivityState
	theme      *material.Theme
	manifest   caseTheme.Manifest
	target     string
	selected   int
	message    string
	imageCache map[string]image.Image
	imageOps   map[string]paint.ImageOp
	previewNow time.Time
	// previewRevision 记录编辑器模型发生的可视变化。属性面板位于画布之后，
	// 当前帧处理点击时画布已经绘制完成，因此每次编辑都必须显式请求下一帧。
	previewRevision uint64

	canvasTag      struct{}
	keyTag         struct{}
	dragging       bool
	dragMode       string
	lastPoint      image.Point
	rotateOffset   float64
	resizeHandle   string
	resizeAnchor   f32.Point
	resizeBase     caseTheme.Rect
	lastScaleUndo  time.Time
	historyGesture bool
	undoStack      []editorSnapshot
	redoStack      []editorSnapshot

	paletteButtons     []gioButton
	targetButtons      []*gioButton
	targetButtonByID   map[string]*gioButton
	targetOptions      []targetOption
	layerButtons       []gioButton
	addButton          widget.Clickable
	deleteButton       widget.Clickable
	raiseButton        widget.Clickable
	lowerButton        widget.Clickable
	applyButton        widget.Clickable
	currentPowerButton widget.Clickable
	allPowerButton     widget.Clickable
	importButton       widget.Clickable
	backgroundBrowse   widget.Clickable
	backgroundColors   [3]widget.Clickable
	undoButton         widget.Clickable
	redoButton         widget.Clickable
	lyricsBrowse       widget.Clickable
	lyricsSave         widget.Clickable
	lyricsProviders    [4]widget.Clickable
	componentOptions   map[string]*widget.Clickable
	optionEditors      map[string]*textField
	propertyList       widget.List
	presetList         widget.List
	layerList          widget.List
	applyRunning       bool
	powerRunning       bool
	powerTarget        string
	lyricsSaveRunning  bool
	lyricsSaveVersion  uint64
	targetLoadVersion  uint64
	targetLoading      string
	// targetExplicit 表示用户已经主动选择过目标。设备连接通常晚于工作台首帧，
	// 在此之前允许状态刷新把初始离线占位自动切换到首台在线屏；用户明确
	// 选择离线屏后则必须尊重其选择，不能在后续刷新时跳走。
	targetExplicit bool
	inspectorTab   string
	inspectorTabs  [6]widget.Clickable

	waveshareLibrary     waveshareLibraryState
	coolerCalibration    coolerCalibrationState
	gpuSettings          gpuSettingsState
	telemetryGPUSettings telemetryGPUSettingsState
	targetStatus         targetStatusRefreshState
	audioPreview         audioPreviewRefreshState
	desktopControl       *desktopWindowControl

	jsonPath              textField
	lyricsProvider        textField
	lyricsPath            textField
	lyricsDefaultProvider string
	lyricsDefaultPath     string
	componentEntries      []*numberField
	componentLayerID      string
	lyricsLayerID         string
	platformAuthStatuses  map[string]PlatformAuthStatus
	platformAuthLoading   bool
}

type editorSnapshot struct {
	Manifest caseTheme.Manifest
	Selected int
}

type gioButton struct {
	id        string
	click     widget.Clickable
	drag      componentDrag
	text      string
	layerType caseTheme.LayerType
	action    func()
}

// componentDrag 用同一个指针手势区分单击和拖放，避免两个重叠处理器互相拦截。
type componentDrag struct {
	Type    string
	drag    gesture.Drag
	hover   gesture.Hover
	click   f32.Point
	pos     f32.Point
	moved   bool
	clicked bool
	hovered bool
}

const componentMIME = "application/x-casescreen-layer"

func (drag *componentDrag) Layout(gtx layout.Context, normal, preview layout.Widget) layout.Dimensions {
	if !gtx.Enabled() {
		return normal(gtx)
	}
	dimensions := normal(gtx)
	stack := clip.Rect{Max: dimensions.Size}.Push(gtx.Ops)
	drag.drag.Add(gtx.Ops)
	drag.hover.Add(gtx.Ops)
	event.Op(gtx.Ops, drag)
	stack.Pop()
	if preview != nil && drag.drag.Pressed() {
		recording := op.Record(gtx.Ops)
		op.Offset(drag.pos.Round()).Add(gtx.Ops)
		preview(gtx)
		op.Defer(gtx.Ops, recording.Stop())
	}
	return dimensions
}

func (drag *componentDrag) Update(gtx layout.Context) (mime string, requested, clicked bool) {
	drag.clicked = false
	drag.hovered = drag.hover.Update(gtx.Source)
	position := drag.pos
	for {
		pointerValue, ok := drag.drag.Update(gtx.Metric, gtx.Source, gesture.Both)
		if !ok {
			break
		}
		switch pointerValue.Kind {
		case pointer.Press:
			drag.click = pointerValue.Position
			position = f32.Point{}
			drag.moved = false
		case pointer.Drag:
			position = pointerValue.Position.Sub(drag.click)
			threshold := float32(gtx.Metric.Dp(4))
			if position.X*position.X+position.Y*position.Y > threshold*threshold {
				drag.moved = true
			}
		case pointer.Release:
			position = pointerValue.Position.Sub(drag.click)
			threshold := float32(gtx.Metric.Dp(4))
			if position.X*position.X+position.Y*position.Y > threshold*threshold {
				drag.moved = true
			}
			if !drag.moved {
				drag.clicked = true
			}
		case pointer.Cancel:
			drag.moved = false
		}
	}
	drag.pos = position
	for {
		eventValue, ok := gtx.Event(transfer.SourceFilter{Target: drag, Type: drag.Type})
		if !ok {
			break
		}
		if request, ok := eventValue.(transfer.RequestEvent); ok {
			return request.Type, true, drag.clicked
		}
	}
	return "", false, drag.clicked
}

func (drag *componentDrag) Offer(gtx layout.Context, mime string, data io.ReadCloser) {
	gtx.Execute(transfer.OfferCmd{Tag: drag, Type: mime, Data: data})
}

func (drag *componentDrag) Hovered() bool { return drag.hovered }

func (drag *componentDrag) Pressed() bool { return drag.drag.Pressed() }

type targetOption struct {
	id                  string
	label               string
	resolution          [2]int
	waveshare           bool
	coolermaster        bool
	displayShape        string
	displayRotation     int
	connected           bool
	lastError           string
	brightnessSupported bool
	defaultBrightness   int
	currentBrightness   int
	poweredOff          bool
}

type targetButtonView struct {
	id         string
	label      string
	button     *gioButton
	action     func()
	active     bool
	loading    bool
	connected  bool
	poweredOff bool
}

type textField struct {
	editor widget.Editor
	last   string
}

type numberField struct {
	label string
	field textField
	apply func(float64)
	last  float64
}

type optionChoice struct {
	value string
	label string
}

type paletteItem struct {
	text      string
	typeValue caseTheme.LayerType
}

type componentFieldValue struct {
	label string
	value float64
	apply func(float64)
}

func newGioEditor(api *API) *gioEditor {
	model := &gioEditor{api: api, uiUpdates: make(chan func(), 8), theme: newWorkbenchTheme(), inspectorTab: "layers", manifest: defaultManifest(), imageCache: make(map[string]image.Image), imageOps: make(map[string]paint.ImageOp), componentOptions: make(map[string]*widget.Clickable), optionEditors: make(map[string]*textField), platformAuthStatuses: make(map[string]PlatformAuthStatus)}
	model.propertyList.Axis = layout.Vertical
	model.presetList.Axis = layout.Vertical
	model.layerList.Axis = layout.Vertical
	model.waveshareLibrary.init()
	model.coolerCalibration.init()
	model.theme.Palette = material.Palette{
		Bg:         color.NRGBA{R: 11, G: 24, B: 31, A: 255},
		Fg:         color.NRGBA{R: 226, G: 238, B: 242, A: 255},
		ContrastBg: color.NRGBA{R: 26, G: 155, B: 168, A: 255},
		ContrastFg: color.NRGBA{R: 4, G: 20, B: 24, A: 255},
	}
	model.jsonPath.editor.SingleLine = true
	model.lyricsProvider.editor.SingleLine = true
	model.lyricsPath.editor.SingleLine = true
	model.refreshFromService()
	return model
}

func (model *gioEditor) loop(window *app.Window) error {
	var ops op.Ops
	for {
		eventValue := window.Event()
		// 先使用事件到达前的原生视图状态判断关闭，
		// 否则 HWND=0 会先覆盖旧句柄，无法区分启动未建窗和真正销毁。
		windowErr, closed := workbenchWindowClosed(eventValue, model.activity.nativeViewAttached)
		model.observeWindowEvent(eventValue)
		model.applyDesktopWindowRequests()
		if closed {
			return windowErr
		}
		switch eventValue := eventValue.(type) {
		case app.FrameEvent:
			model.renderWindowFrame(&ops, eventValue)
		}
	}
}

func (model *gioEditor) renderWindowFrame(ops *op.Ops, frameEvent app.FrameEvent) {
	if !model.periodicUIWorkEnabled() {
		if renderStartupFrame, retryAttach := model.beginStartupWindowFrame(); renderStartupFrame {
			// Gio/Windows 可能在 HWND 出现前就索要首帧。若此时
			// 提交空操作，后端可能不再索要第二帧，窗口就会一直
			// 保持白屏。启动阶段先绘制一帧完整静态界面；因
			// periodicUIWorkEnabled 仍为 false，不会发起状态/音频
			// 轮询或动画。附着重试超过上限后自动停止。
			gtx := app.NewContext(ops, frameEvent)
			model.drainUIUpdates()
			model.layout(gtx)
			if retryAttach {
				gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(startupWindowAttachRetryInterval)})
			}
			frameEvent.Frame(gtx.Ops)
			return
		}
		// 原生窗口被隐藏后，Gio/Windows 仍可能消化隐藏前
		// 已排程的帧。提交一个空帧即可释放该请求，不再
		// 运行完整布局和绘制，避免无 HWND 的进程持续空转。
		ops.Reset()
		frameEvent.Frame(ops)
		return
	}
	gtx := app.NewContext(ops, frameEvent)
	model.drainUIUpdates()
	model.layout(gtx)
	frameEvent.Frame(gtx.Ops)
}

func (model *gioEditor) drainUIUpdates() {
	for {
		select {
		case update := <-model.uiUpdates:
			update()
		default:
			return
		}
	}
}

func (model *gioEditor) layout(gtx layout.Context) layout.Dimensions {
	model.previewNow = gtx.Now
	model.scheduleAudioPreviewRefresh(gtx)
	model.schedulePreviewAnimation(gtx)
	event.Op(gtx.Ops, &model.keyTag)
	model.handleKeyboard(gtx)
	paint.FillShape(gtx.Ops, model.theme.Bg, clip.Rect{Max: gtx.Constraints.Max}.Op())
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(model.layoutHeader),
		layout.Flexed(1, model.layoutBody),
		layout.Rigid(model.layoutFooter),
	)
}

func (model *gioEditor) previewMoment() time.Time {
	if !model.previewNow.IsZero() {
		return model.previewNow
	}
	return time.Now()
}

// requestPreviewRefresh 保证从右侧属性面板修改静态组件时，画布也会立即获得
// 下一帧，而不是等到窗口被遮挡、缩放或动态组件定时刷新后才更新。
func (model *gioEditor) requestPreviewRefresh() {
	model.previewRevision++
	model.invalidateWindow()
}

// schedulePreviewAnimation 只在画布含动态组件时请求动画帧，静态主题不会持续占用 CPU。
func (model *gioEditor) schedulePreviewAnimation(gtx layout.Context) {
	if !model.periodicUIWorkEnabled() {
		return
	}
	if interval := model.previewAnimationInterval(); interval > 0 {
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(interval)})
	}
}

func (model *gioEditor) previewAnimationInterval() time.Duration {
	interval := time.Duration(0)
	for _, layer := range model.manifest.Layers {
		if !caseTheme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		switch layer.Type {
		case caseTheme.LayerAudioBars, caseTheme.LayerAudioRing:
			interval = audioPreviewRefreshInterval
		case caseTheme.LayerLineChart:
			interval = time.Second / 30
		case caseTheme.LayerClock, caseTheme.LayerTelemetry:
			if interval == 0 {
				interval = 250 * time.Millisecond
			}
		}
	}
	return interval
}

func (model *gioEditor) layoutHeader(gtx layout.Context) layout.Dimensions {
	model.scheduleTargetStatusRefresh(gtx)
	return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return material.H6(model.theme, "目标屏幕").Layout(gtx)
						}),
						layout.Rigid(model.layoutTargetButtons),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: unit.Dp(16)}.Layout(gtx, model.layoutPowerButtons)
						}),
					)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return model.layoutTextField(gtx, &model.jsonPath, "主题 JSON 绝对路径")
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return model.buttonSized(gtx, &model.importButton, "载入主题 JSON", model.importJSON, 138)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return model.buttonSizedState(gtx, &model.undoButton, "撤销  Ctrl+Z", model.undo, 112, len(model.undoStack) > 0)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return model.buttonSizedState(gtx, &model.redoButton, "重做  Ctrl+Y", model.redo, 112, len(model.redoStack) > 0)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: unit.Dp(20)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								label := "应用当前画布"
								if model.applyRunning {
									label = "正在应用…"
								}
								return model.buttonSizedState(gtx, &model.applyButton, label, model.applyTheme, 168, true)
							})
						}),
					)
				})
			}),
		)
	})
}

func (model *gioEditor) handleKeyboard(gtx layout.Context) {
	if model.textInputFocused(gtx) {
		return
	}
	for {
		eventValue, ok := gtx.Event(
			key.Filter{Name: key.NameDeleteForward},
			key.Filter{Name: key.NameDeleteBackward},
			key.Filter{Name: "Z", Required: key.ModShortcut, Optional: key.ModShift},
			key.Filter{Name: "Y", Required: key.ModShortcut},
		)
		if !ok {
			return
		}
		keyValue, ok := eventValue.(key.Event)
		if !ok || keyValue.State != key.Press {
			continue
		}
		switch keyValue.Name {
		case key.NameDeleteForward, key.NameDeleteBackward:
			model.removeLayer()
		case "Z":
			if keyValue.Modifiers.Contain(key.ModShift) {
				model.redo()
			} else {
				model.undo()
			}
		case "Y":
			model.redo()
		}
		gtx.Execute(op.InvalidateCmd{})
	}
}

func (model *gioEditor) textInputFocused(gtx layout.Context) bool {
	if gtx.Focused(&model.jsonPath.editor) || gtx.Focused(&model.lyricsProvider.editor) || gtx.Focused(&model.lyricsPath.editor) ||
		gtx.Focused(&model.waveshareLibrary.templateName.editor) || gtx.Focused(&model.waveshareLibrary.renameName.editor) || gtx.Focused(&model.waveshareLibrary.defaultDuration.editor) {
		return true
	}
	for _, field := range model.waveshareLibrary.durationFields {
		if field != nil && gtx.Focused(&field.editor) {
			return true
		}
	}
	for _, field := range model.componentEntries {
		if field != nil && gtx.Focused(&field.field.editor) {
			return true
		}
	}
	for _, field := range model.optionEditors {
		if field != nil && gtx.Focused(&field.editor) {
			return true
		}
	}
	return false
}

func (model *gioEditor) layoutTargetButtons(gtx layout.Context) layout.Dimensions {
	return model.layoutTargetButtonViews(gtx, model.targetButtonViews())
}

// targetButtonViews 固化本帧使用的设备 ID、文字和按钮指针，后台状态变化不会破坏正在执行的布局。
func (model *gioEditor) targetButtonViews() []targetButtonView {
	model.syncTargetButtons()
	views := make([]targetButtonView, 0, len(model.targetButtons))
	for _, button := range model.targetButtons {
		if button == nil {
			continue
		}
		views = append(views, targetButtonView{
			id: button.id, label: button.text, button: button, action: button.action,
			active: button.id == model.target, loading: button.id == model.targetLoading,
			connected: model.targetConnected(button.id), poweredOff: model.targetPoweredOff(button.id),
		})
	}
	return views
}

func (model *gioEditor) layoutTargetButtonViews(gtx layout.Context, views []targetButtonView) layout.Dimensions {
	children := make([]layout.FlexChild, 0, len(views))
	for index := range views {
		view := views[index]
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "离线 · " + view.label
			if view.poweredOff {
				label = "已关屏 · " + view.label
			} else if view.connected {
				label = "在线 · " + view.label
			}
			if view.active {
				label = "● " + label
			} else if view.loading {
				label = "◌ " + label
			}
			return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonSizedState(gtx, &view.button.click, label, view.action, 190, view.active || view.loading)
			})
		}))
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, children...)
}

func (model *gioEditor) layoutPowerButtons(gtx layout.Context) layout.Dimensions {
	current := model.targetOption(model.target)
	currentLabel := "关闭当前副屏"
	currentEnabled := current != nil && current.brightnessSupported && !model.powerRunning
	currentActive := current != nil && current.poweredOff
	if current != nil && current.poweredOff {
		currentLabel = "恢复当前副屏"
	}
	allOff, supported := model.allPowerTargetsOff()
	allLabel := "关闭全部副屏"
	if allOff && supported > 0 {
		allLabel = "恢复全部副屏"
	}
	allEnabled := supported > 0 && !model.powerRunning
	if model.powerRunning {
		if model.powerTarget == "all" {
			allLabel = "全部副屏设置中…"
		} else {
			currentLabel = "当前副屏设置中…"
		}
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.buttonSizedEnabledState(gtx, &model.currentPowerButton, currentLabel, model.toggleCurrentPower, 138, currentActive, currentEnabled)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonSizedEnabledState(gtx, &model.allPowerButton, allLabel, model.toggleAllPower, 138, allOff, allEnabled)
			})
		}),
	)
}

func (model *gioEditor) layoutBody(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(210), gtx.Metric.Dp(210)
			return layout.Inset{Left: unit.Dp(8), Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.panel(gtx, model.layoutPalette)
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(8), Right: unit.Dp(8), Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.panel(gtx, model.layoutCanvas)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(330), gtx.Metric.Dp(330)
			return layout.Inset{Right: unit.Dp(8), Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.panel(gtx, model.layoutProperties)
			})
		}),
	)
}

func (model *gioEditor) panel(gtx layout.Context, content layout.Widget) layout.Dimensions {
	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			size := gtx.Constraints.Min
			paint.FillShape(gtx.Ops, color.NRGBA{R: 18, G: 31, B: 38, A: 255}, clip.UniformRRect(image.Rectangle{Max: size}, gtx.Metric.Dp(7)).Op(gtx.Ops))
			paint.FillShape(gtx.Ops, color.NRGBA{R: 45, G: 64, B: 72, A: 255}, clip.Stroke{Path: clip.UniformRRect(image.Rectangle{Max: size}, gtx.Metric.Dp(7)).Path(gtx.Ops), Width: 1}.Op())
			return layout.Dimensions{Size: size}
		},
		content,
	)
}

func (model *gioEditor) layoutPalette(gtx layout.Context) layout.Dimensions {
	items := []paletteItem{
		{"＋ 图片", caseTheme.LayerImage},
		{"＋ 遥测", caseTheme.LayerTelemetry},
		{"＋ 条状频谱", caseTheme.LayerAudioBars},
		{"＋ 环状频谱", caseTheme.LayerAudioRing},
		{"＋ LRC 歌词", caseTheme.LayerLyrics},
		{"＋ 视频", caseTheme.LayerBackgroundVideo},
	}
	if model.isWaveshareTarget() {
		items = append(items,
			paletteItem{"＋ 自定义文本", caseTheme.LayerText},
			paletteItem{"＋ 时钟 / 日期", caseTheme.LayerClock},
			paletteItem{"＋ 进度条", caseTheme.LayerProgressBar},
			paletteItem{"＋ 圆形仪表", caseTheme.LayerGauge},
			paletteItem{"＋ 渐变仪表", caseTheme.LayerGradientGauge},
			paletteItem{"＋ 折线图", caseTheme.LayerLineChart},
			paletteItem{"＋ 形状 / 面板", caseTheme.LayerShape},
		)
	}
	if len(model.paletteButtons) != len(items) {
		model.paletteButtons = make([]gioButton, len(items))
		for index, item := range items {
			model.paletteButtons[index].text = item.text
			model.paletteButtons[index].layerType = item.typeValue
			model.paletteButtons[index].drag.Type = componentMIME
			item := item
			model.paletteButtons[index].action = func() { model.addLayer(item.typeValue) }
		}
	}
	return layout.Inset{Top: unit.Dp(10), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return material.H6(model.theme, "组件库").Layout(gtx) }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					textStyle := material.Body2(model.theme, "拖入画布创建；单击也可快速添加")
					textStyle.Color = color.NRGBA{R: 137, G: 159, B: 169, A: 255}
					return textStyle.Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, model.paletteButtonLayouts(gtx)...)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(model.theme, "传输策略").Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				textStyle := material.Body2(model.theme, "微雪驻留资源；酷冷按协议自动降级。")
				textStyle.Color = color.NRGBA{R: 137, G: 159, B: 169, A: 255}
				return textStyle.Layout(gtx)
			}),
		)
	})
}

func (model *gioEditor) paletteButtonLayouts(gtx layout.Context) []layout.FlexChild {
	children := make([]layout.FlexChild, 0, len(model.paletteButtons))
	for index := range model.paletteButtons {
		index := index
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			button := &model.paletteButtons[index]
			dims := button.drag.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					return model.buttonSurface(gtx, button.text, false, button.drag.Hovered(), button.drag.Pressed())
				},
				func(gtx layout.Context) layout.Dimensions { return model.dragPreview(gtx, button.text) },
			)
			mime, requested, clicked := button.drag.Update(gtx)
			if requested {
				button.drag.Offer(gtx, mime, io.NopCloser(strings.NewReader(string(button.layerType))))
			}
			if clicked && button.action != nil {
				button.action()
			}
			return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, func(layout.Context) layout.Dimensions { return dims })
		}))
	}
	return children
}

func (model *gioEditor) dragPreview(gtx layout.Context, textValue string) layout.Dimensions {
	gtx.Constraints.Min, gtx.Constraints.Max = image.Pt(gtx.Metric.Dp(170), gtx.Metric.Dp(34)), image.Pt(gtx.Metric.Dp(170), gtx.Metric.Dp(34))
	paint.FillShape(gtx.Ops, color.NRGBA{R: 38, G: 190, B: 201, A: 235}, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Max}, gtx.Metric.Dp(6)).Op(gtx.Ops))
	label := material.Label(model.theme, unit.Sp(13), textValue)
	label.Color = color.NRGBA{R: 3, G: 20, B: 24, A: 255}
	return layout.Center.Layout(gtx, label.Layout)
}

func (model *gioEditor) layoutCanvas(gtx layout.Context) layout.Dimensions {
	paint.FillShape(gtx.Ops, color.NRGBA{R: 8, G: 15, B: 19, A: 255}, clip.Rect{Max: gtx.Constraints.Max}.Op())
	if model.isCoolerMasterTarget() {
		return model.layoutCoolerCanvas(gtx)
	}
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		available := gtx.Constraints.Max
		width, height := max(1, model.manifest.Width), max(1, model.manifest.Height)
		canvasWidth := available.X - gtx.Metric.Dp(24)
		canvasHeight := int(float64(canvasWidth) * float64(height) / float64(width))
		maxHeight := available.Y - gtx.Metric.Dp(24)
		if canvasHeight > maxHeight {
			canvasHeight = maxHeight
			canvasWidth = int(float64(canvasHeight) * float64(width) / float64(height))
		}
		canvasWidth, canvasHeight = max(canvasWidth, 1), max(canvasHeight, 1)
		gtx.Constraints.Min, gtx.Constraints.Max = image.Pt(canvasWidth, canvasHeight), image.Pt(canvasWidth, canvasHeight)
		return model.layoutCanvasSurface(gtx)
	})
}

func (model *gioEditor) layoutCanvasSurface(gtx layout.Context) layout.Dimensions {
	size := gtx.Constraints.Max
	canvasRect := clip.Rect{Max: size}
	passStack := pointer.PassOp{}.Push(gtx.Ops)
	eventStack := canvasRect.Push(gtx.Ops)
	event.Op(gtx.Ops, &model.canvasTag)
	model.canvasCursor(gtx, size)
	eventStack.Pop()
	passStack.Pop()
	if model.isCoolerMasterTarget() {
		circle := clip.Ellipse(image.Rectangle{Max: size}).Push(gtx.Ops)
		model.drawCanvasBackground(gtx, size)
		circle.Pop()
	} else {
		model.drawCanvasBackground(gtx, size)
	}
	model.handleCanvasEvents(gtx, size)
	model.handleComponentDrops(gtx, size)
	model.drawLayers(gtx, size)
	return layout.Dimensions{Size: size}
}

func (model *gioEditor) drawCanvasBackground(gtx layout.Context, size image.Point) {
	background := model.canvasColor()
	if background == "black" {
		paint.FillShape(gtx.Ops, color.NRGBA{A: 255}, clip.Rect{Max: size}.Op())
		return
	}
	if background == "white" {
		paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, clip.Rect{Max: size}.Op())
		return
	}
	cell := max(8, min(size.X, size.Y)/24)
	paint.FillShape(gtx.Ops, color.NRGBA{R: 45, G: 45, B: 45, A: 255}, clip.Rect{Max: size}.Op())
	var lightSquares clip.Path
	lightSquares.Begin(gtx.Ops)
	for y := 0; y < size.Y; y += cell {
		for x := 0; x < size.X; x += cell {
			if (x/cell+y/cell)%2 != 0 {
				continue
			}
			x1, y1 := min(x+cell, size.X), min(y+cell, size.Y)
			lightSquares.MoveTo(f32.Pt(float32(x), float32(y)))
			lightSquares.LineTo(f32.Pt(float32(x1), float32(y)))
			lightSquares.LineTo(f32.Pt(float32(x1), float32(y1)))
			lightSquares.LineTo(f32.Pt(float32(x), float32(y1)))
			lightSquares.Close()
		}
	}
	paint.FillShape(gtx.Ops, color.NRGBA{R: 67, G: 67, B: 67, A: 255}, clip.Outline{Path: lightSquares.End()}.Op())
}

func (model *gioEditor) handleComponentDrops(gtx layout.Context, size image.Point) {
	for {
		eventValue, ok := gtx.Event(transfer.TargetFilter{Target: &model.canvasTag, Type: componentMIME})
		if !ok {
			return
		}
		dataEvent, ok := eventValue.(transfer.DataEvent)
		if !ok {
			continue
		}
		reader := dataEvent.Open()
		data, err := io.ReadAll(io.LimitReader(reader, 128))
		_ = reader.Close()
		if err != nil {
			model.message = "组件拖放失败：" + err.Error()
			continue
		}
		x, y := .5, .5
		if model.lastPoint.X > 0 && model.lastPoint.Y > 0 && model.lastPoint.X < size.X && model.lastPoint.Y < size.Y {
			x = float64(model.lastPoint.X) / float64(max(1, size.X))
			y = float64(model.lastPoint.Y) / float64(max(1, size.Y))
		}
		model.addLayerAt(caseTheme.LayerType(strings.TrimSpace(string(data))), x, y)
	}
}

func (model *gioEditor) drawLayers(gtx layout.Context, size image.Point) {
	indices := make([]int, len(model.manifest.Layers))
	for index := range indices {
		indices[index] = index
	}
	sort.SliceStable(indices, func(a, b int) bool {
		return model.manifest.Layers[indices[a]].ZIndex < model.manifest.Layers[indices[b]].ZIndex
	})
	for _, index := range indices {
		layer := model.manifest.Layers[index]
		if layer.Type == caseTheme.LayerCanvas {
			continue
		}
		if !caseTheme.LayerVisible(layer) {
			continue
		}
		rect := model.layerRect(layer, size)
		if rect.Empty() {
			continue
		}
		fill := layerColor(layer.Type, index == model.selected)
		if model.isCoolerMasterTarget() {
			// 实际 LCD 是圆形；只裁切图层内容，选框和手柄仍在下面的
			// 非裁切阶段绘制，边缘图层因此仍然可以拖动和缩放。
			circle := clip.Ellipse(image.Rectangle{Max: size}).Push(gtx.Ops)
			model.drawLayerVisual(gtx, rect, layer, fill)
			circle.Pop()
		} else {
			model.drawLayerVisual(gtx, rect, layer, fill)
		}
		if index == model.selected {
			model.drawSelection(gtx, rect, layer.Rotation)
		}
	}
	if model.isCoolerMasterTarget() {
		paint.FillShape(gtx.Ops, color.NRGBA{R: 48, G: 220, B: 210, A: 220}, clip.Stroke{Path: clip.Ellipse(image.Rectangle{Max: size}).Path(gtx.Ops), Width: 2}.Op())
	} else {
		paint.FillShape(gtx.Ops, color.NRGBA{R: 48, G: 220, B: 210, A: 220}, clip.Stroke{Path: clip.Rect{Max: size}.Path(), Width: 1}.Op())
	}
}

func (model *gioEditor) drawLayerVisual(gtx layout.Context, rect image.Rectangle, layer caseTheme.Layer, fill color.NRGBA) {
	// 透明度必须作用到整个图层，而不只是没有专用渲染器时的占位色；
	// 图片、文字、面板和所有原生组件因此都会与设备端表现一致。
	opacity := paint.PushOpacity(gtx.Ops, float32(clamp01(layer.Opacity)))
	backgroundDrawn := false
	if layer.Type == caseTheme.LayerImage || layer.Type == caseTheme.LayerBackgroundImage {
		if background, ok := model.loadLayerImageOp(layer); ok {
			imageContext := gtx
			imageContext.Constraints.Min, imageContext.Constraints.Max = rect.Size(), rect.Size()
			center := f32.Pt(float32(rect.Min.X+rect.Max.X)/2, float32(rect.Min.Y+rect.Max.Y)/2)
			rotation := op.Affine(f32.Affine2D{}.Rotate(center, float32(layer.Rotation*math.Pi/180))).Push(imageContext.Ops)
			stack := op.Offset(rect.Min).Push(imageContext.Ops)
			widget.Image{Src: background, Fit: widget.Fill}.Layout(imageContext)
			stack.Pop()
			rotation.Pop()
			backgroundDrawn = true
		}
	}
	if !backgroundDrawn {
		center := f32.Pt(float32(rect.Min.X+rect.Max.X)/2, float32(rect.Min.Y+rect.Max.Y)/2)
		rotation := op.Affine(f32.Affine2D{}.Rotate(center, float32(layer.Rotation*math.Pi/180))).Push(gtx.Ops)
		model.drawLayerPreview(gtx, rect, layer, fill)
		rotation.Pop()
	}
	opacity.Pop()
}

// drawLayerPreview 在编辑器中直接绘制动态组件，确保属性修改能立即看到效果。
func (model *gioEditor) drawLayerPreview(gtx layout.Context, rect image.Rectangle, layer caseTheme.Layer, fallback color.NRGBA) {
	if rect.Empty() {
		return
	}
	stack := op.Offset(rect.Min).Push(gtx.Ops)
	defer stack.Pop()
	size := rect.Size()
	preview := gtx
	preview.Constraints.Min, preview.Constraints.Max = size, size
	clipStack := clip.Rect{Max: size}.Push(preview.Ops)
	defer clipStack.Pop()
	switch layer.Type {
	case caseTheme.LayerBackgroundVideo:
		model.drawVideoPreview(preview, size, layer, fallback)
	case caseTheme.LayerTelemetry:
		model.drawTelemetryPreview(preview, size, layer, fallback)
	case caseTheme.LayerAudioBars:
		model.drawBarsPreview(preview, size, layer, fallback)
	case caseTheme.LayerAudioRing:
		model.drawRingPreview(preview, size, layer, fallback)
	case caseTheme.LayerLyrics:
		model.drawLyricsPreview(preview, size, layer, fallback)
	case caseTheme.LayerText:
		model.drawTextComponentPreview(preview, size, layer)
	case caseTheme.LayerClock:
		model.drawClockPreview(preview, size, layer)
	case caseTheme.LayerProgressBar:
		model.drawProgressBarPreview(preview, size, layer)
	case caseTheme.LayerGauge, caseTheme.LayerGradientGauge:
		model.drawGaugePreview(preview, size, layer)
	case caseTheme.LayerLineChart:
		model.drawLineChartPreview(preview, size, layer)
	case caseTheme.LayerShape:
		model.drawShapePreview(preview, size, layer)
	default:
		paint.FillShape(preview.Ops, fallback, clip.Rect{Max: size}.Op())
		model.previewText(preview, size, layerLabel(layer.Type), unit.Sp(13), color.NRGBA{A: 255})
	}
}

func (model *gioEditor) previewText(gtx layout.Context, size image.Point, value string, fontSize unit.Sp, foreground color.NRGBA) {
	label := material.Label(model.theme, fontSize, value)
	label.Color = foreground
	label.MaxLines = 2
	layout.Inset{Top: unit.Dp(6), Left: unit.Dp(7), Right: unit.Dp(7)}.Layout(gtx, label.Layout)
}

func (model *gioEditor) drawVideoPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	if source, ok := model.loadLayerImageOp(layer); ok {
		gtx.Constraints.Min, gtx.Constraints.Max = size, size
		fit := widget.Cover
		if strings.EqualFold(defaultOption(&layer, "fit", "cover"), "contain") {
			fit = widget.Contain
		}
		widget.Image{Src: source, Fit: fit}.Layout(gtx)
		paint.FillShape(gtx.Ops, color.NRGBA{A: 38}, clip.Rect{Max: size}.Op())
	} else {
		paint.FillShape(gtx.Ops, fallback, clip.Rect{Max: size}.Op())
	}
	center := image.Pt(size.X/2, size.Y/2)
	radius := max(12, min(size.X, size.Y)/5)
	play := image.Rect(center.X-radius, center.Y-radius, center.X+radius, center.Y+radius)
	paint.FillShape(gtx.Ops, color.NRGBA{R: 5, G: 20, B: 25, A: 190}, clip.Ellipse(play).Op(gtx.Ops))
	var triangle clip.Path
	triangle.Begin(gtx.Ops)
	triangle.MoveTo(f32.Pt(float32(center.X-radius/3), float32(center.Y-radius*2/3)))
	triangle.LineTo(f32.Pt(float32(center.X+radius*2/3), float32(center.Y)))
	triangle.LineTo(f32.Pt(float32(center.X-radius/3), float32(center.Y+radius*2/3)))
	triangle.LineTo(f32.Pt(float32(center.X-radius/3), float32(center.Y-radius*2/3)))
	paint.FillShape(gtx.Ops, color.NRGBA{R: 110, G: 240, B: 232, A: 255}, clip.Outline{Path: triangle.End()}.Op())
	name := filepath.Base(model.backgroundResourcePath(&layer))
	if name == "." || name == "" {
		name = "视频预览"
	}
	label := material.Label(model.theme, unit.Sp(12), name)
	label.Color = color.NRGBA{R: 236, G: 249, B: 250, A: 255}
	label.MaxLines = 1
	layout.Stack{Alignment: layout.SW}.Layout(gtx, layout.Expanded(func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: size} }), layout.Stacked(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(5), Left: unit.Dp(7), Right: unit.Dp(7)}.Layout(gtx, label.Layout)
	}))
}

func (model *gioEditor) drawTelemetryPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	// 所有目标屏幕共享同一套遥测预览，保证编辑器与实际输出一致。
	model.drawWaveshareTelemetryPreview(gtx, size, layer, fallback)
}

func (model *gioEditor) drawLegacyTelemetryPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	panel := previewColor(layer, "panel_color", color.NRGBA{R: 4, G: 20, B: 27, A: 220})
	paint.FillShape(gtx.Ops, panel, clip.UniformRRect(image.Rectangle{Max: size}, max(0, min(8, min(size.X, size.Y)/2))).Op(gtx.Ops))
	style := layerOptionString(&layer, "style")
	if style == "" {
		style = "cards"
	}
	values := []struct {
		label, value string
		color        color.NRGBA
	}{
		{"CPU", "42%", color.NRGBA{R: 76, G: 221, B: 255, A: 255}},
		{"GPU", "63%", color.NRGBA{R: 255, G: 186, B: 94, A: 255}},
		{"RAM", "58%", color.NRGBA{R: 140, G: 241, B: 170, A: 255}},
	}
	if style == "gauge" {
		for index, item := range values {
			rowTop := index * size.Y / len(values)
			rowBottom := (index + 1) * size.Y / len(values)
			padding := max(1, min(4, min(size.X, size.Y)/14))
			labelRight := min(size.X-padding-1, max(padding+1, size.X/3))
			labelBounds := image.Rect(padding, min(rowBottom-1, rowTop+padding), labelRight, max(rowTop+padding+1, rowBottom-padding))
			placement := waveshareFitPreviewTextLayout(item.label, 11, labelBounds, "left", "center")
			model.previewTextAt(gtx, placement.point, item.label, placement.size, color.NRGBA{R: 192, G: 212, B: 218, A: 255})
			barX := min(size.X-1, labelRight+padding)
			barRight := max(barX+1, size.X-padding)
			barHeight := max(1, min(8, max(1, rowBottom-rowTop)/3))
			barY := rowTop + max(0, (rowBottom-rowTop-barHeight)/2)
			barWidth := max(1, barRight-barX)
			bar := image.Rect(barX, barY, barRight, min(rowBottom, barY+barHeight))
			paint.FillShape(gtx.Ops, color.NRGBA{R: 28, G: 57, B: 65, A: 255}, clip.UniformRRect(bar, min(4, barHeight/2)).Op(gtx.Ops))
			fillWidth := int(float64(barWidth) * []float64{.42, .63, .58}[index])
			fill := bar
			fill.Max.X = min(bar.Max.X, bar.Min.X+max(1, fillWidth))
			paint.FillShape(gtx.Ops, item.color, clip.UniformRRect(fill, min(4, barHeight/2)).Op(gtx.Ops))
		}
		return
	}
	if style == "compact" {
		padding := max(1, min(8, min(size.X, size.Y)/10))
		middle := max(padding+1, size.Y/2)
		firstBounds := image.Rect(padding, padding, max(padding+1, size.X-padding), middle)
		secondBounds := image.Rect(padding, middle, max(padding+1, size.X-padding), max(middle+1, size.Y-padding))
		first := "CPU 42%   GPU 63%   RAM 58%"
		firstPlacement := waveshareFitPreviewTextLayout(first, 12, firstBounds, "left", "center")
		model.previewTextAt(gtx, firstPlacement.point, first, firstPlacement.size, color.NRGBA{R: 235, G: 247, B: 250, A: 255})
		clock := model.previewMoment().Format("15:04:05")
		clockPlacement := waveshareFitPreviewTextLayout(clock, 18, secondBounds, "left", "center")
		model.previewTextAt(gtx, clockPlacement.point, clock, clockPlacement.size, color.NRGBA{R: 111, G: 231, B: 240, A: 255})
		return
	}
	columns := len(values)
	padding := max(1, min(6, min(size.X, size.Y)/10))
	gap := max(1, min(4, size.X/80))
	footerHeight := max(2, min(14, size.Y/4))
	cardTop := padding
	cardBottom := max(cardTop+1, size.Y-padding-footerHeight-gap)
	cardWidth := max(1, (max(columns, size.X-padding*2-gap*(columns-1)))/columns)
	for index, item := range values {
		x := padding + index*(cardWidth+gap)
		card := image.Rect(x, cardTop, min(size.X-padding, x+cardWidth), cardBottom)
		paint.FillShape(gtx.Ops, color.NRGBA{R: 17, G: 44, B: 53, A: 255}, clip.UniformRRect(card, max(0, min(6, min(card.Dx(), card.Dy())/2))).Op(gtx.Ops))
		inner := image.Rect(min(card.Max.X-1, card.Min.X+1), min(card.Max.Y-1, card.Min.Y+1), max(card.Min.X+2, card.Max.X-1), max(card.Min.Y+2, card.Max.Y-1))
		middle := inner.Min.Y + max(1, inner.Dy()/2)
		labelBounds := image.Rect(inner.Min.X, inner.Min.Y, inner.Max.X, min(inner.Max.Y, middle))
		valueBounds := image.Rect(inner.Min.X, min(inner.Max.Y-1, middle), inner.Max.X, inner.Max.Y)
		labelPlacement := waveshareFitPreviewTextLayout(item.label, 10, labelBounds, "left", "center")
		valuePlacement := waveshareFitPreviewTextLayout(item.value, 15, valueBounds, "left", "center")
		model.previewTextAt(gtx, labelPlacement.point, item.label, labelPlacement.size, color.NRGBA{R: 168, G: 196, B: 202, A: 255})
		model.previewTextAt(gtx, valuePlacement.point, item.value, valuePlacement.size, item.color)
	}
	footer := "实时监控  " + model.previewMoment().Format("15:04")
	footerBounds := image.Rect(padding, min(size.Y-padding-1, cardBottom+gap), max(padding+1, size.X-padding), max(cardBottom+gap+1, size.Y-padding))
	footerPlacement := waveshareFitPreviewTextLayout(footer, 11, footerBounds, "left", "center")
	model.previewTextAt(gtx, footerPlacement.point, footer, footerPlacement.size, color.NRGBA{R: 174, G: 204, B: 212, A: 255})
}

func (model *gioEditor) drawBarsPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	model.drawWaveshareBarsPreview(gtx, size, layer, fallback)
}

func (model *gioEditor) drawRingPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, fallback color.NRGBA) {
	model.drawWaveshareRingPreview(gtx, size, layer, fallback)
}

type lyricPreviewSpec struct {
	style          string
	current        string
	next           string
	align          string
	currentBounds  image.Rectangle
	nextBounds     image.Rectangle
	showNext       bool
	showHighlight  bool
	showProgress   bool
	highlightStyle string
	progressStyle  string
	badge          string
	divider        bool
}

// buildLyricPreviewSpec 把四种播放器外观拆成真正不同的预览布局。旧实现只对
// minimal 做了分支，player、karaoke 和 classic 实际画的是同一个双行面板。
func buildLyricPreviewSpec(layer caseTheme.Layer, size image.Point, padding, progressHeight int) lyricPreviewSpec {
	style := strings.ToLower(strings.TrimSpace(defaultOption(&layer, "style", "player")))
	switch style {
	case "player", "karaoke", "classic", "minimal":
	default:
		style = "player"
	}
	align := strings.ToLower(strings.TrimSpace(defaultOption(&layer, "align", "center")))
	if align != "left" && align != "right" {
		align = "center"
	}
	showHighlight := caseTheme.OptionBool(layer.Options, "karaoke_highlight", style == "player" || style == "karaoke")
	showProgress := caseTheme.OptionBool(layer.Options, "show_progress", style == "player" || style == "karaoke")
	right := max(padding+1, size.X-padding)
	bottom := max(padding+1, size.Y-padding)
	if showProgress && style != "minimal" {
		bottom = max(padding+2, bottom-progressHeight-max(1, padding/2))
	}
	spec := lyricPreviewSpec{style: style, align: align, showHighlight: showHighlight, showProgress: showProgress}
	switch style {
	case "karaoke":
		badgeHeight := max(1, min(18, max(1, (bottom-padding)/5)))
		contentTop := min(bottom-1, padding+badgeHeight)
		middle := contentTop + max(1, (bottom-contentTop)*13/20)
		spec.current = "让时间继续向前"
		spec.next = "下一句 · 穿过城市灯火"
		spec.currentBounds = image.Rect(padding, contentTop, right, min(bottom, middle))
		spec.nextBounds = image.Rect(padding, min(bottom-1, middle), right, bottom)
		spec.showNext = true
		spec.highlightStyle = "karaoke"
		spec.progressStyle = "segments"
		spec.badge = "卡拉 OK · 正在唱"
	case "classic":
		middle := padding + max(1, (bottom-padding)/2)
		spec.current = "♪ 示例歌曲"
		spec.next = "让时间继续向前"
		spec.currentBounds = image.Rect(padding+max(1, padding/2), padding, right, min(bottom, middle))
		spec.nextBounds = image.Rect(padding+max(1, padding/2), min(bottom-1, middle), right, bottom)
		spec.showNext = true
		// 微雪编译器只在 player/karaoke 模式把当前句替换为强调色；
		// classic 即使继承了旧主题的开关也保持普通当前句颜色。
		spec.showHighlight = false
		spec.progressStyle = "line"
		spec.divider = true
	case "minimal":
		spec.current = "让时间继续向前"
		spec.currentBounds = image.Rect(padding, padding, right, max(padding+1, size.Y-padding))
		spec.showHighlight = false
		spec.showProgress = false
	default:
		middle := padding + max(1, (bottom-padding)*11/20)
		spec.current = "示例歌曲 · 流光"
		spec.next = "让时间继续向前"
		spec.currentBounds = image.Rect(padding, padding, right, min(bottom, middle))
		spec.nextBounds = image.Rect(padding, min(bottom-1, middle), right, bottom)
		spec.showNext = true
		spec.highlightStyle = "text"
		spec.progressStyle = "bar"
	}
	return spec
}

// selectedTargetUsesWaveshareLyrics 判断当前画布是否会编译到微雪驻留主题。
// 不能只看主题尺寸；同一分辨率的主机合成设备仍可兑现逐字裁切和淡入。
func (model *gioEditor) selectedTargetUsesWaveshareLyrics() bool {
	// 设备状态尚未刷新或暂时断线时，仍通过稳定目标 ID 判断，避免
	// 微雪画布短暂切成主机逐字预览，而最终编译结果仍是整句强调。
	return model.isWaveshareTarget()
}

func (model *gioEditor) adaptLyricPreviewToTarget(spec lyricPreviewSpec) (lyricPreviewSpec, bool, bool) {
	waveshareTarget := model.selectedTargetUsesWaveshareLyrics()
	wholeLineHighlight := waveshareTarget && spec.showHighlight
	if waveshareTarget {
		spec.showHighlight = false
		spec.badge = ""
	}
	return spec, waveshareTarget, wholeLineHighlight
}

func (model *gioEditor) drawLyricsPreview(gtx layout.Context, size image.Point, layer caseTheme.Layer, _ color.NRGBA) {
	radius := max(0, min(previewInt(layer, "corner_radius", 14), min(size.X, size.Y)/2))
	panel := previewColor(layer, "panel_color", color.NRGBA{R: 3, G: 10, B: 18, A: 146})
	style := strings.ToLower(defaultOption(&layer, "style", "player"))
	if style == "minimal" {
		panel = previewColor(layer, "panel_color", color.NRGBA{})
	}
	border := previewColor(layer, "panel_border_color", color.NRGBA{R: 102, G: 227, B: 255, A: 62})
	if border.A > 0 && style != "minimal" {
		paint.FillShape(gtx.Ops, border, clip.UniformRRect(image.Rectangle{Max: size}, radius).Op(gtx.Ops))
		inner := image.Rect(1, 1, max(2, size.X-1), max(2, size.Y-1))
		paint.FillShape(gtx.Ops, panel, clip.UniformRRect(inner, max(0, radius-1)).Op(gtx.Ops))
	} else if panel.A > 0 {
		paint.FillShape(gtx.Ops, panel, clip.UniformRRect(image.Rectangle{Max: size}, radius).Op(gtx.Ops))
	}

	padding := max(1, min(8, min(size.X, size.Y)/10))
	progressHeight := max(1, min(10, int(math.Round(caseTheme.OptionFloat(layer.Options, "progress_thickness", 3)*model.previewScale(layer, size)))))
	spec := buildLyricPreviewSpec(layer, size, padding, progressHeight)
	// 微雪原生 type8 文字不能按字形动态裁切。预览改为实际可兑现的
	// 整句强调色；真实进度由每秒一个整数键驱动下方 type102 轨道。
	spec, waveshareTarget, wholeLineHighlight := model.adaptLyricPreviewToTarget(spec)
	currentPreferred, nextPreferred := model.lyricPreviewFontSizes(layer, size)
	if spec.style == "karaoke" {
		currentPreferred = unit.Sp(float64(currentPreferred) * 1.14)
	} else if spec.style == "classic" {
		currentPreferred = unit.Sp(float64(currentPreferred) * .92)
	} else if spec.style == "minimal" {
		currentPreferred = unit.Sp(float64(currentPreferred) * 1.06)
	}
	currentPlacement := waveshareFitPreviewTextLayout(spec.current, currentPreferred, spec.currentBounds, spec.align, "center")
	nextPlacement := waveshareFitPreviewTextLayout(spec.next, nextPreferred, spec.nextBounds, spec.align, "center")
	currentColor := previewColor(layer, "current_color", previewColor(layer, "color", color.NRGBA{R: 247, G: 252, B: 255, A: 255}))
	nextColor := previewColor(layer, "next_color", color.NRGBA{R: 184, G: 200, B: 218, A: 164})
	accent := previewColor(layer, "accent_color", color.NRGBA{R: 85, G: 229, B: 255, A: 255})
	if waveshareTarget {
		if wholeLineHighlight {
			currentColor = accent
		}
	} else {
		nextColor = lyricPreviewTransitionColor(layer, nextColor)
	}

	if spec.style == "karaoke" && spec.badge != "" && size.Y >= 24 {
		badgeBounds := image.Rect(padding, padding, max(padding+1, size.X-padding), min(size.Y-padding, padding+max(8, spec.currentBounds.Min.Y-padding)))
		badgePlacement := waveshareFitPreviewTextLayout(spec.badge, unit.Sp(maxFloat(6, float64(nextPreferred)*.58)), badgeBounds, spec.align, "center")
		badgeColor := accent
		badgeColor.A = uint8(float64(badgeColor.A) * .76)
		model.previewStyledTextAt(gtx, badgePlacement.point, spec.badge, badgePlacement.size, badgeColor, layer)
		stripe := image.Rect(padding, max(0, badgeBounds.Max.Y-1), max(padding+1, size.X-padding), badgeBounds.Max.Y)
		paint.FillShape(gtx.Ops, badgeColor, clip.Rect(stripe).Op())
	} else if spec.style == "karaoke" && waveshareTarget {
		stripeColor := accent
		stripeColor.A = uint8(float64(stripeColor.A) * .76)
		stripeY := max(padding, min(size.Y-padding-1, padding+max(2, size.Y/8)))
		stripe := image.Rect(padding, stripeY, min(size.X-padding, padding+max(3, size.X/4)), stripeY+1)
		paint.FillShape(gtx.Ops, stripeColor, clip.Rect(stripe).Op())
	}
	if spec.divider {
		dividerColor := accent
		dividerColor.A = uint8(float64(dividerColor.A) * .38)
		y := max(padding, min(size.Y-padding-1, spec.currentBounds.Max.Y))
		model.previewLine(gtx, padding, y, max(padding+1, size.X-padding), y, 1, dividerColor)
		markerWidth := max(1, min(3, padding/2+1))
		paint.FillShape(gtx.Ops, accent, clip.UniformRRect(image.Rect(padding, spec.currentBounds.Min.Y, padding+markerWidth, spec.nextBounds.Max.Y), markerWidth/2).Op(gtx.Ops))
	}

	model.previewLyricStyledTextAt(gtx, currentPlacement.point, spec.current, currentPlacement.size, currentColor, layer, false)
	if spec.showNext {
		model.previewLyricStyledTextAt(gtx, nextPlacement.point, spec.next, nextPlacement.size, nextColor, layer, true)
	}
	progressRatio := lyricPreviewProgressRatio(layer)
	if waveshareTarget {
		// 这是静态编辑预览的示例值；实际值来自主机每秒发送的 0..100 整数，
		// 与仅供主机合成动画使用的 transition_fraction 无关。
		progressRatio = .62
	}
	if spec.showHighlight {
		switch spec.highlightStyle {
		case "text", "karaoke":
			clipWidth := max(1, int(float64(currentPlacement.extent.X)*progressRatio))
			highlightClip := image.Rect(currentPlacement.point.X, spec.currentBounds.Min.Y, min(spec.currentBounds.Max.X, currentPlacement.point.X+clipWidth), spec.currentBounds.Max.Y)
			stack := clip.Rect(highlightClip).Push(gtx.Ops)
			model.previewLyricStyledTextAt(gtx, currentPlacement.point, spec.current, currentPlacement.size, accent, layer, false)
			stack.Pop()
		case "underline":
			y := min(spec.currentBounds.Max.Y-1, currentPlacement.point.Y+currentPlacement.extent.Y)
			model.previewLine(gtx, currentPlacement.point.X, y, currentPlacement.point.X+max(1, int(float64(currentPlacement.extent.X)*progressRatio)), y, max(1, progressHeight/2), accent)
		}
	}
	if spec.showProgress {
		model.drawLyricsProgressPreview(gtx, size, padding, progressHeight, progressRatio, accent, spec.progressStyle)
	}
}

func (model *gioEditor) lyricPreviewFontSizes(layer caseTheme.Layer, size image.Point) (unit.Sp, unit.Sp) {
	current := model.previewFontSize(layer, size, caseTheme.DefaultLyricsFontSize)
	next := unit.Sp(maxFloat(.5, caseTheme.OptionFloat(layer.Options, "next_font_size", caseTheme.DefaultLyricsNextFontSize)*model.previewScale(layer, size)))
	return current, next
}

func lyricPreviewProgressRatio(layer caseTheme.Layer) float64 {
	return clamp01(.62 + (caseTheme.OptionFloat(layer.Options, "transition_fraction", .08)-.08)*.45)
}

func lyricPreviewTransitionColor(layer caseTheme.Layer, next color.NRGBA) color.NRGBA {
	if caseTheme.OptionBool(layer.Options, "smooth_transition", true) {
		next.A = uint8(float64(next.A) * .78)
	}
	return next
}

func (model *gioEditor) drawLyricsProgressPreview(gtx layout.Context, size image.Point, padding, thickness int, ratio float64, accent color.NRGBA, style string) {
	track := image.Rect(padding, max(0, size.Y-padding-thickness), max(padding+1, size.X-padding), max(1, size.Y-padding))
	trackColor := color.NRGBA{R: 132, G: 162, B: 180, A: 52}
	switch style {
	case "segments":
		segments := max(4, min(14, track.Dx()/12))
		gap := max(1, min(3, track.Dx()/max(1, segments*8)))
		for index := 0; index < segments; index++ {
			segment := image.Rect(track.Min.X+index*track.Dx()/segments, track.Min.Y, track.Min.X+(index+1)*track.Dx()/segments-gap, track.Max.Y)
			fill := trackColor
			if float64(index+1)/float64(segments) <= ratio+.001 {
				fill = accent
			}
			if !segment.Empty() {
				paint.FillShape(gtx.Ops, fill, clip.UniformRRect(segment, min(2, thickness/2)).Op(gtx.Ops))
			}
		}
	case "line":
		track.Max.Y = track.Min.Y + 1
		paint.FillShape(gtx.Ops, trackColor, clip.Rect(track).Op())
		progress := track
		progress.Max.X = progress.Min.X + max(1, int(float64(progress.Dx())*ratio))
		paint.FillShape(gtx.Ops, accent, clip.Rect(progress).Op())
	default:
		paint.FillShape(gtx.Ops, trackColor, clip.UniformRRect(track, min(2, thickness/2)).Op(gtx.Ops))
		progress := track
		progress.Max.X = progress.Min.X + max(1, int(float64(progress.Dx())*ratio))
		paint.FillShape(gtx.Ops, accent, clip.UniformRRect(progress, min(2, thickness/2)).Op(gtx.Ops))
	}
}

func (model *gioEditor) previewLyricStyledTextAt(gtx layout.Context, point image.Point, value string, fontSize unit.Sp, foreground color.NRGBA, layer caseTheme.Layer, secondary bool) {
	style := strings.ToLower(defaultOption(&layer, "style", "player"))
	outlineWidth := max(0, min(4, previewInt(layer, "outline_width", 2)))
	glowRadius := max(0, min(6, previewInt(layer, "glow_radius", 4)))
	if secondary || style == "minimal" {
		outlineWidth = min(outlineWidth, 1)
		glowRadius = 0
	}
	if glowRadius > 0 {
		glow := previewColor(layer, "glow_color", color.NRGBA{R: 83, G: 218, B: 255, A: 66})
		for _, offset := range []image.Point{{X: -glowRadius}, {X: glowRadius}, {Y: -glowRadius}, {Y: glowRadius}} {
			model.previewStyledTextAt(gtx, point.Add(offset), value, fontSize, glow, layer)
		}
	}
	if caseTheme.OptionBool(layer.Options, "shadow", style != "minimal") {
		shadow := previewColor(layer, "shadow_color", color.NRGBA{A: 180})
		offset := image.Pt(previewInt(layer, "shadow_offset_x", 2), previewInt(layer, "shadow_offset_y", 3))
		blur := lyricPreviewShadowBlur(layer)
		if blur > 0 {
			softShadow := shadow
			softShadow.A = uint8(float64(softShadow.A) * .28)
			for _, softOffset := range []image.Point{{X: -blur}, {X: blur}, {Y: -blur}, {Y: blur}, {X: -blur, Y: -blur}, {X: blur, Y: blur}} {
				model.previewStyledTextAt(gtx, point.Add(offset).Add(softOffset), value, fontSize, softShadow, layer)
			}
		}
		model.previewStyledTextAt(gtx, point.Add(offset), value, fontSize, shadow, layer)
	}
	if outlineWidth > 0 {
		outline := previewColor(layer, "outline_color", color.NRGBA{R: 2, G: 7, B: 14, A: 225})
		for _, offset := range []image.Point{{X: -outlineWidth}, {X: outlineWidth}, {Y: -outlineWidth}, {Y: outlineWidth}} {
			model.previewStyledTextAt(gtx, point.Add(offset), value, fontSize, outline, layer)
		}
	}
	model.previewStyledTextAt(gtx, point, value, fontSize, foreground, layer)
}

func lyricPreviewShadowBlur(layer caseTheme.Layer) int {
	return max(0, min(4, previewInt(layer, "shadow_blur", 3)))
}

func (model *gioEditor) previewTextAt(gtx layout.Context, point image.Point, value string, fontSize unit.Sp, foreground color.NRGBA) {
	stack := op.Offset(point).Push(gtx.Ops)
	label := material.Label(model.theme, fontSize, value)
	label.Color = foreground
	label.MaxLines = 1
	label.Layout(gtx)
	stack.Pop()
}

func (model *gioEditor) previewLine(gtx layout.Context, x0, y0, x1, y1, width int, fill color.NRGBA) {
	width = max(1, width)
	var path clip.Path
	path.Begin(gtx.Ops)
	path.MoveTo(f32.Pt(float32(x0), float32(y0)))
	path.LineTo(f32.Pt(float32(x1), float32(y1)))
	paint.FillShape(gtx.Ops, fill, clip.Stroke{Path: path.End(), Width: float32(width)}.Op())
	if width <= 2 {
		return
	}
	// Stroke 默认是平头；只补两个端点即可获得圆头，避免旧实现按像素创建数百个绘制操作。
	radius := max(1, width/2)
	paint.FillShape(gtx.Ops, fill, clip.Ellipse(image.Rect(x0-radius, y0-radius, x0+radius+1, y0+radius+1)).Op(gtx.Ops))
	paint.FillShape(gtx.Ops, fill, clip.Ellipse(image.Rect(x1-radius, y1-radius, x1+radius+1, y1+radius+1)).Op(gtx.Ops))
}

func previewColor(layer caseTheme.Layer, key string, fallback color.NRGBA) color.NRGBA {
	value := strings.TrimPrefix(strings.TrimSpace(layerOptionString(&layer, key)), "#")
	if len(value) != 6 && len(value) != 8 {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 16, 32)
	if err != nil {
		return fallback
	}
	if len(value) == 6 {
		return color.NRGBA{R: uint8(parsed >> 16), G: uint8(parsed >> 8), B: uint8(parsed), A: 255}
	}
	return color.NRGBA{R: uint8(parsed >> 24), G: uint8(parsed >> 16), B: uint8(parsed >> 8), A: uint8(parsed)}
}

func previewFloat(layer caseTheme.Layer, key string, fallback float64) float64 {
	if value, ok := layer.Options[key].(float64); ok {
		return value
	}
	if value, ok := layer.Options[key].(int); ok {
		return float64(value)
	}
	return fallback
}

func previewInt(layer caseTheme.Layer, key string, fallback int) int {
	return int(math.Round(previewFloat(layer, key, float64(fallback))))
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (model *gioEditor) drawSelection(gtx layout.Context, rect image.Rectangle, rotation float64) {
	corners := rotatedCorners(rect, rotation)
	var path clip.Path
	path.Begin(gtx.Ops)
	path.MoveTo(corners[0])
	for index := 1; index < len(corners); index++ {
		path.LineTo(corners[index])
	}
	path.LineTo(corners[0])
	paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 205, B: 90, A: 255}, clip.Stroke{Path: path.End(), Width: 2}.Op())

	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := model.manifest.Layers[model.selected]
	if model.layerResizable(layer) {
		for _, resize := range resizeHandlePoints(rect, rotation) {
			half := 5
			handleRect := image.Rect(int(math.Round(float64(resize.point.X)))-half, int(math.Round(float64(resize.point.Y)))-half, int(math.Round(float64(resize.point.X)))+half+1, int(math.Round(float64(resize.point.Y)))+half+1)
			paint.FillShape(gtx.Ops, color.NRGBA{R: 13, G: 31, B: 38, A: 255}, clip.UniformRRect(handleRect, 2).Op(gtx.Ops))
			paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 205, B: 90, A: 255}, clip.Stroke{Path: clip.UniformRRect(handleRect, 2).Path(gtx.Ops), Width: 2}.Op())
		}
	}
	if layer.Type == caseTheme.LayerImage || layer.Type == caseTheme.LayerBackgroundImage {
		handle := rotationHandle(rect, rotation)
		top := f32.Pt((corners[0].X+corners[1].X)/2, (corners[0].Y+corners[1].Y)/2)
		var stem clip.Path
		stem.Begin(gtx.Ops)
		stem.MoveTo(top)
		stem.LineTo(handle)
		paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 205, B: 90, A: 255}, clip.Stroke{Path: stem.End(), Width: 2}.Op())
		handleRect := image.Rect(int(handle.X)-11, int(handle.Y)-11, int(handle.X)+11, int(handle.Y)+11)
		paint.FillShape(gtx.Ops, color.NRGBA{R: 28, G: 42, B: 49, A: 255}, clip.Ellipse(handleRect).Op(gtx.Ops))
		paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 205, B: 90, A: 255}, clip.Stroke{Path: clip.Ellipse(handleRect).Path(gtx.Ops), Width: 2}.Op())
		stack := op.Offset(image.Pt(handleRect.Min.X+4, handleRect.Min.Y+1)).Push(gtx.Ops)
		label := material.Label(model.theme, unit.Sp(13), "↻")
		label.Color = color.NRGBA{R: 255, G: 218, B: 120, A: 255}
		label.Layout(gtx)
		stack.Pop()
	}
	if model.dragging && strings.HasPrefix(model.dragMode, "resize-") {
		textValue := fmt.Sprintf("%.0f × %.0f px", layer.Bounds.Width*float64(model.manifest.Width), layer.Bounds.Height*float64(model.manifest.Height))
		badgePoint := image.Pt(rect.Min.X, rect.Max.Y+9)
		if badgePoint.Y+24 > gtx.Constraints.Max.Y {
			badgePoint.Y = rect.Min.Y - 28
		}
		stack := op.Offset(badgePoint).Push(gtx.Ops)
		label := material.Label(model.theme, unit.Sp(12), textValue)
		label.Color = color.NRGBA{R: 255, G: 224, B: 139, A: 255}
		label.Layout(gtx)
		stack.Pop()
	}
}

type canvasResizeHandle struct {
	id    string
	point f32.Point
}

// resizeHandlePoints 返回旋转后四角和四边中点，编辑器边框本身不会写入主题或下发到屏幕。
func resizeHandlePoints(rect image.Rectangle, rotation float64) []canvasResizeHandle {
	corners := rotatedCorners(rect, rotation)
	middle := func(a, b f32.Point) f32.Point { return f32.Pt((a.X+b.X)/2, (a.Y+b.Y)/2) }
	return []canvasResizeHandle{
		{id: "nw", point: corners[0]}, {id: "n", point: middle(corners[0], corners[1])},
		{id: "ne", point: corners[1]}, {id: "e", point: middle(corners[1], corners[2])},
		{id: "se", point: corners[2]}, {id: "s", point: middle(corners[2], corners[3])},
		{id: "sw", point: corners[3]}, {id: "w", point: middle(corners[3], corners[0])},
	}
}

func resizeHandleDirection(handle string) (horizontal, vertical float64) {
	if strings.Contains(handle, "w") {
		horizontal = -1
	} else if strings.Contains(handle, "e") {
		horizontal = 1
	}
	if strings.Contains(handle, "n") {
		vertical = -1
	} else if strings.Contains(handle, "s") {
		vertical = 1
	}
	return horizontal, vertical
}

func rotatedCorners(rect image.Rectangle, rotation float64) [4]f32.Point {
	center := f32.Pt(float32(rect.Min.X+rect.Max.X)/2, float32(rect.Min.Y+rect.Max.Y)/2)
	radians := float32(rotation * math.Pi / 180)
	cosine, sine := float32(math.Cos(float64(radians))), float32(math.Sin(float64(radians)))
	rotate := func(point f32.Point) f32.Point {
		dx, dy := point.X-center.X, point.Y-center.Y
		return f32.Pt(center.X+dx*cosine-dy*sine, center.Y+dx*sine+dy*cosine)
	}
	return [4]f32.Point{
		rotate(f32.Pt(float32(rect.Min.X), float32(rect.Min.Y))),
		rotate(f32.Pt(float32(rect.Max.X), float32(rect.Min.Y))),
		rotate(f32.Pt(float32(rect.Max.X), float32(rect.Max.Y))),
		rotate(f32.Pt(float32(rect.Min.X), float32(rect.Max.Y))),
	}
}

func rotationHandle(rect image.Rectangle, rotation float64) f32.Point {
	center := f32.Pt(float32(rect.Min.X+rect.Max.X)/2, float32(rect.Min.Y+rect.Max.Y)/2)
	distance := float64(rect.Dy())/2 + 28
	radians := (rotation - 90) * math.Pi / 180
	return f32.Pt(center.X+float32(math.Cos(radians)*distance), center.Y+float32(math.Sin(radians)*distance))
}

func (model *gioEditor) rotationHandleHit(point image.Point, size image.Point) bool {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return false
	}
	layer := model.manifest.Layers[model.selected]
	if layer.Type != caseTheme.LayerImage && layer.Type != caseTheme.LayerBackgroundImage {
		return false
	}
	rect := model.layerRect(layer, size)
	handle := rotationHandle(rect, layer.Rotation)
	dx, dy := float64(point.X)-float64(handle.X), float64(point.Y)-float64(handle.Y)
	return dx*dx+dy*dy <= 15*15
}

func (model *gioEditor) layerResizable(layer caseTheme.Layer) bool {
	if layer.Type == caseTheme.LayerCanvas {
		return false
	}
	return !(model.isWaveshareTarget() && layer.Type == caseTheme.LayerBackgroundVideo)
}

func (model *gioEditor) resizeHandleHit(point image.Point, size image.Point) string {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return ""
	}
	layer := model.manifest.Layers[model.selected]
	if !model.layerResizable(layer) {
		return ""
	}
	closest, closestDistance := "", float64(13*13+1)
	for _, handle := range resizeHandlePoints(model.layerRect(layer, size), layer.Rotation) {
		dx := float64(point.X) - float64(handle.point.X)
		dy := float64(point.Y) - float64(handle.point.Y)
		distance := dx*dx + dy*dy
		if distance <= 13*13 && distance < closestDistance {
			closest, closestDistance = handle.id, distance
		}
	}
	return closest
}

func (model *gioEditor) canvasCursor(gtx layout.Context, size image.Point) {
	if model.dragging && model.dragMode == "rotate" {
		pointer.CursorGrabbing.Add(gtx.Ops)
		return
	}
	handle := model.resizeHandleHit(model.lastPoint, size)
	if strings.HasPrefix(model.dragMode, "resize-") {
		handle = strings.TrimPrefix(model.dragMode, "resize-")
	}
	switch handle {
	case "n", "s":
		pointer.CursorNorthSouthResize.Add(gtx.Ops)
	case "e", "w":
		pointer.CursorEastWestResize.Add(gtx.Ops)
	case "ne", "sw":
		pointer.CursorNorthEastSouthWestResize.Add(gtx.Ops)
	case "nw", "se":
		pointer.CursorNorthWestSouthEastResize.Add(gtx.Ops)
	default:
		if model.dragging {
			pointer.CursorGrabbing.Add(gtx.Ops)
		} else {
			pointer.CursorGrab.Add(gtx.Ops)
		}
	}
}

func (model *gioEditor) beginResize(handle string) {
	if handle == "" || model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := model.manifest.Layers[model.selected]
	horizontal, vertical := resizeHandleDirection(handle)
	centerX := layer.Bounds.X + layer.Bounds.Width/2
	centerY := layer.Bounds.Y + layer.Bounds.Height/2
	localX := -horizontal * layer.Bounds.Width / 2
	localY := -vertical * layer.Bounds.Height / 2
	radians := layer.Rotation * math.Pi / 180
	worldX := localX*math.Cos(radians) - localY*math.Sin(radians)
	worldY := localX*math.Sin(radians) + localY*math.Cos(radians)
	model.resizeHandle = handle
	model.resizeAnchor = f32.Pt(float32(centerX+worldX), float32(centerY+worldY))
	model.resizeBase = layer.Bounds
}

// resizeSelectedTo 以对侧边/角为锚点修改宽高；组件内容会在下一帧按新矩形重新排版。
func (model *gioEditor) resizeSelectedTo(point image.Point, size image.Point) {
	if model.resizeHandle == "" || model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := &model.manifest.Layers[model.selected]
	horizontal, vertical := resizeHandleDirection(model.resizeHandle)
	pointX := float64(point.X) / float64(max(1, size.X))
	pointY := float64(point.Y) / float64(max(1, size.Y))
	worldX := pointX - float64(model.resizeAnchor.X)
	worldY := pointY - float64(model.resizeAnchor.Y)
	radians := layer.Rotation * math.Pi / 180
	localX := worldX*math.Cos(radians) + worldY*math.Sin(radians)
	localY := -worldX*math.Sin(radians) + worldY*math.Cos(radians)
	minimumWidth := 12 / float64(max(1, model.manifest.Width))
	minimumHeight := 12 / float64(max(1, model.manifest.Height))
	if layer.Type != caseTheme.LayerImage && layer.Type != caseTheme.LayerBackgroundImage {
		minimumWidth = 24 / float64(max(1, model.manifest.Width))
		minimumHeight = 24 / float64(max(1, model.manifest.Height))
	}
	width, height := model.resizeBase.Width, model.resizeBase.Height
	if horizontal != 0 {
		width = math.Max(minimumWidth, horizontal*localX)
	}
	if vertical != 0 {
		height = math.Max(minimumHeight, vertical*localY)
	}
	maximum := 1.0
	if layer.Type == caseTheme.LayerImage || layer.Type == caseTheme.LayerBackgroundImage {
		maximum = 4
	}
	width, height = math.Min(maximum, width), math.Min(maximum, height)
	centerLocalX, centerLocalY := 0.0, 0.0
	if horizontal != 0 {
		centerLocalX = horizontal * width / 2
	}
	if vertical != 0 {
		centerLocalY = vertical * height / 2
	}
	centerX := float64(model.resizeAnchor.X) + centerLocalX*math.Cos(radians) - centerLocalY*math.Sin(radians)
	centerY := float64(model.resizeAnchor.Y) + centerLocalX*math.Sin(radians) + centerLocalY*math.Cos(radians)
	if horizontal == 0 {
		centerX = model.resizeBase.X + model.resizeBase.Width/2
	}
	if vertical == 0 {
		centerY = model.resizeBase.Y + model.resizeBase.Height/2
	}
	layer.Bounds = caseTheme.Rect{X: centerX - width/2, Y: centerY - height/2, Width: width, Height: height}
	if layer.Type == caseTheme.LayerImage || layer.Type == caseTheme.LayerBackgroundImage {
		layer.Bounds.X = math.Max(-width+1/float64(max(1, model.manifest.Width)), math.Min(1-1/float64(max(1, model.manifest.Width)), layer.Bounds.X))
		layer.Bounds.Y = math.Max(-height+1/float64(max(1, model.manifest.Height)), math.Min(1-1/float64(max(1, model.manifest.Height)), layer.Bounds.Y))
	} else {
		layer.Bounds.X = math.Max(0, math.Min(1-width, layer.Bounds.X))
		layer.Bounds.Y = math.Max(0, math.Min(1-height, layer.Bounds.Y))
	}
	model.syncComponentGeometryFields(layer)
	model.message = fmt.Sprintf("%s尺寸：%.0f × %.0f 像素", layerLabel(layer.Type), width*float64(model.manifest.Width), height*float64(model.manifest.Height))
}

func (model *gioEditor) layerRect(layer caseTheme.Layer, size image.Point) image.Rectangle {
	return image.Rect(
		int(math.Round(layer.Bounds.X*float64(size.X))),
		int(math.Round(layer.Bounds.Y*float64(size.Y))),
		int(math.Round((layer.Bounds.X+layer.Bounds.Width)*float64(size.X))),
		int(math.Round((layer.Bounds.Y+layer.Bounds.Height)*float64(size.Y))),
	)
}

func (model *gioEditor) layerCenter(layer caseTheme.Layer, size image.Point) image.Point {
	rect := model.layerRect(layer, size)
	return image.Pt((rect.Min.X+rect.Max.X)/2, (rect.Min.Y+rect.Max.Y)/2)
}

func (model *gioEditor) scaleSelected(scrollY float32, size image.Point) {
	if scrollY == 0 || model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := &model.manifest.Layers[model.selected]
	if layer.Type == caseTheme.LayerCanvas || (model.isWaveshareTarget() && layer.Type == caseTheme.LayerBackgroundVideo) {
		return
	}
	now := time.Now()
	if model.lastScaleUndo.IsZero() || now.Sub(model.lastScaleUndo) > 280*time.Millisecond {
		model.pushUndo()
	}
	model.lastScaleUndo = now
	factor := math.Exp(-float64(scrollY) * .0025)
	minimumFactor := math.Max(8/float64(max(1, size.X))/layer.Bounds.Width, 8/float64(max(1, size.Y))/layer.Bounds.Height)
	maximumFactor := math.Min(4/layer.Bounds.Width, 4/layer.Bounds.Height)
	centerX, centerY := layer.Bounds.X+layer.Bounds.Width/2, layer.Bounds.Y+layer.Bounds.Height/2
	if layer.Type != caseTheme.LayerImage && layer.Type != caseTheme.LayerBackgroundImage {
		// 动态组件必须保持在画布内，避免调整后应用主题被 Validate 拒绝。
		maximumFactor = math.Min(maximumFactor, math.Min(2*math.Min(centerX, 1-centerX)/layer.Bounds.Width, 2*math.Min(centerY, 1-centerY)/layer.Bounds.Height))
	}
	maximumFactor = math.Max(minimumFactor, maximumFactor)
	factor = math.Max(minimumFactor, math.Min(maximumFactor, factor))
	layer.Bounds.Width *= factor
	layer.Bounds.Height *= factor
	layer.Bounds.X = centerX - layer.Bounds.Width/2
	layer.Bounds.Y = centerY - layer.Bounds.Height/2
	model.syncComponentGeometryFields(layer)
	model.message = fmt.Sprintf("%s缩放：%.0f × %.0f 像素", layerLabel(layer.Type), layer.Bounds.Width*float64(model.manifest.Width), layer.Bounds.Height*float64(model.manifest.Height))
}

func normalizeDegrees(value float64) float64 {
	value = math.Mod(value, 360)
	if value > 180 {
		value -= 360
	} else if value <= -180 {
		value += 360
	}
	return value
}

func (model *gioEditor) handleCanvasEvents(gtx layout.Context, size image.Point) {
	for {
		eventValue, ok := gtx.Event(pointer.Filter{
			Target:  &model.canvasTag,
			Kinds:   pointer.Press | pointer.Drag | pointer.Release | pointer.Move | pointer.Cancel | pointer.Scroll,
			ScrollY: pointer.ScrollRange{Min: -10000, Max: 10000},
		})
		if !ok {
			return
		}
		pointerValue, pointerOK := eventValue.(pointer.Event)
		if !pointerOK {
			continue
		}
		point := image.Pt(int(pointerValue.Position.X), int(pointerValue.Position.Y))
		switch pointerValue.Kind {
		case pointer.Press:
			if point.X < 0 || point.Y < 0 || point.X > size.X || point.Y > size.Y {
				// Gio 会把同一指针的共享事件同时送给重叠处理器；画布外事件必须完全忽略。
				continue
			}
			gtx.Execute(key.FocusCmd{Tag: &model.keyTag})
			if handle := model.resizeHandleHit(point, size); handle != "" {
				model.beginHistoryGesture()
				model.beginResize(handle)
				model.dragMode, model.dragging, model.lastPoint = "resize-"+handle, true, point
				continue
			}
			if model.rotationHandleHit(point, size) {
				model.beginHistoryGesture()
				model.dragMode, model.dragging, model.lastPoint = "rotate", true, point
				layer := model.manifest.Layers[model.selected]
				center := model.layerCenter(layer, size)
				model.rotateOffset = layer.Rotation - math.Atan2(float64(point.Y-center.Y), float64(point.X-center.X))*180/math.Pi
				continue
			}
			hit := model.hitLayer(point, size)
			if hit != model.selected {
				model.lastScaleUndo = time.Time{}
				model.componentLayerID = ""
				model.lyricsLayerID = ""
			}
			model.selected = hit
			movable := model.selected >= 0 && model.selected < len(model.manifest.Layers) && model.manifest.Layers[model.selected].Type != caseTheme.LayerCanvas
			if movable && model.isWaveshareTarget() && model.manifest.Layers[model.selected].Type == caseTheme.LayerBackgroundVideo {
				movable = false
			}
			model.dragging, model.lastPoint, model.dragMode = movable, point, "move"
			if model.dragging {
				model.beginHistoryGesture()
			} else {
				// 画布底层只负责选择和设置颜色，不参与拖动。
				model.historyGesture = false
			}
		case pointer.Drag:
			if model.dragging {
				if strings.HasPrefix(model.dragMode, "resize-") {
					model.resizeSelectedTo(point, size)
				} else if model.dragMode == "rotate" {
					layer := &model.manifest.Layers[model.selected]
					center := model.layerCenter(*layer, size)
					layer.Rotation = normalizeDegrees(math.Atan2(float64(point.Y-center.Y), float64(point.X-center.X))*180/math.Pi + model.rotateOffset)
					model.syncComponentGeometryFields(layer)
				} else {
					dx, dy := float64(point.X-model.lastPoint.X)/float64(max(1, size.X)), float64(point.Y-model.lastPoint.Y)/float64(max(1, size.Y))
					model.moveSelectedBy(dx, dy)
				}
				model.lastPoint = point
			}
		case pointer.Scroll:
			model.scaleSelected(pointerValue.Scroll.Y, size)
		case pointer.Move:
			if model.dragging && pointerValue.Buttons == 0 {
				model.dragging, model.dragMode, model.historyGesture = false, "", false
			}
			model.lastPoint = point
		case pointer.Release, pointer.Cancel:
			model.dragging, model.dragMode, model.resizeHandle, model.historyGesture = false, "", "", false
		}
	}
}

// moveSelectedBy 按画布比例移动选中图层，并将其约束在画布内。
func (model *gioEditor) moveSelectedBy(dx, dy float64) {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := &model.manifest.Layers[model.selected]
	if layer.Type == caseTheme.LayerCanvas || (model.isWaveshareTarget() && layer.Type == caseTheme.LayerBackgroundVideo) {
		return
	}
	if layer.Type == caseTheme.LayerImage || layer.Type == caseTheme.LayerBackgroundImage {
		layer.Bounds.X = math.Max(-layer.Bounds.Width+0.01, math.Min(.99, layer.Bounds.X+dx))
		layer.Bounds.Y = math.Max(-layer.Bounds.Height+0.01, math.Min(.99, layer.Bounds.Y+dy))
	} else {
		layer.Bounds.X = math.Max(0, math.Min(1-layer.Bounds.Width, layer.Bounds.X+dx))
		layer.Bounds.Y = math.Max(0, math.Min(1-layer.Bounds.Height, layer.Bounds.Y+dy))
	}
	// 拖动后重建属性编辑值，避免旧文本在下一帧将位置覆盖回去。
	model.syncComponentGeometryFields(layer)
}

// moveSelectedLayer 按当前 ZIndex 顺序调整图层上下级，保证渲染和列表操作一致。
func (model *gioEditor) moveSelectedLayer(direction int) {
	if direction == 0 || model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	if model.manifest.Layers[model.selected].Type == caseTheme.LayerCanvas {
		model.message = "画布背景固定在最底层"
		return
	}
	if model.isWaveshareTarget() && model.manifest.Layers[model.selected].Type == caseTheme.LayerBackgroundVideo {
		model.message = "微雪视频固定为所有组件下方的全屏背景，不能调整层级"
		return
	}
	indices := make([]int, 0, len(model.manifest.Layers))
	for index, layer := range model.manifest.Layers {
		if layer.Type != caseTheme.LayerCanvas && !(model.isWaveshareTarget() && layer.Type == caseTheme.LayerBackgroundVideo) {
			indices = append(indices, index)
		}
	}
	sort.SliceStable(indices, func(left, right int) bool {
		return model.manifest.Layers[indices[left]].ZIndex < model.manifest.Layers[indices[right]].ZIndex
	})
	position := -1
	for index, layerIndex := range indices {
		if layerIndex == model.selected {
			position = index
			break
		}
	}
	target := position + direction
	if position < 0 || target < 0 || target >= len(indices) {
		model.message = "图层已经在当前方向的边界"
		return
	}
	model.pushUndo()
	baseZ := model.manifest.Layers[indices[0]].ZIndex
	indices[position], indices[target] = indices[target], indices[position]
	for order, layerIndex := range indices {
		model.manifest.Layers[layerIndex].ZIndex = baseZ + order*10
	}
	model.componentLayerID = ""
	model.message = map[int]string{1: "图层已上移一级", -1: "图层已下移一级"}[direction]
}

func (model *gioEditor) hitLayer(point image.Point, size image.Point) int {
	x, y := float64(point.X)/float64(max(1, size.X)), float64(point.Y)/float64(max(1, size.Y))
	insideCanvas := x >= 0 && x <= 1 && y >= 0 && y <= 1
	indices := make([]int, len(model.manifest.Layers))
	for index := range indices {
		indices[index] = index
	}
	sort.SliceStable(indices, func(a, b int) bool {
		return model.manifest.Layers[indices[a]].ZIndex > model.manifest.Layers[indices[b]].ZIndex
	})
	for _, index := range indices {
		layer := model.manifest.Layers[index]
		if layer.Type == caseTheme.LayerCanvas {
			continue
		}
		if !caseTheme.LayerVisible(layer) {
			continue
		}
		bounds := layer.Bounds
		centerX, centerY := bounds.X+bounds.Width/2, bounds.Y+bounds.Height/2
		radians := -layer.Rotation * math.Pi / 180
		dx, dy := x-centerX, y-centerY
		localX := dx*math.Cos(radians) - dy*math.Sin(radians) + centerX
		localY := dx*math.Sin(radians) + dy*math.Cos(radians) + centerY
		if localX >= bounds.X && localX <= bounds.X+bounds.Width && localY >= bounds.Y && localY <= bounds.Y+bounds.Height {
			return index
		}
	}
	if insideCanvas {
		for index := range model.manifest.Layers {
			if model.manifest.Layers[index].Type == caseTheme.LayerCanvas {
				return index
			}
		}
	}
	return -1
}

func (model *gioEditor) layoutProperties(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(10), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(model.layoutInspectorTabs),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, model.layoutInspectorContent)
			}),
		)
	})
}

func (model *gioEditor) layoutInspectorTabs(gtx layout.Context) layout.Dimensions {
	model.normalizeInspectorTab()
	tabs := model.inspectorTabOptions()
	children := make([]layout.FlexChild, 0, len(tabs))
	for index, tab := range tabs {
		index, tab := index, tab
		children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonState(gtx, &model.inspectorTabs[index], tab.label, func() { model.inspectorTab = tab.value }, model.inspectorTab == tab.value)
			})
		}))
	}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
}

func (model *gioEditor) inspectorTabOptions() []optionChoice {
	tabs := []optionChoice{{value: "layers", label: "图层"}, {value: "properties", label: "属性"}, {value: "presets", label: "预设"}}
	if model.isWaveshareTarget() {
		tabs = append(tabs,
			optionChoice{value: "waveshare-library", label: "模板"},
			optionChoice{value: "waveshare-settings", label: "设置"},
		)
	}
	if model.isCoolerMasterTarget() {
		tabs = append(tabs, optionChoice{value: "cooler-orientation", label: "方向"})
	}
	tabs = append(tabs, optionChoice{value: "program-settings", label: "程序"})
	return tabs
}

func (model *gioEditor) normalizeInspectorTab() {
	if !model.isWaveshareTarget() && (model.inspectorTab == "waveshare-library" || model.inspectorTab == "waveshare-settings") {
		// 微雪专属页签不会泄漏到其他设备界面。
		model.inspectorTab = "layers"
	}
	if !model.isCoolerMasterTarget() && model.inspectorTab == "cooler-orientation" {
		model.inspectorTab = "layers"
	}
}

func (model *gioEditor) layoutInspectorContent(gtx layout.Context) layout.Dimensions {
	switch model.inspectorTab {
	case "properties":
		return model.propertyList.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
			return model.layoutComponentFields(gtx)
		})
	case "presets":
		return model.layoutUniversalVisualPresets(gtx)
	case "waveshare-library":
		if model.isWaveshareTarget() {
			return model.layoutWaveshareLibrary(gtx)
		}
		model.inspectorTab = "layers"
		return model.layoutInspectorContent(gtx)
	case "waveshare-settings":
		if model.isWaveshareTarget() {
			return model.layoutWaveshareSettings(gtx)
		}
		model.inspectorTab = "layers"
		return model.layoutInspectorContent(gtx)
	case "cooler-orientation":
		if model.isCoolerMasterTarget() {
			return model.layoutCoolerCalibration(gtx)
		}
		model.inspectorTab = "layers"
		return model.layoutInspectorContent(gtx)
	case "program-settings":
		return model.layoutGPUSettings(gtx)
	default:
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return model.layerList.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
					return model.layoutLayerButtons(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return model.button(gtx, &model.addButton, "＋ 添加图层", func() { model.addLayer(caseTheme.LayerTelemetry) })
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return model.button(gtx, &model.deleteButton, "删除选中图层", model.removeLayer)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return model.buttonState(gtx, &model.raiseButton, "↑ 上移层级", func() { model.moveSelectedLayer(1) }, true)
							})
						}),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return model.buttonState(gtx, &model.lowerButton, "↓ 下移层级", func() { model.moveSelectedLayer(-1) }, true)
						}),
					)
				})
			}),
		)
	}
}

func (model *gioEditor) layoutLayerButtons(gtx layout.Context) layout.Dimensions {
	children := make([]layout.FlexChild, 0, len(model.manifest.Layers))
	for index, layer := range model.manifest.Layers {
		index, layer := index, layer
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := fmt.Sprintf("%02d  %s (%s)", index+1, layer.ID, layerLabel(layer.Type))
			return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if !model.isWaveshareTarget() {
					return model.buttonState(gtx, model.layerButton(index), label, func() {
						model.selectLayer(index)
					}, index == model.selected)
				}
				visible := caseTheme.LayerVisible(layer)
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return model.buttonState(gtx, model.layerButton(index), label, func() { model.selectLayer(index) }, index == model.selected)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						textValue := "○ 隐"
						if visible {
							textValue = "◉ 显"
						}
						return layout.Inset{Left: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return model.buttonSizedState(gtx, model.optionButton(fmt.Sprintf("layer-visible-%d", index)), textValue, func() { model.toggleLayerVisibility(index) }, 62, visible)
						})
					}),
				)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// layerButton 返回稳定的按钮存储；当前版本按选中后下一帧重建，数量有限，不影响交互。
func (model *gioEditor) layerButton(index int) *widget.Clickable {
	if len(model.layerButtons) != len(model.manifest.Layers) {
		model.layerButtons = make([]gioButton, len(model.manifest.Layers))
	}
	return &model.layerButtons[index].click
}

func (model *gioEditor) layoutComponentFields(gtx layout.Context) layout.Dimensions {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		textStyle := material.Body2(model.theme, "请先在画布或图层列表中选择一个图层。")
		textStyle.Color = color.NRGBA{R: 137, G: 159, B: 169, A: 255}
		return textStyle.Layout(gtx)
	}
	layer := &model.manifest.Layers[model.selected]
	model.ensureFields(layer)
	children := make([]layout.FlexChild, 0, len(model.componentEntries)+3)
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return material.Body2(model.theme, layerLabel(layer.Type)+" · 图层属性").Layout(gtx)
	}))
	if model.isWaveshareTarget() {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(7), Bottom: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutSelectedVisibility(gtx, layer)
			})
		}))
	}
	for index := range model.componentEntries {
		index := index
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberField(gtx, model.componentEntries[index])
		}))
	}
	if layer.Type == caseTheme.LayerCanvas || layer.Type == caseTheme.LayerImage || layer.Type == caseTheme.LayerBackgroundImage || layer.Type == caseTheme.LayerBackgroundVideo || layer.Type == caseTheme.LayerTelemetry || layer.Type == caseTheme.LayerAudioBars || layer.Type == caseTheme.LayerAudioRing || layer.Type == caseTheme.LayerLyrics || isWaveshareExtendedLayer(layer.Type) {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(model.theme, model.layerSpecificTitle(layer.Type)).Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutLayerSpecificFields(gtx, layer)
			}),
		)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layerSpecificTitle(layerType caseTheme.LayerType) string {
	if layerType == caseTheme.LayerLyrics {
		return "歌词来源与 LRC 文件"
	}
	if layerType == caseTheme.LayerCanvas {
		return "画布背景"
	}
	if layerType == caseTheme.LayerTelemetry || layerType == caseTheme.LayerAudioBars || layerType == caseTheme.LayerAudioRing {
		return "组件样式与数据"
	}
	if isWaveshareExtendedLayer(layerType) {
		return "微雪原生组件属性"
	}
	if layerType == caseTheme.LayerBackgroundVideo {
		return "视频资源与播放"
	}
	return "图片资源"
}

func (model *gioEditor) layoutLayerSpecificFields(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	var content func(layout.Context) layout.Dimensions
	switch layer.Type {
	case caseTheme.LayerCanvas:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutCanvasProperties(gtx, layer) }
	case caseTheme.LayerImage, caseTheme.LayerBackgroundImage, caseTheme.LayerBackgroundVideo:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutBackgroundProperties(gtx, layer) }
	case caseTheme.LayerTelemetry:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutTelemetryProperties(gtx, layer) }
	case caseTheme.LayerAudioBars:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutBarsProperties(gtx, layer) }
	case caseTheme.LayerAudioRing:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutRingProperties(gtx, layer) }
	case caseTheme.LayerLyrics:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutLyricsProperties(gtx, layer) }
	case caseTheme.LayerText:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutTextProperties(gtx, layer) }
	case caseTheme.LayerClock:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutClockProperties(gtx, layer) }
	case caseTheme.LayerProgressBar:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutProgressProperties(gtx, layer) }
	case caseTheme.LayerGauge:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutGaugeProperties(gtx, layer, false) }
	case caseTheme.LayerGradientGauge:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutGaugeProperties(gtx, layer, true) }
	case caseTheme.LayerLineChart:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutLineChartProperties(gtx, layer) }
	case caseTheme.LayerShape:
		content = func(gtx layout.Context) layout.Dimensions { return model.layoutShapeProperties(gtx, layer) }
	default:
		return layout.Dimensions{}
	}
	if !layerSupportsGPUTelemetrySelection(layer.Type) {
		return content(gtx)
	}
	// 监控 GPU 是全局采集来源。把入口放在组件属性的统一分发层，确保所有
	// 能绑定 GPU 指标的组件都可直接选择，且不会因当前先选了 CPU 指标而隐藏。
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(model.layoutTelemetryGPUSelector),
		layout.Rigid(content),
	)
}

func (model *gioEditor) ensureFields(layer *caseTheme.Layer) {
	if layer.Type == caseTheme.LayerCanvas {
		model.componentEntries = nil
		model.componentLayerID = layer.ID
		return
	}
	values := model.componentFieldValues(layer)
	if len(model.componentEntries) != len(values) || model.componentLayerID != layer.ID {
		model.componentEntries = make([]*numberField, len(values))
		for index, current := range values {
			model.componentEntries[index] = &numberField{label: current.label, apply: current.apply, last: current.value}
			model.componentEntries[index].field.set(strconv.FormatFloat(current.value, 'f', 3, 64))
		}
		model.componentLayerID = layer.ID
	} else {
		for index, current := range values {
			model.componentEntries[index].apply = current.apply
			model.componentEntries[index].last = current.value
		}
	}
}

// syncComponentGeometryFields 在画布手势中原位同步几何文本，避免每个拖动事件重建整组编辑器。
func (model *gioEditor) syncComponentGeometryFields(layer *caseTheme.Layer) {
	if layer == nil || model.componentLayerID != layer.ID {
		return
	}
	values := model.componentFieldValues(layer)
	if len(values) != len(model.componentEntries) {
		model.componentLayerID = ""
		return
	}
	for index, current := range values {
		field := model.componentEntries[index]
		if field == nil {
			model.componentLayerID = ""
			return
		}
		field.apply = current.apply
		field.last = current.value
		field.field.set(strconv.FormatFloat(current.value, 'f', 3, 64))
	}
}

func (model *gioEditor) layoutNumberField(gtx layout.Context, field *numberField) layout.Dimensions {
	value := layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(66), gtx.Metric.Dp(66)
			return material.Body2(model.theme, field.label).Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return model.layoutTextField(gtx, &field.field, field.label)
		}),
	)
	if parsed, err := strconv.ParseFloat(field.field.editor.Text(), 64); err == nil && parsed != field.last {
		model.pushUndo()
		field.apply(parsed)
		field.last = parsed
	}
	return value
}

func (model *gioEditor) layoutCanvasProperties(gtx layout.Context, _ *caseTheme.Layer) layout.Dimensions {
	choices := []struct{ value, label string }{{"transparent", "透明"}, {"black", "黑色"}, {"white", "白色"}}
	current := model.canvasColor()
	children := make([]layout.FlexChild, 0, len(choices)+1)
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		textStyle := material.Body2(model.theme, "透明背景在画布中显示为棋盘格；设备 JPEG 输出时透明区域显示为黑色。")
		textStyle.Color = color.NRGBA{R: 137, G: 159, B: 169, A: 255}
		return textStyle.Layout(gtx)
	}))
	for index, choice := range choices {
		index, choice := index, choice
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonState(gtx, &model.backgroundColors[index], choice.label, func() { model.setCanvasColor(choice.value) }, current == choice.value)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layoutBackgroundProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	path := model.backgroundResourcePath(layer)
	emptyText, buttonText := "尚未选择图片", "浏览并载入图片"
	if layer.Type == caseTheme.LayerBackgroundVideo {
		emptyText, buttonText = "尚未选择视频或海报", "浏览并载入视频"
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if layer.Type != caseTheme.LayerBackgroundVideo || !model.isWaveshareTarget() {
				return layout.Dimensions{}
			}
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				textStyle := material.Body2(model.theme, "微雪固件仅支持一个全屏视频背景：视频固定在最底层、不透明且不可旋转；图片与组件面板会预合成到视频中。")
				textStyle.Color = color.NRGBA{R: 137, G: 190, B: 198, A: 255}
				return textStyle.Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutPathDisplay(gtx, path, emptyText)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonState(gtx, &model.backgroundBrowse, buttonText, model.browseBackgroundForSelected, true)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if layer.Type != caseTheme.LayerBackgroundVideo {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutChoiceButtons(gtx, "video-fit", []optionChoice{{"cover", "裁切铺满"}, {"contain", "完整留边"}}, defaultOption(layer, "fit", "cover"), func(value string) {
					model.setSelectedOption("fit", value)
				})
			})
		}),
	)
}

func (model *gioEditor) layoutTelemetryProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	if model.isWaveshareTarget() {
		return model.layoutWaveshareTelemetryProperties(gtx, layer)
	}
	fields := []optionChoice{{"time", "时间"}, {"Cpu_Freq", "CPU"}, {"Gpu_Load", "GPU"}, {"Memory_Load", "内存"}, {"GPU_Temp", "显卡温度"}, {"GPU_Hotspot_Temp", "GPU 热点温度"}, {"GPU_Memory_Temp", "显存温度"}}
	selected := make(map[string]bool)
	for _, value := range optionStrings(layer.Options, "fields") {
		selected[value] = true
	}
	if len(selected) == 0 {
		selected["time"], selected["Cpu_Freq"], selected["Memory_Load"] = true, true, true
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutWaveshareVisualPresets(gtx, layer)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceButtons(gtx, "telemetry-style", []optionChoice{{"cards", "卡片"}, {"gauge", "仪表"}, {"compact", "紧凑"}}, defaultOption(layer, "style", "cards"), func(value string) { model.setSelectedOption("style", value) })
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(7)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutToggleChoices(gtx, "telemetry-field", fields, selected, func(value string) {
					model.toggleSelectedField(value)
				})
			})
		}),
	)
}

func (model *gioEditor) layoutBarsProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return model.layoutWaveshareBarsProperties(gtx, layer)
}

func (model *gioEditor) layoutRingProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	return model.layoutWaveshareRingProperties(gtx, layer)
}

func (model *gioEditor) layoutLyricsProperties(gtx layout.Context, layer *caseTheme.Layer) layout.Dimensions {
	model.ensureLyricsFields(layer)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return model.layoutWaveshareVisualPresets(gtx, layer) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "播放器歌词样式") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutChoiceGrid(gtx, layer.ID+"-lyrics-style", []optionChoice{{"player", "流光播放器"}, {"karaoke", "卡拉 OK"}, {"classic", "经典双行"}, {"minimal", "极简"}}, defaultOption(layer, "style", "player"), func(value string) { model.setSelectedOption("style", value) }, 2)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "字体与层次") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutFontProperties(gtx, layer, caseTheme.DefaultLyricsFontSize)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "next_font_size", "下一句字号 (px)", caseTheme.DefaultLyricsNextFontSize, 8, 180, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "current_color", "当前句颜色", "F7FCFF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "next_color", "下一句颜色", "B8C8DAA4")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "accent_color", "播放高亮颜色", "55E5FF")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "描边、阴影与柔光")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "outline_width", "描边宽度 (px)", 2, 0, 8, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "outline_color", "描边颜色", "02070EE1")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutBoolOption(gtx, layer, "shadow", "启用文字阴影", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "shadow_color", "阴影颜色", "000000B4")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "shadow_offset_x", "阴影水平偏移", 2, -12, 12, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "shadow_offset_y", "阴影垂直偏移", 3, -12, 12, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "shadow_blur", "阴影柔化", 3, 0, 8, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "glow_color", "柔光颜色", "53DAFF42")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "glow_radius", "柔光半径", 4, 0, 12, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "播放过渡") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			style := caseTheme.LyricsStyle(*layer)
			if style != caseTheme.LyricsStylePlayer && style != caseTheme.LyricsStyleKaraoke {
				return layout.Dimensions{}
			}
			label := "按进度流光高亮"
			if model.selectedTargetUsesWaveshareLyrics() {
				label = "当前句使用强调色（微雪整句）"
			}
			return model.layoutBoolOption(gtx, layer, "karaoke_highlight", label, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			style := caseTheme.LyricsStyle(*layer)
			if style != caseTheme.LyricsStylePlayer && style != caseTheme.LyricsStyleKaraoke {
				return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Caption(model.theme, "当前样式使用稳定排版，不启用逐字高亮或换句淡入。").Layout(gtx)
				})
			}
			if model.selectedTargetUsesWaveshareLyrics() {
				return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Caption(model.theme, "微雪原生文字不支持逐字裁切或换句淡入；预览与实际屏幕均使用整句强调色，进度轨由主机每秒发送一个整数键驱动。").Layout(gtx)
				})
			}
			return model.layoutBoolOption(gtx, layer, "smooth_transition", "换句平滑淡入", true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			style := caseTheme.LyricsStyle(*layer)
			if style != caseTheme.LyricsStylePlayer && style != caseTheme.LyricsStyleKaraoke {
				return layout.Dimensions{}
			}
			if model.selectedTargetUsesWaveshareLyrics() {
				return layout.Dimensions{}
			}
			return model.layoutNumberOption(gtx, layer, "transition_fraction", "淡入占单句比例", .08, .01, .35, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if caseTheme.LyricsStyle(*layer) == caseTheme.LyricsStyleMinimal {
				return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Caption(model.theme, "极简样式固定为单行，不显示进度轨。").Layout(gtx)
				})
			}
			label := "显示细进度轨"
			if model.selectedTargetUsesWaveshareLyrics() {
				label = "显示真实进度轨（最多 10Hz 小键更新）"
			}
			return model.layoutBoolOption(gtx, layer, "show_progress", label, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_color", "歌词面板颜色", "030A1292")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutColorOption(gtx, layer, "panel_border_color", "面板描边颜色", "66E3FF3E")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return model.layoutNumberOption(gtx, layer, "corner_radius", "面板圆角 (px)", 14, 0, 100, false)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return sectionTitle(model, gtx, "歌词来源") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.Body2(model.theme, "来源平台").Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, model.layoutLyricsProviderButtons)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, model.layoutPlatformAuth)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return material.Body2(model.theme, "本地 LRC 文件").Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return model.layoutPathDisplay(gtx, strings.TrimSpace(model.lyricsPath.editor.Text()), "尚未选择 LRC 文件")
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return model.buttonSized(gtx, &model.lyricsBrowse, "浏览", model.browseLyricsForSelected, 76)
					})
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := "保存歌词图层设置"
				if model.lyricsSaveRunning {
					label = "正在保存歌词设置…"
				}
				return model.buttonState(gtx, &model.lyricsSave, label, model.saveLyrics, model.lyricsSaveRunning)
			})
		}),
	)
}

func (model *gioEditor) optionButton(id string) *widget.Clickable {
	if model.componentOptions == nil {
		model.componentOptions = make(map[string]*widget.Clickable)
	}
	if model.componentOptions[id] == nil {
		model.componentOptions[id] = &widget.Clickable{}
	}
	return model.componentOptions[id]
}

func (model *gioEditor) layoutChoiceButtons(gtx layout.Context, prefix string, choices []optionChoice, current string, apply func(string)) layout.Dimensions {
	return model.layoutChoiceGrid(gtx, prefix, choices, current, apply, 3)
}

func (model *gioEditor) layoutToggleChoices(gtx layout.Context, prefix string, choices []optionChoice, selected map[string]bool, apply func(string)) layout.Dimensions {
	return model.layoutToggleGrid(gtx, prefix, choices, selected, apply, 3)
}

func (model *gioEditor) layoutLyricsProviderButtons(gtx layout.Context) layout.Dimensions {
	providers := []struct{ id, label string }{{"auto", "自动（严格 ID）"}, {"netease", "网易"}, {"qq", "QQ"}, {"local", "本地"}}
	current := strings.TrimSpace(model.lyricsProvider.editor.Text())
	children := make([]layout.FlexChild, 0, len(providers))
	for index, provider := range providers {
		index, provider := index, provider
		children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonState(gtx, &model.lyricsProviders[index], provider.label, func() {
					if strings.TrimSpace(model.lyricsProvider.editor.Text()) != provider.id {
						model.pushUndo()
					}
					model.lyricsProvider.set(provider.id)
					model.updateSelectedLyricsOptions()
				}, current == provider.id)
			})
		}))
	}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
}

func (model *gioEditor) layoutPlatformAuth(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					label := material.Body2(model.theme, platformAuthHostHeading)
					label.Color = color.NRGBA{R: 184, G: 207, B: 215, A: 255}
					return label.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return model.buttonSized(gtx, model.optionButton("platform-area-refresh"), "刷新状态", model.refreshPlatformArea, 82)
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutPlatformAuthRow(gtx, "qq", "QQ 音乐")
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutPlatformAuthRow(gtx, "netease", "网易云音乐")
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Label(model.theme, unit.Sp(11), platformAuthHostNotice)
				label.Color = color.NRGBA{R: 126, G: 151, B: 161, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
}

func (model *gioEditor) layoutPlatformAuthRow(gtx layout.Context, platform, labelText string) layout.Dimensions {
	statusText := platformAuthStatusSummary(model.platformAuthStatuses[platform], time.Now())
	if model.platformAuthLoading {
		statusText = "正在更新…"
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			label := material.Label(model.theme, unit.Sp(12), labelText+" · "+statusText)
			label.MaxLines = 1
			return label.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonSized(gtx, model.optionButton("platform-auth-login-"+platform), "打开登录页", func() {
					model.openPlatformLogin(platform, labelText)
				}, 82)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonSized(gtx, model.optionButton("platform-auth-import-"+platform), "导入登录态", func() {
					model.importPlatformAuth(platform, labelText)
				}, 96)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonSized(gtx, model.optionButton("platform-auth-clear-"+platform), "清除登录态", func() {
					model.clearPlatformAuth(platform, labelText)
				}, 92)
			})
		}),
	)
}

func platformAuthStateText(state string) string {
	switch strings.TrimSpace(state) {
	case "not_configured":
		return "未配置；公开歌词无需登录"
	case "configured":
		return "登录态已配置"
	case "expired":
		return "登录态已过期"
	case "needs_login":
		return "需要登录或会员权限"
	default:
		return "状态未读取"
	}
}

func (model *gioEditor) layoutPathDisplay(gtx layout.Context, path, emptyText string) layout.Dimensions {
	if strings.TrimSpace(path) == "" {
		path = emptyText
	}
	height := gtx.Metric.Dp(32)
	gtx.Constraints.Min.Y, gtx.Constraints.Max.Y = height, height
	if gtx.Constraints.Max.X > 0 {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
	}
	size := gtx.Constraints.Min
	rounded := clip.UniformRRect(image.Rectangle{Max: size}, gtx.Metric.Dp(5))
	paint.FillShape(gtx.Ops, color.NRGBA{R: 10, G: 20, B: 25, A: 255}, rounded.Op(gtx.Ops))
	paint.FillShape(gtx.Ops, color.NRGBA{R: 48, G: 68, B: 76, A: 255}, clip.Stroke{Path: rounded.Path(gtx.Ops), Width: 1}.Op())
	stack := clip.Rect{Max: size}.Push(gtx.Ops)
	defer stack.Pop()
	return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(5), Left: unit.Dp(8), Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		label := material.Label(model.theme, unit.Sp(12), path)
		label.Color = color.NRGBA{R: 160, G: 181, B: 190, A: 255}
		label.MaxLines = 1
		return label.Layout(gtx)
	})
}

func (model *gioEditor) layoutFooter(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return material.Body2(model.theme, model.message).Layout(gtx)
	})
}

func (model *gioEditor) layoutTextField(gtx layout.Context, field *textField, hint string) layout.Dimensions {
	field.editor.SingleLine = true
	height := gtx.Metric.Dp(32)
	gtx.Constraints.Min.Y, gtx.Constraints.Max.Y = height, height
	focused := gtx.Focused(&field.editor)
	dims := layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			size := gtx.Constraints.Min
			background := color.NRGBA{R: 10, G: 20, B: 25, A: 255}
			border := color.NRGBA{R: 48, G: 68, B: 76, A: 255}
			if focused {
				border = color.NRGBA{R: 48, G: 211, B: 220, A: 255}
			}
			rounded := clip.UniformRRect(image.Rectangle{Max: size}, gtx.Metric.Dp(5))
			paint.FillShape(gtx.Ops, background, rounded.Op(gtx.Ops))
			paint.FillShape(gtx.Ops, border, clip.Stroke{Path: rounded.Path(gtx.Ops), Width: 1}.Op())
			return layout.Dimensions{Size: size}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(5), Bottom: unit.Dp(5), Left: unit.Dp(8), Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				style := material.Editor(model.theme, &field.editor, hint)
				style.TextSize = unit.Sp(13)
				style.HintColor = color.NRGBA{R: 112, G: 135, B: 145, A: 255}
				return style.Layout(gtx)
			})
		},
	)
	// last 是属性编辑器与 manifest 之间的同步基线，必须由调用方在写回后更新。
	// 若在这里提前覆盖，文本和数值属性会误判为“没有变化”。
	return dims
}

func (model *gioEditor) button(gtx layout.Context, click *widget.Clickable, text string, action func()) layout.Dimensions {
	return model.buttonState(gtx, click, text, action, false)
}

func (model *gioEditor) buttonState(gtx layout.Context, click *widget.Clickable, textValue string, action func(), active bool) layout.Dimensions {
	// Gio 的 Clickable.Layout 会先消费当前帧的点击事件，因此必须在布局前读取点击。
	// 动作完成后主动请求下一帧，立即更新目标选中态和底部状态文字。
	if click.Clicked(gtx) && action != nil {
		action()
		gtx.Execute(op.InvalidateCmd{})
	}
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return model.buttonSurface(gtx, textValue, active, click.Hovered(), click.Pressed())
	})
}

func (model *gioEditor) buttonSurface(gtx layout.Context, textValue string, active, hovered, pressed bool) layout.Dimensions {
	height := gtx.Metric.Dp(32)
	gtx.Constraints.Min.Y, gtx.Constraints.Max.Y = height, height
	if gtx.Constraints.Max.X > 0 {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
	}
	size := gtx.Constraints.Min
	background, border, foreground := controlColors(model.theme.Fg, active, hovered, pressed)
	if hovered {
		pointer.CursorPointer.Add(gtx.Ops)
	}
	rounded := clip.UniformRRect(image.Rectangle{Max: size}, gtx.Metric.Dp(5))
	paint.FillShape(gtx.Ops, background, rounded.Op(gtx.Ops))
	paint.FillShape(gtx.Ops, border, clip.Stroke{Path: rounded.Path(gtx.Ops), Width: 1}.Op())
	label := material.Label(model.theme, unit.Sp(13), textValue)
	label.Color = foreground
	label.MaxLines = 1
	return layout.Center.Layout(gtx, label.Layout)
}

// controlColors 为普通、选中、悬停和按下状态提供明确的视觉反馈。
func controlColors(foreground color.NRGBA, active, hovered, pressed bool) (color.NRGBA, color.NRGBA, color.NRGBA) {
	background := color.NRGBA{R: 28, G: 42, B: 49, A: 255}
	border := color.NRGBA{R: 52, G: 72, B: 80, A: 255}
	if active {
		background = color.NRGBA{R: 23, G: 78, B: 84, A: 255}
		border = color.NRGBA{R: 45, G: 204, B: 214, A: 255}
	}
	if hovered {
		background = color.NRGBA{R: 38, G: 61, B: 69, A: 255}
		border = color.NRGBA{R: 67, G: 167, B: 176, A: 255}
	}
	if pressed {
		background = color.NRGBA{R: 44, G: 192, B: 202, A: 255}
		border = color.NRGBA{R: 94, G: 234, B: 240, A: 255}
		foreground = color.NRGBA{R: 4, G: 22, B: 25, A: 255}
	}
	return background, border, foreground
}

func (model *gioEditor) buttonSized(gtx layout.Context, click *widget.Clickable, text string, action func(), width int) layout.Dimensions {
	gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(unit.Dp(width)), gtx.Metric.Dp(unit.Dp(width))
	return model.button(gtx, click, text, action)
}

func (model *gioEditor) buttonSizedState(gtx layout.Context, click *widget.Clickable, text string, action func(), width int, active bool) layout.Dimensions {
	gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(unit.Dp(width)), gtx.Metric.Dp(unit.Dp(width))
	return model.buttonState(gtx, click, text, action, active)
}

func (model *gioEditor) buttonSizedEnabledState(gtx layout.Context, click *widget.Clickable, text string, action func(), width int, active, enabled bool) layout.Dimensions {
	if !enabled {
		gtx = gtx.Disabled()
		action = nil
	}
	return model.buttonSizedState(gtx, click, text, action, width, active)
}

func (model *gioEditor) refreshFromService() {
	if err := model.api.Connect(); err != nil {
		model.message = "离线编辑：" + err.Error()
		return
	}
	if status, err := model.api.Status(); err == nil {
		model.setAudioPreviewStatus(status.Audio)
		model.updateTargetOptions(status, false)
		model.targetStatus.lastRefresh = time.Now()
		options := model.targetOptions
		if len(options) > 0 {
			target := model.preferredTarget(model.target)
			model.selectTarget(target)
		} else {
			model.target = ""
			model.message = "尚未配置目标副屏"
		}
	}
	if cfg, err := model.api.Config(); err == nil {
		provider := strings.TrimSpace(cfg.LyricsProvider)
		if provider == "" {
			provider = "auto"
		}
		model.lyricsDefaultProvider, model.lyricsDefaultPath = provider, cfg.LyricsPath
		model.lyricsProvider.set(provider)
		model.lyricsPath.set(cfg.LyricsPath)
	}
	if statuses, err := model.api.PlatformAuthStatuses(); err == nil {
		model.setPlatformAuthStatuses(statuses)
	}
	if model.target != "" {
		model.message = "已连接统一协议服务；当前主题仅属于 " + model.target
	}
}

func (model *gioEditor) setPlatformAuthStatuses(statuses []PlatformAuthStatus) {
	if model.platformAuthStatuses == nil {
		model.platformAuthStatuses = make(map[string]PlatformAuthStatus)
	}
	for _, status := range statuses {
		model.platformAuthStatuses[status.Platform] = status
	}
}

func (model *gioEditor) refreshPlatformAuth() {
	if model.platformAuthLoading {
		model.message = "平台登录态正在更新，请稍候"
		return
	}
	if model.api == nil {
		model.message = "未连接 CaseScreen 服务"
		return
	}
	model.platformAuthLoading = true
	model.message = "正在刷新平台登录态…"
	model.ensureUIUpdates()
	go func() {
		statuses, refreshErr := model.api.PlatformAuthStatuses()
		model.uiUpdates <- func() {
			model.platformAuthLoading = false
			if refreshErr != nil {
				model.message = refreshErr.Error()
				return
			}
			model.setPlatformAuthStatuses(statuses)
			model.message = "平台登录态已刷新"
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) refreshPlatformArea() {
	if model.platformAuthLoading {
		model.message = "主机平台状态正在更新，请稍候"
		return
	}
	if model.api == nil {
		model.message = "未连接 CaseScreen 服务"
		return
	}
	model.platformAuthLoading = true
	model.message = "正在刷新平台登录态…"
	model.ensureUIUpdates()
	go func() {
		statuses, authErr := model.api.PlatformAuthStatuses()
		model.uiUpdates <- func() {
			model.platformAuthLoading = false
			if authErr == nil {
				model.setPlatformAuthStatuses(statuses)
			}
			if authErr != nil {
				model.message = authErr.Error()
				return
			}
			model.message = "平台登录态已刷新"
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) importPlatformAuth(platform, label string) {
	if model.platformAuthLoading {
		model.message = "平台登录态正在更新，请稍候"
		return
	}
	path, err := choosePlatformCookieFile()
	if errors.Is(err, errFileDialogCancelled) {
		return
	}
	if err != nil {
		model.message = err.Error()
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		model.message = "读取登录态文件失败：" + err.Error()
		return
	}
	if info.Size() <= 0 || info.Size() > 256<<10 {
		model.message = "登录态文件必须大于 0 且不超过 256 KiB"
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		model.message = "读取登录态文件失败：" + err.Error()
		return
	}
	model.platformAuthLoading = true
	model.message = "正在导入" + label + "登录态…"
	model.ensureUIUpdates()
	go func() {
		result, importErr := model.api.ImportPlatformAuthNetscape(platform, string(data))
		model.uiUpdates <- func() {
			model.platformAuthLoading = false
			if importErr != nil {
				model.message = importErr.Error()
				return
			}
			model.setPlatformAuthStatuses([]PlatformAuthStatus{result.Status})
			model.message = platformAuthImportedMessage(label, result)
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) clearPlatformAuth(platform, label string) {
	if model.platformAuthLoading {
		model.message = "平台登录态正在更新，请稍候"
		return
	}
	model.platformAuthLoading = true
	model.message = "正在退出" + label + "…"
	model.ensureUIUpdates()
	go func() {
		status, clearErr := model.api.ClearPlatformAuth(platform)
		model.uiUpdates <- func() {
			model.platformAuthLoading = false
			if clearErr != nil {
				model.message = clearErr.Error()
				return
			}
			model.setPlatformAuthStatuses([]PlatformAuthStatus{status})
			model.message = platformAuthClearedMessage(label)
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) importJSON() {
	path := strings.TrimSpace(model.jsonPath.editor.Text())
	if path == "" {
		model.message = "请在顶部输入主题 JSON 的绝对路径"
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		model.message = "读取主题 JSON 失败：" + err.Error()
		return
	}
	var manifest caseTheme.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		model.message = "主题 JSON 解析失败：" + err.Error()
		return
	}
	model.pushUndo()
	model.manifest = manifest
	model.adaptManifestToTarget()
	model.selected = 0
	model.componentLayerID = ""
	model.lyricsLayerID = ""
	model.message = "主题 JSON 已载入可视化编辑器"
}

func (model *gioEditor) browseBackgroundForSelected() {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		model.message = "请先选择图片图层"
		return
	}
	layerType := model.manifest.Layers[model.selected].Type
	if layerType != caseTheme.LayerImage && layerType != caseTheme.LayerBackgroundImage && layerType != caseTheme.LayerBackgroundVideo {
		model.message = "请先选中图片或视频图层"
		return
	}
	choose := chooseImageFile
	if layerType == caseTheme.LayerBackgroundVideo {
		choose = chooseBackgroundFile
	}
	path, err := choose()
	if errors.Is(err, errFileDialogCancelled) {
		return
	}
	if err != nil {
		model.message = err.Error()
		return
	}
	model.importMediaPath(path)
}

func (model *gioEditor) importBackgroundPath(path string) {
	model.importMediaPath(path)
}

func (model *gioEditor) importMediaPath(path string) {
	path = strings.TrimSpace(path)
	if path == "" || model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		model.message = "请先选择图片图层并选取图片文件"
		return
	}
	layer := &model.manifest.Layers[model.selected]
	if layer.Type != caseTheme.LayerImage && layer.Type != caseTheme.LayerBackgroundImage && layer.Type != caseTheme.LayerBackgroundVideo {
		model.message = "当前选中的不是图片或视频图层"
		return
	}
	isVideo := isVideoPath(path)
	if layer.Type == caseTheme.LayerBackgroundVideo && isVideo {
		resource, err := caseTheme.InspectResource("video-"+strconv.FormatInt(time.Now().UnixNano(), 10), path, filepath.Base(path))
		if err != nil {
			model.message = "读取视频资源失败：" + err.Error()
			return
		}
		model.pushUndo()
		model.manifest.Resources = append(model.manifest.Resources, resource)
		if layer.Options == nil {
			layer.Options = map[string]any{}
		}
		layer.Options["resource_id"] = resource.ID
		delete(layer.Options, "poster_resource_id")
		model.normalizeWaveshareVideoLayer(layer)
		model.componentLayerID = ""
		model.inspectorTab = "properties"
		if model.isWaveshareTarget() {
			model.message = "视频已载入；微雪会作为全屏最底层背景，并预合成上方静态图层"
		} else {
			model.message = "视频已载入；设备会按协议使用视频或主机合成帧"
		}
		return
	}
	input, err := os.Open(path)
	if err != nil {
		model.message = "读取图片失败：" + err.Error()
		return
	}
	configuration, _, decodeErr := image.DecodeConfig(input)
	_ = input.Close()
	if decodeErr != nil || configuration.Width <= 0 || configuration.Height <= 0 {
		model.message = "图片格式无法识别，请选择 JPG 或 PNG"
		return
	}
	resource, err := caseTheme.InspectResource("image-"+strconv.FormatInt(time.Now().UnixNano(), 10), path, filepath.Base(path))
	if err != nil {
		model.message = "读取图片资源失败：" + err.Error()
		return
	}
	model.pushUndo()
	model.manifest.Resources = append(model.manifest.Resources, resource)
	if layer.Options == nil {
		layer.Options = map[string]any{}
	}
	layer.Options["resource_id"] = resource.ID
	if layer.Type == caseTheme.LayerBackgroundVideo {
		layer.Options["poster_resource_id"] = resource.ID
		model.normalizeWaveshareVideoLayer(layer)
		model.inspectorTab = "properties"
		model.componentLayerID = ""
		model.message = "视频海报已载入；可继续选择视频文件替换资源"
		return
	}
	layer.Type = caseTheme.LayerImage
	canvasWidth, canvasHeight := max(1, model.manifest.Width), max(1, model.manifest.Height)
	width, height := fittedImageBounds(configuration.Width, configuration.Height, canvasWidth, canvasHeight)
	centerX, centerY := .5, .5
	if layer.Bounds.Width > 0 && layer.Bounds.Height > 0 {
		centerX, centerY = layer.Bounds.X+layer.Bounds.Width/2, layer.Bounds.Y+layer.Bounds.Height/2
	}
	layer.Bounds = caseTheme.Rect{X: centerX - width/2, Y: centerY - height/2, Width: width, Height: height}
	layer.Rotation = 0
	delete(model.imageCache, path)
	delete(model.imageOps, path)
	model.componentLayerID = ""
	model.message = fmt.Sprintf("图片已按 %.0f × %.0f 像素插入；滚轮可等比缩放，上方 ↻ 可旋转", width*float64(canvasWidth), height*float64(canvasHeight))
}

func isVideoPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".gif":
		return true
	default:
		return false
	}
}

func fittedImageBounds(imageWidth, imageHeight, canvasWidth, canvasHeight int) (float64, float64) {
	canvasWidth, canvasHeight = max(1, canvasWidth), max(1, canvasHeight)
	scale := math.Min(1, math.Min(float64(canvasWidth)/float64(max(1, imageWidth)), float64(canvasHeight)/float64(max(1, imageHeight))))
	return float64(imageWidth) * scale / float64(canvasWidth), float64(imageHeight) * scale / float64(canvasHeight)
}

func (model *gioEditor) backgroundResourcePath(layer *caseTheme.Layer) string {
	resourceID := layerOptionString(layer, "resource_id")
	if resourceID == "" {
		resourceID = layerOptionString(layer, "poster_resource_id")
	}
	for _, resource := range model.manifest.Resources {
		if resource.ID == resourceID {
			return resource.LocalPath
		}
	}
	return ""
}

func (model *gioEditor) loadLayerImage(layer caseTheme.Layer) image.Image {
	resourceID, _ := layer.Options["resource_id"].(string)
	if layer.Type == caseTheme.LayerBackgroundVideo {
		if imageID, _ := layer.Options["poster_resource_id"].(string); imageID != "" {
			resourceID = imageID
		}
	}
	for _, resource := range model.manifest.Resources {
		if resource.ID != resourceID || resource.LocalPath == "" {
			continue
		}
		if cached, ok := model.imageCache[resource.LocalPath]; ok {
			return cached
		}
		file, err := os.Open(resource.LocalPath)
		if err != nil {
			return nil
		}
		decoded, _, decodeErr := image.Decode(file)
		_ = file.Close()
		if decodeErr != nil {
			return nil
		}
		if model.imageCache == nil {
			model.imageCache = make(map[string]image.Image)
		}
		model.imageCache[resource.LocalPath] = decoded
		return decoded
	}
	return nil
}

// loadLayerImageOp 缓存 Gio 图像操作，避免动画预览每帧重复转换整张 PNG/JPEG 并上传纹理。
func (model *gioEditor) loadLayerImageOp(layer caseTheme.Layer) (paint.ImageOp, bool) {
	path := model.backgroundResourcePath(&layer)
	if path == "" {
		return paint.ImageOp{}, false
	}
	if cached, ok := model.imageOps[path]; ok {
		return cached, true
	}
	decoded := model.loadLayerImage(layer)
	if decoded == nil {
		return paint.ImageOp{}, false
	}
	operation := paint.NewImageOp(decoded)
	if model.imageOps == nil {
		model.imageOps = make(map[string]paint.ImageOp)
	}
	model.imageOps[path] = operation
	return operation, true
}

func (model *gioEditor) applyTheme() {
	if model.applyRunning {
		model.message = "主题正在应用，请稍候"
		return
	}
	if model.coolerCalibration.loadedTarget == model.target && model.coolerCalibration.testVisible {
		model.message = "方向测试图仍在显示；请先确认方向或取消测试"
		return
	}
	if model.target == "" {
		model.message = "请先选择一台目标副屏"
		return
	}
	model.adaptManifestToTarget()
	if model.manifest.ID == "" {
		model.manifest.ID = "casescreen-theme"
	}
	if model.manifest.Name == "" {
		model.manifest.Name = "机箱副屏主题"
	}
	manifestData, err := json.Marshal(model.manifest)
	if err != nil {
		model.message = "准备主题失败：" + err.Error()
		return
	}
	var manifest caseTheme.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		model.message = "准备主题失败：" + err.Error()
		return
	}
	target := model.target
	if model.uiUpdates == nil {
		model.uiUpdates = make(chan func(), 8)
	}
	model.applyRunning = true
	model.message = "正在应用主题到 " + target + "；首次上传资源可能需要较长时间"
	go func() {
		result, applyErr := model.api.ApplyTheme(manifest, target)
		model.uiUpdates <- func() {
			model.applyRunning = false
			if applyErr != nil {
				model.message = applyErr.Error()
				return
			}
			model.message = themeApplyMessage(result)
			model.refreshWaveshareActiveTemplate(target)
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) selectTarget(target string) {
	option := model.targetOption(target)
	if option == nil {
		model.target = ""
		model.message = "请选择一台有效的目标副屏"
		return
	}
	model.targetLoadVersion++
	model.targetLoading = ""
	var manifest caseTheme.Manifest
	var err error
	if model.api == nil {
		err = errors.New("未连接 CaseScreen 服务")
	} else {
		manifest, err = model.api.Theme(target)
	}
	model.finishTargetSelection(target, manifest, err)
}

func (model *gioEditor) finishTargetSelection(target string, manifest caseTheme.Manifest, loadErr error) {
	option := model.targetOption(target)
	if option == nil {
		model.message = "目标副屏已不在设备状态中：" + target
		return
	}
	if model.target != target {
		model.waveshareLibrary.rememberPlaylistDraft()
		model.coolerCalibration.reset(target)
	}
	model.target = target
	model.undoStack, model.redoStack = nil, nil
	if loadErr != nil || manifest.ID == "" {
		model.manifest = defaultManifestFor(option.resolution[0], option.resolution[1])
		model.selected = 0
		model.componentLayerID = ""
		model.lyricsLayerID = ""
		if loadErr != nil {
			model.message = targetSelectionMessage(option, "已选择 "+option.label+"；读取已有主题失败，暂用透明画布："+loadErr.Error())
		} else {
			model.message = targetSelectionMessage(option, "已选择 "+option.label+"；已按该屏分辨率建立透明画布")
		}
		return
	}
	model.manifest = manifest
	model.adaptManifestToTarget()
	model.selected = 0
	model.componentLayerID = ""
	model.lyricsLayerID = ""
	model.message = targetSelectionMessage(option, "已载入目标主题："+target)
}

func (model *gioEditor) targetOption(target string) *targetOption {
	for index := range model.targetOptions {
		if model.targetOptions[index].id == target {
			return &model.targetOptions[index]
		}
	}
	return nil
}

func (model *gioEditor) targetConnected(target string) bool {
	option := model.targetOption(target)
	return option != nil && option.connected
}

func (model *gioEditor) targetPoweredOff(target string) bool {
	option := model.targetOption(target)
	return option != nil && option.poweredOff
}

func (model *gioEditor) allPowerTargetsOff() (allOff bool, supported int) {
	allOff = true
	for _, option := range model.targetOptions {
		if !option.brightnessSupported {
			continue
		}
		supported++
		if !option.poweredOff {
			allOff = false
		}
	}
	return allOff, supported
}

// preferredTarget 在首次进入工作台时优先选择在线设备，避免把另一台副屏在线
// 误认为当前离线目标也可直接应用。用户主动选中的在线目标会保持不变。
func (model *gioEditor) preferredTarget(current string) string {
	if option := model.targetOption(current); option != nil && option.connected {
		return current
	}
	for _, option := range model.targetOptions {
		if option.connected {
			return option.id
		}
	}
	if option := model.targetOption(current); option != nil {
		return current
	}
	if len(model.targetOptions) > 0 {
		return model.targetOptions[0].id
	}
	return ""
}

func (model *gioEditor) adaptManifestToTarget() {
	option := model.targetOption(model.target)
	if option != nil && option.resolution[0] > 0 && option.resolution[1] > 0 {
		model.manifest.Width, model.manifest.Height = option.resolution[0], option.resolution[1]
	}
	model.manifest = caseTheme.NormalizeScene(model.manifest)
	model.fitMigratedImages()
	if model.isWaveshareTarget() {
		for index := range model.manifest.Layers {
			model.normalizeWaveshareVideoLayer(&model.manifest.Layers[index])
		}
	}
}

func (model *gioEditor) normalizeWaveshareVideoLayer(layer *caseTheme.Layer) {
	if layer == nil || !model.isWaveshareTarget() || layer.Type != caseTheme.LayerBackgroundVideo {
		return
	}
	layer.Bounds = caseTheme.Rect{Width: 1, Height: 1}
	layer.Opacity = 1
	layer.Rotation = 0
	layer.ZIndex = -9000
	if layer.Options == nil {
		layer.Options = map[string]any{}
	}
	if strings.TrimSpace(layerOptionString(layer, "fit")) == "" {
		layer.Options["fit"] = "cover"
	}
}

func (model *gioEditor) fitMigratedImages() {
	for index := range model.manifest.Layers {
		layer := &model.manifest.Layers[index]
		migrated, _ := layer.Options["migrated_legacy_background"].(bool)
		if layer.Type != caseTheme.LayerImage || !migrated {
			continue
		}
		path := model.backgroundResourcePath(layer)
		input, err := os.Open(path)
		if err != nil {
			continue
		}
		configuration, _, decodeErr := image.DecodeConfig(input)
		_ = input.Close()
		if decodeErr != nil || configuration.Width <= 0 || configuration.Height <= 0 {
			continue
		}
		width, height := fittedImageBounds(configuration.Width, configuration.Height, model.manifest.Width, model.manifest.Height)
		layer.Bounds = caseTheme.Rect{X: .5 - width/2, Y: .5 - height/2, Width: width, Height: height}
		delete(layer.Options, "migrated_legacy_background")
		delete(layer.Options, "fit")
	}
}

func (model *gioEditor) addLayer(layerType caseTheme.LayerType) {
	model.addLayerAt(layerType, .5, .5)
}

func (model *gioEditor) addLayerAt(layerType caseTheme.LayerType, centerX, centerY float64) {
	if layerType == caseTheme.LayerCanvas {
		model.message = "画布背景已存在，请直接选择它设置透明、黑色或白色。"
		return
	}
	if layerType == caseTheme.LayerBackgroundImage {
		layerType = caseTheme.LayerImage
	}
	if isWaveshareExtendedLayer(layerType) && !model.isWaveshareTarget() {
		model.message = "该组件由微雪屏端原生渲染，请先选择微雪副屏"
		return
	}
	if layerType == caseTheme.LayerBackgroundVideo && model.isWaveshareTarget() {
		for index, layer := range model.manifest.Layers {
			if layer.Type == caseTheme.LayerBackgroundVideo && caseTheme.LayerVisible(layer) && layer.Opacity > 0 {
				model.selectLayer(index)
				model.inspectorTab = "properties"
				model.message = "微雪只支持一个可见视频背景；已选中现有视频图层"
				return
			}
		}
	}
	model.pushUndo()
	index := len(model.manifest.Layers)
	width, height := .42, .20
	if layerType == caseTheme.LayerImage {
		width, height = .30, .30
	}
	if layerType == caseTheme.LayerBackgroundVideo {
		if model.isWaveshareTarget() {
			width, height = 1, 1
		} else {
			width, height = .48, .30
		}
	}
	if layerType == caseTheme.LayerAudioRing {
		width, height = .28, .36
	}
	switch layerType {
	case caseTheme.LayerText, caseTheme.LayerClock:
		width, height = .34, .12
	case caseTheme.LayerProgressBar:
		width, height = .42, .12
	case caseTheme.LayerGauge, caseTheme.LayerGradientGauge:
		width, height = .26, .40
	case caseTheme.LayerLineChart:
		width, height = .42, .28
	case caseTheme.LayerShape:
		width, height = .30, .24
	}
	x := math.Max(0, math.Min(1-width, centerX-width/2))
	y := math.Max(0, math.Min(1-height, centerY-height/2))
	zIndex := index
	if layerType == caseTheme.LayerBackgroundVideo && model.isWaveshareTarget() {
		x, y, zIndex = 0, 0, -9000
	}
	options := map[string]any{"visible": true}
	switch layerType {
	case caseTheme.LayerTelemetry:
		options = model.defaultWaveshareOptions(layerType)
	case caseTheme.LayerAudioBars:
		options = model.defaultWaveshareOptions(layerType)
	case caseTheme.LayerAudioRing:
		options = model.defaultWaveshareOptions(layerType)
	case caseTheme.LayerLyrics:
		options = model.defaultWaveshareOptions(layerType)
	case caseTheme.LayerBackgroundVideo:
		options = map[string]any{"visible": true, "fit": "cover", "loop": true}
	case caseTheme.LayerText, caseTheme.LayerClock, caseTheme.LayerProgressBar, caseTheme.LayerGauge, caseTheme.LayerGradientGauge, caseTheme.LayerLineChart, caseTheme.LayerShape:
		options = model.defaultWaveshareOptions(layerType)
	}
	model.manifest.Layers = append(model.manifest.Layers, caseTheme.Layer{
		ID: model.uniqueLayerID(layerType), Type: layerType,
		Bounds: caseTheme.Rect{X: x, Y: y, Width: width, Height: height}, Opacity: 1, ZIndex: zIndex,
		Options: options,
	})
	model.selected = index
	model.componentLayerID = ""
	model.lyricsLayerID = ""
	if layerType == caseTheme.LayerImage {
		model.inspectorTab = "properties"
		model.message = "已添加图片图层；请在右侧属性中浏览图片文件"
	} else if layerType == caseTheme.LayerBackgroundVideo {
		model.inspectorTab = "properties"
		model.message = "已添加视频图层；请在右侧属性中浏览视频或选择海报"
	} else {
		model.message = "已添加 " + layerLabel(layerType) + "，可在画布上直接拖动"
	}
}

func (model *gioEditor) removeLayer() {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := model.manifest.Layers[model.selected]
	if layer.Type == caseTheme.LayerCanvas {
		model.message = "画布背景不能删除，可在属性中改为透明。"
		return
	}
	model.pushUndo()
	model.manifest.Layers = append(model.manifest.Layers[:model.selected], model.manifest.Layers[model.selected+1:]...)
	model.selected = max(0, model.selected-1)
	model.componentLayerID = ""
	model.lyricsLayerID = ""
}

func (model *gioEditor) hasBackground() bool {
	for _, layer := range model.manifest.Layers {
		if layer.Type == caseTheme.LayerCanvas {
			return true
		}
	}
	return false
}

func (model *gioEditor) canvasColor() string {
	for index := range model.manifest.Layers {
		layer := &model.manifest.Layers[index]
		if layer.Type != caseTheme.LayerCanvas {
			continue
		}
		if !caseTheme.LayerVisible(*layer) {
			return "transparent"
		}
		value := strings.ToLower(layerOptionString(layer, "color"))
		switch value {
		case "black", "white", "transparent":
			return value
		default:
			return "transparent"
		}
	}
	return "transparent"
}

func (model *gioEditor) setCanvasColor(value string) {
	for index := range model.manifest.Layers {
		layer := &model.manifest.Layers[index]
		if layer.Type != caseTheme.LayerCanvas {
			continue
		}
		if model.canvasColor() == value {
			return
		}
		model.pushUndo()
		if layer.Options == nil {
			layer.Options = make(map[string]any)
		}
		layer.Options["color"] = value
		model.message = "画布背景已设为" + map[string]string{"transparent": "透明", "black": "黑色", "white": "白色"}[value]
		return
	}
}

func cloneManifest(source caseTheme.Manifest) caseTheme.Manifest {
	data, err := json.Marshal(source)
	if err != nil {
		return source
	}
	var cloned caseTheme.Manifest
	if err := json.Unmarshal(data, &cloned); err != nil {
		return source
	}
	return cloned
}

func (model *gioEditor) currentSnapshot() editorSnapshot {
	return editorSnapshot{Manifest: cloneManifest(model.manifest), Selected: model.selected}
}

func (model *gioEditor) pushUndo() {
	model.undoStack = append(model.undoStack, model.currentSnapshot())
	if len(model.undoStack) > 80 {
		model.undoStack = append([]editorSnapshot(nil), model.undoStack[len(model.undoStack)-80:]...)
	}
	model.redoStack = nil
	model.requestPreviewRefresh()
}

func (model *gioEditor) beginHistoryGesture() {
	if model.historyGesture {
		return
	}
	model.pushUndo()
	model.historyGesture = true
}

func (model *gioEditor) restoreSnapshot(snapshot editorSnapshot) {
	model.manifest = cloneManifest(snapshot.Manifest)
	model.selected = snapshot.Selected
	if model.selected >= len(model.manifest.Layers) {
		model.selected = len(model.manifest.Layers) - 1
	}
	model.componentLayerID, model.lyricsLayerID = "", ""
	model.dragging, model.historyGesture = false, false
	model.requestPreviewRefresh()
}

func (model *gioEditor) undo() {
	if len(model.undoStack) == 0 {
		model.message = "没有可撤销的编辑"
		return
	}
	model.redoStack = append(model.redoStack, model.currentSnapshot())
	last := len(model.undoStack) - 1
	snapshot := model.undoStack[last]
	model.undoStack = model.undoStack[:last]
	model.restoreSnapshot(snapshot)
	model.message = "已撤销上一步编辑"
}

func (model *gioEditor) redo() {
	if len(model.redoStack) == 0 {
		model.message = "没有可重做的编辑"
		return
	}
	model.undoStack = append(model.undoStack, model.currentSnapshot())
	last := len(model.redoStack) - 1
	snapshot := model.redoStack[last]
	model.redoStack = model.redoStack[:last]
	model.restoreSnapshot(snapshot)
	model.message = "已重做上一步编辑"
}

func (model *gioEditor) saveLyrics() {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) || model.manifest.Layers[model.selected].Type != caseTheme.LayerLyrics {
		model.message = "请先选中歌词图层"
		return
	}
	if model.lyricsSaveRunning {
		model.message = "歌词设置正在保存，请稍候"
		return
	}
	model.pushUndo()
	model.updateSelectedLyricsOptions()
	provider := strings.TrimSpace(model.lyricsProvider.editor.Text())
	if provider == "" {
		provider = "auto"
	}
	lyricsPath := strings.TrimSpace(model.lyricsPath.editor.Text())
	target := model.target
	layerID := model.manifest.Layers[model.selected].ID
	model.lyricsSaveVersion++
	version := model.lyricsSaveVersion
	model.lyricsSaveRunning = true
	model.message = "正在保存歌词图层设置…"
	model.ensureUIUpdates()
	go func() {
		var saveErr error
		if model.api == nil {
			saveErr = errors.New("未连接 CaseScreen 服务")
		} else {
			cfg, err := model.api.Config()
			if err != nil {
				saveErr = err
			} else {
				cfg.LyricsProvider, cfg.LyricsPath = provider, lyricsPath
				_, saveErr = model.api.SaveConfig(cfg)
			}
		}
		model.uiUpdates <- func() {
			if version != model.lyricsSaveVersion {
				return
			}
			model.lyricsSaveRunning = false
			if model.target != target || model.selected < 0 || model.selected >= len(model.manifest.Layers) {
				return
			}
			selected := model.manifest.Layers[model.selected]
			if selected.ID != layerID || selected.Type != caseTheme.LayerLyrics {
				return
			}
			if saveErr != nil {
				model.message = saveErr.Error()
				return
			}
			model.lyricsDefaultProvider, model.lyricsDefaultPath = provider, lyricsPath
			model.message = "歌词图层设置已保存"
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) ensureLyricsFields(layer *caseTheme.Layer) {
	if model.lyricsLayerID == layer.ID {
		return
	}
	provider := layerOptionString(layer, "provider")
	if provider == "" {
		provider = model.lyricsDefaultProvider
	}
	if provider == "" {
		provider = "auto"
	}
	lyricsPath := layerOptionString(layer, "lrc_path")
	if lyricsPath == "" {
		lyricsPath = model.lyricsDefaultPath
	}
	model.lyricsProvider.set(provider)
	model.lyricsPath.set(lyricsPath)
	model.lyricsLayerID = layer.ID
}

func (model *gioEditor) browseLyricsForSelected() {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) || model.manifest.Layers[model.selected].Type != caseTheme.LayerLyrics {
		model.message = "请先选中歌词图层"
		return
	}
	path, err := chooseLyricsFile()
	if errors.Is(err, errFileDialogCancelled) {
		return
	}
	if err != nil {
		model.message = err.Error()
		return
	}
	model.pushUndo()
	model.lyricsPath.set(path)
	model.updateSelectedLyricsOptions()
	model.message = "已选择 LRC 文件：" + filepath.Base(path)
}

func (model *gioEditor) updateSelectedLyricsOptions() bool {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return false
	}
	layer := &model.manifest.Layers[model.selected]
	if layer.Type != caseTheme.LayerLyrics {
		return false
	}
	if layer.Options == nil {
		layer.Options = map[string]any{}
	}
	provider := strings.TrimSpace(model.lyricsProvider.editor.Text())
	if provider == "" {
		provider = "auto"
	}
	layer.Options["provider"] = provider
	layer.Options["lrc_path"] = strings.TrimSpace(model.lyricsPath.editor.Text())
	return true
}

func defaultOption(layer *caseTheme.Layer, key, fallback string) string {
	value := layerOptionString(layer, key)
	if value == "" {
		return fallback
	}
	return value
}

func colorOptionValue(layer *caseTheme.Layer, key, fallback string) string {
	value := strings.TrimPrefix(strings.ToUpper(layerOptionString(layer, key)), "#")
	if len(value) != 6 && len(value) != 8 {
		return fallback
	}
	return value
}

func (model *gioEditor) setSelectedOption(key string, value any) {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) {
		return
	}
	layer := &model.manifest.Layers[model.selected]
	if layer.Options == nil {
		layer.Options = make(map[string]any)
	}
	if text, ok := value.(string); ok && (key == "count" || key == "thickness") {
		if parsed, err := strconv.Atoi(text); err == nil {
			value = parsed
		}
	}
	layer.Options[key] = value
	if key == "color" && (layer.Type == caseTheme.LayerAudioBars || layer.Type == caseTheme.LayerAudioRing) {
		// 微雪频谱协议实际把 gradient_start 作为首色。同步主颜色与渐变起点，
		// 否则主题默认自带 gradient_start 时，“主颜色”按钮永远不会改变预览或设备。
		layer.Options["gradient_start"] = value
		delete(model.optionEditors, layer.ID+"\x00gradient_start")
	}
	model.requestPreviewRefresh()
}

func (model *gioEditor) toggleSelectedField(value string) {
	if model.selected < 0 || model.selected >= len(model.manifest.Layers) || model.manifest.Layers[model.selected].Type != caseTheme.LayerTelemetry {
		return
	}
	layer := &model.manifest.Layers[model.selected]
	fields := optionStrings(layer.Options, "fields")
	if len(fields) == 0 {
		fields = []string{"time", "Cpu_Freq", "Memory_Load"}
	}
	found := false
	filtered := fields[:0]
	for _, field := range fields {
		if field == value {
			found = true
			continue
		}
		filtered = append(filtered, field)
	}
	if !found {
		filtered = append(filtered, value)
	}
	if len(filtered) == 0 {
		filtered = []string{"time"}
	}
	model.setSelectedOption("fields", filtered)
}

func layerOptionString(layer *caseTheme.Layer, key string) string {
	if layer == nil || layer.Options == nil {
		return ""
	}
	value, _ := layer.Options[key].(string)
	return strings.TrimSpace(value)
}

func optionStrings(options map[string]any, key string) []string {
	switch values := options[key].(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func (field *textField) set(value string) { field.editor.SetText(value); field.last = value }

func formatResults(results map[string]string) string {
	if len(results) == 0 {
		return "服务未配置设备"
	}
	parts := make([]string, 0, len(results))
	for id, result := range results {
		parts = append(parts, id+":"+result)
	}
	sort.Strings(parts)
	return strings.Join(parts, "；")
}

func themeApplyMessage(result ThemeApplyResult) string {
	if !result.Queued {
		return "主题已应用并记住：" + formatResults(result.Results)
	}
	warning := strings.TrimSpace(result.Warning)
	if warning == "" {
		warning = "主题已保存；目标屏离线，将在重连后自动应用"
	}
	return warning + "；设备状态：" + formatResults(result.Results)
}

func defaultManifest() caseTheme.Manifest {
	return defaultManifestFor(1024, 600)
}

func defaultManifestFor(width, height int) caseTheme.Manifest {
	if width <= 0 {
		width = 1024
	}
	if height <= 0 {
		height = 600
	}
	return caseTheme.Manifest{ID: "casescreen-theme", Name: "机箱副屏主题", Width: width, Height: height, Layers: []caseTheme.Layer{
		{ID: "canvas", Type: caseTheme.LayerCanvas, Bounds: caseTheme.Rect{Width: 1, Height: 1}, Opacity: 1, ZIndex: -10000, Options: map[string]any{"color": "transparent"}},
	}}
}

func layerColor(layerType caseTheme.LayerType, selected bool) color.NRGBA {
	colors := map[caseTheme.LayerType]color.NRGBA{
		caseTheme.LayerCanvas: {R: 25, G: 50, B: 60, A: 255}, caseTheme.LayerImage: {R: 25, G: 50, B: 60, A: 255}, caseTheme.LayerBackgroundImage: {R: 25, G: 50, B: 60, A: 255}, caseTheme.LayerBackgroundVideo: {R: 45, G: 70, B: 80, A: 255},
		caseTheme.LayerTelemetry: {R: 32, G: 158, B: 188, A: 220}, caseTheme.LayerAudioBars: {R: 90, G: 174, B: 115, A: 220}, caseTheme.LayerAudioRing: {R: 180, G: 100, B: 210, A: 220}, caseTheme.LayerLyrics: {R: 210, G: 130, B: 70, A: 220},
		caseTheme.LayerText: {R: 73, G: 144, B: 226, A: 220}, caseTheme.LayerClock: {R: 72, G: 194, B: 172, A: 220}, caseTheme.LayerProgressBar: {R: 93, G: 186, B: 109, A: 220}, caseTheme.LayerGauge: {R: 225, G: 154, B: 69, A: 220}, caseTheme.LayerGradientGauge: {R: 212, G: 102, B: 166, A: 220}, caseTheme.LayerLineChart: {R: 120, G: 109, B: 222, A: 220}, caseTheme.LayerShape: {R: 98, G: 119, B: 132, A: 220},
	}
	value := colors[layerType]
	if selected {
		value.R = minByte(int(value.R)+35, 255)
		value.G = minByte(int(value.G)+35, 255)
	}
	return value
}

func layerLabel(layerType caseTheme.LayerType) string {
	return map[caseTheme.LayerType]string{
		caseTheme.LayerCanvas: "画布背景", caseTheme.LayerImage: "图片", caseTheme.LayerBackgroundImage: "旧版背景图片", caseTheme.LayerBackgroundVideo: "视频", caseTheme.LayerTelemetry: "遥测", caseTheme.LayerAudioBars: "条状频谱", caseTheme.LayerAudioRing: "环状频谱", caseTheme.LayerLyrics: "歌词",
		caseTheme.LayerText: "自定义文本", caseTheme.LayerClock: "时钟 / 日期", caseTheme.LayerProgressBar: "进度条", caseTheme.LayerGauge: "圆形仪表", caseTheme.LayerGradientGauge: "渐变仪表", caseTheme.LayerLineChart: "折线图", caseTheme.LayerShape: "形状 / 面板",
	}[layerType]
}
func minByte(value, limit int) uint8 {
	if value < 0 {
		value = 0
	}
	if value > limit {
		value = limit
	}
	return uint8(value)
}
func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = f32.Pt
var _ = io.EOF
