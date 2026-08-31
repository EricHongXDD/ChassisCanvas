package edgesync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"casescreen/internal/lyrics"
)

func TestLyricsRelayUsesExactSamePlatformIDAndKeepsNowPlayingSmall(t *testing.T) {
	var received Metadata
	server, err := NewWithMetadataAndLyrics(
		Config{Token: testToken},
		MetadataSourceFunc(func(context.Context) (lyrics.NowPlaying, Metadata, error) {
			return lyrics.NowPlaying{Title: "晴天", Artist: "周杰伦", PositionKnown: true},
				Metadata{Source: "netease", TrackID: "186016", LyricMode: LyricModeTimed}, nil
		}),
		LyricsSourceFunc(func(_ context.Context, metadata Metadata) (LyricsResult, error) {
			received = metadata
			return LyricsResult{Mode: LyricModeTimed, Document: lyrics.Document{
				Metadata: map[string]string{"private_debug": "must-be-removed"},
				Lines:    []lyrics.Line{{At: 0, Text: "第一句"}, {At: 2 * time.Second, Text: "第二句"}},
			}}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	nowRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(nowRecorder, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.44:3000"))
	if nowRecorder.Code != http.StatusOK || nowRecorder.Body.Len() > 1024 || strings.Contains(nowRecorder.Body.String(), "第一句") {
		t.Fatalf("高频当前歌曲包不再是小包: %d %s", nowRecorder.Code, nowRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet,
		LyricsPath+"?source=netease&track_id=186016", "192.0.2.44:3000"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("精确歌词请求失败: %d %s", recorder.Code, recorder.Body.String())
	}
	if received.Source != "netease" || received.TrackID != "186016" {
		t.Fatalf("没有按同平台精确 ID 查询: %+v", received)
	}
	if !strings.Contains(recorder.Body.String(), "第一句") || strings.Contains(recorder.Body.String(), "private_debug") {
		t.Fatalf("歌词响应内容或脱敏错误: %s", recorder.Body.String())
	}
}

func TestLyricsRelayClientAndLoginRequired(t *testing.T) {
	needsLogin := false
	server, err := NewWithMetadataAndLyrics(
		Config{Token: testToken},
		MetadataSourceFunc(func(context.Context) (lyrics.NowPlaying, Metadata, error) {
			return lyrics.NowPlaying{}, Metadata{}, nil
		}),
		LyricsSourceFunc(func(_ context.Context, metadata Metadata) (LyricsResult, error) {
			if needsLogin {
				return LyricsResult{}, ErrPlatformLoginRequired
			}
			return LyricsResult{Mode: LyricModePlain, PlainText: "整首纯文本歌词"}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := NewClient(httpServer.URL, testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ExactLyrics(context.Background(), Metadata{Source: "qq", TrackID: "0039MnYb0qxYhV"})
	if err != nil || result.Mode != LyricModePlain || result.PlainText != "整首纯文本歌词" {
		t.Fatalf("一次性纯文本歌词错误: result=%+v err=%v", result, err)
	}
	needsLogin = true
	_, err = client.ExactLyrics(context.Background(), Metadata{Source: "qq", TrackID: "0039MnYb0qxYhV"})
	if !errors.Is(err, ErrPlatformLoginRequired) {
		t.Fatalf("没有把平台登录要求传给屏端: %v", err)
	}
}

func TestLyricsRelayRejectsMissingForeignOrMalformedIDsBeforeSource(t *testing.T) {
	calls := 0
	server, err := NewWithMetadataAndLyrics(
		Config{Token: testToken},
		MetadataSourceFunc(func(context.Context) (lyrics.NowPlaying, Metadata, error) {
			return lyrics.NowPlaying{}, Metadata{}, nil
		}),
		LyricsSourceFunc(func(context.Context, Metadata) (LyricsResult, error) {
			calls++
			return LyricsResult{}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		LyricsPath + "?source=qq&track_id=186016",
		LyricsPath + "?source=netease&track_id=0039MnYb0qxYhV",
		LyricsPath + "?source=other&track_id=123456",
		LyricsPath + "?source=qq",
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, target, "192.0.2.45:3000"))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("无效精确 ID 没有拒绝: %s => %d", target, recorder.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("无效 ID 仍调用了平台源: %d", calls)
	}
}
