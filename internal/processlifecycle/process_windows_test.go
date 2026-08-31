//go:build windows

package processlifecycle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSingleInstanceLockRejectsHighConcurrency(t *testing.T) {
	name := fmt.Sprintf(`Local\CaseScreenDesktop-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	const contenders = 32
	type result struct {
		lock InstanceLock
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, contenders)
	for index := 0; index < contenders; index++ {
		go func() {
			<-start
			lock, err := AcquireSingleInstance(name, 120*time.Millisecond)
			results <- result{lock: lock, err: err}
		}()
	}
	close(start)
	var winner InstanceLock
	runningErrors := 0
	for index := 0; index < contenders; index++ {
		current := <-results
		if current.err == nil {
			if winner != nil {
				_ = current.lock.Close()
				t.Fatal("高并发获得了多个单实例锁")
			}
			winner = current.lock
			continue
		}
		if !errors.Is(current.err, ErrInstanceRunning) {
			t.Fatalf("高并发单实例错误=%v", current.err)
		}
		runningErrors++
	}
	if winner == nil || runningErrors != contenders-1 {
		t.Fatalf("单实例结果 winner=%v running_errors=%d", winner != nil, runningErrors)
	}
	if err := winner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSingleInstanceLockAllowsRestartHandoff(t *testing.T) {
	name := fmt.Sprintf(`Local\CaseScreenDesktop-handoff-%d-%d`, os.Getpid(), time.Now().UnixNano())
	first, err := AcquireSingleInstance(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lock InstanceLock
		err  error
	}
	resultChannel := make(chan result, 1)
	go func() {
		lock, acquireErr := AcquireSingleInstance(name, time.Second)
		resultChannel <- result{lock: lock, err: acquireErr}
	}()
	time.Sleep(100 * time.Millisecond)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := <-resultChannel
	if second.err != nil {
		t.Fatalf("自重启交接没有在旧进程释放后获取锁: %v", second.err)
	}
	if err := second.lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectCurrentProcessHasStableIdentity(t *testing.T) {
	first, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Inspect(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !SameIdentity(first, second) || first.Started == 0 {
		t.Fatalf("当前进程身份不稳定: first=%+v second=%+v", first, second)
	}
	if first.Executable == "" || !strings.Contains(strings.ToLower(first.CommandLine), "processlifecycle") {
		t.Fatalf("当前进程信息不完整: %+v", first)
	}
}

func TestTCPListenerPIDFindsExactOwner(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	deadline := time.Now().Add(time.Second)
	for {
		pid, lookupErr := TCPListenerPID(port)
		if lookupErr == nil {
			if pid != os.Getpid() {
				t.Fatalf("监听进程=%d，want %d", pid, os.Getpid())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("没有找到测试监听端口 %d: %v", port, lookupErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWaitForExitHonorsCancellationAndTerminateRejectsChangedIdentity(t *testing.T) {
	current, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitForExit(ctx, current); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消等待错误=%v", err)
	}
	changed := current
	changed.Started++
	if err := Terminate(changed, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "身份已变化") {
		t.Fatalf("错误身份未被拒绝: %v", err)
	}
}
