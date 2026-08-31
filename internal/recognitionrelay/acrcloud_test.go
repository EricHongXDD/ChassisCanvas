package recognitionrelay

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"casescreen/internal/edgerecognition"
)

const (
	testACRAccessKey    = "test-access-key"
	testACRAccessSecret = "test-access-secret"
)

func TestACRCloudProviderUsesOfficialProtocol1Contract(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != acrIdentifyPath {
			t.Errorf("ACR 请求方法/路径错误: %s %s", request.Method, request.URL.Path)
		}
		if err := request.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("ACR multipart 无效: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		timestamp := strconv.FormatInt(now.Unix(), 10)
		expectedSignature := acrTestSignature(testACRAccessKey, testACRAccessSecret, "audio", timestamp)
		for name, expected := range map[string]string{
			"access_key": testACRAccessKey, "timestamp": timestamp,
			"signature": expectedSignature, "data_type": "audio", "signature_version": "1",
		} {
			if actual := request.FormValue(name); actual != expected {
				t.Errorf("ACR 字段 %s=%q，期望 %q", name, actual, expected)
			}
		}
		file, _, err := request.FormFile("sample")
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		sample, err := io.ReadAll(file)
		if err != nil {
			t.Error(err)
		}
		if len(sample) != 5*edgerecognition.SampleRateHz*2+44 || string(sample[:4]) != "RIFF" || string(sample[8:12]) != "WAVE" {
			t.Errorf("PCM 未被包装成 16-bit WAV: bytes=%d prefix=%q", len(sample), sample[:min(12, len(sample))])
		}
		if request.FormValue("sample_bytes") != strconv.Itoa(len(sample)) {
			t.Errorf("sample_bytes 与实际内容不一致")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
          "status":{"code":0,"msg":"Success","version":"1.0"},
          "metadata":{"music":[{
            "acrid":"recording42","title":"Test Song","score":"94",
            "duration_ms":240000,"play_offset_ms":52000,
            "album":{"name":"Test Album"},
            "artists":[{"name":"Artist A"},{"name":"Artist B"}],
            "external_ids":{"isrc":"US-RC1-76-07839"},
            "external_metadata":{"spotify":{"track":{"id":"ignored"}}}
          }]}
        }`))
	}))
	defer upstream.Close()
	provider, err := NewACRCloudProvider(ACRCloudConfig{
		Host: upstream.URL, AccessKey: testACRAccessKey, AccessSecret: testACRAccessSecret,
		Client: upstream.Client(), Timeout: time.Second, AllowInsecureHTTP: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Recognize(context.Background(), edgerecognition.Submission{
		PCM: make([]byte, 5*edgerecognition.SampleRateHz*2), SampledAt: now, Duration: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response.Result
	if !response.Matched || result.StableID != "acrcloud:recording42" || result.ISRC != "USRC17607839" ||
		result.Title != "Test Song" || result.Artist != "Artist A, Artist B" || result.Album != "Test Album" ||
		result.Duration != 240*time.Second || result.PlaybackOffset != 52*time.Second || result.Confidence != .94 ||
		result.Source != "" || result.TrackID != "" || result.NativeIDVerified {
		t.Fatalf("ACR 结果解析错误: %#v", result)
	}
}

func TestACRCloudProviderForwardsOnlyConfiguredFingerprintFormat(t *testing.T) {
	now := time.Now()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		if request.FormValue("data_type") != "fingerprint" || request.FormValue("sample_bytes") != "4" {
			t.Fatalf("声纹 ACR 字段错误: type=%q bytes=%q", request.FormValue("data_type"), request.FormValue("sample_bytes"))
		}
		file, _, err := request.FormFile("sample")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if string(data) != "\x01\x02\x03\x04" {
			t.Fatalf("声纹被修改: %v", data)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"status": map[string]any{"code": 1001, "msg": "No result"},
		})
	}))
	defer upstream.Close()
	provider, err := NewACRCloudProvider(ACRCloudConfig{
		Host: upstream.URL, AccessKey: testACRAccessKey, AccessSecret: testACRAccessSecret,
		FingerprintAlgorithm: "acrcloud", FingerprintVersion: "1",
		Client: upstream.Client(), Timeout: time.Second, AllowInsecureHTTP: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Recognize(context.Background(), edgerecognition.Submission{
		Fingerprint: &edgerecognition.Fingerprint{Algorithm: "acrcloud", Version: "1", Data: []byte{1, 2, 3, 4}},
		SampledAt:   now, Duration: 10 * time.Second,
	})
	if err != nil || response.Matched || response.RetryAfter != 10*time.Second {
		t.Fatalf("ACR no-result 映射错误: response=%#v err=%v", response, err)
	}
	_, err = provider.Recognize(context.Background(), edgerecognition.Submission{
		Fingerprint: &edgerecognition.Fingerprint{Algorithm: "other", Version: "1", Data: []byte{1}},
		SampledAt:   now, Duration: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("未配置的声纹算法不应转发")
	}
}

func TestACRCloudProviderRejectsLowScoreAndNormalizesScores(t *testing.T) {
	music := []acrMusic{
		{ACRID: "below-threshold", Title: "Low", Score: 69, DurationMS: 1000, PlayOffsetMS: 100},
		{ACRID: "valid-record", Title: "High", Score: 80, DurationMS: 2000, PlayOffsetMS: 200},
	}
	music[0].Artists = append(music[0].Artists, struct {
		Name string `json:"name"`
	}{Name: "Artist"})
	music[1].Artists = append(music[1].Artists, struct {
		Name string `json:"name"`
	}{Name: "Artist"})
	result, found := bestACRMusic(music, time.Unix(1, 0))
	if !found || result.StableID != "acrcloud:valid-record" || result.Confidence != .8 {
		t.Fatalf("ACR 分数 70..100 归一化错误: %#v found=%v", result, found)
	}
}

func TestACRCloudProviderDoesNotLeakUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "37")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte("credential diagnostic detail"))
	}))
	defer upstream.Close()
	provider, err := NewACRCloudProvider(ACRCloudConfig{
		Host: upstream.URL, AccessKey: testACRAccessKey, AccessSecret: testACRAccessSecret,
		Client: upstream.Client(), Timeout: time.Second, AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Recognize(context.Background(), edgerecognition.Submission{
		PCM: make([]byte, 5*edgerecognition.SampleRateHz*2), SampledAt: time.Now(), Duration: 5 * time.Second,
	})
	var temporary TemporaryError
	if err == nil || !strings.Contains(err.Error(), "暂时") || !asTemporary(err, &temporary) ||
		temporary.RetryAfter() != 37*time.Second || strings.Contains(err.Error(), "credential") {
		t.Fatalf("ACR 临时失败处理错误: %T %v", err, err)
	}
}

func asTemporary(err error, target *TemporaryError) bool {
	if candidate, ok := err.(TemporaryError); ok {
		*target = candidate
		return true
	}
	return false
}

func TestACRCloudEndpointRequiresHTTPSAndNoEmbeddedCredentials(t *testing.T) {
	for _, host := range []string{
		"http://identify.example.test", "https://user:secret@identify.example.test",
		"https://identify.example.test/v1/other", "https://identify.example.test?key=secret",
	} {
		if _, err := NewACRCloudProvider(ACRCloudConfig{
			Host: host, AccessKey: testACRAccessKey, AccessSecret: testACRAccessSecret,
		}); err == nil {
			t.Fatalf("不安全 ACR host 应被拒绝: %q", host)
		}
	}
}

func acrTestSignature(accessKey, secret, dataType, timestamp string) string {
	stringToSign := strings.Join([]string{"POST", "/v1/identify", accessKey, dataType, "1", timestamp}, "\n")
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
