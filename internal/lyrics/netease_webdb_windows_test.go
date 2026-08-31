//go:build windows

package lyrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnrichNeteasePlatformTrackIDWithoutDatabaseFailsClosed(t *testing.T) {
	track := neteaseWebDBCurrentTrack()
	result := enrichNeteasePlatformTrackID(track, time.Now(), "missing-webdb.dat",
		func(string) (neteaseWebDBPlaybackEvidence, error) {
			return neteaseWebDBPlaybackEvidence{}, os.ErrNotExist
		})
	if result.TrackID != "" || result.TrackIDOrigin != "" {
		t.Fatalf("无数据库时错误生成了原生 ID: %+v", result)
	}
}

func TestEnrichNeteasePlatformTrackIDMarksStrictWebDBEvidenceAsSource(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	track := neteaseWebDBCurrentTrack()
	evidence := neteaseWebDBEvidence(now.Add(-time.Second), 42,
		`{"id":186016,"name":"同名歌曲","duration":215000,"artists":[{"name":"目标歌手"}],"album":{"name":"目标专辑"}}`)
	result := enrichNeteasePlatformTrackID(track, now, "webdb.dat",
		func(string) (neteaseWebDBPlaybackEvidence, error) { return evidence, nil })
	if result.TrackID != "186016" || result.TrackIDOrigin != TrackIDOriginSource || result.SourceTrackID() != "186016" {
		t.Fatalf("严格数据库证据没有标记为来源原生 ID: %+v", result)
	}
}

func TestEnrichPlatformTrackIDDoesNotGuessQQSongMID(t *testing.T) {
	track := NowPlaying{
		Source: "qq", Title: "同名歌曲", Artist: "目标歌手", Album: "目标专辑",
		PositionKnown: true, Position: int64(30 * time.Second), Duration: int64(215 * time.Second), Playing: true,
	}
	result := enrichPlatformTrackID(context.Background(), track)
	if result.TrackID != "" || result.TrackIDOrigin != "" || result.SourceTrackID() != "" {
		t.Fatalf("没有可验证入口时猜测了 QQ songmid: %+v", result)
	}
}

func TestEnrichPlatformTrackIDDoesNotUseNeteaseHistoryDatabase(t *testing.T) {
	track := neteaseWebDBCurrentTrack()
	result := enrichPlatformTrackID(context.Background(), track)
	if result.TrackID != "" || result.TrackIDOrigin != "" || result.SourceTrackID() != "" {
		t.Fatalf("正式网易云 fallback 从历史数据库补出了原生 ID: %+v", result)
	}
}

func TestReadLatestNeteaseWebDBPlaybackMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-webdb.dat")
	if _, err := readLatestNeteaseWebDBPlayback(path); err == nil {
		t.Fatal("只读打开不存在的数据库没有失败")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("只读查询意外创建了数据库: %v", err)
	}
}

func TestReadLatestNeteaseWebDBPlaybackLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_WEBDB_QUERY_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_WEBDB_QUERY_LIVE=1 后只读验证本机数据库结构")
	}
	localData, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("无法定位本机网易云数据库")
	}
	evidence, err := readLatestNeteaseWebDBPlayback(filepath.Join(localData, "NetEase", "CloudMusic", "Library", "webdb.dat"))
	if err != nil {
		t.Fatalf("本机网易云数据库只读查询失败: %v", err)
	}
	metadata, metadataValid := parseNeteaseWebDBTrack(evidence.TrackJSON)
	if !metadataValid || metadata.ID != evidence.ResourceID {
		t.Fatal("本机 dbTrack 与 playingCount 的歌曲身份结构不一致")
	}
	if _, valid := parseNeteaseWebDBUpdateTime(evidence.UpdateTime); !valid || evidence.PlayDuration < 0 {
		t.Fatal("本机 playingCount 时间证据格式无效")
	}
	t.Logf("只读结构确认: resource_type=%q json_bytes=%d metadata_shape_valid=%v", evidence.ResourceType, len(evidence.TrackJSON), metadataValid)
}

// TestNeteaseWebDBPlaybackClockLive 记录真实客户端播放时本地数据库证据的
// 更新时间与播放时长。它只读查询，不控制播放器；人工验收可分别在播放和
// 暂停状态运行，用于判断数据库能否在缺少 GSMTC 会话时提供可靠时间锚点。
func TestNeteaseWebDBPlaybackClockLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_WEBDB_CLOCK_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_WEBDB_CLOCK_LIVE=1 后观察本机播放时钟")
	}
	localData, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("无法定位本机网易云数据库")
	}
	path := filepath.Join(localData, "NetEase", "CloudMusic", "Library", "webdb.dat")
	var previous neteaseWebDBPlaybackEvidence
	for index := 0; index < 6; index++ {
		evidence, queryErr := readLatestNeteaseWebDBPlayback(path)
		if queryErr != nil {
			t.Fatalf("第 %d 次只读查询失败: %v", index+1, queryErr)
		}
		metadata, valid := parseNeteaseWebDBTrack(evidence.TrackJSON)
		if !valid || metadata.ID != evidence.ResourceID {
			t.Fatalf("第 %d 次歌曲元数据无效", index+1)
		}
		changed := index > 0 && (evidence.UpdateTime != previous.UpdateTime ||
			evidence.PlayDuration != previous.PlayDuration || evidence.ResourceID != previous.ResourceID)
		t.Logf("sample=%d id=%s title=%q update=%d play_duration=%d changed=%v",
			index+1, evidence.ResourceID, metadata.Title, evidence.UpdateTime, evidence.PlayDuration, changed)
		previous = evidence
		if index < 5 {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// TestNeteaseWebDBNativeIDLive 保留旧环境变量作为迁移门禁：即使本机存在
// webdb，正式 fallback 也不得再把它标记为当前来源 ID。网易云真机时间轴
// 由 TestWindowsNowPlayingExactIDLive / TransitionLive 经 CDP 验收。
func TestNeteaseWebDBNativeIDLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_WEBDB_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_WEBDB_LIVE=1 后验证正在播放的网易云客户端")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	track, err := (&smtcSource{}).Current(ctx)
	if err != nil || normalizeText(track.Source) != "netease" || !track.Playing {
		t.Skip("网易云客户端未在播放，无法现场确认原生当前歌曲 ID")
	}
	result := enrichPlatformTrackID(ctx, track)
	if result.TrackID != "" || result.TrackIDOrigin != "" || result.SourceTrackID() != "" {
		t.Fatalf("正式 fallback 仍从历史数据库补出来源 ID: title=%q artist=%q id=%q",
			track.Title, track.Artist, result.SourceTrackID())
	}
}
