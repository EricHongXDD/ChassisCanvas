package workbench

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"casescreen/internal/service"
	caseTheme "casescreen/internal/theme"
	"gioui.org/layout"
	"gioui.org/op"
)

const targetStatusRefreshInterval = 5 * time.Second

type targetStatusRefreshState struct {
	running           bool
	lastRefresh       time.Time
	lastError         string
	requestGeneration uint64
}

// scheduleTargetStatusRefresh 只更新设备名称、分辨率和能力，不重新读取或改写当前主题。
func (model *gioEditor) scheduleTargetStatusRefresh(gtx layout.Context) {
	if model.api == nil || !model.periodicUIWorkEnabled() {
		return
	}
	now := time.Now()
	next := model.targetStatus.lastRefresh.Add(targetStatusRefreshInterval)
	if model.targetStatus.lastRefresh.IsZero() || !now.Before(next) {
		model.refreshTargetStatusAsync()
		next = now.Add(targetStatusRefreshInterval)
	}
	gtx.Execute(op.InvalidateCmd{At: next})
}

func (model *gioEditor) refreshTargetStatusAsync() {
	if model.api == nil || model.targetStatus.running || model.powerRunning {
		return
	}
	model.targetStatus.running = true
	generation := model.windowActivityGeneration()
	requestGeneration := model.targetStatus.requestGeneration
	model.ensureUIUpdates()
	go func() {
		hadToken := model.api.HasToken()
		status, err := model.api.Status()
		var connectErr error
		recovered := false
		if err == nil && !hadToken {
			connectErr = model.api.Connect()
			recovered = connectErr == nil
		}
		model.uiUpdates <- func() {
			model.targetStatus.running = false
			if generation != model.windowActivityGeneration() || requestGeneration != model.targetStatus.requestGeneration {
				model.targetStatus.lastRefresh = time.Time{}
				return
			}
			model.targetStatus.lastRefresh = time.Now()
			if err != nil {
				model.targetStatus.lastError = err.Error()
				return
			}
			if connectErr != nil {
				model.targetStatus.lastError = connectErr.Error()
				model.updateTargetOptions(status, true)
				return
			}
			model.targetStatus.lastError = ""
			model.updateTargetOptions(status, true)
			model.autoSelectConnectedTargetAfterStatus()
			if recovered {
				model.message = "CaseScreen 服务已恢复，请选择目标副屏"
			}
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) updateTargetOptions(status service.Status, notifyResolutionChange bool) {
	previous := make(map[string]targetOption, len(model.targetOptions))
	for _, option := range model.targetOptions {
		previous[option.id] = option
	}
	options := make([]targetOption, 0, len(status.Devices))
	for _, current := range status.Devices {
		id := current.Info.ID
		if id == "" {
			continue
		}
		label := current.Info.Name
		if label == "" {
			label = id
		}
		// 新版工作台不再提供利民设备入口。这里同时过滤旧后台返回的
		// 状态，避免升级过程中短暂重新出现已经移除的目标按钮。
		identity := strings.ToLower(strings.TrimSpace(id + " " + label))
		if strings.Contains(identity, "thermalright") || strings.Contains(identity, "利民") {
			continue
		}
		capabilities := current.Info.Capabilities
		coolerMaster := strings.Contains(identity, "coolermaster") || strings.Contains(identity, "酷冷") ||
			strings.Contains(strings.ToLower(strings.TrimSpace(current.Info.DisplayShape)), "hex")
		option := targetOption{
			id: id, label: label, resolution: current.Info.Resolution,
			waveshare:           capabilities.DeliveryMode == "stored_theme" && capabilities.SmallTelemetry,
			coolermaster:        coolerMaster,
			displayShape:        strings.TrimSpace(current.Info.DisplayShape),
			displayRotation:     current.Info.DisplayRotation,
			connected:           current.Connected,
			lastError:           strings.TrimSpace(current.LastError),
			brightnessSupported: current.BrightnessSupported,
			defaultBrightness:   current.DefaultBrightness,
			currentBrightness:   current.CurrentBrightness,
			poweredOff:          current.PoweredOff,
		}
		options = append(options, option)
		if !notifyResolutionChange || id != model.target {
			continue
		}
		old, existed := previous[id]
		if existed && old.resolution != option.resolution && option.resolution[0] > 0 && option.resolution[1] > 0 {
			model.message = fmt.Sprintf("已识别屏幕实际分辨率 %d × %d；当前编辑内容未被覆盖，点击当前目标可适配画布", option.resolution[0], option.resolution[1])
		}
	}
	sort.SliceStable(options, func(left, right int) bool {
		if options[left].connected != options[right].connected {
			return options[left].connected
		}
		leftRank := targetDisplayRank(options[left])
		rightRank := targetDisplayRank(options[right])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return strings.ToLower(options[left].label) < strings.ToLower(options[right].label)
	})
	model.targetOptions = options
}

// targetDisplayRank 把两块微雪屏固定在最前，且 7 英寸先于 8.8 英寸。
// 动态发现的串口编号并不稳定，因此同时使用分辨率和名称识别型号。
func targetDisplayRank(option targetOption) int {
	identity := strings.ToLower(strings.TrimSpace(option.id + " " + option.label))
	if option.waveshare {
		if option.resolution == [2]int{1024, 600} || strings.Contains(identity, "7 英寸") || strings.Contains(identity, "7inch") {
			return 0
		}
		if option.resolution == [2]int{480, 1920} || strings.Contains(identity, "8.8") || strings.Contains(identity, "8inch") {
			return 1
		}
		return 2
	}
	if option.coolermaster || strings.Contains(identity, "coolermaster") || strings.Contains(identity, "酷冷") {
		return 3
	}
	return 4
}

// syncTargetButtons 按设备 ID 复用 Clickable，即使设备数量不变或顺序变化，文字和动作也会立即更新。
func (model *gioEditor) syncTargetButtons() {
	if model.targetButtonByID == nil {
		model.targetButtonByID = make(map[string]*gioButton, len(model.targetOptions))
		for _, button := range model.targetButtons {
			if button != nil && button.id != "" {
				model.targetButtonByID[button.id] = button
			}
		}
	}
	buttons := make([]*gioButton, 0, len(model.targetOptions))
	present := make(map[string]bool, len(model.targetOptions))
	for _, option := range model.targetOptions {
		button := model.targetButtonByID[option.id]
		if button == nil {
			button = &gioButton{id: option.id}
			model.targetButtonByID[option.id] = button
		}
		targetID := option.id
		button.id = targetID
		button.text = option.label
		button.action = func() { model.selectTargetFromUI(targetID) }
		buttons = append(buttons, button)
		present[targetID] = true
	}
	for id := range model.targetButtonByID {
		if !present[id] {
			delete(model.targetButtonByID, id)
		}
	}
	model.targetButtons = buttons
}

// selectTargetFromUI 在用户明确选择时再适配画布；后台状态刷新不动当前 manifest。
func (model *gioEditor) selectTargetFromUI(target string) {
	model.targetExplicit = true
	model.selectTargetFromStatus(target)
}

// selectTargetFromStatus 与按钮选择共用异步载入逻辑，但不会把启动阶段的
// 自动选择标记为用户意图。
func (model *gioEditor) selectTargetFromStatus(target string) {
	option := model.targetOption(target)
	if option == nil {
		model.message = "当前目标已不在设备状态中"
		return
	}
	if target != model.target {
		model.leaveCoolerCalibrationForTarget(model.target)
		if model.targetLoading == target {
			model.message = "正在载入目标主题：" + option.label
			return
		}
		model.targetLoadVersion++
		version := model.targetLoadVersion
		model.targetLoading = target
		model.message = targetSelectionMessage(option, "正在载入目标主题："+option.label)
		model.ensureUIUpdates()
		go func() {
			var manifest caseTheme.Manifest
			var err error
			if model.api == nil {
				err = fmt.Errorf("未连接 CaseScreen 服务")
			} else {
				manifest, err = model.api.Theme(target)
			}
			model.uiUpdates <- func() {
				if version != model.targetLoadVersion || model.targetLoading != target {
					return
				}
				model.targetLoading = ""
				model.finishTargetSelection(target, manifest, err)
			}
			model.invalidateWindow()
		}()
		return
	}
	// 点击当前目标会取消尚未完成的其他目标载入，避免旧请求稍后覆盖画布。
	model.targetLoadVersion++
	model.targetLoading = ""
	changed := model.manifest.Width != option.resolution[0] || model.manifest.Height != option.resolution[1]
	if changed {
		model.pushUndo()
		model.adaptManifestToTarget()
		model.componentLayerID = ""
		model.message = targetSelectionMessage(option, fmt.Sprintf("画布已适配当前屏幕 %d × %d；现有图层和编辑内容已保留", option.resolution[0], option.resolution[1]))
		return
	}
	model.message = targetSelectionMessage(option, "目标状态已刷新；当前编辑内容保持不变")
}

// automaticConnectedTarget 返回启动阶段应自动切换到的在线目标。用户一旦
// 明确点过目标按钮，或已有在线目标/载入任务，就不再自动改动选择。
func (model *gioEditor) automaticConnectedTarget() string {
	if model.coolerCalibration.loadedTarget == model.target &&
		(model.coolerCalibration.testVisible || model.coolerCalibration.showing) {
		// 校准期间短暂掉线不能被“启动自动选中”解释为用户切换目标；
		// 否则会间接发送 DELETE，把仍需观察的测试图恢复掉。
		return ""
	}
	if model.targetExplicit || model.targetLoading != "" || model.targetConnected(model.target) {
		return ""
	}
	target := model.preferredTarget(model.target)
	if target == "" || target == model.target || !model.targetConnected(target) {
		return ""
	}
	return target
}

func (model *gioEditor) autoSelectConnectedTargetAfterStatus() {
	target := model.automaticConnectedTarget()
	if target == "" {
		return
	}
	model.selectTargetFromStatus(target)
}

func targetSelectionMessage(option *targetOption, onlineMessage string) string {
	if option == nil || option.connected {
		return onlineMessage
	}
	message := "已选择离线目标 " + option.label + "；主题可以保存，设备重连后自动应用"
	if option.lastError != "" {
		message += "（" + option.lastError + "）"
	}
	return message
}
