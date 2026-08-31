//go:build windows

package lyrics

import (
	"context"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                   = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows          = user32.NewProc("EnumWindows")
	procIsWindowVisible      = user32.NewProc("IsWindowVisible")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procGetWindowThreadPID   = user32.NewProc("GetWindowThreadProcessId")
	procGetForegroundWindow  = user32.NewProc("GetForegroundWindow")
)

type windowSource struct{}

type playerWindow struct {
	handle uintptr
	source string
	title  string
}

// NewNowPlayingSource 只组合 Windows 媒体会话和 CaseScreen 内置的
// 只读进程适配器，不接受外部播放信息来源。
func NewNowPlayingSource() NowPlayingSource {
	fallback := &fallbackNowPlayingSource{
		smtc:   &smtcSource{},
		window: &windowSource{},
	}
	return &windowsNowPlayingSource{
		netease:  newNeteaseNowPlayingSource(defaultNeteaseNativeIDSource),
		fallback: fallback,
	}
}

func (*windowSource) Current(ctx context.Context) (NowPlaying, error) {
	if err := ctx.Err(); err != nil {
		return NowPlaying{}, err
	}
	foreground, _, _ := procGetForegroundWindow.Call()
	var candidates []playerWindow
	callback := windows.NewCallback(func(window uintptr, _ uintptr) uintptr {
		if ctx.Err() != nil {
			return 0
		}
		visible, _, _ := procIsWindowVisible.Call(window)
		if visible == 0 {
			return 1
		}
		length, _, _ := procGetWindowTextLengthW.Call(window)
		if length == 0 || length > 2048 {
			return 1
		}
		var processID uint32
		procGetWindowThreadPID.Call(window, uintptr(unsafe.Pointer(&processID)))
		source := sourceForProcess(processID)
		if source == "" {
			return 1
		}
		buffer := make([]uint16, int(length)+1)
		read, _, _ := procGetWindowTextW.Call(window, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if read == 0 {
			return 1
		}
		title := windows.UTF16ToString(buffer)
		if track := parseWindowTitle(source, title); track.Valid() {
			candidates = append(candidates, playerWindow{handle: window, source: source, title: title})
		}
		return 1
	})
	procEnumWindows.Call(callback, 0)
	if len(candidates) == 0 {
		return NowPlaying{}, nil
	}
	selected := candidates[0]
	for _, candidate := range candidates {
		if candidate.handle == foreground {
			selected = candidate
			break
		}
	}
	return parseWindowTitle(selected.source, selected.title), nil
}

func sourceForProcess(processID uint32) string {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(process)
	buffer := make([]uint16, windows.MAX_PATH*4)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(process, 0, &buffer[0], &size) != nil {
		return ""
	}
	name := strings.ToLower(filepath.Base(windows.UTF16ToString(buffer[:size])))
	switch name {
	case "cloudmusic.exe":
		return "netease"
	case "qqmusic.exe":
		return "qq"
	case "kugou.exe":
		return "kugou"
	case "sogoumusic.exe", "sogou_music.exe":
		return "sogou"
	case "kwmusic.exe", "kuwomusic.exe":
		return "kuwo"
	default:
		return ""
	}
}
