//go:build windows

package serialport

import (
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const (
	purgeTXAbort = 0x0001
	purgeRXAbort = 0x0002
	purgeTXClear = 0x0004
	purgeRXClear = 0x0008

	// 同步 COM Read 已经开始后，SetCommTimeouts 不保证立即重算本次等待。
	// 因此底层始终使用很短的物理读取片，逻辑截止时间仍由 readDeadline
	// 决定。取消后台容量查询后，歌词包最多等待一个读取片。
	serialReadPollInterval = 50 * time.Millisecond
)

type Port struct {
	handle windows.Handle
	name   string

	readMu       sync.Mutex
	writeMu      sync.Mutex
	stateMu      sync.RWMutex
	readDeadline time.Time
	closed       bool
}

func Open(name string, options Options) (*Port, error) {
	portName := strings.ToUpper(strings.TrimSpace(name))
	path, err := normalizeName(name)
	if err != nil {
		return nil, err
	}
	if options.BaudRate == 0 {
		options.BaudRate = 115200
	}
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &TransportError{Port: portName, Operation: "生成设备路径", Kind: ErrorKindIO, Err: err}
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, wrapWindowsError(portName, "打开", err)
	}
	port := &Port{handle: handle, name: portName}
	if err := port.configure(options); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, wrapWindowsError(portName, "配置", err)
	}
	return port, nil
}

func (port *Port) configure(options Options) error {
	if err := windows.SetupComm(port.handle, 64<<10, 64<<10); err != nil {
		return err
	}
	dcb := windows.DCB{
		DCBlength: uint32(28),
		BaudRate:  options.BaudRate,
		Flags:     1,
		ByteSize:  8,
		Parity:    0,
		StopBits:  0,
	}
	if err := windows.SetCommState(port.handle, &dcb); err != nil {
		return err
	}
	if err := windows.PurgeComm(port.handle, purgeTXAbort|purgeRXAbort|purgeTXClear|purgeRXClear); err != nil {
		return err
	}
	if options.ReadTimeout > 0 {
		return port.SetReadDeadline(time.Now().Add(options.ReadTimeout))
	}
	return port.SetReadDeadline(time.Time{})
}

func (port *Port) Read(buffer []byte) (int, error) {
	port.readMu.Lock()
	defer port.readMu.Unlock()
	if len(buffer) == 0 {
		return 0, nil
	}
	for {
		port.stateMu.RLock()
		if port.closed {
			port.stateMu.RUnlock()
			return 0, errors.New("串口已关闭")
		}
		handle := port.handle
		deadline := port.readDeadline
		port.stateMu.RUnlock()
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, &TransportError{Port: port.name, Operation: "读取", Kind: ErrorKindTimeout, Err: timeoutError{}}
		}
		read, err := windows.Read(handle, buffer)
		if err != nil {
			port.stateMu.RLock()
			closed := port.closed
			port.stateMu.RUnlock()
			if closed {
				return read, errors.New("串口已关闭")
			}
			return read, wrapWindowsError(port.name, "读取", err)
		}
		if read > 0 {
			return read, nil
		}
		// 0 字节只表示本次 50ms 物理读取片结束；无逻辑截止时间时
		// 继续等待，有截止时间时下一轮重新读取最新值。
	}
}

func (port *Port) Write(buffer []byte) (int, error) {
	port.writeMu.Lock()
	defer port.writeMu.Unlock()
	port.stateMu.RLock()
	if port.closed {
		port.stateMu.RUnlock()
		return 0, errors.New("串口已关闭")
	}
	handle := port.handle
	port.stateMu.RUnlock()
	written, err := windows.Write(handle, buffer)
	if err != nil {
		port.stateMu.RLock()
		closed := port.closed
		port.stateMu.RUnlock()
		if closed {
			return written, errors.New("串口已关闭")
		}
		return written, wrapWindowsError(port.name, "写入", err)
	}
	if err == nil && written == 0 && len(buffer) > 0 {
		return 0, io.ErrShortWrite
	}
	return written, nil
}

func (port *Port) SetReadDeadline(deadline time.Time) error {
	port.stateMu.Lock()
	defer port.stateMu.Unlock()
	if port.closed {
		return errors.New("串口已关闭")
	}
	physicalTimeout := serialReadPollInterval
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining < physicalTimeout {
			physicalTimeout = remaining
		}
	}
	if physicalTimeout <= 0 {
		physicalTimeout = time.Millisecond
	}
	milliseconds := physicalTimeout.Milliseconds()
	if milliseconds <= 0 {
		milliseconds = 1
	}
	timeouts := windows.CommTimeouts{
		ReadIntervalTimeout:         ^uint32(0),
		ReadTotalTimeoutMultiplier:  0,
		ReadTotalTimeoutConstant:    uint32(milliseconds),
		WriteTotalTimeoutMultiplier: 0,
		WriteTotalTimeoutConstant:   3000,
	}
	if err := windows.SetCommTimeouts(port.handle, &timeouts); err != nil {
		return wrapWindowsError(port.name, "设置读写超时", err)
	}
	port.readDeadline = deadline
	return nil
}

func (port *Port) Close() error {
	port.stateMu.Lock()
	if port.closed {
		port.stateMu.Unlock()
		return nil
	}
	port.closed = true
	handle := port.handle
	port.stateMu.Unlock()

	// 先取消内核中的同步 I/O，再等待读写者离开。这样断线恢复
	// 不会在一个已失效的 ReadFile/WriteFile 上卡住，也不会边写边 CloseHandle。
	if err := windows.CancelIoEx(handle, nil); err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) && !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		// 取消失败时仍需关闭句柄；最终 CloseHandle 的错误更能反映恢复结果。
	}
	port.readMu.Lock()
	port.writeMu.Lock()
	err := windows.CloseHandle(handle)
	port.writeMu.Unlock()
	port.readMu.Unlock()
	if err != nil {
		return wrapWindowsError(port.name, "关闭", err)
	}
	return nil
}

func wrapWindowsError(portName, operation string, err error) error {
	return &TransportError{Port: portName, Operation: operation, Kind: windowsErrorKind(err), Err: err}
}

func windowsErrorKind(err error) ErrorKind {
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return ErrorKindBusy
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND),
		errors.Is(err, windows.ERROR_NOT_READY), errors.Is(err, windows.ERROR_NO_SUCH_DEVICE):
		return ErrorKindNotPresent
	case errors.Is(err, windows.ERROR_DEVICE_NOT_CONNECTED), errors.Is(err, windows.ERROR_DEV_NOT_EXIST),
		errors.Is(err, windows.ERROR_DEVICE_REMOVED), errors.Is(err, windows.ERROR_IO_DEVICE),
		errors.Is(err, windows.ERROR_GEN_FAILURE), errors.Is(err, windows.ERROR_INVALID_HANDLE),
		errors.Is(err, windows.ERROR_OPERATION_ABORTED):
		return ErrorKindDisconnected
	case errors.Is(err, windows.ERROR_SEM_TIMEOUT):
		return ErrorKindTimeout
	default:
		return ErrorKindIO
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "串口读取超时" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
