package platformauth

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CookieInput 是 UI/API 可反序列化的一条 Cookie。
// value 保持私有；JSON 输入可包含 value，但任何 JSON 输出和格式化字符串都会脱敏。
type CookieInput struct {
	Name      string    `json:"name"`
	Domain    string    `json:"domain,omitempty"`
	Path      string    `json:"path,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Secure    bool      `json:"secure,omitempty"`
	HTTPOnly  bool      `json:"http_only,omitempty"`
	value     string
}

// NewCookieInput 创建结构化 Cookie 导入项，value 只保存在不可导出的字段中。
func NewCookieInput(name, value, domain, path string, expiresAt time.Time, secure, httpOnly bool) CookieInput {
	return CookieInput{
		Name: name, Domain: domain, Path: path, ExpiresAt: expiresAt,
		Secure: secure, HTTPOnly: httpOnly, value: value,
	}
}

// UnmarshalJSON 允许本地 UI/API 一次性导入 value。
func (input *CookieInput) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Name      string    `json:"name"`
		Value     string    `json:"value"`
		Domain    string    `json:"domain"`
		Path      string    `json:"path"`
		ExpiresAt time.Time `json:"expires_at"`
		Secure    bool      `json:"secure"`
		HTTPOnly  bool      `json:"http_only"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return ErrInvalidCookieInput
	}
	*input = NewCookieInput(decoded.Name, decoded.Value, decoded.Domain, decoded.Path,
		decoded.ExpiresAt, decoded.Secure, decoded.HTTPOnly)
	return nil
}

// MarshalJSON 明确省略 Cookie 值，防止请求对象被响应或日志意外回显。
func (input CookieInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name      string    `json:"name"`
		Domain    string    `json:"domain,omitempty"`
		Path      string    `json:"path,omitempty"`
		ExpiresAt time.Time `json:"expires_at,omitempty"`
		Secure    bool      `json:"secure,omitempty"`
		HTTPOnly  bool      `json:"http_only,omitempty"`
		HasValue  bool      `json:"has_value"`
	}{
		Name: input.Name, Domain: input.Domain, Path: input.Path, ExpiresAt: input.ExpiresAt,
		Secure: input.Secure, HTTPOnly: input.HTTPOnly, HasValue: input.value != "",
	})
}

func (input CookieInput) String() string {
	return fmt.Sprintf("platformauth.CookieInput{name:%q domain:%q value:<redacted>}", input.Name, input.Domain)
}

func (input CookieInput) GoString() string { return input.String() }

// LogValue 保证 slog 不会通过反射记录私有 Cookie 值。
func (input CookieInput) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", input.Name),
		slog.String("domain", input.Domain),
		slog.Bool("has_value", input.value != ""),
	)
}

// ImportStructured 用结构化 Cookie 替换指定平台的旧会话。
func (store *Store) ImportStructured(platformName string, inputs []CookieInput) (ImportResult, error) {
	platform, err := parsePlatform(platformName)
	if err != nil {
		return ImportResult{}, err
	}
	cookies, accepted, ignored, err := filterInputs(platform, inputs)
	if err != nil {
		return ImportResult{}, err
	}
	return store.replacePlatform(platform, cookies, accepted, ignored)
}

// ImportNetscape 用浏览器常见的 Netscape cookie.txt 文本替换指定平台的旧会话。
// 无关域名和非白名单 Cookie 会被忽略，不会进入存储。
func (store *Store) ImportNetscape(platformName, text string) (ImportResult, error) {
	platform, err := parsePlatform(platformName)
	if err != nil {
		return ImportResult{}, err
	}
	// 部分 Windows 导出工具会写入 UTF-8 BOM；它不是 Cookie 文件正文。
	text = strings.TrimPrefix(text, "\ufeff")
	var inputs []CookieInput
	for lineNumber, rawLine := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		httpOnly := false
		if strings.HasPrefix(line, "#HttpOnly_") {
			httpOnly = true
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 7)
		if len(fields) != 7 {
			return ImportResult{}, fmt.Errorf("%w: 第 %d 行", ErrInvalidCookieInput, lineNumber+1)
		}
		expiresUnix, parseErr := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64)
		if parseErr != nil || expiresUnix < 0 {
			return ImportResult{}, fmt.Errorf("%w: 第 %d 行", ErrInvalidCookieInput, lineNumber+1)
		}
		var expiresAt time.Time
		if expiresUnix > 0 {
			expiresAt = time.Unix(expiresUnix, 0)
		}
		secure, parseErr := strconv.ParseBool(strings.TrimSpace(fields[3]))
		if parseErr != nil {
			return ImportResult{}, fmt.Errorf("%w: 第 %d 行", ErrInvalidCookieInput, lineNumber+1)
		}
		inputs = append(inputs, NewCookieInput(
			strings.TrimSpace(fields[5]), fields[6], strings.TrimSpace(fields[0]),
			strings.TrimSpace(fields[2]), expiresAt, secure, httpOnly,
		))
	}
	cookies, accepted, ignored, err := filterInputs(platform, inputs)
	if err != nil {
		return ImportResult{}, err
	}
	return store.replacePlatform(platform, cookies, accepted, ignored)
}

func filterInputs(platform Platform, inputs []CookieInput) ([]storedCookie, int, int, error) {
	policy := platformPolicies[platform]
	accepted := 0
	ignored := 0
	deduplicated := make(map[string]storedCookie)
	authFound := false
	for _, input := range inputs {
		name := strings.TrimSpace(input.Name)
		domain := strings.ToLower(strings.TrimSpace(input.Domain))
		if domain == "" {
			domain = policy.primaryHost
		}
		path := strings.TrimSpace(input.Path)
		if path == "" {
			path = "/"
		}
		if _, allowed := policy.allowedCookies[name]; !allowed ||
			!domainAllowedForPlatform(domain, policy) || !strings.HasPrefix(path, "/") ||
			!validCookieValue(input.value) {
			ignored++
			continue
		}
		cookie := storedCookie{
			Name: name, Value: input.value, Domain: domain, Path: path,
			ExpiresAt: input.ExpiresAt, Secure: input.Secure, HTTPOnly: input.HTTPOnly,
		}
		key := name + "\n" + domain + "\n" + path
		if _, existed := deduplicated[key]; !existed {
			accepted++
		}
		deduplicated[key] = cookie
		if _, auth := policy.authCookies[name]; auth {
			authFound = true
		}
	}
	if !authFound {
		return nil, 0, ignored, ErrNoAuthCookies
	}
	cookies := make([]storedCookie, 0, len(deduplicated))
	for _, cookie := range deduplicated {
		cookies = append(cookies, cookie)
	}
	sortStoredCookies(cookies)
	return cookies, accepted, ignored, nil
}

func (store *Store) replacePlatform(platform Platform, cookies []storedCookie, accepted, ignored int) (ImportResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	previousCookies := store.cookies[platform]
	previousUpdatedAt, hadUpdatedAt := store.updatedAt[platform]
	previousNeedsLogin, hadNeedsLogin := store.needsLogin[platform]
	store.cookies[platform] = append([]storedCookie(nil), cookies...)
	store.updatedAt[platform] = store.now()
	delete(store.needsLogin, platform)
	if err := store.saveLocked(); err != nil {
		store.cookies[platform] = previousCookies
		if hadUpdatedAt {
			store.updatedAt[platform] = previousUpdatedAt
		} else {
			delete(store.updatedAt, platform)
		}
		if hadNeedsLogin {
			store.needsLogin[platform] = previousNeedsLogin
		}
		return ImportResult{}, err
	}
	return ImportResult{
		Accepted: accepted,
		Ignored:  ignored,
		Status:   store.statusLocked(platform, store.now()),
	}, nil
}

func domainAllowedForPlatform(domain string, policy platformPolicy) bool {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return false
	}
	for root := range policy.allowedDomains {
		if domain == root || strings.HasSuffix(domain, "."+root) {
			return true
		}
	}
	return false
}

func validCookieValue(value string) bool {
	if value == "" || len(value) > 8192 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !(character == 0x21 || character >= 0x23 && character <= 0x2b ||
			character >= 0x2d && character <= 0x3a || character >= 0x3c && character <= 0x5b ||
			character >= 0x5d && character <= 0x7e) {
			return false
		}
	}
	return true
}

func sortStoredCookies(cookies []storedCookie) {
	sort.Slice(cookies, func(left, right int) bool {
		if cookies[left].Name != cookies[right].Name {
			return cookies[left].Name < cookies[right].Name
		}
		if cookies[left].Domain != cookies[right].Domain {
			return cookies[left].Domain < cookies[right].Domain
		}
		return cookies[left].Path < cookies[right].Path
	})
}
