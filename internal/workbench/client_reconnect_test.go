package workbench

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	localapi "casescreen/internal/api"
	"casescreen/internal/config"
	"casescreen/internal/device"
	"casescreen/internal/service"
)

func TestAuthenticatedRequestConnectsWhenTokenIsEmpty(t *testing.T) {
	var sessionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/session":
			sessionCalls.Add(1)
			_, _ = writer.Write([]byte(`{"token":"fresh-token","protocol_version":` + strconv.Itoa(localapi.ProtocolVersion) + `}`))
		case "PUT /api/config":
			if request.Header.Get("X-CaseScreen-Token") != "fresh-token" {
				http.Error(writer, "missing token", http.StatusForbidden)
				return
			}
			_, _ = writer.Write([]byte(`{"restart_required":false}`))
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	if _, err := api.SaveConfig(config.Default()); err != nil {
		t.Fatalf("空令牌没有自动连接：%v", err)
	}
	if sessionCalls.Load() != 1 || api.currentToken() != "fresh-token" {
		t.Fatalf("会话令牌状态错误：calls=%d token=%q", sessionCalls.Load(), api.currentToken())
	}
}

func TestApplyThemeAcceptsQueuedPersistedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/api/theme" || request.URL.Query().Get("target") != "waveshare-7" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		if request.Header.Get("X-CaseScreen-Token") != "local-token" {
			http.Error(writer, "missing token", http.StatusForbidden)
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"queued":true,"warning":"主题已保存；设备本次未完成应用，将在重连后自动重试","successes":0,"device_results":{"waveshare-7":"微雪屏幕未连接"}}`))
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("local-token")
	result, err := api.ApplyTheme(defaultManifest(), "waveshare-7")
	if err != nil {
		t.Fatalf("202 排队响应不应作为保存失败：%v", err)
	}
	if !result.Queued || !strings.Contains(result.Warning, "已保存") || !strings.Contains(result.Results["waveshare-7"], "未连接") {
		t.Fatalf("排队响应未完整解码：%+v", result)
	}
	message := themeApplyMessage(result)
	if !strings.Contains(message, "已保存") || !strings.Contains(message, "重连后自动重试") || strings.Contains(message, "保存失败") {
		t.Fatalf("工作台排队提示不清晰：%q", message)
	}
}

func TestExpectedApplicationVersionRejectsSameProtocolOldService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = writer.Write([]byte(`{"token":"old-token","protocol_version":` + strconv.Itoa(localapi.ProtocolVersion) + `,"application_version":"0.3.0-beta.13"}`))
	}))
	defer server.Close()

	api := NewAPI(server.URL, "0.3.0-beta.14")
	if err := api.Connect(); err == nil || !strings.Contains(err.Error(), "不是当前工作台版本") {
		t.Fatalf("同协议旧应用版本应被拒绝: %v", err)
	}
	if api.HasToken() {
		t.Fatal("版本不匹配时不得保存旧服务令牌")
	}
}

func TestForbiddenRequestRefreshesTokenAndRetriesOnlyOnce(t *testing.T) {
	var sessionCalls, configCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/session":
			sessionCalls.Add(1)
			_, _ = writer.Write([]byte(`{"token":"fresh-token","protocol_version":` + strconv.Itoa(localapi.ProtocolVersion) + `}`))
		case "PUT /api/config":
			configCalls.Add(1)
			if request.Header.Get("X-CaseScreen-Token") != "fresh-token" {
				writer.WriteHeader(http.StatusForbidden)
				_, _ = writer.Write([]byte(`{"error":"令牌失效"}`))
				return
			}
			_, _ = writer.Write([]byte(`{"restart_required":false}`))
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	api.setToken("stale-token")
	if _, err := api.SaveConfig(config.Default()); err != nil {
		t.Fatalf("403 后没有使用新令牌重试：%v", err)
	}
	if sessionCalls.Load() != 1 || configCalls.Load() != 2 || api.currentToken() != "fresh-token" {
		t.Fatalf("403 重试次数或令牌错误：session=%d config=%d token=%q", sessionCalls.Load(), configCalls.Load(), api.currentToken())
	}
}

func TestPlatformAuthImportReturnsOnlyRedactedCountsAndReusableStatus(t *testing.T) {
	secret := "client-import-secret-must-not-return"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/session":
			_, _ = writer.Write([]byte(`{"token":"local-token","protocol_version":` + strconv.Itoa(localapi.ProtocolVersion) + `}`))
		case "POST /api/platform-auth":
			data, _ := io.ReadAll(request.Body)
			if request.Header.Get("X-CaseScreen-Token") != "local-token" || !strings.Contains(string(data), secret) {
				http.Error(writer, "invalid request", http.StatusForbidden)
				return
			}
			_, _ = writer.Write([]byte(`{"accepted":2,"ignored":7,"status":{"platform":"qq","state":"configured"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	api := NewAPI(server.URL)
	result, err := api.ImportPlatformAuthNetscape("qq", "cookie-body-"+secret)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 2 || result.Ignored != 7 || result.Status.Platform != "qq" || result.Status.State != "configured" {
		t.Fatalf("客户端丢失脱敏导入结果: %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("客户端导入结果回显了 Cookie 正文")
	}
}

func TestTargetStatusReconnectsAfterServiceRecovers(t *testing.T) {
	var available atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !available.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"error":"服务启动中"}`))
			return
		}
		switch request.URL.Path {
		case "/api/status":
			_ = json.NewEncoder(writer).Encode(service.Status{Devices: []service.DeviceStatus{{Info: device.Info{
				ID: "waveshare-7", Name: "微雪 7 英寸", Resolution: [2]int{1024, 600},
				Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme, SmallTelemetry: true},
			}}}})
		case "/api/session":
			_, _ = writer.Write([]byte(`{"token":"recovered-token","protocol_version":` + strconv.Itoa(localapi.ProtocolVersion) + `}`))
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	model := &gioEditor{api: NewAPI(server.URL), uiUpdates: make(chan func(), 2)}
	model.refreshTargetStatusAsync()
	applyNextUIUpdate(t, model)
	if model.api.HasToken() || len(model.targetOptions) != 0 {
		t.Fatal("服务离线时不应伪造令牌或目标")
	}

	available.Store(true)
	model.refreshTargetStatusAsync()
	applyNextUIUpdate(t, model)
	if model.api.currentToken() != "recovered-token" || len(model.targetOptions) != 1 || model.targetOptions[0].id != "waveshare-7" {
		t.Fatalf("服务恢复后没有重新连接：token=%q options=%+v", model.api.currentToken(), model.targetOptions)
	}
	if model.message != "CaseScreen 服务已恢复，请选择目标副屏" {
		t.Fatalf("恢复提示错误：%q", model.message)
	}
}

func applyNextUIUpdate(t *testing.T, model *gioEditor) {
	t.Helper()
	select {
	case update := <-model.uiUpdates:
		update()
	case <-time.After(time.Second):
		t.Fatal("等待界面异步更新超时")
	}
}
