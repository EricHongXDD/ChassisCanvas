package api

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/theme"
)

var waveshareLibraryIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type waveshareTemplateSaveRequest struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Manifest theme.Manifest `json:"manifest"`
}

type waveshareTemplateRenameRequest struct {
	Name string `json:"name"`
}

var (
	errWaveshareTemplateNotFound     = errors.New("未找到微雪模板")
	errWaveshareTemplateNameConflict = errors.New("同一副屏内模板名称不能重复")
)

type wavesharePlaylistRuntime struct {
	signature       string
	currentID       string
	index           int
	due             time.Time
	retryTemplateID string
	failures        int
	lastAttempt     time.Time
	retryAt         time.Time
	lastError       string
}

type wavesharePlaylistStatus struct {
	Running           bool      `json:"running"`
	CurrentTemplateID string    `json:"current_template_id,omitempty"`
	PendingTemplateID string    `json:"pending_template_id,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	LastAttempt       time.Time `json:"last_attempt,omitempty"`
	NextSwitch        time.Time `json:"next_switch,omitempty"`
	RetryAt           time.Time `json:"retry_at,omitempty"`
}

const maximumWavesharePlaylistRetryDelay = time.Minute

func isWaveshareTargetID(target string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(target)), "waveshare")
}

func (server *Server) validateWaveshareLibraryTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if !isConfiguredWaveshareTarget(server.store.Get(), target) {
		return "", errors.New("模板库只支持微雪副屏")
	}
	return target, nil
}

func isConfiguredWaveshareTarget(cfg config.Config, target string) bool {
	for _, configured := range cfg.Devices {
		if configured.ID == target {
			return configured.Kind == "waveshare"
		}
	}
	return isWaveshareTargetID(target)
}

func validateWaveshareLibraryID(templateID string) (string, error) {
	templateID = strings.TrimSpace(templateID)
	if !waveshareLibraryIDPattern.MatchString(templateID) {
		return "", errors.New("模板 ID 必须为 1..64 位字母、数字、下划线或连字符，且以字母或数字开头")
	}
	return templateID, nil
}

func (server *Server) getWaveshareTemplates(writer http.ResponseWriter, request *http.Request) {
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	cfg := server.store.Get()
	templates := append([]config.WaveshareTemplate(nil), cfg.WaveshareTemplates[target]...)
	writeJSON(writer, http.StatusOK, map[string]any{
		"target": target, "templates": templates,
		"active_template_id": activeWaveshareTemplateID(cfg, target),
	})
}

func (server *Server) putWaveshareTemplate(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	var body waveshareTemplateSaveRequest
	if err := decodeJSONLimit(writer, request, &body, 4<<20); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	body.ID, err = validateWaveshareLibraryID(body.ID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		body.Name = strings.TrimSpace(body.Manifest.Name)
	}
	if body.Name == "" {
		writeError(writer, http.StatusBadRequest, "模板名称不能为空")
		return
	}
	resolution, err := server.targetResolution(target)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}

	operationContext, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 15*time.Minute)
	defer cancel()
	server.templateMu.Lock()
	defer server.templateMu.Unlock()
	body.Manifest.ID = body.ID
	body.Manifest.Name = body.Name
	body.Manifest.Width, body.Manifest.Height = resolution[0], resolution[1]
	body.Manifest = theme.NormalizeScene(body.Manifest)
	if err := absolutizeManifestResources(&body.Manifest); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	body.Manifest, err = server.ensureWaveshareBaseThemeResource(body.Manifest)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	setWaveshareTemplateRemotePath(&body.Manifest, body.ID)
	if err := body.Manifest.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	prepared, err := server.prepareTheme(operationContext, body.Manifest)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "编译微雪驻留主题失败: "+err.Error())
		return
	}
	clearWaveshareEmbeddedResourceRemotePaths(&prepared)
	previousRecord, hadPrevious := findWaveshareTemplate(server.store.Get(), target, body.ID)
	record, err := server.persistPreparedWaveshareTemplate(target, body.ID, body.Name, prepared)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	replaced := false
	if err := server.store.Update(func(cfg *config.Config) error {
		if cfg.WaveshareTemplates == nil {
			cfg.WaveshareTemplates = make(map[string][]config.WaveshareTemplate)
		}
		templates := append([]config.WaveshareTemplate(nil), cfg.WaveshareTemplates[target]...)
		for index := range templates {
			if templates[index].ID == record.ID {
				templates[index] = record
				replaced = true
				break
			}
		}
		if !replaced {
			templates = append(templates, record)
		}
		cfg.WaveshareTemplates[target] = templates
		return nil
	}); err != nil {
		if !hadPrevious || !sameLocalPath(previousRecord.ManifestPath, record.ManifestPath) {
			server.deleteLocalWaveshareTemplateFiles(record)
		}
		writeError(writer, http.StatusInternalServerError, "保存模板记录失败: "+err.Error())
		return
	}
	if hadPrevious && !sameLocalPath(previousRecord.ManifestPath, record.ManifestPath) {
		cfg := server.store.Get()
		if !sameLocalPath(cfg.DeviceThemePaths[target], previousRecord.ManifestPath) {
			server.deleteLocalWaveshareTemplateFiles(previousRecord)
		}
	}
	server.wakeWavesharePlaylist()
	writeJSON(writer, http.StatusOK, map[string]any{"target": target, "template": record, "updated": replaced})
}

// renameWaveshareTemplate 只修改模板库中的显示名称。模板 ID、清单路径、
// 屏端路径、更新时间和轮播引用都保持原值，也不会触发编译或设备上传。
func (server *Server) renameWaveshareTemplate(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	templateID, err := validateWaveshareLibraryID(request.URL.Query().Get("id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	var body waveshareTemplateRenameRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeError(writer, http.StatusBadRequest, "模板名称不能为空")
		return
	}

	server.templateMu.Lock()
	defer server.templateMu.Unlock()
	var record config.WaveshareTemplate
	renamed := false
	err = server.store.Update(func(cfg *config.Config) error {
		templates := append([]config.WaveshareTemplate(nil), cfg.WaveshareTemplates[target]...)
		selected := -1
		for index := range templates {
			if templates[index].ID == templateID {
				selected = index
				break
			}
		}
		if selected < 0 {
			return errWaveshareTemplateNotFound
		}
		for index := range templates {
			if index == selected {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(templates[index].Name), body.Name) {
				return errWaveshareTemplateNameConflict
			}
		}
		record = templates[selected]
		if record.Name == body.Name {
			return nil
		}
		record.Name = body.Name
		templates[selected] = record
		cfg.WaveshareTemplates[target] = templates
		renamed = true
		return nil
	})
	if errors.Is(err, errWaveshareTemplateNotFound) {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, errWaveshareTemplateNameConflict) {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "重命名模板失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"target": target, "template": record, "renamed": renamed,
	})
}

func (server *Server) activateWaveshareTemplate(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	templateID, err := validateWaveshareLibraryID(request.URL.Query().Get("id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	operationContext, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 15*time.Minute)
	defer cancel()
	record, results, err := server.applyPreparedWaveshareTemplate(operationContext, target, templateID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	formatted, successes := formatDeviceResults(results)
	server.wakeWavesharePlaylist()
	response := map[string]any{"target": target, "template": record, "device_results": formatted, "successes": successes}
	if len(results) > 0 && successes == 0 {
		response["error"] = "设备主题应用失败；模板已设为当前主题，将在设备重连后自动重试"
		writeJSON(writer, http.StatusBadGateway, response)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) deleteWaveshareTemplate(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	templateID, err := validateWaveshareLibraryID(request.URL.Query().Get("id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.templateMu.Lock()
	defer server.templateMu.Unlock()
	var record config.WaveshareTemplate
	removed := false
	err = server.store.Update(func(cfg *config.Config) error {
		var ok bool
		record, ok = findWaveshareTemplate(*cfg, target, templateID)
		if !ok {
			return nil
		}
		removed = true
		remaining := make([]config.WaveshareTemplate, 0, len(cfg.WaveshareTemplates[target])-1)
		for _, candidate := range cfg.WaveshareTemplates[target] {
			if candidate.ID != templateID {
				remaining = append(remaining, candidate)
			}
		}
		if len(remaining) == 0 {
			delete(cfg.WaveshareTemplates, target)
		} else {
			cfg.WaveshareTemplates[target] = remaining
		}
		playlist := cfg.WavesharePlaylists[target]
		entries := playlist.Entries[:0]
		for _, entry := range playlist.Entries {
			if entry.TemplateID != templateID {
				entries = append(entries, entry)
			}
		}
		playlist.Entries = entries
		if len(entries) == 0 {
			playlist.Enabled = false
		}
		if cfg.WavesharePlaylists != nil {
			cfg.WavesharePlaylists[target] = playlist
		}
		return nil
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "删除模板记录失败: "+err.Error())
		return
	}
	if !removed {
		writeError(writer, http.StatusNotFound, "未找到微雪模板")
		return
	}
	// 这里只删除模板库记录，绝不调用设备删除命令；TF 中的驻留主题保持不变。
	// 当前正在显示的 revision 仍被 service 重连逻辑引用，必须保留到另一模板真正
	// 激活后再由 applyPreparedWaveshareTemplate 安全回收。
	localFilesRetained := waveshareManifestPathReferenced(server.store.Get(), record.ManifestPath)
	if !localFilesRetained {
		server.deleteLocalWaveshareTemplateFiles(record)
	}
	server.wakeWavesharePlaylist()
	writeJSON(writer, http.StatusOK, map[string]any{
		"target": target, "deleted": templateID, "remote_deleted": false, "local_files_retained": localFilesRetained,
	})
}

func (server *Server) getWavesharePlaylist(writer http.ResponseWriter, request *http.Request) {
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	playlist := server.store.Get().WavesharePlaylists[target]
	if playlist.Mode == "" {
		playlist.Mode = "sequence"
	}
	if playlist.DefaultDurationSeconds == 0 {
		playlist.DefaultDurationSeconds = config.DefaultWavesharePlaylistDurationSeconds
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"target": target, "playlist": playlist, "runtime": server.wavesharePlaylistStatus(target),
	})
}

func (server *Server) putWavesharePlaylist(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	var playlist config.WavesharePlaylist
	if err := decodeJSON(writer, request, &playlist); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if playlist.Mode == "" {
		playlist.Mode = "sequence"
	}
	if playlist.DefaultDurationSeconds == 0 {
		playlist.DefaultDurationSeconds = config.DefaultWavesharePlaylistDurationSeconds
	}
	server.templateMu.Lock()
	err = server.store.Update(func(cfg *config.Config) error {
		if cfg.WavesharePlaylists == nil {
			cfg.WavesharePlaylists = make(map[string]config.WavesharePlaylist)
		}
		cfg.WavesharePlaylists[target] = playlist
		return nil
	})
	server.templateMu.Unlock()
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.wakeWavesharePlaylist()
	writeJSON(writer, http.StatusOK, map[string]any{"target": target, "playlist": playlist})
}

func absolutizeManifestResources(manifest *theme.Manifest) error {
	for index := range manifest.Resources {
		if manifest.Resources[index].LocalPath == "" || filepath.IsAbs(manifest.Resources[index].LocalPath) {
			continue
		}
		absolute, err := filepath.Abs(manifest.Resources[index].LocalPath)
		if err != nil {
			return fmt.Errorf("主题资源路径无效: %w", err)
		}
		manifest.Resources[index].LocalPath = absolute
	}
	return nil
}

func setWaveshareTemplateRemotePath(manifest *theme.Manifest, templateID string) {
	remotePath := fmt.Sprintf("/data/theme/%dx%d/casescreen-%s.Theme", manifest.Width, manifest.Height, templateID)
	for index := range manifest.Resources {
		if strings.EqualFold(filepath.Ext(manifest.Resources[index].RemotePath), ".Theme") || strings.EqualFold(filepath.Ext(manifest.Resources[index].LocalPath), ".Theme") {
			manifest.Resources[index].RemotePath = remotePath
			return
		}
	}
}

// 背景图片/视频已经写入 .Theme，保留本地路径供编辑器预览即可，不再作为
// 独立文件重复占用 TF 空间。
func clearWaveshareEmbeddedResourceRemotePaths(manifest *theme.Manifest) {
	for index := range manifest.Resources {
		resource := &manifest.Resources[index]
		if strings.EqualFold(filepath.Ext(resource.RemotePath), ".Theme") || strings.EqualFold(filepath.Ext(resource.LocalPath), ".Theme") {
			continue
		}
		resource.RemotePath = ""
	}
}

func (server *Server) persistPreparedWaveshareTemplate(target, templateID, name string, manifest theme.Manifest) (config.WaveshareTemplate, error) {
	themeIndex := -1
	var themeData []byte
	totalSize := int64(0)
	for index, resource := range manifest.Resources {
		if resource.RemotePath != "" {
			totalSize += max(int64(0), resource.Size)
		}
		if !strings.EqualFold(filepath.Ext(resource.RemotePath), ".Theme") && !strings.EqualFold(filepath.Ext(resource.LocalPath), ".Theme") {
			continue
		}
		if themeIndex >= 0 {
			return config.WaveshareTemplate{}, errors.New("已准备主题只能包含一个 .Theme 资源")
		}
		data, err := os.ReadFile(resource.LocalPath)
		if err != nil {
			return config.WaveshareTemplate{}, fmt.Errorf("读取已编译微雪主题失败: %w", err)
		}
		if resource.RemotePath == "" {
			return config.WaveshareTemplate{}, errors.New("已准备 .Theme 资源缺少屏端路径")
		}
		themeIndex = index
		themeData = data
	}
	if themeIndex < 0 {
		return config.WaveshareTemplate{}, errors.New("已准备主题中缺少 .Theme 资源")
	}
	targetDigest := sha256.Sum256([]byte(target))
	templateDigest := sha256.Sum256([]byte(templateID))
	revisionDigest := sha256.Sum256(themeData)
	directory := filepath.Join(
		server.dataDirectory, "waveshare-templates", fmt.Sprintf("device-%x", targetDigest[:8]), fmt.Sprintf("template-%x", templateDigest[:8]),
		fmt.Sprintf("revision-%x", revisionDigest[:12]),
	)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return config.WaveshareTemplate{}, fmt.Errorf("创建模板目录失败: %w", err)
	}
	themePath := filepath.Join(directory, "resident.Theme")
	if err := os.WriteFile(themePath, themeData, 0o600); err != nil {
		return config.WaveshareTemplate{}, fmt.Errorf("持久化微雪主题失败: %w", err)
	}
	resource := manifest.Resources[themeIndex]
	persisted, err := theme.InspectResource(resource.ID, themePath, resource.RemotePath)
	if err != nil {
		return config.WaveshareTemplate{}, err
	}
	manifest.Resources[themeIndex] = persisted
	manifestPath := filepath.Join(directory, "manifest.json")
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return config.WaveshareTemplate{}, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		return config.WaveshareTemplate{}, fmt.Errorf("保存模板清单失败: %w", err)
	}
	return config.WaveshareTemplate{
		ID: templateID, Name: name, ManifestPath: manifestPath, RemoteThemePath: persisted.RemotePath,
		SizeBytes: totalSize, UpdatedAt: time.Now().UTC(),
	}, nil
}

func findWaveshareTemplate(cfg config.Config, target, templateID string) (config.WaveshareTemplate, bool) {
	for _, record := range cfg.WaveshareTemplates[target] {
		if record.ID == templateID {
			return record, true
		}
	}
	return config.WaveshareTemplate{}, false
}

func loadPreparedWaveshareManifest(record config.WaveshareTemplate) (theme.Manifest, error) {
	data, err := os.ReadFile(record.ManifestPath)
	if err != nil {
		return theme.Manifest{}, fmt.Errorf("读取模板清单失败: %w", err)
	}
	var manifest theme.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return theme.Manifest{}, fmt.Errorf("解析模板清单失败: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return theme.Manifest{}, fmt.Errorf("模板清单无效: %w", err)
	}
	return manifest, nil
}

type waveshareLyricsSettings struct {
	provider string
	path     string
}

// waveshareTemplateLyricsSettings 把主题中的歌词来源提升为模板运行时设置。
// 歌词引擎是全局单实例，因此同一模板包含多个歌词图层时必须使用相同来源。
func waveshareTemplateLyricsSettings(manifest theme.Manifest) (waveshareLyricsSettings, bool, error) {
	var selected waveshareLyricsSettings
	found := false
	for _, layer := range manifest.Layers {
		if layer.Type != theme.LayerLyrics {
			continue
		}
		provider := "auto"
		path := ""
		if layer.Options != nil {
			if value, ok := layer.Options["provider"]; ok {
				text, valid := value.(string)
				if !valid {
					return waveshareLyricsSettings{}, false, fmt.Errorf("歌词图层 %q 的 provider 必须是字符串", layer.ID)
				}
				if normalized := strings.ToLower(strings.TrimSpace(text)); normalized != "" {
					provider = normalized
				}
			}
			if value, ok := layer.Options["lrc_path"]; ok {
				text, valid := value.(string)
				if !valid {
					return waveshareLyricsSettings{}, false, fmt.Errorf("歌词图层 %q 的 lrc_path 必须是字符串", layer.ID)
				}
				path = strings.TrimSpace(text)
			}
		}
		switch provider {
		case "kugou", "sogou":
			// 旧主题的标题搜索来源在载入时迁移为严格自动模式。
			provider = "auto"
		case "auto", "netease", "qq", "local":
		default:
			return waveshareLyricsSettings{}, false, fmt.Errorf("歌词图层 %q 的来源不受支持: %q", layer.ID, provider)
		}
		current := waveshareLyricsSettings{provider: provider, path: path}
		if found && current != selected {
			return waveshareLyricsSettings{}, false, errors.New("同一模板中的多个歌词图层必须使用相同的歌词来源和本地 LRC")
		}
		selected, found = current, true
	}
	return selected, found, nil
}

func (server *Server) applyPreparedWaveshareTemplate(ctx context.Context, target, templateID string) (config.WaveshareTemplate, map[string]error, error) {
	server.templateMu.Lock()
	defer server.templateMu.Unlock()
	cfg := server.store.Get()
	record, ok := findWaveshareTemplate(cfg, target, templateID)
	if !ok {
		return config.WaveshareTemplate{}, nil, fmt.Errorf("未找到微雪模板 %q", templateID)
	}
	manifest, err := loadPreparedWaveshareManifest(record)
	if err != nil {
		return record, nil, err
	}
	sourceRecord := record
	upgraded := false
	cleanupUncommittedUpgrade := false
	configurationCommitted := false
	defer func() {
		if cleanupUncommittedUpgrade && !configurationCommitted {
			server.deleteLocalWaveshareTemplateFiles(record)
		}
	}()
	// 旧版可能把歌词面板和底轨永久烘焙进驻留视频。激活或轮播时不能
	// 静默复用这类产物；保留的源资源可直接重编译，缺失时向用户返回
	// 明确错误，由编辑器重新导入/保存，不把错误 Theme 写回 TF。
	if manifest.NeedsWaveshareCompilerUpgrade() {
		prepared, prepareErr := server.prepareTheme(ctx, manifest)
		if prepareErr != nil {
			return sourceRecord, nil, fmt.Errorf("旧版微雪歌词主题需要重新编译；请在编辑器重新保存该模板: %w", prepareErr)
		}
		if prepared.NeedsWaveshareCompilerUpgrade() {
			return sourceRecord, nil, errors.New("旧版微雪歌词主题重新编译后仍缺少当前可见性契约")
		}
		clearWaveshareEmbeddedResourceRemotePaths(&prepared)
		refreshed, persistErr := server.persistPreparedWaveshareTemplate(target, templateID, record.Name, prepared)
		if persistErr != nil {
			return sourceRecord, nil, fmt.Errorf("保存升级后的微雪歌词主题失败: %w", persistErr)
		}
		manifest, record, upgraded = prepared, refreshed, true
		cleanupUncommittedUpgrade = !sameLocalPath(record.ManifestPath, sourceRecord.ManifestPath)
	}
	lyricsSettings, hasLyricsSettings, err := waveshareTemplateLyricsSettings(manifest)
	if err != nil {
		return sourceRecord, nil, err
	}
	previousActivePath := cfg.DeviceThemePaths[target]
	deletePaths := make(map[string]string)
	server.settingsMu.Lock()
	updateTemplateConfig := server.updateTemplateConfig
	if updateTemplateConfig == nil {
		updateTemplateConfig = server.store.Update
	}
	err = updateTemplateConfig(func(cfg *config.Config) error {
		current, exists := findWaveshareTemplate(*cfg, target, templateID)
		if !exists || !sameLocalPath(current.ManifestPath, sourceRecord.ManifestPath) {
			return fmt.Errorf("微雪模板 %q 已在激活期间被修改", templateID)
		}
		if upgraded {
			templates := append([]config.WaveshareTemplate(nil), cfg.WaveshareTemplates[target]...)
			for index := range templates {
				if templates[index].ID == templateID {
					templates[index] = record
					break
				}
			}
			cfg.WaveshareTemplates[target] = templates
		}
		if cfg.DeviceThemePaths == nil {
			cfg.DeviceThemePaths = make(map[string]string)
		}
		cfg.DeviceThemePaths[target] = record.ManifestPath
		if hasLyricsSettings {
			cfg.LyricsProvider = lyricsSettings.provider
			cfg.LyricsPath = lyricsSettings.path
		}
		for _, candidate := range []string{previousActivePath, sourceRecord.ManifestPath} {
			if candidate == "" || sameLocalPath(candidate, record.ManifestPath) || waveshareManifestPathReferenced(*cfg, candidate) {
				continue
			}
			deletePaths[strings.ToLower(filepath.Clean(candidate))] = candidate
		}
		return nil
	})
	server.settingsMu.Unlock()
	if err != nil {
		return sourceRecord, nil, fmt.Errorf("记住当前模板失败: %w", err)
	}
	configurationCommitted = true
	if hasLyricsSettings {
		if configurable, ok := server.controller.(interface{ SetLyricsProviderPreference(string) }); ok {
			configurable.SetLyricsProviderPreference(lyricsSettings.provider)
		}
		if configurable, ok := server.controller.(interface{ SetLyricsPath(string) }); ok {
			configurable.SetLyricsPath(lyricsSettings.path)
		}
	}
	// 配置和新 revision 已经原子提交后才允许下发设备。即使硬件暂时失败，
	// service 的 pending 状态与重启恢复也都会指向同一份持久化主题。
	results := server.controller.ApplyThemeTo(ctx, target, manifest)
	for _, path := range deletePaths {
		server.deleteLocalWaveshareTemplateFiles(config.WaveshareTemplate{ManifestPath: path})
	}
	return record, results, nil
}

func waveshareManifestPathReferenced(cfg config.Config, manifestPath string) bool {
	for _, path := range cfg.DeviceThemePaths {
		if sameLocalPath(path, manifestPath) {
			return true
		}
	}
	for _, records := range cfg.WaveshareTemplates {
		for _, record := range records {
			if sameLocalPath(record.ManifestPath, manifestPath) {
				return true
			}
		}
	}
	return false
}

func formatDeviceResults(results map[string]error) (map[string]string, int) {
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
	return formatted, successes
}

func activeWaveshareTemplateID(cfg config.Config, target string) string {
	activePath := cfg.DeviceThemePaths[target]
	for _, record := range cfg.WaveshareTemplates[target] {
		if sameLocalPath(activePath, record.ManifestPath) {
			return record.ID
		}
	}
	return ""
}

func sameLocalPath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil && rightErr == nil {
		return strings.EqualFold(filepath.Clean(leftAbs), filepath.Clean(rightAbs))
	}
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func (server *Server) deleteLocalWaveshareTemplateFiles(record config.WaveshareTemplate) {
	root, rootErr := filepath.Abs(filepath.Join(server.dataDirectory, "waveshare-templates"))
	manifestPath, pathErr := filepath.Abs(record.ManifestPath)
	if rootErr != nil || pathErr != nil {
		return
	}
	relative, err := filepath.Rel(root, manifestPath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return
	}
	if manifest, err := loadPreparedWaveshareManifest(record); err == nil {
		for _, resource := range manifest.Resources {
			if !strings.EqualFold(filepath.Ext(resource.LocalPath), ".Theme") {
				continue
			}
			resourcePath, err := filepath.Abs(resource.LocalPath)
			if err == nil && sameDirectory(filepath.Dir(manifestPath), filepath.Dir(resourcePath)) {
				_ = os.Remove(resourcePath)
			}
		}
	}
	_ = os.Remove(manifestPath)
	_ = os.Remove(filepath.Dir(manifestPath))
}

func sameDirectory(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func (server *Server) wakeWavesharePlaylist() {
	select {
	case server.playlistWake <- struct{}{}:
	default:
	}
}

func (server *Server) wavesharePlaylistStatus(target string) wavesharePlaylistStatus {
	server.playlistStatusMu.RLock()
	defer server.playlistStatusMu.RUnlock()
	return server.playlistStatuses[target]
}

func (server *Server) storeWavesharePlaylistStatus(target string, state wavesharePlaylistRuntime) {
	status := wavesharePlaylistStatus{
		Running:           true,
		CurrentTemplateID: state.currentID,
		PendingTemplateID: state.retryTemplateID,
		LastError:         state.lastError,
		LastAttempt:       state.lastAttempt,
		NextSwitch:        state.due,
		RetryAt:           state.retryAt,
	}
	server.playlistStatusMu.Lock()
	if server.playlistStatuses == nil {
		server.playlistStatuses = make(map[string]wavesharePlaylistStatus)
	}
	server.playlistStatuses[target] = status
	server.playlistStatusMu.Unlock()
}

func (server *Server) clearWavesharePlaylistStatus(target string) {
	server.playlistStatusMu.Lock()
	delete(server.playlistStatuses, target)
	server.playlistStatusMu.Unlock()
}

func secureRandomIndex(count int) int {
	if count <= 1 {
		return 0
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(count))
	}
	return int(value.Int64())
}

func (server *Server) runWavesharePlaylistScheduler(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	states := make(map[string]wavesharePlaylistRuntime)
	server.advanceWavesharePlaylists(ctx, time.Now(), states)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			server.advanceWavesharePlaylists(ctx, time.Now(), states)
		case <-server.playlistWake:
			server.advanceWavesharePlaylists(ctx, time.Now(), states)
		}
	}
}

func (server *Server) advanceWavesharePlaylists(ctx context.Context, now time.Time, states map[string]wavesharePlaylistRuntime) {
	cfg := server.store.Get()
	for target := range states {
		playlist, ok := cfg.WavesharePlaylists[target]
		if !ok || !playlist.Enabled || len(playlist.Entries) == 0 {
			delete(states, target)
			server.clearWavesharePlaylistStatus(target)
		}
	}
	for target, playlist := range cfg.WavesharePlaylists {
		if !isConfiguredWaveshareTarget(cfg, target) || !playlist.Enabled || len(playlist.Entries) == 0 {
			server.clearWavesharePlaylistStatus(target)
			continue
		}
		if playlist.DefaultDurationSeconds <= 0 {
			playlist.DefaultDurationSeconds = config.DefaultWavesharePlaylistDurationSeconds
		}
		signatureData, _ := json.Marshal(playlist)
		signature := string(signatureData)
		activeID := activeWaveshareTemplateID(cfg, target)
		state, exists := states[target]
		if !exists || state.signature != signature {
			state = wavesharePlaylistRuntime{signature: signature}
			if activeIndex := playlistEntryIndex(playlist, activeID); activeIndex >= 0 {
				entry := playlist.Entries[activeIndex]
				state.currentID = entry.TemplateID
				state.index = activeIndex
				state.due = now.Add(wavesharePlaylistEntryDuration(playlist, entry))
				states[target] = state
				server.storeWavesharePlaylistStatus(target, state)
				continue
			}
			index := server.firstPlaylistIndex(playlist)
			state = server.applyWavesharePlaylistEntry(ctx, now, target, playlist, state, index)
			states[target] = state
			server.storeWavesharePlaylistStatus(target, state)
			cfg = server.store.Get()
			continue
		}
		if state.retryTemplateID != "" {
			if now.Before(state.retryAt) {
				server.storeWavesharePlaylistStatus(target, state)
				continue
			}
			index := playlistEntryIndex(playlist, state.retryTemplateID)
			if index < 0 {
				state.retryTemplateID, state.lastError = "", ""
				state.failures, state.retryAt = 0, time.Time{}
				states[target] = state
				server.storeWavesharePlaylistStatus(target, state)
				continue
			}
			state = server.applyWavesharePlaylistEntry(ctx, now, target, playlist, state, index)
			states[target] = state
			server.storeWavesharePlaylistStatus(target, state)
			cfg = server.store.Get()
			continue
		}
		if activeID != "" && activeID != state.currentID {
			if activeIndex := playlistEntryIndex(playlist, activeID); activeIndex >= 0 {
				entry := playlist.Entries[activeIndex]
				state.currentID = entry.TemplateID
				state.index = activeIndex
				state.due = now.Add(wavesharePlaylistEntryDuration(playlist, entry))
				states[target] = state
				server.storeWavesharePlaylistStatus(target, state)
				continue
			}
			index := server.firstPlaylistIndex(playlist)
			state = server.applyWavesharePlaylistEntry(ctx, now, target, playlist, state, index)
			states[target] = state
			server.storeWavesharePlaylistStatus(target, state)
			cfg = server.store.Get()
			continue
		}
		if now.Before(state.due) {
			server.storeWavesharePlaylistStatus(target, state)
			continue
		}
		index := server.nextPlaylistIndex(playlist, state.index)
		state = server.applyWavesharePlaylistEntry(ctx, now, target, playlist, state, index)
		states[target] = state
		server.storeWavesharePlaylistStatus(target, state)
		cfg = server.store.Get()
	}
}

func (server *Server) applyWavesharePlaylistEntry(ctx context.Context, now time.Time, target string, playlist config.WavesharePlaylist, state wavesharePlaylistRuntime, index int) wavesharePlaylistRuntime {
	entry := playlist.Entries[index]
	state.signature = func() string {
		data, _ := json.Marshal(playlist)
		return string(data)
	}()
	state.lastAttempt = now
	_, results, err := server.applyPreparedWaveshareTemplate(ctx, target, entry.TemplateID)
	if err == nil {
		if deviceErr, exists := results[target]; exists && deviceErr != nil {
			err = fmt.Errorf("设备应用模板失败: %w", deviceErr)
		}
	}
	if err != nil {
		state.retryTemplateID = entry.TemplateID
		state.failures++
		state.lastError = err.Error()
		state.retryAt = now.Add(wavesharePlaylistRetryDelay(state.failures))
		state.due = time.Time{}
		return state
	}
	state.currentID = entry.TemplateID
	state.index = index
	state.due = now.Add(wavesharePlaylistEntryDuration(playlist, entry))
	state.retryTemplateID = ""
	state.failures = 0
	state.retryAt = time.Time{}
	state.lastError = ""
	return state
}

func wavesharePlaylistEntryDuration(playlist config.WavesharePlaylist, entry config.WavesharePlaylistEntry) time.Duration {
	seconds := entry.DurationSeconds
	if seconds <= 0 {
		seconds = playlist.DefaultDurationSeconds
	}
	if seconds <= 0 {
		seconds = config.DefaultWavesharePlaylistDurationSeconds
	}
	return time.Duration(seconds) * time.Second
}

func wavesharePlaylistRetryDelay(failures int) time.Duration {
	delay := 2 * time.Second
	for index := 1; index < failures && delay < maximumWavesharePlaylistRetryDelay; index++ {
		delay *= 2
		if delay > maximumWavesharePlaylistRetryDelay {
			delay = maximumWavesharePlaylistRetryDelay
		}
	}
	return delay
}

func playlistEntryIndex(playlist config.WavesharePlaylist, templateID string) int {
	for index, entry := range playlist.Entries {
		if entry.TemplateID == templateID {
			return index
		}
	}
	return -1
}

func (server *Server) firstPlaylistIndex(playlist config.WavesharePlaylist) int {
	if playlist.Mode == "random" {
		return server.randomIndex(len(playlist.Entries))
	}
	return 0
}

func (server *Server) nextPlaylistIndex(playlist config.WavesharePlaylist, current int) int {
	if len(playlist.Entries) <= 1 {
		return 0
	}
	if playlist.Mode != "random" {
		return (current + 1) % len(playlist.Entries)
	}
	// 在剩余 n-1 项中随机取一项，再跳过当前索引，保证不会连续相同。
	next := server.randomIndex(len(playlist.Entries) - 1)
	if next >= current {
		next++
	}
	return next
}
