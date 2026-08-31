package recognitionrelay

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"casescreen/internal/edgerecognition"
)

// e2eClock 同时被屏端状态机和 HTTP 中继协程读取，因此测试时也保留并发安全。
type e2eClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *e2eClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *e2eClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

type e2eRecorder struct {
	clock  *e2eClock
	silent atomic.Bool
}

func (recorder *e2eRecorder) Capture(_ context.Context, duration time.Duration) (edgerecognition.Capture, error) {
	samples := int(duration * edgerecognition.SampleRateHz / time.Second)
	pcm := make([]byte, samples*edgerecognition.BytesPerSample)
	if !recorder.silent.Load() {
		for index := 0; index < samples; index++ {
			value := int16(math.Sin(2*math.Pi*440*float64(index)/edgerecognition.SampleRateHz) * 9000)
			binary.LittleEndian.PutUint16(pcm[index*2:], uint16(value))
		}
	}
	return edgerecognition.Capture{PCM: pcm, SampledAt: recorder.clock.Now(), Duration: duration}, nil
}

type e2eFingerprintExtractor struct {
	data  []byte
	calls atomic.Int32
}

func (extractor *e2eFingerprintExtractor) Extract(_ context.Context, _ edgerecognition.Capture) (edgerecognition.Fingerprint, error) {
	extractor.calls.Add(1)
	return edgerecognition.Fingerprint{
		Algorithm: "acrcloud-audio-fingerprint", Version: "1", Data: append([]byte(nil), extractor.data...),
	}, nil
}

type observedDeterministicProvider struct {
	calls    atomic.Int32
	returned chan struct{}
}

func (provider *observedDeterministicProvider) Recognize(ctx context.Context, submission edgerecognition.Submission) (edgerecognition.Response, error) {
	response, err := (DeterministicProvider{}).Recognize(ctx, submission)
	provider.calls.Add(1)
	select {
	case provider.returned <- struct{}{}:
	default:
	}
	return response, err
}

func TestRecognitionV1DeterministicEndToEndPCMAndFingerprint(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	server, err := New(Config{
		Tokens: testTokenVerifier(t), Provider: DeterministicProvider{}, Now: func() time.Time { return now },
		RequestsPerSecond: 10, Burst: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	provider, err := edgerecognition.NewHTTPProvider(edgerecognition.HTTPConfig{
		Endpoint: httpServer.URL + RecognizePath, Token: testDeviceToken, Client: httpServer.Client(),
		RequestTimeout: 2 * time.Second, AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 5*edgerecognition.SampleRateHz*edgerecognition.BytesPerSample)
	for index := 0; index < len(pcm)/2; index++ {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(int16(index%2048-1024)))
	}
	fingerprint := []byte("casescreen-deterministic-fingerprint-v1")
	tests := []struct {
		name       string
		submission edgerecognition.Submission
		digestData []byte
	}{
		{
			name: "pcm", submission: edgerecognition.Submission{
				SampledAt: now, Duration: 5 * time.Second, PCM: pcm,
			}, digestData: pcm,
		},
		{
			name: "fingerprint", submission: edgerecognition.Submission{
				SampledAt: now, Duration: 5 * time.Second,
				Fingerprint: &edgerecognition.Fingerprint{
					Algorithm: "acrcloud-audio-fingerprint", Version: "1", Data: fingerprint,
				},
			}, digestData: fingerprint,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := provider.Recognize(context.Background(), test.submission)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(test.digestData)
			wantStableID := "deterministic:sha256:" + hex.EncodeToString(digest[:])
			if !response.Matched || response.Result.StableID != wantStableID ||
				response.Result.Title != "Deterministic test sample" || response.Result.Artist != "CaseScreen" ||
				response.Result.Duration != 5*time.Minute || response.Result.PlaybackOffset != 5*time.Second ||
				response.Result.Confidence != 1 || !response.Result.SampledAt.Equal(now) {
				t.Fatalf("%s 请求或响应契约不兼容: %#v", test.name, response)
			}
		})
	}

	badProvider, err := edgerecognition.NewHTTPProvider(edgerecognition.HTTPConfig{
		Endpoint: httpServer.URL + RecognizePath, Token: strings.Repeat("1", 64), Client: httpServer.Client(),
		RequestTimeout: 2 * time.Second, AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badProvider.Recognize(context.Background(), tests[0].submission); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("Bearer 令牌未被中继端到端验证: %v", err)
	}
}

func TestRecognitionV1FullClientDoubleConfirmationAndMonotonicTimeline(t *testing.T) {
	clock := &e2eClock{now: time.UnixMilli(1_800_000_000_000)}
	recorder := &e2eRecorder{clock: clock}
	fingerprintData := []byte("same-compact-fingerprint-for-two-confirmations")
	extractor := &e2eFingerprintExtractor{data: fingerprintData}
	digest := sha256.Sum256(fingerprintData)
	stableID := "deterministic:sha256:" + hex.EncodeToString(digest[:])
	mapper, err := NewStaticMapper(StaticMappingFile{Version: 1, Records: []StaticMappingRecord{{
		StableID: stableID, QQTrackID: "0039MnYb0qxYhV",
	}}}, "none")
	if err != nil {
		t.Fatal(err)
	}
	upstream := &observedDeterministicProvider{returned: make(chan struct{}, 8)}
	server, err := New(Config{
		Tokens: testTokenVerifier(t), Provider: upstream, Mapper: mapper, Now: clock.Now,
		RequestsPerSecond: 10, Burst: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	httpProvider, err := edgerecognition.NewHTTPProvider(edgerecognition.HTTPConfig{
		Endpoint: httpServer.URL + RecognizePath, Token: testDeviceToken, Client: httpServer.Client(),
		RequestTimeout: 2 * time.Second, AllowInsecureHTTP: true, PreferredPlatform: "qq",
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := edgerecognition.NewClient(edgerecognition.Config{
		Recorder: recorder, Extractor: extractor, Provider: httpProvider, Now: clock.Now,
		ChunkDuration: time.Second, WindowDuration: 5 * time.Second,
		SearchInterval: 250 * time.Millisecond, SteadyInterval: 30 * time.Second,
		CandidateTTL: 10 * time.Second, MinimumTrackAge: time.Nanosecond,
		BackoffMin: time.Second, BackoffMax: 10 * time.Second,
		RecognitionTimeout: 2 * time.Second, MinimumConfidence: .7,
		Confirmations: 2, PauseAfterSilent: 2, ResumeAfterActive: 2, PreferredPlatform: "qq",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for index := 0; index < 5; index++ {
		envelope, currentErr := client.Current(ctx)
		if currentErr != nil {
			t.Fatal(currentErr)
		}
		if envelope.Available {
			t.Fatal("五秒窗口和第一份证据完成前不应输出歌曲")
		}
		clock.Advance(time.Second)
	}

	// 第一次结果只能成为候选；继续调用 Current 同时模拟屏端一秒采样循环。
	waitForProviderCalls(t, upstream, 1)
	var firstCandidateSeen bool
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && upstream.calls.Load() < 2; {
		envelope, currentErr := client.Current(ctx)
		if currentErr != nil {
			t.Fatal(currentErr)
		}
		if envelope.Available {
			t.Fatal("只有一次 deterministic 识别证据时不应通过双确认")
		}
		firstCandidateSeen = true
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	if !firstCandidateSeen {
		t.Fatal("未经过第一份候选状态")
	}
	waitForProviderCalls(t, upstream, 2)

	var acceptedPosition int64
	var accepted bool
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		envelope, currentErr := client.Current(ctx)
		if currentErr != nil {
			t.Fatal(currentErr)
		}
		if envelope.Available {
			if envelope.Metadata.StableID != stableID || envelope.Track.Source != "qq" ||
				envelope.Track.TrackID != "0039MnYb0qxYhV" || !envelope.Track.PositionKnown || !envelope.Track.Playing {
				t.Fatalf("双确认后的精确平台身份或时间线错误: %#v", envelope)
			}
			acceptedPosition = envelope.Track.Position
			accepted = true
			break
		}
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	if !accepted || extractor.calls.Load() < 2 {
		t.Fatalf("未完成两次紧凑声纹确认: provider=%d extractor=%d", upstream.calls.Load(), extractor.calls.Load())
	}

	clock.Advance(3 * time.Second)
	advanced, err := client.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if delta := time.Duration(advanced.Track.Position - acceptedPosition); delta != 3*time.Second {
		t.Fatalf("播放时间线未按屏端单调时钟前进: delta=%s", delta)
	}

	// 连续两个静音块后冻结，再连续两个有声块才恢复，避免歌词自行滚动。
	recorder.silent.Store(true)
	clock.Advance(time.Second)
	firstSilent, err := client.Current(ctx)
	if err != nil || !firstSilent.Track.Playing {
		t.Fatalf("第一个静音块仍应在迟滞窗口内: %#v err=%v", firstSilent, err)
	}
	clock.Advance(time.Second)
	paused, err := client.Current(ctx)
	if err != nil || paused.Track.Playing {
		t.Fatalf("第二个静音块后应冻结时间线: %#v err=%v", paused, err)
	}
	pausedPosition := paused.Track.Position
	clock.Advance(3 * time.Second)
	stillPaused, err := client.Current(ctx)
	if err != nil || stillPaused.Track.Position != pausedPosition {
		t.Fatalf("暂停期间时间线不应前进: before=%d after=%d err=%v", pausedPosition, stillPaused.Track.Position, err)
	}

	recorder.silent.Store(false)
	clock.Advance(time.Second)
	firstActive, err := client.Current(ctx)
	if err != nil || firstActive.Track.Playing {
		t.Fatalf("第一个有声块仍应保持暂停迟滞: %#v err=%v", firstActive, err)
	}
	clock.Advance(time.Second)
	resumed, err := client.Current(ctx)
	if err != nil || !resumed.Track.Playing || resumed.Track.Position != pausedPosition {
		t.Fatalf("第二个有声块后应从冻结位置恢复: %#v err=%v", resumed, err)
	}
	clock.Advance(2 * time.Second)
	playing, err := client.Current(ctx)
	if err != nil || playing.Track.Position-pausedPosition != int64(2*time.Second) {
		t.Fatalf("恢复后时间线未继续前进: %#v err=%v", playing, err)
	}
}

func waitForProviderCalls(t *testing.T, provider *observedDeterministicProvider, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for provider.calls.Load() < want {
		select {
		case <-provider.returned:
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("deterministic provider 调用超时: got=%d want=%d", provider.calls.Load(), want)
		}
	}
}
