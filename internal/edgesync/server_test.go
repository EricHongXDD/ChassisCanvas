package edgesync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"casescreen/internal/lyrics"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestResponseWriteTimeoutCoversLyricsResolution(t *testing.T) {
	server, err := NewWithMetadataAndLyrics(
		Config{Token: testToken, LyricsTimeout: 12 * time.Second},
		MetadataSourceFunc(func(context.Context) (lyrics.NowPlaying, Metadata, error) {
			return lyrics.NowPlaying{}, Metadata{}, nil
		}),
		LyricsSourceFunc(func(context.Context, Metadata) (LyricsResult, error) {
			return LyricsResult{}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := server.responseWriteTimeout(); got != 17*time.Second {
		t.Fatalf("HTTP 写超时没有覆盖歌词请求与写回余量: %v", got)
	}
}

func authenticatedRequest(method, target, remoteAddress string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = remoteAddress
	request.Header.Set("Authorization", "Bearer "+testToken)
	return request
}

func TestNowPlayingReturnsOnlyWhitelistedMetadata(t *testing.T) {
	longAlbum := strings.Repeat("专", 600)
	server, err := New(Config{Token: testToken}, SourceFunc(func(context.Context) (lyrics.NowPlaying, error) {
		return lyrics.NowPlaying{
			Source: " qq ", Title: " 测试歌曲 ", Artist: " 测试歌手 ", Album: longAlbum,
			Position: 120, Duration: 100, PositionKnown: true, Playing: true,
		}, nil
	}))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.10:4000"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("缺少安全响应头: %#v", recorder.Header())
	}
	var snapshot Snapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if snapshot.ProtocolVersion != ProtocolVersion || snapshot.SampledAtUnixMS == 0 || !snapshot.Available {
		t.Fatalf("协议元数据不正确: %#v", snapshot)
	}
	if !snapshot.AuthRequired || snapshot.Metadata.Source != "qq" || snapshot.Metadata.LyricMode != LyricModeUnknown {
		t.Fatalf("旧数据源的兼容元数据不正确: %#v", snapshot)
	}
	if snapshot.Metadata.TrackID != "" || strings.Contains(recorder.Body.String(), `"track_id"`) {
		t.Fatalf("旧数据源不应产生歌曲 ID: %s", recorder.Body.String())
	}
	if snapshot.Track.Source != "qq" || snapshot.Track.Title != "测试歌曲" || snapshot.Track.Artist != "测试歌手" {
		t.Fatalf("歌曲元数据不正确: %#v", snapshot.Track)
	}
	if snapshot.Track.Position != snapshot.Track.Duration || snapshot.Track.Duration != 100 {
		t.Fatalf("播放进度没有约束到时长: %#v", snapshot.Track)
	}
	if got := len([]rune(snapshot.Track.Album)); got != 512 {
		t.Fatalf("过长文本未截断: %d", got)
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{"devices", "last_error", "lyrics_path", "theme"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("响应泄漏了非播放器字段 %q: %s", forbidden, body)
		}
	}
}

func TestExtendedMetadataEnvelopeIsSmallAndBackwardCompatible(t *testing.T) {
	server, err := NewWithMetadata(Config{Token: testToken}, MetadataSourceFunc(func(context.Context) (lyrics.NowPlaying, Metadata, error) {
		return lyrics.NowPlaying{
			Source: "generic-player", Title: "晴天", Artist: "周杰伦", Album: "叶惠美",
			Position: 42 * int64(time.Second), Duration: 269 * int64(time.Second),
			PositionKnown: true, Playing: true,
		}, Metadata{Source: "netease", TrackID: "186016", LyricMode: LyricModeTimed}, nil
	}))
	if err != nil {
		t.Fatalf("创建扩展服务失败: %v", err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.11:4000"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() > 1024 {
		t.Fatalf("典型同步包过大: %d 字节，响应 = %s", recorder.Body.Len(), recorder.Body.String())
	}
	var envelope Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析新协议失败: %v", err)
	}
	if !envelope.AuthRequired || !envelope.Available {
		t.Fatalf("envelope 能力状态错误: %#v", envelope)
	}
	if envelope.Metadata.Source != "netease" || envelope.Metadata.TrackID != "186016" || envelope.Metadata.LyricMode != LyricModeTimed {
		t.Fatalf("平台扩展元数据错误: %#v", envelope.Metadata)
	}
	if envelope.Track.Source != "netease" {
		t.Fatalf("旧 track 来源没有与平台元数据保持一致: %#v", envelope.Track)
	}

	// 模拟尚未升级的副屏：未知字段会被忽略，原有 track 仍能完整解码。
	var legacy struct {
		ProtocolVersion int               `json:"protocol_version"`
		SampledAtUnixMS int64             `json:"sampled_at_unix_ms"`
		Available       bool              `json:"available"`
		Track           lyrics.NowPlaying `json:"track"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("旧协议无法解码新响应: %v", err)
	}
	if !legacy.Available || legacy.Track.Title != "晴天" || legacy.Track.Source != "netease" {
		t.Fatalf("旧协议解码后丢失歌曲信息: %#v", legacy)
	}

	var raw map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析白名单响应失败: %v", err)
	}
	assertAllowedJSONKeys(t, "envelope", raw, map[string]bool{
		"protocol_version": true, "sampled_at_unix_ms": true, "available": true,
		"auth_required": true, "metadata": true, "track": true,
	})
	assertAllowedJSONKeys(t, "metadata", raw["metadata"].(map[string]any), map[string]bool{
		"source": true, "track_id": true, "lyric_mode": true,
	})
	assertAllowedJSONKeys(t, "track", raw["track"].(map[string]any), map[string]bool{
		"source": true, "track_id": true, "title": true, "artist": true, "album": true,
		"position_ns": true, "duration_ns": true, "position_known": true, "playing": true,
	})
	for _, forbidden := range []string{
		`"lyrics"`, `"lyric_text"`, `"lrc"`, `"audio"`, `"pcm"`, `"waveform"`,
		`"cookie"`, `"authorization"`,
	} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("同步包包含禁止的歌词或音频字段 %s: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestCompactNowPlayingOmitsRepeatedAndStaticTrackMetadata(t *testing.T) {
	server, err := NewWithMetadata(Config{Token: testToken}, MetadataSourceFunc(func(context.Context) (lyrics.NowPlaying, Metadata, error) {
		return lyrics.NowPlaying{
			Source: "qq", TrackID: "0039MnYb0qxYhV", Title: "每秒不应重复发送的标题",
			Artist: "歌手", Album: "专辑", Position: 42 * int64(time.Second),
			Duration: 269 * int64(time.Second), PositionKnown: true, Playing: true,
		}, Metadata{Source: "qq", TrackID: "0039MnYb0qxYhV", LyricMode: LyricModeTimed}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(
		http.MethodGet, NowPlayingPath+"?compact=1", "192.0.2.12:4000"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() > 320 {
		t.Fatalf("精简同步包超过 320 字节: %d，响应 = %s", recorder.Body.Len(), recorder.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	metadata := raw["metadata"].(map[string]any)
	track := raw["track"].(map[string]any)
	assertAllowedJSONKeys(t, "compact metadata", metadata, map[string]bool{
		"source": true, "track_id": true, "lyric_mode": true,
	})
	assertAllowedJSONKeys(t, "compact track", track, map[string]bool{
		"position_ns": true, "position_known": true, "playing": true,
	})
	for _, forbidden := range []string{"title", "artist", "album", "duration_ns", "lyrics", "cookie", "audio", "microphone"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("精简同步包仍包含禁止字段 %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func assertAllowedJSONKeys(t *testing.T, objectName string, object map[string]any, allowed map[string]bool) {
	t.Helper()
	for key := range object {
		if !allowed[key] {
			t.Fatalf("%s 包含非白名单字段 %q", objectName, key)
		}
	}
}

func TestMetadataSanitizesUnknownLyricMode(t *testing.T) {
	track, metadata := sanitizeMetadata(
		lyrics.NowPlaying{Source: "qq", Title: "歌曲"},
		Metadata{TrackID: " 0039MnYb0qxYhV ", LyricMode: LyricMode("karaoke-binary")},
	)
	if track.Source != "qq" || metadata.Source != "qq" || metadata.TrackID != "0039MnYb0qxYhV" {
		t.Fatalf("元数据清理错误: track=%#v metadata=%#v", track, metadata)
	}
	if metadata.LyricMode != LyricModeUnknown {
		t.Fatalf("未知歌词模式未降级: %q", metadata.LyricMode)
	}
}

func TestSupportedLyricModesArePreserved(t *testing.T) {
	for _, mode := range []LyricMode{LyricModeTimed, LyricModePlain, LyricModeUnknown} {
		_, metadata := sanitizeMetadata(lyrics.NowPlaying{Source: "qq"}, Metadata{LyricMode: mode})
		if metadata.LyricMode != mode {
			t.Fatalf("歌词模式 %q 被错误改写为 %q", mode, metadata.LyricMode)
		}
	}
}

func TestAuthenticationRunsBeforeMetadataSource(t *testing.T) {
	var calls atomic.Int32
	server, err := New(Config{Token: testToken}, SourceFunc(func(context.Context) (lyrics.NowPlaying, error) {
		calls.Add(1)
		return lyrics.NowPlaying{Title: "不应读取"}, nil
	}))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, NowPlayingPath, nil),
		httptest.NewRequest(http.MethodGet, NowPlayingPath+"?token="+testToken, nil),
	}
	wrong := httptest.NewRequest(http.MethodGet, NowPlayingPath, nil)
	wrong.Header.Set("Authorization", "Bearer "+strings.Repeat("f", len(testToken)))
	requests = append(requests, wrong)
	for _, request := range requests {
		request.RemoteAddr = "192.0.2.20:4000"
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("未授权请求状态码 = %d", recorder.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("认证前读取了播放器状态 %d 次", calls.Load())
	}
}

func TestControlAPIRoutesAreNotPresent(t *testing.T) {
	var calls atomic.Int32
	server, err := New(Config{Token: testToken}, SourceFunc(func(context.Context) (lyrics.NowPlaying, error) {
		calls.Add(1)
		return lyrics.NowPlaying{}, nil
	}))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	for _, path := range []string{"/", "/api/status", "/api/config", "/api/theme", "/v1/now-playing/"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, path, "192.0.2.30:4000"))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("不应存在的路由 %q 返回 %d", path, recorder.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("访问无关路由仍读取了播放器状态 %d 次", calls.Load())
	}
}

func TestRateLimitIsPerRemoteAddress(t *testing.T) {
	var calls atomic.Int32
	server, err := New(Config{
		Token: testToken, RequestsPerSecond: 0.001, Burst: 1, ClientTTL: time.Minute,
	}, SourceFunc(func(context.Context) (lyrics.NowPlaying, error) {
		calls.Add(1)
		return lyrics.NowPlaying{Title: "测试歌曲"}, nil
	}))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.40:4000"))
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.40:5000"))
	other := httptest.NewRecorder()
	server.Handler().ServeHTTP(other, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.41:4000"))
	if first.Code != http.StatusOK || second.Code != http.StatusTooManyRequests || other.Code != http.StatusOK {
		t.Fatalf("限流结果异常: first=%d second=%d other=%d", first.Code, second.Code, other.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("限流响应缺少 Retry-After")
	}
	if calls.Load() != 2 {
		t.Fatalf("限流请求仍访问了元数据源，调用次数 = %d", calls.Load())
	}
}

func TestSourceTimeoutIsBounded(t *testing.T) {
	server, err := New(Config{Token: testToken, SourceTimeout: 20 * time.Millisecond}, SourceFunc(func(ctx context.Context) (lyrics.NowPlaying, error) {
		<-ctx.Done()
		return lyrics.NowPlaying{}, ctx.Err()
	}))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	started := time.Now()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.50:4000"))
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("超时状态码 = %d，响应 = %s", recorder.Code, recorder.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("元数据读取没有按时终止: %v", elapsed)
	}
}

func TestStuckSourceCannotAccumulateConcurrentReads(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	server, err := New(Config{Token: testToken, SourceTimeout: 20 * time.Millisecond}, SourceFunc(func(context.Context) (lyrics.NowPlaying, error) {
		calls.Add(1)
		<-release
		return lyrics.NowPlaying{}, nil
	}))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	for index := 0; index < 2; index++ {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.55:4000"))
		if recorder.Code != http.StatusGatewayTimeout {
			t.Fatalf("第 %d 个阻塞请求状态码 = %d", index+1, recorder.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("阻塞源被并发调用 %d 次", calls.Load())
	}
	close(release)
}

func TestSourceErrorIsNotExposed(t *testing.T) {
	server, err := New(Config{Token: testToken}, SourceFunc(func(context.Context) (lyrics.NowPlaying, error) {
		return lyrics.NowPlaying{}, errors.New(`C:\\secret\\player.db 读取失败`)
	}))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.60:4000"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("错误状态码 = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), "player.db") {
		t.Fatalf("响应泄漏了内部错误: %s", recorder.Body.String())
	}
}

func TestNewRejectsWeakTokenAndInvalidLimits(t *testing.T) {
	source := SourceFunc(func(context.Context) (lyrics.NowPlaying, error) { return lyrics.NowPlaying{}, nil })
	if _, err := New(Config{Token: "short"}, source); err == nil {
		t.Fatal("短令牌未被拒绝")
	}
	if _, err := New(Config{Token: testToken, RequestsPerSecond: 101}, source); err == nil {
		t.Fatal("异常速率未被拒绝")
	}
	if _, err := New(Config{Token: testToken}, nil); err == nil {
		t.Fatal("空元数据源未被拒绝")
	}
	if _, err := New(Config{Token: testToken}, SourceFunc(nil)); err == nil {
		t.Fatal("空函数元数据源未被拒绝")
	}
	if _, err := NewWithMetadata(Config{Token: testToken}, nil); err == nil {
		t.Fatal("空扩展元数据源未被拒绝")
	}
	if _, err := NewWithMetadata(Config{Token: testToken}, MetadataSourceFunc(nil)); err == nil {
		t.Fatal("空扩展元数据函数未被拒绝")
	}
}
