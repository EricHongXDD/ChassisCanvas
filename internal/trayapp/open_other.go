//go:build !windows

package trayapp

import (
	"os/exec"
	"runtime"
)

func openURL(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}
