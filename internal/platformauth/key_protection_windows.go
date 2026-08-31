//go:build windows

package platformauth

import (
	"bytes"
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const maximumDPAPIKeyBlobBytes = 64 << 10

// encodeKeyForStorage 使用当前 Windows 用户的 DPAPI 保护会话 AES 密钥。
// 不使用 LOCAL_MACHINE 标志，复制密钥文件到其他账号后不能解密。
func encodeKeyForStorage(key []byte) ([]byte, error) {
	if len(key) != keySize {
		return nil, errors.New("平台会话密钥无效")
	}
	input := windows.DataBlob{Size: uint32(len(key)), Data: &key[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(
		&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output,
	); err != nil {
		return nil, errors.New("无法使用 Windows DPAPI 保护平台会话密钥")
	}
	runtime.KeepAlive(key)
	if output.Data == nil || output.Size == 0 || output.Size > maximumDPAPIKeyBlobBytes {
		if output.Data != nil {
			_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
		}
		return nil, errors.New("Windows DPAPI 返回的平台会话密钥无效")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	protected := append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...)
	return append(append([]byte(nil), protectedKeyMagic...), protected...), nil
}

// decodeStoredKey 同时接受旧版 32 字节明文密钥；调用方会在成功读取后
// 立即原子迁移为 DPAPI 格式，已有 AES-GCM 会话数据无需重写。
func decodeStoredKey(data []byte) ([]byte, bool, error) {
	if len(data) == keySize {
		return append([]byte(nil), data...), true, nil
	}
	if !bytes.HasPrefix(data, protectedKeyMagic) || len(data) == len(protectedKeyMagic) {
		return nil, false, errors.New("平台会话密钥文件无效")
	}
	protected := data[len(protectedKeyMagic):]
	if len(protected) > maximumDPAPIKeyBlobBytes {
		return nil, false, errors.New("平台会话密钥文件无效")
	}
	input := windows.DataBlob{Size: uint32(len(protected)), Data: &protected[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(
		&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output,
	); err != nil {
		return nil, false, errors.New("当前 Windows 用户无法解密平台会话密钥")
	}
	runtime.KeepAlive(protected)
	if output.Data == nil || output.Size != keySize {
		if output.Data != nil {
			_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
		}
		return nil, false, errors.New("Windows DPAPI 解密的平台会话密钥无效")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	return append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...), false, nil
}
