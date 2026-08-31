package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/discovery"
	"casescreen/internal/gpupreference"
	"casescreen/internal/platformauth"
	"casescreen/internal/service"
	"casescreen/internal/theme"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// ProtocolVersion 标识桌面工作台与本机服务之间的控制 API 契约。
// 新工作台依赖旧服务无法处理的能力时必须递增，避免误连仍占用端口的旧后台。
const ProtocolVersion = 8

// DesktopTakeoverHeader 只用于同版本 Desktop 把用户预先启动的同目录
// tray/headless 服务平滑转为桌面托管模式。请求仍必须携带当前
// 进程会话令牌，普通桌面退出请求不使用此头。
const (
	DesktopTakeoverHeader = "X-CaseScreen-Desktop-Takeover"
	DesktopTakeoverValue  = "1"
)

type Controller interface {
	Status() service.Status
	SetTelemetryEnabled(bool)
	SetAudioEnabled(bool)
	SetLyricsEnabled(bool)
	SetInterval(time.Duration)
	SetRenderInterval(time.Duration)
	SetJPEGQuality(int)
	SetFFmpegPath(string)
	ApplyThemeTo(context.Context, string, theme.Manifest) map[string]error
	SetPoweredOff(context.Context, string, bool) map[string]error
}

type Server struct {
	controller             Controller
	store                  *config.Store
	dataDirectory          string
	settingsMu             sync.Mutex
	powerMu                sync.Mutex
	templateMu             sync.Mutex
	themeTargetLocks       sync.Map
	coolerCalibrationMu    sync.Mutex
	coolerCalibrationLocks map[string]*coolerCalibrationTargetLock
	playlistWake           chan struct{}
	randomIndex            func(int) int
	playlistStatusMu       sync.RWMutex
	playlistStatuses       map[string]wavesharePlaylistStatus
	prepareTheme           func(context.Context, theme.Manifest) (theme.Manifest, error)
	updateTemplateConfig   func(func(*config.Config) error) error
	resourceMu             sync.RWMutex
	resources              map[string]theme.Resource
	platformAuth           *platformauth.Store
	gpuPreferences         gpuPreferenceManager
	telemetryGPUs          telemetryGPUManager
	applicationVersion     string
	processLifecycle       ProcessLifecycle
	shutdownProcess        context.CancelFunc
	operationMu            sync.RWMutex
	operationContext       context.Context
	token                  string
	handler                http.Handler
	discover               func() []discovery.Device
	replaceThemeFile       func(string, string) error
}

// ProcessLifecycle 描述 CaseScreen 服务由谁启动。桌面工作台正常退出只会
// 结束 Mode=desktop 的托管服务；tray/headless 只在同版 Desktop 显式接管时退出。
type ProcessLifecycle struct {
	Mode                string `json:"launch_mode,omitempty"`
	ProcessID           int    `json:"process_id,omitempty"`
	OwnerProcessID      int    `json:"owner_process_id,omitempty"`
	OwnerProcessStarted uint64 `json:"owner_process_started,omitempty"`
}

func New(controller Controller, store *config.Store, suppliedSessions ...*platformauth.Store) (*Server, error) {
	if controller == nil || store == nil {
		return nil, errors.New("控制器和配置存储不能为空")
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("生成本地控制令牌失败: %w", err)
	}
	dataDirectory, err := resolveDataDirectory(filepath.Dir(store.Path()))
	if err != nil {
		return nil, err
	}
	var platformSessions *platformauth.Store
	if len(suppliedSessions) > 0 && suppliedSessions[0] != nil {
		platformSessions = suppliedSessions[0]
	} else {
		platformSessions, err = platformauth.Open(filepath.Join(dataDirectory, "platform-auth"))
		if err != nil {
			return nil, fmt.Errorf("打开音乐平台登录态失败: %w", err)
		}
	}
	server := &Server{
		controller: controller, store: store, dataDirectory: dataDirectory,
		resources: make(map[string]theme.Resource), platformAuth: platformSessions,
		token: hex.EncodeToString(tokenBytes), discover: discovery.Find,
		playlistWake: make(chan struct{}, 1), randomIndex: secureRandomIndex,
		playlistStatuses:       make(map[string]wavesharePlaylistStatus),
		coolerCalibrationLocks: make(map[string]*coolerCalibrationTargetLock),
		gpuPreferences:         newGPUPreferenceManager(),
		replaceThemeFile:       replaceThemeFile,
		updateTemplateConfig:   store.Update,
	}
	if manager, ok := controller.(telemetryGPUManager); ok {
		server.telemetryGPUs = manager
	}
	server.prepareTheme = server.prepareWaveshareTheme
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", server.index)
	mux.HandleFunc("GET /api/health", server.health)
	mux.HandleFunc("GET /api/session", server.session)
	mux.HandleFunc("POST /api/lifecycle/shutdown", server.shutdownDesktopProcess)
	mux.HandleFunc("GET /api/status", server.status)
	mux.HandleFunc("GET /api/config", server.getConfig)
	mux.HandleFunc("GET /api/gpu-preference", server.getGPUPreference)
	mux.HandleFunc("PUT /api/gpu-preference", server.putGPUPreference)
	mux.HandleFunc("GET /api/telemetry-gpu", server.getTelemetryGPU)
	mux.HandleFunc("PUT /api/telemetry-gpu", server.putTelemetryGPU)
	mux.HandleFunc("GET /api/theme", server.getTheme)
	mux.HandleFunc("GET /api/theme/resource", server.getThemeResource)
	mux.HandleFunc("POST /api/theme/resource", server.uploadThemeResource)
	mux.HandleFunc("GET /api/discovery", server.getDiscovery)
	mux.HandleFunc("GET /api/platform-auth", server.getPlatformAuth)
	mux.HandleFunc("POST /api/platform-auth", server.importPlatformAuth)
	mux.HandleFunc("DELETE /api/platform-auth", server.clearPlatformAuth)
	mux.HandleFunc("PUT /api/config", server.putConfig)
	mux.HandleFunc("POST /api/telemetry", server.setTelemetry)
	mux.HandleFunc("PUT /api/power", server.putPower)
	mux.HandleFunc("PUT /api/theme", server.putTheme)
	mux.HandleFunc("GET /api/waveshare/templates", server.getWaveshareTemplates)
	mux.HandleFunc("PUT /api/waveshare/templates", server.putWaveshareTemplate)
	mux.HandleFunc("PATCH /api/waveshare/templates", server.renameWaveshareTemplate)
	mux.HandleFunc("POST /api/waveshare/templates/activate", server.activateWaveshareTemplate)
	mux.HandleFunc("DELETE /api/waveshare/templates", server.deleteWaveshareTemplate)
	mux.HandleFunc("GET /api/waveshare/playlist", server.getWavesharePlaylist)
	mux.HandleFunc("PUT /api/waveshare/playlist", server.putWavesharePlaylist)
	mux.HandleFunc("POST /api/waveshare/storage/refresh", server.refreshWaveshareStorage)
	mux.HandleFunc("GET /api/coolermaster/calibration", server.getCoolerMasterCalibration)
	mux.HandleFunc("POST /api/coolermaster/calibration", server.showCoolerMasterCalibration)
	mux.HandleFunc("PUT /api/coolermaster/calibration", server.saveCoolerMasterCalibration)
	mux.HandleFunc("DELETE /api/coolermaster/calibration", server.cancelCoolerMasterCalibration)
	server.handler = securityHeaders(mux)
	return server, nil
}

// resolveDataDirectory 为主题、资源和日志等运行时数据选择可写目录。
// 某些 Windows 安全策略允许读取 Roaming，却禁止应用在其中创建子目录；
// 此时优先回退到程序目录，再回退到系统临时目录，避免上传功能直接失败。
func resolveDataDirectory(preferred string) (string, error) {
	candidates := make([]string, 0, 3)
	if preferred != "" {
		candidates = append(candidates, preferred)
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "CaseScreen-data"))
	}
	if temporaryDirectory := os.TempDir(); temporaryDirectory != "" {
		candidates = append(candidates, filepath.Join(temporaryDirectory, "CaseScreen"))
	}
	seen := make(map[string]struct{}, len(candidates))
	var failures []string
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if err := ensureWritableDirectory(candidate); err != nil {
			failures = append(failures, candidate+": "+err.Error())
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("找不到可写的 CaseScreen 数据目录: %s", strings.Join(failures, "；"))
}

func ensureWritableDirectory(directory string) error {
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

func (server *Server) Handler() http.Handler {
	return server.handler
}

// SetApplicationVersion 记录启动当前服务的正式应用版本，供桌面工作台
// 拒绝复用同协议号但实现不同的旧后台。版本不参与 USB 小包，也不会发往副屏。
func (server *Server) SetApplicationVersion(version string) {
	server.applicationVersion = strings.TrimSpace(version)
}

// SetProcessLifecycle 在启动监听前登记服务进程的归属与退出回调。
// 桌面关闭接口只对 desktop 模式开放，手动托盘和手动 headless 模式保持常驻。
func (server *Server) SetProcessLifecycle(lifecycle ProcessLifecycle, shutdown context.CancelFunc) {
	server.processLifecycle = lifecycle
	server.shutdownProcess = shutdown
}

func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	server.operationMu.Lock()
	server.operationContext = ctx
	server.operationMu.Unlock()
	httpServer := &http.Server{
		Handler:           server.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// 主题 JSON 很小，但处理阶段可能需要通过 USB 上传大型 MP4。
		WriteTimeout: 20 * time.Minute,
		IdleTimeout:  30 * time.Second,
	}
	shutdownDone := make(chan struct{})
	playlistContext, stopPlaylist := context.WithCancel(ctx)
	playlistDone := make(chan struct{})
	go func() {
		defer close(playlistDone)
		server.runWavesharePlaylistScheduler(playlistContext)
	}()
	defer func() {
		stopPlaylist()
		<-playlistDone
	}()
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = httpServer.Shutdown(shutdownContext)
			cancel()
		case <-shutdownDone:
		}
	}()
	err := httpServer.Serve(listener)
	close(shutdownDone)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// detachedOperationContext 忽略单个 HTTP 客户端的断开，但保留服务退出信号。
// Handler 单元测试没有调用 Serve 时，仍只使用调用方的超时上下文。
func (server *Server) detachedOperationContext(requestContext context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(requestContext)
	timedContext, stopTimeout := context.WithTimeout(base, timeout)
	server.operationMu.RLock()
	serviceContext := server.operationContext
	server.operationMu.RUnlock()
	if serviceContext == nil {
		return timedContext, stopTimeout
	}
	operationContext, cancel := context.WithCancel(timedContext)
	stopServiceCancellation := context.AfterFunc(serviceContext, cancel)
	return operationContext, func() {
		stopServiceCancellation()
		cancel()
		stopTimeout()
	}
}

func (server *Server) index(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(writer, strings.ReplaceAll(indexHTML, "__CASESCREEN_TOKEN__", server.token))
}

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "service": "CaseScreen"})
}

// session 只在本机监听地址上提供短期桌面客户端所需的令牌；服务本身不接受远程连接。
func (server *Server) session(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"token":                 server.token,
		"protocol_version":      ProtocolVersion,
		"application_version":   server.applicationVersion,
		"launch_mode":           server.processLifecycle.Mode,
		"process_id":            server.processLifecycle.ProcessID,
		"owner_process_id":      server.processLifecycle.OwnerProcessID,
		"owner_process_started": server.processLifecycle.OwnerProcessStarted,
	})
}

func (server *Server) shutdownDesktopProcess(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	takeover := request.Header.Get(DesktopTakeoverHeader) == DesktopTakeoverValue
	manualTakeover := takeover && (server.processLifecycle.Mode == "tray" || server.processLifecycle.Mode == "headless")
	if (server.processLifecycle.Mode != "desktop" && !manualTakeover) || server.shutdownProcess == nil {
		writeError(writer, http.StatusConflict, "当前服务不是桌面工作台托管模式；请从托盘或原启动终端退出")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"shutting_down": true})
	// 先把成功响应交给桌面端，再取消服务上下文，避免正常退出被误判为超时强杀。
	go server.shutdownProcess()
}

func (server *Server) status(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, server.controller.Status())
}

func (server *Server) getConfig(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, server.store.Get())
}

func (server *Server) getTheme(writer http.ResponseWriter, request *http.Request) {
	manifest, err := server.loadThemeForTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, manifest)
}

func (server *Server) loadThemeForTarget(target string) (theme.Manifest, error) {
	deviceID := strings.TrimSpace(target)
	if deviceID == "" || deviceID == "all" {
		return theme.Manifest{}, errors.New("必须指定目标副屏")
	}
	cfg := server.store.Get()
	path := ""
	if cfg.DeviceThemePaths != nil {
		path = cfg.DeviceThemePaths[deviceID]
	}
	if path == "" {
		return theme.Manifest{}, fmt.Errorf("副屏 %q 尚未保存主题", deviceID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return theme.Manifest{}, fmt.Errorf("读取当前主题失败: %w", err)
	}
	var manifest theme.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return theme.Manifest{}, fmt.Errorf("解析当前主题失败: %w", err)
	}
	return manifest, nil
}

func (server *Server) getThemeResource(writer http.ResponseWriter, request *http.Request) {
	resourceID := strings.TrimSpace(request.URL.Query().Get("id"))
	if resourceID == "" {
		writeError(writer, http.StatusBadRequest, "缺少资源 ID")
		return
	}
	resource, ok := server.uploadedResource(resourceID)
	if !ok {
		manifest, err := server.loadThemeForTarget(request.URL.Query().Get("target"))
		if err != nil {
			writeError(writer, http.StatusNotFound, err.Error())
			return
		}
		for _, candidate := range manifest.Resources {
			if candidate.ID == resourceID {
				resource = candidate
				ok = true
				break
			}
		}
	}
	if !ok {
		writeError(writer, http.StatusNotFound, "未找到主题资源")
		return
	}
	if !isPreviewableResource(resource.LocalPath) {
		writeError(writer, http.StatusBadRequest, "该资源不是可预览的图片")
		return
	}
	if request.URL.Query().Get("preview") == "1" {
		servePreviewImage(writer, resource.LocalPath)
		return
	}
	http.ServeFile(writer, request, resource.LocalPath)
}

func (server *Server) uploadedResource(resourceID string) (theme.Resource, bool) {
	server.resourceMu.RLock()
	defer server.resourceMu.RUnlock()
	resource, ok := server.resources[resourceID]
	return resource, ok
}

func servePreviewImage(writer http.ResponseWriter, path string) {
	file, err := os.Open(path)
	if err != nil {
		writeError(writer, http.StatusNotFound, "打开背景图片失败: "+err.Error())
		return
	}
	defer file.Close()
	decoded, _, err := image.Decode(file)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "解码背景图片失败: "+err.Error())
		return
	}
	writer.Header().Set("Content-Type", "image/png")
	if err := png.Encode(writer, decoded); err != nil {
		return
	}
}

func isPreviewableResource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp":
		return true
	default:
		return false
	}
}

func (server *Server) uploadThemeResource(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	const maxResourceSize = 128 << 20
	if err := request.ParseMultipartForm(maxResourceSize + 1<<20); err != nil {
		writeError(writer, http.StatusBadRequest, "背景资源上传失败: "+err.Error())
		return
	}
	file, header, err := request.FormFile("file")
	if err != nil {
		writeError(writer, http.StatusBadRequest, "请选择背景图片或视频文件")
		return
	}
	defer file.Close()
	name := filepath.Base(header.Filename)
	extension := strings.ToLower(filepath.Ext(name))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".bmp": true, ".mp4": true}
	if !allowed[extension] {
		writeError(writer, http.StatusBadRequest, "只支持 JPG、PNG、WEBP、BMP 或 MP4 背景")
		return
	}
	if name == "" || name == "." {
		name = "background" + extension
	}
	directory := filepath.Join(server.dataDirectory, "theme-assets")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		writeError(writer, http.StatusInternalServerError, "创建背景资源目录失败: "+err.Error())
		return
	}
	path := filepath.Join(directory, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "创建背景资源文件失败: "+err.Error())
		return
	}
	written, copyErr := io.Copy(output, io.LimitReader(file, maxResourceSize+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		if copyErr == nil {
			copyErr = closeErr
		}
		writeError(writer, http.StatusInternalServerError, "保存背景资源失败: "+copyErr.Error())
		return
	}
	if written > maxResourceSize {
		_ = os.Remove(path)
		writeError(writer, http.StatusRequestEntityTooLarge, "背景资源不能超过 128 MiB")
		return
	}
	remoteDirectory := "/data/image/"
	if extension == ".mp4" {
		remoteDirectory = "/data/video/"
	}
	resource, err := theme.InspectResource(filepath.Base(path), path, remoteDirectory+name)
	if err != nil {
		_ = os.Remove(path)
		writeError(writer, http.StatusInternalServerError, "读取背景资源信息失败: "+err.Error())
		return
	}
	server.resourceMu.Lock()
	server.resources[resource.ID] = resource
	server.resourceMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"resource": resource})
}

func (server *Server) getDiscovery(writer http.ResponseWriter, _ *http.Request) {
	devices := preserveDiscoveredLogicalDevices(server.discover(), server.store.Get())
	writeJSON(writer, http.StatusOK, map[string]any{"devices": devices})
}

// preserveDiscoveredLogicalDevices 让设置页的手动“扫描”与运行期热插拔使用
// 同一逻辑身份。微雪原始发现 ID 包含 COM 号；直接回写它会把
// waveshare-7 变成 waveshare-com3，从而遗失主题、模板和轮播绑定。
func preserveDiscoveredLogicalDevices(discovered []discovery.Device, cfg config.Config) []discovery.Device {
	byWavesharePort := make(map[string]config.Device)
	var configuredCooler *config.Device
	for index := range cfg.Devices {
		configured := cfg.Devices[index]
		switch configured.Kind {
		case "waveshare":
			port := strings.ToUpper(strings.TrimSpace(configured.Port))
			if port != "" {
				byWavesharePort[port] = configured
			}
		case "coolermaster":
			if configuredCooler == nil {
				copied := configured
				configuredCooler = &copied
			}
		}
	}
	result := append([]discovery.Device(nil), discovered...)
	for index := range result {
		candidate := &result[index]
		var configured *config.Device
		switch candidate.Kind {
		case "waveshare":
			if value, exists := byWavesharePort[strings.ToUpper(strings.TrimSpace(candidate.Port))]; exists {
				copied := value
				configured = &copied
			}
		case "coolermaster":
			configured = configuredCooler
		}
		if configured == nil {
			continue
		}
		candidate.ID = configured.ID
		candidate.Enabled = configured.Enabled
		if strings.TrimSpace(configured.Name) != "" {
			candidate.Name = configured.Name
		}
		if configured.Width > 0 && configured.Height > 0 {
			candidate.Width, candidate.Height = configured.Width, configured.Height
		}
		candidate.LibraryPath = configured.LibraryPath
		if configured.Kind == "coolermaster" {
			slot := configured.Slot
			candidate.Slot = &slot
		}
	}
	return result
}

func (server *Server) putConfig(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	var cfg config.Config
	if err := decodeJSON(writer, request, &cfg); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.settingsMu.Lock()
	defer server.settingsMu.Unlock()
	previous := server.store.Get()
	// 监控 GPU 由 /api/telemetry-gpu 专用端点独占管理。旧设置页回写
	// 完整配置时不得用缺失或陈旧字段覆盖刚刚保存的监控目标。
	cfg.TelemetryGPUAdapterID = previous.TelemetryGPUAdapterID
	cfg.TelemetryGPUAdapterName = previous.TelemetryGPUAdapterName
	// 模板库、轮播和当前主题路径由专用 API 管理。设置页会读取完整配置后再回写，
	// 因此即使请求带着非空旧快照也必须保留服务端最新值，避免并发保存设置时丢失主题。
	cfg.DeviceThemePaths = previous.DeviceThemePaths
	cfg.WaveshareTemplates = previous.WaveshareTemplates
	cfg.WavesharePlaylists = previous.WavesharePlaylists
	if err := cfg.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	gpuPreferenceChanged := previous.GPUPreference != cfg.GPUPreference ||
		previous.GPUAdapterID != cfg.GPUAdapterID || previous.GPUAdapterName != cfg.GPUAdapterName
	if gpuPreferenceChanged {
		var result gpupreference.ApplyResult
		var err error
		if strings.TrimSpace(cfg.GPUAdapterID) != "" {
			result, err = server.gpuPreferences.ApplyAdapter(cfg.GPUAdapterID)
			if err == nil {
				cfg.GPUPreference = string(result.Adapter.Preference)
				cfg.GPUAdapterID = result.Adapter.ID
				cfg.GPUAdapterName = result.Adapter.Name
			}
		} else {
			result, err = server.gpuPreferences.Apply(gpupreference.Preference(cfg.GPUPreference))
		}
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "应用 Windows GPU 首选项失败: "+err.Error())
			return
		}
	}
	if err := server.store.Update(func(current *config.Config) error {
		cfg.DeviceThemePaths = current.DeviceThemePaths
		cfg.WaveshareTemplates = current.WaveshareTemplates
		cfg.WavesharePlaylists = current.WavesharePlaylists
		// 关屏意图由 /api/power 独占管理。设置页可能回写关屏前的
		// 陈旧快照，必须在同一 Store.Update 临界区按 ID 合并最新值。
		powerByID := make(map[string]bool, len(current.Devices))
		for _, configured := range current.Devices {
			powerByID[configured.ID] = configured.PoweredOff
		}
		for index := range cfg.Devices {
			if poweredOff, exists := powerByID[cfg.Devices[index].ID]; exists {
				cfg.Devices[index].PoweredOff = poweredOff
			}
		}
		*current = cfg
		return nil
	}); err != nil {
		if gpuPreferenceChanged {
			_, rollbackErr := server.gpuPreferences.Apply(gpupreference.Preference(previous.GPUPreference))
			if rollbackErr != nil {
				writeError(writer, http.StatusInternalServerError, err.Error()+"；回滚 Windows GPU 首选项失败: "+rollbackErr.Error())
				return
			}
		}
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	// Update 内部会合并 /api/power 刚持久化的最新意图。重新读取后再
	// 应用运行参数并回包，避免响应仍把调用方提交的陈旧 powered_off 返回给 UI。
	cfg = server.store.Get()
	server.controller.SetTelemetryEnabled(cfg.TelemetryEnabled)
	server.controller.SetAudioEnabled(cfg.AudioEnabled)
	server.controller.SetLyricsEnabled(cfg.LyricsEnabled)
	if configurable, ok := server.controller.(interface{ SetLyricsProviderPreference(string) }); ok {
		configurable.SetLyricsProviderPreference(cfg.LyricsProvider)
	}
	if configurable, ok := server.controller.(interface{ SetLyricsPath(string) }); ok {
		configurable.SetLyricsPath(cfg.LyricsPath)
	}
	server.controller.SetInterval(time.Duration(cfg.TelemetryIntervalM) * time.Millisecond)
	server.controller.SetRenderInterval(time.Second / time.Duration(cfg.HostRenderFPS))
	server.controller.SetJPEGQuality(cfg.JPEGQuality)
	server.controller.SetFFmpegPath(cfg.FFmpegPath)
	writeJSON(writer, http.StatusOK, map[string]any{
		"config":           cfg,
		"restart_required": configNeedsRestart(previous, cfg),
	})
}

func (server *Server) putTheme(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	var manifest theme.Manifest
	if err := decodeJSONLimit(writer, request, &manifest, 2<<20); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	target := strings.TrimSpace(request.URL.Query().Get("target"))
	// 资源上传不跟随浏览器短连接取消，但必须跟随服务生命周期退出；
	// 否则长转换/USB 操作可能在驱动和渲染器关闭后继续提交资源。
	operationContext, cancel := server.detachedOperationContext(request.Context(), 15*time.Minute)
	defer cancel()
	resolution, err := server.targetResolution(target)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	manifest.Width, manifest.Height = resolution[0], resolution[1]
	manifest = theme.NormalizeScene(manifest)
	if err := manifest.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err := absolutizeManifestResources(&manifest); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	configured := server.store.Get()
	isWaveshare := isConfiguredWaveshareTarget(configured, target)
	isCoolerMaster := isConfiguredCoolerMasterTarget(configured, target)
	if isWaveshare {
		server.templateMu.Lock()
		defer server.templateMu.Unlock()
	}
	// 微雪模板 API 已统一使用 templateMu；固定先取 templateMu、再取
	// 单目标事务锁，避免与模板保存/激活形成反向加锁。酷冷不需要模板锁，
	// 但同一目标的应用、主题文件和配置仍必须作为一个整体串行提交。
	unlockTarget := server.lockThemeTarget(target)
	defer unlockTarget()
	if isCoolerMaster {
		unlockCalibration, lockErr := server.lockCoolerCalibrationTarget(operationContext, target)
		if lockErr != nil {
			writeError(writer, http.StatusGatewayTimeout, "等待酷冷至尊方向操作失败: "+lockErr.Error())
			return
		}
		defer unlockCalibration()
		if server.coolerCalibrationActive(target) {
			writeError(writer, http.StatusConflict, "方向测试图正在显示；请先确认方向或取消测试")
			return
		}
	}
	if isWaveshare {
		manifest, err = server.ensureWaveshareBaseThemeResource(manifest)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err.Error())
			return
		}
	}
	preparedManifest, prepareErr := server.prepareTheme(operationContext, manifest)
	if prepareErr != nil {
		writeError(writer, http.StatusBadRequest, "微雪主题背景转换失败: "+prepareErr.Error())
		return
	}
	manifest = preparedManifest
	if isWaveshare {
		clearWaveshareEmbeddedResourceRemotePaths(&manifest)
	}
	// 先原子保存主题和配置，再触发硬件应用。这样主题文件写入失败时
	// 不会把硬件切到一个磁盘上不存在、重启后也无法恢复的主题。
	themePath, err := server.themePathFor(target)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	previousFile, err := snapshotThemeFile(themePath)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "读取原主题文件失败: "+err.Error())
		return
	}
	path, err := server.persistThemeFor(target, manifest)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	if err := server.store.Update(func(cfg *config.Config) error {
		if cfg.DeviceThemePaths == nil {
			cfg.DeviceThemePaths = make(map[string]string)
		}
		cfg.DeviceThemePaths[target] = path
		return nil
	}); err != nil {
		rollbackErr := server.restoreThemeFile(themePath, previousFile)
		if rollbackErr != nil {
			writeError(writer, http.StatusInternalServerError, err.Error()+"；恢复原主题文件失败: "+rollbackErr.Error())
			return
		}
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	results := server.controller.ApplyThemeTo(operationContext, target, manifest)
	formatted := make(map[string]string, len(results))
	successes := 0
	for id, err := range results {
		if err == nil {
			formatted[id] = "ok"
			successes++
		} else {
			formatted[id] = err.Error()
		}
	}
	responseBody := map[string]any{
		"theme_id":       manifest.ID,
		"device_results": formatted,
		"successes":      successes,
		"queued":         false,
	}
	if len(results) > 0 && successes == 0 {
		// ApplyThemeTo 已把失败清单登记为待重试主题，persistThemeFor 也已经
		// 原子保存到磁盘。此时请求不是“保存失败”，而是已经接受、等待设备
		// 恢复；使用 202 让工作台保留结果详情，同时避免把成功持久化误报成 502。
		responseBody["queued"] = true
		responseBody["warning"] = "主题已保存；设备本次未完成应用，将在重连后自动重试"
		writeJSON(writer, http.StatusAccepted, responseBody)
		return
	}
	writeJSON(writer, http.StatusOK, responseBody)
}

func (server *Server) lockThemeTarget(target string) func() {
	value, _ := server.themeTargetLocks.LoadOrStore(strings.TrimSpace(target), &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (server *Server) targetResolution(target string) ([2]int, error) {
	target = strings.TrimSpace(target)
	if target == "" || target == "all" {
		return [2]int{}, errors.New("请选择一台目标副屏；不同副屏不能共用同一主题")
	}
	for _, status := range server.controller.Status().Devices {
		if status.Info.ID != target {
			continue
		}
		resolution := status.Info.Resolution
		if resolution[0] <= 0 || resolution[1] <= 0 {
			return [2]int{}, fmt.Errorf("目标副屏 %q 的分辨率无效", target)
		}
		return resolution, nil
	}
	return [2]int{}, fmt.Errorf("未找到目标副屏 %q", target)
}

// normalizeBackgroundLayers 保留给旧调用点；实际迁移为画布底色和独立图片图层。
func normalizeBackgroundLayers(manifest *theme.Manifest) {
	*manifest = theme.NormalizeScene(*manifest)
}

func (server *Server) setTelemetry(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.settingsMu.Lock()
	defer server.settingsMu.Unlock()
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.TelemetryEnabled = body.Enabled
		return nil
	}); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	server.controller.SetTelemetryEnabled(body.Enabled)
	writeJSON(writer, http.StatusOK, map[string]bool{"telemetry_enabled": body.Enabled})
}

// putPower 接受单屏 ID 或 all。亮度命令的逐台结果会完整返回；
// 部分失败使用 207，工作台仍能解码成功和失败设备。
func (server *Server) putPower(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	var body struct {
		Target     string `json:"target"`
		PoweredOff *bool  `json:"powered_off"`
	}
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	body.Target = strings.TrimSpace(body.Target)
	if body.Target == "" {
		writeError(writer, http.StatusBadRequest, "请选择单台副屏或全部副屏")
		return
	}
	if body.PoweredOff == nil {
		writeError(writer, http.StatusBadRequest, "缺少 powered_off 布尔值")
		return
	}
	poweredOff := *body.PoweredOff
	// 持久化与运行时登记必须是同一个有序事务。否则并发的 off/on
	// 请求可能交错成“配置为 on、硬件最后执行 off”。这里不与设置页
	// 形成反向加锁；persistPowerIntent 内部仍只短暂持有 settingsMu。
	server.powerMu.Lock()
	defer server.powerMu.Unlock()
	powerTargets, err := server.powerTargetIDs(body.Target)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	// 先原子保存意图，再访问 USB。进程在硬件命令期间崩溃时，
	// 下次启动仍会重放用户最后一次选择。
	if err := server.persistPowerIntent(powerTargets, poweredOff); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	operationContext, cancel := server.detachedOperationContext(request.Context(), 30*time.Second)
	defer cancel()
	results := server.controller.SetPoweredOff(operationContext, body.Target, poweredOff)
	formatted := make(map[string]string, len(results))
	successes, failures := 0, 0
	for id, err := range results {
		if err == nil {
			formatted[id] = "ok"
			successes++
			continue
		}
		formatted[id] = err.Error()
		failures++
	}
	statusCode := http.StatusOK
	if failures > 0 {
		statusCode = http.StatusMultiStatus
	}
	writeJSON(writer, statusCode, map[string]any{
		"target": body.Target, "powered_off": poweredOff,
		"device_results": formatted, "successes": successes, "failures": failures,
	})
}

func (server *Server) powerTargetIDs(target string) ([]string, error) {
	statuses := server.controller.Status().Devices
	if target != "all" {
		for _, status := range statuses {
			if status.Info.ID != target {
				continue
			}
			if !status.BrightnessSupported {
				return nil, fmt.Errorf("副屏 %q 不支持已确认的亮度关屏命令", target)
			}
			return []string{target}, nil
		}
		return nil, fmt.Errorf("未找到目标副屏 %q", target)
	}
	ids := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status.BrightnessSupported {
			ids = append(ids, status.Info.ID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("没有支持亮度关屏命令的副屏")
	}
	return ids, nil
}

func (server *Server) persistPowerIntent(targets []string, poweredOff bool) error {
	wanted := make(map[string]bool, len(targets))
	for _, id := range targets {
		wanted[id] = true
	}
	server.settingsMu.Lock()
	defer server.settingsMu.Unlock()
	return server.store.Update(func(cfg *config.Config) error {
		found := make(map[string]bool, len(wanted))
		for index := range cfg.Devices {
			if !wanted[cfg.Devices[index].ID] {
				continue
			}
			cfg.Devices[index].PoweredOff = poweredOff
			found[cfg.Devices[index].ID] = true
		}
		missing := make([]string, 0)
		for id := range wanted {
			if !found[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("配置中未找到副屏: %s", strings.Join(missing, "、"))
		}
		return nil
	})
}

func (server *Server) authorized(request *http.Request) bool {
	contentType := request.Header.Get("Content-Type")
	return request.Header.Get("X-CaseScreen-Token") == server.token && (strings.HasPrefix(contentType, "application/json") || strings.HasPrefix(contentType, "multipart/form-data"))
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	return decodeJSONLimit(writer, request, target, 64<<10)
}

func decodeJSONLimit(writer http.ResponseWriter, request *http.Request, target any, limit int64) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("JSON 请求无效: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON 请求只能包含一个对象")
	}
	return nil
}

func (server *Server) persistTheme(manifest theme.Manifest) (string, error) {
	path := filepath.Join(server.dataDirectory, "active-theme.json")
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := server.writeThemeFileAtomically(path, data, 0o600); err != nil {
		return "", fmt.Errorf("保存当前主题: %w", err)
	}
	return path, nil
}

func (server *Server) persistThemeFor(deviceID string, manifest theme.Manifest) (string, error) {
	path, err := server.themePathFor(deviceID)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := server.writeThemeFileAtomically(path, data, 0o600); err != nil {
		return "", fmt.Errorf("保存设备主题: %w", err)
	}
	return path, nil
}

func (server *Server) themePathFor(deviceID string) (string, error) {
	cleanID := filepath.Base(deviceID)
	if cleanID == "." || cleanID == string(filepath.Separator) || cleanID == "" {
		return "", errors.New("设备 ID 无效")
	}
	return filepath.Join(server.dataDirectory, "themes", cleanID+".json"), nil
}

type themeFileSnapshot struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

func snapshotThemeFile(path string) (themeFileSnapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return themeFileSnapshot{}, nil
	}
	if err != nil {
		return themeFileSnapshot{}, err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	return themeFileSnapshot{exists: true, data: data, mode: mode}, nil
}

func (server *Server) restoreThemeFile(path string, snapshot themeFileSnapshot) error {
	if snapshot.exists {
		return server.writeThemeFileAtomically(path, snapshot.data, snapshot.mode)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (server *Server) writeThemeFileAtomically(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".theme-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(mode); err != nil {
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
	replace := server.replaceThemeFile
	if replace == nil {
		replace = replaceThemeFile
	}
	if err := replace(temporaryPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func configNeedsRestart(previous, next config.Config) bool {
	return previous.ListenAddress != next.ListenAddress ||
		previous.AudioBands != next.AudioBands ||
		previous.GPUPreference != next.GPUPreference ||
		previous.GPUAdapterID != next.GPUAdapterID ||
		previous.GPUAdapterName != next.GPUAdapterName ||
		!reflect.DeepEqual(previous.Devices, next.Devices)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(writer, request)
	})
}
