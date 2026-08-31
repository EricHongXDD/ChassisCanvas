//go:build windows

package main

import (
	"fmt"

	"casescreen/internal/config"
	"casescreen/internal/gpupreference"
)

// synchronizeStartupGPUPreference 在启动服务和创建 Gio 窗口之前，把配置中保存的
// 选择重放到当前发行目录。Windows 的逐程序首选项以 EXE 绝对路径为键，换目录或
// 升级版本后必须重新登记，不能只依赖仍指向旧发行目录的注册表值。
func synchronizeStartupGPUPreference(executable string) (bool, error) {
	store, err := config.Open("")
	if err != nil {
		return false, fmt.Errorf("读取 CaseScreen 配置: %w", err)
	}
	cfg := store.Get()
	result, err := applySavedGPUPreference(
		gpupreference.NewManagerForExecutable(executable),
		cfg.GPUPreference,
		cfg.GPUAdapterID,
	)
	if err != nil {
		return false, err
	}
	return result.Changed, nil
}
