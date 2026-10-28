package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"casescreen/internal/audio"
	"casescreen/internal/device"
	"casescreen/internal/lyrics"
	"casescreen/internal/render"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
	"casescreen/internal/video"
)

type Collector interface {
	Capture() (telemetry.Snapshot, error)
}

type gpuTargetCollector interface {
	SetGPUTarget(string) (telemetry.GPUTargetStatus, error)
	GPUTargetStatus() telemetry.GPUTargetStatus
}

type SpectrumProvider interface {
	Snapshot() audio.SpectrumFrame
}

type LyricsProvider interface {
	Snapshot() render.LyricState
}

type DeviceStatus struct {
	Info                device.Info         `json:"info"`
	Connected           bool                `json:"connected"`
	BrightnessSupported bool                `json:"brightness_supported"`
	DefaultBrightness   int                 `json:"default_brightness"`
	CurrentBrightness   int                 `json:"current_brightness"`
	PoweredOff          bool                `json:"powered_off"`
	LastError           string              `json:"last_error,omitempty"`
	Storage             *device.StorageInfo `json:"storage,omitempty"`
	LastConnectedAt     time.Time           `json:"last_connected_at,omitempty,omitzero"`
	LastVerifiedAt      time.Time           `json:"last_verified_at,omitempty,omitzero"`
	LastConnectionCheck time.Time           `json:"last_connection_check_at,omitempty,omitzero"`
	LastDisconnectedAt  time.Time           `json:"last_disconnected_at,omitempty,omitzero"`
	LastConnectionError string              `json:"last_connection_error,omitempty"`
	ReconnectAttempts   int                 `json:"reconnect_attempts,omitempty"`
	RetryAt             time.Time           `json:"retry_at,omitempty,omitzero"`
}

type AudioStatus struct {
	Enabled      bool      `json:"enabled"`
	Running      bool      `json:"running"`
	Endpoint     string    `json:"endpoint,omitempty"`
	EndpointID   string    `json:"endpoint_id,omitempty"`
	EndpointRole string    `json:"endpoint_role,omitempty"`
	CapturedAt   time.Time `json:"captured_at,omitempty"`
	Level        float64   `json:"level"`
	Peak         float64   `json:"peak"`
	Bands        []float64 `json:"bands"`
	LastError    string    `json:"last_error,omitempty"`
}

const audioStatusBandLimit = 64

// themeStateRefreshError 表示主题画面与运行时渲染器已经提交，仅后续
// 遥测/歌词/频谱初值补发失败。普通主题调用仍把它当错误并进入重试，
// 方向恢复则可据此结束“测试图仍显示”的瞬态会话，避免 UI 与实屏相反。
type themeStateRefreshError struct {
	cause error
}

func (failure *themeStateRefreshError) Error() string { return failure.cause.Error() }
func (failure *themeStateRefreshError) Unwrap() error { return failure.cause }

const maximumReconnectDelay = 30 * time.Second
const defaultHostFrameRetryDelay = 250 * time.Millisecond

type LyricsStatus struct {
	Enabled    bool              `json:"enabled"`
	Running    bool              `json:"running"`
	Track      lyrics.NowPlaying `json:"track"`
	Provider   string            `json:"provider,omitempty"`
	HasLyrics  bool              `json:"has_lyrics"`
	LyricMode  string            `json:"lyric_mode"`
	Scrollable bool              `json:"scrollable"`
	LastError  string            `json:"last_error,omitempty"`
}

type Status struct {
	StartedAt         time.Time          `json:"started_at"`
	TelemetryEnabled  bool               `json:"telemetry_enabled"`
	TelemetryInterval int64              `json:"telemetry_interval_ms"`
	LastSnapshot      telemetry.Snapshot `json:"last_snapshot"`
	Audio             AudioStatus        `json:"audio"`
	Lyrics            LyricsStatus       `json:"lyrics"`
	Devices           []DeviceStatus     `json:"devices"`
}

type lyricProgressSentState struct {
	legacy      int
	legacyKnown bool
	fine        int
	fineKnown   bool
}

// Service 只采集一次遥测，再分发给所有设备。
type Service struct {
	collector             Collector
	interval              time.Duration
	intervalChanged       chan time.Duration
	telemetryWake         chan struct{}
	renderInterval        time.Duration
	renderChanged         chan time.Duration
	renderWake            chan struct{}
	hostFrameRetryDelay   time.Duration
	smallPacketWake       chan struct{}
	reconnectEvery        time.Duration
	reconnectTimeout      time.Duration
	healthCheckEvery      time.Duration
	healthCheckTimeout    time.Duration
	storageRefreshEvery   time.Duration
	storageRefreshTimeout time.Duration
	storageRefreshMu      sync.Mutex
	jpegQuality           int
	ffmpegPath            string

	mu                      sync.RWMutex
	startedAt               time.Time
	telemetryEnabled        bool
	lastSnapshot            telemetry.Snapshot
	telemetryGeneration     uint64
	spectrum                SpectrumProvider
	audioEnabled            bool
	lyrics                  LyricsProvider
	lyricsEnabled           bool
	lyricsClearPending      bool
	renderer                *render.Renderer
	rendererError           error
	videoPlayer             *video.Player
	initialTheme            *theme.Manifest
	activeTheme             *theme.Manifest
	initialDeviceThemes     map[string]theme.Manifest
	pendingDeviceThemes     map[string]theme.Manifest
	deviceThemes            map[string]theme.Manifest
	deviceRenderers         map[string]*render.Renderer
	deviceVideoPlayers      map[string]*video.Player
	renderDirty             bool
	lastRenderHash          uint64
	drivers                 []device.Driver
	driversClosing          bool
	statuses                map[string]DeviceStatus
	hostFrames              map[string]bool
	hostFrameWorkers        map[string]*hostFrameWorker
	hostFrameGenerations    map[string]uint64
	hostFrameSuspended      map[string]bool
	hostFrameStats          map[string]HostFrameStats
	hostFrameWorkersClosed  bool
	themeApplyErrors        map[string]string
	spectrumThemeGeneration map[string]uint64
	// powerIntents 只保存用户明确点击过的关屏/恢复意图。
	// 连接失败不会删除该记录，重连后在同一设备门闩内重放。
	powerIntents map[string]bool
	// generation 用来合并“用户在主题传输中点击关屏”的竞态。
	// 主题收尾若已代为补发 0，等待门闩的用户操作不再重复发送。
	powerIntentGeneration  map[string]uint64
	powerAppliedGeneration map[string]uint64
	// initialPowerReplay 标记从配置中读取、且尚未在本次启动重放的意图。
	// 启动主题先下发，再统一恢复亮度，避免主题把已恢复的亮度覆盖。
	initialPowerReplay map[string]bool
	// lyricProgressSent 分别记录每台驻留主题屏最后确认发送的新旧进度值。
	// 细进度会先按实际轨道宽度量化；旧百分比仅在跨界时附带，既兼容
	// 已经写入设备的旧 Theme，也不会让每个 20Hz 小包都重复两个键。
	lyricProgressSent map[string]lyricProgressSentState
	// smallPacketDeviceGates 把同一设备的遥测、歌词事件和频谱小包严格
	// 串行化。发送方拿到门闩后才读取歌词快照，保证暂停、拖动或换句后，
	// 先前排队的周期包不能在新事件之后把旧进度写回屏幕。
	smallPacketDeviceGates map[string]chan struct{}

	orientationCalibrationMu      sync.Mutex
	orientationCalibrations       map[string]*orientationCalibrationSession
	orientationCalibrationWG      sync.WaitGroup
	orientationCalibrationClosing bool
	// orientationCalibrationTimeout 仅供显式启用自动恢复的测试或专用部署使用。
	// 正式工作台保持为 0：测试图必须一直显示到用户确认、取消、切换目标或退出。
	orientationCalibrationTimeout    time.Duration
	orientationRestoreAttemptTimeout time.Duration
	orientationRestoreRetryInitial   time.Duration
	orientationRestoreRetryMaximum   time.Duration
	orientationShutdownTimeout       time.Duration

	reconnectMu       sync.Mutex
	reconnectInFlight map[string]bool
	reconnectClosing  bool
	reconnectWG       sync.WaitGroup

	// 主题操作需要同时满足两个约束：同一设备严格串行，以及服务退出时
	// 先取消并等待正在进行的操作，再释放渲染器和 USB 句柄。
	themeOperationMu      sync.Mutex
	themeOperationContext context.Context
	themeOperationCancel  context.CancelFunc
	themeOperationClosing bool
	themeOperationWG      sync.WaitGroup
	themeDeviceGates      map[string]chan struct{}
}

func New(collector Collector, interval time.Duration) *Service {
	if interval <= 0 {
		interval = time.Second
	}
	return &Service{
		collector: collector, interval: interval, startedAt: time.Now(), telemetryEnabled: true,
		intervalChanged: make(chan time.Duration, 1), telemetryWake: make(chan struct{}, 1), renderInterval: 50 * time.Millisecond,
		renderChanged: make(chan time.Duration, 1), renderWake: make(chan struct{}, 1),
		hostFrameRetryDelay: defaultHostFrameRetryDelay,
		smallPacketWake:     make(chan struct{}, 1),
		// 容量在连接时读取，也可由用户手动刷新。默认不在歌词播放期间
		// 每 10 秒争用串口；测试或专用部署仍可显式设置自动刷新周期。
		reconnectEvery: 2 * time.Second, reconnectTimeout: 8 * time.Second,
		healthCheckEvery: 5 * time.Second, healthCheckTimeout: 1500 * time.Millisecond,
		storageRefreshEvery: 0,
		// 实机在音频频带和遥测同时发送时，3 号主题清单可能需要两秒以上才回应。
		// 五秒仍是有界查询，同时给串口互斥等待和一次固件回应留出余量。
		storageRefreshTimeout: 5 * time.Second, jpegQuality: 82,
		statuses: make(map[string]DeviceStatus), hostFrames: make(map[string]bool),
		hostFrameWorkers: make(map[string]*hostFrameWorker), hostFrameGenerations: make(map[string]uint64),
		hostFrameSuspended: make(map[string]bool), hostFrameStats: make(map[string]HostFrameStats),
		themeApplyErrors:        make(map[string]string),
		spectrumThemeGeneration: make(map[string]uint64),
		powerIntents:            make(map[string]bool),
		powerIntentGeneration:   make(map[string]uint64),
		powerAppliedGeneration:  make(map[string]uint64),
		initialPowerReplay:      make(map[string]bool),
		lyricProgressSent:       make(map[string]lyricProgressSentState),
		smallPacketDeviceGates:  make(map[string]chan struct{}),
		initialDeviceThemes:     make(map[string]theme.Manifest), pendingDeviceThemes: make(map[string]theme.Manifest),
		deviceThemes:    make(map[string]theme.Manifest),
		deviceRenderers: make(map[string]*render.Renderer), deviceVideoPlayers: make(map[string]*video.Player),
		reconnectInFlight:                make(map[string]bool),
		themeDeviceGates:                 make(map[string]chan struct{}),
		orientationRestoreAttemptTimeout: defaultOrientationRestoreAttemptTimeout,
		orientationRestoreRetryInitial:   defaultOrientationRestoreRetryInitial,
		orientationRestoreRetryMaximum:   defaultOrientationRestoreRetryMaximum,
		orientationShutdownTimeout:       defaultOrientationShutdownTimeout,
	}
}

// SetGPUTarget 只切换遥测采集源，不改变进程的 Windows GPU 运算首选项。
func (service *Service) SetGPUTarget(adapterID string) (telemetry.GPUTargetStatus, error) {
	collector, ok := service.collector.(gpuTargetCollector)
	if !ok {
		return telemetry.GPUTargetStatus{}, errors.New("当前遥测采集器不支持选择 GPU")
	}
	status, err := collector.SetGPUTarget(adapterID)
	if err == nil {
		// 先清掉上一块显卡的值，任何切换间隙都不会把旧温度或占用冒充为新卡。
		service.mu.Lock()
		service.telemetryGeneration++
		service.lastSnapshot.GPUModel = status.EffectiveName
		service.lastSnapshot.GPUPercent = nil
		service.lastSnapshot.GPUTemperature = nil
		service.lastSnapshot.GPUHotspotTemperature = nil
		service.lastSnapshot.GPUMemoryTemperature = nil
		service.renderDirty = true
		service.mu.Unlock()
		// setTarget 已为 PDH 建立第一帧基线；这里立即尝试第二帧，并由
		// telemetryWake 把结果发给小包设备。即使首包暂未就绪也不会阻塞设置。
		if captured, captureErr := service.collector.Capture(); captureErr == nil {
			service.mu.Lock()
			service.lastSnapshot = captured
			service.renderDirty = true
			service.mu.Unlock()
		}
		select {
		case service.telemetryWake <- struct{}{}:
		default:
		}
		service.wakeRenderer()
	}
	return status, err
}

func (service *Service) GPUTargetStatus() telemetry.GPUTargetStatus {
	collector, ok := service.collector.(gpuTargetCollector)
	if !ok {
		return telemetry.GPUTargetStatus{ConfiguredID: telemetry.AutomaticGPUAdapterID, Automatic: true}
	}
	return collector.GPUTargetStatus()
}

func (service *Service) AddDriver(driver device.Driver) {
	service.AddDriverIfAbsent(driver)
}

// AddDriverIfAbsent 为运行期热插拔发现注册新驱动。
// 设备 ID 是服务内的逻辑身份；同一 ID 不能因重复扫描加入两个驱动，
// 否则重连、主机帧和状态会同时操作同一块物理屏。
func (service *Service) AddDriverIfAbsent(driver device.Driver) bool {
	return service.addDriverIfAbsent(driver, nil)
}

// AddDriverWithInitialPower 把运行期新驱动和其持久化亮度意图原子注册。
// 若先 AddDriver 再 SetInitialPoweredOff，重连线程可能在两步之间完成，
// 导致新会话漏发关屏或恢复命令。
func (service *Service) AddDriverWithInitialPower(driver device.Driver, poweredOff bool) bool {
	return service.addDriverIfAbsent(driver, &poweredOff)
}

func (service *Service) addDriverIfAbsent(driver device.Driver, initialPoweredOff *bool) bool {
	if driver == nil || strings.TrimSpace(driver.Info().ID) == "" {
		return false
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.driversClosing {
		return false
	}
	for _, existing := range service.drivers {
		if existing.Info().ID == driver.Info().ID {
			return false
		}
	}
	service.drivers = append(service.drivers, driver)
	status := DeviceStatus{Info: driver.Info()}
	if brightness, ok := driver.(device.BrightnessDriver); ok {
		status.BrightnessSupported = true
		status.DefaultBrightness = brightness.DefaultBrightness()
		status.CurrentBrightness = status.DefaultBrightness
		if initialPoweredOff != nil {
			poweredOff := *initialPoweredOff
			status.PoweredOff = poweredOff
			if poweredOff {
				status.CurrentBrightness = 0
			}
			deviceID := driver.Info().ID
			service.powerIntents[deviceID] = poweredOff
			service.powerIntentGeneration[deviceID]++
			service.initialPowerReplay[deviceID] = true
		}
	}
	service.statuses[driver.Info().ID] = status
	return true
}

// ConnectionReset 描述一次端点变更后的断线原因。
type ConnectionReset struct {
	DeviceID string
	Reason   string
}

// ResetDeviceConnectionsAtomically 先锁住并关闭所有受影响的旧会话，
// 再调用 afterClose 原子提交新端点，最后同时开放重连。这样两个 COM
// 互换时，新的所有者绝不会在旧所有者尚未关闭时打开同一物理端点。
func (service *Service) ResetDeviceConnectionsAtomically(
	ctx context.Context, resets []ConnectionReset, afterClose func(),
) error {
	byID := make(map[string]ConnectionReset, len(resets))
	for _, reset := range resets {
		reset.DeviceID = strings.TrimSpace(reset.DeviceID)
		if reset.DeviceID == "" {
			return errors.New("设备 ID 不能为空")
		}
		byID[reset.DeviceID] = reset
	}
	if len(byID) == 0 {
		if afterClose != nil {
			afterClose()
		}
		return nil
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	service.mu.RLock()
	drivers := make(map[string]device.Driver, len(ids))
	for _, candidate := range service.drivers {
		if _, wanted := byID[candidate.Info().ID]; wanted {
			drivers[candidate.Info().ID] = candidate
		}
	}
	service.mu.RUnlock()
	for _, id := range ids {
		if drivers[id] == nil {
			return fmt.Errorf("未找到设备 %q", id)
		}
	}
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	unlocks := make([]func(), 0, len(ids))
	for _, id := range ids {
		unlockDevice, lockErr := service.lockDeviceTheme(operationContext, id)
		if lockErr != nil {
			for index := len(unlocks) - 1; index >= 0; index-- {
				unlocks[index]()
			}
			return lockErr
		}
		unlocks = append(unlocks, unlockDevice)
	}
	defer func() {
		for index := len(unlocks) - 1; index >= 0; index-- {
			unlocks[index]()
		}
	}()

	closeErrors := make([]error, 0)
	closeByID := make(map[string]error, len(ids))
	for _, id := range ids {
		if closeErr := drivers[id].Close(); closeErr != nil {
			closeByID[id] = closeErr
			closeErrors = append(closeErrors, fmt.Errorf("关闭设备 %s: %w", id, closeErr))
		}
	}
	if afterClose != nil {
		afterClose()
	}
	now := time.Now()
	service.mu.Lock()
	for _, id := range ids {
		reason := strings.TrimSpace(byID[id].Reason)
		if reason == "" {
			reason = "设备端点已更新，正在重新连接"
		}
		if closeErr := closeByID[id]; closeErr != nil {
			reason += "；关闭旧会话失败: " + closeErr.Error()
		}
		status := service.statuses[id]
		status.Info = drivers[id].Info()
		status.Connected = false
		status.LastError = reason
		status.LastConnectionError = reason
		status.LastDisconnectedAt = now
		status.ReconnectAttempts = 0
		status.RetryAt = time.Time{}
		service.statuses[id] = status
	}
	service.mu.Unlock()
	for _, id := range ids {
		if _, ok := drivers[id].(device.HostFrameDriver); ok {
			service.invalidateHostFrameDelivery(id)
		}
	}
	return errors.Join(closeErrors...)

}

// ResetDeviceConnection 用于运行期发现到单个物理端点变化后废弃旧会话。
func (service *Service) ResetDeviceConnection(ctx context.Context, deviceID, reason string) error {
	return service.ResetDeviceConnectionsAtomically(ctx, []ConnectionReset{{DeviceID: deviceID, Reason: reason}}, nil)
}

// SetInitialPoweredOff 把配置中的显式意图注入服务。false 也是已知意图，
// 因此进程崩溃或完全退出后重启，能把硬件与 UI 恢复到同一状态。
func (service *Service) SetInitialPoweredOff(deviceID string, poweredOff bool) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return errors.New("设备 ID 不能为空")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	var brightness device.BrightnessDriver
	for _, driver := range service.drivers {
		if driver.Info().ID == deviceID {
			brightness, _ = driver.(device.BrightnessDriver)
			break
		}
	}
	if brightness == nil {
		return fmt.Errorf("设备 %q 不支持亮度关屏", deviceID)
	}
	value := brightness.DefaultBrightness()
	if poweredOff {
		value = 0
	}
	service.powerIntents[deviceID] = poweredOff
	service.powerIntentGeneration[deviceID]++
	service.initialPowerReplay[deviceID] = true
	status := service.statuses[deviceID]
	status.BrightnessSupported = true
	status.DefaultBrightness = brightness.DefaultBrightness()
	status.CurrentBrightness = value
	status.PoweredOff = poweredOff
	service.statuses[deviceID] = status
	return nil
}

func (service *Service) Run(ctx context.Context) error {
	if service.collector == nil {
		return errors.New("遥测采集器未配置")
	}
	service.startThemeOperations(ctx)
	service.startHostFrameWorkers()
	service.startReconnectAttempts()
	service.connectAll(ctx)
	service.applyInitialTheme(ctx)
	service.applyInitialPower(ctx)
	ticker := time.NewTicker(service.interval)
	defer ticker.Stop()
	renderTicker := time.NewTicker(service.renderInterval)
	defer renderTicker.Stop()
	reconnectTicker := time.NewTicker(service.reconnectEvery)
	defer reconnectTicker.Stop()
	defer service.closeAll()
	defer service.closeRenderer()
	defer service.closeVideoPlayer()
	defer service.stopAllHostFrameWorkers()
	// 重连线程会在更晚注册的 defer 中先停止；主题操作随后全部退出，
	// 最后才关闭帧线程、渲染器和驱动，避免退出时重新装入资源。
	// 主题门禁关闭且已等待普通操作后，对仍显示方向测试图的屏做一次
	// 有界恢复；此 defer 的注册顺序保证恢复早于帧线程和 USB 驱动关闭。
	defer service.restoreOrientationCalibrationsBeforeShutdown()
	defer service.stopThemeOperations()
	reconnectContext, stopReconnect := context.WithCancel(ctx)
	defer func() {
		stopReconnect()
		service.stopReconnectAttempts()
	}()
	spectrumContext, stopSpectrum := context.WithCancel(ctx)
	spectrumDone := make(chan struct{})
	go func() {
		defer close(spectrumDone)
		service.runWaveshareSpectrum(spectrumContext)
	}()
	defer func() {
		stopSpectrum()
		<-spectrumDone
	}()
	storageContext, stopStorage := context.WithCancel(ctx)
	storageDone := make(chan struct{})
	go func() {
		defer close(storageDone)
		service.runStorageRefresh(storageContext)
	}()
	defer func() {
		stopStorage()
		<-storageDone
	}()
	// 歌词截止时间使用独立事件循环。这样每秒硬件采集即使暂时变慢，
	// 也不会阻塞仅含 Current/Next 的换行包。
	lyricsContext, stopLyrics := context.WithCancel(ctx)
	lyricsDone := make(chan struct{})
	go func() {
		defer close(lyricsDone)
		service.runLyricEvents(lyricsContext)
	}()
	defer func() {
		stopLyrics()
		<-lyricsDone
	}()
	// 健康线程可能正持有驱动锁。它的 defer 最后注册，因此退出时
	// 会先取消并等待完成，再停止帧线程和关闭 USB 句柄。
	healthContext, stopHealth := context.WithCancel(ctx)
	healthDone := make(chan struct{})
	go func() {
		defer close(healthDone)
		service.runConnectionHealthChecks(healthContext)
	}()
	defer func() {
		stopHealth()
		<-healthDone
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case interval := <-service.intervalChanged:
			ticker.Reset(interval)
		case <-service.telemetryWake:
			service.tick(ctx)
		case interval := <-service.renderChanged:
			renderTicker.Reset(interval)
		case <-ticker.C:
			service.tick(ctx)
		case <-renderTicker.C:
			service.renderHostFrames(ctx)
		case <-reconnectTicker.C:
			service.reconnectDisconnectedAsync(reconnectContext)
			service.replayPendingPowerAsync(reconnectContext)
		case <-service.renderWake:
			service.renderHostFrames(ctx)
		}
	}
}

func (service *Service) SetTelemetryEnabled(enabled bool) {
	service.mu.Lock()
	service.telemetryEnabled = enabled
	service.renderDirty = true
	service.mu.Unlock()
	service.wakeRenderer()
}

func (service *Service) SetInterval(interval time.Duration) {
	if interval <= 0 {
		return
	}
	service.mu.Lock()
	service.interval = interval
	service.mu.Unlock()
	select {
	case service.intervalChanged <- interval:
	default:
		select {
		case <-service.intervalChanged:
		default:
		}
		select {
		case service.intervalChanged <- interval:
		default:
		}
	}
}

func (service *Service) SetRenderInterval(interval time.Duration) {
	if interval < 20*time.Millisecond || interval > time.Second {
		return
	}
	service.mu.Lock()
	service.renderInterval = interval
	service.mu.Unlock()
	replaceDuration(service.renderChanged, interval)
}

func (service *Service) SetJPEGQuality(quality int) {
	if quality < 35 || quality > 95 {
		return
	}
	service.mu.Lock()
	service.jpegQuality = quality
	service.mu.Unlock()
}

func (service *Service) SetFFmpegPath(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		if executable, err := os.Executable(); err == nil {
			path = filepath.Join(filepath.Dir(executable), "ffmpeg.exe")
		}
	}
	service.mu.Lock()
	service.ffmpegPath = path
	service.mu.Unlock()
}

func (service *Service) SetSpectrumProvider(provider SpectrumProvider) {
	service.mu.Lock()
	service.spectrum = provider
	service.audioEnabled = provider != nil
	service.mu.Unlock()
}

func (service *Service) SetLyricsProvider(provider LyricsProvider) {
	service.mu.Lock()
	hadProvider := service.lyrics != nil
	service.lyrics = provider
	service.lyricsEnabled = provider != nil
	if provider != nil {
		service.lyricsClearPending = false
	} else if hadProvider {
		// 连续移除同一提供方时不能撤销尚未 ACK 的清空包。
		service.lyricsClearPending = true
	}
	service.mu.Unlock()
	service.wakeSmallPacket()
}

// SetLyricsProviderPreference 将在线歌词来源设置转发给可配置的歌词引擎。
func (service *Service) SetLyricsProviderPreference(providerID string) {
	service.mu.RLock()
	provider := service.lyrics
	service.mu.RUnlock()
	if configurable, ok := provider.(interface{ SetPreferredProvider(string) }); ok {
		configurable.SetPreferredProvider(providerID)
	}
}

// SetLyricsPath 将本地 LRC 路径转发给歌词引擎。
func (service *Service) SetLyricsPath(path string) {
	service.mu.RLock()
	provider := service.lyrics
	service.mu.RUnlock()
	if configurable, ok := provider.(interface{ SetLocalPath(string) }); ok {
		configurable.SetLocalPath(path)
	}
}

func (service *Service) SetAudioEnabled(enabled bool) {
	service.mu.Lock()
	service.audioEnabled = enabled && service.spectrum != nil
	provider := service.spectrum
	service.renderDirty = true
	service.mu.Unlock()
	if configurable, ok := provider.(interface{ SetEnabled(bool) }); ok {
		configurable.SetEnabled(enabled)
	}
	service.wakeRenderer()
}

func (service *Service) SetLyricsEnabled(enabled bool) {
	service.mu.Lock()
	wasEnabled := service.lyricsEnabled
	service.lyricsEnabled = enabled && service.lyrics != nil
	if service.lyricsEnabled {
		service.lyricsClearPending = false
	} else if wasEnabled {
		// 只有成功下发空文本后 tick 才会复位；重复点击关闭不能丢掉任务。
		service.lyricsClearPending = true
	}
	provider := service.lyrics
	service.renderDirty = true
	service.mu.Unlock()
	if configurable, ok := provider.(interface{ SetEnabled(bool) }); ok {
		configurable.SetEnabled(enabled)
	}
	service.wakeRenderer()
	service.wakeSmallPacket()
}

func (service *Service) SetInitialTheme(manifest theme.Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	service.mu.Lock()
	copied := manifest
	copied.Resources = append([]theme.Resource(nil), manifest.Resources...)
	copied.Layers = append([]theme.Layer(nil), manifest.Layers...)
	service.initialTheme = &copied
	service.mu.Unlock()
	return nil
}

func (service *Service) SetInitialDeviceTheme(deviceID string, manifest theme.Manifest) error {
	if strings.TrimSpace(deviceID) == "" {
		return errors.New("设备 ID 不能为空")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	service.mu.Lock()
	service.initialDeviceThemes[deviceID] = copyManifest(manifest)
	service.mu.Unlock()
	return nil
}

func copyManifest(manifest theme.Manifest) theme.Manifest {
	manifest.Resources = append([]theme.Resource(nil), manifest.Resources...)
	manifest.Layers = append([]theme.Layer(nil), manifest.Layers...)
	return manifest
}

var errServiceClosing = errors.New("CaseScreen 服务正在退出")

func (service *Service) startThemeOperations(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	operationContext, cancel := context.WithCancel(ctx)
	service.themeOperationMu.Lock()
	if service.themeOperationCancel != nil {
		service.themeOperationCancel()
	}
	service.themeOperationContext = operationContext
	service.themeOperationCancel = cancel
	service.themeOperationClosing = false
	if service.themeDeviceGates == nil {
		service.themeDeviceGates = make(map[string]chan struct{})
	}
	service.themeOperationMu.Unlock()
}

func (service *Service) stopThemeOperations() {
	service.themeOperationMu.Lock()
	service.themeOperationClosing = true
	cancel := service.themeOperationCancel
	service.themeOperationMu.Unlock()
	if cancel != nil {
		cancel()
	}
	service.themeOperationWG.Wait()
}

// beginThemeOperation 把调用方的截止时间与 Service.Run 生命周期合并。
// 没有启动 Run 的单元测试仍可直接调用；一旦退出门禁关闭，后续调用立即失败。
func (service *Service) beginThemeOperation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	service.themeOperationMu.Lock()
	if service.themeOperationClosing {
		service.themeOperationMu.Unlock()
		return nil, nil, errServiceClosing
	}
	root := service.themeOperationContext
	service.themeOperationWG.Add(1)
	service.themeOperationMu.Unlock()

	operationContext, cancel := context.WithCancel(ctx)
	stopRootCancellation := func() bool { return false }
	if root != nil {
		stopRootCancellation = context.AfterFunc(root, cancel)
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			stopRootCancellation()
			cancel()
			service.themeOperationWG.Done()
		})
	}
	return operationContext, finish, nil
}

func (service *Service) lockDeviceTheme(ctx context.Context, deviceID string) (func(), error) {
	service.themeOperationMu.Lock()
	gate := service.themeDeviceGates[deviceID]
	if gate == nil {
		gate = make(chan struct{}, 1)
		service.themeDeviceGates[deviceID] = gate
	}
	service.themeOperationMu.Unlock()
	select {
	case gate <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-gate }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SetPoweredOff 把亮度 0 作为关屏意图，恢复时使用驱动明确给出的默认亮度。
// target 可以是单台设备 ID 或 all。所有结果都按设备返回，因此全部关屏时
// 一台失败不会遮蔽其它屏的成功结果。
func (service *Service) SetPoweredOff(ctx context.Context, target string, poweredOff bool) map[string]error {
	results := make(map[string]error)
	target = strings.TrimSpace(target)
	if target == "" {
		results["service"] = errors.New("请选择目标副屏")
		return results
	}

	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	selected := make([]device.Driver, 0, len(drivers))
	for _, driver := range drivers {
		if target == "all" || driver.Info().ID == target {
			selected = append(selected, driver)
		}
	}
	if len(selected) == 0 {
		results[target] = fmt.Errorf("未找到设备 %q", target)
		return results
	}

	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		for _, driver := range selected {
			results[driver.Info().ID] = err
		}
		return results
	}
	defer finishOperation()
	type brightnessTarget struct {
		id         string
		driver     device.BrightnessDriver
		generation uint64
	}
	targets := make([]brightnessTarget, 0, len(selected))
	for _, driver := range selected {
		id := driver.Info().ID
		brightness, supported := driver.(device.BrightnessDriver)
		if !supported {
			if target != "all" {
				results[id] = device.ErrBrightnessUnsupported
			}
			continue
		}
		targets = append(targets, brightnessTarget{id: id, driver: brightness})
	}
	// 先一次性登记所有受支持设备，不执行任何 USB 等待。
	// 即使某台随后卡住，其它屏的意图也已可在重连时重放。
	for index := range targets {
		targets[index].generation = service.rememberPowerIntent(targets[index].id, targets[index].driver, poweredOff, false)
	}
	for _, target := range targets {
		service.suspendHostFrameDelivery(target.id)
	}

	var resultsMu sync.Mutex
	var wait sync.WaitGroup
	for _, target := range targets {
		target := target
		wait.Add(1)
		go func() {
			defer wait.Done()
			deviceContext, cancel := context.WithTimeout(operationContext, service.powerOperationTimeout())
			defer cancel()
			unlockDevice, lockErr := service.lockDeviceTheme(deviceContext, target.id)
			result := lockErr
			acquired := result == nil
			if result == nil {
				result = service.sendRememberedPowerLocked(deviceContext, target.id, target.driver, target.generation, false)
				unlockDevice()
			}
			if acquired {
				if latestPoweredOff, _ := service.powerIntent(target.id); !latestPoweredOff {
					service.resumeHostFrameDelivery(target.id)
				}
			}
			resultsMu.Lock()
			results[target.id] = result
			resultsMu.Unlock()
		}()
	}
	wait.Wait()
	return results
}

func (service *Service) rememberPowerIntent(deviceID string, driver device.BrightnessDriver, poweredOff, initial bool) uint64 {
	brightness := driver.DefaultBrightness()
	if poweredOff {
		brightness = 0
	}
	service.mu.Lock()
	service.powerIntents[deviceID] = poweredOff
	service.powerIntentGeneration[deviceID]++
	generation := service.powerIntentGeneration[deviceID]
	if initial {
		service.initialPowerReplay[deviceID] = true
	} else {
		delete(service.initialPowerReplay, deviceID)
	}
	status := service.statuses[deviceID]
	status.BrightnessSupported = true
	status.DefaultBrightness = driver.DefaultBrightness()
	status.CurrentBrightness = brightness
	status.PoweredOff = poweredOff
	service.statuses[deviceID] = status
	service.mu.Unlock()
	return generation
}

// sendRememberedPowerLocked 要求调用者已持有该设备门闩。
func (service *Service) sendRememberedPowerLocked(ctx context.Context, deviceID string, driver device.BrightnessDriver, generation uint64, force bool) error {
	for {
		service.mu.RLock()
		poweredOff, known := service.powerIntents[deviceID]
		currentGeneration := service.powerIntentGeneration[deviceID]
		appliedGeneration := service.powerAppliedGeneration[deviceID]
		service.mu.RUnlock()
		if !known || currentGeneration == 0 {
			return nil
		}
		// 门闩持有者总是吸收等待期间的最新意图；尤其是主题收尾的
		// force 调用不能按旧代次确认一个刚读取到的新值，否则随后等待
		// 门闩的用户操作还会重复发送同一条亮度命令。
		if generation == 0 || generation != currentGeneration || force {
			generation = currentGeneration
		}
		if !force && appliedGeneration >= generation {
			return nil
		}
		sentPoweredOff := poweredOff
		sentGeneration := generation
		brightness := driver.DefaultBrightness()
		if sentPoweredOff {
			brightness = 0
		}
		err := driver.SetBrightness(ctx, brightness)
		service.setTransferError(deviceID, err)
		if err != nil {
			return err
		}
		service.mu.Lock()
		latestPoweredOff := service.powerIntents[deviceID]
		latestGeneration := service.powerIntentGeneration[deviceID]
		acknowledgedGeneration := sentGeneration
		// 发送期间若又登记了相同值，刚完成的硬件命令已经满足这些
		// 意图，可一起确认；若值已反转，只确认实际发送的代次并在
		// 下轮补发新值，绝不能吞掉恢复/关屏请求。
		if latestPoweredOff == sentPoweredOff && latestGeneration > acknowledgedGeneration {
			acknowledgedGeneration = latestGeneration
		}
		if service.powerAppliedGeneration[deviceID] < acknowledgedGeneration {
			service.powerAppliedGeneration[deviceID] = acknowledgedGeneration
		}
		service.mu.Unlock()
		if latestGeneration <= acknowledgedGeneration {
			return nil
		}
		generation = latestGeneration
		force = false
	}
}

func (service *Service) powerOperationTimeout() time.Duration {
	if timeout := service.reconnectAttemptTimeout(); timeout > 0 {
		return timeout
	}
	return 8 * time.Second
}

func (service *Service) powerIntent(deviceID string) (poweredOff bool, known bool) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	poweredOff, known = service.powerIntents[deviceID]
	return poweredOff, known
}

func (service *Service) powerIntentApplied(deviceID string) bool {
	service.mu.RLock()
	defer service.mu.RUnlock()
	_, known := service.powerIntents[deviceID]
	return known && service.powerAppliedGeneration[deviceID] >= service.powerIntentGeneration[deviceID]
}

func (service *Service) markPowerIntentUnapplied(deviceID string) {
	service.mu.Lock()
	if _, known := service.powerIntents[deviceID]; known {
		// 新 USB 会话或固件重置后，旧会话的 applied 不能证明当前
		// 硬件仍保持该亮度；归零后由同一门闩内的主题/重连路径重放。
		service.powerAppliedGeneration[deviceID] = 0
	}
	service.mu.Unlock()
}

// applyPendingPowerIntentLocked 只发送尚未在当前会话确认的最新意图。
// 调用者必须持有设备门闩，避免与主题、重连或用户命令交叉。
func (service *Service) applyPendingPowerIntentLocked(ctx context.Context, deviceID string, driver device.Driver) error {
	_, known := service.powerIntent(deviceID)
	if !known || service.powerIntentApplied(deviceID) {
		return nil
	}
	brightness, supported := driver.(device.BrightnessDriver)
	if !supported {
		return device.ErrBrightnessUnsupported
	}
	poweredOff, _ := service.powerIntent(deviceID)
	if poweredOff {
		service.suspendHostFrameDelivery(deviceID)
	}
	service.mu.RLock()
	generation := service.powerIntentGeneration[deviceID]
	service.mu.RUnlock()
	err := service.sendRememberedPowerLocked(ctx, deviceID, brightness, generation, false)
	if latestPoweredOff, _ := service.powerIntent(deviceID); !latestPoweredOff {
		service.resumeHostFrameDelivery(deviceID)
	}
	return err
}

// reapplyPowerIntentLocked 用于重连。与主题和 Connect 共用单设备门闩，
// 保证不会与用户刚点击的关屏/恢复交叉。
func (service *Service) reapplyPowerIntentLocked(ctx context.Context, deviceID string, driver device.Driver) error {
	poweredOff, known := service.powerIntent(deviceID)
	if !known {
		return nil
	}
	brightness, supported := driver.(device.BrightnessDriver)
	if !supported {
		return device.ErrBrightnessUnsupported
	}
	if poweredOff {
		service.suspendHostFrameDelivery(deviceID)
	}
	service.mu.RLock()
	generation := service.powerIntentGeneration[deviceID]
	service.mu.RUnlock()
	err := service.sendRememberedPowerLocked(ctx, deviceID, brightness, generation, true)
	if err == nil {
		service.mu.Lock()
		delete(service.initialPowerReplay, deviceID)
		service.mu.Unlock()
	}
	if latestPoweredOff, _ := service.powerIntent(deviceID); !latestPoweredOff {
		service.resumeHostFrameDelivery(deviceID)
	}
	return err
}

// reapplyPowerAfterThemeLocked 在主题成功后重放最新的开/关屏意图。
// 这同时覆盖“关屏”和“恢复”在长主题传输期间等待门闩的竞态。
func (service *Service) reapplyPowerAfterThemeLocked(ctx context.Context, deviceID string, driver device.Driver) error {
	service.mu.RLock()
	deferredUntilStartupReplay := service.initialPowerReplay[deviceID]
	service.mu.RUnlock()
	if deferredUntilStartupReplay {
		return nil
	}
	poweredOff, known := service.powerIntent(deviceID)
	if !known {
		return nil
	}
	brightness, supported := driver.(device.BrightnessDriver)
	if !supported {
		return device.ErrBrightnessUnsupported
	}
	service.mu.RLock()
	generation := service.powerIntentGeneration[deviceID]
	service.mu.RUnlock()
	// 关屏时必须强制补发亮度 0，因为主题切换可能短暂唤醒屏幕。
	// 正常亮屏且当前代次已在本次 USB 会话确认时，不再发送一条
	// 完全相同的亮度命令；部分设备会在主题已提交后忽略这条冗余
	// 命令，若把它的超时算作主题失败就会造成错误的“保存失败”。
	// 若主题传输期间登记了新意图，appliedGeneration 仍落后，非强制
	// 路径也会精确补发最新值。
	return service.sendRememberedPowerLocked(ctx, deviceID, brightness, generation, poweredOff)
}

func (service *Service) ApplyThemeTo(ctx context.Context, deviceID string, manifest theme.Manifest) map[string]error {
	results := make(map[string]error)
	if err := manifest.Validate(); err != nil {
		results[deviceID] = err
		return results
	}
	service.mu.RLock()
	var driver device.Driver
	for _, candidate := range service.drivers {
		if candidate.Info().ID == deviceID {
			driver = candidate
			break
		}
	}
	service.mu.RUnlock()
	if driver == nil {
		results[deviceID] = fmt.Errorf("未找到设备 %q", deviceID)
		return results
	}
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		results[deviceID] = err
		return results
	}
	defer finishOperation()
	unlockDevice, err := service.lockDeviceTheme(operationContext, deviceID)
	if err != nil {
		results[deviceID] = err
		return results
	}
	defer unlockDevice()
	if service.hasOrientationCalibration(deviceID) {
		results[deviceID] = errors.New("方向测试图正在显示；请先确认方向或取消测试")
		return results
	}
	results = service.applyThemeToLocked(operationContext, deviceID, driver, manifest)
	return results
}

// applyThemeToLocked 要求调用者已经持有目标设备的主题门闩。
func (service *Service) applyThemeToLocked(ctx context.Context, deviceID string, driver device.Driver, manifest theme.Manifest) map[string]error {
	results := make(map[string]error)
	active := usesHostFrames(driver, manifest)
	var nextRenderer *render.Renderer
	var nextVideoPlayer *video.Player
	if active {
		var err error
		nextRenderer, err = render.New(manifest)
		if err == nil {
			sourcePath, isVideo, videoErr := video.FindSource(manifest)
			if videoErr == nil && isVideo {
				service.mu.RLock()
				ffmpegPath := service.ffmpegPath
				fps := max(1, int(time.Second/service.renderInterval))
				service.mu.RUnlock()
				nextVideoPlayer, videoErr = video.Open(ffmpegPath, sourcePath, manifest.Width, manifest.Height, min(fps, 30))
			}
			if videoErr != nil {
				_ = nextRenderer.Close()
				nextRenderer = nil
				err = videoErr
			}
		}
		if err != nil {
			service.rememberPendingTheme(deviceID, manifest)
			service.setDriverError(driver, err)
			results[deviceID] = err
			return results
		}
	}
	// 切换设备模式前先取消并等待该屏的旧帧 worker。
	// 否则旧 JPEG 可能在新主题提交后才到达，又把混合设备切回实时模式。
	service.suspendHostFrameDelivery(deviceID)
	transitionCommitted := false
	defer func() {
		if !transitionCommitted {
			service.resumeHostFrameDelivery(deviceID)
		}
	}()
	if err := applyDriverTheme(ctx, driver, manifest, active); err != nil {
		if nextRenderer != nil {
			_ = nextRenderer.Close()
		}
		if nextVideoPlayer != nil {
			_ = nextVideoPlayer.Close()
		}
		service.rememberPendingTheme(deviceID, manifest)
		service.setDriverError(driver, err)
		results[deviceID] = err
		return results
	}
	if err := service.reapplyPowerAfterThemeLocked(ctx, deviceID, driver); err != nil {
		if nextRenderer != nil {
			_ = nextRenderer.Close()
		}
		if nextVideoPlayer != nil {
			_ = nextVideoPlayer.Close()
		}
		service.rememberPendingTheme(deviceID, manifest)
		results[deviceID] = fmt.Errorf("主题已下发，但恢复亮度状态失败: %w", err)
		return results
	}
	service.mu.Lock()
	previousRenderer := service.deviceRenderers[deviceID]
	previousVideo := service.deviceVideoPlayers[deviceID]
	if active {
		service.deviceRenderers[deviceID] = nextRenderer
		if nextVideoPlayer != nil {
			service.deviceVideoPlayers[deviceID] = nextVideoPlayer
		} else {
			delete(service.deviceVideoPlayers, deviceID)
		}
	} else {
		delete(service.deviceRenderers, deviceID)
		delete(service.deviceVideoPlayers, deviceID)
	}
	service.deviceThemes[deviceID] = copyManifest(manifest)
	service.hostFrames[deviceID] = active
	// 方向恢复会在校准会话仍然活跃时提交主机渲染器。
	// 会话结束前必须继续隔离普通 JPEG，由恢复路径在 finish 后显式 resume。
	if status := service.statuses[deviceID]; status.PoweredOff || service.orientationCalibrationActiveLocked(deviceID) {
		service.hostFrameSuspended[deviceID] = true
	} else {
		delete(service.hostFrameSuspended, deviceID)
	}
	service.spectrumThemeGeneration[deviceID]++
	delete(service.pendingDeviceThemes, deviceID)
	service.renderDirty = true
	service.mu.Unlock()
	transitionCommitted = true
	if previousRenderer != nil {
		_ = previousRenderer.Close()
	}
	if previousVideo != nil {
		_ = previousVideo.Close()
	}
	if active {
		service.wakeRenderer()
	}
	service.setDriverError(driver, nil)
	// 主题重载会让屏端动态控件和驱动 ACK 缓存同时回到初始状态。
	// 成功提交后立即补发当前小包，避免遥测周期设为 60 秒时新主题长期留白。
	if refreshErr := service.refreshDeviceStateAfterTheme(ctx, driver, manifest); refreshErr != nil {
		service.rememberPendingTheme(deviceID, manifest)
		results[deviceID] = &themeStateRefreshError{cause: refreshErr}
		return results
	}
	results[deviceID] = nil
	return results
}

// refreshDeviceStateAfterTheme 只刷新刚刚成功应用主题的设备。
// 遥测复用最近一次实测值；若服务尚未采集过才现场采集一次。歌词与频谱
// 始终读取当前提供方，过期频谱明确清零，绝不把重载前的旧帧补回屏幕。
func (service *Service) refreshDeviceStateAfterTheme(ctx context.Context, driver device.Driver, manifest theme.Manifest) error {
	unlockSmallPacket, err := service.lockSmallPacketDevice(ctx, driver.Info().ID)
	if err != nil {
		return err
	}
	defer unlockSmallPacket()
	if reporter, ok := driver.(device.ConnectionStateDriver); ok && !reporter.Connected() {
		return errors.New("主题已写入，但设备在补发当前状态前断开")
	}
	hasMetrics := manifestNeedsSmallTelemetry(manifest)
	hasAudio := manifestHasLayer(manifest, theme.LayerAudioBars) || manifestHasLayer(manifest, theme.LayerAudioRing)
	hasLyrics := manifestHasLayer(manifest, theme.LayerLyrics)
	_, dedicatedAudio := driver.(waveshareSpectrumSender)

	service.mu.RLock()
	telemetryEnabled := service.telemetryEnabled
	lastSnapshot := service.lastSnapshot
	spectrumProvider := service.spectrum
	audioEnabled := service.audioEnabled
	lyricsProvider := service.lyrics
	lyricsEnabled := service.lyricsEnabled
	service.mu.RUnlock()

	sendMetrics := driver.Info().Capabilities.SmallTelemetry && telemetryEnabled && hasMetrics
	sendAudio := driver.Info().Capabilities.SmallTelemetry && hasAudio && audioEnabled && !dedicatedAudio
	// 即使歌词已关闭且旧清空包已经 ACK，新 Theme 的驻留文字控件仍会
	// 恢复默认占位；因此每次重载都必须显式补发当前歌词或清空。
	sendLyrics := driver.Info().Capabilities.SmallTelemetry && hasLyrics
	if sendMetrics || sendAudio || sendLyrics {
		snapshot := lastSnapshot
		captureOK := !snapshot.CapturedAt.IsZero()
		if sendMetrics && !captureOK && service.collector != nil {
			if captured, err := service.collector.Capture(); err == nil {
				snapshot = captured
				captureOK = true
				service.mu.Lock()
				service.lastSnapshot = captured
				service.mu.Unlock()
			}
		}
		// 时钟必须反映重载完成时刻，硬件值仍保留最近一次真实采样。
		snapshot.CapturedAt = time.Now()
		snapshot.OmitHardwareMetrics = !sendMetrics || !captureOK
		snapshot.OmitClock = !sendMetrics
		if sendAudio {
			if spectrum, fresh := freshSpectrumSnapshot(spectrumProvider, audioEnabled, snapshot.CapturedAt); fresh {
				snapshot.AudioSpectrum = encodeSmallSpectrum(spectrum.Bars)
			} else {
				// 旧帧不能在主题重载后重新出现；零包会清除驻留控件。
				snapshot.AudioSpectrum = encodeSmallSpectrum(nil)
			}
		}
		if sendLyrics {
			var lyricState render.LyricState
			var lyricStatus lyrics.EngineStatus
			lyricTimed := false
			if !lyricsEnabled {
				snapshot.LyricsClear = true
			} else if lyricsProvider != nil && lyricsEnabled {
				lyricState, lyricStatus, lyricTimed = snapshotLyricsProvider(lyricsProvider)
				populateLyricPacket(&snapshot, lyricState, lyricStatus, lyricTimed)
			}
			if driver.Info().Capabilities.DeliveryMode != device.DeliveryStoredTheme ||
				!applyLyricProgressForManifest(&snapshot, manifest, lyricState, lyricTimed) {
				snapshot.LyricsProgressKnown = false
				snapshot.LyricsProgressFineKnown = false
			}
		}
		if err := driver.SendTelemetry(ctx, snapshot); err != nil {
			// 息屏期间跳过的动态包没有进入硬件，不能记为歌词已清空。
			// 主题本身已提交成功，恢复后再由唤醒事件补发当前状态。
			if errors.Is(err, device.ErrRealtimeFrameDropped) {
				return nil
			}
			service.setTransferError(driver.Info().ID, err)
			return err
		}
		if sendLyrics {
			service.rememberLyricProgress(driver.Info().ID, snapshot, nil)
		}
		service.setTransferError(driver.Info().ID, nil)
	}

	if dedicatedAudio && hasAudio {
		if err := service.sendCurrentWaveshareSpectrumAfterTheme(ctx, driver, manifest); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) ApplyTheme(ctx context.Context, manifest theme.Manifest) map[string]error {
	results := make(map[string]error)
	if err := manifest.Validate(); err != nil {
		results["service"] = err
		return results
	}
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		results["service"] = err
		return results
	}
	defer finishOperation()
	ctx = operationContext
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	hostFrames := make(map[string]bool, len(drivers))
	hasHostDriver := false
	for _, driver := range drivers {
		active := usesHostFrames(driver, manifest)
		hostFrames[driver.Info().ID] = active
		if active {
			hasHostDriver = true
		}
	}
	var nextRenderer *render.Renderer
	var nextVideoPlayer *video.Player
	var rendererError error
	if hasHostDriver {
		var err error
		nextRenderer, err = render.New(manifest)
		if err != nil {
			rendererError = err
			for _, driver := range drivers {
				if hostFrames[driver.Info().ID] {
					results[driver.Info().ID] = err
					service.setDriverError(driver, err)
				}
			}
		}
		if nextRenderer != nil {
			sourcePath, isVideo, videoErr := video.FindSource(manifest)
			if videoErr == nil && isVideo {
				service.mu.RLock()
				ffmpegPath := service.ffmpegPath
				fps := max(1, int(time.Second/service.renderInterval))
				service.mu.RUnlock()
				nextVideoPlayer, videoErr = video.Open(ffmpegPath, sourcePath, manifest.Width, manifest.Height, min(fps, 30))
			}
			if videoErr != nil {
				rendererError = videoErr
				_ = nextRenderer.Close()
				nextRenderer = nil
				for _, driver := range drivers {
					if hostFrames[driver.Info().ID] {
						results[driver.Info().ID] = videoErr
						service.setDriverError(driver, videoErr)
					}
				}
			}
		}
	}
	for _, driver := range drivers {
		id := driver.Info().ID
		if hostFrames[id] && nextRenderer == nil {
			service.rememberPendingTheme(id, manifest)
			continue
		}
		results[id] = service.ApplyThemeTo(ctx, id, manifest)[id]
	}
	commit := true
	for _, driver := range drivers {
		if results[driver.Info().ID] != nil {
			commit = false
			break
		}
	}
	if !commit {
		if nextRenderer != nil {
			_ = nextRenderer.Close()
		}
		if nextVideoPlayer != nil {
			_ = nextVideoPlayer.Close()
		}
		return results
	}
	service.mu.Lock()
	previous := service.renderer
	previousVideo := service.videoPlayer
	service.renderer = nextRenderer
	service.rendererError = rendererError
	service.videoPlayer = nextVideoPlayer
	service.hostFrames = hostFrames
	service.renderDirty = true
	service.lastRenderHash = 0
	service.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	if previousVideo != nil {
		_ = previousVideo.Close()
	}
	if nextRenderer != nil {
		service.wakeRenderer()
	}
	service.mu.Lock()
	targetRenderers := service.deviceRenderers
	targetVideos := service.deviceVideoPlayers
	service.deviceRenderers = make(map[string]*render.Renderer)
	service.deviceVideoPlayers = make(map[string]*video.Player)
	service.deviceThemes = make(map[string]theme.Manifest)
	service.mu.Unlock()
	for _, targetRenderer := range targetRenderers {
		if targetRenderer != nil {
			_ = targetRenderer.Close()
		}
	}
	for _, targetVideo := range targetVideos {
		if targetVideo != nil {
			_ = targetVideo.Close()
		}
	}
	service.mu.Lock()
	copied := manifest
	copied.Resources = append([]theme.Resource(nil), manifest.Resources...)
	copied.Layers = append([]theme.Layer(nil), manifest.Layers...)
	service.activeTheme = &copied
	service.mu.Unlock()
	return results
}

func (service *Service) rememberPendingTheme(deviceID string, manifest theme.Manifest) {
	service.mu.Lock()
	if service.pendingDeviceThemes == nil {
		service.pendingDeviceThemes = make(map[string]theme.Manifest)
	}
	service.pendingDeviceThemes[deviceID] = copyManifest(manifest)
	service.mu.Unlock()
}

func (service *Service) Status() Status {
	service.mu.RLock()
	defer service.mu.RUnlock()
	statuses := make([]DeviceStatus, 0, len(service.statuses))
	for _, driver := range service.drivers {
		status := service.statuses[driver.Info().ID]
		if reporter, ok := driver.(device.StorageReporter); ok {
			storage := reporter.StorageInfo()
			status.Storage = &storage
		} else {
			status.Storage = nil
		}
		statuses = append(statuses, status)
	}
	sortDeviceStatuses(statuses)
	audioStatus := AudioStatus{Enabled: service.audioEnabled}
	if provider, ok := service.spectrum.(interface{ Status() audio.EngineStatus }); ok {
		status := provider.Status()
		audioStatus.Running = status.Running
		audioStatus.Endpoint = status.Endpoint
		audioStatus.EndpointID = status.EndpointID
		audioStatus.EndpointRole = status.EndpointRole
		audioStatus.LastError = status.LastError
	}
	if audioStatus.Enabled && audioStatus.Running && service.spectrum != nil {
		frame := service.spectrum.Snapshot()
		audioStatus.CapturedAt = frame.CapturedAt
		audioStatus.Level = finiteAudioUnit(frame.Level)
		count := min(len(frame.Bars), audioStatusBandLimit)
		audioStatus.Bands = make([]float64, count)
		for index := 0; index < count; index++ {
			value := finiteAudioUnit(frame.Bars[index])
			audioStatus.Bands[index] = value
			if value > audioStatus.Peak {
				audioStatus.Peak = value
			}
		}
	}
	lyricsStatus := LyricsStatus{Enabled: service.lyricsEnabled}
	if provider, ok := service.lyrics.(interface{ Status() lyrics.EngineStatus }); ok {
		status := provider.Status()
		lyricsStatus.Running = status.Running
		lyricsStatus.Track = status.Track
		lyricsStatus.Provider = status.Provider
		lyricsStatus.HasLyrics = status.HasLyrics
		lyricsStatus.LyricMode = status.LyricMode
		lyricsStatus.Scrollable = status.Scrollable
		lyricsStatus.LastError = status.LastError
	}
	return Status{
		StartedAt: service.startedAt, TelemetryEnabled: service.telemetryEnabled,
		TelemetryInterval: service.interval.Milliseconds(), LastSnapshot: service.lastSnapshot,
		Audio: audioStatus, Lyrics: lyricsStatus, Devices: statuses,
	}
}

// finiteAudioUnit 防止采集驱动的非有限值污染 /api/status JSON。
func finiteAudioUnit(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Max(0, math.Min(1, value))
}

func (service *Service) tick(ctx context.Context) {
	service.mu.RLock()
	enabled := service.telemetryEnabled
	captureGeneration := service.telemetryGeneration
	drivers := append([]device.Driver(nil), service.drivers...)
	activeTheme := service.activeTheme
	spectrumProvider := service.spectrum
	audioEnabled := service.audioEnabled
	lyricsEnabled := service.lyricsEnabled
	lyricsClearPending := service.lyricsClearPending
	deviceThemes := make(map[string]theme.Manifest, len(service.deviceThemes))
	for id, manifest := range service.deviceThemes {
		deviceThemes[id] = manifest
	}
	service.mu.RUnlock()
	type target struct {
		driver      device.Driver
		manifest    theme.Manifest
		sendMetrics bool
		sendAudio   bool
		sendLyrics  bool
	}
	targets := make([]target, 0, len(drivers))
	needsCapture := false
	for _, driver := range drivers {
		if !driver.Info().Capabilities.SmallTelemetry {
			continue
		}
		if reporter, ok := driver.(device.ConnectionStateDriver); ok && !reporter.Connected() {
			// 真实断线后只由有退避的重连循环重新打开设备。
			// 遥测和歌词事件不再每秒向失效句柄无界试写。
			continue
		}
		manifest := activeTheme
		if targetManifest, ok := deviceThemes[driver.Info().ID]; ok {
			manifest = &targetManifest
		}
		if manifest == nil {
			continue
		}
		hasMetrics := manifestNeedsSmallTelemetry(*manifest)
		hasAudio := manifestHasLayer(*manifest, theme.LayerAudioBars) || manifestHasLayer(*manifest, theme.LayerAudioRing)
		hasLyrics := manifestHasLayer(*manifest, theme.LayerLyrics)
		_, dedicatedAudio := driver.(waveshareSpectrumSender)
		route := target{
			driver:      driver,
			manifest:    *manifest,
			sendMetrics: enabled && hasMetrics,
			sendAudio:   hasAudio && audioEnabled && !dedicatedAudio,
			sendLyrics:  hasLyrics && (lyricsEnabled || lyricsClearPending),
		}
		if !route.sendMetrics && !route.sendAudio && !route.sendLyrics {
			continue
		}
		needsCapture = needsCapture || route.sendMetrics
		targets = append(targets, route)
	}
	if len(targets) == 0 {
		return
	}
	snapshot := telemetry.Snapshot{CapturedAt: time.Now(), OmitHardwareMetrics: true}
	captureOK := false
	if needsCapture {
		captured, err := service.collector.Capture()
		if err == nil {
			snapshot = captured
			snapshot.OmitHardwareMetrics = false
			if snapshot.CapturedAt.IsZero() {
				snapshot.CapturedAt = time.Now()
			}
			service.mu.Lock()
			if service.telemetryGeneration == captureGeneration {
				service.lastSnapshot = snapshot
				captureOK = true
			}
			service.mu.Unlock()
		}
	}
	var spectrum audio.SpectrumFrame
	if spectrumProvider != nil && audioEnabled {
		spectrum = spectrumProvider.Snapshot()
	}
	clearAttempted := false
	clearSucceeded := true
	for _, target := range targets {
		deviceID := target.driver.Info().ID
		unlockSmallPacket, lockErr := service.lockSmallPacketDevice(ctx, deviceID)
		if lockErr != nil {
			return
		}
		deviceSnapshot := snapshot
		deviceSnapshot.OmitHardwareMetrics = !target.sendMetrics || !captureOK
		deviceSnapshot.OmitClock = !target.sendMetrics
		if target.sendAudio {
			deviceSnapshot.AudioSpectrum = encodeSmallSpectrum(spectrum.Bars)
		}
		currentManifest := target.manifest
		service.mu.RLock()
		currentLyricsProvider := service.lyrics
		currentLyricsEnabled := service.lyricsEnabled
		currentLyricsClearPending := service.lyricsClearPending
		if manifest, ok := service.deviceThemes[deviceID]; ok {
			currentManifest = manifest
		} else if service.activeTheme != nil {
			currentManifest = *service.activeTheme
		}
		service.mu.RUnlock()
		sendLyricsNow := target.sendLyrics && manifestHasLayer(currentManifest, theme.LayerLyrics) &&
			(currentLyricsEnabled || currentLyricsClearPending)
		if !target.sendMetrics && !target.sendAudio && !sendLyricsNow {
			unlockSmallPacket()
			continue
		}
		if sendLyricsNow {
			var lyricState render.LyricState
			var lyricStatus lyrics.EngineStatus
			lyricTimed := false
			if currentLyricsProvider != nil && currentLyricsEnabled {
				lyricState, lyricStatus, lyricTimed = snapshotLyricsProvider(currentLyricsProvider)
			}
			if currentLyricsClearPending && !currentLyricsEnabled {
				deviceSnapshot.LyricsClear = true
				clearAttempted = true
			} else {
				populateLyricPacket(&deviceSnapshot, lyricState, lyricStatus, lyricTimed)
			}
			if !deviceSnapshot.LyricsClear {
				sendProgressNow := target.driver.Info().Capabilities.DeliveryMode == device.DeliveryStoredTheme &&
					manifestNeedsLyricProgress(currentManifest)
				if !sendProgressNow || !applyLyricProgressForManifest(&deviceSnapshot, currentManifest, lyricState, lyricTimed) {
					deviceSnapshot.LyricsProgressKnown = false
					deviceSnapshot.LyricsProgressFineKnown = false
				}
			}
		}
		err := target.driver.SendTelemetry(ctx, deviceSnapshot)
		if sendLyricsNow {
			service.rememberLyricProgress(deviceID, deviceSnapshot, err)
		}
		if deviceSnapshot.LyricsClear && err != nil {
			clearSucceeded = false
		}
		unlockSmallPacket()
		service.setTransferError(deviceID, err)
	}
	if clearAttempted && clearSucceeded {
		service.mu.Lock()
		if !service.lyricsEnabled {
			service.lyricsClearPending = false
		}
		service.mu.Unlock()
	}
}

func (service *Service) lyricUpdateChannel() <-chan struct{} {
	service.mu.RLock()
	defer service.mu.RUnlock()
	if provider, ok := service.lyrics.(interface{ Updates() <-chan struct{} }); ok {
		return provider.Updates()
	}
	return nil
}

// runLyricEvents 把主机歌词时钟事件从普通遥测采集路径分离。事件通道
// 只承载“状态已变”信号，发送前总是读取最新双行文本；串口暂时繁忙时
// 最多合并为最新画面，不会排队发送已经过期的歌词行。
func (service *Service) runLyricEvents(ctx context.Context) {
	updates := service.lyricUpdateChannel()
	// 微雪原厂 Theme 没有可验证的本地插值字段，因此主机以 50ms
	// 节拍计算可见进度。活跃频谱以 10Hz 合入三字符 LFP；没有频谱时
	// 才使用独立 Fine 小包。旧长键只在跨整百分比时独立发送，绝不混入
	// 频带包。发送阻塞时 ticker 会自然合并，不积压旧进度。
	progressTicker := time.NewTicker(waveshareLyricProgressInterval)
	defer progressTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-updates:
			service.sendLyricsEvent(ctx)
			service.wakeRenderer()
		case <-service.smallPacketWake:
			// SetLyricsProvider 运行时替换后重新绑定事件通道；启停也走
			// 这里立即发送当前状态或显式清空包。
			updates = service.lyricUpdateChannel()
			service.sendLyricsEvent(ctx)
			service.wakeRenderer()
		case <-progressTicker.C:
			service.sendLyricsProgress(ctx)
		}
	}
}

// sendLyricsEvent 发送 Current/Next、对应行时间戳和 Playing 五个基础字段。
// 需要动态进度轨的驻留主题额外带细进度及按需兼容百分比；它不调用 Collector，
// 也不附加硬件指标、时钟、歌曲 ID 或连续播放位置。
func (service *Service) sendLyricsEvent(ctx context.Context) {
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()

	clearAttempted := false
	clearSucceeded := true
	for _, driver := range drivers {
		info := driver.Info()
		if !info.Capabilities.SmallTelemetry {
			continue
		}
		unlockSmallPacket, lockErr := service.lockSmallPacketDevice(ctx, info.ID)
		if lockErr != nil {
			return
		}
		if reporter, ok := driver.(device.ConnectionStateDriver); ok && !reporter.Connected() {
			unlockSmallPacket()
			continue
		}
		service.mu.RLock()
		lyricsProvider := service.lyrics
		lyricsEnabled := service.lyricsEnabled
		lyricsClearPending := service.lyricsClearPending
		var manifest *theme.Manifest
		if targetManifest, ok := service.deviceThemes[info.ID]; ok {
			copied := targetManifest
			manifest = &copied
		} else if service.activeTheme != nil {
			copied := *service.activeTheme
			manifest = &copied
		}
		service.mu.RUnlock()
		if manifest == nil || !manifestHasLayer(*manifest, theme.LayerLyrics) {
			unlockSmallPacket()
			continue
		}
		if !lyricsEnabled && !lyricsClearPending {
			unlockSmallPacket()
			continue
		}
		var lyricState render.LyricState
		var lyricStatus lyrics.EngineStatus
		lyricTimed := false
		if lyricsProvider != nil && lyricsEnabled {
			lyricState, lyricStatus, lyricTimed = snapshotLyricsProvider(lyricsProvider)
		}
		deviceSnapshot := telemetry.Snapshot{
			CapturedAt:          time.Now(),
			OmitHardwareMetrics: true,
			OmitClock:           true,
		}
		if lyricsClearPending && !lyricsEnabled {
			deviceSnapshot.LyricsClear = true
		} else {
			populateLyricPacket(&deviceSnapshot, lyricState, lyricStatus, lyricTimed)
		}
		progressEnabled := info.Capabilities.DeliveryMode == device.DeliveryStoredTheme && manifestNeedsLyricProgress(*manifest)
		if !deviceSnapshot.LyricsClear {
			if !progressEnabled || !applyLyricProgressForManifest(&deviceSnapshot, *manifest, lyricState, lyricTimed) {
				deviceSnapshot.LyricsProgressKnown = false
				deviceSnapshot.LyricsProgressFineKnown = false
			}
		}
		if deviceSnapshot.LyricsClear {
			clearAttempted = true
		}
		err := driver.SendTelemetry(ctx, deviceSnapshot)
		service.rememberLyricProgress(info.ID, deviceSnapshot, err)
		if deviceSnapshot.LyricsClear && err != nil {
			clearSucceeded = false
		}
		unlockSmallPacket()
		service.setTransferError(info.ID, err)
	}
	if clearAttempted && clearSucceeded {
		service.mu.Lock()
		if !service.lyricsEnabled {
			service.lyricsClearPending = false
		}
		service.mu.Unlock()
	}
}

// sendLyricsProgress 是 20Hz 上限的歌词调度。主机根据来源
// 播放器时间轴算好细进度，再按原生进度轨的可见宽度量化；相同可见步进、
// 暂停或无可信时间轴时完全静默。活跃频谱下本路径只补旧兼容键；否则
// 发送独立 Fine。旧长键即使与 Fine 同时变化也使用单独小包，避免以后
// 误并入频谱。所有路径共用设备门闩，确保暂停/拖动事件最终覆盖旧周期。
func (service *Service) sendLyricsProgress(ctx context.Context) {
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	for _, driver := range drivers {
		info := driver.Info()
		if !info.Capabilities.SmallTelemetry || info.Capabilities.DeliveryMode != device.DeliveryStoredTheme {
			continue
		}
		unlockSmallPacket, lockErr := service.lockSmallPacketDevice(ctx, info.ID)
		if lockErr != nil {
			return
		}
		if reporter, ok := driver.(device.ConnectionStateDriver); ok && !reporter.Connected() {
			unlockSmallPacket()
			continue
		}
		service.mu.RLock()
		lyricsProvider := service.lyrics
		lyricsEnabled := service.lyricsEnabled
		var manifest *theme.Manifest
		if targetManifest, ok := service.deviceThemes[info.ID]; ok {
			copied := targetManifest
			manifest = &copied
		} else if service.activeTheme != nil {
			copied := *service.activeTheme
			manifest = &copied
		}
		service.mu.RUnlock()
		if manifest == nil || !manifestNeedsLyricProgress(*manifest) {
			unlockSmallPacket()
			continue
		}
		if !lyricsEnabled || lyricsProvider == nil {
			unlockSmallPacket()
			continue
		}
		state, _, timed := snapshotLyricsProvider(lyricsProvider)
		if !timed || !state.PositionKnown || !state.Playing || strings.TrimSpace(state.Current) == "" {
			unlockSmallPacket()
			continue
		}
		progress, ok := lyricProgressForManifest(*manifest, state, timed)
		if !ok {
			unlockSmallPacket()
			continue
		}
		snapshot, changed := service.pendingLyricProgressSnapshot(info.ID, progress)
		if waveshareSpectrumCarriesLyricProgress(driver, manifest) {
			// 活跃频谱只承载短 LFP；周期路径仅在旧百分比跨界时
			// 发送兼容键，绝不重复发送 Fine 小包。
			snapshot.LyricsProgressFineKnown = false
			changed = snapshot.LyricsProgressKnown
		}
		if !changed {
			unlockSmallPacket()
			continue
		}
		capturedAt := time.Now()
		var sendErrors []error
		if snapshot.LyricsProgressFineKnown {
			// Fine 永远是短键单包；兼容长键不能搭便车进入这条高频路径。
			fineSnapshot := snapshot
			fineSnapshot.CapturedAt = capturedAt
			fineSnapshot.LyricsProgressKnown = false
			err := driver.SendTelemetry(ctx, fineSnapshot)
			service.rememberLyricProgress(info.ID, fineSnapshot, err)
			if err != nil {
				sendErrors = append(sendErrors, err)
			}
		}
		if snapshot.LyricsProgressKnown {
			// 旧 Theme 的长键只在整百分比跨界时独立发送。
			legacySnapshot := snapshot
			legacySnapshot.CapturedAt = capturedAt
			legacySnapshot.LyricsProgressFineKnown = false
			err := driver.SendTelemetry(ctx, legacySnapshot)
			service.rememberLyricProgress(info.ID, legacySnapshot, err)
			if err != nil {
				sendErrors = append(sendErrors, err)
			}
		}
		unlockSmallPacket()
		service.setTransferError(info.ID, errors.Join(sendErrors...))
	}
}

func snapshotLyricsProvider(provider LyricsProvider) (render.LyricState, lyrics.EngineStatus, bool) {
	if syncProvider, ok := provider.(interface {
		SyncSnapshot() (render.LyricState, lyrics.EngineStatus)
	}); ok {
		state, status := syncProvider.SyncSnapshot()
		return state, status, status.Scrollable
	}
	state := provider.Snapshot()
	if statusProvider, ok := provider.(interface{ Status() lyrics.EngineStatus }); ok {
		status := statusProvider.Status()
		return state, status, status.Scrollable
	}
	return state, lyrics.EngineStatus{}, state.PositionKnown && state.Current != lyrics.UnsynchronizedLyricsMessage
}

func populateLyricPacket(snapshot *telemetry.Snapshot, state render.LyricState, status lyrics.EngineStatus, timed bool) {
	current, next := lyricDisplayLines(state.Current, state.Next, status, timed)
	// 取词中、无歌词或播放器尚未给出当前句时显式清空驻留控件。不能只
	// 省略空字段，否则增量 QStringMap 会让副屏继续显示上一首歌；暂停时
	// Current 仍然存在，因此不会走到这里。
	if strings.TrimSpace(current) == "" {
		snapshot.LyricsClear = true
		return
	}
	snapshot.LyricsCurrent = current
	snapshot.LyricsNext = next
	snapshot.LyricsPlaying = state.Playing
	snapshot.LyricsVisible = true
	snapshot.LyricsVisibleKnown = true
	if timed && state.PositionKnown && state.Current != "" {
		snapshot.LyricsCurrentAtMS = state.CurrentAt.Milliseconds()
		snapshot.LyricsCurrentAtKnown = true
	}
	if timed && state.PositionKnown && state.Next != "" {
		snapshot.LyricsNextAtMS = state.NextAt.Milliseconds()
		snapshot.LyricsNextAtKnown = true
	}
	if timed && state.PositionKnown && state.Current != "" {
		progress := math.Max(0, math.Min(1, state.Progress))
		snapshot.LyricsProgressFine = int(math.Round(progress * telemetry.LyricsProgressFineMax))
		snapshot.LyricsProgressFineKnown = true
		snapshot.LyricsProgress = legacyLyricProgress(snapshot.LyricsProgressFine)
		snapshot.LyricsProgressKnown = true
	}
}

// lyricDisplayLines 只整理真实可显示歌词。取词中、无歌词、登录/网络失败
// 等诊断仍保留在状态页，副屏歌词层保持完全隐藏；不可滚动的纯文本歌词
// 和已经显示到末句的说明仍可作为歌词内容的一部分显示。
func lyricDisplayLines(current, next string, status lyrics.EngineStatus, timed bool) (string, string) {
	current = compactLyricsLine(current)
	next = compactLyricsLine(next)
	if current == "" {
		return "", ""
	}
	if current == lyrics.UnsynchronizedLyricsMessage || (status.HasLyrics && !status.Scrollable) {
		if next == "" {
			next = "请在原音乐播放器查看完整歌词"
		}
		return current, next
	}
	if current != "" {
		if next == "" {
			if timed {
				next = "已是本首歌曲的最后一句"
			} else {
				next = "播放后将自动同步下一句"
			}
		}
		return current, next
	}
	return "", ""
}

func lyricErrorNeedsPlatformLogin(errorText string) bool {
	errorText = strings.ToLower(strings.TrimSpace(errorText))
	for _, marker := range []string{"需要有效的登录", "需要登录", "请登录", "会员权限", "vip"} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

func lyricErrorIsNetwork(errorText string) bool {
	errorText = strings.ToLower(strings.TrimSpace(errorText))
	for _, marker := range []string{"超时", "timeout", "deadline exceeded", "network", "connection", "网络", "dns"} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

// lyricErrorMeansNoLyrics 兼容各精确 ID 提供方的真实错误文本。这里仅把
// 服务端已经明确判定“没有可用歌词”的结果归类为无歌词；网络、登录和
// 播放器错误仍保留为可恢复故障提示。
func lyricErrorMeansNoLyrics(errorText string) bool {
	errorText = strings.ToLower(strings.TrimSpace(errorText))
	for _, marker := range []string{
		"没有歌词",
		"暂无歌词",
		"没有 lrc 歌词",
		"无 lrc 歌词",
		"歌词内容为空",
		"没有与当前歌曲匹配的同步歌词",
	} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

// lyricProgressForManifest 把 0..1 的句内进度量化到实际进度轨的可见
// 像素，再映射为新版 Theme 使用的 0..10000 整数。窄轨不会因为数值
// 变化但像素未变化而浪费 USB 包，宽轨也不再被旧协议的 101 个值限制。
func lyricProgressForManifest(manifest theme.Manifest, state render.LyricState, timed bool) (int, bool) {
	if !timed || !state.PositionKnown || strings.TrimSpace(state.Current) == "" {
		return 0, false
	}
	steps := 0
	for _, layer := range manifest.Layers {
		if layer.Type != theme.LayerLyrics || !theme.LayerVisible(layer) || layer.Opacity <= 0 || !theme.LyricsShowsProgress(layer) {
			continue
		}
		// 清单尚未补齐画布宽度时仍保留 0.1% 的细度；正式微雪清单
		// 总有 Width，因此实际发送会继续按物理像素去重。
		trackWidth := 1000
		if manifest.Width > 0 && layer.Bounds.Width > 0 {
			layerWidth := max(1, int(math.Round(layer.Bounds.Width*float64(manifest.Width))))
			padding := min(8, max(1, layerWidth/24))
			trackWidth = max(1, layerWidth-padding*2)
		}
		steps = max(steps, min(telemetry.LyricsProgressFineMax, trackWidth))
	}
	if steps <= 0 {
		return 0, false
	}
	progress := math.Max(0, math.Min(1, state.Progress))
	visibleStep := int(math.Round(progress * float64(steps)))
	return int(math.Round(float64(visibleStep) * telemetry.LyricsProgressFineMax / float64(steps))), true
}

func legacyLyricProgress(fine int) int {
	fine = max(0, min(telemetry.LyricsProgressFineMax, fine))
	return int(math.Round(float64(fine) * telemetry.LyricsProgressLegacyMax / telemetry.LyricsProgressFineMax))
}

func applyLyricProgressForManifest(snapshot *telemetry.Snapshot, manifest theme.Manifest, state render.LyricState, timed bool) bool {
	if !manifestNeedsLyricProgress(manifest) {
		snapshot.LyricsProgressKnown = false
		snapshot.LyricsProgressFineKnown = false
		return false
	}
	progress, ok := lyricProgressForManifest(manifest, state, timed)
	if !ok {
		// 切歌取词中、无歌词、纯文本不可滚动或播放器
		// 时间轴暂时丢失时，必须在状态事件中显式把新旧进度都归零。
		// 否则增量 QStringMap 会让屏端残留上一句的进度轨。
		snapshot.LyricsProgress = 0
		snapshot.LyricsProgressKnown = true
		snapshot.LyricsProgressFine = 0
		snapshot.LyricsProgressFineKnown = true
		return true
	}
	snapshot.LyricsProgressFine = progress
	snapshot.LyricsProgressFineKnown = true
	snapshot.LyricsProgress = legacyLyricProgress(progress)
	snapshot.LyricsProgressKnown = true
	return true
}

func (service *Service) rememberLyricProgress(deviceID string, snapshot telemetry.Snapshot, sendErr error) {
	if strings.TrimSpace(deviceID) == "" || sendErr != nil {
		return
	}
	if snapshot.LyricsClear {
		snapshot.LyricsProgress = 0
		snapshot.LyricsProgressKnown = true
		snapshot.LyricsProgressFine = 0
		snapshot.LyricsProgressFineKnown = true
	}
	if !snapshot.LyricsProgressKnown && !snapshot.LyricsProgressFineKnown {
		return
	}
	service.mu.Lock()
	last := service.lyricProgressSent[deviceID]
	if snapshot.LyricsProgressKnown {
		last.legacy = max(0, min(telemetry.LyricsProgressLegacyMax, snapshot.LyricsProgress))
		last.legacyKnown = true
	}
	if snapshot.LyricsProgressFineKnown {
		last.fine = max(0, min(telemetry.LyricsProgressFineMax, snapshot.LyricsProgressFine))
		last.fineKnown = true
	}
	service.lyricProgressSent[deviceID] = last
	service.mu.Unlock()
}

// pendingLyricProgressSnapshot 只选择相对上次成功发送发生变化的键。
// 调用方决定把 Fine 合入频谱还是作为短键单包，并把旧百分比始终拆成
// 独立兼容包；各包成功后分别 remember，失败的键会在下个节拍重试。
func (service *Service) pendingLyricProgressSnapshot(deviceID string, fine int) (telemetry.Snapshot, bool) {
	fine = max(0, min(telemetry.LyricsProgressFineMax, fine))
	legacy := legacyLyricProgress(fine)
	service.mu.RLock()
	last, sent := service.lyricProgressSent[deviceID]
	service.mu.RUnlock()
	snapshot := telemetry.Snapshot{OmitHardwareMetrics: true, OmitClock: true}
	if !sent || !last.fineKnown || last.fine != fine {
		snapshot.LyricsProgressFine = fine
		snapshot.LyricsProgressFineKnown = true
	}
	if !sent || !last.legacyKnown || last.legacy != legacy {
		snapshot.LyricsProgress = legacy
		snapshot.LyricsProgressKnown = true
	}
	return snapshot, snapshot.LyricsProgressFineKnown || snapshot.LyricsProgressKnown
}

// lockSmallPacketDevice 获取同一设备的实时小包门闩。调用方必须在取得
// 门闩之后再读取歌词快照，并在确认发送结果及更新去重状态后释放。
func (service *Service) lockSmallPacketDevice(ctx context.Context, deviceID string) (func(), error) {
	service.mu.Lock()
	if service.smallPacketDeviceGates == nil {
		service.smallPacketDeviceGates = make(map[string]chan struct{})
	}
	gate := service.smallPacketDeviceGates[deviceID]
	if gate == nil {
		gate = make(chan struct{}, 1)
		service.smallPacketDeviceGates[deviceID] = gate
	}
	service.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (service *Service) wakeSmallPacket() {
	select {
	case service.smallPacketWake <- struct{}{}:
	default:
	}
}

const maximumSmallPacketLyricRunes = 256

func compactLyricsLine(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maximumSmallPacketLyricRunes {
		return value
	}
	return string(runes[:maximumSmallPacketLyricRunes-1]) + "…"
}

func encodeSmallSpectrum(bars []float64) string {
	if len(bars) == 0 {
		bars = make([]float64, 18)
	}
	values := make([]string, min(48, len(bars)))
	for index := range values {
		value := math.Max(0, math.Min(1, bars[index]))
		values[index] = strconv.Itoa(int(math.Round(value * 100)))
	}
	return strings.Join(values, ",")
}

func manifestHasLayer(manifest theme.Manifest, layerType theme.LayerType) bool {
	for _, layer := range manifest.Layers {
		if layer.Type == layerType && theme.LayerVisible(layer) && layer.Opacity > 0 {
			return true
		}
	}
	return false
}

func (service *Service) connectAll(ctx context.Context) {
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	for _, driver := range drivers {
		attemptContext, cancel := context.WithTimeout(ctx, service.reconnectAttemptTimeout())
		err := driver.Connect(attemptContext)
		cancel()
		service.mu.Lock()
		status := service.statuses[driver.Info().ID]
		status.Info = driver.Info()
		if err != nil {
			status = service.recordReconnectFailureLocked(status, err, time.Now())
		} else {
			status = recordConnectionSuccess(status, time.Now())
			status.LastError = service.themeApplyErrors[driver.Info().ID]
		}
		service.statuses[driver.Info().ID] = status
		service.mu.Unlock()
	}
}

func (service *Service) reconnectDisconnected(ctx context.Context) {
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	for _, driver := range drivers {
		if !service.reconnectDue(driver.Info().ID, time.Now()) {
			continue
		}
		attemptContext, cancel := context.WithTimeout(ctx, service.reconnectAttemptTimeout())
		service.reconnectDriver(attemptContext, driver)
		cancel()
	}
}

// reconnectDisconnectedAsync 为每台断线设备启动独立单次重连。
// HID 枚举、固件握手或主题恢复变慢时，遥测、渲染和其它
// 屏幕的事件循环仍可继续运行。
func (service *Service) reconnectDisconnectedAsync(ctx context.Context) {
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	for _, driver := range drivers {
		id := driver.Info().ID
		if !service.reconnectDue(id, time.Now()) || !service.beginReconnectAttempt(id) {
			continue
		}
		go func(driver device.Driver) {
			defer service.reconnectWG.Done()
			defer service.finishReconnectAttempt(driver.Info().ID)
			attemptContext, cancel := context.WithTimeout(ctx, service.reconnectAttemptTimeout())
			defer cancel()
			service.reconnectDriver(attemptContext, driver)
		}(driver)
	}
}

// replayPendingPowerAsync 补偿等待设备门闩超时的亮度请求。主题可能在
// 本地渲染或视频打开阶段失败，因而没有机会在主题收尾代为重放；只要
// 设备仍在线，这里便会在下一次重连节拍重新争用同一门闩。真实 USB
// 发送错误仍由 sendRememberedPowerLocked 标记断线，交给正常重连处理。
func (service *Service) replayPendingPowerAsync(ctx context.Context) {
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	for _, candidate := range drivers {
		brightness, ok := candidate.(device.BrightnessDriver)
		if !ok || !service.pendingPowerReplayDue(candidate.Info().ID) || !service.beginReconnectAttempt(candidate.Info().ID) {
			continue
		}
		go func(deviceID string, driver device.BrightnessDriver) {
			defer service.reconnectWG.Done()
			defer service.finishReconnectAttempt(deviceID)
			attemptContext, cancel := context.WithTimeout(ctx, service.reconnectAttemptTimeout())
			defer cancel()
			service.replayPendingPower(attemptContext, deviceID, driver)
		}(candidate.Info().ID, brightness)
	}
}

func (service *Service) pendingPowerReplayDue(deviceID string) bool {
	service.mu.RLock()
	defer service.mu.RUnlock()
	_, known := service.powerIntents[deviceID]
	return known && service.statuses[deviceID].Connected && !service.initialPowerReplay[deviceID] &&
		service.powerAppliedGeneration[deviceID] < service.powerIntentGeneration[deviceID]
}

func (service *Service) replayPendingPower(ctx context.Context, deviceID string, driver device.BrightnessDriver) {
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		return
	}
	defer finishOperation()
	unlockDevice, err := service.lockDeviceTheme(operationContext, deviceID)
	if err != nil {
		// 门闩仍忙时保留 pending 代次，下一节拍继续尝试；门闩等待
		// 本身不代表 USB 已断开，不能制造虚假的离线状态。
		return
	}
	defer unlockDevice()
	if !service.pendingPowerReplayDue(deviceID) {
		return
	}
	_ = service.applyPendingPowerIntentLocked(operationContext, deviceID, driver)
}

func (service *Service) reconnectDriver(ctx context.Context, driver device.Driver) {
	id := driver.Info().ID
	service.mu.RLock()
	status := service.statuses[id]
	if status.Connected || (!status.RetryAt.IsZero() && time.Now().Before(status.RetryAt)) {
		service.mu.RUnlock()
		return
	}
	service.mu.RUnlock()

	if _, ok := driver.(device.HostFrameDriver); ok {
		// 重连前丢弃断线会话遗留的帧；重连成功后由
		// renderDirty 补一帧当前主题，不回放旧画面。
		service.invalidateHostFrameDelivery(id)
	}
	// Connect、主题下发与状态提交共享同一设备门闩。重连拿到门闩后
	// 才读取最新主题，因此不会用启动重连时的旧快照覆盖用户刚保存的主题。
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	reconnectedHostFrames := false
	reconnectingCalibration := false
	powerRestoredByTheme := false
	connectSucceeded := false
	if err == nil {
		unlockDevice, lockErr := service.lockDeviceTheme(operationContext, id)
		if lockErr != nil {
			err = lockErr
		} else {
			err = driver.Connect(operationContext)
			if err == nil {
				connectSucceeded = true
				// Connect 建立了新硬件会话，旧会话确认过的亮度不能再
				// 作为 applied 依据。即使随后主题在本地准备阶段失败，
				// 也要在释放设备门闩前补发最新关屏/恢复意图。
				service.markPowerIntentUnapplied(id)
				var manifest *theme.Manifest
				pendingTheme := false
				activeHostFrames := false
				var rendererError error
				service.mu.RLock()
				activeHostFrames = service.hostFrames[id]
				reconnectedHostFrames = activeHostFrames
				rendererError = service.rendererError
				if target, ok := service.pendingDeviceThemes[id]; ok {
					copied := copyManifest(target)
					manifest = &copied
					pendingTheme = true
				} else if target, ok := service.deviceThemes[id]; ok {
					copied := copyManifest(target)
					manifest = &copied
				} else if service.activeTheme != nil {
					copied := copyManifest(*service.activeTheme)
					manifest = &copied
				}
				service.mu.RUnlock()
				reconnectingCalibration = service.hasOrientationCalibration(id)
				if reconnectingCalibration {
					// 重插 USB 会清空屏幕会话。校准期间只能重发原始方向测试图，
					// 不能按常规重连逻辑重新应用主题，否则 UI 仍显示“测试中”而
					// 物理屏已经悄悄回到主题画面。
					service.suspendHostFrameDelivery(id)
					reconnectedHostFrames = false
					calibrator, supported := driver.(device.OrientationCalibrationDriver)
					if !supported {
						err = errors.New("重连设备不再支持方向校准")
					} else {
						err = calibrator.ShowOrientationCalibration(operationContext)
					}
				} else if manifest != nil {
					if pendingTheme {
						reconnectedHostFrames = usesHostFrames(driver, *manifest)
						err = service.applyThemeToLocked(operationContext, id, driver, *manifest)[id]
						if err == nil {
							powerRestoredByTheme = service.powerIntentApplied(id)
						}
					} else if activeHostFrames && rendererError != nil {
						err = rendererError
					} else {
						err = applyDriverTheme(operationContext, driver, *manifest, activeHostFrames)
						if err == nil {
							// 重载主题可能改变背光，先重放最新开/关屏意图，
							// 再发送遥测/歌词小包，避免长刷新期间亮度错位。
							if _, known := service.powerIntent(id); known {
								err = service.reapplyPowerIntentLocked(operationContext, id, driver)
								powerRestoredByTheme = err == nil
							}
						}
						if err == nil {
							// 重连会清空设备 ACK 缓存；让频谱运行态重新开始并补发当前小包。
							service.mu.Lock()
							service.spectrumThemeGeneration[id]++
							service.mu.Unlock()
							err = service.refreshDeviceStateAfterTheme(operationContext, driver, *manifest)
						}
					}
				}
				if err != nil && !service.powerIntentApplied(id) {
					if powerErr := service.applyPendingPowerIntentLocked(operationContext, id, driver); powerErr != nil {
						err = fmt.Errorf("%v；恢复亮度状态失败: %w", err, powerErr)
					}
				}
				if err == nil && !powerRestoredByTheme {
					err = service.reapplyPowerIntentLocked(operationContext, id, driver)
				}
			}
			unlockDevice()
		}
		finishOperation()
	}
	sessionUsable := err == nil
	if err != nil && connectSucceeded && !reconnectingCalibration {
		// 主题的 render/video 本地错误不等于 USB 会话断线。真实驱动
		// 明确报告仍在线时保留 Connected，并把错误作为主题错误展示；
		// 否则仍走正常退避重连。
		if reporter, ok := driver.(device.ConnectionStateDriver); ok {
			sessionUsable = reporter.Connected()
		}
	}
	service.mu.Lock()
	status = service.statuses[id]
	status.Info = driver.Info()
	if sessionUsable {
		status = recordConnectionSuccess(status, time.Now())
		if err == nil {
			status.LastError = service.themeApplyErrors[id]
		} else {
			service.themeApplyErrors[id] = err.Error()
			status.LastError = err.Error()
		}
	} else {
		status = service.recordReconnectFailureLocked(status, err, time.Now())
	}
	service.statuses[id] = status
	service.mu.Unlock()
	if sessionUsable {
		service.mu.Lock()
		if reconnectedHostFrames {
			service.renderDirty = true
		}
		service.mu.Unlock()
		service.wakeRenderer()
	}
}

func (service *Service) reconnectDue(id string, now time.Time) bool {
	service.mu.RLock()
	defer service.mu.RUnlock()
	status := service.statuses[id]
	return !status.Connected && (status.RetryAt.IsZero() || !now.Before(status.RetryAt))
}

func (service *Service) reconnectAttemptTimeout() time.Duration {
	if service.reconnectTimeout > 0 {
		return service.reconnectTimeout
	}
	return 8 * time.Second
}

func (service *Service) startReconnectAttempts() {
	service.reconnectMu.Lock()
	service.reconnectClosing = false
	if service.reconnectInFlight == nil {
		service.reconnectInFlight = make(map[string]bool)
	}
	service.reconnectMu.Unlock()
}

func (service *Service) beginReconnectAttempt(id string) bool {
	service.reconnectMu.Lock()
	defer service.reconnectMu.Unlock()
	if service.reconnectClosing || service.reconnectInFlight[id] {
		return false
	}
	service.reconnectInFlight[id] = true
	service.reconnectWG.Add(1)
	return true
}

func (service *Service) finishReconnectAttempt(id string) {
	service.reconnectMu.Lock()
	delete(service.reconnectInFlight, id)
	service.reconnectMu.Unlock()
}

func (service *Service) stopReconnectAttempts() {
	service.reconnectMu.Lock()
	service.reconnectClosing = true
	service.reconnectMu.Unlock()
	service.reconnectWG.Wait()
}

func (service *Service) setError(id string, err error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	status := service.statuses[id]
	if err == nil {
		status.LastError = service.themeApplyErrors[id]
	} else {
		status.LastError = err.Error()
	}
	service.statuses[id] = status
}

func (service *Service) setDriverError(driver device.Driver, err error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	id := driver.Info().ID
	status := service.statuses[id]
	if err == nil {
		delete(service.themeApplyErrors, id)
		status.LastError = ""
	} else {
		service.themeApplyErrors[id] = err.Error()
		status.LastError = err.Error()
		if reporter, ok := driver.(device.ConnectionStateDriver); ok && !reporter.Connected() {
			status = service.recordTransferDisconnectLocked(status, err, time.Now())
		}
	}
	service.statuses[id] = status
}

func (service *Service) setTransferError(id string, err error) {
	if errors.Is(err, device.ErrRealtimeFrameDropped) {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	status := service.statuses[id]
	if err == nil {
		status.LastError = service.themeApplyErrors[id]
	} else if service.driverReportsConnectedLocked(id) {
		// 调用方截止、ACK 超时或显示模式短暂无响应时，
		// 真实驱动会保留仍可用的 HID 句柄。此时只显示
		// 本次操作错误，不反复翻转 Connected 造成 UI/屏幕闪烁。
		status.LastError = err.Error()
	} else {
		status = service.recordTransferDisconnectLocked(status, err, time.Now())
	}
	service.statuses[id] = status
}

func (service *Service) driverReportsConnectedLocked(id string) bool {
	for _, candidate := range service.drivers {
		if candidate.Info().ID != id {
			continue
		}
		reporter, ok := candidate.(device.ConnectionStateDriver)
		return ok && reporter.Connected()
	}
	return false
}

func (service *Service) recordTransferDisconnectLocked(status DeviceStatus, err error, now time.Time) DeviceStatus {
	wasConnected := status.Connected
	status.Connected = false
	status.LastError = err.Error()
	status.LastConnectionError = err.Error()
	if wasConnected || status.LastDisconnectedAt.IsZero() {
		status.LastDisconnectedAt = now
		// 传输断开本身占用退避序列的第一级：2 秒后第一次重连；
		// 若该次重连失败，下一次必须进入 4 秒，不能再次等待 2 秒。
		status.ReconnectAttempts = 1
		status.RetryAt = now.Add(service.reconnectDelay(1))
	} else if status.RetryAt.IsZero() {
		status.RetryAt = now.Add(service.reconnectDelay(max(1, status.ReconnectAttempts)))
	}
	return status
}

func (service *Service) recordReconnectFailureLocked(status DeviceStatus, err error, now time.Time) DeviceStatus {
	status.Connected = false
	status.LastError = err.Error()
	status.LastConnectionError = err.Error()
	if status.LastDisconnectedAt.IsZero() {
		status.LastDisconnectedAt = now
	}
	status.ReconnectAttempts++
	status.RetryAt = now.Add(service.reconnectDelay(status.ReconnectAttempts))
	return status
}

func recordConnectionSuccess(status DeviceStatus, now time.Time) DeviceStatus {
	status.Connected = true
	status.LastConnectedAt = now
	status.LastVerifiedAt = now
	status.LastConnectionCheck = now
	status.ReconnectAttempts = 0
	status.RetryAt = time.Time{}
	return status
}

func (service *Service) reconnectDelay(attempt int) time.Duration {
	base := service.reconnectEvery
	if base <= 0 {
		base = 2 * time.Second
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for step := 1; step < attempt && delay < maximumReconnectDelay; step++ {
		if delay > maximumReconnectDelay/2 {
			delay = maximumReconnectDelay
			break
		}
		delay *= 2
	}
	if delay > maximumReconnectDelay {
		return maximumReconnectDelay
	}
	return delay
}

func (service *Service) renderHostFrames(ctx context.Context) {
	service.mu.RLock()
	renderer := service.renderer
	videoPlayer := service.videoPlayer
	deviceRenderers := make(map[string]*render.Renderer, len(service.deviceRenderers))
	for id, targetRenderer := range service.deviceRenderers {
		deviceRenderers[id] = targetRenderer
	}
	deviceVideoPlayers := make(map[string]*video.Player, len(service.deviceVideoPlayers))
	for id, targetVideo := range service.deviceVideoPlayers {
		deviceVideoPlayers[id] = targetVideo
	}
	snapshot := service.lastSnapshot
	telemetryEnabled := service.telemetryEnabled
	spectrumProvider := service.spectrum
	audioEnabled := service.audioEnabled
	lyricsProvider := service.lyrics
	lyricsEnabled := service.lyricsEnabled
	quality := service.jpegQuality
	drivers := append([]device.Driver(nil), service.drivers...)
	statuses := make(map[string]DeviceStatus, len(service.statuses))
	for id, status := range service.statuses {
		statuses[id] = status
	}
	hostFrames := make(map[string]bool, len(service.hostFrames))
	for id, active := range service.hostFrames {
		hostFrames[id] = active
	}
	hostFrameGenerations := make(map[string]uint64, len(service.hostFrameGenerations))
	for id, generation := range service.hostFrameGenerations {
		hostFrameGenerations[id] = generation
	}
	hostFrameSuspended := make(map[string]bool, len(service.hostFrameSuspended))
	for id, suspended := range service.hostFrameSuspended {
		hostFrameSuspended[id] = suspended
	}
	activeTheme := service.activeTheme
	deviceThemes := make(map[string]theme.Manifest, len(service.deviceThemes))
	for id, manifest := range service.deviceThemes {
		deviceThemes[id] = manifest
	}
	service.mu.RUnlock()
	if renderer == nil && len(deviceRenderers) == 0 {
		return
	}
	state := render.State{Telemetry: snapshot, TelemetryEnabled: telemetryEnabled}
	if videoPlayer != nil {
		state.VideoFrame, state.VideoSequence = videoPlayer.Snapshot()
		if state.VideoFrame == nil {
			if videoErr := videoPlayer.Err(); videoErr != nil {
				for _, driver := range drivers {
					if _, ok := driver.(device.HostFrameDriver); ok {
						service.setError(driver.Info().ID, videoErr)
					}
				}
			}
		}
	}
	if spectrumProvider != nil && audioEnabled {
		state.Spectrum = spectrumProvider.Snapshot()
	}
	if lyricsProvider != nil && lyricsEnabled {
		lyricState, lyricStatus, lyricTimed := snapshotLyricsProvider(lyricsProvider)
		lyricState.Current, lyricState.Next = lyricDisplayLines(lyricState.Current, lyricState.Next, lyricStatus, lyricTimed)
		state.Lyrics = lyricState
	}
	continuousAudio := false
	if audioEnabled && !spectrumFrameStrictSilent(state.Spectrum) {
		for _, driver := range drivers {
			id := driver.Info().ID
			if !hostFrames[id] || hostFrameSuspended[id] || !statuses[id].Connected || statuses[id].PoweredOff {
				continue
			}
			manifest := activeTheme
			if target, ok := deviceThemes[id]; ok {
				manifest = &target
			}
			if manifestNeedsAudioTimeFrames(manifest) {
				continuousAudio = true
				break
			}
		}
	}
	signature := frameSignature(state)
	service.mu.Lock()
	if !service.renderDirty && signature == service.lastRenderHash && len(deviceVideoPlayers) == 0 && !continuousAudio {
		service.mu.Unlock()
		return
	}
	service.renderDirty = false
	service.lastRenderHash = signature
	service.mu.Unlock()
	type frameKey struct {
		resolution [2]int
		maxBytes   int
		renderer   *render.Renderer
		transform  string
	}
	encoded := make(map[frameKey][]byte)
	attempted := false
	succeeded := false
	for _, driver := range drivers {
		host, ok := driver.(device.HostFrameDriver)
		if !ok || !hostFrames[driver.Info().ID] || hostFrameSuspended[driver.Info().ID] ||
			!statuses[driver.Info().ID].Connected || statuses[driver.Info().ID].PoweredOff {
			continue
		}
		driverRenderer := renderer
		if targetRenderer, ok := deviceRenderers[driver.Info().ID]; ok {
			driverRenderer = targetRenderer
		}
		if driverRenderer == nil {
			continue
		}
		driverVideo := videoPlayer
		if targetVideo, ok := deviceVideoPlayers[driver.Info().ID]; ok {
			driverVideo = targetVideo
		}
		driverState := state
		if driverVideo != nil {
			driverState.VideoFrame, driverState.VideoSequence = driverVideo.Snapshot()
		}
		attempted = true
		resolution := driver.Info().Resolution
		transformKey := ""
		if transformer, ok := driver.(device.HostFrameTransformer); ok {
			transformKey = transformer.HostFrameTransformKey()
		}
		key := frameKey{resolution: resolution, maxBytes: driver.Info().Capabilities.MaxFrameBytes, renderer: driverRenderer, transform: transformKey}
		data, exists := encoded[key]
		if !exists {
			frame, err := driverRenderer.Render(resolution[0], resolution[1], driverState)
			if err != nil {
				service.setError(driver.Info().ID, err)
				continue
			}
			var outputFrame image.Image = frame
			if transformer, ok := driver.(device.HostFrameTransformer); ok {
				outputFrame = transformer.TransformHostFrame(frame)
			}
			data, _, err = encodeJPEGWithinLimit(outputFrame, quality, key.maxBytes)
			if err != nil {
				service.setError(driver.Info().ID, err)
				continue
			}
			encoded[key] = data
		}
		if service.enqueueHostFrame(ctx, driver.Info().ID, host, hostFrameGenerations[driver.Info().ID], data) {
			succeeded = true
		}
	}
	if attempted && !succeeded {
		service.mu.Lock()
		service.renderDirty = true
		service.mu.Unlock()
	}
}

// manifestNeedsAudioTimeFrames 判断渲染结果是否依赖上一帧或经过时间。
// 平滑和峰值保持都必须按 host_render_fps 推进；若输入严格为零，渲染器
// 会立即清空状态，此后可重新启用签名去重，避免静音时持续发送空帧。
func manifestNeedsAudioTimeFrames(manifest *theme.Manifest) bool {
	if manifest == nil {
		return false
	}
	for _, layer := range manifest.Layers {
		if (layer.Type != theme.LayerAudioBars && layer.Type != theme.LayerAudioRing) ||
			!theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		visual := theme.AudioVisualOptionsForLayer(layer)
		if visual.Smoothing > 0 || theme.OptionBool(layer.Options, "peak_hold", false) {
			return true
		}
	}
	return false
}

func spectrumFrameStrictSilent(frame audio.SpectrumFrame) bool {
	if len(frame.Bars) == 0 {
		return true
	}
	for _, value := range frame.Bars {
		if finiteAudioUnit(value) > 0 {
			return false
		}
	}
	return true
}

// encodeJPEGWithinLimit 在设备有帧大小限制时逐级降低质量，避免把超限数据写入 USB。
func encodeJPEGWithinLimit(source image.Image, quality, maxBytes int) ([]byte, int, error) {
	if quality < 1 || quality > 100 {
		quality = 82
	}
	minimumQuality := quality
	if maxBytes > 0 {
		minimumQuality = min(35, quality)
	}
	for current := quality; ; current = max(minimumQuality, current-10) {
		var output bytes.Buffer
		if err := jpeg.Encode(&output, source, &jpeg.Options{Quality: current}); err != nil {
			return nil, current, err
		}
		if maxBytes <= 0 || output.Len() <= maxBytes {
			return output.Bytes(), current, nil
		}
		if current == minimumQuality {
			return nil, current, errors.New("JPEG 在最低自适应质量下仍超过设备帧大小限制")
		}
	}
}

func (service *Service) wakeRenderer() {
	select {
	case service.renderWake <- struct{}{}:
	default:
	}
}

func (service *Service) closeRenderer() {
	service.mu.Lock()
	renderer := service.renderer
	targetRenderers := service.deviceRenderers
	service.renderer = nil
	service.rendererError = nil
	service.deviceRenderers = make(map[string]*render.Renderer)
	service.mu.Unlock()
	if renderer != nil {
		_ = renderer.Close()
	}
	for _, targetRenderer := range targetRenderers {
		if targetRenderer != nil {
			_ = targetRenderer.Close()
		}
	}
}

func (service *Service) closeVideoPlayer() {
	service.mu.Lock()
	player := service.videoPlayer
	targetPlayers := service.deviceVideoPlayers
	service.videoPlayer = nil
	service.deviceVideoPlayers = make(map[string]*video.Player)
	service.mu.Unlock()
	if player != nil {
		_ = player.Close()
	}
	for _, targetPlayer := range targetPlayers {
		if targetPlayer != nil {
			_ = targetPlayer.Close()
		}
	}
}

func (service *Service) applyInitialTheme(ctx context.Context) {
	service.mu.Lock()
	manifest := service.initialTheme
	targets := make(map[string]theme.Manifest, len(service.initialDeviceThemes))
	for id, target := range service.initialDeviceThemes {
		targets[id] = copyManifest(target)
	}
	service.initialTheme = nil
	service.initialDeviceThemes = make(map[string]theme.Manifest)
	service.mu.Unlock()
	if manifest != nil {
		service.ApplyTheme(ctx, *manifest)
	}
	for id, target := range targets {
		service.ApplyThemeTo(ctx, id, target)
	}
}

type initialPowerTarget struct {
	id     string
	driver device.BrightnessDriver
}

func (service *Service) collectInitialPowerTargets() []initialPowerTarget {
	service.mu.RLock()
	defer service.mu.RUnlock()
	targets := make([]initialPowerTarget, 0, len(service.initialPowerReplay))
	for _, candidate := range service.drivers {
		id := candidate.Info().ID
		if !service.initialPowerReplay[id] {
			continue
		}
		brightness, ok := candidate.(device.BrightnessDriver)
		if !ok {
			continue
		}
		targets = append(targets, initialPowerTarget{id: id, driver: brightness})
	}
	return targets
}

// applyInitialPower 必须在启动主题全部完成后执行。进程上次退出时
// 留在硬件上的亮度会被持久化意图纠正，包括显式 false 的恢复开屏。
func (service *Service) applyInitialPower(ctx context.Context) {
	service.applyInitialPowerTargets(ctx, service.collectInitialPowerTargets())
}

func (service *Service) applyInitialPowerTargets(ctx context.Context, targets []initialPowerTarget) {
	if len(targets) == 0 {
		return
	}
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		return
	}
	defer finishOperation()
	for _, target := range targets {
		service.suspendHostFrameDelivery(target.id)
	}
	var wait sync.WaitGroup
	for _, target := range targets {
		target := target
		wait.Add(1)
		go func() {
			defer wait.Done()
			deviceContext, cancel := context.WithTimeout(operationContext, service.powerOperationTimeout())
			defer cancel()
			unlockDevice, lockErr := service.lockDeviceTheme(deviceContext, target.id)
			if lockErr == nil {
				service.mu.RLock()
				replayPending := service.initialPowerReplay[target.id]
				generation := service.powerIntentGeneration[target.id]
				service.mu.RUnlock()
				// collect 之后若用户已经明确操作过，公共电源命令会删除
				// initialPowerReplay；此时不能再重复发送启动快照。
				if replayPending {
					_ = service.sendRememberedPowerLocked(deviceContext, target.id, target.driver, generation, true)
				}
				unlockDevice()
			} else {
				service.setTransferError(target.id, lockErr)
			}
			service.mu.Lock()
			delete(service.initialPowerReplay, target.id)
			service.mu.Unlock()
			// 不能使用收集 targets 时的旧快照：启动重放等待门闩期间，
			// 用户可能已经恢复开屏。按最新意图决定是否唤醒主机帧，
			// 避免 UI 显示已开屏但 worker 永久停在 suspended。
			if latestPoweredOff, _ := service.powerIntent(target.id); !latestPoweredOff {
				service.resumeHostFrameDelivery(target.id)
			}
		}()
	}
	wait.Wait()
}

func replaceDuration(channel chan time.Duration, interval time.Duration) {
	select {
	case channel <- interval:
	default:
		select {
		case <-channel:
		default:
		}
		select {
		case channel <- interval:
		default:
		}
	}
}

func frameSignature(state render.State) uint64 {
	hasher := fnv.New64a()
	var data [8]byte
	writeUint64 := func(value uint64) {
		binary.LittleEndian.PutUint64(data[:], value)
		_, _ = hasher.Write(data[:])
	}
	writeUint64(uint64(state.Telemetry.CapturedAt.UnixNano()))
	if state.TelemetryEnabled {
		writeUint64(1)
	} else {
		writeUint64(0)
	}
	writeUint64(uint64(math.Round(state.Telemetry.CPUPercent * 10)))
	writeUint64(uint64(math.Round(state.Telemetry.MemoryPercent * 10)))
	writeUint64(state.VideoSequence)
	// 频带数值量化后可能连续相同，但新的采样时间仍会推进平滑和峰值衰减。
	writeUint64(uint64(state.Spectrum.CapturedAt.UnixNano()))
	writeUint64(uint64(math.Round(state.Spectrum.Level * 255)))
	for _, value := range state.Spectrum.Bars {
		writeUint64(uint64(math.Round(math.Max(0, math.Min(1, value)) * 255)))
	}
	_, _ = hasher.Write([]byte(state.Lyrics.Current))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(state.Lyrics.Next))
	writeUint64(uint64(math.Round(math.Max(0, math.Min(1, state.Lyrics.Progress)) * 200)))
	return hasher.Sum64()
}

func (service *Service) closeAll() {
	service.cancelAllOrientationCalibrations()
	service.mu.Lock()
	service.driversClosing = true
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.Unlock()
	for _, driver := range drivers {
		_ = driver.Close()
	}
}

func usesHostFrames(driver device.Driver, manifest theme.Manifest) bool {
	if _, ok := driver.(device.HostFrameDriver); !ok {
		return false
	}
	if hybrid, ok := driver.(device.HybridFrameDriver); ok {
		return hybrid.UseHostFrames(manifest)
	}
	return true
}

func applyDriverTheme(ctx context.Context, driver device.Driver, manifest theme.Manifest, hostFrames bool) error {
	if hostFrames {
		if hybrid, ok := driver.(device.HybridFrameDriver); ok {
			return hybrid.ApplyHostTheme(ctx, manifest)
		}
	}
	return driver.ApplyTheme(ctx, manifest)
}
