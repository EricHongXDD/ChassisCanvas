//go:build !linux

package edgedisplay

import (
	"errors"
	"os/exec"
)

func openPlatformEndpoint(Options) (platformEndpoint, error) {
	return nil, errors.New("屏端 PTY 显示代理仅支持 Linux")
}

func configureChildProcess(*exec.Cmd) {}

func signalChild(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}

func killChild(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}
