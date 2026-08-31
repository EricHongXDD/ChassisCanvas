package edgesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maximumSnapshotBytes = 32 << 10

// Client 是副屏使用的只读元数据客户端。它只请求固定的当前歌曲路由，
// 不会访问桌面控制 API，也不会上传歌词、音频或平台登录态。
type Client struct {
	endpoint       string
	lyricsEndpoint string
	token          string
	http           *http.Client
}

// NewClient 创建局域网元数据客户端。配对令牌只保存在内存中，错误信息不会回显它。
func NewClient(baseURL, token string, client *http.Client) (*Client, error) {
	if _, _, err := tokenDigest(token); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return nil, errors.New("边缘同步服务地址无效")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("边缘同步服务地址仅支持 HTTP 或 HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("边缘同步服务地址不能包含账号、查询参数或片段")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	nowPlayingURL := *parsed
	nowPlayingURL.Path = basePath + NowPlayingPath
	// 屏端每秒轮询只需要平台身份、播放锚点和歌词模式。显式请求精简响应，
	// 避免重复传输标题、歌手、专辑、时长以及第二份 source/track_id。
	nowPlayingQuery := nowPlayingURL.Query()
	nowPlayingQuery.Set("compact", "1")
	nowPlayingURL.RawQuery = nowPlayingQuery.Encode()
	lyricsURL := *parsed
	lyricsURL.Path = basePath + LyricsPath
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	copyClient := *client
	if copyClient.Timeout <= 0 {
		copyClient.Timeout = 5 * time.Second
	}
	// Bearer 令牌不能跟随重定向发送到其他主机；固定接口本身也不需要重定向。
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		endpoint: nowPlayingURL.String(), lyricsEndpoint: lyricsURL.String(),
		token: token, http: &copyClient,
	}, nil
}

// Current 获取一个小型当前歌曲快照。
func (client *Client) Current(ctx context.Context) (Envelope, error) {
	if client == nil || client.http == nil {
		return Envelope{}, errors.New("边缘同步客户端尚未初始化")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.endpoint, nil)
	if err != nil {
		return Envelope{}, errors.New("创建边缘同步请求失败")
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return Envelope{}, fmt.Errorf("读取当前歌曲失败: %w", redactURLError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return Envelope{}, errors.New("边缘同步配对令牌无效")
		case http.StatusTooManyRequests:
			return Envelope{}, errors.New("边缘同步请求过于频繁")
		default:
			return Envelope{}, fmt.Errorf("边缘同步服务暂不可用: HTTP %d", response.StatusCode)
		}
	}
	limited := io.LimitReader(response.Body, maximumSnapshotBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return Envelope{}, errors.New("读取边缘同步响应失败")
	}
	if len(data) > maximumSnapshotBytes {
		return Envelope{}, errors.New("边缘同步响应超过 32 KiB，已拒绝")
	}
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Envelope{}, errors.New("边缘同步响应不是有效 JSON")
	}
	if envelope.ProtocolVersion != ProtocolVersion {
		return Envelope{}, fmt.Errorf("不支持的边缘同步协议版本: %d", envelope.ProtocolVersion)
	}
	return envelope, nil
}

// ExactLyrics 仅在切歌且本地缓存未命中时请求一次完整歌词。
func (client *Client) ExactLyrics(ctx context.Context, metadata Metadata) (LyricsResult, error) {
	if client == nil || client.http == nil {
		return LyricsResult{}, errors.New("边缘同步客户端尚未初始化")
	}
	metadata, err := validateExactLyricsMetadata(metadata.Source, metadata.TrackID)
	if err != nil {
		return LyricsResult{}, err
	}
	endpoint, err := url.Parse(client.lyricsEndpoint)
	if err != nil {
		return LyricsResult{}, errors.New("精确歌词服务地址无效")
	}
	query := endpoint.Query()
	query.Set("source", metadata.Source)
	query.Set("track_id", metadata.TrackID)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return LyricsResult{}, errors.New("创建精确歌词请求失败")
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return LyricsResult{}, fmt.Errorf("读取精确歌词失败: %w", redactURLError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return LyricsResult{}, errors.New("边缘同步配对令牌无效")
		case http.StatusFailedDependency:
			return LyricsResult{}, ErrPlatformLoginRequired
		case http.StatusTooManyRequests:
			return LyricsResult{}, errors.New("精确歌词请求过于频繁")
		default:
			return LyricsResult{}, errors.New("该歌曲歌词暂不可用")
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumLyricsRelayBytes+1))
	if err != nil {
		return LyricsResult{}, errors.New("读取精确歌词响应失败")
	}
	if len(data) > maximumLyricsRelayBytes {
		return LyricsResult{}, errors.New("精确歌词响应超过 4 MiB，已拒绝")
	}
	var envelope struct {
		ProtocolVersion int          `json:"protocol_version"`
		Source          string       `json:"source"`
		TrackID         string       `json:"track_id"`
		Lyrics          LyricsResult `json:"lyrics"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return LyricsResult{}, errors.New("精确歌词响应不是有效 JSON")
	}
	if envelope.ProtocolVersion != ProtocolVersion || envelope.Source != metadata.Source || envelope.TrackID != metadata.TrackID {
		return LyricsResult{}, errors.New("精确歌词响应的协议或歌曲身份不匹配")
	}
	return sanitizeLyricsResult(envelope.Lyrics)
}

// redactURLError 丢弃可能携带 URL 或请求头上下文的底层错误文本。
func redactURLError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		if urlError.Err != nil {
			return errors.New("网络连接失败")
		}
	}
	return errors.New("网络连接失败")
}
