package processlifecycle

import (
	"context"
	"errors"
	"time"
)

var (
	ErrProcessNotFound = errors.New("进程不存在")
	ErrUnsupported     = errors.New("当前平台不支持进程归属探测")
	ErrInstanceRunning = errors.New("程序已在运行")
)

// InstanceLock 在持有期间阻止同一产品的第二个桌面进程启动。
// 关闭锁或进程退出后，系统会允许新实例接管。
type InstanceLock interface {
	Close() error
}

// Snapshot 是用于终止前二次核验的进程身份。PID 可能被系统复用，因此所有
// 破坏性操作都必须同时核对 Started，而不能只凭进程名或 PID。
type Snapshot struct {
	PID         int
	ParentPID   int
	Executable  string
	CommandLine string
	Started     uint64
	// HasVisibleWindow 只用于识别旧版 Gio 窗口已销毁、进程却仍阻塞的遗留桌面。
	HasVisibleWindow bool
}

// SameIdentity 判断两个快照是否仍指向同一次进程运行。
func SameIdentity(left, right Snapshot) bool {
	return left.PID > 0 && left.PID == right.PID && left.Started != 0 && left.Started == right.Started
}

// Current 返回当前进程可用于传给子进程的稳定身份。
func Current() (Snapshot, error) {
	return Inspect(currentProcessID())
}

// WaitForExit 在目标退出或上下文取消时结束。实现会核对启动标记，避免 PID
// 复用后把另一个进程误认为仍是原桌面进程。
func WaitForExit(ctx context.Context, process Snapshot) error {
	return waitForExit(ctx, process)
}

// Terminate 仅在目标仍与快照完全一致时强制结束它。
func Terminate(process Snapshot, timeout time.Duration) error {
	return terminate(process, timeout)
}

// AcquireSingleInstance 获取跨进程单实例锁。wait 用于 GPU 首启自重启
// 的短暂交接窗口；已经稳定运行的实例不会被新进程抢占。
func AcquireSingleInstance(name string, wait time.Duration) (InstanceLock, error) {
	return acquireSingleInstance(name, wait)
}
