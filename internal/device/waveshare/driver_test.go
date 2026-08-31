package waveshare

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type memoryPort struct {
	bytes.Buffer
	closed bool
}

func (port *memoryPort) Close() error {
	port.closed = true
	return nil
}

type scriptedGate struct {
	themes        map[string]string
	deviceInfo    map[string]string
	screenMessage map[string]string
	commandErrors map[protocol.Command]error
	commands      []protocol.Command
	waitHook      func(protocol.Command)
}

type priorityGate struct {
	mu             sync.Mutex
	themeRequests  int
	commands       []protocol.Command
	storageStarted chan struct{}
	storageOnce    sync.Once
}

type contextDeadlineGate struct{}

func (contextDeadlineGate) Wait(ctx context.Context, _ protocol.Command, _ string) (protocol.Packet, error) {
	<-ctx.Done()
	return protocol.Packet{}, ctx.Err()
}

func (gate *priorityGate) Wait(ctx context.Context, command protocol.Command, _ string) (protocol.Packet, error) {
	gate.mu.Lock()
	gate.commands = append(gate.commands, command)
	if command == protocol.CommandGetThemes {
		gate.themeRequests++
	}
	themeRequest := gate.themeRequests
	gate.mu.Unlock()
	if command == protocol.CommandGetThemes && themeRequest > 1 {
		gate.storageOnce.Do(func() { close(gate.storageStarted) })
		<-ctx.Done()
		return protocol.Packet{}, ctx.Err()
	}
	packet := protocol.Packet{Type: protocol.PacketTypeCommandAck, Command: command}
	switch command {
	case protocol.CommandFindDevice:
		packet.Payload = protocol.EncodeQStringMap(map[string]string{"screen_width": "1024", "screen_height": "600"})
	case protocol.CommandGetThemes, protocol.CommandGetScreenMessage:
		packet.Payload = protocol.EncodeQStringMap(map[string]string{})
	}
	return packet, nil
}

func (gate *priorityGate) count(command protocol.Command) int {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	count := 0
	for _, current := range gate.commands {
		if current == command {
			count++
		}
	}
	return count
}

func (gate *scriptedGate) Wait(_ context.Context, command protocol.Command, remotePath string) (protocol.Packet, error) {
	gate.commands = append(gate.commands, command)
	if gate.waitHook != nil {
		gate.waitHook(command)
	}
	if err := gate.commandErrors[command]; err != nil {
		return protocol.Packet{}, err
	}
	packet := protocol.Packet{Type: protocol.PacketTypeCommandAck, Command: command}
	switch command {
	case protocol.CommandFindDevice:
		deviceInfo := gate.deviceInfo
		if deviceInfo == nil {
			deviceInfo = map[string]string{"screen_width": "1024", "screen_height": "600", "screen_inch": "7"}
		}
		packet.Payload = protocol.EncodeQStringMap(deviceInfo)
	case protocol.CommandGetThemes:
		packet.Payload = protocol.EncodeQStringMap(gate.themes)
	case protocol.CommandGetScreenMessage:
		packet.Payload = protocol.EncodeQStringMap(gate.screenMessage)
	case protocol.CommandFileEnd:
		packet.Payload = protocol.EncodeQStringMap(map[string]string{"res": "1", "fileName": remotePath})
	}
	return packet, nil
}

func TestSetBrightnessUsesConfirmedCommandAndDefault(t *testing.T) {
	gate := &scriptedGate{themes: map[string]string{}}
	port := &memoryPort{}
	driver := New("wave-brightness", "微雪", [2]int{1024, 600}, port, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	port.Reset()
	gate.commands = nil

	if driver.DefaultBrightness() != 255 {
		t.Fatalf("默认恢复亮度=%d，want 255", driver.DefaultBrightness())
	}
	if err := driver.SetBrightness(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	packet, err := protocol.DecodePacket(port.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if packet.Type != protocol.PacketTypeCommand || packet.Command != protocol.CommandSetBrightness || string(packet.Payload) != "0" {
		t.Fatalf("亮度命令不符合原厂协议: type=%v command=%v payload=%q", packet.Type, packet.Command, packet.Payload)
	}
	if !reflect.DeepEqual(gate.commands, []protocol.Command{protocol.CommandSetBrightness}) {
		t.Fatalf("ACK 等待命令=%v", gate.commands)
	}
}

func TestSetBrightnessRejectsInvalidDisconnectedAndCanceledCalls(t *testing.T) {
	driver := New("wave-brightness", "微雪", [2]int{1024, 600}, &memoryPort{}, &scriptedGate{themes: map[string]string{}})
	for _, value := range []int{-1, 256} {
		if err := driver.SetBrightness(context.Background(), value); err == nil {
			t.Fatalf("越界亮度 %d 被接受", value)
		}
	}
	if err := driver.SetBrightness(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "未连接") {
		t.Fatalf("断线亮度错误不清楚: %v", err)
	}
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := driver.SetBrightness(ctx, 255); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文错误=%v，want context.Canceled", err)
	}
}

func TestApplyThemeSkipsMatchingScreenResource(t *testing.T) {
	contents := []byte("theme-contents")
	remotePath := "/data/theme/1024x600/custom.Theme"
	manifest := testManifest(t, contents, remotePath)
	gate := &scriptedGate{themes: map[string]string{remotePath: strconv.FormatUint(uint64(crc32.ChecksumIEEE(contents)), 10)}}
	port := &memoryPort{}
	driver := New("wave", "微雪", [2]int{}, port, gate)
	driver.ActivationDelay = 0
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	want := []protocol.Command{protocol.CommandFindDevice, protocol.CommandGetThemes, protocol.CommandGetScreenMessage, protocol.CommandReloadTheme}
	if !reflect.DeepEqual(gate.commands, want) {
		t.Fatalf("不应重复上传: got=%v want=%v", gate.commands, want)
	}
	if driver.Resolution != [2]int{1024, 600} {
		t.Fatalf("未读取设备分辨率: %v", driver.Resolution)
	}
	if driver.Info().Name != "微雪 7 英寸" {
		t.Fatalf("未更新 7 英寸友好名称: %q", driver.Info().Name)
	}
}

func TestApplyThemeImmediatelyHidesResidentLyricsUntilTextArrives(t *testing.T) {
	contents := []byte("theme-with-lyrics")
	remotePath := "/data/theme/1024x600/lyrics.Theme"
	manifest := testManifest(t, contents, remotePath)
	manifest.Layers = append(manifest.Layers, theme.Layer{
		ID: "lyrics", Type: theme.LayerLyrics,
		Bounds: theme.Rect{X: .05, Y: .7, Width: .9, Height: .25}, Opacity: 1,
	})
	gate := &scriptedGate{themes: map[string]string{
		remotePath: strconv.FormatUint(uint64(crc32.ChecksumIEEE(contents)), 10),
	}}
	port := &memoryPort{}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	driver.ActivationDelay = 0
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	port.Reset()
	gate.commands = nil

	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	wantCommands := []protocol.Command{protocol.CommandReloadTheme, protocol.CommandSystemData}
	if !reflect.DeepEqual(gate.commands, wantCommands) {
		t.Fatalf("歌词主题重载后必须立即发送透明初始化包: got=%v want=%v", gate.commands, wantCommands)
	}
	reader := bufio.NewReader(bytes.NewReader(port.Bytes()))
	reloadPacket, err := protocol.ReadPacket(reader)
	if err != nil {
		t.Fatal(err)
	}
	if reloadPacket.Command != protocol.CommandReloadTheme {
		t.Fatalf("首包应重载主题，实际 %v", reloadPacket.Command)
	}
	clearPacket, err := protocol.ReadPacket(reader)
	if err != nil {
		t.Fatal(err)
	}
	if clearPacket.Command != protocol.CommandSystemData {
		t.Fatalf("第二包应初始化歌词透明状态，实际 %v", clearPacket.Command)
	}
	if _, err := protocol.ReadPacket(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("出现多余初始化包: %v", err)
	}
	clearValues, err := protocol.DecodeQStringMap(clearPacket.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"CaseScreen_LyricsCurrent", "CaseScreen_LyricsNext",
		"CaseScreen_LyricsCurrentAtMs", "CaseScreen_LyricsNextAtMs",
	} {
		value, exists := clearValues[key]
		if !exists || value != "" {
			t.Errorf("清空包字段 %s=%q exists=%v，want 显式空字符串", key, value, exists)
		}
	}
	for key, expected := range map[string]string{
		telemetry.LyricsVisibleKey:        "0",
		telemetry.LyricsProgressLegacyKey: "0",
		telemetry.LyricsProgressFineKey:   "0",
		"CaseScreen_LyricsPlaying":        "false",
	} {
		if clearValues[key] != expected {
			t.Errorf("清空包字段 %s=%q，want %q", key, clearValues[key], expected)
		}
	}

	// 同一清空状态已经由主题切换确认，不应再次占用串口。
	clearSnapshot := telemetry.Snapshot{OmitHardwareMetrics: true, OmitClock: true, LyricsClear: true}
	if err := driver.SendTelemetry(context.Background(), clearSnapshot); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 1 {
		t.Fatalf("主题初始化清空包没有进入去重缓存: %d", sends)
	}

	// 暂停只冻结时间轴；已有歌词仍要恢复为可见，不能当成等待歌词隐藏。
	paused := telemetry.Snapshot{
		OmitHardwareMetrics: true, OmitClock: true,
		LyricsCurrent: "暂停在这一句", LyricsNext: "下一句",
		LyricsCurrentAtMS: 10000, LyricsNextAtMS: 15000,
		LyricsCurrentAtKnown: true, LyricsNextAtKnown: true,
		LyricsPlaying: false, LyricsMode: "timed",
		LyricsVisible: true, LyricsVisibleKnown: true,
	}
	if err := driver.SendTelemetry(context.Background(), paused); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 2 {
		t.Fatalf("暂停歌词没有覆盖初始化透明状态: %d", sends)
	}
	pausedValues := waveshareTelemetryValues(paused)
	if pausedValues[telemetry.LyricsVisibleKey] != "1" || pausedValues["CaseScreen_LyricsPlaying"] != "false" ||
		pausedValues["CaseScreen_LyricsCurrent"] != "暂停在这一句" {
		t.Fatalf("暂停歌词状态错误: %#v", pausedValues)
	}
}

func TestApplyThemeLyricsInitializationFailureDoesNotReportSuccess(t *testing.T) {
	contents := []byte("theme-with-lyrics")
	remotePath := "/data/theme/1024x600/lyrics.Theme"
	manifest := testManifest(t, contents, remotePath)
	manifest.Layers = append(manifest.Layers, theme.Layer{
		ID: "lyrics", Type: theme.LayerLyrics,
		Bounds: theme.Rect{X: .05, Y: .7, Width: .9, Height: .25}, Opacity: 1,
	})
	port := &memoryPort{}
	gate := &scriptedGate{themes: map[string]string{
		remotePath: strconv.FormatUint(uint64(crc32.ChecksumIEEE(contents)), 10),
	}}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	driver.ActivationDelay = 0
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.commandErrors = map[protocol.Command]error{protocol.CommandSystemData: errors.New("no ack")}

	err := driver.ApplyTheme(context.Background(), manifest)
	if err == nil || !strings.Contains(err.Error(), "初始化微雪歌词透明状态") {
		t.Fatalf("透明初始化失败被当成主题应用成功: %v", err)
	}
	if driver.Connected() || !port.closed {
		t.Fatalf("ACK 失败后串口会话未关闭: connected=%v closed=%v", driver.Connected(), port.closed)
	}
}

func TestManifestHasVisibleLyricsIgnoresHiddenLayers(t *testing.T) {
	base := theme.Manifest{Layers: []theme.Layer{{ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1}}}
	if !manifestHasVisibleLyrics(base) {
		t.Fatal("默认可见的歌词层未识别")
	}
	base.Layers[0].Options = map[string]any{"visible": false}
	if manifestHasVisibleLyrics(base) {
		t.Fatal("隐藏歌词层不应触发运行时初始化包")
	}
	base.Layers[0].Options = nil
	base.Layers[0].Opacity = 0
	if manifestHasVisibleLyrics(base) {
		t.Fatal("零透明度歌词层不应触发运行时初始化包")
	}
}

func TestConnectRecognizesWaveshareEightPointEightInch(t *testing.T) {
	gate := &scriptedGate{
		themes: map[string]string{},
		deviceInfo: map[string]string{
			"screen_width": "480", "screen_height": "1920", "screen_inch": "8.8",
		},
	}
	driver := New("waveshare-com8", "微雪 USB Monitor（COM8）", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	info := driver.Info()
	if info.Resolution != [2]int{480, 1920} || info.Name != "微雪 8.8 英寸" {
		t.Fatalf("8.8 英寸设备识别错误: %+v", info)
	}
	if info.ID != "waveshare-com8" {
		t.Fatalf("不应改变设备 ID: %q", info.ID)
	}
}

func TestConnectPreservesCustomWaveshareName(t *testing.T) {
	gate := &scriptedGate{
		themes: map[string]string{},
		deviceInfo: map[string]string{
			"screen_width": "480", "screen_height": "1920", "screen_inch": "8.8",
		},
	}
	driver := New("waveshare-com8", "机箱右侧屏", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if driver.Info().Name != "机箱右侧屏" {
		t.Fatalf("不应覆盖用户名称: %q", driver.Info().Name)
	}
}

func TestApplyThemeUploadsChangedResourceOnce(t *testing.T) {
	remotePath := "/data/theme/1024x600/custom.Theme"
	manifest := testManifest(t, []byte("new-theme"), remotePath)
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	driver.ActivationDelay = 0
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	want := []protocol.Command{
		protocol.CommandFindDevice, protocol.CommandGetThemes, protocol.CommandGetScreenMessage,
		protocol.CommandFileStart, protocol.CommandFileEnd, protocol.CommandReloadTheme,
		protocol.CommandReloadTheme,
	}
	if !reflect.DeepEqual(gate.commands, want) {
		t.Fatalf("上传命令不符合预期: got=%v want=%v", gate.commands, want)
	}
}

func TestApplyThemeUploadsMissingResourceWithZeroChecksum(t *testing.T) {
	remotePath := "/data/theme/1024x600/empty.Theme"
	manifest := testManifest(t, nil, remotePath)
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	driver.ActivationDelay = 0
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	want := []protocol.Command{
		protocol.CommandFindDevice, protocol.CommandGetThemes, protocol.CommandGetScreenMessage,
		protocol.CommandFileStart, protocol.CommandFileEnd, protocol.CommandReloadTheme,
	}
	if !reflect.DeepEqual(gate.commands, want) {
		t.Fatalf("屏端缺少 CRC=0 的资源时仍应首次上传: got=%v want=%v", gate.commands, want)
	}
}

func TestConnectQueriesAndCachesStorage(t *testing.T) {
	gate := &scriptedGate{
		themes: map[string]string{},
		screenMessage: map[string]string{
			"bytesTotal":     "8000000000",
			"bytesAvailable": "6123456789",
		},
	}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	storage := driver.StorageInfo()
	if !storage.Supported || storage.TotalBytes != 8000000000 || storage.AvailableBytes != 6123456789 {
		t.Fatalf("容量快照错误: %+v", storage)
	}
	if storage.UsedBytes != 1876543211 || storage.UpdatedAt.IsZero() || storage.Error != "" {
		t.Fatalf("容量派生字段错误: %+v", storage)
	}
}

func TestConnectReadsV204StorageFromThemeListMiB(t *testing.T) {
	remotePath := "/data/theme/1024x600/custom.Theme"
	gate := &scriptedGate{themes: map[string]string{
		remotePath:       "123456789",
		"bytesTotal":     "803",
		"bytesAvailable": "734",
	}}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	storage := driver.StorageInfo()
	if !storage.Supported || storage.TotalBytes != 803*waveshareStorageMiB || storage.AvailableBytes != 734*waveshareStorageMiB {
		t.Fatalf("V2.04 主题清单容量换算错误: %+v", storage)
	}
	if storage.UsedBytes != 69*waveshareStorageMiB || storage.Error != "" || storage.UpdatedAt.IsZero() {
		t.Fatalf("V2.04 主题清单容量派生错误: %+v", storage)
	}
	if driver.deviceFiles[remotePath] != 123456789 {
		t.Fatalf("读取容量时不应丢失主题 CRC: %#v", driver.deviceFiles)
	}
	want := []protocol.Command{protocol.CommandFindDevice, protocol.CommandGetThemes}
	if !reflect.DeepEqual(gate.commands, want) {
		t.Fatalf("主题清单已有容量时不应再查询 10 号命令: got=%v want=%v", gate.commands, want)
	}
}

func TestRefreshStorageUsesLiveThemeListAndRefreshesCRC(t *testing.T) {
	remotePath := "/data/theme/1024x600/custom.Theme"
	gate := &scriptedGate{themes: map[string]string{
		remotePath:       "100",
		"bytesTotal":     "803",
		"bytesAvailable": "734",
	}}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.themes = map[string]string{
		remotePath:       "200",
		"bytesTotal":     "803",
		"bytesAvailable": "733",
	}
	if err := driver.RefreshStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	storage := driver.StorageInfo()
	if storage.AvailableBytes != 733*waveshareStorageMiB || storage.UsedBytes != 70*waveshareStorageMiB {
		t.Fatalf("刷新后容量没有变化: %+v", storage)
	}
	if driver.deviceFiles[remotePath] != 200 {
		t.Fatalf("刷新容量时没有同步屏内 CRC: %#v", driver.deviceFiles)
	}
	if got := gate.commands[len(gate.commands)-1]; got != protocol.CommandGetThemes {
		t.Fatalf("V2.04 容量刷新应使用 3 号命令，实际 %v", got)
	}
}

func TestThemeListStorageCanDeclareByteUnit(t *testing.T) {
	gate := &scriptedGate{themes: map[string]string{
		"bytesTotal":     "8000000000",
		"bytesAvailable": "6123456789",
		"storageUnit":    "bytes",
	}}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	storage := driver.StorageInfo()
	if storage.TotalBytes != 8000000000 || storage.AvailableBytes != 6123456789 {
		t.Fatalf("显式 byte 单位解析错误: %+v", storage)
	}
}

func TestInvalidStorageResponseDoesNotDisconnect(t *testing.T) {
	gate := &scriptedGate{
		themes: map[string]string{},
		screenMessage: map[string]string{
			"bytesTotal":     "8000000000",
			"bytesAvailable": "not-a-number",
		},
	}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("容量异常不应使连接失败: %v", err)
	}
	storage := driver.StorageInfo()
	if !storage.Supported || storage.Error == "" {
		t.Fatalf("应记录已支持但数据无效: %+v", storage)
	}
	if !driver.Connected() {
		t.Fatal("容量数据异常不应改变连接状态")
	}
	if err := driver.SendTelemetry(context.Background(), telemetry.Snapshot{}); err != nil {
		t.Fatalf("容量数据异常后遥测仍应可用: %v", err)
	}
}

func TestUnsupportedStorageQueryDoesNotDisconnect(t *testing.T) {
	gate := &scriptedGate{
		themes: map[string]string{},
		commandErrors: map[protocol.Command]error{
			protocol.CommandGetScreenMessage: errors.New("旧固件不支持"),
		},
	}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("可选容量查询不应使连接失败: %v", err)
	}
	storage := driver.StorageInfo()
	if storage.Supported || storage.Error == "" {
		t.Fatalf("旧固件状态错误: %+v", storage)
	}
	if !driver.Connected() {
		t.Fatal("旧固件不支持容量查询时不应断开")
	}
	if err := driver.SendTelemetry(context.Background(), telemetry.Snapshot{}); err != nil {
		t.Fatalf("查询失败后遥测仍应可用: %v", err)
	}
}

func TestScreenMetadataWithoutCapacityUpdatesModelAndStaysSupportedAsOptional(t *testing.T) {
	gate := &scriptedGate{
		themes: map[string]string{},
		screenMessage: map[string]string{
			"screen_width":          "480",
			"screen_height":         "1920",
			"screen_inch":           "8.8inch",
			"upgradeToLatestMethod": "1",
		},
	}
	driver := New("waveshare-com4", "微雪 USB Monitor（COM4）", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("仅缺少可选容量字段不应使连接失败: %v", err)
	}
	info := driver.Info()
	if info.Name != "微雪 8.8 英寸" || info.Resolution != [2]int{480, 1920} {
		t.Fatalf("屏幕元数据没有更新型号或分辨率: %+v", info)
	}
	storage := driver.StorageInfo()
	if storage.Supported || storage.Error != "" || storage.UpdatedAt.IsZero() {
		t.Fatalf("不报告容量的固件应显示为可选能力不支持: %+v", storage)
	}
	if !driver.Connected() {
		t.Fatal("仅缺少可选容量字段不应断开屏幕")
	}
}

func TestRefreshStorageKeepsLastValidCapacity(t *testing.T) {
	gate := &scriptedGate{
		themes: map[string]string{},
		screenMessage: map[string]string{
			"bytesTotal":     "1000",
			"bytesAvailable": "600",
		},
	}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.commandErrors = map[protocol.Command]error{protocol.CommandGetScreenMessage: context.DeadlineExceeded}
	if err := driver.RefreshStorage(context.Background()); err == nil {
		t.Fatal("刷新失败应返回错误")
	}
	storage := driver.StorageInfo()
	if !storage.Supported || storage.TotalBytes != 1000 || storage.AvailableBytes != 600 || storage.Error == "" {
		t.Fatalf("刷新失败后应保留最近有效容量: %+v", storage)
	}
	if !driver.Connected() {
		t.Fatal("容量刷新超时不应改变连接状态")
	}
	if err := driver.SendTelemetry(context.Background(), telemetry.Snapshot{}); err != nil {
		t.Fatalf("刷新超时后遥测仍应可用: %v", err)
	}
}

func TestSendTelemetryUsesThemeNamesAndScalarSpectrum(t *testing.T) {
	port := &memoryPort{}
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	port.Reset()
	gpuLoad := 67.0
	gpuTemperature := 71.0
	gpuHotspotTemperature := 82.0
	gpuMemoryTemperature := 76.0
	snapshot := telemetry.Snapshot{
		CapturedAt:            time.Date(2026, 8, 26, 15, 4, 5, 0, time.Local),
		CPUPercent:            42,
		MemoryPercent:         58,
		MemoryUsedGiB:         12.5,
		MemoryTotalGiB:        32,
		CPUModel:              "AMD Ryzen 9",
		GPUModel:              "GeForce RTX",
		GPUPercent:            &gpuLoad,
		GPUTemperature:        &gpuTemperature,
		GPUHotspotTemperature: &gpuHotspotTemperature,
		GPUMemoryTemperature:  &gpuMemoryTemperature,
		AudioSpectrum:         "0,25,50,125,-3,bad",
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	packet, err := protocol.DecodePacket(port.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if packet.Command != protocol.CommandSystemData {
		t.Fatalf("发送了错误命令: %v", packet.Command)
	}
	values, err := protocol.DecodeQStringMap(packet.Payload)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Cpu_Freq": "42%", "Cpu使用率": "42%", "Memory_Load": "58%", "内存利用率": "58%",
		"Gpu_Load": "67%", "显卡利用率": "67%", "GPU_Temp": "71℃", "显卡温度": "71℃",
		"Cpu使用率_数值": "42", "显卡利用率_数值": "67", "内存利用率_数值": "58", "显卡温度_数值": "71",
		"GPU_Hotspot_Temp": "82℃", "显卡热点温度": "82℃", "显卡热点温度_数值": "82",
		"GPU_Memory_Temp": "76℃", "显存温度": "76℃", "显存温度_数值": "76",
		"已用内存_数值": "12.5", "可用内存_数值": "19.5",
		"Cpu_Model": "AMD Ryzen 9", "Cpu型号": "AMD Ryzen 9", "Gpu_Model": "GeForce RTX", "显卡型号": "GeForce RTX",
		"time": "15:04:05", "时间": "15:04:05", "Audio_00": "0", "Audio_01": "25",
		"Audio_02": "50", "Audio_03": "100", "Audio_04": "0", "Audio_05": "0",
		"Audio_47": "0", "Audio_Level": "17", "Audio_Peak": "100",
	}
	for key, expected := range want {
		if values[key] != expected {
			t.Errorf("字段 %s: got=%q want=%q", key, values[key], expected)
		}
	}
}

func TestSendTelemetrySendsOnlyOnLyricDisplayEvent(t *testing.T) {
	port := &memoryPort{}
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := telemetry.Snapshot{
		OmitHardwareMetrics: true, OmitClock: true,
		LyricsCurrent: "暂停在这一句", LyricsNext: "下一句",
		LyricsCurrentAtMS: 10000, LyricsNextAtMS: 15000,
		LyricsCurrentAtKnown: true, LyricsNextAtKnown: true,
		LyricsPlaying: true, LyricsMode: "timed",
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	sends := 0
	for _, command := range gate.commands {
		if command == protocol.CommandSystemData {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("同一句且已 ACK 的包应只发送一次，实际 %d 次", sends)
	}

	// 暂停既停止主机本地歌词截止时间，也保留一次 playing=false
	// 小状态事件，供协议消费者冻结时间轴。
	snapshot.LyricsPlaying = false
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends = countCommands(gate.commands, protocol.CommandSystemData); sends != 2 {
		t.Fatalf("暂停变化没有发送 playing 状态包: %d", sends)
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends = countCommands(gate.commands, protocol.CommandSystemData); sends != 2 {
		t.Fatalf("暂停后的相同包未被去重: %d", sends)
	}

	// 切句时歌词和时间戳改变，必须发送新包。
	snapshot.LyricsCurrent = "下一句"
	snapshot.LyricsCurrentAtMS = 15000
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends = countCommands(gate.commands, protocol.CommandSystemData); sends != 3 {
		t.Fatalf("切句后没有发送新包: %d", sends)
	}
}

func TestConnectClearsTelemetryDeduplicationCache(t *testing.T) {
	snapshot := telemetryCacheLifecycleSnapshot()
	port := &memoryPort{}
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)

	// 模拟上一个串口会话遗留的已确认包；新连接必须强制下一包重发。
	driver.storeTelemetryPayload(protocol.EncodeQStringMap(waveshareTelemetryValues(snapshot)))
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if driver.telemetryPayloadLength() != 0 {
		t.Fatal("连接成功后没有清空遥测去重缓存")
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 1 {
		t.Fatalf("新连接后的相同遥测包应重新发送一次，实际 %d 次", sends)
	}
}

func TestSuccessfulApplyThemeClearsTelemetryDeduplicationCache(t *testing.T) {
	snapshot := telemetryCacheLifecycleSnapshot()
	contents := []byte("theme-contents")
	remotePath := "/data/theme/1024x600/custom.Theme"
	manifest := testManifest(t, contents, remotePath)
	gate := &scriptedGate{themes: map[string]string{
		remotePath: strconv.FormatUint(uint64(crc32.ChecksumIEEE(contents)), 10),
	}}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	driver.ActivationDelay = 0
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 1 {
		t.Fatalf("主题切换前的相同包应被去重，实际 %d 次", sends)
	}

	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if driver.telemetryPayloadLength() != 0 {
		t.Fatal("主题重载成功后没有清空遥测去重缓存")
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 2 {
		t.Fatalf("主题重载后的相同遥测包应重新发送，累计实际 %d 次", sends)
	}
}

func TestCloseAndReconnectClearTelemetryDeduplicationCache(t *testing.T) {
	snapshot := telemetryCacheLifecycleSnapshot()
	firstPort := &memoryPort{}
	firstGate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, firstPort, firstGate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(firstGate.commands, protocol.CommandSystemData); sends != 1 {
		t.Fatalf("关闭前的相同包应被去重，实际 %d 次", sends)
	}

	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	if !firstPort.closed {
		t.Fatal("关闭驱动时没有关闭原串口")
	}
	if driver.telemetryPayloadLength() != 0 {
		t.Fatal("关闭驱动后没有清空遥测去重缓存")
	}

	secondPort := &memoryPort{}
	secondGate := &scriptedGate{themes: map[string]string{}}
	driver.Port = secondPort
	driver.AckGate = secondGate
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendTelemetry(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(secondGate.commands, protocol.CommandSystemData); sends != 1 {
		t.Fatalf("重连后的相同遥测包应在新会话发送一次，实际 %d 次", sends)
	}
}

func TestWaveshareTelemetryOmitsSpectrumWhenUnavailable(t *testing.T) {
	values := waveshareTelemetryValues(telemetry.Snapshot{CapturedAt: time.Now()})
	if _, ok := values["Audio_00"]; ok {
		t.Fatal("没有主机频谱时不应发送占位频带")
	}
}

func TestWaveshareTelemetryDoesNotInventMetricsWhenCollectionIsUnavailable(t *testing.T) {
	values := waveshareTelemetryValues(telemetry.Snapshot{
		CapturedAt:          time.Date(2026, 8, 26, 15, 4, 5, 0, time.Local),
		OmitHardwareMetrics: true,
		LyricsCurrent:       "当前歌词",
	})
	for _, key := range []string{
		"Cpu_Freq", "Cpu使用率", "Cpu使用率_数值",
		"Memory_Load", "内存利用率", "内存利用率_数值",
		"Gpu_Model", "显卡型号",
		"Gpu_Load", "显卡利用率", "显卡利用率_数值",
		"GPU_Temp", "显卡温度", "显卡温度_数值",
		"GPU_Hotspot_Temp", "显卡热点温度", "显卡热点温度_数值",
		"GPU_Memory_Temp", "显存温度", "显存温度_数值",
	} {
		if _, exists := values[key]; exists {
			t.Errorf("采集不可用时不应发送微雪硬件键 %s=%q", key, values[key])
		}
	}
	if values["时间"] != "15:04:05" || values["CaseScreen_LyricsCurrent"] != "当前歌词" {
		t.Fatalf("真实时钟或歌词数据被误删: %#v", values)
	}
}

func TestWaveshareTelemetryClearsUnavailableGPUScalars(t *testing.T) {
	values := waveshareTelemetryValues(telemetry.Snapshot{
		CapturedAt: time.Now(),
		// 给定型号可防止 Windows 平台回退采样器介入；本测试只验证
		// 目标卡不支持负载或温度时的屏端清理契约。
		GPUModel: "Intel(R) Graphics",
	})
	for _, key := range []string{
		"Gpu_Load", "显卡利用率",
		"GPU_Temp", "显卡温度",
		"GPU_Hotspot_Temp", "显卡热点温度",
		"GPU_Memory_Temp", "显存温度",
	} {
		if values[key] != "--" {
			t.Errorf("微雪规范/中文键未清理 %s=%q", key, values[key])
		}
	}
	for _, key := range []string{
		"显卡利用率_数值", "显卡温度_数值", "显卡热点温度_数值", "显存温度_数值",
	} {
		if values[key] != "0" {
			t.Errorf("微雪纯数值别名未归零 %s=%q", key, values[key])
		}
	}
}

func TestWaveshareTypicalTelemetryWireCost(t *testing.T) {
	gpuLoad, gpuTemperature := 64.0, 52.0
	values := waveshareTelemetryValues(telemetry.Snapshot{
		CapturedAt: time.Date(2026, 8, 28, 15, 4, 5, 0, time.Local),
		CPUPercent: 42, MemoryPercent: 58, MemoryUsedGiB: 27.4, MemoryTotalGiB: 48,
		CPUModel: "Intel(R) Core(TM) Ultra 7 270K Plus", GPUModel: "AMD Radeon RX 7900 XTX",
		GPUPercent: &gpuLoad, GPUTemperature: &gpuTemperature,
		LyricsCurrent: "当前歌词", LyricsNext: "下一句歌词",
		LyricsCurrentAtMS: 12345, LyricsNextAtMS: 16789,
		LyricsCurrentAtKnown: true, LyricsNextAtKnown: true, LyricsPlaying: true,
	})
	payload := protocol.EncodeQStringMap(values)
	onWireBytes := 38 + len(payload) + waveshareSpectrumACKBytes
	lineShareAtOneHertz := float64(onWireBytes*waveshareSerialBitsPerByte) / float64(waveshareConfiguredBaudRate)
	t.Logf("典型每秒遥测字段=%d，payload=%dB，命令+最小ACK=%dB，占115200线速=%.1f%%",
		len(values), len(payload), onWireBytes, lineShareAtOneHertz*100)
	if lineShareAtOneHertz > .30 {
		t.Fatalf("典型每秒遥测占线速过高，无法给频谱与歌词预留安全余量: %.1f%%", lineShareAtOneHertz*100)
	}
}

func TestSendSpectrumUsesOnlySmallScalarFields(t *testing.T) {
	port := &memoryPort{}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, &scriptedGate{themes: map[string]string{}})
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	port.Reset()
	if err := driver.SendSpectrum(context.Background(), []float64{-1, .5, 2}); err != nil {
		t.Fatal(err)
	}
	packet, err := protocol.DecodePacket(port.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	values, err := protocol.DecodeQStringMap(packet.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != waveshareSpectrumBands+2 {
		t.Fatalf("频谱小包字段数错误: %d", len(values))
	}
	want := map[string]string{
		"Audio_00": "0", "Audio_01": "50", "Audio_02": "100", "Audio_47": "0",
		"Audio_Level": "16", "Audio_Peak": "100",
	}
	for key, expected := range want {
		if values[key] != expected {
			t.Errorf("字段 %s: got=%q want=%q", key, values[key], expected)
		}
	}
	if _, ok := values["Cpu_Freq"]; ok {
		t.Fatal("高频频谱包不应重复发送遥测字段")
	}
}

func TestSendSpectrumDeduplicatesStableFrameAndDropsWhenSerialBusy(t *testing.T) {
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	bars := []float64{0.1, 0.2, 0.3}
	if err := driver.SendSpectrum(context.Background(), bars); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendSpectrum(context.Background(), bars); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 1 {
		t.Fatalf("静止频谱帧没有去重: %d", sends)
	}

	driver.mu.Lock()
	if err := driver.SendSpectrum(context.Background(), []float64{0.4, 0.5, 0.6}); !errors.Is(err, device.ErrRealtimeFrameDropped) {
		driver.mu.Unlock()
		t.Fatalf("串口繁忙应返回可识别的丢帧状态: %v", err)
	}
	driver.mu.Unlock()
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 1 {
		t.Fatalf("串口繁忙时频谱帧没有丢弃: %d", sends)
	}
}

func TestSendSpectrumValuesCachesOnlyAfterAck(t *testing.T) {
	ackError := errors.New("频谱 ACK 超时")
	gate := &scriptedGate{
		themes:        map[string]string{},
		commandErrors: map[protocol.Command]error{protocol.CommandSystemData: ackError},
	}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"A00": "0", "A02": "0"}
	if err := driver.SendSpectrumValues(context.Background(), values); !errors.Is(err, ackError) {
		t.Fatalf("频谱 ACK 失败没有返回给调用方: %v", err)
	}
	delete(gate.commandErrors, protocol.CommandSystemData)
	driver.Port = &memoryPort{}
	driver.AckGate = gate
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("ACK 失败断线后无法重连: %v", err)
	}
	if err := driver.SendSpectrumValues(context.Background(), values); err != nil {
		t.Fatalf("ACK 恢复后相同清零包没有重发: %v", err)
	}
	if err := driver.SendSpectrumValues(context.Background(), values); err != nil {
		t.Fatal(err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 2 {
		t.Fatalf("应在失败后重发一次，且 ACK 后去重：%d", sends)
	}
}

func TestCommandTimeoutBoundsAckWaitWithoutCallerDeadline(t *testing.T) {
	port := &memoryPort{}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, contextDeadlineGate{})
	driver.CommandTimeout = 15 * time.Millisecond
	started := time.Now()
	err := driver.Connect(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("根 context 没有截止时间时仍应触发命令硬超时: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("ACK 硬超时没有及时释放单写者: %v", elapsed)
	}
	if driver.Connected() || !port.closed || !strings.Contains(err.Error(), "find_device(0)") {
		t.Fatalf("超时后连接/诊断状态错误: connected=%v closed=%v err=%v", driver.Connected(), port.closed, err)
	}
}

func TestSpectrumUsesBoundedWireBudgetAndKeepsOnlyLatestFrame(t *testing.T) {
	port := &memoryPort{}
	gate := &scriptedGate{themes: map[string]string{}}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	clock := time.Date(2026, 8, 28, 1, 0, 0, 0, time.Local)
	driver.spectrumClock = func() time.Time { return clock }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	port.Reset()
	values := func(value string) map[string]string {
		result := make(map[string]string, 38)
		for index := 0; index < 38; index++ {
			result[fmt.Sprintf("A%02d", index)] = value
		}
		return result
	}
	first := values("10")
	if err := driver.SendSpectrumValues(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := values("20")
	if err := driver.SendSpectrumValues(context.Background(), second); !errors.Is(err, device.ErrRealtimeFrameDropped) {
		t.Fatalf("节流期的中间帧应丢弃而不是排队: %v", err)
	}
	third := values("30")
	payload := protocol.EncodeQStringMap(third)
	minimumInterval := waveshareSpectrumMinimumInterval(len(payload))
	clock = clock.Add(minimumInterval)
	if err := driver.SendSpectrumValues(context.Background(), third); err != nil {
		t.Fatalf("到达线速预算后最新帧未发送: %v", err)
	}

	onWireBytes := 38 + len(payload) + waveshareSpectrumACKBytes
	bitsPerSecond := float64(onWireBytes*waveshareSerialBitsPerByte) / minimumInterval.Seconds()
	budget := float64(waveshareConfiguredBaudRate) * waveshareSpectrumLineShare
	if minimumInterval < waveshareSpectrumFloor || bitsPerSecond > budget+1 {
		t.Fatalf("频谱线速预算失效: payload=%d interval=%v bits/s=%.1f budget=%.1f",
			len(payload), minimumInterval, bitsPerSecond, budget)
	}
	t.Logf("38 键频谱 payload=%dB，命令+最小ACK=%dB，最小间隔=%v，预算=%.1fbps/%.1fbps",
		len(payload), onWireBytes, minimumInterval, bitsPerSecond, budget)

	reader := bufio.NewReader(bytes.NewReader(port.Bytes()))
	firstPacket, err := protocol.ReadPacket(reader)
	if err != nil {
		t.Fatal(err)
	}
	latestPacket, err := protocol.ReadPacket(reader)
	if err != nil {
		t.Fatal(err)
	}
	firstValues, _ := protocol.DecodeQStringMap(firstPacket.Payload)
	latestValues, _ := protocol.DecodeQStringMap(latestPacket.Payload)
	if firstValues["A00"] != "10" || latestValues["A00"] != "30" {
		t.Fatalf("串口上积压了已过时帧: first=%q latest=%q", firstValues["A00"], latestValues["A00"])
	}
	if _, err := protocol.ReadPacket(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("节流期不应还有第三个包: %v", err)
	}
}

func TestSpectrumWireBudgetCountsFromSendStartNotACKCompletion(t *testing.T) {
	port := &memoryPort{}
	clock := time.Date(2026, 8, 28, 1, 0, 0, 0, time.Local)
	gate := &scriptedGate{themes: map[string]string{}}
	gate.waitHook = func(command protocol.Command) {
		if command == protocol.CommandSystemData {
			// 两块实机的 ACK 平均约 58 ms；它属于本帧线速预算，
			// 不能在下一帧前再次完整等待。
			clock = clock.Add(58 * time.Millisecond)
		}
	}
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	driver.spectrumClock = func() time.Time { return clock }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	values := func(value string) map[string]string {
		result := make(map[string]string, waveshareSpectrumBands)
		for index := 0; index < waveshareSpectrumBands; index++ {
			result[fmt.Sprintf("A%02d", index)] = value
		}
		return result
	}
	first := values("10")
	firstPayload := protocol.EncodeQStringMap(first)
	firstStartedAt := clock
	if err := driver.SendSpectrumValues(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	clock = firstStartedAt.Add(waveshareSpectrumMinimumInterval(len(firstPayload)))
	if err := driver.SendSpectrumValues(context.Background(), values("20")); err != nil {
		t.Fatalf("到达相邻发送起点预算后仍丢帧: %v", err)
	}
	if sends := countCommands(gate.commands, protocol.CommandSystemData); sends != 2 {
		t.Fatalf("相邻发送起点达到预算后发送数=%d，期望 2", sends)
	}
}

func TestSpectrumWireBudgetCoversFullFortyEightBandTheme(t *testing.T) {
	typicalValues := make(map[string]string, waveshareSpectrumBands)
	values := make(map[string]string, waveshareSpectrumBands)
	for index := 0; index < waveshareSpectrumBands; index++ {
		typicalValues[fmt.Sprintf("A%02d", index)] = "50"
		// 三位数是 0..100 协议中的最坏常规值长度。
		values[fmt.Sprintf("A%02d", index)] = "100"
	}
	typicalPayload := protocol.EncodeQStringMap(typicalValues)
	typicalInterval := waveshareSpectrumMinimumInterval(len(typicalPayload))
	payload := protocol.EncodeQStringMap(values)
	minimumInterval := waveshareSpectrumMinimumInterval(len(payload))
	onWireBytes := 38 + len(payload) + waveshareSpectrumACKBytes
	bitsPerSecond := float64(onWireBytes*waveshareSerialBitsPerByte) / minimumInterval.Seconds()
	budget := float64(waveshareConfiguredBaudRate) * waveshareSpectrumLineShare
	if bitsPerSecond > budget+1 {
		t.Fatalf("48 键最坏常规载荷超过频谱线速预算: payload=%d interval=%v bits/s=%.1f budget=%.1f",
			len(payload), minimumInterval, bitsPerSecond, budget)
	}
	t.Logf("48 键典型两位值 payload=%dB，最小间隔=%v（%.2fHz）；最坏三位值 payload=%dB，命令+最小ACK=%dB，最小间隔=%v（%.2fHz）",
		len(typicalPayload), typicalInterval, 1/typicalInterval.Seconds(),
		len(payload), onWireBytes, minimumInterval, 1/minimumInterval.Seconds())
	if typicalInterval > 110*time.Millisecond {
		t.Fatalf("48 段短键主题未达到接近 10Hz 的刷新预算: payload=%d interval=%v", len(typicalPayload), typicalInterval)
	}
}

func TestMergedFineLyricsKeepsTypicalFortyEightBandsAtTenHertz(t *testing.T) {
	port := &memoryPort{}
	gate := &scriptedGate{themes: map[string]string{}}
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.Local)
	clock := base
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	driver.spectrumClock = func() time.Time { return clock }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	port.Reset()

	// 两位数频带加一个三字符 Fine 后仍能按 100ms 节拍发送。
	for tick := 0; tick < 10; tick++ {
		clock = base.Add(time.Duration(tick) * 100 * time.Millisecond)
		values := make(map[string]string, waveshareSpectrumBands+1)
		level := strconv.Itoa(50 + tick%2)
		for index := 0; index < waveshareSpectrumBands; index++ {
			values[fmt.Sprintf("A%02d", index)] = level
		}
		values[telemetry.LyricsProgressFineKey] = strconv.Itoa(tick*1000 + 1)
		if err := driver.SendSpectrumValues(context.Background(), values); err != nil {
			t.Fatalf("第 %d 个 10Hz 频谱+Fine 节拍被丢弃: %v", tick, err)
		}
	}

	spectrumPackets := 0
	reader := bufio.NewReader(bytes.NewReader(port.Bytes()))
	for {
		packet, err := protocol.ReadPacket(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		values, err := protocol.DecodeQStringMap(packet.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := values["A00"]; ok {
			spectrumPackets++
			if len(values) != waveshareSpectrumBands+1 {
				t.Fatalf("频谱+Fine 键数错误: keys=%d values=%v", len(values), values)
			}
			if _, exists := values[telemetry.LyricsProgressFineKey]; !exists {
				t.Fatalf("频谱包缺少短 Fine: %v", values)
			}
			if _, exists := values[telemetry.LyricsProgressLegacyKey]; exists {
				t.Fatalf("频谱包混入旧长键: %v", values)
			}
			continue
		}
		t.Fatalf("出现无法归类的系统数据包: %v", values)
	}
	if spectrumPackets != 10 {
		t.Fatalf("10Hz 频谱+Fine 节拍数量错误: %d", spectrumPackets)
	}
}

func TestMergedFineWorstCaseThreeDigitBandsAdaptRate(t *testing.T) {
	port := &memoryPort{}
	gate := &scriptedGate{themes: map[string]string{}}
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.Local)
	clock := base
	driver := New("wave", "微雪", [2]int{1024, 600}, port, gate)
	driver.spectrumClock = func() time.Time { return clock }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	values := func(fine string) map[string]string {
		result := make(map[string]string, waveshareSpectrumBands+1)
		for index := 0; index < waveshareSpectrumBands; index++ {
			result[fmt.Sprintf("A%02d", index)] = "100"
		}
		result[telemetry.LyricsProgressFineKey] = fine
		return result
	}
	first := values("1000")
	if err := driver.SendSpectrumValues(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	payload := protocol.EncodeQStringMap(values("2000"))
	minimumInterval := waveshareSpectrumMinimumInterval(len(payload))
	if minimumInterval <= 100*time.Millisecond {
		t.Fatalf("三位数频带+Fine 未触发自适应降频: %v", minimumInterval)
	}
	clock = base.Add(100 * time.Millisecond)
	if err := driver.SendSpectrumValues(context.Background(), values("2000")); !errors.Is(err, device.ErrRealtimeFrameDropped) {
		t.Fatalf("100ms 内的最坏包应被限流: %v", err)
	}
	clock = base.Add(minimumInterval)
	if err := driver.SendSpectrumValues(context.Background(), values("3000")); err != nil {
		t.Fatalf("达到自适应间隔后仍丢帧: %v", err)
	}
	onWireBytes := 38 + len(payload) + waveshareSpectrumACKBytes
	bitsPerSecond := float64(onWireBytes*waveshareSerialBitsPerByte) / minimumInterval.Seconds()
	budget := float64(waveshareConfiguredBaudRate) * waveshareSpectrumLineShare
	if bitsPerSecond > budget+1 {
		t.Fatalf("三位数频带+Fine超过85%%预算: %.1f > %.1f bps", bitsPerSecond, budget)
	}
}

func TestSpectrumWireBudgetCoversDefaultPeakHoldShortKeys(t *testing.T) {
	layer := theme.Layer{Type: theme.LayerAudioBars, Options: map[string]any{"peak_hold": true}}
	values := make(map[string]string, theme.WaveshareAudioBandCount*2)
	for index := 0; index < theme.WaveshareAudioBandCount; index++ {
		bandKey := theme.WaveshareAudioBandKey(layer, index)
		peakKey := theme.WaveshareAudioPeakKey(layer, index)
		if len(bandKey) != 3 || len(peakKey) != 3 {
			t.Fatalf("默认频带/峰值应使用三字节短键: band=%q peak=%q", bandKey, peakKey)
		}
		values[bandKey] = "100"
		values[peakKey] = "100"
	}
	payload := protocol.EncodeQStringMap(values)
	minimumInterval := waveshareSpectrumMinimumInterval(len(payload))
	onWireBytes := 38 + len(payload) + waveshareSpectrumACKBytes
	bitsPerSecond := float64(onWireBytes*waveshareSerialBitsPerByte) / minimumInterval.Seconds()
	budget := float64(waveshareConfiguredBaudRate) * waveshareSpectrumLineShare
	if bitsPerSecond > budget+1 {
		t.Fatalf("带默认峰值的 96 键载荷超过频谱线速预算: payload=%d interval=%v bits/s=%.1f budget=%.1f",
			len(payload), minimumInterval, bitsPerSecond, budget)
	}
	// 若默认峰值误用带哈希的长键，96 键帧的自适应间隔会超过这个上限。
	if minimumInterval > 550*time.Millisecond {
		t.Fatalf("默认峰值键过长，频谱刷新率被不必要地降低: payload=%d interval=%v", len(payload), minimumInterval)
	}
	t.Logf("48 频带 + 48 峰值短键 payload=%dB，最小间隔=%v（%.2fHz），线速=%.1fbps/%.1fbps",
		len(payload), minimumInterval, 1/minimumInterval.Seconds(), bitsPerSecond, budget)
}

func TestNewLyricEventCancelsBackgroundStorageQuery(t *testing.T) {
	gate := &priorityGate{storageStarted: make(chan struct{})}
	driver := New("wave", "微雪", [2]int{1024, 600}, &memoryPort{}, gate)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	storageDone := make(chan error, 1)
	go func() { storageDone <- driver.RefreshStorage(context.Background()) }()
	select {
	case <-gate.storageStarted:
	case <-time.After(time.Second):
		t.Fatal("后台容量查询没有启动")
	}

	snapshot := telemetry.Snapshot{
		OmitHardwareMetrics: true, OmitClock: true,
		LyricsCurrent: "暂停在这一句", LyricsNext: "下一句",
		LyricsCurrentAtMS: 10000, LyricsNextAtMS: 15000,
		LyricsCurrentAtKnown: true, LyricsNextAtKnown: true,
		LyricsPlaying: false, LyricsMode: "timed",
	}
	sendContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := driver.SendTelemetry(sendContext, snapshot); err != nil {
		t.Fatalf("歌词事件没有抢占低优先级容量查询: %v", err)
	}
	select {
	case err := <-storageDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("容量查询没有因歌词事件取消: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("容量查询取消后没有释放串口")
	}
	if gate.count(protocol.CommandSystemData) != 1 || !driver.Connected() {
		t.Fatalf("优先发送后状态错误: system_data=%d connected=%v", gate.count(protocol.CommandSystemData), driver.Connected())
	}
}

func testManifest(t *testing.T, contents []byte, remotePath string) theme.Manifest {
	t.Helper()
	localPath := filepath.Join(t.TempDir(), "custom.Theme")
	if err := os.WriteFile(localPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	resource, err := theme.InspectResource("theme", localPath, remotePath)
	if err != nil {
		t.Fatal(err)
	}
	return theme.Manifest{
		ID: "custom", Name: "自定义主题", Width: 1024, Height: 600,
		Resources: []theme.Resource{resource},
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
}

func telemetryCacheLifecycleSnapshot() telemetry.Snapshot {
	return telemetry.Snapshot{
		OmitHardwareMetrics:  true,
		OmitClock:            true,
		LyricsCurrent:        "保持当前歌词",
		LyricsNext:           "下一句歌词",
		LyricsCurrentAtMS:    10000,
		LyricsNextAtMS:       15000,
		LyricsCurrentAtKnown: true,
		LyricsNextAtKnown:    true,
		LyricsPlaying:        false,
		LyricsMode:           "timed",
	}
}

func countCommands(commands []protocol.Command, target protocol.Command) int {
	count := 0
	for _, command := range commands {
		if command == target {
			count++
		}
	}
	return count
}
