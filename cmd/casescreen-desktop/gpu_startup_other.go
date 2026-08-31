//go:build !windows

package main

// 非 Windows 平台没有逐程序 GPU 首选项；保持桌面入口可交叉编译。
func synchronizeStartupGPUPreference(_ string) (bool, error) {
	return false, nil
}
