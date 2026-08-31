package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	localapi "casescreen/internal/api"
	"casescreen/internal/processlifecycle"
)

type fakeInstanceLock struct {
	closes atomic.Int32
}

func (lock *fakeInstanceLock) Close() error {
	lock.closes.Add(1)
	return nil
}

func TestClaimDesktopInstanceSignalsExistingVersionAndSkipsPrimaryStartup(t *testing.T) {
	var notifications atomic.Int32
	instanceLock, existingShown, err := claimDesktopInstance(
		func() (processlifecycle.InstanceLock, error) {
			return nil, processlifecycle.ErrInstanceRunning
		},
		func() error {
			notifications.Add(1)
			return nil
		},
	)
	if err != nil || instanceLock != nil || !existingShown || notifications.Load() != 1 {
		t.Fatalf("重复启动分流错误: lock=%v shown=%v notifications=%d err=%v", instanceLock, existingShown, notifications.Load(), err)
	}
}

func TestClaimDesktopInstanceKeepsPrimaryLockWithoutNotification(t *testing.T) {
	lock := &fakeInstanceLock{}
	var notifications atomic.Int32
	instanceLock, existingShown, err := claimDesktopInstance(
		func() (processlifecycle.InstanceLock, error) { return lock, nil },
		func() error {
			notifications.Add(1)
			return nil
		},
	)
	if err != nil || instanceLock != lock || existingShown || notifications.Load() != 0 {
		t.Fatalf("主实例分流错误: lock=%v shown=%v notifications=%d err=%v", instanceLock, existingShown, notifications.Load(), err)
	}
}

func TestClaimDesktopInstanceDoesNotHideSignalFailure(t *testing.T) {
	expected := errors.New("显示信号不可用")
	instanceLock, existingShown, err := claimDesktopInstance(
		func() (processlifecycle.InstanceLock, error) {
			return nil, processlifecycle.ErrInstanceRunning
		},
		func() error { return expected },
	)
	if instanceLock != nil || existingShown || !errors.Is(err, expected) {
		t.Fatalf("显示信号失败边界错误: lock=%v shown=%v err=%v", instanceLock, existingShown, err)
	}
}

func TestDesktopShowRelayKeepsEarlyRequestAndUsesAttachedShow(t *testing.T) {
	var shows atomic.Int32
	relay := &desktopShowRelay{}
	relay.Request()
	relay.Request()
	if shows.Load() != 0 {
		t.Fatal("控制器就绪前不应直接显示")
	}
	relay.Attach(func() { shows.Add(1) })
	if shows.Load() != 1 {
		t.Fatalf("就绪前显示请求没有合并重放: %d", shows.Load())
	}
	relay.Request()
	if shows.Load() != 2 {
		t.Fatalf("控制器就绪后显示请求未直接转发: %d", shows.Load())
	}
}

func TestProbeSessionRequiresMatchingProtocol(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected sessionProbe
	}{
		{name: "匹配", body: fmt.Sprintf(`{"token":"ok","protocol_version":%d,"application_version":%q}`, localapi.ProtocolVersion, desktopVersion), expected: sessionCompatible},
		{name: "旧服务没有版本", body: `{"token":"old"}`, expected: sessionIncompatible},
		{name: "版本不匹配", body: `{"token":"old","protocol_version":1}`, expected: sessionIncompatible},
		{name: "同协议旧应用版本", body: fmt.Sprintf(`{"token":"old","protocol_version":%d,"application_version":"0.3.0-beta.13"}`, localapi.ProtocolVersion), expected: sessionIncompatible},
		{name: "同协议缺少应用版本", body: fmt.Sprintf(`{"token":"old","protocol_version":%d}`, localapi.ProtocolVersion), expected: sessionIncompatible},
		{name: "没有令牌", body: fmt.Sprintf(`{"protocol_version":%d,"application_version":%q}`, localapi.ProtocolVersion, desktopVersion), expected: sessionUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			if actual := probeSession(server.URL); actual != test.expected {
				t.Fatalf("probeSession() = %d, want %d", actual, test.expected)
			}
		})
	}
}

func TestQuerySessionDistinguishesLegacyEndpointFromTransientFailure(t *testing.T) {
	legacy := httptest.NewServer(http.NotFoundHandler())
	defer legacy.Close()
	if _, state := querySession(legacy.URL); state != sessionLegacy {
		t.Fatalf("明确 404 的旧服务状态=%d，want legacy", state)
	}

	temporaryFailure := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "starting", http.StatusServiceUnavailable)
	}))
	defer temporaryFailure.Close()
	if _, state := querySession(temporaryFailure.URL); state != sessionUnavailable {
		t.Fatalf("短暂 503 状态=%d，不应当作可清理旧服务", state)
	}
}

func TestWaitForExistingSessionRecoversAfterTransientFailures(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= 3 {
			http.Error(writer, "starting", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"token":"ready","protocol_version":%d,"application_version":%q,"launch_mode":"desktop"}`, localapi.ProtocolVersion, desktopVersion)
	}))
	defer server.Close()
	session, state := waitForExistingSession(server.URL, time.Second)
	if state != sessionCompatible || session.Token != "ready" {
		t.Fatalf("短暂失败后没有恢复当前会话: state=%d session=%+v", state, session)
	}
	if attempts.Load() < 4 {
		t.Fatalf("未执行预期重试: %d", attempts.Load())
	}
}

func TestWaitForExistingSessionRecoversAfterHTTPTimeout(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			// 超过 querySession 的 500ms 客户端期限，模拟已监听但
			// 启动中或短暂繁忙的当前 1.0 服务。
			time.Sleep(550 * time.Millisecond)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"token":"ready-after-timeout","protocol_version":%d,"application_version":%q,"launch_mode":"desktop"}`, localapi.ProtocolVersion, desktopVersion)
	}))
	defer server.Close()
	session, state := waitForExistingSession(server.URL, 2*time.Second)
	if state != sessionCompatible || session.Token != "ready-after-timeout" {
		t.Fatalf("HTTP 超时后没有恢复当前会话: state=%d session=%+v", state, session)
	}
	if attempts.Load() < 2 {
		t.Fatalf("超时后未重试: %d", attempts.Load())
	}
}

func TestQuerySessionReadsDesktopOwnership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"token":"owner-token","protocol_version":%d,"application_version":%q,"launch_mode":"desktop","process_id":42,"owner_process_id":7,"owner_process_started":99}`, localapi.ProtocolVersion, desktopVersion)
	}))
	defer server.Close()
	session, state := querySession(server.URL)
	if state != sessionCompatible {
		t.Fatalf("会话状态=%d", state)
	}
	if session.LaunchMode != "desktop" || session.ProcessID != 42 || session.OwnerProcessID != 7 || session.OwnerProcessStartedAt != 99 {
		t.Fatalf("桌面归属字段丢失: %+v", session)
	}
}

func TestClassifyExistingServiceProtectsManualModesAndReplacesOrphans(t *testing.T) {
	releases := t.TempDir()
	root := filepath.Join(releases, "CaseScreen-windows-x64-1.0")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	service := processlifecycle.Snapshot{
		PID: 40, ParentPID: 30, Started: 400,
		Executable: filepath.Join(root, "CaseScreen.exe"),
	}
	desktop := processlifecycle.Snapshot{
		PID: 30, Started: 300, HasVisibleWindow: true,
		Executable: filepath.Join(root, "CaseScreenDesktop.exe"),
	}
	manual := processlifecycle.Snapshot{
		PID: 30, Started: 300,
		Executable: filepath.Join(string(filepath.Separator), "Windows", "explorer.exe"),
	}
	tests := []struct {
		name       string
		compatible bool
		session    serviceSession
		command    string
		owner      *processlifecycle.Snapshot
		ownerErr   error
		want       existingServiceAction
	}{
		{name: "当前托盘服务由桌面安全接管", compatible: true, session: serviceSession{LaunchMode: "tray", ProcessID: 40}, want: existingServiceReplace},
		{name: "旧托盘服务受保护", session: serviceSession{LaunchMode: "tray", ProcessID: 40}, want: existingServiceProtected},
		{name: "手动headless由桌面安全接管", compatible: true, session: serviceSession{LaunchMode: "headless", ProcessID: 40}, want: existingServiceReplace},
		{name: "活跃桌面服务不抢占", compatible: true, session: serviceSession{LaunchMode: "desktop", ProcessID: 40, OwnerProcessID: 30, OwnerProcessStartedAt: 300}, owner: &desktop, want: existingServiceActiveDesktop},
		{name: "新桌面孤儿可替换", session: serviceSession{LaunchMode: "desktop", ProcessID: 40, OwnerProcessID: 30, OwnerProcessStartedAt: 300}, ownerErr: processlifecycle.ErrProcessNotFound, want: existingServiceReplace},
		{name: "所有者PID复用后可替换", session: serviceSession{LaunchMode: "desktop", ProcessID: 40, OwnerProcessID: 30, OwnerProcessStartedAt: 299}, owner: &desktop, want: existingServiceReplace},
		{name: "旧桌面孤儿可替换", command: `CaseScreen.exe -headless`, ownerErr: processlifecycle.ErrProcessNotFound, want: existingServiceReplace},
		{name: "旧桌面仍活跃不抢占", command: `CaseScreen.exe -headless`, owner: &desktop, want: existingServiceActiveDesktop},
		{name: "旧桌面窗口已关闭时替换", command: `CaseScreen.exe -headless`, owner: &processlifecycle.Snapshot{PID: 30, Started: 300, Executable: filepath.Join(root, "CaseScreenDesktop.exe")}, want: existingServiceReplace},
		{name: "同版终端headless也由桌面接管", compatible: true, command: `CaseScreen.exe -headless`, owner: &manual, want: existingServiceReplace},
		{name: "无法读取父进程时不误杀", command: `CaseScreen.exe -headless`, ownerErr: errors.New("access denied"), want: existingServiceProtected},
		{name: "会话PID与监听PID不一致", compatible: true, session: serviceSession{LaunchMode: "desktop", ProcessID: 41, OwnerProcessID: 30}, owner: &desktop, want: existingServiceProtected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := service
			candidate.CommandLine = test.command
			if got := classifyExistingService(root, test.compatible, test.session, candidate, test.owner, test.ownerErr); got != test.want {
				t.Fatalf("分类=%d，want %d", got, test.want)
			}
		})
	}
}

func TestReplaceOrRejectExistingNeverKillsUnavailableSession(t *testing.T) {
	replaced, err := replaceOrRejectExisting(t.TempDir(), "http://127.0.0.1:19321", sessionUnavailable, serviceSession{})
	if err == nil || replaced {
		t.Fatalf("未知会话不应自动清理: replaced=%v err=%v", replaced, err)
	}
	if !strings.Contains(err.Error(), "保留原进程") {
		t.Fatalf("保护错误不明确: %v", err)
	}
}

func TestReplaceOrRejectExistingRejectsProcessIdentityProbeFailures(t *testing.T) {
	probeErr := errors.New("模拟进程查询权限不足")
	tests := []struct {
		name          string
		state         sessionProbe
		listenerFails bool
	}{
		{name: "兼容会话监听PID失败", state: sessionCompatible, listenerFails: true},
		{name: "兼容会话进程身份失败", state: sessionCompatible},
		{name: "旧会话监听PID失败", state: sessionLegacy, listenerFails: true},
		{name: "旧会话进程身份失败", state: sessionLegacy},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalListener := lifecycleTCPListenerPID
			originalInspect := lifecycleInspectProcess
			originalTerminate := lifecycleTerminate
			t.Cleanup(func() {
				lifecycleTCPListenerPID = originalListener
				lifecycleInspectProcess = originalInspect
				lifecycleTerminate = originalTerminate
			})
			inspectCalls := 0
			lifecycleTCPListenerPID = func(uint16) (int, error) {
				if test.listenerFails {
					return 0, probeErr
				}
				return 40, nil
			}
			lifecycleInspectProcess = func(int) (processlifecycle.Snapshot, error) {
				inspectCalls++
				return processlifecycle.Snapshot{}, probeErr
			}
			terminateCalls := 0
			lifecycleTerminate = func(processlifecycle.Snapshot, time.Duration) error {
				terminateCalls++
				return nil
			}

			replaced, err := replaceOrRejectExisting(
				t.TempDir(), "http://127.0.0.1:19321", test.state,
				serviceSession{ProcessID: 40, LaunchMode: "headless"},
			)
			if replaced || err == nil || !strings.Contains(err.Error(), "无法核验") || terminateCalls != 0 {
				t.Fatalf("身份探测失败未安全拒绝: replaced=%v terminate=%d err=%v", replaced, terminateCalls, err)
			}
			if test.listenerFails && inspectCalls != 0 {
				t.Fatalf("监听 PID 失败后仍查询进程: %d", inspectCalls)
			}
			if !test.listenerFails && inspectCalls != 1 {
				t.Fatalf("进程身份查询次数=%d，want 1", inspectCalls)
			}
		})
	}
}

func TestReplaceOrRejectExistingTakesOverPrestartedManualService(t *testing.T) {
	root := filepath.Join(t.TempDir(), "CaseScreen-windows-x64-1.0")
	service := processlifecycle.Snapshot{
		PID: 40, ParentPID: 30, Started: 400,
		Executable: filepath.Join(root, "CaseScreen.exe"),
	}
	manualOwner := processlifecycle.Snapshot{
		PID: 30, Started: 300,
		Executable: filepath.Join(string(filepath.Separator), "Windows", "explorer.exe"),
	}
	originalListener := lifecycleTCPListenerPID
	originalInspect := lifecycleInspectProcess
	originalTerminate := lifecycleTerminate
	originalWait := lifecycleWaitForExit
	t.Cleanup(func() {
		lifecycleTCPListenerPID = originalListener
		lifecycleInspectProcess = originalInspect
		lifecycleTerminate = originalTerminate
		lifecycleWaitForExit = originalWait
	})
	lifecycleTCPListenerPID = func(uint16) (int, error) { return service.PID, nil }
	lifecycleInspectProcess = func(pid int) (processlifecycle.Snapshot, error) {
		switch pid {
		case service.PID:
			return service, nil
		case manualOwner.PID:
			return manualOwner, nil
		default:
			return processlifecycle.Snapshot{}, processlifecycle.ErrProcessNotFound
		}
	}
	terminated := make([]int, 0, 1)
	lifecycleTerminate = func(process processlifecycle.Snapshot, _ time.Duration) error {
		terminated = append(terminated, process.PID)
		return nil
	}
	lifecycleWaitForExit = func(_ context.Context, process processlifecycle.Snapshot) error {
		if process.PID != service.PID {
			t.Fatalf("等待了错误的接管进程: %d", process.PID)
		}
		return nil
	}
	var takeoverRequests atomic.Int32
	takeoverServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("X-CaseScreen-Token") != "same-release" ||
			request.Header.Get(localapi.DesktopTakeoverHeader) != localapi.DesktopTakeoverValue {
			http.Error(writer, "invalid takeover", http.StatusForbidden)
			return
		}
		takeoverRequests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"shutting_down":true}`))
	}))
	defer takeoverServer.Close()

	for _, mode := range []string{"tray", "headless"} {
		terminated = terminated[:0]
		replaced, err := replaceOrRejectExisting(root, takeoverServer.URL, sessionCompatible, serviceSession{
			Token: "same-release", ProtocolVersion: localapi.ProtocolVersion,
			ApplicationVersion: desktopVersion, LaunchMode: mode, ProcessID: service.PID,
		})
		if err != nil || !replaced {
			t.Fatalf("%s 服务没有进入桌面接管: replaced=%v err=%v", mode, replaced, err)
		}
		if len(terminated) != 0 {
			t.Fatalf("%s 优雅接管后仍强制终止了进程: %v", mode, terminated)
		}
	}
	if takeoverRequests.Load() != 2 {
		t.Fatalf("预启动 tray/headless 接管请求=%d，want 2", takeoverRequests.Load())
	}
}

func TestReplaceOrRejectExistingFallsBackToIdentityCheckedTerminate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "CaseScreen-windows-x64-1.0")
	service := processlifecycle.Snapshot{PID: 40, ParentPID: 30, Started: 400, Executable: filepath.Join(root, "CaseScreen.exe")}
	originalListener := lifecycleTCPListenerPID
	originalInspect := lifecycleInspectProcess
	originalTerminate := lifecycleTerminate
	t.Cleanup(func() {
		lifecycleTCPListenerPID = originalListener
		lifecycleInspectProcess = originalInspect
		lifecycleTerminate = originalTerminate
	})
	lifecycleTCPListenerPID = func(uint16) (int, error) { return service.PID, nil }
	lifecycleInspectProcess = func(pid int) (processlifecycle.Snapshot, error) {
		if pid == service.PID {
			return service, nil
		}
		return processlifecycle.Snapshot{}, processlifecycle.ErrProcessNotFound
	}
	terminated := 0
	lifecycleTerminate = func(process processlifecycle.Snapshot, _ time.Duration) error {
		if !processlifecycle.SameIdentity(process, service) {
			t.Fatalf("强制回退未使用已核验的进程身份: %+v", process)
		}
		terminated++
		return nil
	}
	refusingServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not ready", http.StatusConflict)
	}))
	defer refusingServer.Close()
	replaced, err := replaceOrRejectExisting(root, refusingServer.URL, sessionCompatible, serviceSession{
		Token: "same-release", ProtocolVersion: localapi.ProtocolVersion,
		ApplicationVersion: desktopVersion, LaunchMode: "headless", ProcessID: service.PID,
	})
	if err != nil || !replaced || terminated != 1 {
		t.Fatalf("优雅接管失败后未完成有界强制回退: replaced=%v terminated=%d err=%v", replaced, terminated, err)
	}
}

func TestClassifyExistingServiceNeverKillsUnrelatedPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "CaseScreen-windows-x64-1.0")
	service := processlifecycle.Snapshot{
		PID: 40, ParentPID: 30, Started: 400, CommandLine: `CaseScreen.exe -headless`,
		Executable: filepath.Join(string(filepath.Separator), "unrelated", "CaseScreen.exe"),
	}
	session := serviceSession{LaunchMode: "desktop", ProcessID: 40, OwnerProcessID: 30}
	if got := classifyExistingService(root, false, session, service, nil, processlifecycle.ErrProcessNotFound); got != existingServiceProtected {
		t.Fatalf("无关目录进程分类=%d，want protected", got)
	}
}

func TestManagedReleasePathRequiresFormalOldReleaseWithDesktopPair(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "CaseScreen-windows-x64-1.0")
	oldRoot := filepath.Join(parent, "CaseScreen-windows-x64-0.3.0-beta.17")
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "CaseScreenDesktop.exe"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !managedReleasePath(root, filepath.Join(oldRoot, "CaseScreen.exe")) {
		t.Fatal("完整的正式旧发行目录没有被识别")
	}
	missingPair := filepath.Join(parent, "CaseScreen-windows-x64-0.3.0-beta.16")
	if err := os.MkdirAll(missingPair, 0o700); err != nil {
		t.Fatal(err)
	}
	if managedReleasePath(root, filepath.Join(missingPair, "CaseScreen.exe")) {
		t.Fatal("缺少桌面二进制的同名目录不应自动接管")
	}
	broadName := filepath.Join(parent, "CaseScreen-unrelated")
	if err := os.MkdirAll(broadName, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broadName, "CaseScreenDesktop.exe"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if managedReleasePath(root, filepath.Join(broadName, "CaseScreen.exe")) {
		t.Fatal("宽泛的 CaseScreen* 目录不应被当作正式发行目录")
	}
	foreignRoot := filepath.Join(t.TempDir(), "CaseScreen-windows-x64-0.3.0-beta.15")
	if err := os.MkdirAll(foreignRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignRoot, "CaseScreenDesktop.exe"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if managedReleasePath(root, filepath.Join(foreignRoot, "CaseScreen.exe")) {
		t.Fatal("异父目录的正式发行版也不能被当前副本自动终止")
	}
}

func TestProcessArgumentMatchesWholeFlag(t *testing.T) {
	if !processArgument(`"C:\\CaseScreen.exe" -headless -desktop-owner-pid 7`, "-headless") {
		t.Fatal("没有识别独立 -headless 参数")
	}
	if processArgument(`"C:\\headless-tools\\CaseScreen.exe"`, "-headless") {
		t.Fatal("把路径片段误判成 -headless 参数")
	}
}
