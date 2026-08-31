package workbench

import (
	"sync/atomic"
	"testing"
)

func TestDesktopControllerShowAndQuitState(t *testing.T) {
	var wakes atomic.Int32
	control := &desktopWindowControl{wake: func() { wakes.Add(1) }}
	controller := &DesktopWindowController{control: control}
	controller.Show()
	if !control.showRequested.Load() || control.exitRequested.Load() || wakes.Load() != 1 {
		t.Fatalf("显示请求状态错误：show=%v exit=%v wakes=%d", control.showRequested.Load(), control.exitRequested.Load(), wakes.Load())
	}
	controller.Quit()
	if !control.exitRequested.Load() || control.showRequested.Load() || wakes.Load() != 2 {
		t.Fatalf("退出请求状态错误：exit=%v show=%v wakes=%d", control.exitRequested.Load(), control.showRequested.Load(), wakes.Load())
	}
	control.showRequested.Store(false)
	controller.Show()
	if control.showRequested.Load() || wakes.Load() != 2 {
		t.Fatal("完全退出后仍接受了显示请求")
	}
}

func TestWorkbenchCloseHidesUntilCompleteExitRequested(t *testing.T) {
	control := &desktopWindowControl{}
	if !shouldHideWorkbenchClose(control) {
		t.Fatal("普通点击窗口 X 没有转为隐藏")
	}
	control.exitRequested.Store(true)
	if shouldHideWorkbenchClose(control) {
		t.Fatal("托盘完全退出仍被 WM_CLOSE 拦截")
	}
}

func TestDesktopWindowHiddenPausesRendering(t *testing.T) {
	model := &gioEditor{}
	controller := newDesktopWindowController(model)
	if controller == nil || model.desktopControl == nil {
		t.Fatal("桌面窗口控制器未建立")
	}
	model.desktopControl.windowHidden()
	if model.periodicUIWorkEnabled() {
		t.Fatal("关闭到托盘后仍允许定时绘制")
	}
}
