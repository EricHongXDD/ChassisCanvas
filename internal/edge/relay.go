package edge

import (
	"context"
	"errors"
	"strings"

	"casescreen/internal/edgelyrics"
	"casescreen/internal/edgesync"
)

// ExactLyricsClient 是 EdgeSync `/v1/lyrics` 的最小客户端接口。
type ExactLyricsClient interface {
	ExactLyrics(context.Context, edgesync.Metadata) (edgesync.LyricsResult, error)
}

// NewRelayLyricsCore 创建默认的屏端歌词核心。完整歌词只从主机 relay 获取，
// 并继续使用 edgelyrics 按 source + track_id 落盘的 TF 缓存。
func NewRelayLyricsCore(cacheDirectory string, client ExactLyricsClient) LyricsEngine {
	return edgelyrics.NewWithExactProviders(cacheDirectory, nil,
		relayExactProvider{source: "qq", client: client},
		relayExactProvider{source: "netease", client: client},
	)
}

type relayExactProvider struct {
	source string
	client ExactLyricsClient
}

func (provider relayExactProvider) ID() string { return provider.source }

func (provider relayExactProvider) FetchExact(ctx context.Context, metadata edgelyrics.Metadata, _ edgelyrics.AuthSession) (edgelyrics.Result, error) {
	if provider.client == nil {
		return edgelyrics.Result{}, errors.New("主机歌词 relay 客户端未配置")
	}
	trackID := strings.TrimSpace(metadata.TrackID)
	if trackID == "" {
		return edgelyrics.Result{}, edgelyrics.ErrMissingTrackID
	}
	result, err := provider.client.ExactLyrics(ctx, edgesync.Metadata{
		Source: provider.source, TrackID: trackID,
	})
	if err != nil {
		return edgelyrics.Result{}, err
	}
	switch result.Mode {
	case edgesync.LyricModeTimed:
		return edgelyrics.Result{Type: edgelyrics.LyricsSynchronized, Document: result.Document}, nil
	case edgesync.LyricModePlain:
		return edgelyrics.Result{Type: edgelyrics.LyricsPlainText, PlainText: result.PlainText}, nil
	default:
		return edgelyrics.Result{}, errors.New("主机歌词 relay 返回未知歌词模式")
	}
}

func loginRequiredErrorText(message string) bool {
	return strings.Contains(message, edgesync.ErrPlatformLoginRequired.Error()) ||
		strings.Contains(message, "needs_login") || strings.Contains(message, "需要登录或有效会员权限")
}
