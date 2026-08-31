package edge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"casescreen/internal/edgelyrics"
	"casescreen/internal/edgesync"
	"casescreen/internal/lyrics"
)

type fakeRelayClient struct {
	mu           sync.Mutex
	envelope     edgesync.Envelope
	result       edgesync.LyricsResult
	lyricsErr    error
	currentCalls int
	lyricsCalls  int
	received     []edgesync.Metadata
}

func (client *fakeRelayClient) Current(context.Context) (edgesync.Envelope, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.currentCalls++
	return client.envelope, nil
}

func (client *fakeRelayClient) ExactLyrics(_ context.Context, metadata edgesync.Metadata) (edgesync.LyricsResult, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.lyricsCalls++
	client.received = append(client.received, metadata)
	return client.result, client.lyricsErr
}

func timedRelayResult() edgesync.LyricsResult {
	return edgesync.LyricsResult{
		Mode: edgesync.LyricModeTimed,
		Document: lyrics.Document{Lines: []lyrics.Line{
			{At: 0, Text: "第一句"}, {At: 4 * time.Second, Text: "第二句"},
		}},
	}
}

func TestRelayCoreFetchesOncePerPlatformIDAndReusesTFCache(t *testing.T) {
	cacheDirectory := t.TempDir()
	client := &fakeRelayClient{result: timedRelayResult()}
	metadata := edgelyrics.Metadata{
		Source: "qq", TrackID: "0039MnYb0qxYhV", Title: "测试歌曲",
		PositionKnown: true, Playing: true, CapturedAt: time.Now(),
	}
	core := NewRelayLyricsCore(cacheDirectory, client)
	if err := core.Update(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Position = int64(time.Second)
	if err := core.Update(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	if client.lyricsCalls != 1 {
		t.Fatalf("同一歌曲进度更新重复访问 /v1/lyrics: %d", client.lyricsCalls)
	}
	if received := client.received[0]; received.Source != "qq" || received.TrackID != "0039MnYb0qxYhV" {
		t.Fatalf("没有使用同平台精确 ID 请求 relay: %#v", received)
	}

	// 模拟副屏服务重启；新的 Core 应直接读取 TF，不再访问主机完整歌词接口。
	restarted := NewRelayLyricsCore(cacheDirectory, client)
	if err := restarted.Update(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	if client.lyricsCalls != 1 {
		t.Fatalf("重启后未命中 TF 歌词缓存: %d", client.lyricsCalls)
	}
	if state := restarted.Snapshot(); state.Provider != "cache" || state.LyricsType != edgelyrics.LyricsSynchronized {
		t.Fatalf("重启后的缓存状态错误: %#v", state)
	}
}

func TestRunnerHighFrequencyPollOnlyUsesNowPlayingEndpoint(t *testing.T) {
	client := &fakeRelayClient{
		envelope: edgesync.Envelope{
			Available: true,
			Metadata:  edgesync.Metadata{Source: "netease", TrackID: "186016", LyricMode: edgesync.LyricModeTimed},
			Track:     lyrics.NowPlaying{Title: "晴天", Playing: true, PositionKnown: true},
		},
		result: timedRelayResult(),
	}
	cacheDirectory := t.TempDir()
	runner, err := NewRunner(validTestConfig(), client, func() LyricsEngine {
		return NewRelayLyricsCore(cacheDirectory, client)
	}, &fakeDisplay{})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		client.envelope.Track.Position = int64(index) * int64(time.Second)
		if err := runner.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if client.currentCalls != 5 {
		t.Fatalf("now-playing 轮询次数错误: %d", client.currentCalls)
	}
	if client.lyricsCalls != 1 {
		t.Fatalf("高频轮询重复拉取完整歌词: %d", client.lyricsCalls)
	}
}

func TestRelayNeedsLoginStaysInStatusWhileLyricsLayerIsHidden(t *testing.T) {
	client := &fakeRelayClient{lyricsErr: edgesync.ErrPlatformLoginRequired}
	core := NewRelayLyricsCore(t.TempDir(), client)
	err := core.Update(context.Background(), edgelyrics.Metadata{
		Source: "qq", TrackID: "0039MnYb0qxYhV", Title: "需要登录的歌曲",
	})
	if err == nil || !loginRequiredErrorText(err.Error()) {
		t.Fatalf("relay needs_login 没有保留到 Core 状态: %v", err)
	}
	state := core.Snapshot()
	update := DisplayUpdate(state, edgesync.LyricModeUnknown, "")
	if update.Current != "" || update.Next != "" {
		t.Fatalf("needs_login 仍污染歌词图层: %#v", update)
	}
}

func TestRelayProviderPreservesLoginSentinelAtBoundary(t *testing.T) {
	client := &fakeRelayClient{lyricsErr: edgesync.ErrPlatformLoginRequired}
	provider := relayExactProvider{source: "netease", client: client}
	_, err := provider.FetchExact(context.Background(), edgelyrics.Metadata{TrackID: "186016"}, nil)
	if !errors.Is(err, edgesync.ErrPlatformLoginRequired) {
		t.Fatalf("provider 边界丢失 needs_login: %v", err)
	}
}
