//go:build linux

package processlifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func currentProcessID() int { return os.Getpid() }

func acquireSingleInstance(string, time.Duration) (InstanceLock, error) {
	return nil, ErrUnsupported
}

func Inspect(pid int) (Snapshot, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, ErrProcessNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	// comm 字段可以包含空格和括号；最后一个右括号之后才是稳定的数字字段。
	closing := strings.LastIndexByte(string(stat), ')')
	if closing < 0 {
		return Snapshot{}, errors.New("进程 stat 格式无效")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	if len(fields) < 20 {
		return Snapshot{}, errors.New("进程 stat 字段不足")
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil {
		return Snapshot{}, err
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return Snapshot{}, err
	}
	executable, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	commandBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	commandLine := strings.TrimSpace(strings.ReplaceAll(string(commandBytes), "\x00", " "))
	return Snapshot{PID: pid, ParentPID: parentID, Executable: executable, CommandLine: commandLine, Started: started}, nil
}

func TCPListenerPID(uint16) (int, error) { return 0, ErrUnsupported }

func waitForExit(ctx context.Context, process Snapshot) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := Inspect(process.PID)
		if errors.Is(err, ErrProcessNotFound) || (err == nil && !SameIdentity(process, current)) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func terminate(process Snapshot, timeout time.Duration) error {
	current, err := Inspect(process.PID)
	if errors.Is(err, ErrProcessNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !SameIdentity(process, current) {
		return errors.New("进程身份已变化，拒绝终止")
	}
	target, err := os.FindProcess(process.PID)
	if err != nil {
		return err
	}
	if err := target.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return waitForExit(ctx, process)
}
