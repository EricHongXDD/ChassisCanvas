package recognitionrelay

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

const testDeviceToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testTokenVerifier(t *testing.T) *TokenVerifier {
	t.Helper()
	digest := sha256.Sum256([]byte(testDeviceToken))
	verifier, err := NewTokenVerifier(TokenDigestFile{
		Version: 1,
		Tokens:  []TokenDigestEntry{{DeviceID: "waveshare-7", TokenSHA256: hex.EncodeToString(digest[:])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func TestTokenVerifierUsesDigestAndFixedTokenFormat(t *testing.T) {
	verifier := testTokenVerifier(t)
	if deviceID, ok := verifier.Verify(testDeviceToken); !ok || deviceID != "waveshare-7" {
		t.Fatalf("正确令牌未通过: device=%q ok=%v", deviceID, ok)
	}
	for _, candidate := range []string{
		"short", testDeviceToken + "0", "g" + testDeviceToken[1:],
		"1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if deviceID, ok := verifier.Verify(candidate); ok || deviceID != "" {
			t.Fatalf("错误令牌不应通过: %q device=%q", candidate, deviceID)
		}
	}
}

func TestTokenVerifierRejectsDuplicateAssignments(t *testing.T) {
	digest := sha256.Sum256([]byte(testDeviceToken))
	hexDigest := hex.EncodeToString(digest[:])
	_, err := NewTokenVerifier(TokenDigestFile{Version: 1, Tokens: []TokenDigestEntry{
		{DeviceID: "screen-a", TokenSHA256: hexDigest},
		{DeviceID: "screen-b", TokenSHA256: hexDigest},
	}})
	if err == nil {
		t.Fatal("同一令牌摘要不应分配给多台设备")
	}
}
