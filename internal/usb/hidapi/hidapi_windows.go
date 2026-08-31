//go:build windows

package hidapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type deviceInfo struct {
	path               *byte
	vendorID           uint16
	productID          uint16
	serialNumber       *uint16
	releaseNumber      uint16
	manufacturerString *uint16
	productString      *uint16
	usagePage          uint16
	usage              uint16
	interfaceNumber    int32
	next               *deviceInfo
}

type Transport struct {
	mu      sync.Mutex
	dll     *windows.LazyDLL
	device  uintptr
	closed  bool
	write   *windows.LazyProc
	read    *windows.LazyProc
	close   *windows.LazyProc
	errorOf *windows.LazyProc
}

func Open(dllPath string, vendorID, productID uint16, interfaceNumber int) (*Transport, error) {
	if dllPath == "" {
		return nil, errors.New("未配置 hidapi.dll 路径")
	}
	absolutePath, err := filepath.Abs(dllPath)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(absolutePath); statErr != nil || info.IsDir() {
		if statErr == nil {
			statErr = errors.New("路径是目录")
		}
		return nil, fmt.Errorf("hidapi 不可用: %w", statErr)
	}
	dll := windows.NewLazyDLL(absolutePath)
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("加载 hidapi: %w", err)
	}
	initialize := dll.NewProc("hid_init")
	enumerate := dll.NewProc("hid_enumerate")
	freeEnumeration := dll.NewProc("hid_free_enumeration")
	openPath := dll.NewProc("hid_open_path")
	setNonblocking := dll.NewProc("hid_set_nonblocking")
	write := dll.NewProc("hid_write")
	read := dll.NewProc("hid_read_timeout")
	closeDevice := dll.NewProc("hid_close")
	errorOf := dll.NewProc("hid_error")
	for _, procedure := range []*windows.LazyProc{
		initialize, enumerate, freeEnumeration, openPath, setNonblocking,
		write, read, closeDevice, errorOf,
	} {
		if err := procedure.Find(); err != nil {
			return nil, fmt.Errorf("hidapi 缺少入口 %s: %w", procedure.Name, err)
		}
	}
	if result, _, _ := initialize.Call(); int32(result) != 0 {
		return nil, errors.New("初始化 hidapi 失败")
	}

	headValue, _, _ := enumerate.Call(uintptr(vendorID), uintptr(productID))
	head := (*deviceInfo)(unsafe.Pointer(headValue))
	if head == nil {
		return nil, fmt.Errorf("未找到 HID 设备 %04X:%04X", vendorID, productID)
	}
	defer freeEnumeration.Call(headValue)
	var selectedPath string
	for current := head; current != nil; current = current.next {
		if current.interfaceNumber == int32(interfaceNumber) && current.path != nil {
			selectedPath = windows.BytePtrToString(current.path)
			break
		}
	}
	if selectedPath == "" {
		return nil, fmt.Errorf("HID 设备 %04X:%04X 缺少接口 %d", vendorID, productID, interfaceNumber)
	}
	pathPointer, err := windows.BytePtrFromString(selectedPath)
	if err != nil {
		return nil, fmt.Errorf("HID 设备路径无效: %w", err)
	}
	device, _, _ := openPath.Call(uintptr(unsafe.Pointer(pathPointer)))
	runtime.KeepAlive(pathPointer)
	if device == 0 {
		return nil, fmt.Errorf("打开 HID 接口 %d 失败", interfaceNumber)
	}
	if result, _, _ := setNonblocking.Call(device, 0); int32(result) != 0 {
		closeDevice.Call(device)
		return nil, errors.New("设置 HID 阻塞读取失败")
	}
	return &Transport{
		dll: dll, device: device, write: write, read: read,
		close: closeDevice, errorOf: errorOf,
	}, nil
}

func (transport *Transport) Write(ctx context.Context, data []byte, _ time.Duration) (int, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return 0, errors.New("HID 设备已关闭")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	result, _, _ := transport.write.Call(
		transport.device,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
	)
	runtime.KeepAlive(data)
	written := int(int32(result))
	if written < 0 {
		return 0, transport.errorLocked("HID 写入")
	}
	return written, nil
}

func (transport *Transport) Read(ctx context.Context, data []byte, timeout time.Duration) (int, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return 0, errors.New("HID 设备已关闭")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	milliseconds, err := boundedTimeout(ctx, timeout)
	if err != nil {
		return 0, err
	}
	result, _, _ := transport.read.Call(
		transport.device,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(milliseconds),
	)
	runtime.KeepAlive(data)
	read := int(int32(result))
	if read < 0 {
		return 0, transport.errorLocked("HID 读取")
	}
	if read == 0 {
		return 0, context.DeadlineExceeded
	}
	return read, nil
}

func (transport *Transport) Close() error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return nil
	}
	transport.closed = true
	transport.close.Call(transport.device)
	transport.device = 0
	return nil
}

func (transport *Transport) errorLocked(operation string) error {
	value, _, _ := transport.errorOf.Call(transport.device)
	if value == 0 {
		return errors.New(operation + "失败")
	}
	message := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(value)))
	if message == "" {
		return errors.New(operation + "失败")
	}
	return fmt.Errorf("%s失败: %s", operation, message)
}

func boundedTimeout(ctx context.Context, timeout time.Duration) (int32, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	milliseconds := timeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	if milliseconds > int64(^uint32(0)>>1) {
		milliseconds = int64(^uint32(0) >> 1)
	}
	return int32(milliseconds), nil
}
