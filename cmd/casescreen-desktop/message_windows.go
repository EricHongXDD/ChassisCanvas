//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

func showError(err error) {
	user32 := syscall.NewLazyDLL("user32.dll")
	messageBox := user32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("CaseScreenDesktop")
	message, _ := syscall.UTF16PtrFromString(err.Error())
	_, _, _ = messageBox.Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x10)
}
