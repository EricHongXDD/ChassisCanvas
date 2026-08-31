//go:build !windows

package workbench

import "errors"

var errFileDialogCancelled = errors.New("用户取消选择文件")

func chooseBackgroundFile() (string, error) {
	return "", errors.New("当前系统尚未提供背景文件选择器")
}

func chooseImageFile() (string, error) {
	return "", errors.New("当前系统尚未提供图片文件选择器")
}

func chooseLyricsFile() (string, error) {
	return "", errors.New("当前系统尚未提供 LRC 文件选择器")
}

func choosePlatformCookieFile() (string, error) {
	return "", errors.New("当前系统尚未提供平台登录态文件选择器")
}
