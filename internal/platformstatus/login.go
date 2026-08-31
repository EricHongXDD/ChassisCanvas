// Package platformstatus 识别音乐平台以 HTTP 200 包装返回的登录或会员错误。
package platformstatus

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
)

var authErrorCodes = map[string]struct{}{
	"301": {},
	"401": {},
	"403": {},
}

// RequiresLogin 只检查结构化状态字段，不扫描歌词正文本身，避免歌词中恰好出现
// “登录”或“会员”等词时产生误判。
func RequiresLogin(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	return requiresLoginValue(value)
}

func requiresLoginValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalizedKey := normalizeKey(key)
			switch {
			case isAuthFlagKey(normalizedKey) && boolValue(child):
				return true
			case isStatusCodeKey(normalizedKey) && authErrorCode(child):
				return true
			case isMessageKey(normalizedKey) && authMessage(child):
				return true
			}
			// lyric/plainText 等正文只由歌词解析器处理，不能参与错误关键字判断。
			if isLyricsBodyKey(normalizedKey) {
				continue
			}
			if requiresLoginValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if requiresLoginValue(child) {
				return true
			}
		}
	}
	return false
}

func isAuthFlagKey(key string) bool {
	switch key {
	case "needlogin", "loginrequired", "requireslogin", "needauth", "authrequired",
		"requiresauth", "needvip", "viprequired", "requiresvip", "needpremium",
		"premiumrequired", "subscriptionrequired":
		return true
	default:
		return false
	}
}

func isStatusCodeKey(key string) bool {
	switch key {
	case "code", "status", "statuscode", "subcode", "retcode", "errorcode":
		return true
	default:
		return false
	}
}

func isMessageKey(key string) bool {
	switch key {
	case "message", "msg", "errmsg", "errormessage", "error", "tip", "tips", "reason":
		return true
	default:
		return false
	}
}

func isLyricsBodyKey(key string) bool {
	switch key {
	case "lyric", "lyrics", "lrc", "plaintext", "content", "translation", "trans":
		return true
	default:
		return false
	}
}

func boolValue(value any) bool {
	boolean, ok := value.(bool)
	return ok && boolean
}

func authErrorCode(value any) bool {
	var code string
	switch typed := value.(type) {
	case json.Number:
		code = typed.String()
	case string:
		code = strings.TrimSpace(typed)
	default:
		return false
	}
	_, ok := authErrorCodes[code]
	return ok
}

func authMessage(value any) bool {
	message, ok := value.(string)
	if !ok {
		return false
	}
	message = strings.ToLower(strings.TrimSpace(message))
	for _, marker := range []string{
		"登录", "登陆",
		"会员", "vip", "premium", "subscription", "付费", "购买", "无权限", "权限不足",
		"login", "log in", "sign in", "unauthorized", "forbidden", "authentication", "cookie",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func normalizeKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}
