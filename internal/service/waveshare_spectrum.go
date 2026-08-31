package service

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	"casescreen/internal/audio"
	"casescreen/internal/device"
	"casescreen/internal/render"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

const waveshareSpectrumInterval = 100 * time.Millisecond
const waveshareSpectrumStaleAfter = 500 * time.Millisecond
const waveshareSpectrumBandCount = theme.WaveshareAudioBandCount

type waveshareSpectrumSender interface {
	SendSpectrumValues(context.Context, map[string]string) error
}

func waveshareSpectrumCarriesLyricProgress(driver device.Driver, manifest *theme.Manifest) bool {
	if manifest == nil {
		return false
	}
	if _, ok := driver.(waveshareSpectrumSender); !ok {
		return false
	}
	return len(waveshareHostAudioLayers(manifest)) > 0
}

type waveshareSpectrumRuntime struct {
	themeGeneration uint64
	lastFrame       time.Time
	layers          map[string]waveshareSpectrumLayerRuntime
	lastKeys        []string
	lastValues      map[string]string
}

type waveshareSpectrumLayerRuntime struct {
	previous  []float64
	peaks     []float64
	heldUntil []time.Time
	updatedAt time.Time
}

type waveshareSpectrumOptions struct {
	peakHold     bool
	peakDuration time.Duration
	peakDecay    float64
}

// runWaveshareSpectrum 独立于主机合成帧运行，只有实现了微雪频谱接口、已连接，
// 且当前主题存在可见音频图层时才以 100ms 采样。驱动按 USB 带宽自适应节流，
// 拥塞时只保留最新一帧，不提高实际发送预算。
func (service *Service) runWaveshareSpectrum(ctx context.Context) {
	ticker := time.NewTicker(waveshareSpectrumInterval)
	defer ticker.Stop()
	runtimeByDevice := make(map[string]waveshareSpectrumRuntime)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			service.sendWaveshareSpectrum(ctx, runtimeByDevice)
		}
	}
}

func (service *Service) sendWaveshareSpectrum(ctx context.Context, runtimeByDevice map[string]waveshareSpectrumRuntime) {
	service.mu.RLock()
	drivers := append([]device.Driver(nil), service.drivers...)
	service.mu.RUnlock()
	for _, driver := range drivers {
		sender, ok := driver.(waveshareSpectrumSender)
		if !ok {
			continue
		}
		id := driver.Info().ID
		func() {
			unlockSmallPacket, lockErr := service.lockSmallPacketDevice(ctx, id)
			if lockErr != nil {
				return
			}
			defer unlockSmallPacket()

			// 频谱与歌词事件共用设备门闩。频谱只允许合入三字符 LFP，
			// 旧兼容长键始终留在独立歌词包，避免放大频带 QStringMap。
			service.mu.RLock()
			provider := service.spectrum
			enabled := service.audioEnabled
			lyricsProvider := service.lyrics
			lyricsEnabled := service.lyricsEnabled
			status := service.statuses[id]
			themeGeneration := service.spectrumThemeGeneration[id]
			var manifest *theme.Manifest
			if target, exists := service.deviceThemes[id]; exists {
				copied := target
				manifest = &copied
			} else if service.activeTheme != nil {
				copied := *service.activeTheme
				manifest = &copied
			}
			service.mu.RUnlock()
			if !status.Connected {
				return
			}

			now := time.Now()
			frame, frameAvailable := freshSpectrumSnapshot(provider, enabled, now)
			capturedAt, capturedBars := frame.CapturedAt, frame.Bars
			layers := waveshareHostAudioLayers(manifest)
			var lyricState render.LyricState
			lyricTimed := false
			if lyricsProvider != nil && lyricsEnabled {
				lyricState, _, lyricTimed = snapshotLyricsProvider(lyricsProvider)
			}
			fineSnapshot := telemetry.Snapshot{OmitHardwareMetrics: true, OmitClock: true}
			fineChanged := false
			if lyricState.Playing && manifest != nil && len(layers) > 0 {
				if fine, ok := lyricProgressForManifest(*manifest, lyricState, lyricTimed); ok {
					fineSnapshot, _ = service.pendingLyricProgressSnapshot(id, fine)
					// 旧长键只能由独立歌词路径发送。
					fineSnapshot.LyricsProgressKnown = false
					fineChanged = fineSnapshot.LyricsProgressFineKnown
				}
			}
			runtime := runtimeByDevice[id]
			if runtime.themeGeneration != themeGeneration {
				// Theme 重载会清空屏端动态值；同一数据前缀也不能继承旧主题
				// 的 smoothing/peak runtime，否则下一帧会把旧波峰重新带回。
				runtime = waveshareSpectrumRuntime{themeGeneration: themeGeneration}
			}
			currentKeys := waveshareSpectrumKeys(layers, manifest)
			if len(layers) == 0 || !frameAvailable {
				// 隐藏或删除频谱时清空旧键；只是无声、采集停滞或
				// 用户暂停采集时，当前频谱保留 4% 静态基线。轨道仍为透明，
				// 所以不会回到整根满高柱框。
				keys := mergeWaveshareSpectrumKeys(runtime.lastKeys, currentKeys)
				if len(keys) == 0 {
					delete(runtimeByDevice, id)
					return
				}
				values := zeroWaveshareSpectrumValues(keys)
				if len(layers) > 0 {
					appendWaveshareIdleSpectrumValues(values, layers, manifest)
				}
				bandValues := cloneWaveshareSpectrumValues(values)
				if fineChanged {
					values[telemetry.LyricsProgressFineKey] = strconv.Itoa(fineSnapshot.LyricsProgressFine)
				}
				err := sender.SendSpectrumValues(ctx, values)
				if errors.Is(err, device.ErrRealtimeFrameDropped) {
					return
				}
				if err == nil {
					runtime.lastFrame = time.Time{}
					runtime.layers = nil
					// 保留最后一组键，使主题隐藏或采集继续停滞时，
					// 驱动缓存因重连/主题重载失效后仍能补发清空值或静态基线。
					runtime.lastKeys = keys
					runtime.lastValues = bandValues
					runtimeByDevice[id] = runtime
					if fineChanged {
						service.rememberLyricProgress(id, fineSnapshot, nil)
					}
				}
				service.setTransferError(id, err)
				return
			}
			layoutChanged := !equalWaveshareSpectrumKeys(runtime.lastKeys, currentKeys)
			frameChanged := capturedAt.After(runtime.lastFrame)
			if !layoutChanged && !frameChanged {
				if fineChanged && len(runtime.lastValues) > 0 {
					values := cloneWaveshareSpectrumValues(runtime.lastValues)
					values[telemetry.LyricsProgressFineKey] = strconv.Itoa(fineSnapshot.LyricsProgressFine)
					err := sender.SendSpectrumValues(ctx, values)
					if errors.Is(err, device.ErrRealtimeFrameDropped) {
						return
					}
					if err == nil {
						service.rememberLyricProgress(id, fineSnapshot, nil)
					}
					service.setTransferError(id, err)
				}
				return
			}
			if runtime.layers == nil {
				runtime.layers = make(map[string]waveshareSpectrumLayerRuntime)
			}
			values := zeroWaveshareSpectrumValues(runtime.lastKeys)
			nextLayers := make(map[string]waveshareSpectrumLayerRuntime, len(layers))
			for _, layer := range layers {
				options := waveshareOptions(layer)
				runtimeKey := waveshareSpectrumLayerRuntimeKey(layer, options)
				layerRuntime := runtime.layers[runtimeKey]
				transformed := transformWaveshareSpectrum(capturedBars, layer, layerRuntime.previous)
				var peaks []float64
				var heldUntil []time.Time
				if waveshareSpectrumSilent(transformed) {
					// 零帧必须同时绕过平滑和峰值保持，否则无声时仍会显示上一帧。
					peaks = make([]float64, len(transformed))
				} else {
					peaks, heldUntil = updateWaveshareSpectrumPeaks(transformed, layerRuntime, options, capturedAt)
				}
				appendWaveshareLayerSpectrumValues(values, layer, transformed, peaks, options.peakHold,
					theme.WaveshareAudioBandIndexes(layer, manifest.Width, manifest.Height))
				nextLayers[runtimeKey] = waveshareSpectrumLayerRuntime{
					previous: transformed, peaks: peaks, heldUntil: heldUntil, updatedAt: capturedAt,
				}
			}
			bandValues := cloneWaveshareSpectrumValues(values)
			if fineChanged {
				values[telemetry.LyricsProgressFineKey] = strconv.Itoa(fineSnapshot.LyricsProgressFine)
			}
			err := sender.SendSpectrumValues(ctx, values)
			if errors.Is(err, device.ErrRealtimeFrameDropped) {
				return
			}
			if err == nil {
				runtime.lastFrame = capturedAt
				runtime.layers = nextLayers
				runtime.lastKeys = currentKeys
				runtime.lastValues = bandValues
				runtimeByDevice[id] = runtime
				if fineChanged {
					service.rememberLyricProgress(id, fineSnapshot, nil)
				}
			}
			service.setTransferError(id, err)
		}()
	}
}

// freshSpectrumSnapshot 统一主题重载与周期频谱的时效判断。
// 超过 500ms 的帧视为旧帧，调用方必须发送零值而不是把旧波形补回屏幕。
func freshSpectrumSnapshot(provider SpectrumProvider, enabled bool, now time.Time) (audio.SpectrumFrame, bool) {
	if !enabled || provider == nil {
		return audio.SpectrumFrame{}, false
	}
	frame := provider.Snapshot()
	available := !frame.CapturedAt.IsZero() && len(frame.Bars) > 0 &&
		(frame.CapturedAt.After(now) || now.Sub(frame.CapturedAt) <= waveshareSpectrumStaleAfter)
	return frame, available
}

// sendCurrentWaveshareSpectrumAfterTheme 在新 .Theme 成功驻留后补发一包当前频带。
// 它不复用周期循环的 runtime，因此不会带入旧主题的平滑、峰值或键集合。
func (service *Service) sendCurrentWaveshareSpectrumAfterTheme(ctx context.Context, driver device.Driver, manifest theme.Manifest) error {
	sender, ok := driver.(waveshareSpectrumSender)
	if !ok {
		return nil
	}
	if reporter, ok := driver.(device.ConnectionStateDriver); ok && !reporter.Connected() {
		return errors.New("主题已写入，但设备在补发当前频谱前断开")
	}
	layers := waveshareHostAudioLayers(&manifest)
	keys := waveshareSpectrumKeys(layers, &manifest)
	if len(keys) == 0 {
		return nil
	}
	service.mu.RLock()
	provider := service.spectrum
	enabled := service.audioEnabled
	service.mu.RUnlock()
	frame, fresh := freshSpectrumSnapshot(provider, enabled, time.Now())
	values := zeroWaveshareSpectrumValues(keys)
	appendWaveshareIdleSpectrumValues(values, layers, &manifest)
	if fresh {
		for _, layer := range layers {
			transformed := transformWaveshareSpectrum(frame.Bars, layer, nil)
			options := waveshareOptions(layer)
			var peaks []float64
			if options.peakHold && !waveshareSpectrumSilent(transformed) {
				peaks, _ = updateWaveshareSpectrumPeaks(transformed, waveshareSpectrumLayerRuntime{}, options, frame.CapturedAt)
			}
			appendWaveshareLayerSpectrumValues(values, layer, transformed, peaks, options.peakHold,
				theme.WaveshareAudioBandIndexes(layer, manifest.Width, manifest.Height))
		}
	}
	err := sender.SendSpectrumValues(ctx, values)
	service.setTransferError(driver.Info().ID, err)
	if errors.Is(err, device.ErrRealtimeFrameDropped) {
		return nil
	}
	return err
}

// waveshareHostAudioLayers 返回全部可见音频图层。旧 manifest 中的
// source=device_mic 会在重新编译时迁移为 Windows WASAPI。
func waveshareHostAudioLayers(manifest *theme.Manifest) []theme.Layer {
	if manifest == nil {
		return nil
	}
	layers := make([]theme.Layer, 0, 2)
	for _, layer := range manifest.Layers {
		if (layer.Type != theme.LayerAudioBars && layer.Type != theme.LayerAudioRing) || !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		layers = append(layers, layer)
	}
	return layers
}

// waveshareAudioLayer 保留单图层查询契约。
func waveshareAudioLayer(manifest *theme.Manifest) (theme.Layer, bool) {
	layers := waveshareHostAudioLayers(manifest)
	if len(layers) == 0 {
		return theme.Layer{}, false
	}
	return layers[0], true
}

func waveshareOptions(layer theme.Layer) waveshareSpectrumOptions {
	return waveshareSpectrumOptions{
		peakHold:     theme.OptionBool(layer.Options, "peak_hold", false),
		peakDuration: time.Duration(math.Max(0, math.Min(5000, theme.OptionFloat(layer.Options, "peak_hold_ms", 450)))) * time.Millisecond,
		peakDecay:    math.Max(0.05, math.Min(10, theme.OptionFloat(layer.Options, "peak_decay", 1.8))),
	}
}

// waveshareSpectrumLayerRuntimeKey 将基础变换与峰值策略分开标识。
// 基础频带键不会因峰值选项变化，但不同保持/回落参数仍需要各自的历史状态。
func waveshareSpectrumLayerRuntimeKey(layer theme.Layer, options waveshareSpectrumOptions) string {
	key := theme.WaveshareAudioDataPrefix(layer) + "|"
	if !options.peakHold {
		return key
	}
	return key + theme.WaveshareAudioPeakKey(layer, 0)
}

func transformWaveshareSpectrum(bars []float64, layer theme.Layer, previous []float64) []float64 {
	return theme.TransformAudioBands(bars, waveshareSpectrumBandCount, layer, previous)
}

func waveshareSpectrumSilent(values []float64) bool {
	for _, value := range values {
		if finiteAudioUnit(value) > 0 {
			return false
		}
	}
	return true
}

func updateWaveshareSpectrumPeaks(values []float64, runtime waveshareSpectrumLayerRuntime, options waveshareSpectrumOptions, capturedAt time.Time) ([]float64, []time.Time) {
	if !options.peakHold {
		return nil, nil
	}
	peaks := make([]float64, len(values))
	heldUntil := make([]time.Time, len(values))
	elapsed := waveshareSpectrumInterval.Seconds()
	if !runtime.updatedAt.IsZero() && capturedAt.After(runtime.updatedAt) {
		elapsed = math.Min(1, capturedAt.Sub(runtime.updatedAt).Seconds())
	}
	for index, value := range values {
		peak := value
		if index < len(runtime.peaks) {
			peak = runtime.peaks[index]
		}
		if index < len(runtime.heldUntil) {
			heldUntil[index] = runtime.heldUntil[index]
		}
		if value >= peak {
			peak = value
			heldUntil[index] = capturedAt.Add(options.peakDuration)
		} else if !capturedAt.Before(heldUntil[index]) {
			peak = math.Max(value, peak-options.peakDecay*elapsed)
		}
		peaks[index] = math.Max(0, math.Min(1, peak))
	}
	return peaks, heldUntil
}

func appendWaveshareLayerSpectrumValues(values map[string]string, layer theme.Layer, bars, peaks []float64, includePeaks bool, indexes []int) {
	for _, index := range indexes {
		value := waveshareVisibleSpectrumPercent(bars, index)
		values[theme.WaveshareAudioBandKey(layer, index)] = strconv.Itoa(value)
		if includePeaks {
			values[theme.WaveshareAudioPeakKey(layer, index)] = strconv.Itoa(waveshareSpectrumPercent(peaks, index))
		}
	}
}

// appendWaveshareIdleSpectrumValues 只给当前主题真正引用的基础频带
// 设置低幅基线。旧主题键和峰值键保持 0，避免换样式后残留鬼影。
func appendWaveshareIdleSpectrumValues(values map[string]string, layers []theme.Layer, manifest *theme.Manifest) {
	if manifest == nil {
		return
	}
	idle := strconv.Itoa(int(math.Round(theme.DefaultAudioIdleAmplitude * 100)))
	for _, layer := range layers {
		for _, index := range theme.WaveshareAudioBandIndexes(layer, manifest.Width, manifest.Height) {
			values[theme.WaveshareAudioBandKey(layer, index)] = idle
			if theme.OptionBool(layer.Options, "peak_hold", false) {
				values[theme.WaveshareAudioPeakKey(layer, index)] = "0"
			}
		}
	}
}

func waveshareVisibleSpectrumPercent(values []float64, index int) int {
	return max(int(math.Round(theme.DefaultAudioIdleAmplitude*100)), waveshareSpectrumPercent(values, index))
}

func waveshareSpectrumPercent(values []float64, index int) int {
	if index < 0 || index >= len(values) {
		return 0
	}
	return int(math.Round(math.Max(0, math.Min(1, values[index])) * 100))
}

func waveshareSpectrumKeys(layers []theme.Layer, manifest *theme.Manifest) []string {
	if len(layers) == 0 || manifest == nil {
		return nil
	}
	set := make(map[string]bool)
	for _, layer := range layers {
		indexes := theme.WaveshareAudioBandIndexes(layer, manifest.Width, manifest.Height)
		for _, index := range indexes {
			set[theme.WaveshareAudioBandKey(layer, index)] = true
			if theme.OptionBool(layer.Options, "peak_hold", false) {
				set[theme.WaveshareAudioPeakKey(layer, index)] = true
			}
		}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mergeWaveshareSpectrumKeys(first, second []string) []string {
	set := make(map[string]bool, len(first)+len(second))
	for _, keys := range [][]string{first, second} {
		for _, key := range keys {
			set[key] = true
		}
	}
	merged := make([]string, 0, len(set))
	for key := range set {
		merged = append(merged, key)
	}
	sort.Strings(merged)
	return merged
}

func equalWaveshareSpectrumKeys(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func zeroWaveshareSpectrumValues(keys []string) map[string]string {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		values[key] = "0"
	}
	return values
}

func cloneWaveshareSpectrumValues(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

// resampleWaveshareSpectrum 保证 Audio_00..Audio_47 始终覆盖完整频率范围，
// 不会在主机分析器只配置 32 个频带时把后 16 个键永久补零。
func resampleWaveshareSpectrum(input []float64, count int) []float64 {
	return theme.ResampleAudioBands(input, count)
}
