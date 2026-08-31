package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.TelemetryEnabled = false
	cfg.TelemetryIntervalM = 1500
	cfg.Devices = []Device{{ID: "wave-1", Kind: "waveshare", Enabled: true, Port: "COM7", Width: 1024, Height: 600}}
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := reopened.Get()
	if actual.TelemetryEnabled || actual.TelemetryIntervalM != 1500 || len(actual.Devices) != 1 {
		t.Fatalf("配置未正确保存: %+v", actual)
	}
}

func TestDefaultProductionConfigHasNoScreenRecognitionOrNetworking(t *testing.T) {
	cfg := Default()
	if cfg.ListenAddress != "127.0.0.1:19321" || !cfg.AudioEnabled || !cfg.LyricsEnabled || cfg.LyricsProvider != "auto" {
		t.Fatalf("正式主机默认值错误: %+v", cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower(data)
	for _, forbidden := range [][]byte{
		[]byte(`"recognition"`), []byte(`"acrcloud"`), []byte(`"microphone"`),
		[]byte(`"edge_sync"`), []byte(`"source_push"`), []byte(`"screen_network"`),
		[]byte(`"screen_cookie"`), []byte(`"relay_token"`),
	} {
		if bytes.Contains(lower, forbidden) {
			t.Fatalf("正式默认配置不得包含屏端实验字段 %s: %s", forbidden, data)
		}
	}
}

func TestOpenIgnoresRemovedEdgeSyncFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := map[string]any{
		"listen_address":    "127.0.0.1:19321",
		"edge_sync":         map[string]any{"enabled": true, "listen_address": "0.0.0.0:19322"},
		"telemetry_enabled": true, "telemetry_interval_ms": 1000,
		"audio_enabled": true, "audio_bands": 32, "lyrics_enabled": true,
		"lyrics_provider": "auto", "host_render_fps": 20, "jpeg_quality": 82,
		"start_minimized": true,
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("含旧 edge_sync 字段的配置应该继续可读: %v", err)
	}
	if err := store.Set(store.Get()); err != nil {
		t.Fatalf("保存迁移后的配置失败: %v", err)
	}
	if saved, err := os.ReadFile(store.Path()); err != nil {
		t.Fatal(err)
	} else if bytes.Contains(saved, []byte(`"edge_sync"`)) {
		t.Fatalf("迁移后仍保留已删除的 edge_sync 字段: %s", saved)
	}
}

func TestStoreRoundTripWaveshareTemplatesAndPlaylist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.WaveshareTemplates = map[string][]WaveshareTemplate{
		"waveshare-7": {{
			ID: "night", Name: "夜间", ManifestPath: filepath.Join(t.TempDir(), "night.json"),
			RemoteThemePath: "/data/theme/1024x600/night.Theme", SizeBytes: 1234,
			UpdatedAt: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		}},
	}
	cfg.WavesharePlaylists = map[string]WavesharePlaylist{
		"waveshare-7": {Enabled: true, Mode: "random", DefaultDurationSeconds: 12, Entries: []WavesharePlaylistEntry{{TemplateID: "night"}}},
	}
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}

	// Get 必须深拷贝，调用方改写切片不能污染存储中的配置。
	copy := store.Get()
	copy.WaveshareTemplates["waveshare-7"][0].Name = "被修改"
	copy.WavesharePlaylists["waveshare-7"] = WavesharePlaylist{}
	if actual := store.Get(); actual.WaveshareTemplates["waveshare-7"][0].Name != "夜间" || !actual.WavesharePlaylists["waveshare-7"].Enabled || actual.WavesharePlaylists["waveshare-7"].DefaultDurationSeconds != 12 {
		t.Fatalf("模板配置没有完整深拷贝: %+v", actual)
	}
}

func TestStoreUpdateSerializesConcurrentFieldChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- store.Update(func(cfg *Config) error {
			close(firstEntered)
			<-releaseFirst
			cfg.TelemetryEnabled = false
			return nil
		})
	}()
	<-firstEntered
	go func() {
		secondDone <- store.Update(func(cfg *Config) error {
			close(secondEntered)
			cfg.AudioEnabled = false
			return nil
		})
	}()
	select {
	case <-secondEntered:
		close(releaseFirst)
		<-firstDone
		<-secondDone
		t.Fatal("第二个更新不应在第一个更新提交前读取配置")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	for _, result := range []<-chan error{firstDone, secondDone} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("并发配置更新没有结束")
		}
	}
	actual := store.Get()
	if actual.TelemetryEnabled || actual.AudioEnabled {
		t.Fatalf("并发更新丢失字段: %+v", actual)
	}
}

func TestOpenOldConfigWithoutWaveshareLibrary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := Default()
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧配置应该继续可读: %v", err)
	}
	if got := store.Get(); got.WaveshareTemplates != nil || got.WavesharePlaylists != nil {
		t.Fatalf("旧配置不应被错误填充: %+v", got)
	}
}

func TestConfigRejectsInvalidWavesharePlaylist(t *testing.T) {
	cfg := Default()
	cfg.WaveshareTemplates = map[string][]WaveshareTemplate{
		"waveshare-7": {{ID: "valid", Name: "有效", ManifestPath: "valid.json"}},
	}
	cfg.WavesharePlaylists = map[string]WavesharePlaylist{
		"waveshare-7": {Enabled: true, Mode: "sequence", Entries: []WavesharePlaylistEntry{{TemplateID: "missing", DurationSeconds: 5}}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("应拒绝引用不存在模板的轮播配置")
	}
	cfg.WavesharePlaylists = map[string]WavesharePlaylist{
		"coolermaster-atmos-v2": {Enabled: true, Mode: "sequence"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("应拒绝非微雪目标的轮播配置")
	}
}

func TestConfigAcceptsGlobalWaveshareDurationWithPerTemplateOverride(t *testing.T) {
	cfg := Default()
	cfg.WaveshareTemplates = map[string][]WaveshareTemplate{
		"waveshare-7": {
			{ID: "inherit", Name: "继承", ManifestPath: "inherit.json"},
			{ID: "override", Name: "覆盖", ManifestPath: "override.json"},
		},
	}
	cfg.WavesharePlaylists = map[string]WavesharePlaylist{
		"waveshare-7": {
			Enabled: true, Mode: "sequence", DefaultDurationSeconds: 45,
			Entries: []WavesharePlaylistEntry{
				{TemplateID: "inherit"},
				{TemplateID: "override", DurationSeconds: 9},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("统一时长和单项覆盖应可同时使用: %v", err)
	}
	playlist := cfg.WavesharePlaylists["waveshare-7"]
	playlist.DefaultDurationSeconds = -1
	cfg.WavesharePlaylists["waveshare-7"] = playlist
	if err := cfg.Validate(); err == nil {
		t.Fatal("应拒绝负数统一时长")
	}
}

func TestConfigRejectsRemoteListener(t *testing.T) {
	cfg := Default()
	cfg.ListenAddress = "0.0.0.0:19321"
	if err := cfg.Validate(); err == nil {
		t.Fatal("应拒绝暴露到局域网的设置服务")
	}
}

func TestConfigRejectsRetiredTitleSearchLyricsProvider(t *testing.T) {
	cfg := Default()
	cfg.LyricsProvider = "sogou"
	if err := cfg.Validate(); err == nil {
		t.Fatal("已经退役的搜狗标题搜索来源不应继续受支持")
	}
}

func TestMigrateRetiredLyricsProviderUsesStrictAutoMode(t *testing.T) {
	for _, provider := range []string{"kugou", "sogou"} {
		cfg := Default()
		cfg.LyricsProvider = provider
		if !migrateRetiredLyricsProvider(&cfg) || cfg.LyricsProvider != "auto" {
			t.Fatalf("旧歌词来源没有迁移到严格自动模式: source=%q result=%q", provider, cfg.LyricsProvider)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("迁移后的配置无效: %v", err)
		}
	}
}

func TestConfigRejectsRetiredThermalrightDevice(t *testing.T) {
	cfg := Default()
	cfg.Devices = []Device{{
		ID: "retired", Kind: "thermalright", Enabled: true, Width: 1920, Height: 468,
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("新配置不应再接受已移除的利民设备类型")
	}
}

func TestOpenMigratesRetiredThermalrightDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := Default()
	legacy.Devices = []Device{
		{ID: "thermal-old", Kind: "thermalright", Enabled: true, Width: 1920, Height: 468},
		{ID: "waveshare-7", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
	}
	legacy.DeviceThemePaths = map[string]string{
		"thermal-old":         "retired.json",
		"thermalright-orphan": "orphan.json",
		"waveshare-7":         "active.json",
	}
	legacy.WaveshareTemplates = map[string][]WaveshareTemplate{
		"thermal-old": {{ID: "retired", Name: "旧模板", ManifestPath: "retired.json"}},
	}
	legacy.WavesharePlaylists = map[string]WavesharePlaylist{
		"thermal-old": {Enabled: true, Mode: "sequence", Entries: []WavesharePlaylistEntry{{TemplateID: "retired"}}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("包含已退役设备的旧配置应可继续启动: %v", err)
	}
	got := store.Get()
	if len(got.Devices) != 1 || got.Devices[0].ID != "waveshare-7" {
		t.Fatalf("旧利民设备没有从运行配置中移除: %+v", got.Devices)
	}
	if _, ok := got.DeviceThemePaths["thermal-old"]; ok {
		t.Fatal("旧利民主题绑定没有清理")
	}
	if _, ok := got.DeviceThemePaths["thermalright-orphan"]; ok {
		t.Fatal("没有设备条目的旧利民绑定没有清理")
	}
	if _, ok := got.WaveshareTemplates["thermal-old"]; ok {
		t.Fatal("旧利民模板记录没有清理")
	}
	if _, ok := got.WavesharePlaylists["thermal-old"]; ok {
		t.Fatal("旧利民轮播记录没有清理")
	}
	if got.DeviceThemePaths["waveshare-7"] != "active.json" {
		t.Fatal("迁移不应影响仍受支持设备的主题绑定")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(saved), []byte("thermalright")) || bytes.Contains(saved, []byte("thermal-old")) {
		t.Fatalf("迁移结果没有永久写回配置文件: %s", saved)
	}
}

func TestConfigRejectsInvalidDeviceDefinitions(t *testing.T) {
	tests := []struct {
		name    string
		devices []Device
	}{
		{
			name: "重复 ID",
			devices: []Device{
				{ID: "same", Kind: "waveshare", Port: "COM3", Width: 1024, Height: 600},
				{ID: "same", Kind: "coolermaster", Width: 480, Height: 480},
			},
		},
		{
			name: "重复微雪串口",
			devices: []Device{
				{ID: "waveshare-7", Kind: "waveshare", Port: "COM4", Width: 1024, Height: 600},
				{ID: "waveshare-auto", Kind: "waveshare", Port: " com4 ", Width: 1024, Height: 600},
			},
		},
		{name: "未知类型", devices: []Device{{ID: "unknown", Kind: "unknown", Width: 1, Height: 1}}},
		{name: "空分辨率", devices: []Device{{ID: "cooler", Kind: "coolermaster"}}},
		{name: "酷冷槽位零", devices: []Device{{ID: "cooler-zero", Kind: "coolermaster", Width: 480, Height: 480, Slot: 0}}},
		{name: "酷冷槽位六", devices: []Device{{ID: "cooler-six", Kind: "coolermaster", Width: 480, Height: 480, Slot: 6}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.Devices = test.devices
			if err := cfg.Validate(); err == nil {
				t.Fatalf("应拒绝配置: %+v", test.devices)
			}
		})
	}
}

func TestOpenMigratesOnlyLegacyCoolerMasterSlotZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := Default()
	legacy.Devices = []Device{
		{ID: "cooler-zero", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: 0},
		{ID: "cooler-four", Kind: "coolermaster", Enabled: true, Width: 480, Height: 480, Slot: 4},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧版 0 基槽位配置迁移失败: %v", err)
	}
	got := store.Get()
	if len(got.Devices) != 2 || got.Devices[0].Slot != 1 || got.Devices[1].Slot != 4 {
		t.Fatalf("槽位迁移范围错误: %+v", got.Devices)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(saved, &persisted); err != nil {
		t.Fatalf("迁移后的配置无法解析: %v", err)
	}
	if len(persisted.Devices) != 2 || persisted.Devices[0].Slot != 1 || persisted.Devices[1].Slot != 4 {
		t.Fatalf("槽位迁移没有永久写回或改动了用户槽位 4: %+v", persisted.Devices)
	}
}

func TestOpenMigratesOnlyProvableDuplicateWavesharePort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := Default()
	legacy.Devices = []Device{
		// 不同端口代表可能暂时离线的另一块屏，必须原样保留。
		{ID: "waveshare-7", Name: "微雪 7 英寸", Kind: "waveshare", Enabled: true, Port: "COM3", Width: 1024, Height: 600},
		{ID: "waveshare-com4", Name: "微雪 USB Monitor（COM4）", Kind: "waveshare", Enabled: true, Port: "com4", Width: 480, Height: 1920},
		{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 480, Height: 1920},
	}
	legacy.DeviceThemePaths = map[string]string{"waveshare-8.8": "active.json"}
	legacy.WaveshareTemplates = map[string][]WaveshareTemplate{
		"waveshare-8.8": {{ID: "night", Name: "夜间", ManifestPath: "night.json"}},
	}
	legacy.WavesharePlaylists = map[string]WavesharePlaylist{
		"waveshare-8.8": {Enabled: true, Mode: "sequence", Entries: []WavesharePlaylistEntry{{TemplateID: "night"}}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("同端口旧配置应自动迁移: %v", err)
	}
	got := store.Get()
	if len(got.Devices) != 2 || got.Devices[0].ID != "waveshare-7" || got.Devices[0].Port != "COM3" ||
		got.Devices[1].ID != "waveshare-8.8" || got.Devices[1].Port != "COM4" {
		t.Fatalf("同端口去重或离线屏保留错误: %+v", got.Devices)
	}
	if got.DeviceThemePaths["waveshare-8.8"] != "active.json" ||
		len(got.WaveshareTemplates["waveshare-8.8"]) != 1 ||
		len(got.WavesharePlaylists["waveshare-8.8"].Entries) != 1 {
		t.Fatalf("重复条目的主题状态没有迁移到稳定 ID: %+v", got)
	}
	if _, exists := got.DeviceThemePaths["waveshare-com4"]; exists {
		t.Fatal("自动发现重复 ID 的主题绑定仍然残留")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved, []byte(`"waveshare-com4"`)) {
		t.Fatalf("同端口去重结果没有写回磁盘: %s", saved)
	}
}

func TestMigrateDuplicateWavesharePortsNeverDropsDistinctPersistentDevices(t *testing.T) {
	tests := []struct {
		name    string
		devices []Device
	}{
		{
			name: "不同强身份",
			devices: []Device{
				{ID: "seven", Name: "七寸", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:1d6b:0104:seven", Width: 1024, Height: 600},
				{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:1d6b:0104:portrait", Width: 480, Height: 1920},
			},
		},
		{
			name: "两个有状态逻辑设备",
			devices: []Device{
				{ID: "seven", Name: "七寸", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 1024, Height: 600},
				{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM4", Width: 480, Height: 1920},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.Devices = test.devices
			cfg.DeviceThemePaths = map[string]string{"seven": "seven.json", "portrait": "portrait.json"}
			cfg.WaveshareTemplates = map[string][]WaveshareTemplate{
				"seven":    {{ID: "seven", ManifestPath: "seven.json"}},
				"portrait": {{ID: "portrait", ManifestPath: "portrait.json"}},
			}
			if migrateDuplicateWavesharePorts(&cfg) {
				t.Fatalf("不可证明为别名的同 COM 条目不应被自动改写: %+v", cfg.Devices)
			}
			if len(cfg.Devices) != 2 || cfg.DeviceThemePaths["seven"] != "seven.json" ||
				cfg.DeviceThemePaths["portrait"] != "portrait.json" ||
				len(cfg.WaveshareTemplates["seven"]) != 1 || len(cfg.WaveshareTemplates["portrait"]) != 1 {
				t.Fatalf("同 COM 冲突静默丢失了主题/模板: %+v", cfg)
			}
		})
	}
}

func TestOpenAmbiguousDuplicateWavesharePortDoesNotRewriteDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.Devices = []Device{
		{ID: "seven", Name: "七寸", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:one", Width: 1024, Height: 600},
		{ID: "portrait", Name: "竖屏", Kind: "waveshare", Enabled: true, Port: "COM4", StableID: "usb-serial:two", Width: 480, Height: 1920},
	}
	cfg.DeviceThemePaths = map[string]string{"seven": "seven.json", "portrait": "portrait.json"}
	original, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("不可证明的同 COM 冲突应明确报错，而不是静默删除设备")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, original) {
		t.Fatalf("Open 失败时仍改写了含两份主题的原配置: %s", saved)
	}
}
