//go:build !windows

package lyrics

import (
	"context"
)

type windowSource struct{}

func NewNowPlayingSource() NowPlayingSource {
	return &windowSource{}
}

func (*windowSource) Current(context.Context) (NowPlaying, error) {
	return NowPlaying{}, nil
}
