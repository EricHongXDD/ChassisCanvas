//go:build linux

package edgedisplay

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type ptyEndpoint struct {
	*os.File
	swap     endpointSwap
	lockFile *os.File

	closeOnce   sync.Once
	closeErr    error
	restoreOnce sync.Once
	restoreErr  error

	hostTermiosMu       sync.Mutex
	hostOriginalTermios *unix.Termios
}

func openPlatformEndpoint(options Options) (platformEndpoint, error) {
	if !filepath.IsAbs(options.DevicePath) || !filepath.IsAbs(options.BackupPath) || !filepath.IsAbs(options.LockPath) {
		return nil, errors.New("设备节点、备份和锁路径必须是绝对路径")
	}
	if filepath.Dir(options.DevicePath) != filepath.Dir(options.BackupPath) {
		return nil, errors.New("设备节点与备份必须位于同一目录，以保证原子切换")
	}
	lockFile, err := os.OpenFile(options.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开屏端显示代理锁: %w", err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("已有屏端显示代理占用 %s: %w", options.DevicePath, err)
	}
	if err := recoverInterruptedSwap(osPathOperations{}, options.DevicePath, options.BackupPath); err != nil {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
		return nil, err
	}
	if info, err := os.Lstat(options.DevicePath); err == nil {
		if info.Mode()&fs.ModeDevice == 0 || info.Mode()&fs.ModeCharDevice == 0 || info.Mode()&fs.ModeSymlink != 0 {
			_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
			_ = lockFile.Close()
			return nil, fmt.Errorf("%s 不是可安全接管的字符设备节点", options.DevicePath)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
		return nil, fmt.Errorf("检查 %s: %w", options.DevicePath, err)
	}

	master, slavePath, err := openPTY()
	if err != nil {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
		return nil, err
	}
	temporary := fmt.Sprintf("%s.casescreen-new-%d-%d", options.DevicePath, os.Getpid(), time.Now().UnixNano())
	endpoint := &ptyEndpoint{
		File: master, lockFile: lockFile,
		swap: endpointSwap{
			ops: osPathOperations{}, devicePath: options.DevicePath, backupPath: options.BackupPath,
			temporary: temporary, replacement: slavePath,
		},
	}
	if err := endpoint.swap.install(); err != nil {
		_ = endpoint.Close()
		_ = endpoint.releaseLock()
		return nil, err
	}
	return endpoint, nil
}

func openPTY() (*os.File, string, error) {
	fileDescriptor, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("打开 /dev/ptmx: %w", err)
	}
	// os.File 只有在接管一个非阻塞描述符时才会把它注册到运行时轮询器；
	// ACK 超时和上下文取消依赖该轮询器，不能让 Read 永久卡住清理流程。
	if err := unix.SetNonblock(fileDescriptor, true); err != nil {
		_ = unix.Close(fileDescriptor)
		return nil, "", fmt.Errorf("设置 PTY 主端非阻塞模式: %w", err)
	}
	master := os.NewFile(uintptr(fileDescriptor), "/dev/ptmx")
	cleanup := func(cause error) (*os.File, string, error) {
		_ = master.Close()
		return nil, "", cause
	}
	if err := unix.IoctlSetPointerInt(fileDescriptor, unix.TIOCSPTLCK, 0); err != nil {
		return cleanup(fmt.Errorf("解锁 PTY 从端: %w", err))
	}
	index, err := unix.IoctlGetInt(fileDescriptor, unix.TIOCGPTN)
	if err != nil {
		return cleanup(fmt.Errorf("读取 PTY 编号: %w", err))
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", index)
	if err := configureRawPTY(slavePath); err != nil {
		return cleanup(err)
	}
	return master, slavePath, nil
}

func configureRawPTY(path string) error {
	fileDescriptor, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("打开 PTY 从端 %s: %w", path, err)
	}
	defer unix.Close(fileDescriptor)
	if err := configureRawFD(fileDescriptor); err != nil {
		return fmt.Errorf("设置 PTY 原始模式: %w", err)
	}
	return nil
}

// configureRawFD 关闭 tty 行规程对二进制协议的缓存、回显和换行转换。
// PTY 从端与真实 USB gadget 都必须调用；只设置 PTY 无法影响备份后的 ttyGS0。
func configureRawFD(fileDescriptor int) error {
	termios, err := unix.IoctlGetTermios(fileDescriptor, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("读取 tty 终端参数: %w", err)
	}
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF | unix.IXANY
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag &^= unix.CSIZE | unix.PARENB
	termios.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fileDescriptor, unix.TCSETS, termios); err != nil {
		return fmt.Errorf("写入 tty 终端参数: %w", err)
	}
	return nil
}

func (endpoint *ptyEndpoint) Close() error {
	endpoint.closeOnce.Do(func() {
		if endpoint.File != nil {
			endpoint.closeErr = endpoint.File.Close()
		}
	})
	return endpoint.closeErr
}

// HostOpener 返回真实 USB gadget 的可重连入口。原始节点不存在时，代理仍可
// 仅提供屏端本地注入，但不会虚构电脑端连接。
func (endpoint *ptyEndpoint) HostOpener() hostPortOpener {
	if !endpoint.swap.hadOriginal {
		return nil
	}
	path := endpoint.swap.backupPath
	return func() (io.ReadWriteCloser, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("检查真实 USB gadget %s: %w", path, err)
		}
		if info.Mode()&fs.ModeDevice == 0 || info.Mode()&fs.ModeCharDevice == 0 || info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("真实 USB gadget %s 已不是字符设备节点", path)
		}
		fileDescriptor, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, fmt.Errorf("打开真实 USB gadget %s: %w", path, err)
		}
		if err := unix.SetNonblock(fileDescriptor, true); err != nil {
			_ = unix.Close(fileDescriptor)
			return nil, fmt.Errorf("设置真实 USB gadget 非阻塞模式: %w", err)
		}
		if err := endpoint.captureHostTermios(fileDescriptor); err != nil {
			_ = unix.Close(fileDescriptor)
			return nil, fmt.Errorf("保存真实 USB gadget 终端参数: %w", err)
		}
		if err := configureRawFD(fileDescriptor); err != nil {
			_ = unix.Close(fileDescriptor)
			return nil, fmt.Errorf("设置真实 USB gadget 原始模式: %w", err)
		}
		return os.NewFile(uintptr(fileDescriptor), path), nil
	}
}

func (endpoint *ptyEndpoint) captureHostTermios(fileDescriptor int) error {
	endpoint.hostTermiosMu.Lock()
	defer endpoint.hostTermiosMu.Unlock()
	if endpoint.hostOriginalTermios != nil {
		return nil
	}
	termios, err := unix.IoctlGetTermios(fileDescriptor, unix.TCGETS)
	if err != nil {
		return err
	}
	copyOfTermios := *termios
	endpoint.hostOriginalTermios = &copyOfTermios
	return nil
}

func (endpoint *ptyEndpoint) restoreHostTermios() error {
	endpoint.hostTermiosMu.Lock()
	if endpoint.hostOriginalTermios == nil {
		endpoint.hostTermiosMu.Unlock()
		return nil
	}
	copyOfTermios := *endpoint.hostOriginalTermios
	endpoint.hostTermiosMu.Unlock()

	fileDescriptor, err := unix.Open(
		endpoint.swap.backupPath,
		unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("重新打开真实 USB gadget 以恢复终端参数: %w", err)
	}
	setErr := unix.IoctlSetTermios(fileDescriptor, unix.TCSETS, &copyOfTermios)
	closeErr := unix.Close(fileDescriptor)
	if setErr == nil {
		endpoint.hostTermiosMu.Lock()
		endpoint.hostOriginalTermios = nil
		endpoint.hostTermiosMu.Unlock()
	}
	return errors.Join(setErr, closeErr)
}

func (endpoint *ptyEndpoint) Restore() error {
	endpoint.restoreOnce.Do(func() {
		endpoint.restoreErr = errors.Join(endpoint.restoreHostTermios(), endpoint.swap.restore(), endpoint.releaseLock())
	})
	return endpoint.restoreErr
}

func (endpoint *ptyEndpoint) releaseLock() error {
	if endpoint.lockFile == nil {
		return nil
	}
	unlockErr := unix.Flock(int(endpoint.lockFile.Fd()), unix.LOCK_UN)
	closeErr := endpoint.lockFile.Close()
	endpoint.lockFile = nil
	return errors.Join(unlockErr, closeErr)
}

func configureChildProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalChild(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
}

func killChild(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
