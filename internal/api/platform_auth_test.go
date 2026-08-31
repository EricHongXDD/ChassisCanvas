package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlatformAuthImportStatusAndClearAreRedacted(t *testing.T) {
	server, _ := newTestServer(t)
	secret := "COOKIE_SECRET_MUST_NOT_BE_RETURNED_731"
	expires := time.Now().Add(time.Hour).Unix()
	text := fmt.Sprintf(".music.163.com\tTRUE\t/\tTRUE\t%d\tMUSIC_U\t%s", expires, secret)
	body, _ := json.Marshal(map[string]string{
		"platform": "netease", "format": "netscape", "text": text,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/platform-auth", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CaseScreen-Token", server.token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"configured"`) {
		t.Fatalf("导入登录态失败: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("登录态导入响应允许缓存: %+v", response.Header())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatal("导入响应泄露 Cookie")
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/platform-auth", nil)
	statusResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || strings.Contains(statusResponse.Body.String(), secret) ||
		!strings.Contains(statusResponse.Body.String(), `"state":"configured"`) {
		t.Fatalf("脱敏状态响应错误: %d %s", statusResponse.Code, statusResponse.Body.String())
	}
	if statusResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("登录态状态响应允许缓存: %+v", statusResponse.Header())
	}
	if err := server.platformAuth.MarkNeedsLogin("netease"); err != nil {
		t.Fatal(err)
	}
	needsLoginResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(needsLoginResponse, httptest.NewRequest(http.MethodGet, "/api/platform-auth", nil))
	if needsLoginResponse.Code != http.StatusOK || strings.Contains(needsLoginResponse.Body.String(), secret) ||
		!strings.Contains(needsLoginResponse.Body.String(), `"state":"needs_login"`) {
		t.Fatalf("平台拒绝会话后 API 没有返回脱敏 needs_login: %d %s",
			needsLoginResponse.Code, needsLoginResponse.Body.String())
	}

	clearRequest := httptest.NewRequest(http.MethodDelete, "/api/platform-auth?platform=netease", nil)
	clearRequest.Header.Set("Content-Type", "application/json")
	clearRequest.Header.Set("X-CaseScreen-Token", server.token)
	clearResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(clearResponse, clearRequest)
	if clearResponse.Code != http.StatusOK || !strings.Contains(clearResponse.Body.String(), `"state":"not_configured"`) {
		t.Fatalf("清除登录态失败: %d %s", clearResponse.Code, clearResponse.Body.String())
	}
	if clearResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("登录态清除响应允许缓存: %+v", clearResponse.Header())
	}
}

func TestPlatformAuthMutationsRequireTokenAndRejectUnsupportedPlatform(t *testing.T) {
	server, _ := newTestServer(t)
	secret := "qq-secret-not-for-response"
	body := `{"platform":"qq","format":"structured","cookies":[{"name":"qqmusic_key","value":"` + secret + `","domain":".qq.com","path":"/"}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/platform-auth", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("未授权导入状态码=%d", response.Code)
	}

	unsupported := httptest.NewRequest(http.MethodPost, "/api/platform-auth", strings.NewReader(
		`{"platform":"other","format":"structured","cookies":[]}`,
	))
	unsupported.Header.Set("Content-Type", "application/json")
	unsupported.Header.Set("X-CaseScreen-Token", server.token)
	unsupportedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(unsupportedResponse, unsupported)
	if unsupportedResponse.Code != http.StatusBadRequest || strings.Contains(unsupportedResponse.Body.String(), secret) {
		t.Fatalf("不支持平台响应错误: %d %s", unsupportedResponse.Code, unsupportedResponse.Body.String())
	}
}

func TestPlatformAuthNetscapeTextLimitIsAppliedToDecodedCookieText(t *testing.T) {
	server, _ := newTestServer(t)
	for _, test := range []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "too_large", text: strings.Repeat("x", maximumPlatformAuthTextBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"platform": "qq", "format": "netscape", "text": test.text})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/platform-auth", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CaseScreen-Token", server.token)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "256 KiB") {
				t.Fatalf("cookie.txt 正文上限错误: code=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
