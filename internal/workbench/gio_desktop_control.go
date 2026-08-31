package workbench

import (
	"sync/atomic"
)

// DesktopRunOptions 把系统托盘与 Gio 窗口生命周期连接起来。
// Ready 在进入 app.Main 前调用，便于桌面入口先创建托盘图标。
type DesktopRunOptions struct {
	Ready  func(*DesktopWindowController) error
	Closed func(error)
}

type desktopWindowControl struct {
	showRequested atomic.Bool
	exitRequested atomic.Bool
	nativeView    atomic.Uintptr
	wake          func()
	onHidden      func()
}

// DesktopWindowController 是系统托盘可以安全跨线程调用的窗口控制器。
// Show 恢复工作台，Quit 才会真正销毁原生窗口。
type DesktopWindowController struct {
	control *desktopWindowControl
}

func newDesktopWindowController(model *gioEditor) *DesktopWindowController {
	control := &desktopWindowControl{}
	control.wake = func() {
		if model.window != nil {
			model.window.Invalidate()
		}
	}
	control.onHidden = func() { model.setWindowPaused(true) }
	model.desktopControl = control
	return &DesktopWindowController{control: control}
}

// Show 显示并激活工作台窗口。
func (controller *DesktopWindowController) Show() {
	if controller == nil || controller.control == nil || controller.control.exitRequested.Load() {
		return
	}
	controller.control.showRequested.Store(true)
	if handle := controller.control.nativeView.Load(); handle != 0 {
		_ = showNativeWorkbenchWindow(handle)
	}
	controller.control.wakeWindow()
}

// Quit 请求完全退出；原生 WM_CLOSE 拦截器只在此标志为真时放行销毁。
func (controller *DesktopWindowController) Quit() {
	if controller == nil || controller.control == nil {
		return
	}
	controller.control.exitRequested.Store(true)
	// 退出优先级高于之前尚未消化的托盘显示请求，
	// 避免完全退出与打开操作紧邻时又把窗口短暂唤醒。
	controller.control.showRequested.Store(false)
	if handle := controller.control.nativeView.Load(); handle != 0 {
		_ = requestNativeWorkbenchExit(handle, controller.control)
	}
	controller.control.wakeWindow()
}

func (control *desktopWindowControl) wakeWindow() {
	if control != nil && control.wake != nil {
		control.wake()
	}
}

func (control *desktopWindowControl) windowHidden() {
	if control != nil && control.onHidden != nil {
		control.onHidden()
	}
}

func shouldHideWorkbenchClose(control *desktopWindowControl) bool {
	return control != nil && !control.exitRequested.Load()
}

func (model *gioEditor) applyDesktopWindowRequests() {
	control := model.desktopControl
	if control == nil {
		return
	}
	if control.exitRequested.Load() {
		if requestNativeWorkbenchExit(model.activity.nativeView, control) {
			return
		}
	}
	if control.showRequested.Swap(false) && showNativeWorkbenchWindow(model.activity.nativeView) {
		model.setWindowPaused(false)
	}
}
