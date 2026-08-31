package workbench

import (
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/event"
)

// windowActivityState 记录工作台是否真正可见。Gio 在 Windows 上会让
// 已排程的 InvalidateCmd 在最小化后继续触发，因此不能只依赖帧事件节流。
type windowActivityState struct {
	active             atomic.Bool
	generation         atomic.Uint64
	nativeView         uintptr
	nativeViewAttached bool
	startupMinimized   bool
	startupFrameCount  uint8
}

const (
	// Windows 可能在 HWND 真正可见前就投递首个 FrameEvent。
	// 首帧必须绘制完整静态界面，同时用有界重试等待原生窗口附着；
	// 如果后端始终没有建立窗口，最多约 1 秒后停止唤醒。
	startupWindowAttachRetryLimit    = 20
	startupWindowAttachRetryInterval = 50 * time.Millisecond
)

func (model *gioEditor) periodicUIWorkEnabled() bool {
	// 零值必须是暂停。托盘启动时可能根本收不到 Win32ViewEvent，
	// 只有确认存在可见原生窗口后才允许定时绘制与 API 轮询。
	return model.activity.active.Load()
}

func (model *gioEditor) windowActivityGeneration() uint64 {
	return model.activity.generation.Load()
}

// observeWindowEvent 把 Gio 的窗口生命周期转换为统一的“可绘制/暂停”状态。
// Gio v0.10 已没有 StageEvent，Windows 最小化也可能仍报 Windowed，
// 所以 Windows 实现会结合 Win32ViewEvent 的 HWND 读取真实可见性。
func (model *gioEditor) observeWindowEvent(eventValue event.Event) {
	if configEvent, ok := eventValue.(app.ConfigEvent); ok && !model.activity.nativeViewAttached {
		// 显式最小化不是“启动建窗窗口”，不应为它绘制
		// 首帧或继续排程附着重试。恢复 Windowed 后可再尝试。
		model.activity.startupMinimized = configEvent.Config.Mode == app.Minimized
	}
	handle, paused, known := nativeWindowActivity(model.activity.nativeView, eventValue)
	model.activity.nativeView = handle
	if handle != 0 {
		model.activity.nativeViewAttached = true
	}
	if model.desktopControl != nil {
		model.desktopControl.nativeView.Store(handle)
		if handle != 0 {
			if err := attachNativeWorkbenchWindow(handle, model.desktopControl); err != nil {
				model.message = "无法启用关闭到托盘：" + err.Error()
			}
		}
	}
	if known {
		model.setWindowPaused(paused)
	}
}

// beginStartupWindowFrame 仅在首次附着原生窗口之前允许绘制。
// 返回值分别表示“绘制静态首帧”和“绘制后再请求一帧”。
func (model *gioEditor) beginStartupWindowFrame() (bool, bool) {
	activity := &model.activity
	if activity.nativeViewAttached || activity.startupMinimized || activity.startupFrameCount >= startupWindowAttachRetryLimit {
		return false, false
	}
	activity.startupFrameCount++
	return true, activity.startupFrameCount < startupWindowAttachRetryLimit
}

// workbenchWindowClosed 同时兼容 Gio 的通用 DestroyEvent 和 Windows
// 原生视图销毁通知。Gio v0.10 在 WM_DESTROY 时先发送无效的
// Win32ViewEvent，然后才发送 DestroyEvent；若后者因窗口后端异常
// 未被上层取到，已经建立过原生视图的工作台仍必须把前者视为
// 终止事件。启动阶段尚未建窗的 HWND=0 只保持暂停，不能误退出。
func workbenchWindowClosed(eventValue event.Event, nativeViewAttached bool) (error, bool) {
	if destroyEvent, ok := eventValue.(app.DestroyEvent); ok {
		return destroyEvent.Err, true
	}
	if nativeWindowDestroyed(eventValue, nativeViewAttached) {
		return nil, true
	}
	return nil, false
}

func (model *gioEditor) setWindowPaused(paused bool) {
	active := !paused
	previous := model.activity.active.Swap(active)
	if previous == active {
		return
	}
	model.activity.generation.Add(1)
	if !active {
		return
	}

	// 恢复时不继续等待暂停前的截止时间：下一帧立即重取真实
	// 音频与设备状态，同时重绘静态编辑内容。
	model.audioPreview.lastRefresh = time.Time{}
	model.targetStatus.lastRefresh = time.Time{}
	model.invalidateWindow()
}
