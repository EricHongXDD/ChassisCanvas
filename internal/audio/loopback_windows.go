//go:build windows

package audio

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

const (
	waveFormatIEEEFloat  = 0x0003
	waveFormatExtensible = 0xFFFE
)

type Loopback struct {
	BufferDuration time.Duration
	endpointRole   wca.ERole
	endpointMu     sync.RWMutex
	endpointName   string
	endpointID     string
}

func NewLoopback() *Loopback {
	// Windows “设置 > 系统 > 声音”的默认输出对应 Console 角色。
	// Multimedia 可以被单独指向一个没有实际播放的端点，导致只采集到底噪。
	return &Loopback{BufferDuration: 200 * time.Millisecond, endpointRole: wca.EConsole}
}

// Capture 从 Windows 默认系统播放端点读取 WASAPI loopback 数据。
func (source *Loopback) Capture(ctx context.Context, emit func(PCMFrame) error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		return fmt.Errorf("初始化 COM: %w", err)
	}
	defer ole.CoUninitialize()

	var enumerator *wca.IMMDeviceEnumerator
	if err = wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return fmt.Errorf("创建音频设备枚举器: %w", err)
	}
	defer enumerator.Release()
	defaultEndpointChanged := make(chan struct{}, 1)
	notification := wca.NewIMMNotificationClient(wca.IMMNotificationClientCallback{
		OnDefaultDeviceChanged: func(flow wca.EDataFlow, role wca.ERole, _ string) error {
			if isRenderEndpointChange(flow, role, source.role()) {
				select {
				case defaultEndpointChanged <- struct{}{}:
				default:
				}
			}
			return nil
		},
	})
	if err = enumerator.RegisterEndpointNotificationCallback(notification); err != nil {
		return fmt.Errorf("监听默认系统播放设备变更: %w", err)
	}
	defer func() {
		unregisterErr := unregisterEndpointNotificationCallback(enumerator, notification)
		runtime.KeepAlive(notification)
		if err == nil && ctx.Err() == nil && unregisterErr != nil {
			err = fmt.Errorf("停止监听默认系统播放设备变更: %w", unregisterErr)
		}
	}()

	var endpoint *wca.IMMDevice
	if err = enumerator.GetDefaultAudioEndpoint(wca.ERender, uint32(source.role()), &endpoint); err != nil {
		return fmt.Errorf("获取默认系统播放设备: %w", err)
	}
	defer endpoint.Release()
	source.setEndpointDiagnostic(endpoint)

	var client *wca.IAudioClient
	if err = endpoint.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &client); err != nil {
		return fmt.Errorf("激活音频客户端: %w", err)
	}
	defer client.Release()

	var wave *wca.WAVEFORMATEX
	if err = client.GetMixFormat(&wave); err != nil {
		return fmt.Errorf("读取混音格式: %w", err)
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(wave)))
	format, err := sampleFormatFromWave(wave)
	if err != nil {
		return err
	}

	bufferDuration := source.BufferDuration
	if bufferDuration <= 0 {
		bufferDuration = 200 * time.Millisecond
	}
	referenceTime := wca.REFERENCE_TIME(bufferDuration / (100 * time.Nanosecond))
	if err = client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, wca.AUDCLNT_STREAMFLAGS_LOOPBACK, referenceTime, 0, wave, nil); err != nil {
		return fmt.Errorf("初始化回环采集: %w", err)
	}

	var capture *wca.IAudioCaptureClient
	if err = client.GetService(wca.IID_IAudioCaptureClient, &capture); err != nil {
		return fmt.Errorf("获取回环采集器: %w", err)
	}
	defer capture.Release()
	if err = client.Start(); err != nil {
		return fmt.Errorf("启动回环采集: %w", err)
	}
	defer func() {
		if stopErr := client.Stop(); err == nil && stopErr != nil {
			err = stopErr
		}
	}()

	var defaultPeriod, minimumPeriod wca.REFERENCE_TIME
	if err = client.GetDevicePeriod(&defaultPeriod, &minimumPeriod); err != nil {
		return fmt.Errorf("读取音频周期: %w", err)
	}
	// 回环缓冲区可容纳 200ms，按设备周期读取即可；无需每半个
	// 周期轮询，这可在静音时将 COM 查询次数约减半。
	wait := time.Duration(defaultPeriod) * 100 * time.Nanosecond
	if wait < 5*time.Millisecond {
		wait = 5 * time.Millisecond
	} else if wait > 20*time.Millisecond {
		wait = 20 * time.Millisecond
	}
	ticker := time.NewTicker(wait)
	defer ticker.Stop()

	for {
		if err := drainCapture(ctx, capture, wave, format, emit); err != nil {
			return fmt.Errorf("默认系统播放设备回环采集失败，将重新连接: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-defaultEndpointChanged:
			return errDefaultOutputEndpointChanged
		case <-ticker.C:
		}
	}
}

func (source *Loopback) SourceStatus() SourceStatus {
	if source == nil {
		return SourceStatus{EndpointRole: "console"}
	}
	source.endpointMu.RLock()
	defer source.endpointMu.RUnlock()
	return SourceStatus{Endpoint: source.endpointName, EndpointID: source.endpointID, EndpointRole: "console"}
}

func (source *Loopback) setEndpointDiagnostic(endpoint *wca.IMMDevice) {
	if source == nil || endpoint == nil {
		return
	}
	var endpointID string
	_ = endpoint.GetId(&endpointID)
	name := strings.TrimSpace(endpointFriendlyName(endpoint))
	source.endpointMu.Lock()
	source.endpointName = name
	source.endpointID = endpointID
	source.endpointMu.Unlock()
}

func endpointFriendlyName(endpoint *wca.IMMDevice) string {
	var properties *wca.IPropertyStore
	if err := endpoint.OpenPropertyStore(wca.STGM_READ, &properties); err != nil || properties == nil {
		return ""
	}
	defer properties.Release()
	var value wca.PROPVARIANT
	if err := properties.GetValue(&wca.PKEY_Device_FriendlyName, &value); err != nil {
		return ""
	}
	defer func() { _ = ole.VariantClear(&value.VARIANT) }()
	return value.String()
}

func (source *Loopback) role() wca.ERole {
	if source != nil && source.endpointRole >= wca.EConsole && source.endpointRole < wca.ERole_enum_count {
		return source.endpointRole
	}
	return wca.EConsole
}

func isRenderEndpointChange(flow wca.EDataFlow, role, selectedRole wca.ERole) bool {
	return flow == wca.ERender && role == selectedRole
}

// go-wca v0.3.0 的公开 Unregister 方法仍返回 E_NOTIMPL，这里直接调用 COM vtable。
func unregisterEndpointNotificationCallback(enumerator *wca.IMMDeviceEnumerator, notification *wca.IMMNotificationClient) error {
	if enumerator == nil || notification == nil {
		return nil
	}
	hr, _, _ := syscall.Syscall(
		enumerator.VTable().UnregisterEndpointNotificationCallback,
		2,
		uintptr(unsafe.Pointer(enumerator)),
		uintptr(unsafe.Pointer(notification)),
		0,
	)
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

func drainCapture(ctx context.Context, capture *wca.IAudioCaptureClient, wave *wca.WAVEFORMATEX, format SampleFormat, emit func(PCMFrame) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		var packetFrames uint32
		if err := capture.GetNextPacketSize(&packetFrames); err != nil {
			return fmt.Errorf("读取回环数据长度: %w", err)
		}
		if packetFrames == 0 {
			return nil
		}
		var data *byte
		var frames, flags uint32
		var devicePosition, qpcPosition uint64
		if err := capture.GetBuffer(&data, &frames, &flags, &devicePosition, &qpcPosition); err != nil {
			return fmt.Errorf("读取回环数据: %w", err)
		}
		var samples []float64
		if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
			samples = make([]float64, int(frames))
		} else {
			byteCount := int(frames) * int(wave.NBlockAlign)
			raw := append([]byte(nil), unsafe.Slice(data, byteCount)...)
			var decodeErr error
			samples, decodeErr = DecodeInterleavedPCM(raw, format)
			if decodeErr != nil {
				_ = capture.ReleaseBuffer(frames)
				return decodeErr
			}
		}
		emitErr := emit(PCMFrame{
			CapturedAt: time.Now(), SampleRate: int(wave.NSamplesPerSec), Samples: samples,
			Discontinuity: wasapiPacketDiscontinuous(flags),
		})
		releaseErr := capture.ReleaseBuffer(frames)
		if emitErr != nil {
			return emitErr
		}
		if releaseErr != nil {
			return fmt.Errorf("释放回环数据: %w", releaseErr)
		}
	}
}

func wasapiPacketDiscontinuous(flags uint32) bool {
	return flags&(wca.AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY|wca.AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR) != 0
}

func sampleFormatFromWave(wave *wca.WAVEFORMATEX) (SampleFormat, error) {
	if wave == nil || wave.NSamplesPerSec == 0 {
		return SampleFormat{}, errors.New("混音格式无效")
	}
	tag := wave.WFormatTag
	if tag == waveFormatExtensible {
		if wave.CbSize < 22 {
			return SampleFormat{}, errors.New("WAVEFORMATEXTENSIBLE 扩展长度不足")
		}
		// SubFormat GUID 的 Data1 就是 PCM/IEEE_FLOAT 格式标记。
		tag = uint16(*(*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(wave)) + 24)))
	}
	format := SampleFormat{Channels: int(wave.NChannels), BitsPerSample: int(wave.WBitsPerSample), BlockAlign: int(wave.NBlockAlign)}
	switch tag {
	case wca.WAVE_FORMAT_PCM:
		if wave.WBitsPerSample == 8 {
			format.Encoding = PCMUnsigned
		} else {
			format.Encoding = PCMSigned
		}
	case waveFormatIEEEFloat:
		format.Encoding = PCMFloat
	default:
		return SampleFormat{}, fmt.Errorf("不支持的 Windows 混音格式: 0x%04X", tag)
	}
	if err := format.Validate(); err != nil {
		return SampleFormat{}, err
	}
	return format, nil
}
