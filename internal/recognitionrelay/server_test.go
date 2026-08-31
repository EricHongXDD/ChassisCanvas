package recognitionrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"casescreen/internal/edgerecognition"
)

type providerFunc func(context.Context, edgerecognition.Submission) (edgerecognition.Response, error)

func (function providerFunc) Recognize(ctx context.Context, submission edgerecognition.Submission) (edgerecognition.Response, error) {
	return function(ctx, submission)
}

type mapperFunc func(context.Context, edgerecognition.Result, string) (NativeIdentity, bool, error)

func (function mapperFunc) Resolve(ctx context.Context, result edgerecognition.Result, preferred string) (NativeIdentity, bool, error) {
	return function(ctx, result, preferred)
}

func TestServerIsCompatibleWithScreenHTTPProviderAndAppliesOnlyExplicitMapping(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	pcm := make([]byte, 5*edgerecognition.SampleRateHz*edgerecognition.BytesPerSample)
	digest := sha256.Sum256(pcm)
	stableID := "deterministic:sha256:" + hex.EncodeToString(digest[:])
	mapper, err := NewStaticMapper(StaticMappingFile{Version: 1, Records: []StaticMappingRecord{{
		StableID: stableID, QQTrackID: "0039MnYb0qxYhV",
	}}}, "none")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Tokens: testTokenVerifier(t), Provider: DeterministicProvider{}, Mapper: mapper,
		Now: func() time.Time { return now }, RequestsPerSecond: 10, Burst: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := edgerecognition.NewHTTPProvider(edgerecognition.HTTPConfig{
		Endpoint: httpServer.URL + RecognizePath, Token: testDeviceToken, Client: httpServer.Client(),
		RequestTimeout: time.Second, AllowInsecureHTTP: true, PreferredPlatform: "qq",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Recognize(context.Background(), edgerecognition.Submission{
		PCM: pcm, SampledAt: now, Duration: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Matched || response.Result.StableID != stableID || response.Result.Source != "qq" ||
		response.Result.TrackID != "0039MnYb0qxYhV" || !response.Result.NativeIDVerified || response.Result.SampledAt != now {
		t.Fatalf("屏端与中继协议不兼容: %#v", response)
	}
}

func TestServerStripsProviderClaimedNativeIdentityWithoutMapper(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	provider := providerFunc(func(_ context.Context, submission edgerecognition.Submission) (edgerecognition.Response, error) {
		return edgerecognition.Response{Matched: true, Result: edgerecognition.Result{
			StableID: "provider:recording", Title: "Song", Artist: "Artist", Confidence: .9,
			Source: "qq", TrackID: "0039MnYb0qxYhV", NativeIDVerified: true,
		}}, nil
	})
	server, err := New(Config{Tokens: testTokenVerifier(t), Provider: provider, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	response := performRecognition(t, server, now, 5, testDeviceToken)
	if !response.Matched || response.Result.Source != "" || response.Result.TrackID != "" || response.Result.NativeIDVerified {
		t.Fatalf("provider 不应绕过显式映射: %#v", response.Result)
	}
}

func TestServerNormalizesValidatedMapperIdentityBeforeSending(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	provider := providerFunc(func(_ context.Context, submission edgerecognition.Submission) (edgerecognition.Response, error) {
		return edgerecognition.Response{Matched: true, Result: edgerecognition.Result{
			StableID: "provider:recording", Title: "Song", Artist: "Artist", Confidence: .9,
		}}, nil
	})
	mapper := mapperFunc(func(context.Context, edgerecognition.Result, string) (NativeIdentity, bool, error) {
		return NativeIdentity{Source: " QQ ", TrackID: " 0039MnYb0qxYhV "}, true, nil
	})
	server, err := New(Config{
		Tokens: testTokenVerifier(t), Provider: provider, Mapper: mapper, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	response := performRecognition(t, server, now, 5, testDeviceToken)
	if response.Result.Source != "qq" || response.Result.TrackID != "0039MnYb0qxYhV" ||
		!response.Result.NativeIDVerified {
		t.Fatalf("中继必须输出规范化的已验证原生 ID: %#v", response.Result)
	}
}

func TestServerRejectsUnknownFieldsAndOversizedBodies(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	server, err := New(Config{
		Tokens: testTokenVerifier(t), Provider: DeterministicProvider{}, Now: func() time.Time { return now },
		MaximumRequestBytes: 128 << 10, RequestsPerSecond: 10, Burst: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	unknown := `{"protocol_version":1,"unexpected":true}`
	request := authenticatedRequest(t, strings.NewReader(unknown), testDeviceToken)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未知字段状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	large := strings.Repeat("x", (128<<10)+1)
	request = authenticatedRequest(t, strings.NewReader(large), testDeviceToken)
	request.ContentLength = int64(len(large))
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestServerAuthenticationAndPerDeviceRateLimit(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	server, err := New(Config{
		Tokens: testTokenVerifier(t), Provider: DeterministicProvider{}, Now: func() time.Time { return now },
		RequestsPerSecond: .1, Burst: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := authenticatedRequest(t, bytes.NewReader([]byte(`{}`)), strings.Repeat("1", 64))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("错误令牌状态码=%d", recorder.Code)
	}
	_ = performRecognition(t, server, now, 5, testDeviceToken)
	request = validRecognitionRequest(t, now, 5, testDeviceToken)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("限流状态码=%d retry=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
}

func TestServerConvertsTemporaryProviderFailureWithoutLeakingDetails(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	provider := providerFunc(func(context.Context, edgerecognition.Submission) (edgerecognition.Response, error) {
		return edgerecognition.Response{}, temporaryError{message: "upstream secret response", after: 37 * time.Second}
	})
	server, err := New(Config{Tokens: testTokenVerifier(t), Provider: provider, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := validRecognitionRequest(t, now, 5, testDeviceToken)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "37" ||
		strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("临时失败处理错误: status=%d retry=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
}

func TestServerEnforcesTimeoutForProviderThatIgnoresContext(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	release := make(chan struct{})
	provider := providerFunc(func(context.Context, edgerecognition.Submission) (edgerecognition.Response, error) {
		<-release
		return edgerecognition.Response{}, nil
	})
	server, err := New(Config{
		Tokens: testTokenVerifier(t), Provider: provider, Now: func() time.Time { return now },
		ProviderTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := validRecognitionRequest(t, now, 5, testDeviceToken)
	recorder := httptest.NewRecorder()
	started := time.Now()
	server.Handler().ServeHTTP(recorder, request)
	elapsed := time.Since(started)
	close(release)
	if recorder.Code != http.StatusGatewayTimeout || elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("未严格执行 provider 超时: status=%d elapsed=%s", recorder.Code, elapsed)
	}
}

func performRecognition(t *testing.T, server *Server, sampledAt time.Time, seconds int, token string) recognitionResponse {
	t.Helper()
	request := validRecognitionRequest(t, sampledAt, seconds, token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("识歌请求状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response recognitionResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func validRecognitionRequest(t *testing.T, sampledAt time.Time, seconds int, token string) *http.Request {
	t.Helper()
	pcm := make([]byte, seconds*edgerecognition.SampleRateHz*edgerecognition.BytesPerSample)
	payload := recognitionRequest{
		ProtocolVersion: 1, SampledAtUnixMS: sampledAt.UnixMilli(), PreferredPlatform: "auto",
		AudioFormat: edgerecognition.AudioFormat, SampleRateHz: edgerecognition.SampleRateHz,
		Channels: 1, SampleCount: seconds * edgerecognition.SampleRateHz, PCM: pcm,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return authenticatedRequest(t, bytes.NewReader(data), token)
}

func authenticatedRequest(t *testing.T, body io.Reader, token string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, RecognizePath, body)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("X-CaseScreen-Protocol", "recognition-v1")
	return request
}

func TestProviderErrorClassificationRecognizesNoMatch(t *testing.T) {
	if !errors.Is(ErrNoMatch, edgerecognition.ErrNoMatch) {
		t.Fatal("ErrNoMatch 应与屏端共享语义")
	}
}
