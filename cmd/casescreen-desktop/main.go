package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	localapi "casescreen/internal/api"
	"casescreen/internal/processlifecycle"
	"casescreen/internal/trayapp"
	"casescreen/internal/workbench"
)

const desktopVersion = "1.0.1"

const (
	desktopInstanceMutexName   = `Local\CaseScreenDesktop`
	desktopInstanceSignalName  = desktopInstanceMutexName + `.Show.` + desktopVersion
	desktopInstanceWait        = 3 * time.Second
	existingSessionRetryWindow = 2 * time.Second
)

// serviceSession 同时校验服务版本和桌面托管归属。application_version 是
// 拒绝复用同协议旧后台的发布门禁字段，不能只检查协议号。
type serviceSession struct {
	Token                 string `json:"token"`
	ProtocolVersion       int    `json:"protocol_version"`
	ApplicationVersion    string `json:"application_version"`
	LaunchMode            string `json:"launch_mode"`
	ProcessID             int    `json:"process_id"`
	OwnerProcessID        int    `json:"owner_process_id"`
	OwnerProcessStartedAt uint64 `json:"owner_process_started"`
}

// CaseScreenDesktop 是纯 Go 桌面入口：启动统一协议服务后直接打开 Gio 工作台，
// 不再查找 Python、安装 PySide6 或启动浏览器页面。
func main() {
	if err := run(); err != nil {
		showError(err)
		os.Exit(1)
	}
}

func run() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	instanceLock, existingShown, err := claimDesktopInstance(
		func() (processlifecycle.InstanceLock, error) {
			return processlifecycle.AcquireSingleInstance(desktopInstanceMutexName, desktopInstanceWait)
		},
		func() error { return notifyDesktopInstance(desktopInstanceSignalName) },
	)
	if err != nil {
		return fmt.Errorf("建立工作台单实例保护: %w", err)
	}
	if existingShown {
		return nil
	}
	defer instanceLock.Close()
	instanceSignal, err := listenDesktopInstanceSignal(desktopInstanceSignalName)
	if err != nil {
		return fmt.Errorf("建立工作台显示信号: %w", err)
	}
	defer instanceSignal.Close()
	showRelay := &desktopShowRelay{}
	go func() {
		for range instanceSignal.Requests() {
			showRelay.Request()
		}
	}()
	preferenceChanged, err := synchronizeStartupGPUPreference(executable)
	if err != nil {
		return fmt.Errorf("同步当前发行目录的 Windows GPU 首选项: %w", err)
	}
	restartAttempted := os.Getenv(gpuPreferenceRestartMarker) == gpuPreferenceRestartMarkerValue
	_ = os.Unsetenv(gpuPreferenceRestartMarker)
	if shouldRestartDesktopForGPUPreference(preferenceChanged, restartAttempted) {
		if err := restartDesktopForGPUPreference(executable, os.Args[1:], os.Environ()); err != nil {
			return fmt.Errorf("GPU 首选项迁移后重新启动桌面工作台: %w", err)
		}
		return nil
	}
	root := filepath.Dir(executable)
	serviceURL := "http://127.0.0.1:19321"
	serviceProcess, err := ensureService(root, serviceURL)
	if err != nil {
		return err
	}
	var serviceSupervisor *managedServiceSupervisor
	if serviceProcess != nil {
		serviceSupervisor = newManagedServiceSupervisor(
			root, serviceURL, serviceProcess,
			func(root, baseURL string) (managedServiceHandle, error) {
				return ensureService(root, baseURL)
			},
		)
	}
	var stopOnce sync.Once
	stopService := func() {
		stopOnce.Do(func() {
			if serviceSupervisor != nil {
				serviceSupervisor.Stop()
			} else if serviceProcess != nil {
				serviceProcess.shutdown()
			}
		})
	}
	defer stopService()
	var desktopTray *trayapp.DesktopTray
	var runtimeFailureMu sync.Mutex
	var runtimeFailure error
	workbench.RunDesktop(workbench.NewAPI(serviceURL, desktopVersion), workbench.DesktopRunOptions{
		Ready: func(controller *workbench.DesktopWindowController) error {
			showRelay.Attach(controller.Show)
			trayRuntime, trayErr := trayapp.StartDesktopTray(trayapp.DesktopTrayOptions{
				Show: controller.Show,
				Quit: controller.Quit,
			})
			if trayErr != nil {
				return trayErr
			}
			desktopTray = trayRuntime
			go func() {
				runErr := <-trayRuntime.Done()
				if runErr != nil {
					runtimeFailureMu.Lock()
					runtimeFailure = runErr
					runtimeFailureMu.Unlock()
				}
				// 托盘消息泵若意外退出，不能留下无入口的后台工作台。
				controller.Quit()
			}()
			if serviceSupervisor != nil {
				go func() {
					if supervisorErr := <-serviceSupervisor.Fatal(); supervisorErr != nil {
						runtimeFailureMu.Lock()
						runtimeFailure = supervisorErr
						runtimeFailureMu.Unlock()
						// 多次有界重启仍失败时，不能留下一个
						// 看似正常但永久无 API 的工作台外壳。
						controller.Quit()
					}
				}()
			}
			return nil
		},
		Closed: func(windowErr error) {
			if desktopTray != nil {
				desktopTray.Close()
			}
			// Gio/Windows 的 app.Main 在最后一个窗口销毁后仍会阻塞；
			// 只有托盘“完全退出”或异常销毁才会进入此处并清理托管服务。
			stopService()
			runtimeFailureMu.Lock()
			if windowErr == nil {
				windowErr = runtimeFailure
			}
			runtimeFailureMu.Unlock()
			if windowErr != nil {
				showError(windowErr)
				os.Exit(1)
			}
			os.Exit(0)
		},
	})
	return nil
}

// claimDesktopInstance 在任何后台服务或 Gio 资源创建之前完成单实例分流。
// 已运行的同版工作台只接收一个无参数的“显示窗口”信号，新进程随后正常退出。
func claimDesktopInstance(
	acquire func() (processlifecycle.InstanceLock, error),
	notify func() error,
) (processlifecycle.InstanceLock, bool, error) {
	instanceLock, err := acquire()
	if err == nil {
		return instanceLock, false, nil
	}
	if !errors.Is(err, processlifecycle.ErrInstanceRunning) {
		return nil, false, err
	}
	if notifyErr := notify(); notifyErr != nil {
		return nil, false, fmt.Errorf("已有工作台正在运行，但无法向当前 %s 版本发送显示请求: %w", desktopVersion, notifyErr)
	}
	return nil, true, nil
}

// desktopShowRelay 保留主窗口控制器就绪前到达的一次显示请求。
// 后续请求直接复用托盘相同的 Show 路径，不创建第二个后台服务。
type desktopShowRelay struct {
	mu      sync.Mutex
	show    func()
	pending bool
}

func (relay *desktopShowRelay) Request() {
	relay.mu.Lock()
	show := relay.show
	if show == nil {
		relay.pending = true
	}
	relay.mu.Unlock()
	if show != nil {
		show()
	}
}

func (relay *desktopShowRelay) Attach(show func()) {
	relay.mu.Lock()
	relay.show = show
	pending := relay.pending
	relay.pending = false
	relay.mu.Unlock()
	if pending && show != nil {
		show()
	}
}

func ensureService(root, baseURL string) (*managedService, error) {
	session, state := querySession(baseURL)
	if state == sessionUnavailable {
		if port, portErr := servicePort(baseURL); portErr == nil {
			if _, listenerErr := processlifecycle.TCPListenerPID(port); listenerErr == nil {
				// 端口已存在时先给正在启动或短暂繁忙的同版服务
				// 一个恢复窗口。仍不可用时只报错，绝不根据隐藏窗口误杀。
				session, state = waitForExistingSession(baseURL, existingSessionRetryWindow)
				if state == sessionUnavailable {
					return nil, errors.New("本机 19321 端口的 CaseScreen 服务暂未响应会话检查；已保留原进程，请稍后重试")
				}
			} else if !errors.Is(listenerErr, processlifecycle.ErrProcessNotFound) && !errors.Is(listenerErr, processlifecycle.ErrUnsupported) {
				return nil, fmt.Errorf("检查本机服务端口失败: %w", listenerErr)
			}
		}
	}
	if state != sessionUnavailable {
		replaced, err := replaceOrRejectExisting(root, baseURL, state, session)
		if err != nil {
			return nil, err
		}
		if !replaced {
			return nil, nil
		}
	}
	servicePath := filepath.Join(root, "CaseScreen.exe")
	if _, err := os.Stat(servicePath); err != nil {
		return nil, fmt.Errorf("未找到 CaseScreen.exe；请确认发行包完整: %w", err)
	}
	owner, err := processlifecycle.Current()
	if err != nil {
		return nil, fmt.Errorf("读取桌面进程身份失败: %w", err)
	}
	command := exec.Command(servicePath,
		"-headless",
		"-desktop-owner-pid", strconv.Itoa(owner.PID),
		"-desktop-owner-started", strconv.FormatUint(owner.Started, 10),
	)
	command.Dir = root
	if err := command.Start(); err != nil {
		return nil, err
	}
	managed := newManagedService(command, baseURL)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		session, state := querySession(baseURL)
		switch state {
		case sessionCompatible:
			if session.LaunchMode != "desktop" || session.ProcessID != command.Process.Pid ||
				session.OwnerProcessID != owner.PID || session.OwnerProcessStartedAt != owner.Started {
				managed.shutdown()
				return nil, errors.New("启动后的服务未返回有效的桌面归属信息；请确认 CaseScreen.exe 与工作台来自同一发行包")
			}
			managed.session = session
			return managed, nil
		case sessionIncompatible:
			managed.shutdown()
			return nil, errors.New("本机端口被另一个不兼容服务占用，无法启动当前后台")
		}
		select {
		case <-managed.Done():
			waitErr := managed.Err()
			if waitErr == nil {
				return nil, errors.New("统一协议服务在启动阶段提前退出")
			}
			return nil, fmt.Errorf("统一协议服务在启动阶段退出: %w", waitErr)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	managed.shutdown()
	return nil, errors.New("统一协议服务未能在 8 秒内启动")
}

type sessionProbe int

const (
	sessionUnavailable sessionProbe = iota
	sessionLegacy
	sessionCompatible
	sessionIncompatible
)

func probeSession(baseURL string) sessionProbe {
	_, state := querySession(baseURL)
	return state
}

func querySession(baseURL string) (serviceSession, sessionProbe) {
	client := http.Client{Timeout: 500 * time.Millisecond}
	response, err := client.Get(baseURL + "/api/session")
	if err != nil {
		return serviceSession{}, sessionUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed {
		// 能够完整返回 HTTP，但尚未提供 /api/session，才是可明确
		// 进入旧版孤儿识别的 legacy 服务。超时和 5xx 不得走此路径。
		return serviceSession{}, sessionLegacy
	}
	if response.StatusCode != http.StatusOK {
		return serviceSession{}, sessionUnavailable
	}
	var body serviceSession
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&body); err != nil {
		return serviceSession{}, sessionUnavailable
	}
	if body.Token == "" {
		return serviceSession{}, sessionUnavailable
	}
	if body.ProtocolVersion != localapi.ProtocolVersion {
		return body, sessionIncompatible
	}
	if body.ApplicationVersion != desktopVersion {
		return body, sessionIncompatible
	}
	return body, sessionCompatible
}

func waitForExistingSession(baseURL string, wait time.Duration) (serviceSession, sessionProbe) {
	deadline := time.Now().Add(wait)
	for {
		session, state := querySession(baseURL)
		if state != sessionUnavailable || wait <= 0 || !time.Now().Before(deadline) {
			return session, state
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
