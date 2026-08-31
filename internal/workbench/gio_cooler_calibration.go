package workbench

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// coolerCalibrationState 保存酷冷方向页的草稿和异步请求状态。
// 安装方向属于设备而不是主题，因此这里不会写入编辑器撤销栈。
type coolerCalibrationState struct {
	list         widget.List
	markers      [12]widget.Clickable
	show         widget.Clickable
	confirm      widget.Clickable
	cancel       widget.Clickable
	refresh      widget.Clickable
	loadedTarget string
	loaded       bool
	loading      bool
	showing      bool
	saving       bool
	cancelling   bool
	testVisible  bool
	selectedMark string
	version      uint64
	status       CoolerCalibrationStatus
	lastError    string
	showSequence uint64
	pendingShows map[uint64]coolerPendingShow
}

// coolerPendingShow 保留尚未返回的测试图请求。用户可能在 POST
// 完成前已切换目标，此时请求若后来成功，必须再主动 DELETE 恢复旧屏。
type coolerPendingShow struct {
	target  string
	version uint64
	stale   bool
}

func (state *coolerCalibrationState) init() {
	state.list.Axis = layout.Vertical
	if state.pendingShows == nil {
		state.pendingShows = make(map[uint64]coolerPendingShow)
	}
}

func (state *coolerCalibrationState) busy() bool {
	return state.loading || state.showing || state.saving || state.cancelling
}

func (state *coolerCalibrationState) reset(target string) {
	state.version++
	state.loadedTarget = target
	state.loaded = false
	state.loading = false
	state.showing = false
	state.saving = false
	state.cancelling = false
	state.testVisible = false
	state.selectedMark = ""
	state.status = CoolerCalibrationStatus{Target: target}
	state.lastError = ""
}

func (model *gioEditor) isCoolerMasterTarget() bool {
	if option := model.targetOption(model.target); option != nil {
		if option.coolermaster {
			return true
		}
		identity := strings.ToLower(strings.TrimSpace(option.id + " " + option.label + " " + option.displayShape))
		return strings.Contains(identity, "coolermaster") || strings.Contains(identity, "酷冷") || strings.Contains(identity, "rounded_hex")
	}
	identity := strings.ToLower(strings.TrimSpace(model.target))
	return strings.Contains(identity, "coolermaster") || strings.Contains(identity, "酷冷")
}

func (model *gioEditor) layoutCoolerCalibration(gtx layout.Context) layout.Dimensions {
	state := &model.coolerCalibration
	state.init()
	if state.loadedTarget != model.target {
		state.reset(model.target)
	}
	model.ensureCoolerCalibrationLoaded()
	return state.list.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutWavesharePageIntro(gtx, "冷头安装方向", "只需在安装或重新磁吸冷头后设置一次。测试图会持续显示，直到确认保存、取消恢复、切换目标或退出软件。")
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, model.layoutCoolerCalibrationStatus)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, model.layoutCoolerCalibrationMarkers)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(14), Bottom: unit.Dp(12)}.Layout(gtx, model.layoutCoolerCalibrationActions)
			}),
		)
	})
}

func (model *gioEditor) layoutCoolerCalibrationStatus(gtx layout.Context) layout.Dimensions {
	state := &model.coolerCalibration
	option := model.targetOption(model.target)
	status := coolerCalibrationStatusText(state)
	statusColor := color.NRGBA{R: 151, G: 178, B: 188, A: 255}
	if state.lastError != "" {
		statusColor = color.NRGBA{R: 239, G: 143, B: 116, A: 255}
	} else if state.testVisible {
		statusColor = color.NRGBA{R: 255, G: 205, B: 90, A: 255}
	} else if state.loaded && state.status.Calibrated {
		statusColor = color.NRGBA{R: 124, G: 242, B: 154, A: 255}
	}
	connection := "设备状态未知"
	if option != nil {
		switch {
		case option.poweredOff:
			connection = "当前副屏已关屏；恢复后才能显示测试图"
		case option.connected:
			connection = "当前副屏在线"
		default:
			connection = "当前副屏离线；连接后才能显示测试图"
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "方向状态")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, status)
				label.Color = statusColor
				return label.Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Caption(model.theme, connection)
				label.Color = color.NRGBA{R: 112, G: 137, B: 147, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
}

func coolerCalibrationStatusText(state *coolerCalibrationState) string {
	if state == nil {
		return "尚未读取方向设置"
	}
	switch {
	case state.loading && !state.loaded:
		return "正在读取已保存的安装方向…"
	case state.showing:
		return "正在把 1..12 方向测试图发送到副屏…"
	case state.saving:
		return "正在保存方向并恢复当前主题…"
	case state.cancelling:
		return "正在取消测试并恢复当前主题…"
	case state.lastError != "" && state.testVisible:
		return "操作失败：" + state.lastError + "；测试图仍在显示，可重试确认或取消。"
	case state.lastError != "" && state.loaded:
		return "操作失败：" + state.lastError + "；页面保留上次读取的方向。"
	case state.lastError != "":
		return "读取失败：" + state.lastError
	case state.testVisible && coolerCalibrationUncertain(state.status):
		notice := strings.TrimSpace(state.status.Warning)
		if notice == "" {
			notice = "设备尚未确认执行结果"
		}
		return notice + "；测试图仍可能显示，可重试确认或取消。"
	case state.testVisible && state.selectedMark != "":
		return "已选择数字 " + state.selectedMark + "；确认它位于外壳向下尖角处后再保存。"
	case state.testVisible:
		return "测试图已显示：请选择位于冷头外壳向下尖角处的数字。"
	case state.loaded && state.status.Calibrated:
		marker := coolerMarkerForRotation(state.status.Rotation)
		return fmt.Sprintf("正向已保存：下方尖角对应数字 %s（补偿 %d°）。冷头未重新安装时无需再设置。", marker, normalizeCoolerRotation(state.status.Rotation))
	case state.loaded:
		return "尚未校准；主题仍可编辑，但实际屏幕方向可能与画布不同。"
	default:
		return "尚未读取方向设置"
	}
}

func (model *gioEditor) layoutCoolerCalibrationMarkers(gtx layout.Context) layout.Dimensions {
	state := &model.coolerCalibration
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "选择测试图数字")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(5), Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Caption(model.theme, "不要默认选择 6；请以外壳物理向下尖角为准，选择与尖角相邻的数字；若正好是 6 就选择 6。")
				label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
				return label.Layout(gtx)
			})
		}),
	}
	for row := 0; row < 3; row++ {
		row := row
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			columns := make([]layout.FlexChild, 0, 4)
			for column := 0; column < 4; column++ {
				index := row*4 + column
				marker := strconv.Itoa(index + 1)
				columns = append(columns, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						enabled := state.testVisible && !state.busy()
						if !enabled {
							gtx = gtx.Disabled()
						}
						action := func() {
							state.selectedMark = marker
							model.message = "已选择方向数字 " + marker + "；点击确认后才会保存"
						}
						if !enabled {
							action = nil
						}
						return model.buttonState(gtx, &state.markers[index], marker, action, state.selectedMark == marker)
					})
				}))
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, columns...)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layoutCoolerCalibrationActions(gtx layout.Context) layout.Dimensions {
	state := &model.coolerCalibration
	option := model.targetOption(model.target)
	canShow := model.api != nil && option != nil && option.connected && !option.poweredOff && !state.testVisible && !state.busy()
	canConfirm := state.testVisible && state.selectedMark != "" && !state.busy()
	canCancel := state.testVisible && !state.busy()
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "显示 1..12 方向测试图"
			if state.showing {
				label = "正在显示测试图…"
			}
			return model.buttonSizedEnabledState(gtx, &state.show, label, model.showCoolerCalibration, 300, state.testVisible, canShow)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := "确认并保存方向"
				if state.saving {
					label = "正在保存并恢复主题…"
				}
				return model.buttonSizedEnabledState(gtx, &state.confirm, label, model.saveCoolerCalibration, 300, state.saving, canConfirm)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := "取消测试并恢复原画面"
				if state.cancelling {
					label = "正在恢复原画面…"
				}
				return model.buttonSizedEnabledState(gtx, &state.cancel, label, model.cancelCoolerCalibration, 300, state.cancelling, canCancel)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.buttonSizedEnabledState(gtx, &state.refresh, "重新读取已保存方向", func() { model.loadCoolerCalibration(true) }, 300, state.loading, !state.busy() && model.api != nil)
			})
		}),
	)
}

func (model *gioEditor) ensureCoolerCalibrationLoaded() {
	state := &model.coolerCalibration
	if !model.isCoolerMasterTarget() || state.loaded || state.loading || model.api == nil {
		return
	}
	model.loadCoolerCalibration(false)
}

func (model *gioEditor) loadCoolerCalibration(report bool) {
	state := &model.coolerCalibration
	target := model.target
	if model.api == nil || !model.isCoolerMasterTarget() || state.busy() {
		return
	}
	if state.loadedTarget != target {
		state.reset(target)
	}
	state.loading = true
	state.lastError = ""
	state.version++
	version := state.version
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.CoolerCalibration(target)
		model.uiUpdates <- func() {
			if state.version != version || model.target != target || state.loadedTarget != target {
				return
			}
			state.loading = false
			if err != nil {
				state.lastError = err.Error()
				if report {
					model.message = "读取酷冷方向失败：" + err.Error()
				}
				return
			}
			state.status = status
			state.loaded = true
			state.testVisible = status.Active
			if !status.Active {
				state.selectedMark = ""
			}
			state.lastError = ""
			if option := model.targetOption(target); option != nil {
				option.displayRotation = status.Rotation
			}
			if report {
				model.message = "已重新读取酷冷冷头安装方向"
			}
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) showCoolerCalibration() {
	state := &model.coolerCalibration
	option := model.targetOption(model.target)
	if model.api == nil || !model.isCoolerMasterTarget() || option == nil || !option.connected || option.poweredOff || state.busy() {
		return
	}
	target := model.target
	state.showing = true
	state.lastError = ""
	state.version++
	version := state.version
	state.showSequence++
	requestID := state.showSequence
	state.init()
	state.pendingShows[requestID] = coolerPendingShow{target: target, version: version}
	model.message = "正在显示酷冷方向测试图…"
	model.ensureUIUpdates()
	go func() {
		err := model.api.ShowCoolerCalibration(target)
		model.uiUpdates <- func() {
			pending, tracked := state.pendingShows[requestID]
			delete(state.pendingShows, requestID)
			stale := !tracked || pending.stale || pending.target != target || pending.version != version ||
				state.version != version || model.target != target || state.loadedTarget != target
			if stale {
				// 不能在切换当下直接 DELETE：POST 可能尚在设备上执行，
				// 先 DELETE 反而会被后到的 POST 覆盖。只在过期 POST 成功后补发恢复。
				if err == nil {
					model.cancelCoolerCalibrationBestEffort(target)
				}
				return
			}
			state.showing = false
			if err != nil {
				state.lastError = err.Error()
				model.message = "显示方向测试图失败：" + err.Error()
				return
			}
			state.testVisible = true
			state.selectedMark = ""
			state.lastError = ""
			model.message = "测试图已显示；请选择位于冷头外壳向下尖角处的数字"
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) saveCoolerCalibration() {
	state := &model.coolerCalibration
	marker := strings.TrimSpace(state.selectedMark)
	if model.api == nil || !state.testVisible || marker == "" || state.busy() {
		return
	}
	target := model.target
	state.saving = true
	state.lastError = ""
	state.version++
	version := state.version
	model.message = "正在保存酷冷安装方向并恢复主题…"
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.SaveCoolerCalibration(target, marker)
		model.uiUpdates <- func() {
			if state.version != version || model.target != target || state.loadedTarget != target {
				return
			}
			state.saving = false
			if err != nil {
				state.lastError = err.Error()
				model.message = "保存酷冷方向失败：" + err.Error()
				return
			}
			state.status = status
			state.loaded = true
			state.lastError = ""
			if option := model.targetOption(target); option != nil {
				option.displayRotation = status.Rotation
			}
			if coolerCalibrationUncertain(status) {
				state.testVisible = true
				state.selectedMark = marker
				notice := strings.TrimSpace(status.Warning)
				if notice == "" {
					notice = "方向已保存，但设备尚未确认执行"
				}
				model.message = notice + "；测试图仍可能显示，可重试确认或点击取消恢复"
			} else {
				state.testVisible = false
				state.selectedMark = ""
				model.message = "方向已保存并恢复当前主题；以后画布均以尖角朝下为正向"
			}
			model.requestPreviewRefresh()
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) cancelCoolerCalibration() {
	state := &model.coolerCalibration
	if model.api == nil || !state.testVisible || state.busy() {
		return
	}
	target := model.target
	state.cancelling = true
	state.lastError = ""
	state.version++
	version := state.version
	model.message = "正在取消方向测试并恢复当前主题…"
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.CancelCoolerCalibration(target)
		model.uiUpdates <- func() {
			if state.version != version || model.target != target || state.loadedTarget != target {
				return
			}
			state.cancelling = false
			if err != nil {
				state.lastError = err.Error()
				model.message = "恢复原画面失败：" + err.Error()
				return
			}
			state.status = status
			state.loaded = true
			state.lastError = ""
			if coolerCalibrationUncertain(status) {
				state.testVisible = true
				notice := strings.TrimSpace(status.Warning)
				if notice == "" {
					notice = "设备尚未确认恢复结果"
				}
				model.message = notice + "；测试图仍可能显示，可再次取消或重试确认"
			} else {
				state.testVisible = false
				state.selectedMark = ""
				model.message = "方向测试已取消，原主题已恢复"
			}
		}
		model.invalidateWindow()
	}()
}

// leaveCoolerCalibrationForTarget 在切换到其他副屏前尽力恢复旧酷冷画面。
// 请求在后台进行，不阻塞新目标主题载入；旧请求的结果也不会覆盖新目标 UI。
func (model *gioEditor) leaveCoolerCalibrationForTarget(target string) {
	state := &model.coolerCalibration
	if model.api == nil || target == "" || state.loadedTarget != target {
		return
	}
	state.init()
	for requestID, pending := range state.pendingShows {
		if pending.target != target {
			continue
		}
		pending.stale = true
		state.pendingShows[requestID] = pending
	}
	state.version++
	state.showing = false
	wasVisible := state.testVisible
	state.testVisible = false
	state.selectedMark = ""
	if wasVisible {
		model.cancelCoolerCalibrationBestEffort(target)
	}
}

func (model *gioEditor) cancelCoolerCalibrationBestEffort(target string) {
	if model.api == nil || strings.TrimSpace(target) == "" {
		return
	}
	api := model.api
	go func() {
		_, _ = api.CancelCoolerCalibration(target)
	}()
}

func coolerCalibrationUncertain(status CoolerCalibrationStatus) bool {
	return status.Queued || strings.TrimSpace(status.Warning) != ""
}

func coolerMarkerForRotation(rotation int) string {
	rotation = normalizeCoolerRotation(rotation)
	marker := (rotation/30 + 6) % 12
	if marker == 0 {
		marker = 12
	}
	return strconv.Itoa(marker)
}

func normalizeCoolerRotation(rotation int) int {
	rotation %= 360
	if rotation < 0 {
		rotation += 360
	}
	return rotation
}
