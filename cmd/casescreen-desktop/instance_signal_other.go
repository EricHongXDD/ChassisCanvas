//go:build !windows

package main

import "casescreen/internal/processlifecycle"

type desktopInstanceSignal interface {
	Requests() <-chan struct{}
	Close() error
}

func listenDesktopInstanceSignal(string) (desktopInstanceSignal, error) {
	return nil, processlifecycle.ErrUnsupported
}

func notifyDesktopInstance(string) error {
	return processlifecycle.ErrUnsupported
}
