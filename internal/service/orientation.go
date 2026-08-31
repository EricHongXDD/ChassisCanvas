package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"casescreen/internal/device"
	"casescreen/internal/theme"
)

const (
	defaultOrientationRestoreAttemptTimeout = 5 * time.Minute
	defaultOrientationRestoreRetryInitial   = 2 * time.Second
	defaultOrientationRestoreRetryMaximum   = 30 * time.Second
	defaultOrientationShutdownTimeout       = 30 * time.Second
)

type orientationCalibrationSession struct {
	context                 context.Context
	cancel                  context.CancelFunc
	once                    sync.Once
	automaticRestoreStarted bool
	keepaliveStarted        bool
}

func newOrientationCalibrationSession() *orientationCalibrationSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &orientationCalibrationSession{context: ctx, cancel: cancel}
}

func (session *orientationCalibrationSession) stop() {
	if session != nil {
		session.once.Do(session.cancel)
	}
}

// ShowOrientationCalibration 暂停目标屏的普通主机帧，显示不经过当前
// 旋转变换的方向标识图。这样用户看到的数字才代表真实安装方向。
func (service *Service) ShowOrientationCalibration(ctx context.Context, deviceID string) error {
	driver, _, _, err := service.orientationTarget(deviceID)
	if err != nil {
		return err
	}
	calibrator, ok := driver.(device.OrientationCalibrationDriver)
	if !ok {
		return errors.New("目标副屏不支持方向校准")
	}
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	unlockDevice, err := service.lockDeviceTheme(operationContext, deviceID)
	if err != nil {
		return err
	}
	defer unlockDevice()
	// 主题可能在等待设备门闩时被另一次操作更换，必须在门闩内重新确认。
	if _, _, hasTheme, targetErr := service.orientationTarget(deviceID); targetErr != nil {
		return targetErr
	} else if !hasTheme {
		return errors.New("当前副屏没有可恢复的主题；请先应用一个主题，再显示方向测试图")
	}

	// 先登记 service 级瞬态模式，再暂停 worker 和发送测试 JPEG。这样并发的
	// 亮度恢复即使恰好进入 resumeHostFrameDelivery，也会看到校准正在进行，
	// 不可能在 Show 成功与会话登记之间插入一张普通主题帧。
	session, created, registered := service.beginOrientationCalibration(deviceID)
	if !registered {
		return errServiceClosing
	}
	service.suspendHostFrameDelivery(deviceID)
	if err := calibrator.ShowOrientationCalibration(operationContext); err != nil {
		// 重发失败时保留原会话；首次显示失败则撤销刚登记的模式并恢复主机帧。
		if created {
			service.finishOrientationCalibration(deviceID, session)
			service.resumeHostFrameDelivery(deviceID)
		}
		service.setDriverError(driver, err)
		return err
	}
	service.setDriverError(driver, nil)
	service.scheduleOrientationCalibrationKeepalive(deviceID, session, calibrator)
	service.scheduleOrientationCalibrationRestore(deviceID, session, service.orientationCalibrationDelay())
	return nil
}

// SetDisplayRotation 立即更新运行中驱动，并用相同主题重新建立离线资源
// 或主机帧链路。只有主题恢复成功后才结束校准会话。
func (service *Service) SetDisplayRotation(ctx context.Context, deviceID string, rotation int) error {
	driver, _, _, err := service.orientationTarget(deviceID)
	if err != nil {
		return err
	}
	calibrator, ok := driver.(device.OrientationCalibrationDriver)
	if !ok {
		return errors.New("目标副屏不支持方向校准")
	}
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	unlockDevice, err := service.lockDeviceTheme(operationContext, deviceID)
	if err != nil {
		return err
	}
	defer unlockDevice()
	// 不能使用等待门闩之前的主题快照：并发的普通主题可能已经
	// 先提交，若继续使用旧快照会把用户的新主题覆盖回去。
	currentDriver, manifest, hasTheme, err := service.orientationTarget(deviceID)
	if err != nil {
		return err
	}
	if currentCalibrator, supported := currentDriver.(device.OrientationCalibrationDriver); supported {
		driver = currentDriver
		calibrator = currentCalibrator
	} else {
		return errors.New("目标副屏不支持方向校准")
	}

	if err := calibrator.SetDisplayRotation(rotation); err != nil {
		return err
	}
	service.invalidateHostFrameDelivery(deviceID)
	if !hasTheme {
		if service.hasOrientationCalibration(deviceID) {
			return errors.New("方向已更新，但测试图没有可恢复的当前主题")
		}
		service.resumeHostFrameDelivery(deviceID)
		return nil
	}
	if err := service.restoreOrientationThemeLocked(operationContext, deviceID, driver, manifest); err != nil {
		return err
	}
	if service.finishOrientationCalibration(deviceID, nil) {
		service.resumeHostFrameDelivery(deviceID)
	}
	return nil
}

// CancelOrientationCalibration 恢复校准前的当前主题，不修改已保存方向。
// 恢复失败时会话仍在，用户可立即重试，后台也会按有界退避继续恢复。
func (service *Service) CancelOrientationCalibration(ctx context.Context, deviceID string) error {
	return service.restoreOrientationTheme(ctx, deviceID)
}

func (service *Service) restoreOrientationTheme(ctx context.Context, deviceID string) error {
	_, _, _, err := service.orientationTarget(deviceID)
	if err != nil {
		return err
	}
	operationContext, finishOperation, err := service.beginThemeOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	unlockDevice, err := service.lockDeviceTheme(operationContext, deviceID)
	if err != nil {
		return err
	}
	defer unlockDevice()
	driver, manifest, hasTheme, err := service.orientationTarget(deviceID)
	if err != nil {
		return err
	}
	if !hasTheme {
		return errors.New("方向测试图没有可恢复的当前主题")
	}
	if err := service.restoreOrientationThemeLocked(operationContext, deviceID, driver, manifest); err != nil {
		return err
	}
	// 必须在释放设备门闩前结束会话。否则重连或普通主题可能在“主题已恢复”
	// 与“会话已删除”之间抢到门闩，又把测试图重新发回屏幕。
	if service.finishOrientationCalibration(deviceID, nil) {
		service.resumeHostFrameDelivery(deviceID)
	}
	return nil
}

// restoreOrientationThemeWithoutOperation 仅用于主题操作门禁已关闭的退出阶段。
// 退出前已取消并等待所有普通主题操作，因此可直接使用设备门闩。
func (service *Service) restoreOrientationThemeWithoutOperation(ctx context.Context, deviceID string) error {
	_, _, _, err := service.orientationTarget(deviceID)
	if err != nil {
		return err
	}
	unlockDevice, err := service.lockDeviceTheme(ctx, deviceID)
	if err != nil {
		return err
	}
	defer unlockDevice()
	driver, manifest, hasTheme, err := service.orientationTarget(deviceID)
	if err != nil {
		return err
	}
	if !hasTheme {
		return errors.New("方向测试图没有可恢复的当前主题")
	}
	return service.restoreOrientationThemeLocked(ctx, deviceID, driver, manifest)
}

func (service *Service) restoreOrientationThemeLocked(
	ctx context.Context, deviceID string, driver device.Driver, manifest theme.Manifest,
) error {
	results := service.applyThemeToLocked(ctx, deviceID, driver, manifest)
	if applyErr, exists := results[deviceID]; exists {
		if applyErr == nil {
			return nil
		}
		var refreshFailure *themeStateRefreshError
		if errors.As(applyErr, &refreshFailure) {
			// 主题画面和 host-frame 运行时已经完成提交；只有小状态补发失败。
			// 此时物理屏不再显示测试图，必须结束校准会话，pending 主题会在
			// 后续重连中继续补齐动态状态。
			return nil
		}
		if !service.hasOrientationCalibration(deviceID) {
			return applyErr
		}
		// 普通失败可能发生在设备已经收到主题、但亮度等收尾命令尚未
		// 成功的阶段。校准会话仍然是活跃状态，因此必须重新显示测试图，
		// 保证物理画面与“仍在测试”的服务状态一致，用户随后可以重试恢复。
		service.suspendHostFrameDelivery(deviceID)
		if calibrator, ok := driver.(device.OrientationCalibrationDriver); ok {
			if showErr := calibrator.ShowOrientationCalibration(ctx); showErr != nil {
				combinedErr := fmt.Errorf("恢复当前主题失败，且重新显示方向测试图也失败: %w", errors.Join(applyErr, showErr))
				service.scheduleOrientationCalibrationReconnect(deviceID, combinedErr)
				return combinedErr
			}
		}
		return applyErr
	}
	return errors.New("恢复方向校准主题未返回设备结果")
}

// scheduleOrientationCalibrationReconnect 强制把“测试图重发失败”视为
// 当前 USB 会话不可用。即使底层句柄仍报告在线，也必须经过重连分支；
// 活跃校准的重连只会重发测试图，不会应用 pending 普通主题。
func (service *Service) scheduleOrientationCalibrationReconnect(deviceID string, failure error) {
	service.mu.Lock()
	status := service.statuses[deviceID]
	status = service.recordTransferDisconnectLocked(status, failure, time.Now())
	service.statuses[deviceID] = status
	service.mu.Unlock()
}

// beginOrientationCalibration 在任何可能重新启动普通帧的路径之前登记模式。
// 已存在会话时复用它，避免一次重复“显示测试图”失败后丢失恢复入口。
func (service *Service) beginOrientationCalibration(deviceID string) (*orientationCalibrationSession, bool, bool) {
	service.orientationCalibrationMu.Lock()
	defer service.orientationCalibrationMu.Unlock()
	if service.orientationCalibrationClosing {
		return nil, false, false
	}
	if current := service.orientationCalibrations[deviceID]; current != nil {
		return current, false, true
	}
	session := newOrientationCalibrationSession()
	if service.orientationCalibrations == nil {
		service.orientationCalibrations = make(map[string]*orientationCalibrationSession)
	}
	service.orientationCalibrations[deviceID] = session
	return session, true, true
}

// scheduleOrientationCalibrationKeepalive 只为同一个校准会话启动一个保活循环。
// 普通方向驱动不实现可选接口时保持原行为，不产生额外 goroutine。
func (service *Service) scheduleOrientationCalibrationKeepalive(
	deviceID string, session *orientationCalibrationSession, calibrator device.OrientationCalibrationDriver,
) {
	keepalive, supported := calibrator.(device.OrientationCalibrationKeepaliveDriver)
	if !supported || session == nil {
		return
	}
	interval := keepalive.OrientationCalibrationKeepaliveInterval()
	if interval <= 0 {
		return
	}
	service.orientationCalibrationMu.Lock()
	if service.orientationCalibrationClosing || service.orientationCalibrations[deviceID] != session || session.keepaliveStarted {
		service.orientationCalibrationMu.Unlock()
		return
	}
	session.keepaliveStarted = true
	service.orientationCalibrationWG.Add(1)
	service.orientationCalibrationMu.Unlock()
	go service.runOrientationCalibrationKeepalive(deviceID, session, interval)
}

func (service *Service) runOrientationCalibrationKeepalive(
	deviceID string, session *orientationCalibrationSession, interval time.Duration,
) {
	defer service.orientationCalibrationWG.Done()
	// 使用固定节拍而不是“上一次完成后再等待”。这样偶发的 ACK
	// 等待不会累加到 4 秒周期，始终给设备 5 秒看门狗留出余量。
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-session.context.Done():
			return
		case <-ticker.C:
		}
		operationContext, finishOperation, err := service.beginThemeOperation(session.context)
		if err != nil {
			return
		}
		unlockDevice, lockErr := service.lockDeviceTheme(operationContext, deviceID)
		if lockErr != nil {
			finishOperation()
			if session.context.Err() != nil || !service.isOrientationCalibrationCurrent(deviceID, session) {
				return
			}
			service.setError(deviceID, fmt.Errorf("方向测试图保活等待设备失败: %w", lockErr))
			continue
		}

		// 恢复、确认或取消可能在保活等待设备门闩期间完成。必须在门闩内
		// 再检查会话，确保恢复主题之后绝不会补发迟到的保活包。
		if !service.isOrientationCalibrationCurrent(deviceID, session) {
			unlockDevice()
			finishOperation()
			return
		}
		driver, _, _, targetErr := service.orientationTarget(deviceID)
		keepalive, supported := driver.(device.OrientationCalibrationKeepaliveDriver)
		if targetErr == nil && !supported {
			targetErr = errors.New("目标副屏不再支持方向测试图保活")
		}
		if targetErr == nil {
			targetErr = keepalive.SendOrientationCalibrationKeepalive(operationContext)
		}
		unlockDevice()
		finishOperation()

		if session.context.Err() != nil || !service.isOrientationCalibrationCurrent(deviceID, session) {
			return
		}
		if targetErr != nil {
			// 驱动明确报告句柄断开时，setDriverError 会沿用既有退避重连；
			// 仍在线的单次 ACK 超时只记录错误，不能切回普通主题。
			if driver != nil {
				service.setDriverError(driver, fmt.Errorf("方向测试图保活失败: %w", targetErr))
			} else {
				service.setError(deviceID, fmt.Errorf("方向测试图保活失败: %w", targetErr))
			}
			continue
		}
		service.setDriverError(driver, nil)
	}
}

func (service *Service) scheduleOrientationCalibrationRestore(
	deviceID string, session *orientationCalibrationSession, firstDelay time.Duration,
) {
	// 正式工作台不设置倒计时：方向数字必须持续驻留，避免用户观察或
	// 选择期间被普通主题悄悄覆盖。退出服务仍由关闭安全网恢复原主题。
	if firstDelay <= 0 || session == nil {
		return
	}
	service.orientationCalibrationMu.Lock()
	if service.orientationCalibrationClosing || service.orientationCalibrations[deviceID] != session || session.automaticRestoreStarted {
		service.orientationCalibrationMu.Unlock()
		return
	}
	session.automaticRestoreStarted = true
	service.orientationCalibrationWG.Add(1)
	service.orientationCalibrationMu.Unlock()
	go service.runOrientationCalibrationRestore(deviceID, session, firstDelay)
}

func (service *Service) runOrientationCalibrationRestore(
	deviceID string, session *orientationCalibrationSession, firstDelay time.Duration,
) {
	defer service.orientationCalibrationWG.Done()
	delay := max(firstDelay, time.Duration(0))
	retryDelay := min(service.orientationRestoreRetryInitialDuration(), service.orientationRestoreRetryMaximumDuration())
	for {
		if !waitOrientationDelay(session.context, delay) {
			return
		}
		attemptContext, cancel := context.WithTimeout(session.context, service.orientationRestoreAttemptDuration())
		err := service.restoreOrientationTheme(attemptContext, deviceID)
		cancel()
		if err == nil {
			if service.finishOrientationCalibration(deviceID, session) {
				service.resumeHostFrameDelivery(deviceID)
			}
			return
		}
		if session.context.Err() != nil || !service.isOrientationCalibrationCurrent(deviceID, session) {
			return
		}
		service.setError(deviceID, fmt.Errorf("方向测试图自动恢复失败，将自动重试: %w", err))
		delay = retryDelay
		retryDelay = boundedOrientationBackoff(retryDelay, service.orientationRestoreRetryMaximumDuration())
	}
}

func waitOrientationDelay(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func boundedOrientationBackoff(current, maximum time.Duration) time.Duration {
	if maximum <= 0 {
		maximum = defaultOrientationRestoreRetryMaximum
	}
	if current <= 0 {
		return min(defaultOrientationRestoreRetryInitial, maximum)
	}
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}

func (service *Service) finishOrientationCalibration(deviceID string, expected *orientationCalibrationSession) bool {
	service.orientationCalibrationMu.Lock()
	session := service.orientationCalibrations[deviceID]
	if session == nil || (expected != nil && session != expected) {
		service.orientationCalibrationMu.Unlock()
		return false
	}
	delete(service.orientationCalibrations, deviceID)
	service.orientationCalibrationMu.Unlock()
	session.stop()
	return true
}

func (service *Service) hasOrientationCalibration(deviceID string) bool {
	service.orientationCalibrationMu.Lock()
	defer service.orientationCalibrationMu.Unlock()
	return service.orientationCalibrations[deviceID] != nil
}

// orientationCalibrationActiveLocked 要求调用者已持有 service.mu
// 的读锁或写锁。所有主机帧最终门禁统一使用
// service.mu -> orientationCalibrationMu 的锁顺序，避免恢复和发帧交叉。
func (service *Service) orientationCalibrationActiveLocked(deviceID string) bool {
	service.orientationCalibrationMu.Lock()
	defer service.orientationCalibrationMu.Unlock()
	return service.orientationCalibrations[deviceID] != nil
}

// OrientationCalibrationActive 向本机 API 暴露权威的设备级瞬态状态，
// 让工作台重载或丢失 POST 响应后仍能恢复确认/取消入口。
func (service *Service) OrientationCalibrationActive(deviceID string) bool {
	return service.hasOrientationCalibration(deviceID)
}

func (service *Service) isOrientationCalibrationCurrent(
	deviceID string, expected *orientationCalibrationSession,
) bool {
	service.orientationCalibrationMu.Lock()
	defer service.orientationCalibrationMu.Unlock()
	return service.orientationCalibrations[deviceID] == expected
}

// cancelAllOrientationCalibrations 取消定时器和正在恢复的请求，并等待全部
// goroutine 退出。这个方法不负责恢复画面，服务退出使用下方的安全网。
func (service *Service) cancelAllOrientationCalibrations() {
	service.orientationCalibrationMu.Lock()
	service.orientationCalibrationClosing = true
	sessions := service.orientationCalibrations
	service.orientationCalibrations = nil
	service.orientationCalibrationMu.Unlock()
	for _, session := range sessions {
		session.stop()
	}
	service.orientationCalibrationWG.Wait()
}

// restoreOrientationCalibrationsBeforeShutdown 在 USB 驱动关闭前尽最后一次
// 有界恢复。所有目标共用同一个总截止时间并并行执行，设备数量不会线性拖长退出。
func (service *Service) restoreOrientationCalibrationsBeforeShutdown() {
	service.orientationCalibrationMu.Lock()
	service.orientationCalibrationClosing = true
	sessions := make(map[string]*orientationCalibrationSession, len(service.orientationCalibrations))
	ids := make([]string, 0, len(service.orientationCalibrations))
	for id, session := range service.orientationCalibrations {
		sessions[id] = session
		ids = append(ids, id)
	}
	service.orientationCalibrationMu.Unlock()
	if len(ids) == 0 {
		return
	}
	for _, session := range sessions {
		session.stop()
	}
	service.orientationCalibrationWG.Wait()
	sort.Strings(ids)

	shutdownContext, cancel := context.WithTimeout(context.Background(), service.orientationShutdownRestoreDuration())
	defer cancel()
	var wait sync.WaitGroup
	for _, id := range ids {
		id := id
		session := sessions[id]
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := service.restoreOrientationThemeWithoutOperation(shutdownContext, id); err != nil {
				service.setError(id, fmt.Errorf("服务退出前恢复方向测试图失败: %w", err))
				return
			}
			if service.finishOrientationCalibration(id, session) {
				service.resumeHostFrameDelivery(id)
			}
		}()
	}
	wait.Wait()
}

func (service *Service) orientationCalibrationDelay() time.Duration {
	if service.orientationCalibrationTimeout > 0 {
		return service.orientationCalibrationTimeout
	}
	return 0
}

func (service *Service) orientationRestoreAttemptDuration() time.Duration {
	if service.orientationRestoreAttemptTimeout > 0 {
		return service.orientationRestoreAttemptTimeout
	}
	return defaultOrientationRestoreAttemptTimeout
}

func (service *Service) orientationRestoreRetryInitialDuration() time.Duration {
	if service.orientationRestoreRetryInitial > 0 {
		return service.orientationRestoreRetryInitial
	}
	return defaultOrientationRestoreRetryInitial
}

func (service *Service) orientationRestoreRetryMaximumDuration() time.Duration {
	if service.orientationRestoreRetryMaximum > 0 {
		return service.orientationRestoreRetryMaximum
	}
	return defaultOrientationRestoreRetryMaximum
}

func (service *Service) orientationShutdownRestoreDuration() time.Duration {
	if service.orientationShutdownTimeout > 0 {
		return service.orientationShutdownTimeout
	}
	return defaultOrientationShutdownTimeout
}

func (service *Service) orientationTarget(deviceID string) (device.Driver, theme.Manifest, bool, error) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	var target device.Driver
	for _, candidate := range service.drivers {
		if candidate.Info().ID == deviceID {
			target = candidate
			break
		}
	}
	if target == nil {
		return nil, theme.Manifest{}, false, fmt.Errorf("未找到设备 %q", deviceID)
	}
	if manifest, exists := service.deviceThemes[deviceID]; exists {
		return target, copyManifest(manifest), true, nil
	}
	if service.activeTheme != nil {
		return target, copyManifest(*service.activeTheme), true, nil
	}
	return target, theme.Manifest{}, false, nil
}
