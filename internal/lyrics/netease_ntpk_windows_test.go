//go:build windows

package lyrics

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNeteaseNTPKPlaybackFieldsLive 只读取用户本机已安装客户端的资源包，
// 用于确认当前版本公开给自身前端的播放状态字段。测试不会解包到磁盘。
func TestNeteaseNTPKPlaybackFieldsLive(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CASESCREEN_NETEASE_NTPK"))
	if path == "" {
		t.Skip("设置 CASESCREEN_NETEASE_NTPK 后只读检查本机客户端资源包")
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 100 {
		t.Fatal("网易云资源包头或长度无效")
	}
	const headerBytes int64 = 100
	reader, err := zip.NewReader(io.NewSectionReader(file, headerBytes, info.Size()-headerBytes), info.Size()-headerBytes)
	if err != nil {
		t.Fatalf("读取网易云资源包失败: %v", err)
	}
	needles := [][]byte{
		[]byte("playingState"), []byte("curPlaying"), []byte("playingProgress"),
		[]byte("playProgress"), []byte("currentTime"), []byte("playTime"),
		[]byte("duration"), []byte("setPlaying"), []byte("resourceArtists"),
		[]byte("resourceDuration"), []byte("window.__autotest__"),
	}
	found := make(map[string]bool, len(needles))
	for _, entry := range reader.File {
		if !strings.HasSuffix(strings.ToLower(entry.Name), ".js") || entry.UncompressedSize64 > 96<<20 {
			continue
		}
		stream, openErr := entry.Open()
		if openErr != nil {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(stream, 96<<20))
		_ = stream.Close()
		if readErr != nil {
			continue
		}
		for _, needle := range needles {
			if bytes.Contains(content, needle) {
				found[string(needle)] = true
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("资源包中没有找到播放状态字段")
	}
	t.Logf("已确认 %d 个播放状态字段（不输出客户端脚本内容）", len(found))
}

func TestNeteaseNTPKPlayingProgressContractLive(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CASESCREEN_NETEASE_NTPK"))
	if path == "" {
		t.Skip("设置 CASESCREEN_NETEASE_NTPK 后检查播放进度契约")
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 100 {
		t.Fatal("网易云资源包头或长度无效")
	}
	const headerBytes int64 = 100
	reader, err := zip.NewReader(io.NewSectionReader(file, headerBytes, info.Size()-headerBytes), info.Size()-headerBytes)
	if err != nil {
		t.Fatal(err)
	}
	needles := [][]byte{
		[]byte("get playingInfo(){"),
		[]byte("get playingStatus(){"),
		[]byte("get playingProgress(){"),
		[]byte("window.__autotest__="),
	}
	for _, entry := range reader.File {
		if !strings.HasSuffix(entry.Name, "app.chunk.f26195c.js") {
			continue
		}
		stream, openErr := entry.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		content, readErr := io.ReadAll(io.LimitReader(stream, 96<<20))
		_ = stream.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, needle := range needles {
			if !bytes.Contains(content, needle) {
				t.Fatalf("资源包缺少播放契约字段 %q", needle)
			}
		}
		t.Log("已确认播放信息、状态、进度契约（不输出客户端脚本内容）")
		return
	}
	t.Fatal("没有找到播放进度契约")
}
