//go:build !windows

package serialport

import "errors"

type Port struct{}

func Open(_ string, _ Options) (*Port, error) {
	return nil, errors.New("当前系统尚未实现串口驱动")
}
