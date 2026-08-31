//go:build !windows

package hidapi

import (
	"context"
	"errors"
	"time"
)

type Transport struct{}

func Open(_ string, _, _ uint16, _ int) (*Transport, error) {
	return nil, errors.New("当前系统尚未实现 hidapi 动态传输")
}

func (*Transport) Write(context.Context, []byte, time.Duration) (int, error) {
	return 0, errors.New("当前系统尚未实现 hidapi 动态传输")
}

func (*Transport) Read(context.Context, []byte, time.Duration) (int, error) {
	return 0, errors.New("当前系统尚未实现 hidapi 动态传输")
}

func (*Transport) Close() error { return nil }
