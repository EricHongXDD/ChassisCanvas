package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeManagedService struct {
	done         chan struct{}
	err          error
	shutdownOnce sync.Once
	shutdowns    atomic.Int32
}

func newFakeManagedService() *fakeManagedService {
	return &fakeManagedService{done: make(chan struct{})}
}

func (service *fakeManagedService) Done() <-chan struct{} { return service.done }
func (service *fakeManagedService) Err() error            { return service.err }
func (service *fakeManagedService) shutdown() {
	service.shutdownOnce.Do(func() {
		service.shutdowns.Add(1)
		close(service.done)
	})
}
func (service *fakeManagedService) fail(err error) {
	service.err = err
	service.shutdownOnce.Do(func() { close(service.done) })
}

func TestManagedServiceSupervisorRestartsUnexpectedExit(t *testing.T) {
	initial := newFakeManagedService()
	replacement := newFakeManagedService()
	var starts atomic.Int32
	supervisor := newManagedServiceSupervisor("release", "http://127.0.0.1:19321", initial,
		func(string, string) (managedServiceHandle, error) {
			starts.Add(1)
			return replacement, nil
		})
	supervisor.restartBase = time.Millisecond
	supervisor.restartMax = time.Millisecond
	defer supervisor.Stop()

	initial.fail(errors.New("模拟后台崩溃"))
	deadline := time.Now().Add(time.Second)
	for starts.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() != 1 {
		t.Fatal("后台崩溃后没有自动重启")
	}
	select {
	case err := <-supervisor.Fatal():
		t.Fatalf("首次恢复成功却上报致命错误: %v", err)
	default:
	}
}

func TestManagedServiceSupervisorUsesBoundedRetries(t *testing.T) {
	initial := newFakeManagedService()
	var starts atomic.Int32
	supervisor := newManagedServiceSupervisor("release", "http://127.0.0.1:19321", initial,
		func(string, string) (managedServiceHandle, error) {
			starts.Add(1)
			return nil, errors.New("模拟启动失败")
		})
	supervisor.restartLimit = 3
	supervisor.restartBase = time.Millisecond
	supervisor.restartMax = time.Millisecond
	defer supervisor.Stop()

	initial.fail(errors.New("模拟后台崩溃"))
	select {
	case err := <-supervisor.Fatal():
		if err == nil || starts.Load() != 3 {
			t.Fatalf("有界重试结果错误: starts=%d err=%v", starts.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("连续启动失败后没有终止空壳状态")
	}
}

func TestManagedServiceSupervisorStopDoesNotRestart(t *testing.T) {
	initial := newFakeManagedService()
	var starts atomic.Int32
	supervisor := newManagedServiceSupervisor("release", "http://127.0.0.1:19321", initial,
		func(string, string) (managedServiceHandle, error) {
			starts.Add(1)
			return newFakeManagedService(), nil
		})
	supervisor.restartBase = time.Millisecond
	supervisor.restartMax = time.Millisecond
	supervisor.Stop()
	if starts.Load() != 0 {
		t.Fatalf("主动退出时错误重启了服务: %d", starts.Load())
	}
}

func TestManagedServiceSupervisorStopDuringReplacementPublicationClosesReplacement(t *testing.T) {
	initial := newFakeManagedService()
	replacement := newFakeManagedService()
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	supervisor := newManagedServiceSupervisor("release", "http://127.0.0.1:19321", initial,
		func(string, string) (managedServiceHandle, error) {
			close(startEntered)
			<-releaseStart
			return replacement, nil
		})
	supervisor.restartBase = time.Millisecond
	supervisor.restartMax = time.Millisecond

	initial.fail(errors.New("模拟后台崩溃"))
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("重启没有进入可控发布阶段")
	}
	stopDone := make(chan struct{})
	go func() {
		supervisor.Stop()
		close(stopDone)
	}()
	// 这个屏障确保 Stop 已 cancel，但 starter 尚未交还新句柄。
	select {
	case <-supervisor.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop 没有取消监督上下文")
	}
	close(releaseStart)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop 没有等待并发启动安全收尾")
	}
	if got := replacement.shutdowns.Load(); got != 1 {
		t.Fatalf("取消后返回的新后台关闭次数=%d，want 1", got)
	}
	supervisor.mu.Lock()
	current := supervisor.current
	supervisor.mu.Unlock()
	if current != nil {
		t.Fatal("Stop 返回后仍发布了新后台句柄")
	}
}
