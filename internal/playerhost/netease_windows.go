//go:build windows && casescreen_legacy_netease_cdp

package playerhost

import (
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	neteaseRemoteDebuggingPipeArgument = "--remote-debugging-pipe"
	neteaseCRTInheritedHandleCount     = 5
	neteaseCRTOpenPipeFlags            = 0x01 | 0x08 // FOPEN | FPIPE，保持二进制模式。

	// NeteaseDisposableWindowsEnvironment 只是真机测试的防误触确认，不是
	// 隔离技术。设置者仍必须实际位于 Windows Sandbox、一次性用户或可销毁 VM。
	NeteaseDisposableWindowsEnvironment     = "CASESCREEN_NETEASE_DISPOSABLE_WINDOWS"
	NeteaseDisposableWindowsAcknowledgement = "I_ACKNOWLEDGE_THIS_WINDOWS_ACCOUNT_IS_DISPOSABLE"
)

func newPlatformNeteaseDependencies() neteaseDependencies {
	return neteaseDependencies{
		locateExecutable: locateNeteaseExecutableFromAppPaths,
		processRunning:   neteaseProcessRunning,
		launch:           launchNeteaseExecutableWithPrivatePipe,
	}
}

type neteaseAppPathLocation struct {
	root registry.Key
	view uint32
}

func locateNeteaseExecutableFromAppPaths() (string, error) {
	const keyPath = `Software\Microsoft\Windows\CurrentVersion\App Paths\cloudmusic.exe`
	locations := []neteaseAppPathLocation{
		{root: registry.CURRENT_USER},
		{root: registry.LOCAL_MACHINE, view: registry.WOW64_64KEY},
		{root: registry.LOCAL_MACHINE, view: registry.WOW64_32KEY},
	}
	var firstFailure error
	for _, location := range locations {
		key, err := registry.OpenKey(location.root, keyPath, registry.QUERY_VALUE|location.view)
		if err != nil {
			if !errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) && firstFailure == nil {
				firstFailure = err
			}
			continue
		}
		value, _, valueErr := key.GetStringValue("")
		_ = key.Close()
		if valueErr != nil {
			if firstFailure == nil {
				firstFailure = valueErr
			}
			continue
		}
		path, validateErr := validateNeteaseExecutablePath(value)
		if validateErr == nil {
			return path, nil
		}
		if firstFailure == nil {
			firstFailure = validateErr
		}
	}
	if firstFailure != nil {
		return "", firstFailure
	}
	return "", errNeteaseNotInstalled
}

func validateNeteaseExecutablePath(raw string) (string, error) {
	path := strings.Trim(strings.TrimSpace(raw), `"`)
	if path == "" || !filepath.IsAbs(path) || !strings.EqualFold(filepath.Base(path), "cloudmusic.exe") {
		return "", errors.New("网易云 App Paths 的 cloudmusic.exe 路径无效")
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("网易云 App Paths 指向的不是普通文件")
	}
	return path, nil
}

func neteaseProcessRunning() (bool, error) {
	processIDs, err := neteaseProcessIDs()
	return len(processIDs) > 0, err
}

func neteaseProcessIDs() ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	var processIDs []uint32
	for {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "cloudmusic.exe") {
			processIDs = append(processIDs, entry.ProcessID)
		}
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
			return processIDs, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// NeteaseProcessIDsForVerification 只供显式真机门禁核对断管前后的同一批
// cloudmusic.exe；它不打开进程句柄，也不返回路径或任何客户端内容。
func NeteaseProcessIDsForVerification() ([]uint32, error) {
	return neteaseProcessIDs()
}

// RequireDisposableWindowsForNeteaseVerification 是首次 CEF 91 真机测试的
// 防误触门禁。环境变量确认不能证明隔离成立；操作者必须先进入 Windows
// Sandbox、一次性 Windows 用户或可销毁 VM，绝不能在正常账户中设置后运行。
func RequireDisposableWindowsForNeteaseVerification() error {
	if os.Getenv(NeteaseDisposableWindowsEnvironment) != NeteaseDisposableWindowsAcknowledgement {
		return errors.New("首次网易云 CEF91 验证只允许在 Windows Sandbox、一次性用户或可销毁 VM 中运行")
	}
	return nil
}

// neteaseStartupInfo 显式命名 Windows SDK 在 x/sys 中隐藏的 cbReserved2 与
// lpReserved2。字段顺序和类型必须与 STARTUPINFOW 完全一致。
type neteaseStartupInfo struct {
	Cb            uint32
	Reserved      *uint16
	Desktop       *uint16
	Title         *uint16
	X             uint32
	Y             uint32
	XSize         uint32
	YSize         uint32
	XCountChars   uint32
	YCountChars   uint32
	FillAttribute uint32
	Flags         uint32
	ShowWindow    uint16
	CbReserved2   uint16
	Reserved2     *byte
	StdInput      windows.Handle
	StdOutput     windows.Handle
	StdErr        windows.Handle
}

type neteaseStartupInfoEx struct {
	StartupInfo             neteaseStartupInfo
	ProcThreadAttributeList *windows.ProcThreadAttributeList
}

type neteaseWindowsPipe struct {
	reader *os.File
	writer *os.File
	once   sync.Once
	err    error
}

func (pipe *neteaseWindowsPipe) Read(buffer []byte) (int, error) {
	if pipe == nil || pipe.reader == nil {
		return 0, io.ErrClosedPipe
	}
	return pipe.reader.Read(buffer)
}

func (pipe *neteaseWindowsPipe) Write(buffer []byte) (int, error) {
	if pipe == nil || pipe.writer == nil {
		return 0, io.ErrClosedPipe
	}
	return pipe.writer.Write(buffer)
}

func (pipe *neteaseWindowsPipe) Close() error {
	if pipe == nil {
		return nil
	}
	pipe.once.Do(func() {
		var failures []error
		if pipe.reader != nil {
			failures = append(failures, pipe.reader.Close())
		}
		if pipe.writer != nil {
			failures = append(failures, pipe.writer.Close())
		}
		pipe.err = errors.Join(failures...)
	})
	return pipe.err
}

// launchNeteaseExecutableWithPrivatePipe 为 CEF 91 创建两条匿名管道，并把
// 子端映射到其固定读取的 CRT fd 3 和写入的 fd 4。没有 TCP 监听端口。
func launchNeteaseExecutableWithPrivatePipe(path string) (io.ReadWriteCloser, error) {
	validated, err := validateNeteaseExecutablePath(path)
	if err != nil {
		return nil, err
	}
	pointerSize, err := neteaseExecutablePointerSize(validated)
	if err != nil {
		return nil, err
	}

	security := windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	var childRead, parentWrite windows.Handle
	var parentRead, childWrite windows.Handle
	closeHandle := func(handle *windows.Handle) {
		if *handle != 0 && *handle != windows.InvalidHandle {
			_ = windows.CloseHandle(*handle)
			*handle = 0
		}
	}
	defer closeHandle(&childRead)
	defer closeHandle(&parentWrite)
	defer closeHandle(&parentRead)
	defer closeHandle(&childWrite)
	if err := windows.CreatePipe(&childRead, &parentWrite, &security, 0); err != nil {
		return nil, fmt.Errorf("创建网易云命令管道: %w", err)
	}
	if err := windows.CreatePipe(&parentRead, &childWrite, &security, 0); err != nil {
		return nil, fmt.Errorf("创建网易云响应管道: %w", err)
	}
	if err := windows.SetHandleInformation(parentWrite, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("隔离网易云命令管道父端: %w", err)
	}
	if err := windows.SetHandleInformation(parentRead, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("隔离网易云响应管道父端: %w", err)
	}

	reservedBlock, err := buildNeteaseCRTHandleBlock(pointerSize, childRead, childWrite)
	if err != nil {
		return nil, err
	}
	attributeList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, fmt.Errorf("创建网易云句柄白名单: %w", err)
	}
	defer attributeList.Delete()
	inheritedHandles := []windows.Handle{childRead, childWrite}
	if err := attributeList.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&inheritedHandles[0]),
		uintptr(len(inheritedHandles))*unsafe.Sizeof(inheritedHandles[0]),
	); err != nil {
		return nil, fmt.Errorf("设置网易云句柄白名单: %w", err)
	}

	applicationName, err := windows.UTF16PtrFromString(validated)
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{validated, neteaseRemoteDebuggingPipeArgument}))
	if err != nil {
		return nil, err
	}
	currentDirectory, err := windows.UTF16PtrFromString(filepath.Dir(validated))
	if err != nil {
		return nil, err
	}
	startup := neteaseStartupInfoEx{
		StartupInfo: neteaseStartupInfo{
			Cb:          uint32(unsafe.Sizeof(neteaseStartupInfoEx{})),
			CbReserved2: uint16(len(reservedBlock)),
			Reserved2:   &reservedBlock[0],
		},
		ProcThreadAttributeList: attributeList.List(),
	}
	processInfo := windows.ProcessInformation{}
	creationFlags := uint32(windows.CREATE_DEFAULT_ERROR_MODE | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	err = windows.CreateProcess(
		applicationName,
		commandLine,
		nil,
		nil,
		true,
		creationFlags,
		nil,
		currentDirectory,
		(*windows.StartupInfo)(unsafe.Pointer(&startup.StartupInfo)),
		&processInfo,
	)
	runtime.KeepAlive(reservedBlock)
	runtime.KeepAlive(inheritedHandles)
	runtime.KeepAlive(startup)
	if err != nil {
		return nil, fmt.Errorf("以私有管道启动网易云音乐: %w", err)
	}
	_ = windows.CloseHandle(processInfo.Thread)
	_ = windows.CloseHandle(processInfo.Process)
	closeHandle(&childRead)
	closeHandle(&childWrite)

	reader := os.NewFile(uintptr(parentRead), "casescreen-netease-response")
	writer := os.NewFile(uintptr(parentWrite), "casescreen-netease-command")
	if reader == nil || writer == nil {
		if reader != nil {
			_ = reader.Close()
			parentRead = 0
		}
		if writer != nil {
			_ = writer.Close()
			parentWrite = 0
		}
		return nil, errors.New("创建网易云私有管道文件失败")
	}
	parentRead = 0
	parentWrite = 0
	return &neteaseWindowsPipe{reader: reader, writer: writer}, nil
}

func neteaseExecutablePointerSize(path string) (int, error) {
	file, err := pe.Open(path)
	if err != nil {
		return 0, fmt.Errorf("读取网易云可执行文件架构: %w", err)
	}
	defer file.Close()
	switch file.FileHeader.Machine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return 8, nil
	case pe.IMAGE_FILE_MACHINE_I386:
		return 4, nil
	default:
		return 0, fmt.Errorf("不支持的网易云可执行文件架构: 0x%04x", file.FileHeader.Machine)
	}
}

// buildNeteaseCRTHandleBlock 构造 UCRT 的 lpReserved2 继承表。句柄数组紧跟
// 5 个单字节 osfile 标志，不得插入自然对齐填充。
func buildNeteaseCRTHandleBlock(pointerSize int, childRead, childWrite windows.Handle) ([]byte, error) {
	if pointerSize != 4 && pointerSize != 8 {
		return nil, errors.New("网易云 CRT 句柄宽度必须是 4 或 8 字节")
	}
	handleValues := []uint64{
		math.MaxUint64,
		math.MaxUint64,
		math.MaxUint64,
		uint64(childRead),
		uint64(childWrite),
	}
	if pointerSize == 4 {
		for index := 3; index < len(handleValues); index++ {
			if handleValues[index] > math.MaxUint32 {
				return nil, errors.New("32 位网易云无法继承当前 64 位管道句柄")
			}
		}
		handleValues[0], handleValues[1], handleValues[2] = math.MaxUint32, math.MaxUint32, math.MaxUint32
	}
	block := make([]byte, 4+neteaseCRTInheritedHandleCount+neteaseCRTInheritedHandleCount*pointerSize)
	binary.LittleEndian.PutUint32(block[:4], neteaseCRTInheritedHandleCount)
	block[4+3] = neteaseCRTOpenPipeFlags
	block[4+4] = neteaseCRTOpenPipeFlags
	offset := 4 + neteaseCRTInheritedHandleCount
	for _, value := range handleValues {
		if pointerSize == 8 {
			binary.LittleEndian.PutUint64(block[offset:offset+8], value)
		} else {
			binary.LittleEndian.PutUint32(block[offset:offset+4], uint32(value))
		}
		offset += pointerSize
	}
	if len(block) > math.MaxUint16 {
		return nil, errors.New("网易云 CRT 句柄继承表超过 Windows 限制")
	}
	return block, nil
}
