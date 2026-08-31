//go:build windows && casescreen_legacy_netease_cdp

package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeNeteasePlaybackReader struct {
	payload json.RawMessage
	err     error
	wait    bool
	mu      sync.Mutex
	calls   int
	closed  bool
}

func (reader *fakeNeteasePlaybackReader) ReadPlayback(ctx context.Context) (json.RawMessage, error) {
	reader.mu.Lock()
	reader.calls++
	reader.mu.Unlock()
	if reader.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append(json.RawMessage(nil), reader.payload...), reader.err
}

func (reader *fakeNeteasePlaybackReader) Close() error {
	reader.mu.Lock()
	reader.closed = true
	reader.mu.Unlock()
	return nil
}

type recordingNowPlayingSource struct {
	track  NowPlaying
	err    error
	calls  int
	closed bool
}

func (source *recordingNowPlayingSource) Current(context.Context) (NowPlaying, error) {
	source.calls++
	return source.track, source.err
}

func (source *recordingNowPlayingSource) Close() error {
	source.closed = true
	return nil
}

func TestNeteaseCDPSourceReadsPrivateHostSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 4, 5, 123*int(time.Millisecond), time.Local)
	value := validNeteaseCDPSnapshotValue(now)
	// 验证解析层也兼容客户端可能直接返回的 {name} 歌手对象。
	value["info"].(map[string]any)["artists"] = []any{"阿肆", map[string]any{"name": "合作歌手"}}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeNeteasePlaybackReader{payload: payload}
	source := &neteaseCDPSource{reader: reader, clock: func() time.Time { return now }}
	track, err := source.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if track.Source != "netease" || track.SourceTrackID() != "526464145" ||
		track.Title != "喜欢" || track.Artist != "阿肆 / 合作歌手" || track.Album != "预谋邂逅" {
		t.Fatalf("网易云私有管道元数据解析错误: %+v", track)
	}
	if !track.Playing || !track.PositionKnown || time.Duration(track.Position) != 12_375*time.Millisecond ||
		time.Duration(track.Duration) != 250_334*time.Millisecond || !track.PositionUpdatedAt.Equal(now) ||
		track.TrackIDOrigin != TrackIDOriginSource {
		t.Fatalf("网易云私有管道时间轴或 ID 来源错误: %+v", track)
	}
}

func TestParseNeteaseCDPSnapshotRejectsInvalidTimelineNumbers(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "NaN 被 CDP JSON 转成 null", mutate: func(value map[string]any) {
			value["progress"].(map[string]any)["current"] = nil
		}},
		{name: "Infinity 字符串", mutate: func(value map[string]any) {
			value["progress"].(map[string]any)["current"] = "Infinity"
		}},
		{name: "负数位置", mutate: func(value map[string]any) {
			value["progress"].(map[string]any)["current"] = -0.01
		}},
		{name: "负数时长", mutate: func(value map[string]any) {
			value["info"].(map[string]any)["duration"] = -1
			value["progress"].(map[string]any)["duration"] = -1
		}},
		{name: "信息与进度时长不一致", mutate: func(value map[string]any) {
			value["progress"].(map[string]any)["duration"] = 250_000
		}},
		{name: "位置越过结尾", mutate: func(value map[string]any) {
			value["progress"].(map[string]any)["current"] = 300.0
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validNeteaseCDPSnapshotValue(now)
			test.mutate(value)
			payload, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = parseNeteaseCDPSnapshot(payload, now); err == nil {
				t.Fatal("无效时间轴没有失败关闭")
			}
		})
	}
	payload, err := json.Marshal(validNeteaseCDPSnapshotValue(now))
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(stringReplaceOnce(string(payload), `"current":12.375`, `"current":1e309`))
	if _, err = parseNeteaseCDPSnapshot(payload, now); err == nil {
		t.Fatal("溢出为 Infinity 的数字没有失败关闭")
	}
}

func TestParseNeteaseCDPSnapshotRejectsUnstableOrStaleIdentity(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "切歌混合快照", mutate: func(value map[string]any) { value["stable"] = false }},
		{name: "非歌曲资源", mutate: func(value map[string]any) {
			value["info"].(map[string]any)["resourceType"] = "voice"
		}},
		{name: "非数字 ID", mutate: func(value map[string]any) {
			value["info"].(map[string]any)["resourceId"] = "喜欢"
		}},
		{name: "陈旧时间戳", mutate: func(value map[string]any) {
			value["sampledAtMs"] = fmt.Sprint(now.Add(-time.Minute).UnixMilli())
		}},
		{name: "未知状态", mutate: func(value map[string]any) { value["status"] = "Buffering" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validNeteaseCDPSnapshotValue(now)
			test.mutate(value)
			payload, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = parseNeteaseCDPSnapshot(payload, now); err == nil {
				t.Fatal("不可信身份或状态没有失败关闭")
			}
		})
	}
}

func TestParseNeteaseCDPSnapshotMapsPauseWithoutLosingTimeline(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)
	value := validNeteaseCDPSnapshotValue(now)
	value["status"] = "Pause"
	value["progress"].(map[string]any)["current"] = 42.25
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	track, err := parseNeteaseCDPSnapshot(payload, now)
	if err != nil {
		t.Fatal(err)
	}
	if track.Playing || !track.PositionKnown || time.Duration(track.Position) != 42_250*time.Millisecond ||
		!track.PositionUpdatedAt.Equal(now) {
		t.Fatalf("暂停状态没有保留可信冻结锚点: %+v", track)
	}
}

func TestNeteaseCDPSourceHasBoundedTimeout(t *testing.T) {
	reader := &fakeNeteasePlaybackReader{wait: true}
	source := &neteaseCDPSource{reader: reader}
	started := time.Now()
	_, err := source.Current(context.Background())
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("私有管道超时没有保留 context deadline 错误: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("私有管道读取超时没有上界: %s", elapsed)
	}
}

func TestNeteasePrioritySourceSelection(t *testing.T) {
	neteasePlaying := NowPlaying{Source: "netease", TrackID: "526464145", TrackIDOrigin: TrackIDOriginSource, Title: "喜欢", Playing: true, PositionKnown: true}
	neteasePaused := neteasePlaying
	neteasePaused.Playing = false
	qqPlaying := NowPlaying{Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource, Title: "QQ 歌曲", Playing: true, PositionKnown: true}
	chromePaused := NowPlaying{Source: "Chrome", Title: "网页歌曲", Playing: false, PositionKnown: true}

	tests := []struct {
		name          string
		nativeTrack   NowPlaying
		nativeErr     error
		fallbackTrack NowPlaying
		wantSource    string
		wantTitle     string
		wantError     bool
		fallbackCalls int
	}{
		{name: "网易云正在播放时优先", nativeTrack: neteasePlaying, fallbackTrack: qqPlaying, wantSource: "netease", wantTitle: "喜欢", fallbackCalls: 0},
		{name: "网易云暂停时让位给其他真实播放", nativeTrack: neteasePaused, fallbackTrack: qqPlaying, wantSource: "qq", wantTitle: "QQ 歌曲", fallbackCalls: 1},
		{name: "没有其他播放时保留网易云暂停曲目", nativeTrack: neteasePaused, fallbackTrack: chromePaused, wantSource: "netease", wantTitle: "喜欢", fallbackCalls: 1},
		{name: "私有管道缺失不接受网易云窗口冒充", nativeErr: errors.New("pipe closed"), fallbackTrack: neteasePlaying, wantError: true, fallbackCalls: 1},
		{name: "私有管道缺失不影响其他真实播放", nativeErr: errors.New("pipe closed"), fallbackTrack: qqPlaying, wantSource: "qq", wantTitle: "QQ 歌曲", fallbackCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			native := &recordingNowPlayingSource{track: test.nativeTrack, err: test.nativeErr}
			fallback := &recordingNowPlayingSource{track: test.fallbackTrack}
			source := &neteasePrioritySource{native: native, fallback: fallback}
			track, err := source.Current(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("错误状态不符: err=%v track=%+v", err, track)
			}
			if track.Source != test.wantSource || track.Title != test.wantTitle {
				t.Fatalf("选择了错误播放源: %+v", track)
			}
			if fallback.calls != test.fallbackCalls {
				t.Fatalf("fallback 调用次数=%d，期望 %d", fallback.calls, test.fallbackCalls)
			}
		})
	}
}

func TestNewNowPlayingSourceUsesSuppliedSharedHostAndClosesOnlyFallback(t *testing.T) {
	reader := &fakeNeteasePlaybackReader{}
	source, ok := newLegacyNeteaseCDPNowPlayingSource(reader).(*neteasePrioritySource)
	if !ok {
		t.Fatalf("legacy 当前播放源没有安装网易云原生优先级: %T", newLegacyNeteaseCDPNowPlayingSource(reader))
	}
	native, ok := source.native.(*neteaseCDPSource)
	if !ok || native.reader != reader {
		t.Fatalf("优先源没有使用 main 提供的共享网易云主机: %T", source.native)
	}
	fallback := &recordingNowPlayingSource{}
	source.fallback = fallback
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if !fallback.closed || reader.closed {
		t.Fatalf("关闭边界错误: fallback=%v sharedHost=%v", fallback.closed, reader.closed)
	}
}

func TestParseNeteaseCDPNumberRejectsNonFiniteValues(t *testing.T) {
	for _, raw := range []string{"null", `"NaN"`, `"Infinity"`, "1e309", "-1e309"} {
		if value, err := parseNeteaseCDPNumber(json.RawMessage(raw), "测试字段"); err == nil {
			t.Fatalf("非有限数字没有被拒绝: raw=%s value=%v", raw, value)
		}
	}
}

func validNeteaseCDPSnapshotValue(now time.Time) map[string]any {
	return map[string]any{
		"stable":      true,
		"sampledAtMs": fmt.Sprint(now.UnixMilli()),
		"status":      "Playing",
		"info": map[string]any{
			"name":         "喜欢",
			"artists":      []any{"阿肆"},
			"album":        "预谋邂逅",
			"resourceType": "track",
			"resourceId":   "526464145",
			"duration":     250_334,
		},
		"progress": map[string]any{
			"current":  12.375,
			"duration": 250_334,
		},
	}
}

func stringReplaceOnce(value, old, replacement string) string {
	for index := 0; index+len(old) <= len(value); index++ {
		if value[index:index+len(old)] == old {
			return value[:index] + replacement + value[index+len(old):]
		}
	}
	return value
}
