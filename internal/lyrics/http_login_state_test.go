package lyrics

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

func TestHostHTTPReportsPlatformLoginRequirementToSessionStore(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "http_403", statusCode: http.StatusForbidden, body: `{}`},
		{name: "http_200_vip", statusCode: http.StatusOK, body: `{"code":200,"needVip":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			session := &recordingLoginStateSession{}
			_, err := httpGetAuthorized(context.Background(), server.Client(), server.URL, "", "qq", session)
			if !errors.Is(err, ErrPlatformLoginRequired) {
				t.Fatalf("没有保留登录/VIP sentinel: %v", err)
			}
			if session.markCalls != 1 || session.markedPlatform != "qq" {
				t.Fatalf("没有把 needs_login 回报给对应平台: calls=%d platform=%q", session.markCalls, session.markedPlatform)
			}
		})
	}
}
