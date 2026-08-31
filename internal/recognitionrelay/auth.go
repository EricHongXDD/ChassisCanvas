package recognitionrelay

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

const deviceTokenCharacters = 64

const (
	maximumTokenDigestFile = int64(2 << 20)
	maximumMappingFile     = int64(64 << 20)
)

// TokenDigestFile 只保存设备令牌的 SHA-256，不保存屏端使用的明文令牌。
type TokenDigestFile struct {
	Version int                `json:"version"`
	Tokens  []TokenDigestEntry `json:"tokens"`
}

// TokenDigestEntry 把一个可撤销令牌摘要绑定到非敏感设备标识。
type TokenDigestEntry struct {
	DeviceID    string `json:"device_id"`
	TokenSHA256 string `json:"token_sha256"`
}

type tokenRecord struct {
	deviceID string
	digest   [sha256.Size]byte
}

// TokenVerifier 对所有固定长度摘要完成常量时间比较，不在首个命中处提前返回。
type TokenVerifier struct {
	records []tokenRecord
}

// LoadTokenVerifier 读取仅含摘要的 JSON 文件。Linux 上拒绝组/其他用户可访问的文件。
func LoadTokenVerifier(path string) (*TokenVerifier, error) {
	data, err := readBoundedRegularFile(path, maximumTokenDigestFile, true)
	if err != nil {
		return nil, fmt.Errorf("读取设备令牌摘要文件失败: %w", err)
	}
	defer clear(data)
	var document TokenDigestFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("设备令牌摘要文件不是有效 JSON")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, errors.New("设备令牌摘要文件包含多余内容")
	}
	return NewTokenVerifier(document)
}

func readBoundedRegularFile(path string, maximumBytes int64, private bool) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("配置路径必须是普通文件")
	}
	if private && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("配置文件权限必须为 0600 或更严格")
	}
	if info.Size() < 0 || info.Size() > maximumBytes {
		return nil, errors.New("配置文件超过大小上限")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximumBytes {
		clear(data)
		return nil, errors.New("配置文件超过大小上限")
	}
	return data, nil
}

// NewTokenVerifier 验证摘要清单并拒绝重复设备或重复令牌。
func NewTokenVerifier(document TokenDigestFile) (*TokenVerifier, error) {
	if document.Version != 1 {
		return nil, errors.New("设备令牌摘要文件版本必须是 1")
	}
	if len(document.Tokens) == 0 || len(document.Tokens) > 4096 {
		return nil, errors.New("设备令牌摘要数量必须在 1..4096 之间")
	}
	deviceIDs := make(map[string]struct{}, len(document.Tokens))
	digests := make(map[[sha256.Size]byte]struct{}, len(document.Tokens))
	records := make([]tokenRecord, 0, len(document.Tokens))
	for _, entry := range document.Tokens {
		deviceID := strings.TrimSpace(entry.DeviceID)
		if !safeIdentifier(deviceID, 1, 64) {
			return nil, errors.New("设备标识只能包含 ASCII 字母、数字、点、下划线和连字符")
		}
		decoded, err := hex.DecodeString(strings.TrimSpace(entry.TokenSHA256))
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("设备令牌摘要必须是 64 位十六进制 SHA-256")
		}
		var digest [sha256.Size]byte
		copy(digest[:], decoded)
		clear(decoded)
		if _, found := deviceIDs[deviceID]; found {
			return nil, errors.New("设备令牌摘要文件包含重复设备标识")
		}
		if _, found := digests[digest]; found {
			return nil, errors.New("同一个设备令牌摘要不能分配给多台设备")
		}
		deviceIDs[deviceID] = struct{}{}
		digests[digest] = struct{}{}
		records = append(records, tokenRecord{deviceID: deviceID, digest: digest})
	}
	return &TokenVerifier{records: records}, nil
}

// Verify 返回令牌绑定的设备标识。候选摘要始终计算，格式判断也纳入最终结果。
func (verifier *TokenVerifier) Verify(candidate string) (string, bool) {
	if verifier == nil || len(verifier.records) == 0 {
		return "", false
	}
	candidateDigest := sha256.Sum256([]byte(candidate))
	formatValid := subtle.ConstantTimeEq(int32(validDeviceToken(candidate)), 1)
	matched := 0
	matchedIndex := 0
	for index := range verifier.records {
		equal := subtle.ConstantTimeCompare(candidateDigest[:], verifier.records[index].digest[:]) & formatValid
		matchedIndex = subtle.ConstantTimeSelect(equal, index+1, matchedIndex)
		matched |= equal
	}
	if matched != 1 || matchedIndex < 1 || matchedIndex > len(verifier.records) {
		return "", false
	}
	return verifier.records[matchedIndex-1].deviceID, true
}

func validDeviceToken(value string) int32 {
	if len(value) != deviceTokenCharacters {
		return 0
	}
	valid := int32(1)
	for index := 0; index < len(value); index++ {
		character := value[index]
		isDigit := character >= '0' && character <= '9'
		isLower := character >= 'a' && character <= 'f'
		isUpper := character >= 'A' && character <= 'F'
		if !isDigit && !isLower && !isUpper {
			valid = 0
		}
	}
	return valid
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := header[len(prefix):]
	if strings.ContainsAny(token, " \t\r\n,") {
		return token, false
	}
	return token, true
}

func safeIdentifier(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}
