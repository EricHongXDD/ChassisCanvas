package service

import (
	"context"
	"sync"
	"time"

	"casescreen/internal/device"
)

// HostFrameStats 记录主机帧在本次服务运行期内的实际投递情况。
// Dropped 同时包含被更新帧覆盖、主题代次过期、断线和发送失败的帧。
type HostFrameStats struct {
	Sent       uint64
	Dropped    uint64
	LastSendAt time.Time
}

type queuedHostFrame struct {
	generation uint64
	data       []byte
}

// hostFrameWorker 为一台设备串行发送主机帧。队列容量固定为 1，
// 屏幕来不及消费时只保留最新帧，不会把旧画面排队补发。
type hostFrameWorker struct {
	service  *Service
	deviceID string
	driver   device.HostFrameDriver

	ctx    context.Context
	cancel context.CancelFunc
	frames chan queuedHostFrame
	done   chan struct{}

	offerMu sync.Mutex
}

func newHostFrameWorker(parent context.Context, service *Service, deviceID string, driver device.HostFrameDriver) *hostFrameWorker {
	ctx, cancel := context.WithCancel(parent)
	return &hostFrameWorker{
		service: service, deviceID: deviceID, driver: driver,
		ctx: ctx, cancel: cancel, frames: make(chan queuedHostFrame, 1), done: make(chan struct{}),
	}
}

func (worker *hostFrameWorker) run() {
	defer close(worker.done)
	for {
		// 先检查取消，避免在取消和队列同时就绪时多发一帧。
		select {
		case <-worker.ctx.Done():
			worker.dropPending()
			return
		default:
		}
		select {
		case <-worker.ctx.Done():
			worker.dropPending()
			return
		case frame := <-worker.frames:
			if !worker.service.hostFrameMaySend(worker.deviceID, frame.generation) {
				worker.service.recordHostFrameDropped(worker.deviceID, 1)
				continue
			}
			err := worker.driver.SendJPEG(worker.ctx, frame.data)
			if err == nil {
				worker.service.recordHostFrameSent(worker.deviceID, time.Now())
				worker.service.setTransferError(worker.deviceID, nil)
				continue
			}
			worker.service.recordHostFrameDropped(worker.deviceID, 1)
			if worker.ctx.Err() != nil {
				worker.dropPending()
				return
			}
			worker.service.setTransferError(worker.deviceID, err)
			// 确定断线时后续条件检查会停止重试；若驱动仍报告在线，
			// ACK 短暂超时只代表本帧未送达。丢掉旧帧并在有界退避后
			// 重新渲染当前主题，避免静态主题因签名未变化而永久不再发送。
			worker.dropPending()
			if !worker.waitBeforeRetry() {
				return
			}
			worker.service.requestHostFrameRetry(worker.deviceID, frame.generation)
		}
	}
}

func (worker *hostFrameWorker) waitBeforeRetry() bool {
	worker.service.mu.RLock()
	delay := worker.service.hostFrameRetryDelay
	worker.service.mu.RUnlock()
	if delay <= 0 {
		delay = defaultHostFrameRetryDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-worker.ctx.Done():
		worker.dropPending()
		return false
	case <-timer.C:
		return true
	}
}

// offer 返回新帧是否已入队，以及本次因覆盖或取消丢弃的帧数。
func (worker *hostFrameWorker) offer(frame queuedHostFrame) (bool, uint64) {
	worker.offerMu.Lock()
	defer worker.offerMu.Unlock()
	if worker.ctx.Err() != nil {
		return false, 1
	}
	select {
	case worker.frames <- frame:
		return true, 0
	default:
	}

	replaced := uint64(0)
	select {
	case <-worker.frames:
		replaced = 1
	default:
		// worker 刚好取走了旧帧，新帧仍可直接入队。
	}
	select {
	case worker.frames <- frame:
		return true, replaced
	case <-worker.ctx.Done():
		return false, replaced + 1
	}
}

func (worker *hostFrameWorker) stop() {
	worker.cancel()
	<-worker.done
}

func (worker *hostFrameWorker) dropPending() {
	dropped := uint64(0)
	for {
		select {
		case <-worker.frames:
			dropped++
		default:
			worker.service.recordHostFrameDropped(worker.deviceID, dropped)
			return
		}
	}
}

func (service *Service) HostFrameStats(deviceID string) HostFrameStats {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.hostFrameStats[deviceID]
}

func (service *Service) enqueueHostFrame(ctx context.Context, deviceID string, driver device.HostFrameDriver, generation uint64, data []byte) bool {
	service.mu.Lock()
	if service.hostFrameWorkersClosed || service.orientationCalibrationActiveLocked(deviceID) ||
		service.hostFrameSuspended[deviceID] ||
		service.hostFrameGenerations[deviceID] != generation || !service.hostFrames[deviceID] ||
		!service.statuses[deviceID].Connected || service.statuses[deviceID].PoweredOff {
		stats := service.hostFrameStats[deviceID]
		stats.Dropped++
		service.hostFrameStats[deviceID] = stats
		service.mu.Unlock()
		return false
	}
	worker := service.hostFrameWorkers[deviceID]
	start := false
	if worker == nil {
		worker = newHostFrameWorker(ctx, service, deviceID, driver)
		service.hostFrameWorkers[deviceID] = worker
		start = true
	}
	service.mu.Unlock()
	if start {
		go worker.run()
	}
	accepted, dropped := worker.offer(queuedHostFrame{generation: generation, data: data})
	service.recordHostFrameDropped(deviceID, dropped)
	return accepted
}

func (service *Service) hostFrameMaySend(deviceID string, generation uint64) bool {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return !service.hostFrameWorkersClosed && !service.orientationCalibrationActiveLocked(deviceID) &&
		!service.hostFrameSuspended[deviceID] &&
		service.hostFrameGenerations[deviceID] == generation && service.hostFrames[deviceID] &&
		service.statuses[deviceID].Connected && !service.statuses[deviceID].PoweredOff
}

func (service *Service) requestHostFrameRetry(deviceID string, generation uint64) {
	service.mu.Lock()
	retry := !service.hostFrameWorkersClosed && !service.orientationCalibrationActiveLocked(deviceID) &&
		!service.hostFrameSuspended[deviceID] &&
		service.hostFrameGenerations[deviceID] == generation && service.hostFrames[deviceID] &&
		service.statuses[deviceID].Connected && !service.statuses[deviceID].PoweredOff
	if retry {
		service.renderDirty = true
	}
	service.mu.Unlock()
	if retry {
		service.wakeRenderer()
	}
}

func (service *Service) recordHostFrameSent(deviceID string, sentAt time.Time) {
	service.mu.Lock()
	stats := service.hostFrameStats[deviceID]
	stats.Sent++
	stats.LastSendAt = sentAt
	service.hostFrameStats[deviceID] = stats
	service.mu.Unlock()
}

func (service *Service) recordHostFrameDropped(deviceID string, count uint64) {
	if count == 0 {
		return
	}
	service.mu.Lock()
	stats := service.hostFrameStats[deviceID]
	stats.Dropped += count
	service.hostFrameStats[deviceID] = stats
	service.mu.Unlock()
}

func (service *Service) stopHostFrameWorker(deviceID string) {
	service.mu.Lock()
	worker := service.hostFrameWorkers[deviceID]
	delete(service.hostFrameWorkers, deviceID)
	service.mu.Unlock()
	if worker != nil {
		worker.stop()
	}
}

func (service *Service) suspendHostFrameDelivery(deviceID string) {
	service.mu.Lock()
	service.hostFrameGenerations[deviceID]++
	service.hostFrameSuspended[deviceID] = true
	service.mu.Unlock()
	service.stopHostFrameWorker(deviceID)
}

func (service *Service) resumeHostFrameDelivery(deviceID string) {
	// 方向测试图是设备级瞬态模式。主题恢复失败、电源恢复或其他通用
	// 收尾路径都可能调用这里，但不得在校准会话仍存在时重启旧 JPEG worker。
	service.mu.Lock()
	// 与 hostFrameSuspended 的读写放在同一 service.mu 临界区；再读取
	// orientation map，避免先读到 false、随后 Show 已登记会话并暂停，
	// 最后这个旧 false 又把暂停清掉的 TOCTOU。
	calibrating := service.orientationCalibrationActiveLocked(deviceID)
	status := service.statuses[deviceID]
	if calibrating {
		service.hostFrameSuspended[deviceID] = true
		service.mu.Unlock()
		return
	}
	if status.PoweredOff {
		// 主题传输失败的 defer 也会走到这里；用户的关屏
		// 意图优先级更高，不能因主题收尾而重新启动 JPEG 工作线程。
		service.hostFrameSuspended[deviceID] = true
		service.mu.Unlock()
		return
	}
	delete(service.hostFrameSuspended, deviceID)
	active := service.hostFrames[deviceID] && status.Connected
	if active {
		service.renderDirty = true
	}
	service.mu.Unlock()
	if active {
		service.wakeRenderer()
	}
}

func (service *Service) invalidateHostFrameDelivery(deviceID string) {
	service.mu.Lock()
	service.hostFrameGenerations[deviceID]++
	service.mu.Unlock()
	service.stopHostFrameWorker(deviceID)
}

func (service *Service) startHostFrameWorkers() {
	service.mu.Lock()
	service.hostFrameWorkersClosed = false
	service.mu.Unlock()
}

func (service *Service) stopAllHostFrameWorkers() {
	service.mu.Lock()
	service.hostFrameWorkersClosed = true
	workers := make([]*hostFrameWorker, 0, len(service.hostFrameWorkers))
	for _, worker := range service.hostFrameWorkers {
		workers = append(workers, worker)
	}
	service.hostFrameWorkers = make(map[string]*hostFrameWorker)
	service.mu.Unlock()
	// 先同时取消所有设备，再等待它们退出，避免多屏关闭时串行累加 pacing 延迟。
	for _, worker := range workers {
		worker.cancel()
	}
	for _, worker := range workers {
		<-worker.done
	}
}
