package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/theme"
)

func TestWaveshareTemplateAPICompilesOnceAndActivatesPreparedManifest(t *testing.T) {
	server, controller := newTestServer(t)
	prepareCalls := 0
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		prepareCalls++
		found := false
		for _, resource := range manifest.Resources {
			if strings.HasSuffix(strings.ToLower(resource.RemotePath), ".theme") {
				found = true
			}
		}
		if !found {
			t.Fatal("新建微雪主题没有自动注入 .Theme")
		}
		return manifest, nil
	}

	saveWaveshareTemplateForTest(t, server, "waveshare-7", "night", "夜间")
	if prepareCalls != 1 {
		t.Fatalf("保存应只编译一次，实际 %d", prepareCalls)
	}
	request := authorizedRequest(server, http.MethodPost, "/api/waveshare/templates/activate?target=waveshare-7&id=night", `{}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("激活失败: %d %s", response.Code, response.Body.String())
	}
	if prepareCalls != 1 || controller.appliedTarget != "waveshare-7" || controller.appliedTheme != "night" {
		t.Fatalf("激活不应重编译，运行结果=%+v，编译次数=%d", controller, prepareCalls)
	}
	record, ok := findWaveshareTemplate(server.store.Get(), "waveshare-7", "night")
	if !ok || !sameLocalPath(server.store.Get().DeviceThemePaths["waveshare-7"], record.ManifestPath) {
		t.Fatal("激活后没有持久化当前模板")
	}
	manifest, err := loadPreparedWaveshareManifest(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range manifest.Resources {
		if strings.EqualFold(filepathExtension(resource.LocalPath), ".Theme") {
			if _, err := os.Stat(resource.LocalPath); err != nil {
				t.Fatalf("持久化 .Theme 不存在: %v", err)
			}
		}
	}

	list := httptest.NewRecorder()
	server.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/waveshare/templates?target=waveshare-7", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"active_template_id":"night"`) {
		t.Fatalf("模板清单响应无效: %d %s", list.Code, list.Body.String())
	}
}

func TestActivatingLegacyLyricsTemplateRecompilesBeforeApply(t *testing.T) {
	server, controller := newTestServer(t)
	prepareCalls := 0
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		prepareCalls++
		// 第一次模拟升级前保存的旧产物；激活时的第二次编译补上新契约。
		if prepareCalls > 1 {
			manifest.WaveshareCompilerRevision = theme.CurrentWaveshareCompilerRevision
		}
		return manifest, nil
	}
	manifest := theme.Manifest{
		ID: "legacy-lyrics", Name: "旧歌词主题", Width: 1024, Height: 600,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .1, Y: .7, Width: .8, Height: .2}, Opacity: 1},
		},
	}
	saveWaveshareManifestForTest(t, server, "waveshare-7", "legacy-lyrics", "旧歌词主题", manifest)
	legacy, ok := findWaveshareTemplate(server.store.Get(), "waveshare-7", "legacy-lyrics")
	if !ok {
		t.Fatal("旧歌词模板没有保存")
	}
	legacyManifest, err := loadPreparedWaveshareManifest(legacy)
	if err != nil || !legacyManifest.NeedsWaveshareCompilerUpgrade() {
		t.Fatalf("测试前置不是旧编译契约: manifest=%+v err=%v", legacyManifest, err)
	}

	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "legacy-lyrics"); err != nil {
		t.Fatal(err)
	}
	if prepareCalls != 2 || controller.appliedTheme != "legacy-lyrics" {
		t.Fatalf("旧歌词模板未在应用前重编译: calls=%d applied=%q", prepareCalls, controller.appliedTheme)
	}
	current, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", "legacy-lyrics")
	currentManifest, err := loadPreparedWaveshareManifest(current)
	if err != nil || currentManifest.NeedsWaveshareCompilerUpgrade() {
		t.Fatalf("升级后的歌词模板仍是旧契约: manifest=%+v err=%v", currentManifest, err)
	}
}

func TestActivatingLegacyLyricsTemplateFailsClosedWhenRecompileFails(t *testing.T) {
	server, controller := newTestServer(t)
	prepareCalls := 0
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		prepareCalls++
		if prepareCalls > 1 {
			return manifest, errors.New("源图片不存在")
		}
		return manifest, nil
	}
	manifest := theme.Manifest{
		ID: "legacy-missing", Name: "缺资源旧主题", Width: 1024, Height: 600,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .1, Y: .7, Width: .8, Height: .2}, Opacity: 1},
		},
	}
	saveWaveshareManifestForTest(t, server, "waveshare-7", "legacy-missing", "缺资源旧主题", manifest)
	_, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "legacy-missing")
	if err == nil || !strings.Contains(err.Error(), "请在编辑器重新保存") {
		t.Fatalf("旧主题重编译失败没有明确关闭: %v", err)
	}
	if controller.appliedTheme != "" {
		t.Fatalf("重编译失败后仍应用了旧 Theme: %q", controller.appliedTheme)
	}
}

func TestLegacyLyricsMigrationDoesNotApplyOrLeakWhenConfigCommitFails(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Server, config.WaveshareTemplate)
	}{
		{
			name: "配置保存失败",
			configure: func(server *Server, _ config.WaveshareTemplate) {
				server.updateTemplateConfig = func(func(*config.Config) error) error {
					return errors.New("模拟配置保存失败")
				}
			},
		},
		{
			name: "模板记录冲突",
			configure: func(server *Server, source config.WaveshareTemplate) {
				server.updateTemplateConfig = func(update func(*config.Config) error) error {
					return server.store.Update(func(cfg *config.Config) error {
						templates := append([]config.WaveshareTemplate(nil), cfg.WaveshareTemplates["waveshare-7"]...)
						for index := range templates {
							if templates[index].ID == source.ID {
								templates[index].ManifestPath = source.ManifestPath + ".conflict"
							}
						}
						cfg.WaveshareTemplates["waveshare-7"] = templates
						return update(cfg)
					})
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, controller := newTestServer(t)
			installDistinctLegacyLyricsUpgradeCompiler(t, server)
			manifest := legacyLyricsManifestForTest("legacy-transaction", "旧歌词事务")
			saveWaveshareManifestForTest(t, server, "waveshare-7", manifest.ID, manifest.Name, manifest)
			source, ok := findWaveshareTemplate(server.store.Get(), "waveshare-7", manifest.ID)
			if !ok {
				t.Fatal("旧歌词模板没有保存")
			}
			templateDirectory := filepath.Dir(filepath.Dir(source.ManifestPath))
			test.configure(server, source)

			_, results, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", manifest.ID)
			if err == nil || results != nil {
				t.Fatalf("配置提交失败没有关闭设备应用: results=%v err=%v", results, err)
			}
			if controller.appliedTheme != "" {
				t.Fatalf("配置提交失败后仍下发了新 Theme: %q", controller.appliedTheme)
			}
			current, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", manifest.ID)
			if !sameLocalPath(current.ManifestPath, source.ManifestPath) {
				t.Fatalf("配置提交失败后旧模板记录被替换: old=%q current=%q", source.ManifestPath, current.ManifestPath)
			}
			entries, readErr := os.ReadDir(templateDirectory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || entries[0].Name() != filepath.Base(filepath.Dir(source.ManifestPath)) {
				t.Fatalf("配置提交失败后遗留了孤儿 revision: %+v", entries)
			}
		})
	}
}

func TestLegacyLyricsMigrationKeepsCommittedRevisionWhenApplyFails(t *testing.T) {
	server, controller := newTestServer(t)
	installDistinctLegacyLyricsUpgradeCompiler(t, server)
	manifest := legacyLyricsManifestForTest("legacy-apply-failure", "旧歌词应用失败")
	saveWaveshareManifestForTest(t, server, "waveshare-7", manifest.ID, manifest.Name, manifest)
	source, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", manifest.ID)
	controller.applyErr = errors.New("模拟串口暂时断开")

	record, results, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", manifest.ID)
	if err != nil || !errors.Is(results["waveshare-7"], controller.applyErr) {
		t.Fatalf("设备失败结果错误: results=%v err=%v", results, err)
	}
	current, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", manifest.ID)
	if sameLocalPath(current.ManifestPath, source.ManifestPath) || !sameLocalPath(current.ManifestPath, record.ManifestPath) ||
		!sameLocalPath(server.store.Get().DeviceThemePaths["waveshare-7"], record.ManifestPath) {
		t.Fatalf("设备失败后没有保留已提交的新 revision: source=%q record=%q current=%q", source.ManifestPath, record.ManifestPath, current.ManifestPath)
	}
	if _, statErr := os.Stat(record.ManifestPath); statErr != nil {
		t.Fatalf("设备失败后新 revision 不可用于重试: %v", statErr)
	}
	if _, statErr := os.Stat(source.ManifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("设备失败后无引用旧 revision 未清理: %v", statErr)
	}
}

func TestOverwritingActiveWaveshareTemplateKeepsOldRevisionUntilActivation(t *testing.T) {
	server, controller := newTestServer(t)
	preparedDirectory := t.TempDir()
	revision := 0
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		revision++
		contents := []byte(fmt.Sprintf("prepared-revision-%d", revision))
		path := filepath.Join(preparedDirectory, fmt.Sprintf("revision-%d.Theme", revision))
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return manifest, err
		}
		for index, resource := range manifest.Resources {
			if !strings.EqualFold(filepath.Ext(resource.RemotePath), ".Theme") && !strings.EqualFold(filepath.Ext(resource.LocalPath), ".Theme") {
				continue
			}
			remotePath := waveshareVersionedThemeRemotePath(resource.RemotePath, contents)
			prepared, err := theme.InspectResource(resource.ID, path, remotePath)
			if err != nil {
				return manifest, err
			}
			manifest.Resources[index] = prepared
			return manifest, nil
		}
		return manifest, fmt.Errorf("缺少 .Theme 资源")
	}

	saveWaveshareTemplateForTest(t, server, "waveshare-7", "night", "夜间")
	first, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", "night")
	firstManifest, err := loadPreparedWaveshareManifest(first)
	if err != nil {
		t.Fatal(err)
	}
	firstThemePath := waveshareThemeLocalPath(firstManifest)
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "night"); err != nil {
		t.Fatal(err)
	}
	controller.appliedTheme = ""

	saveWaveshareTemplateForTest(t, server, "waveshare-7", "night", "夜间")
	cfg := server.store.Get()
	second, _ := findWaveshareTemplate(cfg, "waveshare-7", "night")
	if sameLocalPath(first.ManifestPath, second.ManifestPath) {
		t.Fatalf("不同内容的同 ID 模板覆盖了活动 revision: %q", second.ManifestPath)
	}
	if len(cfg.WaveshareTemplates["waveshare-7"]) != 1 {
		t.Fatalf("覆盖同 ID 后不应生成重复记录: %+v", cfg.WaveshareTemplates["waveshare-7"])
	}
	if active := activeWaveshareTemplateID(cfg, "waveshare-7"); active != "" {
		t.Fatalf("尚未激活的新 revision 不应报告为活动模板: %q", active)
	}
	if controller.appliedTheme != "" {
		t.Fatal("保存新 revision 不应隐式写入 TF")
	}
	activeManifest, err := loadPreparedWaveshareManifest(config.WaveshareTemplate{ManifestPath: cfg.DeviceThemePaths["waveshare-7"]})
	if err != nil {
		t.Fatalf("覆盖后旧活动清单应保持可读，以供重连使用: %v", err)
	}
	activeThemePath := waveshareThemeLocalPath(activeManifest)
	contents, err := os.ReadFile(activeThemePath)
	if err != nil || string(contents) != "prepared-revision-1" || !sameLocalPath(activeThemePath, firstThemePath) {
		t.Fatalf("旧活动 revision 被改写: path=%q data=%q err=%v", activeThemePath, contents, err)
	}

	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "night"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.ManifestPath); !os.IsNotExist(err) {
		t.Fatalf("激活新 revision 后未回收无引用旧清单: %v", err)
	}
	if _, err := os.Stat(firstThemePath); !os.IsNotExist(err) {
		t.Fatalf("激活新 revision 后未回收无引用旧主题: %v", err)
	}
	if active := activeWaveshareTemplateID(server.store.Get(), "waveshare-7"); active != "night" {
		t.Fatalf("新 revision 激活状态错误: %q", active)
	}
}

func TestActivatingAnotherTemplateKeepsReferencedRevision(t *testing.T) {
	server, _ := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "first", "第一套")
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "second", "第二套")
	first, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", "first")
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.ManifestPath); err != nil {
		t.Fatalf("切换模板时不应删除库中仍被引用的 revision: %v", err)
	}
}

func TestPutThemeInjectsBaseThemeOnlyForWaveshare(t *testing.T) {
	server, controller := newTestServer(t)
	prepareCalls := 0
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		prepareCalls++
		if prepareCalls == 1 && !manifestHasThemeResource(manifest) {
			t.Fatalf("微雪 PUT 主题没有自动注入基础 .Theme: %+v", manifest.Resources)
		}
		return manifest, nil
	}
	manifest := theme.Manifest{
		ID: "fresh", Name: "新建主题", Width: 12, Height: 34,
		Layers: []theme.Layer{{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1}},
	}
	body, _ := json.Marshal(manifest)
	request := authorizedRequest(server, http.MethodPut, "/api/theme?target=waveshare-7", string(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || prepareCalls != 1 || controller.appliedTarget != "waveshare-7" {
		t.Fatalf("微雪新主题应用失败: %d %s", response.Code, response.Body.String())
	}
	foundRemote := ""
	for _, resource := range controller.appliedManifest.Resources {
		if strings.EqualFold(filepathExtension(resource.LocalPath), ".Theme") {
			foundRemote = resource.RemotePath
		}
	}
	if !strings.HasPrefix(foundRemote, "/data/theme/1024x600/casescreen-base") {
		t.Fatalf("基础主题远端路径无效: %q", foundRemote)
	}

	// 非微雪仍沿用原行为，不注入任何微雪资源。
	controller.appliedManifest = theme.Manifest{}
	request = authorizedRequest(server, http.MethodPut, "/api/theme?target=coolermaster-atmos-v2", string(body))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || prepareCalls != 2 || manifestHasThemeResource(controller.appliedManifest) {
		t.Fatalf("非微雪目标被错误注入主题包: %d %+v", response.Code, controller.appliedManifest.Resources)
	}
}

func TestDeleteWaveshareTemplateOnlyDeletesLocalFiles(t *testing.T) {
	server, controller := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "delete-me", "待删除")
	record, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", "delete-me")
	request := authorizedRequest(server, http.MethodDelete, "/api/waveshare/templates?target=waveshare-7&id=delete-me", `{}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"remote_deleted":false`) {
		t.Fatalf("删除响应无效: %d %s", response.Code, response.Body.String())
	}
	if controller.appliedTheme != "" {
		t.Fatal("删除本地模板时不应向屏端发送任何主题命令")
	}
	if _, err := os.Stat(record.ManifestPath); !os.IsNotExist(err) {
		t.Fatalf("本地模板清单没有删除: %v", err)
	}
	if _, ok := findWaveshareTemplate(server.store.Get(), "waveshare-7", "delete-me"); ok {
		t.Fatal("配置中的模板记录没有删除")
	}
}

func TestRenameWaveshareTemplateOnlyChangesDisplayName(t *testing.T) {
	server, controller := newTestServer(t)
	prepareCalls := 0
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		prepareCalls++
		return manifest, nil
	}
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "night", "夜间")
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "day", "日间")
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "night"); err != nil {
		t.Fatal(err)
	}
	if err := server.store.Update(func(cfg *config.Config) error {
		if cfg.WavesharePlaylists == nil {
			cfg.WavesharePlaylists = make(map[string]config.WavesharePlaylist)
		}
		cfg.WavesharePlaylists["waveshare-7"] = config.WavesharePlaylist{
			Enabled: true, Mode: "sequence", DefaultDurationSeconds: 30,
			Entries: []config.WavesharePlaylistEntry{{TemplateID: "night"}, {TemplateID: "day"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	beforeConfig := server.store.Get()
	before, ok := findWaveshareTemplate(beforeConfig, "waveshare-7", "night")
	if !ok {
		t.Fatal("重命名前模板不存在")
	}
	beforeManifest, err := os.ReadFile(before.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(before.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	beforePrepareCalls := prepareCalls
	beforeApplyCount := controller.applyCount

	request := authorizedRequest(server, http.MethodPatch, "/api/waveshare/templates?target=waveshare-7&id=night", `{"name":"  深夜主题  "}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"深夜主题"`) || !strings.Contains(response.Body.String(), `"renamed":true`) {
		t.Fatalf("重命名响应无效: %d %s", response.Code, response.Body.String())
	}

	afterConfig := server.store.Get()
	after, ok := findWaveshareTemplate(afterConfig, "waveshare-7", "night")
	if !ok || after.Name != "深夜主题" {
		t.Fatalf("模板显示名称没有更新: %+v", after)
	}
	want := before
	want.Name = "深夜主题"
	if after != want {
		t.Fatalf("重命名修改了显示名称之外的字段:\n got=%+v\nwant=%+v", after, want)
	}
	if !reflect.DeepEqual(afterConfig.WavesharePlaylists, beforeConfig.WavesharePlaylists) {
		t.Fatalf("重命名修改了轮播引用: got=%+v want=%+v", afterConfig.WavesharePlaylists, beforeConfig.WavesharePlaylists)
	}
	if !reflect.DeepEqual(afterConfig.DeviceThemePaths, beforeConfig.DeviceThemePaths) {
		t.Fatalf("重命名修改了当前主题清单路径: got=%+v want=%+v", afterConfig.DeviceThemePaths, beforeConfig.DeviceThemePaths)
	}
	afterManifest, err := os.ReadFile(after.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(after.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterManifest, beforeManifest) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("重命名改写了模板清单文件")
	}
	if prepareCalls != beforePrepareCalls {
		t.Fatalf("重命名触发了重新编译: before=%d after=%d", beforePrepareCalls, prepareCalls)
	}
	if controller.applyCount != beforeApplyCount {
		t.Fatalf("重命名触发了设备上传: before=%d after=%d", beforeApplyCount, controller.applyCount)
	}
	if active := activeWaveshareTemplateID(afterConfig, "waveshare-7"); active != "night" {
		t.Fatalf("重命名改变了活动模板 ID: %q", active)
	}
}

func TestRenameWaveshareTemplateRejectsInvalidRequests(t *testing.T) {
	server, _ := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "night", "夜间")
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "day", "日间")
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "english", "Day")

	tests := []struct {
		name       string
		url        string
		body       string
		wantStatus int
		wantError  string
	}{
		{name: "空名称", url: "/api/waveshare/templates?target=waveshare-7&id=night", body: `{"name":"  "}`, wantStatus: http.StatusBadRequest, wantError: "模板名称不能为空"},
		{name: "同屏名称冲突", url: "/api/waveshare/templates?target=waveshare-7&id=night", body: `{"name":"  日间  "}`, wantStatus: http.StatusConflict, wantError: "同一副屏内模板名称不能重复"},
		{name: "同屏名称大小写冲突", url: "/api/waveshare/templates?target=waveshare-7&id=night", body: `{"name":"DAY"}`, wantStatus: http.StatusConflict, wantError: "同一副屏内模板名称不能重复"},
		{name: "模板不存在", url: "/api/waveshare/templates?target=waveshare-7&id=missing", body: `{"name":"新名称"}`, wantStatus: http.StatusNotFound, wantError: "未找到微雪模板"},
		{name: "未选择具体目标", url: "/api/waveshare/templates?target=all&id=night", body: `{"name":"新名称"}`, wantStatus: http.StatusBadRequest, wantError: "模板库只支持微雪副屏"},
		{name: "目标不是微雪", url: "/api/waveshare/templates?target=coolermaster-atmos-v2&id=night", body: `{"name":"新名称"}`, wantStatus: http.StatusBadRequest, wantError: "模板库只支持微雪副屏"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := server.store.Get()
			request := authorizedRequest(server, http.MethodPatch, test.url, test.body)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantError) {
				t.Fatalf("错误响应不符: %d %s", response.Code, response.Body.String())
			}
			if !reflect.DeepEqual(server.store.Get().WaveshareTemplates, before.WaveshareTemplates) {
				t.Fatal("失败的重命名修改了模板记录")
			}
		})
	}
}

func TestRenameWaveshareTemplateNameConflictIsScopedToTarget(t *testing.T) {
	server, _ := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "night", "夜间")
	source, ok := findWaveshareTemplate(server.store.Get(), "waveshare-7", "night")
	if !ok {
		t.Fatal("源模板不存在")
	}
	if err := server.store.Update(func(cfg *config.Config) error {
		cfg.Devices = append(cfg.Devices, config.Device{
			ID: "vertical-screen", Kind: "waveshare", Enabled: true, Port: "COM9", Width: 480, Height: 1920,
		})
		if cfg.WaveshareTemplates == nil {
			cfg.WaveshareTemplates = make(map[string][]config.WaveshareTemplate)
		}
		other := source
		other.ID = "vertical"
		other.Name = "竖屏"
		cfg.WaveshareTemplates["vertical-screen"] = []config.WaveshareTemplate{other}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	request := authorizedRequest(server, http.MethodPatch, "/api/waveshare/templates?target=vertical-screen&id=vertical", `{"name":"夜间"}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("另一台微雪副屏应允许使用同名模板: %d %s", response.Code, response.Body.String())
	}
	record, ok := findWaveshareTemplate(server.store.Get(), "vertical-screen", "vertical")
	if !ok || record.Name != "夜间" {
		t.Fatalf("跨设备重命名结果无效: %+v", record)
	}
}

func TestDeleteActiveWaveshareTemplateKeepsReconnectRevisionUntilNextActivation(t *testing.T) {
	server, controller := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "active", "当前模板")
	active, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", "active")
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "active"); err != nil {
		t.Fatal(err)
	}
	controller.appliedTheme = ""

	request := authorizedRequest(server, http.MethodDelete, "/api/waveshare/templates?target=waveshare-7&id=active", `{}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"local_files_retained":true`) {
		t.Fatalf("删除活动模板响应无效: %d %s", response.Code, response.Body.String())
	}
	if controller.appliedTheme != "" {
		t.Fatal("删除活动模板时不应向屏端发送任何主题命令")
	}
	cfg := server.store.Get()
	if _, ok := findWaveshareTemplate(cfg, "waveshare-7", "active"); ok {
		t.Fatal("活动模板的库记录没有删除")
	}
	if !sameLocalPath(cfg.DeviceThemePaths["waveshare-7"], active.ManifestPath) {
		t.Fatalf("重连所需活动 revision 引用丢失: %q", cfg.DeviceThemePaths["waveshare-7"])
	}
	if _, err := os.Stat(active.ManifestPath); err != nil {
		t.Fatalf("重连所需活动清单被提前删除: %v", err)
	}
	if activeID := activeWaveshareTemplateID(cfg, "waveshare-7"); activeID != "" {
		t.Fatalf("已从库删除的模板不应继续显示为活动模板: %q", activeID)
	}

	saveWaveshareTemplateForTest(t, server, "waveshare-7", "next", "下一模板")
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "next"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active.ManifestPath); !os.IsNotExist(err) {
		t.Fatalf("激活下一模板后未回收已删除的旧 revision: %v", err)
	}
}

func TestPreparedTemplateSizeCountsOnlyTFResources(t *testing.T) {
	server, _ := newTestServer(t)
	directory := t.TempDir()
	themePath := directory + string(os.PathSeparator) + "base.Theme"
	imagePath := directory + string(os.PathSeparator) + "background.png"
	if err := os.WriteFile(themePath, []byte("theme-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("embedded-image-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	themeResource, _ := theme.InspectResource("theme", themePath, "/data/theme/1024x600/demo.Theme")
	imageResource, _ := theme.InspectResource("image", imagePath, "/data/image/background.png")
	manifest := theme.Manifest{
		ID: "demo", Name: "演示", Width: 1024, Height: 600,
		Resources: []theme.Resource{themeResource, imageResource},
		Layers:    []theme.Layer{{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1}},
	}
	clearWaveshareEmbeddedResourceRemotePaths(&manifest)
	if manifest.Resources[1].RemotePath != "" {
		t.Fatal("已经嵌入 .Theme 的图片不应重复上传")
	}
	record, err := server.persistPreparedWaveshareTemplate("waveshare-7", "demo", "演示", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if record.SizeBytes != themeResource.Size {
		t.Fatalf("TF 大小应只统计驻留资源: got=%d want=%d", record.SizeBytes, themeResource.Size)
	}
}

func TestWavesharePlaylistSequenceUsesPerEntryDuration(t *testing.T) {
	server, controller := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "first", "第一套")
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "second", "第二套")
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "first"); err != nil {
		t.Fatal(err)
	}
	controller.appliedTheme = ""

	playlist := `{"enabled":true,"mode":"sequence","default_duration_seconds":3,"entries":[{"template_id":"first"},{"template_id":"second","duration_seconds":7}]}`
	request := authorizedRequest(server, http.MethodPut, "/api/waveshare/playlist?target=waveshare-7", playlist)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存轮播失败: %d %s", response.Code, response.Body.String())
	}
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	states := make(map[string]wavesharePlaylistRuntime)
	server.advanceWavesharePlaylists(context.Background(), now, states)
	if controller.appliedTheme != "" {
		t.Fatal("已有当前模板时不应立即切换")
	}
	server.advanceWavesharePlaylists(context.Background(), now.Add(2999*time.Millisecond), states)
	if controller.appliedTheme != "" {
		t.Fatal("第一项时长未到时不应切换")
	}
	server.advanceWavesharePlaylists(context.Background(), now.Add(3*time.Second), states)
	if controller.appliedTheme != "second" {
		t.Fatalf("3 秒后应切换第二项，实际 %q", controller.appliedTheme)
	}
	controller.appliedTheme = ""
	server.advanceWavesharePlaylists(context.Background(), now.Add(9*time.Second), states)
	if controller.appliedTheme != "" {
		t.Fatal("第二项独立 7 秒时长未到时不应切换")
	}
	server.advanceWavesharePlaylists(context.Background(), now.Add(10*time.Second), states)
	if controller.appliedTheme != "first" {
		t.Fatalf("第二项到期后应回到第一项，实际 %q", controller.appliedTheme)
	}
}

func TestActivatingWaveshareTemplateSynchronizesLyricsRuntimeSettings(t *testing.T) {
	server, controller := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		manifest.WaveshareCompilerRevision = theme.CurrentWaveshareCompilerRevision
		return manifest, nil
	}
	manifest := theme.Manifest{
		ID: "lyrics-local", Name: "本地歌词模板", Width: 1024, Height: 600,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{Width: 1, Height: .2}, Opacity: 1, Options: map[string]any{
				"provider": "local", "lrc_path": `D:\Music\当前歌曲.lrc`,
			}},
		},
	}
	saveWaveshareManifestForTest(t, server, "waveshare-7", "lyrics-local", "本地歌词模板", manifest)
	if _, _, err := server.applyPreparedWaveshareTemplate(context.Background(), "waveshare-7", "lyrics-local"); err != nil {
		t.Fatal(err)
	}
	cfg := server.store.Get()
	if cfg.LyricsProvider != "local" || cfg.LyricsPath != `D:\Music\当前歌曲.lrc` {
		t.Fatalf("模板歌词设置没有持久化: provider=%q path=%q", cfg.LyricsProvider, cfg.LyricsPath)
	}
	if controller.lyricsProvider != "local" || controller.lyricsPath != `D:\Music\当前歌曲.lrc` {
		t.Fatalf("模板歌词设置没有同步到运行中引擎: provider=%q path=%q", controller.lyricsProvider, controller.lyricsPath)
	}
}

func TestRetiredTitleSearchProviderInThemeMigratesToStrictAuto(t *testing.T) {
	manifest := theme.Manifest{Layers: []theme.Layer{{
		ID: "lyrics", Type: theme.LayerLyrics, Opacity: 1,
		Options: map[string]any{"provider": "sogou"},
	}}}
	settings, found, err := waveshareTemplateLyricsSettings(manifest)
	if err != nil || !found {
		t.Fatalf("读取旧主题歌词设置失败: found=%v err=%v", found, err)
	}
	if settings.provider != "auto" {
		t.Fatalf("旧标题搜索来源没有迁移到严格自动模式: %q", settings.provider)
	}
}

func TestWavesharePlaylistApplyFailureUsesBackoffAndExposesStatus(t *testing.T) {
	server, _ := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "broken", "损坏模板")
	record, _ := findWaveshareTemplate(server.store.Get(), "waveshare-7", "broken")
	if err := os.Remove(record.ManifestPath); err != nil {
		t.Fatal(err)
	}
	playlist := config.WavesharePlaylist{
		Enabled: true, Mode: "sequence", DefaultDurationSeconds: 30,
		Entries: []config.WavesharePlaylistEntry{{TemplateID: "broken"}},
	}
	if err := server.store.Update(func(cfg *config.Config) error {
		if cfg.WavesharePlaylists == nil {
			cfg.WavesharePlaylists = make(map[string]config.WavesharePlaylist)
		}
		cfg.WavesharePlaylists["waveshare-7"] = playlist
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	states := make(map[string]wavesharePlaylistRuntime)
	server.advanceWavesharePlaylists(context.Background(), now, states)
	first := states["waveshare-7"]
	if first.failures != 1 || first.retryTemplateID != "broken" || first.lastError == "" || !first.retryAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("首次失败没有进入退避: %+v", first)
	}
	server.advanceWavesharePlaylists(context.Background(), now.Add(time.Second), states)
	duringBackoff := states["waveshare-7"]
	if duringBackoff.failures != 1 || !duringBackoff.lastAttempt.Equal(now) {
		t.Fatalf("退避期仍重复应用模板: %+v", duringBackoff)
	}
	server.advanceWavesharePlaylists(context.Background(), now.Add(2*time.Second), states)
	second := states["waveshare-7"]
	if second.failures != 2 || !second.retryAt.Equal(now.Add(6*time.Second)) {
		t.Fatalf("第二次失败没有指数退避: %+v", second)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/waveshare/playlist?target=waveshare-7", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"pending_template_id":"broken"`) || !strings.Contains(response.Body.String(), `"last_error"`) {
		t.Fatalf("轮播错误状态不可见: %d %s", response.Code, response.Body.String())
	}
}

func TestWavesharePlaylistDeviceFailureUsesBackoffThenRecovers(t *testing.T) {
	server, controller := newTestServer(t)
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) { return manifest, nil }
	saveWaveshareTemplateForTest(t, server, "waveshare-7", "night", "夜间")
	controller.applyErr = errors.New("串口暂时断开")
	playlist := config.WavesharePlaylist{
		Enabled: true, Mode: "sequence", DefaultDurationSeconds: 30,
		Entries: []config.WavesharePlaylistEntry{{TemplateID: "night"}},
	}
	if err := server.store.Update(func(cfg *config.Config) error {
		if cfg.WavesharePlaylists == nil {
			cfg.WavesharePlaylists = make(map[string]config.WavesharePlaylist)
		}
		cfg.WavesharePlaylists["waveshare-7"] = playlist
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	states := make(map[string]wavesharePlaylistRuntime)
	server.advanceWavesharePlaylists(context.Background(), now, states)
	failed := states["waveshare-7"]
	if failed.failures != 1 || !strings.Contains(failed.lastError, "串口暂时断开") || controller.applyCount != 1 {
		t.Fatalf("设备错误没有进入轮播退避: state=%+v count=%d", failed, controller.applyCount)
	}
	server.advanceWavesharePlaylists(context.Background(), now.Add(time.Second), states)
	if controller.applyCount != 1 {
		t.Fatalf("退避期内重复下发模板: %d", controller.applyCount)
	}
	controller.applyErr = nil
	server.advanceWavesharePlaylists(context.Background(), now.Add(2*time.Second), states)
	recovered := states["waveshare-7"]
	if recovered.failures != 0 || recovered.lastError != "" || recovered.currentID != "night" || controller.applyCount != 2 {
		t.Fatalf("设备恢复后轮播未清除错误: state=%+v count=%d", recovered, controller.applyCount)
	}
}

func TestWavesharePlaylistRandomNeverRepeatsCurrent(t *testing.T) {
	server, _ := newTestServer(t)
	server.randomIndex = func(int) int { return 0 }
	playlist := config.WavesharePlaylist{Mode: "random", Entries: []config.WavesharePlaylistEntry{
		{TemplateID: "a", DurationSeconds: 1},
		{TemplateID: "b", DurationSeconds: 1},
		{TemplateID: "c", DurationSeconds: 1},
	}}
	for current := range playlist.Entries {
		if next := server.nextPlaylistIndex(playlist, current); next == current {
			t.Fatalf("随机轮播连续选择了相同索引 %d", current)
		}
	}
}

func TestWavesharePlaylistSchedulerStopsWithContext(t *testing.T) {
	server, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.runWavesharePlaylistScheduler(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("轮播调度器没有随服务上下文退出")
	}
}

func TestWaveshareTemplateAPIRejectsOtherDeviceKinds(t *testing.T) {
	server, _ := newTestServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/waveshare/templates?target=coolermaster-atmos-v2", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("非微雪目标应被拒绝: %d %s", response.Code, response.Body.String())
	}

	cfg := server.store.Get()
	cfg.Devices = append(cfg.Devices, config.Device{ID: "my-screen", Kind: "waveshare", Enabled: true, Port: "COM9", Width: 1024, Height: 600})
	if err := server.store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/waveshare/templates?target=my-screen", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("自定义 ID 的微雪目标应被接受: %d %s", response.Code, response.Body.String())
	}
}

func saveWaveshareTemplateForTest(t *testing.T, server *Server, target, id, name string) {
	t.Helper()
	body, err := json.Marshal(waveshareTemplateSaveRequest{
		ID: id, Name: name,
		Manifest: theme.Manifest{
			ID: id, Name: name, Width: 1, Height: 1,
			Layers: []theme.Layer{{
				ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1},
				Opacity: 1, Options: map[string]any{"color": "black"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := authorizedRequest(server, http.MethodPut, "/api/waveshare/templates?target="+target, string(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存模板失败: %d %s", response.Code, response.Body.String())
	}
}

func saveWaveshareManifestForTest(t *testing.T, server *Server, target, id, name string, manifest theme.Manifest) {
	t.Helper()
	body, err := json.Marshal(waveshareTemplateSaveRequest{ID: id, Name: name, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	request := authorizedRequest(server, http.MethodPut, "/api/waveshare/templates?target="+target, string(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("保存自定义模板失败: %d %s", response.Code, response.Body.String())
	}
}

func authorizedRequest(server *Server, method, url, body string) *http.Request {
	request := httptest.NewRequest(method, url, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	return request
}

func filepathExtension(path string) string {
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return ""
	}
	return path[index:]
}

func manifestHasThemeResource(manifest theme.Manifest) bool {
	for _, resource := range manifest.Resources {
		if strings.EqualFold(filepathExtension(resource.LocalPath), ".Theme") || strings.EqualFold(filepathExtension(resource.RemotePath), ".Theme") {
			return true
		}
	}
	return false
}

func waveshareThemeLocalPath(manifest theme.Manifest) string {
	for _, resource := range manifest.Resources {
		if strings.EqualFold(filepath.Ext(resource.LocalPath), ".Theme") {
			return resource.LocalPath
		}
	}
	return ""
}

func legacyLyricsManifestForTest(id, name string) theme.Manifest {
	return theme.Manifest{
		ID: id, Name: name, Width: 1024, Height: 600,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .1, Y: .7, Width: .8, Height: .2}, Opacity: 1},
		},
	}
}

func installDistinctLegacyLyricsUpgradeCompiler(t *testing.T, server *Server) {
	t.Helper()
	directory := t.TempDir()
	prepareCalls := 0
	server.prepareTheme = func(_ context.Context, manifest theme.Manifest) (theme.Manifest, error) {
		prepareCalls++
		if prepareCalls == 1 {
			return manifest, nil
		}
		contents := []byte("upgraded-lyrics-visibility-theme")
		path := filepath.Join(directory, "upgraded.Theme")
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return manifest, err
		}
		for index, resource := range manifest.Resources {
			if !strings.EqualFold(filepath.Ext(resource.RemotePath), ".Theme") && !strings.EqualFold(filepath.Ext(resource.LocalPath), ".Theme") {
				continue
			}
			prepared, err := theme.InspectResource(resource.ID, path, waveshareVersionedThemeRemotePath(resource.RemotePath, contents))
			if err != nil {
				return manifest, err
			}
			manifest.Resources[index] = prepared
			manifest.WaveshareCompilerRevision = theme.CurrentWaveshareCompilerRevision
			return manifest, nil
		}
		return manifest, errors.New("缺少 .Theme 资源")
	}
}
