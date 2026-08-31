package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const commandTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestCheckModeBuildsDeterministicRelayWithoutListening(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tokens.json")
	digest := sha256.Sum256([]byte(commandTestToken))
	document := fmt.Sprintf(`{"version":1,"tokens":[{"device_id":"screen-test","token_sha256":"%s"}]}`, hex.EncodeToString(digest[:]))
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"-check", "-provider", "deterministic", "-token-digests", path}, &stdout, &stderr); err != nil {
		t.Fatalf("配置检查失败: %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "配置检查通过") {
		t.Fatalf("检查输出缺失: %q", stdout.String())
	}
}

func TestCommandRejectsMissingTokenDigestFileAndPublicListenByDefault(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"-check", "-provider", "deterministic"}, &output, &output); err == nil {
		t.Fatal("缺少设备令牌摘要文件不应通过")
	}
	if !loopbackListenAddress("127.0.0.1:19323") || !loopbackListenAddress("[::1]:19323") ||
		loopbackListenAddress("0.0.0.0:19323") {
		t.Fatal("回环监听地址判断错误")
	}
}
