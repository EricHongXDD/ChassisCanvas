//go:build windows

package workbench

import (
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	workbenchWMClose     = 0x0010
	workbenchWMNCDestroy = 0x0082
	workbenchSWHide      = 0
	workbenchSWShow      = 5
	workbenchSWRestore   = 9
	workbenchGWLPWndProc = ^uintptr(3)
)

var (
	workbenchUser32                 = windows.NewLazySystemDLL("user32.dll")
	workbenchGetWindowLongPtrProc   = workbenchUser32.NewProc("GetWindowLongPtrW")
	workbenchSetWindowLongPtrProc   = workbenchUser32.NewProc("SetWindowLongPtrW")
	workbenchCallWindowProcProc     = workbenchUser32.NewProc("CallWindowProcW")
	workbenchDefWindowProcProc      = workbenchUser32.NewProc("DefWindowProcW")
	workbenchShowWindowProc         = workbenchUser32.NewProc("ShowWindow")
	workbenchShowWindowAsyncProc    = workbenchUser32.NewProc("ShowWindowAsync")
	workbenchSetForegroundProc      = workbenchUser32.NewProc("SetForegroundWindow")
	workbenchPostMessageProc        = workbenchUser32.NewProc("PostMessageW")
	workbenchWindowProcedurePointer = syscall.NewCallback(workbenchWindowProcedure)
	workbenchWindowHooks            sync.Map
)

type workbenchWindowHook struct {
	original atomic.Uintptr
	control  *desktopWindowControl
}

func attachNativeWorkbenchWindow(handle uintptr, control *desktopWindowControl) error {
	if handle == 0 || control == nil || !windows.IsWindow(windows.HWND(handle)) {
		return nil
	}
	if _, ok := workbenchWindowHooks.Load(handle); ok {
		return nil
	}
	original, _, originalErr := workbenchGetWindowLongPtrProc.Call(handle, workbenchGWLPWndProc)
	if original == 0 {
		return fmt.Errorf("读取 Gio 窗口过程失败: %v", originalErr)
	}
	hook := &workbenchWindowHook{control: control}
	hook.original.Store(original)
	workbenchWindowHooks.Store(handle, hook)
	previous, _, installErr := workbenchSetWindowLongPtrProc.Call(handle, workbenchGWLPWndProc, workbenchWindowProcedurePointer)
	if previous == 0 {
		workbenchWindowHooks.Delete(handle)
		return fmt.Errorf("安装关闭到托盘过程失败: %v", installErr)
	}
	// 如果其他组件在读取和安装之间已经子类化，以 SetWindowLongPtr
	// 返回的实际上一层过程为准，不截断窗口消息链。
	hook.original.Store(previous)
	return nil
}

func workbenchWindowProcedure(handle uintptr, message uint32, wParam, lParam uintptr) uintptr {
	value, ok := workbenchWindowHooks.Load(handle)
	if !ok {
		result, _, _ := workbenchDefWindowProcProc.Call(handle, uintptr(message), wParam, lParam)
		return result
	}
	hook := value.(*workbenchWindowHook)
	if message == workbenchWMClose && shouldHideWorkbenchClose(hook.control) {
		_, _, _ = workbenchShowWindowProc.Call(handle, workbenchSWHide)
		hook.control.windowHidden()
		return 0
	}
	result, _, _ := workbenchCallWindowProcProc.Call(hook.original.Load(), handle, uintptr(message), wParam, lParam)
	if message == workbenchWMNCDestroy {
		workbenchWindowHooks.Delete(handle)
	}
	return result
}

func showNativeWorkbenchWindow(handle uintptr) bool {
	if handle == 0 || !windows.IsWindow(windows.HWND(handle)) {
		return false
	}
	showMode := uintptr(workbenchSWShow)
	if minimized, _, _ := isIconicProc.Call(handle); minimized != 0 {
		showMode = workbenchSWRestore
	}
	// ShowWindowAsync 与 ShowWindow 的返回语义不同：非零表示
	// 异步显示操作已成功发起，不是“窗口之前可见”。
	shown, _, _ := workbenchShowWindowAsyncProc.Call(handle, showMode)
	if shown == 0 {
		return false
	}
	_, _, _ = workbenchSetForegroundProc.Call(handle)
	return true
}

func requestNativeWorkbenchExit(handle uintptr, control *desktopWindowControl) bool {
	if handle == 0 || control == nil || !windows.IsWindow(windows.HWND(handle)) {
		return false
	}
	control.exitRequested.Store(true)
	posted, _, _ := workbenchPostMessageProc.Call(handle, workbenchWMClose, 0, 0)
	return posted != 0
}
