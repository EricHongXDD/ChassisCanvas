package edgesync

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateTokenProduces256Bits(t *testing.T) {
	first, err := GenerateToken()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	second, err := GenerateToken()
	if err != nil {
		t.Fatalf("生成第二个令牌失败: %v", err)
	}
	if first == second {
		t.Fatal("连续生成了相同令牌")
	}
	decoded, err := hex.DecodeString(first)
	if err != nil || len(decoded) != tokenBytes {
		t.Fatalf("令牌不是 %d 字节十六进制值: %q", tokenBytes, first)
	}
}

func TestTokenValidationAndConstantTimeComparison(t *testing.T) {
	token := strings.Repeat("a1", tokenBytes)
	digest, length, err := tokenDigest(token)
	if err != nil {
		t.Fatalf("有效令牌被拒绝: %v", err)
	}
	if !secureTokenEqual(token, digest, length) {
		t.Fatal("正确令牌没有通过比较")
	}
	if secureTokenEqual(strings.Repeat("b2", tokenBytes), digest, length) {
		t.Fatal("错误令牌通过了比较")
	}
	if secureTokenEqual(token[:len(token)-1], digest, length) {
		t.Fatal("长度错误的令牌通过了比较")
	}
	for _, invalid := range []string{"", "abcd", strings.Repeat("z", tokenBytes*2), " " + token} {
		if _, _, err := tokenDigest(invalid); err == nil {
			t.Fatalf("无效令牌未被拒绝: %q", invalid)
		}
	}
}

func TestBearerTokenParsing(t *testing.T) {
	token := strings.Repeat("ab", tokenBytes)
	for _, header := range []string{"Bearer " + token, "bearer " + token, "BEARER " + token} {
		parsed, ok := bearerToken(header)
		if !ok || parsed != token {
			t.Fatalf("无法解析有效认证头: %q", header)
		}
	}
	for _, header := range []string{"", token, "Basic " + token, "Bearer ", "Bearer  " + token, "Bearer " + token + ",extra"} {
		if _, ok := bearerToken(header); ok {
			t.Fatalf("错误认证头未被拒绝: %q", header)
		}
	}
}
