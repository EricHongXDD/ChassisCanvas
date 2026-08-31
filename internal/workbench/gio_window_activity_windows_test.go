//go:build windows

package workbench

import (
	"image"
	"testing"

	"gioui.org/app"
)

func TestWindowsStartupWithoutHWNDStaysPausedUntilVisibleWindowAppears(t *testing.T) {
	previousFind := findVisibleWorkbenchWindow
	previousValid := validWorkbenchWindow
	previousPaused := pausedWorkbenchWindow
	t.Cleanup(func() {
		findVisibleWorkbenchWindow = previousFind
		validWorkbenchWindow = previousValid
		pausedWorkbenchWindow = previousPaused
	})

	var visibleHandle uintptr
	findVisibleWorkbenchWindow = func() uintptr { return visibleHandle }
	validWorkbenchWindow = func(handle uintptr) bool { return handle != 0 }
	pausedWorkbenchWindow = func(uintptr) bool { return false }

	model := &gioEditor{}
	// 托盘启动可能只收到 Windowed/FrameEvent，但它们在无 HWND 时不是可见证据。
	configEvent := app.ConfigEvent{Config: app.Config{Mode: app.Windowed, Focused: true}}
	frameEvent := app.FrameEvent{Size: image.Pt(1500, 900)}
	if windowErr, closed := workbenchWindowClosed(configEvent, model.activity.nativeViewAttached); closed || windowErr != nil {
		t.Fatalf("无 HWND 的 ConfigEvent 被误判为销毁：closed=%v err=%v", closed, windowErr)
	}
	model.observeWindowEvent(configEvent)
	if windowErr, closed := workbenchWindowClosed(frameEvent, model.activity.nativeViewAttached); closed || windowErr != nil {
		t.Fatalf("无 HWND 的 FrameEvent 被误判为销毁：closed=%v err=%v", closed, windowErr)
	}
	model.observeWindowEvent(frameEvent)
	if model.periodicUIWorkEnabled() || model.activity.nativeView != 0 {
		t.Fatalf("启动无 HWND 时被误激活：active=%v hwnd=%#x", model.periodicUIWorkEnabled(), model.activity.nativeView)
	}

	// 窗口首次真正显示后，FrameEvent 通过进程可见窗口枚举找回 HWND。
	visibleHandle = 0x1234
	model.observeWindowEvent(app.FrameEvent{Size: image.Pt(1500, 900)})
	if !model.periodicUIWorkEnabled() || model.activity.nativeView != visibleHandle {
		t.Fatalf("首次可见帧没有恢复：active=%v hwnd=%#x", model.periodicUIWorkEnabled(), model.activity.nativeView)
	}

	// 失去焦点不等于隐藏，已确认的可见窗口应继续保持编辑交互。
	model.observeWindowEvent(app.ConfigEvent{Config: app.Config{Mode: app.Windowed, Focused: false}})
	if !model.periodicUIWorkEnabled() {
		t.Fatal("可见窗口仅失焦时被误暂停")
	}
}

func TestWindowsWin32ViewEventActivatesAndInvalidViewPauses(t *testing.T) {
	previousValid := validWorkbenchWindow
	previousPaused := pausedWorkbenchWindow
	t.Cleanup(func() {
		validWorkbenchWindow = previousValid
		pausedWorkbenchWindow = previousPaused
	})
	validWorkbenchWindow = func(handle uintptr) bool { return handle != 0 }
	pausedWorkbenchWindow = func(uintptr) bool { return false }

	model := &gioEditor{}
	initialInvalidView := app.Win32ViewEvent{}
	if windowErr, closed := workbenchWindowClosed(initialInvalidView, model.activity.nativeViewAttached); closed || windowErr != nil {
		t.Fatalf("启动尚未建立 HWND 时的无效视图被误判为销毁：closed=%v err=%v", closed, windowErr)
	}
	model.observeWindowEvent(initialInvalidView)
	model.observeWindowEvent(app.Win32ViewEvent{HWND: 0x5678})
	if !model.periodicUIWorkEnabled() || model.activity.nativeView != 0x5678 || !model.activity.nativeViewAttached {
		t.Fatal("可见 Win32ViewEvent 没有激活工作台")
	}
	invalidView := app.Win32ViewEvent{}
	if windowErr, closed := workbenchWindowClosed(invalidView, model.activity.nativeViewAttached); !closed || windowErr != nil {
		t.Fatalf("无效 Win32ViewEvent 未触发工作台退出：closed=%v err=%v", closed, windowErr)
	}
	model.observeWindowEvent(invalidView)
	if model.periodicUIWorkEnabled() || model.activity.nativeView != 0 {
		t.Fatal("原生视图失效后仍在运行定时界面任务")
	}
	if windowErr, closed := workbenchWindowClosed(app.Win32ViewEvent{HWND: 0x5678}, model.activity.nativeViewAttached); closed || windowErr != nil {
		t.Fatalf("有效原生窗口被误判为销毁：closed=%v err=%v", closed, windowErr)
	}
	if windowErr, closed := workbenchWindowClosed(app.ConfigEvent{Config: app.Config{Mode: app.Minimized}}, model.activity.nativeViewAttached); closed || windowErr != nil {
		t.Fatalf("最小化被误判为原生窗口销毁：closed=%v err=%v", closed, windowErr)
	}
}
