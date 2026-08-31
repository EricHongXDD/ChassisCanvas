//go:build windows

package trayapp

import "os/exec"

func openURL(url string) error {
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
}
