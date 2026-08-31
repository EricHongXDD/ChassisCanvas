package edgepush

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const testPairingKey = "0101010101010101010101010101010101010101010101010101010101010101"

type fakeClock struct {
	value time.Time
}

func (clock *fakeClock) Now() time.Time { return clock.value }

func (clock *fakeClock) Add(duration time.Duration) { clock.value = clock.value.Add(duration) }

func testRandom() *bytes.Reader {
	data := make([]byte, challengeBytes*16)
	for index := range data {
		data[index] = byte(index + 1)
	}
	return bytes.NewReader(data)
}

func newTestReceiver(t *testing.T, clock *fakeClock) *Receiver {
	t.Helper()
	receiver, err := New(Config{
		PairingKey: testPairingKey, ChallengeTTL: time.Minute,
		SessionTTL: time.Hour, StaleAfter: 45 * time.Second,
		Now: clock.Now, Random: testRandom(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return receiver
}

func testSessionID(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, sessionBytes))
}

func issueTestChallenge(t *testing.T, receiver *Receiver, remote string) ChallengeResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, ChallengePath, nil)
	request.RemoteAddr = remote
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("获取挑战失败: %d %s", recorder.Code, recorder.Body.String())
	}
	var response ChallengeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func openTestSession(t *testing.T, receiver *Receiver, remote, challenge, session string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(SessionRequest{Version: ProtocolVersion, Challenge: challenge, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	signature, err := SessionSignature(testPairingKey, body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, SessionPath, bytes.NewReader(body))
	request.RemoteAddr = remote
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(HeaderMAC, signature)
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, request)
	return recorder
}

func sendTestUpdate(t *testing.T, receiver *Receiver, session string, sequence uint64, update Update) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := StateSignature(testPairingKey, session, sequence, body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, StatePath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set(HeaderSession, session)
	request.Header.Set(HeaderSequence, stringUint(sequence))
	request.Header.Set(HeaderMAC, signature)
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, request)
	return recorder
}

func stringUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}

func activeQQUpdate(position time.Duration, playing bool) Update {
	return Update{
		Version: ProtocolVersion, Available: true, Source: "qq", TrackID: "0039MnYb0qxYhV",
		PositionMS: int64(position / time.Millisecond), DurationMS: int64((3 * time.Minute) / time.Millisecond),
		PositionKnown: true, Playing: playing,
	}
}

func TestUpdateValidationLocksNativeIDToPlatform(t *testing.T) {
	valid := []Update{
		activeQQUpdate(12*time.Second, true),
		{Version: 1, Available: true, Source: "netease", TrackID: "186016", PositionKnown: true},
		{Version: 1, Available: false},
	}
	for _, update := range valid {
		if err := update.Validate(); err != nil {
			t.Fatalf("合法状态被拒绝: %#v: %v", update, err)
		}
	}

	invalid := []Update{
		{Version: 2, Available: false},
		{Version: 1, Available: true, Source: "QQ Music", TrackID: "0039MnYb0qxYhV", PositionKnown: true},
		{Version: 1, Available: true, Source: "qq", TrackID: "186016", PositionKnown: true},
		{Version: 1, Available: true, Source: "netease", TrackID: "0039MnYb0qxYhV", PositionKnown: true},
		{Version: 1, Available: true, Source: "netease", TrackID: "0186016", PositionKnown: true},
		{Version: 1, Available: true, Source: "qq", TrackID: "0039MnYb0qxYhV"},
		{Version: 1, Available: true, Source: "qq", TrackID: "0039MnYb0qxYhV", PositionKnown: true, PositionMS: -1},
		{Version: 1, Available: true, Source: "qq", TrackID: "0039MnYb0qxYhV", PositionKnown: true, PositionMS: 2, DurationMS: 1},
		{Version: 1, Available: false, Source: "qq"},
	}
	for _, update := range invalid {
		if err := update.Validate(); err == nil {
			t.Fatalf("非法状态被接受: %#v", update)
		}
	}
}

func TestTypicalUpdateBodyStaysBelow256BytesAndContainsNoSensitiveFields(t *testing.T) {
	body, err := json.Marshal(activeQQUpdate(42*time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 256 {
		t.Fatalf("播放状态正文过大: %d 字节: %s", len(body), body)
	}
	for _, forbidden := range []string{"title", "artist", "album", "lyrics", "cookie", "audio", "microphone", "password"} {
		if bytes.Contains(bytes.ToLower(body), []byte(forbidden)) {
			t.Fatalf("小包包含禁止字段 %q: %s", forbidden, body)
		}
	}
}

func TestAuthenticatedSessionAcceptsStateAndProjectsLocalPosition(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	remote := "192.0.2.10:4000"
	challenge := issueTestChallenge(t, receiver, remote)
	if challenge.Version != ProtocolVersion || !validChallenge(challenge.Challenge) {
		t.Fatalf("挑战响应无效: %#v", challenge)
	}
	session := testSessionID(0x31)
	if response := openTestSession(t, receiver, remote, challenge.Challenge, session); response.Code != http.StatusNoContent {
		t.Fatalf("建立会话失败: %d %s", response.Code, response.Body.String())
	}
	if response := sendTestUpdate(t, receiver, session, 1, activeQQUpdate(12*time.Second, true)); response.Code != http.StatusNoContent {
		t.Fatalf("发送状态失败: %d %s", response.Code, response.Body.String())
	}

	clock.Add(2500 * time.Millisecond)
	envelope, err := receiver.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !envelope.Available || envelope.Metadata.Source != "qq" || envelope.Metadata.TrackID != "0039MnYb0qxYhV" {
		t.Fatalf("歌曲身份错误: %#v", envelope)
	}
	if envelope.Track.Position != int64(14500*time.Millisecond) || !envelope.Track.PositionKnown || !envelope.Track.Playing {
		t.Fatalf("本地播放位置没有推进: %#v", envelope.Track)
	}
	if envelope.Track.Title != "" || envelope.Track.Artist != "" || envelope.Track.Album != "" || envelope.Metadata.LyricMode != "unknown" {
		t.Fatalf("接收器生成了不属于小包的元数据: %#v", envelope)
	}
}

func TestPauseSeekAndUnavailableState(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	remote := "192.0.2.11:4000"
	challenge := issueTestChallenge(t, receiver, remote)
	session := testSessionID(0x32)
	if response := openTestSession(t, receiver, remote, challenge.Challenge, session); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	if response := sendTestUpdate(t, receiver, session, 1, activeQQUpdate(10*time.Second, true)); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	clock.Add(3 * time.Second)
	if response := sendTestUpdate(t, receiver, session, 2, activeQQUpdate(13*time.Second, false)); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	clock.Add(5 * time.Second)
	paused, _ := receiver.Current(context.Background())
	if paused.Track.Position != int64(13*time.Second) || paused.Track.Playing {
		t.Fatalf("暂停状态仍在推进: %#v", paused.Track)
	}
	if response := sendTestUpdate(t, receiver, session, 3, activeQQUpdate(2*time.Second, true)); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	clock.Add(2 * time.Second)
	seeked, _ := receiver.Current(context.Background())
	if seeked.Track.Position != int64(4*time.Second) {
		t.Fatalf("倒退跳转没有重建锚点: %#v", seeked.Track)
	}
	if response := sendTestUpdate(t, receiver, session, 4, Update{Version: 1}); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	cleared, _ := receiver.Current(context.Background())
	if cleared.Available || cleared.Metadata.Source != "" || cleared.Track.Position != 0 {
		t.Fatalf("不可用状态没有清空歌曲: %#v", cleared)
	}
}

func TestStateExpiresWithoutHeartbeat(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	remote := "192.0.2.12:4000"
	challenge := issueTestChallenge(t, receiver, remote)
	session := testSessionID(0x33)
	if response := openTestSession(t, receiver, remote, challenge.Challenge, session); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	if response := sendTestUpdate(t, receiver, session, 1, activeQQUpdate(time.Second, true)); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	clock.Add(46 * time.Second)
	envelope, _ := receiver.Current(context.Background())
	if envelope.Available {
		t.Fatalf("过期播放状态仍可用: %#v", envelope)
	}
}

func TestReplayAndSequenceConflictsDoNotReplaceState(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	remote := "192.0.2.13:4000"
	challenge := issueTestChallenge(t, receiver, remote)
	session := testSessionID(0x34)
	if response := openTestSession(t, receiver, remote, challenge.Challenge, session); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	first := activeQQUpdate(10*time.Second, true)
	if response := sendTestUpdate(t, receiver, session, 1, first); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	if response := sendTestUpdate(t, receiver, session, 1, first); response.Code != http.StatusNoContent {
		t.Fatalf("完全相同的网络重试应幂等: %d %s", response.Code, response.Body.String())
	}
	changed := activeQQUpdate(20*time.Second, true)
	if response := sendTestUpdate(t, receiver, session, 1, changed); response.Code != http.StatusConflict {
		t.Fatalf("相同序号的不同正文未被拒绝: %d", response.Code)
	}
	if response := sendTestUpdate(t, receiver, session, 2, changed); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	if response := sendTestUpdate(t, receiver, session, 1, first); response.Code != http.StatusConflict {
		t.Fatalf("旧序号未被拒绝: %d", response.Code)
	}
	envelope, _ := receiver.Current(context.Background())
	if envelope.Track.Position != int64(20*time.Second) {
		t.Fatalf("重放覆盖了较新状态: %#v", envelope.Track)
	}
}

func TestBadMACAndMalformedSequenceAreRejected(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	remote := "192.0.2.14:4000"
	challenge := issueTestChallenge(t, receiver, remote)
	session := testSessionID(0x35)
	if response := openTestSession(t, receiver, remote, challenge.Challenge, session); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	body, _ := json.Marshal(activeQQUpdate(time.Second, true))
	request := httptest.NewRequest(http.MethodPut, StatePath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(HeaderSession, session)
	request.Header.Set(HeaderSequence, "1")
	request.Header.Set(HeaderMAC, "invalid")
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("错误 MAC 未被拒绝: %d", recorder.Code)
	}

	signature, _ := StateSignature(testPairingKey, session, 1, body)
	request = httptest.NewRequest(http.MethodPut, StatePath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(HeaderSession, session)
	request.Header.Set(HeaderSequence, "01")
	request.Header.Set(HeaderMAC, signature)
	recorder = httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非规范序号未被拒绝: %d", recorder.Code)
	}
	if response := sendTestUpdate(t, receiver, session, 1, activeQQUpdate(time.Second, true)); response.Code != http.StatusNoContent {
		t.Fatalf("认证失败不应消耗序号: %d %s", response.Code, response.Body.String())
	}
}

func TestChallengeIsBoundToRemoteAndSingleUse(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	challenge := issueTestChallenge(t, receiver, "192.0.2.15:4000")
	firstSession := testSessionID(0x36)
	wrongRemote := openTestSession(t, receiver, "192.0.2.16:4000", challenge.Challenge, firstSession)
	if wrongRemote.Code != http.StatusConflict {
		t.Fatalf("其他来源使用挑战未被拒绝: %d", wrongRemote.Code)
	}
	if response := openTestSession(t, receiver, "192.0.2.15:4999", challenge.Challenge, firstSession); response.Code != http.StatusNoContent {
		t.Fatalf("同一主机更换端口后无法使用挑战: %d %s", response.Code, response.Body.String())
	}
	secondSession := testSessionID(0x37)
	reused := openTestSession(t, receiver, "192.0.2.15:4999", challenge.Challenge, secondSession)
	if reused.Code != http.StatusConflict {
		t.Fatalf("已消费挑战被再次使用: %d", reused.Code)
	}
}

func TestNewSessionTakesOverAndOldSessionCannotWrite(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	remote := "192.0.2.17:4000"
	firstChallenge := issueTestChallenge(t, receiver, remote)
	firstSession := testSessionID(0x38)
	if response := openTestSession(t, receiver, remote, firstChallenge.Challenge, firstSession); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	secondChallenge := issueTestChallenge(t, receiver, remote)
	secondSession := testSessionID(0x39)
	if response := openTestSession(t, receiver, remote, secondChallenge.Challenge, secondSession); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	if response := sendTestUpdate(t, receiver, firstSession, 1, activeQQUpdate(time.Second, true)); response.Code != http.StatusConflict {
		t.Fatalf("旧会话在接管后仍可写入: %d", response.Code)
	}
	if response := sendTestUpdate(t, receiver, secondSession, 1, activeQQUpdate(2*time.Second, true)); response.Code != http.StatusNoContent {
		t.Fatalf("新会话无法写入: %d %s", response.Code, response.Body.String())
	}
}

func TestStrictJSONContentTypeAndSizeLimits(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)

	for _, test := range []struct {
		name        string
		contentType string
		body        []byte
		want        int
	}{
		{name: "未知字段", contentType: "application/json", body: []byte(`{"v":1,"challenge":"x","session":"y","extra":true}`), want: http.StatusBadRequest},
		{name: "重复字段", contentType: "application/json", body: []byte(`{"v":1,"v":1,"challenge":"x","session":"y"}`), want: http.StatusBadRequest},
		{name: "尾随 JSON", contentType: "application/json", body: []byte(`{"v":1,"challenge":"x","session":"y"}{}`), want: http.StatusBadRequest},
		{name: "错误类型", contentType: "text/plain", body: []byte(`{}`), want: http.StatusUnsupportedMediaType},
		{name: "正文过大", contentType: "application/json", body: bytes.Repeat([]byte{'x'}, maximumRequestBytes+1), want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, SessionPath, bytes.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			recorder := httptest.NewRecorder()
			receiver.Handler().ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("状态码=%d，期望=%d，响应=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, ChallengePath, nil)
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, request)
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("安全响应头缺失: %#v", recorder.Header())
	}
}

func TestExpiredChallengeAndSessionAreRejected(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver, err := New(Config{
		PairingKey: testPairingKey, ChallengeTTL: time.Second,
		SessionTTL: 2 * time.Second, StaleAfter: time.Minute,
		Now: clock.Now, Random: testRandom(),
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := "192.0.2.18:4000"
	expiredChallenge := issueTestChallenge(t, receiver, remote)
	clock.Add(2 * time.Second)
	if response := openTestSession(t, receiver, remote, expiredChallenge.Challenge, testSessionID(0x40)); response.Code != http.StatusConflict {
		t.Fatalf("过期挑战被接受: %d", response.Code)
	}
	challenge := issueTestChallenge(t, receiver, remote)
	session := testSessionID(0x41)
	if response := openTestSession(t, receiver, remote, challenge.Challenge, session); response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	clock.Add(3 * time.Second)
	if response := sendTestUpdate(t, receiver, session, 1, activeQQUpdate(time.Second, true)); response.Code != http.StatusUnauthorized {
		t.Fatalf("过期会话仍能写入: %d", response.Code)
	}
}

func TestCurrentHonorsCancelledOrNilContext(t *testing.T) {
	clock := &fakeClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	receiver := newTestReceiver(t, clock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.Current(ctx); err == nil {
		t.Fatal("已取消上下文没有返回错误")
	}
	if _, err := receiver.Current(nil); err == nil {
		t.Fatal("空上下文没有返回错误")
	}
}

func TestSignatureHelpersRejectMalformedKeySessionAndSequence(t *testing.T) {
	if _, err := SessionSignature("short", []byte(`{}`)); err == nil {
		t.Fatal("短配对密钥被接受")
	}
	if _, err := StateSignature(testPairingKey, "bad", 1, []byte(`{}`)); err == nil {
		t.Fatal("无效会话 ID 被接受")
	}
	if _, err := StateSignature(testPairingKey, testSessionID(0x42), 0, []byte(`{}`)); err == nil {
		t.Fatal("零序号被接受")
	}
}
