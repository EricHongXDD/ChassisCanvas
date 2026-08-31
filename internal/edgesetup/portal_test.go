package edgesetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeWiFi struct {
	request WiFiRequest
	calls   int
	err     error
}

func (wifi *fakeWiFi) Status(context.Context) WiFiStatus {
	return WiFiStatus{Available: true, Configured: wifi.calls > 0, Interface: "wlan0"}
}

func (wifi *fakeWiFi) Configure(_ context.Context, request WiFiRequest) error {
	wifi.calls++
	wifi.request = request
	return wifi.err
}

type fakeClock struct {
	unix int64
}

type fakePlatformCapabilities struct{}

func (fakePlatformCapabilities) Capabilities(context.Context) []PlatformCapability {
	return []PlatformCapability{{
		Platform: "qq", Name: "ignored", Ready: true, Flow: "device_code", Reason: "adapter ready",
	}}
}

func (clock *fakeClock) Status(context.Context) ClockStatus {
	return ClockStatus{Ready: clock.unix > 0, Current: time.Unix(clock.unix, 0)}
}

func (clock *fakeClock) SetUnix(_ context.Context, unix int64) error {
	clock.unix = unix
	return nil
}

func newPortalForTest(t *testing.T, wifi WiFiController, clock ClockController) *Portal {
	t.Helper()
	randomBytes := bytes.Repeat([]byte{0x5a}, 256)
	portal, err := New(Config{
		PublicURL: "http://192.168.77.2:19324", SessionTTL: 10 * time.Minute,
		Random: bytes.NewReader(randomBytes), Now: func() time.Time {
			return time.Date(2026, 8, 27, 2, 30, 0, 0, time.Local)
		}, WiFi: wifi, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return portal
}

func portalRequest(t *testing.T, portal *Portal, method, path, body string, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Host = "192.168.77.2:19324"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		request.Header.Set(csrfHeaderName, csrf)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	portal.Handler().ServeHTTP(response, request)
	return response
}

func authenticatePortal(t *testing.T, portal *Portal) ([]*http.Cookie, string) {
	t.Helper()
	bootstrapURL, err := url.Parse(portal.BootstrapFragmentURL())
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(bootstrapURL.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	token := fragment.Get("setup_token")
	if token == "" || strings.Contains(portal.PublicURL(), token) {
		t.Fatal("一次性 token 没有仅放在 URL fragment 中")
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	response := portalRequest(t, portal, http.MethodPost, "/api/session", string(body), nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("建立设置会话失败: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.CSRF == "" {
		t.Fatalf("设置会话响应无效: %v %s", err, response.Body.String())
	}
	return response.Result().Cookies(), result.CSRF
}

func TestPortalBootstrapIsOneTimeAndSecretsAreRedacted(t *testing.T) {
	portal := newPortalForTest(t, &fakeWiFi{}, &fakeClock{})
	bootstrapURL := portal.BootstrapFragmentURL()
	code := portal.BootstrapCode()
	if len(code) != 8 || !strings.Contains(bootstrapURL, "#setup_token=") {
		t.Fatalf("启动材料格式错误: url=%q code=%q", bootstrapURL, code)
	}
	if strings.Contains(portal.String(), code) || strings.Contains(portal.String(), "Wlpa") {
		t.Fatalf("Portal.String 泄露启动材料: %s", portal)
	}
	cookies, _ := authenticatePortal(t, portal)
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("设置 Cookie 缺少保护: %#v", cookies)
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	second := portalRequest(t, portal, http.MethodPost, "/api/session", string(body), nil, "")
	if second.Code != http.StatusGone || strings.Contains(second.Body.String(), code) {
		t.Fatalf("一次性设置码可重用或被回显: %d %s", second.Code, second.Body.String())
	}
}

func TestPortalWiFiRequiresSessionAndCSRFAndNeverEchoesPassphrase(t *testing.T) {
	wifi := &fakeWiFi{}
	portal := newPortalForTest(t, wifi, &fakeClock{})
	secret := "Local-WiFi-Secret-731!"
	body := `{"ssid":"home","passphrase":"` + secret + `"}`
	unauthorized := portalRequest(t, portal, http.MethodPost, "/api/wifi", body, nil, "")
	if unauthorized.Code != http.StatusForbidden || wifi.calls != 0 {
		t.Fatalf("未授权 Wi-Fi 写入没有被拒绝: %d calls=%d", unauthorized.Code, wifi.calls)
	}
	cookies, csrf := authenticatePortal(t, portal)
	withoutCSRF := portalRequest(t, portal, http.MethodPost, "/api/wifi", body, cookies, "")
	if withoutCSRF.Code != http.StatusForbidden || wifi.calls != 0 {
		t.Fatalf("无 CSRF 的 Wi-Fi 写入没有被拒绝: %d calls=%d", withoutCSRF.Code, wifi.calls)
	}
	response := portalRequest(t, portal, http.MethodPost, "/api/wifi", body, cookies, csrf)
	if response.Code != http.StatusOK || wifi.calls != 1 || wifi.request.Passphrase != secret {
		t.Fatalf("Wi-Fi 写入失败: %d calls=%d", response.Code, wifi.calls)
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(wifi.request.String(), secret) {
		t.Fatal("Wi-Fi PSK 出现在响应或格式化字符串中")
	}
}

func TestPortalDoesNotEchoSensitiveBackendErrors(t *testing.T) {
	secret := "backend-accidentally-mentioned-psk"
	wifi := &fakeWiFi{err: errors.New(secret)}
	portal := newPortalForTest(t, wifi, nil)
	cookies, csrf := authenticatePortal(t, portal)
	response := portalRequest(t, portal, http.MethodPost, "/api/wifi",
		`{"ssid":"home","passphrase":"12345678"}`, cookies, csrf)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("后端错误泄露到响应: %d %s", response.Code, response.Body.String())
	}
}

func TestPortalRejectsCredentialShapedFieldsAndPlatformAuthFailsClosed(t *testing.T) {
	wifi := &fakeWiFi{}
	portal := newPortalForTest(t, wifi, &fakeClock{})
	cookies, csrf := authenticatePortal(t, portal)
	body := `{"ssid":"home","passphrase":"12345678","account_password":"must-not-enter"}`
	response := portalRequest(t, portal, http.MethodPost, "/api/wifi", body, cookies, csrf)
	if response.Code != http.StatusBadRequest || wifi.calls != 0 {
		t.Fatalf("未知账号字段未被拒绝: %d calls=%d", response.Code, wifi.calls)
	}
	platformBody := `{"username":"alice","password":"secret","cookie":"secret"}`
	platform := portalRequest(t, portal, http.MethodPost, "/api/platform-auth/qq", platformBody, cookies, csrf)
	if platform.Code != http.StatusNotImplemented || strings.Contains(platform.Body.String(), "alice") ||
		strings.Contains(platform.Body.String(), "secret") {
		t.Fatalf("平台授权没有失败关闭或回显凭据: %d %s", platform.Code, platform.Body.String())
	}
	status := portalRequest(t, portal, http.MethodGet, "/api/platform-auth", "", cookies, "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"ready":false`) ||
		!strings.Contains(status.Body.String(), "device-code") {
		t.Fatalf("平台能力状态错误: %d %s", status.Code, status.Body.String())
	}
}

func TestPortalHostGuardAndManualClockBootstrap(t *testing.T) {
	clock := &fakeClock{}
	portal := newPortalForTest(t, &fakeWiFi{}, clock)
	wrongHost := httptest.NewRequest(http.MethodGet, "http://attacker.invalid/", nil)
	wrongResponse := httptest.NewRecorder()
	portal.Handler().ServeHTTP(wrongResponse, wrongHost)
	if wrongResponse.Code != http.StatusMisdirectedRequest {
		t.Fatalf("Host 防护状态码=%d", wrongResponse.Code)
	}
	cookies, csrf := authenticatePortal(t, portal)
	unix := time.Date(2026, 8, 27, 2, 40, 0, 0, time.UTC).Unix()
	response := portalRequest(t, portal, http.MethodPost, "/api/time",
		`{"unix_seconds":`+strconv.FormatInt(unix, 10)+`}`, cookies, csrf)
	if response.Code != http.StatusOK || clock.unix != unix {
		t.Fatalf("无 RTC 时间引导失败: %d %s", response.Code, response.Body.String())
	}
}

func TestPortalLocksAfterInvalidBootstrapAttempts(t *testing.T) {
	portal := newPortalForTest(t, nil, nil)
	for attempt := 1; attempt <= defaultBootstrapTries; attempt++ {
		response := portalRequest(t, portal, http.MethodPost, "/api/session", `{"code":"00000000"}`, nil, "")
		want := http.StatusForbidden
		if attempt == defaultBootstrapTries {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("第 %d 次错误尝试状态码=%d want=%d", attempt, response.Code, want)
		}
	}
	if portal.BootstrapCode() != "" || !strings.HasSuffix(portal.BootstrapFragmentURL(), "/") {
		t.Fatal("锁定后仍保留一次性启动材料")
	}
}

func TestPortalFinishRequiresCSRFAndSignalsCaller(t *testing.T) {
	portal := newPortalForTest(t, nil, nil)
	cookies, csrf := authenticatePortal(t, portal)
	withoutCSRF := portalRequest(t, portal, http.MethodPost, "/api/finish", `{}`, cookies, "")
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("无 CSRF 的完成请求状态码=%d", withoutCSRF.Code)
	}
	select {
	case <-portal.Done():
		t.Fatal("未授权请求提前关闭设置窗口")
	default:
	}
	response := portalRequest(t, portal, http.MethodPost, "/api/finish", `{}`, cookies, csrf)
	if response.Code != http.StatusOK {
		t.Fatalf("完成设置失败: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-portal.Done():
	default:
		t.Fatal("完成设置没有通知调用方")
	}
}

func TestPlatformCapabilityCannotBypassMissingCAAndClock(t *testing.T) {
	portal, err := New(Config{
		PublicURL:          "http://192.168.77.2:19324",
		Random:             bytes.NewReader(bytes.Repeat([]byte{0x33}, 256)),
		PlatformCapability: fakePlatformCapabilities{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies, _ := authenticatePortal(t, portal)
	response := portalRequest(t, portal, http.MethodGet, "/api/platform-auth", "", cookies, "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"ready":true`) ||
		!strings.Contains(response.Body.String(), "缺少 CA") {
		t.Fatalf("平台 capability 绕过 TLS 前置条件: %d %s", response.Code, response.Body.String())
	}
}
