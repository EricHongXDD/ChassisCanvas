//go:build windows

package coolermaster

import "golang.org/x/sys/windows"

func replaceStateFile(source, target string) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	// REPLACE_EXISTING 保证不经过“先删后改名”的空档；
	// WRITE_THROUGH 让 Windows 在替换写入持久介质后才返回。
	return windows.MoveFileEx(
		sourcePath,
		targetPath,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func syncStateDirectory(string) error {
	// MoveFileExW(MOVEFILE_WRITE_THROUGH) 已覆盖 Windows 的替换持久化语义。
	return nil
}
