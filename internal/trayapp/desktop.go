package trayapp

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	appassets "casescreen/assets"

	"github.com/gogpu/systray"
)

const desktopTrayStartupTimeout = 5 * time.Second

type DesktopTrayOptions struct {
	Show func()
	Quit func()
}

// DesktopTray 是 CaseScreenDesktop 独立的托盘生命周期。
// systray 的 HWND_MESSAGE 与 Gio 主窗口使用不同 OS 线程和消息泵，
// 二者不会争抢 WM_QUIT。
type DesktopTray struct {
	mu         sync.Mutex
	system     *systray.SystemTray
	closeOnce  sync.Once
	removeOnce sync.Once
	closed     atomic.Bool
	done       chan error
}

type desktopTrayActions struct {
	show func()
	quit func()
}

func newDesktopTrayActions(options DesktopTrayOptions) (*desktopTrayActions, error) {
	if options.Show == nil || options.Quit == nil {
		return nil, errors.New("桌面托盘参数不完整")
	}
	return &desktopTrayActions{show: options.Show, quit: options.Quit}, nil
}

func (actions *desktopTrayActions) showWindow() {
	actions.show()
}

func (actions *desktopTrayActions) quitCompletely() {
	// 先请求 Gio 窗口正常销毁，托盘由 Closed 回调移除。
	// 如果原生关闭请求短暂失败，托盘仍是用户可重试的退出入口，
	// 不会再造成“后台运行但无托盘”的孤儿状态。
	actions.quit()
}

func desktopTrayRunResult(runErr error, closeRequested bool) error {
	if runErr != nil {
		return fmt.Errorf("桌面托盘消息循环失败: %w", runErr)
	}
	if !closeRequested {
		return errors.New("桌面托盘消息循环意外退出")
	}
	return nil
}

// StartDesktopTray 启动桌面工作台托盘，并在图标完成注册后返回。
func StartDesktopTray(options DesktopTrayOptions) (*DesktopTray, error) {
	trayRuntime := &DesktopTray{done: make(chan error, 1)}
	ready := make(chan error, 1)
	go trayRuntime.run(options, ready)
	select {
	case err := <-ready:
		if err != nil {
			trayRuntime.Close()
			return nil, err
		}
		return trayRuntime, nil
	case <-time.After(desktopTrayStartupTimeout):
		trayRuntime.Close()
		return nil, errors.New("系统托盘在 5 秒内未完成启动")
	}
}

func (trayRuntime *DesktopTray) run(options DesktopTrayOptions, ready chan<- error) {
	// Win32 消息队列属于创建 HWND_MESSAGE 的 OS 线程。
	// 从 systray.New 到 Run 必须锁定同一线程。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	actions, err := newDesktopTrayActions(options)
	if err != nil {
		ready <- err
		trayRuntime.done <- err
		return
	}
	systemTray := systray.New()
	menu := systray.NewMenu()
	menu.Add("打开工作台", actions.showWindow)
	menu.AddSeparator()
	menu.Add("完全退出", actions.quitCompletely)
	icon := appassets.IconPNG()
	systemTray.SetIcon(icon).
		SetDarkModeIcon(icon).
		SetTooltip("CaseScreen 机箱副屏").
		SetMenu(menu).
		OnClick(actions.showWindow).
		OnDoubleClick(actions.showWindow)
	trayRuntime.mu.Lock()
	trayRuntime.system = systemTray
	if trayRuntime.closed.Load() {
		trayRuntime.mu.Unlock()
		trayRuntime.removeSystemTray(systemTray)
		ready <- errors.New("系统托盘在显示前已关闭")
		trayRuntime.done <- nil
		return
	}
	systemTray.Show()
	trayRuntime.mu.Unlock()
	ready <- nil
	trayRuntime.done <- desktopTrayRunResult(systemTray.Run(), trayRuntime.closed.Load())
}

// Done 在托盘消息循环退出时返回结果。
func (trayRuntime *DesktopTray) Done() <-chan error {
	if trayRuntime == nil {
		closed := make(chan error)
		close(closed)
		return closed
	}
	return trayRuntime.done
}

// Close 可从 Gio 窗口线程或托盘回调安全调用。
func (trayRuntime *DesktopTray) Close() {
	if trayRuntime == nil {
		return
	}
	trayRuntime.closeOnce.Do(func() {
		trayRuntime.closed.Store(true)
		trayRuntime.mu.Lock()
		systemTray := trayRuntime.system
		trayRuntime.mu.Unlock()
		if systemTray != nil {
			trayRuntime.removeSystemTray(systemTray)
		}
	})
}

func (trayRuntime *DesktopTray) removeSystemTray(systemTray *systray.SystemTray) {
	trayRuntime.removeOnce.Do(systemTray.Remove)
}
