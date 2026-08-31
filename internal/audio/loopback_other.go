//go:build !windows

package audio

import (
	"context"
	"errors"
)

type Loopback struct{}

func NewLoopback() *Loopback {
	return &Loopback{}
}

func (*Loopback) Capture(context.Context, func(PCMFrame) error) error {
	return errors.New("当前平台尚未实现系统音频回环采集")
}

func (*Loopback) SourceStatus() SourceStatus {
	return SourceStatus{EndpointRole: "console"}
}
