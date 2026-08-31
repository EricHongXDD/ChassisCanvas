// Package edgepush 接收由已配对播放来源主动发送的原生平台歌曲 ID 和播放锚点。
//
// 本包只接受 QQ 音乐 songmid 与网易云数字歌曲 ID，不按标题搜索，不读取音频，
// 也不提供登录态、主题、文件或设备控制入口。
package edgepush

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"casescreen/internal/platformid"
)

const (
	ProtocolVersion = 1

	ChallengePath = "/v1/source/challenge"
	SessionPath   = "/v1/source/session"
	StatePath     = "/v1/source-state"

	HeaderMAC      = "X-CaseScreen-MAC"
	HeaderSession  = "X-CaseScreen-Session"
	HeaderSequence = "X-CaseScreen-Sequence"

	maximumRequestBytes = 1 << 10
	maximumTrackTimeMS  = int64((24 * time.Hour) / time.Millisecond)
	pairingKeyBytes     = 32
	challengeBytes      = 32
	sessionBytes        = 16

	canonicalPrefix = "casescreen-edgepush/v1"
)

// ChallengeResponse 是来源建立防重放会话前获取的一次性挑战。
type ChallengeResponse struct {
	Version         int    `json:"v"`
	Challenge       string `json:"challenge"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms"`
}

// SessionRequest 用一次性挑战注册随机来源会话。Session 必须是 16 字节
// 随机值的无填充 base64url 表示。
type SessionRequest struct {
	Version   int    `json:"v"`
	Challenge string `json:"challenge"`
	Session   string `json:"session"`
}

// Update 是播放来源发送的全部日常状态。可用状态必须始终携带平台原生 ID，
// 以免进度包被错误关联到上一首歌。毫秒精度足以驱动 LRC 时间轴，同时比纳秒更小。
type Update struct {
	Version       int    `json:"v"`
	Available     bool   `json:"available"`
	Source        string `json:"source,omitempty"`
	TrackID       string `json:"track_id,omitempty"`
	PositionMS    int64  `json:"position_ms,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	PositionKnown bool   `json:"position_known,omitempty"`
	Playing       bool   `json:"playing,omitempty"`
}

// Validate 严格校验平台、原生歌曲 ID 与播放锚点。它不接受平台别名，避免
// QQ songmid 和网易云数字 ID 被跨平台解释。
func (update Update) Validate() error {
	if update.Version != ProtocolVersion {
		return fmt.Errorf("不支持的播放来源协议版本: %d", update.Version)
	}
	if !update.Available {
		if update.Source != "" || update.TrackID != "" || update.PositionMS != 0 ||
			update.DurationMS != 0 || update.PositionKnown || update.Playing {
			return errors.New("不可用状态不能携带歌曲或播放字段")
		}
		return nil
	}
	if update.Source != strings.TrimSpace(update.Source) || update.TrackID != strings.TrimSpace(update.TrackID) {
		return errors.New("平台或歌曲 ID 不能包含首尾空白")
	}
	switch update.Source {
	case "qq":
		if !platformid.QQ(update.TrackID) {
			return errors.New("QQ 音乐 songmid 无效")
		}
	case "netease":
		if !platformid.Netease(update.TrackID) {
			return errors.New("网易云歌曲 ID 无效")
		}
	default:
		return errors.New("播放来源仅支持 qq 或 netease")
	}
	if !update.PositionKnown {
		return errors.New("可用状态必须提供已知播放位置")
	}
	if update.PositionMS < 0 || update.PositionMS > maximumTrackTimeMS {
		return errors.New("播放位置超出允许范围")
	}
	if update.DurationMS < 0 || update.DurationMS > maximumTrackTimeMS {
		return errors.New("歌曲时长超出允许范围")
	}
	if update.DurationMS > 0 && update.PositionMS > update.DurationMS {
		return errors.New("播放位置不能超过歌曲时长")
	}
	return nil
}

// SessionSignature 为原始 SessionRequest JSON 生成无填充 base64url HMAC。
// 调用方必须先序列化最终正文，再对完全相同的字节签名。
func SessionSignature(pairingKey string, body []byte) (string, error) {
	key, err := decodePairingKey(pairingKey)
	if err != nil {
		return "", err
	}
	return signature(key, sessionCanonical(body)), nil
}

// StateSignature 为原始 Update JSON、会话和序号生成无填充 base64url HMAC。
func StateSignature(pairingKey, session string, sequence uint64, body []byte) (string, error) {
	key, err := decodePairingKey(pairingKey)
	if err != nil {
		return "", err
	}
	if !validSessionID(session) {
		return "", errors.New("来源会话 ID 无效")
	}
	if sequence == 0 {
		return "", errors.New("来源序号必须大于 0")
	}
	return signature(key, stateCanonical(session, sequence, body)), nil
}

func decodePairingKey(value string) ([]byte, error) {
	if value != strings.TrimSpace(value) || len(value) != pairingKeyBytes*2 {
		return nil, errors.New("播放来源配对密钥必须是 64 位十六进制随机值")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != pairingKeyBytes {
		return nil, errors.New("播放来源配对密钥必须是 64 位十六进制随机值")
	}
	return decoded, nil
}

func sessionCanonical(body []byte) []byte {
	digest := sha256.Sum256(body)
	return []byte(canonicalPrefix + "\nPOST\n" + SessionPath + "\n" + hex.EncodeToString(digest[:]))
}

func stateCanonical(session string, sequence uint64, body []byte) []byte {
	digest := sha256.Sum256(body)
	return []byte(canonicalPrefix + "\nPUT\n" + StatePath + "\n" + session + "\n" +
		strconv.FormatUint(sequence, 10) + "\n" + hex.EncodeToString(digest[:]))
}

func signature(key, canonical []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(canonical)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifySignature(key, canonical []byte, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	decoded, err := base64.RawURLEncoding.DecodeString(candidate)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(canonical)
	return hmac.Equal(decoded, mac.Sum(nil))
}

func validChallenge(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == challengeBytes &&
		base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validSessionID(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sessionBytes &&
		base64.RawURLEncoding.EncodeToString(decoded) == value
}
