package edgesync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"casescreen/internal/lyrics"
)

const clientTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestClientReadsSmallMetadataWithBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != NowPlayingPath {
			t.Fatalf("请求路径错误: %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+clientTestToken {
			t.Fatal("没有发送配对令牌")
		}
		if request.URL.Query().Get("compact") != "1" {
			t.Fatalf("屏端没有请求精简高频包: %s", request.URL.RawQuery)
		}
		_ = json.NewEncoder(writer).Encode(Envelope{
			ProtocolVersion: ProtocolVersion,
			SampledAtUnixMS: time.Now().UnixMilli(),
			Available:       true,
			AuthRequired:    true,
			Metadata:        Metadata{Source: "netease", TrackID: "186016", LyricMode: LyricModeTimed},
			Track:           lyrics.NowPlaying{Title: "晴天", Position: int64(time.Second), PositionKnown: true, Playing: true},
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, clientTestToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := client.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Metadata.TrackID != "186016" || envelope.Track.Title != "晴天" {
		t.Fatalf("元数据不匹配: %#v", envelope)
	}
}

func TestClientNeverLeaksTokenInErrors(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:1", clientTestToken, &http.Client{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Current(context.Background())
	if err == nil {
		t.Fatal("不可连接地址应返回错误")
	}
	if strings.Contains(err.Error(), clientTestToken) {
		t.Fatal("错误信息泄露了配对令牌")
	}
}

func TestClientRejectsRedirectAndOversizedResponse(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, "http://example.invalid/steal", http.StatusFound)
		}))
		defer server.Close()
		client, err := NewClient(server.URL, clientTestToken, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Current(context.Background()); err == nil {
			t.Fatal("重定向不应被接受")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_, _ = writer.Write([]byte(strings.Repeat("x", maximumSnapshotBytes+1)))
		}))
		defer server.Close()
		client, err := NewClient(server.URL, clientTestToken, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Current(context.Background()); err == nil || !strings.Contains(err.Error(), "32 KiB") {
			t.Fatalf("超大响应错误不正确: %v", err)
		}
	})
}

func TestNewClientRejectsCredentialsInURL(t *testing.T) {
	if _, err := NewClient("http://user:password@127.0.0.1:19322", clientTestToken, nil); err == nil {
		t.Fatal("服务地址中不得嵌入账号密码")
	}
}
