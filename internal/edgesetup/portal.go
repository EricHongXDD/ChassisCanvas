// Package edgesetup 提供只在首次设置窗口内开放的屏端本地门户。
//
// 门户不接收平台账号、密码或 Cookie。平台授权目前只有脱敏能力状态，
// 没有官方设备授权适配器时始终失败关闭。
package edgesetup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultPublicURL        = "http://127.0.0.1:19324"
	defaultSessionTTL       = 10 * time.Minute
	defaultBootstrapTries   = 5
	maximumJSONRequestBytes = 16 << 10
	setupCookieName         = "casescreen_setup"
	csrfHeaderName          = "X-CaseScreen-CSRF"
)

var (
	ErrPortalExpired       = errors.New("设置窗口已过期")
	ErrBootstrapConsumed   = errors.New("一次性设置令牌已使用")
	ErrBootstrapLocked     = errors.New("一次性设置令牌已锁定")
	ErrUnauthorized        = errors.New("设置会话无效")
	ErrUnsupportedPlatform = errors.New("平台没有可用的官方设备授权适配器")
)

// WiFiRequest 是门户允许接收的全部 Wi-Fi 字段。Passphrase 只传给本地控制器，
// 不会进入响应、状态或日志。
type WiFiRequest struct {
	SSID       string `json:"ssid"`
	Passphrase string `json:"-"`
	Hidden     bool   `json:"hidden,omitempty"`
}

func (request WiFiRequest) String() string {
	return fmt.Sprintf("edgesetup.WiFiRequest{ssid:%q hidden:%t passphrase:<redacted>}",
		request.SSID, request.Hidden)
}

func (request WiFiRequest) GoString() string { return request.String() }

// WiFiStatus 是不包含 SSID 和 PSK 的脱敏网络状态。
type WiFiStatus struct {
	Available  bool   `json:"available"`
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	Interface  string `json:"interface,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// WiFiController 由具体镜像适配器实现。实现不得记录 Passphrase。
type WiFiController interface {
	Status(ctx context.Context) WiFiStatus
	Configure(ctx context.Context, request WiFiRequest) error
}

// ClockStatus 描述在没有可靠 RTC 时，系统时间是否足以进行严格 TLS 校验。
type ClockStatus struct {
	Ready   bool      `json:"ready"`
	Current time.Time `json:"current"`
	Reason  string    `json:"reason,omitempty"`
}

// ClockController 允许已认证的本地浏览器显式把手机时间同步到屏端。
// 平台 TLS 仍必须使用正常 CA、主机名和有效期验证。
type ClockController interface {
	Status(ctx context.Context) ClockStatus
	SetUnix(ctx context.Context, unixSeconds int64) error
}

// TLSReadiness 描述平台 HTTPS 的最低本地前置条件。网络连通性仍由具体
// 官方适配器请求验证。
type TLSReadiness struct {
	Ready    bool   `json:"ready"`
	CABundle bool   `json:"ca_bundle"`
	Clock    bool   `json:"clock"`
	Reason   string `json:"reason,omitempty"`
}

// TLSReadinessSource 让平台 capability 受 CA 与可信时间的共同门控。
type TLSReadinessSource interface {
	Readiness(ctx context.Context) TLSReadiness
}

// PlatformCapability 只描述官方设备授权能力，不包含账号或会话凭据。
type PlatformCapability struct {
	Platform string `json:"platform"`
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	Flow     string `json:"flow,omitempty"`
	Reason   string `json:"reason"`
}

// PlatformCapabilitySource 是未来官方 device-code / QR 适配器的只读插槽。
// 当前门户故意没有平台凭据导入或授权启动接口。
type PlatformCapabilitySource interface {
	Capabilities(ctx context.Context) []PlatformCapability
}

// Config 配置一次性门户。PublicURL 必须是数字 IP 或 localhost 的 HTTP URL；
// 它应与实际监听的显式接口一致。
type Config struct {
	PublicURL          string
	SessionTTL         time.Duration
	MaxBootstrapTries  int
	Random             io.Reader
	Now                func() time.Time
	WiFi               WiFiController
	Clock              ClockController
	TLS                TLSReadinessSource
	PlatformCapability PlatformCapabilitySource
}

// Portal 是单进程、单会话的一次性设置门户。
type Portal struct {
	mu                   sync.Mutex
	doneOnce             sync.Once
	done                 chan struct{}
	publicURL            *url.URL
	createdAt            time.Time
	expiresAt            time.Time
	maxBootstrapTries    int
	bootstrapTries       int
	bootstrapConsumed    bool
	bootstrapLocked      bool
	bootstrapToken       string
	bootstrapCode        string
	sessionToken         string
	csrfToken            string
	random               io.Reader
	now                  func() time.Time
	wifi                 WiFiController
	clock                ClockController
	tls                  TLSReadinessSource
	platformCapabilities PlatformCapabilitySource
	handler              http.Handler
}

// New 创建新的门户与一次性 256 位 capability token。
func New(config Config) (*Portal, error) {
	if strings.TrimSpace(config.PublicURL) == "" {
		config.PublicURL = defaultPublicURL
	}
	publicURL, err := validatePublicURL(config.PublicURL)
	if err != nil {
		return nil, err
	}
	if config.SessionTTL == 0 {
		config.SessionTTL = defaultSessionTTL
	}
	if config.SessionTTL < time.Minute || config.SessionTTL > time.Hour {
		return nil, errors.New("设置窗口必须在 1 分钟到 1 小时之间")
	}
	if config.MaxBootstrapTries == 0 {
		config.MaxBootstrapTries = defaultBootstrapTries
	}
	if config.MaxBootstrapTries < 1 || config.MaxBootstrapTries > 20 {
		return nil, errors.New("一次性设置码尝试次数必须在 1..20 之间")
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	token, err := randomToken(config.Random, 32)
	if err != nil {
		return nil, errors.New("无法生成一次性设置令牌")
	}
	codeBytes := make([]byte, 4)
	if _, err := io.ReadFull(config.Random, codeBytes); err != nil {
		return nil, errors.New("无法生成一次性设置码")
	}
	createdAt := config.Now()
	portal := &Portal{
		done:                 make(chan struct{}),
		publicURL:            publicURL,
		createdAt:            createdAt,
		expiresAt:            createdAt.Add(config.SessionTTL),
		maxBootstrapTries:    config.MaxBootstrapTries,
		bootstrapToken:       token,
		bootstrapCode:        fmt.Sprintf("%08d", binary.BigEndian.Uint32(codeBytes)%100000000),
		random:               config.Random,
		now:                  config.Now,
		wifi:                 config.WiFi,
		clock:                config.Clock,
		tls:                  config.TLS,
		platformCapabilities: config.PlatformCapability,
	}
	portal.handler = portal.routes()
	return portal, nil
}

// Handler 返回带 Host、会话、CSRF、大小限制和安全响应头保护的 HTTP 入口。
func (portal *Portal) Handler() http.Handler { return portal.handler }

// PublicURL 返回不含任何 capability 的人工输入地址。
func (portal *Portal) PublicURL() string { return portal.publicURL.String() }

// BootstrapCode 返回显示在本地屏幕上的一次性短码。调用方不得写入持久日志。
func (portal *Portal) BootstrapCode() string {
	portal.mu.Lock()
	defer portal.mu.Unlock()
	return portal.bootstrapCode
}

// BootstrapFragmentURL 返回供本地二维码编码的 URL。令牌位于 fragment，
// 浏览器不会把它发送给 HTTP 服务器、代理或 Referer。
func (portal *Portal) BootstrapFragmentURL() string {
	portal.mu.Lock()
	defer portal.mu.Unlock()
	if portal.bootstrapToken == "" {
		return portal.publicURL.String()
	}
	copyURL := *portal.publicURL
	copyURL.Fragment = "setup_token=" + url.QueryEscape(portal.bootstrapToken)
	return copyURL.String()
}

// ExpiresAt 返回设置窗口的固定截止时间。
func (portal *Portal) ExpiresAt() time.Time { return portal.expiresAt }

// Done 在已认证用户提交“完成设置”时关闭。设置窗口到期由调用方负责。
func (portal *Portal) Done() <-chan struct{} { return portal.done }

func (portal *Portal) String() string {
	return fmt.Sprintf("edgesetup.Portal{url:%q expires:%s token:<redacted> code:<redacted>}",
		portal.publicURL.String(), portal.expiresAt.UTC().Format(time.RFC3339))
}

func (portal *Portal) GoString() string { return portal.String() }

func (portal *Portal) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", portal.index)
	mux.HandleFunc("POST /api/session", portal.createSession)
	mux.HandleFunc("GET /api/status", portal.status)
	mux.HandleFunc("POST /api/wifi", portal.configureWiFi)
	mux.HandleFunc("POST /api/time", portal.setTime)
	mux.HandleFunc("POST /api/finish", portal.finish)
	mux.HandleFunc("GET /api/platform-auth", portal.platformAuthStatus)
	mux.HandleFunc("POST /api/platform-auth/{platform}", portal.rejectPlatformAuth)
	return portal.securityHeaders(portal.hostGuard(mux))
}

func (portal *Portal) index(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_ = portalTemplate.Execute(writer, map[string]string{
		"ExpiresAt": portal.expiresAt.Local().Format("15:04:05"),
	})
}

type sessionRequest struct {
	Token string `json:"token,omitempty"`
	Code  string `json:"code,omitempty"`
}

func (portal *Portal) createSession(writer http.ResponseWriter, request *http.Request) {
	var body sessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "设置令牌格式无效")
		return
	}

	portal.mu.Lock()
	defer portal.mu.Unlock()
	if !portal.now().Before(portal.expiresAt) {
		writeError(writer, http.StatusGone, ErrPortalExpired.Error())
		return
	}
	if portal.bootstrapLocked {
		writeError(writer, http.StatusTooManyRequests, ErrBootstrapLocked.Error())
		return
	}
	if portal.bootstrapConsumed {
		writeError(writer, http.StatusGone, ErrBootstrapConsumed.Error())
		return
	}
	validToken := body.Token != "" && body.Code == "" && secretEqual(body.Token, portal.bootstrapToken)
	validCode := body.Code != "" && body.Token == "" && secretEqual(body.Code, portal.bootstrapCode)
	if !validToken && !validCode {
		portal.bootstrapTries++
		if portal.bootstrapTries >= portal.maxBootstrapTries {
			portal.bootstrapLocked = true
			portal.bootstrapToken = ""
			portal.bootstrapCode = ""
			writeError(writer, http.StatusTooManyRequests, ErrBootstrapLocked.Error())
			return
		}
		writeError(writer, http.StatusForbidden, "一次性设置令牌无效")
		return
	}

	sessionToken, err := randomToken(portal.random, 32)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "无法建立设置会话")
		return
	}
	csrfToken, err := randomToken(portal.random, 32)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "无法建立设置会话")
		return
	}
	portal.sessionToken = sessionToken
	portal.csrfToken = csrfToken
	portal.bootstrapConsumed = true
	portal.bootstrapToken = ""
	portal.bootstrapCode = ""
	http.SetCookie(writer, &http.Cookie{
		Name: setupCookieName, Value: sessionToken, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(portal.expiresAt.Sub(portal.now()).Seconds()),
	})
	writeJSON(writer, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf":          csrfToken,
		"expires_at":    portal.expiresAt,
	})
}

func (portal *Portal) status(writer http.ResponseWriter, request *http.Request) {
	csrf, ok := portal.authorized(request, false)
	if !ok {
		writeError(writer, http.StatusUnauthorized, ErrUnauthorized.Error())
		return
	}
	wifiStatus := WiFiStatus{Available: false, Reason: "无线硬件或适配器未确认"}
	if portal.wifi != nil {
		wifiStatus = portal.wifi.Status(request.Context())
	}
	clockStatus := ClockStatus{Ready: false, Current: portal.now(), Reason: "未配置可信时间源"}
	if portal.clock != nil {
		clockStatus = portal.clock.Status(request.Context())
	}
	tlsStatus := TLSReadiness{Ready: false, Reason: "未配置 CA 与可信时间探测"}
	if portal.tls != nil {
		tlsStatus = portal.tls.Readiness(request.Context())
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf":          csrf,
		"expires_at":    portal.expiresAt,
		"wifi":          wifiStatus,
		"clock":         clockStatus,
		"tls":           tlsStatus,
	})
}

func (portal *Portal) configureWiFi(writer http.ResponseWriter, request *http.Request) {
	if _, ok := portal.authorized(request, true); !ok {
		writeError(writer, http.StatusForbidden, "设置会话或 CSRF 令牌无效")
		return
	}
	if portal.wifi == nil {
		writeError(writer, http.StatusNotImplemented, "无线硬件或适配器未确认，拒绝写入配置")
		return
	}
	var body struct {
		SSID       string `json:"ssid"`
		Passphrase string `json:"passphrase"`
		Hidden     bool   `json:"hidden,omitempty"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "Wi-Fi 配置格式无效")
		return
	}
	wifiRequest := WiFiRequest{SSID: body.SSID, Passphrase: body.Passphrase, Hidden: body.Hidden}
	err := portal.wifi.Configure(request.Context(), wifiRequest)
	body.Passphrase = ""
	wifiRequest.Passphrase = ""
	if err != nil {
		// 后端错误不回显，避免未来适配器意外把 PSK 拼进错误字符串。
		writeError(writer, http.StatusBadRequest, "Wi-Fi 配置失败")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"configured": true})
}

type timeRequest struct {
	UnixSeconds int64 `json:"unix_seconds"`
}

func (portal *Portal) setTime(writer http.ResponseWriter, request *http.Request) {
	if _, ok := portal.authorized(request, true); !ok {
		writeError(writer, http.StatusForbidden, "设置会话或 CSRF 令牌无效")
		return
	}
	if portal.clock == nil {
		writeError(writer, http.StatusNotImplemented, "系统时间适配器不可用")
		return
	}
	var body timeRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "时间格式无效")
		return
	}
	requested := time.Unix(body.UnixSeconds, 0).UTC()
	if requested.Before(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		requested.After(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) {
		writeError(writer, http.StatusBadRequest, "时间超出安全范围")
		return
	}
	if err := portal.clock.SetUnix(request.Context(), body.UnixSeconds); err != nil {
		writeError(writer, http.StatusBadRequest, "设置系统时间失败")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"clock": portal.clock.Status(request.Context())})
}

func (portal *Portal) finish(writer http.ResponseWriter, request *http.Request) {
	if _, ok := portal.authorized(request, true); !ok {
		writeError(writer, http.StatusForbidden, "设置会话或 CSRF 令牌无效")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"finished": true})
	portal.doneOnce.Do(func() { close(portal.done) })
}

func (portal *Portal) platformAuthStatus(writer http.ResponseWriter, request *http.Request) {
	if _, ok := portal.authorized(request, false); !ok {
		writeError(writer, http.StatusUnauthorized, ErrUnauthorized.Error())
		return
	}
	capabilities := defaultPlatformCapabilities()
	if portal.platformCapabilities != nil {
		capabilities = sanitizeCapabilities(portal.platformCapabilities.Capabilities(request.Context()))
	}
	tlsStatus := TLSReadiness{Ready: false, Reason: "缺少 CA 或可信系统时间"}
	if portal.tls != nil {
		tlsStatus = portal.tls.Readiness(request.Context())
	}
	if !tlsStatus.Ready {
		for index := range capabilities {
			if capabilities[index].Ready {
				capabilities[index].Ready = false
				capabilities[index].Reason = "平台 HTTPS 前置条件未满足：" + tlsStatus.Reason
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"platforms": capabilities, "tls": tlsStatus})
}

func (portal *Portal) rejectPlatformAuth(writer http.ResponseWriter, request *http.Request) {
	if _, ok := portal.authorized(request, true); !ok {
		writeError(writer, http.StatusForbidden, "设置会话或 CSRF 令牌无效")
		return
	}
	// 故意不解码正文：账号、密码和 Cookie 没有进入应用对象的路径。
	writeError(writer, http.StatusNotImplemented, ErrUnsupportedPlatform.Error())
}

func (portal *Portal) authorized(request *http.Request, requireCSRF bool) (string, bool) {
	portal.mu.Lock()
	defer portal.mu.Unlock()
	if !portal.now().Before(portal.expiresAt) || portal.sessionToken == "" {
		return "", false
	}
	cookie, err := request.Cookie(setupCookieName)
	if err != nil || !secretEqual(cookie.Value, portal.sessionToken) {
		return "", false
	}
	if requireCSRF && !secretEqual(request.Header.Get(csrfHeaderName), portal.csrfToken) {
		return "", false
	}
	return portal.csrfToken, true
}

func (portal *Portal) hostGuard(next http.Handler) http.Handler {
	expected := canonicalHost(portal.publicURL.Host)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if canonicalHost(request.Host) != expected {
			writeError(writer, http.StatusMisdirectedRequest, "请求 Host 与设置入口不匹配")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (portal *Portal) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(writer, request)
	})
}

func validatePublicURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("设置入口必须是无凭据、无查询参数的 HTTP URL")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("设置入口 URL 不能包含路径")
	}
	host := parsed.Hostname()
	if host != "localhost" && net.ParseIP(host) == nil {
		return nil, errors.New("设置入口必须使用 localhost 或显式数字 IP")
	}
	if parsed.Port() == "" {
		return nil, errors.New("设置入口必须显式指定端口")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("设置入口端口无效")
	}
	parsed.Path = "/"
	return parsed, nil
}

func canonicalHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if strings.HasSuffix(host, ".") {
		host = strings.TrimSuffix(host, ".")
	}
	return host
}

func randomToken(reader io.Reader, size int) (string, error) {
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func secretEqual(left, right string) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func decodeJSON(request *http.Request, target any) error {
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
		return errors.New("Content-Type 必须是 application/json")
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, maximumJSONRequestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON 正文只能包含一个对象")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func defaultPlatformCapabilities() []PlatformCapability {
	reason := "未接入平台官方 device-code/二维码授权，已失败关闭；本门户不接收账号、密码或 Cookie"
	return []PlatformCapability{
		{Platform: "qq", Name: "QQ 音乐", Ready: false, Reason: reason},
		{Platform: "netease", Name: "网易云音乐", Ready: false, Reason: reason},
	}
}

func sanitizeCapabilities(input []PlatformCapability) []PlatformCapability {
	allowed := map[string]string{"qq": "QQ 音乐", "netease": "网易云音乐"}
	result := make([]PlatformCapability, 0, len(allowed))
	seen := make(map[string]bool)
	for _, item := range input {
		platform := strings.ToLower(strings.TrimSpace(item.Platform))
		name, ok := allowed[platform]
		if !ok || seen[platform] {
			continue
		}
		seen[platform] = true
		item.Platform = platform
		item.Name = name
		item.Flow = strings.TrimSpace(item.Flow)
		item.Reason = strings.TrimSpace(item.Reason)
		if item.Ready && item.Flow != "device_code" && item.Flow != "qr_device_authorization" {
			item.Ready = false
			item.Flow = ""
			item.Reason = "授权适配器没有声明受支持的官方设备授权流程"
		}
		if item.Reason == "" {
			item.Reason = "授权适配器未提供状态说明"
		}
		result = append(result, item)
	}
	for platform, name := range allowed {
		if !seen[platform] {
			result = append(result, PlatformCapability{
				Platform: platform, Name: name, Ready: false,
				Reason: "没有可用的官方设备授权适配器",
			})
		}
	}
	return result
}

var portalTemplate = template.Must(template.New("portal").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>CaseScreen 一次性设置</title><style>
:root{color-scheme:dark;font-family:system-ui,sans-serif;background:#081217;color:#e8f6f7}body{max-width:700px;margin:0 auto;padding:24px}section{background:#102128;border:1px solid #29434b;border-radius:14px;padding:18px;margin:14px 0}input,button{box-sizing:border-box;width:100%;padding:12px;margin:7px 0;border-radius:9px;border:1px solid #42616a;background:#071216;color:#eef}button{background:#19a7a0;border:0;font-weight:700;color:#031313}.muted{color:#9db2b7;font-size:.92rem}.error{color:#ffb6aa}.ok{color:#86e7bf}pre{white-space:pre-wrap}</style></head>
<body><h1>CaseScreen 一次性设置</h1><p class="muted">设置窗口截止 {{.ExpiresAt}}。页面不接收 QQ/网易账号、密码或 Cookie；平台未提供官方设备授权时会失败关闭。</p>
<section id="unlock"><h2>连接屏幕</h2><p>扫码会自动验证；手工打开地址时请输入屏幕显示的一次性设置码。</p><input id="code" inputmode="numeric" autocomplete="one-time-code" maxlength="8" placeholder="8 位一次性设置码"><button id="unlockButton">进入设置</button></section>
<main id="settings" hidden><section><h2>Wi-Fi</h2><p class="muted">这里只接收本地 Wi-Fi 密码；不会回显或写入日志，落盘为派生 PSK。</p><input id="ssid" autocomplete="off" maxlength="32" placeholder="Wi-Fi 名称"><input id="passphrase" type="password" autocomplete="new-password" minlength="8" maxlength="63" placeholder="Wi-Fi 密码"><label><input id="hidden" type="checkbox" style="width:auto"> 隐藏网络</label><button id="wifiButton">保存并连接</button></section>
<section><h2>系统时间</h2><p class="muted">没有可靠 RTC 时，先由已认证的本机浏览器同步时间；平台 HTTPS 仍需有效 CA 并执行完整证书校验。</p><button id="timeButton">同步本机时间</button></section>
<section><h2>平台授权</h2><pre id="platforms">读取中…</pre><p class="muted">当前没有账号、密码或 Cookie 导入入口。</p></section><section><button id="finishButton">完成设置并进入屏幕</button></section></main><p id="message"></p>
<script>
let csrf='';const $=id=>document.getElementById(id);function message(text,ok=false){$('message').className=ok?'ok':'error';$('message').textContent=text}
async function request(path,options={}){options.headers=Object.assign({'Content-Type':'application/json'},options.headers||{});if(csrf)options.headers['X-CaseScreen-CSRF']=csrf;const response=await fetch(path,options),data=await response.json().catch(()=>({error:'响应格式错误'}));if(!response.ok)throw new Error(data.error||('HTTP '+response.status));return data}
async function openSession(value,isToken){const body=isToken?{token:value}:{code:value};const data=await request('/api/session',{method:'POST',body:JSON.stringify(body)});csrf=data.csrf;history.replaceState(null,'',location.pathname);await loadStatus()}
async function loadStatus(){const status=await request('/api/status');csrf=status.csrf;$('unlock').hidden=true;$('settings').hidden=false;const auth=await request('/api/platform-auth');$('platforms').textContent=auth.platforms.map(p=>p.name+'：'+(p.ready?'可用':'不可用')+'\n'+p.reason).join('\n\n')}
$('unlockButton').onclick=()=>openSession($('code').value.trim(),false).catch(e=>message(e.message));$('wifiButton').onclick=async()=>{const passphrase=$('passphrase').value;try{await request('/api/wifi',{method:'POST',body:JSON.stringify({ssid:$('ssid').value,passphrase,hidden:$('hidden').checked})});$('passphrase').value='';message('Wi-Fi 配置已保存，网络切换时页面可能断开。',true)}catch(e){$('passphrase').value='';message(e.message)}};$('timeButton').onclick=()=>request('/api/time',{method:'POST',body:JSON.stringify({unix_seconds:Math.floor(Date.now()/1000)})}).then(()=>message('系统时间已同步。',true)).catch(e=>message(e.message));$('finishButton').onclick=()=>request('/api/finish',{method:'POST',body:'{}'}).then(()=>message('设置完成，屏幕正在启动。',true)).catch(e=>message(e.message));
const fragment=new URLSearchParams(location.hash.slice(1)),token=fragment.get('setup_token');if(token){openSession(token,true).catch(e=>message(e.message))}else{loadStatus().catch(()=>{})}
</script></body></html>`))
