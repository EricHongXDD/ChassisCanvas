package waveshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/serialport"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

const (
	waveshareStorageMiB = uint64(1024 * 1024)
	// DefaultBrightness 是原厂协议的满亮恢复值。关屏只临时发送 0，
	// 恢复时回到这个设备默认值，不在 Service 中硬编码型号细节。
	DefaultBrightness = 255
)

// Driver 使用屏内 PCMonitorLinux 渲染，日常只发小型键值包。
type Driver struct {
	ID                   string
	Name                 string
	Resolution           [2]int
	Port                 io.ReadWriteCloser
	Opener               func() (io.ReadWriteCloser, error)
	AckGate              protocol.AckGate
	OpenError            error
	ActivationDelay      time.Duration
	StorageTimeout       time.Duration
	CommandTimeout       time.Duration
	ReconnectSettleDelay time.Duration

	mu           sync.Mutex
	metaMu       sync.RWMutex
	storageMu    sync.RWMutex
	telemetryMu  sync.RWMutex
	spectrumMu   sync.RWMutex
	backgroundMu sync.Mutex
	connected    bool
	deviceFiles  map[string]uint32
	storage      device.StorageInfo
	// lastTelemetryPayload 只在收到 ACK 后更新；当前/下一句完全相同时
	// 可以直接跳过，重新连接或切换主题时会清空并强制重发。
	lastTelemetryPayload  []byte
	lastSpectrumPayload   []byte
	lastSpectrumStartedAt time.Time
	spectrumClock         func() time.Time
	backgroundCancel      context.CancelFunc
	backgroundVersion     uint64
}

func New(id, name string, resolution [2]int, port io.ReadWriteCloser, gate protocol.AckGate) *Driver {
	if gate == nil && port != nil {
		gate = protocol.NewCommandAckGate(port)
	}
	return &Driver{
		ID: id, Name: name, Resolution: resolution, Port: port, AckGate: gate,
		ActivationDelay: 2 * time.Second, StorageTimeout: time.Second,
		CommandTimeout: 3 * time.Second, ReconnectSettleDelay: 350 * time.Millisecond,
		deviceFiles: make(map[string]uint32),
	}
}

func (driver *Driver) Info() device.Info {
	driver.metaMu.RLock()
	defer driver.metaMu.RUnlock()
	return device.Info{
		ID: driver.ID, Name: driver.Name, Resolution: driver.Resolution,
		Capabilities: device.Capabilities{
			DeliveryMode: device.DeliveryStoredTheme, StoresResources: true, SmallTelemetry: true,
		},
	}
}

// Connected 返回核心串口会话状态，可选的 TF 容量查询失败不会改变它。
func (driver *Driver) Connected() bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.connected
}

func (driver *Driver) Connect(ctx context.Context) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.OpenError != nil {
		if driver.Opener == nil {
			return fmt.Errorf("打开微雪串口: %w", driver.OpenError)
		}
		driver.OpenError = nil
	}
	if driver.connected {
		return nil
	}
	opened := false
	if driver.Port == nil && driver.Opener != nil {
		port, err := driver.Opener()
		if err != nil {
			driver.OpenError = err
			return fmt.Errorf("打开微雪串口: %w", err)
		}
		driver.Port = port
		driver.AckGate = protocol.NewCommandAckGate(port)
		driver.OpenError = nil
		opened = true
	}
	if driver.Port == nil {
		return errors.New("微雪串口未配置")
	}
	if driver.AckGate == nil {
		return errors.New("微雪 ACK 读取器未配置")
	}
	if opened && driver.ReconnectSettleDelay > 0 {
		if err := waitForContext(ctx, driver.ReconnectSettleDelay); err != nil {
			driver.disconnectLocked()
			return fmt.Errorf("等待微雪 USB 重新枚举稳定: %w", err)
		}
	}
	deviceInfo, err := driver.sendAndWaitLocked(ctx, protocol.CommandFindDevice, nil)
	if err != nil {
		return fmt.Errorf("发现微雪设备: %w", err)
	}
	driver.updateDeviceInfo(deviceInfo.Payload)
	themes, err := driver.sendAndWaitLocked(ctx, protocol.CommandGetThemes, nil)
	if err != nil {
		return fmt.Errorf("读取屏内主题: %w", err)
	}
	values, err := protocol.DecodeQStringMap(themes.Payload)
	if err != nil {
		driver.disconnectLocked()
		return fmt.Errorf("解析屏内主题列表: %w", err)
	}
	driver.replaceDeviceFiles(values)
	driver.connected = true
	driver.clearDynamicPayloadCaches()
	reported, storageErr := driver.storeThemeListStorage(values)
	if !reported || storageErr != nil {
		driver.refreshStorageAfterConnectLocked(ctx)
	}
	return nil
}

// RefreshStorage 查询 TF 卡容量。这是可选能力：旧固件不支持、超时
// 或返回异常数据时，只更新容量状态，不会关闭屏幕连接。
func (driver *Driver) RefreshStorage(ctx context.Context) error {
	refreshContext, finish := driver.beginBackgroundOperation(ctx)
	defer finish()
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.refreshStorageLocked(refreshContext)
}

// StorageInfo 返回最近一次容量查询的缓存快照。
func (driver *Driver) StorageInfo() device.StorageInfo {
	driver.storageMu.RLock()
	defer driver.storageMu.RUnlock()
	return driver.storage
}

func (driver *Driver) storeStorage(storage device.StorageInfo) {
	driver.storageMu.Lock()
	driver.storage = storage
	driver.storageMu.Unlock()
}

func (driver *Driver) storeStorageError(message string) {
	storage := driver.StorageInfo()
	storage.Error = message
	driver.storeStorage(storage)
}

func (driver *Driver) refreshStorageAfterConnectLocked(parent context.Context) {
	timeout := driver.StorageTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	_ = driver.refreshScreenMessageStorageLocked(ctx)
}

func (driver *Driver) refreshStorageLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !driver.connected || driver.Port == nil {
		err := errors.New("微雪屏幕未连接")
		driver.storeStorageError(err.Error())
		return err
	}
	// V2.04 实机把 TF 容量混在 3 号“主题清单”回应中，单位为 MiB；
	// 同一回应还能刷新屏内 CRC 缓存。旧版或其他固件没有这些字段时，
	// 再回退到 10 号屏幕信息命令，保持向后兼容。
	response, err := driver.sendAndWaitOptionalLocked(ctx, protocol.CommandGetThemes, nil)
	if err == nil {
		values, decodeErr := protocol.DecodeQStringMap(response.Payload)
		if decodeErr == nil {
			driver.replaceDeviceFiles(values)
			reported, storageErr := driver.storeThemeListStorage(values)
			if reported {
				return storageErr
			}
		}
	}
	return driver.refreshScreenMessageStorageLocked(ctx)
}

func (driver *Driver) refreshScreenMessageStorageLocked(ctx context.Context) error {
	response, err := driver.sendAndWaitOptionalLocked(ctx, protocol.CommandGetScreenMessage, nil)
	if err != nil {
		driver.storeStorageError(fmt.Sprintf("读取 TF 卡容量: %v", err))
		return err
	}
	values, err := protocol.DecodeQStringMap(response.Payload)
	if err != nil {
		driver.storeStorageError(fmt.Sprintf("解析 TF 卡容量: %v", err))
		return err
	}
	// 部分 7 英寸固件的 10 号命令只返回型号与分辨率，不提供容量字段。
	// 这些字段仍用于识别屏幕；容量则按“不支持”处理，而不是污染连接状态。
	driver.updateDeviceInfo(response.Payload)
	_, hasTotal := values["bytesTotal"]
	_, hasAvailable := values["bytesAvailable"]
	if !hasTotal && !hasAvailable {
		storage := driver.StorageInfo()
		if storage.Supported {
			err = fmt.Errorf("固件本次未报告 TF 卡容量（返回：%s）", storageValuesSummary(values))
			storage.Error = err.Error()
			driver.storeStorage(storage)
			return err
		}
		driver.storeStorage(device.StorageInfo{Supported: false, UpdatedAt: time.Now()})
		return nil
	}
	err = driver.storeStorageValues(values, 1, "屏幕信息")
	return err
}

func (driver *Driver) replaceDeviceFiles(values map[string]string) {
	files := make(map[string]uint32)
	for remotePath, checksumText := range values {
		if !strings.HasPrefix(remotePath, "/") {
			continue
		}
		checksum, parseErr := strconv.ParseUint(checksumText, 10, 32)
		if parseErr == nil {
			files[remotePath] = uint32(checksum)
		}
	}
	driver.deviceFiles = files
}

// storeThemeListStorage 解析微雪 V2.04 的 3 号命令容量字段。
// 固件字段名虽然叫 bytesTotal/bytesAvailable，实际值已经右移 20 位，单位是 MiB。
func (driver *Driver) storeThemeListStorage(values map[string]string) (bool, error) {
	_, hasTotal := values["bytesTotal"]
	_, hasAvailable := values["bytesAvailable"]
	if !hasTotal && !hasAvailable {
		return false, nil
	}
	multiplier := waveshareStorageMiB
	unit := strings.ToLower(strings.TrimSpace(values["storageUnit"]))
	if unit == "bytes" || unit == "byte" || unit == "b" {
		multiplier = 1
	}
	err := driver.storeStorageValues(values, multiplier, "主题清单")
	return true, err
}

func (driver *Driver) storeStorageValues(values map[string]string, multiplier uint64, source string) error {
	storage := driver.StorageInfo()
	storage.Supported = true
	total, err := parseStorageValue(values, "bytesTotal", multiplier)
	if err != nil {
		err = fmt.Errorf("%s容量：%w（固件返回：%s）", source, err, storageValuesSummary(values))
		storage.Error = err.Error()
		driver.storeStorage(storage)
		return err
	}
	available, err := parseStorageValue(values, "bytesAvailable", multiplier)
	if err != nil {
		err = fmt.Errorf("%s容量：%w（固件返回：%s）", source, err, storageValuesSummary(values))
		storage.Error = err.Error()
		driver.storeStorage(storage)
		return err
	}
	if total == 0 {
		err = errors.New("TF 卡总容量必须大于 0")
		storage.Error = err.Error()
		driver.storeStorage(storage)
		return err
	}
	if available > total {
		err = fmt.Errorf("TF 卡可用容量 %d 大于总容量 %d", available, total)
		storage.Error = err.Error()
		driver.storeStorage(storage)
		return err
	}
	driver.storeStorage(device.StorageInfo{
		Supported:      true,
		TotalBytes:     total,
		AvailableBytes: available,
		UsedBytes:      total - available,
		UpdatedAt:      time.Now(),
	})
	return nil
}

func (driver *Driver) sendAndWaitOptionalLocked(ctx context.Context, command protocol.Command, payload []byte) (protocol.Packet, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Packet{}, err
	}
	if driver.Port == nil {
		return protocol.Packet{}, errors.New("微雪串口未配置")
	}
	if driver.AckGate == nil {
		return protocol.Packet{}, errors.New("微雪 ACK 读取器未配置")
	}
	commandContext, cancel := driver.withCommandTimeout(ctx)
	defer cancel()
	packet := protocol.EncodePacket(protocol.Packet{Type: protocol.PacketTypeCommand, Command: command, Payload: payload})
	if err := writeAll(driver.Port, packet); err != nil {
		driver.disconnectIfPhysicalTransportErrorLocked(err)
		return protocol.Packet{}, fmt.Errorf("微雪命令 %s 写入失败: %w", command, err)
	}
	response, err := driver.AckGate.Wait(commandContext, command, "")
	if err != nil {
		driver.disconnectIfPhysicalTransportErrorLocked(err)
		return protocol.Packet{}, fmt.Errorf("微雪命令 %s 等待 ACK 失败: %w", command, err)
	}
	return response, nil
}

func parseStorageValue(values map[string]string, key string, multiplier uint64) (uint64, error) {
	text, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("TF 卡容量回应缺少 %s", key)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("TF 卡容量字段 %s 无效: %q", key, text)
	}
	if multiplier == 0 || value > ^uint64(0)/multiplier {
		return 0, fmt.Errorf("TF 卡容量字段 %s 超出范围: %q", key, text)
	}
	return value * multiplier, nil
}

// storageValuesSummary 保留旧固件容量响应的实际字段，便于兼容不同版本；
// 单个值限制长度，避免异常固件把状态接口膨胀成大文本。
func storageValuesSummary(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(values[key])
		if len(value) > 64 {
			value = value[:64] + "…"
		}
		parts = append(parts, key+"="+strconv.Quote(value))
	}
	if len(parts) == 0 {
		return "空映射"
	}
	return strings.Join(parts, ", ")
}

func (driver *Driver) ApplyTheme(ctx context.Context, manifest theme.Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Port == nil {
		return errors.New("微雪屏幕未连接")
	}

	var themeRemotePath string
	uploaded := false
	for _, resource := range manifest.Resources {
		if resource.RemotePath == "" {
			continue
		}
		file, err := os.Open(resource.LocalPath)
		if err != nil {
			return err
		}
		_, checksum, inspectErr := protocol.InspectSource(file)
		if inspectErr != nil {
			_ = file.Close()
			return inspectErr
		}
		if strings.EqualFold(filepath.Ext(resource.RemotePath), ".Theme") {
			themeRemotePath = resource.RemotePath
		}
		deviceChecksum, exists := driver.deviceFiles[resource.RemotePath]
		if exists && deviceChecksum == checksum {
			if closeErr := file.Close(); closeErr != nil {
				return closeErr
			}
			continue
		}
		uploader := protocol.Uploader{Writer: driver.Port, Gate: driver.AckGate}
		_, uploadErr := uploader.Upload(ctx, resource.RemotePath, file)
		closeErr := file.Close()
		if uploadErr != nil {
			driver.disconnectLocked()
			return uploadErr
		}
		if closeErr != nil {
			return closeErr
		}
		driver.deviceFiles[resource.RemotePath] = checksum
		uploaded = true
	}
	if themeRemotePath == "" {
		return errors.New("微雪主题缺少 .Theme 资源")
	}
	if uploaded && driver.ActivationDelay > 0 {
		timer := time.NewTimer(driver.ActivationDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if _, err := driver.sendAndWaitLocked(ctx, protocol.CommandReloadTheme, []byte(themeRemotePath)); err != nil {
		return err
	}
	driver.clearDynamicPayloadCaches()
	if !manifestHasVisibleLyrics(manifest) {
		return nil
	}

	// 原厂 PCMonitorLinux 在创建进度型控件时会先写入 50。歌词面板、
	// 边框和底轨使用 LV 进度控件实现透明隐藏，因此主题刚重载时必须
	// 立即显式归零，不能等待下一次遥测或取词结果，否则等待歌词期间
	// 会短暂或持续显示半透明残影。
	clearPayload := protocol.EncodeQStringMap(waveshareTelemetryValues(telemetry.Snapshot{
		OmitHardwareMetrics: true,
		OmitClock:           true,
		LyricsClear:         true,
	}))
	if _, err := driver.sendAndWaitLocked(ctx, protocol.CommandSystemData, clearPayload); err != nil {
		return fmt.Errorf("初始化微雪歌词透明状态: %w", err)
	}
	driver.storeTelemetryPayload(clearPayload)
	return nil
}

func manifestHasVisibleLyrics(manifest theme.Manifest) bool {
	for _, layer := range manifest.Layers {
		if layer.Type == theme.LayerLyrics && layer.Opacity > 0 && theme.LayerVisible(layer) {
			return true
		}
	}
	return false
}

func (driver *Driver) SendTelemetry(ctx context.Context, snapshot telemetry.Snapshot) error {
	payload := protocol.EncodeQStringMap(waveshareTelemetryValues(snapshot))
	if driver.telemetryPayloadEqual(payload) {
		return nil
	}
	// 容量查询是可选后台任务；暂停、切句等新显示包必须优先。
	driver.cancelBackgroundOperation()
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Port == nil {
		return errors.New("微雪屏幕未连接")
	}
	if driver.telemetryPayloadEqual(payload) {
		return nil
	}
	_, err := driver.sendAndWaitLocked(ctx, protocol.CommandSystemData, payload)
	if err == nil {
		driver.storeTelemetryPayload(payload)
	}
	return err
}

func (driver *Driver) DefaultBrightness() int {
	return DefaultBrightness
}

func (driver *Driver) SetBrightness(ctx context.Context, value int) error {
	if value < 0 || value > 255 {
		return fmt.Errorf("亮度必须在 0..255 之间")
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Port == nil {
		return errors.New("微雪屏幕未连接")
	}
	if _, err := driver.sendAndWaitLocked(ctx, protocol.CommandSetBrightness, []byte(strconv.Itoa(value))); err != nil {
		return fmt.Errorf("设置微雪屏幕亮度: %w", err)
	}
	return nil
}

func (driver *Driver) send(ctx context.Context, command protocol.Command, payload []byte) (protocol.Packet, error) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.sendAndWaitLocked(ctx, command, payload)
}

func (driver *Driver) sendAndWaitLocked(ctx context.Context, command protocol.Command, payload []byte) (protocol.Packet, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Packet{}, err
	}
	if driver.Port == nil {
		return protocol.Packet{}, errors.New("微雪串口未配置")
	}
	if driver.AckGate == nil {
		return protocol.Packet{}, errors.New("微雪 ACK 读取器未配置")
	}
	commandContext, cancel := driver.withCommandTimeout(ctx)
	defer cancel()
	packet := protocol.EncodePacket(protocol.Packet{Type: protocol.PacketTypeCommand, Command: command, Payload: payload})
	if err := writeAll(driver.Port, packet); err != nil {
		driver.disconnectLocked()
		return protocol.Packet{}, fmt.Errorf("微雪命令 %s 写入失败: %w", command, err)
	}
	response, err := driver.AckGate.Wait(commandContext, command, "")
	if err != nil {
		driver.disconnectLocked()
		return protocol.Packet{}, fmt.Errorf("微雪命令 %s 等待 ACK 失败: %w", command, err)
	}
	return response, nil
}

func (driver *Driver) withCommandTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := driver.CommandTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return context.WithTimeout(parent, timeout)
}

func (driver *Driver) disconnectIfPhysicalTransportErrorLocked(err error) {
	kind, ok := serialport.KindOf(err)
	if ok && (kind == serialport.ErrorKindDisconnected || kind == serialport.ErrorKindNotPresent) {
		driver.disconnectLocked()
	}
}

func waitForContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (driver *Driver) updateDeviceInfo(payload []byte) {
	values, err := protocol.DecodeQStringMap(payload)
	if err != nil {
		return
	}
	driver.metaMu.Lock()
	defer driver.metaMu.Unlock()
	width, widthErr := strconv.Atoi(values["screen_width"])
	height, heightErr := strconv.Atoi(values["screen_height"])
	if widthErr == nil && heightErr == nil && width > 0 && height > 0 {
		driver.Resolution = [2]int{width, height}
	}
	if !isDefaultWaveshareName(driver.Name, driver.ID) {
		return
	}
	width, height = driver.Resolution[0], driver.Resolution[1]
	switch {
	case (width == 480 && height == 1920) || (width == 1920 && height == 480):
		driver.Name = "微雪 8.8 英寸"
	case (width == 1024 && height == 600) || (width == 600 && height == 1024):
		driver.Name = "微雪 7 英寸"
	default:
		inch := strings.TrimSpace(values["screen_inch"])
		if strings.Contains(inch, "8.8") {
			driver.Name = "微雪 8.8 英寸"
		} else if inch == "7" || strings.HasPrefix(inch, "7.") || strings.HasPrefix(inch, "7 ") {
			driver.Name = "微雪 7 英寸"
		}
	}
}

func isDefaultWaveshareName(name, id string) bool {
	name = strings.TrimSpace(name)
	return name == "" || name == id || name == "微雪" || name == "微雪 7 英寸" || name == "微雪 8.8 英寸" ||
		strings.HasPrefix(name, "微雪 USB Monitor")
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (driver *Driver) Close() error {
	driver.cancelBackgroundOperation()
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.connected = false
	driver.clearDynamicPayloadCaches()
	if driver.Port == nil {
		return nil
	}
	err := driver.Port.Close()
	driver.Port = nil
	return err
}

func (driver *Driver) disconnectLocked() {
	driver.connected = false
	driver.clearDynamicPayloadCaches()
	if driver.Port != nil {
		_ = driver.Port.Close()
	}
	driver.Port = nil
	driver.AckGate = nil
}

func (driver *Driver) telemetryPayloadEqual(payload []byte) bool {
	driver.telemetryMu.RLock()
	defer driver.telemetryMu.RUnlock()
	return bytes.Equal(payload, driver.lastTelemetryPayload)
}

func (driver *Driver) storeTelemetryPayload(payload []byte) {
	driver.telemetryMu.Lock()
	driver.lastTelemetryPayload = append(driver.lastTelemetryPayload[:0], payload...)
	driver.telemetryMu.Unlock()
}

func (driver *Driver) telemetryPayloadLength() int {
	driver.telemetryMu.RLock()
	defer driver.telemetryMu.RUnlock()
	return len(driver.lastTelemetryPayload)
}

func (driver *Driver) storeSpectrumPayload(payload []byte, startedAt time.Time) {
	driver.spectrumMu.Lock()
	driver.lastSpectrumPayload = append(driver.lastSpectrumPayload[:0], payload...)
	driver.lastSpectrumStartedAt = startedAt
	driver.spectrumMu.Unlock()
}

func (driver *Driver) spectrumPayloadState(payload []byte, now time.Time) (bool, bool) {
	driver.spectrumMu.RLock()
	defer driver.spectrumMu.RUnlock()
	if bytes.Equal(payload, driver.lastSpectrumPayload) {
		return true, false
	}
	minimumInterval := waveshareSpectrumMinimumInterval(len(payload))
	return false, !driver.lastSpectrumStartedAt.IsZero() && now.Sub(driver.lastSpectrumStartedAt) < minimumInterval
}

func (driver *Driver) currentSpectrumTime() time.Time {
	driver.spectrumMu.RLock()
	clock := driver.spectrumClock
	driver.spectrumMu.RUnlock()
	if clock != nil {
		return clock()
	}
	return time.Now()
}

func (driver *Driver) clearDynamicPayloadCaches() {
	driver.telemetryMu.Lock()
	driver.lastTelemetryPayload = nil
	driver.telemetryMu.Unlock()
	driver.spectrumMu.Lock()
	driver.lastSpectrumPayload = nil
	driver.lastSpectrumStartedAt = time.Time{}
	driver.spectrumMu.Unlock()
}

func (driver *Driver) beginBackgroundOperation(ctx context.Context) (context.Context, func()) {
	backgroundContext, cancel := context.WithCancel(ctx)
	driver.backgroundMu.Lock()
	driver.backgroundVersion++
	version := driver.backgroundVersion
	previousCancel := driver.backgroundCancel
	driver.backgroundCancel = cancel
	driver.backgroundMu.Unlock()
	if previousCancel != nil {
		previousCancel()
	}
	return backgroundContext, func() {
		driver.backgroundMu.Lock()
		if driver.backgroundVersion == version {
			driver.backgroundCancel = nil
		}
		driver.backgroundMu.Unlock()
		cancel()
	}
}

func (driver *Driver) cancelBackgroundOperation() {
	driver.backgroundMu.Lock()
	cancel := driver.backgroundCancel
	driver.backgroundMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
