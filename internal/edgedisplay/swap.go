package edgedisplay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// pathOperations 把设备节点切换逻辑与真实文件系统隔离，便于验证所有失败回滚分支。
type pathOperations interface {
	Lstat(string) (fs.FileInfo, error)
	Readlink(string) (string, error)
	Rename(string, string) error
	Symlink(string, string) error
	Remove(string) error
}

type osPathOperations struct{}

func (osPathOperations) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }
func (osPathOperations) Readlink(path string) (string, error)   { return os.Readlink(path) }
func (osPathOperations) Rename(oldPath, newPath string) error   { return os.Rename(oldPath, newPath) }
func (osPathOperations) Symlink(target, path string) error      { return os.Symlink(target, path) }
func (osPathOperations) Remove(path string) error               { return os.Remove(path) }

// endpointSwap 在原厂程序启动前，把 ttyGS0 暂时替换为 PTY 从端链接。
// 它不会覆盖已有备份，也不会在退出时删除不属于自己的节点。
type endpointSwap struct {
	ops         pathOperations
	devicePath  string
	backupPath  string
	temporary   string
	replacement string

	hadOriginal bool
	installed   bool
}

// recoverInterruptedSwap 只在进程锁已经独占后调用。它修复上次进程被
// SIGKILL 后遗留的“原节点备份 + PTY 链接”，但拒绝触碰其他类型的节点。
func recoverInterruptedSwap(operations pathOperations, devicePath, backupPath string) error {
	backupInfo, err := operations.Lstat(backupPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查遗留设备节点备份: %w", err)
	}
	if backupInfo.Mode()&fs.ModeDevice == 0 || backupInfo.Mode()&fs.ModeCharDevice == 0 {
		return fmt.Errorf("遗留备份 %s 不是字符设备，拒绝自动恢复", backupPath)
	}

	deviceInfo, err := operations.Lstat(devicePath)
	if errors.Is(err, fs.ErrNotExist) {
		if renameErr := operations.Rename(backupPath, devicePath); renameErr != nil {
			return fmt.Errorf("恢复遗留原设备节点: %w", renameErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查遗留 PTY 链接: %w", err)
	}
	if deviceInfo.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("%s 与遗留备份同时存在且不是 PTY 链接，拒绝自动覆盖", devicePath)
	}
	target, err := operations.Readlink(devicePath)
	if err != nil {
		return fmt.Errorf("读取遗留 PTY 链接: %w", err)
	}
	if !strings.HasPrefix(target, "/dev/pts/") {
		return fmt.Errorf("%s 指向非 PTY 路径 %s，拒绝自动覆盖", devicePath, target)
	}
	if err := operations.Remove(devicePath); err != nil {
		return fmt.Errorf("移除遗留 PTY 链接: %w", err)
	}
	if err := operations.Rename(backupPath, devicePath); err != nil {
		return fmt.Errorf("恢复遗留原设备节点: %w", err)
	}
	return nil
}

func (swap *endpointSwap) install() error {
	if swap.ops == nil {
		swap.ops = osPathOperations{}
	}
	if swap.devicePath == "" || swap.backupPath == "" || swap.temporary == "" || swap.replacement == "" {
		return errors.New("PTY 设备节点切换参数不完整")
	}
	if swap.devicePath == swap.backupPath || swap.devicePath == swap.temporary || swap.backupPath == swap.temporary {
		return errors.New("PTY 设备节点、备份和临时路径必须不同")
	}
	if _, err := swap.ops.Lstat(swap.backupPath); err == nil {
		return fmt.Errorf("发现未恢复的设备节点备份 %s，请先恢复后再启动", swap.backupPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查设备节点备份: %w", err)
	}
	if _, err := swap.ops.Lstat(swap.temporary); err == nil {
		return fmt.Errorf("临时设备链接已存在: %s", swap.temporary)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查临时设备链接: %w", err)
	}

	if info, err := swap.ops.Lstat(swap.devicePath); err == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("拒绝覆盖已有符号链接 %s", swap.devicePath)
		}
		if err := swap.ops.Rename(swap.devicePath, swap.backupPath); err != nil {
			return fmt.Errorf("备份原设备节点: %w", err)
		}
		swap.hadOriginal = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查原设备节点: %w", err)
	}

	if err := swap.ops.Symlink(swap.replacement, swap.temporary); err != nil {
		return errors.Join(fmt.Errorf("创建临时 PTY 链接: %w", err), swap.rollbackOriginal())
	}
	if err := swap.ops.Rename(swap.temporary, swap.devicePath); err != nil {
		removeErr := swap.ops.Remove(swap.temporary)
		return errors.Join(fmt.Errorf("安装 PTY 设备链接: %w", err), removeErr, swap.rollbackOriginal())
	}
	swap.installed = true
	return nil
}

func (swap *endpointSwap) restore() error {
	if !swap.installed {
		return nil
	}
	info, err := swap.ops.Lstat(swap.devicePath)
	switch {
	case err == nil:
		if info.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("设备节点 %s 已被外部替换；保留备份 %s，拒绝覆盖", swap.devicePath, swap.backupPath)
		}
		target, readErr := swap.ops.Readlink(swap.devicePath)
		if readErr != nil {
			return fmt.Errorf("读取 PTY 设备链接: %w", readErr)
		}
		if target != swap.replacement {
			return fmt.Errorf("设备链接 %s 已指向 %s；保留备份 %s，拒绝覆盖", swap.devicePath, target, swap.backupPath)
		}
		if removeErr := swap.ops.Remove(swap.devicePath); removeErr != nil {
			return fmt.Errorf("移除 PTY 设备链接: %w", removeErr)
		}
	case errors.Is(err, fs.ErrNotExist):
		// 链接已不存在时仍可继续恢复原设备节点。
	default:
		return fmt.Errorf("检查 PTY 设备链接: %w", err)
	}

	if swap.hadOriginal {
		if err := swap.ops.Rename(swap.backupPath, swap.devicePath); err != nil {
			return fmt.Errorf("恢复原设备节点 %s: %w", swap.devicePath, err)
		}
	}
	swap.installed = false
	return nil
}

func (swap *endpointSwap) rollbackOriginal() error {
	if !swap.hadOriginal {
		return nil
	}
	if err := swap.ops.Rename(swap.backupPath, swap.devicePath); err != nil {
		return fmt.Errorf("回滚原设备节点: %w", err)
	}
	swap.hadOriginal = false
	return nil
}
