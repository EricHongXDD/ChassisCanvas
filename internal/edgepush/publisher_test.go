package edgepush

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestPublisher(t *testing.T, baseURL string, random io.Reader) *Publisher {
	t.Helper()
	publisher, err := NewPublisher(PublisherConfig{
		BaseURL: baseURL, PairingKey: testPairingKey, Random: random,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func TestNativePlaybackConstructorsAcceptOnlyPlatformIDShapes(t *testing.T) {
	qq, err := QQNativePlayback("0039MnYb0qxYhV", 12*time.Second, 3*time.Minute, true)
	if err != nil || qq.Source() != "qq" || qq.TrackID() != "0039MnYb0qxYhV" {
		t.Fatalf("QQ 原生状态构造失败: state=%#v err=%v", qq, err)
	}
	netease, err := NeteaseNativePlayback("186016", 0, 0, false)
	if err != nil || netease.Source() != "netease" || netease.TrackID() != "186016" {
		t.Fatalf("网易云原生状态构造失败: state=%#v err=%v", netease, err)
	}
	for _, test := range []struct {
		name string
		make func() (NativePlayback, error)
	}{
		{name: "QQ 使用网易数字 ID", make: func() (NativePlayback, error) {
			return QQNativePlayback("186016", 0, 0, false)
		}},
		{name: "网易使用 QQ songmid", make: func() (NativePlayback, error) {
			return NeteaseNativePlayback("0039MnYb0qxYhV", 0, 0, false)
		}},
		{name: "网易前导零", make: func() (NativePlayback, error) {
			return NeteaseNativePlayback("0186016", 0, 0, false)
		}},
		{name: "负播放位置", make: func() (NativePlayback, error) {
			return QQNativePlayback("0039MnYb0qxYhV", -time.Second, 0, false)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.make(); err == nil {
				t.Fatal("非法原生状态被接受")
			}
		})
	}
}

func TestPublisherEndToEndSendAndClear(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	publisher := newTestPublisher(t, server.URL, bytes.NewReader(bytes.Repeat([]byte{0x61}, sessionBytes)))
	playback, err := NeteaseNativePlayback("186016", 42*time.Second, 4*time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Send(context.Background(), playback); err != nil {
		t.Fatalf("发送原生状态失败: %v", err)
	}
	clock.Add(1500 * time.Millisecond)
	envelope, err := receiver.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !envelope.Available || envelope.Metadata.Source != "netease" || envelope.Metadata.TrackID != "186016" {
		t.Fatalf("屏端没有收到原生歌曲身份: %#v", envelope)
	}
	if envelope.Track.Position != int64(43500*time.Millisecond) || !envelope.Track.Playing {
		t.Fatalf("屏端播放锚点错误: %#v", envelope.Track)
	}
	if err := publisher.Clear(context.Background()); err != nil {
		t.Fatalf("发送清空状态失败: %v", err)
	}
	cleared, _ := receiver.Current(context.Background())
	if cleared.Available {
		t.Fatalf("清空后仍保留歌曲: %#v", cleared)
	}
}

func TestPublisherStatePacketNeverContainsSearchMetadataOrCredentials(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	var captured []byte
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == StatePath {
			data, _ := io.ReadAll(request.Body)
			captured = append([]byte(nil), data...)
			request.Body = io.NopCloser(bytes.NewReader(data))
		}
		receiver.Handler().ServeHTTP(writer, request)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	publisher := newTestPublisher(t, server.URL, bytes.NewReader(bytes.Repeat([]byte{0x62}, sessionBytes)))
	playback, _ := QQNativePlayback("0039MnYb0qxYhV", time.Second, time.Minute, true)
	if err := publisher.Send(context.Background(), playback); err != nil {
		t.Fatal(err)
	}
	if len(captured) == 0 || len(captured) > 256 {
		t.Fatalf("来源小包大小无效: %d", len(captured))
	}
	lower := bytes.ToLower(captured)
	for _, forbidden := range []string{"title", "artist", "album", "lyrics", "cookie", "password", "audio", "microphone", "query"} {
		if bytes.Contains(lower, []byte(forbidden)) {
			t.Fatalf("来源小包包含禁止字段 %q: %s", forbidden, captured)
		}
	}
	var update Update
	if err := json.Unmarshal(captured, &update); err != nil || update.Source != "qq" || update.TrackID != "0039MnYb0qxYhV" {
		t.Fatalf("来源小包身份错误: %#v err=%v", update, err)
	}
}

func TestPublisherRecoversAfterReceiverRestart(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)}
	first := newTestReceiver(t, clock)
	current := first
	var receiverMu sync.RWMutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receiverMu.RLock()
		handler := current.Handler()
		receiverMu.RUnlock()
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()
	publisher := newTestPublisher(t, server.URL, bytes.NewReader(bytes.Repeat([]byte{0x63}, sessionBytes*2)))
	playback, _ := QQNativePlayback("0039MnYb0qxYhV", time.Second, time.Minute, true)
	if err := publisher.Send(context.Background(), playback); err != nil {
		t.Fatal(err)
	}
	second := newTestReceiver(t, clock)
	receiverMu.Lock()
	current = second
	receiverMu.Unlock()
	playback, _ = QQNativePlayback("0039MnYb0qxYhV", 2*time.Second, time.Minute, true)
	if err := publisher.Send(context.Background(), playback); err != nil {
		t.Fatalf("屏端重启后没有自动重建会话: %v", err)
	}
	envelope, _ := second.Current(context.Background())
	if !envelope.Available || envelope.Track.Position != int64(2*time.Second) {
		t.Fatalf("重建会话后状态错误: %#v", envelope)
	}
}

func TestPublisherRecoversWhenAnotherSourceSessionTakesOver(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	firstRandom := append(bytes.Repeat([]byte{0x64}, sessionBytes), bytes.Repeat([]byte{0x69}, sessionBytes)...)
	first := newTestPublisher(t, server.URL, bytes.NewReader(firstRandom))
	second := newTestPublisher(t, server.URL, bytes.NewReader(bytes.Repeat([]byte{0x65}, sessionBytes)))
	playback, _ := QQNativePlayback("0039MnYb0qxYhV", time.Second, time.Minute, true)
	if err := first.Send(context.Background(), playback); err != nil {
		t.Fatal(err)
	}
	if err := second.Send(context.Background(), playback); err != nil {
		t.Fatal(err)
	}
	playback, _ = QQNativePlayback("0039MnYb0qxYhV", 3*time.Second, time.Minute, true)
	if err := first.Send(context.Background(), playback); err != nil {
		t.Fatalf("会话被接管后没有重新配对: %v", err)
	}
	envelope, _ := receiver.Current(context.Background())
	if envelope.Track.Position != int64(3*time.Second) {
		t.Fatalf("重新接管后的状态错误: %#v", envelope)
	}
}

func TestPublisherSerializesConcurrentSequenceNumbers(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	publisher := newTestPublisher(t, server.URL, bytes.NewReader(bytes.Repeat([]byte{0x66}, sessionBytes)))
	const count = 12
	errorsChannel := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(position int) {
			defer wait.Done()
			playback, err := NeteaseNativePlayback("186016", time.Duration(position)*time.Second, time.Minute, true)
			if err == nil {
				err = publisher.Send(context.Background(), playback)
			}
			errorsChannel <- err
		}(index)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("并发发送失败: %v", err)
		}
	}
	if publisher.sequence != count {
		t.Fatalf("并发发送序号=%d，期望=%d", publisher.sequence, count)
	}
}

func TestPublisherRejectsInvalidChallengeAndDoesNotFollowRedirect(t *testing.T) {
	playback, _ := QQNativePlayback("0039MnYb0qxYhV", 0, time.Minute, false)
	invalidChallenge := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"v":1,"challenge":"bad","expires_at_unix_ms":1,"extra":true}`))
	}))
	publisher := newTestPublisher(t, invalidChallenge.URL, bytes.NewReader(bytes.Repeat([]byte{0x67}, sessionBytes)))
	if err := publisher.Send(context.Background(), playback); err == nil || !strings.Contains(err.Error(), "挑战响应无效") {
		t.Fatalf("非法挑战响应被接受: %v", err)
	}
	invalidChallenge.Close()

	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL)
		writer.WriteHeader(http.StatusFound)
	}))
	defer redirector.Close()
	publisher = newTestPublisher(t, redirector.URL, bytes.NewReader(bytes.Repeat([]byte{0x68}, sessionBytes)))
	if err := publisher.Send(context.Background(), playback); err == nil {
		t.Fatal("重定向挑战被接受")
	}
	if redirected.Load() {
		t.Fatal("来源认证客户端跟随了跨地址重定向")
	}
}

func TestPublisherRejectsGenericOrCredentialedBaseURLAndZeroPlayback(t *testing.T) {
	for _, rawURL := range []string{"", "ftp://screen.local", "http://user:pass@screen.local", "http://screen.local?token=x", "http://screen.local/#fragment"} {
		if _, err := NewPublisher(PublisherConfig{BaseURL: rawURL, PairingKey: testPairingKey}); err == nil {
			t.Fatalf("非法屏端地址被接受: %q", rawURL)
		}
	}
	publisher := &Publisher{}
	if err := publisher.Send(context.Background(), NativePlayback{}); err == nil {
		t.Fatal("零值原生播放状态被发送")
	}
}
