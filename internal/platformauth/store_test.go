package platformauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"casescreen/internal/edgelyrics"
)

var _ edgelyrics.AuthSession = (*Store)(nil)

func TestConcurrentOpenInstallsOneCompletePersistentKey(t *testing.T) {
	directory := t.TempDir()
	const workers = 12
	stores := make([]*Store, workers)
	errorsByWorker := make([]error, workers)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for index := range stores {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			stores[index], errorsByWorker[index] = Open(directory)
		}(index)
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByWorker {
		if err != nil {
			t.Fatalf("并发打开 %d 失败: %v", index, err)
		}
		if len(stores[index].key) != keySize {
			t.Fatalf("并发打开 %d 得到不完整密钥: %d", index, len(stores[index].key))
		}
		if index > 0 && !bytes.Equal(stores[0].key, stores[index].key) {
			t.Fatalf("并发打开 %d 使用了不同密钥", index)
		}
	}
	stored, err := os.ReadFile(filepath.Join(directory, keyFileName))
	if err != nil {
		t.Fatalf("无法读取并发创建的持久化密钥: %v", err)
	}
	decoded, migrate, err := decodeStoredKey(stored)
	if err != nil || !bytes.Equal(decoded, stores[0].key) {
		t.Fatalf("持久化密钥不是完整可解码格式: bytes=%d migrate=%v err=%v", len(stored), migrate, err)
	}
	if runtime.GOOS == "windows" && migrate {
		t.Fatal("Windows 并发创建后仍保存了旧版明文密钥")
	}
}

func TestStructuredImportInjectsOnlyQQWhitelistToStrictHTTPSHost(t *testing.T) {
	now := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	store := newMemoryWithClock(func() time.Time { return now })
	result, err := store.ImportStructured("QQMusic", []CookieInput{
		NewCookieInput("uin", "100001", ".qq.com", "/", now.Add(time.Hour), true, true),
		NewCookieInput("qqmusic_key", "qq-secret-token", ".qq.com", "/", now.Add(time.Hour), true, true),
		NewCookieInput("not_allowed", "must-never-appear", ".qq.com", "/", time.Time{}, true, true),
	})
	if err != nil {
		t.Fatalf("导入 QQ 会话失败: %v", err)
	}
	if result.Accepted != 2 || result.Ignored != 1 || result.Status.State != StateConfigured {
		t.Fatalf("导入结果错误: %#v", result)
	}

	request, err := http.NewRequest(http.MethodGet, "https://c.y.qq.com/lyric/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", "caller_untrusted=caller-secret")
	if err := store.Authorize(context.Background(), "qq", request); err != nil {
		t.Fatalf("注入 QQ 会话失败: %v", err)
	}
	cookies := cookieMap(request)
	if cookies["uin"] != "100001" || cookies["qqmusic_key"] != "qq-secret-token" {
		t.Fatalf("白名单 Cookie 未注入，实际名称: %v", cookieNames(cookies))
	}
	if _, exists := cookies["not_allowed"]; exists {
		t.Fatalf("注入了非白名单 Cookie，实际名称: %v", cookieNames(cookies))
	}
	if _, exists := cookies["caller_untrusted"]; exists {
		t.Fatalf("保留了调用方非白名单 Cookie，实际名称: %v", cookieNames(cookies))
	}
}

func TestWrongDomainSchemePortAndPlatformNeverReceiveCredentials(t *testing.T) {
	store := NewMemory()
	_, err := store.ImportStructured("netease", []CookieInput{
		NewCookieInput("MUSIC_U", "netease-secret-token", ".163.com", "/", time.Time{}, true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		platform string
		url      string
	}{
		{name: "错误域名", platform: "netease", url: "https://attacker.example/api"},
		{name: "错误协议", platform: "netease", url: "http://music.163.com/api"},
		{name: "错误端口", platform: "netease", url: "https://music.163.com:444/api"},
		{name: "跨平台主机", platform: "qq", url: "https://music.163.com/api"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, requestErr := http.NewRequest(http.MethodGet, test.url, nil)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			authorizeErr := store.Authorize(context.Background(), test.platform, request)
			if !errors.Is(authorizeErr, ErrDisallowedTarget) {
				t.Fatalf("错误目标没有被拒绝: %v", authorizeErr)
			}
			if strings.Contains(request.Header.Get("Cookie"), "netease-secret-token") {
				t.Fatal("凭据被注入到错误目标")
			}
		})
	}
}

func TestPlatformNamesAreNotMatchedBySubstring(t *testing.T) {
	for _, value := range []string{"fakeqqservice", "netease-proxy.example", "not-163-platform"} {
		if _, err := parsePlatform(value); !errors.Is(err, ErrUnsupportedPlatform) {
			t.Fatalf("不受支持的平台名称被宽松匹配: %q, err=%v", value, err)
		}
	}
}

func TestNeteaseCookieNamesAreRestricted(t *testing.T) {
	store := NewMemory()
	result, err := store.ImportStructured("网易云音乐", []CookieInput{
		NewCookieInput("MUSIC_U", "music-u-token", ".163.com", "/", time.Time{}, true, true),
		NewCookieInput("__csrf", "csrf-token", "music.163.com", "/", time.Time{}, true, false),
		NewCookieInput("JSESSIONID", "forbidden-token", ".163.com", "/", time.Time{}, true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 2 || result.Ignored != 1 {
		t.Fatalf("网易云白名单过滤错误: %#v", result)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://music.163.com/api/song/lyric", nil)
	if err := store.Authorize(context.Background(), "netease", request); err != nil {
		t.Fatal(err)
	}
	cookies := cookieMap(request)
	if cookies["MUSIC_U"] != "music-u-token" || cookies["__csrf"] != "csrf-token" {
		t.Fatalf("网易云允许 Cookie 缺失，实际名称: %v", cookieNames(cookies))
	}
	if _, exists := cookies["JSESSIONID"]; exists {
		t.Fatalf("网易云非白名单 Cookie 被注入，实际名称: %v", cookieNames(cookies))
	}
}

func TestStatusConfiguredExpiredAndNeedsLogin(t *testing.T) {
	now := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	store := newMemoryWithClock(func() time.Time { return now })
	status, err := store.StatusFor("qq")
	if err != nil || status.State != StateNotConfigured {
		t.Fatalf("初始状态错误: status=%#v err=%v", status, err)
	}
	_, err = store.ImportStructured("qq", []CookieInput{
		NewCookieInput("qqmusic_key", "expired-token", ".qq.com", "/", now.Add(-time.Second), true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ = store.StatusFor("qq")
	if status.State != StateExpired {
		t.Fatalf("过期状态错误: %#v", status)
	}
	_, err = store.ImportStructured("qq", []CookieInput{
		NewCookieInput("qqmusic_key", "live-token", ".qq.com", "/", now.Add(time.Hour), true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ = store.StatusFor("qq")
	if status.State != StateConfigured || status.ExpiresAt == nil || !status.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("已配置状态错误: %#v", status)
	}
	if err := store.Clear("qq"); err != nil {
		t.Fatal(err)
	}
	status, _ = store.StatusFor("qq")
	if status.State != StateNotConfigured {
		t.Fatalf("清除后状态错误: %#v", status)
	}
}

func TestNetscapeImportSupportsHttpOnlyAndIgnoresUnrelatedCookies(t *testing.T) {
	now := time.Now().Add(time.Hour).Unix()
	text := strings.Join([]string{
		"# Netscape HTTP Cookie File",
		fmt.Sprintf("#HttpOnly_.music.163.com\tTRUE\t/\tTRUE\t%d\tMUSIC_U\tnetscape-secret", now),
		fmt.Sprintf(".music.163.com\tTRUE\t/\tTRUE\t%d\t__csrf\tcsrf-value", now),
		fmt.Sprintf(".example.com\tTRUE\t/\tTRUE\t%d\tMUSIC_U\twrong-domain-secret", now),
		fmt.Sprintf(".music.163.com\tTRUE\t/\tTRUE\t%d\tUNSUPPORTED\tignored-secret", now),
	}, "\n")
	store := NewMemory()
	result, err := store.ImportNetscape("netease", text)
	if err != nil {
		t.Fatalf("Netscape 导入失败: %v", err)
	}
	if result.Accepted != 2 || result.Ignored != 2 || result.Status.State != StateConfigured {
		t.Fatalf("Netscape 过滤结果错误: %#v", result)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://music.163.com/api/song/lyric", nil)
	if err := store.Authorize(context.Background(), "netease", request); err != nil {
		t.Fatal(err)
	}
	cookies := cookieMap(request)
	if cookies["MUSIC_U"] != "netscape-secret" || cookies["__csrf"] != "csrf-value" || len(cookies) != 2 {
		t.Fatalf("Netscape Cookie 注入错误，实际名称: %v", cookieNames(cookies))
	}
}

func TestNetscapeImportAcceptsUTF8BOMButRejectsOverbroadCookieDomains(t *testing.T) {
	expires := time.Now().Add(time.Hour).Unix()
	text := strings.Join([]string{
		"\ufeff# Netscape HTTP Cookie File",
		fmt.Sprintf(".com\tTRUE\t/\tTRUE\t%d\tqqmusic_key\toverbroad-must-be-ignored", expires),
		fmt.Sprintf(".y.qq.com\tTRUE\t/\tTRUE\t%d\tqqmusic_key\tvalid-qq-session", expires),
	}, "\n")
	store := NewMemory()
	result, err := store.ImportNetscape("qq", text)
	if err != nil {
		t.Fatalf("带 UTF-8 BOM 的标准 cookie.txt 导入失败: %v", err)
	}
	if result.Accepted != 1 || result.Ignored != 1 {
		t.Fatalf("平台根域约束没有生效: %#v", result)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://c.y.qq.com/lyric/test", nil)
	if err := store.Authorize(context.Background(), "qq", request); err != nil {
		t.Fatal(err)
	}
	if got := cookieMap(request)["qqmusic_key"]; got != "valid-qq-session" {
		t.Fatalf("注入了错误域登录态: %q", got)
	}
}

func TestMalformedNetscapeErrorNeverEchoesInput(t *testing.T) {
	secret := "SECRET_INSIDE_MALFORMED_LINE"
	_, err := NewMemory().ImportNetscape("qq", "bad\t"+secret)
	if err == nil {
		t.Fatal("损坏的 Netscape 输入应返回错误")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("解析错误泄露输入内容: %v", err)
	}
}

func TestJSONStringsAndErrorsNeverLeakCredentials(t *testing.T) {
	secret := "VERY_SECRET_COOKIE_VALUE_92731"
	input := NewCookieInput("qqmusic_key", secret, ".qq.com", "/", time.Time{}, true, true)
	store := NewMemory()
	if _, err := store.ImportStructured("qq", []CookieInput{input}); err != nil {
		t.Fatal(err)
	}
	status, _ := store.StatusFor("qq")
	inputJSON, _ := json.Marshal(input)
	storeJSON, _ := json.Marshal(store)
	statusJSON, _ := json.Marshal(status)
	request, _ := http.NewRequest(http.MethodGet, "https://attacker.example/", nil)
	authorizeErr := store.Authorize(context.Background(), "qq", request)
	outputs := []string{
		string(inputJSON), string(storeJSON), string(statusJSON),
		fmt.Sprint(input), fmt.Sprintf("%#v", input), fmt.Sprintf("%+v", input),
		fmt.Sprint(store), fmt.Sprintf("%#v", store), fmt.Sprintf("%+v", store),
		fmt.Sprint(status), fmt.Sprintf("%#v", status),
		authorizeErr.Error(),
	}
	var logOutput bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logOutput, nil))
	logger.Info("会话脱敏检查", "input", input, "store", store)
	outputs = append(outputs, logOutput.String())
	for index, output := range outputs {
		if strings.Contains(output, secret) {
			t.Fatalf("第 %d 个输出泄露 Cookie: %s", index, output)
		}
	}
}

func TestPersistentFilesAre0600EncryptedAndReloadable(t *testing.T) {
	directory := t.TempDir()
	secret := "PERSISTED_COOKIE_SECRET_55192"
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("打开持久化存储失败: %v", err)
	}
	_, err = store.ImportStructured("qq", []CookieInput{
		NewCookieInput("qqmusic_key", secret, ".qq.com", "/", time.Now().Add(time.Hour), true, true),
	})
	if err != nil {
		t.Fatalf("持久化导入失败: %v", err)
	}
	for _, name := range []string{keyFileName, dataFileName} {
		path := filepath.Join(directory, name)
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("缺少持久化文件 %s: %v", name, statErr)
		}
		// Windows 的 os.FileMode 不表达 ACL；Linux 固件上必须没有组/其他用户权限。
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("持久化文件权限过宽 %s: %o", name, info.Mode().Perm())
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("持久化文件 %s 含有明文 Cookie", name)
		}
	}

	reopened, err := Open(directory)
	if err != nil {
		t.Fatalf("重新打开持久化存储失败: %v", err)
	}
	status, _ := reopened.StatusFor("qq")
	if status.State != StateConfigured {
		t.Fatalf("重载后的状态错误: %#v", status)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://c.y.qq.com/lyric/test", nil)
	if err := reopened.Authorize(context.Background(), "qq", request); err != nil {
		t.Fatal(err)
	}
	if cookieMap(request)["qqmusic_key"] != secret {
		t.Fatal("重载后无法使用已加密保存的会话")
	}
}

func TestPersistentSessionCanBeReplacedAndCleared(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"first-session-token", "second-session-token"} {
		_, err = store.ImportStructured("netease", []CookieInput{
			NewCookieInput("MUSIC_U", secret, ".163.com", "/", time.Time{}, true, true),
		})
		if err != nil {
			t.Fatalf("替换持久化会话失败: %v", err)
		}
	}
	if err := store.Clear("netease"); err != nil {
		t.Fatalf("清除持久化会话失败: %v", err)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	status, err := reopened.StatusFor("netease")
	if err != nil || status.State != StateNotConfigured {
		t.Fatalf("持久化清除没有生效: status=%#v err=%v", status, err)
	}
}

func TestCookieTextIsOnlyAnImportCarrierAndEncryptedSessionSurvivesItsRemoval(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "cookie.txt")
	secret := "one-import-carrier-secret"
	expires := time.Now().Add(time.Hour).Unix()
	text := fmt.Sprintf(".music.163.com\tTRUE\t/\tTRUE\t%d\tMUSIC_U\t%s", expires, secret)
	if err := os.WriteFile(sourcePath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportNetscape("netease", string(data)); err != nil {
		t.Fatalf("一次导入载体失败: %v", err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory)
	if err != nil {
		t.Fatalf("删除导入载体后无法重载主机会话: %v", err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://music.163.com/api/song/lyric", nil)
	if err := reopened.Authorize(context.Background(), "netease", request); err != nil {
		t.Fatal(err)
	}
	if cookieMap(request)["MUSIC_U"] != secret {
		t.Fatal("删除 cookie.txt 后没有复用主机端加密会话")
	}

	if err := reopened.Clear("netease"); err != nil {
		t.Fatal(err)
	}
	afterClear, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	clearedRequest, _ := http.NewRequest(http.MethodGet, "https://music.163.com/api/song/lyric", nil)
	if err := afterClear.Authorize(context.Background(), "netease", clearedRequest); err != nil {
		t.Fatal(err)
	}
	if clearedRequest.Header.Get("Cookie") != "" {
		t.Fatal("清除并重启后仍注入了平台登录态")
	}
}

func TestNeedsLoginMarkerPersistsWithoutDeletingSessionAndImportClearsIt(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "login-marker-session-secret"
	_, err = store.ImportStructured("qq", []CookieInput{
		NewCookieInput("qqmusic_key", secret, ".qq.com", "/", time.Now().Add(time.Hour), true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkNeedsLogin("qq"); err != nil {
		t.Fatal(err)
	}
	status, err := store.StatusFor("qq")
	if err != nil || status.State != StateNeedsLogin {
		t.Fatalf("平台拒绝会话后没有标记 needs_login: status=%#v err=%v", status, err)
	}

	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	status, err = reopened.StatusFor("qq")
	if err != nil || status.State != StateNeedsLogin {
		t.Fatalf("needs_login 标记没有持久化: status=%#v err=%v", status, err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://c.y.qq.com/lyric/test", nil)
	if err := reopened.Authorize(context.Background(), "qq", request); err != nil {
		t.Fatal(err)
	}
	if cookieMap(request)["qqmusic_key"] != secret {
		t.Fatal("needs_login 标记错误删除了仍可能有效的登录态")
	}

	result, err := reopened.ImportStructured("qq", []CookieInput{
		NewCookieInput("qqmusic_key", "refreshed-session", ".qq.com", "/", time.Now().Add(time.Hour), true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.State != StateConfigured {
		t.Fatalf("重新导入没有清除 needs_login 标记: %#v", result.Status)
	}
}

func TestExpiredCookieIsNotInjected(t *testing.T) {
	now := time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC)
	store := newMemoryWithClock(func() time.Time { return now })
	_, err := store.ImportStructured("netease", []CookieInput{
		NewCookieInput("MUSIC_U", "expired-secret", ".163.com", "/", now.Add(-time.Minute), true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://music.163.com/api/song/lyric", nil)
	if err := store.Authorize(context.Background(), "netease", request); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Cookie") != "" {
		t.Fatal("过期 Cookie 仍被注入")
	}
}

func cookieMap(request *http.Request) map[string]string {
	result := make(map[string]string)
	for _, cookie := range request.Cookies() {
		result[cookie.Name] = cookie.Value
	}
	return result
}

func cookieNames(cookies map[string]string) []string {
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
