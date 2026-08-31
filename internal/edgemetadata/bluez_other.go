//go:build !linux

package edgemetadata

import (
	"context"

	"casescreen/internal/lyrics"
)

// BlueZSource 在非 Linux 平台保持相同接口，便于桌面端构建共享代码。
type BlueZSource struct{}

func NewBlueZSource() *BlueZSource { return &BlueZSource{} }

func (*BlueZSource) Current(context.Context) (lyrics.NowPlaying, error) {
	return lyrics.NowPlaying{}, nil
}
