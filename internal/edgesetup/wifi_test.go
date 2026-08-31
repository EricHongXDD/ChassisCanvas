package edgesetup

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPBKDF2MatchesWPAKnownVector(t *testing.T) {
	key := pbkdf2SHA1([]byte("password"), []byte("IEEE"), 4096, 32)
	defer wipe(key)
	want := "f42c6fc52df0ebef9ebb4b90b38a5f902e83fe1b135a70e23aed762e9710a12e"
	if got := hex.EncodeToString(key); got != want {
		t.Fatalf("WPA PSK=%s want=%s", got, want)
	}
}

func TestWPAConfigStoreWritesAtomic0600WithoutPlaintext(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "wpa_supplicant.conf")
	secret := "WiFi-Secret-731!"
	applied := false
	store := &WPAConfigStore{
		Path: path, Interface: "wlan0",
		Apply: func(_ context.Context, appliedPath string) error {
			applied = appliedPath == path
			return nil
		},
	}
	request := WiFiRequest{SSID: "家庭网络", Passphrase: secret, Hidden: true}
	if err := store.Configure(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "家庭网络") {
		t.Fatal("配置文件保存了明文 SSID 或 passphrase")
	}
	if !strings.Contains(string(data), "psk=") || !strings.Contains(string(data), "scan_ssid=1") || !applied {
		t.Fatalf("配置内容或 Apply 状态错误: applied=%t\n%s", applied, data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("配置权限=%#o want=0600", info.Mode().Perm())
	}
	leftovers, err := filepath.Glob(filepath.Join(directory, ".casescreen-wifi-*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("存在临时文件残留: %v %v", leftovers, err)
	}
	status := store.Status(context.Background())
	if !status.Available || !status.Configured || status.Interface != "wlan0" {
		t.Fatalf("Wi-Fi 状态错误: %#v", status)
	}
}

func TestWPAConfigStoreRejectsInvalidInputWithoutReplacingOldConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wpa_supplicant.conf")
	old := []byte("old-config\n")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &WPAConfigStore{Path: path}
	if err := store.Configure(context.Background(), WiFiRequest{
		SSID: "bad\nssid", Passphrase: "12345678",
	}); err == nil {
		t.Fatal("包含换行的 SSID 未被拒绝")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(old) {
		t.Fatalf("无效输入替换了旧配置: %q", data)
	}
}

func TestWiFiRequestFormattingRedactsPassphrase(t *testing.T) {
	request := WiFiRequest{SSID: "home", Passphrase: "do-not-log-this"}
	if strings.Contains(request.String(), request.Passphrase) || strings.Contains(request.GoString(), request.Passphrase) {
		t.Fatal("WiFiRequest 格式化泄露 passphrase")
	}
}
