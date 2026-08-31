package edgesync

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

const tokenBytes = 32

// GenerateToken 生成 256 位随机配对令牌。令牌只应在本机配置界面显示一次。
func GenerateToken() (string, error) {
	random := make([]byte, tokenBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func tokenDigest(token string) ([sha256.Size]byte, int, error) {
	var empty [sha256.Size]byte
	if token != strings.TrimSpace(token) {
		return empty, 0, errors.New("边缘同步令牌不能包含首尾空白")
	}
	if len(token) != tokenBytes*2 {
		return empty, 0, errors.New("边缘同步令牌必须是 64 位十六进制随机值")
	}
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != tokenBytes {
		return empty, 0, errors.New("边缘同步令牌必须是 64 位十六进制随机值")
	}
	return sha256.Sum256([]byte(token)), len(token), nil
}

// secureTokenEqual 对固定长度摘要执行常量时间比较；长度也纳入最终判断。
func secureTokenEqual(candidate string, expected [sha256.Size]byte, expectedLength int) bool {
	candidateDigest := sha256.Sum256([]byte(candidate))
	digestEqual := subtle.ConstantTimeCompare(candidateDigest[:], expected[:])
	lengthEqual := 0
	if len(candidate) == expectedLength {
		lengthEqual = 1
	}
	return digestEqual&lengthEqual == 1
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := header[len(prefix):]
	if strings.ContainsAny(token, " \t\r\n,") {
		return "", false
	}
	return token, true
}
