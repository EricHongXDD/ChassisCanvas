package edge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"casescreen/internal/edgelyrics"
	"casescreen/internal/edgesync"
	"casescreen/internal/lyrics"
)

type standaloneTestSource struct {
	mu    sync.Mutex
	track lyrics.NowPlaying
	err   error
}

func (source *standaloneTestSource) Current(context.Context) (lyrics.NowPlaying, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.track, source.err
}

func (source *standaloneTestSource) setTrack(track lyrics.NowPlaying) {
	source.mu.Lock()
	source.track = track
	source.mu.Unlock()
}

type standaloneTestIdentityProvider struct {
	id       string
	document lyrics.Document
	err      error
	calls    int
}

func (provider *standaloneTestIdentityProvider) ID() string { return provider.id }

func (provider *standaloneTestIdentityProvider) Fetch(context.Context, lyrics.NowPlaying) (lyrics.Document, error) {
	provider.calls++
	return provider.document, provider.err
}

type standaloneTestExactProvider struct {
	id     string
	result edgelyrics.Result
	err    error
	calls  int
}

func (provider *standaloneTestExactProvider) ID() string { return provider.id }

func (provider *standaloneTestExactProvider) FetchExact(context.Context, edgelyrics.Metadata, edgelyrics.AuthSession) (edgelyrics.Result, error) {
	provider.calls++
	return provider.result, provider.err
}

func standaloneQQTrack() lyrics.NowPlaying {
	return lyrics.NowPlaying{
		Source: "QQMusic", Title: "准确歌曲", Artist: "准确歌手", Album: "准确专辑",
		Duration: int64(240 * time.Second), Position: int64(12 * time.Second),
		PositionKnown: true, Playing: true,
	}
}

func standaloneTimedDocument() lyrics.Document {
	return lyrics.Document{
		Metadata: map[string]string{"platform": "qq", "track_id": "0039MnYb0qxYhV"},
		Lines: []lyrics.Line{
			{At: 10 * time.Second, Text: "第一句"},
			{At: 20 * time.Second, Text: "第二句"},
		},
	}
}

func TestStandalonePipelineDoesNotInferPlatformIDFromMetadata(t *testing.T) {
	source := &standaloneTestSource{track: standaloneQQTrack()}
	identity := &standaloneTestIdentityProvider{id: "qq", document: standaloneTimedDocument()}
	metadata, _, err := newStandalonePipeline(StandaloneOptions{
		Source: source, Platform: "auto", IdentityCacheDir: t.TempDir(), LyricsCacheDir: t.TempDir(),
		IdentityRetry: time.Second,
	}, []lyrics.Provider{identity}, nil)
	if err != nil {
		t.Fatal(err)
	}

	envelope, err := metadata.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Metadata.Source != "qq" || envelope.Metadata.TrackID != "" ||
		envelope.Metadata.LyricMode != edgesync.LyricModeUnknown || identity.calls != 0 {
		t.Fatalf("退役的屏端链路不应按元数据猜平台 ID: metadata=%#v calls=%d", envelope.Metadata, identity.calls)
	}

	track := standaloneQQTrack()
	track.Position = int64(15 * time.Second)
	source.setTrack(track)
	second, err := metadata.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Metadata.TrackID != "" || identity.calls != 0 {
		t.Fatalf("同曲进度变化后仍不应启动身份搜索: id=%q calls=%d", second.Metadata.TrackID, identity.calls)
	}
}

func TestStandalonePipelineRefusesIncompleteIdentityEvidence(t *testing.T) {
	for _, missing := range []string{"artist", "album", "duration"} {
		t.Run(missing, func(t *testing.T) {
			track := standaloneQQTrack()
			switch missing {
			case "artist":
				track.Artist = ""
			case "album":
				track.Album = ""
			case "duration":
				track.Duration = 0
			}
			source := &standaloneTestSource{track: track}
			identity := &standaloneTestIdentityProvider{id: "qq", document: standaloneTimedDocument()}
			metadata, _, err := newStandalonePipeline(StandaloneOptions{
				Source: source, IdentityCacheDir: t.TempDir(), LyricsCacheDir: t.TempDir(),
				IdentityRetry: time.Second,
			}, []lyrics.Provider{identity}, nil)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := metadata.Current(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if envelope.Metadata.TrackID != "" || identity.calls != 0 {
				t.Fatalf("证据不完整时不应猜测歌曲 ID: metadata=%#v calls=%d", envelope.Metadata, identity.calls)
			}
		})
	}
}

func TestStandalonePipelineCanLabelNeteaseButCannotInventItsTrackID(t *testing.T) {
	track := standaloneQQTrack()
	track.Source = "bluetooth"
	document := standaloneTimedDocument()
	document.Metadata["platform"] = "netease"
	document.Metadata["track_id"] = "186016"
	identity := &standaloneTestIdentityProvider{id: "netease", document: document}
	metadata, _, err := newStandalonePipeline(StandaloneOptions{
		Source: &standaloneTestSource{track: track}, Platform: "netease",
		IdentityCacheDir: t.TempDir(), LyricsCacheDir: t.TempDir(), IdentityRetry: time.Second,
	}, []lyrics.Provider{identity}, nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := metadata.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Metadata.Source != "netease" || envelope.Metadata.TrackID != "" || identity.calls != 0 {
		t.Fatalf("固定平台只应标记来源，不能猜歌曲 ID: metadata=%#v calls=%d", envelope.Metadata, identity.calls)
	}
}

func TestStandaloneIdentitySearchRemainsDisabledAcrossRetryWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	identity := &standaloneTestIdentityProvider{id: "qq", err: errors.New("临时网络错误")}
	metadata, _, err := newStandalonePipeline(StandaloneOptions{
		Source: &standaloneTestSource{track: standaloneQQTrack()}, IdentityCacheDir: t.TempDir(),
		LyricsCacheDir: t.TempDir(), IdentityRetry: 10 * time.Second,
	}, []lyrics.Provider{identity}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := metadata.(*standaloneMetadataClient)
	client.now = func() time.Time { return now }
	for range 2 {
		if _, err := client.Current(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if identity.calls != 0 {
		t.Fatalf("退役链路在退避窗口内启动了身份搜索: %d", identity.calls)
	}
	now = now.Add(11 * time.Second)
	if _, err := client.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if identity.calls != 0 {
		t.Fatalf("退避到期后启动了禁止的身份搜索: %d", identity.calls)
	}
}
