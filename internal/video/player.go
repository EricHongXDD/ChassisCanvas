package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"casescreen/internal/theme"
)

type Player struct {
	mu       sync.RWMutex
	frame    *image.RGBA
	sequence uint64
	err      error

	cancel context.CancelFunc
	done   chan struct{}
}

func Open(ffmpegPath, sourcePath string, width, height, fps int) (*Player, error) {
	if width <= 0 || height <= 0 || width > 3840 || height > 2160 {
		return nil, fmt.Errorf("视频解码尺寸无效: %dx%d", width, height)
	}
	if fps < 1 || fps > 30 {
		return nil, fmt.Errorf("视频帧率无效: %d", fps)
	}
	if err := requireFile(ffmpegPath, "FFmpeg"); err != nil {
		return nil, err
	}
	if err := requireFile(sourcePath, "视频资源"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, ffmpegPath, commandArgs(sourcePath, width, height, fps)...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("创建 FFmpeg 输出管道: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("启动 FFmpeg: %w", err)
	}
	player := &Player{cancel: cancel, done: make(chan struct{})}
	go player.readLoop(ctx, command, stdout, &stderr, width, height)
	return player, nil
}

func (player *Player) Snapshot() (*image.RGBA, uint64) {
	player.mu.RLock()
	defer player.mu.RUnlock()
	return player.frame, player.sequence
}

func (player *Player) Err() error {
	player.mu.RLock()
	defer player.mu.RUnlock()
	return player.err
}

func (player *Player) Close() error {
	if player == nil || player.cancel == nil {
		return nil
	}
	player.cancel()
	select {
	case <-player.done:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("等待 FFmpeg 退出超时")
	}
}

func (player *Player) readLoop(
	ctx context.Context,
	command *exec.Cmd,
	stdout io.Reader,
	stderr *bytes.Buffer,
	width, height int,
) {
	defer close(player.done)
	frameSize := width * height * 4
	for {
		data := make([]byte, frameSize)
		if _, err := io.ReadFull(stdout, data); err != nil {
			waitErr := command.Wait()
			if ctx.Err() == nil {
				message := strings.TrimSpace(stderr.String())
				if message != "" {
					err = fmt.Errorf("FFmpeg 解码停止: %w（%s）", firstError(err, waitErr), message)
				} else {
					err = fmt.Errorf("FFmpeg 解码停止: %w", firstError(err, waitErr))
				}
				player.setError(err)
			}
			return
		}
		frame := &image.RGBA{
			Pix: data, Stride: width * 4,
			Rect: image.Rect(0, 0, width, height),
		}
		player.mu.Lock()
		player.frame = frame
		player.sequence++
		player.mu.Unlock()
	}
}

func (player *Player) setError(err error) {
	player.mu.Lock()
	player.err = err
	player.mu.Unlock()
}

func commandArgs(sourcePath string, width, height, fps int) []string {
	filter := fmt.Sprintf("fps=%d,scale=%d:%d:flags=lanczos", fps, width, height)
	return []string{
		"-hide_banner", "-loglevel", "error", "-stream_loop", "-1",
		"-i", sourcePath, "-an", "-vf", filter,
		"-pix_fmt", "rgba", "-f", "rawvideo", "pipe:1",
	}
}

func FindSource(manifest theme.Manifest) (string, bool, error) {
	for _, layer := range manifest.Layers {
		if layer.Type != theme.LayerBackgroundVideo {
			continue
		}
		resourceID, _ := layer.Options["resource_id"].(string)
		if resourceID != "" {
			for _, resource := range manifest.Resources {
				if resource.ID == resourceID {
					if !isVideoExtension(filepath.Ext(resource.LocalPath)) {
						// 允许只有海报的编辑状态，不启动 FFmpeg，渲染器仍可显示海报。
						return "", false, nil
					}
					return resource.LocalPath, true, nil
				}
			}
			return "", true, fmt.Errorf("视频背景引用了不存在的资源 %q", resourceID)
		}
		for _, resource := range manifest.Resources {
			if isVideoExtension(filepath.Ext(resource.LocalPath)) {
				return resource.LocalPath, true, nil
			}
		}
		return "", false, nil
	}
	return "", false, nil
}

func isVideoExtension(extension string) bool {
	switch strings.ToLower(extension) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".gif":
		return true
	default:
		return false
	}
}

func requireFile(path, label string) error {
	if path == "" {
		return fmt.Errorf("%s 路径为空", label)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s 不可用: %w", label, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s 路径是目录", label)
	}
	return nil
}

func firstError(errorsToCheck ...error) error {
	for _, err := range errorsToCheck {
		if err != nil {
			return err
		}
	}
	return io.EOF
}
