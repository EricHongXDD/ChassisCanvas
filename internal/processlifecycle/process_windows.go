//go:build windows

package processlifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	tcpTableOwnerPIDListener = 3
	waitSliceMilliseconds    = 250
)

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

type windowsInstanceLock struct {
	mu     sync.Mutex
	handle windows.Handle
}

func (lock *windowsInstanceLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(lock.handle)
	lock.handle = 0
	return err
}

func acquireSingleInstance(name string, wait time.Duration) (InstanceLock, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("单实例锁名称为空")
	}
	wideName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("编码单实例锁名称: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		handle, createErr := windows.CreateMutex(nil, false, wideName)
		if createErr == nil {
			return &windowsInstanceLock{handle: handle}, nil
		}
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		if !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
			return nil, fmt.Errorf("创建单实例锁: %w", createErr)
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			return nil, ErrInstanceRunning
		}
		remaining := time.Until(deadline)
		pause := 50 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		if pause > 0 {
			time.Sleep(pause)
		}
	}
}

type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

func currentProcessID() int { return osProcessID() }

// osProcessID 单独封装，便于保持 Windows 文件只依赖一个底层 PID 来源。
func osProcessID() int { return int(windows.GetCurrentProcessId()) }

func Inspect(pid int) (Snapshot, error) {
	if pid <= 0 {
		return Snapshot{}, ErrProcessNotFound
	}
	entry, err := processEntry(uint32(pid))
	if err != nil {
		return Snapshot{}, err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return Snapshot{}, ErrProcessNotFound
		}
		return Snapshot{}, fmt.Errorf("打开进程 %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	executable, err := queryExecutable(handle)
	if err != nil {
		return Snapshot{}, fmt.Errorf("读取进程 %d 路径: %w", pid, err)
	}
	started, err := queryStarted(handle)
	if err != nil {
		return Snapshot{}, fmt.Errorf("读取进程 %d 启动时间: %w", pid, err)
	}
	commandLine, commandErr := queryCommandLine(handle)
	if commandErr != nil {
		// 路径和启动时间仍可用于安全核验，但无法读取参数时，上层不会把旧服务
		// 判为可自动清理的桌面托管进程。
		commandLine = ""
	}
	return Snapshot{
		PID:              pid,
		ParentPID:        int(entry.ParentProcessID),
		Executable:       executable,
		CommandLine:      commandLine,
		Started:          started,
		HasVisibleWindow: hasVisibleTopLevelWindow(uint32(pid)),
	}, nil
}

func hasVisibleTopLevelWindow(pid uint32) bool {
	found := false
	callback := syscall.NewCallback(func(window uintptr, _ uintptr) uintptr {
		var ownerPID uint32
		_, _ = windows.GetWindowThreadProcessId(windows.HWND(window), &ownerPID)
		if ownerPID == pid && windows.IsWindowVisible(windows.HWND(window)) {
			found = true
		}
		// 始终枚举完毕，避免以返回 false 停止时被 EnumWindows 包装成系统错误。
		return 1
	})
	_ = windows.EnumWindows(callback, nil)
	return found
}

func processEntry(pid uint32) (windows.ProcessEntry32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return windows.ProcessEntry32{}, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return windows.ProcessEntry32{}, err
	}
	for {
		if entry.ProcessID == pid {
			return entry, nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
				return windows.ProcessEntry32{}, ErrProcessNotFound
			}
			return windows.ProcessEntry32{}, err
		}
	}
}

func queryExecutable(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buffer[:size]), nil
}

func queryStarted(handle windows.Handle) (uint64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}

func queryCommandLine(handle windows.Handle) (string, error) {
	var size uint32
	err := windows.NtQueryInformationProcess(handle, windows.ProcessCommandLineInformation, nil, 0, &size)
	if err != windows.STATUS_INFO_LENGTH_MISMATCH || size < uint32(unsafe.Sizeof(windows.NTUnicodeString{})) {
		if err == nil {
			err = errors.New("命令行缓冲区长度无效")
		}
		return "", err
	}
	buffer := make([]byte, size)
	if err := windows.NtQueryInformationProcess(handle, windows.ProcessCommandLineInformation, unsafe.Pointer(&buffer[0]), size, &size); err != nil {
		return "", err
	}
	value := (*windows.NTUnicodeString)(unsafe.Pointer(&buffer[0]))
	if value.Buffer == nil || value.Length == 0 {
		return "", nil
	}
	return windows.UTF16ToString(unsafe.Slice(value.Buffer, int(value.Length/2))), nil
}

// TCPListenerPID 返回监听指定 IPv4 TCP 端口的进程。CaseScreen 强制绑定
// 127.0.0.1，因此无需扫描远程连接或猜测进程名。
func TCPListenerPID(port uint16) (int, error) {
	var size uint32
	result, _, _ := getExtendedTCPTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, windows.AF_INET, tcpTableOwnerPIDListener, 0)
	if syscall.Errno(result) != windows.ERROR_INSUFFICIENT_BUFFER || size < 4 {
		return 0, fmt.Errorf("读取 TCP 监听表长度: %w", syscall.Errno(result))
	}
	buffer := make([]byte, size)
	result, _, _ = getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0, windows.AF_INET, tcpTableOwnerPIDListener, 0)
	if result != 0 {
		return 0, fmt.Errorf("读取 TCP 监听表: %w", syscall.Errno(result))
	}
	count := *(*uint32)(unsafe.Pointer(&buffer[0]))
	rowSize := unsafe.Sizeof(mibTCPRowOwnerPID{})
	for index := uint32(0); index < count; index++ {
		offset := uintptr(4) + uintptr(index)*rowSize
		if offset+rowSize > uintptr(len(buffer)) {
			return 0, errors.New("TCP 监听表内容不完整")
		}
		row := (*mibTCPRowOwnerPID)(unsafe.Pointer(&buffer[offset]))
		if windows.Ntohs(uint16(row.LocalPort)) == port {
			return int(row.OwningPID), nil
		}
	}
	return 0, ErrProcessNotFound
}

func waitForExit(ctx context.Context, process Snapshot) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(process.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	if current, err := queryStarted(handle); err != nil || process.Started == 0 || current != process.Started {
		return nil
	}
	for {
		result, err := windows.WaitForSingleObject(handle, waitSliceMilliseconds)
		if err != nil {
			return err
		}
		switch result {
		case windows.WAIT_OBJECT_0:
			return nil
		case uint32(windows.WAIT_TIMEOUT):
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		default:
			return fmt.Errorf("等待进程 %d 返回未知状态 %d", process.PID, result)
		}
	}
}

func terminate(process Snapshot, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(process.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	started, err := queryStarted(handle)
	if err != nil {
		return err
	}
	if process.Started == 0 || started != process.Started {
		return errors.New("进程身份已变化，拒绝终止")
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return err
	}
	waitMilliseconds := uint32(timeout / time.Millisecond)
	if waitMilliseconds == 0 {
		waitMilliseconds = 1
	}
	result, err := windows.WaitForSingleObject(handle, waitMilliseconds)
	if err != nil {
		return err
	}
	if result != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("等待进程 %d 退出超时", process.PID)
	}
	return nil
}
