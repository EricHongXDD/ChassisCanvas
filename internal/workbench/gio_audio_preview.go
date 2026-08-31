package workbench

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"casescreen/internal/service"
	caseTheme "casescreen/internal/theme"
	"gioui.org/layout"
	"gioui.org/op"
)

const (
	audioPreviewRefreshInterval = 100 * time.Millisecond
	audioPreviewStaleAfter      = 500 * time.Millisecond
)

type audioPreviewRefreshState struct {
	running     bool
	lastRefresh time.Time
	status      service.AudioStatus
	layers      map[string]audioPreviewLayerRuntime
}

type audioPreviewLayerRuntime struct {
	settings   string
	previous   []float64
	peaks      []float64
	heldUntil  []time.Time
	capturedAt time.Time
}

func (model *gioEditor) hasVisibleAudioLayer() bool {
	for _, layer := range model.manifest.Layers {
		if (layer.Type == caseTheme.LayerAudioBars || layer.Type == caseTheme.LayerAudioRing) &&
			caseTheme.LayerVisible(layer) && layer.Opacity > 0 {
			return true
		}
	}
	return false
}

// scheduleAudioPreviewRefresh 只在画布真正显示频谱时以 10 FPS 读取本地状态。
// 每次最多一个在途请求，服务离线时不会累积 goroutine。
func (model *gioEditor) scheduleAudioPreviewRefresh(gtx layout.Context) {
	if !model.periodicUIWorkEnabled() || !model.hasVisibleAudioLayer() {
		return
	}
	now := gtx.Now
	next := model.audioPreview.lastRefresh.Add(audioPreviewRefreshInterval)
	if model.audioPreview.lastRefresh.IsZero() || !now.Before(next) {
		model.refreshAudioPreviewAsync()
		next = now.Add(audioPreviewRefreshInterval)
	}
	gtx.Execute(op.InvalidateCmd{At: next})
}

func (model *gioEditor) refreshAudioPreviewAsync() {
	if model.api == nil || model.audioPreview.running {
		return
	}
	model.audioPreview.running = true
	generation := model.windowActivityGeneration()
	model.ensureUIUpdates()
	go func() {
		status, err := model.api.Status()
		model.uiUpdates <- func() {
			model.audioPreview.running = false
			if generation != model.windowActivityGeneration() {
				// 暂停期间完成的请求已过期；恢复后必须立即获取新帧。
				model.audioPreview.lastRefresh = time.Time{}
				return
			}
			model.audioPreview.lastRefresh = time.Now()
			if err != nil {
				// 服务离线或超时后立即清空，不把最后一帧当成实时音频。
				model.audioPreview.status = service.AudioStatus{}
				return
			}
			model.setAudioPreviewStatus(status.Audio)
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) setAudioPreviewStatus(status service.AudioStatus) {
	status.Bands = append([]float64(nil), status.Bands...)
	if !status.Enabled || !status.Running {
		status.CapturedAt = time.Time{}
		status.Level = 0
		status.Peak = 0
		status.Bands = nil
	}
	model.audioPreview.status = status
}

// audioPreviewValues 优先显示 /api/status 的 WASAPI 频带；没有可用声音时
// 改用仅存在于编辑器中的演示频带，让新建的频谱图层仍能看清形态。
func (model *gioEditor) audioPreviewValues(layer caseTheme.Layer, count int) []float64 {
	values, _ := model.audioPreviewFrame(layer, count)
	return values
}

// audioPreviewFrame 保存每个图层的平滑和峰值状态，让属性面板中的峰值保持
// 与真实设备一样可见。演示频带只用于画布，不会进入服务或设备数据流。
func (model *gioEditor) audioPreviewFrame(layer caseTheme.Layer, count int) ([]float64, []float64) {
	count = max(1, count)
	status := model.audioPreview.status
	now := model.previewMoment()
	key, settings := audioPreviewLayerIdentity(layer, count)
	input, capturedAt, source := audioPreviewInput(status, layer, count, now)
	settings += "|" + source
	if model.audioPreview.layers == nil {
		model.audioPreview.layers = make(map[string]audioPreviewLayerRuntime)
	}
	runtime := model.audioPreview.layers[key]
	if runtime.settings != settings || capturedAt.Before(runtime.capturedAt) {
		runtime = audioPreviewLayerRuntime{settings: settings}
	}
	if capturedAt.Equal(runtime.capturedAt) && len(runtime.previous) == count {
		return append([]float64(nil), runtime.previous...), append([]float64(nil), runtime.peaks...)
	}
	values := caseTheme.TransformAudioBands(input, count, layer, runtime.previous)
	if previewAudioBandsSilent(values) {
		delete(model.audioPreview.layers, key)
		return values, nil
	}
	peaks, heldUntil := updateAudioPreviewPeaks(values, runtime, layer, capturedAt)
	model.audioPreview.layers[key] = audioPreviewLayerRuntime{
		settings: settings, previous: append([]float64(nil), values...),
		peaks: append([]float64(nil), peaks...), heldUntil: append([]time.Time(nil), heldUntil...), capturedAt: capturedAt,
	}
	return values, peaks
}

func audioPreviewInput(status service.AudioStatus, layer caseTheme.Layer, count int, now time.Time) ([]float64, time.Time, string) {
	fresh := status.Enabled && status.Running && !status.CapturedAt.IsZero() && len(status.Bands) > 0 &&
		(!now.After(status.CapturedAt) || now.Sub(status.CapturedAt) <= audioPreviewStaleAfter)
	if fresh {
		// 低于门限的实时输入在设备上应归零；编辑器则切换到明确的演示
		// 形态，避免只剩选框，同时不把演示值误当成真实采样。
		probe := caseTheme.TransformAudioBands(status.Bands, count, layer, nil)
		if !previewAudioBandsSilent(probe) {
			return append([]float64(nil), status.Bands...), status.CapturedAt, "live"
		}
	}
	frameTime := now.Truncate(audioPreviewRefreshInterval)
	return simulatedAudioPreviewBands(count, frameTime), frameTime, "simulation"
}

// simulatedAudioPreviewBands 组合两个缓慢移动的能量峰和一组细小纹波。
// 所有值都低于满量程且随时间变化，便于同时看清真实高度、平滑和峰值样式。
func simulatedAudioPreviewBands(count int, now time.Time) []float64 {
	count = max(1, count)
	values := make([]float64, count)
	phase := float64(now.UnixNano()) / float64(time.Second) * 2 * math.Pi / 3.2
	for index := range values {
		position := float64(index) / float64(max(1, count-1))
		lowCenter := .32 + .07*math.Sin(phase*.61)
		highCenter := .72 + .05*math.Sin(phase*.43+1.1)
		lowPeak := .57 * math.Exp(-math.Pow((position-lowCenter)*3.5, 2))
		highPeak := .24 * math.Exp(-math.Pow((position-highCenter)*5.2, 2))
		ripple := .10 * (.5 + .5*math.Sin(position*9*math.Pi-phase*1.35))
		pulse := .82 + .13*math.Sin(phase+position*2*math.Pi)
		values[index] = finitePreviewAudioUnit(.07 + (lowPeak+highPeak+ripple)*pulse)
	}
	return values
}

func audioPreviewLayerIdentity(layer caseTheme.Layer, count int) (string, string) {
	key := layer.ID
	if key == "" {
		key = fmt.Sprintf("%s:%.4f:%.4f:%.4f:%.4f", layer.Type, layer.Bounds.X, layer.Bounds.Y, layer.Bounds.Width, layer.Bounds.Height)
	}
	options, _ := json.Marshal(layer.Options)
	return key, fmt.Sprintf("%s|%d|%s", layer.Type, count, options)
}

func updateAudioPreviewPeaks(values []float64, runtime audioPreviewLayerRuntime, layer caseTheme.Layer, capturedAt time.Time) ([]float64, []time.Time) {
	if !caseTheme.OptionBool(layer.Options, "peak_hold", false) {
		return nil, nil
	}
	duration := time.Duration(math.Max(0, math.Min(5000, caseTheme.OptionFloat(layer.Options, "peak_hold_ms", 450)))) * time.Millisecond
	decay := math.Max(.05, math.Min(10, caseTheme.OptionFloat(layer.Options, "peak_decay", 1.8)))
	elapsed := .1
	if !runtime.capturedAt.IsZero() && capturedAt.After(runtime.capturedAt) {
		elapsed = math.Min(1, capturedAt.Sub(runtime.capturedAt).Seconds())
	}
	peaks := make([]float64, len(values))
	heldUntil := make([]time.Time, len(values))
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
			heldUntil[index] = capturedAt.Add(duration)
		} else if !capturedAt.Before(heldUntil[index]) {
			peak = math.Max(value, peak-decay*elapsed)
		}
		peaks[index] = finitePreviewAudioUnit(peak)
	}
	return peaks, heldUntil
}

func previewAudioBandsSilent(values []float64) bool {
	for _, value := range values {
		if finitePreviewAudioUnit(value) > 0 {
			return false
		}
	}
	return true
}

func resampleAudioPreview(input []float64, count int) []float64 {
	if count <= 0 {
		return nil
	}
	output := make([]float64, count)
	if len(input) == 0 {
		return output
	}
	if len(input) == 1 || count == 1 {
		for index := range output {
			output[index] = finitePreviewAudioUnit(input[0])
		}
		return output
	}
	for index := range output {
		position := float64(index) * float64(len(input)-1) / float64(count-1)
		left := int(math.Floor(position))
		right := min(len(input)-1, left+1)
		fraction := position - float64(left)
		output[index] = finitePreviewAudioUnit(input[left])*(1-fraction) + finitePreviewAudioUnit(input[right])*fraction
	}
	return output
}

func finitePreviewAudioUnit(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return clamp01(value)
}

// visibleAudioPreviewValues 与两类实体屏的最终绘制保持一致：
// 真实频带为零时只留 4% 静态基线。此处只改变可见值，
// 平滑和峰值状态仍使用变换后的全零值立即清空。
func (model *gioEditor) visibleAudioPreviewValues(values []float64) []float64 {
	return caseTheme.AudioBandsWithIdleBaseline(values)
}
