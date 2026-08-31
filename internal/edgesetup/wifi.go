package edgesetup

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// WPAConfigStore 把单个 WPA2-Personal 网络写成 wpa_supplicant 配置。
// 文件中只保存 PBKDF2 派生的 256 位 PSK，不保存用户输入的明文 passphrase。
type WPAConfigStore struct {
	Path        string
	Interface   string
	Apply       func(context.Context, string) error
	IsConnected func(context.Context) bool
}

func (store *WPAConfigStore) Status(ctx context.Context) WiFiStatus {
	status := WiFiStatus{Available: store != nil, Interface: strings.TrimSpace(store.Interface)}
	if store == nil {
		status.Reason = "Wi-Fi 配置存储不可用"
		return status
	}
	path := filepath.Clean(strings.TrimSpace(store.Path))
	if path == "" || path == "." {
		status.Available = false
		status.Reason = "Wi-Fi 配置路径无效"
		return status
	}
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		status.Configured = true
	}
	if store.IsConnected != nil {
		status.Connected = store.IsConnected(ctx)
	}
	return status
}

// Configure 校验 SSID/passphrase、在内存中派生 PSK，再以 0600 原子替换配置。
func (store *WPAConfigStore) Configure(ctx context.Context, request WiFiRequest) error {
	if store == nil {
		return errors.New("Wi-Fi 配置存储不可用")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	ssid := []byte(request.SSID)
	passphrase := []byte(request.Passphrase)
	defer wipe(passphrase)
	if !utf8.Valid(ssid) || len(ssid) < 1 || len(ssid) > 32 || containsUnsafeText(ssid) {
		return errors.New("Wi-Fi 名称必须是 1..32 字节的有效文本")
	}
	if !utf8.Valid(passphrase) || len(passphrase) < 8 || len(passphrase) > 63 || containsUnsafeText(passphrase) {
		return errors.New("Wi-Fi 密码必须是 8..63 字节的有效文本")
	}
	path := filepath.Clean(strings.TrimSpace(store.Path))
	if path == "" || path == "." {
		return errors.New("Wi-Fi 配置路径无效")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("Wi-Fi 配置目标必须是普通文件")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("无法检查 Wi-Fi 配置目标")
	}

	psk := pbkdf2SHA1(passphrase, ssid, 4096, 32)
	defer wipe(psk)
	content := buildWPAConfig(ssid, psk, request.Hidden)
	defer wipe(content)
	if err := writeAtomicPrivateFile(path, content); err != nil {
		return err
	}
	if store.Apply != nil {
		if err := store.Apply(ctx, path); err != nil {
			return errors.New("Wi-Fi 配置已保存，但重新连接失败")
		}
	}
	return nil
}

func buildWPAConfig(ssid, psk []byte, hidden bool) []byte {
	var output strings.Builder
	output.WriteString("# 由 CaseScreen 一次性设置门户生成；文件权限必须为 0600\n")
	output.WriteString("ctrl_interface=/etc/wifi/sockets\n")
	output.WriteString("update_config=0\n\nnetwork={\n")
	output.WriteString("\tssid=")
	output.WriteString(hex.EncodeToString(ssid))
	output.WriteString("\n\tpsk=")
	output.WriteString(hex.EncodeToString(psk))
	output.WriteString("\n\tkey_mgmt=WPA-PSK\n\tproto=RSN\n\tpairwise=CCMP\n")
	if hidden {
		output.WriteString("\tscan_ssid=1\n")
	}
	output.WriteString("}\n")
	return []byte(output.String())
}

func containsUnsafeText(value []byte) bool {
	for _, character := range value {
		if character == 0 || character == '\r' || character == '\n' || character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

// pbkdf2SHA1 实现 WPA2-Personal 规定的 PBKDF2-HMAC-SHA1。
func pbkdf2SHA1(password, salt []byte, iterations, keyLength int) []byte {
	hashLength := sha1.Size
	blocks := (keyLength + hashLength - 1) / hashLength
	output := make([]byte, 0, blocks*hashLength)
	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha1.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		previous := mac.Sum(nil)
		combined := append([]byte(nil), previous...)
		for round := 1; round < iterations; round++ {
			mac = hmac.New(sha1.New, password)
			_, _ = mac.Write(previous)
			next := mac.Sum(nil)
			for index := range combined {
				combined[index] ^= next[index]
			}
			wipe(previous)
			previous = next
		}
		wipe(previous)
		output = append(output, combined...)
		wipe(combined)
	}
	return output[:keyLength]
}

func writeAtomicPrivateFile(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("无法创建 Wi-Fi 配置目录")
	}
	_ = os.Chmod(directory, 0o700)
	temporary, err := os.CreateTemp(directory, ".casescreen-wifi-*.tmp")
	if err != nil {
		return errors.New("无法创建 Wi-Fi 配置临时文件")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("无法限制 Wi-Fi 配置临时文件权限")
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return errors.New("无法写入 Wi-Fi 配置")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("无法同步 Wi-Fi 配置")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("无法关闭 Wi-Fi 配置")
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return errors.New("无法原子替换 Wi-Fi 配置")
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
	// 阻止编译器把只写清零提前消除。
	_, _ = io.Discard.Write(value)
}
