// Package platformauth 安全保存并注入 QQ 音乐和网易云音乐的已登录会话。
//
// 本包只接受用户一次性导入的 Cookie，不接收、不保存账号或密码。
package platformauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Platform 是受支持的音乐平台。
type Platform string

const (
	PlatformQQMusic Platform = "qq"
	PlatformNetease Platform = "netease"
)

// SessionState 是不会泄露 Cookie 的登录态摘要。
type SessionState string

const (
	StateNotConfigured SessionState = "not_configured"
	StateConfigured    SessionState = "configured"
	StateExpired       SessionState = "expired"
	StateNeedsLogin    SessionState = "needs_login"
)

var (
	ErrUnsupportedPlatform = errors.New("仅支持 QQ 音乐和网易云音乐会话")
	ErrDisallowedTarget    = errors.New("拒绝向非平台 HTTPS 主机注入会话")
	ErrNoAuthCookies       = errors.New("导入内容中没有该平台允许的登录 Cookie")
	ErrInvalidCookieInput  = errors.New("Cookie 导入格式无效")
)

var platformPolicies = map[Platform]platformPolicy{
	PlatformQQMusic: {
		primaryHost: "c.y.qq.com",
		allowedDomains: stringSet(
			"qq.com",
		),
		allowedHosts: stringSet(
			"c.y.qq.com",
			"y.qq.com",
			"u.y.qq.com",
		),
		allowedCookies: stringSet(
			"uin",
			"qqmusic_key",
			"qm_keyst",
			"psrf_qqaccess_token",
			"psrf_qqopenid",
			"psrf_qqunionid",
			"psrf_musickey_createtime",
			"login_type",
			"wxuin",
			"wxrefresh_token",
			"wxunionid",
			"wxopenid",
			"tmeLoginType",
			"euin",
		),
		authCookies: stringSet(
			"qqmusic_key",
			"qm_keyst",
			"psrf_qqaccess_token",
			"wxrefresh_token",
		),
	},
	PlatformNetease: {
		primaryHost: "music.163.com",
		allowedDomains: stringSet(
			"163.com",
		),
		allowedHosts: stringSet(
			"music.163.com",
			"interface.music.163.com",
		),
		allowedCookies: stringSet(
			"MUSIC_U",
			"MUSIC_A",
			"MUSIC_R_T",
			"__csrf",
			"NMTID",
		),
		authCookies: stringSet(
			"MUSIC_U",
			"MUSIC_A",
		),
	},
}

type platformPolicy struct {
	primaryHost    string
	allowedDomains map[string]struct{}
	allowedHosts   map[string]struct{}
	allowedCookies map[string]struct{}
	authCookies    map[string]struct{}
}

type storedCookie struct {
	Name      string
	Value     string
	Domain    string
	Path      string
	ExpiresAt time.Time
	Secure    bool
	HTTPOnly  bool
}

// Status 是适合 UI/API 使用的脱敏会话状态。
type Status struct {
	Platform  Platform     `json:"platform"`
	State     SessionState `json:"state"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
	UpdatedAt *time.Time   `json:"updated_at,omitempty"`
}

func (status Status) String() string {
	return fmt.Sprintf("platformauth.Status{platform:%s state:%s}", status.Platform, status.State)
}

func (status Status) GoString() string { return status.String() }

// ImportResult 只返回数量与脱敏状态，不返回 Cookie 名或值。
type ImportResult struct {
	Accepted int    `json:"accepted"`
	Ignored  int    `json:"ignored"`
	Status   Status `json:"status"`
}

// Store 同时实现 edgelyrics.AuthSession 所需的 Authorize 方法。
type Store struct {
	mu         sync.RWMutex
	now        func() time.Time
	cookies    map[Platform][]storedCookie
	updatedAt  map[Platform]time.Time
	needsLogin map[Platform]bool
	directory  string
	key        []byte
}

// NewMemory 创建不落盘的会话存储，主要用于临时运行和测试。
func NewMemory() *Store {
	return newMemoryWithClock(time.Now)
}

func newMemoryWithClock(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		now:        now,
		cookies:    make(map[Platform][]storedCookie),
		updatedAt:  make(map[Platform]time.Time),
		needsLogin: make(map[Platform]bool),
	}
}

// StatusFor 返回单个平台的脱敏登录态。
func (store *Store) StatusFor(platformName string) (Status, error) {
	platform, err := parsePlatform(platformName)
	if err != nil {
		return Status{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.statusLocked(platform, store.now()), nil
}

// StatusAll 固定返回 QQ 音乐和网易云音乐两个平台，便于 UI 直接展示。
func (store *Store) StatusAll() []Status {
	if store == nil {
		return []Status{
			{Platform: PlatformQQMusic, State: StateNotConfigured},
			{Platform: PlatformNetease, State: StateNotConfigured},
		}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	now := store.now()
	return []Status{
		store.statusLocked(PlatformQQMusic, now),
		store.statusLocked(PlatformNetease, now),
	}
}

// Authorize 仅向指定平台的严格 HTTPS 主机添加白名单 Cookie。
func (store *Store) Authorize(ctx context.Context, platformName string, request *http.Request) error {
	if store == nil {
		return ErrNoAuthCookies
	}
	platform, err := parsePlatform(platformName)
	if err != nil {
		return err
	}
	if request == nil || request.URL == nil {
		return ErrDisallowedTarget
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	policy := platformPolicies[platform]
	host := strings.ToLower(strings.TrimSuffix(request.URL.Hostname(), "."))
	_, hostAllowed := policy.allowedHosts[host]
	port := request.URL.Port()
	if request.URL.Scheme != "https" || !hostAllowed || (port != "" && port != "443") {
		return ErrDisallowedTarget
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}

	store.mu.RLock()
	candidates := append([]storedCookie(nil), store.cookies[platform]...)
	now := store.now()
	store.mu.RUnlock()

	selected := make(map[string]storedCookie)
	for _, cookie := range candidates {
		if cookieExpired(cookie, now) || !cookieDomainMatches(cookie.Domain, host) ||
			!cookiePathMatches(cookie.Path, request.URL.Path) {
			continue
		}
		if _, allowed := policy.allowedCookies[cookie.Name]; !allowed {
			continue
		}
		previous, exists := selected[cookie.Name]
		if !exists || cookieSpecificity(cookie) > cookieSpecificity(previous) {
			selected[cookie.Name] = cookie
		}
	}

	// 清除调用方预置的 Cookie，保证授权后请求中只存在本包白名单内的名称。
	request.Header.Del("Cookie")
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cookie := selected[name]
		request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	return nil
}

// MarkNeedsLogin 记录平台已经明确拒绝当前会话。它只保存一个脱敏状态位，
// 不删除 Cookie；“需要会员”与“登录失效”可能使用同一个平台错误，删除会话会
// 让仍然有效的登录态丢失。重新导入登录态或退出会清除此标记。
func (store *Store) MarkNeedsLogin(platformName string) error {
	if store == nil {
		return ErrNoAuthCookies
	}
	platform, err := parsePlatform(platformName)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.needsLogin[platform] {
		return nil
	}
	store.needsLogin[platform] = true
	if err := store.saveLocked(); err != nil {
		delete(store.needsLogin, platform)
		return err
	}
	return nil
}

// Clear 删除指定平台的会话并同步更新持久化文件。
func (store *Store) Clear(platformName string) error {
	platform, err := parsePlatform(platformName)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	previousCookies := store.cookies[platform]
	previousUpdatedAt, hadUpdatedAt := store.updatedAt[platform]
	previousNeedsLogin, hadNeedsLogin := store.needsLogin[platform]
	delete(store.cookies, platform)
	delete(store.updatedAt, platform)
	delete(store.needsLogin, platform)
	if err := store.saveLocked(); err != nil {
		store.cookies[platform] = previousCookies
		if hadUpdatedAt {
			store.updatedAt[platform] = previousUpdatedAt
		}
		if hadNeedsLogin {
			store.needsLogin[platform] = previousNeedsLogin
		}
		return err
	}
	return nil
}

func (store *Store) statusLocked(platform Platform, now time.Time) Status {
	policy := platformPolicies[platform]
	status := Status{Platform: platform, State: StateNotConfigured}
	if updated, ok := store.updatedAt[platform]; ok {
		copyOfUpdated := updated
		status.UpdatedAt = &copyOfUpdated
	}
	authFound := false
	configured := false
	var nearestExpiry time.Time
	for _, cookie := range store.cookies[platform] {
		if _, auth := policy.authCookies[cookie.Name]; !auth {
			continue
		}
		authFound = true
		if cookieExpired(cookie, now) {
			continue
		}
		configured = true
		if !cookie.ExpiresAt.IsZero() && (nearestExpiry.IsZero() || cookie.ExpiresAt.Before(nearestExpiry)) {
			nearestExpiry = cookie.ExpiresAt
		}
	}
	switch {
	case store.needsLogin[platform]:
		status.State = StateNeedsLogin
	case configured:
		status.State = StateConfigured
		if !nearestExpiry.IsZero() {
			copyOfExpiry := nearestExpiry
			status.ExpiresAt = &copyOfExpiry
		}
	case authFound:
		status.State = StateExpired
	default:
		status.State = StateNotConfigured
	}
	return status
}

// MarshalJSON 只序列化脱敏状态，绝不序列化 Cookie。
func (store *Store) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Sessions []Status `json:"sessions"`
	}{Sessions: store.StatusAll()})
}

func (store *Store) String() string {
	statuses := store.StatusAll()
	return fmt.Sprintf("platformauth.Store{qq:%s netease:%s}", statuses[0].State, statuses[1].State)
}

func (store *Store) GoString() string { return store.String() }

// LogValue 保证 slog 结构化日志只记录脱敏状态。
func (store *Store) LogValue() slog.Value { return slog.StringValue(store.String()) }

func parsePlatform(value string) (Platform, error) {
	normalized := normalizeIdentifier(value)
	switch {
	case normalized == "qq", normalized == "qqmusic", normalized == "腾讯qq音乐", normalized == "腾讯音乐":
		return PlatformQQMusic, nil
	case normalized == "netease", normalized == "neteasemusic", normalized == "cloudmusic",
		normalized == "neteasecloudmusic", normalized == "163", normalized == "music163com",
		normalized == "网易", normalized == "网易云", normalized == "网易云音乐":
		return PlatformNetease, nil
	default:
		return "", ErrUnsupportedPlatform
	}
}

func normalizeIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}

func stringSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func cookieExpired(cookie storedCookie, now time.Time) bool {
	return !cookie.ExpiresAt.IsZero() && !cookie.ExpiresAt.After(now)
}

func cookieDomainMatches(domain, host string) bool {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	host = strings.ToLower(strings.TrimSpace(host))
	return domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

func cookiePathMatches(cookiePath, requestPath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if requestPath == "" {
		requestPath = "/"
	}
	if requestPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") || (len(requestPath) > len(cookiePath) && requestPath[len(cookiePath)] == '/')
}

func cookieSpecificity(cookie storedCookie) int {
	return len(strings.TrimPrefix(cookie.Domain, "."))*1024 + len(cookie.Path)
}
