package video

import (
	"os"
	"reflect"
	"testing"
	"time"

	"casescreen/internal/theme"
)

func TestCommandArgs(t *testing.T) {
	got := commandArgs(`D:\media\背景.mp4`, 480, 480, 20)
	want := []string{
		"-hide_banner", "-loglevel", "error", "-stream_loop", "-1",
		"-i", `D:\media\背景.mp4`, "-an", "-vf", "fps=20,scale=480:480:flags=lanczos",
		"-pix_fmt", "rgba", "-f", "rawvideo", "pipe:1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FFmpeg 参数不同\n got=%v\nwant=%v", got, want)
	}
}

func TestPlayerSmoke(t *testing.T) {
	ffmpegPath := os.Getenv("CASESCREEN_VIDEO_SMOKE_FFMPEG")
	sourcePath := os.Getenv("CASESCREEN_VIDEO_SMOKE_SOURCE")
	if ffmpegPath == "" || sourcePath == "" {
		t.Skip("设置 CASESCREEN_VIDEO_SMOKE_FFMPEG 和 CASESCREEN_VIDEO_SMOKE_SOURCE 后执行视频解码测试")
	}
	player, err := Open(ffmpegPath, sourcePath, 160, 90, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer player.Close()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		frame, sequence := player.Snapshot()
		if frame != nil && sequence > 0 {
			if frame.Bounds().Dx() != 160 || frame.Bounds().Dy() != 90 {
				t.Fatalf("视频帧尺寸无效: %v", frame.Bounds())
			}
			return
		}
		if err := player.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待视频帧超时")
}

func TestFindSource(t *testing.T) {
	manifest := theme.Manifest{
		Resources: []theme.Resource{{ID: "movie", LocalPath: `D:\media\背景.mp4`}},
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundVideo,
			Options: map[string]any{"resource_id": "movie"},
		}},
	}
	path, isVideo, err := FindSource(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !isVideo || path != `D:\media\背景.mp4` {
		t.Fatalf("视频资源查找结果无效: %q %v", path, isVideo)
	}
}
