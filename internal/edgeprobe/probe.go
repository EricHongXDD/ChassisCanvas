// Package edgeprobe 生成不包含网络凭据和账号信息的屏端硬件诊断报告。
package edgeprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const reportVersion = 2

const (
	networkModePhysical        = "physical"
	networkModeUSBGadgetBridge = "usb-gadget-bridge"
	usbBridgeInterface         = "usb0"
	usbBridgeGateway           = "192.168.77.1"
)

const (
	defaultArecordPath       = "/usr/bin/arecord"
	defaultSampleDuration    = time.Second
	defaultSampleTimeout     = 4 * time.Second
	defaultMinimumSignalRMS  = 0.002
	maximumRecognitionConfig = 16 * 1024
	maximumCapturedWAV       = 2 * 1024 * 1024
)

var (
	safePCMDevice            = regexp.MustCompile(`^[A-Za-z0-9_:,.-]{1,128}$`)
	safeFingerprintAlgorithm = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	safeFingerprintVersion   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
)

// CommandRunner 让短采样可以在测试中使用不会启动外部进程的实现。
type CommandRunner interface {
	Run(ctx context.Context, executable string, arguments ...string) error
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, executable string, arguments ...string) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	// arecord 正常模式不需要标准输出；限制错误输出，避免异常设备无限占用内存。
	var stderr limitedBuffer
	command.Stdout = io.Discard
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("arecord 失败: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type limitedBuffer struct{ bytes.Buffer }

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	const maximum = 4096
	original := len(data)
	remaining := maximum - buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = buffer.Buffer.Write(data)
	}
	return original, nil
}

// Options 控制真机短采样及识歌 relay 配置门禁。所有路径均是屏端绝对路径；
// root 不是 "/" 时会映射到测试根目录，报告不会记录 URL、令牌或 PCM 设备名。
type Options struct {
	PCMDevice             string
	ArecordPath           string
	CABundlePath          string
	SampleDuration        time.Duration
	SampleTimeout         time.Duration
	MinimumSignalRMS      float64
	TemporaryDirectory    string
	RecognitionConfigPath string
	Runner                CommandRunner
}

// RecognitionReadiness 是屏端独立识歌的失败关闭门禁。
type RecognitionReadiness struct {
	Ready   bool     `json:"ready"`
	Reasons []string `json:"reasons"`
}

// NetworkInterface 只记录接口类型和运行状态，不记录 MAC、IP、SSID 或流量。
type NetworkInterface struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	OperState string `json:"oper_state,omitempty"`
	Driver    string `json:"driver,omitempty"`
}

// BusDevice 是用于确认板载无线芯片是否枚举的非敏感总线信息。
type BusDevice struct {
	Name   string `json:"name"`
	Vendor string `json:"vendor,omitempty"`
	Device string `json:"device,omitempty"`
	Driver string `json:"driver,omitempty"`
}

// RFKillDevice 描述内核 rfkill 开关，不保存用户配置。
type RFKillDevice struct {
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`
	Enabled bool   `json:"enabled"`
}

// Checks 汇总独立歌词链路所需的可验证前置条件。
type Checks struct {
	WiFiInterface              bool `json:"wifi_interface"`
	WiFiDriverArtifact         bool `json:"wifi_driver_artifact"`
	BluetoothAdapter           bool `json:"bluetooth_adapter"`
	BluetoothInitializer       bool `json:"bluetooth_initializer"`
	BluetoothFirmware          bool `json:"bluetooth_firmware"`
	SystemDBus                 bool `json:"system_dbus"`
	BlueZDaemon                bool `json:"bluez_daemon"`
	DefaultRoute               bool `json:"default_route"`
	DNSConfigured              bool `json:"dns_configured"`
	CABundle                   bool `json:"ca_bundle"`
	ClockPlausible             bool `json:"clock_plausible"`
	UART1                      bool `json:"uart1"`
	ALSACard                   bool `json:"alsa_card"`
	Arecord                    bool `json:"arecord"`
	AudioCaptureReady          bool `json:"audio_capture_ready"`
	AudioSignalDetected        bool `json:"audio_signal_detected"`
	IndependentNetwork         bool `json:"independent_network"`
	DefaultRouteIndependent    bool `json:"default_route_independent"`
	TrustedTime                bool `json:"trusted_time"`
	RecognitionRelayURL        bool `json:"recognition_relay_url"`
	RecognitionDeviceToken     bool `json:"recognition_device_token"`
	RecognitionTransportReady  bool `json:"recognition_transport_ready"`
	TrustedUSBGadgetBridge     bool `json:"trusted_usb_gadget_bridge"`
	DirectLyricsReady          bool `json:"direct_lyrics_ready"`
	BluetoothMetadataReady     bool `json:"bluetooth_metadata_ready"`
	StandaloneRecognitionReady bool `json:"standalone_recognition_ready"`
}

// Report 是写到 TF 卡的脱敏诊断结果。
type Report struct {
	Version                int                  `json:"version"`
	GeneratedAt            string               `json:"generated_at"`
	KernelRelease          string               `json:"kernel_release,omitempty"`
	DeviceModel            string               `json:"device_model,omitempty"`
	Interfaces             []NetworkInterface   `json:"interfaces"`
	SDIODevices            []BusDevice          `json:"sdio_devices"`
	BluetoothAdapters      []string             `json:"bluetooth_adapters"`
	RFKill                 []RFKillDevice       `json:"rfkill"`
	Checks                 Checks               `json:"checks"`
	AudioSampleRMS         float64              `json:"audio_sample_rms,omitempty"`
	RecognitionTransport   string               `json:"recognition_transport,omitempty"`
	RecognitionNetworkMode string               `json:"recognition_network_mode,omitempty"`
	StandaloneRecognition  RecognitionReadiness `json:"standalone_recognition"`
	Warnings               []string             `json:"warnings"`
}

// Collect 从给定根目录读取 Linux 伪文件系统。root 主要用于测试；真机传入 "/"。
func Collect(root string, now time.Time) (Report, error) {
	return collect(context.Background(), root, now, Options{}, false)
}

// CollectWithOptions 在静态检查之外执行一次有超时的单声道 PCM 短采样，并验证独立识歌配置。
func CollectWithOptions(ctx context.Context, root string, now time.Time, options Options) (Report, error) {
	if ctx == nil {
		return Report{}, errors.New("诊断上下文不能为空")
	}
	return collect(ctx, root, now, options, true)
}

func collect(ctx context.Context, root string, now time.Time, options Options, dynamic bool) (Report, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return Report{}, errors.New("诊断根目录无效")
	}
	report := Report{
		Version: reportVersion, GeneratedAt: now.UTC().Format(time.RFC3339),
		Interfaces: []NetworkInterface{}, SDIODevices: []BusDevice{},
		BluetoothAdapters: []string{}, RFKill: []RFKillDevice{}, Warnings: []string{},
		StandaloneRecognition: RecognitionReadiness{Reasons: []string{}},
	}
	report.KernelRelease = readText(root, "/proc/sys/kernel/osrelease", 512)
	report.DeviceModel = readText(root, "/proc/device-tree/model", 512)
	report.Interfaces = collectInterfaces(root)
	report.SDIODevices = collectBusDevices(root, "/sys/bus/sdio/devices")
	report.BluetoothAdapters = collectNames(root, "/sys/class/bluetooth", "hci")
	report.RFKill = collectRFKill(root)

	checks := Checks{}
	for _, item := range report.Interfaces {
		if item.Kind == "wifi" {
			checks.WiFiInterface = true
		}
	}
	checks.WiFiDriverArtifact = containsFilename(root, "/lib/modules", []string{"8723ds", "rtw_8723ds", "rtl8723ds"})
	checks.BluetoothAdapter = len(report.BluetoothAdapters) > 0
	checks.BluetoothInitializer = anyExists(root, []string{
		"/usr/bin/rtk_hciattach", "/usr/sbin/rtk_hciattach", "/bin/rtk_hciattach", "/sbin/rtk_hciattach",
	})
	checks.BluetoothFirmware = containsFilename(root, "/lib/firmware", []string{"rtl8723d", "8723d_fw", "rtlbt"})
	checks.SystemDBus = anyExists(root, []string{"/run/dbus/system_bus_socket", "/var/run/dbus/system_bus_socket"})
	checks.BlueZDaemon = processRunning(root, "bluetoothd")
	checks.DefaultRoute = hasDefaultRoute(root)
	checks.DNSConfigured = hasNameserver(root)
	checks.CABundle = anyNonEmptyFile(root, []string{
		"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt",
	})
	if customCA := normalizeAbsoluteDevicePath(options.CABundlePath); customCA != "" {
		// 显式传入 CA 时核心 HTTP 客户端不会回退系统证书，因此门禁也必须
		// 验证同一个文件，不能被另一个系统 CA 文件误放行。
		checks.CABundle = validPEMCABundle(root, customCA)
	}
	checks.ClockPlausible = !now.Before(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	checks.UART1 = exists(root, "/dev/ttyS1")
	checks.ALSACard = hasALSACard(root)
	arecordPath := normalizeArecordPath(options.ArecordPath)
	checks.Arecord = regularExecutable(root, arecordPath)
	defaultRouteInterface := defaultRouteInterface(root)
	checks.IndependentNetwork = hasIndependentNetwork(report.Interfaces)
	checks.DefaultRouteIndependent = defaultRouteInterface != "" && interfaceUpIndependent(report.Interfaces, defaultRouteInterface)
	checks.TrustedTime = checks.ClockPlausible && hasTrustedTime(root)
	audioCaptureReason := ""
	config := recognitionConfigStatus{NetworkMode: networkModePhysical, NetworkModeReady: true}
	if dynamic {
		config = readRecognitionConfig(root, options.RecognitionConfigPath)
		checks.RecognitionRelayURL = config.URLReady
		checks.RecognitionDeviceToken = config.TokenReady
		checks.RecognitionTransportReady = !config.FingerprintRequested || config.FingerprintReady
		report.RecognitionTransport = config.Transport
		report.RecognitionNetworkMode = config.NetworkMode
		if config.NetworkMode == networkModeUSBGadgetBridge && config.NetworkModeReady {
			checks.TrustedUSBGadgetBridge = interfaceUp(report.Interfaces, usbBridgeInterface) &&
				defaultRouteMatches(root, usbBridgeInterface, usbBridgeGateway)
		}
		if checks.ALSACard && checks.Arecord {
			rms, captured, sampleErr := captureRMS(ctx, root, arecordPath, options)
			if sampleErr == nil {
				report.AudioSampleRMS = math.Round(rms*1_000_000) / 1_000_000
				checks.AudioCaptureReady = captured
				checks.AudioSignalDetected = rms >= normalizedMinimumRMS(options.MinimumSignalRMS)
			} else {
				audioCaptureReason = classifyCaptureError(sampleErr)
			}
		}
	}
	checks.DirectLyricsReady = checks.WiFiInterface && checks.DefaultRoute && checks.DNSConfigured && checks.CABundle && checks.ClockPlausible
	checks.BluetoothMetadataReady = checks.BluetoothAdapter && checks.SystemDBus && checks.BlueZDaemon
	networkReady := checks.IndependentNetwork && checks.DefaultRouteIndependent
	if config.NetworkMode == networkModeUSBGadgetBridge {
		networkReady = config.NetworkModeReady && checks.TrustedUSBGadgetBridge
	}
	checks.StandaloneRecognitionReady = dynamic && checks.ALSACard && checks.Arecord &&
		checks.AudioCaptureReady && networkReady &&
		checks.DNSConfigured && checks.CABundle && checks.TrustedTime &&
		checks.RecognitionRelayURL && checks.RecognitionDeviceToken && checks.RecognitionTransportReady
	report.Checks = checks
	report.StandaloneRecognition = RecognitionReadiness{
		Ready:   checks.StandaloneRecognitionReady,
		Reasons: standaloneRecognitionReasons(checks, dynamic, audioCaptureReason, config),
	}
	report.Warnings = warnings(checks)
	return report, nil
}

// IdleNotice 返回可直接显示在歌词控件中的首要诊断提示。
func IdleNotice(report Report) string {
	if report.StandaloneRecognition.Ready {
		return "独立识歌已就绪，正在聆听"
	}
	if len(report.StandaloneRecognition.Reasons) > 0 &&
		report.StandaloneRecognition.Reasons[0] != "未执行独立识歌动态门禁" {
		return report.StandaloneRecognition.Reasons[0] + "；详情已写入 TF 诊断报告"
	}
	if len(report.Warnings) > 0 {
		return report.Warnings[0] + "；详情已写入 TF 诊断报告"
	}
	return "请连接已配对的播放器并开始播放"
}

// WriteAtomic 以 0600 权限原子写入报告；报告中不含 SSID、PSK、Cookie 或账号。
func WriteAtomic(path string, report Report) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return errors.New("硬件报告路径无效")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".hardware-report-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return os.Chmod(path, 0o600)
}

func collectInterfaces(root string) []NetworkInterface {
	base := hostPath(root, "/sys/class/net")
	entries, err := os.ReadDir(base)
	if err != nil {
		return []NetworkInterface{}
	}
	result := make([]NetworkInterface, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name())
		if name == "" || name == "lo" {
			continue
		}
		// bridge、VLAN、TUN 等虚拟 netdev 没有指向总线/平台设备的 device 链接。
		// 不能仅凭名称和 operstate 把 br-lan 误判成可独立联网的物理以太网。
		kind := "virtual"
		if exists(root, "/sys/class/net/"+name+"/device") {
			kind = "ethernet"
		}
		if kind != "virtual" && (exists(root, "/sys/class/net/"+name+"/wireless") ||
			strings.HasPrefix(strings.ToLower(name), "wlan")) {
			kind = "wifi"
		}
		driver := symlinkBase(root, "/sys/class/net/"+name+"/device/driver")
		driverLower := strings.ToLower(driver)
		if strings.Contains(driverLower, "rndis") || strings.Contains(driverLower, "g_ether") ||
			strings.Contains(driverLower, "u_ether") || strings.Contains(driverLower, "usb_f") {
			kind = "usb-gadget"
		}
		result = append(result, NetworkInterface{
			Name: name, Kind: kind,
			OperState: readText(root, "/sys/class/net/"+name+"/operstate", 64),
			Driver:    driver,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

func collectBusDevices(root, path string) []BusDevice {
	entries, err := os.ReadDir(hostPath(root, path))
	if err != nil {
		return []BusDevice{}
	}
	result := make([]BusDevice, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		result = append(result, BusDevice{
			Name:   name,
			Vendor: readText(root, path+"/"+name+"/vendor", 64),
			Device: readText(root, path+"/"+name+"/device", 64),
			Driver: symlinkBase(root, path+"/"+name+"/driver"),
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

func collectNames(root, path, prefix string) []string {
	entries, err := os.ReadDir(hostPath(root, path))
	if err != nil {
		return []string{}
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			result = append(result, entry.Name())
		}
	}
	sort.Strings(result)
	return result
}

func collectRFKill(root string) []RFKillDevice {
	base := "/sys/class/rfkill"
	entries, err := os.ReadDir(hostPath(root, base))
	if err != nil {
		return []RFKillDevice{}
	}
	result := make([]RFKillDevice, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		soft := readText(root, base+"/"+name+"/soft", 8)
		hard := readText(root, base+"/"+name+"/hard", 8)
		result = append(result, RFKillDevice{
			Name: name, Type: readText(root, base+"/"+name+"/type", 64),
			Enabled: soft != "1" && hard != "1",
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

type recognitionConfigStatus struct {
	URLReady             bool
	TokenReady           bool
	FingerprintRequested bool
	FingerprintReady     bool
	Transport            string
	NetworkMode          string
	NetworkModeReady     bool
}

func normalizeArecordPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return defaultArecordPath
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

func normalizeAbsoluteDevicePath(path string) string {
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if path == "." || !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

func normalizedMinimumRMS(value float64) float64 {
	if value <= 0 || value > 1 {
		return defaultMinimumSignalRMS
	}
	return value
}

func hasALSACard(root string) bool {
	if data := readText(root, "/proc/asound/cards", 64*1024); data != "" && data != "--- no soundcards ---" {
		for _, line := range strings.Split(data, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				if _, err := strconv.Atoi(fields[0]); err == nil {
					return true
				}
			}
		}
	}
	entries, err := os.ReadDir(hostPath(root, "/sys/class/sound"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "card") && len(name) > len("card") {
			if _, err := strconv.Atoi(name[len("card"):]); err == nil {
				return true
			}
		}
	}
	return false
}

func regularExecutable(root, path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(hostPath(root, path))
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

func validPEMCABundle(root, path string) bool {
	info, err := os.Stat(hostPath(root, path))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4*1024*1024 {
		return false
	}
	data, err := os.ReadFile(hostPath(root, path))
	return err == nil && bytes.Contains(data, []byte("-----BEGIN CERTIFICATE-----")) &&
		bytes.Contains(data, []byte("-----END CERTIFICATE-----"))
}

func arm32ELFExecutable(root, path string) bool {
	if !regularExecutable(root, path) {
		return false
	}
	file, err := os.Open(hostPath(root, path))
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) &&
		header[4] == 1 && // ELFCLASS32
		header[5] == 1 && // ELFDATA2LSB
		header[6] == 1 && // 当前 ELF 版本
		binary.LittleEndian.Uint16(header[18:20]) == 40 // EM_ARM
}

func defaultRouteInterface(root string) string {
	data := readText(root, "/proc/net/route", 64*1024)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 64)
		if err == nil && flags&0x2 != 0 {
			return fields[0]
		}
	}
	return ""
}

func hasIndependentNetwork(interfaces []NetworkInterface) bool {
	for _, item := range interfaces {
		if isIndependentInterface(interfaces, item.Name) && strings.EqualFold(item.OperState, "up") {
			return true
		}
	}
	return false
}

func isIndependentInterface(interfaces []NetworkInterface, name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || lower == "lo" || strings.HasPrefix(lower, "usb") ||
		strings.HasPrefix(lower, "rndis") || strings.HasPrefix(lower, "gadget") {
		return false
	}
	for _, item := range interfaces {
		if item.Name == name {
			return item.Kind == "wifi" || item.Kind == "ethernet"
		}
	}
	return false
}

func interfaceUpIndependent(interfaces []NetworkInterface, name string) bool {
	if !isIndependentInterface(interfaces, name) {
		return false
	}
	return interfaceUp(interfaces, name)
}

func interfaceUp(interfaces []NetworkInterface, name string) bool {
	for _, item := range interfaces {
		if item.Name == name {
			return strings.EqualFold(item.OperState, "up")
		}
	}
	return false
}

func defaultRouteMatches(root, wantedInterface, wantedGateway string) bool {
	wantedAddress, err := netip.ParseAddr(strings.TrimSpace(wantedGateway))
	if err != nil || !wantedAddress.Is4() {
		return false
	}
	data := readText(root, "/proc/net/route", 64*1024)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != wantedInterface || fields[1] != "00000000" {
			continue
		}
		flags, flagErr := strconv.ParseUint(fields[3], 16, 64)
		gatewayValue, gatewayErr := strconv.ParseUint(fields[2], 16, 32)
		if flagErr != nil || gatewayErr != nil || flags&0x2 == 0 {
			continue
		}
		gateway := netip.AddrFrom4([4]byte{
			byte(gatewayValue), byte(gatewayValue >> 8), byte(gatewayValue >> 16), byte(gatewayValue >> 24),
		})
		if gateway == wantedAddress {
			return true
		}
	}
	return false
}

func hasTrustedTime(root string) bool {
	for _, marker := range []string{
		"/run/casescreen/time-synchronized",
		"/run/systemd/timesync/synchronized",
		"/var/lib/systemd/timesync/clock",
	} {
		if exists(root, marker) {
			return true
		}
	}
	// 同步守护进程常会在无网络、DNS 失败或尚未收到首个 NTP 响应时常驻；
	// “进程存在”不能证明 TLS 所需的系统时间已经可信。
	return false
}

func readRecognitionConfig(root, configPath string) recognitionConfigStatus {
	status := recognitionConfigStatus{
		Transport:        "pcm_json",
		NetworkMode:      networkModePhysical,
		NetworkModeReady: true,
	}
	configPath = filepath.ToSlash(filepath.Clean(strings.TrimSpace(configPath)))
	if configPath == "" || configPath == "." || !strings.HasPrefix(configPath, "/") {
		return status
	}
	info, err := os.Stat(hostPath(root, configPath))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumRecognitionConfig {
		return status
	}
	data, err := os.ReadFile(hostPath(root, configPath))
	if err != nil {
		return status
	}
	values := make(map[string]string)
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || value == "" || strings.ContainsAny(value, "\x00\r\n") {
			continue
		}
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 2 {
			value = strings.Trim(value[1:len(value)-1], " ")
		}
		switch key {
		case "CASESCREEN_RECOGNITION_RELAY_URL",
			"CASESCREEN_RECOGNITION_TOKEN_FILE",
			"CASESCREEN_RECOGNITION_FINGERPRINT_EXTRACTOR",
			"CASESCREEN_RECOGNITION_FINGERPRINT_ALGORITHM",
			"CASESCREEN_RECOGNITION_FINGERPRINT_VERSION",
			"CASESCREEN_RECOGNITION_NETWORK_MODE",
			"CASESCREEN_RECOGNITION_NETWORK_INTERFACE",
			"CASESCREEN_RECOGNITION_NETWORK_GATEWAY":
			values[key] = value
		}
	}
	if configuredMode := strings.TrimSpace(values["CASESCREEN_RECOGNITION_NETWORK_MODE"]); configuredMode != "" {
		status.NetworkMode = configuredMode
	}
	switch status.NetworkMode {
	case networkModePhysical:
		status.NetworkModeReady = true
	case networkModeUSBGadgetBridge:
		gateway, gatewayErr := netip.ParseAddr(strings.TrimSpace(values["CASESCREEN_RECOGNITION_NETWORK_GATEWAY"]))
		status.NetworkModeReady = strings.TrimSpace(values["CASESCREEN_RECOGNITION_NETWORK_INTERFACE"]) == usbBridgeInterface &&
			gatewayErr == nil && gateway.Is4() && gateway.String() == usbBridgeGateway
	default:
		status.NetworkModeReady = false
	}
	parsed, err := url.Parse(values["CASESCREEN_RECOGNITION_RELAY_URL"])
	status.URLReady = err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" &&
		!strings.Contains(values["CASESCREEN_RECOGNITION_RELAY_URL"], "#")
	tokenPath := filepath.ToSlash(filepath.Clean(values["CASESCREEN_RECOGNITION_TOKEN_FILE"]))
	if strings.HasPrefix(tokenPath, "/") {
		tokenInfo, tokenErr := os.Stat(hostPath(root, tokenPath))
		if tokenErr == nil && tokenInfo.Mode().IsRegular() && tokenInfo.Size() <= 256 &&
			(runtime.GOOS == "windows" || tokenInfo.Mode().Perm()&0o077 == 0) {
			token, readErr := os.ReadFile(hostPath(root, tokenPath))
			tokenText := strings.TrimSuffix(strings.TrimSuffix(string(token), "\n"), "\r")
			status.TokenReady = readErr == nil && regexp.MustCompile(`^[0-9a-fA-F]{64}$`).MatchString(tokenText)
		}
	}
	rawExtractorPath := strings.TrimSpace(values["CASESCREEN_RECOGNITION_FINGERPRINT_EXTRACTOR"])
	extractorPath := normalizeAbsoluteDevicePath(rawExtractorPath)
	algorithm := strings.TrimSpace(values["CASESCREEN_RECOGNITION_FINGERPRINT_ALGORITHM"])
	version := strings.TrimSpace(values["CASESCREEN_RECOGNITION_FINGERPRINT_VERSION"])
	status.FingerprintRequested = rawExtractorPath != "" || algorithm != "" || version != ""
	if status.FingerprintRequested {
		status.Transport = "fingerprint"
		status.FingerprintReady = extractorPath != "" && safeFingerprintAlgorithm.MatchString(algorithm) &&
			safeFingerprintVersion.MatchString(version) && arm32ELFExecutable(root, extractorPath)
	}
	return status
}

func captureRMS(ctx context.Context, root, arecordPath string, options Options) (float64, bool, error) {
	device := strings.TrimSpace(options.PCMDevice)
	if !safePCMDevice.MatchString(device) {
		return 0, false, errors.New("PCM 设备名无效")
	}
	duration := options.SampleDuration
	if duration <= 0 {
		duration = defaultSampleDuration
	}
	if duration < 250*time.Millisecond || duration > 5*time.Second {
		return 0, false, errors.New("采样时长必须在 250 毫秒..5 秒之间")
	}
	timeout := options.SampleTimeout
	if timeout <= 0 {
		timeout = defaultSampleTimeout
	}
	if timeout < duration || timeout > 15*time.Second {
		return 0, false, errors.New("采样超时必须覆盖采样时长且不超过 15 秒")
	}
	temporaryDirectory := strings.TrimSpace(options.TemporaryDirectory)
	if temporaryDirectory == "" {
		temporaryDirectory = os.TempDir()
	} else {
		unixAbsolute := strings.HasPrefix(filepath.ToSlash(temporaryDirectory), "/")
		if !filepath.IsAbs(temporaryDirectory) && !unixAbsolute {
			return 0, false, errors.New("采样临时目录必须是绝对路径")
		}
		if root != "/" {
			temporaryDirectory = hostPath(root, temporaryDirectory)
		}
	}
	if err := os.MkdirAll(temporaryDirectory, 0o700); err != nil {
		return 0, false, err
	}
	temporary, err := os.CreateTemp(temporaryDirectory, ".casescreen-audio-probe-*.wav")
	if err != nil {
		return 0, false, err
	}
	temporaryPath := temporary.Name()
	if closeErr := temporary.Close(); closeErr != nil {
		_ = os.Remove(temporaryPath)
		return 0, false, closeErr
	}
	if err := os.Remove(temporaryPath); err != nil {
		return 0, false, err
	}
	defer os.Remove(temporaryPath)

	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	executable := arecordPath
	if root != "/" {
		executable = hostPath(root, arecordPath)
	}
	sampleCount := int(math.Ceil(duration.Seconds() * 16000))
	sampleContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := runner.Run(sampleContext, executable,
		"--device", device,
		"--quiet",
		"--format", "S16_LE",
		"--channels", "1",
		"--rate", "16000",
		"--samples", strconv.Itoa(sampleCount),
		"--file-type", "wav",
		temporaryPath,
	); err != nil {
		return 0, false, err
	}
	info, err := os.Stat(temporaryPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 44 || info.Size() > maximumCapturedWAV {
		return 0, false, errors.New("arecord 没有生成有效的有界 WAV 文件")
	}
	data, err := os.ReadFile(temporaryPath)
	if err != nil {
		return 0, false, err
	}
	rms, samples, err := pcm16WAVRMS(data)
	return rms, samples > 0, err
}

func pcm16WAVRMS(data []byte) (float64, int, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, 0, errors.New("采样文件不是 RIFF/WAVE")
	}
	formatPCM := false
	channels := uint16(0)
	bits := uint16(0)
	var pcm []byte
	for offset := 12; offset+8 <= len(data); {
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + chunkSize
		if chunkSize < 0 || end < start || end > len(data) {
			return 0, 0, errors.New("WAV 区块长度无效")
		}
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if chunkSize >= 16 {
				formatPCM = binary.LittleEndian.Uint16(data[start:start+2]) == 1
				channels = binary.LittleEndian.Uint16(data[start+2 : start+4])
				bits = binary.LittleEndian.Uint16(data[start+14 : start+16])
			}
		case "data":
			pcm = data[start:end]
		}
		offset = end + chunkSize%2
	}
	if !formatPCM || channels != 1 || bits != 16 || len(pcm) < 2 || len(pcm)%2 != 0 {
		return 0, 0, errors.New("采样必须是单声道 PCM S16_LE")
	}
	var sum float64
	count := len(pcm) / 2
	for offset := 0; offset < len(pcm); offset += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[offset:offset+2]))) / 32768
		sum += sample * sample
	}
	return math.Sqrt(sum / float64(count)), count, nil
}

func classifyCaptureError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "指定 PCM 设备短采样超时"
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "PCM 设备名"):
		return "PCM 设备名无效"
	case strings.Contains(text, "单声道 PCM"), strings.Contains(text, "RIFF/WAVE"), strings.Contains(text, "WAV"):
		return "arecord 返回的采样格式无效"
	case strings.Contains(text, "arecord"):
		return "arecord 无法从指定 PCM 设备采样"
	default:
		return "指定 PCM 设备短采样失败"
	}
}

func standaloneRecognitionReasons(
	checks Checks,
	dynamic bool,
	audioCaptureReason string,
	config recognitionConfigStatus,
) []string {
	result := make([]string, 0, 10)
	if !dynamic {
		return append(result, "未执行独立识歌动态门禁")
	}
	if !config.NetworkModeReady {
		result = append(result, "识歌网络模式配置无效")
	} else if config.NetworkMode == networkModeUSBGadgetBridge {
		if !checks.TrustedUSBGadgetBridge {
			result = append(result, "专用 USB 网络桥未就绪或默认路由不匹配")
		}
	} else {
		if !checks.IndependentNetwork {
			result = append(result, "未发现已启用的独立网络接口（USB/RNDIS 不计入）")
		}
		if !checks.DefaultRouteIndependent {
			result = append(result, "默认路由不在独立网络接口上")
		}
	}
	items := []struct {
		ready  bool
		reason string
	}{
		{checks.ALSACard, "未发现 ALSA 声卡"},
		{checks.Arecord, "缺少可执行的 arecord"},
		{checks.AudioCaptureReady, firstNonEmpty(audioCaptureReason, "指定 PCM 设备短采样失败")},
		{checks.DNSConfigured, "DNS 尚未配置"},
		{checks.CABundle, "缺少 HTTPS 根证书"},
		{checks.TrustedTime, "系统时间尚未由可信同步服务确认"},
		{checks.RecognitionRelayURL, "识歌 relay 必须配置为有效 HTTPS 地址"},
		{checks.RecognitionDeviceToken, "缺少有效的每屏设备令牌"},
		{checks.RecognitionTransportReady, "已配置的声纹提取器必须是可执行的 ARM32 ELF，且算法与版本标识完整"},
	}
	for _, item := range items {
		if !item.ready {
			result = append(result, item.reason)
		}
	}
	return result
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func warnings(checks Checks) []string {
	result := make([]string, 0, 8)
	if !checks.WiFiInterface {
		result = append(result, "未发现 Wi-Fi 接口")
	}
	if !checks.BluetoothAdapter {
		result = append(result, "未发现蓝牙 HCI 适配器")
	}
	if !checks.BluetoothInitializer {
		result = append(result, "缺少 Realtek 蓝牙初始化程序")
	}
	if !checks.BluetoothFirmware {
		result = append(result, "缺少候选蓝牙固件")
	}
	if !checks.CABundle {
		result = append(result, "缺少 HTTPS 根证书")
	}
	if !checks.DefaultRoute {
		result = append(result, "没有默认网络路由")
	}
	if !checks.DNSConfigured {
		result = append(result, "DNS 尚未配置")
	}
	if !checks.ClockPlausible {
		result = append(result, "系统时间不正确")
	}
	return result
}

func hasDefaultRoute(root string) bool {
	data := readText(root, "/proc/net/route", 64*1024)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 64)
		if err == nil && flags&0x2 != 0 {
			return true
		}
	}
	return false
}

func hasNameserver(root string) bool {
	data := readText(root, "/etc/resolv.conf", 64*1024)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if strings.HasPrefix(line, "nameserver ") && len(strings.Fields(line)) >= 2 {
			return true
		}
	}
	return false
}

func processRunning(root, wanted string) bool {
	entries, err := os.ReadDir(hostPath(root, "/proc"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		if readText(root, "/proc/"+entry.Name()+"/comm", 256) == wanted {
			return true
		}
	}
	return false
}

func containsFilename(root, path string, needles []string) bool {
	base := hostPath(root, path)
	found := false
	_ = filepath.WalkDir(base, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := strings.ToLower(entry.Name())
		for _, needle := range needles {
			if strings.Contains(name, strings.ToLower(needle)) {
				found = true
				return fs.SkipAll
			}
		}
		return nil
	})
	return found
}

func anyExists(root string, paths []string) bool {
	for _, path := range paths {
		if exists(root, path) {
			return true
		}
	}
	return false
}

func anyNonEmptyFile(root string, paths []string) bool {
	for _, path := range paths {
		info, err := os.Stat(hostPath(root, path))
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return true
		}
	}
	return false
}

func exists(root, path string) bool {
	_, err := os.Stat(hostPath(root, path))
	return err == nil
}

func symlinkBase(root, path string) string {
	target, err := os.Readlink(hostPath(root, path))
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Clean(target))
}

func readText(root, path string, maximum int64) string {
	file, err := os.Open(hostPath(root, path))
	if err != nil {
		return ""
	}
	defer file.Close()
	data := make([]byte, maximum+1)
	count, _ := file.Read(data)
	if int64(count) > maximum {
		count = int(maximum)
	}
	return strings.Trim(strings.TrimSpace(string(data[:count])), "\x00")
}

func hostPath(root, path string) string {
	clean := filepath.FromSlash(strings.TrimLeft(path, "/\\"))
	return filepath.Join(root, clean)
}

func (report Report) String() string {
	return fmt.Sprintf("edgeprobe v%d: direct=%t bluetooth=%t warnings=%d", report.Version,
		report.Checks.DirectLyricsReady, report.Checks.BluetoothMetadataReady, len(report.Warnings))
}
