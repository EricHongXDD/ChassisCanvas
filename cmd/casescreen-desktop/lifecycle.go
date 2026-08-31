package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	localapi "casescreen/internal/api"
	"casescreen/internal/processlifecycle"
)

type existingServiceAction int

const (
	existingServiceReuse existingServiceAction = iota
	existingServiceReplace
	existingServiceActiveDesktop
	existingServiceProtected
)

type managedService struct {
	command *exec.Cmd
	done    chan struct{}
	baseURL string
	session serviceSession
	stop    sync.Once
	waitMu  sync.RWMutex
	waitErr error
}

// 函数变量让身份核验、端口二次确认与终止顺序可以在不操作
// 真实用户进程的情况下做完整接管测试。
var (
	lifecycleTCPListenerPID = processlifecycle.TCPListenerPID
	lifecycleInspectProcess = processlifecycle.Inspect
	lifecycleTerminate      = processlifecycle.Terminate
	lifecycleWaitForExit    = processlifecycle.WaitForExit
)

func newManagedService(command *exec.Cmd, baseURL string) *managedService {
	managed := &managedService{command: command, done: make(chan struct{}), baseURL: baseURL}
	go func() {
		err := command.Wait()
		managed.waitMu.Lock()
		managed.waitErr = err
		managed.waitMu.Unlock()
		close(managed.done)
	}()
	return managed
}

func (managed *managedService) Done() <-chan struct{} { return managed.done }

func (managed *managedService) Err() error {
	managed.waitMu.RLock()
	defer managed.waitMu.RUnlock()
	return managed.waitErr
}

func (managed *managedService) shutdown() {
	if managed == nil {
		return
	}
	managed.stop.Do(func() {
		select {
		case <-managed.done:
			return
		default:
		}
		// 只有会话仍属于刚才启动的同一 PID 时才请求优雅退出。端口若已被
		// 其他服务接管，直接跳过 HTTP，随后仅操作原 exec.Process 句柄。
		if current, state := querySession(managed.baseURL); state == sessionCompatible &&
			current.ProcessID == managed.command.Process.Pid && current.Token == managed.session.Token {
			request, _ := http.NewRequest(http.MethodPost, managed.baseURL+"/api/lifecycle/shutdown", bytes.NewReader([]byte("{}")))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CaseScreen-Token", current.Token)
			response, err := (&http.Client{Timeout: 800 * time.Millisecond}).Do(request)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
			}
		}
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		select {
		case <-managed.done:
			return
		case <-timer.C:
		}
		_ = managed.command.Process.Kill()
		select {
		case <-managed.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func servicePort(baseURL string) (uint16, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return 0, err
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return 0, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return 0, fmt.Errorf("服务端口无效: %q", portText)
	}
	return uint16(port), nil
}

func processArgument(commandLine, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, field := range strings.Fields(strings.ToLower(commandLine)) {
		field = strings.Trim(field, "\"'")
		if field == name || strings.HasPrefix(field, name+"=") {
			return true
		}
	}
	return false
}

func samePath(left, right string) bool {
	leftPath, leftErr := filepath.Abs(filepath.Clean(left))
	rightPath, rightErr := filepath.Abs(filepath.Clean(right))
	return leftErr == nil && rightErr == nil && strings.EqualFold(leftPath, rightPath)
}

func releaseDirectoryName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "casescreen-windows-x64" || strings.HasPrefix(name, "casescreen-windows-x64-")
}

// managedReleasePath 只接受当前目录或同一父目录中的 CaseScreen 发行目录。
// 即使另一个程序碰巧占用了 19321，也不会因为文件名相同就被结束。
func managedReleasePath(root, executable string) bool {
	if !strings.EqualFold(filepath.Base(executable), "CaseScreen.exe") {
		return false
	}
	candidateRoot := filepath.Dir(executable)
	if samePath(root, candidateRoot) {
		return true
	}
	if !samePath(filepath.Dir(root), filepath.Dir(candidateRoot)) ||
		!releaseDirectoryName(filepath.Base(root)) || !releaseDirectoryName(filepath.Base(candidateRoot)) {
		return false
	}
	// 旧目录必须保留完整的桌面/服务二进制对。这样即便任意目录下存在一个
	// 同名 CaseScreen.exe，也不会仅凭名称和端口被当作可清理旧发行版。
	desktopInfo, err := os.Stat(filepath.Join(candidateRoot, "CaseScreenDesktop.exe"))
	return err == nil && !desktopInfo.IsDir()
}

func desktopOwnerProcess(service, owner processlifecycle.Snapshot) bool {
	return owner.PID > 0 && strings.EqualFold(filepath.Base(owner.Executable), "CaseScreenDesktop.exe") &&
		samePath(filepath.Dir(service.Executable), filepath.Dir(owner.Executable))
}

func actionForManual(compatible bool) existingServiceAction {
	if compatible {
		// 正式 Desktop 不与同目录的手动 tray/headless 服务并存。
		// 启动时安全结束已核验身份的同版服务，再重建桌面托管
		// 服务，确保只有一个托盘，且“完全退出”不留无入口后台。
		return existingServiceReplace
	}
	return existingServiceProtected
}

// classifyExistingService 只把有可验证桌面归属的服务判为可替换。托盘模式、
// 手动 headless、路径不属于本发行系列或无法核验身份的进程一律不自动结束。
func classifyExistingService(root string, compatible bool, session serviceSession, service processlifecycle.Snapshot, owner *processlifecycle.Snapshot, ownerErr error) existingServiceAction {
	if session.ProcessID > 0 && session.ProcessID != service.PID {
		return existingServiceProtected
	}
	if !managedReleasePath(root, service.Executable) {
		return existingServiceProtected
	}
	switch strings.ToLower(strings.TrimSpace(session.LaunchMode)) {
	case "tray", "headless":
		return actionForManual(compatible)
	case "desktop":
		if session.OwnerProcessID <= 0 {
			return existingServiceReplace
		}
		if ownerErr == nil && owner != nil && desktopOwnerProcess(service, *owner) &&
			(session.OwnerProcessStartedAt == 0 || owner.Started == session.OwnerProcessStartedAt) {
			return existingServiceActiveDesktop
		}
		if ownerErr == nil || errors.Is(ownerErr, processlifecycle.ErrProcessNotFound) {
			return existingServiceReplace
		}
		return existingServiceProtected
	case "":
		// 旧版桌面固定以 -headless 启动。仍有同目录桌面父进程时不可抢占；
		// 父进程已经消失时才认定为历史遗留的无托盘后台。
		if !processArgument(service.CommandLine, "-headless") {
			return actionForManual(compatible)
		}
		if ownerErr == nil && owner != nil {
			if desktopOwnerProcess(service, *owner) {
				if owner.HasVisibleWindow {
					return existingServiceActiveDesktop
				}
				// beta.17 及更早版本关闭 Gio 窗口后，Desktop 和它启动的
				// headless 服务会一起成为无窗口孤儿；两者都允许安全替换。
				return existingServiceReplace
			}
			return actionForManual(compatible)
		}
		if errors.Is(ownerErr, processlifecycle.ErrProcessNotFound) {
			return existingServiceReplace
		}
		return existingServiceProtected
	default:
		return existingServiceProtected
	}
}

func inspectExistingService(root, baseURL string, compatible bool, session serviceSession) (existingServiceAction, processlifecycle.Snapshot, *processlifecycle.Snapshot, error) {
	port, err := servicePort(baseURL)
	if err != nil {
		return existingServiceProtected, processlifecycle.Snapshot{}, nil, err
	}
	pid, err := lifecycleTCPListenerPID(port)
	if err != nil {
		return existingServiceProtected, processlifecycle.Snapshot{}, nil, err
	}
	service, err := lifecycleInspectProcess(pid)
	if err != nil {
		return existingServiceProtected, processlifecycle.Snapshot{}, nil, err
	}
	ownerPID := service.ParentPID
	if strings.EqualFold(session.LaunchMode, "desktop") && session.OwnerProcessID > 0 {
		ownerPID = session.OwnerProcessID
	}
	var owner *processlifecycle.Snapshot
	ownerSnapshot, ownerErr := lifecycleInspectProcess(ownerPID)
	if ownerErr == nil {
		owner = &ownerSnapshot
	}
	return classifyExistingService(root, compatible, session, service, owner, ownerErr), service, owner, nil
}

func gracefulManualTakeover(baseURL string, session serviceSession, process processlifecycle.Snapshot) bool {
	mode := strings.ToLower(strings.TrimSpace(session.LaunchMode))
	if session.Token == "" || (mode != "tray" && mode != "headless") {
		return false
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/lifecycle/shutdown", bytes.NewReader([]byte("{}")))
	if err != nil {
		return false
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", session.Token)
	request.Header.Set(localapi.DesktopTakeoverHeader, localapi.DesktopTakeoverValue)
	response, err := (&http.Client{Timeout: 800 * time.Millisecond}).Do(request)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return lifecycleWaitForExit(waitContext, process) == nil
}

func replaceOrRejectExisting(root, baseURL string, state sessionProbe, session serviceSession) (bool, error) {
	if state == sessionUnavailable {
		// 无法区分“正在启动/短暂繁忙的当前版”和“旧版”时，
		// 绝不得根据桌面窗口已隐藏就执行终止。
		return false, errors.New("已有服务暂时无法核验版本与归属；已保留原进程")
	}
	compatible := state == sessionCompatible
	action, process, owner, err := inspectExistingService(root, baseURL, compatible, session)
	if err != nil {
		// 即使 HTTP 会话版本兼容，也不能在无法核验监听 PID/
		// 进程身份时静默复用。那样 Desktop 拿不到 managedService
		// 句柄，该服务日后退出会再次留下永久无 API 的空壳。
		return false, fmt.Errorf("无法核验本机已有 CaseScreen 服务的进程身份；已保留原进程: %w", err)
	}
	switch action {
	case existingServiceReuse:
		return false, nil
	case existingServiceActiveDesktop:
		return false, errors.New("已有 CaseScreen 工作台正在运行；请从系统托盘右键选择“完全退出”")
	case existingServiceProtected:
		return false, errors.New("检测到手动启动或无法确认归属的 CaseScreen 服务；请从其托盘或启动终端退出")
	case existingServiceReplace:
		port, portErr := servicePort(baseURL)
		if portErr != nil {
			return false, portErr
		}
		pid, pidErr := lifecycleTCPListenerPID(port)
		if pidErr != nil || pid != process.PID {
			return false, errors.New("后台监听进程已变化，已取消自动替换")
		}
		if !gracefulManualTakeover(baseURL, session, process) {
			if err := lifecycleTerminate(process, 3*time.Second); err != nil {
				return false, fmt.Errorf("结束遗留或待接管的后台失败: %w", err)
			}
		}
		if session.LaunchMode == "" && owner != nil && desktopOwnerProcess(process, *owner) && !owner.HasVisibleWindow {
			if err := lifecycleTerminate(*owner, 3*time.Second); err != nil && !errors.Is(err, processlifecycle.ErrProcessNotFound) {
				return false, fmt.Errorf("结束遗留的无窗口工作台失败: %w", err)
			}
		}
		return true, nil
	default:
		return false, errors.New("无法判断已有后台的归属")
	}
}
