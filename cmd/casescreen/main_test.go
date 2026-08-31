package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/discovery"
	"casescreen/internal/service"
)

func TestMergeDiscoveredWaveshareDevicesOnlyAddsNewPorts(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "custom-wave", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "coolermaster-atmos-v2", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: 4},
	}
	found := []discovery.Device{
		{ID: "waveshare-com3", Kind: "waveshare", Port: "com3", Width: 1024, Height: 600},
		{ID: "waveshare-com8", Name: "微雪 8.8 英寸", Kind: "waveshare", Port: "COM8", Width: 480, Height: 1920},
		{ID: "coolermaster-atmos-v2", Kind: "coolermaster", Width: 480, Height: 480},
	}

	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed || len(merged.Devices) != 3 {
		t.Fatalf("应只新增一台微雪设备: %+v", merged.Devices)
	}
	added := merged.Devices[2]
	if added.ID != "waveshare-com8" || added.Port != "COM8" || added.Width != 480 || added.Height != 1920 {
		t.Fatalf("自动接入初始配置无效: %+v", added)
	}
	if merged.Devices[1] != cfg.Devices[1] {
		t.Fatal("不应改动酷冷至尊配置")
	}

	again, changed := mergeDiscoveredWaveshareDevices(merged, found)
	if changed || len(again.Devices) != len(merged.Devices) {
		t.Fatal("按串口去重失败")
	}
}

func TestValidateDesktopOwnerFlagsRequiresCompleteIdentity(t *testing.T) {
	tests := []struct {
		name      string
		headless  bool
		pid       int
		started   uint64
		wantError bool
	}{
		{name: "手动托盘", headless: false},
		{name: "手动headless", headless: true},
		{name: "完整桌面归属", headless: true, pid: 42, started: 99},
		{name: "缺启动标记", headless: true, pid: 42, wantError: true},
		{name: "缺PID", headless: true, started: 99, wantError: true},
		{name: "桌面归属却启用托盘", pid: 42, started: 99, wantError: true},
		{name: "负PID", headless: true, pid: -1, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotError := validateDesktopOwnerFlags(test.headless, test.pid, test.started) != nil
			if gotError != test.wantError {
				t.Fatalf("错误状态=%v，want %v", gotError, test.wantError)
			}
		})
	}
}

func TestMergeDiscoveredWaveshareDevicesMigratesSingleLogicalDevicePort(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: 4},
	}
	cfg.DeviceThemePaths = map[string]string{"waveshare-7": `C:\themes\resident.json`}
	cfg.WaveshareTemplates = map[string][]config.WaveshareTemplate{
		"waveshare-7": {{ID: "resident", Name: "驻留频谱", ManifestPath: `C:\themes\resident.json`}},
	}
	cfg.WavesharePlaylists = map[string]config.WavesharePlaylist{
		"waveshare-7": {Enabled: true, Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "resident"}}},
	}
	found := []discovery.Device{
		{ID: "waveshare-com4", Name: "微雪 USB Monitor（COM4）", Kind: "waveshare", Port: "com4", Width: 1024, Height: 600},
	}

	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed || len(merged.Devices) != 2 {
		t.Fatalf("单设备 COM 变化应迁移原配置，不应新增未绑定设备: %+v", merged.Devices)
	}
	device := merged.Devices[0]
	if device.ID != "waveshare-7" || device.Port != "COM4" || device.Name != "微雪 7 英寸" || !device.Enabled {
		t.Fatalf("迁移应只更新串口并保留逻辑设备: %+v", device)
	}
	if cfg.Devices[0].Port != "COM3" {
		t.Fatalf("不应就地修改调用者配置: %+v", cfg.Devices[0])
	}
	if merged.Devices[1] != cfg.Devices[1] {
		t.Fatalf("微雪串口迁移不应改动其他类型设备: %+v", merged.Devices[1])
	}
	if merged.DeviceThemePaths["waveshare-7"] != cfg.DeviceThemePaths["waveshare-7"] ||
		len(merged.WaveshareTemplates["waveshare-7"]) != 1 ||
		len(merged.WavesharePlaylists["waveshare-7"].Entries) != 1 {
		t.Fatal("更换 COM 号不应丢失主题、模板库或轮播绑定")
	}

	again, changed := mergeDiscoveredWaveshareDevices(merged, found)
	if changed || len(again.Devices) != 2 || again.Devices[0].Port != "COM4" {
		t.Fatalf("串口迁移应幂等: %+v", again.Devices)
	}
}

func TestMergeDiscoveredWaveshareDevicesDowngradesMissingSerialOnlyByUniqueResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
		{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{
		"waveshare-7":   `C:\themes\seven.json`,
		"waveshare-8.8": `C:\themes\portrait.json`,
	}
	found := []discovery.Device{
		{ID: "waveshare-com5", Kind: "waveshare", Port: "COM5", Width: 480, Height: 1920},
		{ID: "waveshare-com7", Kind: "waveshare", Port: "COM7", Width: 1024, Height: 600},
	}

	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed {
		t.Fatal("旧序列号消失后的唯一分辨率回绑应修改配置")
	}
	byID := make(map[string]config.Device)
	for _, configured := range merged.Devices {
		byID[configured.ID] = configured
	}
	if len(merged.Devices) != 2 || byID["waveshare-7"].Port != "COM7" || byID["waveshare-7"].StableID != "" ||
		byID["waveshare-8.8"].Port != "COM5" {
		t.Fatalf("启动预检没有安全降级回绑: %+v", merged.Devices)
	}
	if merged.DeviceThemePaths["waveshare-7"] != cfg.DeviceThemePaths["waveshare-7"] ||
		merged.DeviceThemePaths["waveshare-8.8"] != cfg.DeviceThemePaths["waveshare-8.8"] {
		t.Fatal("启动降级回绑破坏了主题绑定")
	}
}

func TestMergeDiscoveredWaveshareDevicesNeverDowngradesToDifferentSerial(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600,
	}}
	found := []discovery.Device{{
		ID: "waveshare-other", Kind: "waveshare", Port: "COM7",
		StableID: "usb-serial:1d6b:0104:other", Width: 1024, Height: 600,
	}}

	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if changed || len(merged.Devices) != 1 || merged.Devices[0].Port != "COM4" ||
		merged.Devices[0].StableID != "usb-serial:1d6b:0104:syk" {
		t.Fatalf("启动预检不得接管不同序列号的屏: changed=%v devices=%+v", changed, merged.Devices)
	}
}

func TestMergeDiscoveredWaveshareDevicesRepairsExactThreeEntryCorruptionWithoutLosingThemes(t *testing.T) {
	themeDirectory := t.TempDir()
	sevenTheme := filepath.Join(themeDirectory, "waveshare-7.json")
	eightTheme := filepath.Join(themeDirectory, "waveshare-com4.json")
	if err := os.WriteFile(sevenTheme, []byte(`{"id":"seven","name":"seven","width":1024,"height":600,"layers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eightTheme, []byte(`{"id":"eight","name":"eight","width":480,"height":1920,"layers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: 4},
		// 该条目的 Device 尺寸曾被错误 COM4 会话覆写为 7 英寸，
		// 但它的 active theme 仍能证明逻辑目标是 8.8 英寸。
		{ID: "waveshare-com4", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600, PoweredOff: true},
		{ID: "waveshare-com5", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{
		"waveshare-7":    sevenTheme,
		"waveshare-com4": eightTheme,
	}
	cfg.WaveshareTemplates = map[string][]config.WaveshareTemplate{
		"waveshare-com4": {{ID: "portrait", Name: "8.8 英寸模板", ManifestPath: eightTheme}},
	}
	cfg.WavesharePlaylists = map[string]config.WavesharePlaylist{
		"waveshare-com4": {Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "portrait"}}},
	}
	found := []discovery.Device{
		{ID: "waveshare-usb-seven", Name: "微雪 7 英寸", Kind: "waveshare", Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
		{ID: "waveshare-com5", Name: "微雪 8.8 英寸", Kind: "waveshare", Port: "COM5", Width: 480, Height: 1920},
	}

	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed {
		t.Fatal("当前三条错误配置应被修复")
	}
	byID := make(map[string]config.Device)
	for _, configured := range merged.Devices {
		byID[configured.ID] = configured
	}
	if len(merged.Devices) != 3 || len(byID) != 3 {
		t.Fatalf("应只保留两块微雪和酷冷至尊: %+v", merged.Devices)
	}
	seven := byID["waveshare-7"]
	if seven.Port != "COM4" || seven.StableID != "usb-serial:1d6b:0104:syk" || seven.Width != 1024 || seven.Height != 600 {
		t.Fatalf("7 英寸没有按 USB 序列号回绑 COM4: %+v", seven)
	}
	eight := byID["waveshare-com4"]
	if eight.Port != "COM5" || eight.StableID != "" || eight.Width != 480 || eight.Height != 1920 || !eight.PoweredOff {
		t.Fatalf("8.8 英寸逻辑 ID/关屏意图没有迁移到 COM5: %+v", eight)
	}
	if _, exists := byID["waveshare-com5"]; exists {
		t.Fatal("无用户状态的旧版自动重复项没有删除")
	}
	if merged.DeviceThemePaths["waveshare-7"] != sevenTheme || merged.DeviceThemePaths["waveshare-com4"] != eightTheme {
		t.Fatalf("两块屏的 active theme 必须分别保留: %#v", merged.DeviceThemePaths)
	}
	if len(merged.WaveshareTemplates["waveshare-com4"]) != 1 || len(merged.WavesharePlaylists["waveshare-com4"].Entries) != 1 {
		t.Fatal("8.8 英寸的模板库和轮播绑定丢失")
	}
}

func TestMergeDiscoveredWaveshareDevicesRejectsDuplicateUSBSerialAmbiguity(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM3", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600,
	}}
	found := []discovery.Device{
		{ID: "clone-a", Kind: "waveshare", Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
		{ID: "clone-b", Kind: "waveshare", Port: "COM5", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
	}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if changed || len(merged.Devices) != 1 || merged.Devices[0].Port != "COM3" {
		t.Fatalf("两块设备克隆同一序列号时必须拒绝猜测: %+v", merged.Devices)
	}
}

func TestMergeDiscoveredWaveshareDevicesNeverDeletesCustomNamedAutoDevice(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "waveshare-com4", Name: "我的竖屏", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 480, Height: 1920},
	}
	found := []discovery.Device{{
		ID: "waveshare-usb-seven", Name: "微雪 7 英寸", Kind: "waveshare", Port: "COM4",
		StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600,
	}}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed || len(merged.Devices) != 2 || merged.Devices[1].ID != "waveshare-com4" ||
		merged.Devices[1].Name != "我的竖屏" || merged.Devices[1].Port != "COM3" {
		t.Fatalf("用户自定义名称属于持久状态，绝不能当作自动重复项删除: %+v", merged.Devices)
	}
}

func TestMergeDiscoveredWaveshareDevicesPartialPresenceMovesDisplacedLoserOffStrongPort(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
		{ID: "waveshare-com4", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920, PoweredOff: true},
	}
	cfg.DeviceThemePaths = map[string]string{"waveshare-7": "seven.json", "waveshare-com4": "portrait.json"}
	found := []discovery.Device{{
		ID: "waveshare-usb-seven", Name: "微雪 7 英寸", Kind: "waveshare", Port: "COM5",
		StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600,
	}}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed || len(merged.Devices) != 2 {
		t.Fatalf("部分在线时强身份迁移未生效: %+v", merged.Devices)
	}
	byID := map[string]config.Device{}
	for _, configured := range merged.Devices {
		byID[configured.ID] = configured
	}
	if byID["waveshare-7"].Port != "COM5" || byID["waveshare-com4"].Port != "COM4" ||
		!byID["waveshare-com4"].PoweredOff {
		t.Fatalf("强所有者应迁到 COM5，离线 loser 应原子换到已确认离线的 COM4: %+v", merged.Devices)
	}
	if merged.DeviceThemePaths["waveshare-7"] != "seven.json" || merged.DeviceThemePaths["waveshare-com4"] != "portrait.json" {
		t.Fatal("部分在线端点交换不应改变主题 ID")
	}
	if err := merged.Validate(); err != nil {
		t.Fatalf("部分在线迁移后配置必须保持无重复 COM: %v", err)
	}
}

func TestMergeDiscoveredWaveshareDevicesDoesNotGuessOneOfTwoSameModelNoSerialOwners(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "left-screen", Name: "左侧屏", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "right-screen", Name: "右侧屏", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
	}
	found := []discovery.Device{{ID: "candidate", Kind: "waveshare", Port: "COM9", Width: 1024, Height: 600}}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if changed || len(merged.Devices) != 2 || merged.Devices[0].Port != "COM3" || merged.Devices[1].Port != "COM4" {
		t.Fatalf("两个同型无序列号逻辑设备只有一个候选时不得猜测: %+v", merged.Devices)
	}
}

func TestMergeDiscoveredWaveshareDevicesPartialFirstSerialUsesReciprocalResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "seven", Name: "七寸逻辑屏", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
		{ID: "portrait", Name: "竖屏逻辑屏", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{"seven": "seven.json", "portrait": "portrait.json"}
	found := []discovery.Device{{
		ID: "serial-seven", Kind: "waveshare", Port: "COM5", StableID: "usb-serial:1d6b:0104:syk",
		Width: 1024, Height: 600,
	}}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed {
		t.Fatal("首次取得序列号且只有一块在线时仍应按双向唯一分辨率完成回绑")
	}
	byID := make(map[string]config.Device)
	for _, configured := range merged.Devices {
		byID[configured.ID] = configured
	}
	if byID["seven"].Port != "COM5" || byID["seven"].StableID != "usb-serial:1d6b:0104:syk" ||
		byID["portrait"].Port != "COM4" {
		t.Fatalf("首次 partial 回绑/占位交换错误: %+v", merged.Devices)
	}
	if merged.DeviceThemePaths["seven"] != "seven.json" || merged.DeviceThemePaths["portrait"] != "portrait.json" {
		t.Fatal("首次 partial 回绑不应改变主题")
	}
	if err := merged.Validate(); err != nil {
		t.Fatalf("首次 partial 回绑后配置无效: %v", err)
	}
}

func TestMergeDiscoveredWaveshareDevicesPartialNoSerialUsesReciprocalResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "seven", Name: "七寸", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
		{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	found := []discovery.Device{{ID: "no-serial-seven", Kind: "waveshare", Port: "COM5", Width: 1024, Height: 600}}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed || len(merged.Devices) != 2 || merged.Devices[0].Port != "COM5" ||
		merged.Devices[1].Port != "COM4" || merged.Devices[0].StableID != "" {
		t.Fatalf("无序列号 partial 屏未按双向唯一尺寸原子回绑: %+v", merged.Devices)
	}
	if err := merged.Validate(); err != nil {
		t.Fatalf("无序列号 partial 回绑后配置无效: %v", err)
	}
}

func TestMergeDiscoveredWaveshareDevicesNeverUsesLeftoverForDifferentModel(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "left", Name: "左屏", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "right", Name: "右屏", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
	}
	found := []discovery.Device{
		{ID: "same-model", Kind: "waveshare", Port: "COM3", Width: 1024, Height: 600},
		{ID: "portrait", Kind: "waveshare", Port: "COM8", Width: 480, Height: 1920},
	}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if changed || merged.Devices[0].Port != "COM3" || merged.Devices[1].Port != "COM4" {
		t.Fatalf("同型号逻辑歧义后不得把剩余竖屏强塞给另一个逻辑 ID: %+v", merged.Devices)
	}
}

func TestMergeDiscoveredWaveshareDevicesReservesDisabledDevice(t *testing.T) {
	tests := []struct {
		name       string
		devices    []config.Device
		candidate  discovery.Device
		wantPorts  map[string]string
		wantStable string
	}{
		{
			name:      "禁用无序列号设备仍在原端口",
			devices:   []config.Device{{ID: "waveshare-com4", Name: "微雪 USB Monitor（COM4）", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600}},
			candidate: discovery.Device{ID: "candidate", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600},
			wantPorts: map[string]string{"waveshare-com4": "COM4"},
		},
		{
			name: "禁用序列号设备换到 enabled 设备旧端口",
			devices: []config.Device{
				{ID: "disabled-seven", Name: "禁用 7 英寸", Kind: "waveshare", Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
				{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
			},
			candidate:  discovery.Device{ID: "candidate", Kind: "waveshare", Port: "COM5", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
			wantPorts:  map[string]string{"disabled-seven": "COM5", "portrait": "COM4"},
			wantStable: "usb-serial:1d6b:0104:syk",
		},
		{
			name: "enabled 序列号所有者优先于 disabled 同 COM 弱保留",
			devices: []config.Device{
				{ID: "enabled-seven", Name: "在线 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
				{ID: "disabled-no-serial", Name: "禁用无序列号屏", Kind: "waveshare", Port: "COM5", Width: 480, Height: 1920},
			},
			candidate:  discovery.Device{ID: "candidate", Kind: "waveshare", Port: "COM5", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
			wantPorts:  map[string]string{"enabled-seven": "COM5", "disabled-no-serial": "COM4"},
			wantStable: "usb-serial:1d6b:0104:syk",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Devices = test.devices
			merged, _ := mergeDiscoveredWaveshareDevices(cfg, []discovery.Device{test.candidate})
			if len(merged.Devices) != len(test.devices) {
				t.Fatalf("禁用设备不得生成 enabled 重复项: %+v", merged.Devices)
			}
			for _, configured := range merged.Devices {
				if configured.Port != test.wantPorts[configured.ID] {
					t.Fatalf("端点保留/交换错误: %+v", merged.Devices)
				}
				if strings.Contains(configured.ID, "disabled") && configured.Enabled {
					t.Fatalf("禁用设备被意外启用: %+v", configured)
				}
			}
			if test.wantStable != "" {
				found := false
				for _, configured := range merged.Devices {
					if configured.StableID == test.wantStable {
						found = true
					}
				}
				if !found {
					t.Fatalf("序列号所有者丢失: %+v", merged.Devices)
				}
			}
			if err := merged.Validate(); err != nil {
				t.Fatalf("禁用保留后配置无效: %v", err)
			}
		})
	}
}

func TestMergeDiscoveredWaveshareDevicesMovesDisabledNoSerialByUniqueResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "disabled-seven", Name: "禁用七寸", Kind: "waveshare", Enabled: false,
		Port: "COM4", Width: 1024, Height: 600,
	}}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, []discovery.Device{{
		ID: "moved-seven", Kind: "waveshare", Port: "COM5", Width: 1024, Height: 600,
	}})
	if !changed || len(merged.Devices) != 1 || merged.Devices[0].ID != "disabled-seven" ||
		merged.Devices[0].Enabled || merged.Devices[0].Port != "COM5" {
		t.Fatalf("禁用无序列号屏换口后不得生成 enabled 重复项: %+v", merged.Devices)
	}
}

func TestMergeDiscoveredWaveshareDevicesDoesNotGiveAmbiguousCandidateToEnabledTwin(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "disabled-seven", Name: "禁用七寸", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600},
		{ID: "enabled-seven", Name: "启用七寸", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 1024, Height: 600},
	}
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, []discovery.Device{{
		ID: "ambiguous-seven", Kind: "waveshare", Port: "COM5", Width: 1024, Height: 600,
	}})
	if changed || len(merged.Devices) != 2 || merged.Devices[0].Enabled ||
		merged.Devices[0].Port != "COM4" || merged.Devices[1].Port != "COM5" {
		t.Fatalf("禁用/启用同型歧义时不得改绑或追加设备: %+v", merged.Devices)
	}
}

func TestOrderedConfiguredDevicesUsesStableBackendOrder(t *testing.T) {
	input := []config.Device{
		{ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster"},
		{ID: "waveshare-other", Name: "微雪 USB Monitor", Kind: "waveshare"},
		{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Width: 480, Height: 1920},
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Width: 1024, Height: 600},
	}
	ordered := orderedConfiguredDevices(input)
	want := []string{"waveshare-7", "waveshare-8.8", "waveshare-other", "coolermaster-atmos-v2"}
	for index, id := range want {
		if ordered[index].ID != id {
			t.Fatalf("后端设备顺序错误: got=%v want=%v", deviceIDs(ordered), want)
		}
	}
	if input[0].ID != "coolermaster-atmos-v2" {
		t.Fatalf("排序不应改写配置切片: %+v", input)
	}
}

func TestAddConfiguredDevicesRegistersOnlySupportedDevicesInOrder(t *testing.T) {
	controller := service.New(nil, time.Second)
	cfg := config.Default()
	// 直接构造旧配置验证运行时工厂；正常 Open 会先迁移并删除退役条目。
	cfg.Devices = []config.Device{
		{ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster", Enabled: true, LibraryPath: filepath.Join(t.TempDir(), "missing-hidapi.dll"), Width: 480, Height: 480, Slot: 4, PoweredOff: true},
		{ID: "retired", Name: "已退役设备", Kind: "thermalright", Enabled: true, Width: 1920, Height: 468},
		{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM998", Width: 480, Height: 1920},
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM999", Width: 1024, Height: 600},
	}
	addConfiguredDevices(controller, cfg, t.TempDir())
	devices := controller.Status().Devices
	want := []string{"waveshare-7", "waveshare-8.8", "coolermaster-atmos-v2"}
	if len(devices) != len(want) {
		t.Fatalf("运行状态不应包含已移除的设备类型: %+v", devices)
	}
	for index, id := range want {
		if devices[index].Info.ID != id {
			t.Fatalf("运行状态设备顺序错误: got=%+v want=%v", devices, want)
		}
	}
	if !devices[2].PoweredOff || devices[2].CurrentBrightness != 0 || !devices[2].BrightnessSupported {
		t.Fatalf("配置中的关屏意图未注入服务: %+v", devices[2])
	}
}

func TestResolveCoolerMasterLibraryPathFallsBackFromStaleRelease(t *testing.T) {
	releaseDirectory := t.TempDir()
	executablePath := filepath.Join(releaseDirectory, "CaseScreen.exe")
	releaseLibraryPath := filepath.Join(releaseDirectory, "hidapi.dll")
	if err := os.WriteFile(releaseLibraryPath, []byte("release hidapi"), 0o600); err != nil {
		t.Fatalf("创建发行目录 hidapi 失败: %v", err)
	}
	stalePath := filepath.Join(t.TempDir(), "旧版本", "hidapi.dll")
	if got := resolveCoolerMasterLibraryPath(stalePath, executablePath); got != releaseLibraryPath {
		t.Fatalf("陈旧路径没有回退到当前发行目录: got=%q want=%q", got, releaseLibraryPath)
	}
}

func TestResolveCoolerMasterLibraryPathPreservesExistingCustomPath(t *testing.T) {
	customPath := filepath.Join(t.TempDir(), "custom-hidapi.dll")
	if err := os.WriteFile(customPath, []byte("custom hidapi"), 0o600); err != nil {
		t.Fatalf("创建自定义 hidapi 失败: %v", err)
	}
	executablePath := filepath.Join(t.TempDir(), "CaseScreen.exe")
	if got := resolveCoolerMasterLibraryPath(customPath, executablePath); got != customPath {
		t.Fatalf("有效自定义路径不应被覆盖: got=%q want=%q", got, customPath)
	}
}

func deviceIDs(devices []config.Device) []string {
	ids := make([]string, len(devices))
	for index, configured := range devices {
		ids[index] = configured.ID
	}
	return ids
}

func TestMergeDiscoveredWaveshareDevicesDoesNotMergePresentSecondDevice(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
	}
	found := []discovery.Device{
		{ID: "waveshare-com3", Kind: "waveshare", Port: "COM3", Width: 1024, Height: 600},
		{ID: "waveshare-com4", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600},
	}

	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if !changed || len(merged.Devices) != 2 {
		t.Fatalf("同时存在的第二块微雪应作为新设备接入: %+v", merged.Devices)
	}
	if merged.Devices[0].ID != "waveshare-7" || merged.Devices[0].Port != "COM3" {
		t.Fatalf("不应把第二块屏合并到已配置逻辑设备: %+v", merged.Devices[0])
	}
	if merged.Devices[1].ID != "waveshare-com4" || merged.Devices[1].Port != "COM4" {
		t.Fatalf("第二块屏的自动配置无效: %+v", merged.Devices[1])
	}
}

func TestMergeDiscoveredWaveshareDevicesDoesNotGuessAmbiguousPortMigration(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
	}
	found := []discovery.Device{
		{ID: "waveshare-com4", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600},
		{ID: "waveshare-com5", Kind: "waveshare", Port: "COM5", Width: 1024, Height: 600},
	}

	merged, changed := mergeDiscoveredWaveshareDevices(cfg, found)
	if changed || len(merged.Devices) != 1 {
		t.Fatalf("多块同型无序列号屏应等待更强证据，不得猜测或生成重复项: %+v", merged.Devices)
	}
	if merged.Devices[0].ID != "waveshare-7" || merged.Devices[0].Port != "COM3" {
		t.Fatalf("歧义场景不应更改已配置逻辑设备: %+v", merged.Devices[0])
	}
}
