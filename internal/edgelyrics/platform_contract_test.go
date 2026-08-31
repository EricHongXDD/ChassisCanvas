package edgelyrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestExactProvidersMapHTTP200LoginOrVIPBodyToSentinel(t *testing.T) {
	tests := []struct {
		name    string
		trackID string
		body    string
	}{
		{name: "qq", trackID: "0039MnYb0qxYhV", body: `{"code":1000,"message":"请先登录 QQ 音乐"}`},
		{name: "netease", trackID: "186016", body: `{"code":200,"need_vip":true,"message":"该歌曲需要会员权限"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			var err error
			switch test.name {
			case "qq":
				_, err = (QQExactProvider{BaseURL: server.URL}).FetchExact(
					context.Background(), Metadata{Source: "qq", TrackID: test.trackID}, nil)
			case "netease":
				_, err = (NeteaseExactProvider{BaseURL: server.URL}).FetchExact(
					context.Background(), Metadata{Source: "netease", TrackID: test.trackID}, nil)
			}
			if !errors.Is(err, ErrLoginRequired) {
				t.Fatalf("HTTP 200 登录/VIP 错误没有映射为 ErrLoginRequired: %v", err)
			}
		})
	}
}

func TestExactResolverPreservesLoginRequiredErrorsIs(t *testing.T) {
	mutex := &sync.Mutex{}
	calls := map[string]int{}
	order := []string{}
	resolver := newExactResolver("", nil, recordingExactProvider{
		id: "qq", err: ErrLoginRequired, mu: mutex, calls: &calls, order: &order,
	})
	_, _, err := resolver.Resolve(context.Background(), Metadata{Source: "qq", TrackID: "0039MnYb0qxYhV"})
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("精确解析器丢失 ErrLoginRequired 错误链: %v", err)
	}
}

func TestExactProvidersRejectMalformedIDsBeforeHTTP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	for _, trackID := range []string{"186016", "short", "0039-MnYb0qxYhV", strings.Repeat("A", 33)} {
		_, err := (QQExactProvider{BaseURL: server.URL}).FetchExact(
			context.Background(), Metadata{Source: "qq", TrackID: trackID}, nil)
		if err == nil || !strings.Contains(err.Error(), "ID 无效") {
			t.Fatalf("畸形 QQ songmid 没有拒绝: id=%q err=%v", trackID, err)
		}
	}
	for _, trackID := range []string{"0039MnYb0qxYhV", "+186016", "0186016", "0", "9223372036854775808", strings.Repeat("9", 20)} {
		_, err := (NeteaseExactProvider{BaseURL: server.URL}).FetchExact(
			context.Background(), Metadata{Source: "netease", TrackID: trackID}, nil)
		if err == nil || !strings.Contains(err.Error(), "ID 无效") {
			t.Fatalf("畸形网易云 ID 没有拒绝: id=%q err=%v", trackID, err)
		}
	}
	if calls != 0 {
		t.Fatalf("无效平台 ID 到达了 HTTP 层: %d", calls)
	}
}
