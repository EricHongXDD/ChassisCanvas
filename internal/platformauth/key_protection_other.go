//go:build !windows

package platformauth

import "errors"

// 非 Windows 平台没有 DPAPI；继续依赖 0600 文件权限和 AES-GCM 数据文件。
func encodeKeyForStorage(key []byte) ([]byte, error) {
	if len(key) != keySize {
		return nil, errors.New("平台会话密钥无效")
	}
	return append([]byte(nil), key...), nil
}

func decodeStoredKey(data []byte) ([]byte, bool, error) {
	if len(data) != keySize {
		return nil, false, errors.New("平台会话密钥文件无效")
	}
	return append([]byte(nil), data...), false, nil
}
