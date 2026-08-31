package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/device"
	caseTheme "casescreen/internal/theme"
)

func TestWaveshareLibraryClientContract(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.RequestURI())
		if request.URL.Query().Get("target") != "waveshare-8.8" {
			http.Error(writer, "target", http.StatusBadRequest)
			return
		}
		if request.Method != http.MethodGet && request.Header.Get("X-CaseScreen-Token") != "desktop-token" {
			http.Error(writer, "token", http.StatusForbidden)
			return
		}
		if request.Method != http.MethodGet && !strings.HasPrefix(request.Header.Get("Content-Type"), "application/json") {
			http.Error(writer, "content-type", http.StatusUnsupportedMediaType)
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/waveshare/templates":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"target": "waveshare-8.8", "active_template_id": "night",
				"templates": []config.WaveshareTemplate{{ID: "night", Name: "夜间"}},
			})
		case "PUT /api/waveshare/templates":
			var body struct {
				ID       string             `json:"id"`
				Name     string             `json:"name"`
				Manifest caseTheme.Manifest `json:"manifest"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("解码模板请求失败：%v", err)
			}
			if !strings.HasPrefix(body.ID, "theme-") || body.Name != "新模板" || body.Manifest.Width != 480 {
				t.Fatalf("模板请求错误：%+v", body)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"template": config.WaveshareTemplate{ID: body.ID, Name: body.Name}})
		case "PATCH /api/waveshare/templates":
			if request.URL.Query().Get("id") != "night" {
				t.Fatalf("重命名模板 ID 错误：%s", request.URL.RawQuery)
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Name != "深夜" {
				t.Fatalf("重命名模板请求错误：%+v, %v", body, err)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"template": config.WaveshareTemplate{ID: "night", Name: body.Name}})
		case "POST /api/waveshare/templates/activate":
			if request.URL.Query().Get("id") != "night" {
				t.Fatalf("激活模板 ID 错误：%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"successes": 1})
		case "DELETE /api/waveshare/templates":
			if request.URL.Query().Get("id") != "night" {
				t.Fatalf("删除模板 ID 错误：%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"deleted": "night"})
		case "GET /api/waveshare/playlist":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"playlist": config.WavesharePlaylist{Enabled: true, Mode: "random", DefaultDurationSeconds: 18, Entries: []config.WavesharePlaylistEntry{{TemplateID: "night"}}},
				"runtime":  WavesharePlaylistRuntime{Running: true, CurrentTemplateID: "night"},
			})
		case "PUT /api/waveshare/playlist":
			var playlist config.WavesharePlaylist
			if err := json.NewDecoder(request.Body).Decode(&playlist); err != nil || playlist.Mode != "sequence" || playlist.DefaultDurationSeconds != 30 || len(playlist.Entries) != 1 || playlist.Entries[0].DurationSeconds != 0 {
				t.Fatalf("轮播请求错误：%+v, %v", playlist, err)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"playlist": playlist})
		default:
			http.Error(writer, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.Token = "desktop-token"
	library, err := api.WaveshareTemplates("waveshare-8.8")
	if err != nil || library.ActiveTemplateID != "night" || len(library.Templates) != 1 {
		t.Fatalf("读取模板库失败：%+v, %v", library, err)
	}
	if _, err = api.SaveWaveshareTemplate("waveshare-8.8", "", "新模板", caseTheme.Manifest{Width: 480, Height: 1920}); err != nil {
		t.Fatalf("保存模板失败：%v", err)
	}
	renamed, err := api.RenameWaveshareTemplate("waveshare-8.8", "night", "  深夜  ")
	if err != nil || renamed.ID != "night" || renamed.Name != "深夜" {
		t.Fatalf("重命名模板失败：%+v, %v", renamed, err)
	}
	if err = api.ActivateWaveshareTemplate("waveshare-8.8", "night"); err != nil {
		t.Fatalf("激活模板失败：%v", err)
	}
	if err = api.DeleteWaveshareTemplate("waveshare-8.8", "night"); err != nil {
		t.Fatalf("删除模板失败：%v", err)
	}
	playlist, runtime, err := api.WavesharePlaylistState("waveshare-8.8")
	if err != nil || !playlist.Enabled || playlist.Mode != "random" || playlist.DefaultDurationSeconds != 18 || !runtime.Running || runtime.CurrentTemplateID != "night" {
		t.Fatalf("读取轮播失败：playlist=%+v runtime=%+v err=%v", playlist, runtime, err)
	}
	if err = api.SaveWavesharePlaylist("waveshare-8.8", config.WavesharePlaylist{Mode: "sequence", DefaultDurationSeconds: 30, Entries: []config.WavesharePlaylistEntry{{TemplateID: "night"}}}); err != nil {
		t.Fatalf("保存轮播失败：%v", err)
	}
	if len(calls) != 7 {
		t.Fatalf("请求数量错误：%v", calls)
	}
}

func TestRenameWaveshareTemplateClientRejectsInvalidInputLocally(t *testing.T) {
	api := NewAPI("http://127.0.0.1:1")
	for _, test := range []struct {
		name       string
		target     string
		templateID string
		newName    string
	}{
		{name: "未选择目标", target: "all", templateID: "night", newName: "深夜"},
		{name: "模板 ID 为空", target: "waveshare-7", templateID: "  ", newName: "深夜"},
		{name: "模板名称为空", target: "waveshare-7", templateID: "night", newName: "  "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := api.RenameWaveshareTemplate(test.target, test.templateID, test.newName); err == nil {
				t.Fatal("无效输入应在发起请求前被拒绝")
			}
		})
	}
}

func TestWaveshareStorageDecodesOptionalStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = writer.Write([]byte(`{"devices":[{"info":{"id":"waveshare-7"},"storage":{"supported":true,"total_bytes":8589934592,"available_bytes":6442450944,"used_bytes":2147483648}}]}`))
	}))
	defer server.Close()
	storage, found, err := NewAPI(server.URL).WaveshareStorage("waveshare-7")
	if err != nil || !found || storage == nil || storage.UsedBytes != 2*1024*1024*1024 {
		t.Fatalf("容量状态解码失败：found=%v storage=%+v err=%v", found, storage, err)
	}
	if got := formatStorageBytes(storage.AvailableBytes); got != "6.00 GiB" {
		t.Fatalf("容量格式化错误：%s", got)
	}
}

func TestWaveshareStorageStatusKeepsLastValidCapacityOnRefreshError(t *testing.T) {
	storage := &device.StorageInfo{
		Supported:      true,
		TotalBytes:     803 << 20,
		AvailableBytes: 730 << 20,
		UsedBytes:      73 << 20,
		Error:          "读取 TF 卡容量: context deadline exceeded",
	}
	status := waveshareStorageStatus(storage, true)
	if !strings.Contains(status, "803.0 MiB") || !strings.Contains(status, "730.0 MiB") || !strings.Contains(status, "上次有效数据") {
		t.Fatalf("刷新失败时没有保留最近有效容量：%s", status)
	}

	status = waveshareStorageStatus(&device.StorageInfo{Error: "设备未响应"}, true)
	if !strings.Contains(status, "容量刷新失败") || !strings.Contains(status, "设备未响应") {
		t.Fatalf("没有有效容量时应显示查询错误：%s", status)
	}
}

func TestRefreshWaveshareStoragePostsAuthenticatedDeviceQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/waveshare/storage/refresh" || request.URL.Query().Get("target") != "waveshare-7" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		if request.Header.Get("X-CaseScreen-Token") != "desktop-token" || !strings.HasPrefix(request.Header.Get("Content-Type"), "application/json") {
			http.Error(writer, "missing authorization", http.StatusForbidden)
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"target": "waveshare-7",
			"storage": device.StorageInfo{
				Supported: true, TotalBytes: 803 << 20, AvailableBytes: 734 << 20, UsedBytes: 69 << 20,
			},
		})
	}))
	defer server.Close()
	api := NewAPI(server.URL)
	api.setToken("desktop-token")
	storage, err := api.RefreshWaveshareStorage("waveshare-7")
	if err != nil || storage == nil || !storage.Supported || storage.AvailableBytes != 734<<20 {
		t.Fatalf("客户端没有取得主动刷新结果：storage=%+v err=%v", storage, err)
	}
}

func TestRefreshWaveshareStorageButtonUsesActiveEndpoint(t *testing.T) {
	var activeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/waveshare/playlist":
			_ = json.NewEncoder(writer).Encode(map[string]any{"playlist": config.WavesharePlaylist{}, "runtime": WavesharePlaylistRuntime{}})
		case "POST /api/waveshare/storage/refresh":
			activeCalls++
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"storage": device.StorageInfo{Supported: true, TotalBytes: 803 << 20, AvailableBytes: 734 << 20, UsedBytes: 69 << 20},
			})
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("desktop-token")
	model := &gioEditor{api: api, target: "waveshare-7", uiUpdates: make(chan func(), 1)}
	model.waveshareLibrary.reset("waveshare-7")
	model.refreshWaveshareStorage()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("刷新容量按钮请求没有完成")
	}
	if activeCalls != 1 {
		t.Fatalf("刷新按钮没有调用主动查询接口：%d", activeCalls)
	}
	if model.waveshareLibrary.storage == nil || model.waveshareLibrary.storage.AvailableBytes != 734<<20 {
		t.Fatalf("界面没有采用主动刷新结果：%+v", model.waveshareLibrary.storage)
	}
	if !strings.Contains(model.message, "已向设备查询") {
		t.Fatalf("刷新提示未说明已查询设备：%q", model.message)
	}
}

func TestRefreshWaveshareSettingsStatusUsesCacheAndKeepsPlaylistDraft(t *testing.T) {
	var activeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/waveshare/playlist":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"playlist": config.WavesharePlaylist{Entries: []config.WavesharePlaylistEntry{{TemplateID: "saved"}}},
				"runtime":  WavesharePlaylistRuntime{Running: true, CurrentTemplateID: "saved"},
			})
		case "GET /api/status":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"devices": []map[string]any{{
					"info":    device.Info{ID: "waveshare-7"},
					"storage": device.StorageInfo{Supported: true, TotalBytes: 803 << 20, AvailableBytes: 730 << 20, UsedBytes: 73 << 20},
				}},
			})
		case "POST /api/waveshare/storage/refresh":
			activeCalls++
			http.Error(writer, "后台同步不应主动访问设备", http.StatusInternalServerError)
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), target: "waveshare-7", uiUpdates: make(chan func(), 1)}
	model.waveshareLibrary.reset("waveshare-7")
	model.waveshareLibrary.applyServerPlaylist("waveshare-7", config.WavesharePlaylist{
		Mode: "sequence", DefaultDurationSeconds: 30,
		Entries: []config.WavesharePlaylistEntry{{TemplateID: "saved"}},
	})
	model.waveshareLibrary.lastRefresh = time.Now().Add(-time.Minute)
	model.waveshareLibrary.playlist = config.WavesharePlaylist{
		Enabled: true, Mode: "random", Entries: []config.WavesharePlaylistEntry{{TemplateID: "draft", DurationSeconds: 45}},
	}
	model.waveshareLibrary.durationFields = make(map[string]*textField)
	model.waveshareLibrary.durationField("draft", 45)
	model.refreshWaveshareSettingsStatus()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("设置页后台状态同步没有完成")
	}
	if activeCalls != 0 {
		t.Fatalf("设置页后台同步错误调用了主动设备接口：%d", activeCalls)
	}
	if entries := model.waveshareLibrary.playlist.Entries; len(entries) != 1 || entries[0].TemplateID != "draft" || entries[0].DurationSeconds != 45 {
		t.Fatalf("后台状态同步覆盖了未保存轮播草稿：%+v", entries)
	}
	if !model.waveshareLibrary.playlistRuntime.Running || model.waveshareLibrary.playlistRuntime.CurrentTemplateID != "saved" {
		t.Fatalf("后台状态同步没有更新运行态：%+v", model.waveshareLibrary.playlistRuntime)
	}
	if storage := model.waveshareLibrary.storage; storage == nil || storage.AvailableBytes != 730<<20 {
		t.Fatalf("后台状态同步没有采用缓存容量：%+v", storage)
	}
}

func TestWaveshareSettingsStatusRecoversAfterInitialPlaylistFailure(t *testing.T) {
	var playlistCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/waveshare/templates":
			_ = json.NewEncoder(writer).Encode(map[string]any{"target": "waveshare-7", "templates": []config.WaveshareTemplate{}})
		case "GET /api/waveshare/playlist":
			playlistCalls++
			if playlistCalls == 1 {
				http.Error(writer, "temporary", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"playlist": config.WavesharePlaylist{
					Enabled: true, Mode: "random", DefaultDurationSeconds: 18,
					Entries: []config.WavesharePlaylistEntry{{TemplateID: "day"}},
				},
				"runtime": WavesharePlaylistRuntime{Running: true, CurrentTemplateID: "day"},
			})
		case "GET /api/status":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"devices": []map[string]any{{"info": device.Info{ID: "waveshare-7"}}},
			})
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), target: "waveshare-7", uiUpdates: make(chan func(), 1)}
	model.waveshareLibrary.reset("waveshare-7")
	model.refreshWaveshareLibrary()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("初次模板库刷新没有完成")
	}
	if model.waveshareLibrary.playlistBaselineReady {
		t.Fatal("轮播接口失败后不应把占位配置标记为已加载")
	}
	if model.waveshareLibrary.lastRefresh.IsZero() {
		t.Fatal("初次刷新完成时间没有记录，设置页无法触发后续轻量重试")
	}

	model.refreshWaveshareSettingsStatus()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("设置页轻量重试没有完成")
	}
	state := &model.waveshareLibrary
	if !state.playlistBaselineReady || !state.playlist.Enabled || state.playlist.Mode != "random" || len(state.playlist.Entries) != 1 || state.playlist.Entries[0].TemplateID != "day" {
		t.Fatalf("轻量重试没有恢复轮播配置：ready=%v playlist=%+v", state.playlistBaselineReady, state.playlist)
	}
	if !state.playlistRuntime.Running || state.playlistRuntime.CurrentTemplateID != "day" {
		t.Fatalf("轻量重试没有恢复轮播运行态：%+v", state.playlistRuntime)
	}
	if playlistCalls != 2 {
		t.Fatalf("轮播接口重试次数错误：%d", playlistCalls)
	}
}

func TestReloadWaveshareLibraryDoesNotOverwritePlaylistDraft(t *testing.T) {
	var templateCalls, playlistCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.URL.Path {
		case "/api/waveshare/templates":
			templateCalls++
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"target": "waveshare-7", "templates": []config.WaveshareTemplate{{ID: "new", Name: "新模板"}},
			})
		case "/api/waveshare/playlist":
			playlistCalls++
			_ = json.NewEncoder(writer).Encode(map[string]any{"playlist": config.WavesharePlaylist{}})
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), target: "waveshare-7", uiUpdates: make(chan func(), 1)}
	model.waveshareLibrary.reset("waveshare-7")
	model.waveshareLibrary.lastRefresh = time.Now()
	model.waveshareLibrary.playlist = config.WavesharePlaylist{
		Enabled: true, Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "draft", DurationSeconds: 45}},
	}
	model.reloadWaveshareLibrary()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("模板库刷新没有完成")
	}
	if templateCalls != 1 || playlistCalls != 0 {
		t.Fatalf("模板页刷新访问了错误接口：templates=%d playlist=%d", templateCalls, playlistCalls)
	}
	if entries := model.waveshareLibrary.playlist.Entries; len(entries) != 1 || entries[0].TemplateID != "draft" || entries[0].DurationSeconds != 45 {
		t.Fatalf("模板页刷新覆盖了未保存轮播草稿：%+v", entries)
	}
	if len(model.waveshareLibrary.templates) != 1 || model.waveshareLibrary.templates[0].ID != "new" {
		t.Fatalf("模板库没有刷新：%+v", model.waveshareLibrary.templates)
	}
}

func TestSaveWaveshareTemplateKeepsPlaylistDraft(t *testing.T) {
	var templatePuts, templateGets, playlistGets int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "PUT /api/waveshare/templates":
			templatePuts++
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"template": config.WaveshareTemplate{ID: "created", Name: "新模板"},
			})
		case "GET /api/waveshare/templates":
			templateGets++
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"target": "waveshare-7", "templates": []config.WaveshareTemplate{{ID: "created", Name: "新模板"}},
			})
		case "GET /api/waveshare/playlist":
			playlistGets++
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"playlist": config.WavesharePlaylist{Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "saved"}}},
			})
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("desktop-token")
	model := &gioEditor{
		api:       api,
		target:    "waveshare-7",
		manifest:  defaultManifestFor(1024, 600),
		uiUpdates: make(chan func(), 2),
	}
	model.waveshareLibrary.reset("waveshare-7")
	model.waveshareLibrary.lastRefresh = time.Now()
	model.waveshareLibrary.playlist = config.WavesharePlaylist{
		Enabled: true, Mode: "random", Entries: []config.WavesharePlaylistEntry{{TemplateID: "draft", DurationSeconds: 45}},
	}
	model.waveshareLibrary.templateName.set("新模板")
	model.saveCurrentWaveshareTemplate()
	for index := 0; index < 2; index++ {
		select {
		case update := <-model.uiUpdates:
			update()
		case <-time.After(time.Second):
			t.Fatalf("模板保存后的第 %d 个界面更新没有完成", index+1)
		}
	}
	if templatePuts != 1 || templateGets != 1 || playlistGets != 0 {
		t.Fatalf("模板保存刷新了错误接口：PUT=%d GET=%d playlist=%d", templatePuts, templateGets, playlistGets)
	}
	if entries := model.waveshareLibrary.playlist.Entries; len(entries) != 1 || entries[0].TemplateID != "draft" || entries[0].DurationSeconds != 45 {
		t.Fatalf("模板保存覆盖了未保存轮播草稿：%+v", entries)
	}
	if len(model.waveshareLibrary.templates) != 1 || model.waveshareLibrary.templates[0].ID != "created" {
		t.Fatalf("模板保存后没有刷新模板清单：%+v", model.waveshareLibrary.templates)
	}
}

func TestWavesharePlaylistDraftSurvivesTargetSwitch(t *testing.T) {
	state := waveshareLibraryState{}
	state.reset("waveshare-7")
	state.applyServerPlaylist("waveshare-7", config.WavesharePlaylist{
		Mode: "sequence", DefaultDurationSeconds: 30,
		Entries: []config.WavesharePlaylistEntry{{TemplateID: "day"}},
	})
	state.playlist.Enabled = true
	state.playlist.Mode = "random"
	state.playlist.Entries = append(state.playlist.Entries, config.WavesharePlaylistEntry{TemplateID: "night", DurationSeconds: 45})
	state.defaultDuration.set("18")
	state.durationField("night", 45).set("47")
	state.rememberPlaylistDraft()

	state.reset("waveshare-8.8")
	state.applyServerPlaylist("waveshare-8.8", config.WavesharePlaylist{
		Mode: "sequence", DefaultDurationSeconds: 25,
		Entries: []config.WavesharePlaylistEntry{{TemplateID: "vertical"}},
	})
	if state.playlist.Mode != "sequence" || len(state.playlist.Entries) != 1 || state.playlist.Entries[0].TemplateID != "vertical" {
		t.Fatalf("另一目标错误复用了 7 英寸草稿：%+v", state.playlist)
	}

	state.rememberPlaylistDraft()
	state.reset("waveshare-7")
	state.applyServerPlaylist("waveshare-7", config.WavesharePlaylist{
		Mode: "sequence", DefaultDurationSeconds: 30,
		Entries: []config.WavesharePlaylistEntry{{TemplateID: "day"}},
	})
	if !state.playlist.Enabled || state.playlist.Mode != "random" || len(state.playlist.Entries) != 2 || state.playlist.Entries[1].TemplateID != "night" {
		t.Fatalf("切回目标后没有恢复未保存轮播草稿：%+v", state.playlist)
	}
	if state.defaultDuration.editor.Text() != "18" || state.durationField("night", 0).editor.Text() != "47" {
		t.Fatalf("切回目标后没有恢复时长原文：default=%q night=%q", state.defaultDuration.editor.Text(), state.durationField("night", 0).editor.Text())
	}
	if !state.playlistDirty() {
		t.Fatal("恢复的目标草稿应继续标记为未保存")
	}
}

func TestSaveWavesharePlaylistKeepsEditsMadeWhileRequestIsRunning(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/api/waveshare/playlist" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		close(requestStarted)
		<-releaseResponse
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("desktop-token")
	model := &gioEditor{api: api, target: "waveshare-7", uiUpdates: make(chan func(), 1)}
	model.waveshareLibrary.reset("waveshare-7")
	model.waveshareLibrary.applyServerPlaylist("waveshare-7", config.WavesharePlaylist{
		Mode: "sequence", DefaultDurationSeconds: 30,
		Entries: []config.WavesharePlaylistEntry{{TemplateID: "day"}},
	})
	model.waveshareLibrary.playlist.Enabled = true
	model.saveWavesharePlaylist()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("轮播保存请求没有开始")
	}

	model.waveshareLibrary.playlist.Mode = "random"
	model.waveshareLibrary.playlist.Entries = append(model.waveshareLibrary.playlist.Entries, config.WavesharePlaylistEntry{TemplateID: "night"})
	model.waveshareLibrary.defaultDuration.set("18")
	model.waveshareLibrary.durationField("night", 0).set("47")
	close(releaseResponse)
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("轮播保存回调没有完成")
	}

	state := &model.waveshareLibrary
	if state.playlist.Mode != "random" || len(state.playlist.Entries) != 2 || state.playlist.Entries[1].TemplateID != "night" {
		t.Fatalf("保存回调覆盖了请求期间的后续编辑：%+v", state.playlist)
	}
	if state.defaultDuration.editor.Text() != "18" || state.durationField("night", 0).editor.Text() != "47" {
		t.Fatalf("保存回调覆盖了请求期间的时长输入：default=%q night=%q", state.defaultDuration.editor.Text(), state.durationField("night", 0).editor.Text())
	}
	if !state.playlistDirty() || !strings.Contains(model.message, "仍未保存") {
		t.Fatalf("后续编辑没有保持未保存状态：dirty=%v message=%q", state.playlistDirty(), model.message)
	}
	if state.playlistBaseline.Playlist.Mode != "sequence" || len(state.playlistBaseline.Playlist.Entries) != 1 {
		t.Fatalf("已保存基线没有采用请求快照：%+v", state.playlistBaseline)
	}
}

func TestSaveWavesharePlaylistIgnoresCallbackAfterTargetRoundTrip(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/api/waveshare/playlist" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		close(requestStarted)
		<-releaseResponse
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("desktop-token")
	model := &gioEditor{api: api, target: "waveshare-7", uiUpdates: make(chan func(), 1)}
	state := &model.waveshareLibrary
	state.reset("waveshare-7")
	state.applyServerPlaylist("waveshare-7", config.WavesharePlaylist{Mode: "sequence", DefaultDurationSeconds: 30})
	state.playlist.Enabled = true
	state.playlist.Entries = []config.WavesharePlaylistEntry{{TemplateID: "day"}}
	state.durationField("day", 0)
	state.defaultDuration.set("18")
	model.saveWavesharePlaylist()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("轮播保存请求没有开始")
	}

	state.rememberPlaylistDraft()
	model.target = "waveshare-8.8"
	state.reset("waveshare-8.8")
	state.applyServerPlaylist("waveshare-8.8", config.WavesharePlaylist{Mode: "sequence", DefaultDurationSeconds: 25})
	state.rememberPlaylistDraft()
	model.target = "waveshare-7"
	state.reset("waveshare-7")
	before := state.playlistDrafts["waveshare-7"]
	close(releaseResponse)
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("跨目标轮播保存回调没有完成")
	}
	if state.playlistBaselineReady {
		t.Fatal("旧保存回调不应把重新加载中的占位状态标记为已加载")
	}
	after := state.playlistDrafts["waveshare-7"]
	if !reflect.DeepEqual(after, before) || len(after.Playlist.Entries) != 1 || after.Playlist.Entries[0].TemplateID != "day" || after.DefaultDuration != "18" {
		t.Fatalf("旧保存回调覆盖了原目标草稿：before=%+v after=%+v", before, after)
	}
}

func TestWavesharePlaylistDirtyNormalizesEmptyEntries(t *testing.T) {
	state := waveshareLibraryState{}
	state.reset("waveshare-7")
	state.applyServerPlaylist("waveshare-7", config.WavesharePlaylist{Mode: "sequence", DefaultDurationSeconds: 30})
	state.playlist.Entries = append(state.playlist.Entries, config.WavesharePlaylistEntry{TemplateID: "day"})
	state.durationField("day", 0)
	state.playlist.Entries = state.playlist.Entries[:0]
	delete(state.durationFields, "day")
	if state.playlistDirty() {
		t.Fatal("选择再取消最后一个模板后不应因 nil/空切片差异误报未保存")
	}
}

func TestWavesharePlaylistVisualEditingKeepsOrderAndDuration(t *testing.T) {
	model := &gioEditor{target: "waveshare-7"}
	model.waveshareLibrary.init()
	model.toggleWavesharePlaylistEntry("day")
	model.toggleWavesharePlaylistEntry("night")
	model.moveWavesharePlaylistEntry("night", -1)
	if len(model.waveshareLibrary.playlist.Entries) != 2 || model.waveshareLibrary.playlist.Entries[0].TemplateID != "night" {
		t.Fatalf("轮播顺序调整失败：%+v", model.waveshareLibrary.playlist.Entries)
	}
	if field := model.waveshareLibrary.durationField("night", 45); field.editor.Text() != "" {
		// night 加入时已创建“继承统一时长”的空编辑器，后续重绘不应覆盖用户输入。
		t.Fatalf("时长编辑器未保持当前值：%s", field.editor.Text())
	}
	model.toggleWavesharePlaylistEntry("day")
	if model.waveshareLibrary.playlistEntryIndex("day") >= 0 {
		t.Fatal("移除轮播项失败")
	}
}

func TestWavesharePlaylistSnapshotUsesGlobalDurationAndOptionalOverrides(t *testing.T) {
	state := waveshareLibraryState{
		playlist: config.WavesharePlaylist{
			Enabled: true,
			Mode:    "sequence",
			Entries: []config.WavesharePlaylistEntry{{TemplateID: "day"}, {TemplateID: "night", DurationSeconds: 45}},
		},
	}
	state.init()
	state.defaultDuration.set("12")
	state.durationField("day", 0).set("")
	state.durationField("night", 45).set("90")
	playlist, err := state.playlistSnapshot()
	if err != nil {
		t.Fatalf("生成轮播快照失败：%v", err)
	}
	if playlist.DefaultDurationSeconds != 12 || playlist.Entries[0].DurationSeconds != 0 || playlist.Entries[1].DurationSeconds != 90 {
		t.Fatalf("轮播时长快照错误：%+v", playlist)
	}

	state.defaultDuration.set("0")
	if _, err = state.playlistSnapshot(); err == nil {
		t.Fatal("统一时长为 0 时应拒绝保存")
	}
	state.defaultDuration.set("12")
	state.durationField("night", 45).set("abc")
	if _, err = state.playlistSnapshot(); err == nil {
		t.Fatal("单模板时长非数字时应拒绝保存")
	}
}

func TestWavesharePlaylistRuntimeTextShowsRetry(t *testing.T) {
	retryAt := time.Date(2026, 8, 26, 14, 30, 5, 0, time.Local)
	text := wavesharePlaylistRuntimeText(WavesharePlaylistRuntime{LastError: "清单不存在", PendingTemplateID: "night", RetryAt: retryAt})
	if !strings.Contains(text, "清单不存在") || !strings.Contains(text, "night") || !strings.Contains(text, "14:30:05") {
		t.Fatalf("轮播错误状态不可见：%s", text)
	}
}

func TestCloneWavesharePlaylistDoesNotShareEntries(t *testing.T) {
	source := config.WavesharePlaylist{Enabled: true, Mode: "sequence", Entries: []config.WavesharePlaylistEntry{{TemplateID: "night", DurationSeconds: 30}}}
	cloned := cloneWavesharePlaylist(source)
	cloned.Entries[0].DurationSeconds = 90
	cloned.Entries = append(cloned.Entries, config.WavesharePlaylistEntry{TemplateID: "day", DurationSeconds: 45})
	if source.Entries[0].DurationSeconds != 30 || len(source.Entries) != 1 {
		t.Fatalf("异步保存快照仍与界面轮播切片共享存储：source=%+v clone=%+v", source, cloned)
	}
}

func TestWaveshareLibraryRejectsOtherTargets(t *testing.T) {
	api := NewAPI("http://127.0.0.1:1")
	if _, err := api.WaveshareTemplates("all"); err == nil {
		t.Fatal("all 不应进入微雪模板库 API")
	}
	model := &gioEditor{target: "thermalright-kv", inspectorTab: "waveshare-library"}
	if model.isWaveshareTarget() {
		t.Fatal("利民目标被误判为微雪")
	}
	model.target = "desk-panel"
	model.targetOptions = []targetOption{{id: "desk-panel", waveshare: true}}
	if !model.isWaveshareTarget() {
		t.Fatal("自定义 ID 的微雪目标未通过能力识别")
	}
}
