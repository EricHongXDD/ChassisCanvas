package workbench

import (
	"image/color"
	"strings"

	localapi "casescreen/internal/api"
	"casescreen/internal/gpupreference"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const systemAutomaticGPUOption = "system"

type gpuAdapterOption struct {
	id    string
	label string
}

type gpuSettingsState struct {
	list           widget.List
	dropdown       widget.Clickable
	adapterChoices []widget.Clickable
	dropdownOpen   bool
	refresh        widget.Clickable
	save           widget.Clickable
	loaded         bool
	loading        bool
	saving         bool
	version        uint64
	draft          string
	status         localapi.GPUPreferenceStatus
	lastError      string
}

func (state *gpuSettingsState) init() {
	state.list.Axis = layout.Vertical
}

// layoutGPUSettings 展示 Windows 针对两个正式程序的 GPU 调度首选项。
// 该设置与当前选中的副屏无关，因此任何目标下都可打开。
func (model *gioEditor) layoutGPUSettings(gtx layout.Context) layout.Dimensions {
	state := &model.gpuSettings
	state.init()
	model.ensureGPUPreferenceLoaded()
	return state.list.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutWavesharePageIntro(gtx, "程序与显卡", "这里只设置后台服务和主题工作台参与运算的显卡；硬件监控显卡请在遥测组件属性中单独设置。")
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(18)}.Layout(gtx, model.layoutGPUAdapters)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(12)}.Layout(gtx, model.layoutGPUActions)
			}),
		)
	})
}

func (model *gioEditor) layoutGPUAdapters(gtx layout.Context) layout.Dimensions {
	state := &model.gpuSettings
	options := gpuAdapterOptions(state.status)
	state.ensureAdapterChoiceCount(len(options))
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "检测到的显卡")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := selectedGPUAdapterLabel(state.status, state.draft) + "  ▾"
				return model.buttonState(gtx, &state.dropdown, label, func() {
					if state.editable() {
						state.dropdownOpen = !state.dropdownOpen
					}
				}, state.dropdownOpen)
			})
		}),
	}
	if state.dropdownOpen && state.editable() {
		for index, option := range options {
			index, option := index, option
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(4), Left: unit.Dp(8), Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return model.buttonState(gtx, &state.adapterChoices[index], option.label, func() {
						state.draft = option.id
						state.dropdownOpen = false
					}, state.draft == option.id)
				})
			}))
		}
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				var text string
				switch {
				case state.loading && !state.loaded:
					text = "正在读取 Windows GPU 设置…"
				case state.lastError != "" && !state.loaded:
					text = "读取失败：" + state.lastError
				case state.loaded && !state.status.Supported:
					text = "当前系统不支持逐程序 GPU 首选项。"
				case state.loaded && len(state.status.Adapters) == 0:
					text = gpuAdapterStatusText(state) + "；Windows 未返回物理显示适配器"
				default:
					text = gpuAdapterStatusText(state)
				}
				label := material.Caption(model.theme, text)
				label.Color = color.NRGBA{R: 112, G: 137, B: 147, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (state *gpuSettingsState) ensureAdapterChoiceCount(count int) {
	if count <= len(state.adapterChoices) {
		return
	}
	state.adapterChoices = append(state.adapterChoices, make([]widget.Clickable, count-len(state.adapterChoices))...)
}

func gpuAdapterOptions(status localapi.GPUPreferenceStatus) []gpuAdapterOption {
	options := []gpuAdapterOption{{id: systemAutomaticGPUOption, label: "系统自动"}}
	for _, adapter := range status.Adapters {
		id, name := strings.TrimSpace(adapter.ID), strings.TrimSpace(adapter.Name)
		if id == "" || name == "" {
			continue
		}
		options = append(options, gpuAdapterOption{id: id, label: name})
	}
	return options
}

func selectedGPUAdapterLabel(status localapi.GPUPreferenceStatus, selectedID string) string {
	selectedID = strings.TrimSpace(selectedID)
	for _, option := range gpuAdapterOptions(status) {
		if strings.EqualFold(option.id, selectedID) {
			return option.label
		}
	}
	if selectedID != "" && strings.EqualFold(selectedID, strings.TrimSpace(status.ConfiguredAdapterID)) {
		if name := strings.TrimSpace(status.ConfiguredAdapterName); name != "" {
			return name + "（当前未检测到）"
		}
	}
	return "请选择检测到的显卡"
}

func gpuAdapterStatusText(state *gpuSettingsState) string {
	if state == nil || !state.loaded {
		return ""
	}
	prefix := "已保存："
	if state.lastError != "" {
		prefix = "上次成功读取："
	}
	selection := systemAutomaticGPUOption
	if strings.TrimSpace(state.status.ConfiguredAdapterID) != "" {
		selection = state.status.ConfiguredAdapterID
	}
	text := prefix + selectedGPUAdapterLabel(state.status, selection)
	if state.status.Consistent {
		text += "；两个程序的 Windows 设置已统一"
	} else {
		text += "；两个程序的 Windows 设置不一致，保存可统一"
	}
	return text
}

func (model *gioEditor) layoutGPUActions(gtx layout.Context) layout.Dimensions {
	state := &model.gpuSettings
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return model.buttonState(gtx, &state.refresh, "重新读取", model.refreshGPUPreference, state.loading)
					})
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return model.buttonState(gtx, &state.save, "保存 GPU 设置", model.saveGPUPreference, state.saving)
					})
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(9)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				text := strings.TrimSpace(state.status.Notice)
				if text == "" {
					text = "这是 Windows 调度首选项；保存后需重启 CaseScreen.exe 和 CaseScreenDesktop.exe 才能让新建 GPU 资源采用该选择。"
				}
				label := material.Body2(model.theme, text)
				label.Color = color.NRGBA{R: 225, G: 176, B: 90, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
}

func (model *gioEditor) ensureGPUPreferenceLoaded() {
	state := &model.gpuSettings
	if state.loaded || state.loading || model.api == nil {
		return
	}
	model.loadGPUPreference(false)
}

func (model *gioEditor) refreshGPUPreference() {
	model.loadGPUPreference(true)
}

func (model *gioEditor) loadGPUPreference(report bool) {
	state := &model.gpuSettings
	if state.loading || state.saving || model.api == nil {
		return
	}
	state.loading = true
	state.lastError = ""
	state.version++
	version := state.version
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.GPUPreference()
		model.uiUpdates <- func() {
			if state.version != version {
				return
			}
			state.loading = false
			if err != nil {
				state.lastError = err.Error()
				if report {
					model.message = "读取 GPU 设置失败：" + err.Error()
				}
				return
			}
			state.status = status
			state.draft = configuredGPUSelection(status)
			state.dropdownOpen = false
			state.loaded = true
			state.lastError = ""
			if report {
				model.message = "已重新读取 Windows GPU 设置"
			}
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) saveGPUPreference() {
	state := &model.gpuSettings
	if model.api == nil || !state.editable() {
		return
	}
	adapterID := strings.TrimSpace(state.draft)
	if !validGPUAdapterSelection(state.status, adapterID) {
		model.message = "请选择系统自动或当前检测到的显卡"
		return
	}
	state.saving = true
	state.lastError = ""
	state.version++
	version := state.version
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.SaveGPUAdapter(adapterID)
		model.uiUpdates <- func() {
			if state.version != version {
				return
			}
			state.saving = false
			if err != nil {
				state.lastError = err.Error()
				model.message = "保存 GPU 设置失败：" + err.Error()
				return
			}
			state.status = status
			state.draft = configuredGPUSelection(status)
			state.dropdownOpen = false
			state.loaded = true
			state.lastError = ""
			if status.RestartRequired {
				processes := strings.Join(status.RestartProcesses, "、")
				if processes == "" {
					processes = "CaseScreen.exe、CaseScreenDesktop.exe"
				}
				model.message = "GPU 设置已保存；退出并重新打开 " + processes + " 后生效"
			} else {
				model.message = "GPU 设置已保存；Windows 登记值未发生变化"
			}
		}
		model.invalidateWindow()
	}()
}

func validGPUAdapterSelection(status localapi.GPUPreferenceStatus, selectedID string) bool {
	selectedID = strings.TrimSpace(selectedID)
	if strings.EqualFold(selectedID, systemAutomaticGPUOption) {
		return true
	}
	_, ok := gpupreference.FindAdapter(status.Adapters, selectedID)
	return ok
}

func configuredGPUSelection(status localapi.GPUPreferenceStatus) string {
	if adapterID := strings.TrimSpace(status.ConfiguredAdapterID); adapterID != "" {
		return adapterID
	}
	if status.Configured == gpupreference.SystemDefault {
		return systemAutomaticGPUOption
	}
	for _, adapter := range status.Adapters {
		if adapter.Preference == status.Configured {
			return adapter.ID
		}
	}
	return ""
}

// editable 仅在状态来自最近一次成功读取时允许修改，避免刷新失败后覆盖未知的 Windows 状态。
func (state *gpuSettingsState) editable() bool {
	return state.loaded && state.status.Supported && !state.loading && !state.saving && state.lastError == ""
}
