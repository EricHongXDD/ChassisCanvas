package edgepush

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultPublisherTimeout = 5 * time.Second
	maximumResponseBytes    = 1 << 10
)

// NativePlayback 表示播放平台直接给出的歌曲身份和时间轴。字段有意不导出，
// 防止把 SMTC、窗口标题或歌词搜索补全出的 TrackID 直接转换为“原生 ID”。
// 只有明确的平台原生适配器才能通过 QQNativePlayback 或
// NeteaseNativePlayback 构造此值。
type NativePlayback struct {
	update Update
}

// QQNativePlayback 接受 QQ 音乐播放来源直接给出的 songmid。调用方必须保证
// songmid 来自当前播放对象本身，而不是标题搜索、候选匹配或跨平台映射。
func QQNativePlayback(songmid string, position, duration time.Duration, playing bool) (NativePlayback, error) {
	return newNativePlayback("qq", songmid, position, duration, playing)
}

// NeteaseNativePlayback 接受网易云播放来源直接给出的数字歌曲 ID。调用方必须
// 保证 ID 来自当前播放对象本身，而不是 playingList 的元数据匹配结果。
func NeteaseNativePlayback(trackID string, position, duration time.Duration, playing bool) (NativePlayback, error) {
	return newNativePlayback("netease", trackID, position, duration, playing)
}

func newNativePlayback(source, trackID string, position, duration time.Duration, playing bool) (NativePlayback, error) {
	if position < 0 || duration < 0 {
		return NativePlayback{}, errors.New("原生播放位置和时长不能为负数")
	}
	update := Update{
		Version: ProtocolVersion, Available: true, Source: source, TrackID: trackID,
		PositionMS: int64(position / time.Millisecond), DurationMS: int64(duration / time.Millisecond),
		PositionKnown: true, Playing: playing,
	}
	if err := update.Validate(); err != nil {
		return NativePlayback{}, err
	}
	return NativePlayback{update: update}, nil
}

// Source 返回严格的平台标识，不返回标题、歌手或其他可用于搜索的字段。
func (playback NativePlayback) Source() string { return playback.update.Source }

// TrackID 返回播放来源声明的原生平台歌曲 ID。
func (playback NativePlayback) TrackID() string { return playback.update.TrackID }

// PublisherConfig 配置来源 companion 到屏端接收器的固定 HTTP 通道。
type PublisherConfig struct {
	BaseURL    string
	PairingKey string
	HTTPClient *http.Client
	Random     io.Reader
}

// Publisher 建立 challenge/session 后按严格递增序号发送小包。它不读取播放器，
// 不获取歌词，也不接收标题或登录态。
type Publisher struct {
	challengeEndpoint string
	sessionEndpoint   string
	stateEndpoint     string
	pairingKey        string
	http              *http.Client
	random            io.Reader

	mu       sync.Mutex
	session  string
	sequence uint64
}

// NewPublisher 创建来源发送器。基础地址不能包含账号、查询参数或片段；认证头
// 永不跟随重定向，避免把签名上下文交给其他主机。
func NewPublisher(config PublisherConfig) (*Publisher, error) {
	if _, err := decodePairingKey(config.PairingKey); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("屏端播放来源接收地址无效")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("屏端播放来源接收地址不能包含账号、查询参数或片段")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	endpoint := func(path string) string {
		copyURL := *parsed
		copyURL.Path = basePath + path
		return copyURL.String()
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultPublisherTimeout}
	}
	copyClient := *client
	if copyClient.Timeout <= 0 {
		copyClient.Timeout = defaultPublisherTimeout
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	randomSource := config.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	return &Publisher{
		challengeEndpoint: endpoint(ChallengePath), sessionEndpoint: endpoint(SessionPath),
		stateEndpoint: endpoint(StatePath), pairingKey: config.PairingKey,
		http: &copyClient, random: randomSource,
	}, nil
}

// Send 发送一份由平台原生适配器构造的播放状态。网络响应丢失时，下一次发送
// 会复用尚未确认的序号；接收器对相同正文提供幂等处理。
func (publisher *Publisher) Send(ctx context.Context, playback NativePlayback) error {
	if err := playback.update.Validate(); err != nil {
		return fmt.Errorf("原生播放状态无效: %w", err)
	}
	return publisher.send(ctx, playback.update)
}

// Clear 通知屏端当前没有可用歌曲；该清空包同样经过 HMAC 和序号保护。
func (publisher *Publisher) Clear(ctx context.Context) error {
	return publisher.send(ctx, Update{Version: ProtocolVersion})
}

func (publisher *Publisher) send(ctx context.Context, update Update) error {
	if publisher == nil || publisher.http == nil {
		return errors.New("来源发送器尚未初始化")
	}
	if ctx == nil {
		return errors.New("来源发送上下文不能为空")
	}
	if err := update.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(update)
	if err != nil {
		return errors.New("编码播放状态失败")
	}
	if len(body) > maximumRequestBytes {
		return errors.New("播放状态超过 1 KiB")
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if publisher.session == "" {
			if err := publisher.openSession(ctx); err != nil {
				return err
			}
		}
		sequence := publisher.sequence + 1
		status, err := publisher.putState(ctx, publisher.session, sequence, body)
		if err != nil {
			return err
		}
		if status == http.StatusNoContent {
			publisher.sequence = sequence
			return nil
		}
		if status == http.StatusUnauthorized || status == http.StatusConflict {
			// 屏端重启、会话超时或另一来源显式接管后，重新进行 challenge。
			publisher.session = ""
			publisher.sequence = 0
			continue
		}
		return fmt.Errorf("屏端拒绝播放状态: HTTP %d", status)
	}
	return errors.New("屏端来源会话无法恢复")
}

func (publisher *Publisher) openSession(ctx context.Context) error {
	challenge, err := publisher.fetchChallenge(ctx)
	if err != nil {
		return err
	}
	randomSession := make([]byte, sessionBytes)
	if _, err := io.ReadFull(publisher.random, randomSession); err != nil {
		return errors.New("无法生成来源会话 ID")
	}
	session := base64.RawURLEncoding.EncodeToString(randomSession)
	command := SessionRequest{Version: ProtocolVersion, Challenge: challenge.Challenge, Session: session}
	body, err := json.Marshal(command)
	if err != nil {
		return errors.New("编码来源会话失败")
	}
	signature, err := SessionSignature(publisher.pairingKey, body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, publisher.sessionEndpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("创建来源会话请求失败")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CaseScreen-Source/1")
	request.Header.Set(HeaderMAC, signature)
	response, err := publisher.http.Do(request)
	if err != nil {
		return errors.New("建立屏端来源会话失败: 网络连接失败")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes))
		return fmt.Errorf("建立屏端来源会话失败: HTTP %d", response.StatusCode)
	}
	publisher.session = session
	publisher.sequence = 0
	return nil
}

func (publisher *Publisher) fetchChallenge(ctx context.Context) (ChallengeResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, publisher.challengeEndpoint, nil)
	if err != nil {
		return ChallengeResponse{}, errors.New("创建屏端挑战请求失败")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CaseScreen-Source/1")
	response, err := publisher.http.Do(request)
	if err != nil {
		return ChallengeResponse{}, errors.New("获取屏端挑战失败: 网络连接失败")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes))
		return ChallengeResponse{}, fmt.Errorf("获取屏端挑战失败: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil || len(data) > maximumResponseBytes {
		return ChallengeResponse{}, errors.New("屏端挑战响应无效")
	}
	var challenge ChallengeResponse
	if decodeStrict(data, &challenge) != nil || challenge.Version != ProtocolVersion || !validChallenge(challenge.Challenge) {
		return ChallengeResponse{}, errors.New("屏端挑战响应无效")
	}
	return challenge, nil
}

func (publisher *Publisher) putState(ctx context.Context, session string, sequence uint64, body []byte) (int, error) {
	signature, err := StateSignature(publisher.pairingKey, session, sequence, body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, publisher.stateEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("创建播放状态请求失败")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CaseScreen-Source/1")
	request.Header.Set(HeaderSession, session)
	request.Header.Set(HeaderSequence, fmt.Sprintf("%d", sequence))
	request.Header.Set(HeaderMAC, signature)
	response, err := publisher.http.Do(request)
	if err != nil {
		return 0, errors.New("发送播放状态失败: 网络连接失败")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes))
	return response.StatusCode, nil
}

func (publisher *Publisher) String() string {
	if publisher == nil {
		return "edgepush.Publisher<nil>"
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	return fmt.Sprintf("edgepush.Publisher{paired:%t sequence:%d}", publisher.session != "", publisher.sequence)
}
