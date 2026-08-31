//go:build windows

package lyrics

import (
	"context"
	"time"
)

type neteaseWebDBQuery func(string) (neteaseWebDBPlaybackEvidence, error)

var defaultQQSongMIDClient = &qqSongMIDClient{}
var defaultQQNativeIDSource = newQQProcessMemorySource()
var defaultNeteaseNativeIDSource = newNeteaseProcessMemorySource()

// enrichPlatformTrackID 只接受当前播放对象携带的同平台证据。
// QQ 由本进程只读核对 QQMusic 当前曲目内存；不接受外部工具写入的
// SMTC 标记。网易云同样由内置只读适配器取得原生数字 ID，
// 并要求 SMTC 元数据完整、播放状态/进度/时长与内存时间轴一致。
// 这里不会用窗口标题搜索或历史数据库补 ID。
func enrichPlatformTrackID(ctx context.Context, track NowPlaying) NowPlaying {
	switch normalizeText(track.Source) {
	case "qq":
		// QQ 和网易云都不信任上游或上一轮预填的身份；
		// 各自的内置只读探针必须在本轮重新取得并验证。
		return enrichQQPlatformTrackIDFromSources(ctx, track, defaultQQNativeIDSource, defaultQQSongMIDClient)
	case "netease":
		// 网易云不接受上游或上一轮预填的 ID；内置只读探针
		// 必须在本轮重新取得并验证原生身份。
		return enrichNeteasePlatformTrackIDFromSource(ctx, track, defaultNeteaseNativeIDSource)
	default:
		return track
	}
}

// enrichNeteasePlatformTrackID 把时钟和只读查询注入，单测不接触用户数据库。
func enrichNeteasePlatformTrackID(track NowPlaying, now time.Time, webDBPath string,
	query neteaseWebDBQuery) NowPlaying {
	if query != nil {
		if evidence, err := query(webDBPath); err == nil {
			if trackID := resolveNeteaseWebDBTrackID(evidence, track, now); trackID != "" {
				track.TrackID = trackID
				track.TrackIDOrigin = TrackIDOriginSource
				return track
			}
		}
	}
	return track
}
