package workbench

import (
	"image/color"
	"strings"

	caseTheme "casescreen/internal/theme"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const automaticTelemetryGPUOption = "auto"

type telemetryGPUAdapterOption struct {
	id    string
	label string
}

// telemetryGPUSettingsState 只管理 GPU 遥测来源。这个状态不得与
// gpuSettingsState（Windows 逐程序 GPU 调度）复用，避免两种选择相互覆盖。
type telemetryGPUSettingsState struct {
	dropdown       widget.Clickable
	adapterChoices []widget.Clickable
	refresh        widget.Clickable
	dropdownOpen   bool
	loaded         bool
	loading        bool
	saving         bool
	version        uint64
	draft          string
	status         TelemetryGPUStatus
	lastError      string
}

func (state *telemetryGPUSettingsState) ensureAdapterChoiceCount(count int) {
	if count <= len(state.adapterChoices) {
		return
	}
	state.adapterChoices = append(state.adapterChoices, make([]widget.Clickable, count-len(state.adapterChoices))...)
}

func (state *telemetryGPUSettingsState) editable() bool {
	// 枚举失败后只允许重新读取，不能继续用可能过期的显卡列表保存。
	return state.loaded && !state.loading && !state.saving && state.lastError == ""
}

func telemetryGPUAdapterOptions(status TelemetryGPUStatus) []telemetryGPUAdapterOption {
	options := []telemetryGPUAdapterOption{{id: automaticTelemetryGPUOption, label: "自动选择独立显卡（推荐）"}}
	for _, adapter := range status.Adapters {
		id, name := strings.TrimSpace(adapter.ID), strings.TrimSpace(adapter.Name)
		if id == "" || name == "" {
			continue
		}
		options = append(options, telemetryGPUAdapterOption{id: id, label: name})
	}
	return options
}

func configuredTelemetryGPUSelection(status TelemetryGPUStatus) string {
	id := strings.TrimSpace(status.ConfiguredAdapterID)
	if status.Automatic || id == "" || strings.EqualFold(id, automaticTelemetryGPUOption) {
		return automaticTelemetryGPUOption
	}
	return id
}

func selectedTelemetryGPUAdapterLabel(status TelemetryGPUStatus, selectedID string) string {
	selectedID = strings.TrimSpace(selectedID)
	for _, option := range telemetryGPUAdapterOptions(status) {
		if strings.EqualFold(option.id, selectedID) {
			return option.label
		}
	}
	if selectedID != "" && strings.EqualFold(selectedID, strings.TrimSpace(status.ConfiguredAdapterID)) {
		if name := strings.TrimSpace(status.ConfiguredAdapterName); name != "" {
			return name + "（当前未检测到）"
		}
	}
	return "请选择监控显卡"
}

func validTelemetryGPUSelection(status TelemetryGPUStatus, selectedID string) bool {
	selectedID = strings.TrimSpace(selectedID)
	if strings.EqualFold(selectedID, automaticTelemetryGPUOption) {
		return true
	}
	for _, adapter := range status.Adapters {
		if strings.EqualFold(strings.TrimSpace(adapter.ID), selectedID) {
			return true
		}
	}
	return false
}

func telemetryGPUEffectiveText(state *telemetryGPUSettingsState) string {
	if state == nil {
		return ""
	}
	switch {
	case state.loading && !state.loaded:
		return "正在读取 GPU 遥测来源…"
	case state.saving:
		return "正在切换全局 GPU 遥测来源…"
	case state.lastError != "":
		return "操作失败：" + state.lastError
	case !state.loaded:
		return ""
	}
	name := strings.TrimSpace(state.status.EffectiveAdapterName)
	if name == "" {
		name = selectedTelemetryGPUAdapterLabel(state.status, configuredTelemetryGPUSelection(state.status))
	}
	return "当前生效：" + name
}

// layoutTelemetryGPUSelector 显示在组件属性中，但修改的是全局采集来源。
// 所有 GPU 使用率、型号与温度指标必须始终来自同一张卡。
func (model *gioEditor) layoutTelemetryGPUSelector(gtx layout.Context) layout.Dimensions {
	state := &model.telemetryGPUSettings
	model.ensureTelemetryGPULoaded()
	options := telemetryGPUAdapterOptions(state.status)
	state.ensureAdapterChoiceCount(len(options))

	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "监控显卡（全局）")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Caption(model.theme, "切换后，所有 GPU 组件的负载、型号和温度会同步切换；不影响“程序”页的运算显卡。")
				label.Color = color.NRGBA{R: 164, G: 181, B: 190, A: 255}
				return label.Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := selectedTelemetryGPUAdapterLabel(state.status, state.draft) + "  ▾"
			return model.buttonState(gtx, &state.dropdown, label, func() {
				if state.editable() {
					state.dropdownOpen = !state.dropdownOpen
				}
			}, state.dropdownOpen || state.saving)
		}),
	}
	if state.dropdownOpen && state.editable() {
		for index, option := range options {
			index, option := index, option
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(4), Left: unit.Dp(8), Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return model.buttonState(gtx, &state.adapterChoices[index], option.label, func() {
						model.saveTelemetryGPUSelection(option.id)
					}, strings.EqualFold(state.draft, option.id))
				})
			}))
		}
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Caption(model.theme, telemetryGPUEffectiveText(state))
				if state.lastError != "" {
					label.Color = color.NRGBA{R: 239, G: 118, B: 118, A: 255}
				} else {
					label.Color = color.NRGBA{R: 112, G: 166, B: 176, A: 255}
				}
				return label.Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(5), Bottom: unit.Dp(9)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonState(gtx, &state.refresh, "重新读取监控显卡", func() {
					model.loadTelemetryGPU(true)
				}, state.loading)
			})
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) ensureTelemetryGPULoaded() {
	state := &model.telemetryGPUSettings
	// 初次失败后等待用户点击“重新读取”，避免属性面板
	// 每一帧都立即重试并持续占用本机 HTTP 连接。
	if state.loaded || state.loading || state.lastError != "" || model.api == nil {
		return
	}
	model.loadTelemetryGPU(false)
}

func (model *gioEditor) loadTelemetryGPU(report bool) {
	state := &model.telemetryGPUSettings
	if state.loading || state.saving || model.api == nil {
		return
	}
	state.loading = true
	state.lastError = ""
	state.version++
	version := state.version
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.TelemetryGPU()
		model.uiUpdates <- func() {
			if state.version != version {
				return
			}
			state.loading = false
			if err != nil {
				state.lastError = err.Error()
				if report {
					model.message = "读取监控显卡失败：" + err.Error()
				}
				return
			}
			state.status = status
			state.draft = configuredTelemetryGPUSelection(status)
			state.dropdownOpen = false
			state.loaded = true
			state.lastError = ""
			if report {
				model.message = telemetryGPUEffectiveText(state)
			}
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) saveTelemetryGPUSelection(adapterID string) {
	state := &model.telemetryGPUSettings
	adapterID = strings.TrimSpace(adapterID)
	if model.api == nil || !state.editable() {
		return
	}
	if !validTelemetryGPUSelection(state.status, adapterID) {
		model.message = "请选择自动或当前检测到的显卡"
		return
	}
	state.draft = adapterID
	state.dropdownOpen = false
	state.saving = true
	state.lastError = ""
	state.version++
	version := state.version
	model.message = "正在切换全局 GPU 遥测来源…"
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.SaveTelemetryGPUAdapter(adapterID)
		model.uiUpdates <- func() {
			if state.version != version {
				return
			}
			state.saving = false
			if err != nil {
				state.lastError = err.Error()
				state.draft = configuredTelemetryGPUSelection(state.status)
				model.message = "切换监控显卡失败：" + err.Error()
				return
			}
			state.status = status
			state.draft = configuredTelemetryGPUSelection(status)
			state.loaded = true
			state.lastError = ""
			model.message = telemetryGPUEffectiveText(state) + "；所有 GPU 指标已同步切换"
		}
		model.invalidateWindow()
	}()
}

func isGPUTelemetryMetric(metric string) bool {
	switch strings.ToLower(strings.TrimSpace(metric)) {
	case "gpu_load", "gpu_temp", "gpu_hotspot_temp", "gpu_memory_temp", "gpu_model":
		return true
	default:
		return false
	}
}

// layerSupportsGPUTelemetrySelection 集中声明哪些组件能从属性面板绑定 GPU 指标。
// 选择器由统一属性入口渲染，避免每种组件各自维护时遗漏。
func layerSupportsGPUTelemetrySelection(layerType caseTheme.LayerType) bool {
	switch layerType {
	case caseTheme.LayerTelemetry, caseTheme.LayerText, caseTheme.LayerProgressBar,
		caseTheme.LayerGauge, caseTheme.LayerGradientGauge, caseTheme.LayerLineChart:
		return true
	default:
		return false
	}
}

func layerUsesSingleGPUTelemetryMetric(layer *caseTheme.Layer, fallback string) bool {
	if layer == nil {
		return false
	}
	return isGPUTelemetryMetric(defaultOption(layer, "data_key", fallback))
}
