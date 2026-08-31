package workbench

import (
	"encoding/json"
	"errors"
	"image"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"casescreen/internal/service"
	caseTheme "casescreen/internal/theme"
	"gioui.org/app"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

func TestDestroyEventClosesWorkbenchAndPreservesError(t *testing.T) {
	expected := errors.New("窗口后端已退出")
	actual, closed := workbenchWindowClosed(app.DestroyEvent{Err: expected}, false)
	if !closed || !errors.Is(actual, expected) {
		t.Fatalf("DestroyEvent 未正确终止工作台：closed=%v err=%v", closed, actual)
	}
	if actual, closed = workbenchWindowClosed(app.ConfigEvent{Config: app.Config{Mode: app.Minimized}}, true); closed || actual != nil {
		t.Fatalf("普通最小化被误判为销毁：closed=%v err=%v", closed, actual)
	}
}

func TestPausedWindowSubmitsEmptyFrameWithoutRunningLayout(t *testing.T) {
	updates := make(chan func(), 1)
	updateApplied := false
	updates <- func() { updateApplied = true }
	model := &gioEditor{uiUpdates: updates}
	// 已附着过 HWND 才是真正的隐藏/最小化状态，
	// 不能与启动建窗窗口混为一谈。
	model.activity.nativeViewAttached = true
	var operations op.Ops
	frameSubmitted := false
	model.renderWindowFrame(&operations, app.FrameEvent{
		Now:  time.Now(),
		Size: image.Pt(1024, 600),
		Frame: func(*op.Ops) {
			frameSubmitted = true
		},
	})
	if !frameSubmitted {
		t.Fatal("暂停窗口没有正常完成 Gio 帧请求")
	}
	if updateApplied || !model.previewNow.IsZero() {
		t.Fatalf("暂停窗口仍执行了界面更新或布局：update=%v preview=%v", updateApplied, model.previewNow)
	}
}

func TestUnattachedStartupFrameRendersStaticUIAndRetriesBoundedly(t *testing.T) {
	var router input.Router
	model := &gioEditor{
		theme:        material.NewTheme(),
		manifest:     defaultManifest(),
		target:       "waveshare-7",
		inspectorTab: "layers",
		targetOptions: []targetOption{{
			id: "waveshare-7", label: "微雪 7 英寸", resolution: [2]int{1024, 600},
		}},
	}
	var operations op.Ops
	now := time.Now()
	frameSubmitted := false
	model.renderWindowFrame(&operations, app.FrameEvent{
		Now:    now,
		Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Size:   image.Pt(1500, 900),
		Source: router.Source(),
		Frame: func(*op.Ops) {
			frameSubmitted = true
		},
	})
	if !frameSubmitted || !model.previewNow.Equal(now) {
		t.Fatalf("未附着 HWND 的启动首帧未绘制静态界面：submitted=%v preview=%v", frameSubmitted, model.previewNow)
	}
	if model.periodicUIWorkEnabled() || model.audioPreview.running || model.targetStatus.running {
		t.Fatal("启动静态首帧误启用了定时轮询")
	}
	if model.activity.startupFrameCount != 1 {
		t.Fatalf("启动帧计数错误：%d", model.activity.startupFrameCount)
	}
	wakeupAt, wakeupScheduled := router.WakeupTime()
	if !wakeupScheduled || !wakeupAt.Equal(now.Add(startupWindowAttachRetryInterval)) {
		t.Fatalf("启动帧未按有界延迟请求附着重试：scheduled=%v at=%v", wakeupScheduled, wakeupAt)
	}

	// 重试有严格上限，后端未建窗时不会无限空转。
	for index := 1; index < startupWindowAttachRetryLimit; index++ {
		render, retry := model.beginStartupWindowFrame()
		if !render {
			t.Fatalf("第 %d 次有界重试被过早停止", index+1)
		}
		if retry != (index+1 < startupWindowAttachRetryLimit) {
			t.Fatalf("第 %d 次重试标志错误：%v", index+1, retry)
		}
	}
	if render, retry := model.beginStartupWindowFrame(); render || retry {
		t.Fatalf("超过上限后仍继续请求帧：render=%v retry=%v", render, retry)
	}
}

func TestStartupFrameIsSuppressedWhenExplicitlyMinimized(t *testing.T) {
	model := &gioEditor{}
	model.observeWindowEvent(app.ConfigEvent{Config: app.Config{Mode: app.Minimized}})
	if render, retry := model.beginStartupWindowFrame(); render || retry {
		t.Fatalf("显式最小化时仍绘制启动帧：render=%v retry=%v", render, retry)
	}

	// 在 HWND 建立前恢复为 Windowed，允许重新走启动附着路径。
	model.observeWindowEvent(app.ConfigEvent{Config: app.Config{Mode: app.Windowed}})
	if render, _ := model.beginStartupWindowFrame(); !render {
		t.Fatal("恢复 Windowed 后没有重新启用启动首帧")
	}
}

func TestMinimizedWindowStopsPeriodicPreviewAndStatusWork(t *testing.T) {
	model := &gioEditor{
		api: NewAPI("http://127.0.0.1:1"),
		manifest: caseTheme.Manifest{Layers: []caseTheme.Layer{{
			Type: caseTheme.LayerAudioBars, Opacity: 1, Options: map[string]any{"visible": true},
		}}},
	}
	model.observeWindowEvent(app.ConfigEvent{Config: app.Config{Mode: app.Minimized}})
	if model.periodicUIWorkEnabled() {
		t.Fatal("最小化后仍允许定时界面任务")
	}

	var operations op.Ops
	gtx := layout.Context{Ops: &operations, Now: time.Now(), Constraints: layout.Exact(image.Pt(1024, 600))}
	model.scheduleAudioPreviewRefresh(gtx)
	model.scheduleTargetStatusRefresh(gtx)
	model.schedulePreviewAnimation(gtx)
	if model.audioPreview.running || model.targetStatus.running {
		t.Fatal("最小化时仍发起了 API 轮询")
	}
}

func TestWindowActivityStartsPausedAndRestoreRefreshesImmediately(t *testing.T) {
	model := &gioEditor{}
	model.audioPreview.lastRefresh = time.Now()
	model.targetStatus.lastRefresh = time.Now()
	if model.periodicUIWorkEnabled() {
		t.Fatal("未确认原生窗口前不应启用定时界面任务")
	}

	model.setWindowPaused(false)
	if !model.periodicUIWorkEnabled() {
		t.Fatal("确认窗口可见后没有启用界面任务")
	}
	model.setWindowPaused(true)
	generation := model.windowActivityGeneration()
	model.setWindowPaused(false)
	if !model.periodicUIWorkEnabled() || model.windowActivityGeneration() == generation {
		t.Fatal("恢复窗口后没有切换到新的活动世代")
	}
	if !model.audioPreview.lastRefresh.IsZero() || !model.targetStatus.lastRefresh.IsZero() {
		t.Fatalf("恢复后没有触发立即刷新：audio=%v target=%v", model.audioPreview.lastRefresh, model.targetStatus.lastRefresh)
	}
}

func TestAudioResultCompletedAcrossPauseIsDiscardedAndRefetched(t *testing.T) {
	var calls atomic.Int32
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/status" {
			http.NotFound(writer, request)
			return
		}
		call := calls.Add(1)
		if call == 1 {
			firstStarted <- struct{}{}
			<-releaseFirst
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(writer).Encode(service.Status{Audio: service.AudioStatus{
			Enabled: true, Running: true, CapturedAt: time.Now(), Bands: []float64{.2, .8},
		}})
	}))
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), uiUpdates: make(chan func(), 4)}
	model.setWindowPaused(false)
	model.refreshAudioPreviewAsync()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("首次音频状态请求未启动")
	}

	model.setWindowPaused(true)
	model.setWindowPaused(false)
	close(releaseFirst)
	applyNextUIUpdate(t, model)
	if model.audioPreview.running || !model.audioPreview.lastRefresh.IsZero() || len(model.audioPreview.status.Bands) != 0 {
		t.Fatalf("暂停前的过期音频结果被应用：running=%v refresh=%v bands=%v", model.audioPreview.running, model.audioPreview.lastRefresh, model.audioPreview.status.Bands)
	}

	// 恢复后的第一帧应立即发起新请求，不再等 100 ms 旧截止时间。
	model.refreshAudioPreviewAsync()
	applyNextUIUpdate(t, model)
	if calls.Load() != 2 || model.audioPreview.lastRefresh.IsZero() || len(model.audioPreview.status.Bands) != 2 {
		t.Fatalf("恢复后没有立即重取音频状态：calls=%d refresh=%v bands=%v", calls.Load(), model.audioPreview.lastRefresh, model.audioPreview.status.Bands)
	}
}
