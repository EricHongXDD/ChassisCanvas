//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

const desktopInstanceSignalWaitMilliseconds = 100

type desktopInstanceSignal interface {
	Requests() <-chan struct{}
	Close() error
}

type windowsDesktopInstanceSignal struct {
	handle   windows.Handle
	requests chan struct{}
	stop     chan struct{}
	done     chan struct{}
	close    sync.Once
	closeErr error
}

func listenDesktopInstanceSignal(name string) (desktopInstanceSignal, error) {
	wideName, err := desktopInstanceSignalNameUTF16(name)
	if err != nil {
		return nil, err
	}
	handle, createErr := windows.CreateEvent(nil, 0, 0, wideName)
	if errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, errors.New("同名显示信号已被其他进程占用")
	}
	if createErr != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, fmt.Errorf("创建显示信号: %w", createErr)
	}
	signal := &windowsDesktopInstanceSignal{
		handle:   handle,
		requests: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go signal.wait()
	return signal, nil
}

func notifyDesktopInstance(name string) error {
	wideName, err := desktopInstanceSignalNameUTF16(name)
	if err != nil {
		return err
	}
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, wideName)
	if err != nil {
		return fmt.Errorf("打开同版工作台显示信号: %w", err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetEvent(handle); err != nil {
		return fmt.Errorf("发送同版工作台显示信号: %w", err)
	}
	return nil
}

func desktopInstanceSignalNameUTF16(name string) (*uint16, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("工作台显示信号名称为空")
	}
	wideName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("编码工作台显示信号名称: %w", err)
	}
	return wideName, nil
}

func (signal *windowsDesktopInstanceSignal) Requests() <-chan struct{} {
	return signal.requests
}

func (signal *windowsDesktopInstanceSignal) wait() {
	defer close(signal.done)
	defer close(signal.requests)
	for {
		select {
		case <-signal.stop:
			return
		default:
		}
		result, err := windows.WaitForSingleObject(signal.handle, desktopInstanceSignalWaitMilliseconds)
		if err != nil {
			return
		}
		switch result {
		case windows.WAIT_OBJECT_0:
			select {
			case signal.requests <- struct{}{}:
			default:
			}
		case uint32(windows.WAIT_TIMEOUT):
		default:
			return
		}
	}
}

func (signal *windowsDesktopInstanceSignal) Close() error {
	if signal == nil {
		return nil
	}
	signal.close.Do(func() {
		close(signal.stop)
		<-signal.done
		signal.closeErr = windows.CloseHandle(signal.handle)
		signal.handle = 0
	})
	return signal.closeErr
}
