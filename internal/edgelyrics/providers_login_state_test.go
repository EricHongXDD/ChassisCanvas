package edgelyrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type recordingLoginStateSession struct {
	markedPlatform string
	markCalls      int
}

func (*recordingLoginStateSession) Authorize(context.Context, string, *http.Request) error {
	return nil
}

func (session *recordingLoginStateSession) MarkNeedsLogin(platform string) error {
	session.markedPlatform = platform
	session.markCalls++
	return nil
}

func TestDirectHTTPReportsPlatformLoginRequirementToSessionStore(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "http_401", statusCode: http.StatusUnauthorized, body: `{}`},
		{name: "http_200_login", statusCode: http.StatusOK, body: `{"code":301,"message":"需要登录"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			session := &recordingLoginStateSession{}
			_, err := exactGet(context.Background(), server.Client(), server.URL, "", "netease", session)
			if !errors.Is(err, ErrLoginRequired) {
				t.Fatalf("没有保留登录/VIP sentinel: %v", err)
			}
			if session.markCalls != 1 || session.markedPlatform != "netease" {
				t.Fatalf("没有把 needs_login 回报给对应平台: calls=%d platform=%q", session.markCalls, session.markedPlatform)
			}
		})
	}
}
