//go:build windows

package workbench

import (
	"errors"
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	openFileExplorer     = 0x00080000
	openFileMustExist    = 0x00001000
	openPathMustExist    = 0x00000800
	openFileNoChangeDir  = 0x00000008
	openFileHideReadOnly = 0x00000004
)

var errFileDialogCancelled = errors.New("用户取消选择文件")

type windowsOpenFileName struct {
	structSize      uint32
	owner           windows.Handle
	instance        windows.Handle
	filter          *uint16
	customFilter    *uint16
	maxCustomFilter uint32
	filterIndex     uint32
	file            *uint16
	maxFile         uint32
	fileTitle       *uint16
	maxFileTitle    uint32
	initialDir      *uint16
	title           *uint16
	flags           uint32
	fileOffset      uint16
	fileExtension   uint16
	defaultExt      *uint16
	customData      uintptr
	hook            uintptr
	templateName    *uint16
	reserved        unsafe.Pointer
	reservedFlags   uint32
	flagsEx         uint32
}

var (
	commonDialogLibrary = windows.NewLazySystemDLL("comdlg32.dll")
	getOpenFileNameW    = commonDialogLibrary.NewProc("GetOpenFileNameW")
	commonDialogError   = commonDialogLibrary.NewProc("CommDlgExtendedError")
)

func chooseBackgroundFile() (string, error) {
	return chooseLocalFile("选择背景图片或视频", "图片和视频\x00*.jpg;*.jpeg;*.png;*.mp4;*.mov\x00图片\x00*.jpg;*.jpeg;*.png\x00视频\x00*.mp4;*.mov\x00所有文件\x00*.*\x00\x00")
}

func chooseImageFile() (string, error) {
	return chooseLocalFile("选择图片图层文件", "图片\x00*.jpg;*.jpeg;*.png\x00所有文件\x00*.*\x00\x00")
}

func chooseLyricsFile() (string, error) {
	return chooseLocalFile("选择 LRC 歌词文件", "LRC 歌词\x00*.lrc\x00文本文件\x00*.txt\x00所有文件\x00*.*\x00\x00")
}

func choosePlatformCookieFile() (string, error) {
	return chooseLocalFile("选择登录态导入文件 cookie.txt（导入后由主机加密复用）", "Cookie 文本\x00*.txt;*.cookies\x00所有文件\x00*.*\x00\x00")
}

func chooseLocalFile(title, filter string) (string, error) {
	fileBuffer := make([]uint16, 32768)
	filterBuffer := utf16.Encode([]rune(filter))
	titleBuffer, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return "", err
	}
	options := windowsOpenFileName{
		// Gio 的窗口消息循环与绘制协程不在同一 Win32 线程。把 Gio HWND
		// 直接设为 owner 会让 GetOpenFileNameW 在部分系统上互相等待；无 owner
		// 的系统对话框由 Windows 自己置前，是当前线程模型下的稳定做法。
		owner:       0,
		filter:      &filterBuffer[0],
		filterIndex: 1,
		file:        &fileBuffer[0],
		maxFile:     uint32(len(fileBuffer)),
		title:       titleBuffer,
		flags:       openFileExplorer | openFileMustExist | openPathMustExist | openFileNoChangeDir | openFileHideReadOnly,
	}
	options.structSize = uint32(unsafe.Sizeof(options))
	result, _, callErr := getOpenFileNameW.Call(uintptr(unsafe.Pointer(&options)))
	if result == 0 {
		extended, _, _ := commonDialogError.Call()
		if extended == 0 {
			return "", errFileDialogCancelled
		}
		return "", fmt.Errorf("打开文件选择器失败 (0x%x): %w", extended, callErr)
	}
	return syscall.UTF16ToString(fileBuffer), nil
}
