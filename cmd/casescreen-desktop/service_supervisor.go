package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	managedServiceRestartLimit = 5
	managedServiceRestartBase  = 250 * time.Millisecond
	managedServiceRestartMax   = 4 * time.Second
	managedServiceStableWindow = 30 * time.Second
)

type managedServiceHandle interface {
	Done() <-chan struct{}
	Err() error
	shutdown()
}

type managedServiceStarter func(root, baseURL string) (managedServiceHandle, error)

// managedServiceSupervisor 只监督 Desktop 自己启动且持有的后台。
// 服务崩溃后按有界指数退避重建；多次失败则通知工作台
// 完整退出，避免托盘与窗口留下无 API 的“空壳”。
type managedServiceSupervisor struct {
	root    string
	baseURL string
	start   managedServiceStarter

	ctx    context.Context
	cancel context.CancelFunc
	fatal  chan error
	done   chan struct{}

	mu      sync.Mutex
	current managedServiceHandle

	restartLimit int
	restartBase  time.Duration
	restartMax   time.Duration
	stableWindow time.Duration
	stopOnce     sync.Once
}

func newManagedServiceSupervisor(root, baseURL string, initial managedServiceHandle, start managedServiceStarter) *managedServiceSupervisor {
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := &managedServiceSupervisor{
		root: root, baseURL: baseURL, start: start,
		ctx: ctx, cancel: cancel, fatal: make(chan error, 1), done: make(chan struct{}),
		current: initial, restartLimit: managedServiceRestartLimit,
		restartBase: managedServiceRestartBase, restartMax: managedServiceRestartMax,
		stableWindow: managedServiceStableWindow,
	}
	go supervisor.run(initial)
	return supervisor
}

func (supervisor *managedServiceSupervisor) Fatal() <-chan error { return supervisor.fatal }

func (supervisor *managedServiceSupervisor) Stop() {
	if supervisor == nil {
		return
	}
	supervisor.stopOnce.Do(func() {
		supervisor.mu.Lock()
		// cancel 与取走 current 必须与重启发布共用同一把锁。
		// 否则 run 可能在 Stop 读到旧句柄后才发布新进程，
		// 使新进程没有任何所有者可以关闭。
		supervisor.cancel()
		current := supervisor.current
		supervisor.current = nil
		supervisor.mu.Unlock()
		if current != nil {
			current.shutdown()
		}
		<-supervisor.done
	})
}

func (supervisor *managedServiceSupervisor) run(initial managedServiceHandle) {
	defer close(supervisor.done)
	current := initial
	consecutiveFailures := 0
	startedAt := time.Now()
	for current != nil {
		select {
		case <-supervisor.ctx.Done():
			return
		case <-current.Done():
		}
		if supervisor.ctx.Err() != nil {
			return
		}
		if supervisor.stableWindow > 0 && time.Since(startedAt) >= supervisor.stableWindow {
			consecutiveFailures = 0
		}
		lastErr := current.Err()
		if lastErr == nil {
			lastErr = errors.New("后台服务意外退出")
		}
		for {
			consecutiveFailures++
			if consecutiveFailures > supervisor.restartLimit {
				supervisor.reportFatal(fmt.Errorf(
					"CaseScreen 后台连续 %d 次无法恢复: %w",
					supervisor.restartLimit, lastErr,
				))
				return
			}
			if !supervisor.waitRestartDelay(consecutiveFailures) {
				return
			}
			replacement, err := supervisor.start(supervisor.root, supervisor.baseURL)
			if err == nil && replacement == nil {
				err = errors.New("重启后服务不属于当前工作台")
			}
			if err != nil {
				lastErr = err
				continue
			}
			supervisor.mu.Lock()
			// 与 Stop 的 cancel+take 原子交接：Stop 先拿锁时
			// 新句柄不得发布；run 先发布时则由 Stop 取走它。
			if supervisor.ctx.Err() != nil {
				supervisor.mu.Unlock()
				replacement.shutdown()
				return
			}
			supervisor.current = replacement
			supervisor.mu.Unlock()
			current = replacement
			startedAt = time.Now()
			break
		}
	}
}

func (supervisor *managedServiceSupervisor) waitRestartDelay(attempt int) bool {
	delay := supervisor.restartBase
	if delay <= 0 {
		delay = time.Millisecond
	}
	maximum := supervisor.restartMax
	if maximum <= 0 {
		maximum = delay
	}
	for step := 1; step < attempt && delay < maximum; step++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-supervisor.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (supervisor *managedServiceSupervisor) reportFatal(err error) {
	select {
	case supervisor.fatal <- err:
	default:
	}
}
