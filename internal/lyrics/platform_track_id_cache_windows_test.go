//go:build windows

package lyrics

import (
	"context"
	"testing"
	"time"
)

func TestPlatformTrackIDIsRevalidatedOnEverySuccessfulPoll(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	calls := 0
	source := &fallbackNowPlayingSource{
		clock: func() time.Time { return now },
		enrich: func(_ context.Context, track NowPlaying) NowPlaying {
			calls++
			track.TrackID = "186016"
			track.TrackIDOrigin = TrackIDOriginSource
			return track
		},
	}
	raw := neteaseWebDBCurrentTrack()
	first := source.withPlatformTrackID(context.Background(), raw)
	now = now.Add(time.Second)
	second := source.withPlatformTrackID(context.Background(), raw)
	if first.SourceTrackID() != "186016" || second.SourceTrackID() != "186016" {
		t.Fatalf("原生 ID 没有在同一首歌中复用: first=%+v second=%+v", first, second)
	}
	if calls != 2 {
		t.Fatalf("成功的旧 ID 被沿用，没有重新验证当前播放证据: %d", calls)
	}
}

func TestPlatformTrackIDCacheThrottlesFailedQueriesAndReprobes(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	calls := 0
	source := &fallbackNowPlayingSource{
		clock: func() time.Time { return now },
		enrich: func(_ context.Context, track NowPlaying) NowPlaying {
			calls++
			return track
		},
	}
	raw := neteaseWebDBCurrentTrack()
	_ = source.withPlatformTrackID(context.Background(), raw)
	now = now.Add(time.Second)
	_ = source.withPlatformTrackID(context.Background(), raw)
	if calls != 1 {
		t.Fatalf("五秒窗口内重复查询了本地数据库: %d", calls)
	}
	now = now.Add(platformIDProbeInterval)
	_ = source.withPlatformTrackID(context.Background(), raw)
	if calls != 2 {
		t.Fatalf("重试窗口后没有重新尝试取得原生 ID: %d", calls)
	}
}

func TestPlatformTrackIDCacheDoesNotCarryIDAcrossTracks(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	calls := 0
	source := &fallbackNowPlayingSource{
		clock: func() time.Time { return now },
		enrich: func(_ context.Context, track NowPlaying) NowPlaying {
			calls++
			if track.Title == "第一首" {
				track.TrackID = "186016"
				track.TrackIDOrigin = TrackIDOriginSource
			}
			return track
		},
	}
	first := neteaseWebDBCurrentTrack()
	first.Title = "第一首"
	if source.withPlatformTrackID(context.Background(), first).SourceTrackID() == "" {
		t.Fatal("第一首没有取得原生 ID")
	}
	second := first
	second.Title = "第二首"
	now = now.Add(time.Second)
	if got := source.withPlatformTrackID(context.Background(), second).SourceTrackID(); got != "" {
		t.Fatalf("切歌后沿用了上一首的 ID: %q", got)
	}
	if calls != 2 {
		t.Fatalf("切歌后没有立即查询新身份: %d", calls)
	}
}

func TestPlatformTrackIDProbeHonorsCanceledRefresh(t *testing.T) {
	calls := 0
	source := &fallbackNowPlayingSource{
		enrich: func(_ context.Context, track NowPlaying) NowPlaying {
			calls++
			return track
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	track := neteaseWebDBCurrentTrack()
	track.TrackID = "186016"
	track.TrackIDOrigin = TrackIDOriginSource
	if got := source.withPlatformTrackID(ctx, track); got.SourceTrackID() != "" || got.TrackIDOrigin != "" || calls != 0 {
		t.Fatalf("已取消刷新仍执行原生 ID 探测: id=%q calls=%d", got.SourceTrackID(), calls)
	}
}

func TestPlatformTrackIDWrapperRevalidatesPrefilledNeteaseID(t *testing.T) {
	calls := 0
	source := &fallbackNowPlayingSource{
		enrich: func(_ context.Context, track NowPlaying) NowPlaying {
			calls++
			track.TrackID = "757567"
			track.TrackIDOrigin = TrackIDOriginSource
			return track
		},
	}
	track := neteaseWebDBCurrentTrack()
	track.TrackID = "186016"
	track.TrackIDOrigin = TrackIDOriginSource
	result := source.withPlatformTrackID(context.Background(), track)
	if result.SourceTrackID() != "757567" || calls != 1 {
		t.Fatalf("外层调用没有强制重验预填网易云 ID: id=%q calls=%d", result.SourceTrackID(), calls)
	}

	source.lastProbeFailed = true
	source.lastProbeAt = time.Now()
	source.lastProbeTrack = track
	calls = 0
	result = source.withPlatformTrackID(context.Background(), track)
	if result.SourceTrackID() != "" || result.TrackIDOrigin != "" || calls != 0 {
		t.Fatalf("退避期返回了未重验的网易云 ID: result=%+v calls=%d", result, calls)
	}
}

func TestPlatformTrackIDWrapperRevalidatesPrefilledQQID(t *testing.T) {
	calls := 0
	source := &fallbackNowPlayingSource{
		enrich: func(_ context.Context, track NowPlaying) NowPlaying {
			calls++
			track.TrackID = "0039MnYb0qxYhV"
			track.TrackIDOrigin = TrackIDOriginSource
			return track
		},
	}
	track := qqInjectorCurrentTrack()
	track.TrackID = "004Z8Ihr0JIu5s"
	track.TrackIDOrigin = TrackIDOriginSource
	result := source.withPlatformTrackID(context.Background(), track)
	if result.SourceTrackID() != "0039MnYb0qxYhV" || calls != 1 {
		t.Fatalf("外层调用没有强制重验预填 QQ ID: id=%q calls=%d", result.SourceTrackID(), calls)
	}

	source.lastProbeFailed = true
	source.lastProbeAt = time.Now()
	source.lastProbeTrack = track
	calls = 0
	result = source.withPlatformTrackID(context.Background(), track)
	if result.SourceTrackID() != "" || result.TrackIDOrigin != "" || result.QQSongID != 0 || calls != 0 {
		t.Fatalf("退避期返回了未重验的 QQ ID: result=%+v calls=%d", result, calls)
	}
}
