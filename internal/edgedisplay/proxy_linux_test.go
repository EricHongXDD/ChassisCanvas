//go:build linux

package edgedisplay

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocol "casescreen/internal/protocol/waveshare"
	"golang.org/x/sys/unix"
)

func TestHostOpenerConfiguresRealGadgetRawAndNonblocking(t *testing.T) {
	master, slavePath, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()

	setupDescriptor, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	termios, err := unix.IoctlGetTermios(setupDescriptor, unix.TCGETS)
	if err != nil {
		_ = unix.Close(setupDescriptor)
		t.Fatal(err)
	}
	termios.Iflag |= unix.ICRNL | unix.IXON | unix.IXOFF | unix.IXANY
	termios.Oflag |= unix.OPOST
	termios.Lflag |= unix.ICANON | unix.ECHO | unix.ISIG
	if err := unix.IoctlSetTermios(setupDescriptor, unix.TCSETS, termios); err != nil {
		_ = unix.Close(setupDescriptor)
		t.Fatal(err)
	}
	if err := unix.Close(setupDescriptor); err != nil {
		t.Fatal(err)
	}

	endpoint := &ptyEndpoint{swap: endpointSwap{hadOriginal: true, backupPath: slavePath}}
	port, err := endpoint.HostOpener()()
	if err != nil {
		t.Fatal(err)
	}
	file, ok := port.(*os.File)
	if !ok {
		t.Fatalf("真实 gadget opener 返回了非 os.File: %T", port)
	}
	fileDescriptor := int(file.Fd())
	raw, err := unix.IoctlGetTermios(fileDescriptor, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Iflag&(unix.IGNBRK|unix.BRKINT|unix.PARMRK|unix.ISTRIP|unix.INLCR|unix.IGNCR|unix.ICRNL|unix.IXON|unix.IXOFF|unix.IXANY) != 0 {
		t.Fatalf("真实 gadget 输入转换未关闭: %#x", raw.Iflag)
	}
	if raw.Oflag&unix.OPOST != 0 {
		t.Fatalf("真实 gadget 输出转换未关闭: %#x", raw.Oflag)
	}
	if raw.Lflag&(unix.ECHO|unix.ECHONL|unix.ICANON|unix.ISIG|unix.IEXTEN) != 0 {
		t.Fatalf("真实 gadget 规范模式或回显未关闭: %#x", raw.Lflag)
	}
	if raw.Cflag&unix.CSIZE != unix.CS8 || raw.Cflag&(unix.CREAD|unix.CLOCAL) != unix.CREAD|unix.CLOCAL {
		t.Fatalf("真实 gadget 字符模式不是 8-bit raw: %#x", raw.Cflag)
	}
	if raw.Cc[unix.VMIN] != 1 || raw.Cc[unix.VTIME] != 0 {
		t.Fatalf("真实 gadget VMIN/VTIME 错误: %d/%d", raw.Cc[unix.VMIN], raw.Cc[unix.VTIME])
	}
	statusFlags, err := unix.FcntlInt(uintptr(fileDescriptor), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if statusFlags&unix.O_NONBLOCK == 0 {
		t.Fatalf("真实 gadget 未保留 O_NONBLOCK: %#x", statusFlags)
	}
	if err := port.Close(); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Restore(); err != nil {
		t.Fatal(err)
	}

	restoredDescriptor, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(restoredDescriptor)
	restored, err := unix.IoctlGetTermios(restoredDescriptor, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Iflag&(unix.ICRNL|unix.IXON|unix.IXOFF|unix.IXANY) != unix.ICRNL|unix.IXON|unix.IXOFF|unix.IXANY ||
		restored.Oflag&unix.OPOST == 0 || restored.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != unix.ICANON|unix.ECHO|unix.ISIG {
		t.Fatalf("退出时没有恢复真实 gadget 原终端参数: iflag=%#x oflag=%#x lflag=%#x", restored.Iflag, restored.Oflag, restored.Lflag)
	}
}

// TestPTYHelperProcess 在子测试进程中模拟只打开固定 ttyGS0 的原厂程序。
func TestPTYHelperProcess(t *testing.T) {
	if os.Getenv("CASESCREEN_PTY_HELPER") != "1" {
		return
	}
	port, err := os.OpenFile(os.Getenv("CASESCREEN_PTY_DEVICE"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	reader := bufio.NewReader(port)
	for {
		packet, err := protocol.ReadPacket(reader)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte(nil)
		if packet.Command == protocol.CommandFindDevice {
			payload = protocol.EncodeQStringMap(map[string]string{
				"screen_width": "1024", "screen_height": "600", "screen_inch": "7",
			})
		}
		ack := protocol.EncodePacket(protocol.Packet{
			Type: protocol.PacketTypeCommandAck, Command: packet.Command, Payload: payload,
		})
		if _, err := port.Write(ack); err != nil {
			t.Fatal(err)
		}
		if packet.Command == protocol.CommandSystemData {
			return
		}
	}
}

func TestProxyUsesPTYAndRestoresAbsentGadgetNode(t *testing.T) {
	directory := t.TempDir()
	devicePath := filepath.Join(directory, "ttyGS0")
	proxy, err := Start(context.Background(), Options{
		DevicePath: devicePath, BackupPath: devicePath + ".backup",
		LockPath:    filepath.Join(directory, "display.lock"),
		ProgramPath: os.Args[0], ProgramArgs: []string{"-test.run=^TestPTYHelperProcess$"},
		WorkingDirectory: directory,
		Environment:      []string{"CASESCREEN_PTY_HELPER=1", "CASESCREEN_PTY_DEVICE=" + devicePath},
		ReadyTimeout:     5 * time.Second, AckTimeout: time.Second, StopTimeout: time.Second,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proxy.DeviceInfo()["screen_width"] != "1024" {
		t.Fatalf("PTY 握手没有收到设备信息: %#v", proxy.DeviceInfo())
	}
	if err := proxy.SendLyricsUpdate(context.Background(), LyricsUpdate{
		Source: "qq", TrackID: "mid", Position: int64(time.Second), Playing: true,
		LyricMode: LyricModeTimed, Current: "当前句", Next: "下一句",
	}); err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := proxy.Wait(waitContext); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(devicePath); !os.IsNotExist(err) {
		t.Fatalf("原本不存在的 gadget 节点退出后未清理: %v", err)
	}
	if _, err := os.Lstat(devicePath + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("退出后残留设备备份: %v", err)
	}
}
