package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"casescreen/internal/edgepush"
)

const testPairingKey = "0101010101010101010101010101010101010101010101010101010101010101"

func TestRunSendsExactNativePlayback(t *testing.T) {
	tests := []struct {
		name, source, trackID string
	}{
		{name: "QQ songmid", source: "qq", trackID: "0039MnYb0qxYhV"},
		{name: "网易云数字 ID", source: "netease", trackID: "186016"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receiver, err := edgepush.New(edgepush.Config{PairingKey: testPairingKey})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(receiver.Handler())
			defer server.Close()
			keyFile := writeTestKey(t, testPairingKey+"\n")
			var stdout, stderr bytes.Buffer
			err = run(context.Background(), []string{
				"-screen-url", server.URL, "-key-file", keyFile,
				"-source", test.source, "-track-id", test.trackID,
				"-position-ms", "1250", "-duration-ms", "240000", "-playing",
			}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("run() = %v; stderr=%q", err, stderr.String())
			}
			envelope, err := receiver.Current(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !envelope.Available || envelope.Metadata.Source != test.source ||
				envelope.Metadata.TrackID != test.trackID || !envelope.Track.Playing {
				t.Fatalf("接收状态不符: %+v", envelope)
			}
			position := time.Duration(envelope.Track.Position)
			if position < 1250*time.Millisecond || position > 2*time.Second {
				t.Fatalf("播放位置不符: %v", position)
			}
			output := stdout.String() + stderr.String()
			if strings.Contains(output, test.trackID) || strings.Contains(output, testPairingKey) {
				t.Fatalf("输出泄露歌曲 ID 或配对密钥: %q", output)
			}
		})
	}
}

func TestRunClear(t *testing.T) {
	receiver, err := edgepush.New(edgepush.Config{PairingKey: testPairingKey})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	keyFile := writeTestKey(t, testPairingKey)
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"-screen-url", server.URL, "-key-file", keyFile,
		"-source", "qq", "-track-id", "0039MnYb0qxYhV",
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"-screen-url", server.URL, "-key-file", keyFile, "-clear",
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	envelope, err := receiver.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Available {
		t.Fatalf("清空后仍有播放状态: %+v", envelope)
	}
}

func TestRunRejectsInferredOrMalformedInputs(t *testing.T) {
	keyFile := writeTestKey(t, testPairingKey)
	base := []string{"-screen-url", "http://127.0.0.1:1", "-key-file", keyFile}
	tests := []struct {
		name string
		args []string
	}{
		{name: "平台别名", args: []string{"-source", "qqmusic", "-track-id", "0039MnYb0qxYhV"}},
		{name: "QQ 使用网易云 ID", args: []string{"-source", "qq", "-track-id", "186016"}},
		{name: "网易云使用 QQ songmid", args: []string{"-source", "netease", "-track-id", "0039MnYb0qxYhV"}},
		{name: "缺少 ID", args: []string{"-source", "qq"}},
		{name: "负位置", args: []string{"-source", "qq", "-track-id", "0039MnYb0qxYhV", "-position-ms", "-1"}},
		{name: "位置超过时长", args: []string{"-source", "qq", "-track-id", "0039MnYb0qxYhV", "-position-ms", "2", "-duration-ms", "1"}},
		{name: "清空混入歌曲", args: []string{"-clear", "-source", "qq", "-track-id", "0039MnYb0qxYhV"}},
		{name: "禁止标题参数", args: []string{"-source", "qq", "-track-id", "0039MnYb0qxYhV", "-title", "猜测歌曲"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append(append([]string{}, base...), test.args...)
			if err := run(context.Background(), args, ioDiscard{}, ioDiscard{}); err == nil {
				t.Fatal("run() 未拒绝无效输入")
			}
		})
	}
}

func TestReadPairingKeyRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name, content string
	}{
		{name: "密钥无效", content: strings.Repeat("z", 64)},
		{name: "前导空白", content: " " + testPairingKey},
		{name: "多行", content: testPairingKey + "\nextra\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTestKey(t, test.content)
			key, err := readPairingKey(path)
			if err == nil {
				// 十六进制内容由 Publisher 做最终校验；本辅助函数只验证文件边界。
				if _, publisherErr := edgepush.NewPublisher(edgepush.PublisherConfig{BaseURL: "http://127.0.0.1", PairingKey: key}); publisherErr == nil {
					t.Fatal("无效密钥未被拒绝")
				}
			}
		})
	}
}

func TestReadPairingKeyRejectsWideUnixPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACL 不映射为 Unix 权限位")
	}
	path := filepath.Join(t.TempDir(), "pairing.key")
	if err := os.WriteFile(path, []byte(testPairingKey), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPairingKey(path); err == nil {
		t.Fatal("未拒绝权限过宽的密钥文件")
	}
}

func writeTestKey(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pairing.key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) { return len(data), nil }
