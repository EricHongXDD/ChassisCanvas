package coolermaster

import "errors"

// ErrOEMControlConflict 表示酷冷原厂后台仍在控制同一块 LCD。
// 两套控制器即使都能打开 HID，也会互相覆盖离线槽位和显示模式。
var ErrOEMControlConflict = errors.New("酷冷原厂 MasterCTRL 正在控制副屏")

func oemControlConflict(serviceActive, agentActive bool) error {
	if !serviceActive && !agentActive {
		return nil
	}
	return ErrOEMControlConflict
}

// oemControlOwnershipConflict 还要覆盖 Windows 登录时的启动竞态：
// 原厂服务可能在 CaseScreen 已打开 HID 后才延迟启动，随后重新
// 切换屏幕模式并覆盖用户主题。只要原厂控制器正在运行，或者
// 已明确配置为会在本次登录自动启动，就必须先保持单控制器。
func oemControlOwnershipConflict(serviceActive, serviceMayAutoStart, agentActive bool) error {
	if serviceMayAutoStart {
		return ErrOEMControlConflict
	}
	return oemControlConflict(serviceActive, agentActive)
}

func oemServiceMayAutoStart(serviceAutomatic, serviceDisabled, trayAutoStart bool) bool {
	return serviceAutomatic || (!serviceDisabled && trayAutoStart)
}
