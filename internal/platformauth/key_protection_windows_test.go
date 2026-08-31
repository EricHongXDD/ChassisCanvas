//go:build windows

package platformauth

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsPersistentKeyUsesCurrentUserDPAPI(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(directory, keyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(stored, protectedKeyMagic) || len(stored) == keySize || bytes.Contains(stored, store.key) {
		t.Fatalf("Windows 平台会话密钥没有使用 DPAPI 保护: bytes=%d", len(stored))
	}
	decoded, migrate, err := decodeStoredKey(stored)
	if err != nil || migrate || !bytes.Equal(decoded, store.key) {
		t.Fatalf("当前 Windows 用户无法恢复 DPAPI 密钥: migrate=%v err=%v", migrate, err)
	}
}

func TestWindowsLegacyRawKeyMigratesWithoutLosingSession(t *testing.T) {
	directory := t.TempDir()
	legacyKey := make([]byte, keySize)
	if _, err := rand.Read(legacyKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, keyFileName), legacyKey, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := newMemoryWithClock(time.Now)
	legacy.directory = directory
	legacy.key = append([]byte(nil), legacyKey...)
	legacy.cookies[PlatformQQMusic] = []storedCookie{{
		Name: "qqmusic_key", Value: "legacy-session-secret", Domain: ".qq.com", Path: "/", Secure: true,
	}}
	legacy.updatedAt[PlatformQQMusic] = time.Now()
	if err := legacy.saveLocked(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory)
	if err != nil {
		t.Fatalf("旧版平台会话迁移失败: %v", err)
	}
	if status, statusErr := reopened.StatusFor("qq"); statusErr != nil || status.State != StateConfigured {
		t.Fatalf("旧版会话迁移后状态丢失: status=%+v err=%v", status, statusErr)
	}
	stored, err := os.ReadFile(filepath.Join(directory, keyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(stored, protectedKeyMagic) || bytes.Equal(stored, legacyKey) {
		t.Fatal("旧版明文密钥没有原子迁移为 DPAPI 格式")
	}
}
