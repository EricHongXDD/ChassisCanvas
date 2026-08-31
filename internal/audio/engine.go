package audio

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	defaultAnalysisWindow = 4096
	minimumAnalysisWindow = 1024
	// 服务主机渲染上限为 30 FPS，频谱分析更快只会重复计算。
	minimumFrameInterval = time.Second / 30
	// 部分 WASAPI 驱动在播放停止后不再产生静音包。超过这个
	// 宽限后 Snapshot 直接发布零帧，避免副屏停在最后的波峰。
	spectrumSilenceGrace = 80 * time.Millisecond
)

type EngineStatus struct {
	Running      bool   `json:"running"`
	Endpoint     string `json:"endpoint,omitempty"`
	EndpointID   string `json:"endpoint_id,omitempty"`
	EndpointRole string `json:"endpoint_role,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// Engine 持续重试系统音频源，并为渲染器保留最新频谱。
type Engine struct {
	source     Source
	bandCount  int
	retryDelay time.Duration
	changed    chan struct{}

	mu          sync.RWMutex
	enabled     bool
	captureStop context.CancelFunc
	analyzer    *Analyzer
	sampleRate  int
	samples     []float64
	lastAnalyze time.Time
	lastCapture time.Time
	latest      SpectrumFrame
	status      EngineStatus
}

func NewEngine(source Source, bandCount int) *Engine {
	if bandCount < 4 || bandCount > 128 {
		bandCount = 32
	}
	return &Engine{
		source: source, bandCount: bandCount, retryDelay: 2 * time.Second,
		changed: make(chan struct{}, 1), enabled: true,
	}
}

func (engine *Engine) Run(ctx context.Context) error {
	if engine.source == nil {
		return nil
	}
	for {
		if !engine.isEnabled() {
			engine.setRunning(false, "")
			select {
			case <-ctx.Done():
				return nil
			case <-engine.changed:
				continue
			}
		}
		captureContext, captureStop := context.WithCancel(ctx)
		engine.mu.Lock()
		if !engine.enabled {
			engine.mu.Unlock()
			captureStop()
			continue
		}
		engine.captureStop = captureStop
		engine.beginCaptureLocked(time.Now())
		engine.mu.Unlock()
		err := engine.source.Capture(captureContext, engine.consume)
		captureStop()
		engine.mu.Lock()
		engine.captureStop = nil
		enabled := engine.enabled
		engine.mu.Unlock()
		if ctx.Err() != nil {
			engine.setRunning(false, "")
			return nil
		}
		if !enabled {
			engine.setRunning(false, "")
			continue
		}
		if errors.Is(err, errDefaultOutputEndpointChanged) {
			// 这是用户主动切换声音输出，立即重建 WASAPI，不必等错误重试间隔。
			engine.setRunning(false, "")
			continue
		}
		message := "音频采集已停止"
		if err != nil {
			message = err.Error()
		}
		engine.setRunning(false, message)
		timer := time.NewTimer(engine.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-engine.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// SetEnabled 热启停 WASAPI；关闭时会取消当前采集并清空旧频谱。
func (engine *Engine) SetEnabled(enabled bool) {
	engine.mu.Lock()
	if engine.enabled == enabled {
		engine.mu.Unlock()
		return
	}
	engine.enabled = enabled
	stop := engine.captureStop
	if !enabled {
		engine.status = EngineStatus{}
		engine.latest = SpectrumFrame{}
		engine.analyzer = nil
		engine.sampleRate = 0
		engine.samples = nil
		engine.lastAnalyze = time.Time{}
		engine.lastCapture = time.Time{}
	}
	engine.mu.Unlock()
	if !enabled && stop != nil {
		stop()
	}
	select {
	case engine.changed <- struct{}{}:
	default:
	}
}

func (engine *Engine) isEnabled() bool {
	engine.mu.RLock()
	defer engine.mu.RUnlock()
	return engine.enabled
}

func (engine *Engine) Snapshot() SpectrumFrame {
	return engine.snapshotAt(time.Now())
}

func (engine *Engine) snapshotAt(now time.Time) SpectrumFrame {
	engine.mu.RLock()
	defer engine.mu.RUnlock()
	frame := engine.latest
	frame.Bars = append([]float64(nil), frame.Bars...)
	if frame.CapturedAt.IsZero() || !now.After(frame.CapturedAt.Add(spectrumSilenceGrace)) {
		return frame
	}
	frame.Level = 0
	for index := range frame.Bars {
		frame.Bars[index] = 0
	}
	// 推进时间戳，让下游 10 FPS 小数据包能及时清除屏端值。
	frame.CapturedAt = now
	return frame
}

func (engine *Engine) Status() EngineStatus {
	engine.mu.RLock()
	status := engine.status
	source := engine.source
	engine.mu.RUnlock()
	if reporter, ok := source.(sourceStatusProvider); ok {
		diagnostic := reporter.SourceStatus()
		status.Endpoint = diagnostic.Endpoint
		status.EndpointID = diagnostic.EndpointID
		status.EndpointRole = diagnostic.EndpointRole
	}
	return status
}

func (engine *Engine) consume(frame PCMFrame) error {
	if frame.SampleRate < 8000 || len(frame.Samples) == 0 {
		return nil
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if !engine.enabled {
		return nil
	}
	resetAnalysis := engine.analyzer == nil || engine.sampleRate != frame.SampleRate || engine.captureDiscontinuousLocked(frame)
	if resetAnalysis {
		analyzer, err := NewAnalyzer(frame.SampleRate, engine.bandCount)
		if err != nil {
			return err
		}
		engine.analyzer = analyzer
		engine.sampleRate = frame.SampleRate
		engine.samples = engine.samples[:0]
		engine.lastAnalyze = time.Time{}
		// 断流后的第一个短包还不足以执行 FFT，也必须立即覆盖旧频谱。
		engine.latest = SpectrumFrame{CapturedAt: frame.CapturedAt, Bars: make([]float64, engine.bandCount)}
	}
	engine.lastCapture = frame.CapturedAt
	engine.samples = append(engine.samples, frame.Samples...)
	if len(engine.samples) > defaultAnalysisWindow {
		engine.samples = append(engine.samples[:0], engine.samples[len(engine.samples)-defaultAnalysisWindow:]...)
	}
	if len(engine.samples) < minimumAnalysisWindow || (!engine.lastAnalyze.IsZero() && frame.CapturedAt.Sub(engine.lastAnalyze) < minimumFrameInterval) {
		return nil
	}
	spectrum, err := engine.analyzer.Analyze(engine.samples, frame.CapturedAt)
	if err != nil {
		return err
	}
	engine.latest = spectrum
	engine.lastAnalyze = frame.CapturedAt
	return nil
}

// captureDiscontinuousLocked 同时识别驱动报告的断流和采样时钟间断。
// 调用者必须持有 engine.mu。
func (engine *Engine) captureDiscontinuousLocked(frame PCMFrame) bool {
	if frame.Discontinuity {
		return true
	}
	if frame.CapturedAt.IsZero() || engine.lastCapture.IsZero() {
		return false
	}
	if frame.CapturedAt.Before(engine.lastCapture) {
		return true
	}
	return frame.CapturedAt.Sub(engine.lastCapture) > spectrumSilenceGrace
}

func (engine *Engine) setRunning(running bool, lastError string) {
	engine.mu.Lock()
	engine.status = EngineStatus{Running: running, LastError: lastError}
	engine.mu.Unlock()
}

// beginCaptureLocked 每次连接新的默认播放端点时先发布一帧静音。
// 这样即使驱动在空闲时不产生包，屏端也会清除主题模板的初始波峰。
// 调用者必须持有 engine.mu。
func (engine *Engine) beginCaptureLocked(capturedAt time.Time) {
	engine.status = EngineStatus{Running: true}
	engine.analyzer = nil
	engine.sampleRate = 0
	engine.samples = nil
	engine.lastAnalyze = time.Time{}
	engine.lastCapture = time.Time{}
	engine.latest = SpectrumFrame{CapturedAt: capturedAt, Bars: make([]float64, engine.bandCount)}
}
