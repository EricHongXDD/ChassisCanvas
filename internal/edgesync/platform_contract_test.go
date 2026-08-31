package edgesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"casescreen/internal/lyrics"
)

func TestRelayRejectsMalformedIDsBeforeLyricsSource(t *testing.T) {
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
		LyricsPath + "?source=qq&track_id=0039-MnYb0qxYhV",
		LyricsPath + "?source=netease&track_id=%2B186016",
		LyricsPath + "?source=netease&track_id=0186016",
		LyricsPath + "?source=netease&track_id=9223372036854775808",
		LyricsPath + "?source=netease&track_id=" + strings.Repeat("9", 20),
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, target, "192.0.2.70:4000"))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("无效 ID 没有被中继拒绝: %s => %d %s", target, recorder.Code, recorder.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("无效 ID 到达歌词源: %d", calls)
	}
}

func TestNowPlayingDropsMalformedOrCrossPlatformTrackID(t *testing.T) {
	for _, metadata := range []Metadata{
		{Source: "qq", TrackID: "186016", LyricMode: LyricModeTimed},
		{Source: "netease", TrackID: "0039MnYb0qxYhV", LyricMode: LyricModeTimed},
		{Source: "unknown", TrackID: "0039MnYb0qxYhV", LyricMode: LyricModeTimed},
	} {
		server, err := NewWithMetadata(Config{Token: testToken}, MetadataSourceFunc(
			func(context.Context) (lyrics.NowPlaying, Metadata, error) {
				return lyrics.NowPlaying{Title: "歌曲", Source: metadata.Source, TrackID: metadata.TrackID}, metadata, nil
			},
		))
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, NowPlayingPath, "192.0.2.71:4000"))
		if recorder.Code != http.StatusOK {
			t.Fatalf("当前歌曲请求失败: %d %s", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), metadata.TrackID) || strings.Contains(recorder.Body.String(), `"track_id"`) {
			t.Fatalf("畸形/跨平台 ID 进入了高频小包: %s", recorder.Body.String())
		}
	}
}
