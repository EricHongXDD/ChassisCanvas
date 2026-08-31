package workbench

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (model *gioEditor) toggleCurrentPower() {
	if model.powerRunning {
		return
	}
	option := model.targetOption(model.target)
	if option == nil {
		model.message = "请先选择一台目标副屏"
		return
	}
	if !option.brightnessSupported {
		model.message = option.label + " 不支持已确认的亮度关屏命令"
		return
	}
	model.setPoweredOff(option.id, !option.poweredOff)
}

func (model *gioEditor) toggleAllPower() {
	if model.powerRunning {
		return
	}
	allOff, supported := model.allPowerTargetsOff()
	if supported == 0 {
		model.message = "当前没有支持亮度关屏命令的副屏"
		return
	}
	model.setPoweredOff("all", !allOff)
}

func (model *gioEditor) setPoweredOff(target string, poweredOff bool) {
	if model.powerRunning {
		return
	}
	if model.api == nil {
		model.message = "未连接 CaseScreen 服务"
		return
	}
	target = strings.TrimSpace(target)
	if target == "" {
		model.message = "请先选择单台副屏或全部副屏"
		return
	}
	model.powerRunning = true
	model.powerTarget = target
	// 使关屏前已经在途的周期状态请求失效；旧快照即使更晚回调，
	// 也不能把刚完成的 powered_off 状态回滚五秒。
	model.targetStatus.requestGeneration++
	model.targetStatus.lastRefresh = time.Time{}
	action, scope := powerActionLabels(target, poweredOff, model.targetOptions)
	model.message = "正在" + action + scope + "…"
	model.ensureUIUpdates()
	go func() {
		result, err := model.api.SetPoweredOff(target, poweredOff)
		// HTTP 超时或响应丢失不代表服务端没有持久化/执行。无论命令
		// 响应如何都读取一次权威状态，同时保留原始错误供用户判断。
		status, statusErr := model.api.Status()
		model.uiUpdates <- func() {
			model.powerRunning = false
			model.powerTarget = ""
			if err != nil {
				if statusErr == nil {
					model.updateTargetOptions(status, false)
					model.targetStatus.lastRefresh = time.Now()
				}
				model.message = action + scope + "失败：" + err.Error()
				if statusErr != nil {
					model.message += "；状态对账失败：" + statusErr.Error()
				}
				return
			}
			model.applyPowerResultLocally(result)
			// 成功响应中的逐台结果先更新本地；状态查询成功时再以
			// 权威快照校准。查询失败也仍能立即反馈成功设备。
			if statusErr == nil {
				model.updateTargetOptions(status, false)
				model.targetStatus.lastRefresh = time.Now()
			}
			model.message = model.powerResultMessage(result)
			if statusErr != nil {
				model.message += "；状态刷新失败：" + statusErr.Error()
			}
		}
		model.invalidateWindow()
	}()
}

func powerActionLabels(target string, poweredOff bool, options []targetOption) (action, scope string) {
	action = "关闭"
	if !poweredOff {
		action = "恢复"
	}
	scope = "全部副屏"
	if target == "all" {
		return action, scope
	}
	for _, option := range options {
		if option.id == target {
			return action, "副屏「" + option.label + "」"
		}
	}
	return action, "副屏「" + target + "」"
}

func (model *gioEditor) applyPowerResultLocally(result PowerResult) {
	for index := range model.targetOptions {
		value, exists := result.Results[model.targetOptions[index].id]
		if !exists || value != "ok" {
			continue
		}
		model.targetOptions[index].poweredOff = result.PoweredOff
		if result.PoweredOff {
			model.targetOptions[index].currentBrightness = 0
		} else {
			model.targetOptions[index].currentBrightness = model.targetOptions[index].defaultBrightness
		}
	}
}

// powerResultMessage 按界面中的设备顺序展示每台结果，不把部分失败缩成一句模糊提示。
func (model *gioEditor) powerResultMessage(result PowerResult) string {
	action, scope := powerActionLabels(result.Target, result.PoweredOff, model.targetOptions)
	labels := make(map[string]string, len(model.targetOptions))
	order := make(map[string]int, len(model.targetOptions))
	for index, option := range model.targetOptions {
		labels[option.id] = option.label
		order[option.id] = index
	}
	ids := make([]string, 0, len(result.Results))
	for id := range result.Results {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(left, right int) bool {
		leftOrder, leftKnown := order[ids[left]]
		rightOrder, rightKnown := order[ids[right]]
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftKnown && leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return ids[left] < ids[right]
	})
	details := make([]string, 0, len(ids))
	for _, id := range ids {
		label := labels[id]
		if label == "" {
			label = id
		}
		value := result.Results[id]
		if value == "ok" {
			details = append(details, label+"：成功")
		} else {
			details = append(details, fmt.Sprintf("%s：失败（%s）", label, value))
		}
	}
	summary := fmt.Sprintf("%s%s完成：%d 成功，%d 失败", action, scope, result.Successes, result.Failures)
	if len(details) > 0 {
		summary += "；" + strings.Join(details, "；")
	}
	return summary
}
