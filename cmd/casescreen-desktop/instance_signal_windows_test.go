//go:build windows

package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestDesktopInstanceSignalRoundTripAndExclusiveListener(t *testing.T) {
	name := fmt.Sprintf(`Local\CaseScreenDesktop.Show.test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	signal, err := listenDesktopInstanceSignal(name)
	if err != nil {
		t.Fatal(err)
	}
	defer signal.Close()

	if duplicate, duplicateErr := listenDesktopInstanceSignal(name); duplicateErr == nil {
		_ = duplicate.Close()
		t.Fatal("同名显示信号被第二个监听者占用")
	}
	if err := notifyDesktopInstance(name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signal.Requests():
	case <-time.After(time.Second):
		t.Fatal("同版工作台显示信号未送达")
	}
}
