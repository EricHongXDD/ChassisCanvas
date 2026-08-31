//go:build !windows

package coolermaster

// CheckOEMControlConflict 只在 Windows 上需要检测原厂 MasterCTRL 后台。
func CheckOEMControlConflict() error { return nil }
