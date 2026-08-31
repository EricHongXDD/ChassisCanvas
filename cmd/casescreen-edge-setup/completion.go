package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const completionMarkerContent = "casescreen-edge-setup-complete-v1\n"

func finalizeCompletionMarker(ctx context.Context, path string, exitReason portalExitReason, runErr error) error {
	if runErr != nil || exitReason != portalExitCompleted || strings.TrimSpace(path) == "" {
		return runErr
	}
	select {
	case <-ctx.Done():
		return runErr
	default:
	}
	return writeCompletionMarker(path)
}

// writeCompletionMarker 只应由已认证的 portal.Done() 成功路径调用。
// 临时文件与目标位于同一目录，原子替换后不会暴露半写入的标记。
func writeCompletionMarker(rawPath string) error {
	path := filepath.Clean(strings.TrimSpace(rawPath))
	if path == "" || path == "." {
		return errors.New("完成标记路径无效")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("完成标记目标必须是普通文件")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("无法检查完成标记目标")
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("无法创建完成标记目录")
	}
	temporary, err := os.CreateTemp(directory, ".casescreen-complete-*.tmp")
	if err != nil {
		return errors.New("无法创建完成标记临时文件")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("无法限制完成标记临时文件权限")
	}
	if _, err := temporary.WriteString(completionMarkerContent); err != nil {
		temporary.Close()
		return errors.New("无法写入完成标记")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("无法同步完成标记")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("无法关闭完成标记")
	}
	if err := replaceCompletionMarker(temporaryPath, path); err != nil {
		return errors.New("无法原子替换完成标记")
	}
	return nil
}
