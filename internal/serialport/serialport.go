package serialport

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var comNamePattern = regexp.MustCompile(`(?i)^COM([1-9][0-9]*)$`)

type Options struct {
	BaudRate    uint32
	ReadTimeout time.Duration
}

// ErrorKind 用稳定分类区分“被占用”“未枚举”和“传输中掉线”。
// Windows 的原始错误文本会随系统语言变化，服务层不应依赖它做恢复决策。
type ErrorKind string

const (
	ErrorKindBusy         ErrorKind = "busy"
	ErrorKindNotPresent   ErrorKind = "not_present"
	ErrorKindDisconnected ErrorKind = "disconnected"
	ErrorKindTimeout      ErrorKind = "timeout"
	ErrorKindIO           ErrorKind = "io"
)

// TransportError 保留串口、操作阶段和原始 Windows 错误，便于区分
// 硬件从 USB 总线消失与普通的 ACK 超时。
type TransportError struct {
	Port      string
	Operation string
	Kind      ErrorKind
	Err       error
}

func (err *TransportError) Error() string {
	if err == nil {
		return "串口传输错误"
	}
	message := "串口"
	if strings.TrimSpace(err.Port) != "" {
		message += " " + strings.TrimSpace(err.Port)
	}
	if strings.TrimSpace(err.Operation) != "" {
		message += " " + strings.TrimSpace(err.Operation)
	}
	switch err.Kind {
	case ErrorKindBusy:
		message += "被其他程序占用（请退出原厂 PCMonitor）"
	case ErrorKindNotPresent:
		message += "尚未在 Windows 中枚举"
	case ErrorKindDisconnected:
		message += "传输中从 USB 总线断开"
	case ErrorKindTimeout:
		message += "超时"
	default:
		message += "失败"
	}
	if err.Err != nil {
		message += ": " + err.Err.Error()
	}
	return message
}

func (err *TransportError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func (err *TransportError) Timeout() bool {
	return err != nil && err.Kind == ErrorKindTimeout
}

func (err *TransportError) Temporary() bool {
	return err != nil && (err.Kind == ErrorKindTimeout || err.Kind == ErrorKindNotPresent || err.Kind == ErrorKindDisconnected)
}

// KindOf 返回错误链中的串口分类。
func KindOf(err error) (ErrorKind, bool) {
	var transportError *TransportError
	if !errors.As(err, &transportError) || transportError == nil {
		return "", false
	}
	return transportError.Kind, true
}

func DefaultOptions() Options {
	return Options{BaudRate: 115200, ReadTimeout: 3 * time.Second}
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	matches := comNamePattern.FindStringSubmatch(name)
	if matches == nil {
		return "", fmt.Errorf("串口名无效: %q", name)
	}
	number, err := strconv.Atoi(matches[1])
	if err != nil || number <= 0 || number > 4096 {
		return "", fmt.Errorf("串口编号无效: %q", name)
	}
	return fmt.Sprintf(`\\.\COM%d`, number), nil
}
