//go:build windows && casescreen_legacy_netease_cdp

package lyrics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"casescreen/internal/playerhost"
)

const (
	defaultPlaybackStepTimeout = 45 * time.Second
	livePlaybackPollInterval   = 500 * time.Millisecond
)

// TestQQProcessMemoryNativeIDLive 只验证 CaseScreen 内置的 QQ 只读进程
// 适配器，不依赖 SMTC Genres 标记、DLL 注入、外部桥或联网歌曲搜索。
// 运行前应让受支持版本的 QQ 音乐正在播放，并让它成为系统当前媒体会话。
func TestQQProcessMemoryNativeIDLive(t *testing.T) {
	if os.Getenv("CASESCREEN_QQ_MEMORY_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_QQ_MEMORY_LIVE=1 后验证真实 QQMusic 客户端")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	track, err := (&smtcSource{}).Current(ctx)
	if err != nil {
		t.Fatalf("读取 QQ 的 Windows 当前媒体会话失败: %v", err)
	}
	if sourceProviderID(track.Source) != "qq" {
		t.Fatalf("系统当前媒体会话不是 QQ 音乐: source=%q title=%q", track.Source, track.Title)
	}
	source := newQQProcessMemorySource()
	defer source.closeAll()
	songID, ok := source.CurrentSongID(ctx, track)
	if !ok || songID == 0 {
		t.Fatalf("内置只读 QQ 适配器没有取得与 SMTC 完全一致的 songId: title=%q artist=%q album=%q position_known=%v playing=%v",
			track.Title, track.Artist, track.Album, track.PositionKnown, track.Playing)
	}
	t.Logf("QQ 只读进程适配器真机通过: songId=%d title=%q artist=%q album=%q", songID, track.Title, track.Artist, track.Album)
}

// TestWindowsNowPlayingExactIDLive 只在人工真机验收时运行。
// 它确认播放器来源、平台歌曲 ID 和时间轴来自 Windows 当前媒体会话，
// 不使用麦克风、录音或声纹识别。
func TestWindowsNowPlayingExactIDLive(t *testing.T) {
	if os.Getenv("CASESCREEN_PLAYBACK_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_PLAYBACK_LIVE=1 后验证当前 QQ/网易云播放")
	}
	expectedSource := requiredExpectedPlaybackSource(t)
	expectedState := optionalExpectedPlaybackState(t)
	expectedTrackID := strings.TrimSpace(os.Getenv("CASESCREEN_PLAYBACK_EXPECT_TRACK_ID"))

	source := newStrictLiveNowPlayingSource(t, expectedSource)
	t.Logf("请让 %s 进入待验收的播放状态", expectedSource)
	observed := waitForLiveTrack(t, source, expectedSource, playbackStepTimeout(t), func(track NowPlaying, _ time.Time) bool {
		return expectedState == nil || track.Playing == *expectedState
	})
	track := observed.track
	if expectedTrackID != "" && track.SourceTrackID() != expectedTrackID {
		t.Fatalf("当前歌曲 ID=%q，期望 CASESCREEN_PLAYBACK_EXPECT_TRACK_ID=%q",
			track.SourceTrackID(), expectedTrackID)
	}
	t.Logf("严格当前播放验收通过: %s", describeLiveTrack(track, time.Now()))
}

// TestWindowsNowPlayingTransitionLive 由人工操作播放器，测试只观察媒体会话。
// 运行后按日志提示依次切歌、暂停、恢复和拖动至少 10 秒；
// 任一阶段未在超时内命中都会失败，不会以其他播放器或 Skip 代替。
func TestWindowsNowPlayingTransitionLive(t *testing.T) {
	if os.Getenv("CASESCREEN_PLAYBACK_TRANSITIONS_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_PLAYBACK_TRANSITIONS_LIVE=1 后执行人工切歌/暂停/恢复/拖动验收")
	}
	expectedSource := requiredExpectedPlaybackSource(t)
	stepTimeout := playbackStepTimeout(t)
	source := newStrictLiveNowPlayingSource(t, expectedSource)

	t.Logf("准备：请让 %s 播放一首有可验证原生 ID 的歌曲", expectedSource)
	baseline := waitForLiveTrack(t, source, expectedSource, stepTimeout, func(track NowPlaying, _ time.Time) bool {
		return track.Playing
	})
	baselineID := baseline.track.SourceTrackID()
	t.Logf("基线已记录: %s", describeLiveTrack(baseline.track, baseline.observedAt))

	t.Log("步骤 1/4：请切换到另一首歌曲，并保持播放")
	switched := waitForLiveTrack(t, source, expectedSource, stepTimeout, func(track NowPlaying, _ time.Time) bool {
		return track.Playing && track.SourceTrackID() != baselineID
	})
	switchedID := switched.track.SourceTrackID()
	t.Logf("切歌已记录: before=%q after=%q; %s", baselineID, switchedID,
		describeLiveTrack(switched.track, switched.observedAt))

	t.Log("步骤 2/4：请暂停当前歌曲，并保持暂停至少 2 秒")
	paused := waitForLiveTrack(t, source, expectedSource, stepTimeout, func(track NowPlaying, _ time.Time) bool {
		return !track.Playing && track.SourceTrackID() == switchedID
	})
	verifyLivePauseRemainsStable(t, source, expectedSource, paused, 2*time.Second)
	t.Logf("暂停已记录且位置稳定: %s", describeLiveTrack(paused.track, paused.observedAt))

	t.Log("步骤 3/4：请恢复播放")
	resumed := waitForLiveTrack(t, source, expectedSource, stepTimeout, func(track NowPlaying, _ time.Time) bool {
		return track.Playing && track.SourceTrackID() == switchedID
	})
	advanced := verifyLivePlaybackAdvances(t, source, expectedSource, resumed, 2*time.Second)
	t.Logf("恢复已记录且位置继续推进: %s", describeLiveTrack(advanced.track, advanced.observedAt))

	t.Log("步骤 4/4：请在当前歌曲中向前或向后拖动至少 10 秒")
	seekBaseline := advanced
	seeked := waitForLiveTrack(t, source, expectedSource, stepTimeout, func(track NowPlaying, observedAt time.Time) bool {
		if !track.Playing || track.SourceTrackID() != switchedID {
			return false
		}
		// 拖动要与“按墙钟自然继续播放”的期望位置比较；
		// 不能把操作者等待的几秒本身误判为向前拖动。
		expectedPosition := projectedLivePosition(seekBaseline.track, seekBaseline.observedAt) +
			observedAt.Sub(seekBaseline.observedAt)
		actualPosition := projectedLivePosition(track, observedAt)
		return absoluteDuration(actualPosition-expectedPosition) >= 5*time.Second
	})
	t.Logf("拖动已记录: before=%s after=%s; %s",
		projectedLivePosition(seekBaseline.track, seekBaseline.observedAt),
		projectedLivePosition(seeked.track, seeked.observedAt),
		describeLiveTrack(seeked.track, seeked.observedAt))
}

// TestNeteasePrivatePipeDisconnectKeepsClientTimelineLive 是 CEF 91 断管发布
// 门禁。它只允许在外部一次性 Windows 环境中启动客户端，再交叉核对
// 私有管道与独立 SMTC 的
// 同一首播放中歌曲，再关闭 CaseScreen 管道；之后必须同时观察到断管前的
// cloudmusic.exe PID 仍存活，以及 SMTC 同曲位置继续推进。
func TestNeteasePrivatePipeDisconnectKeepsClientTimelineLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_DISCONNECT_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_DISCONNECT_LIVE=1 后运行网易云断管真机门禁")
	}
	beforeLaunch, err := playerhost.NeteaseProcessIDsForVerification()
	if err != nil {
		t.Fatal(err)
	}
	if err := playerhost.RequireDisposableWindowsForNeteaseVerification(); err != nil {
		t.Fatal(err)
	}
	if len(beforeLaunch) != 0 {
		t.Fatal("断管门禁前必须完全退出网易云音乐；测试不会自动关闭或重启客户端")
	}

	// Windows Known Folder 不受 APPDATA 等环境变量可靠隔离；这里只在外部
	// 已经可销毁的 Windows 环境中使用与生产相同的 host。
	host := playerhost.NewNeteaseHost()
	source := newLegacyNeteaseCDPNowPlayingSource(host)
	var trackedProcessIDs []uint32
	t.Cleanup(func() {
		if closer, ok := source.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		_ = host.Close()
	})

	connectContext, connectCancel := context.WithTimeout(context.Background(), 9*time.Second)
	status := host.StartAndConnect(connectContext)
	connectCancel()
	if !status.Connected || status.Channel != playerhost.NeteasePrivatePipeChannel {
		t.Fatalf("CEF91 私有管道未连接: %+v", status)
	}
	t.Log("一次性 Windows 环境中的网易云已启动；请在 60 秒内播放一首歌曲并保持播放")
	native := waitForLiveTrack(t, source, "netease", time.Minute, func(track NowPlaying, _ time.Time) bool {
		return track.Playing
	})
	smtcBefore := waitForNeteaseSMTCCrossEvidence(t, native.track, 30*time.Second)

	trackedProcessIDs, err = playerhost.NeteaseProcessIDsForVerification()
	if err != nil || len(trackedProcessIDs) == 0 {
		t.Fatalf("无法记录断管前的 cloudmusic.exe PID: pids=%v err=%v", trackedProcessIDs, err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	remainingProcessIDs, err := playerhost.NeteaseProcessIDsForVerification()
	if err != nil {
		t.Fatal(err)
	}
	if !liveNeteasePIDSetIntersects(trackedProcessIDs, remainingProcessIDs) {
		t.Fatalf("关闭私有管道后断管前的 cloudmusic.exe PID 全部消失: before=%v after=%v", trackedProcessIDs, remainingProcessIDs)
	}

	smtcAfterClose := waitForNeteaseSMTCCrossEvidence(t, native.track, 15*time.Second)
	time.Sleep(2 * time.Second)
	smtcAdvanced := waitForNeteaseSMTCCrossEvidence(t, native.track, 10*time.Second)
	beforePosition := projectedLivePosition(smtcAfterClose.track, smtcAfterClose.observedAt)
	afterPosition := projectedLivePosition(smtcAdvanced.track, smtcAdvanced.observedAt)
	if afterPosition-beforePosition < time.Second {
		t.Fatalf("断管后独立 SMTC 同曲位置没有继续推进: before=%s after=%s", beforePosition, afterPosition)
	}
	if firstPosition := projectedLivePosition(smtcBefore.track, smtcBefore.observedAt); afterPosition <= firstPosition {
		t.Fatalf("断管前后独立 SMTC 时间轴没有保持连续: before=%s after=%s", firstPosition, afterPosition)
	}
	t.Logf("断管门禁通过: pid_before=%v pid_after=%v; smtc_before=%s smtc_after=%s",
		trackedProcessIDs, remainingProcessIDs, beforePosition, afterPosition)

	t.Log("请现在从网易云界面正常完全退出隔离客户端；测试不会结束进程")
	exited, waitErr := waitForAllLiveNeteaseProcessesToExit(90 * time.Second)
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if !exited {
		t.Fatal("90 秒内未观察到一次性 Windows 环境中的客户端正常退出")
	}
}

func waitForNeteaseSMTCCrossEvidence(t *testing.T, native NowPlaying, timeout time.Duration) observedLiveTrack {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastTrack NowPlaying
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		track, err := (&smtcSource{}).Current(ctx)
		cancel()
		observedAt := time.Now()
		if err == nil {
			err = validateNeteaseSMTCCrossEvidence(native, track)
		}
		lastTrack, lastErr = track, err
		if err == nil {
			return observedLiveTrack{track: track, observedAt: observedAt}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("未在 %s 内取得与私有管道同曲的独立 SMTC 播放时间轴: last_error=%v last=%s",
				timeout, lastErr, describeLiveTrack(lastTrack, observedAt))
		}
		time.Sleep(livePlaybackPollInterval)
	}
}

func validateNeteaseSMTCCrossEvidence(native, smtc NowPlaying) error {
	if sourceProviderID(smtc.Source) != "netease" || !smtc.Valid() {
		return errorsForLiveGate("独立 SMTC 当前来源不是网易云: source=%q title=%q", smtc.Source, smtc.Title)
	}
	if !smtc.Playing || !smtc.PositionKnown {
		return errorsForLiveGate("独立 SMTC 没有播放中的可用时间轴: playing=%v position_known=%v", smtc.Playing, smtc.PositionKnown)
	}
	if normalizeText(native.Title) == "" || normalizeText(native.Title) != normalizeText(smtc.Title) ||
		normalizeText(native.Artist) == "" || normalizeText(native.Artist) != normalizeText(smtc.Artist) {
		return errorsForLiveGate("私有管道与独立 SMTC 不是同一首歌: native=%q/%q smtc=%q/%q",
			native.Title, native.Artist, smtc.Title, smtc.Artist)
	}
	if native.Album != "" && smtc.Album != "" && normalizeText(native.Album) != normalizeText(smtc.Album) {
		return errorsForLiveGate("私有管道与独立 SMTC 专辑不一致: native=%q smtc=%q", native.Album, smtc.Album)
	}
	if native.Duration <= 0 || smtc.Duration <= 0 || absoluteDuration(time.Duration(native.Duration-smtc.Duration)) > 2*time.Second {
		return errorsForLiveGate("私有管道与独立 SMTC 时长不一致: native=%s smtc=%s",
			time.Duration(native.Duration), time.Duration(smtc.Duration))
	}
	return nil
}

func liveNeteasePIDSetIntersects(left, right []uint32) bool {
	seen := make(map[uint32]struct{}, len(left))
	for _, processID := range left {
		if processID != 0 {
			seen[processID] = struct{}{}
		}
	}
	for _, processID := range right {
		if _, ok := seen[processID]; ok {
			return true
		}
	}
	return false
}

func waitForAllLiveNeteaseProcessesToExit(timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		currentProcessIDs, err := playerhost.NeteaseProcessIDsForVerification()
		if err != nil {
			return false, err
		}
		if len(currentProcessIDs) == 0 {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(livePlaybackPollInterval)
	}
}

// newStrictLiveNowPlayingSource 让网易云 live 门禁与生产程序使用完全相同的
// “同一 host 启动并持有私有管道、同一 host 交给歌词源”边界。
func newStrictLiveNowPlayingSource(t *testing.T, expectedSource string) NowPlayingSource {
	t.Helper()
	if expectedSource != "netease" {
		source := NewNowPlayingSource()
		if closer, ok := source.(interface{ Close() error }); ok {
			t.Cleanup(func() { _ = closer.Close() })
		}
		return source
	}
	if err := playerhost.RequireDisposableWindowsForNeteaseVerification(); err != nil {
		t.Fatal(err)
	}
	host := playerhost.NewNeteaseHost()
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	status := host.StartAndConnect(ctx)
	cancel()
	if !status.Connected || status.Channel != playerhost.NeteasePrivatePipeChannel {
		_ = host.Close()
		t.Fatalf("网易云私有管道未连接；运行 live 门禁前必须完全退出主窗口和托盘进程: %+v", status)
	}
	source := newLegacyNeteaseCDPNowPlayingSource(host)
	t.Cleanup(func() {
		if closer, ok := source.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		_ = host.Close()
	})
	return source
}

type observedLiveTrack struct {
	track      NowPlaying
	observedAt time.Time
}

func requiredExpectedPlaybackSource(t *testing.T) string {
	t.Helper()
	value, err := parseExpectedPlaybackSource(os.Getenv("CASESCREEN_PLAYBACK_EXPECT_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func parseExpectedPlaybackSource(value string) (string, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "qq", "netease":
		return normalized, nil
	case "":
		return "", errorsForLiveGate("CASESCREEN_PLAYBACK_EXPECT_SOURCE 必须显式设为 qq 或 netease")
	default:
		return "", errorsForLiveGate("CASESCREEN_PLAYBACK_EXPECT_SOURCE=%q 无效，只允许 qq 或 netease", value)
	}
}

func optionalExpectedPlaybackState(t *testing.T) *bool {
	t.Helper()
	value, err := parseExpectedPlaybackState(os.Getenv("CASESCREEN_PLAYBACK_EXPECT_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func parseExpectedPlaybackState(value string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return nil, nil
	case "playing":
		playing := true
		return &playing, nil
	case "paused":
		playing := false
		return &playing, nil
	default:
		return nil, errorsForLiveGate("CASESCREEN_PLAYBACK_EXPECT_STATE=%q 无效，只允许 playing 或 paused", value)
	}
}

func errorsForLiveGate(format string, arguments ...any) error {
	return fmt.Errorf(format, arguments...)
}

func validateStrictLiveTrack(track NowPlaying, expectedSource string, expectedPlaying *bool) error {
	if !track.Valid() {
		return errorsForLiveGate("当前没有可识别的播放歌曲")
	}
	if actualSource := sourceProviderID(track.Source); actualSource != expectedSource {
		return errorsForLiveGate("当前来源=%q（识别为 %q），期望 %q", track.Source, actualSource, expectedSource)
	}
	if track.TrackIDOrigin != TrackIDOriginSource || strings.TrimSpace(track.SourceTrackID()) == "" {
		return errorsForLiveGate("%s 当前歌曲没有 source-origin 原生 ID: title=%q artist=%q origin=%q",
			expectedSource, track.Title, track.Artist, track.TrackIDOrigin)
	}
	if !track.PositionKnown {
		return errorsForLiveGate("%s 当前歌曲没有可验证播放位置: id=%q title=%q",
			expectedSource, track.SourceTrackID(), track.Title)
	}
	if expectedPlaying != nil && track.Playing != *expectedPlaying {
		expected := "paused"
		if *expectedPlaying {
			expected = "playing"
		}
		return errorsForLiveGate("当前播放状态 playing=%v，期望 %s", track.Playing, expected)
	}
	return nil
}

func playbackStepTimeout(t *testing.T) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("CASESCREEN_PLAYBACK_STEP_TIMEOUT"))
	if raw == "" {
		return defaultPlaybackStepTimeout
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration < 5*time.Second || duration > 5*time.Minute {
		t.Fatalf("CASESCREEN_PLAYBACK_STEP_TIMEOUT=%q 无效，必须为 5s..5m", raw)
	}
	return duration
}

func waitForLiveTrack(t *testing.T, source NowPlayingSource, expectedSource string, timeout time.Duration,
	accept func(NowPlaying, time.Time) bool) observedLiveTrack {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastTrack NowPlaying
	var lastObservedAt time.Time
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		track, err := source.Current(ctx)
		cancel()
		observedAt := time.Now()
		if err == nil {
			err = validateStrictLiveTrack(track, expectedSource, nil)
		}
		lastTrack, lastObservedAt, lastErr = track, observedAt, err
		if err == nil && accept(track, observedAt) {
			return observedLiveTrack{track: track, observedAt: observedAt}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("人工播放步骤在 %s 内未命中: last_error=%v last=%s",
				timeout, lastErr, describeLiveTrack(lastTrack, lastObservedAt))
		}
		time.Sleep(livePlaybackPollInterval)
	}
}

func verifyLivePauseRemainsStable(t *testing.T, source NowPlayingSource, expectedSource string,
	paused observedLiveTrack, hold time.Duration) {
	t.Helper()
	deadline := time.Now().Add(hold)
	anchorPosition := projectedLivePosition(paused.track, paused.observedAt)
	for time.Now().Before(deadline) {
		time.Sleep(livePlaybackPollInterval)
		current := readStrictLiveTrack(t, source, expectedSource)
		if current.track.SourceTrackID() != paused.track.SourceTrackID() || current.track.Playing {
			t.Fatalf("暂停稳定窗口内歌曲或状态发生变化: %s", describeLiveTrack(current.track, current.observedAt))
		}
		position := projectedLivePosition(current.track, current.observedAt)
		if absoluteDuration(position-anchorPosition) > time.Second {
			t.Fatalf("暂停后位置仍明显推进: before=%s after=%s", anchorPosition, position)
		}
	}
}

func verifyLivePlaybackAdvances(t *testing.T, source NowPlayingSource, expectedSource string,
	resumed observedLiveTrack, hold time.Duration) observedLiveTrack {
	t.Helper()
	startPosition := projectedLivePosition(resumed.track, resumed.observedAt)
	time.Sleep(hold)
	current := readStrictLiveTrack(t, source, expectedSource)
	if current.track.SourceTrackID() != resumed.track.SourceTrackID() || !current.track.Playing {
		t.Fatalf("恢复验收窗口内歌曲或状态发生变化: %s", describeLiveTrack(current.track, current.observedAt))
	}
	position := projectedLivePosition(current.track, current.observedAt)
	if position-startPosition < time.Second {
		t.Fatalf("恢复后播放位置没有推进: before=%s after=%s", startPosition, position)
	}
	return current
}

func readStrictLiveTrack(t *testing.T, source NowPlayingSource, expectedSource string) observedLiveTrack {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	track, err := source.Current(ctx)
	observedAt := time.Now()
	if err != nil {
		t.Fatalf("读取 Windows 当前播放失败: %v", err)
	}
	if err := validateStrictLiveTrack(track, expectedSource, nil); err != nil {
		t.Fatalf("人工播放步骤中的媒体证据失效: %v", err)
	}
	return observedLiveTrack{track: track, observedAt: observedAt}
}

func projectedLivePosition(track NowPlaying, observedAt time.Time) time.Duration {
	position := time.Duration(track.Position)
	if !track.Playing || track.PositionUpdatedAt.IsZero() {
		return position
	}
	elapsed := observedAt.Sub(track.PositionUpdatedAt)
	if elapsed > 0 && elapsed < 24*time.Hour {
		position += elapsed
	}
	return position
}

func absoluteDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func describeLiveTrack(track NowPlaying, observedAt time.Time) string {
	return fmt.Sprintf("source=%q id=%q origin=%q title=%q artist=%q position=%s position_known=%v playing=%v observed_at=%s",
		track.Source, track.SourceTrackID(), track.TrackIDOrigin, track.Title, track.Artist,
		projectedLivePosition(track, observedAt), track.PositionKnown, track.Playing,
		observedAt.Format(time.RFC3339Nano))
}

func TestStrictLivePlaybackGate(t *testing.T) {
	if _, err := parseExpectedPlaybackSource(""); err == nil {
		t.Fatal("未指定目标平台时不应启动 live 验收")
	}
	if _, err := parseExpectedPlaybackSource("chrome"); err == nil {
		t.Fatal("非 QQ/网易云来源不应通过 live 门禁")
	}
	if source, err := parseExpectedPlaybackSource(" NetEase "); err != nil || source != "netease" {
		t.Fatalf("网易云目标平台解析失败: source=%q err=%v", source, err)
	}
	if _, err := parseExpectedPlaybackState("buffering"); err == nil {
		t.Fatal("未支持的播放状态不应通过 live 门禁")
	}

	playing := true
	valid := NowPlaying{
		Source: "qq", TrackID: "0039MnYb0qxYhV", TrackIDOrigin: TrackIDOriginSource,
		Title: "晴天", Position: int64(42 * time.Second), PositionKnown: true, Playing: true,
	}
	if err := validateStrictLiveTrack(valid, "qq", &playing); err != nil {
		t.Fatalf("严格 live 证据被误拒绝: %v", err)
	}
	invalidTracks := []NowPlaying{
		{Source: "Chrome", TrackID: valid.TrackID, TrackIDOrigin: TrackIDOriginSource, Title: valid.Title, PositionKnown: true, Playing: true},
		{Source: "qq", TrackID: valid.TrackID, TrackIDOrigin: TrackIDOriginInferred, Title: valid.Title, PositionKnown: true, Playing: true},
		{Source: "qq", TrackID: valid.TrackID, TrackIDOrigin: TrackIDOriginSource, Title: valid.Title, PositionKnown: false, Playing: true},
	}
	for index, track := range invalidTracks {
		if err := validateStrictLiveTrack(track, "qq", &playing); err == nil {
			t.Fatalf("弱证据样例 %d 不应通过 live 门禁: %+v", index, track)
		}
	}
	paused := false
	if err := validateStrictLiveTrack(valid, "qq", &paused); err == nil {
		t.Fatal("播放状态与明确期望不符时不应通过")
	}
}
