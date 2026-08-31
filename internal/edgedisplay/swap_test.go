package edgedisplay

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

type fakeEntry struct {
	mode   fs.FileMode
	target string
}

type fakePathOperations struct {
	entries    map[string]fakeEntry
	symlinkErr error
	renameErr  map[string]error
}

func (operations *fakePathOperations) Lstat(path string) (fs.FileInfo, error) {
	entry, exists := operations.entries[path]
	if !exists {
		return nil, fs.ErrNotExist
	}
	return fakeFileInfo{name: filepath.Base(path), mode: entry.mode}, nil
}

func (operations *fakePathOperations) Readlink(path string) (string, error) {
	entry, exists := operations.entries[path]
	if !exists {
		return "", fs.ErrNotExist
	}
	if entry.mode&fs.ModeSymlink == 0 {
		return "", errors.New("不是符号链接")
	}
	return entry.target, nil
}

func (operations *fakePathOperations) Rename(oldPath, newPath string) error {
	if err := operations.renameErr[oldPath+"->"+newPath]; err != nil {
		return err
	}
	entry, exists := operations.entries[oldPath]
	if !exists {
		return fs.ErrNotExist
	}
	operations.entries[newPath] = entry
	delete(operations.entries, oldPath)
	return nil
}

func (operations *fakePathOperations) Symlink(target, path string) error {
	if operations.symlinkErr != nil {
		return operations.symlinkErr
	}
	if _, exists := operations.entries[path]; exists {
		return fs.ErrExist
	}
	operations.entries[path] = fakeEntry{mode: fs.ModeSymlink, target: target}
	return nil
}

func (operations *fakePathOperations) Remove(path string) error {
	if _, exists := operations.entries[path]; !exists {
		return fs.ErrNotExist
	}
	delete(operations.entries, path)
	return nil
}

type fakeFileInfo struct {
	name string
	mode fs.FileMode
}

func (info fakeFileInfo) Name() string       { return info.name }
func (info fakeFileInfo) Size() int64        { return 0 }
func (info fakeFileInfo) Mode() fs.FileMode  { return info.mode }
func (info fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (info fakeFileInfo) IsDir() bool        { return false }
func (info fakeFileInfo) Sys() any           { return nil }

func TestEndpointSwapInstallAndRestoreOriginalDevice(t *testing.T) {
	operations := &fakePathOperations{entries: map[string]fakeEntry{
		"/dev/ttyGS0": {mode: fs.ModeDevice | 0o600},
	}}
	swap := endpointSwap{
		ops: operations, devicePath: "/dev/ttyGS0", backupPath: "/dev/ttyGS0.backup",
		temporary: "/dev/ttyGS0.new", replacement: "/dev/pts/7",
	}
	if err := swap.install(); err != nil {
		t.Fatal(err)
	}
	entry := operations.entries["/dev/ttyGS0"]
	if entry.mode&fs.ModeSymlink == 0 || entry.target != "/dev/pts/7" {
		t.Fatalf("未安装预期 PTY 链接: %#v", entry)
	}
	if _, exists := operations.entries["/dev/ttyGS0.backup"]; !exists {
		t.Fatal("原设备节点没有备份")
	}
	if err := swap.restore(); err != nil {
		t.Fatal(err)
	}
	if entry := operations.entries["/dev/ttyGS0"]; entry.mode&fs.ModeDevice == 0 {
		t.Fatalf("原设备节点没有恢复: %#v", entry)
	}
	if _, exists := operations.entries["/dev/ttyGS0.backup"]; exists {
		t.Fatal("恢复后不应残留设备节点备份")
	}
}

func TestEndpointSwapRollsBackWhenSymlinkCreationFails(t *testing.T) {
	operations := &fakePathOperations{
		entries:    map[string]fakeEntry{"/dev/ttyGS0": {mode: fs.ModeDevice}},
		symlinkErr: errors.New("模拟创建失败"),
	}
	swap := endpointSwap{
		ops: operations, devicePath: "/dev/ttyGS0", backupPath: "/dev/ttyGS0.backup",
		temporary: "/dev/ttyGS0.new", replacement: "/dev/pts/8",
	}
	if err := swap.install(); err == nil {
		t.Fatal("创建链接失败时应返回错误")
	}
	if entry := operations.entries["/dev/ttyGS0"]; entry.mode&fs.ModeDevice == 0 {
		t.Fatalf("失败后没有回滚原设备节点: %#v", entry)
	}
	if _, exists := operations.entries["/dev/ttyGS0.backup"]; exists {
		t.Fatal("失败回滚后不应留下备份")
	}
}

func TestEndpointSwapRefusesToOverwriteExternalReplacement(t *testing.T) {
	operations := &fakePathOperations{entries: map[string]fakeEntry{
		"/dev/ttyGS0": {mode: fs.ModeDevice},
	}}
	swap := endpointSwap{
		ops: operations, devicePath: "/dev/ttyGS0", backupPath: "/dev/ttyGS0.backup",
		temporary: "/dev/ttyGS0.new", replacement: "/dev/pts/9",
	}
	if err := swap.install(); err != nil {
		t.Fatal(err)
	}
	operations.entries["/dev/ttyGS0"] = fakeEntry{mode: fs.ModeDevice}
	if err := swap.restore(); err == nil {
		t.Fatal("外部替换节点后应拒绝覆盖")
	}
	if _, exists := operations.entries["/dev/ttyGS0.backup"]; !exists {
		t.Fatal("拒绝覆盖时必须保留可人工恢复的原节点")
	}
}

func TestEndpointSwapWorksWhenGadgetNodeIsAbsent(t *testing.T) {
	operations := &fakePathOperations{entries: map[string]fakeEntry{}}
	swap := endpointSwap{
		ops: operations, devicePath: "/dev/ttyGS0", backupPath: "/dev/ttyGS0.backup",
		temporary: "/dev/ttyGS0.new", replacement: "/dev/pts/10",
	}
	if err := swap.install(); err != nil {
		t.Fatal(err)
	}
	if err := swap.restore(); err != nil {
		t.Fatal(err)
	}
	if len(operations.entries) != 0 {
		t.Fatalf("原节点不存在时退出后应恢复为空: %#v", operations.entries)
	}
}

func TestRecoverInterruptedSwapRestoresStalePTY(t *testing.T) {
	operations := &fakePathOperations{entries: map[string]fakeEntry{
		"/dev/ttyGS0":        {mode: fs.ModeSymlink, target: "/dev/pts/11"},
		"/dev/ttyGS0.backup": {mode: fs.ModeDevice | fs.ModeCharDevice},
	}}
	if err := recoverInterruptedSwap(operations, "/dev/ttyGS0", "/dev/ttyGS0.backup"); err != nil {
		t.Fatal(err)
	}
	if entry := operations.entries["/dev/ttyGS0"]; entry.mode&fs.ModeCharDevice == 0 {
		t.Fatalf("没有恢复遗留设备节点: %#v", entry)
	}
	if _, exists := operations.entries["/dev/ttyGS0.backup"]; exists {
		t.Fatal("自动恢复后不应残留备份")
	}
}

func TestRecoverInterruptedSwapRefusesUnknownLink(t *testing.T) {
	operations := &fakePathOperations{entries: map[string]fakeEntry{
		"/dev/ttyGS0":        {mode: fs.ModeSymlink, target: "/tmp/not-ours"},
		"/dev/ttyGS0.backup": {mode: fs.ModeDevice | fs.ModeCharDevice},
	}}
	if err := recoverInterruptedSwap(operations, "/dev/ttyGS0", "/dev/ttyGS0.backup"); err == nil {
		t.Fatal("未知链接不应被自动覆盖")
	}
	if operations.entries["/dev/ttyGS0"].target != "/tmp/not-ours" {
		t.Fatal("拒绝恢复时不应改变外部链接")
	}
}
