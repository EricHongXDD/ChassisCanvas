//go:build !windows && !linux

package processlifecycle

import (
	"context"
	"os"
	"time"
)

func currentProcessID() int { return os.Getpid() }

func acquireSingleInstance(string, time.Duration) (InstanceLock, error) {
	return nil, ErrUnsupported
}

func Inspect(int) (Snapshot, error) { return Snapshot{}, ErrUnsupported }

func TCPListenerPID(uint16) (int, error) { return 0, ErrUnsupported }

func waitForExit(context.Context, Snapshot) error { return ErrUnsupported }

func terminate(Snapshot, time.Duration) error { return ErrUnsupported }
