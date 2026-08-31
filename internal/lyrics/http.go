package lyrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"casescreen/internal/platformstatus"
)

const maximumLyricsResponse = 4 << 20

var ErrPlatformLoginRequired = errors.New("平台歌词需要有效的登录/VIP 会话")

// RequestAuthorizer 为 QQ 音乐、网易云音乐请求注入用户已导入的登录态。
// 实现不应接收账号密码，也不能向非平台主机发送凭据。
type RequestAuthorizer interface {
	Authorize(context.Context, string, *http.Request) error
}

type loginStateReporter interface {
	MarkNeedsLogin(string) error
}

func httpGet(ctx context.Context, client *http.Client, rawURL, referer string) ([]byte, error) {
	return httpGetAuthorized(ctx, client, rawURL, referer, "", nil)
}

func httpGetAuthorized(ctx context.Context, client *http.Client, rawURL, referer, platform string, session RequestAuthorizer) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) CaseScreen/0.2")
	request.Header.Set("Accept", "application/json,text/plain,*/*")
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	if session != nil {
		if err := session.Authorize(ctx, platform, request); err != nil {
			return nil, fmt.Errorf("%s 登录态不可用: %w", platform, err)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		reportNeedsLogin(session, platform)
		return nil, fmt.Errorf("%w: %s HTTP %s", ErrPlatformLoginRequired, platform, response.Status)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumLyricsResponse+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximumLyricsResponse {
		return nil, errors.New("歌词响应超过 4 MiB")
	}
	if platformstatus.RequiresLogin(unwrapJSONP(data)) {
		reportNeedsLogin(session, platform)
		return nil, fmt.Errorf("%w: %s 平台返回登录或会员限制", ErrPlatformLoginRequired, platform)
	}
	return data, nil
}

func reportNeedsLogin(session RequestAuthorizer, platform string) {
	if reporter, ok := session.(loginStateReporter); ok {
		// 状态回报失败不能掩盖平台已经返回的登录/VIP错误。
		_ = reporter.MarkNeedsLogin(platform)
	}
}

func unwrapJSONP(data []byte) []byte {
	start := -1
	end := -1
	for index, value := range data {
		if value == '{' && start < 0 {
			start = index
		}
		if value == '}' {
			end = index + 1
		}
	}
	if start >= 0 && end > start {
		return data[start:end]
	}
	return data
}
