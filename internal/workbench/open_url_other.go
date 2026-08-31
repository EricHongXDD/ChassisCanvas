//go:build !windows

package workbench

import (
	"os/exec"
	"runtime"
)

func openExternalURL(address string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", address).Start()
	}
	return exec.Command("xdg-open", address).Start()
}
