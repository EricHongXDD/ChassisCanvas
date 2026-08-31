// Package edge 编排副屏上的轻量播放元数据、精确 ID 歌词缓存和原厂显示注入。
// 本包不读取音频设备，不访问麦克风，也不实现声纹识别。
package edge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"casescreen/internal/edgedisplay"
	"casescreen/internal/edgelyrics"
	"casescreen/internal/edgesync"
)

const (
	minimumRenderInterval = 250 * time.Millisecond
	maximumRenderInterval = time.Second

	// PlatformLoginNotice 是主机 relay 返回 needs_login 时的固定屏幕提示。
	PlatformLoginNotice = "请在电脑端导入平台登录态或确认会员权限"

	// StandalonePlatformLoginNotice 是 direct 模式在副屏本地读取登录态时的提示。
	StandalonePlatformLoginNotice = "请在副屏导入平台登录态或确认会员权限"
)

// MetadataClient 只读取主机暴露的小型 now-playing 包。
type MetadataClient interface {
	Current(context.Context) (edgesync.Envelope, error)
}

// LyricsEngine 是可替换的屏端歌词时间轴核心，便于断网后有界重试和单元测试。
type LyricsEngine interface {
	Update(context.Context, edgelyrics.Metadata) error
	Snapshot() edgelyrics.State
}

// Display 只接受当前句、下一句和轻量播放状态。
type Display interface {
	SendLyricsUpdate(context.Context, edgedisplay.LyricsUpdate) error
}

// CoreFactory 创建一个读取相同 TF 缓存和登录态的新歌词核心。
// 只有某首歌首次精确查询失败且退避到期时才会再次调用。
type CoreFactory func() LyricsEngine

// ErrorSink 接收已经去重的运行诊断，不应记录配对令牌或登录态。
type ErrorSink func(scope, message string)

// Config 控制局域网轮询、屏内时间轴刷新和失败退避。
type Config struct {
	PollInterval        time.Duration
	RenderInterval      time.Duration
	SyncTimeout         time.Duration
	LyricsRetryMin      time.Duration
	LyricsRetryMax      time.Duration
	ErrorRepeatAfter    time.Duration
	PlatformLoginNotice string
	IdleNotice          string
	ErrorSink           ErrorSink
}

// Runner 在独立 goroutine 中轮询元数据；歌词联网阻塞不会停止显示线程的本地时间轴。
type Runner struct {
	client  MetadataClient
	display Display
	factory CoreFactory
	config  Config
	now     func() time.Time

	coreMu sync.RWMutex
	core   LyricsEngine

	stateMu          sync.RWMutex
	lastIdentity     string
	lastMetadata     edgelyrics.Metadata
	remoteMode       edgesync.LyricMode
	failedIdentity   string
	failurePermanent bool
	nextLyricsRetry  time.Time
	lyricsRetryDelay time.Duration
	syncError        string
	lastSent         *edgedisplay.LyricsUpdate

	errorMu sync.Mutex
	errors  map[string]reportedError
}

type reportedError struct {
	message string
	at      time.Time
}

// NewRunner 验证时序边界并创建编排器。
func NewRunner(config Config, client MetadataClient, factory CoreFactory, display Display) (*Runner, error) {
	return newRunnerWithClock(config, client, factory, display, time.Now)
}

func newRunnerWithClock(config Config, client MetadataClient, factory CoreFactory, display Display, now func() time.Time) (*Runner, error) {
	if client == nil {
		return nil, errors.New("屏端元数据客户端不能为空")
	}
	if display == nil {
		return nil, errors.New("屏端显示注入器不能为空")
	}
	if factory == nil {
		return nil, errors.New("屏端歌词核心工厂不能为空")
	}
	if now == nil {
		now = time.Now
	}
	config = normalizeConfig(config)
	if config.PollInterval < 250*time.Millisecond || config.PollInterval > 30*time.Second {
		return nil, errors.New("now-playing 轮询间隔必须在 250 毫秒..30 秒之间")
	}
	if config.RenderInterval < minimumRenderInterval || config.RenderInterval > maximumRenderInterval {
		return nil, errors.New("屏端歌词注入间隔必须在 250..1000 毫秒之间")
	}
	if config.SyncTimeout <= 0 || config.SyncTimeout > 30*time.Second {
		return nil, errors.New("now-playing 请求超时必须在 0..30 秒之间")
	}
	if config.LyricsRetryMin <= 0 || config.LyricsRetryMax < config.LyricsRetryMin {
		return nil, errors.New("歌词重试退避参数无效")
	}
	if config.LyricsRetryMax > 24*time.Hour || config.ErrorRepeatAfter <= 0 || config.ErrorRepeatAfter > 24*time.Hour {
		return nil, errors.New("歌词重试或错误去重时间超过安全范围")
	}
	if len(config.IdleNotice) > 2<<10 {
		return nil, errors.New("屏端空闲提示过长")
	}
	core := factory()
	if core == nil {
		return nil, errors.New("屏端歌词核心工厂返回空实例")
	}
	return &Runner{
		client: client, display: display, factory: factory, config: config, now: now,
		core: core, errors: make(map[string]reportedError),
	}, nil
}

func normalizeConfig(config Config) Config {
	if config.PollInterval == 0 {
		config.PollInterval = time.Second
	}
	if config.RenderInterval == 0 {
		config.RenderInterval = 500 * time.Millisecond
	}
	if config.SyncTimeout == 0 {
		config.SyncTimeout = 5 * time.Second
	}
	if config.LyricsRetryMin == 0 {
		config.LyricsRetryMin = 5 * time.Second
	}
	if config.LyricsRetryMax == 0 {
		config.LyricsRetryMax = time.Minute
	}
	if config.ErrorRepeatAfter == 0 {
		config.ErrorRepeatAfter = 30 * time.Second
	}
	if strings.TrimSpace(config.PlatformLoginNotice) == "" {
		config.PlatformLoginNotice = PlatformLoginNotice
	}
	return config
}

// Run 同时运行元数据轮询和 250..1000ms 的本地歌词渲染循环。
func (runner *Runner) Run(parent context.Context) error {
	if parent == nil {
		return errors.New("屏端编排上下文不能为空")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		runner.runPollLoop(ctx)
	}()
	go func() {
		defer wait.Done()
		runner.runRenderLoop(ctx)
	}()
	<-ctx.Done()
	wait.Wait()
	return nil
}

func (runner *Runner) runPollLoop(ctx context.Context) {
	delay := time.Duration(0)
	backoff := runner.config.PollInterval
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return
		case <-timer.C:
		}
		if err := runner.PollOnce(ctx); err != nil {
			delay = backoff
			backoff = growDelay(backoff, 30*time.Second)
			continue
		}
		backoff = runner.config.PollInterval
		delay = runner.config.PollInterval
	}
}

func (runner *Runner) runRenderLoop(ctx context.Context) {
	_ = runner.RenderOnce(ctx)
	ticker := time.NewTicker(runner.config.RenderInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = runner.RenderOnce(ctx)
		}
	}
}

// PollOnce 读取一次轻量元数据。网络失败时不会清空歌词核心，因此已缓存的
// 当前歌曲仍按本地锚点滚动；同一个错误只交给 ErrorSink 一次。
func (runner *Runner) PollOnce(parent context.Context) error {
	if parent == nil {
		return errors.New("now-playing 轮询上下文不能为空")
	}
	ctx, cancel := context.WithTimeout(parent, runner.config.SyncTimeout)
	envelope, err := runner.client.Current(ctx)
	cancel()
	if err != nil {
		runner.stateMu.Lock()
		runner.syncError = "播放信息同步暂时断开"
		runner.stateMu.Unlock()
		runner.reportError("sync", err)
		return err
	}
	runner.clearError("sync")
	runner.stateMu.Lock()
	runner.syncError = ""
	runner.stateMu.Unlock()

	now := runner.now()
	metadata, remoteMode := MetadataFromEnvelope(envelope, now)
	return runner.applyMetadata(parent, metadata, remoteMode, now)
}

func (runner *Runner) applyMetadata(ctx context.Context, metadata edgelyrics.Metadata, remoteMode edgesync.LyricMode, now time.Time) error {
	identity := metadataIdentity(metadata)
	runner.stateMu.Lock()
	changed := identity != runner.lastIdentity
	runner.lastIdentity = identity
	runner.lastMetadata = metadata
	runner.remoteMode = remoteMode
	if changed {
		runner.failedIdentity = ""
		runner.failurePermanent = false
		runner.nextLyricsRetry = time.Time{}
		runner.lyricsRetryDelay = 0
	}
	failed := identity != "" && identity == runner.failedIdentity
	retryDue := failed && !runner.failurePermanent && !now.Before(runner.nextLyricsRetry)
	if identity == "" {
		runner.failedIdentity = ""
		runner.failurePermanent = false
		runner.nextLyricsRetry = time.Time{}
		runner.lyricsRetryDelay = 0
	}
	runner.stateMu.Unlock()
	if changed {
		runner.clearError("lyrics")
	}

	core := runner.currentCore()
	if err := core.Update(ctx, metadata); err != nil {
		runner.recordLyricsFailure(identity, err, now)
		return err
	}
	if changed || identity == "" {
		runner.clearLyricsFailure()
		return nil
	}
	if !retryDue {
		return nil
	}

	// 同曲目的普通进度包不会触发联网；只有显式退避到期才新建核心重试。
	candidate := runner.factory()
	if candidate == nil {
		err := errors.New("屏端歌词核心工厂返回空实例")
		runner.recordLyricsFailure(identity, err, now)
		return err
	}
	if err := candidate.Update(ctx, metadata); err != nil {
		runner.recordLyricsFailure(identity, err, now)
		return err
	}
	runner.coreMu.Lock()
	runner.core = candidate
	runner.coreMu.Unlock()
	runner.clearLyricsFailure()
	return nil
}

func (runner *Runner) recordLyricsFailure(identity string, err error, now time.Time) {
	permanent := errors.Is(err, edgelyrics.ErrMissingTrackID) || errors.Is(err, edgelyrics.ErrUnsupportedPlatform)
	runner.stateMu.Lock()
	runner.failedIdentity = identity
	runner.failurePermanent = permanent
	if permanent {
		runner.nextLyricsRetry = time.Time{}
	} else {
		if runner.lyricsRetryDelay <= 0 {
			runner.lyricsRetryDelay = runner.config.LyricsRetryMin
		} else {
			runner.lyricsRetryDelay = growDelay(runner.lyricsRetryDelay, runner.config.LyricsRetryMax)
		}
		runner.nextLyricsRetry = now.Add(runner.lyricsRetryDelay)
	}
	runner.stateMu.Unlock()
	runner.reportError("lyrics", err)
}

func (runner *Runner) clearLyricsFailure() {
	runner.stateMu.Lock()
	runner.failedIdentity = ""
	runner.failurePermanent = false
	runner.nextLyricsRetry = time.Time{}
	runner.lyricsRetryDelay = 0
	runner.stateMu.Unlock()
	runner.clearError("lyrics")
}

func (runner *Runner) currentCore() LyricsEngine {
	runner.coreMu.RLock()
	defer runner.coreMu.RUnlock()
	return runner.core
}

// RenderOnce 只读取屏内 Core 的本地时间轴。暂停且内容未变化时跳过重复小包；
// 播放中 position 会推进，因此会按 RenderInterval 持续更新。
func (runner *Runner) RenderOnce(ctx context.Context) error {
	if ctx == nil {
		return errors.New("屏端显示上下文不能为空")
	}
	state := runner.currentCore().Snapshot()
	runner.stateMu.RLock()
	remoteMode := runner.remoteMode
	syncError := runner.syncError
	runner.stateMu.RUnlock()
	update := DisplayUpdateWithNotices(state, remoteMode, syncError, runner.config.PlatformLoginNotice, runner.config.IdleNotice)

	runner.stateMu.Lock()
	if runner.lastSent != nil && *runner.lastSent == update {
		runner.stateMu.Unlock()
		return nil
	}
	runner.stateMu.Unlock()
	if err := runner.display.SendLyricsUpdate(ctx, update); err != nil {
		runner.reportError("display", err)
		return err
	}
	runner.clearError("display")
	runner.stateMu.Lock()
	copyOfUpdate := update
	runner.lastSent = &copyOfUpdate
	runner.stateMu.Unlock()
	return nil
}

// MetadataFromEnvelope 把局域网协议映射为屏端精确歌词查询所需的最小元数据。
// TrackID 为空时仍保留为空，由 Core 明确拒绝，绝不回退为歌名搜索。
func MetadataFromEnvelope(envelope edgesync.Envelope, receivedAt time.Time) (edgelyrics.Metadata, edgesync.LyricMode) {
	if !envelope.Available {
		return edgelyrics.Metadata{}, edgesync.LyricModeUnknown
	}
	source := strings.TrimSpace(envelope.Metadata.Source)
	if source == "" {
		source = strings.TrimSpace(envelope.Track.Source)
	}
	trackID := strings.TrimSpace(envelope.Metadata.TrackID)
	if trackID == "" {
		trackID = strings.TrimSpace(envelope.Track.TrackID)
	}
	mode := envelope.Metadata.LyricMode
	switch mode {
	case edgesync.LyricModeTimed, edgesync.LyricModePlain, edgesync.LyricModeUnknown:
	default:
		mode = edgesync.LyricModeUnknown
	}
	return edgelyrics.Metadata{
		Source: source, TrackID: trackID,
		Title: strings.TrimSpace(envelope.Track.Title), Artist: strings.TrimSpace(envelope.Track.Artist),
		Album: strings.TrimSpace(envelope.Track.Album), Position: envelope.Track.Position,
		Duration: envelope.Track.Duration, PositionKnown: envelope.Track.PositionKnown,
		Playing: envelope.Track.Playing, CapturedAt: receivedAt,
	}, mode
}

// DisplayUpdate 只产生原厂渲染器需要的七个轻量字段。
func DisplayUpdate(state edgelyrics.State, remoteMode edgesync.LyricMode, syncError string) edgedisplay.LyricsUpdate {
	return DisplayUpdateWithLoginNotice(state, remoteMode, syncError, PlatformLoginNotice)
}

// DisplayUpdateWithLoginNotice 允许独立固件把登录提示指向副屏本身，同时保留
// relay 模式原有的“在电脑端更新”提示。
func DisplayUpdateWithLoginNotice(state edgelyrics.State, remoteMode edgesync.LyricMode, syncError, loginNotice string) edgedisplay.LyricsUpdate {
	return DisplayUpdateWithNotices(state, remoteMode, syncError, loginNotice, "")
}

// DisplayUpdateWithNotices 保留兼容参数，但等待播放器、取词、登录或网络恢复
// 时不再把诊断文字塞进歌词层；这些信息写入日志/状态页，屏幕保持完全隐藏。
func DisplayUpdateWithNotices(state edgelyrics.State, remoteMode edgesync.LyricMode, syncError, loginNotice, idleNotice string) edgedisplay.LyricsUpdate {
	mode := edgedisplay.LyricMode(remoteMode)
	current := ""
	next := ""
	switch state.LyricsType {
	case edgelyrics.LyricsSynchronized:
		mode = edgedisplay.LyricModeTimed
		current = state.Current.Text
		next = state.Next.Text
	case edgelyrics.LyricsPlainText:
		mode = edgedisplay.LyricModePlain
		current = edgedisplay.PlainLyricsNotice
	default:
		if mode == "" {
			mode = edgedisplay.LyricModeUnknown
		}
	}
	return edgedisplay.LyricsUpdate{
		Source: state.Track.Source, TrackID: state.Track.TrackID, Position: state.Position,
		Playing: state.Track.Playing, LyricMode: mode, Current: current, Next: next,
	}
}

func metadataIdentity(metadata edgelyrics.Metadata) string {
	source := strings.ToLower(strings.TrimSpace(metadata.Source))
	trackID := strings.TrimSpace(metadata.TrackID)
	if source == "" && trackID == "" && metadata.Title == "" && metadata.Artist == "" {
		return ""
	}
	if trackID != "" {
		return source + "\nid=" + trackID
	}
	return source + "\nmissing-id\n" + strings.ToLower(strings.TrimSpace(metadata.Title)) + "\n" +
		strings.ToLower(strings.TrimSpace(metadata.Artist))
}

func (runner *Runner) reportError(scope string, err error) {
	if err == nil || runner.config.ErrorSink == nil {
		return
	}
	now := runner.now()
	message := err.Error()
	runner.errorMu.Lock()
	previous, exists := runner.errors[scope]
	if exists && previous.message == message && now.Sub(previous.at) < runner.config.ErrorRepeatAfter {
		runner.errorMu.Unlock()
		return
	}
	runner.errors[scope] = reportedError{message: message, at: now}
	runner.errorMu.Unlock()
	runner.config.ErrorSink(scope, message)
}

func (runner *Runner) clearError(scope string) {
	runner.errorMu.Lock()
	delete(runner.errors, scope)
	runner.errorMu.Unlock()
}

func growDelay(current, maximum time.Duration) time.Duration {
	if current <= 0 {
		return maximum
	}
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (runner *Runner) String() string {
	runner.stateMu.RLock()
	defer runner.stateMu.RUnlock()
	return fmt.Sprintf("edge.Runner{track:%q mode:%q}", runner.lastIdentity, runner.remoteMode)
}
