//go:build windows

package workbench

import "os/exec"

func openExternalURL(address string) error {
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", address).Start()
}
