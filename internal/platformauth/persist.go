package platformauth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/gob"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	keyFileName  = "platform-sessions.key"
	dataFileName = "platform-sessions.bin"
	keySize      = 32
)

var encryptedFileMagic = []byte("CSPA1")
var protectedKeyMagic = []byte("CSPK2")

type persistedState struct {
	Version    int
	Cookies    map[string][]persistedCookie
	UpdatedAt  map[string]int64
	NeedsLogin map[string]bool
}

type persistedCookie struct {
	Name          string
	Value         string
	Domain        string
	Path          string
	ExpiresAtUnix int64
	HasExpiry     bool
	Secure        bool
	HTTPOnly      bool
}

// Open 打开持久化会话存储。密钥和加密数据文件均尽量设置为 0600，目录为 0700。
func Open(directory string) (*Store, error) {
	directory = filepath.Clean(directory)
	if directory == "" || directory == "." {
		return nil, errors.New("平台会话存储目录无效")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, errors.New("无法创建平台会话存储目录")
	}
	_ = os.Chmod(directory, 0o700)
	key, err := loadOrCreateKey(filepath.Join(directory, keyFileName))
	if err != nil {
		return nil, err
	}
	store := newMemoryWithClock(time.Now)
	store.directory = directory
	store.key = key
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		key, migrate, decodeErr := decodeStoredKey(data)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if migrate {
			encoded, encodeErr := encodeKeyForStorage(key)
			if encodeErr != nil {
				return nil, encodeErr
			}
			if writeErr := writeFileAtomic0600(path, encoded); writeErr != nil {
				return nil, errors.New("无法迁移平台会话密钥保护")
			}
		}
		_ = os.Chmod(path, 0o600)
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, errors.New("无法读取平台会话密钥")
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, errors.New("无法生成平台会话密钥")
	}
	encoded, err := encodeKeyForStorage(key)
	if err != nil {
		return nil, err
	}
	installed, err := installNewKeyAtomic(path, encoded)
	if err != nil {
		return nil, err
	}
	if installed {
		return key, nil
	}
	// 另一进程已经原子安装了完整密钥；丢弃本进程随机生成的候选，
	// 读取胜出的文件，保证所有实例使用同一个 AES 密钥。
	return loadOrCreateKey(path)
}

// installNewKeyAtomic 先完整写入同目录临时文件，再用硬链接执行
// “仅当目标不存在时创建”。因此其他进程不会读到空文件或半个 DPAPI
// blob，创建进程在发布前崩溃也只会留下可忽略的临时文件。
func installNewKeyAtomic(path string, encoded []byte) (bool, error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".platform-key-*.tmp")
	if err != nil {
		return false, errors.New("无法创建平台会话密钥临时文件")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	_ = temporary.Chmod(0o600)
	if _, err = temporary.Write(encoded); err != nil {
		temporary.Close()
		return false, errors.New("无法写入平台会话密钥")
	}
	if err = temporary.Sync(); err != nil {
		temporary.Close()
		return false, errors.New("无法同步平台会话密钥")
	}
	if err = temporary.Close(); err != nil {
		return false, errors.New("无法关闭平台会话密钥")
	}
	if err = os.Link(temporaryPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return false, nil
		}
		return false, errors.New("无法原子创建平台会话密钥")
	}
	_ = os.Chmod(path, 0o600)
	return true, nil
}

func (store *Store) saveLocked() error {
	if store.directory == "" {
		return nil
	}
	state := persistedState{
		Version:    1,
		Cookies:    make(map[string][]persistedCookie, len(store.cookies)),
		UpdatedAt:  make(map[string]int64, len(store.updatedAt)),
		NeedsLogin: make(map[string]bool, len(store.needsLogin)),
	}
	for platform, cookies := range store.cookies {
		persisted := make([]persistedCookie, 0, len(cookies))
		for _, cookie := range cookies {
			item := persistedCookie{
				Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path,
				Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly,
			}
			if !cookie.ExpiresAt.IsZero() {
				item.HasExpiry = true
				item.ExpiresAtUnix = cookie.ExpiresAt.Unix()
			}
			persisted = append(persisted, item)
		}
		state.Cookies[string(platform)] = persisted
	}
	for platform, updatedAt := range store.updatedAt {
		state.UpdatedAt[string(platform)] = updatedAt.UnixNano()
	}
	for platform, required := range store.needsLogin {
		if required {
			state.NeedsLogin[string(platform)] = true
		}
	}

	var plaintext bytes.Buffer
	if err := gob.NewEncoder(&plaintext).Encode(state); err != nil {
		return errors.New("无法编码平台会话")
	}
	encrypted, err := encryptState(store.key, plaintext.Bytes())
	if err != nil {
		return err
	}
	return writeFileAtomic0600(filepath.Join(store.directory, dataFileName), encrypted)
}

func (store *Store) load() error {
	data, err := os.ReadFile(filepath.Join(store.directory, dataFileName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("无法读取平台会话文件")
	}
	plaintext, err := decryptState(store.key, data)
	if err != nil {
		return err
	}
	var state persistedState
	if err := gob.NewDecoder(bytes.NewReader(plaintext)).Decode(&state); err != nil || state.Version != 1 {
		return errors.New("平台会话文件损坏或版本不受支持")
	}
	for platformName, cookies := range state.Cookies {
		platform, parseErr := parsePlatform(platformName)
		if parseErr != nil {
			continue
		}
		policy := platformPolicies[platform]
		for _, item := range cookies {
			if _, allowed := policy.allowedCookies[item.Name]; !allowed ||
				!domainAllowedForPlatform(item.Domain, policy) || !validCookieValue(item.Value) {
				continue
			}
			cookie := storedCookie{
				Name: item.Name, Value: item.Value, Domain: item.Domain, Path: item.Path,
				Secure: item.Secure, HTTPOnly: item.HTTPOnly,
			}
			if item.HasExpiry {
				cookie.ExpiresAt = time.Unix(item.ExpiresAtUnix, 0)
			}
			store.cookies[platform] = append(store.cookies[platform], cookie)
		}
		sortStoredCookies(store.cookies[platform])
	}
	for platformName, nanoseconds := range state.UpdatedAt {
		platform, parseErr := parsePlatform(platformName)
		if parseErr == nil {
			store.updatedAt[platform] = time.Unix(0, nanoseconds)
		}
	}
	for platformName, required := range state.NeedsLogin {
		platform, parseErr := parsePlatform(platformName)
		if parseErr == nil && required {
			store.needsLogin[platform] = true
		}
	}
	_ = os.Chmod(filepath.Join(store.directory, dataFileName), 0o600)
	return nil
}

func encryptState(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("平台会话密钥无效")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("无法初始化平台会话加密")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("无法生成平台会话随机数")
	}
	output := append([]byte(nil), encryptedFileMagic...)
	output = append(output, nonce...)
	output = aead.Seal(output, nonce, plaintext, encryptedFileMagic)
	return output, nil
}

func decryptState(key, encrypted []byte) ([]byte, error) {
	if len(encrypted) < len(encryptedFileMagic) || !bytes.Equal(encrypted[:len(encryptedFileMagic)], encryptedFileMagic) {
		return nil, errors.New("平台会话文件格式无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("平台会话密钥无效")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("无法初始化平台会话解密")
	}
	offset := len(encryptedFileMagic)
	if len(encrypted) < offset+aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("平台会话文件长度无效")
	}
	nonce := encrypted[offset : offset+aead.NonceSize()]
	ciphertext := encrypted[offset+aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, encryptedFileMagic)
	if err != nil {
		return nil, errors.New("平台会话文件损坏或密钥不匹配")
	}
	return plaintext, nil
}

func writeFileAtomic0600(path string, data []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".platform-session-*.tmp")
	if err != nil {
		return errors.New("无法创建平台会话临时文件")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	_ = temporary.Chmod(0o600)
	if _, err = temporary.Write(data); err != nil {
		temporary.Close()
		return errors.New("无法写入平台会话文件")
	}
	if err = temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("无法同步平台会话文件")
	}
	if err = temporary.Close(); err != nil {
		return errors.New("无法关闭平台会话文件")
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return errors.New("无法替换平台会话文件")
	}
	_ = os.Chmod(path, 0o600)
	return nil
}
