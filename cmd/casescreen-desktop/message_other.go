//go:build !windows

package main

import (
	"fmt"
	"os"
)

func showError(err error) {
	fmt.Fprintln(os.Stderr, "CaseScreenDesktop 启动失败:", err)
}
