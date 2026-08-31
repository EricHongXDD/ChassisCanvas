//go:build windows

package coolermaster

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const masterCTRLServiceName = "MasterCTRLService"

// CheckOEMControlConflict 用只读系统查询判断原厂服务或代理是否会控制 LCD。
// 它不停止、禁用或修改服务；调用方应把冲突作为可恢复的连接错误展示。
// 除了当前 Running/StartPending，还检查服务自启与 CMTrayApp 登录启动；
// 这能阻止开机时 CaseScreen 先显示用户主题、原厂稍后再抢占为默认画面。
func CheckOEMControlConflict() error {
	serviceActive, serviceMayAutoStart, serviceErr := masterCTRLServiceState()
	agentActive, agentErr := processNameActive("CMAgent.exe")
	if conflict := oemControlOwnershipConflict(serviceActive, serviceMayAutoStart, agentActive); conflict != nil {
		if !serviceActive && !agentActive && serviceMayAutoStart {
			return fmt.Errorf(
				"%w；MasterCTRL 已配置为登录/开机自动控制 LCD，稍后启动会覆盖 CaseScreen 主题；若要由 CaseScreen 独占副屏，请将“MasterCTRL Service”启动类型设为“禁用”并退出 MasterCTRL，随后会自动重连",
				conflict,
			)
		}
		return fmt.Errorf(
			"%w；为避免两套程序互相覆盖，请退出 MasterCTRL，并将“MasterCTRL Service”停止且设为“禁用”，CaseScreen 会自动重连并恢复已保存主题",
			conflict,
		)
	}
	// 查询失败不应把未安装原厂软件的机器永久挡在连接之外。真正的
	// HID 打开和后续健康检查仍会给出设备级错误。
	_ = serviceErr
	_ = agentErr
	return nil
}

func masterCTRLServiceState() (active bool, mayAutoStart bool, resultErr error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, false, err
	}
	defer windows.CloseServiceHandle(manager)
	name, err := windows.UTF16PtrFromString(masterCTRLServiceName)
	if err != nil {
		return false, false, err
	}
	service, err := windows.OpenService(
		manager,
		name,
		windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG,
	)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	defer windows.CloseServiceHandle(service)
	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	statusErr := windows.QueryServiceStatusEx(
		service,
		windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&status)),
		uint32(unsafe.Sizeof(status)),
		&needed,
	)
	if statusErr == nil {
		active = status.CurrentState != windows.SERVICE_STOPPED
	}
	startType, configErr := serviceStartType(service)
	trayAutoStart, trayErr := masterCTRLTrayAutoStart()
	// 服务已禁用时，即使原厂留下了 CMTrayApp 登录项也无法
	// 按正常链路重启 LocalSystem 控制服务；若它真的直接拉起
	// CMAgent，上面的进程查询仍会实时拦截。
	mayAutoStart = oemServiceMayAutoStart(
		startType == windows.SERVICE_AUTO_START,
		startType == windows.SERVICE_DISABLED,
		trayAutoStart,
	)
	return active, mayAutoStart, errors.Join(statusErr, configErr, trayErr)
}

func serviceStartType(service windows.Handle) (uint32, error) {
	size := uint32(1024)
	for {
		buffer := make([]byte, size)
		configuration := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buffer[0]))
		needed := uint32(0)
		err := windows.QueryServiceConfig(service, configuration, size, &needed)
		if err == nil {
			return configuration.StartType, nil
		}
		if !errors.Is(err, syscall.ERROR_INSUFFICIENT_BUFFER) || needed <= size {
			return windows.SERVICE_NO_CHANGE, err
		}
		size = needed
	}
}

func masterCTRLTrayAutoStart() (bool, error) {
	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.QUERY_VALUE,
	)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer key.Close()
	value, _, err := key.GetStringValue("MasterCTRL")
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(value) != "", nil
}

func processNameActive(name string) (bool, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return false, nil
		}
		return false, err
	}
	for {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), name) {
			return true, nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return false, nil
			}
			return false, err
		}
	}
}
