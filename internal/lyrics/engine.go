package lyrics

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"casescreen/internal/render"
)

type EngineStatus struct {
	Running    bool       `json:"running"`
	Track      NowPlaying `json:"track"`
	Provider   string     `json:"provider,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	HasLyrics  bool       `json:"has_lyrics"`
	LyricMode  string     `json:"lyric_mode"`
	Scrollable bool       `json:"scrollable"`
	LastUpdate time.Time  `json:"last_update"`
}

// lyricDeliveryState 只包含歌词小包协议字段。播放位置和来源歌曲 ID留在
// 主机内；暂停/恢复会改变 Playing，必须产生一次状态事件。
type lyricDeliveryState struct {
	Current        string
	Next           string
	CurrentAt      time.Duration
	NextAt         time.Duration
	CurrentAtKnown bool
	NextAtKnown    bool
	Playing        bool
}

// Engine 轮询当前歌曲，切歌时才请求歌词；日常渲染只做本地时间轴查询。
type Engine struct {
	source     NowPlayingSource
	resolver   *Resolver
	interval   time.Duration
	retryDelay time.Duration
	changed    chan struct{}
	updates    chan struct{}
	reschedule chan struct{}

	mu             sync.RWMutex
	enabled        bool
	refreshStop    context.CancelFunc
	resolveStop    context.CancelFunc
	resolveVersion uint64
	status         EngineStatus
	document       Document
	trackKey       string
	anchorPosition time.Duration
	anchorTime     time.Time
	lastResolve    time.Time
	sourceError    string
	resolveError   string
	lastDelivery   lyricDeliveryState
}

func NewEngine(source NowPlayingSource, resolver *Resolver) *Engine {
	return &Engine{
		source: source, resolver: resolver, interval: time.Second, retryDelay: 30 * time.Second,
		changed: make(chan struct{}, 1), updates: make(chan struct{}, 1),
		reschedule: make(chan struct{}, 1), enabled: true,
	}
}

func (engine *Engine) Run(ctx context.Context) error {
	if engine.source == nil || engine.resolver == nil {
		return nil
	}
	defer engine.closeSource()
	clockContext, stopClock := context.WithCancel(ctx)
	clockDone := make(chan struct{})
	go func() {
		defer close(clockDone)
		engine.runDeliveryClock(clockContext)
	}()
	defer func() {
		stopClock()
		<-clockDone
	}()
	ticker := time.NewTicker(engine.interval)
	defer ticker.Stop()
	if engine.isEnabled() {
		engine.setRunning(true)
		engine.runRefresh(ctx)
	}
	defer engine.setRunning(false)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-engine.changed:
			if engine.isEnabled() {
				engine.setRunning(true)
				engine.runRefresh(ctx)
			} else {
				engine.setRunning(false)
			}
		case <-ticker.C:
			if engine.isEnabled() {
				engine.runRefresh(ctx)
			}
		}
	}
}

// SetEnabled 关闭时停止媒体会话轮询并清空旧歌词，重新启用后立即刷新。
func (engine *Engine) SetEnabled(enabled bool) {
	engine.mu.Lock()
	if engine.enabled == enabled {
		engine.mu.Unlock()
		return
	}
	engine.enabled = enabled
	refreshStop := engine.refreshStop
	resolveStop := engine.resolveStop
	if !enabled {
		engine.resolveVersion++
		engine.resolveStop = nil
		engine.status = EngineStatus{}
		engine.document = Document{}
		engine.trackKey = ""
		engine.anchorPosition = 0
		engine.anchorTime = time.Time{}
		engine.lastResolve = time.Time{}
		engine.sourceError = ""
		engine.resolveError = ""
		engine.lastDelivery = lyricDeliveryState{}
	}
	engine.mu.Unlock()
	if !enabled && refreshStop != nil {
		refreshStop()
	}
	if !enabled && resolveStop != nil {
		resolveStop()
	}
	if !enabled {
		engine.closeSource()
	}
	select {
	case engine.changed <- struct{}{}:
	default:
	}
	engine.rescheduleDelivery()
	engine.notifyUpdate()
}

func (engine *Engine) closeSource() {
	if engine == nil || engine.source == nil {
		return
	}
	if closer, ok := engine.source.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// SetPreferredProvider 允许运行中的桌面设置立即切换在线歌词来源。
func (engine *Engine) SetPreferredProvider(providerID string) {
	if engine.resolver != nil {
		engine.resolver.SetPreferredProvider(providerID)
	}
	engine.invalidateResolution()
}

// SetLocalPath 更新本地 LRC 文件路径。
func (engine *Engine) SetLocalPath(path string) {
	if engine.resolver != nil {
		engine.resolver.SetLocalPath(path)
	}
	engine.invalidateResolution()
}

func (engine *Engine) invalidateResolution() {
	engine.mu.Lock()
	resolveStop := engine.resolveStop
	engine.resolveVersion++
	engine.resolveStop = nil
	engine.document = Document{}
	engine.trackKey = ""
	engine.lastResolve = time.Time{}
	engine.resolveError = ""
	engine.status.Provider = ""
	engine.status.HasLyrics = false
	engine.status.LyricMode = ""
	engine.status.Scrollable = false
	engine.status.LastError = ""
	engine.lastDelivery = lyricDeliveryState{}
	engine.mu.Unlock()
	if resolveStop != nil {
		resolveStop()
	}
	select {
	case engine.changed <- struct{}{}:
	default:
	}
	engine.rescheduleDelivery()
	engine.notifyUpdate()
}

func (engine *Engine) isEnabled() bool {
	engine.mu.RLock()
	defer engine.mu.RUnlock()
	return engine.enabled
}

func (engine *Engine) Snapshot() render.LyricState {
	return engine.SnapshotAt(time.Now())
}

// SyncSnapshot 在同一把读锁下取得歌词画面与歌曲状态，避免切歌恰好发生在
// 两次独立读取之间时，把上一首歌词和下一首播放锚点装进同一个 USB 小包。
func (engine *Engine) SyncSnapshot() (render.LyricState, EngineStatus) {
	now := time.Now()
	engine.mu.RLock()
	document := engine.document
	position := engine.anchorPosition
	anchorTime := engine.anchorTime
	status := engine.status
	engine.mu.RUnlock()
	return lyricStateAt(document, position, anchorTime, status.Track.PositionKnown, status.Track.Playing, now), status
}

// SnapshotAt 使用同一个采样时刻计算主机显示行与进度，也便于验证暂停后
// 本地歌词时钟不会继续前进。连续播放位置和主机锚点不进入 USB 包；
// 当前/下一句的离散时间戳会随五字段歌词事件一并发送。
func (engine *Engine) SnapshotAt(now time.Time) render.LyricState {
	engine.mu.RLock()
	document := engine.document
	position := engine.anchorPosition
	anchorTime := engine.anchorTime
	playing := engine.status.Track.Playing
	positionKnown := engine.status.Track.PositionKnown
	engine.mu.RUnlock()
	return lyricStateAt(document, position, anchorTime, positionKnown, playing, now)
}

func lyricStateAt(document Document, position time.Duration, anchorTime time.Time, positionKnown, playing bool, now time.Time) render.LyricState {
	if positionKnown && playing && !anchorTime.IsZero() && now.After(anchorTime) {
		position += now.Sub(anchorTime)
	}
	baseState := render.LyricState{Position: position, PositionKnown: positionKnown, Playing: playing}
	if !document.Scrollable() && document.HasContent() {
		baseState.Current = UnsynchronizedLyricsMessage
		return baseState
	}
	// 没有可靠播放位置时不能从 0 或上一次锚点猜测当前句。保留已取到的
	// 文档，但显示层失败关闭，直到播放器重新给出可信时间轴。
	if !positionKnown {
		return baseState
	}
	current, next, progress, ok := document.Current(position)
	if !ok {
		return baseState
	}
	baseState.Current = current.Text
	baseState.Next = next.Text
	baseState.Progress = progress
	baseState.CurrentAt = current.At
	baseState.NextAt = next.At
	return baseState
}

func (engine *Engine) Status() EngineStatus {
	engine.mu.RLock()
	defer engine.mu.RUnlock()
	return engine.status
}

// Updates 在显示文字、对应行时间戳或播放状态变化时通知分发服务。
// 暂停/恢复既重排主机本地截止时间，也发送一次小型 playing 状态事件。
func (engine *Engine) Updates() <-chan struct{} { return engine.updates }

func (engine *Engine) notifyUpdate() {
	select {
	case engine.updates <- struct{}{}:
	default:
	}
}

// rescheduleDelivery 唤醒主机本地歌词时钟。该信号不会写入 USB，连续
// 媒体采样会被容量为 1 的通道合并。
func (engine *Engine) rescheduleDelivery() {
	select {
	case engine.reschedule <- struct{}{}:
	default:
	}
}

// runDeliveryClock 只在下一句的本地截止时间唤醒 Updates。这样短于
// 一秒的歌词不会受 SMTC 轮询周期限制；暂停、拖动和切歌会通过
// reschedule 重新计算截止时间，期间既不轮询 USB，也不发送播放位置。
func (engine *Engine) runDeliveryClock(ctx context.Context) {
	var timer *time.Timer
	var timerChannel <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerChannel = nil
	}
	defer stopTimer()
	resetTimer := func() {
		stopTimer()
		delay, ok := engine.nextDeliveryDelay(time.Now())
		if !ok {
			return
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		timerChannel = timer.C
	}

	resetTimer()
	for {
		select {
		case <-ctx.Done():
			return
		case <-engine.reschedule:
			resetTimer()
		case <-timerChannel:
			now := time.Now()
			engine.mu.Lock()
			changed := engine.markDeliveryChangedLocked(now)
			engine.mu.Unlock()
			if changed {
				engine.notifyUpdate()
			}
			resetTimer()
		}
	}
}

// nextDeliveryDelay 返回从 now 到下一条严格更晚歌词行的时长。相同
// 时间戳的多行在同一次唤醒中合并；纯文本、暂停和未知位置不启动计时器。
func (engine *Engine) nextDeliveryDelay(now time.Time) (time.Duration, bool) {
	engine.mu.RLock()
	defer engine.mu.RUnlock()
	if !engine.enabled || !engine.document.Scrollable() || !engine.status.Track.Playing ||
		!engine.status.Track.PositionKnown || engine.anchorTime.IsZero() {
		return 0, false
	}
	position := engine.anchorPosition
	if now.After(engine.anchorTime) {
		position += now.Sub(engine.anchorTime)
	}
	index := sort.Search(len(engine.document.Lines), func(index int) bool {
		return engine.document.Lines[index].At > position
	})
	if index >= len(engine.document.Lines) {
		return 0, false
	}
	delay := engine.document.Lines[index].At - position
	if delay <= 0 {
		delay = time.Nanosecond
	}
	return delay, true
}

func (engine *Engine) runRefresh(ctx context.Context) {
	refreshContext, stop := context.WithTimeout(ctx, 3*time.Second)
	engine.mu.Lock()
	if !engine.enabled {
		engine.mu.Unlock()
		stop()
		return
	}
	engine.refreshStop = stop
	engine.mu.Unlock()
	engine.poll(refreshContext, ctx)
	stop()
	engine.mu.Lock()
	engine.refreshStop = nil
	engine.mu.Unlock()
}

func (engine *Engine) refresh(ctx context.Context) {
	done := engine.poll(ctx, ctx)
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// poll 只同步读取一次本机播放状态。联网取词在独立协程中进行，
// 因此暂停和切歌不会被一个慢的 HTTP 请求阻塞。
func (engine *Engine) poll(sourceContext, resolveContext context.Context) <-chan struct{} {
	if !engine.isEnabled() {
		return nil
	}
	track, err := engine.source.Current(sourceContext)
	now := time.Now()
	if !engine.isEnabled() {
		return nil
	}
	if err != nil {
		engine.mu.Lock()
		position := engine.anchorPosition
		if engine.status.Track.Playing && !engine.anchorTime.IsZero() && now.After(engine.anchorTime) {
			position += now.Sub(engine.anchorTime)
		}
		engine.anchorPosition = position
		engine.anchorTime = now
		engine.status.Track.Position = int64(position)
		engine.status.Track.Playing = false
		engine.sourceError = err.Error()
		engine.status.LastError = engine.sourceError
		engine.status.LastUpdate = now
		displayChanged := engine.markDeliveryChangedLocked(now)
		engine.mu.Unlock()
		if displayChanged {
			engine.notifyUpdate()
		}
		engine.rescheduleDelivery()
		return nil
	}
	engine.mu.Lock()
	engine.sourceError = ""
	engine.mu.Unlock()
	if !track.Valid() {
		engine.mu.Lock()
		hadTrack := engine.status.Track.Valid() || engine.document.HasContent()
		resolveStop := engine.resolveStop
		engine.resolveVersion++
		engine.resolveStop = nil
		engine.status = EngineStatus{Running: true, LastUpdate: now}
		engine.document = Document{}
		engine.trackKey = ""
		engine.anchorPosition = 0
		engine.anchorTime = time.Time{}
		engine.lastResolve = time.Time{}
		engine.resolveError = ""
		engine.lastDelivery = lyricDeliveryState{}
		engine.mu.Unlock()
		if resolveStop != nil {
			resolveStop()
		}
		if hadTrack {
			engine.notifyUpdate()
		}
		engine.rescheduleDelivery()
		return nil
	}
	key := track.key()
	engine.mu.RLock()
	sameTrack := key == engine.trackKey
	previousPosition := engine.anchorPosition
	hasLyrics := engine.status.HasLyrics
	lastResolve := engine.lastResolve
	retryDelay := engine.retryDelay
	resolveInFlight := engine.resolveStop != nil
	engine.mu.RUnlock()

	position := playbackPositionAt(track, now)
	if !track.PositionKnown && sameTrack {
		// 位置证据丢失时只保留最后可信锚点，不根据 playing 猜测推进。
		// Snapshot 会隐藏同步行，直到来源重新给出 PositionKnown。
		position = previousPosition
	}
	track.Position = int64(position)
	if track.PositionKnown {
		track.PositionUpdatedAt = now
	}
	if sameTrack {
		engine.mu.Lock()
		engine.status.Track = track
		engine.status.LastError = engine.resolveError
		engine.status.LastUpdate = now
		engine.anchorPosition = position
		engine.anchorTime = now
		displayChanged := engine.markDeliveryChangedLocked(now)
		engine.mu.Unlock()
		if displayChanged {
			engine.notifyUpdate()
		}
		engine.rescheduleDelivery()
		if hasLyrics || resolveInFlight || (!lastResolve.IsZero() && now.Sub(lastResolve) < retryDelay) {
			return nil
		}
	} else {
		// 切歌后先立即清空上一首歌词并发布新播放状态，再异步取词。
		// 旧请求即使稍后返回，也会因版本号不匹配而被丢弃。
		engine.mu.Lock()
		resolveStop := engine.resolveStop
		engine.resolveVersion++
		engine.resolveStop = nil
		engine.trackKey = key
		engine.document = Document{}
		engine.resolveError = ""
		engine.status = EngineStatus{Running: true, Track: track, LastUpdate: now}
		engine.anchorPosition = position
		engine.anchorTime = now
		engine.markDeliveryChangedLocked(now)
		engine.mu.Unlock()
		if resolveStop != nil {
			resolveStop()
		}
		engine.notifyUpdate()
		engine.rescheduleDelivery()
	}
	return engine.startResolve(resolveContext, track, key, now)
}

// playbackPositionAt 把播放器给出的 Position 投影到本轮采样时刻。
// Windows SMTC 明确规定 Position 是 LastUpdatedTime 时刻的值；忽略该
// 时刻会在每次轮询时把主机本地歌词时钟反复拉回旧位置。
func playbackPositionAt(track NowPlaying, sampledAt time.Time) time.Duration {
	position := time.Duration(track.Position)
	if position < 0 {
		position = 0
	}
	if track.PositionKnown && track.Playing && !track.PositionUpdatedAt.IsZero() &&
		sampledAt.After(track.PositionUpdatedAt) {
		position += sampledAt.Sub(track.PositionUpdatedAt)
	}
	duration := time.Duration(track.Duration)
	if duration > 0 && position > duration {
		position = duration
	}
	return position
}

func (engine *Engine) startResolve(ctx context.Context, track NowPlaying, key string, startedAt time.Time) <-chan struct{} {
	done := make(chan struct{})
	resolveContext, stop := context.WithCancel(ctx)
	engine.mu.Lock()
	if !engine.enabled || engine.trackKey != key {
		engine.mu.Unlock()
		stop()
		close(done)
		return done
	}
	previousStop := engine.resolveStop
	engine.resolveVersion++
	version := engine.resolveVersion
	engine.resolveStop = stop
	engine.lastResolve = startedAt
	engine.mu.Unlock()
	if previousStop != nil {
		previousStop()
	}

	go func() {
		defer close(done)
		document, provider, resolveErr := engine.resolver.Resolve(resolveContext, track)
		stop()
		engine.mu.Lock()
		if !engine.enabled || version != engine.resolveVersion || engine.trackKey != key {
			engine.mu.Unlock()
			return
		}
		engine.resolveStop = nil
		engine.resolveError = ""
		engine.status.Provider = provider
		engine.status.HasLyrics = resolveErr == nil && document.HasContent()
		engine.status.LyricMode = document.Mode()
		engine.status.Scrollable = document.Scrollable()
		engine.status.LastError = ""
		engine.status.LastUpdate = time.Now()
		if resolveErr != nil {
			engine.resolveError = resolveErr.Error()
			engine.status.LastError = engine.resolveError
			document = Document{}
		}
		engine.document = document
		engine.markDeliveryChangedLocked(time.Now())
		engine.mu.Unlock()
		engine.rescheduleDelivery()
		engine.notifyUpdate()
	}()
	return done
}

// markDeliveryChangedLocked 比较真正会发给副屏的显示状态。切句由主机
// 本地截止时间计时器唤醒，SMTC 轮询只负责校正播放锚点；相同状态不会
// 唤醒分发服务。
func (engine *Engine) markDeliveryChangedLocked(now time.Time) bool {
	state := lyricStateAt(
		engine.document,
		engine.anchorPosition,
		engine.anchorTime,
		engine.status.Track.PositionKnown,
		engine.status.Track.Playing,
		now,
	)
	delivery := lyricDeliveryState{
		Current:        state.Current,
		Next:           state.Next,
		CurrentAt:      state.CurrentAt,
		NextAt:         state.NextAt,
		CurrentAtKnown: engine.document.Scrollable() && state.PositionKnown && state.Current != "",
		NextAtKnown:    engine.document.Scrollable() && state.PositionKnown && state.Next != "",
		Playing:        state.Playing,
	}
	if delivery == engine.lastDelivery {
		return false
	}
	engine.lastDelivery = delivery
	return true
}

func sameTrackWithoutID(left, right NowPlaying) bool {
	if normalizeText(left.Source) != normalizeText(right.Source) ||
		normalizeText(left.Title) != normalizeText(right.Title) ||
		normalizeText(left.Artist) != normalizeText(right.Artist) {
		return false
	}
	leftAlbum, rightAlbum := normalizeText(left.Album), normalizeText(right.Album)
	if (leftAlbum != "" || rightAlbum != "") && leftAlbum != rightAlbum {
		return false
	}
	// 当前轮询没有 ID、专辑和时长时，仅凭同名同歌手不足以证明仍是同一版本。
	// 这种情况下让解析器重新核对本地带身份缓存，也不能沿用上一首的 ID。
	if strings.TrimSpace(left.TrackID) == "" && strings.TrimSpace(right.TrackID) != "" &&
		leftAlbum == "" && rightAlbum == "" && left.Duration <= 0 && right.Duration <= 0 {
		return false
	}
	if left.Duration > 0 || right.Duration > 0 {
		if left.Duration <= 0 || right.Duration <= 0 {
			return false
		}
		difference := left.Duration - right.Duration
		if difference < 0 {
			difference = -difference
		}
		if difference > int64(2*time.Second) {
			return false
		}
	}
	return true
}

func (engine *Engine) setRunning(running bool) {
	engine.mu.Lock()
	engine.status.Running = running
	engine.mu.Unlock()
}
