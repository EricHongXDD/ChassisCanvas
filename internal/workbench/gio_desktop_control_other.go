//go:build !windows

package workbench

func attachNativeWorkbenchWindow(uintptr, *desktopWindowControl) error {
	return nil
}

func showNativeWorkbenchWindow(uintptr) bool {
	return false
}

func requestNativeWorkbenchExit(uintptr, *desktopWindowControl) bool {
	return false
}
