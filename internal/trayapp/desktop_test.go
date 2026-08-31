package trayapp

import (
	"errors"
	"sync/atomic"
	"testing"
)

func TestDesktopTrayActionsAllowShowAndExitRetry(t *testing.T) {
	var shows atomic.Int32
	var quits atomic.Int32
	actions, err := newDesktopTrayActions(DesktopTrayOptions{
		Show: func() { shows.Add(1) },
		Quit: func() { quits.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	actions.showWindow()
	actions.showWindow()
	actions.quitCompletely()
	actions.quitCompletely()
	if shows.Load() != 2 || quits.Load() != 2 {
		t.Fatalf("托盘动作次数错误：show=%d quit=%d", shows.Load(), quits.Load())
	}
}

func TestDesktopTrayActionsRejectIncompleteOptions(t *testing.T) {
	tests := []DesktopTrayOptions{
		{},
		{Show: func() {}},
		{Quit: func() {}},
	}
	for _, options := range tests {
		if _, err := newDesktopTrayActions(options); err == nil {
			t.Fatal("不完整托盘参数未被拒绝")
		}
	}
}

func TestDesktopTrayRunResultDistinguishesExpectedAndUnexpectedExit(t *testing.T) {
	if err := desktopTrayRunResult(nil, true); err != nil {
		t.Fatalf("主动关闭托盘被误报为异常: %v", err)
	}
	if err := desktopTrayRunResult(nil, false); err == nil {
		t.Fatal("托盘未收到关闭请求却退出时没有报错")
	}
	expected := errors.New("消息泵失败")
	if err := desktopTrayRunResult(expected, true); !errors.Is(err, expected) {
		t.Fatalf("托盘运行错误丢失: %v", err)
	}
}
