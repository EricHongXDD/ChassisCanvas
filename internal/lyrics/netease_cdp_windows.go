//go:build windows && casescreen_legacy_netease_cdp

package lyrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"casescreen/internal/playerhost"
)

const (
	neteaseCDPReadTimeout      = 900 * time.Millisecond
	neteaseCDPSnapshotLimit    = 16 << 10
	neteaseCDPMaxClockSkew     = 10 * time.Second
	neteaseCDPMaxTrackDuration = 24 * time.Hour
)

type neteaseCDPSource struct {
	reader playerhost.NeteasePlaybackReader
	clock  func() time.Time
}

type neteasePrioritySource struct {
	native   NowPlayingSource
	fallback NowPlayingSource
}

type neteaseCDPSnapshot struct {
	Error       string `json:"error,omitempty"`
	Empty       bool   `json:"empty,omitempty"`
	Stable      bool   `json:"stable"`
	SampledAtMS string `json:"sampledAtMs"`
	Status      string `json:"status"`
	Info        *struct {
		Name         string          `json:"name"`
		Artists      json.RawMessage `json:"artists"`
		Album        string          `json:"album"`
		ResourceType string          `json:"resourceType"`
		ResourceID   string          `json:"resourceId"`
		Duration     json.RawMessage `json:"duration"`
	} `json:"info"`
	Progress *struct {
		Current  json.RawMessage `json:"current"`
		Duration json.RawMessage `json:"duration"`
	} `json:"progress"`
}

func newNeteaseCDPSource(readers ...playerhost.NeteasePlaybackReader) *neteaseCDPSource {
	var reader playerhost.NeteasePlaybackReader
	if len(readers) > 0 {
		reader = readers[0]
	}
	return &neteaseCDPSource{reader: reader}
}

// newLegacyNeteaseCDPNowPlayingSource 只供显式 legacy 构建恢复旧组合。
// 默认 NewNowPlayingSource 不接收 host，也不会引用此路径。
func newLegacyNeteaseCDPNowPlayingSource(reader playerhost.NeteasePlaybackReader) NowPlayingSource {
	return &neteasePrioritySource{
		native: newNeteaseCDPSource(reader),
		fallback: &fallbackNowPlayingSource{
			smtc:   &smtcSource{},
			window: &windowSource{},
		},
	}
}

func (source *neteaseCDPSource) Current(ctx context.Context) (NowPlaying, error) {
	if source == nil || source.reader == nil {
		return NowPlaying{}, errors.New("网易云私有管道状态源未配置")
	}
	readContext, cancel := context.WithTimeout(ctx, neteaseCDPReadTimeout)
	defer cancel()
	payload, err := source.reader.ReadPlayback(readContext)
	if err != nil {
		return NowPlaying{}, fmt.Errorf("读取网易云官方客户端私有管道状态: %w", err)
	}
	now := time.Now()
	if source.clock != nil {
		now = source.clock()
	}
	track, err := parseNeteaseCDPSnapshot(payload, now)
	if err != nil {
		return NowPlaying{}, fmt.Errorf("校验网易云官方客户端当前播放状态: %w", err)
	}
	return track, nil
}

// Current 让网易云原生状态参与 Windows 播放源选择，但不会让一个已暂停
// 的网易云窗口盖住其他真正播放中的会话。私有管道缺失时，窗口标题或旧
// SMTC 会话不能冒充网易云完整时间轴。
func (source *neteasePrioritySource) Current(ctx context.Context) (NowPlaying, error) {
	var native NowPlaying
	var nativeErr error
	if source.native != nil {
		native, nativeErr = source.native.Current(ctx)
	}
	if nativeErr == nil && native.Valid() && native.Playing {
		return native, nil
	}

	var fallback NowPlaying
	var fallbackErr error
	if source.fallback != nil {
		fallback, fallbackErr = source.fallback.Current(ctx)
	}
	if nativeErr == nil && native.Valid() {
		if fallbackErr == nil && fallback.Valid() && fallback.Playing &&
			sourceProviderID(fallback.Source) != "netease" {
			return fallback, nil
		}
		return native, nil
	}
	if fallbackErr == nil && fallback.Valid() {
		if sourceProviderID(fallback.Source) == "netease" {
			if nativeErr != nil {
				return NowPlaying{}, fmt.Errorf("网易云客户端没有可验证的私有管道时间轴: %w", nativeErr)
			}
			return NowPlaying{}, nil
		}
		return fallback, nil
	}
	if fallbackErr != nil {
		return NowPlaying{}, fallbackErr
	}
	// 没有任何播放器证据时，私有管道未连接只是“当前无受支持播放源”，
	// 不把常驻错误显示给未使用网易云的用户。
	return NowPlaying{}, nil
}

// Close 只释放 fallback（例如 QQ 的只读进程句柄），共享的 NeteaseHost
// 由 main 统一关闭，避免歌词引擎误关 API 正在使用的私有管道。
func (source *neteasePrioritySource) Close() error {
	if source == nil || source.fallback == nil {
		return nil
	}
	if closer, ok := source.fallback.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func parseNeteaseCDPSnapshot(data []byte, now time.Time) (NowPlaying, error) {
	if len(bytes.TrimSpace(data)) == 0 || len(data) > neteaseCDPSnapshotLimit {
		return NowPlaying{}, errors.New("快照为空或超过长度限制")
	}
	var snapshot neteaseCDPSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return NowPlaying{}, fmt.Errorf("解析快照 JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return NowPlaying{}, errors.New("快照 JSON 含有多余内容")
	}
	if snapshot.Error != "" {
		return NowPlaying{}, errors.New("客户端没有暴露只读播放接口")
	}
	if snapshot.Empty {
		return NowPlaying{}, nil
	}
	if !snapshot.Stable {
		return NowPlaying{}, errors.New("播放状态正在切换，拒绝混合快照")
	}
	if snapshot.Info == nil || snapshot.Progress == nil {
		return NowPlaying{}, errors.New("快照缺少歌曲或播放进度")
	}
	if snapshot.Info.ResourceType != "track" {
		return NowPlaying{}, fmt.Errorf("当前资源不是歌曲: %q", snapshot.Info.ResourceType)
	}
	trackID := strings.TrimSpace(snapshot.Info.ResourceID)
	if _, ok := parseNeteaseSongID(trackID); !ok {
		return NowPlaying{}, errors.New("当前歌曲缺少有效的网易云数字 ID")
	}
	title, err := validateNeteaseCDPText("歌曲标题", snapshot.Info.Name, 512, true)
	if err != nil {
		return NowPlaying{}, err
	}
	album, err := validateNeteaseCDPText("专辑", snapshot.Info.Album, 512, false)
	if err != nil {
		return NowPlaying{}, err
	}
	artists, err := parseNeteaseCDPArtists(snapshot.Info.Artists)
	if err != nil {
		return NowPlaying{}, err
	}

	infoDurationMS, err := parseNeteaseCDPNumber(snapshot.Info.Duration, "歌曲时长")
	if err != nil {
		return NowPlaying{}, err
	}
	progressDurationMS, err := parseNeteaseCDPNumber(snapshot.Progress.Duration, "进度时长")
	if err != nil {
		return NowPlaying{}, err
	}
	currentSeconds, err := parseNeteaseCDPNumber(snapshot.Progress.Current, "播放位置")
	if err != nil {
		return NowPlaying{}, err
	}
	maximumDurationMS := float64(neteaseCDPMaxTrackDuration / time.Millisecond)
	if infoDurationMS <= 0 || infoDurationMS > maximumDurationMS ||
		progressDurationMS <= 0 || progressDurationMS > maximumDurationMS {
		return NowPlaying{}, errors.New("歌曲时长超出允许范围")
	}
	if math.Abs(infoDurationMS-progressDurationMS) > 1 {
		return NowPlaying{}, errors.New("歌曲信息与进度时长不一致")
	}
	durationSeconds := infoDurationMS / 1000
	if currentSeconds < 0 || currentSeconds > durationSeconds+2 {
		return NowPlaying{}, errors.New("播放位置超出歌曲时长")
	}
	if currentSeconds > durationSeconds {
		currentSeconds = durationSeconds
	}

	sampledAtMillis, err := strconv.ParseInt(snapshot.SampledAtMS, 10, 64)
	if err != nil || sampledAtMillis <= 0 {
		return NowPlaying{}, errors.New("状态采样时间无效")
	}
	sampledAt := time.UnixMilli(sampledAtMillis)
	clockDifference := now.Sub(sampledAt)
	if clockDifference < -neteaseCDPMaxClockSkew || clockDifference > neteaseCDPMaxClockSkew {
		return NowPlaying{}, errors.New("状态采样时间与主机时钟不一致")
	}
	playing := false
	switch snapshot.Status {
	case "Playing":
		playing = true
	case "Pause", "Stop", "End":
	default:
		return NowPlaying{}, fmt.Errorf("未知网易云播放状态: %q", snapshot.Status)
	}

	duration := time.Duration(math.Round(infoDurationMS * float64(time.Millisecond)))
	position := time.Duration(math.Round(currentSeconds * float64(time.Second)))
	return NowPlaying{
		Source:            "netease",
		TrackID:           trackID,
		TrackIDOrigin:     TrackIDOriginSource,
		Title:             title,
		Artist:            strings.Join(artists, " / "),
		Album:             album,
		Position:          int64(position),
		Duration:          int64(duration),
		PositionKnown:     true,
		Playing:           playing,
		PositionUpdatedAt: sampledAt,
	}, nil
}

func parseNeteaseCDPNumber(raw json.RawMessage, field string) (float64, error) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || len(value) > 64 || bytes.Equal(value, []byte("null")) || value[0] == '"' {
		return 0, fmt.Errorf("%s不是有限数字", field)
	}
	parsed, err := strconv.ParseFloat(string(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s不是有限数字", field)
	}
	return parsed, nil
}

func parseNeteaseCDPArtists(raw json.RawMessage) ([]string, error) {
	var values []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&values); err != nil {
		return nil, errors.New("歌手列表不是数组")
	}
	if len(values) > 16 {
		return nil, errors.New("歌手数量超过限制")
	}
	artists := make([]string, 0, len(values))
	for _, rawValue := range values {
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			var object struct {
				Name string `json:"name"`
			}
			objectDecoder := json.NewDecoder(bytes.NewReader(rawValue))
			objectDecoder.DisallowUnknownFields()
			if objectDecoder.Decode(&object) != nil {
				return nil, errors.New("歌手条目既不是字符串也不是 {name} 对象")
			}
			value = object.Name
		}
		artist, err := validateNeteaseCDPText("歌手", value, 256, false)
		if err != nil {
			return nil, err
		}
		if artist != "" {
			artists = append(artists, artist)
		}
	}
	return artists, nil
}

func validateNeteaseCDPText(field, value string, maximumRunes int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumRunes {
		return "", fmt.Errorf("%s超过长度限制或不是有效 UTF-8", field)
	}
	if required && value == "" {
		return "", fmt.Errorf("%s为空", field)
	}
	return value, nil
}
