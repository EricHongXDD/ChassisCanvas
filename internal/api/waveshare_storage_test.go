package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"casescreen/internal/device"
)

type storageRefreshController struct {
	*fakeController
	storage device.StorageInfo
	err     error
	targets []string
}

func (controller *storageRefreshController) RefreshDeviceStorage(_ context.Context, target string) (device.StorageInfo, error) {
	controller.targets = append(controller.targets, target)
	return controller.storage, controller.err
}

func TestRefreshWaveshareStorageQueriesSelectedDevice(t *testing.T) {
	server, base := newTestServer(t)
	controller := &storageRefreshController{
		fakeController: base,
		storage: device.StorageInfo{
			Supported: true, TotalBytes: 803 << 20, AvailableBytes: 734 << 20, UsedBytes: 69 << 20,
		},
	}
	server.controller = controller
	request := httptest.NewRequest(http.MethodPost, "/api/waveshare/storage/refresh?target=waveshare-7", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("主动刷新容量失败：%d %s", response.Code, response.Body.String())
	}
	if len(controller.targets) != 1 || controller.targets[0] != "waveshare-7" {
		t.Fatalf("没有只查询所选微雪设备：%v", controller.targets)
	}
	if !strings.Contains(response.Body.String(), `"available_bytes":769654784`) {
		t.Fatalf("容量响应错误：%s", response.Body.String())
	}
}

func TestRefreshWaveshareStorageRequiresAuthorizationAndWaveshareTarget(t *testing.T) {
	server, base := newTestServer(t)
	controller := &storageRefreshController{fakeController: base}
	server.controller = controller

	request := httptest.NewRequest(http.MethodPost, "/api/waveshare/storage/refresh?target=waveshare-7", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("无令牌请求未被拒绝：%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/waveshare/storage/refresh?target=coolermaster-atmos-v2", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("非微雪目标未被拒绝：%d %s", response.Code, response.Body.String())
	}
	if len(controller.targets) != 0 {
		t.Fatalf("无效请求不应访问控制器：%v", controller.targets)
	}
}

func TestRefreshWaveshareStorageReportsControllerFailures(t *testing.T) {
	server, base := newTestServer(t)
	controller := &storageRefreshController{fakeController: base, err: errors.New("串口超时")}
	server.controller = controller
	request := httptest.NewRequest(http.MethodPost, "/api/waveshare/storage/refresh?target=waveshare-7", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "串口超时") {
		t.Fatalf("容量查询错误没有返回给客户端：%d %s", response.Code, response.Body.String())
	}
}
