package lyrics

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
)

// LocalFileProvider 从桌面端指定的 LRC 文件读取歌词，适合播放器不提供媒体会话的场景。
type LocalFileProvider struct {
	Path string
	mu   sync.RWMutex
}

func (*LocalFileProvider) ID() string { return "local" }

func (provider *LocalFileProvider) Fetch(_ context.Context, _ NowPlaying) (Document, error) {
	provider.mu.RLock()
	path := strings.TrimSpace(provider.Path)
	provider.mu.RUnlock()
	if path == "" {
		return Document{}, errors.New("尚未设置本地 LRC 路径")
	}
	file, err := os.Open(path)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	return ParseLRC(file)
}
