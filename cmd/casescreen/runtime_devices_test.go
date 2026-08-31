package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/device"
	devicecoolermaster "casescreen/internal/device/coolermaster"
	"casescreen/internal/discovery"
	"casescreen/internal/service"
)

func TestRuntimeDiscoveryKeepsSingleWaveshareLogicalIDAfterCOMChange(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM3", Width: 1024, Height: 600,
	}}
	cfg.DeviceThemePaths = map[string]string{"waveshare-7": `C:\themes\seven.json`}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.openSerial = func(string) (io.ReadWriteCloser, error) { return nil, errors.New("测试不打开真实串口") }
	probeCalls := 0
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		probeCalls++
		if port != "COM9" {
			t.Fatalf("探测了错误串口: %s", port)
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: "waveshare-com9", Kind: "waveshare", Port: "COM9", Width: 1024, Height: 600}}
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := manager.entries["waveshare-7"]
	if entry == nil || entry.waveshare.Port() != "COM9" {
		t.Fatalf("原逻辑 ID 没有迁移到新 COM: %+v", entry)
	}
	status := controller.Status()
	if len(status.Devices) != 1 || status.Devices[0].Info.ID != "waveshare-7" {
		t.Fatalf("重扫后生成了重复驱动: %+v", status.Devices)
	}
	saved := store.Get()
	if len(saved.Devices) != 1 || saved.Devices[0].ID != "waveshare-7" || saved.Devices[0].Port != "COM9" ||
		saved.DeviceThemePaths["waveshare-7"] != cfg.DeviceThemePaths["waveshare-7"] {
		t.Fatalf("串口迁移破坏逻辑 ID 或主题绑定: %+v", saved)
	}
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probeCalls != 1 || len(controller.Status().Devices) != 1 {
		t.Fatalf("幂等重扫失败: probes=%d devices=%+v", probeCalls, controller.Status().Devices)
	}
}

func TestReconcileConfiguredBeforeRegisterUsesHandshakeAndPersistsSerialIdentity(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM3", Width: 1024, Height: 600,
	}}
	cfg.DeviceThemePaths = map[string]string{"waveshare-7": `C:\themes\seven.json`}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{
			ID: "waveshare-usb-seven", Kind: "waveshare", Port: "COM9",
			StableID: "usb-serial:1d6b:0104:syk",
		}}
	}
	probeCalls := 0
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		probeCalls++
		if port != "COM9" {
			t.Fatalf("启动预检探测了错误串口: %s", port)
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}

	merged, err := manager.ReconcileConfiguredBeforeRegister(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls != 1 {
		t.Fatalf("驱动注册前应只握手一次: %d", probeCalls)
	}
	if len(controller.Status().Devices) != 0 {
		t.Fatal("身份对账阶段不应提前注册驱动占用串口")
	}
	if len(merged.Devices) != 1 || merged.Devices[0].ID != "waveshare-7" || merged.Devices[0].Port != "COM9" ||
		merged.Devices[0].StableID != "usb-serial:1d6b:0104:syk" ||
		merged.DeviceThemePaths["waveshare-7"] != cfg.DeviceThemePaths["waveshare-7"] {
		t.Fatalf("启动预检没有保留逻辑 ID/主题并回绑新 COM: %+v", merged)
	}
	saved := store.Get()
	if saved.Devices[0].Port != "COM9" || saved.Devices[0].StableID != "usb-serial:1d6b:0104:syk" {
		t.Fatalf("启动预检结果未在驱动注册前持久化: %+v", saved.Devices)
	}
	manager.RegisterConfigured(merged)
	if devices := controller.Status().Devices; len(devices) != 1 || devices[0].Info.ID != "waveshare-7" {
		t.Fatalf("预检后应只注册原逻辑设备: %+v", devices)
	}
}

func TestRuntimeDiscoveryUsesUSBSerialBeforeOldCOMPort(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM3", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{
			ID: "waveshare-usb-seven", Kind: "waveshare", Port: "COM9",
			StableID: "USB-SERIAL:1D6B:0104:SYK",
		}}
	}
	manager.probeWaveshare = func(context.Context, string) (device.Info, error) {
		t.Fatal("强序列号匹配不应再打开探针串口")
		return device.Info{}, nil
	}
	manager.RegisterConfigured(cfg)
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.entries["waveshare-7"].waveshare.Port(); got != "COM9" {
		t.Fatalf("USB 序列号没有优先回绑新 COM: %s", got)
	}
	if saved := store.Get(); len(saved.Devices) != 1 || saved.Devices[0].ID != "waveshare-7" || saved.Devices[0].Port != "COM9" {
		t.Fatalf("序列号热插拔生成了重复项: %+v", saved.Devices)
	}
}

func TestRuntimeDiscoveryRecoversWhenFormerSerialDeviceStopsReportingSerial(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
		{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{
		"waveshare-7":   `C:\themes\seven.json`,
		"waveshare-8.8": `C:\themes\portrait.json`,
	}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		return []discovery.Device{
			{ID: "waveshare-com5", Kind: "waveshare", Port: "COM5"},
			{ID: "waveshare-com7", Kind: "waveshare", Port: "COM7"},
		}
	}
	probeCalls := make(map[string]int)
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		probeCalls[port]++
		if port != "COM7" {
			t.Fatalf("已在原端点的 8.8 英寸不应被重新打开: %s", port)
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.entries["waveshare-7"].waveshare.Port(); got != "COM7" {
		t.Fatalf("序列号消失后没有按唯一分辨率回绑: %s", got)
	}
	if got := manager.entries["waveshare-8.8"].waveshare.Port(); got != "COM5" {
		t.Fatalf("降级回绑不应扰动另一块屏: %s", got)
	}
	if probeCalls["COM7"] != 1 {
		t.Fatalf("新端点应且只应握手一次: %#v", probeCalls)
	}
	saved := store.Get()
	byID := make(map[string]config.Device)
	for _, configured := range saved.Devices {
		byID[configured.ID] = configured
	}
	if len(saved.Devices) != 2 || byID["waveshare-7"].Port != "COM7" || byID["waveshare-7"].StableID != "" ||
		byID["waveshare-8.8"].Port != "COM5" {
		t.Fatalf("降级回绑没有原子保存或没有清除过期序列号: %+v", saved.Devices)
	}
	if saved.DeviceThemePaths["waveshare-7"] != cfg.DeviceThemePaths["waveshare-7"] ||
		saved.DeviceThemePaths["waveshare-8.8"] != cfg.DeviceThemePaths["waveshare-8.8"] {
		t.Fatal("降级回绑破坏了逻辑屏的主题绑定")
	}
}

func TestRuntimeDiscoveryNeverDowngradesToDifferentReportedSerial(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{
			ID: "waveshare-other", Kind: "waveshare", Port: "COM7",
			StableID: "usb-serial:1d6b:0104:other",
		}}
	}
	manager.probeWaveshare = func(context.Context, string) (device.Info, error) {
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.entries["waveshare-7"].waveshare.Port(); got != "COM4" {
		t.Fatalf("不同序列号的物理屏抢占了原逻辑屏: %s", got)
	}
	saved := store.Get()
	if len(saved.Devices) != 1 || saved.Devices[0].StableID != "usb-serial:1d6b:0104:syk" || saved.Devices[0].Port != "COM4" {
		t.Fatalf("不同序列号不应改写原配置: %+v", saved.Devices)
	}
}

func TestRuntimeDiscoveryDoesNotDowngradeAmbiguousFormerSerialDevices(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "seven-a", Kind: "waveshare", Enabled: true, Port: "COM3", StableID: "usb-serial:a", Width: 1024, Height: 600},
		{ID: "seven-b", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:b", Width: 1024, Height: 600},
	}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		return []discovery.Device{
			{ID: "physical-a", Kind: "waveshare", Port: "COM7"},
			{ID: "physical-b", Kind: "waveshare", Port: "COM8"},
		}
	}
	manager.probeWaveshare = func(context.Context, string) (device.Info, error) {
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.entries["seven-a"].waveshare.Port() != "COM3" || manager.entries["seven-b"].waveshare.Port() != "COM4" {
		t.Fatalf("同型双屏序列号同时消失时不得猜测: a=%s b=%s",
			manager.entries["seven-a"].waveshare.Port(), manager.entries["seven-b"].waveshare.Port())
	}
	for _, configured := range store.Get().Devices {
		if configured.StableID == "" {
			t.Fatalf("歧义分支不得清除强身份: %+v", configured)
		}
	}
}

func TestRuntimeDiscoveryStableIdentityScanDoesNotRewriteConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.status = func() service.Status { return service.Status{} }
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{
			ID: "waveshare-usb-seven", Kind: "waveshare", Port: "COM4",
			StableID: "usb-serial:1d6b:0104:syk",
		}}
	}
	manager.probeWaveshare = func(context.Context, string) (device.Info, error) {
		t.Fatal("稳定序列号同端口扫描不应探针打开串口")
		return device.Info{}, nil
	}
	manager.RegisterConfigured(cfg)
	before, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := manager.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("无变化的 2 秒稳定身份扫描不应重写配置: before=%v/%d after=%v/%d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

func TestRuntimeDiscoverySerialIdentityAndNoSerialScreenCanSwapCOMPorts(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
		{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{
		"waveshare-7":   `C:\themes\seven.json`,
		"waveshare-8.8": `C:\themes\portrait.json`,
	}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		return []discovery.Device{
			{ID: "waveshare-no-serial", Kind: "waveshare", Port: "COM4"},
			{ID: "waveshare-usb-seven", Kind: "waveshare", Port: "COM5", StableID: "usb-serial:1d6b:0104:syk"},
		}
	}
	probeCalls := make(map[string]int)
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		probeCalls[port]++
		if port != "COM4" {
			t.Fatalf("序列号已确认的 COM5 不应再被探针打开: %s", port)
		}
		return runtimeWaveshareInfo("微雪 8.8 英寸", [2]int{480, 1920}), nil
	}
	manager.RegisterConfigured(cfg)
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.entries["waveshare-7"].waveshare.Port(); got != "COM5" {
		t.Fatalf("7 英寸序列号屏未迁移到 COM5: %s", got)
	}
	if got := manager.entries["waveshare-8.8"].waveshare.Port(); got != "COM4" {
		t.Fatalf("无序列号 8.8 英寸屏未按握手分辨率迁移到 COM4: %s", got)
	}
	if probeCalls["COM4"] != 1 || probeCalls["COM5"] != 0 {
		t.Fatalf("端点交换探针路径错误: %#v", probeCalls)
	}
	saved := store.Get()
	ports := make(map[string]string)
	for _, configured := range saved.Devices {
		ports[configured.ID] = configured.Port
		if configured.ID == "waveshare-8.8" && configured.StableID != "" {
			t.Fatalf("无序列号屏不得偷取 7 英寸的序列号: %+v", configured)
		}
	}
	if len(saved.Devices) != 2 || ports["waveshare-7"] != "COM5" || ports["waveshare-8.8"] != "COM4" {
		t.Fatalf("交换 COM 后应仍只有两个原逻辑 ID: %+v", saved.Devices)
	}
	if saved.DeviceThemePaths["waveshare-7"] != cfg.DeviceThemePaths["waveshare-7"] ||
		saved.DeviceThemePaths["waveshare-8.8"] != cfg.DeviceThemePaths["waveshare-8.8"] {
		t.Fatal("交换 COM 不应改变主题绑定")
	}
}

func TestRuntimeDiscoveryFirstSerialPartialPreemptsWrongOldCOMAndRecovers(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "seven", Name: "七寸逻辑屏", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
		{ID: "portrait", Name: "竖屏逻辑屏", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{"seven": "seven.json", "portrait": "portrait.json"}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.RegisterConfigured(cfg)
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{
			ID: "serial-seven", Kind: "waveshare", Port: "COM5", StableID: "usb-serial:1d6b:0104:syk",
		}}
	}
	firstStatus := true
	manager.status = func() service.Status {
		if firstStatus {
			return service.Status{Devices: []service.DeviceStatus{{
				Info:      runtimeWaveshareInfo("错误连到七寸", [2]int{1024, 600}),
				Connected: true,
			}}}
		}
		return service.Status{}
	}
	// status 的逻辑 ID 必须指向当前错误 claimant。
	wrong := runtimeWaveshareInfo("错误连到七寸", [2]int{1024, 600})
	wrong.ID = "portrait"
	manager.status = func() service.Status {
		if firstStatus {
			return service.Status{Devices: []service.DeviceStatus{{Info: wrong, Connected: true}}}
		}
		return service.Status{}
	}
	probeAttempts := 0
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		probeAttempts++
		if port != "COM5" || manager.entries["portrait"].waveshare.Port() != "" {
			t.Fatalf("必须先关闭并清空错误 claimant 再探针: port=%s endpoint=%s", port, manager.entries["portrait"].waveshare.Port())
		}
		if probeAttempts == 1 {
			return device.Info{}, errors.New("设备刚释放，第一次探针超时")
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstStatus = false
	if manager.entries["portrait"].waveshare.Port() != "" || !manager.entries["portrait"].requiresProbe {
		t.Fatalf("探针失败后错误 claimant 必须保持离线: %+v", manager.entries["portrait"])
	}
	if got := store.Get(); got.Devices[1].Width != 480 || got.Devices[1].Height != 1920 {
		t.Fatalf("错误 live.Info 覆盖了竖屏历史型号: %+v", got.Devices)
	}
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved := store.Get()
	byID := make(map[string]config.Device)
	for _, configured := range saved.Devices {
		byID[configured.ID] = configured
	}
	if byID["seven"].Port != "COM5" || byID["seven"].StableID != "usb-serial:1d6b:0104:syk" ||
		byID["portrait"].Port != "COM4" || saved.DeviceThemePaths["seven"] != "seven.json" ||
		saved.DeviceThemePaths["portrait"] != "portrait.json" {
		t.Fatalf("第二轮探针成功后没有原子恢复逻辑 ID: %+v", saved)
	}
	if err := saved.Validate(); err != nil {
		t.Fatalf("partial 两轮恢复后配置无效: %v", err)
	}
}

func TestRuntimeDiscoveryPartialNoSerialCandidatePreemptsOldCOMByResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "seven", Name: "七寸", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
		{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.RegisterConfigured(cfg)
	manager.status = func() service.Status { return service.Status{} }
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: "no-serial-seven", Kind: "waveshare", Port: "COM5"}}
	}
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		if port != "COM5" || manager.entries["portrait"].waveshare.Port() != "" {
			t.Fatalf("partial 无序列号候选也必须先关闭旧 claimant: port=%s endpoint=%q",
				port, manager.entries["portrait"].waveshare.Port())
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved := store.Get()
	if len(saved.Devices) != 2 || saved.Devices[0].Port != "COM5" || saved.Devices[1].Port != "COM4" {
		t.Fatalf("partial 无序列号候选未按双向唯一尺寸恢复: %+v", saved.Devices)
	}
	if err := saved.Validate(); err != nil {
		t.Fatalf("partial 无序列号运行期配置无效: %v", err)
	}
}

func TestRuntimeDiscoveryDisabledStableSwapClosesDisplacedOwnerBeforeProbe(t *testing.T) {
	for _, probeFails := range []bool{false, true} {
		name := "探针成功"
		if probeFails {
			name = "探针暂时失败"
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Devices = []config.Device{
				{ID: "disabled-seven", Name: "禁用七寸", Kind: "waveshare", Port: "COM4", StableID: "usb-serial:1d6b:0104:syk", Width: 1024, Height: 600},
				{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
			}
			cfg.DeviceThemePaths = map[string]string{"disabled-seven": "seven.json", "portrait": "portrait.json"}
			store := runtimeTestStore(t, cfg)
			controller := service.New(nil, time.Second)
			manager := newRuntimeDeviceManager(controller, store, t.TempDir())
			manager.RegisterConfigured(cfg)
			manager.status = func() service.Status { return service.Status{} }
			manager.discover = func() []discovery.Device {
				return []discovery.Device{
					{ID: "disabled-physical", Kind: "waveshare", Port: "COM5", StableID: "usb-serial:1d6b:0104:syk"},
					{ID: "portrait-physical", Kind: "waveshare", Port: "COM4"},
				}
			}
			manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
				if port != "COM4" || manager.entries["portrait"].waveshare.Port() != "" {
					t.Fatalf("禁用强身份占端口后必须先关闭 displaced driver: port=%s endpoint=%s", port, manager.entries["portrait"].waveshare.Port())
				}
				if probeFails {
					return device.Info{}, errors.New("暂时读取失败")
				}
				return runtimeWaveshareInfo("微雪 8.8 英寸", [2]int{480, 1920}), nil
			}
			if err := manager.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved := store.Get()
			byID := make(map[string]config.Device)
			for _, configured := range saved.Devices {
				byID[configured.ID] = configured
			}
			if byID["disabled-seven"].Enabled || byID["disabled-seven"].Port != "COM5" ||
				byID["portrait"].Port != "COM4" || len(controller.Status().Devices) != 1 {
				t.Fatalf("禁用强身份交换错误: %+v status=%+v", saved.Devices, controller.Status().Devices)
			}
			entry := manager.entries["portrait"]
			if probeFails && (entry.present || !entry.requiresProbe) {
				t.Fatalf("探针失败时 displaced driver 不得假在线: %+v", entry)
			}
			if !probeFails && (!entry.present || entry.requiresProbe) {
				t.Fatalf("探针成功后 enabled 竖屏应完成验证: %+v", entry)
			}
			if err := saved.Validate(); err != nil {
				t.Fatalf("禁用交换后配置无效: %v", err)
			}
		})
	}
}

func TestRuntimeDiscoveryMovesDisabledNoSerialWithoutAddingEnabledDuplicate(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "disabled-seven", Name: "禁用七寸", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.RegisterConfigured(cfg)
	manager.status = func() service.Status { return service.Status{} }
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: "moved-seven", Kind: "waveshare", Port: "COM5"}}
	}
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		if port != "COM5" {
			t.Fatalf("探测了错误端口: %s", port)
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved := store.Get()
	if len(saved.Devices) != 1 || saved.Devices[0].ID != "disabled-seven" || saved.Devices[0].Enabled ||
		saved.Devices[0].Port != "COM5" || len(controller.Status().Devices) != 0 {
		t.Fatalf("运行期禁用无序列号屏换口生成了 enabled 重复项: config=%+v status=%+v", saved.Devices, controller.Status().Devices)
	}
}

func TestStartupAndRuntimeBlockAmbiguousDisabledNoSerialTwin(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "disabled-seven", Name: "禁用七寸", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600},
		{ID: "enabled-seven", Name: "启用七寸", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 1024, Height: 600},
	}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: "ambiguous-seven", Kind: "waveshare", Port: "COM5"}}
	}
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	merged, err := manager.ReconcileConfiguredBeforeRegister(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager.RegisterConfigured(merged)
	entry := manager.entries["enabled-seven"]
	if entry == nil || entry.waveshare.Port() != "" || !entry.requiresProbe {
		t.Fatalf("启动预检的禁用/启用同型歧义未阻止 enabled opener: %+v", entry)
	}
	manager.status = func() service.Status { return service.Status{} }
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved := store.Get()
	if len(saved.Devices) != 2 || saved.Devices[0].Enabled || manager.entries["ambiguous-seven"] != nil ||
		entry.waveshare.Port() != "" || entry.present {
		t.Fatalf("同型歧义被错误认领或追加: config=%+v entry=%+v", saved.Devices, entry)
	}
}

func TestRuntimeDiscoveryNoSerialScreensSwapByVerifiedResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "seven", Name: "七寸", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
		{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM5", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{"seven": "seven.json", "portrait": "portrait.json"}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.RegisterConfigured(cfg)
	sevenWrong := runtimeWaveshareInfo("竖屏实机", [2]int{480, 1920})
	sevenWrong.ID = "seven"
	portraitWrong := runtimeWaveshareInfo("七寸实机", [2]int{1024, 600})
	portraitWrong.ID = "portrait"
	manager.status = func() service.Status {
		return service.Status{Devices: []service.DeviceStatus{
			{Info: sevenWrong, Connected: true},
			{Info: portraitWrong, Connected: true},
		}}
	}
	manager.discover = func() []discovery.Device {
		return []discovery.Device{
			{ID: "physical-portrait", Kind: "waveshare", Port: "COM4"},
			{ID: "physical-seven", Kind: "waveshare", Port: "COM5"},
		}
	}
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		if manager.entries["seven"].waveshare.Port() != "" || manager.entries["portrait"].waveshare.Port() != "" {
			t.Fatalf("互换探针前必须批量关闭两个错误会话: seven=%q portrait=%q",
				manager.entries["seven"].waveshare.Port(), manager.entries["portrait"].waveshare.Port())
		}
		if port == "COM4" {
			return runtimeWaveshareInfo("微雪 8.8 英寸", [2]int{480, 1920}), nil
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved := store.Get()
	byID := make(map[string]config.Device)
	for _, configured := range saved.Devices {
		byID[configured.ID] = configured
	}
	if byID["seven"].Port != "COM5" || byID["portrait"].Port != "COM4" ||
		saved.DeviceThemePaths["seven"] != "seven.json" || saved.DeviceThemePaths["portrait"] != "portrait.json" {
		t.Fatalf("无序列号双屏互换未按实机分辨率原子恢复: %+v", saved)
	}
}

func TestWaveshareIdentityPreflightHardwareSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_WAVESHARE_IDENTITY_SMOKE") != "1" {
		t.Skip("设置 CASESCREEN_WAVESHARE_IDENTITY_SMOKE=1 后执行真机身份迁移预检")
	}
	source, err := config.Open("")
	if err != nil {
		t.Fatal(err)
	}
	original := source.Get()
	store := runtimeTestStore(t, original)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), runtimeProbeTimeout+time.Second)
	defer cancel()
	merged, err := manager.ReconcileConfiguredBeforeRegister(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]config.Device)
	waveshareCount := 0
	for _, configured := range merged.Devices {
		byID[configured.ID] = configured
		if configured.Kind == "waveshare" {
			waveshareCount++
		}
	}
	seven := byID["waveshare-7"]
	eight := byID["waveshare-com4"]
	if waveshareCount != 2 || seven.Port != "COM4" || seven.StableID != "usb-serial:1d6b:0104:syk" ||
		eight.Port != "COM5" || eight.Width != 480 || eight.Height != 1920 {
		t.Fatalf("真机预检未收敛为两个原逻辑 ID: %+v", merged.Devices)
	}
	if merged.DeviceThemePaths["waveshare-7"] != original.DeviceThemePaths["waveshare-7"] ||
		merged.DeviceThemePaths["waveshare-com4"] != original.DeviceThemePaths["waveshare-com4"] {
		t.Fatalf("真机预检破坏了两个原主题绑定: %#v", merged.DeviceThemePaths)
	}
}

func TestRuntimeDiscoverySyncsConnectedWaveshareIdentityToStore(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-com4", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM4", Width: 1024, Height: 600,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.status = func() service.Status {
		return service.Status{Devices: []service.DeviceStatus{{
			Info: device.Info{
				ID: "waveshare-com4", Name: "微雪 8.8 英寸", Resolution: [2]int{480, 1920},
			},
			Connected: true,
		}}}
	}
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: "waveshare-com4", Kind: "waveshare", Port: "COM4"}}
	}
	manager.probeWaveshare = func(context.Context, string) (device.Info, error) {
		t.Fatal("已占用端口不应重新探测")
		return device.Info{}, nil
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := manager.entries["waveshare-com4"]
	if entry == nil || entry.configured.Width != 480 || entry.configured.Height != 1920 ||
		entry.configured.Name != "微雪 8.8 英寸" {
		t.Fatalf("实机身份未同步到运行期 entry: %+v", entry)
	}
	saved := store.Get()
	if len(saved.Devices) != 1 || saved.Devices[0].Width != 480 || saved.Devices[0].Height != 1920 ||
		saved.Devices[0].Name != "微雪 8.8 英寸" {
		t.Fatalf("实机身份未持久化: %+v", saved.Devices)
	}
}

func TestRuntimeDiscoveryUsesHandshakeIdentityBeforeMatchingChangedCOM(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-com4", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM4", Width: 1024, Height: 600,
	}}
	cfg.DeviceThemePaths = map[string]string{"waveshare-com4": `C:\themes\portrait.json`}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.status = func() service.Status {
		return service.Status{Devices: []service.DeviceStatus{{
			Info: device.Info{
				ID: "waveshare-com4", Name: "微雪 8.8 英寸", Resolution: [2]int{480, 1920},
			},
			// 拔出后 Service 可能已标记离线，但 LastConnectedAt
			// 证明 Info 是上一次握手后的实机身份。
			LastConnectedAt: time.Now(),
		}}}
	}
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: "waveshare-com9", Kind: "waveshare", Port: "COM9"}}
	}
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		if port != "COM9" {
			t.Fatalf("探测了错误串口: %s", port)
		}
		return runtimeWaveshareInfo("微雪 8.8 英寸", [2]int{480, 1920}), nil
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := manager.entries["waveshare-com4"]
	if entry == nil || entry.waveshare.Port() != "COM9" || entry.configured.Width != 480 ||
		entry.configured.Height != 1920 || manager.entries["waveshare-com9"] != nil {
		t.Fatalf("旧尺寸错误时没有保留原逻辑 ID: entries=%+v", manager.entries)
	}
	saved := store.Get()
	if len(saved.Devices) != 1 || saved.Devices[0].ID != "waveshare-com4" || saved.Devices[0].Port != "COM9" ||
		saved.Devices[0].Width != 480 || saved.Devices[0].Height != 1920 ||
		saved.DeviceThemePaths["waveshare-com4"] != cfg.DeviceThemePaths["waveshare-com4"] {
		t.Fatalf("尺寸校正后 COM 迁移破坏配置或主题绑定: %+v", saved)
	}
	devices := controller.Status().Devices
	if len(devices) != 1 || devices[0].Info.ID != "waveshare-com4" {
		t.Fatalf("COM 迁移后生成了重复驱动: %+v", devices)
	}
}

func TestRuntimeDiscoveryResetsReconnectBackoffWhenWaveshareReappearsOnSamePort(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM3", Width: 1024, Height: 600,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	present := false
	manager.discover = func() []discovery.Device {
		if !present {
			return nil
		}
		return []discovery.Device{{ID: "waveshare-com3", Kind: "waveshare", Port: "COM3"}}
	}
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		if port != "COM3" {
			t.Fatalf("同端口重插探测了错误端点: %s", port)
		}
		return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	present = true
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	devices := controller.Status().Devices
	if len(devices) != 1 || devices[0].Info.ID != "waveshare-7" {
		t.Fatalf("同端口重插改变了逻辑设备集: %+v", devices)
	}
	status := devices[0]
	if !status.RetryAt.IsZero() || status.ReconnectAttempts != 0 ||
		!strings.Contains(status.LastError, "COM3 重新插入") {
		t.Fatalf("同端口重插没有清除旧退避: %+v", status)
	}
	if got := manager.entries["waveshare-7"].waveshare.Port(); got != "COM3" {
		t.Fatalf("同端口重插不应改变端点: %s", got)
	}
}

func TestRuntimeDiscoveryMapsTwoWaveshareScreensByActualResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{
		"waveshare-7":   `C:\themes\seven.json`,
		"waveshare-8.8": `C:\themes\eight-eight.json`,
	}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.openSerial = func(string) (io.ReadWriteCloser, error) { return nil, errors.New("测试不打开真实串口") }
	manager.probeWaveshare = func(_ context.Context, port string) (device.Info, error) {
		switch port {
		case "COM8":
			return runtimeWaveshareInfo("微雪 8.8 英寸", [2]int{480, 1920}), nil
		case "COM9":
			return runtimeWaveshareInfo("微雪 7 英寸", [2]int{1024, 600}), nil
		default:
			return device.Info{}, errors.New("未知串口")
		}
	}
	manager.discover = func() []discovery.Device {
		return []discovery.Device{
			{ID: "waveshare-com8", Kind: "waveshare", Port: "COM8"},
			{ID: "waveshare-com9", Kind: "waveshare", Port: "COM9"},
		}
	}
	manager.RegisterConfigured(cfg)

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.entries["waveshare-7"].waveshare.Port(); got != "COM9" {
		t.Fatalf("7 英寸被绑到错误端点: %s", got)
	}
	if got := manager.entries["waveshare-8.8"].waveshare.Port(); got != "COM8" {
		t.Fatalf("8.8 英寸被绑到错误端点: %s", got)
	}
	if devices := controller.Status().Devices; len(devices) != 2 || devices[0].Info.ID != "waveshare-7" || devices[1].Info.ID != "waveshare-8.8" {
		t.Fatalf("双屏重扫改变了逻辑设备集: %+v", devices)
	}
	saved := store.Get()
	ports := map[string]string{}
	for _, configured := range saved.Devices {
		ports[configured.ID] = configured.Port
	}
	if ports["waveshare-7"] != "COM9" || ports["waveshare-8.8"] != "COM8" ||
		saved.DeviceThemePaths["waveshare-7"] != cfg.DeviceThemePaths["waveshare-7"] ||
		saved.DeviceThemePaths["waveshare-8.8"] != cfg.DeviceThemePaths["waveshare-8.8"] {
		t.Fatalf("双屏映射或主题绑定未保留: %+v", saved)
	}
}

func TestRuntimeDiscoveryAddsCoolerMasterOnceAfterHotplug(t *testing.T) {
	cfg := config.Default()
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.discover = func() []discovery.Device {
		slot := 1
		return []discovery.Device{{
			ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster",
			Enabled: true, Width: 480, Height: 480, Slot: &slot,
		}}
	}
	openCalls := 0
	transport := &runtimeFakeHID{}
	manager.openCooler = func(string) (devicecoolermaster.HIDTransport, error) {
		openCalls++
		return transport, nil
	}
	manager.coolerConflict = func() error { return nil }

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	devices := controller.Status().Devices
	if openCalls != 1 || len(devices) != 1 || devices[0].Info.ID != "coolermaster-atmos-v2" {
		t.Fatalf("酷冷热插拔重复打开或注册: opens=%d devices=%+v", openCalls, devices)
	}
	saved := store.Get()
	if len(saved.Devices) != 1 || saved.Devices[0].ID != "coolermaster-atmos-v2" ||
		saved.Devices[0].Kind != "coolermaster" || saved.Devices[0].Slot != defaultCoolerMasterSlot {
		t.Fatalf("酷冷热插拔配置未持久化: %+v", saved.Devices)
	}
}

func TestCoolerMasterOEMConflictBlocksHIDOpenAndRecovers(t *testing.T) {
	manager := newRuntimeDeviceManager(service.New(nil, time.Second), nil, t.TempDir())
	openCalls := 0
	manager.openCooler = func(string) (devicecoolermaster.HIDTransport, error) {
		openCalls++
		return &runtimeFakeHID{}, nil
	}
	manager.coolerConflict = func() error { return devicecoolermaster.ErrOEMControlConflict }
	if _, err := manager.openCoolerTransport("hidapi.dll"); !errors.Is(err, devicecoolermaster.ErrOEMControlConflict) {
		t.Fatalf("原厂后台冲突没有阻止 HID 打开: %v", err)
	}
	if openCalls != 0 {
		t.Fatalf("发现原厂冲突后仍打开 HID: %d", openCalls)
	}
	manager.coolerConflict = func() error { return nil }
	transport, err := manager.openCoolerTransport("hidapi.dll")
	if err != nil || transport == nil || openCalls != 1 {
		t.Fatalf("原厂后台退出后没有恢复打开: transport=%v opens=%d err=%v", transport, openCalls, err)
	}
}

func TestRuntimeDiscoveryReleasesConnectedCoolerWhenOEMStartsLater(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster", Enabled: true,
		Width: 480, Height: 480, Slot: defaultCoolerMasterSlot,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	transport := &runtimeFakeHID{}
	manager.registerDeviceLocked(cfg.Devices[0], transport)
	statePath := manager.entries[cfg.Devices[0].ID].cooler.StatePath
	if err := os.WriteFile(statePath, []byte(`{
  "trust_version": 1,
  "slots": {
    "4": {
      "digest": "possibly-overwritten",
      "file_name": "suspend_3.jpg",
      "replace_version": 2
    }
  }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: cfg.Devices[0].ID, Kind: "coolermaster", Width: 480, Height: 480}}
	}
	manager.status = func() service.Status {
		return service.Status{Devices: []service.DeviceStatus{{
			Info: device.Info{ID: cfg.Devices[0].ID}, Connected: true,
		}}}
	}
	manager.coolerConflict = func() error {
		return fmt.Errorf("%w；模拟原厂延迟启动", devicecoolermaster.ErrOEMControlConflict)
	}

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	closed := transport.closed
	transport.mu.Unlock()
	if !closed {
		t.Fatal("原厂稍后启动时没有释放 CaseScreen 的 HID 句柄")
	}
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stateData), `"uncertain": true`) {
		t.Fatalf("原厂接管后仍信任旧驻留摘要: %s", stateData)
	}
	status := controller.Status().Devices[0]
	if status.Connected || !strings.Contains(status.LastConnectionError, "已释放 HID") ||
		!strings.Contains(status.LastConnectionError, "自动恢复已保存主题") {
		t.Fatalf("冲突时的设备状态不清晰: %+v", status)
	}
}

func TestStartupOEMConflictInvalidatesCacheBeforeFirstConnection(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Kind: "coolermaster", Enabled: true,
		Width: 480, Height: 480, Slot: defaultCoolerMasterSlot,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.registerDeviceLocked(cfg.Devices[0], nil)
	statePath := manager.entries[cfg.Devices[0].ID].cooler.StatePath
	if err := os.WriteFile(statePath, []byte(`{
  "slots": {
    "4": {
      "digest": "old-version-cache",
      "file_name": "suspend_3.jpg",
      "replace_version": 2
    }
  }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.status = func() service.Status {
		return service.Status{Devices: []service.DeviceStatus{{
			Info: device.Info{ID: cfg.Devices[0].ID}, Connected: false,
		}}}
	}
	manager.coolerConflict = func() error {
		return fmt.Errorf("%w；模拟开机前已启动", devicecoolermaster.ErrOEMControlConflict)
	}
	manager.discover = func() []discovery.Device { return nil }

	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stateData), `"trust_version": 1`) ||
		!strings.Contains(string(stateData), `"uncertain": true`) {
		t.Fatalf("首次连接前没有持久化 OEM 覆盖风险: %s", stateData)
	}
}

func TestRuntimeDiscoveryDoesNotGuessDifferentWaveshareModel(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true,
		Port: "COM3", Width: 1024, Height: 600,
	}}
	store := runtimeTestStore(t, cfg)
	controller := service.New(nil, time.Second)
	manager := newRuntimeDeviceManager(controller, store, t.TempDir())
	manager.probeWaveshare = func(context.Context, string) (device.Info, error) {
		return runtimeWaveshareInfo("微雪 8.8 英寸", [2]int{480, 1920}), nil
	}
	manager.discover = func() []discovery.Device {
		return []discovery.Device{{ID: "waveshare-com8", Kind: "waveshare", Port: "COM8"}}
	}
	manager.RegisterConfigured(cfg)
	if err := manager.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.entries["waveshare-7"].waveshare.Port(); got != "COM3" {
		t.Fatalf("不同型号被强行迁移: %s", got)
	}
	if len(controller.Status().Devices) != 2 || manager.entries["waveshare-com8"] == nil {
		t.Fatalf("新型号没有独立接入: %+v", controller.Status().Devices)
	}
}

func runtimeTestStore(t *testing.T, cfg config.Config) *config.Store {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	return store
}

func runtimeWaveshareInfo(name string, resolution [2]int) device.Info {
	return device.Info{
		ID: "runtime-probe", Name: name, Resolution: resolution,
		Capabilities: device.Capabilities{
			DeliveryMode: device.DeliveryStoredTheme, StoresResources: true, SmallTelemetry: true,
		},
	}
}

type runtimeFakeHID struct {
	mu     sync.Mutex
	closed bool
}

func (*runtimeFakeHID) Write(context.Context, []byte, time.Duration) (int, error) {
	return 0, errors.New("测试 HID 未连接 Service.Run")
}

func (*runtimeFakeHID) Read(context.Context, []byte, time.Duration) (int, error) {
	return 0, errors.New("测试 HID 未连接 Service.Run")
}

func (transport *runtimeFakeHID) Close() error {
	transport.mu.Lock()
	transport.closed = true
	transport.mu.Unlock()
	return nil
}
