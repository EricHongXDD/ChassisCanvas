package edgerecognition

import (
	"context"
	"errors"
	"testing"
	"time"

	"casescreen/internal/edge"
)

type recognitionClock struct{ now time.Time }

func (clock *recognitionClock) Now() time.Time                 { return clock.now }
func (clock *recognitionClock) Advance(duration time.Duration) { clock.now = clock.now.Add(duration) }

type queuedRecorder struct {
	clock   *recognitionClock
	pcm     [][]byte
	calls   int
	lastRaw []byte
}

func (recorder *queuedRecorder) Capture(_ context.Context, duration time.Duration) (Capture, error) {
	recorder.calls++
	if len(recorder.pcm) == 0 {
		return Capture{}, errors.New("测试录音队列为空")
	}
	raw := recorder.pcm[0]
	recorder.pcm = recorder.pcm[1:]
	recorder.lastRaw = raw
	return Capture{PCM: raw, SampledAt: recorder.clock.Now(), Duration: duration}, nil
}

type queuedProvider struct {
	responses       []Response
	errors          []error
	calls           int
	called          chan struct{}
	lastPCMBytes    int
	lastFingerprint int
}

func (provider *queuedProvider) Recognize(_ context.Context, submission Submission) (Response, error) {
	index := provider.calls
	provider.calls++
	provider.lastPCMBytes = len(submission.PCM)
	if submission.Fingerprint != nil {
		provider.lastFingerprint = len(submission.Fingerprint.Data)
	}
	if provider.called != nil {
		provider.called <- struct{}{}
	}
	if index < len(provider.errors) && provider.errors[index] != nil {
		return Response{}, provider.errors[index]
	}
	if index >= len(provider.responses) {
		return Response{}, nil
	}
	return provider.responses[index], nil
}

type fixedExtractor struct {
	calls int
	data  []byte
}

type blockingProvider struct {
	started chan struct{}
	release chan struct{}
}

func (provider *blockingProvider) Recognize(ctx context.Context, _ Submission) (Response, error) {
	close(provider.started)
	select {
	case <-provider.release:
		return Response{}, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

func (extractor *fixedExtractor) Extract(_ context.Context, _ Capture) (Fingerprint, error) {
	extractor.calls++
	return Fingerprint{Algorithm: "vendor-acr", Version: "1", Data: append([]byte(nil), extractor.data...)}, nil
}

func TestClientBuildsTenSecondRingBeforeLowFrequencyRecognition(t *testing.T) {
	clock := &recognitionClock{now: time.Unix(1_700_000_000, 0)}
	recorder := &queuedRecorder{clock: clock, pcm: repeatedPCM(12, activeSecond)}
	match := matched("acr:recording-7", "Song", clock.Now())
	match.Result.ISRC = "USRC17607839"
	match.Result.Source = "qq"
	match.Result.TrackID = "0039MnYb0qxYhV"
	match.Result.NativeIDVerified = false
	provider := &queuedProvider{
		responses: []Response{match, match}, called: make(chan struct{}, 2),
	}
	client := newTestClient(t, clock, recorder, provider, Config{
		WindowDuration: 10 * time.Second, Confirmations: 2,
	})

	for index := 0; index < 9; index++ {
		envelope, err := client.Current(context.Background())
		if err != nil || envelope.Available {
			t.Fatalf("前 9 秒不能识别成功: index=%d envelope=%#v err=%v", index, envelope, err)
		}
		clock.Advance(time.Second)
	}
	if provider.calls != 0 {
		t.Fatalf("10 秒窗口未满时不应联网: calls=%d", provider.calls)
	}
	if _, err := client.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitProviderCall(t, provider.called)
	waitOutcome(t, client)
	if provider.calls != 1 || provider.lastPCMBytes != 320_000 {
		t.Fatalf("PCM 回退应只上传一个 10 秒窗口: calls=%d bytes=%d", provider.calls, provider.lastPCMBytes)
	}

	clock.Advance(2 * time.Second)
	first, err := client.Current(context.Background())
	if err != nil || first.Available {
		t.Fatalf("第一份结果只能成为候选: %#v err=%v", first, err)
	}
	waitProviderCall(t, provider.called)
	waitOutcome(t, client)
	clock.Advance(2 * time.Second)
	second, err := client.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Available || second.Metadata.StableID != "acr:recording-7" || second.Metadata.ISRC != "USRC17607839" {
		t.Fatalf("连续确认后应输出稳定身份: %#v", second)
	}
	if second.Metadata.Source != "recognition" || second.Metadata.TrackID != "" {
		t.Fatalf("未经验证的平台 ID 必须丢弃，不能由标题猜测: %#v", second.Metadata)
	}
	for _, value := range recorder.lastRaw {
		if value != 0 {
			t.Fatal("每个短块在进入内存环后应清零原始缓冲")
		}
	}
}

func TestClientPrefersFingerprintAndOmitsPCMUpload(t *testing.T) {
	clock := &recognitionClock{now: time.Unix(1_700_000_000, 0)}
	recorder := &queuedRecorder{clock: clock, pcm: repeatedPCM(5, activeSecond)}
	provider := &queuedProvider{called: make(chan struct{}, 1)}
	extractor := &fixedExtractor{data: []byte{1, 2, 3, 4}}
	client := newTestClient(t, clock, recorder, provider, Config{
		WindowDuration: 5 * time.Second, Extractor: extractor,
	})
	for index := 0; index < 5; index++ {
		if _, err := client.Current(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Second)
	}
	waitProviderCall(t, provider.called)
	waitOutcome(t, client)
	if extractor.calls != 1 || provider.lastFingerprint != 4 || provider.lastPCMBytes != 0 {
		t.Fatalf("有提取器时必须优先紧凑声纹: extractor=%d fp=%d pcm=%d",
			extractor.calls, provider.lastFingerprint, provider.lastPCMBytes)
	}
}

func TestClientSilencePausesWithinTwoChunksAndRecognitionResumes(t *testing.T) {
	clock := &recognitionClock{now: time.Unix(1_700_000_000, 0)}
	pcm := [][]byte{silentSecond(), silentSecond()}
	pcm = append(pcm, repeatedPCM(6, activeSecond)...)
	recorder := &queuedRecorder{clock: clock, pcm: pcm}
	provider := &queuedProvider{
		responses: []Response{matched("stable:a", "A", clock.Now())},
		called:    make(chan struct{}, 1),
	}
	client := newTestClient(t, clock, recorder, provider, Config{
		WindowDuration: 5 * time.Second, Confirmations: 1, PauseAfterSilent: 2,
	})
	client.current = &acceptedResult{
		result:   Result{StableID: "stable:a", Title: "A", Artist: "Artist", Duration: 3 * time.Minute},
		identity: "stable:a", playing: true, anchorPosition: 20 * time.Second,
		anchorTime: clock.Now(), acceptedAt: clock.Now().Add(-time.Minute),
	}

	firstSilent, err := client.Current(context.Background())
	if err != nil || !firstSilent.Track.Playing {
		t.Fatalf("第一次静音仍在迟滞窗口: %#v err=%v", firstSilent, err)
	}
	clock.Advance(time.Second)
	secondSilent, err := client.Current(context.Background())
	if err != nil || secondSilent.Track.Playing {
		t.Fatalf("第二个 1 秒静音块应冻结歌词: %#v err=%v", secondSilent, err)
	}
	if len(client.ring) != 0 {
		t.Fatalf("暂停时必须清空旧歌 PCM 环，实际仍有 %d 字节", len(client.ring))
	}
	frozenPosition := secondSilent.Track.Position

	for index := 0; index < 5; index++ {
		clock.Advance(time.Second)
		envelope, err := client.Current(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && envelope.Track.Playing {
			t.Fatal("第一个有声块仍应处在恢复迟滞窗口")
		}
		if index == 1 && !envelope.Track.Playing {
			t.Fatal("第二个连续有声块后应先恢复本地歌词时间轴")
		}
	}
	waitProviderCall(t, provider.called)
	waitOutcome(t, client)
	clock.Advance(time.Second)
	resumed, err := client.Current(context.Background())
	if err != nil || !resumed.Track.Playing || resumed.Track.Title != "A" {
		t.Fatalf("同一稳定 ID 再次识别后应恢复滚动: %#v err=%v", resumed, err)
	}
	if frozenPosition <= 0 || provider.calls != 1 {
		t.Fatalf("暂停位置或联网频率错误: pos=%d calls=%d", frozenPosition, provider.calls)
	}
}

func TestClientBackoffStopsNetworkButKeepsOneSecondVAD(t *testing.T) {
	clock := &recognitionClock{now: time.Unix(1_700_000_000, 0)}
	recorder := &queuedRecorder{clock: clock, pcm: repeatedPCM(16, activeSecond)}
	provider := &queuedProvider{
		errors: []error{errors.New("network down")}, called: make(chan struct{}, 1),
	}
	client := newTestClient(t, clock, recorder, provider, Config{
		WindowDuration: 5 * time.Second, BackoffMin: 10 * time.Second, BackoffMax: 20 * time.Second,
	})
	for index := 0; index < 5; index++ {
		if _, err := client.Current(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Second)
	}
	waitProviderCall(t, provider.called)
	waitOutcome(t, client)
	if _, err := client.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 8; index++ {
		clock.Advance(time.Second)
		if _, err := client.Current(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls != 1 || recorder.calls != 14 {
		t.Fatalf("退避只应停止联网，短块 VAD 必须继续: provider=%d recorder=%d", provider.calls, recorder.calls)
	}
}

func TestClientInFlightHTTPDoesNotBlockSilencePause(t *testing.T) {
	clock := &recognitionClock{now: time.Unix(1_700_000_000, 0)}
	pcm := repeatedPCM(5, activeSecond)
	pcm = append(pcm, silentSecond(), silentSecond())
	recorder := &queuedRecorder{clock: clock, pcm: pcm}
	provider := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	client := newTestClient(t, clock, recorder, provider, Config{WindowDuration: 5 * time.Second})
	client.current = &acceptedResult{
		result:   Result{StableID: "stable:a", Title: "A", Artist: "Artist", Duration: 3 * time.Minute},
		identity: "stable:a", playing: true, anchorPosition: 20 * time.Second,
		anchorTime: clock.Now(), acceptedAt: clock.Now().Add(-time.Minute),
	}
	for index := 0; index < 5; index++ {
		if _, err := client.Current(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Second)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("异步 HTTP 未开始")
	}
	for index := 0; index < 2; index++ {
		started := time.Now()
		envelope, err := client.Current(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(started) > 100*time.Millisecond {
			t.Fatal("HTTP 在途时短块 VAD 被阻塞")
		}
		if index == 1 && envelope.Track.Playing {
			t.Fatal("HTTP 在途时仍应在两个静音块后暂停")
		}
		clock.Advance(time.Second)
	}
	close(provider.release)
}

func TestClientDiscardsRecognitionFromAudioBeforePause(t *testing.T) {
	now := time.Now()
	client := &Client{
		config: Config{SearchInterval: time.Second, SteadyInterval: 15 * time.Second, MinimumConfidence: 0.7,
			Confirmations: 1, PauseAfterSilent: 2, ResumeAfterActive: 2, PreferredPlatform: "auto"},
		current: &acceptedResult{
			result:   Result{StableID: "stable:a", Title: "A", Artist: "Artist"},
			identity: "stable:a", playing: false, anchorTime: now, acceptedAt: now,
		},
		audioGeneration: 2, inFlight: true, outcomes: make(chan recognitionOutcome, 1),
	}
	client.outcomes <- recognitionOutcome{
		generation: 1, capture: Capture{SampledAt: now, Duration: 10 * time.Second},
		response: matched("stable:b", "B", now),
	}
	client.consumeOutcomeLocked(now)
	if client.current.identity != "stable:a" || client.current.result.Title != "A" || client.inFlight {
		t.Fatalf("暂停前旧音频的迟到结果必须丢弃: %#v", client.current)
	}
}

func TestClientAcceptsOnlyVerifiedPreferredPlatformID(t *testing.T) {
	capture := Capture{SampledAt: time.Unix(1_700_000_000, 0), Duration: 10 * time.Second}
	input := Result{
		StableID: "relay:qq", Title: "Song", Artist: "Artist", Confidence: 0.99,
		Source: "qq", TrackID: "0039MnYb0qxYhV", NativeIDVerified: true,
	}
	verified, _, ok := sanitizeRecognitionResult(input, capture, capture.SampledAt, "qq")
	if !ok || !verified.NativeIDVerified || verified.TrackID == "" {
		t.Fatalf("匹配偏好平台的强验证 ID 应进入精确取词: %#v", verified)
	}
	rejected, _, ok := sanitizeRecognitionResult(input, capture, capture.SampledAt, "netease")
	if !ok || rejected.NativeIDVerified || rejected.TrackID != "" {
		t.Fatalf("声纹不能猜来源平台，偏好不符时必须丢弃原生 ID: %#v", rejected)
	}
}

func TestVerifiedNativeIDReachesExistingExactLyricsPipeline(t *testing.T) {
	now := time.Now()
	client := &Client{current: &acceptedResult{
		result: Result{
			StableID: "relay:qq", Title: "Song", Artist: "Artist",
			Source: "qq", TrackID: "0039MnYb0qxYhV", NativeIDVerified: true,
			Duration: 3 * time.Minute, SampledAt: now,
		},
		identity: "relay:qq", playing: true, anchorTime: now, acceptedAt: now,
	}}
	envelope := client.envelopeLocked(now)
	metadata, _ := edge.MetadataFromEnvelope(envelope, now)
	if !metadata.Valid() || metadata.Source != "qq" || metadata.TrackID != "0039MnYb0qxYhV" {
		t.Fatalf("已验证原生 ID 没有进入现有 edgelyrics 精确取词入口: %#v", metadata)
	}
}

func TestRecognitionTimestampIsReanchoredToLocalMonotonicCapture(t *testing.T) {
	localCapture := time.Now()
	// JSON 解码后的时间没有单调时钟，并且这里模拟 relay 报告偏移对应采样前 2 秒。
	relayTimestamp := time.UnixMilli(localCapture.Add(-2 * time.Second).UnixMilli())
	result, _, ok := sanitizeRecognitionResult(Result{
		StableID: "stable:a", Title: "A", Artist: "Artist", Confidence: 0.99,
		PlaybackOffset: 30 * time.Second, Duration: 3 * time.Minute, SampledAt: relayTimestamp,
	}, Capture{SampledAt: localCapture, Duration: 10 * time.Second}, localCapture.Add(time.Second), "auto")
	if !ok {
		t.Fatal("有效识别结果不应被拒绝")
	}
	if result.SampledAt != localCapture {
		t.Fatalf("时间轴必须锚定本机单调采样时间: got=%#v want=%#v", result.SampledAt, localCapture)
	}
	if delta := result.PlaybackOffset - 32*time.Second; delta < -time.Millisecond || delta > time.Millisecond {
		t.Fatalf("relay 偏移没有校正到本机采样时刻: %v", result.PlaybackOffset)
	}
}

func newTestClient(t *testing.T, clock *recognitionClock, recorder Recorder, provider Provider, overrides Config) *Client {
	t.Helper()
	config := Config{
		Recorder: recorder, Provider: provider, Now: clock.Now,
		ChunkDuration: time.Second, WindowDuration: 5 * time.Second,
		SearchInterval: time.Second, SteadyInterval: 30 * time.Second,
		CandidateTTL: 30 * time.Second, MinimumTrackAge: time.Nanosecond,
		BackoffMin: time.Second, BackoffMax: 30 * time.Second,
		RecognitionTimeout: time.Second, MinimumConfidence: 0.7,
		Confirmations: 2, PauseAfterSilent: 2, ResumeAfterActive: 2, PreferredPlatform: "auto",
	}
	if overrides.Extractor != nil {
		config.Extractor = overrides.Extractor
	}
	if overrides.WindowDuration != 0 {
		config.WindowDuration = overrides.WindowDuration
	}
	if overrides.Confirmations != 0 {
		config.Confirmations = overrides.Confirmations
	}
	if overrides.PauseAfterSilent != 0 {
		config.PauseAfterSilent = overrides.PauseAfterSilent
	}
	if overrides.MinimumTrackAge != 0 {
		config.MinimumTrackAge = overrides.MinimumTrackAge
	}
	if overrides.BackoffMin != 0 {
		config.BackoffMin = overrides.BackoffMin
	}
	if overrides.BackoffMax != 0 {
		config.BackoffMax = overrides.BackoffMax
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func repeatedPCM(count int, factory func() []byte) [][]byte {
	result := make([][]byte, count)
	for index := range result {
		result[index] = factory()
	}
	return result
}

func waitProviderCall(t *testing.T, called <-chan struct{}) {
	t.Helper()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("异步识歌没有在预期时间开始")
	}
}

func waitOutcome(t *testing.T, client *Client) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(client.outcomes) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("异步识歌结果没有在预期时间返回")
		}
		time.Sleep(time.Millisecond)
	}
}

func activeSecond() []byte { return sinePCM(SampleRateHz, 440, 9000) }
func silentSecond() []byte { return make([]byte, SampleRateHz*BytesPerSample) }

func matched(stableID, title string, sampledAt time.Time) Response {
	return Response{Matched: true, Result: Result{
		StableID: stableID, Title: title, Artist: "Artist", Confidence: 0.99,
		Duration: 3 * time.Minute, PlaybackOffset: 10 * time.Second, SampledAt: sampledAt,
	}}
}
