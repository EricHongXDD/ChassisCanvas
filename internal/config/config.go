package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"casescreen/internal/gpupreference"
)

const (
	defaultListenAddress = "127.0.0.1:19321"
	defaultIntervalMS    = 1000
	// DefaultWavesharePlaylistDurationSeconds 是旧配置未保存统一时长时的兼容默认值。
	DefaultWavesharePlaylistDurationSeconds = 30
)

type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Port    string `json:"port,omitempty"`
	// StableID 仅保存固件报告的 USB 序列号等强物理身份。
	StableID    string `json:"stable_id,omitempty"`
	LibraryPath string `json:"library_path,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Slot        int    `json:"slot,omitempty"`
	// DisplayRotation 保存酷冷至尊冷头相对“尖角朝下”逻辑画布的校准角度。
	// 30° 步进同时覆盖原厂 90° 旋转档位和六边形磁吸外壳的安装方向。
	DisplayRotation       int  `json:"display_rotation,omitempty"`
	OrientationCalibrated bool `json:"orientation_calibrated,omitempty"`
	OrientationProfile    int  `json:"orientation_profile,omitempty"`
	PoweredOff            bool `json:"powered_off"`
}

// WaveshareTemplate 记录一份已经编译完成的微雪驻留主题。
// ManifestPath 指向包含持久化 .Theme 资源的清单，切换时不再重复编译。
type WaveshareTemplate struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	ManifestPath    string    `json:"manifest_path"`
	RemoteThemePath string    `json:"remote_theme_path"`
	SizeBytes       int64     `json:"size_bytes"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type WavesharePlaylistEntry struct {
	TemplateID      string `json:"template_id"`
	DurationSeconds int    `json:"duration_seconds"`
}

type WavesharePlaylist struct {
	Enabled                bool                     `json:"enabled"`
	Mode                   string                   `json:"mode"`
	DefaultDurationSeconds int                      `json:"default_duration_seconds,omitempty"`
	Entries                []WavesharePlaylistEntry `json:"entries,omitempty"`
}

type Config struct {
	ListenAddress      string `json:"listen_address"`
	TelemetryEnabled   bool   `json:"telemetry_enabled"`
	TelemetryIntervalM int    `json:"telemetry_interval_ms"`
	AudioEnabled       bool   `json:"audio_enabled"`
	AudioBands         int    `json:"audio_bands"`
	LyricsEnabled      bool   `json:"lyrics_enabled"`
	LyricsProvider     string `json:"lyrics_provider,omitempty"`
	LyricsPath         string `json:"lyrics_path,omitempty"`
	HostRenderFPS      int    `json:"host_render_fps"`
	JPEGQuality        int    `json:"jpeg_quality"`
	FFmpegPath         string `json:"ffmpeg_path,omitempty"`
	GPUPreference      string `json:"gpu_preference"`
	GPUAdapterID       string `json:"gpu_adapter_id,omitempty"`
	GPUAdapterName     string `json:"gpu_adapter_name,omitempty"`
	// TelemetryGPUAdapterID 只决定遥测组件读取哪块物理 GPU，和上面的
	// CaseScreen 进程运算显卡首选项完全独立。auto 表示自动选择独显。
	TelemetryGPUAdapterID   string                         `json:"telemetry_gpu_adapter_id,omitempty"`
	TelemetryGPUAdapterName string                         `json:"telemetry_gpu_adapter_name,omitempty"`
	StartMinimized          bool                           `json:"start_minimized"`
	LastThemePath           string                         `json:"last_theme_path,omitempty"`
	DeviceThemePaths        map[string]string              `json:"device_theme_paths,omitempty"`
	WaveshareTemplates      map[string][]WaveshareTemplate `json:"waveshare_templates,omitempty"`
	WavesharePlaylists      map[string]WavesharePlaylist   `json:"waveshare_playlists,omitempty"`
	Devices                 []Device                       `json:"devices,omitempty"`
}

var (
	waveshareTemplateIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	autoWaveshareIDPattern     = regexp.MustCompile(`(?i)^waveshare-com[1-9][0-9]*(?:-[1-9][0-9]*)?$`)
)

func Default() Config {
	// 正式默认只启用 Windows 主机遥测、WASAPI 频谱与主机歌词。
	// 屏端识歌、网络和登录从未是 Config 字段，也不能由默认配置启用。
	return Config{
		ListenAddress:      defaultListenAddress,
		TelemetryEnabled:   true,
		TelemetryIntervalM: defaultIntervalMS,
		AudioEnabled:       true,
		AudioBands:         32,
		LyricsEnabled:      true,
		LyricsProvider:     "auto",
		HostRenderFPS:      20,
		JPEGQuality:        82,
		GPUPreference:      string(gpupreference.SystemDefault),
		StartMinimized:     true,
	}
}

func DefaultPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "CaseScreen", "config.json"), nil
}

// writableDirectory 确认目录可以创建并写入文件。部分受控 Windows 环境会允许读取
// Roaming 目录，却禁止应用创建子目录；启动时需要及时发现这种情况并切换到回退目录。
func writableDirectory(directory string) error {
	if directory == "" {
		return errors.New("目录为空")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(directory, ".casescreen-write-*")
	if err != nil {
		return err
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return err
	}
	if err := os.Remove(probePath); err != nil {
		return err
	}
	return nil
}

func fallbackConfigPath(original string) (string, error) {
	originalDirectory := filepath.Clean(filepath.Dir(original))
	candidates := make([]string, 0, 3)
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "CaseScreen-data", "config.json"))
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(workingDirectory, "CaseScreen-data", "config.json"))
	}
	if temporaryDirectory := os.TempDir(); temporaryDirectory != "" {
		candidates = append(candidates, filepath.Join(temporaryDirectory, "CaseScreen", "config.json"))
	}
	var failures []string
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, ok := seen[candidate]; ok || filepath.Clean(filepath.Dir(candidate)) == originalDirectory {
			continue
		}
		seen[candidate] = struct{}{}
		if err := writableDirectory(filepath.Dir(candidate)); err != nil {
			failures = append(failures, candidate+": "+err.Error())
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("找不到可写的配置回退目录: %s", strings.Join(failures, "；"))
}

func (cfg Config) Validate() error {
	if _, err := gpupreference.Parse(cfg.GPUPreference); err != nil {
		return err
	}
	if err := validateGPUAdapterSelection(cfg.GPUAdapterID, cfg.GPUAdapterName); err != nil {
		return err
	}
	if err := validateTelemetryGPUAdapterSelection(cfg.TelemetryGPUAdapterID, cfg.TelemetryGPUAdapterName); err != nil {
		return err
	}
	switch cfg.LyricsProvider {
	case "", "auto", "netease", "qq", "local":
	default:
		return fmt.Errorf("歌词提供方不受支持: %q", cfg.LyricsProvider)
	}
	if cfg.TelemetryIntervalM < 250 || cfg.TelemetryIntervalM > 60000 {
		return errors.New("遥测间隔必须在 250..60000 毫秒之间")
	}
	if cfg.AudioBands < 4 || cfg.AudioBands > 128 {
		return errors.New("音频频带数必须在 4..128 之间")
	}
	if cfg.HostRenderFPS < 1 || cfg.HostRenderFPS > 30 {
		return errors.New("主机渲染帧率必须在 1..30 之间")
	}
	if cfg.JPEGQuality < 35 || cfg.JPEGQuality > 95 {
		return errors.New("JPEG 质量必须在 35..95 之间")
	}
	host, _, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("本地监听地址无效: %w", err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return errors.New("设置服务只能监听本机回环地址")
	}
	identifiers := make(map[string]bool, len(cfg.Devices))
	wavesharePorts := make(map[string]string)
	for index, device := range cfg.Devices {
		if device.ID == "" || device.Kind == "" {
			return fmt.Errorf("第 %d 个设备缺少 ID 或类型", index+1)
		}
		if identifiers[device.ID] {
			return fmt.Errorf("设备 ID 重复: %q", device.ID)
		}
		identifiers[device.ID] = true
		switch device.Kind {
		case "waveshare", "coolermaster":
		default:
			return fmt.Errorf("设备 %q 的类型不受支持: %q", device.ID, device.Kind)
		}
		if device.Kind == "waveshare" && device.Port == "" {
			return fmt.Errorf("微雪设备 %q 缺少串口", device.ID)
		}
		if device.Kind == "waveshare" {
			port := strings.ToUpper(strings.TrimSpace(device.Port))
			if existingID, exists := wavesharePorts[port]; exists {
				return fmt.Errorf("微雪设备 %q 与 %q 重复使用串口 %s", existingID, device.ID, port)
			}
			wavesharePorts[port] = device.ID
		}
		if device.Kind == "coolermaster" && (device.Slot < 1 || device.Slot > 5) {
			return fmt.Errorf("酷冷至尊设备 %q 的离线槽位必须在 1..5", device.ID)
		}
		if device.Kind == "coolermaster" && !validCoolerDisplayRotation(device.DisplayRotation) {
			return fmt.Errorf("酷冷至尊设备 %q 的显示方向必须为 0..330 度之间的 30 度整数倍", device.ID)
		}
		if device.Kind == "coolermaster" && device.OrientationProfile != 0 && device.OrientationProfile != 1 {
			return fmt.Errorf("酷冷至尊设备 %q 的方向校准版本无效", device.ID)
		}
		if device.Width <= 0 || device.Height <= 0 {
			return fmt.Errorf("设备 %q 分辨率无效", device.ID)
		}
	}
	for deviceID, templates := range cfg.WaveshareTemplates {
		if !cfg.isWaveshareTarget(deviceID) {
			return fmt.Errorf("微雪模板库目标无效: %q", deviceID)
		}
		seen := make(map[string]bool, len(templates))
		for _, record := range templates {
			if !waveshareTemplateIDPattern.MatchString(record.ID) {
				return fmt.Errorf("微雪模板 ID 无效: %q", record.ID)
			}
			if seen[record.ID] {
				return fmt.Errorf("微雪模板 ID 重复: %q", record.ID)
			}
			seen[record.ID] = true
			if strings.TrimSpace(record.Name) == "" || strings.TrimSpace(record.ManifestPath) == "" {
				return fmt.Errorf("微雪模板 %q 缺少名称或清单路径", record.ID)
			}
			if record.SizeBytes < 0 {
				return fmt.Errorf("微雪模板 %q 的大小无效", record.ID)
			}
		}
	}
	for deviceID, playlist := range cfg.WavesharePlaylists {
		if !cfg.isWaveshareTarget(deviceID) {
			return fmt.Errorf("微雪轮播目标无效: %q", deviceID)
		}
		if playlist.Mode != "" && playlist.Mode != "sequence" && playlist.Mode != "random" {
			return fmt.Errorf("微雪轮播模式无效: %q", playlist.Mode)
		}
		if playlist.Enabled && len(playlist.Entries) == 0 {
			return fmt.Errorf("微雪轮播 %q 已启用但没有模板", deviceID)
		}
		if playlist.DefaultDurationSeconds < 0 || playlist.DefaultDurationSeconds > 86400 {
			return fmt.Errorf("微雪轮播 %q 的统一时长必须留空使用默认值，或在 1..86400 秒之间", deviceID)
		}
		available := make(map[string]bool)
		for _, record := range cfg.WaveshareTemplates[deviceID] {
			available[record.ID] = true
		}
		seenEntries := make(map[string]bool, len(playlist.Entries))
		for _, entry := range playlist.Entries {
			if !waveshareTemplateIDPattern.MatchString(entry.TemplateID) || !available[entry.TemplateID] {
				return fmt.Errorf("微雪轮播引用了不存在的模板: %q", entry.TemplateID)
			}
			if seenEntries[entry.TemplateID] {
				return fmt.Errorf("微雪轮播模板重复: %q", entry.TemplateID)
			}
			seenEntries[entry.TemplateID] = true
			if entry.DurationSeconds < 0 || entry.DurationSeconds > 86400 {
				return fmt.Errorf("微雪模板 %q 的单独轮播时长必须留空继承或在 1..86400 秒之间", entry.TemplateID)
			}
		}
	}
	return nil
}

func validCoolerDisplayRotation(value int) bool {
	return value >= 0 && value < 360 && value%30 == 0
}

func validateGPUAdapterSelection(adapterID, adapterName string) error {
	adapterID = strings.TrimSpace(adapterID)
	adapterName = strings.TrimSpace(adapterName)
	if (adapterID == "") != (adapterName == "") {
		return errors.New("GPU 适配器 ID 与名称必须同时设置")
	}
	if len([]rune(adapterID)) > 512 || len([]rune(adapterName)) > 256 {
		return errors.New("GPU 适配器标识过长")
	}
	if strings.ContainsAny(adapterID, "\x00\r\n") || strings.ContainsAny(adapterName, "\x00\r\n") {
		return errors.New("GPU 适配器标识包含控制字符")
	}
	return nil
}

func validateTelemetryGPUAdapterSelection(adapterID, adapterName string) error {
	adapterID = strings.TrimSpace(adapterID)
	adapterName = strings.TrimSpace(adapterName)
	if adapterID == "" || strings.EqualFold(adapterID, "auto") {
		if adapterName != "" {
			return errors.New("自动监控 GPU 不能保存适配器名称")
		}
		return nil
	}
	if adapterName == "" {
		return errors.New("监控 GPU 适配器 ID 与名称必须同时设置")
	}
	if len([]rune(adapterID)) > 512 || len([]rune(adapterName)) > 256 ||
		strings.ContainsAny(adapterID, "\x00\r\n") || strings.ContainsAny(adapterName, "\x00\r\n") {
		return errors.New("监控 GPU 适配器标识无效")
	}
	if !strings.Contains(strings.ToLower(adapterID), `pci\ven_`) {
		return errors.New("监控 GPU 必须使用物理 PCI 适配器 ID 或 auto")
	}
	return nil
}

func (cfg Config) isWaveshareTarget(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	for _, device := range cfg.Devices {
		if device.ID == deviceID {
			return device.Kind == "waveshare"
		}
	}
	// 兼容未配置 Devices 列表的旧版本微雪 ID。
	return strings.HasPrefix(strings.ToLower(deviceID), "waveshare")
}

type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

func Open(path string) (*Store, error) {
	defaultPathRequested := path == ""
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("配置路径无效: %w", err)
	}
	path = absolutePath
	store := &Store{path: filepath.Clean(path), cfg: Default()}
	data, err := os.ReadFile(store.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := store.save(store.cfg); err != nil {
			if !defaultPathRequested {
				return nil, err
			}
			fallbackPath, fallbackErr := fallbackConfigPath(store.path)
			if fallbackErr != nil {
				return nil, fmt.Errorf("默认配置目录不可写 (%v)，且回退目录不可用: %w", err, fallbackErr)
			}
			fallbackStore := &Store{path: filepath.Clean(fallbackPath), cfg: clone(store.cfg)}
			if saveErr := fallbackStore.save(fallbackStore.cfg); saveErr != nil {
				return nil, fmt.Errorf("迁移到可写配置目录失败: %w", saveErr)
			}
			return fallbackStore, nil
		}
		return store, nil
	}
	if err := json.Unmarshal(data, &store.cfg); err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	// 酷狗/搜狗属于旧版按标题搜索链路。读取旧配置时静默迁移到
	// 严格自动模式，避免升级后无法启动，也绝不继续联网猜歌。
	migrateRetiredLyricsProvider(&store.cfg)
	// 利民运行时已经移除。旧配置静默剔除对应设备及其绑定数据，避免
	// 用户升级后因为一个已退役设备条目而无法启动其余两类副屏。
	retiredDevicesMigrated := migrateRetiredDevices(&store.cfg)
	duplicateWavesharePortsMigrated := migrateDuplicateWavesharePorts(&store.cfg)
	coolerMasterSlotMigrated := migrateCoolerMasterSlotNumbering(&store.cfg)
	migrateGPUPreference(&store.cfg)
	if err := store.cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置无效: %w", err)
	}
	if defaultPathRequested {
		if err := writableDirectory(filepath.Dir(store.path)); err != nil {
			fallbackPath, fallbackErr := fallbackConfigPath(store.path)
			if fallbackErr != nil {
				return nil, fmt.Errorf("默认配置目录不可写 (%v)，且回退目录不可用: %w", err, fallbackErr)
			}
			fallbackStore := &Store{path: filepath.Clean(fallbackPath), cfg: clone(store.cfg)}
			if saveErr := fallbackStore.save(fallbackStore.cfg); saveErr != nil {
				return nil, fmt.Errorf("迁移到可写配置目录失败: %w", saveErr)
			}
			return fallbackStore, nil
		}
	}
	if retiredDevicesMigrated || duplicateWavesharePortsMigrated || coolerMasterSlotMigrated {
		if err := store.save(store.cfg); err != nil {
			return nil, fmt.Errorf("保存设备配置迁移结果失败: %w", err)
		}
	}
	return store, nil
}

// migrateCoolerMasterSlotNumbering 兼容旧版曾直接暴露内部 0 基索引的
// 第一个槽位。新版统一采用原厂界面可见的 1..5；其余 1..4 数字无法
// 从配置本身判断用户意图，保持原值，避免擅自切换到相邻槽位。
func migrateCoolerMasterSlotNumbering(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	changed := false
	for index := range cfg.Devices {
		configured := &cfg.Devices[index]
		if strings.EqualFold(strings.TrimSpace(configured.Kind), "coolermaster") && configured.Slot == 0 {
			configured.Slot = 1
			changed = true
		}
	}
	return changed
}

func migrateRetiredLyricsProvider(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg.LyricsProvider)) {
	case "kugou", "sogou":
		cfg.LyricsProvider = "auto"
		return true
	default:
		return false
	}
}

func migrateRetiredDevices(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	retiredIDs := make(map[string]struct{})
	retained := make([]Device, 0, len(cfg.Devices))
	for _, configured := range cfg.Devices {
		if strings.EqualFold(strings.TrimSpace(configured.Kind), "thermalright") {
			retiredIDs[configured.ID] = struct{}{}
			continue
		}
		retained = append(retained, configured)
	}
	// 兼容设备条目已经手工删除、但旧版绑定映射仍残留的配置。
	for deviceID := range cfg.DeviceThemePaths {
		if isRetiredDeviceID(deviceID) {
			retiredIDs[deviceID] = struct{}{}
		}
	}
	for deviceID := range cfg.WaveshareTemplates {
		if isRetiredDeviceID(deviceID) {
			retiredIDs[deviceID] = struct{}{}
		}
	}
	for deviceID := range cfg.WavesharePlaylists {
		if isRetiredDeviceID(deviceID) {
			retiredIDs[deviceID] = struct{}{}
		}
	}
	if len(retiredIDs) == 0 {
		return false
	}
	cfg.Devices = retained
	for deviceID := range retiredIDs {
		delete(cfg.DeviceThemePaths, deviceID)
		delete(cfg.WaveshareTemplates, deviceID)
		delete(cfg.WavesharePlaylists, deviceID)
	}
	return true
}

// migrateDuplicateWavesharePorts 只清理“一个持久逻辑设备 + 若干无状态自动别名”。
// COM 号只是 Windows 当前端点，不能单独证明两条配置属于同一块屏；若两条
// 都含用户状态或强身份冲突，宁可让 Validate 报出可恢复冲突，也绝不静默删主题。
func migrateDuplicateWavesharePorts(cfg *Config) bool {
	if cfg == nil || len(cfg.Devices) < 2 {
		return false
	}
	groups := make(map[string][]int)
	changed := false
	for index := range cfg.Devices {
		configured := &cfg.Devices[index]
		if !strings.EqualFold(strings.TrimSpace(configured.Kind), "waveshare") {
			continue
		}
		port := strings.ToUpper(strings.TrimSpace(configured.Port))
		if port == "" {
			continue
		}
		if configured.Port != port {
			configured.Port = port
			changed = true
		}
		groups[port] = append(groups[port], index)
	}

	removed := make(map[int]bool)
	aliases := make(map[string]string)
	for _, indexes := range groups {
		if len(indexes) < 2 {
			continue
		}
		persistent := make([]int, 0, len(indexes))
		for _, index := range indexes {
			if waveshareDeviceHasPersistentState(cfg, cfg.Devices[index]) {
				persistent = append(persistent, index)
			}
		}
		if len(persistent) != 1 {
			// 没有明确所有者，或多个条目各自含状态，均不可自动合并。
			continue
		}
		keeperIndex := persistent[0]
		keeper := cfg.Devices[keeperIndex]
		canMerge := true
		for _, index := range indexes {
			if index == keeperIndex {
				continue
			}
			candidate := cfg.Devices[index]
			if !waveshareStatelessAutoAlias(cfg, candidate) ||
				waveshareStableIDsConflict(keeper.StableID, candidate.StableID) {
				canMerge = false
				break
			}
		}
		if !canMerge {
			continue
		}
		for _, index := range indexes {
			if index == keeperIndex {
				continue
			}
			removed[index] = true
			aliases[cfg.Devices[index].ID] = keeper.ID
		}
		changed = true
	}
	for index := range cfg.Devices {
		if !strings.EqualFold(strings.TrimSpace(cfg.Devices[index].Kind), "waveshare") {
			continue
		}
		normalized := strings.ToUpper(strings.TrimSpace(cfg.Devices[index].Port))
		if normalized != "" && cfg.Devices[index].Port != normalized {
			cfg.Devices[index].Port = normalized
			changed = true
		}
	}
	if !changed {
		return false
	}
	if len(removed) > 0 {
		retained := make([]Device, 0, len(cfg.Devices)-len(removed))
		for index, configured := range cfg.Devices {
			if !removed[index] {
				retained = append(retained, configured)
			}
		}
		cfg.Devices = retained
	}
	for removedID, keeperID := range aliases {
		rebindWaveshareState(cfg, removedID, keeperID)
	}
	return true
}

func waveshareDeviceHasPersistentState(cfg *Config, configured Device) bool {
	if !autoWaveshareIDPattern.MatchString(strings.TrimSpace(configured.ID)) || !configured.Enabled ||
		configured.PoweredOff || strings.TrimSpace(configured.StableID) != "" ||
		!defaultWaveshareDeviceName(configured.Name, configured.ID) {
		return true
	}
	if strings.TrimSpace(cfg.DeviceThemePaths[configured.ID]) != "" || len(cfg.WaveshareTemplates[configured.ID]) > 0 {
		return true
	}
	_, hasPlaylist := cfg.WavesharePlaylists[configured.ID]
	return hasPlaylist
}

func waveshareStatelessAutoAlias(cfg *Config, configured Device) bool {
	return autoWaveshareIDPattern.MatchString(strings.TrimSpace(configured.ID)) &&
		!waveshareDeviceHasPersistentState(cfg, configured)
}

func waveshareStableIDsConflict(left, right string) bool {
	left = strings.ToLower(strings.TrimSpace(left))
	right = strings.ToLower(strings.TrimSpace(right))
	return left != "" && right != "" && left != right
}

func defaultWaveshareDeviceName(name, id string) bool {
	name = strings.TrimSpace(name)
	return name == "" || name == strings.TrimSpace(id) || name == "微雪" || name == "微雪 7 英寸" ||
		name == "微雪 8.8 英寸" || strings.HasPrefix(name, "微雪 USB Monitor")
}

func rebindWaveshareState(cfg *Config, removedID, keeperID string) {
	if cfg == nil || removedID == "" || keeperID == "" || removedID == keeperID {
		return
	}
	if path, exists := cfg.DeviceThemePaths[removedID]; exists {
		if strings.TrimSpace(cfg.DeviceThemePaths[keeperID]) == "" {
			cfg.DeviceThemePaths[keeperID] = path
		}
		delete(cfg.DeviceThemePaths, removedID)
	}
	if removed, exists := cfg.WaveshareTemplates[removedID]; exists {
		known := make(map[string]bool, len(cfg.WaveshareTemplates[keeperID]))
		for _, record := range cfg.WaveshareTemplates[keeperID] {
			known[record.ID] = true
		}
		for _, record := range removed {
			if !known[record.ID] {
				cfg.WaveshareTemplates[keeperID] = append(cfg.WaveshareTemplates[keeperID], record)
				known[record.ID] = true
			}
		}
		delete(cfg.WaveshareTemplates, removedID)
	}
	if playlist, exists := cfg.WavesharePlaylists[removedID]; exists {
		if _, keeperExists := cfg.WavesharePlaylists[keeperID]; !keeperExists {
			cfg.WavesharePlaylists[keeperID] = playlist
		}
		delete(cfg.WavesharePlaylists, removedID)
	}
}

func isRetiredDeviceID(deviceID string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(deviceID)), "thermalright")
}

func migrateGPUPreference(cfg *Config) bool {
	if cfg == nil || strings.TrimSpace(cfg.GPUPreference) != "" {
		return false
	}
	cfg.GPUPreference = string(gpupreference.SystemDefault)
	return true
}

func (store *Store) Path() string {
	return store.path
}

func (store *Store) Get() Config {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return clone(store.cfg)
}

func (store *Store) Set(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.save(cfg); err != nil {
		return err
	}
	store.cfg = clone(cfg)
	return nil
}

// Update 在同一把锁内读取、修改并保存配置，避免多个 API 端点各自执行
// Get/Set 时用旧快照覆盖彼此刚写入的字段。回调中不能再次调用此 Store。
func (store *Store) Update(update func(*Config) error) error {
	if update == nil {
		return errors.New("配置更新函数不能为空")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	next := clone(store.cfg)
	if err := update(&next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := store.save(next); err != nil {
		return err
	}
	store.cfg = clone(next)
	return nil
}

func (store *Store) save(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(store.path), ".config-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return err
	}
	if err := replaceFile(temporaryPath, store.path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func clone(cfg Config) Config {
	copied := cfg
	copied.Devices = append([]Device(nil), cfg.Devices...)
	if cfg.DeviceThemePaths != nil {
		copied.DeviceThemePaths = make(map[string]string, len(cfg.DeviceThemePaths))
		for id, path := range cfg.DeviceThemePaths {
			copied.DeviceThemePaths[id] = path
		}
	}
	if cfg.WaveshareTemplates != nil {
		copied.WaveshareTemplates = make(map[string][]WaveshareTemplate, len(cfg.WaveshareTemplates))
		for id, templates := range cfg.WaveshareTemplates {
			copied.WaveshareTemplates[id] = append([]WaveshareTemplate(nil), templates...)
		}
	}
	if cfg.WavesharePlaylists != nil {
		copied.WavesharePlaylists = make(map[string]WavesharePlaylist, len(cfg.WavesharePlaylists))
		for id, playlist := range cfg.WavesharePlaylists {
			playlist.Entries = append([]WavesharePlaylistEntry(nil), playlist.Entries...)
			copied.WavesharePlaylists[id] = playlist
		}
	}
	return copied
}
