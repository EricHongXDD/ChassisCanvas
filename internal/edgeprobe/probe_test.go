package edgeprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type wavRunner struct {
	data  []byte
	err   error
	paths []string
}

func (runner *wavRunner) Run(_ context.Context, _ string, arguments ...string) error {
	path := arguments[len(arguments)-1]
	runner.paths = append(runner.paths, path)
	if runner.err != nil {
		return runner.err
	}
	return os.WriteFile(path, runner.data, 0o600)
}

type timeoutRunner struct{ paths []string }

func (runner *timeoutRunner) Run(ctx context.Context, _ string, arguments ...string) error {
	runner.paths = append(runner.paths, arguments[len(arguments)-1])
	<-ctx.Done()
	return ctx.Err()
}

func TestCollectReportsReadyWithoutSecrets(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "/proc/sys/kernel/osrelease", "5.4.61\n")
	writeFixture(t, root, "/proc/device-tree/model", "Waveshare 7inch A\x00")
	writeFixture(t, root, "/sys/class/net/wlan0/operstate", "up\n")
	makeFixtureDir(t, root, "/sys/class/net/wlan0/wireless")
	makeFixtureDir(t, root, "/sys/class/net/wlan0/device")
	if runtime.GOOS != "windows" {
		if err := os.Symlink("../../drivers/8723ds", fixturePath(root, "/sys/class/net/wlan0/device/driver")); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, root, "/lib/modules/5.4.61/8723ds.ko", "module")
	writeFixture(t, root, "/lib/firmware/rtlbt/rtl8723d_fw", "firmware")
	writeFixture(t, root, "/usr/bin/rtk_hciattach", "binary")
	writeFixture(t, root, "/sys/class/bluetooth/hci0/address", "00:00:00:00:00:00")
	writeFixture(t, root, "/run/dbus/system_bus_socket", "socket-placeholder")
	writeFixture(t, root, "/proc/123/comm", "bluetoothd\n")
	writeFixture(t, root, "/proc/net/route", "Iface Destination Gateway Flags\nwlan0 00000000 01010101 0003\n")
	writeFixture(t, root, "/etc/resolv.conf", "nameserver 192.0.2.53\n")
	writeFixture(t, root, "/etc/ssl/certs/ca-certificates.crt", "certificate")
	writeFixture(t, root, "/dev/ttyS1", "")
	// Windows 测试目录不能包含冒号；采集器只把目录名当作脱敏总线标签。
	writeFixture(t, root, "/sys/bus/sdio/devices/mmc1_0001_1/vendor", "0x024c")
	writeFixture(t, root, "/sys/bus/sdio/devices/mmc1_0001_1/device", "0xd723")
	writeFixture(t, root, "/sys/class/rfkill/rfkill0/type", "wlan")
	writeFixture(t, root, "/sys/class/rfkill/rfkill0/soft", "0")
	writeFixture(t, root, "/sys/class/rfkill/rfkill0/hard", "0")

	report, err := Collect(root, time.Date(2026, 8, 27, 10, 0, 0, 0, time.FixedZone("CST", 8*3600)))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Checks.DirectLyricsReady || !report.Checks.BluetoothMetadataReady {
		t.Fatalf("独立链路应已就绪: %#v", report.Checks)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("就绪报告不应有警告: %#v", report.Warnings)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"192.0.2.53", "Cookie", "password", "psk", "ssid"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("报告泄露敏感字段 %q: %s", forbidden, text)
		}
	}
}

func TestCollectMissingOriginalImageDependencies(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "/proc/sys/kernel/osrelease", "5.4.61")
	writeFixture(t, root, "/dev/ttyS1", "")
	report, err := Collect(root, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if report.Checks.DirectLyricsReady || report.Checks.BluetoothMetadataReady {
		t.Fatalf("缺少硬件依赖时不能报告就绪: %#v", report.Checks)
	}
	if got := IdleNotice(report); !strings.Contains(got, "Wi-Fi") || !strings.Contains(got, "TF") {
		t.Fatalf("空闲提示不明确: %q", got)
	}
}

func TestWriteAtomicUsesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "latest.json")
	report := Report{Version: 1, Interfaces: []NetworkInterface{}, Warnings: []string{"测试"}}
	if err := WriteAtomic(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("报告内容错误: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("报告权限过宽: %o", info.Mode().Perm())
		}
	}
}

func TestCollectWithOptionsReportsStandaloneRecognitionReady(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "/proc/asound/cards", " 0 [audiocodec]: sunxi-codec - audiocodec\n")
	writeFixture(t, root, "/usr/bin/arecord", "binary")
	if err := os.Chmod(fixturePath(root, "/usr/bin/arecord"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "/sys/class/net/wlan0/operstate", "up\n")
	makeFixtureDir(t, root, "/sys/class/net/wlan0/wireless")
	makeFixtureDir(t, root, "/sys/class/net/wlan0/device")
	writeFixture(t, root, "/proc/net/route", "Iface Destination Gateway Flags\nwlan0 00000000 01010101 0003\n")
	writeFixture(t, root, "/etc/resolv.conf", "nameserver 192.0.2.53\n")
	writeFixture(t, root, "/data/casescreen/ca-bundle.pem", "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n")
	writeFixture(t, root, "/run/casescreen/time-synchronized", "ok\n")
	writeFixture(t, root, "/data/casescreen/recognition.token", strings.Repeat("a", 64)+"\n")
	writeFixture(t, root, "/data/casescreen/recognition.env", strings.Join([]string{
		"CASESCREEN_RECOGNITION_RELAY_URL=https://recognition.example.test/v1/recognize",
		"CASESCREEN_RECOGNITION_TOKEN_FILE=/data/casescreen/recognition.token",
	}, "\n"))
	runner := &wavRunner{data: makePCM16WAV([]int16{0, 8000, -8000, 16000, -16000})}
	report, err := CollectWithOptions(context.Background(), root,
		time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC), Options{
			PCMDevice:             "hw:0,0",
			SampleDuration:        250 * time.Millisecond,
			SampleTimeout:         time.Second,
			TemporaryDirectory:    "/tmp",
			RecognitionConfigPath: "/data/casescreen/recognition.env",
			CABundlePath:          "/data/casescreen/ca-bundle.pem",
			Runner:                runner,
		})
	if err != nil {
		t.Fatal(err)
	}
	if !report.StandaloneRecognition.Ready || !report.Checks.StandaloneRecognitionReady {
		t.Fatalf("独立识歌门禁应通过: checks=%#v reasons=%#v", report.Checks, report.StandaloneRecognition.Reasons)
	}
	if report.RecognitionTransport != "pcm_json" || !report.Checks.RecognitionTransportReady {
		t.Fatalf("未配置声纹提取器时应明确采用 PCM JSON 兼容模式: %#v", report)
	}
	if got := IdleNotice(report); !strings.Contains(got, "正在聆听") {
		t.Fatalf("独立识歌就绪后不应显示缺少蓝牙等旧模式警告: %q", got)
	}
	if report.AudioSampleRMS <= 0 || !report.Checks.AudioSignalDetected {
		t.Fatalf("应记录脱敏 RMS 和声音活动: %#v", report)
	}
	if len(runner.paths) != 1 {
		t.Fatalf("短采样调用次数错误: %#v", runner.paths)
	}
	if _, err := os.Stat(runner.paths[0]); !os.IsNotExist(err) {
		t.Fatalf("采样临时文件必须清理: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"recognition.example.test", strings.Repeat("a", 64), "hw:0,0"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("诊断报告泄露配置或令牌: %s", encoded)
		}
	}
}

func TestFingerprintTransportRequiresValidARM32Extractor(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "wlan0")
	extractorPath := fixturePath(root, "/data/casescreen/fingerprint-extractor")
	if err := os.WriteFile(extractorPath, makeARM32ELF(), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "/data/casescreen/recognition.env", strings.Join([]string{
		"CASESCREEN_RECOGNITION_RELAY_URL=https://recognition.example.test/v1/recognize",
		"CASESCREEN_RECOGNITION_TOKEN_FILE=/data/casescreen/recognition.token",
		"CASESCREEN_RECOGNITION_FINGERPRINT_EXTRACTOR=/data/casescreen/fingerprint-extractor",
		"CASESCREEN_RECOGNITION_FINGERPRINT_ALGORITHM=vendor-acr",
		"CASESCREEN_RECOGNITION_FINGERPRINT_VERSION=1",
	}, "\n"))
	options := Options{
		PCMDevice: "plughw:audiocodec", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	}
	report, err := CollectWithOptions(context.Background(), root, time.Now(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.StandaloneRecognition.Ready || !report.Checks.RecognitionTransportReady ||
		report.RecognitionTransport != "fingerprint" {
		t.Fatalf("有效 ARM32 提取器应启用声纹传输: %#v", report)
	}

	if err := os.Remove(extractorPath); err != nil {
		t.Fatal(err)
	}
	report, err = CollectWithOptions(context.Background(), root, time.Now(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.StandaloneRecognition.Ready || report.Checks.RecognitionTransportReady ||
		report.RecognitionTransport != "fingerprint" {
		t.Fatalf("显式配置但缺失提取器时必须失败关闭: %#v", report)
	}
	if reasons := strings.Join(report.StandaloneRecognition.Reasons, "|"); !strings.Contains(reasons, "ARM32 ELF") {
		t.Fatalf("缺少明确的声纹提取器失败原因: %s", reasons)
	}
}

func TestRecognitionRelayURLRejectsQuery(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "wlan0")
	writeFixture(t, root, "/data/casescreen/recognition.env", strings.Join([]string{
		"CASESCREEN_RECOGNITION_RELAY_URL=https://recognition.example.test/v1/recognize?secret=leak",
		"CASESCREEN_RECOGNITION_TOKEN_FILE=/data/casescreen/recognition.token",
	}, "\n"))
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "plughw:audiocodec", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Checks.RecognitionRelayURL || report.StandaloneRecognition.Ready {
		t.Fatalf("relay URL 查询串可能夹带秘密，必须拒绝: %#v", report.Checks)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "leak") {
		t.Fatalf("报告不能泄露 URL 查询串: %s", encoded)
	}
}

func TestExplicitCABundleCannotFallBackToAnotherSystemFile(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "wlan0")
	writeFixture(t, root, "/data/casescreen/ca-bundle.pem", "not-a-pem")
	writeFixture(t, root, "/etc/ssl/certs/ca-certificates.crt", "system-ca-present")
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "plughw:audiocodec", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Checks.CABundle || report.StandaloneRecognition.Ready {
		t.Fatalf("显式 CA 无效时不能用其他系统证书文件误放行: %#v", report.Checks)
	}
}

func TestSilentCaptureIsReadyButSignalIsInactive(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "wlan0")
	runner := &wavRunner{data: makePCM16WAV(make([]int16, 64))}
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "plughw:0,0", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem", Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Checks.AudioCaptureReady || report.Checks.AudioSignalDetected {
		t.Fatalf("静音应证明采样链可用，但不应伪造声音活动: %#v", report.Checks)
	}
	if !report.StandaloneRecognition.Ready {
		t.Fatalf("安静环境不能阻止独立识歌服务启动: %#v", report.StandaloneRecognition.Reasons)
	}
}

func TestRecognitionReadinessRejectsUSBGadgetRouteAndUntrustedClock(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "usb0")
	// 移除可信时间标记，证明“年份合理”本身不足以通过 TLS 时间门禁。
	if err := os.Remove(fixturePath(root, "/run/casescreen/time-synchronized")); err != nil {
		t.Fatal(err)
	}
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "hw:0,0", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.StandaloneRecognition.Ready {
		t.Fatalf("仅 USB Gadget 默认路由且时间未同步时不能放行: %#v", report.Checks)
	}
	joined := strings.Join(report.StandaloneRecognition.Reasons, "|")
	for _, expected := range []string{"独立网络接口", "默认路由", "可信同步"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("缺少明确失败原因 %q: %s", expected, joined)
		}
	}
}

func TestRecognitionReadinessAcceptsExplicitDedicatedUSBGadgetBridge(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "usb0")
	writeFixture(t, root, "/proc/net/route", "Iface Destination Gateway Flags\nusb0 00000000 014DA8C0 0003\n")
	writeFixture(t, root, "/data/casescreen/recognition.env", strings.Join([]string{
		"CASESCREEN_RECOGNITION_RELAY_URL=https://recognition.example.test/v1/recognize",
		"CASESCREEN_RECOGNITION_TOKEN_FILE=/data/casescreen/recognition.token",
		"CASESCREEN_RECOGNITION_NETWORK_MODE=usb-gadget-bridge",
		"CASESCREEN_RECOGNITION_NETWORK_INTERFACE=usb0",
		"CASESCREEN_RECOGNITION_NETWORK_GATEWAY=192.168.77.1",
	}, "\n"))
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "hw:0,0", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Checks.TrustedUSBGadgetBridge || !report.StandaloneRecognition.Ready {
		t.Fatalf("显式配置且网关匹配的专用 USB 网络桥应通过门禁: %#v", report)
	}
	if report.RecognitionNetworkMode != "usb-gadget-bridge" {
		t.Fatalf("诊断报告应记录非敏感网络模式: %#v", report)
	}
}

func TestRecognitionReadinessRejectsMismatchedUSBGadgetBridge(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "usb0")
	writeFixture(t, root, "/proc/net/route", "Iface Destination Gateway Flags\nusb0 00000000 01010101 0003\n")
	writeFixture(t, root, "/data/casescreen/recognition.env", strings.Join([]string{
		"CASESCREEN_RECOGNITION_RELAY_URL=https://recognition.example.test/v1/recognize",
		"CASESCREEN_RECOGNITION_TOKEN_FILE=/data/casescreen/recognition.token",
		"CASESCREEN_RECOGNITION_NETWORK_MODE=usb-gadget-bridge",
		"CASESCREEN_RECOGNITION_NETWORK_INTERFACE=usb0",
		"CASESCREEN_RECOGNITION_NETWORK_GATEWAY=192.168.77.1",
	}, "\n"))
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "hw:0,0", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Checks.TrustedUSBGadgetBridge || report.StandaloneRecognition.Ready {
		t.Fatalf("网关不匹配时专用 USB 网络桥必须失败关闭: %#v", report)
	}
	if reasons := strings.Join(report.StandaloneRecognition.Reasons, "|"); !strings.Contains(reasons, "USB 网络桥") {
		t.Fatalf("缺少专用网络桥失败原因: %s", reasons)
	}
}

func TestRecognitionReadinessRejectsBridgeWithoutPhysicalDevice(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "br-lan")
	// bridge 是虚拟 netdev，不应具有指向总线或平台设备的 device 链接。
	if err := os.RemoveAll(fixturePath(root, "/sys/class/net/br-lan/device")); err != nil {
		t.Fatal(err)
	}
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "hw:0,0", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Checks.IndependentNetwork || report.Checks.DefaultRouteIndependent ||
		report.StandaloneRecognition.Ready {
		t.Fatalf("没有物理 device 的 bridge 不能通过独立网络门禁: %#v", report.Checks)
	}
	if len(report.Interfaces) != 1 || report.Interfaces[0].Kind != "virtual" {
		t.Fatalf("bridge 应明确标为虚拟接口: %#v", report.Interfaces)
	}
}

func TestRecognitionReadinessRejectsRunningNTPWithoutSyncMarker(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "wlan0")
	if err := os.Remove(fixturePath(root, "/run/casescreen/time-synchronized")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "/proc/321/comm", "ntpd\n")
	report, err := CollectWithOptions(context.Background(), root, time.Now(), Options{
		PCMDevice: "hw:0,0", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: time.Second, TemporaryDirectory: "/tmp",
		RecognitionConfigPath: "/data/casescreen/recognition.env",
		CABundlePath:          "/data/casescreen/ca-bundle.pem",
		Runner:                &wavRunner{data: makePCM16WAV([]int16{1000, -1000})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Checks.TrustedTime || report.StandaloneRecognition.Ready {
		t.Fatalf("仅有 ntpd 进程、没有成功同步 marker 时不能通过可信时间门禁: %#v", report.Checks)
	}
	if reasons := strings.Join(report.StandaloneRecognition.Reasons, "|"); !strings.Contains(reasons, "可信同步") {
		t.Fatalf("缺少明确的可信时间失败原因: %s", reasons)
	}
}

func TestCaptureTimeoutCleansTemporaryFile(t *testing.T) {
	root := t.TempDir()
	prepareMinimumRecognitionFixture(t, root, "wlan0")
	runner := &timeoutRunner{}
	_, _, err := captureRMS(context.Background(), root, defaultArecordPath, Options{
		PCMDevice: "hw:0,0", SampleDuration: 250 * time.Millisecond,
		SampleTimeout: 250 * time.Millisecond, TemporaryDirectory: "/tmp", Runner: runner,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应安全终止采样: %v", err)
	}
	if len(runner.paths) != 1 {
		t.Fatalf("采样调用次数错误: %#v", runner.paths)
	}
	if _, statErr := os.Stat(runner.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("超时后临时文件必须清理: %v", statErr)
	}
}

func prepareMinimumRecognitionFixture(t *testing.T, root, interfaceName string) {
	t.Helper()
	writeFixture(t, root, "/proc/asound/cards", " 0 [audiocodec]: sunxi-codec - audiocodec\n")
	writeFixture(t, root, "/usr/bin/arecord", "binary")
	if err := os.Chmod(fixturePath(root, "/usr/bin/arecord"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "/sys/class/net/"+interfaceName+"/operstate", "up\n")
	makeFixtureDir(t, root, "/sys/class/net/"+interfaceName+"/device")
	if strings.HasPrefix(interfaceName, "wlan") {
		makeFixtureDir(t, root, "/sys/class/net/"+interfaceName+"/wireless")
	}
	writeFixture(t, root, "/proc/net/route", "Iface Destination Gateway Flags\n"+interfaceName+" 00000000 01010101 0003\n")
	writeFixture(t, root, "/etc/resolv.conf", "nameserver 192.0.2.53\n")
	writeFixture(t, root, "/data/casescreen/ca-bundle.pem", "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n")
	writeFixture(t, root, "/run/casescreen/time-synchronized", "ok\n")
	writeFixture(t, root, "/data/casescreen/recognition.token", strings.Repeat("b", 64)+"\n")
	writeFixture(t, root, "/data/casescreen/recognition.env", strings.Join([]string{
		"CASESCREEN_RECOGNITION_RELAY_URL=https://recognition.example.test/v1/recognize",
		"CASESCREEN_RECOGNITION_TOKEN_FILE=/data/casescreen/recognition.token",
	}, "\n"))
}

func makePCM16WAV(samples []int16) []byte {
	dataSize := len(samples) * 2
	data := make([]byte, 44+dataSize)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(36+dataSize))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 16000)
	binary.LittleEndian.PutUint32(data[28:32], 32000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(dataSize))
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(data[44+index*2:46+index*2], uint16(sample))
	}
	return bytes.Clone(data)
}

func makeARM32ELF() []byte {
	header := make([]byte, 52)
	copy(header[:4], []byte{0x7f, 'E', 'L', 'F'})
	header[4] = 1 // ELFCLASS32
	header[5] = 1 // ELFDATA2LSB
	header[6] = 1 // 当前 ELF 版本
	binary.LittleEndian.PutUint16(header[16:18], 2)
	binary.LittleEndian.PutUint16(header[18:20], 40) // EM_ARM
	return header
}

func writeFixture(t *testing.T, root, path, text string) {
	t.Helper()
	full := fixturePath(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeFixtureDir(t *testing.T, root, path string) {
	t.Helper()
	if err := os.MkdirAll(fixturePath(root, path), 0o700); err != nil {
		t.Fatal(err)
	}
}

func fixturePath(root, path string) string {
	return filepath.Join(root, filepath.FromSlash(strings.TrimLeft(path, "/")))
}
