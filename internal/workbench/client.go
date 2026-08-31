package workbench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	localapi "casescreen/internal/api"
	"casescreen/internal/config"
	"casescreen/internal/device"
	"casescreen/internal/service"
	"casescreen/internal/theme"
)

// WaveshareTemplateLibrary 是微雪单屏模板库的快照。
type WaveshareTemplateLibrary struct {
	Target           string                     `json:"target"`
	Templates        []config.WaveshareTemplate `json:"templates"`
	ActiveTemplateID string                     `json:"active_template_id,omitempty"`
}

// WavesharePlaylistRuntime 是轮播调度器的当前运行状态。
type WavesharePlaylistRuntime struct {
	Running           bool      `json:"running"`
	CurrentTemplateID string    `json:"current_template_id,omitempty"`
	PendingTemplateID string    `json:"pending_template_id,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	LastAttempt       time.Time `json:"last_attempt,omitempty"`
	NextSwitch        time.Time `json:"next_switch,omitempty"`
	RetryAt           time.Time `json:"retry_at,omitempty"`
}

// PlatformAuthStatus 是 QQ 音乐或网易云音乐的脱敏登录态。
type PlatformAuthStatus struct {
	Platform  string     `json:"platform"`
	State     string     `json:"state"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// PlatformAuthImportResult 是登录态导入后的脱敏摘要。
// 它只包含白名单条目数量与状态，不包含 Cookie 名称或正文。
type PlatformAuthImportResult struct {
	Accepted int                `json:"accepted"`
	Ignored  int                `json:"ignored"`
	Status   PlatformAuthStatus `json:"status"`
}

// ThemeApplyResult 区分“已在设备上生效”和“已保存、等待设备重连”。
// 两种结果都表示主题数据已经被服务接受，不能把后者误报为保存失败。
type ThemeApplyResult struct {
	Results   map[string]string `json:"device_results"`
	Successes int               `json:"successes"`
	Queued    bool              `json:"queued"`
	Warning   string            `json:"warning,omitempty"`
}

// CoolerCalibrationStatus 是酷冷冷头的物理安装方向，不属于主题 JSON。
type CoolerCalibrationStatus struct {
	Target       string `json:"target"`
	Rotation     int    `json:"rotation"`
	Calibrated   bool   `json:"calibrated"`
	Profile      int    `json:"profile"`
	SelectedMark string `json:"selected_mark,omitempty"`
	Queued       bool   `json:"queued"`
	Warning      string `json:"warning,omitempty"`
	Active       bool   `json:"active"`
}

// PowerResult 保留全部关屏/恢复时每台设备的独立结果。
// HTTP 207 仍是可解码的成功响应，界面会逐台展示失败原因。
type PowerResult struct {
	Target     string            `json:"target"`
	PoweredOff bool              `json:"powered_off"`
	Results    map[string]string `json:"device_results"`
	Successes  int               `json:"successes"`
	Failures   int               `json:"failures"`
}

// TelemetryGPUAdapter 是可作为全局 GPU 遥测来源的物理显卡。
// 它与 Windows 逐程序 GPU 调度首选项完全独立。
type TelemetryGPUAdapter struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Active  bool   `json:"active,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// TelemetryGPUStatus 是 GPU 遥测来源的已保存选择与当前生效结果。
// configured 允许暂时不在线的显卡，effective 始终表示本次采集实际使用的显卡。
type TelemetryGPUStatus struct {
	ConfiguredAdapterID   string                `json:"configured_adapter_id"`
	ConfiguredAdapterName string                `json:"configured_adapter_name,omitempty"`
	EffectiveAdapterID    string                `json:"effective_adapter_id,omitempty"`
	EffectiveAdapterName  string                `json:"effective_adapter_name,omitempty"`
	Automatic             bool                  `json:"automatic"`
	Adapters              []TelemetryGPUAdapter `json:"adapters"`
}

// API 是纯 Go 工作台访问本地统一协议服务的轻量客户端。
type API struct {
	BaseURL                    string
	Token                      string
	Client                     *http.Client
	expectedApplicationVersion string
	tokenMu                    sync.RWMutex
	connectMu                  sync.Mutex
}

func NewAPI(baseURL string, expectedApplicationVersions ...string) *API {
	// 容量主动刷新在微雪串口忙于频谱/遥测时允许最多五秒，客户端需要保留
	// 少量 HTTP 编解码余量，避免设备已经回应但界面先行超时。
	expectedApplicationVersion := ""
	if len(expectedApplicationVersions) > 0 {
		expectedApplicationVersion = strings.TrimSpace(expectedApplicationVersions[0])
	}
	return &API{
		BaseURL: strings.TrimRight(baseURL, "/"), Client: &http.Client{Timeout: 8 * time.Second},
		expectedApplicationVersion: expectedApplicationVersion,
	}
}

func (api *API) request(path, method string, body any, auth bool, result any) error {
	return api.requestWithClient(api.Client, path, method, body, auth, result)
}

func (api *API) requestWithClient(client *http.Client, path, method string, body any, auth bool, result any) error {
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = data
	}
	for attempt := 0; attempt < 2; attempt++ {
		token := ""
		if auth {
			if err := api.ensureToken(); err != nil {
				return err
			}
			token = api.currentToken()
		}
		statusCode, responseData, err := api.executeRequest(client, path, method, payload, body != nil, token)
		if err != nil {
			return err
		}
		if auth && statusCode == http.StatusForbidden && attempt == 0 {
			if err := api.refreshRejectedToken(token); err != nil {
				return fmt.Errorf("控制令牌失效且重新连接失败: %w", err)
			}
			continue
		}
		if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
			return serviceResponseError(statusCode, responseData)
		}
		if result != nil && len(responseData) > 0 {
			if err := json.Unmarshal(responseData, result); err != nil {
				return fmt.Errorf("服务返回无效 JSON: %w", err)
			}
		}
		return nil
	}
	return fmt.Errorf("控制令牌重试次数已用尽")
}

func (api *API) executeRequest(client *http.Client, path, method string, payload []byte, hasBody bool, token string) (int, []byte, error) {
	var reader io.Reader
	if hasBody {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequest(method, api.BaseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if hasBody {
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if token != "" {
		request.Header.Set("X-CaseScreen-Token", token)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("无法连接 CaseScreen 服务: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, data, nil
}

func serviceResponseError(statusCode int, data []byte) error {
	var message struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &message)
	if message.Error == "" {
		message.Error = strings.TrimSpace(string(data))
	}
	return fmt.Errorf("服务请求失败 (%d): %s", statusCode, message.Error)
}

func (api *API) currentToken() string {
	api.tokenMu.RLock()
	defer api.tokenMu.RUnlock()
	return api.Token
}

func (api *API) setToken(token string) {
	api.tokenMu.Lock()
	api.Token = token
	api.tokenMu.Unlock()
}

func (api *API) HasToken() bool {
	return api.currentToken() != ""
}

func (api *API) fetchSessionToken() error {
	var session struct {
		Token              string `json:"token"`
		ProtocolVersion    int    `json:"protocol_version"`
		ApplicationVersion string `json:"application_version"`
	}
	if err := api.requestWithClient(api.Client, "/api/session", http.MethodGet, nil, false, &session); err != nil {
		return err
	}
	if session.Token == "" {
		return fmt.Errorf("服务未返回桌面客户端令牌")
	}
	if session.ProtocolVersion != localapi.ProtocolVersion {
		return fmt.Errorf("本机 CaseScreen 服务版本不兼容；请退出旧版后台后重新打开工作台")
	}
	if api.expectedApplicationVersion != "" && session.ApplicationVersion != api.expectedApplicationVersion {
		return fmt.Errorf("本机 CaseScreen 后台不是当前工作台版本；请退出旧版后台后重新打开工作台")
	}
	api.setToken(session.Token)
	return nil
}

func (api *API) ensureToken() error {
	if api.HasToken() {
		return nil
	}
	api.connectMu.Lock()
	defer api.connectMu.Unlock()
	if api.HasToken() {
		return nil
	}
	return api.fetchSessionToken()
}

func (api *API) refreshRejectedToken(rejected string) error {
	api.connectMu.Lock()
	defer api.connectMu.Unlock()
	if current := api.currentToken(); current != "" && current != rejected {
		return nil
	}
	api.setToken("")
	return api.fetchSessionToken()
}

func (api *API) Connect() error {
	api.connectMu.Lock()
	defer api.connectMu.Unlock()
	return api.fetchSessionToken()
}

func (api *API) Status() (service.Status, error) {
	var value service.Status
	err := api.request("/api/status", http.MethodGet, nil, false, &value)
	return value, err
}

// SetPoweredOff 对单屏 ID 或 all 设置关屏意图。
func (api *API) SetPoweredOff(target string, poweredOff bool) (PowerResult, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return PowerResult{}, fmt.Errorf("请选择单台副屏或全部副屏")
	}
	var response PowerResult
	body := struct {
		Target     string `json:"target"`
		PoweredOff bool   `json:"powered_off"`
	}{Target: target, PoweredOff: poweredOff}
	// 全部副屏按设备串行发送，客户端截止时间要略长于
	// 服务端 30 秒上限，避免服务仍在完成命令时界面先报结果未知。
	powerClient := *api.Client
	powerClient.Timeout = 35 * time.Second
	err := api.requestWithClient(&powerClient, "/api/power", http.MethodPut, body, true, &response)
	return response, err
}

func (api *API) Config() (config.Config, error) {
	var value config.Config
	err := api.request("/api/config", http.MethodGet, nil, false, &value)
	return value, err
}

func (api *API) SaveConfig(value config.Config) (bool, error) {
	var response struct {
		RestartRequired bool `json:"restart_required"`
	}
	err := api.request("/api/config", http.MethodPut, value, true, &response)
	return response.RestartRequired, err
}

// GPUPreference 读取两个正式程序当前保存与实际注册的 Windows GPU 首选项。
func (api *API) GPUPreference() (localapi.GPUPreferenceStatus, error) {
	var response localapi.GPUPreferenceStatus
	err := api.request("/api/gpu-preference", http.MethodGet, nil, false, &response)
	return response, err
}

// SaveGPUPreference 同时更新 CaseScreen.exe 与 CaseScreenDesktop.exe 的逐程序 GPU 首选项。
func (api *API) SaveGPUPreference(preference string) (localapi.GPUPreferenceStatus, error) {
	var response localapi.GPUPreferenceStatus
	body := localapi.GPUPreferenceRequest{Preference: strings.TrimSpace(preference)}
	err := api.request("/api/gpu-preference", http.MethodPut, body, true, &response)
	return response, err
}

// SaveGPUAdapter 按工作台中选中的真实适配器更新两个正式程序。
// “系统自动”使用 system 选择值；具体显卡使用枚举得到的稳定设备 ID。
func (api *API) SaveGPUAdapter(adapterID string) (localapi.GPUPreferenceStatus, error) {
	var response localapi.GPUPreferenceStatus
	body := localapi.GPUPreferenceRequest{AdapterID: strings.TrimSpace(adapterID)}
	err := api.request("/api/gpu-preference", http.MethodPut, body, true, &response)
	return response, err
}

// TelemetryGPU 读取全局 GPU 遥测来源，不读取逐程序 GPU 调度设置。
func (api *API) TelemetryGPU() (TelemetryGPUStatus, error) {
	var response TelemetryGPUStatus
	err := api.request("/api/telemetry-gpu", http.MethodGet, nil, false, &response)
	return response, err
}

// SaveTelemetryGPUAdapter 立即更新所有 GPU 指标的全局采集来源。
// adapterID 为 auto 时由后台自动选择物理独显。
func (api *API) SaveTelemetryGPUAdapter(adapterID string) (TelemetryGPUStatus, error) {
	var response TelemetryGPUStatus
	body := struct {
		AdapterID string `json:"adapter_id"`
	}{AdapterID: strings.TrimSpace(adapterID)}
	err := api.request("/api/telemetry-gpu", http.MethodPut, body, true, &response)
	return response, err
}

func (api *API) PlatformAuthStatuses() ([]PlatformAuthStatus, error) {
	var response struct {
		Sessions []PlatformAuthStatus `json:"sessions"`
	}
	err := api.request("/api/platform-auth", http.MethodGet, nil, false, &response)
	return response.Sessions, err
}

// ImportPlatformAuthNetscape 导入浏览器导出的 cookie.txt；服务端响应不会回显正文。
// 文件只是一次导入载体，成功后由主机端加密存储自动复用。
func (api *API) ImportPlatformAuthNetscape(platform, text string) (PlatformAuthImportResult, error) {
	var response PlatformAuthImportResult
	body := struct {
		Platform string `json:"platform"`
		Format   string `json:"format"`
		Text     string `json:"text"`
	}{Platform: platform, Format: "netscape", Text: text}
	err := api.request("/api/platform-auth", http.MethodPost, body, true, &response)
	return response, err
}

func (api *API) ClearPlatformAuth(platform string) (PlatformAuthStatus, error) {
	var response struct {
		Status PlatformAuthStatus `json:"status"`
	}
	path := "/api/platform-auth?platform=" + url.QueryEscape(strings.TrimSpace(platform))
	// 本地控制服务要求变更请求带 JSON Content-Type；空对象不会包含任何凭据。
	err := api.request(path, http.MethodDelete, struct{}{}, true, &response)
	return response.Status, err
}

func (api *API) Theme(target string) (theme.Manifest, error) {
	target = strings.TrimSpace(target)
	if target == "" || target == "all" {
		return theme.Manifest{}, fmt.Errorf("必须指定目标副屏")
	}
	path := "/api/theme?target=" + url.QueryEscape(target)
	var value theme.Manifest
	err := api.request(path, http.MethodGet, nil, false, &value)
	return value, err
}

func (api *API) ApplyTheme(value theme.Manifest, target string) (ThemeApplyResult, error) {
	target = strings.TrimSpace(target)
	if target == "" || target == "all" {
		return ThemeApplyResult{}, fmt.Errorf("必须指定目标副屏")
	}
	path := "/api/theme?target=" + url.QueryEscape(target)
	var response ThemeApplyResult
	// 酷冷至尊离线 MP4/大图需要逐块写入 HID 闪存，不能沿用普通状态接口的短超时。
	applyClient := *api.Client
	applyClient.Timeout = 15 * time.Minute
	err := api.requestWithClient(&applyClient, path, http.MethodPut, value, true, &response)
	return response, err
}

func coolerCalibrationPath(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || target == "all" {
		return "", fmt.Errorf("必须指定一台酷冷至尊副屏")
	}
	return "/api/coolermaster/calibration?target=" + url.QueryEscape(target), nil
}

func (api *API) CoolerCalibration(target string) (CoolerCalibrationStatus, error) {
	path, err := coolerCalibrationPath(target)
	if err != nil {
		return CoolerCalibrationStatus{}, err
	}
	var response CoolerCalibrationStatus
	err = api.request(path, http.MethodGet, nil, false, &response)
	return response, err
}

func (api *API) ShowCoolerCalibration(target string) error {
	path, err := coolerCalibrationPath(target)
	if err != nil {
		return err
	}
	client := *api.Client
	client.Timeout = 35 * time.Second
	return api.requestWithClient(&client, path, http.MethodPost, struct{}{}, true, nil)
}

func (api *API) SaveCoolerCalibration(target, marker string) (CoolerCalibrationStatus, error) {
	path, err := coolerCalibrationPath(target)
	if err != nil {
		return CoolerCalibrationStatus{}, err
	}
	client := *api.Client
	client.Timeout = 15 * time.Minute
	var response CoolerCalibrationStatus
	err = api.requestWithClient(&client, path, http.MethodPut, struct {
		Marker string `json:"marker"`
	}{Marker: strings.TrimSpace(marker)}, true, &response)
	return response, err
}

func (api *API) CancelCoolerCalibration(target string) (CoolerCalibrationStatus, error) {
	path, err := coolerCalibrationPath(target)
	if err != nil {
		return CoolerCalibrationStatus{}, err
	}
	client := *api.Client
	client.Timeout = 15 * time.Minute
	var response CoolerCalibrationStatus
	err = api.requestWithClient(&client, path, http.MethodDelete, struct{}{}, true, &response)
	return response, err
}

func waveshareTargetPath(endpoint, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || target == "all" {
		return "", fmt.Errorf("必须指定一台微雪副屏")
	}
	return endpoint + "?target=" + url.QueryEscape(target), nil
}

// WaveshareTemplates 读取目标屏的本地模板记录。
func (api *API) WaveshareTemplates(target string) (WaveshareTemplateLibrary, error) {
	path, err := waveshareTargetPath("/api/waveshare/templates", target)
	if err != nil {
		return WaveshareTemplateLibrary{}, err
	}
	var value WaveshareTemplateLibrary
	err = api.request(path, http.MethodGet, nil, false, &value)
	return value, err
}

// SaveWaveshareTemplate 把当前完整清单编译一次并持久化。
func (api *API) SaveWaveshareTemplate(target, templateID, name string, manifest theme.Manifest) (config.WaveshareTemplate, error) {
	path, err := waveshareTargetPath("/api/waveshare/templates", target)
	if err != nil {
		return config.WaveshareTemplate{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return config.WaveshareTemplate{}, fmt.Errorf("模板名称不能为空")
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		templateID = "theme-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	payload := struct {
		ID       string         `json:"id"`
		Name     string         `json:"name"`
		Manifest theme.Manifest `json:"manifest"`
	}{ID: templateID, Name: name, Manifest: manifest}
	var response struct {
		Template config.WaveshareTemplate `json:"template"`
	}
	// 编译可能包含视频或大图资源，使用与主题应用一致的长超时。
	requestClient := *api.Client
	requestClient.Timeout = 15 * time.Minute
	err = api.requestWithClient(&requestClient, path, http.MethodPut, payload, true, &response)
	return response.Template, err
}

// RenameWaveshareTemplate 只修改模板库显示名称，不编译主题，也不访问设备。
func (api *API) RenameWaveshareTemplate(target, templateID, name string) (config.WaveshareTemplate, error) {
	path, err := waveshareTargetPath("/api/waveshare/templates", target)
	if err != nil {
		return config.WaveshareTemplate{}, err
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return config.WaveshareTemplate{}, fmt.Errorf("模板 ID 不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return config.WaveshareTemplate{}, fmt.Errorf("模板名称不能为空")
	}
	payload := struct {
		Name string `json:"name"`
	}{Name: name}
	var response struct {
		Template config.WaveshareTemplate `json:"template"`
	}
	err = api.request(path+"&id="+url.QueryEscape(templateID), http.MethodPatch, payload, true, &response)
	return response.Template, err
}

func (api *API) ActivateWaveshareTemplate(target, templateID string) error {
	path, err := waveshareTargetPath("/api/waveshare/templates/activate", target)
	if err != nil {
		return err
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return fmt.Errorf("模板 ID 不能为空")
	}
	path += "&id=" + url.QueryEscape(templateID)
	requestClient := *api.Client
	requestClient.Timeout = 15 * time.Minute
	return api.requestWithClient(&requestClient, path, http.MethodPost, struct{}{}, true, nil)
}

func (api *API) DeleteWaveshareTemplate(target, templateID string) error {
	path, err := waveshareTargetPath("/api/waveshare/templates", target)
	if err != nil {
		return err
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return fmt.Errorf("模板 ID 不能为空")
	}
	return api.request(path+"&id="+url.QueryEscape(templateID), http.MethodDelete, struct{}{}, true, nil)
}

func (api *API) WavesharePlaylist(target string) (config.WavesharePlaylist, error) {
	playlist, _, err := api.WavesharePlaylistState(target)
	return playlist, err
}

// WavesharePlaylistState 同时读取已保存设置和调度器状态。
func (api *API) WavesharePlaylistState(target string) (config.WavesharePlaylist, WavesharePlaylistRuntime, error) {
	path, err := waveshareTargetPath("/api/waveshare/playlist", target)
	if err != nil {
		return config.WavesharePlaylist{}, WavesharePlaylistRuntime{}, err
	}
	var response struct {
		Playlist config.WavesharePlaylist `json:"playlist"`
		Runtime  WavesharePlaylistRuntime `json:"runtime"`
	}
	err = api.request(path, http.MethodGet, nil, false, &response)
	return response.Playlist, response.Runtime, err
}

func (api *API) SaveWavesharePlaylist(target string, playlist config.WavesharePlaylist) error {
	path, err := waveshareTargetPath("/api/waveshare/playlist", target)
	if err != nil {
		return err
	}
	return api.request(path, http.MethodPut, playlist, true, nil)
}

// WaveshareStorage 直接从状态 JSON 解码可选 storage 字段，便于兼容尚未升级的服务。
func (api *API) WaveshareStorage(target string) (*device.StorageInfo, bool, error) {
	if _, err := waveshareTargetPath("", target); err != nil {
		return nil, false, err
	}
	var status struct {
		Devices []struct {
			Info    device.Info         `json:"info"`
			Storage *device.StorageInfo `json:"storage,omitempty"`
		} `json:"devices"`
	}
	if err := api.request("/api/status", http.MethodGet, nil, false, &status); err != nil {
		return nil, false, err
	}
	for _, current := range status.Devices {
		if current.Info.ID == target {
			if current.Storage == nil {
				return nil, false, nil
			}
			copy := *current.Storage
			return &copy, true, nil
		}
	}
	return nil, false, fmt.Errorf("状态中未找到目标副屏 %q", target)
}

// RefreshWaveshareStorage 请求服务立即访问一次目标微雪设备，而不是只读取状态缓存。
func (api *API) RefreshWaveshareStorage(target string) (*device.StorageInfo, error) {
	path, err := waveshareTargetPath("/api/waveshare/storage/refresh", target)
	if err != nil {
		return nil, err
	}
	var response struct {
		Storage device.StorageInfo `json:"storage"`
	}
	// 发送空 JSON 对象，使写操作明确带有 JSON Content-Type 和本地控制令牌。
	if err := api.request(path, http.MethodPost, struct{}{}, true, &response); err != nil {
		return nil, err
	}
	return &response.Storage, nil
}
