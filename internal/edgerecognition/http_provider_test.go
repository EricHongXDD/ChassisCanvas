package edgerecognition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const recognitionTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHTTPProviderUsesRevocableTokenAndJSONProtocol(t *testing.T) {
	sampledAt := time.UnixMilli(1_700_000_000_123)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+recognitionTestToken ||
			request.Header.Get("X-CaseScreen-Protocol") != "recognition-v1" {
			t.Errorf("请求头或方法错误: method=%s auth=%q", request.Method, request.Header.Get("Authorization"))
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		var payload recognitionRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("请求 JSON 无效: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.ProtocolVersion != 1 || payload.AudioFormat != AudioFormat || payload.SampleRateHz != SampleRateHz ||
			payload.Channels != 1 || payload.SampleCount != 16000 || len(payload.PCM) != 32000 ||
			payload.SampledAtUnixMS != sampledAt.UnixMilli() || payload.PreferredPlatform != "auto" {
			t.Errorf("识歌请求字段错误: %#v pcm=%d", payload, len(payload.PCM))
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-CaseScreen-Protocol", "recognition-v1")
		_ = json.NewEncoder(writer).Encode(recognitionResponse{
			ProtocolVersion: 1, Matched: true,
			Result: recognitionResult{
				StableID: "relay:track-42", ISRC: "USRC17607839",
				Title: "Test Song", Artist: "Test Artist", Album: "Test Album",
				Source: "qq", TrackID: "0039MnYb0qxYhV", NativeIDVerified: true,
				DurationMS: 240000, PlaybackOffsetMS: 52000, Confidence: 0.94,
				SampledAtUnixMS: sampledAt.UnixMilli(),
			},
		})
	}))
	defer server.Close()

	provider, err := NewHTTPProvider(HTTPConfig{
		Endpoint: server.URL, Token: recognitionTestToken, Client: server.Client(),
		RequestTimeout: time.Second, AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Recognize(context.Background(), Submission{
		PCM: make([]byte, 32000), SampledAt: sampledAt, Duration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Matched || response.Result.StableID != "relay:track-42" || response.Result.ISRC != "USRC17607839" ||
		!response.Result.NativeIDVerified || response.Result.PlaybackOffset != 52*time.Second || response.Result.SampledAt != sampledAt {
		t.Fatalf("识歌响应映射错误: %#v", response)
	}
}

func TestHTTPProviderRequiresHTTPSUnlessExplicitlyAllowed(t *testing.T) {
	if _, err := NewHTTPProvider(HTTPConfig{
		Endpoint: "http://192.0.2.10/v1/recognize", Token: recognitionTestToken,
	}); err == nil {
		t.Fatal("默认不应允许明文 HTTP 携带识歌令牌")
	}
	if _, err := NewHTTPProvider(HTTPConfig{
		Endpoint: "https://relay.example/v1/recognize", Token: "short",
	}); err == nil {
		t.Fatal("过短令牌应被拒绝")
	}
	if _, err := NewHTTPProvider(HTTPConfig{
		Endpoint: "https://relay.example/v1/recognize", Token: "g" + recognitionTestToken[1:],
	}); err == nil {
		t.Fatal("非十六进制令牌应被拒绝")
	}
	if _, err := NewHTTPProvider(HTTPConfig{
		Endpoint: "https://relay.example/v1/recognize?api_key=secret", Token: recognitionTestToken,
	}); err == nil {
		t.Fatal("识歌中继 URL 不应允许可能泄密的 query")
	}
	for _, endpoint := range []string{
		"https://user:secret@relay.example/v1/recognize",
		"https://relay.example/v1/recognize?",
		"https://relay.example/v1/recognize#",
		"https://relay.example/v1/recognize#secret",
	} {
		if _, err := NewHTTPProvider(HTTPConfig{Endpoint: endpoint, Token: recognitionTestToken}); err == nil {
			t.Fatalf("识歌中继 URL 不应允许凭据或 fragment: %q", endpoint)
		}
	}
}

func TestHTTPProviderReturnsRetryAfterWithoutResponseBodyLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "37")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte("secret backend detail"))
	}))
	defer server.Close()
	provider, err := NewHTTPProvider(HTTPConfig{
		Endpoint: server.URL, Token: recognitionTestToken, Client: server.Client(),
		RequestTimeout: time.Second, AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Recognize(context.Background(), Submission{
		PCM: make([]byte, 32000), SampledAt: time.Now(), Duration: time.Second,
	})
	var retryError RetryAfterError
	if err == nil || !errors.As(err, &retryError) || retryError.RetryAfter() != 37*time.Second {
		t.Fatalf("应保留安全退避而不回显响应正文: %T %v", err, err)
	}
}

func TestHTTPProviderPreservesGatewayTimeoutRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "37")
		writer.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer server.Close()
	provider, err := NewHTTPProvider(HTTPConfig{
		Endpoint: server.URL, Token: recognitionTestToken, Client: server.Client(),
		RequestTimeout: time.Second, AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Recognize(context.Background(), Submission{
		PCM: make([]byte, 32000), SampledAt: time.Now(), Duration: time.Second,
	})
	var retryError RetryAfterError
	if err == nil || !errors.As(err, &retryError) || retryError.RetryAfter() != 37*time.Second {
		t.Fatalf("HTTP 504 应保留中继 Retry-After: %T %v", err, err)
	}
}

func TestHTTPProviderRejectsTrailingOrOversizedSuccessResponse(t *testing.T) {
	responses := [][]byte{
		[]byte(`{"protocol_version":1}{"protocol_version":1}`),
		append([]byte(`{"protocol_version":1}`), make([]byte, maximumRecognitionResponse)...),
	}
	for index, body := range responses {
		t.Run(fmt.Sprintf("response-%d", index), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("X-CaseScreen-Protocol", "recognition-v1")
				_, _ = writer.Write(body)
			}))
			defer server.Close()
			provider, err := NewHTTPProvider(HTTPConfig{
				Endpoint: server.URL, Token: recognitionTestToken, Client: server.Client(),
				RequestTimeout: time.Second, AllowInsecureHTTP: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Recognize(context.Background(), Submission{
				PCM: make([]byte, 32000), SampledAt: time.Now(), Duration: time.Second,
			}); err == nil {
				t.Fatal("尾随或超限的成功响应必须失败关闭")
			}
		})
	}
}

func TestHTTPProviderSendsFingerprintInsteadOfPCM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload recognitionRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.PreferredPlatform != "netease" || payload.DeviceFingerprint == nil ||
			payload.DeviceFingerprint.Algorithm != "vendor-acr" || len(payload.DeviceFingerprint.Data) != 4 ||
			len(payload.PCM) != 0 || payload.AudioFormat != "" {
			t.Fatalf("声纹优先请求错误: %#v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-CaseScreen-Protocol", "recognition-v1")
		_ = json.NewEncoder(writer).Encode(recognitionResponse{ProtocolVersion: 1})
	}))
	defer server.Close()
	provider, err := NewHTTPProvider(HTTPConfig{
		Endpoint: server.URL, Token: recognitionTestToken, Client: server.Client(),
		RequestTimeout: time.Second, AllowInsecureHTTP: true, PreferredPlatform: "netease",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Recognize(context.Background(), Submission{
		SampledAt: time.Now(), Duration: 10 * time.Second,
		Fingerprint: &Fingerprint{Algorithm: "vendor-acr", Version: "1", Data: []byte{1, 2, 3, 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
}
