//go:build windows

package workbench

import (
	"os"
	"sync"
	"syscall"

	"gioui.org/app"
	"gioui.org/io/event"
	"golang.org/x/sys/windows"
)

var isIconicProc = windows.NewLazySystemDLL("user32.dll").NewProc("IsIconic")

// 函数变量让启动无 HWND、首次显示和隐藏恢复的边界可以脱离真实桌面测试。
var (
	findVisibleWorkbenchWindow = currentProcessVisibleWindow
	validWorkbenchWindow       = func(handle uintptr) bool { return handle != 0 && windows.IsWindow(windows.HWND(handle)) }
	pausedWorkbenchWindow      = win32WindowPaused
)

func nativeWindowActivity(current uintptr, eventValue event.Event) (uintptr, bool, bool) {
	switch currentEvent := eventValue.(type) {
	case app.Win32ViewEvent:
		current = currentEvent.HWND
		if current == 0 {
			return 0, true, true
		}
		paused := pausedWorkbenchWindow(current)
		return current, paused, true
	case app.ConfigEvent:
		return resolveWin32WindowActivity(current, currentEvent.Config.Mode == app.Minimized)
	case app.FrameEvent:
		return resolveWin32WindowActivity(current, false)
	}
	return current, false, false
}

func nativeWindowDestroyed(eventValue event.Event, nativeViewAttached bool) bool {
	viewEvent, ok := eventValue.(app.Win32ViewEvent)
	return nativeViewAttached && ok && viewEvent.HWND == 0
}

func resolveWin32WindowActivity(current uintptr, reportedMinimized bool) (uintptr, bool, bool) {
	if current != 0 && !validWorkbenchWindow(current) {
		current = 0
	}
	if current == 0 {
		current = findVisibleWorkbenchWindow()
	}
	if current == 0 {
		// Config.Mode 在 Windows 用户最小化时仍可能是 Windowed。
		// 无 HWND 时绝不把 FrameEvent/Windowed 当成可见证据。
		return 0, true, true
	}
	paused := reportedMinimized || pausedWorkbenchWindow(current)
	return current, paused, true
}

func win32WindowPaused(handle uintptr) bool {
	if handle == 0 || !windows.IsWindow(windows.HWND(handle)) || !windows.IsWindowVisible(windows.HWND(handle)) {
		return true
	}
	minimized, _, _ := isIconicProc.Call(handle)
	return minimized != 0
}

var visibleProcessWindowSearch = struct {
	sync.Mutex
	processID uint32
	window    uintptr
}{}

var visibleProcessWindowCallback = syscall.NewCallback(func(window uintptr, _ uintptr) uintptr {
	var ownerPID uint32
	_, _ = windows.GetWindowThreadProcessId(windows.HWND(window), &ownerPID)
	if visibleProcessWindowSearch.window == 0 && ownerPID == visibleProcessWindowSearch.processID && windows.IsWindowVisible(windows.HWND(window)) {
		visibleProcessWindowSearch.window = window
	}
	// 始终枚举完毕，避免 EnumWindows 把主动停止包装成系统错误。
	return 1
})

func currentProcessVisibleWindow() uintptr {
	visibleProcessWindowSearch.Lock()
	defer visibleProcessWindowSearch.Unlock()
	visibleProcessWindowSearch.processID = uint32(os.Getpid())
	visibleProcessWindowSearch.window = 0
	_ = windows.EnumWindows(visibleProcessWindowCallback, nil)
	return visibleProcessWindowSearch.window
}
