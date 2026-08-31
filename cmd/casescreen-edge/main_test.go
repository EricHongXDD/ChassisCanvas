package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"casescreen/internal/platformauth"
)

const testPairingToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestReadTokenFileAcceptsSingleTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.token")
	if err := os.WriteFile(path, []byte(testPairingToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := readTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if token != testPairingToken {
		t.Fatalf("令牌内容错误: %q", token)
	}
}

func TestReadTokenFileRejectsEmbeddedWhitespaceWithoutLeakingSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.token")
	secret := testPairingToken[:32] + " " + testPairingToken[32:]
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readTokenFile(path)
	if err == nil {
		t.Fatal("含空白的令牌文件应被拒绝")
	}
	if strings.Contains(err.Error(), testPairingToken[:16]) {
		t.Fatal("错误信息不应泄露令牌内容")
	}
}

func TestParseFlagsIncludesSyncCacheAuthAndDisplayOptions(t *testing.T) {
	var output bytes.Buffer
	options, programArguments, err := parseFlags([]string{
		"-metadata-mode", "bluez",
		"-sync-url", "http://192.168.1.2:19322",
		"-token-file", "/data/private/sync.token",
		"-lyrics-cache", "/data/cache/lyrics",
		"-lyrics-mode", "direct",
		"-auth-dir", "/data/private/platform-auth",
		"-identity-cache", "/data/cache/identity",
		"-standalone-platform", "qq",
		"-identity-retry", "45s",
		"-source-listen", "127.0.0.1:29323",
		"-source-key-file", "/data/private/source-push.key",
		"-source-stale-after", "50s",
		"-qq-cookie-file", "/data/import/qq.cookies.txt",
		"-netease-cookie-file", "/data/import/netease.cookies.txt",
		"-ca-bundle", "/etc/ssl/certs/ca-certificates.crt",
		"-hardware-report", "/data/casescreen/diagnostics/latest.json",
		"-startup-theme", "/data/theme/1024x600/casescreen-standalone.Theme",
		"-poll-interval", "750ms",
		"-render-interval", "250ms",
		"-device", "/dev/custom-tty",
		"-program", "/data/original-PCMonitorLinux",
		"--", "--factory-argument",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if options.syncURL != "http://192.168.1.2:19322" || options.tokenFile != "/data/private/sync.token" {
		t.Fatalf("同步参数解析错误: %#v", options)
	}
	if options.metadataMode != "bluez" || options.identityCache != "/data/cache/identity" ||
		options.standalonePlatform != "qq" || options.identityRetry != 45*time.Second {
		t.Fatalf("独立元数据参数解析错误: %#v", options)
	}
	if options.qqCookieFile != "/data/import/qq.cookies.txt" || options.neteaseCookieFile != "/data/import/netease.cookies.txt" {
		t.Fatalf("平台登录态导入参数解析错误: %#v", options)
	}
	if options.caBundle != "/etc/ssl/certs/ca-certificates.crt" || options.hardwareReport != "/data/casescreen/diagnostics/latest.json" {
		t.Fatalf("证书或硬件报告参数解析错误: %#v", options)
	}
	if options.startupTheme != "/data/theme/1024x600/casescreen-standalone.Theme" {
		t.Fatalf("启动主题参数解析错误: %#v", options)
	}
	if options.sourceListen != "127.0.0.1:29323" || options.sourceKeyFile != "/data/private/source-push.key" ||
		options.sourceStaleAfter != 50*time.Second {
		t.Fatalf("来源原生 ID 小包参数解析错误: %#v", options)
	}
	if options.lyricsCache != "/data/cache/lyrics" || options.authDirectory != "/data/private/platform-auth" {
		t.Fatalf("缓存或登录态参数解析错误: %#v", options)
	}
	if options.lyricsMode != "direct" {
		t.Fatalf("歌词模式解析错误: %q", options.lyricsMode)
	}
	if options.pollInterval != 750*time.Millisecond || options.renderInterval != 250*time.Millisecond {
		t.Fatalf("轮询或显示间隔解析错误: %#v", options)
	}
	if options.display.DevicePath != "/dev/custom-tty" || options.display.ProgramPath != "/data/original-PCMonitorLinux" {
		t.Fatalf("显示代理参数解析错误: %#v", options.display)
	}
	if options.display.HostAckTimeout != 10*time.Second || options.display.TransferIdleTimeout != 15*time.Second {
		t.Fatalf("电脑端协议桥超时默认值错误: %#v", options.display)
	}
	if len(programArguments) != 1 || programArguments[0] != "--factory-argument" {
		t.Fatalf("原厂程序参数没有保留: %#v", programArguments)
	}
}

func TestDirectHTTPClientRejectsInvalidCABundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newDirectHTTPClient(path, time.Second); err == nil {
		t.Fatal("无效 CA 包必须在联网前被拒绝")
	}
}

func TestParseFlagsRejectsUnsafeLyricsTimeout(t *testing.T) {
	if _, _, err := parseFlags([]string{"-lyrics-timeout", "2m"}, &bytes.Buffer{}); err == nil {
		t.Fatal("无界歌词请求超时应被拒绝")
	}
	if _, _, err := parseFlags([]string{"-render-interval", "100ms"}, &bytes.Buffer{}); err == nil {
		t.Fatal("过高的屏端注入频率应在启动显示进程前被拒绝")
	}
	if _, _, err := parseFlags([]string{"-lyrics-mode", "auto"}, &bytes.Buffer{}); err == nil {
		t.Fatal("未知歌词模式应被拒绝")
	}
	if _, _, err := parseFlags([]string{"-metadata-mode", "microphone"}, &bytes.Buffer{}); err == nil {
		t.Fatal("麦克风或未知元数据模式必须被拒绝")
	}
	if _, _, err := parseFlags([]string{"-source-stale-after", "2s"}, &bytes.Buffer{}); err == nil {
		t.Fatal("来源状态失效窗口过短时必须拒绝")
	}
	if _, _, err := parseFlags([]string{"-standalone-platform", "kugou"}, &bytes.Buffer{}); err == nil {
		t.Fatal("独立模式只能选择 QQ 音乐或网易云音乐")
	}
	if _, _, err := parseFlags([]string{"-host-ack-timeout", "2s"}, &bytes.Buffer{}); err == nil {
		t.Fatal("电脑端 ACK 超时不能早于原驱动默认等待窗口")
	}
	if _, _, err := parseFlags([]string{"-transfer-idle-timeout", "500ms"}, &bytes.Buffer{}); err == nil {
		t.Fatal("文件裸流空闲超时过短应被拒绝")
	}
	for _, value := range []string{"/tmp/theme.Theme", "/data/theme/1024x600/theme.json", "relative.Theme"} {
		if _, _, err := parseFlags([]string{"-startup-theme", value}, &bytes.Buffer{}); err == nil {
			t.Fatalf("无效启动主题路径应被拒绝: %q", value)
		}
	}
}

func TestParseFlagsAcceptsExactSourcePushMode(t *testing.T) {
	options, _, err := parseFlags([]string{
		"-metadata-mode", "source-push",
		"-lyrics-mode", "direct",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.metadataMode != "source-push" || options.lyricsMode != "direct" {
		t.Fatalf("来源原生 ID 模式解析错误: %#v", options)
	}
}

func TestParseFlagsAcceptsSourcePushWithHostRelayLyrics(t *testing.T) {
	options, _, err := parseFlags([]string{
		"-metadata-mode", "source-push",
		"-lyrics-mode", "relay",
		"-sync-url", "http://192.168.77.1:19322",
		"-token-file", "/data/casescreen/sync.token",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.metadataMode != "source-push" || options.lyricsMode != "relay" ||
		options.syncURL != "http://192.168.77.1:19322" {
		t.Fatalf("来源小包 + 主机精确歌词参数错误: %#v", options)
	}
}

func TestParseFlagsAcceptsStandaloneRecognitionContract(t *testing.T) {
	options, _, err := parseFlags([]string{
		"-metadata-mode", "recognition",
		"-lyrics-mode", "direct",
		"-recognition-relay-url", "https://relay.example/v1/recognize",
		"-recognition-token-file", "/data/casescreen/recognition.token",
		"-recognition-pcm", "plughw:audiocodec",
		"-recognition-chunk", "1s",
		"-recognition-capture", "10s",
		"-recognition-preferred-platform", "qqmusic",
		"-recognition-fingerprint-extractor", "/data/casescreen/fingerprint-extractor",
		"-recognition-fingerprint-algorithm", "vendor-acr",
		"-recognition-fingerprint-version", "1",
		"-recognition-config", "/data/casescreen/recognition.env",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.metadataMode != "recognition" || options.lyricsMode != "direct" ||
		options.recognitionChunk != time.Second || options.recognitionWindow != 10*time.Second ||
		options.recognitionPreferredPlatform != "qq" || options.recognitionResumeActive != 2 ||
		options.recognitionFingerprintAlgorithm != "vendor-acr" {
		t.Fatalf("独立识歌参数解析错误: %#v", options)
	}
}

func TestParseFlagsRejectsUnsafeRecognitionContract(t *testing.T) {
	for _, arguments := range [][]string{
		{"-recognition-chunk", "3s"},
		{"-recognition-capture", "9.5s"},
		{"-recognition-preferred-platform", "unknown"},
		{"-recognition-fingerprint-extractor", "/data/fp"},
		{"-recognition-fingerprint-algorithm", "vendor-acr", "-recognition-fingerprint-version", "1"},
		{"-recognition-config", "relative.env"},
		{"-recognition-resume-active", "0"},
	} {
		if _, _, err := parseFlags(arguments, &bytes.Buffer{}); err == nil {
			t.Fatalf("不安全识歌参数应被拒绝: %#v", arguments)
		}
	}
}

func TestDefaultLyricsModeIsHostRelay(t *testing.T) {
	options, _, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.lyricsMode != "relay" {
		t.Fatalf("默认模式必须是主机 relay，实际 %q", options.lyricsMode)
	}
	if options.metadataMode != "relay" {
		t.Fatalf("默认元数据模式必须保持主机 relay，实际 %q", options.metadataMode)
	}
	if options.lyricsTimeout != 20*time.Second {
		t.Fatalf("屏端歌词超时必须覆盖主机 15 秒查询窗口，实际 %v", options.lyricsTimeout)
	}
	if options.recognitionChunk != time.Second || options.recognitionWindow != 10*time.Second ||
		options.recognitionPCM != "plughw:audiocodec" || options.recognitionConfirmations != 2 ||
		options.recognitionPauseSilent != 2 || options.recognitionResumeActive != 2 {
		t.Fatalf("识歌低占用默认参数错误: %#v", options)
	}
}

func TestImportPlatformCookieFileEncryptsAndConsumesPlaintext(t *testing.T) {
	store, err := platformauth.Open(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "qq.cookies.txt")
	content := ".qq.com\tTRUE\t/\tTRUE\t0\tqqmusic_key\ttest-session-value\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result, imported, err := importPlatformCookieFile(store, "qq", path)
	if err != nil {
		t.Fatal(err)
	}
	if !imported || result.Accepted != 1 || result.Status.State != platformauth.StateConfigured {
		t.Fatalf("登录态没有正确导入: imported=%v result=%#v", imported, result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("成功导入后明文文件仍存在: %v", err)
	}
}

func TestImportPlatformCookieFileSkipsAbsentDropFile(t *testing.T) {
	store := platformauth.NewMemory()
	_, imported, err := importPlatformCookieFile(store, "netease", filepath.Join(t.TempDir(), "missing.txt"))
	if err != nil || imported {
		t.Fatalf("尚未放入登录态文件时应正常启动: imported=%v err=%v", imported, err)
	}
}
