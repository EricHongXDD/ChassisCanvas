//go:build windows

package lyrics

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	asyncStarted   = 0
	asyncCompleted = 1
	asyncCanceled  = 2
	asyncError     = 3

	mediaPlaybackPlaying = 4
)

var (
	combase                    = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize           = combase.NewProc("RoInitialize")
	procRoUninitialize         = combase.NewProc("RoUninitialize")
	procRoGetActivationFactory = combase.NewProc("RoGetActivationFactory")
	procWindowsCreateString    = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString    = combase.NewProc("WindowsDeleteString")
	procWindowsStringRawBuffer = combase.NewProc("WindowsGetStringRawBuffer")

	iidAsyncInfo             = mustGUID("{00000036-0000-0000-C000-000000000046}")
	iidSessionManagerStatics = mustGUID("{2050C4EE-11A0-57DE-AED7-C97C70338245}")
)

type smtcSource struct {
	// preferredSource 只供平台内置适配器和真机门禁从
	// SMTC 列表中精确取得同平台会话。空值保持系统
	// 当前/正在播放会话的原有选择规则。
	preferredSource string
}

const platformIDProbeInterval = 5 * time.Second

type fallbackNowPlayingSource struct {
	smtc   *smtcSource
	window *windowSource

	mu              sync.Mutex
	lastProbeTrack  NowPlaying
	lastProbeAt     time.Time
	lastProbeFailed bool
	clock           func() time.Time
	enrich          func(context.Context, NowPlaying) NowPlaying
}

func (source *fallbackNowPlayingSource) Current(ctx context.Context) (NowPlaying, error) {
	track, err := source.smtc.Current(ctx)
	if err == nil && track.Valid() {
		return source.withPlatformTrackID(ctx, track), nil
	}
	fallback, fallbackErr := source.window.Current(ctx)
	if fallbackErr != nil {
		return NowPlaying{}, fallbackErr
	}
	if fallback.Valid() || err == nil {
		// 窗口标题只用于识别并冻结当前显示；它没有可信时间轴，
		// 不允许继承或查询原生歌曲 ID，避免同名版本误用旧身份。
		return fallback, nil
	}
	return NowPlaying{}, err
}

// withPlatformTrackID 每次都用当前播放证据重新验证原生 ID。
// 失败查询只每 5 秒重试一次，但成功的旧 ID 绝不会在
// 下一轮缺少来源证据时被沿用。
func (source *fallbackNowPlayingSource) withPlatformTrackID(ctx context.Context, track NowPlaying) NowPlaying {
	platform := sourceProviderID(track.Source)
	if platform == "qq" || platform == "netease" {
		// 先清除预填身份，再进入失败退避判断；即使本轮处于
		// 退避期或上下文已经取消，也不能把上游或上一轮的 ID 原样返回。
		track.TrackID = ""
		track.TrackIDOrigin = ""
		if platform == "qq" {
			track.QQSongID = 0
		}
	} else if track.SourceTrackID() != "" {
		return track
	}
	if ctx == nil || ctx.Err() != nil {
		return track
	}
	now := source.now()
	source.mu.Lock()
	if source.lastProbeFailed && !source.lastProbeAt.IsZero() && now.Sub(source.lastProbeAt) >= 0 &&
		now.Sub(source.lastProbeAt) < platformIDProbeInterval &&
		sameTrackWithoutID(track, source.lastProbeTrack) {
		source.mu.Unlock()
		return track
	}
	source.lastProbeTrack = track
	source.lastProbeAt = now
	enrich := source.enrich
	source.mu.Unlock()
	if enrich == nil {
		enrich = enrichPlatformTrackID
	}
	enriched := enrich(ctx, track)
	source.mu.Lock()
	source.lastProbeFailed = enriched.SourceTrackID() == ""
	source.mu.Unlock()
	return enriched
}

func (source *fallbackNowPlayingSource) now() time.Time {
	if source.clock != nil {
		return source.clock()
	}
	return time.Now()
}

// Close 释放 CaseScreen 对 QQ 音乐和网易云音乐持有的
// 只读查询句柄。适配器本身可在重新启用歌词后再次建立探针。
func (source *fallbackNowPlayingSource) Close() error {
	defaultQQNativeIDSource.closeAll()
	defaultNeteaseNativeIDSource.closeAll()
	return nil
}

func (source *smtcSource) Current(ctx context.Context) (NowPlaying, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := procRoInitialize.Call(1)
	if hresultFailed(hr) {
		return NowPlaying{}, hresultError("RoInitialize", hr)
	}
	defer procRoUninitialize.Call()

	className, err := createHString("Windows.Media.Control.GlobalSystemMediaTransportControlsSessionManager")
	if err != nil {
		return NowPlaying{}, err
	}
	defer deleteHString(className)
	var factory uintptr
	hr, _, _ = procRoGetActivationFactory.Call(
		className,
		uintptr(unsafe.Pointer(&iidSessionManagerStatics)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hresultFailed(hr) {
		return NowPlaying{}, hresultError("RoGetActivationFactory", hr)
	}
	defer comRelease(factory)

	var managerAsync uintptr
	if err := comMethod(factory, 6, uintptr(unsafe.Pointer(&managerAsync))); err != nil {
		return NowPlaying{}, fmt.Errorf("请求 SMTC 管理器: %w", err)
	}
	defer comRelease(managerAsync)
	manager, err := awaitObject(ctx, managerAsync)
	if err != nil {
		return NowPlaying{}, fmt.Errorf("等待 SMTC 管理器: %w", err)
	}
	defer comRelease(manager)

	var session uintptr
	if wanted := sourceProviderID(source.preferredSource); wanted != "" {
		session, err = selectSessionForSource(manager, wanted)
	} else {
		session, err = selectSession(manager)
	}
	if err != nil {
		return NowPlaying{}, err
	}
	if session == 0 {
		return NowPlaying{}, nil
	}
	defer comRelease(session)
	return readSession(ctx, session)
}

// selectSessionForSource 从 SMTC 列表中只选择指定的 QQ/网易云
// 会话。这避免 Chrome 等另一个正在播放的会话把平台
// 真机验证导向错误应用；它不会按标题猜测平台。
func selectSessionForSource(manager uintptr, wanted string) (uintptr, error) {
	wanted = sourceProviderID(wanted)
	if wanted == "" {
		return 0, errors.New("SMTC 指定媒体平台无效")
	}
	var list uintptr
	if err := comMethod(manager, 7, uintptr(unsafe.Pointer(&list))); err != nil {
		return 0, err
	}
	defer comRelease(list)
	var count uint32
	if err := comMethod(list, 7, uintptr(unsafe.Pointer(&count))); err != nil {
		return 0, err
	}
	if count > 256 {
		return 0, errors.New("SMTC 媒体会话数量超过上限")
	}
	var selected uintptr
	bestScore := -1
	for index := uint32(0); index < count; index++ {
		var session uintptr
		if err := comMethod(list, 6, uintptr(index), uintptr(unsafe.Pointer(&session))); err != nil || session == 0 {
			continue
		}
		sourceID, _ := getHStringProperty(session, 6)
		if sourceProviderID(identifySource(sourceID)) != wanted {
			comRelease(session)
			continue
		}
		score := sessionScore(session)
		if score > bestScore {
			comRelease(selected)
			selected = session
			bestScore = score
		} else {
			comRelease(session)
		}
	}
	return selected, nil
}

func selectSession(manager uintptr) (uintptr, error) {
	var current uintptr
	if err := comMethod(manager, 6, uintptr(unsafe.Pointer(&current))); err != nil {
		return 0, fmt.Errorf("读取当前 SMTC 会话: %w", err)
	}
	var list uintptr
	if err := comMethod(manager, 7, uintptr(unsafe.Pointer(&list))); err != nil {
		return current, nil
	}
	defer comRelease(list)
	var count uint32
	if err := comMethod(list, 7, uintptr(unsafe.Pointer(&count))); err != nil {
		return current, nil
	}
	sessions := make([]uintptr, 0, count)
	for index := uint32(0); index < count; index++ {
		var session uintptr
		if err := comMethod(list, 6, uintptr(index), uintptr(unsafe.Pointer(&session))); err == nil && session != 0 {
			sessions = append(sessions, session)
		}
	}
	selected := current
	bestScore := sessionScore(current)
	selectedIndex := -1
	for index, session := range sessions {
		score := sessionScore(session)
		if score > bestScore {
			selected = session
			bestScore = score
			selectedIndex = index
		}
	}
	for index, session := range sessions {
		if index != selectedIndex {
			comRelease(session)
		}
	}
	if selectedIndex >= 0 {
		comRelease(current)
	}
	return selected, nil
}

func sessionScore(session uintptr) int {
	if session == 0 {
		return -1
	}
	sourceID, _ := getHStringProperty(session, 6)
	source := identifySource(sourceID)
	score := 0
	if source == "netease" || source == "qq" || source == "kugou" || source == "sogou" || source == "kuwo" {
		score += 50
	}
	var playback uintptr
	if err := comMethod(session, 9, uintptr(unsafe.Pointer(&playback))); err == nil && playback != 0 {
		defer comRelease(playback)
		var status int32
		if getInt32Property(playback, 7, &status) == nil && status == mediaPlaybackPlaying {
			score += 100
		}
	}
	return score
}

func readSession(ctx context.Context, session uintptr) (NowPlaying, error) {
	sourceID, _ := getHStringProperty(session, 6)
	var propertiesAsync uintptr
	if err := comMethod(session, 7, uintptr(unsafe.Pointer(&propertiesAsync))); err != nil {
		return NowPlaying{}, fmt.Errorf("读取媒体属性任务: %w", err)
	}
	defer comRelease(propertiesAsync)
	properties, err := awaitObject(ctx, propertiesAsync)
	if err != nil {
		return NowPlaying{}, fmt.Errorf("等待媒体属性: %w", err)
	}
	defer comRelease(properties)
	title, err := getHStringProperty(properties, 6)
	if err != nil {
		return NowPlaying{}, fmt.Errorf("读取歌曲标题: %w", err)
	}
	artist, _ := getHStringProperty(properties, 9)
	album, _ := getHStringProperty(properties, 10)
	track := NowPlaying{
		Source: identifySource(sourceID),
		Title:  strings.TrimSpace(title),
		Artist: strings.TrimSpace(artist),
		Album:  strings.TrimSpace(album),
	}

	var timeline uintptr
	if err := comMethod(session, 8, uintptr(unsafe.Pointer(&timeline))); err == nil && timeline != 0 {
		defer comRelease(timeline)
		var end, position, lastUpdated int64
		if getInt64Property(timeline, 7, &end) == nil && getInt64Property(timeline, 10, &position) == nil {
			track.Duration = end * int64(100*time.Nanosecond)
			track.Position = position * int64(100*time.Nanosecond)
			track.PositionKnown = true
			if getInt64Property(timeline, 11, &lastUpdated) == nil {
				track.PositionUpdatedAt, _ = windowsDateTime(lastUpdated)
			}
		}
	}
	// 歌词时间轴不能把“播放状态未知”猜成“正在播放”。如果这里读取失败，
	// 让上层按媒体源错误冻结当前锚点，避免暂停后歌词继续滚动。
	var playback uintptr
	if err := comMethod(session, 9, uintptr(unsafe.Pointer(&playback))); err != nil {
		return NowPlaying{}, fmt.Errorf("读取播放状态: %w", err)
	}
	if playback == 0 {
		return NowPlaying{}, errors.New("读取播放状态: 返回空对象")
	}
	defer comRelease(playback)
	var status int32
	if err := getInt32Property(playback, 7, &status); err != nil {
		return NowPlaying{}, fmt.Errorf("读取播放状态值: %w", err)
	}
	track.Playing = status == mediaPlaybackPlaying
	return track, nil
}

func awaitObject(ctx context.Context, async uintptr) (uintptr, error) {
	if async == 0 {
		return 0, errors.New("异步操作为空")
	}
	var info uintptr
	if err := comMethod(async, 0, uintptr(unsafe.Pointer(&iidAsyncInfo)), uintptr(unsafe.Pointer(&info))); err != nil {
		return 0, err
	}
	defer comRelease(info)
	for {
		if err := ctx.Err(); err != nil {
			_ = comMethod(info, 9)
			return 0, err
		}
		var status int32
		if err := getInt32Property(info, 7, &status); err != nil {
			return 0, err
		}
		switch status {
		case asyncCompleted:
			var result uintptr
			if err := comMethod(async, 8, uintptr(unsafe.Pointer(&result))); err != nil {
				return 0, err
			}
			_ = comMethod(info, 10)
			return result, nil
		case asyncCanceled:
			return 0, errors.New("WinRT 异步操作已取消")
		case asyncError:
			var code int32
			_ = getInt32Property(info, 8, &code)
			return 0, fmt.Errorf("WinRT 异步操作失败: 0x%08X", uint32(code))
		case asyncStarted:
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = comMethod(info, 9)
				return 0, ctx.Err()
			case <-timer.C:
			}
		default:
			return 0, fmt.Errorf("未知 WinRT 异步状态: %d", status)
		}
	}
}

func getHStringProperty(object uintptr, index int) (string, error) {
	var value uintptr
	if err := comMethod(object, index, uintptr(unsafe.Pointer(&value))); err != nil {
		return "", err
	}
	if value == 0 {
		return "", nil
	}
	defer deleteHString(value)
	var length uint32
	raw, _, _ := procWindowsStringRawBuffer.Call(value, uintptr(unsafe.Pointer(&length)))
	if raw == 0 || length == 0 {
		return "", nil
	}
	return windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(raw)), int(length))), nil
}

func getInt32Property(object uintptr, index int, value *int32) error {
	return comMethod(object, index, uintptr(unsafe.Pointer(value)))
}

func getInt64Property(object uintptr, index int, value *int64) error {
	return comMethod(object, index, uintptr(unsafe.Pointer(value)))
}

const windowsToUnixEpochSeconds = int64(11644473600)

// windowsDateTime 把 WinRT DateTime 的 1601 UTC 起 100ns 计数转换为
// time.Time。分拆秒与余数可避免直接乘纳秒发生 int64 溢出。
func windowsDateTime(ticks int64) (time.Time, bool) {
	const ticksPerSecond = int64(10_000_000)
	if ticks <= 0 {
		return time.Time{}, false
	}
	seconds := ticks/ticksPerSecond - windowsToUnixEpochSeconds
	nanoseconds := ticks % ticksPerSecond * 100
	converted := time.Unix(seconds, nanoseconds).UTC()
	// 媒体会话更新时间不应早于 Unix 纪元，也不能超出 Go 可可靠
	// 表示并用于时长差计算的范围。
	if converted.Before(time.Unix(0, 0).UTC()) || converted.Year() > 9999 {
		return time.Time{}, false
	}
	return converted, true
}

func createHString(value string) (uintptr, error) {
	utf16Value, err := windows.UTF16FromString(value)
	if err != nil {
		return 0, err
	}
	var result uintptr
	hr, _, _ := procWindowsCreateString.Call(
		uintptr(unsafe.Pointer(&utf16Value[0])),
		uintptr(len(utf16Value)-1),
		uintptr(unsafe.Pointer(&result)),
	)
	if hresultFailed(hr) {
		return 0, hresultError("WindowsCreateString", hr)
	}
	return result, nil
}

func deleteHString(value uintptr) {
	if value != 0 {
		procWindowsDeleteString.Call(value)
	}
}

func comMethod(object uintptr, index int, arguments ...uintptr) error {
	if object == 0 {
		return errors.New("COM 对象为空")
	}
	vtable := *(*uintptr)(unsafe.Pointer(object))
	method := *(*uintptr)(unsafe.Pointer(vtable + uintptr(index)*unsafe.Sizeof(uintptr(0))))
	callArguments := make([]uintptr, 1, len(arguments)+1)
	callArguments[0] = object
	callArguments = append(callArguments, arguments...)
	hr, _, _ := syscall.SyscallN(method, callArguments...)
	if hresultFailed(hr) {
		return hresultError(fmt.Sprintf("COM[%d]", index), hr)
	}
	return nil
}

func comRelease(object uintptr) {
	if object == 0 {
		return
	}
	vtable := *(*uintptr)(unsafe.Pointer(object))
	method := *(*uintptr)(unsafe.Pointer(vtable + 2*unsafe.Sizeof(uintptr(0))))
	syscall.SyscallN(method, object)
}

func hresultFailed(value uintptr) bool {
	return uint32(value)&0x80000000 != 0
}

func hresultError(operation string, value uintptr) error {
	return fmt.Errorf("%s 失败: HRESULT 0x%08X", operation, uint32(value))
}

func mustGUID(value string) windows.GUID {
	guid, err := windows.GUIDFromString(value)
	if err != nil {
		panic(err)
	}
	return guid
}

func identifySource(appID string) string {
	lower := strings.ToLower(appID)
	switch {
	case strings.Contains(lower, "cloudmusic"), strings.Contains(lower, "netease"):
		return "netease"
	case strings.Contains(lower, "qqmusic"):
		return "qq"
	case strings.Contains(lower, "kugou"):
		return "kugou"
	case strings.Contains(lower, "sogoumusic"), strings.Contains(lower, "sogou_music"):
		return "sogou"
	case strings.Contains(lower, "kwmusic"), strings.Contains(lower, "kuwomusic"), strings.Contains(lower, "kuwo"):
		return "kuwo"
	default:
		return appID
	}
}
