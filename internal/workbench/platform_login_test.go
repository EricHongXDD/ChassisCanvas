package workbench

import (
	"strings"
	"testing"
	"time"
)

func TestPlatformLoginURLsAreFixedOfficialHTTPSPages(t *testing.T) {
	tests := map[string]string{
		"qq":      "https://y.qq.com/",
		"netease": "https://music.163.com/",
	}
	for platform, expected := range tests {
		actual, ok := platformLoginURL(platform)
		if !ok || actual != expected {
			t.Fatalf("平台 %s 登录地址=%q, ok=%v，期望 %q", platform, actual, ok, expected)
		}
	}
	if address, ok := platformLoginURL("unknown"); ok || address != "" {
		t.Fatalf("未知平台不应返回可打开地址: %q", address)
	}
}

func TestPlatformLoginCopyMakesHostOnlyManualImportBoundaryExplicit(t *testing.T) {
	for _, required := range []string{"主机端", "Windows 主机", "不发送给副屏", "不会自动读取会话", "cookie.txt", "不是只能使用一次", "自动复用", "不接收账号密码", "取词与登录都在当前 Windows 主机完成", "副屏只接收显示小包"} {
		combined := platformAuthHostHeading + "\n" + platformAuthHostNotice
		if !strings.Contains(combined, required) {
			t.Fatalf("平台登录说明缺少边界 %q: %s", required, combined)
		}
	}
	message := platformLoginOpenedMessage("QQ 音乐")
	if !strings.Contains(message, "不会自动读取浏览器会话") || !strings.Contains(message, "主机端导入一次") || !strings.Contains(message, "加密复用") {
		t.Fatalf("打开网页后的提示会让用户误以为已经登录: %s", message)
	}
}

func TestPlatformAuthStatusAndImportMessagesExplainReuseExpiryAndClear(t *testing.T) {
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.Local)
	expires := now.Add(2 * time.Hour)
	configured := platformAuthStatusSummary(PlatformAuthStatus{State: "configured", ExpiresAt: &expires}, now)
	if !strings.Contains(configured, "自动复用") || !strings.Contains(configured, "2026-08-27 12:00") {
		t.Fatalf("已配置状态没有显示复用与失效时间: %q", configured)
	}
	if expired := platformAuthStatusSummary(PlatformAuthStatus{State: "expired"}, now); !strings.Contains(expired, "重新登录并导入") {
		t.Fatalf("过期状态没有给出可执行操作: %q", expired)
	}
	if rejected := platformAuthStatusSummary(PlatformAuthStatus{State: "needs_login"}, now); !strings.Contains(rejected, "登录/VIP") || !strings.Contains(rejected, "重新导入") {
		t.Fatalf("平台拒绝状态没有区分登录/VIP与重新导入: %q", rejected)
	}

	message := platformAuthImportedMessage("网易云音乐", PlatformAuthImportResult{
		Accepted: 2, Ignored: 5, Status: PlatformAuthStatus{State: "configured"},
	})
	for _, expected := range []string{"安全导入 2 项", "自动复用", "无需每次导入", "5 项非白名单内容已忽略"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("导入提示缺少 %q: %s", expected, message)
		}
	}
	if expired := platformAuthImportedMessage("QQ 音乐", PlatformAuthImportResult{Status: PlatformAuthStatus{State: "expired"}}); !strings.Contains(expired, "已过期") || !strings.Contains(expired, "新的 cookie.txt") {
		t.Fatalf("导入过期文件仍声称可以复用: %q", expired)
	}
	if cleared := platformAuthClearedMessage("QQ 音乐"); !strings.Contains(cleared, "CaseScreen 主机登录态已清除") || !strings.Contains(cleared, "不会退出浏览器或音乐客户端") {
		t.Fatalf("清除提示边界不明确: %q", cleared)
	}
}
