package edgerecognition

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"casescreen/internal/edgesync"
	"casescreen/internal/lyrics"
	"casescreen/internal/platformid"
)

const (
	defaultChunkDuration      = time.Second
	defaultWindowDuration     = 10 * time.Second
	defaultSearchInterval     = 2 * time.Second
	defaultSteadyInterval     = 15 * time.Second
	defaultCandidateTTL       = 45 * time.Second
	defaultMinimumTrackAge    = 5 * time.Second
	defaultBackoffMin         = 10 * time.Second
	defaultBackoffMax         = 5 * time.Minute
	defaultRecognitionTimeout = 12 * time.Second
	defaultMinimumConfidence  = 0.78
)

var isrcPattern = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{3}[0-9]{7}$`)

// Config 控制短块采样、10 秒内存环、连续确认、静音暂停与联网退避。
type Config struct {
	Recorder  Recorder
	Extractor FingerprintExtractor
	Provider  Provider

	ChunkDuration      time.Duration
	WindowDuration     time.Duration
	SearchInterval     time.Duration
	SteadyInterval     time.Duration
	CandidateTTL       time.Duration
	MinimumTrackAge    time.Duration
	BackoffMin         time.Duration
	BackoffMax         time.Duration
	RecognitionTimeout time.Duration
	MinimumConfidence  float64
	Confirmations      int
	PauseAfterSilent   int
	ResumeAfterActive  int
	PreferredPlatform  string
	VAD                VADConfig
	Now                func() time.Time
	ErrorSink          func(string)
}

type acceptedResult struct {
	result         Result
	identity       string
	playing        bool
	anchorPosition time.Duration
	anchorTime     time.Time
	acceptedAt     time.Time
}

type pendingResult struct {
	result    Result
	identity  string
	count     int
	firstSeen time.Time
	lastSeen  time.Time
}

type recognitionOutcome struct {
	response   Response
	err        error
	capture    Capture
	generation uint64
}

// Client 实现 edge.MetadataClient。每次 Current 只录一个 1 秒块；达到 10 秒
// 窗口后才异步识别，HTTP 在途期间后续短块仍可执行静音暂停判断。
type Client struct {
	recorder  Recorder
	extractor FingerprintExtractor
	provider  Provider
	config    Config
	now       func() time.Time

	mu              sync.Mutex
	current         *acceptedResult
	pending         *pendingResult
	ring            []byte
	ringSampledAt   time.Time
	nextRecognition time.Time
	silentCount     int
	activeCount     int
	backoff         time.Duration
	inFlight        bool
	inFlightCancel  context.CancelFunc
	audioGeneration uint64
	outcomes        chan recognitionOutcome
}

// NewClient 创建识歌状态机。
func NewClient(config Config) (*Client, error) {
	if config.Recorder == nil || config.Provider == nil {
		return nil, errors.New("识歌录音器和提供方不能为空")
	}
	if config.ChunkDuration == 0 {
		config.ChunkDuration = defaultChunkDuration
	}
	if config.WindowDuration == 0 {
		config.WindowDuration = defaultWindowDuration
	}
	if config.SearchInterval == 0 {
		config.SearchInterval = defaultSearchInterval
	}
	if config.SteadyInterval == 0 {
		config.SteadyInterval = defaultSteadyInterval
	}
	if config.CandidateTTL == 0 {
		config.CandidateTTL = defaultCandidateTTL
	}
	if config.MinimumTrackAge == 0 {
		config.MinimumTrackAge = defaultMinimumTrackAge
	}
	if config.BackoffMin == 0 {
		config.BackoffMin = defaultBackoffMin
	}
	if config.BackoffMax == 0 {
		config.BackoffMax = defaultBackoffMax
	}
	if config.RecognitionTimeout == 0 {
		config.RecognitionTimeout = defaultRecognitionTimeout
	}
	if config.MinimumConfidence == 0 {
		config.MinimumConfidence = defaultMinimumConfidence
	}
	if config.Confirmations == 0 {
		config.Confirmations = 2
	}
	if config.PauseAfterSilent == 0 {
		config.PauseAfterSilent = 2
	}
	if config.ResumeAfterActive == 0 {
		config.ResumeAfterActive = 2
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ChunkDuration < time.Second || config.ChunkDuration > 2*time.Second || config.ChunkDuration%time.Second != 0 {
		return nil, errors.New("识歌短块必须是 1..2 秒内的整秒数")
	}
	if config.WindowDuration < 5*time.Second || config.WindowDuration > 30*time.Second ||
		config.WindowDuration%config.ChunkDuration != 0 {
		return nil, errors.New("识歌内存窗口必须是 5..30 秒且为短块整数倍")
	}
	for _, value := range []time.Duration{config.SearchInterval, config.SteadyInterval} {
		if value < 250*time.Millisecond || value > 24*time.Hour {
			return nil, errors.New("识歌请求间隔必须在 250 毫秒..24 小时")
		}
	}
	if config.CandidateTTL < config.SearchInterval || config.CandidateTTL > 24*time.Hour ||
		config.MinimumTrackAge < 0 || config.MinimumTrackAge > time.Hour {
		return nil, errors.New("识歌候选或切歌抑制时间无效")
	}
	if config.BackoffMin < time.Second || config.BackoffMax < config.BackoffMin || config.BackoffMax > 24*time.Hour {
		return nil, errors.New("识歌失败退避参数无效")
	}
	if config.RecognitionTimeout < time.Second || config.RecognitionTimeout > 30*time.Second {
		return nil, errors.New("识歌异步请求超时必须在 1..30 秒")
	}
	if config.MinimumConfidence <= 0 || config.MinimumConfidence > 1 {
		return nil, errors.New("识歌最低置信度必须位于 0..1")
	}
	if config.Confirmations < 1 || config.Confirmations > 5 || config.PauseAfterSilent < 1 || config.PauseAfterSilent > 10 ||
		config.ResumeAfterActive < 1 || config.ResumeAfterActive > 10 {
		return nil, errors.New("识歌确认次数或静音迟滞次数无效")
	}
	preferred, err := normalizePreferredPlatform(config.PreferredPlatform)
	if err != nil {
		return nil, err
	}
	config.PreferredPlatform = preferred
	if _, err := normalizeVADConfig(config.VAD); err != nil {
		return nil, err
	}
	return &Client{
		recorder: config.Recorder, extractor: config.Extractor, provider: config.Provider,
		config: config, now: config.Now, outcomes: make(chan recognitionOutcome, 1),
	}, nil
}

// Current 录制一个短块并更新内存环。原始短块在函数结束前清零；识别副本由
// 后台请求持有并在请求结束后清零，不会写入 TF 卡。
func (client *Client) Current(ctx context.Context) (edgesync.Envelope, error) {
	if client == nil {
		return edgesync.Envelope{}, errors.New("识歌状态机尚未初始化")
	}
	if ctx == nil {
		return edgesync.Envelope{}, errors.New("识歌上下文不能为空")
	}
	client.mu.Lock()
	client.consumeOutcomeLocked(client.now())
	client.mu.Unlock()

	capture, err := client.recorder.Capture(ctx, client.config.ChunkDuration)
	if err != nil {
		return client.snapshotWithError(err)
	}
	defer clear(capture.PCM)
	activity, err := AnalyzePCM(capture.PCM, client.config.VAD)
	if err != nil {
		return client.snapshotWithError(err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	now := client.now()
	client.consumeOutcomeLocked(now)
	client.appendRingLocked(capture)
	if !activity.Active {
		client.silentCount++
		client.activeCount = 0
		client.pending = nil
		if client.silentCount >= client.config.PauseAfterSilent {
			if client.current != nil && client.current.playing {
				client.pauseLocked(now)
			}
			// 暂停前的旧歌不能留在下一次识歌窗口中。
			client.clearRingLocked()
		}
	} else {
		client.silentCount = 0
		client.activeCount++
		if client.current != nil && !client.current.playing && client.pending == nil &&
			client.activeCount >= client.config.ResumeAfterActive {
			client.resumeLocked(now)
		}
	}

	if !client.inFlight && !now.Before(client.nextRecognition) && client.ringFullLocked() {
		windowActivity, vadErr := AnalyzePCM(client.ring, client.config.VAD)
		if vadErr == nil && windowActivity.Active {
			client.startRecognitionLocked(ctx)
		}
	}
	return client.envelopeLocked(now), nil
}

func (client *Client) snapshotWithError(err error) (edgesync.Envelope, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.config.ErrorSink != nil {
		client.config.ErrorSink(err.Error())
	}
	return client.envelopeLocked(client.now()), err
}

func (client *Client) appendRingLocked(capture Capture) {
	maximum := int(client.config.WindowDuration/time.Second) * SampleRateHz * ChannelCount * BytesPerSample
	chunk := capture.PCM
	if len(chunk) > maximum {
		chunk = chunk[len(chunk)-maximum:]
	}
	client.ring = append(client.ring, chunk...)
	if excess := len(client.ring) - maximum; excess > 0 {
		copy(client.ring, client.ring[excess:])
		clear(client.ring[len(client.ring)-excess:])
		client.ring = client.ring[:maximum]
	}
	client.ringSampledAt = capture.SampledAt
}

func (client *Client) ringFullLocked() bool {
	want := int(client.config.WindowDuration/time.Second) * SampleRateHz * ChannelCount * BytesPerSample
	return len(client.ring) == want
}

func (client *Client) clearRingLocked() {
	clear(client.ring)
	client.ring = client.ring[:0]
	client.ringSampledAt = time.Time{}
	client.audioGeneration++
	if client.inFlightCancel != nil {
		client.inFlightCancel()
	}
}

func (client *Client) startRecognitionLocked(parent context.Context) {
	pcm := append([]byte(nil), client.ring...)
	capture := Capture{PCM: pcm, SampledAt: client.ringSampledAt, Duration: client.config.WindowDuration}
	client.inFlight = true
	client.nextRecognition = client.now().Add(client.config.SearchInterval)
	generation := client.audioGeneration
	extractor := client.extractor
	provider := client.provider
	timeout := client.config.RecognitionTimeout
	outcomes := client.outcomes
	requestContext, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	client.inFlightCancel = cancel
	go func() {
		defer clear(pcm)
		defer cancel()
		submission := Submission{SampledAt: capture.SampledAt, Duration: capture.Duration}
		if extractor != nil {
			fingerprint, err := extractor.Extract(requestContext, capture)
			if err != nil {
				outcomes <- recognitionOutcome{err: err, capture: Capture{SampledAt: capture.SampledAt, Duration: capture.Duration}, generation: generation}
				return
			}
			defer clear(fingerprint.Data)
			submission.Fingerprint = &fingerprint
		} else {
			submission.PCM = pcm
		}
		response, err := provider.Recognize(requestContext, submission)
		outcomes <- recognitionOutcome{
			response: response, err: err,
			capture:    Capture{SampledAt: capture.SampledAt, Duration: capture.Duration},
			generation: generation,
		}
	}()
}

func (client *Client) consumeOutcomeLocked(now time.Time) {
	select {
	case outcome := <-client.outcomes:
		client.inFlight = false
		client.inFlightCancel = nil
		if outcome.generation != client.audioGeneration {
			client.nextRecognition = now
			return
		}
		if outcome.err != nil {
			client.scheduleFailureLocked(now, outcome.err)
			if client.config.ErrorSink != nil {
				client.config.ErrorSink(outcome.err.Error())
			}
			return
		}
		client.backoff = 0
		if !outcome.response.Matched {
			client.pending = nil
			client.nextRecognition = nextAfterSample(outcome.capture.SampledAt, now,
				maxDuration(client.config.SearchInterval, outcome.response.RetryAfter))
			return
		}
		result, identity, valid := sanitizeRecognitionResult(
			outcome.response.Result, outcome.capture, now, client.config.PreferredPlatform,
		)
		if !valid || result.Confidence < client.config.MinimumConfidence {
			client.pending = nil
			client.nextRecognition = nextAfterSample(outcome.capture.SampledAt, now,
				maxDuration(client.config.SearchInterval, outcome.response.RetryAfter))
			return
		}
		client.applyCandidateLocked(result, identity, now)
		if client.current != nil && client.current.identity == identity {
			client.nextRecognition = nextAfterSample(outcome.capture.SampledAt, now,
				maxDuration(client.config.SteadyInterval, outcome.response.RetryAfter))
		} else {
			client.nextRecognition = nextAfterSample(outcome.capture.SampledAt, now,
				maxDuration(client.config.SearchInterval, outcome.response.RetryAfter))
		}
	default:
	}
}

func (client *Client) applyCandidateLocked(result Result, identity string, now time.Time) {
	if client.current != nil && client.current.identity == identity {
		acceptedAt := client.current.acceptedAt
		client.acceptLocked(result, identity, now)
		client.current.acceptedAt = acceptedAt
		client.pending = nil
		return
	}
	if client.pending == nil || client.pending.identity != identity || now.Sub(client.pending.lastSeen) > client.config.CandidateTTL {
		client.pending = &pendingResult{result: result, identity: identity, count: 1, firstSeen: now, lastSeen: now}
		if client.config.Confirmations == 1 &&
			(client.current == nil || now.Sub(client.current.acceptedAt) >= client.config.MinimumTrackAge) {
			client.acceptLocked(result, identity, now)
			client.pending = nil
			return
		}
		// 第一份不同歌曲证据先冻结旧歌词；第二份连续证据才正式切歌。
		if client.current != nil && client.current.playing {
			client.pauseLocked(now)
		}
		return
	}
	client.pending.result = result
	client.pending.count++
	client.pending.lastSeen = now
	if client.pending.count < client.config.Confirmations {
		return
	}
	if client.current != nil && now.Sub(client.current.acceptedAt) < client.config.MinimumTrackAge {
		return
	}
	client.acceptLocked(result, identity, now)
	client.pending = nil
}

func (client *Client) acceptLocked(result Result, identity string, now time.Time) {
	if result.SampledAt.IsZero() {
		result.SampledAt = now
	}
	playing := client.silentCount < client.config.PauseAfterSilent
	position := result.PlaybackOffset
	anchorTime := result.SampledAt
	if !playing && now.After(anchorTime) {
		position += now.Sub(anchorTime)
		anchorTime = now
	}
	if result.Duration > 0 && position > result.Duration {
		position = result.Duration
	}
	client.current = &acceptedResult{
		result: result, identity: identity, playing: playing,
		anchorPosition: position, anchorTime: anchorTime, acceptedAt: now,
	}
}

func (client *Client) pauseLocked(now time.Time) {
	if client.current == nil || !client.current.playing {
		return
	}
	client.current.anchorPosition = client.positionLocked(now)
	client.current.anchorTime = now
	client.current.playing = false
}

func (client *Client) resumeLocked(now time.Time) {
	if client.current == nil || client.current.playing {
		return
	}
	client.current.anchorTime = now
	client.current.playing = true
}

func (client *Client) scheduleFailureLocked(now time.Time, err error) {
	delay := client.config.BackoffMin
	if client.backoff > 0 {
		delay = growDuration(client.backoff, client.config.BackoffMax)
	}
	var retryError RetryAfterError
	if errors.As(err, &retryError) && retryError.RetryAfter() > delay {
		delay = retryError.RetryAfter()
		if delay > client.config.BackoffMax {
			delay = client.config.BackoffMax
		}
	}
	client.backoff = delay
	client.nextRecognition = now.Add(delay)
}

func (client *Client) envelopeLocked(now time.Time) edgesync.Envelope {
	envelope := edgesync.Envelope{
		ProtocolVersion: edgesync.ProtocolVersion,
		SampledAtUnixMS: now.UnixMilli(),
		Metadata:        edgesync.Metadata{LyricMode: edgesync.LyricModeUnknown},
	}
	if client.current == nil {
		return envelope
	}
	result := client.current.result
	position := client.positionLocked(now)
	source := "recognition"
	trackID := ""
	if result.NativeIDVerified {
		source = result.Source
		trackID = result.TrackID
	}
	envelope.Available = true
	envelope.Track = lyrics.NowPlaying{
		Source: source, TrackID: trackID, TrackIDOrigin: lyrics.TrackIDOriginSource,
		Title: result.Title, Artist: result.Artist, Album: result.Album,
		Position: int64(position), Duration: int64(result.Duration), PositionKnown: true,
		Playing: client.current.playing,
	}
	envelope.Metadata = edgesync.Metadata{
		Source: source, TrackID: trackID, LyricMode: edgesync.LyricModeUnknown,
		StableID: result.StableID, ISRC: result.ISRC, Confidence: result.Confidence,
		RecognitionSampledAtUnixMS: result.SampledAt.UnixMilli(),
	}
	return envelope
}

func (client *Client) positionLocked(now time.Time) time.Duration {
	if client.current == nil {
		return 0
	}
	position := client.current.anchorPosition
	if client.current.playing && now.After(client.current.anchorTime) {
		position += now.Sub(client.current.anchorTime)
	}
	if position < 0 {
		position = 0
	}
	if client.current.result.Duration > 0 && position > client.current.result.Duration {
		position = client.current.result.Duration
	}
	return position
}

func sanitizeRecognitionResult(input Result, capture Capture, now time.Time, preferredPlatform string) (Result, string, bool) {
	result := trimResult(input)
	if len(result.StableID) > 256 || len(result.Title) > 512 || len(result.Artist) > 512 || len(result.Album) > 512 {
		return Result{}, "", false
	}
	if result.ISRC != "" && !isrcPattern.MatchString(result.ISRC) {
		result.ISRC = ""
	}
	if result.StableID == "" && result.ISRC != "" {
		result.StableID = "isrc:" + result.ISRC
	}
	if result.StableID == "" || !safeStableID(result.StableID) || result.Title == "" || result.Artist == "" ||
		result.Confidence < 0 || result.Confidence > 1 || result.PlaybackOffset < 0 || result.Duration < 0 {
		return Result{}, "", false
	}
	if result.Duration > 0 && result.PlaybackOffset > result.Duration {
		result.PlaybackOffset = result.Duration
	}
	if !result.SampledAt.IsZero() && !result.SampledAt.Before(capture.SampledAt.Add(-30*time.Second)) &&
		!result.SampledAt.After(now.Add(30*time.Second)) {
		// HTTP JSON 时间戳不含 Go 单调时钟。先把 relay 偏移校正到本机采样
		// 时刻，再固定使用 capture.SampledAt，避免系统校时让歌词突然跳动。
		result.PlaybackOffset += capture.SampledAt.Sub(result.SampledAt)
		if result.PlaybackOffset < 0 {
			result.PlaybackOffset = 0
		}
	}
	result.SampledAt = capture.SampledAt
	if result.Duration > 0 && result.PlaybackOffset > result.Duration {
		result.PlaybackOffset = result.Duration
	}
	result.Source, result.TrackID, result.NativeIDVerified = verifiedNativeIdentity(result, preferredPlatform)
	return result, result.StableID, true
}

func verifiedNativeIdentity(result Result, preferredPlatform string) (string, string, bool) {
	if !result.NativeIDVerified {
		return "", "", false
	}
	source := strings.ToLower(strings.TrimSpace(result.Source))
	trackID := strings.TrimSpace(result.TrackID)
	if preferredPlatform != "auto" && source != preferredPlatform {
		return "", "", false
	}
	switch source {
	case "qq":
		if platformid.QQ(trackID) {
			return source, trackID, true
		}
	case "netease":
		if platformid.Netease(trackID) {
			return source, trackID, true
		}
	}
	return "", "", false
}

func safeStableID(value string) bool {
	for _, character := range value {
		if character < 0x21 || character == 0x7f {
			return false
		}
	}
	return true
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func nextAfterSample(sampledAt, now time.Time, delay time.Duration) time.Time {
	target := sampledAt.Add(delay)
	if target.Before(now) {
		return now
	}
	return target
}

func growDuration(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}
