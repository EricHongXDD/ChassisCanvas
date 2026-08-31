package coolermaster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"casescreen/internal/coolergeometry"
	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/coolermaster"
	"casescreen/internal/render"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

type HIDTransport interface {
	Write(context.Context, []byte, time.Duration) (int, error)
	Read(context.Context, []byte, time.Duration) (int, error)
	Close() error
}

type storedSlot struct {
	Digest               string `json:"digest"`
	FileName             string `json:"file_name"`
	Uncertain            bool   `json:"uncertain,omitempty"`
	CommittedAtUnixMilli int64  `json:"committed_at_unix_ms,omitempty"`
	ReplaceVersion       int    `json:"replace_version,omitempty"`
	LegacyAliasMigration string `json:"legacy_alias_migration,omitempty"`
}

type storedState struct {
	// TrustVersion 记录槽位摘要是否由具备外部控制器失效保护的版本建立。
	// 旧版本没有该字段；它们可能在 MasterCTRL 抢占后仍把旧 digest 当真，
	// 因此首次升级必须重传一次，不能直接命中离线资源快路径。
	TrustVersion int                `json:"trust_version,omitempty"`
	Slots        map[int]storedSlot `json:"slots"`
}

type pendingTransportOpen struct {
	done      chan struct{}
	canceled  chan struct{}
	result    openTransportResult
	finished  bool
	discarded bool
	claimed   bool
	waiters   int
}

var errTransportOpenCanceled = errors.New("打开任务已取消")
var errEmptyMediaFile = errors.New("媒体文件为空")

// transportIOError 只标记 HID 句柄本身已无法读写的确定错误。
// 调用方超时、固件未回 ACK 和过期 ACK 都不证明 USB 已拔出，
// 不能因此关闭仍可写的句柄。
type transportIOError struct {
	cause error
}

func (failure *transportIOError) Error() string { return failure.cause.Error() }
func (failure *transportIOError) Unwrap() error { return failure.cause }

func fatalTransportFailure(err error) bool {
	var failure *transportIOError
	return errors.As(err, &failure)
}

// commandResponseTimeoutError 只表示命令报文已经完整写出，随后读取
// 对应 ACK 时设备内部等待超时。它保留 DeadlineExceeded 错误链，供
// 调用方展示原始原因，但不会把写超时、暂停超时或调用方取消混进来。
type commandResponseTimeoutError struct {
	cause error
}

func (failure *commandResponseTimeoutError) Error() string { return failure.cause.Error() }
func (failure *commandResponseTimeoutError) Unwrap() error { return failure.cause }

func commandResponseTimedOut(err error) bool {
	var failure *commandResponseTimeoutError
	return errors.As(err, &failure)
}

// commandResponseError 表示命令已经完整写出，但固件返回了
// NACK、损坏帧或其他无法匹配的响应。它与写入失败、HID
// 句柄故障和调用方取消分开，便于离线资源严格复制原厂
// FlashUpdate 的有界响应重试。
type commandResponseError struct {
	cause error
}

func (failure *commandResponseError) Error() string { return failure.cause.Error() }
func (failure *commandResponseError) Unwrap() error { return failure.cause }

func commandResponseFailed(err error) bool {
	var timeout *commandResponseTimeoutError
	var failure *commandResponseError
	return errors.As(err, &timeout) || errors.As(err, &failure)
}

const (
	offlineReplaceVersion = 2
	// offlineStateTrustVersion 与离线协议版本分开：协议没有变化，变化的是
	// 本地摘要的可信边界。新版成功提交一次后即可继续正常去重。
	offlineStateTrustVersion = 1
	offlineUploadAttempts    = 5
	offlineResponseTimeout   = time.Second
	offlineStaleBarrierLimit = 5 * time.Second
	// 真机连续更换离线资源时，前一次 commit 后立即
	// 开始新传输会让后续 transported 持续无回包；静置
	// 失败的5轮本身持续 26.9s，再静置12s后才恢复，因此
	// 从最近一次成功 commit 起保守留45s，避免失败尝试
	// 反复刷新固件忙状态。
	offlineCommitCooldown        = 45 * time.Second
	offlineRetryInitialDelay     = 2 * time.Second
	offlineRetryMaximumDelay     = 12 * time.Second
	brightnessResponseReads      = 2
	legacySlot5DeleteAttempts    = 6
	legacySlot5MigrationPending  = "pending"
	legacySlot5MigrationApproved = "approved"
	legacySlot5MigrationComplete = "complete"
	// ATMOS V2 固件的静态实时画面看门狗为 5 秒。真机证明原厂的
	// timestamp/realtimeDisplay 小命令只维持通讯和模式，不能续期一张
	// 不再变化的 JPEG，因此方向测试每 2 秒重发缓存帧。即使一次调度
	// 延迟，下一次刷新仍有充足余量，不会在第 5 秒恢复离线主题。
	orientationCalibrationKeepaliveInterval = 2 * time.Second
	orientationKeepaliveResponseTimeout     = 500 * time.Millisecond
)

type Driver struct {
	ID         string
	Name       string
	Resolution [2]int
	Transport  HIDTransport
	Opener     func() (HIDTransport, error)
	OpenError  error
	StatePath  string
	FFmpegPath string
	Slot       int
	// ConnectDelay 给 HID 句柄关闭后重新打开的固件留出稳定时间。
	ConnectDelay time.Duration

	mu            sync.Mutex
	connected     bool
	realtime      bool
	realtimeKnown bool
	activeTheme   theme.Manifest
	pause         func(context.Context, time.Duration) error
	// lastSuccessfulRealtimeIO 记录最近一帧完整 JPEG 写入的时刻。
	// 实时模式下该成功写入比 param 查询更能证明设备在线。
	lastSuccessfulRealtimeIO time.Time
	pendingOpen              *pendingTransportOpen
	now                      func() time.Time
	offlineCommitUncertain   bool
	lastOfflineCommit        time.Time
	stateSaver               func(string, storedState) error
	displayRotation          atomic.Int32
	settingsMu               sync.RWMutex
	mediaMu                  sync.Mutex
	orientationFrameOnce     sync.Once
	orientationFrame         []byte
	orientationFrameErr      error
}

var _ device.BrightnessDriver = (*Driver)(nil)
var _ device.HostFrameTransformer = (*Driver)(nil)
var _ device.OrientationCalibrationDriver = (*Driver)(nil)
var _ device.OrientationCalibrationKeepaliveDriver = (*Driver)(nil)

func (driver *Driver) Connected() bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.connected && driver.Transport != nil
}

func New(id, name string, resolution [2]int, slot int, transport HIDTransport, statePath string) *Driver {
	if resolution[0] <= 0 || resolution[1] <= 0 {
		resolution = [2]int{480, 480}
	}
	return &Driver{
		ID: id, Name: name, Resolution: resolution, Slot: slot,
		Transport: transport, StatePath: statePath, ConnectDelay: 500 * time.Millisecond,
		pause: sleepContext, now: time.Now, stateSaver: saveState,
	}
}

func (driver *Driver) Info() device.Info {
	return device.Info{
		ID: driver.ID, Name: driver.Name, Resolution: driver.Resolution,
		DisplayShape: "circle_in_hexagon_point_down", DisplayRotation: driver.DisplayRotation(),
		Capabilities: device.Capabilities{
			DeliveryMode:       device.DeliveryHostFrame,
			StoresResources:    true,
			HostRenderedFrames: true,
		},
	}
}

// SetDisplayRotation 保存安装方向补偿。任意整数角都可用，存储前归一化到 0..359。
func (driver *Driver) SetDisplayRotation(rotation int) error {
	driver.displayRotation.Store(int32(coolergeometry.NormalizeRotation(rotation)))
	return nil
}

func (driver *Driver) DisplayRotation() int {
	return int(driver.displayRotation.Load())
}

// SetFFmpegPath 设置离线视频方向转码器。空值表示运行时优先查找发行目录。
func (driver *Driver) SetFFmpegPath(path string) {
	driver.settingsMu.Lock()
	driver.FFmpegPath = strings.TrimSpace(path)
	driver.settingsMu.Unlock()
}

func (driver *Driver) TransformHostFrame(frame image.Image) image.Image {
	return coolergeometry.Transform(frame, driver.DisplayRotation())
}

func (driver *Driver) HostFrameTransformKey() string {
	return fmt.Sprintf(
		"coolermaster-circle-v1:%dx%d:r%d",
		driver.Resolution[0], driver.Resolution[1], driver.DisplayRotation(),
	)
}

// orientationCalibrationJPEG 只编码一次方向测试图。保活期间复用不可变
// JPEG，避免为了抵抗固件看门狗而重复占用 CPU。
func (driver *Driver) orientationCalibrationJPEG() ([]byte, error) {
	driver.orientationFrameOnce.Do(func() {
		calibration := coolergeometry.CalibrationImage(driver.Resolution[0], driver.Resolution[1])
		var output bytes.Buffer
		if err := jpeg.Encode(&output, calibration, &jpeg.Options{Quality: 94}); err != nil {
			driver.orientationFrameErr = fmt.Errorf("编码酷冷至尊方向校准图: %w", err)
			return
		}
		driver.orientationFrame = output.Bytes()
	})
	return driver.orientationFrame, driver.orientationFrameErr
}

// ShowOrientationCalibration 直接发送原始像素方向的钟面测试图，不套用已保存的旋转。
func (driver *Driver) ShowOrientationCalibration(ctx context.Context) error {
	data, err := driver.orientationCalibrationJPEG()
	if err != nil {
		return err
	}
	return driver.SendJPEG(ctx, data)
}

func (*Driver) OrientationCalibrationKeepaliveInterval() time.Duration {
	return orientationCalibrationKeepaliveInterval
}

// SendOrientationCalibrationKeepalive 先复制原厂 SendKeepAlive：发送 Unix
// 毫秒 timestamp，再强制 realtimeDisplay=true，两条命令都使用 SeqNumber=42。
// 随后重发缓存的同一张测试 JPEG。真机上小命令会正常 ACK，但不足以续期
// 静态帧；重发 JPEG 才能防止固件在第 5 秒恢复离线主题。整个过程不会
// 写入离线槽位或闪存。
func (driver *Driver) SendOrientationCalibrationKeepalive(ctx context.Context) error {
	data, err := driver.orientationCalibrationJPEG()
	if err != nil {
		return err
	}
	driver.mu.Lock()
	if !driver.connected || driver.Transport == nil {
		driver.mu.Unlock()
		return errors.New("酷冷至尊屏幕未连接")
	}
	capturedAt := driver.nowTime()
	_, timestampErr := driver.sendCommandLockedTimeout(
		ctx,
		"timestamp",
		protocol.RealtimeKeepaliveCommandSeq,
		protocol.RealtimeKeepaliveTimestampPayload(capturedAt),
		orientationKeepaliveResponseTimeout,
	)
	if timestampErr != nil && (ctx.Err() != nil || fatalTransportFailure(timestampErr)) {
		driver.disconnectOnFatalTransportErrorLocked(timestampErr)
		driver.mu.Unlock()
		return fmt.Errorf("发送酷冷至尊实时画面时间戳保活: %w", timestampErr)
	}

	_, modeErr := driver.sendCommandLockedTimeout(
		ctx,
		"realtimeDisplay",
		protocol.RealtimeKeepaliveCommandSeq,
		map[string]bool{"enable": true},
		orientationKeepaliveResponseTimeout,
	)
	if modeErr != nil {
		if ctx.Err() != nil || fatalTransportFailure(modeErr) {
			driver.disconnectOnFatalTransportErrorLocked(modeErr)
			driver.mu.Unlock()
			return fmt.Errorf("续期酷冷至尊实时画面: %w", modeErr)
		}
	}
	// 原厂最终结果只取第二条 realtimeDisplay 命令。timestamp 即使
	// 仅丢失 ACK，命令也已经完整写出，不能因此跳过真正的模式续期。
	driver.realtime = true
	driver.realtimeKnown = true
	driver.lastSuccessfulRealtimeIO = driver.nowTime()
	driver.mu.Unlock()

	// SendJPEG 会重新获取驱动锁并消费实时帧 ACK。即使小命令仅丢失
	// ACK，只要完整帧写入成功，就能证明设备仍在线并真正续期画面。
	if err := driver.SendJPEG(ctx, data); err != nil {
		return fmt.Errorf("刷新酷冷至尊方向测试图: %w", err)
	}
	return nil
}

func (driver *Driver) Connect(ctx context.Context) error {
	driver.mu.Lock()
	if driver.Slot < 1 || driver.Slot > 5 {
		driver.mu.Unlock()
		return fmt.Errorf("酷冷至尊离线槽位必须在 1..5: %d", driver.Slot)
	}
	if driver.connected && driver.Transport != nil {
		driver.mu.Unlock()
		return nil
	}
	if driver.OpenError != nil {
		if driver.Opener == nil {
			err := driver.OpenError
			driver.mu.Unlock()
			return fmt.Errorf("打开酷冷至尊 HID: %w", err)
		}
		driver.OpenError = nil
	}
	if driver.Transport == nil && driver.Opener != nil {
		pending := driver.pendingOpen
		if pending == nil {
			pending = &pendingTransportOpen{done: make(chan struct{}), canceled: make(chan struct{})}
			driver.pendingOpen = pending
			go driver.runTransportOpen(pending, driver.Opener)
		}
		pending.waiters++
		driver.mu.Unlock()
		select {
		case <-pending.done:
		case <-pending.canceled:
		case <-ctx.Done():
			driver.mu.Lock()
			pending.waiters--
			driver.mu.Unlock()
			return fmt.Errorf("打开酷冷至尊 HID: %w", ctx.Err())
		}
		driver.mu.Lock()
		pending.waiters--
		if pending.discarded {
			driver.mu.Unlock()
			return fmt.Errorf("打开酷冷至尊 HID: %w", errTransportOpenCanceled)
		}
		if err := ctx.Err(); err != nil {
			driver.mu.Unlock()
			return fmt.Errorf("打开酷冷至尊 HID: %w", err)
		}
		if driver.connected && driver.Transport != nil {
			driver.mu.Unlock()
			return nil
		}
		opened := pending.result
		if opened.err != nil {
			if driver.pendingOpen == pending {
				driver.pendingOpen = nil
				driver.OpenError = opened.err
			}
			driver.mu.Unlock()
			return fmt.Errorf("打开酷冷至尊 HID: %w", opened.err)
		}
		// opener 的结果会广播给所有等待者，但 HID 句柄只能被其中
		// 一个调用者接管。其余调用者在首个调用者完成连接后直接复用会话。
		if !pending.claimed {
			pending.claimed = true
			if driver.pendingOpen == pending {
				driver.pendingOpen = nil
			}
			if driver.Transport == nil {
				driver.Transport = opened.transport
			} else if opened.transport != nil {
				_ = opened.transport.Close()
			}
		}
	}
	defer driver.mu.Unlock()
	if driver.Transport == nil {
		return errors.New("酷冷至尊 HID 传输未配置")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if driver.ConnectDelay > 0 {
		if err := driver.pause(ctx, driver.ConnectDelay); err != nil {
			return err
		}
	}
	driver.connected = true
	driver.realtime = false
	driver.realtimeKnown = false
	driver.lastSuccessfulRealtimeIO = time.Time{}
	return nil
}

type openTransportResult struct {
	transport HIDTransport
	err       error
}

// runTransportOpen 将不可取消的第三方 hidapi 调用保持为
// 每台设备最多一个 pending 任务。完成通知使用关闭 channel
// 广播给所有等待者；Close 只取消等待，旧 opener 真正返回前
// 仍占住 pending 槽位，不允许累积第二个不可取消协程。
func (driver *Driver) runTransportOpen(pending *pendingTransportOpen, opener func() (HIDTransport, error)) {
	transport, err := opener()
	driver.mu.Lock()
	pending.result = openTransportResult{transport: transport, err: err}
	pending.finished = true
	close(pending.done)
	discarded := pending.discarded
	if discarded {
		pending.claimed = true
	}
	driver.mu.Unlock()
	if discarded && transport != nil {
		_ = transport.Close()
	}
	if discarded {
		// 迟到句柄完成 Close 前仍保留 pending，避免新 opener
		// 与旧句柄的收尾阶段重叠。
		driver.mu.Lock()
		if driver.pendingOpen == pending {
			driver.pendingOpen = nil
		}
		driver.mu.Unlock()
	}
}

func (driver *Driver) ApplyTheme(ctx context.Context, manifest theme.Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	fileName, data, err := driver.prepareOfflineResourceContext(ctx, manifest)
	if err != nil {
		return err
	}
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])

	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Transport == nil {
		return errors.New("酷冷至尊屏幕未连接")
	}
	state, err := loadState(driver.StatePath)
	if err != nil {
		return err
	}
	previous, slotCached := state.Slots[driver.Slot]
	if err := driver.rememberLegacySlot5MigrationEvidenceLocked(&state, &previous); err != nil {
		return err
	}
	driver.rememberStoredOfflineCommit(previous)
	if slotCached && previous.Uncertain {
		driver.offlineCommitUncertain = true
	}
	if slotCached && state.TrustVersion >= offlineStateTrustVersion &&
		!previous.Uncertain && !driver.offlineCommitUncertain &&
		previous.ReplaceVersion >= offlineReplaceVersion &&
		previous.Digest == digest && previous.FileName == fileName {
		if !driver.realtimeKnown || driver.realtime {
			if err := driver.setOfflineModeForVerifiedThemeLocked(ctx, false); err != nil {
				driver.disconnectOnFatalTransportErrorLocked(err)
				return err
			}
		}
		driver.realtime = false
		driver.realtimeKnown = true
		if err := driver.migrateLegacySlot5IfNeededLocked(ctx, &state); err != nil {
			driver.disconnectOnFatalTransportErrorLocked(err)
			return err
		}
		driver.activeTheme = manifest
		return nil
	}
	// 快路径已在上面返回；只有本次确实需要再次写入
	// 离线资源时才等待固件冷却。等待在 uncertain 落盘之前，
	// 因此调用方在冷却期取消不会破坏上一份已验证缓存。
	if err := driver.waitForOfflineCommitCooldownLocked(ctx); err != nil {
		return fmt.Errorf("等待酷冷至尊离线资源冷却: %w", err)
	}
	// 任何离线写入开始前先持久标记目标槽位不确定。
	// 即使中途超时、崩溃或重启，快路也必须重传；
	// 只有明确 transported/200 后才把同一记录改回已验证。
	state.Slots[driver.Slot] = storedSlot{
		Digest: digest, FileName: fileName, Uncertain: true,
		CommittedAtUnixMilli: driver.latestOfflineCommitUnixMilli(previous),
		ReplaceVersion:       offlineReplaceVersion,
		LegacyAliasMigration: previous.LegacyAliasMigration,
	}
	state.TrustVersion = max(state.TrustVersion, offlineStateTrustVersion)
	if err := driver.persistState(state); err != nil {
		return fmt.Errorf("标记酷冷至尊离线资源状态不确定: %w", err)
	}
	if err := driver.uploadLocked(ctx, fileName, data); err != nil {
		driver.disconnectOnFatalTransportErrorLocked(err)
		return err
	}
	committedAt := driver.nowTime()
	driver.lastOfflineCommit = committedAt
	if err := ctx.Err(); err != nil {
		return err
	}
	// 静默屏障后的 transported ACK 已经证明资源提交，先把该事实
	// 持久化，再切换显示模式。如果模式命令超时或失败，
	// 下次只重试幂等的模式命令，不会重传已提交的资源。
	state.Slots[driver.Slot] = storedSlot{
		Digest: digest, FileName: fileName, Uncertain: false,
		CommittedAtUnixMilli: committedAt.UnixMilli(),
		ReplaceVersion:       offlineReplaceVersion,
		LegacyAliasMigration: previous.LegacyAliasMigration,
	}
	state.TrustVersion = max(state.TrustVersion, offlineStateTrustVersion)
	if err := driver.persistState(state); err != nil {
		return fmt.Errorf("保存酷冷至尊离线资源状态: %w", err)
	}
	if err := driver.setOfflineModeForVerifiedThemeLocked(ctx, true); err != nil {
		driver.disconnectOnFatalTransportErrorLocked(err)
		return err
	}
	if err := driver.migrateLegacySlot5IfNeededLocked(ctx, &state); err != nil {
		driver.disconnectOnFatalTransportErrorLocked(err)
		return err
	}
	driver.activeTheme = manifest
	return nil
}

func (driver *Driver) persistState(state storedState) error {
	if driver.stateSaver != nil {
		return driver.stateSaver(driver.StatePath, state)
	}
	return saveState(driver.StatePath, state)
}

// InvalidateResidentThemeCache 把外部控制器可能覆盖过的驻留槽位标记为不可信。
// 它不需要打开 HID：开机时即使 MasterCTRL 已先占用设备，也能在 CaseScreen
// 之后重新取得独占权时强制重传保存主题。重复调用不会反复改写同一状态文件。
func (driver *Driver) InvalidateResidentThemeCache() error {
	driver.mu.Lock()
	defer driver.mu.Unlock()

	// 当前进程即使遇到状态文件落盘失败，也不能再走旧摘要快路径。
	driver.offlineCommitUncertain = true
	state, err := loadState(driver.StatePath)
	if err != nil {
		return err
	}
	previous, exists := state.Slots[driver.Slot]
	if !exists {
		return nil
	}
	changed := !previous.Uncertain || state.TrustVersion < offlineStateTrustVersion
	if !changed {
		return nil
	}
	previous.Uncertain = true
	state.Slots[driver.Slot] = previous
	state.TrustVersion = max(state.TrustVersion, offlineStateTrustVersion)
	if err := driver.persistState(state); err != nil {
		return fmt.Errorf("标记酷冷至尊驻留主题缓存不可信: %w", err)
	}
	return nil
}

func (driver *Driver) rememberLegacySlot5MigrationEvidenceLocked(
	state *storedState,
	previous *storedSlot,
) error {
	if driver.Slot != 4 || previous.LegacyAliasMigration == legacySlot5MigrationComplete {
		return nil
	}
	switch previous.LegacyAliasMigration {
	case legacySlot5MigrationPending, legacySlot5MigrationApproved:
		return nil
	case "":
		// 旧版曾把用户槽位 4 直接写成 suspend_4，实际落到了物理槽位 5。
		// 只有状态文件本身保留这个归属证据时才自动建立迁移任务；不能仅凭
		// 设备槽位 5 有内容就删除，因为它也可能是用户在原厂软件中保存的素材。
		if !isLegacySlot5Alias(previous.FileName) {
			return nil
		}
	default:
		return fmt.Errorf("酷冷至尊旧槽位迁移标记无效: %q", previous.LegacyAliasMigration)
	}
	previous.LegacyAliasMigration = legacySlot5MigrationPending
	state.Slots[driver.Slot] = *previous
	if err := driver.persistState(*state); err != nil {
		return fmt.Errorf("记录酷冷至尊旧槽位迁移待处理状态: %w", err)
	}
	return nil
}

func isLegacySlot5Alias(fileName string) bool {
	base := filepath.Base(strings.TrimSpace(fileName))
	return strings.EqualFold(base, "suspend_4.jpg") ||
		strings.EqualFold(base, "suspend_4.mp4")
}

func isVerifiedSlot4Target(slot storedSlot) bool {
	if slot.Uncertain || strings.TrimSpace(slot.Digest) == "" {
		return false
	}
	base := filepath.Base(strings.TrimSpace(slot.FileName))
	return strings.EqualFold(base, "suspend_3.jpg") ||
		strings.EqualFold(base, "suspend_3.mp4")
}

func (driver *Driver) waitForOfflineCommitCooldownLocked(ctx context.Context) error {
	if driver.lastOfflineCommit.IsZero() {
		return nil
	}
	elapsed := driver.nowTime().Sub(driver.lastOfflineCommit)
	if elapsed < 0 {
		// 系统时钟回拨或状态文件中的未来时间最多等一个
		// 完整冷却窗，不能生成无界等待。
		elapsed = 0
	}
	remaining := offlineCommitCooldown - elapsed
	if remaining <= 0 {
		return nil
	}
	return driver.pause(ctx, remaining)
}

func (driver *Driver) rememberStoredOfflineCommit(slot storedSlot) {
	if slot.CommittedAtUnixMilli <= 0 {
		return
	}
	committedAt := time.UnixMilli(slot.CommittedAtUnixMilli)
	if committedAt.After(driver.lastOfflineCommit) {
		driver.lastOfflineCommit = committedAt
	}
}

func (driver *Driver) latestOfflineCommitUnixMilli(previous storedSlot) int64 {
	latest := previous.CommittedAtUnixMilli
	if !driver.lastOfflineCommit.IsZero() && driver.lastOfflineCommit.UnixMilli() > latest {
		latest = driver.lastOfflineCommit.UnixMilli()
	}
	return latest
}

// UseHostFrames 只把离线协议能完整表达的图片、静态文字、形状
// 或单一 MP4 主题留在屏内。时钟、仪表等运行期会变化的图层
// 必须进入主机合成，否则离线资源只会留下应用瞬间的旧值。
func (driver *Driver) UseHostFrames(manifest theme.Manifest) bool {
	visibleVideos := 0
	visibleStaticOverlays := 0
	for _, layer := range manifest.Layers {
		if !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		switch layer.Type {
		case theme.LayerCanvas:
			continue
		case theme.LayerImage, theme.LayerBackgroundImage, theme.LayerText, theme.LayerShape:
			// 图片、静态文字和形状可在上传前一次性合成进离线
			// JPEG；但 MP4 是原文件直传，不能再叠加任何前景。
			if visibleVideos > 0 {
				return true
			}
			visibleStaticOverlays++
		case theme.LayerBackgroundVideo:
			resourceID, _ := layer.Options["resource_id"].(string)
			resource, ok := findResource(manifest.Resources, resourceID, ".mp4")
			if !ok || !strings.EqualFold(filepath.Ext(resource.LocalPath), ".mp4") ||
				!offlineVideoGeometry(layer) {
				// 酷冷离线模式只接受原样全屏 MP4；无法编码进文件名的
				// 透明度、位置、旋转、contain 或单次播放设置改用主机帧。
				return true
			}
			visibleVideos++
			if visibleVideos > 1 {
				return true
			}
			// prepareOfflineResource 会原样直传 MP4，已经出现的
			// 可见静态前景不能随视频一起保留。
			if visibleStaticOverlays > 0 {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// ApplyHostTheme 只登记实时主题；首个 JPEG 到达时再切换 realtimeDisplay。
func (driver *Driver) ApplyHostTheme(_ context.Context, manifest theme.Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Transport == nil {
		return errors.New("酷冷至尊屏幕未连接")
	}
	driver.activeTheme = manifest
	return nil
}

func (driver *Driver) SendTelemetry(context.Context, telemetry.Snapshot) error {
	return nil
}

func (driver *Driver) SendJPEG(ctx context.Context, data []byte) error {
	configuration, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("酷冷至尊实时 JPEG 无效: %w", err)
	}
	if configuration.Width != driver.Resolution[0] || configuration.Height != driver.Resolution[1] {
		return fmt.Errorf(
			"酷冷至尊实时 JPEG 分辨率 %dx%d 与设备 %dx%d 不同",
			configuration.Width, configuration.Height, driver.Resolution[0], driver.Resolution[1],
		)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Transport == nil {
		return errors.New("酷冷至尊屏幕未连接")
	}
	if !driver.realtimeKnown || !driver.realtime {
		if err := driver.setDisplayModeLocked(ctx, true, protocol.SetDisplayModeSeq, false); err != nil {
			driver.disconnectOnFatalTransportErrorLocked(err)
			return err
		}
		driver.realtime = true
		driver.realtimeKnown = true
	}
	transferID, err := randomTransferID()
	if err != nil {
		return err
	}
	reports, err := protocol.BuildDataReports(data, transferID, protocol.MediaDataJPEG)
	if err != nil {
		return err
	}
	for index, report := range reports {
		if err := driver.writeReportLocked(ctx, report); err != nil {
			driver.disconnectOnFatalTransportErrorLocked(err)
			return fmt.Errorf("发送酷冷至尊实时帧块 %d/%d: %w", index+1, len(reports), err)
		}
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return err
	}
	// 固件会在完整实时 JPEG 后回一个状态 ACK。主机帧队列容量 1
	// 只能限制出站积压；若一直不读，HID 入站队列仍会逐帧增长，
	// 最终让画面冻结并触发重连闪烁。ACK 可能稍晚到达，因此这里只
	// 有界清理已经可读的响应；下一帧会继续回收迟到 ACK。
	if err := driver.consumeRealtimeResponsesLocked(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		driver.disconnectOnFatalTransportErrorLocked(err)
		return fmt.Errorf("读取酷冷至尊实时帧 ACK: %w", err)
	}
	driver.lastSuccessfulRealtimeIO = time.Now()
	return nil
}

// SetBrightness 使用原厂 ATMOS V2 的 0..100 亮度命令。
// 0 是协议允许值；是否等价于面板物理断电由固件决定。
func (driver *Driver) SetBrightness(ctx context.Context, value int) error {
	payload, err := protocol.BrightnessPayload(value)
	if err != nil {
		return err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Transport == nil {
		return errors.New("酷冷至尊屏幕未连接")
	}
	if err := driver.sendBrightnessLocked(ctx, payload); err != nil {
		driver.disconnectOnFatalTransportErrorLocked(err)
		return fmt.Errorf("设置酷冷至尊亮度: %w", err)
	}
	return nil
}

func (driver *Driver) DefaultBrightness() int {
	return protocol.DefaultBrightness
}

func (driver *Driver) SetRealtimeEnabled(ctx context.Context, enabled bool) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.connected || driver.Transport == nil {
		return errors.New("酷冷至尊屏幕未连接")
	}
	if driver.realtimeKnown && driver.realtime == enabled {
		return nil
	}
	if err := driver.setDisplayModeLocked(ctx, enabled, protocol.SetDisplayModeSeq, false); err != nil {
		driver.disconnectOnFatalTransportErrorLocked(err)
		return err
	}
	driver.realtime = enabled
	driver.realtimeKnown = true
	return nil
}

func (driver *Driver) Close() error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.connected = false
	driver.realtime = false
	driver.realtimeKnown = false
	if driver.pendingOpen != nil {
		pending := driver.pendingOpen
		if !pending.discarded {
			pending.discarded = true
			close(pending.canceled)
		}
		if pending.finished {
			driver.pendingOpen = nil
			if !pending.claimed && pending.result.transport != nil {
				pending.claimed = true
				_ = pending.result.transport.Close()
			}
		}
	}
	if driver.Transport == nil {
		return nil
	}
	err := driver.Transport.Close()
	driver.Transport = nil
	return err
}

func (driver *Driver) disconnectLocked() {
	driver.connected = false
	driver.realtime = false
	driver.realtimeKnown = false
	driver.lastSuccessfulRealtimeIO = time.Time{}
	if driver.Transport != nil {
		_ = driver.Transport.Close()
		driver.Transport = nil
	}
}

func (driver *Driver) disconnectOnFatalTransportErrorLocked(err error) {
	if fatalTransportFailure(err) {
		driver.disconnectLocked()
	}
}

func (driver *Driver) uploadLocked(ctx context.Context, fileName string, data []byte) error {
	if driver.offlineCommitUncertain {
		if err := driver.waitForOfflineResponseQuietLocked(ctx); err != nil {
			return fmt.Errorf("清理酷冷至尊上次离线传输响应: %w", err)
		}
		driver.offlineCommitUncertain = false
	}
	var lastCommitError error
	var attemptDiagnostics []string
	for attempt := 1; attempt <= offlineUploadAttempts; attempt++ {
		diagnostic := offlineAttemptDiagnostic{}
		committed, err := driver.uploadAttemptLocked(ctx, fileName, data, &diagnostic)
		attemptDiagnostics = append(attemptDiagnostics, fmt.Sprintf(
			"第%d轮[transportObserved=%t,dataObserved=%t,transported200=%d,"+
				"transportedSkipped200=%d,lastSkippedAck=%s,debt=%d]",
			attempt, diagnostic.transportResponseObserved, diagnostic.dataResponseObserved,
			diagnostic.transportedSuccessResponses, diagnostic.transportedSkippedSuccessResponses,
			diagnostic.lastSkippedAck, diagnostic.outstandingResponseDebt,
		))
		if err == nil && committed {
			driver.offlineCommitUncertain = false
			return nil
		}
		if err == nil {
			driver.offlineCommitUncertain = true
			return errors.New("酷冷至尊离线资源传输未返回提交结果")
		}
		if !commandResponseFailed(err) {
			driver.offlineCommitUncertain = true
			return fmt.Errorf("%s: %w", attemptDiagnostics[len(attemptDiagnostics)-1], err)
		}
		lastCommitError = err
		if attempt < offlineUploadAttempts {
			// 各轮和各阶段都复用原厂序号 0x2A；失败后必须等到完整
			// 1s 静默窗口，避免前一轮的迟到 ACK 污染下一轮。
			if barrierErr := driver.waitForOfflineResponseQuietLocked(ctx); barrierErr != nil {
				driver.offlineCommitUncertain = true
				return fmt.Errorf("%s；重试前清理迟到响应: %w", strings.Join(attemptDiagnostics, "；"), barrierErr)
			}
			backoff := offlineUploadRetryDelay(attempt)
			if err := driver.pause(ctx, backoff); err != nil {
				driver.offlineCommitUncertain = true
				return fmt.Errorf("%s；等待酷冷至尊离线传输重试退避 %s: %w",
					strings.Join(attemptDiagnostics, "；"), backoff, err)
			}
		}
	}
	driver.offlineCommitUncertain = true
	return fmt.Errorf(
		"完成酷冷至尊离线资源传输：%d 次完整尝试均未收到 transported/200；%s: %w",
		offlineUploadAttempts, strings.Join(attemptDiagnostics, "；"), lastCommitError,
	)
}

func offlineUploadRetryDelay(failedAttempt int) time.Duration {
	if failedAttempt <= 1 {
		return offlineRetryInitialDelay
	}
	delay := offlineRetryInitialDelay
	for attempt := 1; attempt < failedAttempt; attempt++ {
		if delay >= offlineRetryMaximumDelay/2 {
			return offlineRetryMaximumDelay
		}
		delay *= 2
	}
	return min(delay, offlineRetryMaximumDelay)
}

func (driver *Driver) setOfflineModeForVerifiedThemeLocked(ctx context.Context, settleAfterCommit bool) error {
	// transported 只证明新文件已经提交，不证明播放器已经重新载入
	// 槽位。先复刻原厂 FlashUpdate：等待 200ms 后直接发送 false；
	// 只有固件对重复 false 不回 ACK 时，才制造一次 true -> false
	// 的兼容边沿。新提交后还要按原厂顺序发送 param 探测，避免只凭
	// 控制命令 ACK 把尚未恢复通信的播放器误报为“已应用”。
	previousRealtime := driver.realtime
	previousKnown := driver.realtimeKnown
	if settleAfterCommit {
		if err := driver.pause(ctx, 200*time.Millisecond); err != nil {
			return fmt.Errorf("酷冷至尊资源已提交，但离线显示模式未确认: %w", err)
		}
		if err := driver.waitForOfflineResponseQuietLocked(ctx); err != nil {
			return fmt.Errorf("酷冷至尊资源已提交，但清理模式切换前响应失败: %w", err)
		}
	}
	_, err := driver.sendCommandLockedTimeout(
		ctx, "realtimeDisplay", protocol.OfflineDisplayModeSeq,
		map[string]bool{"enable": false}, offlineResponseTimeout,
	)
	if err == nil {
		driver.realtime = false
		driver.realtimeKnown = true
		if settleAfterCommit {
			return driver.probeOfflineRefreshLocked(ctx)
		}
		return nil
	}
	if !commandResponseTimedOut(err) || (!settleAfterCommit && previousKnown) {
		driver.realtime = previousRealtime
		driver.realtimeKnown = previousKnown
		return fmt.Errorf("酷冷至尊资源已提交，但离线显示模式未确认: %w", err)
	}
	// 重启后模式未知且重复 false 纯超时，可能只是设备已经在离线模式。
	// 先清掉迟到回包，再强制制造可验证的模式边沿。
	if quietErr := driver.waitForOfflineResponseQuietLocked(ctx); quietErr != nil {
		driver.realtime = previousRealtime
		driver.realtimeKnown = previousKnown
		return fmt.Errorf("酷冷至尊资源已提交，但清理模式恢复响应失败: %w", quietErr)
	}
	if err := driver.forceOfflineReloadLocked(ctx, previousRealtime, previousKnown); err != nil {
		return err
	}
	if settleAfterCommit {
		return driver.probeOfflineRefreshLocked(ctx)
	}
	return nil
}

func (driver *Driver) probeOfflineRefreshLocked(ctx context.Context) error {
	// 原厂在最终 realtimeDisplay 后固定等待 300ms，再发送一次
	// POST param 1，并在 60ms 后尝试读取状态。该探测不承担文件
	// 提交判定（transported/200 已完成该职责）；原厂也会忽略
	// param 无回包或非 200，因此这里只保留刷新副作用，并且只把
	// 调用方取消或确定的 HID 故障向上返回。
	if err := driver.pause(ctx, 300*time.Millisecond); err != nil {
		return fmt.Errorf("酷冷至尊离线模式已切换，但等待刷新探测失败: %w", err)
	}
	report, err := protocol.BuildParamRequest()
	if err != nil {
		return err
	}
	if err := driver.writeReportLocked(ctx, report); err != nil {
		return fmt.Errorf("酷冷至尊离线模式已切换，但发送刷新探测失败: %w", err)
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return fmt.Errorf("酷冷至尊离线模式已切换，但等待刷新响应失败: %w", err)
	}
	response, err := driver.readResponseLockedTimeout(ctx, offlineResponseTimeout)
	if err != nil {
		if ctx.Err() != nil || fatalTransportFailure(err) || errors.Is(err, context.Canceled) {
			return fmt.Errorf("酷冷至尊离线模式已切换，但刷新探测失败: %w", err)
		}
		return nil
	}
	// 原厂不以 param 状态否定 transported 提交；但若设备明确返回
	// suspendMediaActive，就必须确认目标槽位已经进入活动集合，避免
	// 工作台在播放器仍指向旧内容时显示“OK”。
	if protocol.ValidateStatus(response) == nil {
		if active, found := suspendSlotActive(response.JSON, driver.Slot); found && !active {
			return fmt.Errorf("酷冷至尊资源已提交，但设备报告槽位 %d 尚未激活", driver.Slot)
		}
	}
	return nil
}

func suspendSlotActive(status map[string]any, slot int) (bool, bool) {
	if slot < 1 {
		return false, false
	}
	raw, ok := status["suspendMediaActive"]
	if !ok {
		return false, false
	}
	values, ok := raw.([]any)
	if !ok || slot > len(values) {
		return false, false
	}
	switch value := values[slot-1].(type) {
	case float64:
		return value != 0, true
	case int:
		return value != 0, true
	case bool:
		return value, true
	case string:
		normalized := strings.TrimSpace(strings.ToLower(value))
		if normalized == "1" || normalized == "true" {
			return true, true
		}
		if normalized == "0" || normalized == "false" {
			return false, true
		}
	}
	return false, false
}

func (driver *Driver) migrateLegacySlot5IfNeededLocked(ctx context.Context, state *storedState) error {
	if driver.Slot != 4 {
		return nil
	}
	slot, exists := state.Slots[4]
	if !exists || slot.LegacyAliasMigration == "" ||
		slot.LegacyAliasMigration == legacySlot5MigrationPending ||
		slot.LegacyAliasMigration == legacySlot5MigrationComplete {
		// pending 只记录旧版曾经写错槽位这一事实，不包含当前删除授权。
		// 用户后来可能已用原厂软件覆盖槽位 5，普通 ApplyTheme 不得
		// 根据历史归属自动删除；只有独立强确认入口能写入 approved。
		return nil
	}
	if slot.LegacyAliasMigration != legacySlot5MigrationApproved {
		return fmt.Errorf("酷冷至尊旧槽位迁移标记无效: %q", slot.LegacyAliasMigration)
	}
	if !isVerifiedSlot4Target(slot) {
		return errors.New("酷冷至尊旧槽位迁移已批准，但槽位 4 的新资源尚未完成验证")
	}

	beforeSlot4, beforeSlot5, err := driver.queryLegacySlot5ActiveSetLocked(ctx)
	if err != nil {
		return fmt.Errorf("确认酷冷至尊旧槽位迁移前状态: %w", err)
	}
	if !beforeSlot4 {
		return errors.New("酷冷至尊旧槽位迁移已批准，但设备未报告物理槽位 4 已激活；未删除槽位 5")
	}
	if beforeSlot5 {
		if err := driver.deleteLegacySlot5MediaLocked(ctx); err != nil {
			return err
		}
	}

	// 无论本轮是否实际发送删除，都要重新触发一次原厂离线播放器加载。
	// 这样进程若在删除后、完成标记前退出，下一次也能安全续跑并验证。
	_, modeErr := driver.sendCommandLockedTimeout(
		ctx, "realtimeDisplay", protocol.OfflineDisplayModeSeq,
		map[string]bool{"enable": false}, offlineResponseTimeout,
	)
	if legacyMigrationCommandTerminal(ctx, modeErr) {
		return fmt.Errorf("酷冷至尊旧槽位已处理，但无法触发离线播放器重载: %w", modeErr)
	}
	if modeErr == nil {
		driver.realtime = false
		driver.realtimeKnown = true
	}
	if err := driver.pause(ctx, 300*time.Millisecond); err != nil {
		return fmt.Errorf("等待酷冷至尊旧槽位迁移生效: %w", err)
	}
	afterSlot4, afterSlot5, err := driver.queryLegacySlot5ActiveSetLocked(ctx)
	if err != nil {
		return fmt.Errorf("验证酷冷至尊旧槽位迁移结果: %w", err)
	}
	if !afterSlot4 || afterSlot5 {
		return fmt.Errorf(
			"酷冷至尊旧槽位迁移未达到安全状态: 槽位4=%t 槽位5=%t（需要槽位4=true、槽位5=false）",
			afterSlot4, afterSlot5,
		)
	}

	slot.LegacyAliasMigration = legacySlot5MigrationComplete
	state.Slots[4] = slot
	if err := driver.persistState(*state); err != nil {
		slot.LegacyAliasMigration = legacySlot5MigrationApproved
		state.Slots[4] = slot
		return fmt.Errorf("保存酷冷至尊旧槽位迁移完成状态: %w", err)
	}
	return nil
}

func (driver *Driver) deleteLegacySlot5MediaLocked(ctx context.Context) error {
	deleteFile := func(fileName string) error {
		_, err := driver.sendCommandLockedTimeout(
			ctx, "delMedia", protocol.OfflineCommandSeq,
			protocol.DeleteMediaPayload(protocol.MediaSuspend, fileName),
			offlineResponseTimeout,
		)
		return err
	}

	// 原厂 FlashUpdate 会先发送一次 MP4 删除并忽略其结果，等待 60ms 后
	// 再做最多五次确认重试；即总共最多六次。槽位原本是 JPG 时，MP4
	// 不存在产生的 NACK/超时不能阻止随后删除真正残留的 JPG。
	firstErr := deleteFile("suspend_4.mp4")
	if legacyMigrationCommandTerminal(ctx, firstErr) {
		return fmt.Errorf("删除酷冷至尊旧槽位 MP4: %w", firstErr)
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return err
	}
	for attempt := 1; attempt < legacySlot5DeleteAttempts; attempt++ {
		err := deleteFile("suspend_4.mp4")
		if err == nil {
			break
		}
		if legacyMigrationCommandTerminal(ctx, err) {
			return fmt.Errorf("重试删除酷冷至尊旧槽位 MP4: %w", err)
		}
		if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
			return err
		}
	}

	jpgErr := deleteFile("suspend_4.jpg")
	if legacyMigrationCommandTerminal(ctx, jpgErr) {
		return fmt.Errorf("删除酷冷至尊旧槽位 JPG: %w", jpgErr)
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return err
	}
	return nil
}

func legacyMigrationCommandTerminal(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return ctx.Err() != nil || fatalTransportFailure(err) || !commandResponseFailed(err)
}

func (driver *Driver) queryLegacySlot5ActiveSetLocked(ctx context.Context) (bool, bool, error) {
	report, err := protocol.BuildParamRequest()
	if err != nil {
		return false, false, err
	}
	driver.drainResponsesLocked(ctx)
	if err := driver.writeReportLocked(ctx, report); err != nil {
		return false, false, err
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return false, false, err
	}
	deadline := driver.nowTime().Add(offlineResponseTimeout)
	for read := 1; read <= 5; read++ {
		remaining := deadline.Sub(driver.nowTime())
		if remaining <= 0 {
			return false, false, context.DeadlineExceeded
		}
		response, readErr := driver.readResponseLockedTimeout(ctx, remaining)
		if readErr != nil {
			return false, false, readErr
		}
		if statusErr := protocol.ValidateStatus(response); statusErr != nil {
			return false, false, statusErr
		}
		slot4, found4 := suspendSlotActive(response.JSON, 4)
		slot5, found5 := suspendSlotActive(response.JSON, 5)
		if found4 && found5 {
			return slot4, slot5, nil
		}
	}
	return false, false, errors.New("设备响应中没有完整的 suspendMediaActive 五槽状态")
}

func (driver *Driver) forceOfflineReloadLocked(ctx context.Context, previousRealtime, previousKnown bool) error {
	_, trueErr := driver.sendCommandLockedTimeout(
		ctx, "realtimeDisplay", protocol.OfflineDisplayModeSeq,
		map[string]bool{"enable": true}, offlineResponseTimeout,
	)
	trueConfirmed := trueErr == nil
	if trueErr != nil && !commandResponseTimedOut(trueErr) {
		driver.realtime = previousRealtime
		driver.realtimeKnown = previousKnown
		return fmt.Errorf("酷冷至尊资源已提交，但无法触发槽位重载: %w", trueErr)
	}
	if trueConfirmed {
		driver.realtime = true
		driver.realtimeKnown = true
	} else {
		driver.realtimeKnown = false
	}
	if err := driver.pause(ctx, 200*time.Millisecond); err != nil {
		return fmt.Errorf("酷冷至尊资源已提交，但等待槽位重载失败: %w", err)
	}
	if err := driver.waitForOfflineResponseQuietLocked(ctx); err != nil {
		return fmt.Errorf("酷冷至尊资源已提交，但清理重载响应失败: %w", err)
	}
	_, falseErr := driver.sendCommandLockedTimeout(
		ctx, "realtimeDisplay", protocol.OfflineDisplayModeSeq,
		map[string]bool{"enable": false}, offlineResponseTimeout,
	)
	if falseErr != nil {
		if !trueConfirmed {
			driver.realtime = previousRealtime
			driver.realtimeKnown = previousKnown
		}
		return fmt.Errorf("酷冷至尊资源已提交，但重载后离线模式未确认: %w", falseErr)
	}
	driver.realtime = false
	driver.realtimeKnown = true
	return nil
}

type offlineAttemptDiagnostic struct {
	transportResponseObserved          bool
	dataResponseObserved               bool
	transportedSuccessResponses        int
	transportedSkippedSuccessResponses int
	lastSkippedAck                     string
	outstandingResponseDebt            int
}

func (driver *Driver) uploadAttemptLocked(
	ctx context.Context,
	fileName string,
	data []byte,
	diagnostic *offlineAttemptDiagnostic,
) (bool, error) {
	diagnostic.lastSkippedAck = "-"
	mediaDataKind, err := offlineMediaDataKind(fileName)
	if err != nil {
		return false, err
	}
	payload := protocol.TransportPayload(protocol.MediaSuspend, fileName, len(data))
	transportObserved, transportErr := driver.sendOfflineTransportLocked(ctx, payload)
	diagnostic.transportResponseObserved = transportObserved
	if transportErr != nil {
		// 原厂不以 transport 回包作为最终提交依据；
		// 读响应超时、NACK 或损坏响应仍继续发数据。
		// 命令未写出、HID 故障或调用方取消则立即停止。
		if !commandResponseFailed(transportErr) {
			return false, fmt.Errorf("开始酷冷至尊离线资源传输: %w", transportErr)
		}
	}
	if err := driver.pause(ctx, 300*time.Millisecond); err != nil {
		return false, err
	}
	transferID, err := randomTransferID()
	if err != nil {
		return false, err
	}
	reports, err := protocol.BuildDataReports(data, transferID, mediaDataKind)
	if err != nil {
		return false, err
	}
	for index, report := range reports {
		if err := driver.writeReportLocked(ctx, report); err != nil {
			return false, fmt.Errorf("发送酷冷至尊资源块 %d/%d: %w", index+1, len(reports), err)
		}
	}
	if err := driver.pause(ctx, 500*time.Millisecond); err != nil {
		return false, err
	}
	// 原厂的 transport、data 和 transported 都复用 seq 0x2A。
	// 因此发送 transported 前必须消费所有中间响应，并再等待
	// 完整 1s 静默窗口，尽量避免迟到的数据 ACK 污染提交确认。
	dataResponses, err := driver.drainOfflineResponsesUntilQuietLocked(ctx)
	diagnostic.dataResponseObserved = dataResponses > 0
	if err != nil {
		return false, fmt.Errorf("清理酷冷至尊资源数据 ACK: %w", err)
	}
	diagnostic.outstandingResponseDebt = 0
	// 原厂 JPEG 分支在数据写完后必须至少成功读取并解码一个回包，
	// 否则整轮重传且不发送 transported；它不检查该回包的状态或
	// ACK。MP4 分支即使没有数据回包也会继续提交，不能共用此门槛。
	if mediaDataKind == protocol.MediaDataJPEG && dataResponses == 0 {
		return false, &commandResponseError{
			cause: errors.New("JPEG 数据阶段未收到可解码回包"),
		}
	}
	transportedErr := driver.sendTransportedLocked(ctx, fileName, diagnostic)
	if transportedErr != nil && !commandResponseFailed(transportedErr) {
		return false, fmt.Errorf("完成酷冷至尊离线资源传输: %w", transportedErr)
	}
	if transportedErr != nil {
		if err := driver.pause(ctx, 200*time.Millisecond); err != nil {
			return false, err
		}
		return false, fmt.Errorf("完成酷冷至尊离线资源传输: %w", transportedErr)
	}
	return true, nil
}

func offlineMediaDataKind(fileName string) (byte, error) {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".jpg", ".jpeg":
		return protocol.MediaDataJPEG, nil
	case ".mp4":
		return protocol.MediaDataMP4, nil
	default:
		return 0, fmt.Errorf("酷冷至尊离线资源格式不受支持: %s", filepath.Ext(fileName))
	}
}

func (driver *Driver) setDisplayModeLocked(ctx context.Context, enabled bool, sequence int, confirm bool) error {
	_, commandErr := driver.sendCommandLocked(ctx, "realtimeDisplay", sequence, map[string]bool{"enable": enabled})
	if commandErr == nil {
		return nil
	}
	// param 只证明 HID 会话仍可通信，不能证明显示模式已经切换。
	// 只有“驱动此前已确认处于目标模式 + 本次重复命令纯 ACK 超时”
	// 才允许用 param 兼容部分固件不回复重复命令的行为。明确 NACK、
	// 损坏回包、传输错误或调用方取消都必须原样失败。
	if !confirm || !driver.realtimeKnown || driver.realtime != enabled ||
		ctx.Err() != nil || !commandResponseTimedOut(commandErr) {
		return fmt.Errorf("设置酷冷至尊显示模式: %w", commandErr)
	}
	// 固件在目标模式本来就生效时偶尔不回重复命令 ACK；此时用
	// param 确认会话仍可写，并沿用此前已确认的显示模式状态。
	if err := driver.pause(ctx, 300*time.Millisecond); err != nil {
		return err
	}
	report, err := protocol.BuildParamRequest()
	if err != nil {
		return err
	}
	if err := driver.writeReportLocked(ctx, report); err != nil {
		return fmt.Errorf("确认酷冷至尊显示模式: %w", err)
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return err
	}
	response, err := driver.readResponseLocked(ctx)
	if err != nil {
		return fmt.Errorf("设置酷冷至尊显示模式: %v；确认状态: %w", commandErr, err)
	}
	if err := protocol.ValidateStatus(response); err != nil {
		return fmt.Errorf("确认酷冷至尊显示模式: %w", err)
	}
	return nil
}

func (driver *Driver) sendCommandLocked(ctx context.Context, action string, sequence int, body any) (protocol.Response, error) {
	return driver.sendCommandLockedTimeout(ctx, action, sequence, body, 5*time.Second)
}

func (driver *Driver) sendBrightnessLocked(ctx context.Context, payload map[string]int) error {
	report, err := protocol.BuildRequest(
		"brightness", protocol.BrightnessCommandSeq, time.Now(), payload,
	)
	if err != nil {
		return err
	}
	driver.drainResponsesLocked(ctx)
	if err := driver.writeReportLocked(ctx, report); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 原厂 SSRPostCommand 对 brightness 只写一次，之后最多
	// 读两次、每次 1s；第一次超时、坏包、ACK 错或
	// NACK 都只消费一次额外响应，不会重复写命令。
	var lastResponseError error
	for attempt := 0; attempt < brightnessResponseReads; attempt++ {
		response, readErr := driver.readResponseLockedTimeout(ctx, offlineResponseTimeout)
		if readErr == nil {
			if validationErr := protocol.ValidateAck(response, protocol.BrightnessCommandSeq); validationErr == nil {
				return nil
			} else {
				lastResponseError = &commandResponseError{cause: validationErr}
			}
		} else {
			classified := classifyCommandResponseReadError(ctx, readErr)
			if ctx.Err() != nil || fatalTransportFailure(classified) || errors.Is(classified, context.Canceled) {
				return classified
			}
			lastResponseError = classified
		}
	}
	return lastResponseError
}

func (driver *Driver) sendOfflineTransportLocked(ctx context.Context, payload map[string]any) (bool, error) {
	report, err := protocol.BuildRequest("transport", protocol.OfflineCommandSeq, time.Now(), payload)
	if err != nil {
		return false, err
	}
	driver.drainResponsesLocked(ctx)
	if err := driver.writeReportLocked(ctx, report); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return false, err
	}
	response, observed, _, err := driver.readOfflineStageResponseLocked(ctx, 0)
	if err != nil {
		return observed, err
	}
	if err := protocol.ValidateAck(response, protocol.OfflineCommandSeq); err != nil {
		return true, &commandResponseError{cause: err}
	}
	return true, nil
}

func (driver *Driver) sendTransportedLocked(
	ctx context.Context,
	fileName string,
	diagnostic *offlineAttemptDiagnostic,
) error {
	report, err := protocol.BuildRequest(
		"transported", protocol.TransportedCommandSeq, time.Now(), protocol.TransportedPayload(fileName),
	)
	if err != nil {
		return err
	}
	// 原厂 SSRPostCommand 对该命令固定使用 0x2A，并且最终提交
	// 只检查 response=200，不要求 AckNumber 匹配。因为前序阶段
	// 也使用同一序号，真正的隔离依靠上面的完整静默屏障；不能
	// 为了便于区分而改成真机未采用的自定义序号或额外 ACK 条件。
	if err := driver.writeReportLocked(ctx, report); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return err
	}
	deadline := driver.nowTime().Add(offlineResponseTimeout)
	for {
		remaining := deadline.Sub(driver.nowTime())
		if remaining <= 0 {
			diagnostic.outstandingResponseDebt = 1
			return &commandResponseTimeoutError{cause: context.DeadlineExceeded}
		}
		response, readErr := driver.readResponseLockedTimeout(ctx, remaining)
		if readErr != nil {
			classified := classifyCommandResponseReadError(ctx, readErr)
			if commandResponseTimedOut(classified) {
				diagnostic.outstandingResponseDebt = 1
			}
			return classified
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if statusErr := protocol.ValidateStatus(response); statusErr == nil {
			break
		} else {
			return &commandResponseError{cause: statusErr}
		}
	}
	diagnostic.transportedSuccessResponses++
	return nil
}

func (driver *Driver) readOfflineStageResponseLocked(
	ctx context.Context,
	priorResponseDebt int,
) (protocol.Response, bool, int, error) {
	deadline := driver.nowTime().Add(offlineResponseTimeout)
	for {
		remaining := deadline.Sub(driver.nowTime())
		if remaining <= 0 {
			return protocol.Response{}, false, priorResponseDebt,
				&commandResponseTimeoutError{cause: context.DeadlineExceeded}
		}
		response, readErr := driver.readResponseLockedTimeout(ctx, remaining)
		if readErr != nil {
			classified := classifyCommandResponseReadError(ctx, readErr)
			consumedFrame := ctx.Err() == nil && !errors.Is(readErr, context.DeadlineExceeded) &&
				!errors.Is(readErr, context.Canceled) && !fatalTransportFailure(readErr)
			if consumedFrame && priorResponseDebt > 0 {
				priorResponseDebt--
				continue
			}
			return protocol.Response{}, consumedFrame, priorResponseDebt, classified
		}
		if err := ctx.Err(); err != nil {
			return protocol.Response{}, false, priorResponseDebt, err
		}
		if priorResponseDebt > 0 {
			priorResponseDebt--
			continue
		}
		return response, true, 0, nil
	}
}

func (driver *Driver) sendCommandLockedTimeout(
	ctx context.Context,
	action string,
	sequence int,
	body any,
	responseTimeout time.Duration,
) (protocol.Response, error) {
	report, err := protocol.BuildRequest(action, sequence, time.Now(), body)
	if err != nil {
		return protocol.Response{}, err
	}
	// 离线资源的数据 ACK 可能在下一条命令前才进入 HID 队列，先清掉旧响应。
	driver.drainResponsesLocked(ctx)
	if err := driver.writeReportLocked(ctx, report); err != nil {
		return protocol.Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	if err := driver.pause(ctx, 60*time.Millisecond); err != nil {
		return protocol.Response{}, err
	}
	deadline := driver.nowTime().Add(responseTimeout)
	var lastValidation error
	for attempt := 0; attempt < 64; attempt++ {
		remaining := deadline.Sub(driver.nowTime())
		if remaining <= 0 {
			return protocol.Response{}, &commandResponseTimeoutError{cause: context.DeadlineExceeded}
		}
		response, err := driver.readResponseLockedTimeout(ctx, remaining)
		if err != nil {
			return protocol.Response{}, classifyCommandResponseReadError(ctx, err)
		}
		if err := ctx.Err(); err != nil {
			return protocol.Response{}, err
		}
		if err := protocol.ValidateAck(response, sequence); err == nil {
			return response, nil
		} else if protocol.ValidateStatus(response) == nil {
			// 实时 JPEG 的数据 ACK 可能晚于下一条命令到达；状态正常时跳过旧序号。
			lastValidation = err
			continue
		} else {
			return protocol.Response{}, &commandResponseError{cause: err}
		}
	}
	return protocol.Response{}, &commandResponseError{
		cause: fmt.Errorf("酷冷命令连续收到过期 ACK: %w", lastValidation),
	}
}

func (driver *Driver) nowTime() time.Time {
	if driver.now != nil {
		return driver.now()
	}
	return time.Now()
}

func classifyCommandResponseReadError(ctx context.Context, err error) error {
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return &commandResponseTimeoutError{cause: err}
	}
	if ctx.Err() == nil && !fatalTransportFailure(err) && !errors.Is(err, context.Canceled) {
		return &commandResponseError{cause: err}
	}
	return err
}

func (driver *Driver) waitForOfflineResponseQuietLocked(ctx context.Context) error {
	_, err := driver.drainOfflineResponsesUntilQuietLocked(ctx)
	return err
}

func (driver *Driver) drainOfflineResponsesUntilQuietLocked(ctx context.Context) (int, error) {
	deadline := driver.nowTime().Add(offlineStaleBarrierLimit)
	decoded := 0
	for {
		remaining := deadline.Sub(driver.nowTime())
		if remaining <= 0 {
			return decoded, errors.New("酷冷离线响应队列在有界窗口内未静默")
		}
		quietWindow := min(offlineResponseTimeout, remaining)
		_, err := driver.readResponseLockedTimeout(ctx, quietWindow)
		if err == nil {
			decoded++
			// 消费了一个旧响应，重新等待完整 1s 静默窗口。
			continue
		}
		if ctx.Err() != nil || fatalTransportFailure(err) || errors.Is(err, context.Canceled) {
			return decoded, err
		}
		if errors.Is(err, context.DeadlineExceeded) && quietWindow == offlineResponseTimeout {
			return decoded, nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return decoded, errors.New("酷冷离线响应队列未达到完整静默窗口")
		}
		// 损坏帧也已从 HID 队列中消费，但不能算作 JPEG 所需的
		// “成功读取并解码”回包；继续等待队列达到静默。
	}
}

func (driver *Driver) drainResponsesLocked(ctx context.Context) {
	for attempt := 0; attempt < 64; attempt++ {
		if _, err := driver.readResponseWithTimeoutLocked(ctx, 5*time.Millisecond); err != nil {
			return
		}
	}
}

func (driver *Driver) consumeRealtimeResponsesLocked(ctx context.Context) error {
	for attempt := 0; attempt < 64; attempt++ {
		response, err := driver.readResponseWithTimeoutLocked(ctx, 5*time.Millisecond)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil
			}
			return err
		}
		if err := protocol.ValidateStatus(response); err != nil {
			return err
		}
	}
	// 清理上限只约束单帧读取时间，不代表设备故障。若升级前已有
	// 较多成功 ACK，下一帧或下一条命令会继续回收，不能因此断线。
	return nil
}

func (driver *Driver) writeReportLocked(ctx context.Context, report []byte) error {
	written, err := driver.Transport.Write(ctx, report, time.Second)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return err
		}
		return &transportIOError{cause: err}
	}
	if written != len(report) {
		return &transportIOError{cause: fmt.Errorf("HID 短写: %d/%d", written, len(report))}
	}
	return nil
}

func (driver *Driver) readResponseLocked(ctx context.Context) (protocol.Response, error) {
	return driver.readResponseLockedTimeout(ctx, 5*time.Second)
}

func (driver *Driver) readResponseLockedTimeout(ctx context.Context, timeout time.Duration) (protocol.Response, error) {
	return driver.readResponseWithTimeoutLocked(ctx, timeout)
}

func (driver *Driver) readResponseWithTimeoutLocked(ctx context.Context, timeout time.Duration) (protocol.Response, error) {
	report := make([]byte, protocol.ReportSize)
	read, err := driver.Transport.Read(ctx, report, timeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return protocol.Response{}, err
		}
		return protocol.Response{}, &transportIOError{cause: err}
	}
	return protocol.DecodeResponse(report[:read])
}

func (driver *Driver) prepareOfflineResource(manifest theme.Manifest) (string, []byte, error) {
	return driver.prepareOfflineResourceContext(context.Background(), manifest)
}

func (driver *Driver) prepareOfflineResourceContext(ctx context.Context, manifest theme.Manifest) (string, []byte, error) {
	manifest = theme.NormalizeScene(manifest)
	if driver.UseHostFrames(manifest) {
		return "", nil, errors.New("酷冷至尊主题包含离线资源无法完整表达的可见图层")
	}
	var backgroundVideo theme.Layer
	for _, layer := range manifest.Layers {
		if layer.Type == theme.LayerBackgroundVideo && theme.LayerVisible(layer) && layer.Opacity > 0 {
			backgroundVideo = layer
			break
		}
	}
	if backgroundVideo.Type == theme.LayerBackgroundVideo {
		resourceID, _ := backgroundVideo.Options["resource_id"].(string)
		resource, ok := findResource(manifest.Resources, resourceID, ".mp4")
		if !ok || !strings.EqualFold(filepath.Ext(resource.LocalPath), ".mp4") {
			return "", nil, errors.New("酷冷至尊离线视频背景需要 MP4 resource_id")
		}
		data, err := os.ReadFile(resource.LocalPath)
		if err != nil {
			return "", nil, fmt.Errorf("读取酷冷至尊离线视频: %w", err)
		}
		if len(data) == 0 {
			return "", nil, errors.New("酷冷至尊离线视频为空")
		}
		if rotation := driver.DisplayRotation(); rotation != 0 {
			data, err = driver.prepareRotatedOfflineVideo(ctx, resource.LocalPath, data, rotation)
			if err != nil {
				return "", nil, err
			}
		}
		return fmt.Sprintf("suspend_%d.mp4", driver.Slot-1), data, nil
	}
	staticManifest := manifest
	staticManifest.Layers = nil
	for _, layer := range manifest.Layers {
		switch layer.Type {
		case theme.LayerCanvas, theme.LayerImage, theme.LayerBackgroundImage, theme.LayerText, theme.LayerShape:
			staticManifest.Layers = append(staticManifest.Layers, layer)
		}
	}
	renderer, err := render.New(staticManifest)
	if err != nil {
		return "", nil, fmt.Errorf("准备酷冷至尊离线图片: %w", err)
	}
	defer renderer.Close()
	frame, err := renderer.Render(driver.Resolution[0], driver.Resolution[1], render.State{})
	if err != nil {
		return "", nil, fmt.Errorf("渲染酷冷至尊离线图片: %w", err)
	}
	transformed := coolergeometry.Transform(frame, driver.DisplayRotation())
	var output bytes.Buffer
	if err := jpeg.Encode(&output, transformed, &jpeg.Options{Quality: 92}); err != nil {
		return "", nil, fmt.Errorf("编码酷冷至尊离线图片: %w", err)
	}
	// 原厂界面向用户展示槽位 1..5，但 FlashUpdate 遍历渲染器列表
	// 时使用 0 基索引生成 suspend_0..suspend_4。配置保存的是用户
	// 看到的槽位号，因此文件名必须减一；槽位 4 对应 suspend_3。
	return fmt.Sprintf("suspend_%d.jpg", driver.Slot-1), output.Bytes(), nil
}

func (driver *Driver) prepareRotatedOfflineVideo(ctx context.Context, sourcePath string, sourceData []byte, rotation int) ([]byte, error) {
	rotation = coolergeometry.NormalizeRotation(rotation)
	if rotation == 0 {
		return sourceData, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	driver.mediaMu.Lock()
	defer driver.mediaMu.Unlock()

	digest := sha256.Sum256(sourceData)
	cacheDirectory := driver.offlineMediaCacheDirectory()
	if err := os.MkdirAll(cacheDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("创建酷冷至尊方向视频缓存: %w", err)
	}
	cacheName := fmt.Sprintf(
		"v1-%x-%dx%d-r%03d.mp4",
		digest[:], driver.Resolution[0], driver.Resolution[1], rotation,
	)
	cachePath := filepath.Join(cacheDirectory, cacheName)
	if cached, err := readNonEmptyFile(cachePath); err == nil {
		return cached, nil
	} else if errors.Is(err, errEmptyMediaFile) {
		// 崩溃遗留的空缓存不是有效命中，删除后重新生成。
		_ = os.Remove(cachePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取酷冷至尊方向视频缓存: %w", err)
	}

	ffmpegPath, err := driver.resolveFFmpegPath()
	if err != nil {
		return nil, err
	}
	filter := fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,rotate=%d*PI/180:ow=%d:oh=%d:c=black,setsar=1,format=yuv420p",
		driver.Resolution[0], driver.Resolution[1], driver.Resolution[0], driver.Resolution[1],
		rotation, driver.Resolution[0], driver.Resolution[1],
	)
	var failures []string
	for _, encoder := range []string{"h264_mf", "mpeg4"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		temporary, err := os.CreateTemp(cacheDirectory, ".coolermaster-rotation-*.mp4")
		if err != nil {
			return nil, fmt.Errorf("创建酷冷至尊方向视频临时文件: %w", err)
		}
		temporaryPath := temporary.Name()
		if err := temporary.Close(); err != nil {
			_ = os.Remove(temporaryPath)
			return nil, fmt.Errorf("关闭酷冷至尊方向视频临时文件: %w", err)
		}
		_ = os.Remove(temporaryPath)

		arguments := []string{
			"-hide_banner", "-loglevel", "error", "-y", "-i", sourcePath,
			"-map", "0:v:0", "-vf", filter, "-an", "-sn", "-dn", "-c:v", encoder,
		}
		if encoder == "mpeg4" {
			arguments = append(arguments, "-q:v", "3")
		}
		arguments = append(arguments, "-pix_fmt", "yuv420p", "-movflags", "+faststart", temporaryPath)
		command := exec.CommandContext(ctx, ffmpegPath, arguments...)
		output, runErr := command.CombinedOutput()
		if runErr != nil {
			_ = os.Remove(temporaryPath)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			failures = append(failures, fmt.Sprintf("%s: %v (%s)", encoder, runErr, strings.TrimSpace(string(output))))
			continue
		}
		if _, err := readNonEmptyFile(temporaryPath); err != nil {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: 输出无效: %v", encoder, err))
			continue
		}
		if err := os.Rename(temporaryPath, cachePath); err != nil {
			_ = os.Remove(temporaryPath)
			if cached, cacheErr := readNonEmptyFile(cachePath); cacheErr == nil {
				return cached, nil
			}
			return nil, fmt.Errorf("保存酷冷至尊方向视频缓存: %w", err)
		}
		cached, err := readNonEmptyFile(cachePath)
		if err != nil {
			return nil, fmt.Errorf("读取已生成的酷冷至尊方向视频: %w", err)
		}
		return cached, nil
	}
	return nil, fmt.Errorf("转换酷冷至尊离线视频方向失败: %s", strings.Join(failures, "; "))
}

func (driver *Driver) offlineMediaCacheDirectory() string {
	if strings.TrimSpace(driver.StatePath) != "" {
		return filepath.Join(filepath.Dir(driver.StatePath), "coolermaster-media")
	}
	return filepath.Join(os.TempDir(), "casescreen", "coolermaster-media")
}

func (driver *Driver) resolveFFmpegPath() (string, error) {
	driver.settingsMu.RLock()
	configured := strings.TrimSpace(driver.FFmpegPath)
	driver.settingsMu.RUnlock()
	if configured != "" {
		if resolved, err := exec.LookPath(configured); err == nil {
			return resolved, nil
		}
		return filepath.Clean(configured), nil
	}
	if executable, err := os.Executable(); err == nil {
		for _, name := range []string{"ffmpeg.exe", "ffmpeg"} {
			candidate := filepath.Join(filepath.Dir(executable), name)
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	for _, name := range []string{"ffmpeg.exe", "ffmpeg"} {
		if resolved, err := exec.LookPath(name); err == nil {
			return resolved, nil
		}
	}
	return "", errors.New("酷冷至尊离线视频需要 FFmpeg 进行方向转换，但未找到 ffmpeg.exe")
}

func readNonEmptyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errEmptyMediaFile
	}
	return data, nil
}

func findResource(resources []theme.Resource, id, extension string) (theme.Resource, bool) {
	if id != "" {
		for _, resource := range resources {
			if resource.ID == id {
				return resource, true
			}
		}
		return theme.Resource{}, false
	}
	for _, resource := range resources {
		if strings.EqualFold(filepath.Ext(resource.LocalPath), extension) {
			return resource, true
		}
	}
	return theme.Resource{}, false
}

func offlineVideoGeometry(layer theme.Layer) bool {
	const epsilon = 1e-9
	bounds := layer.Bounds
	if math.Abs(bounds.X) > epsilon || math.Abs(bounds.Y) > epsilon ||
		math.Abs(bounds.Width-1) > epsilon || math.Abs(bounds.Height-1) > epsilon ||
		math.Abs(layer.Opacity-1) > epsilon || math.Abs(layer.Rotation) > epsilon {
		return false
	}
	fit, _ := layer.Options["fit"].(string)
	fit = strings.ToLower(strings.TrimSpace(fit))
	if fit != "" && fit != "cover" {
		return false
	}
	return theme.OptionBool(layer.Options, "loop", true)
}

func loadState(path string) (storedState, error) {
	state := storedState{Slots: make(map[int]storedSlot)}
	if path == "" {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("读取酷冷至尊离线资源状态: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("解析酷冷至尊离线资源状态: %w", err)
	}
	if state.Slots == nil {
		state.Slots = make(map[int]storedSlot)
	}
	return state, nil
}

func saveState(path string, state storedState) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeStateFileAtomically(path, data, replaceStateFile)
}

func writeStateFileAtomically(
	path string,
	data []byte,
	replace func(string, string) error,
) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".coolermaster-state-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	replaced := false
	defer func() {
		_ = temporary.Close()
		if !replaced {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	// 先把完整 JSON 刷到临时文件，再做同目录原子替换；
	// 不会在崩溃或断电时留下半个新 state.json。
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if replace == nil {
		replace = replaceStateFile
	}
	if err := replace(temporaryPath, path); err != nil {
		return err
	}
	replaced = true
	return syncStateDirectory(directory)
}

func randomTransferID() (byte, error) {
	var value [1]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0, fmt.Errorf("生成酷冷至尊传输 ID: %w", err)
	}
	return value[0] % 60, nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
