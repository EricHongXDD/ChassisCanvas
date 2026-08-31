// Package platformid 提供主机、边缘中继和屏端共用的平台歌曲 ID 校验。
package platformid

import "strconv"

// QQ 验证 QQ 音乐 songmid 的长度和 ASCII 字母数字格式。平台归属由来源字段
// 单独锁定；不能仅凭字符外形猜测一个 ID 属于另一平台。
func QQ(value string) bool {
	if len(value) < 10 || len(value) > 32 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= '0' && character <= '9':
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z':
		default:
			return false
		}
	}
	return true
}

// Netease 验证规范十进制、无前导零且可解析为正 int64 的网易云歌曲 ID。
func Netease(value string) bool {
	if len(value) < 1 || len(value) > 19 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for index := 1; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return err == nil && parsed > 0
}
