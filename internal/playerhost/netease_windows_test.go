//go:build windows && casescreen_legacy_netease_cdp

package playerhost

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestValidateNeteaseExecutablePathAcceptsOnlyExistingCloudMusicExe(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "cloudmusic.exe")
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	actual, err := validateNeteaseExecutablePath(`"` + path + `"`)
	if err != nil || !strings.EqualFold(actual, path) {
		t.Fatalf("合法 App Paths 可执行文件被拒绝: path=%q err=%v", actual, err)
	}
	other := filepath.Join(directory, "other.exe")
	if err := os.WriteFile(other, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]string{
		"wrong_name": other,
		"directory":  directory,
		"relative":   `cloudmusic.exe`,
		"missing":    filepath.Join(directory, "missing", "cloudmusic.exe"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateNeteaseExecutablePath(invalid); err == nil {
				t.Fatal("无效 App Paths 目标被接受")
			}
		})
	}
}

func TestBuildNeteaseCRTHandleBlockMapsCEF91FD3AndFD4(t *testing.T) {
	block, err := buildNeteaseCRTHandleBlock(8, windows.Handle(0x11223344), windows.Handle(0x55667788))
	if err != nil {
		t.Fatal(err)
	}
	if len(block) != 49 || binary.LittleEndian.Uint32(block[:4]) != 5 {
		t.Fatalf("x64 CRT 继承表尺寸或数量错误: len=%d count=%d", len(block), binary.LittleEndian.Uint32(block[:4]))
	}
	wantedFlags := []byte{0, 0, 0, neteaseCRTOpenPipeFlags, neteaseCRTOpenPipeFlags}
	if string(block[4:9]) != string(wantedFlags) {
		t.Fatalf("CRT fd 标志错误: got=%x want=%x", block[4:9], wantedFlags)
	}
	handles := make([]uint64, 5)
	for index := range handles {
		offset := 9 + index*8
		handles[index] = binary.LittleEndian.Uint64(block[offset : offset+8])
	}
	if handles[0] != ^uint64(0) || handles[1] != ^uint64(0) || handles[2] != ^uint64(0) ||
		handles[3] != 0x11223344 || handles[4] != 0x55667788 {
		t.Fatalf("CEF91 fd3/fd4 映射错误: %x", handles)
	}
}

func TestNeteaseStartupInfoLayoutMatchesWindowsSDK(t *testing.T) {
	if unsafe.Sizeof(neteaseStartupInfo{}) != unsafe.Sizeof(windows.StartupInfo{}) ||
		unsafe.Sizeof(neteaseStartupInfoEx{}) != unsafe.Sizeof(windows.StartupInfoEx{}) {
		t.Fatalf("自定义 STARTUPINFO 布局与 x/sys 不一致: got=%d/%d want=%d/%d",
			unsafe.Sizeof(neteaseStartupInfo{}), unsafe.Sizeof(neteaseStartupInfoEx{}),
			unsafe.Sizeof(windows.StartupInfo{}), unsafe.Sizeof(windows.StartupInfoEx{}))
	}
	if unsafe.Sizeof(uintptr(0)) == 8 {
		if unsafe.Sizeof(neteaseStartupInfo{}) != 104 || unsafe.Sizeof(neteaseStartupInfoEx{}) != 112 ||
			unsafe.Offsetof(neteaseStartupInfo{}.CbReserved2) != 66 ||
			unsafe.Offsetof(neteaseStartupInfo{}.Reserved2) != 72 ||
			unsafe.Offsetof(neteaseStartupInfoEx{}.ProcThreadAttributeList) != 104 {
			t.Fatalf("amd64 STARTUPINFO 关键偏移错误")
		}
	}
	if neteaseRemoteDebuggingPipeArgument != "--remote-debugging-pipe" ||
		strings.Contains(neteaseRemoteDebuggingPipeArgument, "io-pipes") ||
		strings.Contains(neteaseRemoteDebuggingPipeArgument, "port") {
		t.Fatalf("CEF91 启动参数不兼容或重新引入端口: %q", neteaseRemoteDebuggingPipeArgument)
	}
}

func TestRequireDisposableWindowsForNeteaseVerificationIsExplicit(t *testing.T) {
	t.Setenv(NeteaseDisposableWindowsEnvironment, "")
	if err := RequireDisposableWindowsForNeteaseVerification(); err == nil {
		t.Fatal("普通环境意外通过了首次 CEF91 防误触门禁")
	}
	t.Setenv(NeteaseDisposableWindowsEnvironment, NeteaseDisposableWindowsAcknowledgement)
	if err := RequireDisposableWindowsForNeteaseVerification(); err != nil {
		t.Fatalf("精确的一次性 Windows 确认被拒绝: %v", err)
	}
	t.Setenv(NeteaseDisposableWindowsEnvironment, NeteaseDisposableWindowsAcknowledgement+"_TYPO")
	if err := RequireDisposableWindowsForNeteaseVerification(); err == nil {
		t.Fatal("近似确认值不应通过防误触门禁")
	}
}

// TestNeteasePrivatePipeCEF91Live 是显式门控的本机真机烟测。运行前必须由
// 操作者完全退出网易云（包括托盘进程）。测试不会调用终止接口；断管后还会
// 用相同 PID 门禁证明客户端没有因 CaseScreen 关闭管道而退出。
func TestNeteasePrivatePipeCEF91Live(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_PIPE_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_PIPE_LIVE=1 后运行 CEF91 私有管道真机烟测")
	}
	if runtime.GOARCH != "amd64" {
		t.Fatalf("当前真机门禁只验收 amd64，实际为 %s", runtime.GOARCH)
	}
	if err := RequireDisposableWindowsForNeteaseVerification(); err != nil {
		t.Fatal(err)
	}
	running, err := neteaseProcessRunning()
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("真机烟测前必须完全退出网易云音乐；测试不会自动关闭或重启客户端")
	}
	// Windows Known Folder 不受 APPDATA 等环境变量可靠隔离；因此测试只在
	// 外部已经可销毁的 Windows 环境中使用与生产完全相同的 host。
	host := NewNeteaseHost()
	var trackedProcessIDs []uint32
	t.Cleanup(func() {
		_ = host.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	status := host.StartAndConnect(ctx)
	cancel()
	if !status.Connected || status.Channel != NeteasePrivatePipeChannel {
		t.Fatalf("CEF91 私有管道未连接: %+v", status)
	}
	t.Log("一次性 Windows 环境中的网易云已启动；请在 60 秒内播放或暂停一首歌曲")
	deadline := time.Now().Add(time.Minute)
	for {
		readContext, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
		payload, readErr := host.ReadPlayback(readContext)
		readCancel()
		if readErr != nil || len(payload) == 0 {
			t.Fatalf("CEF91 固定表达式读取失败: payload=%s err=%v", payload, readErr)
		}
		var snapshot struct {
			Empty  bool   `json:"empty"`
			Stable bool   `json:"stable"`
			Status string `json:"status"`
			Info   *struct {
				ResourceType string  `json:"resourceType"`
				ResourceID   string  `json:"resourceId"`
				Duration     float64 `json:"duration"`
			} `json:"info"`
			Progress *struct {
				Current  float64 `json:"current"`
				Duration float64 `json:"duration"`
			} `json:"progress"`
		}
		if json.Unmarshal(payload, &snapshot) != nil {
			t.Fatalf("CEF91 返回的固定快照不是 JSON: %s", payload)
		}
		if snapshot.Empty || !snapshot.Stable || snapshot.Info == nil || snapshot.Progress == nil ||
			snapshot.Status == "Stop" || snapshot.Status == "End" {
			if !time.Now().Before(deadline) {
				t.Fatalf("60 秒内没有取得 Playing/Pause 稳定歌曲快照: %s", payload)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if snapshot.Info.ResourceType != "track" {
			t.Fatalf("真机烟测当前资源不是歌曲: %s", payload)
		}
		if _, err := strconv.ParseUint(snapshot.Info.ResourceID, 10, 64); err != nil ||
			snapshot.Info.Duration <= 0 || snapshot.Progress.Duration <= 0 ||
			math.Abs(snapshot.Info.Duration-snapshot.Progress.Duration) > 1 ||
			snapshot.Progress.Current < 0 || snapshot.Progress.Current > snapshot.Info.Duration/1000+2 {
			t.Fatalf("真机烟测 ID 或毫秒/秒时间轴无效: %s", payload)
		}
		if snapshot.Status != "Playing" && snapshot.Status != "Pause" {
			t.Fatalf("真机烟测需要 Playing/Pause 状态，实际为 %q", snapshot.Status)
		}
		break
	}
	trackedProcessIDs, err = NeteaseProcessIDsForVerification()
	if err != nil || len(trackedProcessIDs) == 0 {
		t.Fatalf("无法记录断管前的 cloudmusic.exe PID: pids=%v err=%v", trackedProcessIDs, err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	disconnectContext, disconnectCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer disconnectCancel()
	if _, err := host.ReadPlayback(disconnectContext); err == nil {
		t.Fatal("CaseScreen 关闭私有管道后仍可读取，断管边界失效")
	}
	time.Sleep(2 * time.Second)
	remainingProcessIDs, err := NeteaseProcessIDsForVerification()
	if err != nil {
		t.Fatal(err)
	}
	if !neteasePIDSetIntersects(trackedProcessIDs, remainingProcessIDs) {
		t.Fatalf("关闭私有管道后断管前的 cloudmusic.exe PID 全部消失: before=%v after=%v", trackedProcessIDs, remainingProcessIDs)
	}
	t.Logf("断管后同一 cloudmusic.exe 进程仍存活: before=%v after=%v", trackedProcessIDs, remainingProcessIDs)
	t.Log("请现在从网易云界面正常完全退出隔离客户端；测试不会结束进程")
	exited, waitErr := waitForNeteaseProcessesToExit(90 * time.Second)
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if !exited {
		t.Fatal("90 秒内未观察到一次性 Windows 环境中的客户端正常退出")
	}
}

func neteasePIDSetIntersects(left, right []uint32) bool {
	seen := make(map[uint32]struct{}, len(left))
	for _, processID := range left {
		if processID != 0 {
			seen[processID] = struct{}{}
		}
	}
	for _, processID := range right {
		if _, ok := seen[processID]; ok {
			return true
		}
	}
	return false
}

func waitForNeteaseProcessesToExit(timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		currentProcessIDs, err := NeteaseProcessIDsForVerification()
		if err != nil {
			return false, err
		}
		if len(currentProcessIDs) == 0 {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}
