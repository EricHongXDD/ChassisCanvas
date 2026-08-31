package lyrics

import (
	"strings"
	"testing"
	"time"
)

func TestResolveNeteaseWebDBTrackIDAcceptsFreshStrictEvidence(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	current := neteaseWebDBCurrentTrack()
	current.Position = int64(44 * time.Second)
	evidence := neteaseWebDBEvidence(now.Add(-2*time.Second), 42,
		`{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`)
	if got := resolveNeteaseWebDBTrackID(evidence, current, now); got != "186016" {
		t.Fatalf("新鲜严格证据没有返回原生 ID: %q", got)
	}
}

func TestResolveNeteaseWebDBTrackIDAcceptsMillisecondPositionAndSecondTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	current := neteaseWebDBCurrentTrack()
	current.Position = int64(43 * time.Second)
	evidence := neteaseWebDBEvidence(now.Add(-time.Second), 42_000,
		`{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`)
	evidence.UpdateTime = now.Add(-time.Second).Unix()
	if got := resolveNeteaseWebDBTrackID(evidence, current, now); got != "186016" {
		t.Fatalf("毫秒位置或秒级时间戳没有被严格识别: %q", got)
	}
}

func TestResolveNeteaseWebDBTrackIDRejectsStaleEvidence(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	evidence := neteaseWebDBEvidence(now.Add(-2*time.Minute), 42,
		`{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`)
	if got := resolveNeteaseWebDBTrackID(evidence, neteaseWebDBCurrentTrack(), now); got != "" {
		t.Fatalf("过期证据被升级为原生 ID: %q", got)
	}
}

func TestResolveNeteaseWebDBTrackIDRejectsWrongMetadata(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		json string
	}{
		{"错标题", `{"id":186016,"name":"另一首歌","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`},
		{"错歌手", `{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"其他歌手"}],"album":{"name":"目标专辑"}}`},
		{"错专辑", `{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"其他专辑"}}`},
		{"错时长", `{"id":186016,"name":"同名歌曲","duration":230000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`},
		{"JSON ID 冲突", `{"id":347230,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := neteaseWebDBEvidence(now.Add(-time.Second), 42, test.json)
			if got := resolveNeteaseWebDBTrackID(evidence, neteaseWebDBCurrentTrack(), now); got != "" {
				t.Fatalf("冲突元数据被升级为原生 ID: %q", got)
			}
		})
	}
}

func TestResolveNeteaseWebDBTrackIDRejectsDifferentPlatform(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	current := neteaseWebDBCurrentTrack()
	current.Source = "qq"
	evidence := neteaseWebDBEvidence(now.Add(-time.Second), 42,
		`{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`)
	if got := resolveNeteaseWebDBTrackID(evidence, current, now); got != "" {
		t.Fatalf("网易云数据库 ID 被错误用于 QQ 来源: %q", got)
	}
}

func TestResolveNeteaseWebDBTrackIDRejectsUnrelatedPosition(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	current := neteaseWebDBCurrentTrack()
	current.Position = int64(90 * time.Second)
	evidence := neteaseWebDBEvidence(now.Add(-time.Second), 12,
		`{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`)
	if got := resolveNeteaseWebDBTrackID(evidence, current, now); got != "" {
		t.Fatalf("无关播放位置被升级为原生 ID: %q", got)
	}
}

func TestResolveNeteaseWebDBTrackIDRejectsDamagedJSON(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	tests := [][]byte{
		[]byte(`{"id":186016`),
		append([]byte(`{"id":186016}`), []byte(` {"second":true}`)...),
		[]byte(strings.Repeat("x", maximumNeteaseDBTrackJSONBytes+1)),
	}
	for _, data := range tests {
		evidence := neteaseWebDBEvidence(now.Add(-time.Second), 42, "{}")
		evidence.TrackJSON = data
		if got := resolveNeteaseWebDBTrackID(evidence, neteaseWebDBCurrentTrack(), now); got != "" {
			t.Fatalf("损坏或超限 JSON 被升级为原生 ID: %q", got)
		}
	}
}

func TestResolveNeteaseWebDBTrackIDMatchesAllArtists(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	evidence := neteaseWebDBEvidence(now.Add(-time.Second), 42,
		`{"id":"186016","name":"合唱歌曲","dt":215000,"ar":[{"name":"歌手甲"},{"name":"歌手乙"}],"al":{"name":"合唱专辑"}}`)
	current := neteaseWebDBCurrentTrack()
	current.Title, current.Artist, current.Album = "合唱歌曲", "歌手甲 / 歌手乙", "合唱专辑"
	if got := resolveNeteaseWebDBTrackID(evidence, current, now); got != "186016" {
		t.Fatalf("多歌手严格匹配失败: %q", got)
	}
	current.Artist = "歌手甲"
	if got := resolveNeteaseWebDBTrackID(evidence, current, now); got != "" {
		t.Fatalf("缺少一个歌手仍被升级为原生 ID: %q", got)
	}
}

func neteaseWebDBCurrentTrack() NowPlaying {
	return NowPlaying{
		Source: "netease", Title: "同名歌曲", Artist: "目标歌手", Album: "目标专辑",
		Position: int64(43 * time.Second), Duration: int64(215 * time.Second),
		PositionKnown: true, Playing: true,
	}
}

func neteaseWebDBEvidence(updatedAt time.Time, playDuration int64, metadata string) neteaseWebDBPlaybackEvidence {
	return neteaseWebDBPlaybackEvidence{
		ResourceID: "186016", ResourceType: "track", PlayDuration: playDuration,
		UpdateTime: updatedAt.UnixMilli(), TrackJSON: []byte(metadata),
	}
}
