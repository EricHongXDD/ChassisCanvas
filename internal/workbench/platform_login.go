package workbench

import (
	"fmt"
	"strings"
	"time"
)

const (
	platformAuthHostHeading = "主机端平台登录态（仅会员或受限歌词需要）"
	platformAuthHostNotice  = "cookie.txt 只是把浏览器中已登录会话导入 CaseScreen 的本地载体，不是只能使用一次的 Cookie；成功导入后，会话仅由当前 Windows 用户加密保存在 Windows 主机并自动复用，只有过期、被平台撤销或平台提示登录/VIP 时才需重新导入。打开官方页不会自动读取会话；源文件不会被持续读取、不上传且不发送给副屏，可由用户自行妥善删除。CaseScreen 不接收账号密码；取词与登录都在当前 Windows 主机完成，副屏只接收显示小包。无需购买、连接或安装任何额外设备或辅助程序。"
)

func platformLoginURL(platform string) (string, bool) {
	switch platform {
	case "qq":
		return "https://y.qq.com/", true
	case "netease":
		return "https://music.163.com/", true
	default:
		return "", false
	}
}

func (model *gioEditor) openPlatformLogin(platform, label string) {
	loginURL, ok := platformLoginURL(platform)
	if !ok {
		model.message = "不支持打开该音乐平台的登录页"
		return
	}
	if err := openExternalURL(loginURL); err != nil {
		model.message = fmt.Sprintf("打开%s官方登录页失败：%v", label, err)
		return
	}
	model.message = platformLoginOpenedMessage(label)
}

func platformLoginOpenedMessage(label string) string {
	return "已打开" + label + "官方页面；此操作不会自动读取浏览器会话。登录后请导出 Netscape cookie.txt 并在主机端导入一次，后续由 CaseScreen 加密复用"
}

func platformAuthStatusSummary(status PlatformAuthStatus, now time.Time) string {
	switch strings.TrimSpace(status.State) {
	case "configured":
		if status.ExpiresAt != nil && status.ExpiresAt.After(now) {
			return "已配置；主机自动复用至 " + status.ExpiresAt.Local().Format("2006-01-02 15:04")
		}
		return "已配置；主机将自动复用"
	case "expired":
		return "登录态已过期；请重新登录并导入"
	case "needs_login":
		return "平台已拒绝；请确认登录/VIP 后重新导入"
	default:
		return platformAuthStateText(status.State)
	}
}

func platformAuthImportedMessage(label string, result PlatformAuthImportResult) string {
	if strings.TrimSpace(result.Status.State) == "expired" {
		return label + "导入文件中的登录 Cookie 已过期；请重新登录后再导出新的 cookie.txt"
	}
	message := fmt.Sprintf("%s登录态已安全导入 %d 项；以后启动时会自动复用，无需每次导入", label, result.Accepted)
	if result.Ignored > 0 {
		message += fmt.Sprintf("（另有 %d 项非白名单内容已忽略）", result.Ignored)
	}
	return message
}

func platformAuthClearedMessage(label string) string {
	return label + "的 CaseScreen 主机登录态已清除；不会退出浏览器或音乐客户端中的账号"
}
