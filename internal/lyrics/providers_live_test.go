package lyrics

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestProvidersLiveSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_LYRICS_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_LYRICS_LIVE=1 后执行三家歌词在线测试")
	}
	tests := []struct {
		provider Provider
		track    NowPlaying
	}{
		{provider: NeteaseProvider{}, track: NowPlaying{Source: "netease", TrackID: "186016", TrackIDOrigin: TrackIDOriginSource, Title: "晴天", Artist: "周杰伦", Album: "叶惠美", Duration: int64(269 * time.Second)}},
		{provider: QQProvider{}, track: NowPlaying{Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource, Title: "晴天", Artist: "周杰伦", Album: "叶惠美", Duration: int64(269 * time.Second)}},
		{provider: KugouProvider{}, track: NowPlaying{Title: "晴天", Artist: "周杰伦", Album: "叶惠美", Duration: int64(269 * time.Second)}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.provider.ID(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			document, err := test.provider.Fetch(ctx, test.track)
			if err != nil {
				t.Fatal(err)
			}
			if len(document.Lines) < 10 {
				t.Fatalf("同步歌词行数异常: %d", len(document.Lines))
			}
			t.Logf("%s 返回 %d 行同步歌词", test.provider.ID(), len(document.Lines))
		})
	}
}

func TestQQAndNeteaseExactIDLiveSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_LYRICS_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_LYRICS_LIVE=1 后执行平台歌曲 ID 在线测试")
	}
	tests := []struct {
		provider Provider
		trackID  string
	}{
		{provider: NeteaseProvider{}, trackID: "186016"},
		{provider: QQProvider{}, trackID: "0039MnYb0qxYhV"},
	}
	for _, test := range tests {
		provider := test.provider
		t.Run(provider.ID(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			second, err := provider.Fetch(ctx, NowPlaying{Source: provider.ID(), TrackID: test.trackID, TrackIDOrigin: TrackIDOriginSource, Title: "晴天", Artist: "周杰伦"})
			if err != nil {
				t.Fatalf("按平台歌曲 ID 获取歌词失败: %v", err)
			}
			if !second.Scrollable() || second.Metadata["track_id"] != test.trackID {
				t.Fatalf("精确 ID 歌词结果无效: id=%q mode=%s", test.trackID, second.Mode())
			}
			t.Logf("%s 歌曲 ID %s 返回 %d 行同步歌词", provider.ID(), test.trackID, len(second.Lines))
		})
	}
}

func TestSogouProviderLiveSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_SOGOU_LYRICS_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_SOGOU_LYRICS_LIVE=1 后执行搜狗播放器多源歌词在线测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	provider := SogouProvider{}
	document, err := provider.Fetch(ctx, NowPlaying{Title: "晴天", Artist: "周杰伦"})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Lines) < 10 {
		t.Fatalf("搜狗同步歌词行数异常: %d", len(document.Lines))
	}
	t.Logf("搜狗播放器链路由 %s 返回 %d 行同步歌词", provider.ResultSource(document), len(document.Lines))
}

func TestSogouLegacyEndpointLiveEvidence(t *testing.T) {
	if os.Getenv("CASESCREEN_SOGOU_LYRICS_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_SOGOU_LYRICS_LIVE=1 后记录搜狗旧接口现状")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	provider := SogouProvider{BaseURL: defaultSogouLegacyBaseURL, LegacyTimeout: 3 * time.Second}
	document, err := provider.Fetch(ctx, NowPlaying{Title: "晴天", Artist: "周杰伦"})
	if err != nil {
		t.Logf("搜狗旧歌词端点当前不可用（兼容源会接管）: %v", err)
		return
	}
	if len(document.Lines) < 3 {
		t.Fatalf("搜狗旧端点返回的同步歌词行数异常: %d", len(document.Lines))
	}
	t.Logf("搜狗旧歌词端点已恢复，返回 %d 行", len(document.Lines))
}
