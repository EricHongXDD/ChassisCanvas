package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"casescreen/internal/gpupreference"
)

const (
	gpuPreferenceRestartMarker      = "CASESCREEN_GPU_PREFERENCE_RESTARTED"
	gpuPreferenceRestartMarkerValue = "1"
)

type startupGPUPreferenceManager interface {
	Apply(gpupreference.Preference) (gpupreference.ApplyResult, error)
	ApplyAdapter(string) (gpupreference.ApplyResult, error)
}

// applySavedGPUPreference 优先按已保存的物理适配器恢复当前映射。显卡暂时未被
// Windows 枚举时仍使用配置中已经验证过的调度类别，避免换目录升级后退回系统自动。
func applySavedGPUPreference(manager startupGPUPreferenceManager, configured, adapterID string) (gpupreference.ApplyResult, error) {
	if manager == nil {
		return gpupreference.ApplyResult{}, errors.New("GPU 首选项管理器未初始化")
	}
	preference, err := gpupreference.Parse(configured)
	if err != nil {
		return gpupreference.ApplyResult{}, err
	}
	adapterID = strings.TrimSpace(adapterID)
	if adapterID != "" {
		result, adapterErr := manager.ApplyAdapter(adapterID)
		if adapterErr == nil {
			return result, nil
		}
		if !errors.Is(adapterErr, gpupreference.ErrAdapterNotFound) {
			return gpupreference.ApplyResult{}, adapterErr
		}
	}
	return manager.Apply(preference)
}

func shouldRestartDesktopForGPUPreference(preferenceChanged, restartAttempted bool) bool {
	return preferenceChanged && !restartAttempted
}

// restartDesktopForGPUPreference 在旧进程创建 Gio/D3D 资源之前启动一个带一次性
// 标记的新进程。注册表写入发生在 CreateProcess 之前，新进程可从启动之初采用新首选项。
func restartDesktopForGPUPreference(executable string, arguments, environment []string) error {
	executable = filepath.Clean(strings.TrimSpace(executable))
	if executable == "" || executable == "." {
		return errors.New("桌面工作台路径为空")
	}
	command := exec.Command(executable, arguments...)
	command.Dir = filepath.Dir(executable)
	command.Env = environmentWithGPUPreferenceRestartMarker(environment)
	if err := command.Start(); err != nil {
		return err
	}
	// 当前进程会立即退出，不等待替代进程；无需保留子进程句柄。
	_ = command.Process.Release()
	return nil
}

func environmentWithGPUPreferenceRestartMarker(environment []string) []string {
	prefix := gpuPreferenceRestartMarker + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(strings.TrimSpace(key), gpuPreferenceRestartMarker) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, fmt.Sprintf("%s%s", prefix, gpuPreferenceRestartMarkerValue))
}
